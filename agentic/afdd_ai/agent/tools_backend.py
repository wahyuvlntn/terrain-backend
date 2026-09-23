"""Backend API tools of the insight agent (DESIGN 6.3).

All tools run as the end user (`backend_api.Api`: the user's session token from
`temp:` state and `X-AFDD-Origin: agent`), so the backend applies the user's
permissions and location scope. Results are compact JSON for the model: times
in plant-local ISO with offset, rounded numbers, bounded lists. Failures come
back as {"error": "..."} with a message the model can relay.

Equipment is addressed by alias (CH_01, CT_02, CHWP_01, CWP_03, CHWS_01, MAU_04,
…); names and forms like "ch-1" are resolved too.
"""

from __future__ import annotations

import math
from collections import Counter
from collections.abc import Mapping
from datetime import timedelta
from typing import Any

from google.adk.tools import ToolContext

from . import charts
from .backend_api import Api, ToolError, compact, error_result, iso_utc, local_ts, now_utc, rnd
from .rag_tool import renumber_instruction

PLANT_SYSTEM = "CHWS_01"
# Plant KPIs reported by get_dashboard_overview (CHWS_01 parameters, when present).
PLANT_KPIS = (
    "cooling_load",
    "plant_kw",
    "plant_eff",
    "chillers_running",
    "chws_temp",
    "chwr_temp",
    "delta_t",
    "chws_sp",
    "riser_dp",
    "riser_dp_sp",
    "bypass_flow",
    "cw_supply_temp",
    "cwst_sp",
    "approach_sp",
    "oa_temp",
    "wet_bulb",
)
KW_PER_RT = 3.517

MO_STATUS = {"REQUESTED": 1, "REJECTED": 2, "OPEN": 3, "IN_PROGRESS": 4, "DONE": 5, "CLOSED": 6, "PAUSED": 7}
MO_TYPE = {"CORRECTIVE": 1, "PREVENTIVE": 2}
NOT_HEALTHY_STATUS = {"UNPLANNED DOWN", "PLANNED DOWN", "OFFLINE"}
NOT_HEALTHY_CONDITION = {"WARNING", "CRITICAL", "OFFLINE"}


def _clamp(value: Any, lo: int, hi: int, default: int) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError):
        return default
    return max(lo, min(v, hi))


def _ago(hours: float) -> str:
    return iso_utc(now_utc() - timedelta(hours=hours))


def _ahead(hours: float) -> str:
    return iso_utc(now_utc() + timedelta(hours=hours))


def _fmt_value(param: Mapping[str, Any]) -> Any:
    """Live value rounded to the parameter's display decimals; mode options as labels."""
    value = param.get("value")
    control = param.get("control") or {}
    options = control.get("options") if isinstance(control, Mapping) else None
    if options and value is not None:
        for opt in options:
            if isinstance(opt, Mapping) and opt.get("value") == value:
                return opt.get("label")
    if isinstance(value, float):
        if not math.isfinite(value):
            return None
        scale = param.get("scale")
        return round(value, int(scale)) if isinstance(scale, int) and 0 <= scale <= 6 else rnd(value)
    return value


def _band(band: Any) -> str | None:
    if not isinstance(band, Mapping):
        return None
    lo, hi = band.get("lower"), band.get("upper")
    if lo is None and hi is None:
        return None
    return f"{'' if lo is None else rnd(lo)}..{'' if hi is None else rnd(hi)}"


def _asset_row(a: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "alias": a.get("alias"),
        "name": a.get("name"),
        "type": a.get("type_name"),
        "model": a.get("model_name"),
        "status": a.get("status"),
        "status_since": local_ts(a.get("status_start_time")),
        "condition": a.get("condition"),
        "condition_since": local_ts(a.get("condition_start_time")),
    }


def _asset_filter(units: list[Mapping[str, Any]]) -> dict[str, Any]:
    """JSON `filter` of the maintenance pages: specific equipment, or everything visible."""
    if not units:
        return {}
    return {"locationIds": [], "assetIds": [int(u["id"]) for u in units]}


async def _visible_ids(api: Api, units: list[Mapping[str, Any]]) -> list[int]:
    if units:
        return [int(u["id"]) for u in units]
    return [int(a["id"]) for a in await api.assets() if a.get("id") is not None]


# ---------------------------------------------------------------------------
# Overview and equipment
# ---------------------------------------------------------------------------


