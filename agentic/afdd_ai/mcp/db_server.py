"""MCP server `afdd-db` (:8101): parameterised, scope-filtered, read-only plant database tools.

No free SQL: every tool is one or two fixed, parameterised statements on the
read-only role `afdd_ai_ro` (`statement_timeout` 5 s, at most 5,000 rows). Rows
are restricted to the caller's location scope (assigned locations and their
descendants; admins unrestricted). Times are plant-local ISO strings; relative
inputs such as "-24h" are accepted.

Tools: query_parameter_history, get_energy_summary, list_alarm_events,
list_anomalies, list_faults, get_point_map, search_documents,
list_simulation_jobs.
"""

from __future__ import annotations

import inspect
import logging
import math
from collections import defaultdict
from datetime import datetime, timedelta
from typing import Any

from mcp.server.mcpserver import Context, MCPServer
from mcp.server.mcpserver.exceptions import ToolError

from ..common.config import get_settings
from ..common.gemini import BudgetExceeded, GeminiNotConfigured
from .auth import McpUser, current_user, request_user_id
from .util import MAX_ROWS, clamp, jsonable, local_iso, norm_aliases, plant_tz, ro_query, window

log = logging.getLogger(__name__)

KW_PER_RT = 3.517
AGGREGATES = {
    "avg": "avg(pv.value)",
    "min": "min(pv.value)",
    "max": "max(pv.value)",
    "first": "(array_agg(pv.value ORDER BY pv.ts))[1]",
    "last": "(array_agg(pv.value ORDER BY pv.ts DESC))[1]",
}
ENERGY_BUCKETS = ("hour", "day", "week", "month")
# Plant energy points by equipment type (asset.types.name).
ENERGY_POINTS = {
    "Chilled Water System": ("plant_kw", "cooling_load"),
    "Centrifugal Chiller": ("power_kw",),
    "Chilled Water Pump": ("power_kw",),
    "Condenser Water Pump": ("power_kw",),
    "Cooling Tower": ("fan_power",),
}
# Equipment aliases referenced by a job's actions (common and per variant), as in the backend's
# simulation visibility rule: a job is visible when the user requested it or can see every unit it touches.
SIM_JOB_ALIASES = (
    "(SELECT a->>'asset' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(j.spec->'actions') = 'array'"
    " THEN j.spec->'actions' ELSE '[]'::jsonb END) a"
    " UNION ALL SELECT a->>'asset' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(j.spec->'variants') = 'array'"
    " THEN j.spec->'variants' ELSE '[]'::jsonb END) v"
    " CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(v->'actions') = 'array'"
    " THEN v->'actions' ELSE '[]'::jsonb END) a)"
)
ENERGY_GROUP = {
    "Centrifugal Chiller": "chillers_kwh",
    "Chilled Water Pump": "chw_pumps_kwh",
    "Condenser Water Pump": "cw_pumps_kwh",
    "Cooling Tower": "tower_fans_kwh",
}

mcp = MCPServer(
    "afdd-db",
    instructions=(
        "Read-only, scope-filtered access to the AFDD plant database: parameter history, energy, "
        "alarm events, anomalies, detected faults, the LBNL point map, documents and simulation jobs."
    ),
)


async def _asset(user: McpUser, alias: str) -> dict[str, Any]:
    want = (norm_aliases([alias]) or [""])[0]
    rows, _ = await ro_query(
        "SELECT id, alias, name, type_name, model_name, location_id, location_path FROM analytics.v_assets"
        " WHERE upper(alias) = %(alias)s AND (%(locs)s::bigint[] IS NULL OR location_id = ANY(%(locs)s))",
        {"alias": want, "locs": user.location_list()},
        max_rows=1,
    )
    if not rows:
        raise ToolError(f"No visible equipment with alias '{alias}'")
    return rows[0]


def _require(user: McpUser, permission: str, what: str) -> None:
    if not user.can(permission):
        raise ToolError(f"The user is not allowed to read {what} (needs {permission})")


