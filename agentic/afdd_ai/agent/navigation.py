"""`open_in_app`: the agent opens a page of the application for the user (contract I-1).

The tool validates a navigation target and returns it as {"navigation": {...}};
the SSE layer turns a successful result into a `navigate` event (and the chat
history into the message's `navigations`), and the assistant UI builds the URL
and follows it. The tool never changes anything in the plant: it only says
which page, equipment, instrument, location or view to show.

Validation runs as the user (`backend_api.Api`): the page needs the user's read
permission, the equipment must be visible to the user (resolved to its alias
and id) and the parameter must be one of that equipment's points. A location
must be one the user can see (their location scope; the twin binds its 3D
scenes by these names), and so must the chiller plant for the BMS plant
overview. Fields that don't apply to the page are dropped (null) and named in
the note; anything else wrong comes back as {"error": "..."} so the model can
correct the call.

`filters` are the page's own filters in semantic terms (status words, equipment
aliases, dates): they are validated as the user too and come back as the page's
URL search params with ids resolved (`filters`: URL key -> values), together
with the tab or view param a section needs; the UI only appends them.

The chat request's UI context reaches the tool as `temp:` state (`ui_state`,
set by the runner on every turn): while the user walks through the twin, a twin
target without `view` stays in walk mode, and a twin target during a simulation
replay stays in that replay (the app keeps the replay's job, variant and time).
"""

from __future__ import annotations

import re
from collections.abc import Callable, Mapping, Sequence
from datetime import date, datetime, timedelta
from typing import Any, Literal, NamedTuple

from google.adk.tools import ToolContext

from . import state as st
from .backend_api import ASSET_CACHE_S, Api, ToolError, cache_get, cache_put, error_result, normalise_alias, plant_tz
from .maintenance import ORDER_TYPE, PRIORITY, SEVERITY
from .rag_tool import DOC_TYPES
from .tools_backend import MO_STATUS

# Keys of a navigation target, in contract order (unset keys are null).
NAV_KEYS = ("page", "label", "asset", "asset_id", "parameter", "location", "tab", "item_id", "view", "panel", "section", "filters")

Page = Literal[
    "digital-twin",
    "ai-insights",
    "simulation",
    "production",
    "asset-overview",
    "alarm-report",
    "maintenance-order",
    "maintenance-performance",
    "maintenance-instruction",
    "location-overview",
    "early-warning-and-diagnostics",
    "early-warning-dashboard",
    "personnel-management",
    "plant-overview",
]

# page -> (title for labels, permission group the route needs, optional fields that apply)
PAGES: dict[str, tuple[str, str, frozenset[str]]] = {
    "digital-twin": ("the digital twin", "digitaltwin", frozenset({"asset", "parameter", "location", "view", "panel"})),
    "ai-insights": ("AI Insights", "aiinsights", frozenset({"tab", "item_id", "filters"})),
    "simulation": ("Simulation", "simulation", frozenset({"item_id", "section", "filters"})),
    "production": ("Production", "production", frozenset({"section", "filters"})),
    "asset-overview": ("Equipment overview", "equipmentoverview", frozenset({"asset", "section", "filters"})),
    "alarm-report": ("Alarm report", "alarmsreport", frozenset({"section", "filters"})),
    "maintenance-order": ("Maintenance orders", "maintenanceorder", frozenset({"section", "filters"})),
    "maintenance-performance": ("Maintenance performance", "maintenanceperformance", frozenset()),
    "maintenance-instruction": ("Maintenance instructions", "maintenanceinstruction", frozenset()),
    "location-overview": ("Location overview", "locationoverview", frozenset()),
    "early-warning-and-diagnostics": ("Early warning & diagnostics", "earlywarningdiagnostics", frozenset({"section", "filters"})),
    "early-warning-dashboard": ("Early warning dashboard", "earlywarningdashboard", frozenset()),
    "personnel-management": ("Personnel", "personnelmanagement", frozenset()),
    "plant-overview": ("Plant overview", "locationoverview", frozenset({"section", "filters"})),
}


def _spellings(table: Mapping[str, tuple[str, ...]]) -> dict[str, str]:
    """{value: (alias, ...)} -> {alias: value}."""
    return {alias: value for value, aliases in table.items() for alias in aliases}


TABS = ("recommendations", "faults", "anomalies", "documents", "rules", "injection")
ITEM_TABS = frozenset({"recommendations", "faults", "anomalies"})  # tabs whose item_id selects a row
# production section -> the tab it opens (a tab id opens itself; None: the KPI strip above the tabs)
SECTION_TAB: dict[str, str | None] = {
    **{tab: tab for tab in ("overview", "tools", "lots", "quality")},
    **dict.fromkeys(("kpi", "oee", "output", "flow")),
    **dict.fromkeys(("oee-losses", "oee-trend", "outs", "wip-area", "cycle-time", "line-profile", "products", "tool-table"), "overview"),
    **dict.fromkeys(("timeline", "oee-by-tool", "oee-by-type"), "tools"),
    "lot-detail": "lots",
    **dict.fromkeys(("characteristic", "capability", "scrap", "capability-summary"), "quality"),
}
SECTIONS = {
    "simulation": ("new", "new-plan"),
    "production": tuple(SECTION_TAB),
    "asset-overview": ("status", "parameters", "spc", "alarms", "maintenance"),
    "plant-overview": ("summary", "locations"),
    "maintenance-order": ("list", "calendar"),
    "alarm-report": ("report", "events"),
    "early-warning-and-diagnostics": ("list", "diagnostics", "current-issues", "other-issues"),
}
VIEWS = ("walk", "orbit")
PANELS = ("plant-overview",)