async def get_dashboard_overview(tool_context: ToolContext) -> dict[str, Any]:
    """Plant-wide snapshot: equipment status / condition counts, units needing attention,
    chiller plant KPIs (cooling load, plant kW, kW/RT, CHW temperatures, riser dP, weather),
    alarm counts of the last 24 h and, when available, the AI insights summary.
    Use it first for "how is the plant doing" questions."""
    try:
        api = Api(tool_context.state)
        assets = await api.assets()
        out: dict[str, Any] = {
            "equipment": len(assets),
            "by_status": dict(Counter(str(a.get("status") or "UNKNOWN") for a in assets)),
            "by_condition": dict(Counter(str(a.get("condition") or "UNKNOWN") for a in assets)),
            "by_type": dict(Counter(str(a.get("type_name") or "Unknown") for a in assets)),
            "attention": [
                _asset_row(a)
                for a in assets
                if (a.get("status") in NOT_HEALTHY_STATUS) or (a.get("condition") in NOT_HEALTHY_CONDITION)
            ][:30],
        }
        if any((a.get("alias") or "").upper() == PLANT_SYSTEM for a in assets):
            try:
                live = await api.call("digital_twin/getliveoverview", {"aliases": [PLANT_SYSTEM]}, what="the chiller plant KPIs")
                params = (live[0].get("params") if live else None) or {}
                plant = {k: rnd((params.get(k) or {}).get("value")) for k in PLANT_KPIS if k in params}
                units = {k: (params.get(k) or {}).get("unit") for k in plant}
                if isinstance(plant.get("cooling_load"), (int, float)):
                    plant["cooling_load_rt"] = round(plant["cooling_load"] / KW_PER_RT, 1)
                out["plant"] = {"values": plant, "units": units, "as_of": local_ts((params.get("plant_kw") or {}).get("timestamp"))}
            except ToolError as err:
                out["plant"] = {"error": str(err)}
        ids = [int(a["id"]) for a in assets if a.get("id") is not None]
        if ids:
            try:
                out["alarms_24h"] = await api.call(
                    "alarm/getalarmcount", {"asset_ids": ids, "start_ts": _ago(24)}, what="the alarm counts"
                )
            except ToolError as err:
                out["alarms_24h"] = {"error": str(err)}
        if api.can("aiinsights.read"):
            try:
                ai = await api.call("ai_insights/getoverview", {}, what="the AI insights overview")
                out["ai_insights"] = compact(ai, max_items=5, max_str=300)
            except ToolError as err:
                out["ai_insights"] = {"error": str(err)}
        return out
    except Exception as err:  # noqa: BLE001 - reported to the model
        return error_result(err)