@mcp.tool()
async def query_parameter_history(
    ctx: Context,
    asset_alias: str,
    param_aliases: list[str],
    start: str | None = None,
    end: str | None = None,
    bucket_minutes: int = 15,
    agg: str = "avg",
) -> dict[str, Any]:
    """Logged history of up to 8 parameters of one equipment, aggregated per time bucket.

    Args:
      asset_alias: Equipment alias, e.g. "CH_01", "CHWS_01".
      param_aliases: Parameter aliases, e.g. ["chwst", "power_kw"].
      start: ISO 8601 (plant time when no offset), "now" or relative ("-24h", "-7d"); default end - 24 h.
      end: Same formats; default now. The window may span at most 92 days.
      bucket_minutes: Bucket size in minutes (1-1440, default 15); enlarged automatically to stay under 5,000 points.
      agg: avg, min, max, first or last (default avg).
    """
    user = await current_user(ctx)
    if not param_aliases:
        raise ToolError("Give at least one parameter alias")
    if len(param_aliases) > 8:
        raise ToolError("At most 8 parameters per call")
    fn = AGGREGATES.get((agg or "avg").lower())
    if fn is None:
        raise ToolError(f"agg must be one of {', '.join(AGGREGATES)}")
    t0, t1 = window(start, end, default_hours=24, max_days=92)
    asset = await _asset(user, asset_alias)
    wanted = [p.strip().lower() for p in param_aliases if p and p.strip()]
    params, _ = await ro_query(
        "SELECT ep.id, mp.alias, mp.name, mp.unit FROM asset.equipment_parameters ep"
        " JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id"
        " WHERE ep.equipment_id = %(eid)s ORDER BY mp.alias",
        {"eid": asset["id"]},
    )
    by_alias = {p["alias"].lower(): p for p in params}
    missing = [p for p in wanted if p not in by_alias]
    if missing:
        raise ToolError(f"{asset['alias']} has no parameter {', '.join(missing)}; available: {', '.join(sorted(by_alias))}")
    chosen = [by_alias[p] for p in dict.fromkeys(wanted)]
    minutes = (t1 - t0).total_seconds() / 60
    bucket = clamp(bucket_minutes, 1, 1440, 15)
    needed = math.ceil(minutes * len(chosen) / MAX_ROWS)
    adjusted = needed > bucket
    bucket = max(bucket, needed)
    origin = datetime(2000, 1, 1, tzinfo=plant_tz())
    rows, truncated = await ro_query(
        f"SELECT pv.equipment_parameter_id AS pid, date_bin(make_interval(mins => %(b)s), pv.ts, %(origin)s) AS bucket, {fn} AS v"
        " FROM ts.parameter_values pv"
        " WHERE pv.equipment_parameter_id = ANY(%(pids)s) AND pv.ts >= %(t0)s AND pv.ts < %(t1)s"
        " GROUP BY 1, 2 ORDER BY 2, 1",
        {"b": bucket, "origin": origin, "pids": [p["id"] for p in chosen], "t0": t0, "t1": t1},
    )
    series: dict[str, dict[str, Any]] = {p["alias"]: {"name": p["name"], "unit": p["unit"], "points": []} for p in chosen}
    alias_of = {p["id"]: p["alias"] for p in chosen}
    for r in rows:
        series[alias_of[r["pid"]]]["points"].append([local_iso(r["bucket"]), jsonable(r["v"])])
    out: dict[str, Any] = {
        "asset": asset["alias"],
        "start": local_iso(t0),
        "end": local_iso(t1),
        "bucket_minutes": bucket,
        "agg": agg.lower(),
        "series": series,
    }
    if adjusted:
        out["note"] = f"bucket enlarged to {bucket} min to stay under {MAX_ROWS} points"
    if truncated:
        out["truncated"] = True
    return out


def _bucket_end(start: datetime, unit: str) -> datetime:
    if unit == "hour":
        return start + timedelta(hours=1)
    if unit == "day":
        return start + timedelta(days=1)
    if unit == "week":
        return start + timedelta(days=7)
    return start.replace(year=start.year + 1, month=1) if start.month == 12 else start.replace(month=start.month + 1)


