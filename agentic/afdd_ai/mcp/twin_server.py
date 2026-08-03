"""MCP server `afdd-twin` (:8102): digital-twin simulation tools for the twin agent.

Everything goes through the backend API with the caller's own session (and
`X-AFDD-Origin: agent`), so permissions, location scope and the human-only
guard apply: the agent can propose a simulation (status PROPOSED) but never
approve one; a person approves it in the application. The only direct
database read is the climatology of `get_forecast_conditions` (read-only role,
scope-checked), because the backend has no endpoint for 14-day hour-of-day
statistics.

Times returned to the model are plant-local ISO strings with offset.

Tools: get_plant_state, get_fault_catalog, get_forecast_conditions,
propose_simulation, get_simulation_status, get_simulation_results,
list_simulations, get_latest_recommendation.
"""

from __future__ import annotations

import json
import logging
from collections import defaultdict
from collections.abc import Mapping
from datetime import timedelta
from typing import Any

from mcp.server.mcpserver import Context, MCPServer
from mcp.server.mcpserver.exceptions import ToolError

from ..common.backend import BackendClient, BackendError
from ..common.config import get_settings
from .auth import McpUser, agent_session_id, current_user
from .util import clamp, jsonable, local_iso, local_text, now, parse_time, plant_tz, ro_query

log = logging.getLogger(__name__)

KW_PER_RT = 3.517
CHILLER_KW = 3517.0
PLANT_UNITS = [
    "CHWS_01",
    "CH_01",
    "CH_02",
    "CH_03",
    "CHWP_01",
    "CHWP_02",
    "CHWP_03",
    "CWP_01",
    "CWP_02",
    "CWP_03",
    "CT_01",
    "CT_02",
    "CT_03",
    "MAU_01",
    "MAU_02",
    "MAU_03",
    "MAU_04",
    "MAU_05",
    "MAU_06",
]
# Points reported per unit kind (alias prefix); CHWS_01 reports every point.
KEY_POINTS = {
    "CH": ("load_pct", "chwst", "chwst_sp", "chwst_asp", "chwrt", "chw_flow", "cond_flow", "cwet", "cwlt", "power_kw", "efficiency", "run_mode"),
    "CHWP": ("speed", "speed_mode", "speed_sp", "flow", "power_kw"),
    "CWP": ("speed", "speed_mode", "speed_sp", "flow", "power_kw"),
    "CT": ("cwst", "cwrt", "approach", "fan_speed", "fan_mode", "fan_speed_sp", "fan_power"),
    "MAU": ("sat", "chw_valve", "fan_speed", "airflow"),
}
# Mode values (simulator ControlSpec options: 0 = automatic).
MODE_LABELS = {
    "chws_reset_mode": ["Reset", "Manual"],
    "staging_mode": ["Load", "Manual"],
    "run_mode": ["Auto", "On", "Off"],
    "fan_mode": ["Auto", "Manual"],
    "speed_mode": ["Auto", "Manual"],
}
FINISHED = {"SUCCEEDED", "FAILED"}
STATUS_MEANING = {
    "PROPOSED": "proposed by the assistant, waiting for a person to approve it; it has not run",
    "QUEUED": "approved and waiting for the simulator; it has not run yet",
    "RUNNING": "the simulator is running it",
    "ANALYZING": "the run finished; analytics are evaluating it",
    "SUCCEEDED": "finished; results are available",
    "FAILED": "failed; see error",
    "REJECTED": "rejected by a person; it will not run",
    "CANCELLED": "cancelled; it will not run",
}
MAX_SPEC_BYTES = 64_000

mcp = MCPServer(
    "afdd-twin",
    instructions=(
        "Digital twin of the CUB chiller plant: live state, injectable faults, expected conditions, and "
        "simulation jobs. Simulations can only be proposed here; a person approves them in the application."
    ),
)


def _map_error(err: BackendError, what: str) -> ToolError:
    msg = (err.message or "").strip() or "no details"
    if err.status == 401:
        return ToolError("The user's session has expired; ask the user to sign in again")
    if err.status == 403:
        return ToolError(f"The user is not allowed to access {what} ({msg})")
    if err.status == 404:
        return ToolError(f"{what} was not found ({msg})")
    if err.status in (400, 409, 422):
        return ToolError(f"The application rejected the request for {what}: {msg}")
    return ToolError(f"The application could not return {what} (HTTP {err.status}: {msg})")