async def list_assets(tool_context: ToolContext, asset_type: str = "", search: str = "", status: str = "", condition: str = "") -> dict[str, Any]:
    """List the equipment the user can see, with alias, type, model, live status and condition.

    Args:
      asset_type: Optional type filter (substring), e.g. "Chiller", "Cooling Tower", "Pump", "Make-up Air".
      search: Optional text matched against alias and name.
      status: Optional status filter: RUNNING, IDLE, UNPLANNED DOWN, PLANNED DOWN, OFFLINE.
      condition: Optional condition filter: NORMAL, WARNING, CRITICAL, OFFLINE.
    """
    try:
        api = Api(tool_context.state)
        rows = await api.assets()
        t, q = asset_type.strip().lower(), search.strip().lower()
        st_f, cond_f = status.strip().upper(), condition.strip().upper()
        out = []
        for a in rows:
            if t and t not in (a.get("type_name") or "").lower():
                continue
            if q and q not in f"{a.get('alias') or ''} {a.get('name') or ''}".lower():
                continue
            if st_f and (a.get("status") or "") != st_f:
                continue
            if cond_f and (a.get("condition") or "") != cond_f:
                continue
            out.append(_asset_row(a))
        return {"count": len(out), "assets": out[:80]}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_asset_summary(tool_context: ToolContext, asset: str, hours: int = 24) -> dict[str, Any]:
    """Equipment summary: status and condition (with since-times), uptime over the window and 7 days,
    running hours, alarm counts, the latest alarms and maintenance orders, and the status timeline.

    Args:
      asset: Equipment alias, e.g. "CH_02".
      hours: Window for uptime, timeline and alarm counts (1-168, default 24).
    """
    try:
        api = Api(tool_context.state)
        unit = await api.resolve_asset(asset)
        h = _clamp(hours, 1, 168, 24)
        s = await api.call("digital_twin/getassetsummary", {"asset_id": unit["id"], "hours": h}, what=f"the summary of {unit.get('alias')}")
        s = s or {}

        def intervals(rows: Any) -> list[dict[str, Any]]:
            return [{"value": r.get("value"), "start": local_ts(r.get("start")), "end": local_ts(r.get("end"))} for r in (rows or [])[-15:]]

        return {
            "alias": s.get("alias") or unit.get("alias"),
            "name": s.get("name"),
            "type": s.get("type_name"),
            "model": s.get("model_name"),
            "location": s.get("location"),
            "status": s.get("status"),
            "status_since": local_ts(s.get("status_start_time")),
            "condition": s.get("condition"),
            "condition_since": local_ts(s.get("condition_start_time")),
            "window_hours": h,
            "uptime_window": compact(s.get("uptime_window")),
            "uptime_7d": compact(s.get("uptime_7d")),
            "running_hours_lifetime": rnd(s.get("running_hours"), 1),
            "alarm_counts_window": {"warning": s.get("warning_counts"), "critical": s.get("critical_counts")},
            "status_timeline": intervals(s.get("statuses")),
            "condition_timeline": intervals(s.get("conditions")),
            "recent_alarms": [
                {
                    "name": a.get("name"),
                    "severity": a.get("severity"),
                    "type": a.get("type"),
                    "message": a.get("message"),
                    "start": local_ts(a.get("start")),
                    "end": local_ts(a.get("end")),
                    "active": a.get("active"),
                }
                for a in (s.get("alarms") or [])[:10]
            ],
            "maintenance": [
                {
                    "id": m.get("id"),
                    "order": m.get("name"),
                    "type": m.get("type"),
                    "status": m.get("status"),
                    "issue": m.get("issue"),
                    "planned_start": local_ts(m.get("planned_start")),
                }
                for m in (s.get("maintenances") or [])[:6]
            ],
        }
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_asset_parameters(tool_context: ToolContext, asset: str) -> dict[str, Any]:
    """Live values of every displayed parameter of one equipment, with unit, condition, warning and
    critical limits; writable set points and modes are flagged (read only for you: never change them).

    Args:
      asset: Equipment alias, e.g. "CH_01", "CT_03", "CHWS_01".
    """
    try:
        api = Api(tool_context.state)
        unit = await api.resolve_asset(asset)
        params = await api.parameters(unit)
        rows = []
        latest = None
        for p in params:
            row: dict[str, Any] = {
                "alias": p.get("alias"),
                "name": p.get("name"),
                "value": _fmt_value(p),
                "unit": p.get("unit") or None,
                "condition": p.get("condition"),
            }
            if (w := _band(p.get("warning"))) is not None:
                row["warning"] = w
            if (c := _band(p.get("critical"))) is not None:
                row["critical"] = c
            if p.get("control"):
                row["writable"] = True
            rows.append(row)
            ts = p.get("timestamp")
            if ts and (latest is None or ts > latest):
                latest = ts
        return {"asset": unit.get("alias"), "name": unit.get("name"), "status": unit.get("status"), "as_of": local_ts(latest), "parameters": rows}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_parameter_trend(tool_context: ToolContext, asset: str, parameter: str, hours: int = 4) -> dict[str, Any]:
    """Trend and SPC of one parameter over the last hours, shown to the user as a chart.
    Returns statistics (count, mean, std dev, min, max, Cp/Cpk), control limits (UCL / LCL / centre line;
    computed from running samples when not configured), recent SPC rule violations and the latest values.

    Args:
      asset: Equipment alias, e.g. "CT_01".
      parameter: Parameter alias, e.g. "cwst", "approach", "power_kw".
      hours: Window length in hours (1-168, default 4).
    """
    try:
        api = Api(tool_context.state)
        unit = await api.resolve_asset(asset)
        p = await api.resolve_parameter(unit, parameter)
        h = _clamp(hours, 1, 168, 4)
        trend = await api.trend(int(p["id"]), h * 60, f"the trend of {unit.get('alias')} {p.get('alias')}")
        pts = charts.trend_points(trend)
        title = f"{unit.get('alias')} {p.get('name') or p.get('alias')} (last {h} h)"
        option = charts.time_series_option(
            [{"name": f"{unit.get('alias')} {p.get('alias')}", "unit": p.get("unit") or "", "points": pts}],
            thresholds=charts.limit_lines(trend),
        )
        violations = trend.get("violations") or []
        return {
            "asset": unit.get("alias"),
            "parameter": p.get("alias"),
            "name": p.get("name"),
            "unit": p.get("unit"),
            "hours": h,
            "points": len(pts),
            "stats": compact(trend.get("stats")),
            "spc": compact(trend.get("spc")),
            "warning": _band(trend.get("warning")),
            "critical": _band(trend.get("critical")),
            "violations_total": len(violations),
            "recent_violations": [
                {"time": local_ts(v.get("timestamp")), "value": rnd(v.get("value")), "rules": v.get("rules")} for v in violations[-5:]
            ],
            "latest": [[local_ts(t), v] for t, v in pts[-6:]],
            "chart": charts.chart_payload(title, option),
        }
    except Exception as err:  # noqa: BLE001
        return error_result(err)


# ---------------------------------------------------------------------------
# Alarms, maintenance, early warnings, reliability
# ---------------------------------------------------------------------------