@mcp.tool()
async def get_energy_summary(ctx: Context, start: str | None = None, end: str | None = None, bucket: str = "day") -> dict[str, Any]:
    """Chiller plant energy per period: plant kWh, cooling delivered (kWh thermal and RTh), average kW/RT,
    and kWh of the chillers, CHW pumps, CW pumps and tower fans (from the logged kW points).

    Args:
      start: ISO 8601, "now" or relative ("-7d"); default end - 7 days.
      end: Same formats; default now. At most 366 days.
      bucket: hour, day, week or month (plant time; default day).
    """
    user = await current_user(ctx)
    unit = (bucket or "day").lower()
    if unit not in ENERGY_BUCKETS:
        raise ToolError(f"bucket must be one of {', '.join(ENERGY_BUCKETS)}")
    t0, t1 = window(start, end, default_hours=24 * 7, max_days=366)
    if unit == "hour" and t1 - t0 > timedelta(days=31):
        raise ToolError("Hourly buckets are limited to 31 days")
    eps, _ = await ro_query(
        "SELECT ep.id, a.alias, a.type_name, mp.alias AS param FROM asset.equipment_parameters ep"
        " JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id"
        " JOIN analytics.v_assets a ON a.id = ep.equipment_id"
        " WHERE a.type_name = ANY(%(types)s) AND (%(locs)s::bigint[] IS NULL OR a.location_id = ANY(%(locs)s))",
        {"types": list(ENERGY_POINTS), "locs": user.location_list()},
    )
    eps = [e for e in eps if e["param"] in ENERGY_POINTS.get(e["type_name"], ())]
    if not eps:
        return {"start": local_iso(t0), "end": local_iso(t1), "bucket": unit, "periods": [], "note": "No visible plant energy points"}
    meta = {e["id"]: e for e in eps}
    tz = get_settings().timezone
    rows, truncated = await ro_query(
        "SELECT pv.equipment_parameter_id AS pid, date_trunc(%(unit)s, pv.ts AT TIME ZONE %(tz)s) AS b, avg(pv.value) AS v"
        " FROM ts.parameter_values pv"
        " WHERE pv.equipment_parameter_id = ANY(%(pids)s) AND pv.ts >= %(t0)s AND pv.ts < %(t1)s"
        " GROUP BY 1, 2 ORDER BY 2",
        {"unit": unit, "tz": tz, "pids": list(meta), "t0": t0, "t1": t1},
    )
    periods: dict[datetime, dict[str, float]] = defaultdict(lambda: defaultdict(float))
    for r in rows:
        if r["v"] is None:
            continue
        e = meta[r["pid"]]
        b_start = r["b"].replace(tzinfo=plant_tz())
        hours = max(0.0, (min(_bucket_end(b_start, unit), t1) - max(b_start, t0)).total_seconds() / 3600)
        kw = float(r["v"])
        p = periods[b_start]
        p["hours"] = max(p["hours"], hours)
        if e["type_name"] == "Chilled Water System":
            if e["param"] == "plant_kw":
                p["plant_kwh"] += kw * hours
            else:
                p["cooling_kwh_th"] += kw * hours
        else:
            p[ENERGY_GROUP[e["type_name"]]] += kw * hours
    out_periods = []
    totals: dict[str, float] = defaultdict(float)
    for b_start in sorted(periods):
        p = periods[b_start]
        row: dict[str, Any] = {"period_start": local_iso(b_start), "hours": round(p["hours"], 2)}
        for key in ("plant_kwh", "cooling_kwh_th", "chillers_kwh", "chw_pumps_kwh", "cw_pumps_kwh", "tower_fans_kwh"):
            if key in p:
                row[key] = round(p[key], 1)
                totals[key] += p[key]
        if p.get("cooling_kwh_th"):
            row["cooling_rth"] = round(p["cooling_kwh_th"] / KW_PER_RT, 1)
            if p.get("plant_kwh"):
                row["avg_kw_per_rt"] = round(p["plant_kwh"] / (p["cooling_kwh_th"] / KW_PER_RT), 3)
        out_periods.append(row)
    total_row: dict[str, Any] = {k: round(v, 1) for k, v in totals.items()}
    if totals.get("cooling_kwh_th") and totals.get("plant_kwh"):
        total_row["avg_kw_per_rt"] = round(totals["plant_kwh"] / (totals["cooling_kwh_th"] / KW_PER_RT), 3)
    out: dict[str, Any] = {
        "start": local_iso(t0),
        "end": local_iso(t1),
        "bucket": unit,
        "periods": out_periods,
        "totals": total_row,
        "method": "energy = mean logged kW in the period x hours covered",
    }
    if truncated:
        out["truncated"] = True
    return out