async def _call(user: McpUser, script: str, data: dict[str, Any] | None, what: str) -> Any:
    try:
        return await BackendClient(user.token).call(script, data or {})
    except BackendError as err:
        log.info("backend call failed script=%s status=%s", script, err.status)
        raise _map_error(err, what) from None


def _mode_label(param: str, value: Any) -> Any:
    labels = MODE_LABELS.get(param)
    if labels is None or not isinstance(value, (int, float)):
        return value
    i = int(round(value))
    return labels[i] if 0 <= i < len(labels) else value


def _unit_points(alias: str, params: Mapping[str, Any]) -> dict[str, Any]:
    kind = alias.split("_", 1)[0]
    keys = KEY_POINTS.get(kind)
    out = {}
    for key, p in params.items():
        if keys is not None and key not in keys:
            continue
        value = p.get("value") if isinstance(p, Mapping) else p
        out[key] = _mode_label(key, jsonable(value, 3))
    return out


@mcp.tool()
async def get_plant_state(ctx: Context) -> dict[str, Any]:
    """Current state of the chiller plant for building a scenario: plant controller points and modes
    (CHWS_01), every chiller, pump, tower and MAU with status, condition and key values, and the faults
    currently active in the simulated plant."""
    user = await current_user(ctx)
    rows = await _call(user, "digital_twin/getliveoverview", {"aliases": PLANT_UNITS}, "the live plant state") or []
    units = []
    plant: dict[str, Any] = {}
    latest = None
    for r in rows:
        alias = str(r.get("alias") or "")
        params = r.get("params") or {}
        for p in params.values():
            ts = p.get("timestamp") if isinstance(p, Mapping) else None
            if ts and (latest is None or ts > latest):
                latest = ts
        if alias == "CHWS_01":
            plant = {"status": r.get("status"), "condition": r.get("condition"), "points": _unit_points(alias, params)}
            plant["units"] = {k: (v or {}).get("unit") for k, v in params.items() if isinstance(v, Mapping) and (v or {}).get("unit")}
            continue
        units.append(
            {
                "alias": alias,
                "type": r.get("type_name"),
                "status": r.get("status"),
                "condition": r.get("condition"),
                "values": _unit_points(alias, params),
            }
        )
    out: dict[str, Any] = {"as_of": local_text(latest), "plant_controller": plant or None, "units": units}
    if not rows:
        out["note"] = "No plant units are visible to this user"
    try:
        faults = await _call(user, "digital_twin/getactivefaults", {}, "the active faults")
        out["active_faults"] = [{**f, "start": local_text(f.get("start"))} if isinstance(f, Mapping) else f for f in jsonable(faults or [])]
    except ToolError as err:
        out["active_faults_error"] = str(err)
    return out


@mcp.tool()
async def get_fault_catalog(ctx: Context) -> dict[str, Any]:
    """Faults that a simulation can inject: kind, equipment kind and units, documented LBNL intensities,
    unit, label and the alarm / rule code it should trigger."""
    user = await current_user(ctx)
    rows = await _call(user, "simulation/getfaultcatalog", {}, "the fault catalog") or []
    return {"count": len(rows), "faults": jsonable(rows)}