async def get_alarm_events(
    tool_context: ToolContext,
    assets: list[str] | None = None,
    hours: int = 24,
    alarm_type: str = "all",
    search: str = "",
    limit: int = 50,
) -> dict[str, Any]:
    """Alarm and abnormal-behaviour events (newest first) with counts by severity.

    Args:
      assets: Optional equipment aliases; empty = every visible unit.
      hours: Look-back window in hours (1-2160, default 24).
      alarm_type: "alarm" (threshold / rule alarms), "abnormal" (abnormal behaviour) or "all".
      search: Optional text matched against equipment name, alarm name, message and severity.
      limit: Maximum events returned (1-200, default 50).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        ids = await _visible_ids(api, units)
        if not ids:
            return {"total": 0, "events": [], "note": "No visible equipment."}
        type_id = {"alarm": 1, "abnormal": 2}.get(alarm_type.strip().lower(), 0)
        h = _clamp(hours, 1, 2160, 24)
        body: dict[str, Any] = {"asset_ids": ids, "start_ts": _ago(h), "search_filter": search.strip()}
        if type_id:
            body["alarm_type_id"] = type_id
        counts = await api.call("alarm/getalarmcount", body, what="the alarm counts")
        lim = _clamp(limit, 1, 200, 50)
        logs = await api.call("alarm/getalarmbyassetid", {**body, "page": 1, "limit": lim}, what="the alarm events")
        events = [
            {
                "time": local_ts(e.get("timestamp")),
                "equipment": e.get("assetName"),
                "type": e.get("alarmType"),
                "severity": e.get("alarmStatus"),
                "message": e.get("alarmMessage"),
            }
            for e in ((logs or {}).get("data") or [])
        ]
        return {"window_hours": h, "counts": counts, "total": (logs or {}).get("total"), "events": events}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_maintenance_orders(
    tool_context: ToolContext,
    assets: list[str] | None = None,
    status: str = "",
    order_type: str = "",
    days_back: int = 30,
    days_ahead: int = 30,
    search: str = "",
    limit: int = 30,
) -> dict[str, Any]:
    """Maintenance (work) orders planned in a window, newest first, with counts by status.

    Args:
      assets: Optional equipment aliases; empty = every visible unit.
      status: Optional comma list of REQUESTED, REJECTED, OPEN, IN_PROGRESS, DONE, CLOSED, PAUSED.
      order_type: Optional "corrective" or "preventive".
      days_back: Days before now to include (0-365, default 30).
      days_ahead: Days after now to include (0-365, default 30).
      search: Optional text matched against order name (e.g. CM-10-26-001), equipment and description.
      limit: Maximum orders returned (1-100, default 30).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        filt = _asset_filter(units)
        if status.strip():
            ids = []
            for s in status.split(","):
                key = s.strip().upper().replace(" ", "_")
                if key not in MO_STATUS:
                    raise ToolError(f"Unknown order status '{s.strip()}'. Use {', '.join(MO_STATUS)}.")
                ids.append(MO_STATUS[key])
            filt["maintenanceStatusIds"] = ids
        if order_type.strip():
            key = order_type.strip().upper()
            if key not in MO_TYPE:
                raise ToolError("order_type must be 'corrective' or 'preventive'.")
            filt["maintenanceTypeIds"] = [MO_TYPE[key]]
        body = {
            "filter": filt,
            "search": search.strip(),
            "start_time": _ago(24 * _clamp(days_back, 0, 365, 30)),
            "end_time": _ahead(24 * _clamp(days_ahead, 0, 365, 30)),
        }
        rows = await api.call("maintenance_order/get_maintenance_export_data", body, what="the maintenance orders") or []
        rows = list(reversed(rows))
        lim = _clamp(limit, 1, 100, 30)
        orders = [
            {
                "order": r.get("order_id"),
                "type": r.get("maintenance_type"),
                "status": r.get("status"),
                "severity": r.get("severity"),
                "priority": r.get("priority"),
                "equipment": r.get("equipment"),
                "issue": r.get("issue"),
                "planned_start": local_ts(r.get("planned_start")),
                "planned_end": local_ts(r.get("planned_end")),
                "technician": r.get("technician") or None,
                "requested_by": r.get("requested_by"),
            }
            for r in rows[:lim]
        ]
        return {"total": len(rows), "by_status": dict(Counter(str(r.get("status")) for r in rows)), "orders": orders}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_maintenance_order_detail(tool_context: ToolContext, order: str) -> dict[str, Any]:
    """Full detail of one maintenance order: description, issue codes, people, planned / actual times
    and the progress history (approvals, start, pauses, work notes, completion).

    Args:
      order: Order name such as "CM-10-26-004" or "PM-10-26-012", or its numeric id.
    """
    try:
        api = Api(tool_context.state)
        ref = (order or "").strip()
        if not ref:
            raise ToolError("Give the order name (e.g. CM-10-26-004) or id.")
        row: dict[str, Any] | None = None
        if ref.isdigit():
            order_id = int(ref)
        else:
            listing = await api.call("maintenance_order/get_maintenance_order_list", {"filter": {}, "search": ref}, what=f"order {ref}")
            match = [r for r in ((listing or {}).get("data") or []) if (r.get("name") or "").upper() == ref.upper()]
            if not match:
                raise ToolError(f"No visible maintenance order is named '{ref}'.")
            row = match[0]
            order_id = int(match[0]["id"])
        detail = await api.call("maintenance_order/get_maintenance_order_detail", {"order_id": order_id}, what=f"order {ref}")
        if not detail:
            raise ToolError(f"Maintenance order '{ref}' was not found.")
        d = detail[0]
        progress = await api.call("maintenance_order/get_progress", {"order_id": order_id}, what=f"the history of order {ref}") or []
        name = (row or {}).get("name") or ref
        export = await api.call("maintenance_order/get_maintenance_export_data", {"filter": {}, "search": name}, what=f"order {ref}") or []
        ex = next((r for r in export if (r.get("order_id") or "").upper() == str(name).upper()), {})
        return {
            "id": order_id,
            "order": ex.get("order_id") or name,
            "type": ex.get("maintenance_type"),
            "status": ex.get("status") or d.get("status"),
            "severity": ex.get("severity"),
            "priority": ex.get("priority"),
            "equipment": ex.get("equipment"),
            "location": " > ".join(str(x) for x in (ex.get("building"), ex.get("floor"), ex.get("room")) if x),
            "issue": ex.get("issue"),
            "description": compact(d.get("description"), max_str=1500),
            "requested_by": d.get("requested_by"),
            "approved_by": d.get("approved_by") or None,
            "technicians": ex.get("technician") or None,
            "created_at": local_ts(d.get("created_at")),
            "planned_start": local_ts(d.get("planned_start")),
            "planned_end": local_ts(d.get("planned_end")),
            "estimated_time_s": d.get("estimated_time"),
            "actual_time_s": d.get("actual_time"),
            "reject_reason": d.get("reject_reason") or None,
            "from_early_warning": d.get("issue_relation_id") is not None,
            "history": [
                {"time": local_ts(p.get("datetime")), "status": p.get("status"), "details": compact(p.get("details"), max_items=10, max_str=400)}
                for p in progress[:20]
            ],
        }
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_early_warnings(tool_context: ToolContext, assets: list[str] | None = None, days: int = 30, search: str = "", limit: int = 30) -> dict[str, Any]:
    """AFDD early warnings / diagnoses: system-raised corrective orders with the detected issue, other
    possible issues, diagnostic and repair results, related parameters, severity and status.

    Args:
      assets: Optional equipment aliases; empty = every visible unit.
      days: Look-back window in days (1-365, default 30).
      search: Optional text filter.
      limit: Maximum rows (1-100, default 30).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        body = {"filter": _asset_filter(units), "search": search.strip(), "start_time": _ago(24 * _clamp(days, 1, 365, 30)), "end_time": _ahead(24)}
        rows = await api.call("early_warning_diagnostics/export_early_warning_diagnostics", body, what="the early warnings") or []
        rows = list(reversed(rows))
        lim = _clamp(limit, 1, 100, 30)
        items = [
            {
                "order": r.get("order_id"),
                "equipment": r.get("equipment"),
                "detected_issue": r.get("detected_issue"),
                "other_possible_issue": r.get("other_possible_issue"),
                "diagnostic_result": r.get("diagnostic_result"),
                "repair_result": r.get("repair_result"),
                "related_parameter": r.get("related_parameter"),
                "severity": r.get("severity"),
                "status": r.get("status"),
                "planned_start": local_ts(r.get("planned_start")),
                "technician": r.get("technician"),
            }
            for r in rows[:lim]
        ]
        return {"total": len(rows), "early_warnings": items}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_reliability_kpis(tool_context: ToolContext, assets: list[str] | None = None, days: int = 90) -> dict[str, Any]:
    """Reliability KPIs: average MTBF and MTTR (hours) over the window, and per-equipment MTBF / MTTR
    over the trailing year (MTBF = running hours / corrective orders; MTTR = mean repair time).

    Args:
      assets: Optional equipment aliases; empty = every visible unit.
      days: Window for the averages in days (1-730, default 90).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        scope: dict[str, Any] = {"location": [], "asset": [int(u["id"]) for u in units]} if units else {}
        window = {**scope, "start": _ago(24 * _clamp(days, 1, 730, 90))}
        mtbf = await api.call("maintenance_performance/get_mtbf_average", window, what="the MTBF")
        mttr = await api.call("maintenance_performance/get_mttr_average", window, what="the MTTR")
        paged = await api.call("maintenance_performance/get_paginated_mtbf_mttr", {**scope, "page": 1, "limit": 60}, what="the MTBF / MTTR per unit")

        def first(rows: Any, key: str) -> Any:
            return rnd((rows or [{}])[0].get(key), 1) if rows else None

        per_unit = [{"equipment": r.get("assetName"), "mtbf_h": r.get("mtbf"), "mttr_h": r.get("mttr")} for r in ((paged or {}).get("data") or [])]
        return {
            "window_days": _clamp(days, 1, 730, 90),
            "average_mtbf_h": first(mtbf, "mtbf"),
            "average_mttr_h": first(mttr, "mttr"),
            "per_unit_trailing_year": per_unit,
            "note": "'-' means no corrective failures (MTBF) or repairs (MTTR) in the trailing year.",
        }
    except Exception as err:  # noqa: BLE001
        return error_result(err)