# Spellings the model may use for the enum values.
_TAB_ALIASES = {"recommendation": "recommendations", "fault": "faults", "anomaly": "anomalies", "document": "documents", "rule": "rules", "fault-injection": "injection"}
_VIEW_ALIASES = {"walk-mode": "walk", "first-person": "walk", "fpv": "walk"}
_PANEL_ALIASES = {"plant": "plant-overview", "bms": "plant-overview", "bms-overview": "plant-overview", "bms-plant-overview": "plant-overview"}
SECTION_ALIASES: dict[str, dict[str, str]] = {
    "simulation": _spellings({"new": ("new-scenario", "scenario-builder", "builder", "scenario"), "new-plan": ("new-production-plan", "production-plan", "plan")}),
    "production": _spellings(
        {
            "tools": ("equipment",),  # equipment -> tools and wip -> lots kept for stored answers
            "lots": ("lot", "wip"),
            "quality": ("spc-tab",),
            "kpi": ("kpis", "kpi-strip", "headline", "headlines", "summary"),
            "oee": ("oee-kpi", "equipment-kpi", "availability"),
            "output": ("outputs", "output-kpi"),
            "flow": ("flow-kpi",),
            "oee-losses": ("losses", "oee-loss", "loss-waterfall"),
            "oee-trend": ("trend",),
            "outs": ("wafer-outs", "moves"),
            "wip-area": ("wip-by-area",),
            "cycle-time": ("cycle-time-trend",),
            "line-profile": ("mask-layers", "layers"),
            "products": ("product",),
            "tool-table": ("tools-table", "production-tools"),
            "timeline": ("gantt", "time-states"),
            "oee-by-tool": ("tool-oee",),
            "oee-by-type": ("type-oee",),
            "lot-detail": ("lot-details",),
            "characteristic": ("spc", "spc-chart", "chart"),
            "capability": ("cpk", "capability-by-chamber"),
            "scrap": ("scrap-pareto", "scrap-by-reason"),
            "capability-summary": ("cpk-summary",),
        }
    ),
    "asset-overview": _spellings(
        {
            "status": ("status-history",),
            "parameters": ("parameter", "live", "real-time"),
            "spc": ("spc-charts",),
            "alarms": ("alarm", "recent-alarms"),
            "maintenance": ("recent-maintenance", "orders", "maintenance-orders"),
        }
    ),
    "plant-overview": _spellings({"summary": ("tiles", "counts", "kpis", "period"), "locations": ("cards", "grid", "location-cards")}),
    "maintenance-order": _spellings({"list": ("table", "list-view"), "calendar": ("calendar-view",)}),
    "alarm-report": _spellings({"report": ("table", "summary", "report-view"), "events": ("event-list", "event-view", "events-list")}),
    "early-warning-and-diagnostics": _spellings(
        {
            "list": ("early-warning-list", "warnings"),
            "diagnostics": ("diagnosis", "detail", "details"),
            "current-issues": ("issues",),
            "other-issues": ("possible-issues", "other-possible-issues"),
        }
    ),
}

# The plant's locations (simulator seed); the twin binds its 3D scenes to these names.
LOCATIONS = (
    "FAB1 Wafer Fab",
    "FAB1 L1 Sub-Fab",
    "Sub-Fab North",
    "Sub-Fab South",
    "FAB1 L2 Cleanroom",
    "Photo Bay",
    "Etch & Thin Film Bay",
    "Implant & CMP Bay",
    "Diffusion Bay",
    "Wet & Metrology Bay",
    "FAB1 L4 Mechanical Deck",
    "MAU Plant Room North",
    "MAU Plant Room South",
    "CUB Central Utility Building",
    "CUB L1 Utility Floor",
    "Chiller Plant Room",
    "PCW Pump Room",
    "CDA Compressor Room",
    "CUB Roof",
    "Cooling Tower Yard",
)
# Every location has a 3D scene (the FAB1 building, sub-fab rooms, the L2 cleanroom and its five bays,
# the L4 MAU rooms, the CUB rooms and roof).
LOCATIONS_3D = frozenset(LOCATIONS)
# Equipment kinds (alias prefix) with a 3D model in the twin: every unit of the plant, the FAB1
# production tools and the sub-fab support units.
MODELLED_KINDS = frozenset(
    {"CH", "CHWP", "CWP", "CT", "CHWS", "PCWP", "CDA", "MAU", "DP", "SCR", "PCH"}
    | {"SCN", "STP", "TRK", "ETCH", "CVD", "PVD", "ECP", "CMP", "FRN", "RTP", "IMP", "WET", "SWC", "ASH", "MET", "CDS", "OVL", "DEF", "WAT"}
)
# Unmodelled kinds whose room has a 3D scene: the twin shows the room (none at the moment).
ROOM_ONLY_KINDS: dict[str, str] = {}
# The location whose 3D scene holds the plant the BMS plant overview shows (CHWS_01).
PLANT_ROOM = "Chiller Plant Room"

LABEL_MAX = 80
REPLAY_SUFFIX = " (replay of simulation #{job})"  # label of a twin target shown inside a replay
NAV_STATE = "temp:navigated"  # invocation id of the answer that navigated (or is navigating)
# What the user is looking at (chat request UI context), set by the runner on every turn.
UI_VIEW = "temp:ui_view"  # "walk" while the user walks through the twin, else None
UI_REPLAY = "temp:ui_replay_job"  # simulation job the twin is replaying, else None
_CONTROL = re.compile(r"[\x00-\x1f\x7f<>]")


def _key(value: Any) -> str:
    """'Walk Mode ' -> 'walk-mode' (enum spelling)."""
    return re.sub(r"[\s_]+", "-", str(value or "").strip().lower())


def _enum(value: Any, allowed: tuple[str, ...], aliases: Mapping[str, str], what: str) -> str | None:
    key = _key(value)
    if not key:
        return None
    key = aliases.get(key, key)
    if key not in allowed:
        raise ToolError(f"Unknown {what} '{value}'. Use one of: {', '.join(allowed)}.")
    return key


def ui_state(context: Mapping[str, Any] | None) -> dict[str, Any]:
    """`temp:` state for the tools from a chat request's UI context: walk mode and the replayed
    simulation job, both only on the twin page (the simulation page sends its selected job as
    `replay_job_id` too). Both keys every turn, so nothing of an earlier turn survives."""
    ctx = context or {}
    twin = _key(ctx.get("page")) == "digital-twin"
    view = _key(ctx.get("view"))
    try:
        job = int(ctx.get("replay_job_id") or 0)
    except (TypeError, ValueError):
        job = 0
    return {UI_VIEW: "walk" if twin and _VIEW_ALIASES.get(view, view) == "walk" else None, UI_REPLAY: job if twin and job > 0 else None}