@mcp.tool()
async def get_forecast_conditions(ctx: Context, start: str | None = None, hours: int = 24) -> dict[str, Any]:
    """Expected outdoor conditions and cooling load for the coming hours, as a climatology of the last 14 days
    at the same hours of day (mean, 10th and 90th percentile). It is not a weather forecast.

    Args:
      start: First hour (ISO 8601, "now" or relative); default now.
      hours: Number of hours (1-72, default 24).
    """
    user = await current_user(ctx)
    t_start = parse_time(start, now(), "start").astimezone(plant_tz()).replace(minute=0, second=0, microsecond=0)
    n = clamp(hours, 1, 72, 24)
    tz = get_settings().timezone
    candidates, _ = await ro_query(
        "SELECT ep.id, a.alias, a.type_name, mp.alias AS param FROM asset.equipment_parameters ep"
        " JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id"
        " JOIN analytics.v_assets a ON a.id = ep.equipment_id"
        " WHERE ((a.alias = 'CHWS_01' AND mp.alias IN ('oa_temp', 'wet_bulb', 'cooling_load'))"
        "   OR (a.type_name = 'Cooling Tower' AND mp.alias = 'wet_bulb')"
        "   OR (a.type_name = 'Make-up Air Unit' AND mp.alias = 'oat'))"
        " AND (%(locs)s::bigint[] IS NULL OR a.location_id = ANY(%(locs)s))",
        {"locs": user.location_list()},
    )
    # quantity -> source priority (plant controller first, unit sensors as fallback)
    sources = {
        "wet_bulb_c": [("CHWS_01", "wet_bulb"), ("CT", "wet_bulb")],
        "dry_bulb_c": [("CHWS_01", "oa_temp"), ("MAU", "oat")],
        "cooling_load_kw": [("CHWS_01", "cooling_load")],
    }
    by_source: dict[tuple[str, str], list[int]] = defaultdict(list)
    for c in candidates:
        key = ("CHWS_01", c["param"]) if c["alias"] == "CHWS_01" else (c["alias"].split("_", 1)[0], c["param"])
        by_source[key].append(int(c["id"]))
    chosen: dict[str, tuple[str, str] | None] = {qty: next((c for c in cands if c in by_source), None) for qty, cands in sources.items()}
    if not any(chosen.values()):
        raise ToolError("No weather or load points are visible to this user")
    qty_of: dict[int, str] = {}
    for qty, src in chosen.items():
        if src is not None:
            for ep_id in by_source[src]:
                qty_of[ep_id] = qty
    rows, _ = await ro_query(
        "SELECT pv.equipment_parameter_id AS pid, extract(hour FROM pv.ts AT TIME ZONE %(tz)s)::int AS h,"
        " avg(pv.value) AS mean, percentile_cont(0.1) WITHIN GROUP (ORDER BY pv.value) AS p10,"
        " percentile_cont(0.9) WITHIN GROUP (ORDER BY pv.value) AS p90, count(*) AS n"
        " FROM ts.parameter_values pv"
        " WHERE pv.equipment_parameter_id = ANY(%(pids)s) AND pv.ts >= %(since)s AND pv.value IS NOT NULL"
        " GROUP BY 1, 2",
        {"tz": tz, "pids": list(qty_of), "since": now() - timedelta(days=14)},
    )
    stats: dict[str, dict[int, list[dict[str, Any]]]] = defaultdict(lambda: defaultdict(list))
    for r in rows:
        stats[qty_of[r["pid"]]][int(r["h"])].append(r)
    if not stats:
        raise ToolError("No weather or load history in the last 14 days")
    out_hours = []
    for i in range(n):
        t = t_start + timedelta(hours=i)
        row: dict[str, Any] = {"time": local_iso(t)}
        for qty, src in chosen.items():
            if src is None:
                continue
            group = stats[qty].get(t.hour) or []
            total = sum(int(g["n"]) for g in group)
            if not total:
                continue
            row[qty] = {
                "mean": round(sum(float(g["mean"]) * int(g["n"]) for g in group) / total, 2),
                "p10": round(min(float(g["p10"]) for g in group), 2),
                "p90": round(max(float(g["p90"]) for g in group), 2),
            }
        load = row.get("cooling_load_kw")
        if load:
            row["load_frac_of_3_chillers"] = round(load["mean"] / (3 * CHILLER_KW), 3)
            row["chillers_needed"] = max(1, min(3, int(-(-load["mean"] // (CHILLER_KW * 0.95)))))
        out_hours.append(row)
    return {
        "method": "climatology: statistics of the last 14 days at the same hour of day (plant time); not a weather forecast",
        "sources": {k: (f"{v[0]}.{v[1]}" if v else None) for k, v in chosen.items()},
        "hours": out_hours,
    }


def summarise_spec(spec: Mapping[str, Any]) -> str:
    """One-line human summary of a scenario spec (for the approval card)."""
    parts = []
    duration = spec.get("duration_min")
    parts.append(f"{duration} min" if duration is not None else "default duration")
    parts.append(f"from {spec.get('start') or 'now'}")
    parts.append(f"{spec.get('initial') or 'live'} initial state")
    weather = spec.get("weather") or {}
    if isinstance(weather, Mapping) and (weather.get("dry_bulb_offset") or weather.get("wet_bulb_offset")):
        parts.append(f"weather offsets DB {weather.get('dry_bulb_offset') or 0:+} K / WB {weather.get('wet_bulb_offset') or 0:+} K")
    if spec.get("load_scale") not in (None, 1, 1.0):
        parts.append(f"load x{spec.get('load_scale')}")
    common = spec.get("actions") or []
    if common:
        parts.append(f"{len(common)} common action(s)")
    variants = spec.get("variants") or []
    if variants:
        names = [f"{v.get('name')} ({len(v.get('actions') or [])} action(s))" for v in variants if isinstance(v, Mapping)]
        parts.append("variants: " + ", ".join(names))
    if spec.get("stochastic"):
        parts.append("stochastic")
    return "; ".join(parts)


def _validate_spec(spec: Any) -> dict[str, Any]:
    if not isinstance(spec, Mapping):
        raise ToolError("spec must be a JSON object")
    if len(json.dumps(spec, default=str)) > MAX_SPEC_BYTES:
        raise ToolError("spec is too large")
    kind = spec.get("kind", "scenario")
    if kind != "scenario":
        raise ToolError("Only scenario simulations can be proposed")
    duration = spec.get("duration_min")
    if not isinstance(duration, (int, float)) or not 10 <= duration <= 2880:
        raise ToolError("spec.duration_min must be a number from 10 to 2880")
    variants = spec.get("variants") or []
    if not isinstance(variants, list) or len(variants) > 3:
        raise ToolError("spec.variants must be a list of at most 3 variants")
    actions = list(spec.get("actions") or [])
    for v in variants:
        if not isinstance(v, Mapping) or not str(v.get("name") or "").strip():
            raise ToolError("Each variant needs a name")
        actions += list(v.get("actions") or [])
    if len(actions) > 200:
        raise ToolError("At most 200 actions in total")
    for a in actions:
        if not isinstance(a, Mapping) or not a.get("kind") or not a.get("asset"):
            raise ToolError("Each action needs at least kind and asset")
    return dict(spec)


def _job_row(job: Mapping[str, Any]) -> dict[str, Any]:
    status = job.get("status")
    return {
        "job_id": job.get("id"),
        "title": job.get("title"),
        "kind": job.get("kind"),
        "origin": job.get("origin"),
        "status": status,
        "meaning": STATUS_MEANING.get(str(status), ""),
        "progress": jsonable(job.get("progress"), 2),
        "requested_by": job.get("requested_by_name"),
        "approved_by": job.get("approved_by_name"),
        "approved_at": local_text(job.get("approved_at")),
        "created_at": local_text(job.get("created_at")),
        "started_at": local_text(job.get("started_at")),
        "finished_at": local_text(job.get("finished_at")),
        "sim_start": local_text(job.get("sim_start")),
        "sim_end": local_text(job.get("sim_end")),
        "variants": job.get("variants"),
        "recommendation_id": job.get("recommendation_id"),
        "error": job.get("error"),
    }


@mcp.tool()
async def propose_simulation(ctx: Context, title: str, rationale: str, spec: dict[str, Any]) -> dict[str, Any]:
    """Propose a digital-twin scenario simulation. It is created as PROPOSED and runs only after a person
    approves it in the application; it has not run when this returns.

    Args:
      title: Short title, e.g. "Raise CHW supply set point to 7.0 degC for 4 h".
      rationale: Why this scenario, citing the evidence (optimiser recommendation, plant state, documents).
      spec: Scenario spec: {start, duration_min, initial, stochastic, seed, weather, load_scale, actions, variants}.
    """
    user = await current_user(ctx)
    t = (title or "").strip()
    r = (rationale or "").strip()
    if not 3 <= len(t) <= 200:
        raise ToolError("title must be 3-200 characters")
    if not r:
        raise ToolError("A rationale is required")
    clean = _validate_spec(spec)
    body: dict[str, Any] = {"title": t, "rationale": r[:4000], "spec": clean}
    if (sid := agent_session_id(ctx)) is not None:
        body["agent_session_id"] = sid
    job = await _call(user, "simulation/propose", body, "proposing a simulation") or {}
    status = job.get("status") or "PROPOSED"
    return {
        "job_id": job.get("id"),
        "title": job.get("title") or t,
        "status": status,
        "approval_required": status == "PROPOSED",
        "rationale": job.get("rationale") or r[:4000],
        "spec_summary": summarise_spec(clean),
        "message": "Proposed. A person must approve it in the application before it runs; it has not run.",
    }


@mcp.tool()
async def get_simulation_status(ctx: Context, job_id: int) -> dict[str, Any]:
    """Status of a simulation job (PROPOSED, QUEUED, RUNNING, ANALYZING, SUCCEEDED, FAILED, REJECTED,
    CANCELLED), progress, who requested and approved it, and timing.

    Args:
      job_id: Simulation job id.
    """
    user = await current_user(ctx)
    job = await _call(user, "simulation/getjob", {"job_id": int(job_id)}, f"simulation job {job_id}") or {}
    return _job_row(job)


@mcp.tool()
async def get_simulation_results(ctx: Context, job_id: int, events_limit: int = 30) -> dict[str, Any]:
    """Results of a finished simulation: per-variant KPIs (energy, plant kW, kW/RT, CHW temperatures,
    riser dP, MAU valves, alarms, trips, constraint violations, success), the variant comparison, and the
    most important events (alarms, faults, trips, anomalies).

    Args:
      job_id: Simulation job id.
      events_limit: Maximum events (1-100, default 30).
    """
    user = await current_user(ctx)
    job = await _call(user, "simulation/getjob", {"job_id": int(job_id)}, f"simulation job {job_id}") or {}
    out = _job_row(job)
    status = str(job.get("status") or "")
    if status not in FINISHED:
        out["note"] = f"No results: the job is {status or 'unknown'} ({STATUS_MEANING.get(status, '')})."
        if status == "ANALYZING":
            out["partial_summary"] = jsonable(job.get("summary") or {}, 3)
        return out
    out["summary"] = jsonable(job.get("summary") or {}, 3)
    try:
        events = await _call(
            user,
            "simulation/getevents",
            {"job_id": int(job_id), "kinds": "alarm,fault,trip,anomaly,diagnosis,fault_injected,fault_cleared", "limit": clamp(events_limit, 1, 100, 30)},
            f"the events of simulation job {job_id}",
        ) or {}
        out["events_total"] = events.get("total")
        out["events"] = [
            {
                "variant": e.get("variant"),
                "time": local_text(e.get("ts")),
                "asset": e.get("asset_alias"),
                "kind": e.get("kind"),
                "code": e.get("code"),
                "severity": e.get("severity"),
                "message": e.get("message"),
            }
            for e in (events.get("events") or [])
        ]
    except ToolError as err:
        out["events_error"] = str(err)
    return out


@mcp.tool()
async def list_simulations(ctx: Context, limit: int = 10, status: str | None = None) -> dict[str, Any]:
    """Recent simulation jobs (newest first).

    Args:
      limit: Maximum jobs (1-50, default 10).
      status: Optional comma list of statuses.
    """
    user = await current_user(ctx)
    body: dict[str, Any] = {"limit": clamp(limit, 1, 50, 10)}
    if status and status.strip():
        body["status"] = ",".join(s.strip().upper() for s in status.split(",") if s.strip())
    data = await _call(user, "simulation/listjobs", body, "the simulation jobs") or {}
    return {"total": data.get("total"), "jobs": [_job_row(j) for j in (data.get("jobs") or [])]}


@mcp.tool()
async def get_latest_recommendation(ctx: Context) -> dict[str, Any]:
    """The newest set-point optimisation recommendation: conditions, current vs recommended set points,
    predicted plant kW and savings, the writes it would apply, twin validation and status."""
    user = await current_user(ctx)
    rows = await _call(user, "ai_insights/getrecommendations", {"limit": 1}, "the optimisation recommendations") or []
    if not rows:
        return {"recommendation": None, "note": "The optimiser has not produced a recommendation yet"}
    rec = jsonable(rows[0], 3)
    if isinstance(rec, dict):
        rec = {k: local_text(v) if k.endswith("_at") or k == "valid_until" else v for k, v in rec.items()}
    return {"recommendation": rec}