# ---------------------------------------------------------------------------
# AI insights
# ---------------------------------------------------------------------------


def _rec_row(r: Mapping[str, Any]) -> dict[str, Any]:
    cur = r.get("current") or {}
    rec = r.get("recommended") or {}
    return {
        "id": r.get("id"),
        "status": r.get("status"),
        "created_at": local_ts(r.get("created_at")),
        "valid_until": local_ts(r.get("valid_until")),
        "savings_kw": rnd(r.get("savings_kw"), 1),
        "savings_pct": rnd(r.get("savings_pct"), 2),
        "conditions": compact(r.get("conditions"), max_items=20),
        "current_setpoints": compact(cur.get("setpoints") if isinstance(cur, Mapping) else None),
        "recommended_setpoints": compact(rec.get("setpoints") if isinstance(rec, Mapping) else None),
        "predicted_current": compact(cur.get("predicted") if isinstance(cur, Mapping) else None),
        "predicted_recommended": compact(rec.get("predicted") if isinstance(rec, Mapping) else None),
        "actions": compact(r.get("actions"), max_items=12),
        "constraints": compact(r.get("constraints"), max_items=12),
        "validation_job": compact(r.get("validation_job")),
        "validation": compact(r.get("validation"), max_items=10, max_str=300),
        "rationale": compact(r.get("rationale"), max_str=800),
    }