async def _location_rows(api: Api) -> list[dict[str, Any]]:
    """The locations the user can see (their location scope) as [{id, parent_id, name}], cached per user like the equipment list."""
    cached = cache_get(api.token, "location_rows")
    if cached is not None:
        return cached
    rows = await api.call("filter/getalllocationsandassets", {}, what="the location list")
    out = [{"id": r.get("id"), "parent_id": r.get("parent_id"), "name": str(r["name"])} for r in rows or [] if isinstance(r, dict) and r.get("name")]
    cache_put(api.token, "location_rows", out, ASSET_CACHE_S)
    return out


async def visible_locations(api: Api) -> list[str]:
    """Names of the locations the user can see (their location scope)."""
    return list(dict.fromkeys(r["name"] for r in await _location_rows(api)))


def _match_locations(text: str, names: Sequence[str]) -> list[str]:
    lowered = text.lower()
    exact = [name for name in names if name.lower() == lowered]
    return exact or [name for name in names if lowered in name.lower()]


# Former location names (renamed with the FAB1 production line, 2026-10-13).
LOCATION_ALIASES = {"sub-fab bay a (etch)": "Sub-Fab North", "sub-fab bay b (cvd)": "Sub-Fab South", "bay a": "Sub-Fab North",
                    "bay b": "Sub-Fab South", "cleanroom": "FAB1 L2 Cleanroom", "litho bay": "Photo Bay", "lithography bay": "Photo Bay"}


def resolve_location(value: Any, visible: Sequence[str] = LOCATIONS) -> str | None:
    """Canonical name of a location the user can see (case-insensitive; a unique partial name is
    accepted). A plant location outside `visible` is refused as outside the user's access."""
    text = re.sub(r"\s+", " ", str(value or "")).strip()
    if not text:
        return None
    text = LOCATION_ALIASES.get(text.lower(), text)
    found = _match_locations(text, visible)
    if len(found) == 1:
        return found[0]
    if found:
        raise ToolError(f"Location '{text}' matches several locations ({', '.join(found)}); use the full name.")
    hidden = _match_locations(text, LOCATIONS)
    if hidden:
        raise ToolError(f"{' / '.join(hidden)} is outside the user's location access, so it cannot be shown. Tell the user instead of navigating.")
    raise ToolError(f"Unknown location '{text}'. Locations: {', '.join(visible) or 'none (no location access)'}.")


def _item_id(value: Any) -> int | None:
    if value is None or value == "" or isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        raise ToolError(f"item_id must be a whole number (got '{value}').") from None
    if number != int(number):
        raise ToolError(f"item_id must be a whole number (got '{value}').")
    item = int(number)
    if item == 0:
        return None
    if item < 0:
        raise ToolError("item_id must be a positive id taken from a tool result.")
    return item


def _display(alias: Any) -> str:
    """CT_01 -> CT-01 (how tags are written on the equipment)."""
    return str(alias or "").replace("_", "-")


def _kind(alias: Any) -> str:
    return str(alias or "").split("_", 1)[0].upper()


def _clean_text(value: Any, limit: int | None = None) -> str:
    """Control characters and <> to spaces, whitespace collapsed; cut at `limit` without an ellipsis."""
    text = re.sub(r"\s+", " ", _CONTROL.sub(" ", str(value or ""))).strip()
    return text if limit is None else text[:limit].rstrip()


def _clean_label(value: Any, limit: int = LABEL_MAX) -> str:
    text = _clean_text(value)
    return text if len(text) <= limit else text[: limit - 1].rstrip() + "…"


# ---------------------------------------------------------------------------
# Filters: the model's semantic filters -> the page's own URL search params
# ---------------------------------------------------------------------------


def _words(value: str) -> str:
    return value.replace("-", " ")


class _Filter(NamedTuple):
    """A filter of a page: its URL key and kind; for enums the canonical values (`_key` spelling), their
    aliases, URL form (None: nothing emitted) and label words."""

    url: str
    kind: str = "one"  # one / list (enums), unit / units / alias (equipment), text, token, rule, dates, day, location, parameter
    allowed: tuple[str, ...] = ()
    aliases: Mapping[str, str] = {}
    to_url: Callable[[str], str | None] = str
    label: Callable[[str], str] = _words
    size: int = 0  # text: the longest value kept
    tab: str | None = None  # production: the tab the filter belongs to
    expand: Mapping[str, tuple[str, ...]] = {}  # list: a value that stands for several


