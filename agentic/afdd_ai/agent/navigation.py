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

The chat request's UI context reaches the tool as `temp:` state (`ui_state`,
set by the runner on every turn): while the user walks through the twin, a twin
target without `view` stays in walk mode, and a twin target during a simulation
replay stays in that replay (the app keeps the replay's job, variant and time).
"""

from __future__ import annotations

import re
from collections.abc import Mapping, Sequence
from typing import Any, Literal

from google.adk.tools import ToolContext

from . import state as st
from .backend_api import ASSET_CACHE_S, Api, ToolError, cache_get, cache_put, error_result

# Keys of a navigation target, in contract order (unset keys are null).
NAV_KEYS = ("page", "label", "asset", "asset_id", "parameter", "location", "tab", "item_id", "view", "panel", "section")

Page = Literal[
    "digital-twin",
    "ai-insights",
    "simulation",
    "asset-overview",
    "alarm-report",
    "maintenance-order",
    "maintenance-performance",
    "maintenance-instruction",
    "location-overview",
    "early-warning-and-diagnostics",
    "early-warning-dashboard",
    "personnel-management",
]

# page -> (title for labels, permission group the route needs, optional fields that apply)
PAGES: dict[str, tuple[str, str, frozenset[str]]] = {
    "digital-twin": ("the digital twin", "digitaltwin", frozenset({"asset", "parameter", "location", "view", "panel"})),
    "ai-insights": ("AI Insights", "aiinsights", frozenset({"tab", "item_id"})),
    "simulation": ("Simulation", "simulation", frozenset({"item_id", "section"})),
    "asset-overview": ("Equipment overview", "equipmentoverview", frozenset({"asset", "section"})),
    "alarm-report": ("Alarm report", "alarmsreport", frozenset()),
    "maintenance-order": ("Maintenance orders", "maintenanceorder", frozenset()),
    "maintenance-performance": ("Maintenance performance", "maintenanceperformance", frozenset()),
    "maintenance-instruction": ("Maintenance instructions", "maintenanceinstruction", frozenset()),
    "location-overview": ("Location overview", "locationoverview", frozenset()),
    "early-warning-and-diagnostics": ("Early warning & diagnostics", "earlywarningdiagnostics", frozenset()),
    "early-warning-dashboard": ("Early warning dashboard", "earlywarningdashboard", frozenset()),
    "personnel-management": ("Personnel", "personnelmanagement", frozenset()),
}

TABS = ("recommendations", "faults", "anomalies", "documents", "rules", "injection")
ITEM_TABS = frozenset({"recommendations", "faults", "anomalies"})  # tabs whose item_id selects a row
SECTIONS = {"simulation": ("new",), "asset-overview": ("status", "parameters", "spc")}
VIEWS = ("walk", "orbit")
PANELS = ("plant-overview",)

# Spellings the model may use for the enum values.
_TAB_ALIASES = {"recommendation": "recommendations", "fault": "faults", "anomaly": "anomalies", "document": "documents", "rule": "rules", "fault-injection": "injection"}
_VIEW_ALIASES = {"walk-mode": "walk", "first-person": "walk", "fpv": "walk"}
_PANEL_ALIASES = {"plant": "plant-overview", "bms": "plant-overview", "bms-overview": "plant-overview", "bms-plant-overview": "plant-overview"}
_SECTION_ALIASES = {"new-scenario": "new", "scenario-builder": "new", "builder": "new"}

# The plant's locations (simulator seed); the twin binds its 3D scenes to these names.
LOCATIONS = (
    "FAB1 Wafer Fab",
    "FAB1 L1 Sub-Fab",
    "Sub-Fab Bay A (Etch)",
    "Sub-Fab Bay B (CVD)",
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
LOCATIONS_3D = frozenset(
    {"CUB Central Utility Building", "CUB L1 Utility Floor", "Chiller Plant Room", "PCW Pump Room", "CDA Compressor Room", "CUB Roof", "Cooling Tower Yard"}
)
# Equipment kinds (alias prefix) with a 3D model in the twin (every CUB unit since 2026-10-12).
MODELLED_KINDS = frozenset({"CH", "CHWP", "CWP", "CT", "CHWS", "PCWP", "CDA"})
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


async def visible_locations(api: Api) -> list[str]:
    """Names of the locations the user can see (their location scope), cached per user like the equipment list."""
    cached = cache_get(api.token, "location_names")
    if cached is not None:
        return cached
    rows = await api.call("filter/getalllocationsandassets", {}, what="the location list")
    names = list(dict.fromkeys(str(r["name"]) for r in rows or [] if isinstance(r, dict) and r.get("name")))
    cache_put(api.token, "location_names", names, ASSET_CACHE_S)
    return names


def _match_locations(text: str, names: Sequence[str]) -> list[str]:
    lowered = text.lower()
    exact = [name for name in names if name.lower() == lowered]
    return exact or [name for name in names if lowered in name.lower()]


def resolve_location(value: Any, visible: Sequence[str] = LOCATIONS) -> str | None:
    """Canonical name of a location the user can see (case-insensitive; a unique partial name is
    accepted). A plant location outside `visible` is refused as outside the user's access."""
    text = re.sub(r"\s+", " ", str(value or "")).strip()
    if not text:
        return None
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


def _clean_label(value: Any, limit: int = LABEL_MAX) -> str:
    text = re.sub(r"\s+", " ", _CONTROL.sub(" ", str(value or ""))).strip()
    return text if len(text) <= limit else text[: limit - 1].rstrip() + "…"


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
        return "New simulation scenario" if nav.get("section") == "new" else "Simulation jobs"
    if page == "asset-overview" and unit:
        section = {"status": " (status)", "parameters": " (parameters)", "spc": " (SPC)"}.get(str(nav.get("section")), "")
        return f"{unit} equipment details{section}"
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
) -> dict[str, Any]:
    """Open a page of the application for the user (the browser follows at once). Only when the user asks
    to open, show, go to, take them to or walk to something, or where something is. Once per answer.

    Args:
      page: The page to open.
      label: Optional short label of what opens (generated when empty).
      asset: digital-twin / asset-overview: equipment alias, e.g. "CT_01".
      parameter: digital-twin with asset: parameter alias of that equipment, e.g. "fan_speed" (opens its instrument).
      location: digital-twin: location name, e.g. "Cooling Tower Yard".
      tab: ai-insights: recommendations, faults, anomalies, documents, rules or injection.
      item_id: ai-insights: recommendation / fault / anomaly id (with its tab); simulation: job id. 0 = none.
      view: digital-twin: "walk" (first-person walk mode) or "orbit" (leave walk mode); empty keeps walk mode while the user walks.
      panel: digital-twin: "plant-overview" (BMS chiller plant overview).
      section: simulation: "new" (scenario builder); asset-overview with asset: status, parameters or spc.
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
        if page_key == "simulation":
            nav["item_id"] = _item_id(item_id)
            nav["section"] = _enum(section, SECTIONS["simulation"], _SECTION_ALIASES, "section")
            if nav["item_id"] is not None and nav["section"] is not None:
                ignored.append("section")  # a job opens its detail, not the scenario builder
                nav["section"] = None
        if page_key == "asset-overview":
            nav["section"] = _enum(section, SECTIONS["asset-overview"], {}, "section")

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
        text = text or default_label(nav, unit.get("name") if unit else None, param.get("name") if param else None)
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