async def get_recommendations(tool_context: ToolContext, status: str = "", limit: int = 5) -> dict[str, Any]:
    """Set-point optimisation recommendations (newest first): current vs recommended supervisory set points,
    predicted plant kW and savings, digital-twin validation result and status
    (PROPOSED, VALIDATING, VALIDATED, REJECTED, APPLIED, DISMISSED, EXPIRED, FAILED).

    Args:
      status: Optional status filter, e.g. "VALIDATED".
      limit: Maximum recommendations (1-20, default 5).
    """
    try:
        api = Api(tool_context.state)
        body: dict[str, Any] = {"limit": _clamp(limit, 1, 20, 5)}
        if status.strip():
            body["status"] = status.strip().upper()
        rows = await api.call("ai_insights/getrecommendations", body, what="the optimisation recommendations") or []
        return {"count": len(rows), "recommendations": [_rec_row(r) for r in rows if isinstance(r, Mapping)]}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_faults(
    tool_context: ToolContext,
    status: str = "ACTIVE",
    assets: list[str] | None = None,
    rule_codes: list[str] | None = None,
    days: int = 7,
    limit: int = 20,
) -> dict[str, Any]:
    """Faults found by the LBNL fault-detection rules (sensor bias, tower fouling, 3-way valve leak / stuck,
    riser dP bias, CW loop hunting) with rule code, equipment, severity, intensity estimate and status.
    For the evidence and the maintenance instruction of one fault use call_app_api("ai_insights/getfault", {"id": ...}).

    Args:
      status: "ACTIVE", "CLEARED" or "" for both (default ACTIVE).
      assets: Optional equipment aliases.
      rule_codes: Optional rule / alarm codes, e.g. ["CT-014", "CHS-006"].
      days: Look-back window in days for faults that started (1-365, default 7).
      limit: Maximum rows (1-100, default 20).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        body: dict[str, Any] = {"limit": _clamp(limit, 1, 100, 20), "from": _ago(24 * _clamp(days, 1, 365, 7))}
        if status.strip():
            body["status"] = status.strip().upper()
        if units:
            body["asset_ids"] = [int(u["id"]) for u in units]
        if rule_codes:
            body["rule_codes"] = [c.strip().upper() for c in rule_codes if c and c.strip()]
        rows = await api.call("ai_insights/getfaults", body, what="the detected faults") or []
        faults = [
            {
                "id": r.get("id"),
                "rule": r.get("rule_code"),
                "equipment": r.get("asset_alias") or r.get("asset_name"),
                "severity": r.get("severity"),
                "status": r.get("status"),
                "message": r.get("message"),
                "intensity_estimate": rnd(r.get("intensity_estimate")),
                "started_at": local_ts(r.get("started_at")),
                "last_seen_at": local_ts(r.get("last_seen_at")),
                "ended_at": local_ts(r.get("ended_at")),
                "instruction_status": r.get("instruction_status"),
            }
            for r in rows
            if isinstance(r, Mapping)
        ]
        return {"count": len(faults), "faults": faults}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def get_anomalies(
    tool_context: ToolContext,
    status: str = "ACTIVE",
    assets: list[str] | None = None,
    detectors: list[str] | None = None,
    hours: int = 24,
    limit: int = 30,
) -> dict[str, Any]:
    """Statistical sensor anomalies (detectors: range, flatline, zscore, ewma, spc) with equipment,
    parameter, severity, value vs expected, score and status.

    Args:
      status: "ACTIVE", "CLEARED" or "" for both (default ACTIVE).
      assets: Optional equipment aliases.
      detectors: Optional detector names.
      hours: Look-back window in hours for anomalies that started (1-2160, default 24).
      limit: Maximum rows (1-200, default 30).
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        body: dict[str, Any] = {"limit": _clamp(limit, 1, 200, 30), "from": _ago(_clamp(hours, 1, 2160, 24))}
        if status.strip():
            body["status"] = status.strip().upper()
        if units:
            body["asset_ids"] = [int(u["id"]) for u in units]
        if detectors:
            body["detectors"] = [d.strip().lower() for d in detectors if d and d.strip()]
        rows = await api.call("ai_insights/getanomalies", body, what="the anomalies") or []
        items = [
            {
                "id": r.get("id"),
                "equipment": r.get("asset_alias") or r.get("asset_name"),
                "parameter": r.get("param_alias"),
                "detector": r.get("detector"),
                "severity": r.get("severity"),
                "status": r.get("status"),
                "value": rnd(r.get("value")),
                "expected": rnd(r.get("expected")),
                "score": rnd(r.get("score"), 2),
                "message": r.get("message"),
                "started_at": local_ts(r.get("started_at")),
                "last_seen_at": local_ts(r.get("last_seen_at")),
            }
            for r in rows
            if isinstance(r, Mapping)
        ]
        return {"count": len(items), "anomalies": items}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