@mcp.tool()
async def list_alarm_events(
    ctx: Context,
    asset_aliases: list[str] | None = None,
    start: str | None = None,
    end: str | None = None,
    severity: str | None = None,
    alarm_type: str | None = None,
    code: str | None = None,
    active_only: bool = False,
    limit: int = 200,
) -> dict[str, Any]:
    """Alarm events (newest first) overlapping a window, with alarm code, severity, start / end and whether
    they are still active.

    Args:
      asset_aliases: Optional equipment aliases.
      start: ISO 8601, "now" or relative ("-24h"); default end - 24 h. At most 366 days.
      end: Same formats; default now.
      severity: Optional WARNING or CRITICAL.
      alarm_type: Optional ALARM or ABNORMAL.
      code: Optional alarm name / code, e.g. "CHL-008".
      active_only: Only alarms that have not cleared.
      limit: Maximum rows (1-5000, default 200).
    """
    user = await current_user(ctx)
    t0, t1 = window(start, end, default_hours=24, max_days=366)
    rows, truncated = await ro_query(
        "SELECT ev.id, a.alias, a.name AS equipment, ev.alarm_type, ev.severity, ev.name, ac.code, ac.name AS code_name,"
        " ev.message, ev.start_ts, ev.end_ts, ev.end_ts IS NULL AS active"
        " FROM ts.alarm_events ev JOIN analytics.v_assets a ON a.id = ev.asset_id"
        " LEFT JOIN dm.alarm_codes ac ON ac.id = ev.alarm_code_id"
        " WHERE ev.start_ts < %(t1)s AND (ev.end_ts IS NULL OR ev.end_ts >= %(t0)s)"
        " AND (%(locs)s::bigint[] IS NULL OR a.location_id = ANY(%(locs)s))"
        " AND (%(aliases)s::text[] IS NULL OR upper(a.alias) = ANY(%(aliases)s))"
        " AND (%(sev)s::text IS NULL OR ev.severity = %(sev)s)"
        " AND (%(typ)s::text IS NULL OR ev.alarm_type = %(typ)s)"
        " AND (%(code)s::text IS NULL OR upper(ev.name) = %(code)s OR upper(ac.code) = %(code)s)"
        " AND (NOT %(active)s OR ev.end_ts IS NULL)"
        " ORDER BY ev.start_ts DESC LIMIT %(limit)s",
        {
            "t0": t0,
            "t1": t1,
            "locs": user.location_list(),
            "aliases": norm_aliases(asset_aliases),
            "sev": severity.strip().upper() if severity and severity.strip() else None,
            "typ": alarm_type.strip().upper() if alarm_type and alarm_type.strip() else None,
            "code": code.strip().upper() if code and code.strip() else None,
            "active": bool(active_only),
            "limit": clamp(limit, 1, MAX_ROWS, 200),
        },
    )
    return {"start": local_iso(t0), "end": local_iso(t1), "count": len(rows), "truncated": truncated, "events": jsonable(rows)}


@mcp.tool()
async def list_anomalies(
    ctx: Context,
    asset_aliases: list[str] | None = None,
    status: str | None = None,
    detector: str | None = None,
    start: str | None = None,
    end: str | None = None,
    limit: int = 200,
) -> dict[str, Any]:
    """Statistical sensor anomalies (range, flatline, zscore, ewma, spc) that started in a window.

    Args:
      asset_aliases: Optional equipment aliases.
      status: Optional ACTIVE or CLEARED.
      detector: Optional detector name.
      start: ISO 8601, "now" or relative; default end - 7 days. At most 366 days.
      end: Same formats; default now.
      limit: Maximum rows (1-5000, default 200).
    """
    user = await current_user(ctx)
    _require(user, "aiinsights.read", "anomalies")
    t0, t1 = window(start, end, default_hours=24 * 7, max_days=366)
    rows, truncated = await ro_query(
        "SELECT an.id, a.alias, an.param_alias, an.detector, an.severity, an.status, an.value, an.expected, an.score,"
        " an.message, an.started_at, an.last_seen_at, an.ended_at"
        " FROM analytics.anomalies an JOIN analytics.v_assets a ON a.id = an.asset_id"
        " WHERE an.started_at >= %(t0)s AND an.started_at < %(t1)s"
        " AND (%(locs)s::bigint[] IS NULL OR a.location_id = ANY(%(locs)s))"
        " AND (%(aliases)s::text[] IS NULL OR upper(a.alias) = ANY(%(aliases)s))"
        " AND (%(status)s::text IS NULL OR an.status = %(status)s)"
        " AND (%(det)s::text IS NULL OR an.detector = %(det)s)"
        " ORDER BY an.started_at DESC LIMIT %(limit)s",
        {
            "t0": t0,
            "t1": t1,
            "locs": user.location_list(),
            "aliases": norm_aliases(asset_aliases),
            "status": status.strip().upper() if status and status.strip() else None,
            "det": detector.strip().lower() if detector and detector.strip() else None,
            "limit": clamp(limit, 1, MAX_ROWS, 200),
        },
    )
    return {"start": local_iso(t0), "end": local_iso(t1), "count": len(rows), "truncated": truncated, "anomalies": jsonable(rows)}