# Window words of the hour and day filters.
_HOURS = _spellings(
    {"12": ("shift", "12h"), "24": ("day", "24h", "1d"), "72": ("3d", "72h", "3-days"), "168": ("week", "7d", "last-7-days", "this-week"),
     "720": ("month", "30d", "last-30-days"), "2160": ("quarter", "90d", "last-90-days")}
)  # fmt: skip
_DAYS = {**{word: str(int(h) // 24) for word, h in _HOURS.items() if int(h) >= 24}, "today": "1"}
_HOUR_LABEL = {"12": "last 12 h", "24": "last 24 h", "72": "last 3 days", "168": "last 7 days", "720": "last 30 days", "2160": "last 90 days"}


def _hours(url: str, allowed: tuple[str, ...], tab: str | None = None) -> _Filter:
    return _Filter(url, "one", allowed, _HOURS, label=_HOUR_LABEL.__getitem__, tab=tab)


_FAULT_STATUS = _Filter("status", "one", ("active", "cleared", "all"), {"any": "all"}, lambda v: v if v == "all" else v.upper())
_INSIGHT_WINDOW = _hours("hours", ("24", "168", "720", "2160"))
_SEVERITY = _Filter("severityIds", "list", ("critical", "high", "medium", "low"), {"crit": "critical", "med": "medium"}, lambda v: str(SEVERITY[v.upper()]))
_EQUIPMENT = _Filter("assetIds", "units")  # + no-locationIds-filter, without which the page ignores assetIds
_PERIOD = {"start": _Filter("start", "dates"), "end": _Filter("end", "dates")}
_SEARCH = _Filter("search", "text", size=60)

# page (ai-insights: its tab) -> {filter: _Filter} in the order of the page's filters (labels follow it)
FILTERS: dict[str, dict[str, _Filter]] = {
    "production": {
        "window": _hours("window", ("12", "24", "168", "720")),
        "lot_status": _Filter(
            "lot-status", "one", ("wip", "hold", "done", "all"),
            _spellings({"wip": ("in-fab", "in-the-fab", "in-process", "processing", "active"), "hold": ("on-hold", "held"),
                        "done": ("completed", "finished", "shipped", "out"), "all": ("any",)}),
            tab="lots",
        ),  # fmt: skip
        "lot_search": _Filter("lot-search", "text", size=40, tab="lots"),
        "lot": _Filter("lot", "token", label=lambda v: f"lot {v}", tab="lots"),
        "characteristic": _Filter("char", "token", label=str, tab="quality"),
        "quality_window": _hours("quality-window", ("24", "168", "720"), "quality"),
        "quality_tool": _Filter("quality-tool", "unit", tab="quality"),
        "timeline_window": _hours("timeline-window", ("12", "24", "72", "168"), "tools"),
    },
    "plant-overview": {
        "location": _Filter("location", "location"),
        "period": _Filter("period", "one", ("1", "7", "30", "90"), _DAYS, label=lambda v: "today" if v == "1" else f"last {v} days"),
    },
    "faults": {
        "status": _FAULT_STATUS,
        "equipment": _Filter("asset", "unit"),
        "rule": _Filter("rule", "rule"),
        "window": _INSIGHT_WINDOW,
        "display": _Filter("display", "one", ("list", "timeline"), {"table": "list", "chart": "timeline", "gantt": "timeline"}),
    },
    "anomalies": {
        "status": _FAULT_STATUS,
        "detector": _Filter("detector", "one", ("range", "flatline", "zscore", "ewma", "spc"), {"z-score": "zscore", "flat-line": "flatline"}),
        "equipment": _Filter("asset", "unit"),
        "window": _INSIGHT_WINDOW,
    },
    "recommendations": {
        "status": _Filter(
            "status", "one", ("proposed", "validating", "validated", "rejected", "applied", "expired", "dismissed", "failed", "all"),
            to_url=lambda v: None if v == "all" else v.upper(),
        ),  # fmt: skip
    },
    "documents": {
        "search": _Filter("q", "text", size=60),
        "doc_type": _Filter(
            "type", "one", tuple(t.replace("_", "-") for t in DOC_TYPES),
            {"report": "maintenance-report", "setpoint": "setpoint-log", "fdd": "fdd-reference"}, lambda v: v.replace("-", "_"),
        ),  # fmt: skip
        "status": _Filter("status", "one", ("pending", "indexing", "ready", "failed"), {"indexed": "ready"}, str.upper),
        "equipment": _Filter("unit", "alias"),
    },
    "simulation": {
        "status": _Filter(
            "status", "one", ("all", "approval", "active", "succeeded", "failed", "closed"),
            _spellings({"approval": ("needs-approval", "pending-approval", "proposed", "pending"),
                        "active": ("queued", "running", "analyzing", "in-progress"),
                        "succeeded": ("success", "successful", "completed", "done", "finished"), "failed": ("failure", "error"),
                        "closed": ("rejected", "cancelled", "canceled"), "all": ("any",)}),
            lambda v: None if v == "all" else v,
        ),  # fmt: skip
        "origin": _Filter(
            "origin", "one", ("ui", "agent", "optimizer", "analytics"),
            _spellings({"ui": ("user", "manual", "operator"), "agent": ("assistant", "ai", "chat"),
                        "optimizer": ("optimiser", "optimisation", "optimization"), "analytics": ("analysis",)}),
        ),  # fmt: skip
        "workspace": _Filter("domain", "one", ("chiller", "fab"), _spellings({"chiller": ("plant", "chiller-plant", "cub"), "fab": ("production", "fab1", "wafer-fab")})),
    },
    "asset-overview": {
        "date": _Filter("date", "day"),
        "parameter_search": _Filter("parameter-search", "text", size=40),
        "parameter": _Filter("parameter-search", "parameter", size=40),
    },
    "maintenance-order": {
        "status": _Filter(
            "maintenanceStatusIds", "list", ("requested", "rejected", "open", "in-progress", "paused", "done", "closed", "outstanding"),
            _spellings({"in-progress": ("started", "ongoing"), "paused": ("on-hold",), "done": ("completed", "finished"), "outstanding": ("pending", "unfinished")}),
            lambda v: str(MO_STATUS[v.upper().replace("-", "_")]),
            # the order API adds paused whenever in progress is filtered
            expand={"outstanding": ("requested", "open", "in-progress")},
        ),  # fmt: skip
        "severity": _SEVERITY,
        "priority": _Filter("priorityIds", "list", ("high", "medium", "low"), to_url=lambda v: str(PRIORITY[v.upper()])),
        "order_type": _Filter(
            "maintenanceTypeIds", "list", ("corrective", "preventive"),
            _spellings({"corrective": ("cm", "repair"), "preventive": ("pm", "planned", "preventative")}), lambda v: str(ORDER_TYPE[v.upper()]),
        ),  # fmt: skip
        "equipment": _EQUIPMENT,
        **_PERIOD,
        "search": _SEARCH,
    },
    "alarm-report": {
        # the page shows one type (alarmTypeIds[0]): both types emit nothing, the page's "all"
        "alarm_type": _Filter(
            "alarmTypeIds", "list", ("alarm", "abnormal", "both"),
            _spellings({"alarm": ("alarms",), "abnormal": ("abnormality", "abnormal-behaviour", "abnormal-behavior", "anomaly"), "both": ("all", "any")}),
            str.upper, lambda v: "alarm, abnormal" if v == "both" else v, expand={"both": ("alarm", "abnormal")},
        ),  # fmt: skip
        "equipment": _EQUIPMENT,
        **_PERIOD,
        "search": _SEARCH,
    },
    "early-warning-and-diagnostics": {"severity": _SEVERITY, "equipment": _EQUIPMENT, **_PERIOD, "search": _SEARCH},
}
# Filter names the model may use (after lower case and '_' for spaces and '-'), shared and per page (ai-insights: per tab).
_FILTER_KEY_ALIASES = _spellings(
    {"equipment": ("asset", "assets", "unit", "units", "equipments"), "start": ("from",), "end": ("to",), "search": ("query", "q", "text"),
     "status": ("statuses",), "severity": ("severities",), "priority": ("priorities",)}
)  # fmt: skip
_PERIOD_KEYS = dict.fromkeys(("date", "day", "period", "window"), "start")
_PAGE_KEY_ALIASES: dict[str, dict[str, str]] = {
    "production": _spellings({"characteristic": ("char", "char_key"), "quality_tool": ("tool",), "lot_status": ("lots", "lot_filter"), "lot": ("lot_code",)}),
    "documents": {"type": "doc_type"},
    "asset-overview": {"search": "parameter_search"},
    "maintenance-order": {**dict.fromkeys(("type", "types", "maintenance_type"), "order_type"), **_PERIOD_KEYS},
    "alarm-report": {"type": "alarm_type", **_PERIOD_KEYS},
    "early-warning-and-diagnostics": _PERIOD_KEYS,
}
# page -> (URL key, {section: value}): the param that switches the page to a section's tab or view
SECTION_PARAMS = {
    "asset-overview": ("recent", {"alarms": "alarm", "maintenance": "maintenance"}),
    "maintenance-order": ("view", {"list": "list", "calendar": "calendar"}),
    "alarm-report": ("eventview", {"report": "false", "events": "true"}),
}
SECTION_WORKSPACE = {"new": "chiller", "new-plan": "fab"}  # simulation: the workspace a section opens
_RELATIVE = ("today", "yesterday", "this-week", "last-7-days", "last-30-days")
_TOKEN = re.compile(r"[A-Za-z0-9][\w.\-]{0,39}", re.ASCII)
_RULE = re.compile(r"[A-Z]{2,4}-[0-9]{3}")


def _today() -> date:
    """Today in plant time (a function, so tests can fix it)."""
    return datetime.now(plant_tz()).date()


def _values(key: str, raw: Any) -> list[str]:
    """A filter's values as strings: a value or a list of values, integral floats as integers, empties skipped."""
    out: list[str] = []
    for value in raw if isinstance(raw, (list, tuple)) else [raw]:
        if isinstance(value, (bool, Mapping, list, tuple)):
            raise ToolError(f"{key} must be a value or a list of values.")
        if isinstance(value, float) and value.is_integer():
            value = int(value)  # the model sends numbers as floats
        text = "" if value is None else str(value).strip()
        if text:
            out.append(text)
    return out


def _iso_day(key: str, value: str) -> date:
    try:
        return date.fromisoformat(value)
    except ValueError:
        raise ToolError(f"{key} must be a date like 2026-10-08 or one of today, yesterday, this-week, last-7-days, last-30-days (got '{value}').") from None


def _period(start: str | None, end: str | None, week_to_sunday: bool) -> tuple[date, date, str]:
    """First and last plant day of a start / end filter, and its label words."""
    today = _today()
    word = next((_key(v) for v in (start, end) if v and _key(v) in _RELATIVE), None)
    if word:
        if any(v and _key(v) != word for v in (start, end)):
            raise ToolError("Use a relative period (e.g. last-7-days) alone, without another date.")
        s = today - timedelta(days={"today": 0, "yesterday": 1, "this-week": today.weekday(), "last-7-days": 6, "last-30-days": 29}[word])
        e = s if word == "yesterday" else today
        if word == "this-week" and week_to_sunday:
            e = s + timedelta(days=6)  # the order list keeps planned_start < end: the rest of the week too
        return s, e, _words(word)
    first = _iso_day("start", start) if start else None
    last = _iso_day("end", end) if end else None
    s = first or last
    e = last or max(s, today)
    if s > e:
        raise ToolError(f"start ({s}) is after end ({e}).")
    if (e - s).days + 1 > 366:
        raise ToolError("The period is longer than 366 days; use a shorter one.")
    return s, e, str(s) if s == e else f"{s} to {e}"


def _one_day(value: str) -> tuple[date, str]:
    word = _key(value)
    if word in ("today", "yesterday"):
        return _today() - timedelta(days=1 if word == "yesterday" else 0), word
    try:
        day = date.fromisoformat(value)
    except ValueError:
        raise ToolError(f"date is one day: use a date like 2026-10-08, today or yesterday (got '{value}').") from None
    return day, str(day)


async def _unit(api: Api, ref: str) -> Mapping[str, Any]:
    """Visible equipment by alias, id or name; retried by its tag ('CH_01' -> 'CH-01'), because without
    digitaltwin.read the equipment rows have names only."""
    try:
        return await api.resolve_asset(ref)
    except ToolError as err:
        tag = _display(normalise_alias(ref))
        # the tag as a word of the name: 'CH-01' is also part of 'Process Chiller PCH-01'
        named = [a for a in await api.assets() if tag.upper() in str(a.get("name") or "").upper().split()]
        if len(named) == 1:
            return named[0]
        if tag == ref:
            raise
        try:
            return await api.resolve_asset(tag)
        except ToolError:
            raise err from None


async def _units(api: Api, refs: Sequence[str]) -> list[Mapping[str, Any]]:
    units: dict[Any, Mapping[str, Any]] = {}
    for ref in refs[:60]:
        unit = await _unit(api, ref)
        units.setdefault(unit.get("id"), unit)
    return list(units.values())


def _unit_label(unit: Mapping[str, Any]) -> str:
    return _display(unit["alias"]) if unit.get("alias") else str(unit.get("name"))


async def _filters(
    page_key: str, nav: dict[str, Any], raw: Any, api: Api, unit: Mapping[str, Any] | None, location: Any, parameter: Any
) -> tuple[dict[str, list[str]] | None, list[str], list[str], list[str], list[str]]:
    """The page's URL params for the model's `filters`, with the param a section needs: (params or None,
    ignored names, top-level fields used, label pieces, notes). May set nav["section"]."""
    if raw is not None and not isinstance(raw, Mapping):
        raise ToolError('filters must be an object of filter names and values, e.g. {"status": ["open"]}.')
    scope = nav["tab"] if page_key == "ai-insights" else page_key
    used = "filters" in PAGES[page_key][2] and not (page_key == "asset-overview" and unit is None)
    specs = FILTERS.get(scope or "", {}) if used else {}
    values: dict[str, list[str]] = {}
    one_day = False  # an ISO date under date / day / period / window is that day
    for name, given in (raw or {}).items():
        key = re.sub(r"[\s-]+", "_", str(name).strip().lower())
        key = _FILTER_KEY_ALIASES.get(key, key)
        one_day |= key in _PERIOD_KEYS and key in _PAGE_KEY_ALIASES.get(scope, {})
        key = _PAGE_KEY_ALIASES.get(scope, {}).get(key, key)
        if key not in specs:  # not used here: named as ignored, not validated
            if given not in (None, "", [], ()):
                values.setdefault(key, [])
        elif found := _values(key, given):
            values.setdefault(key, []).extend(found)  # status + statuses: one list
    if not used:
        return None, [f"filters.{k}" for k in values], [], [], []
    if page_key == "ai-insights" and values and nav["tab"] is None:
        raise ToolError("AI Insights filters need their tab: faults, anomalies, recommendations or documents.")

    # the model predictably sends these as top-level fields: used as the filter, not ignored
    consumed: list[str] = []
    if page_key == "plant-overview" and "location" not in values and str(location or "").strip():
        values["location"], consumed = [str(location).strip()], ["location"]
    if page_key == "asset-overview" and not {"parameter", "parameter_search"} & values.keys() and str(parameter or "").strip():
        values["parameter"], consumed = [str(parameter).strip()], ["parameter"]
    if values.get("parameter") and values.get("parameter_search"):
        raise ToolError(f"parameter_search takes one value (got {', '.join(values['parameter'] + values['parameter_search'])}).")

    def one(key: str, f: _Filter | None = None) -> str | None:
        given = values.get(key) or []
        # one value once spelled canonically: 'Cleared' and 'cleared', '7d' and 'week'
        if len({_enum(v, f.allowed, f.aliases, f"{key} filter") if f else v.casefold() for v in given}) > 1:
            raise ToolError(f"{key} takes one value (got {', '.join(dict.fromkeys(given))}).")
        return given[0] if given else None

    section = nav["section"]
    out: dict[str, list[str]] = {}
    tab = None
    if page_key == "production":
        # a section opens its tab; without one, the first tab filter opens its tab; other tabs' filters are ignored
        tab = SECTION_TAB[section] if section else next((f.tab for k, f in specs.items() if f.tab and k in values), None)
        if section is None and tab:
            nav["section"] = section = tab
        if tab and tab != section:
            out["tab"] = [tab]  # a tab id section: the UI sets the tab itself
    if page_key in SECTION_PARAMS and section in SECTION_PARAMS[page_key][1]:
        param, by_section = SECTION_PARAMS[page_key]
        out[param] = [by_section[section]]

    ignored = [f"filters.{k}" for k in values if k not in specs]
    canon: dict[str, str] = {}
    parts: list[str] = []
    for key, f in specs.items():
        if key not in values or (key == "end" and "start" in values):
            continue
        # faults / anomalies: the page ignores the window while it shows the active ones
        if (f.tab and f.tab != tab) or (key == "window" and canon.get("status") == "active"):
            ignored.append(f"filters.{key}")
            continue
        if f.kind == "one":
            value = canon[key] = _enum(one(key, f), f.allowed, f.aliases, f"{key} filter") or ""
            if key == "workspace" and section:  # the section opens its own workspace
                if value != SECTION_WORKSPACE[section]:
                    ignored.append(f"filters.{key}")
                continue
            if (url := f.to_url(value)) is not None:
                out[f.url] = [url]
            parts.append(f.label(value))
        elif f.kind == "list":
            words = [w.strip() for v in values[key] for w in v.split(",") if w.strip()]
            chosen = list(dict.fromkeys(_enum(w, f.allowed, f.aliases, f"{key} filter") or "" for w in words))
            urls = list(dict.fromkeys(f.to_url(x) for v in chosen for x in f.expand.get(v, (v,))))
            if not {f.to_url(x) for v in f.allowed for x in f.expand.get(v, (v,))} <= set(urls):  # all of them: the page's "all"
                out[f.url] = urls
            parts += dict.fromkeys(", ".join(map(f.label, chosen)).split(", "))  # each word once ('both' is 'alarm, abnormal')
        elif f.kind == "units":
            units = await _units(api, [w.strip() for v in values[key] for w in v.split(",") if w.strip()])
            out[f.url] = [str(u.get("id")) for u in units]
            out["no-locationIds-filter"] = ["true"]
            parts += [_unit_label(u) for u in units]
        elif f.kind in ("unit", "alias"):
            found = await _unit(api, one(key) or "")
            if f.kind == "alias" and not found.get("alias"):
                ignored.append(f"filters.{key}")  # without digitaltwin.read the rows have no alias
                continue
            out[f.url] = [str(found["alias"] if f.kind == "alias" else found.get("id"))]
            parts.append(_unit_label(found))
        elif f.kind == "text":
            if text := _clean_text(one(key), f.size):
                out[f.url] = [text]
                parts.append(f"search '{text}'")
        elif f.kind == "token":
            value = one(key) or ""
            if not _TOKEN.fullmatch(value):
                raise ToolError(f"{key} must be a code like the ones the production tools return (got '{value}').")
            out[f.url] = [value.upper()]
            parts.append(f.label(value.upper()))
        elif f.kind == "rule":
            value = one(key) or ""
            if not _RULE.fullmatch(value.upper()):
                raise ToolError(f"rule must be a fault rule code like CHL-011 (got '{value}').")
            out[f.url] = [value.upper()]
            parts.append(value.upper())
        elif f.kind == "dates":
            s, e, text = _period(one("start"), one("end") or (one("start") if one_day else None), page_key == "maintenance-order")
            out["start"], out["end"] = [str(s)], [str(e)]
            parts.append(text)
        elif f.kind == "day":
            day, text = _one_day(one(key) or "")
            out[f.url] = [str(day)]
            parts.append(text)
        elif f.kind == "location":
            rows = await _location_rows(api)
            name = resolve_location(one(key), list(dict.fromkeys(r["name"] for r in rows)))
            row = next(r for r in rows if r["name"] == name)
            shown = row["id"]
            if row["parent_id"] is not None and not any(r["parent_id"] == row["id"] for r in rows):
                shown = row["parent_id"]  # the page shows a location's children: a leaf is shown among its parent's
                nav["section"] = nav["section"] or "locations"
            out[f.url] = [str(shown)]
            parts.append(str(name))
        elif f.kind == "parameter":
            # the page searches parameter names; without digitaltwin.read the parameters cannot be read
            ref = one(key) or ""
            found_name = (await api.resolve_parameter(unit, ref)).get("name") if api.can("digitaltwin.read") else ref.replace("_", " ")
            if text := _clean_text(found_name, f.size):
                out[f.url] = [text]
                parts.append(f"search '{text}'")
    if "hours" in out and "status" not in canon:
        out = {"status": ["all"], **out}  # the window applies to every status, not the default active ones

    notes = []
    if page_key in ("maintenance-order", "early-warning-and-diagnostics") and values and not {"start", "end"} & values.keys():
        notes.append(f"Without a period the page lists only today's {'orders' if page_key == 'maintenance-order' else 'warnings'}; say so.")
    return out or None, ignored, consumed, parts, notes


def default_label(nav: Mapping[str, Any], unit_name: str | None = None, param_name: str | None = None) -> str:
    """A short human label of a target, e.g. "CT-01 Fan Speed in the digital twin"."""
    page = str(nav.get("page"))
    title = PAGES[page][0]
    unit = _display(nav.get("asset")) if nav.get("asset") else unit_name
    if page == "digital-twin":
        subject = " ".join(x for x in (unit, param_name) if x) or nav.get("location")
        if nav.get("view") == "walk":
            return f"Walk to {subject} (first person)" if subject else "First-person walk mode in the digital twin"
        if nav.get("panel") == "plant-overview":
            return f"{subject} with the BMS plant overview" if subject else "BMS plant overview in the digital twin"
        return f"{subject} in {title}" if subject else "The digital twin"
    if page == "ai-insights":
        tab = nav.get("tab")
        if tab and nav.get("item_id"):
            noun = {"recommendations": "recommendation", "faults": "fault", "anomalies": "anomaly"}[str(tab)]
            return f"AI Insights: {noun} #{nav['item_id']}"
        return f"AI Insights: {str(tab).capitalize()}" if tab else title
    if page == "simulation":
        if nav.get("item_id"):
            return f"Simulation #{nav['item_id']}"
        if nav.get("section") == "new-plan":
            return "New production plan"
        return "New simulation scenario" if nav.get("section") == "new" else "Simulation jobs"
    section = nav.get("section")
    if page == "production":
        return f"Production: {_words(str(section))}" if section else "Production monitoring"
    if page == "asset-overview" and unit:
        suffix = {"status": " (status)", "parameters": " (parameters)", "spc": " (SPC)", "alarms": " (recent alarms)", "maintenance": " (recent maintenance)"}
        return f"{unit} equipment details{suffix.get(str(section), '')}"
    if page in ("plant-overview", "maintenance-order", "alarm-report", "early-warning-and-diagnostics") and section:
        return f"{title}: {_words(str(section))}"
    return title


async def open_in_app(
    tool_context: ToolContext,
    page: Page,
    label: str = "",
    asset: str = "",
    parameter: str = "",
    location: str = "",
    tab: str = "",
    item_id: int = 0,
    view: str = "",
    panel: str = "",
    section: str = "",
    filters: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Open a page of the application for the user (the browser follows at once). Only when the user asks
    to open, show, go to, take them to or walk to something, or where something is. Once per answer.

    Args:
      page: The page to open.
      label: Optional short label of what opens (generated when empty).
      asset: digital-twin / asset-overview: equipment alias, e.g. "CT_01".
      parameter: digital-twin with asset: parameter alias of that equipment, e.g. "fan_speed" (opens its instrument);
        asset-overview with asset: narrows its parameter list to that parameter.
      location: digital-twin: location name, e.g. "Cooling Tower Yard"; plant-overview: the location whose cards to show.
      tab: ai-insights: recommendations, faults, anomalies, documents, rules or injection.
      item_id: ai-insights: recommendation / fault / anomaly id (with its tab); simulation: job id. 0 = none.
      view: digital-twin: "walk" (first-person walk mode) or "orbit" (leave walk mode); empty keeps walk mode while the user walks.
      panel: digital-twin: "plant-overview" (BMS chiller plant overview).
      section: simulation: "new" or "new-plan"; production: a tab or panel; asset-overview with asset: status,
        parameters, spc, alarms or maintenance; plant-overview, maintenance-order, alarm-report,
        early-warning-and-diagnostics: a part of the page (NAVIGATION lists the ids).
      filters: the page's filters as {name: value or [values]}, e.g. {"status": ["outstanding"], "equipment":
        ["CH_01"], "start": "this-week"}; NAVIGATION lists them per page. Only what the user asked for.
    """
    state = tool_context.state
    inv = getattr(tool_context, "invocation_id", None)
    claimed, previous = False, None
    try:
        page_key = _key(page)
        if page_key not in PAGES:
            raise ToolError(f"Unknown page '{page}'. Pages: {', '.join(PAGES)}.")
        title, group, fields = PAGES[page_key]
        api = Api(state)
        if not api.can(f"{group}.read"):
            raise ToolError(f"The user cannot open {title}: it needs the permission {group}.read. Tell the user instead of navigating.")

        # One navigation per answer. The claim is taken before anything is awaited, so of several
        # calls in one model response (ADK runs them concurrently) the first one wins; a call that
        # then fails gives the claim back.
        if inv:
            previous = state.get(NAV_STATE)
            if previous == inv:
                raise ToolError(
                    "Another open_in_app call of this answer already opened (or is opening) a page; this one was not used. "
                    "open_in_app is used at most once per answer."
                )
            state[NAV_STATE] = inv
            claimed = True

        given = {"asset": asset, "parameter": parameter, "location": location, "tab": tab, "item_id": item_id, "view": view, "panel": panel, "section": section}
        ignored = [k for k, v in given.items() if k not in fields and v not in (None, "", 0)]
        nav: dict[str, Any] = dict.fromkeys(NAV_KEYS)
        nav["page"] = page_key
        notes: list[str] = []

        if page_key == "digital-twin":
            nav["view"] = _enum(view, VIEWS, _VIEW_ALIASES, "view")
            nav["panel"] = _enum(panel, PANELS, _PANEL_ALIASES, "panel")
            if str(location or "").strip() or nav["panel"] is not None:
                visible = await visible_locations(api)
                nav["location"] = resolve_location(location, visible)
                if nav["panel"] == "plant-overview" and PLANT_ROOM.lower() not in {n.lower() for n in visible}:
                    raise ToolError(
                        f"The BMS plant overview shows the chiller plant ({PLANT_ROOM}), which is outside the user's location access. "
                        "Tell the user instead of navigating."
                    )
        if page_key == "ai-insights":
            nav["tab"] = _enum(tab, TABS, _TAB_ALIASES, "tab")
            if nav["tab"] == "injection" and st.ADMIN not in api.permissions:
                raise ToolError("Fault injection is for administrators only (the AI Insights page hides that tab from the user). Tell the user instead of navigating.")
            item = _item_id(item_id)
            if item is not None and nav["tab"] is None:
                raise ToolError("item_id needs its tab: recommendations, faults or anomalies.")
            if item is not None and nav["tab"] not in ITEM_TABS:
                ignored.append("item_id")
                item = None
            nav["item_id"] = item
        if page_key in SECTIONS:
            nav["section"] = _enum(section, SECTIONS[page_key], SECTION_ALIASES[page_key], "section")
        if page_key == "simulation":
            nav["item_id"] = _item_id(item_id)
            if nav["item_id"] is not None and nav["section"] is not None:
                ignored.append("section")  # a job opens its detail, not the scenario builder
                nav["section"] = None

        unit: Mapping[str, Any] | None = None
        param: Mapping[str, Any] | None = None
        if "asset" in fields and str(asset or "").strip():
            unit = await api.resolve_asset(str(asset))
            nav["asset"] = unit.get("alias")
            nav["asset_id"] = unit.get("id")
        if "parameter" in fields and str(parameter or "").strip():
            if unit is None:
                raise ToolError("parameter needs asset: give the equipment alias too (e.g. asset CT_01, parameter fan_speed).")
            param = await api.resolve_parameter(unit, str(parameter))
            nav["parameter"] = param.get("alias")
        if page_key == "digital-twin" and unit is not None and nav["location"] is not None:
            ignored.append("location")  # the equipment decides where the twin goes
            nav["location"] = None
        if page_key == "asset-overview" and unit is None and nav["section"] is not None:
            ignored.append("section")  # sections belong to one equipment's detail page
            nav["section"] = None
        nav["filters"], filter_ignored, consumed, filter_text, filter_notes = await _filters(page_key, nav, filters, api, unit, location, parameter)
        ignored = [k for k in ignored if k not in consumed] + filter_ignored
        notes.extend(filter_notes)

        replay = None
        if page_key == "digital-twin":
            kind = _kind(nav["asset"])
            no_3d = None  # why walk mode cannot go to the target
            if unit is not None and kind in ROOM_ONLY_KINDS:
                notes.append(f"{nav['asset']} has no 3D model yet; the twin shows its room ({ROOM_ONLY_KINDS[kind]}).")
            elif unit is not None and kind not in MODELLED_KINDS:
                no_3d = f"{nav['asset'] or unit.get('name')} has no 3D model (nor has its location)"
                notes.append(f"{nav['asset'] or unit.get('name')} is not modelled in 3D yet; the twin shows a note.")
            if nav["location"] is not None and nav["location"] not in LOCATIONS_3D:
                no_3d = f"{nav['location']} has no 3D model"
                notes.append(f"{nav['location']} has no 3D model yet; the twin shows it empty.")
            if nav["view"] == "walk" and no_3d:
                raise ToolError(f"{no_3d}, so walk mode cannot go there. 3D locations: {', '.join(sorted(LOCATIONS_3D))}.")
            if nav["view"] is None and state.get(UI_VIEW) == "walk":
                # a target without a view keeps the walking user in walk mode; only "orbit" leaves it
                if no_3d:
                    notes.append("The user was in walk mode; the twin leaves it because there is nothing in 3D to walk to.")
                else:
                    nav["view"] = "walk"
            replay = state.get(UI_REPLAY)
            if replay:
                notes.append(
                    f"The twin stays in the replay of simulation #{replay}: it shows the replayed run, not live data. "
                    "Say so if it matters for the answer."
                )

        # The model's label only when every field it may describe was used as given.
        text = "" if ignored else _clean_label(label)
        if not text:
            text = default_label(nav, unit.get("name") if unit else None, param.get("name") if param else None)
            text = _clean_label(f"{text} · {', '.join(filter_text)}") if filter_text else text
        suffix = REPLAY_SUFFIX.format(job=replay) if replay else ""
        nav["label"] = _clean_label(text, LABEL_MAX - len(suffix)) + suffix if replay else text
        if ignored:
            notes.append(f"Ignored (not used by {page_key}): {', '.join(dict.fromkeys(ignored))}.")
        note = f"The application now shows: {nav['label']}. Say so in one short sentence."
        return {"navigation": nav, "status": "opened", "note": " ".join([note, *notes])}
    except Exception as err:  # noqa: BLE001 - reported to the model
        if claimed:
            state[NAV_STATE] = previous  # a failed call does not count
        return error_result(err)


def navigation_item(payload: Any) -> dict[str, Any] | None:
    """The contract target of a successful open_in_app result (every key, null when unset)."""
    if not isinstance(payload, Mapping) or payload.get("status") != "opened":
        return None
    nav = payload.get("navigation")
    if not isinstance(nav, Mapping) or nav.get("page") not in PAGES:
        return None
    return {k: nav.get(k) for k in NAV_KEYS}