# ---------------------------------------------------------------------------
# Generic read-only access
# ---------------------------------------------------------------------------

INSTRUCTION_MAX_STR = 8000  # fault instructions (instruction_md) are kept whole; other strings are cut at 1,500

# Parameter kinds: int, str, bool, time (ISO 8601), ids (list of ints), strs (list of strings), json (object).
ALLOWED_SCRIPTS: dict[str, dict[str, str]] = {
    "digital_twin/getliveoverview": {"aliases": "strs"},
    "digital_twin/getcontrolrequests": {"asset_id": "int", "limit": "int"},
    "digital_twin/getactivefaults": {},
    "asset/getassetdetails": {"asset_id": "int", "date": "str"},
    "asset/getassetaspectinfo": {"asset_id": "int"},
    "alarm/getalarmcountperday": {"asset_ids": "ids", "start_ts": "time", "end_ts": "time", "period": "str"},
    "alarm/getmostoccuralarmperasset": {"asset_ids": "ids", "start_ts": "time", "end_ts": "time"},
    "early_warning_dashboard/get_error_code_occurences": {"asset_ids": "ids", "location_ids": "ids", "start_date": "time", "end_date": "time"},
    "early_warning_dashboard/get_parameter_warning_occurences": {"asset_ids": "ids", "location_ids": "ids", "start_date": "time", "end_date": "time"},
    "early_warning_diagnostics/get_instruction_list": {"maintenance_id": "int"},
    "early_warning_diagnostics/get_parameter_message": {"maintenance_id": "int"},
    "maintenance_instruction/get_instructions": {"model_ids": "ids", "asset_ids": "ids", "search": "str", "page": "int"},
    "maintenance_instruction/get_instruction_detail": {"id": "int"},
    "maintenance_performance/get_maintenance_order_overview": {"group_by": "str", "start": "time", "end": "time"},
    "maintenance_performance/get_top_frequent_issues": {"start": "time", "end": "time"},
    "location_overview/getalllocations": {},
    "location_overview/getlocationapq": {"location_id": "int", "start": "time", "end": "time"},
    "ai_insights/getoverview": {},
    "ai_insights/getrecommendation": {"id": "int"},
    "ai_insights/getfault": {"id": "int"},
    "ai_insights/getrules": {},
    "ai_insights/getpointmap": {},
    "ai_documents/list": {"doc_type": "str", "status": "str", "asset_type": "str", "unit": "str", "search": "str", "limit": "int", "offset": "int"},
    "ai_documents/get": {"id": "int"},
    "simulation/listjobs": {"status": "str", "origin": "str", "limit": "int", "offset": "int"},
    "simulation/getjob": {"job_id": "int"},
    "simulation/getevents": {"job_id": "int", "variant": "str", "kinds": "str", "asset": "str", "limit": "int", "offset": "int"},
    "simulation/getfaultcatalog": {},
    "production/getoverview": {"hours": "int"},
    "production/gettools": {"hours": "int"},
    "production/gettrend": {"days": "int", "bucket": "str", "tool_type": "str", "equipment_id": "int"},
    "production/gettimeline": {"hours": "int", "equipment_ids": "str"},
    "production/getlots": {"status": "str", "search": "str", "product_code": "str", "area": "str", "page": "int", "limit": "int"},
    "production/getlot": {"lot_code": "str"},
    "production/getqualitykeys": {"hours": "int"},
    "production/getquality": {"char_key": "str", "hours": "int", "equipment_id": "int"},
    "production/getplanning": {},
}


def _coerce(kind: str, name: str, value: Any) -> Any:
    try:
        if kind == "int":
            if isinstance(value, bool):
                raise ValueError
            return int(value)
        if kind == "str":
            return str(value)[:500]
        if kind == "bool":
            return bool(value)
        if kind == "time":
            text = str(value).strip()
            if not text or len(text) > 40:
                raise ValueError
            return text
        if kind == "ids":
            items = value if isinstance(value, list) else str(value).split(",")
            return [int(v) for v in items][:500]
        if kind == "strs":
            items = value if isinstance(value, list) else str(value).split(",")
            return [str(v).strip()[:80] for v in items if str(v).strip()][:60]
    except (TypeError, ValueError):
        raise ToolError(f"Parameter '{name}' must be of type {kind}.") from None
    raise ToolError(f"Parameter '{name}' has an unsupported type.")