@mcp.tool()
async def list_faults(
    ctx: Context,
    asset_aliases: list[str] | None = None,
    status: str | None = None,
    rule_code: str | None = None,
    start: str | None = None,
    end: str | None = None,
    limit: int = 200,
) -> dict[str, Any]:
    """Faults detected by the LBNL FDD rules that started in a window (rule code, equipment, severity,
    intensity estimate, status, instruction status).

    Args:
      asset_aliases: Optional equipment aliases.
      status: Optional ACTIVE or CLEARED.
      rule_code: Optional rule code, e.g. "CT-014".
      start: ISO 8601, "now" or relative; default end - 30 days. At most 366 days.
      end: Same formats; default now.
      limit: Maximum rows (1-5000, default 200).
    """
    user = await current_user(ctx)
    _require(user, "aiinsights.read", "detected faults")
    t0, t1 = window(start, end, default_hours=24 * 30, max_days=366)
    rows, truncated = await ro_query(
        "SELECT f.id, f.rule_code, r.name AS rule_name, a.alias, f.severity, f.status, f.message, f.intensity_estimate,"
        " f.started_at, f.last_seen_at, f.ended_at, f.instruction_status"
        " FROM analytics.faults f JOIN analytics.v_assets a ON a.id = f.asset_id"
        " LEFT JOIN analytics.fault_rules r ON r.code = f.rule_code"
        " WHERE f.started_at >= %(t0)s AND f.started_at < %(t1)s"
        " AND (%(locs)s::bigint[] IS NULL OR a.location_id = ANY(%(locs)s))"
        " AND (%(aliases)s::text[] IS NULL OR upper(a.alias) = ANY(%(aliases)s))"
        " AND (%(status)s::text IS NULL OR f.status = %(status)s)"
        " AND (%(rule)s::text IS NULL OR f.rule_code = %(rule)s)"
        " ORDER BY f.started_at DESC LIMIT %(limit)s",
        {
            "t0": t0,
            "t1": t1,
            "locs": user.location_list(),
            "aliases": norm_aliases(asset_aliases),
            "status": status.strip().upper() if status and status.strip() else None,
            "rule": rule_code.strip().upper() if rule_code and rule_code.strip() else None,
            "limit": clamp(limit, 1, MAX_ROWS, 200),
        },
    )
    return {"start": local_iso(t0), "end": local_iso(t1), "count": len(rows), "truncated": truncated, "faults": jsonable(rows)}


@mcp.tool()
async def get_point_map(ctx: Context) -> dict[str, Any]:
    """The LBNL FDD point names mapped to our tags: LBNL name and unit, equipment pattern ({n} = unit
    index), parameter alias, SI unit and description."""
    await current_user(ctx)
    rows, _ = await ro_query(
        "SELECT lbnl_name, lbnl_unit, asset_pattern, param_alias, si_unit, description FROM analytics.point_map ORDER BY lbnl_name"
    )
    return {"count": len(rows), "points": jsonable(rows)}