async def call_app_api(tool_context: ToolContext, script: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    """Call one read-only application API that the other tools do not cover. Only these scripts and
    parameters are accepted (ids come from other tool results; times are ISO 8601):
    - digital_twin/getliveoverview {aliases: [alias]} - live values of several units at once.
    - digital_twin/getcontrolrequests {asset_id, limit} - operator set-point / mode changes of a unit.
    - digital_twin/getactivefaults {} - faults currently injected into the simulated plant.
    - asset/getassetdetails {asset_id, date (YYYY-MM-DD)} - one day: status timeline, alarms, maintenance.
    - asset/getassetaspectinfo {asset_id} - critical / supporting aspects of a unit.
    - alarm/getalarmcountperday {asset_ids, start_ts, end_ts, period: daily|monthly} - alarm counts per day.
    - alarm/getmostoccuralarmperasset {asset_ids, start_ts, end_ts} - top 10 units by alarm count.
    - early_warning_dashboard/get_error_code_occurences and .../get_parameter_warning_occurences
      {asset_ids, location_ids, start_date, end_date} - top alarm codes / parameters (send location_ids: [] with asset_ids).
    - early_warning_diagnostics/get_instruction_list {maintenance_id} - diagnostic and repair SOPs of an order.
    - early_warning_diagnostics/get_parameter_message {maintenance_id} - alarms behind an early warning.
    - maintenance_instruction/get_instructions {model_ids, asset_ids, search, page} - SOP list (4 per page).
    - maintenance_instruction/get_instruction_detail {id} - one SOP.
    - maintenance_performance/get_maintenance_order_overview {group_by: status|priority|severity, start, end}.
    - maintenance_performance/get_top_frequent_issues {start, end}.
    - location_overview/getalllocations {} and location_overview/getlocationapq {location_id, start, end}.
    - ai_insights/getoverview {}, ai_insights/getrecommendation {id}, ai_insights/getfault {id}
      (evidence + RAG maintenance instruction), ai_insights/getrules {}, ai_insights/getpointmap {}.
    - ai_documents/list {doc_type, status, asset_type, unit, search, limit, offset}, ai_documents/get {id}.
    - simulation/listjobs {status, origin, limit, offset}, simulation/getjob {job_id},
      simulation/getevents {job_id, variant, kinds, asset, limit, offset}, simulation/getfaultcatalog {}.
    - production/getoverview {hours} - FAB1 OEE and losses, starts / outs, cycle time per product, WIP by area / layer,
      overhead transport (transfers, mean delivery minutes, OHT busy %).
    - production/gettools {hours} - each production tool: status, OEE factors, current lot, queue, throughput.
    - production/gettrend {days, bucket: day|shift, tool_type, equipment_id} - daily OEE, outs, starts, cycle time, WIP.
    - production/gettimeline {hours, equipment_ids: "1,2"} - tool status intervals and runs.
    - production/getlots {status: wip|hold|done|all, search, product_code, area, page, limit} - lots.
    - production/getlot {lot_code} - one lot: route, step history, measurements, scrap, where its FOUP is
      (location: a load port TOOL:n, a stocker STK-0n or inside a batch tool) and its overhead transfers.
    - production/getqualitykeys {hours} - measured characteristics with Cpk and fails.
    - production/getquality {char_key, hours, equipment_id} - one characteristic: SPC, Cpk by chamber, scrap, FPY.
    - production/getplanning {} - the current production plan, products and tools.

    Args:
      script: One of the scripts above.
      params: Its parameters (unknown parameters are rejected).
    """
    try:
        name = (script or "").strip().strip("/").lower()
        spec = ALLOWED_SCRIPTS.get(name)
        if spec is None:
            raise ToolError(f"'{script}' is not an allowed read-only script. Allowed: {', '.join(sorted(ALLOWED_SCRIPTS))}.")
        body: dict[str, Any] = {}
        for key, value in (params or {}).items():
            if key not in spec:
                allowed = ", ".join(spec) or "none"
                raise ToolError(f"Parameter '{key}' is not accepted by {name} (accepted: {allowed}).")
            if value is None:
                continue
            body[key] = _coerce(spec[key], key, value)
        api = Api(tool_context.state)
        data = await api.call(name, body, what=name)
        if name == "ai_insights/getfault":
            data = renumber_instruction(tool_context.state, data)
        out = compact(data, max_items=40, max_str=1500)
        if name == "ai_insights/getfault" and isinstance(data, Mapping) and isinstance(data.get("instruction_md"), str):
            # the whole maintenance instruction (up to ~5k characters), not its first 1,500
            out["instruction_md"] = compact(data["instruction_md"], max_str=INSTRUCTION_MAX_STR)
        return {"script": name, "data": out}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


BACKEND_TOOLS = [
    get_dashboard_overview,
    list_assets,
    get_asset_summary,
    get_asset_parameters,
    get_parameter_trend,
    get_alarm_events,
    get_maintenance_orders,
    get_maintenance_order_detail,
    get_early_warnings,
    get_reliability_kpis,
    get_recommendations,
    get_faults,
    get_anomalies,
    call_app_api,
]