@mcp.tool()
async def search_documents(ctx: Context, query: str, filters: dict[str, Any] | None = None, k: int = 6) -> dict[str, Any]:
    """Search the plant document library (manuals, SOPs, maintenance reports, set-point logs, FDD reference).

    Args:
      query: What to look for, in plain words.
      filters: Optional {doc_type, asset_type, unit, alarm_codes: [..], date_from, date_to}.
      k: Number of passages (1-10, default 6).
    """
    user = await current_user(ctx)
    if not (user.can("aiassistant.read") or user.can("aiinsights.read")):
        raise ToolError("The user is not allowed to read the document library")
    q = (query or "").strip()[:500]
    if not q:
        raise ToolError("Give a search query")
    allowed = {"doc_type", "asset_type", "unit", "alarm_codes", "date_from", "date_to"}
    f: dict[str, Any] = {"site_code": get_settings().site_code}
    for key, value in (filters or {}).items():
        if key not in allowed:
            raise ToolError(f"Unknown filter '{key}'; allowed: {', '.join(sorted(allowed))}")
        if value not in (None, "", []):
            f[key] = value
    try:
        from ..rag.search import search as rag_search
    except ImportError:
        raise ToolError("Document search is not available on this server yet") from None
    try:
        found = rag_search(q, k=clamp(k, 1, 10, 6), filters=f, allowed_location_ids=user.location_list(), component="mcp_rag_search", ro=True)
        if inspect.isawaitable(found):
            found = await found
    except (BudgetExceeded, GeminiNotConfigured) as err:
        raise ToolError(f"Document search is unavailable: {err}") from None
    except Exception:  # noqa: BLE001 - no internals (SQL, hosts) in tool errors
        log.exception("document search failed")
        raise ToolError("Document search failed; try again later") from None
    results = []
    for n, r in enumerate(found or [], start=1):
        content = str(r.get("content") or "")
        results.append(
            {
                "ref": n,
                "document_id": r.get("document_id"),
                "chunk_id": r.get("chunk_id"),
                "title": r.get("title"),
                "doc_type": r.get("doc_type"),
                "page": r.get("page"),
                "heading": r.get("heading"),
                "unit": r.get("unit"),
                "asset_type": r.get("asset_type"),
                "doc_date": jsonable(r.get("doc_date")),
                "score": jsonable(r.get("score"), 3),
                "content": content[:1400],
            }
        )
    return {"query": q, "count": len(results), "results": results}


@mcp.tool()
async def list_simulation_jobs(ctx: Context, status: str | None = None, origin: str | None = None, limit: int = 20) -> dict[str, Any]:
    """Digital-twin simulation jobs (newest first) with status, progress, requester, approver and outcome.
    Only jobs the user requested or whose equipment is all in the user's scope are listed.

    Args:
      status: Optional comma list of PROPOSED, QUEUED, RUNNING, ANALYZING, SUCCEEDED, FAILED, REJECTED, CANCELLED.
      origin: Optional ui, agent, optimizer or analytics.
      limit: Maximum rows (1-200, default 20).
    """
    user = await current_user(ctx)
    _require(user, "simulation.read", "simulation jobs")
    statuses = [s.strip().upper() for s in (status or "").split(",") if s.strip()] or None
    rows, truncated = await ro_query(
        "WITH vis AS (SELECT lower(alias) AS alias FROM analytics.v_assets WHERE alias IS NOT NULL AND location_id = ANY(%(locs)s))"
        " SELECT j.id, j.kind, j.title, j.origin, j.status, j.progress, j.rationale,"
        " COALESCE(NULLIF(ru.full_name, ''), ru.username) AS requested_by, COALESCE(NULLIF(au.full_name, ''), au.username) AS approved_by,"
        " j.created_at, j.started_at, j.finished_at, j.sim_start, j.sim_end, j.recommendation_id, j.error,"
        " j.summary -> 'success' AS success, j.summary ->> 'reason' AS reason"
        " FROM sim.jobs j LEFT JOIN core.users ru ON ru.id = j.requested_by LEFT JOIN core.users au ON au.id = j.approved_by"
        " WHERE (%(st)s::text[] IS NULL OR j.status = ANY(%(st)s)) AND (%(origin)s::text IS NULL OR j.origin = %(origin)s)"
        f" AND (%(locs)s::bigint[] IS NULL OR j.requested_by = %(uid)s::bigint OR NOT EXISTS (SELECT 1 FROM {SIM_JOB_ALIASES} AS x(alias)"
        " WHERE x.alias IS NOT NULL AND lower(x.alias) NOT IN (SELECT alias FROM vis)))"
        " ORDER BY j.created_at DESC LIMIT %(limit)s",
        {
            "st": statuses,
            "origin": origin.strip().lower() if origin and origin.strip() else None,
            "locs": user.location_list(),
            "uid": user.user_id or request_user_id(ctx),
            "limit": clamp(limit, 1, 200, 20),
        },
    )
    return {"count": len(rows), "truncated": truncated, "jobs": jsonable(rows)}
