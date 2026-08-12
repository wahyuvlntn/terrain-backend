"""Post-analysis of finished simulation jobs (DESIGN 5.3).

The simulator leaves a job in ANALYZING with its series (`sim.series`), states,
events and the physical KPIs per variant (`summary.variants[v]`). Here the
same anomaly detectors and FDD rules as in production run in batch mode on
each variant's measured series; their episodes become `sim.events` (kind
`anomaly` / `fault`), and the summary gains per variant
`{anomalies, faults_detected, constraint_violations, success, notes}` and at
job level `{success, reason, comparison}` (energy saving of every other variant
against `baseline`). The job then moves to SUCCEEDED (NOTIFY sim_jobs).

Success = the run finished without a critical violation: CHW supply header
above 8.5 C for more than 10 min, riser DP below 80 kPa for more than 10 min,
a MAU CHW valve at 100 % for more than 15 min, or any trip of a duty chiller.

DOE jobs only get their sample count (and FAILED when they produced none).
"""

from __future__ import annotations

import asyncio
import logging
import zlib
from dataclasses import dataclass
from typing import Any

import numpy as np

from ..common import db
from . import anomaly, data, faults, rules
from .series import Frame

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class ConstraintSpec:
    name: str
    label: str
    limit_min: float  # critical when continuously violated for longer than this
    worst: str  # max | min


CONSTRAINTS = (
    ConstraintSpec("chws_temp_high", "CHW supply header above 8.5 °C", 10, "max"),
    ConstraintSpec("riser_dp_low", "Riser DP below 80 kPa", 10, "min"),
    ConstraintSpec("mau_valve_saturated", "MAU CHW valve at 100 %", 15, "max"),
)
CHWS_LIMIT = 8.5
RISER_DP_LIMIT = 80.0
VALVE_FULL = 99.5
MAX_ANOMALY_EVENTS = 200  # per variant; the summary counts every anomaly


def _runs(mask: np.ndarray) -> list[tuple[int, int]]:
    """(start, end) inclusive index runs of a boolean mask."""
    out = []
    start = None
    for i, m in enumerate(mask):
        if m and start is None:
            start = i
        elif not m and start is not None:
            out.append((start, i - 1))
            start = None
    if start is not None:
        out.append((start, len(mask) - 1))
    return out


def _violation(frame: Frame, spec: ConstraintSpec, mask: np.ndarray, values: np.ndarray) -> dict[str, Any] | None:
    if not np.any(mask):
        return None
    runs = _runs(mask)
    longest = max(e - s + 1 for s, e in runs) * frame.step_s / 60
    total = int(np.count_nonzero(mask)) * frame.step_s / 60
    v = values[mask]
    worst = float(np.nanmax(v) if spec.worst == "max" else np.nanmin(v))
    first = runs[0][0]
    return {
        "constraint": spec.name,
        "label": spec.label,
        "minutes": round(total, 1),
        "longest_min": round(longest, 1),
        "limit_min": spec.limit_min,
        "worst": round(worst, 3),
        "critical": longest > spec.limit_min,
        "first_at": frame.time(first),
    }


def duty_chiller_trips(frame: Frame, lay: faults.Layout, trip_events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Trips of chillers that were running (on duty) just before."""
    out: list[dict[str, Any]] = []
    for c in lay.chillers:
        trip, run = frame.get(c, "trip"), frame.get(c, "running")
        if trip is None:
            continue
        t = np.nan_to_num(trip) >= 0.5
        r = np.nan_to_num(run) >= 0.5 if run is not None else np.ones(frame.n, dtype=bool)
        for i in np.flatnonzero(t[1:] & ~t[:-1] & r[:-1]) + 1:
            out.append({"asset": c, "at": frame.time(int(i)), "source": "series"})
    for ev in trip_events:
        alias = ev.get("asset_alias") or ""
        if alias in lay.chillers and not any(o["asset"] == alias and abs((o["at"] - ev["ts"]).total_seconds()) <= 300 for o in out):
            out.append({"asset": alias, "at": ev["ts"], "source": "event"})
    return sorted(out, key=lambda o: o["at"])


def constraint_violations(frame: Frame, lay: faults.Layout, trip_events: list[dict[str, Any]]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """(violations, duty chiller trips) of one variant."""
    out: list[dict[str, Any]] = []
    s = lay.system
    if s:
        chws = frame.get(s, "chws_temp")
        if chws is not None:
            v = _violation(frame, CONSTRAINTS[0], np.nan_to_num(chws, nan=-np.inf) > CHWS_LIMIT, chws)
            if v:
                out.append(v)
        dp = frame.get(s, "riser_dp")
        running = frame.get(s, "running")
        if dp is not None:
            on = np.nan_to_num(running, nan=1.0) >= 0.5 if running is not None else np.ones(frame.n, dtype=bool)
            v = _violation(frame, CONSTRAINTS[1], on & (np.nan_to_num(dp, nan=np.inf) < RISER_DP_LIMIT), dp)
            if v:
                out.append(v)
    valves = [frame.get(m, "chw_valve") for m in lay.maus]
    valves = [v for v in valves if v is not None]
    if valves:
        stack = np.vstack(valves)
        vmax = np.nanmax(np.where(np.isnan(stack), -np.inf, stack), axis=0)
        v = _violation(frame, CONSTRAINTS[2], vmax >= VALVE_FULL, vmax)
        if v:
            out.append(v)
    trips = duty_chiller_trips(frame, lay, trip_events)
    if trips:
        out.append(
            {
                "constraint": "duty_chiller_trip",
                "label": "Duty chiller trip",
                "minutes": 0,
                "worst": len(trips),
                "critical": True,
                "first_at": trips[0]["at"],
                "assets": sorted({t["asset"] for t in trips}),
            }
        )
    return out, trips


async def param_metas(aliases: list[str]) -> dict[str, anomaly.ParamMeta]:
    """Anomaly-eligible parameters of the job's equipment, keyed "ALIAS.param"."""
    rows = await db.fetch_all(
        """SELECT e.alias AS asset_alias, mp.alias, mp.name, mp.unit, mp.data_type, mp.scale, mp.is_writable, ep.is_displayed, ep.is_logged
           FROM asset.equipment e
           JOIN asset.equipment_parameters ep ON ep.equipment_id = e.id
           JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
           WHERE e.alias = ANY(%s)""",
        (aliases,),
    )
    out = {}
    for r in rows:
        if r["data_type"] == "number" and r["is_displayed"] and r["is_logged"] and not r["is_writable"] and r["alias"] not in data.STATUS_PARAMS:
            out[f"{r['asset_alias']}.{r['alias']}"] = anomaly.ParamMeta(r["alias"], r["name"], r["unit"], r["scale"])
    return out


def _energy(frame: Frame, lay: faults.Layout, go: dict[str, Any]) -> tuple[float | None, float | None]:
    """(energy kWh, mean plant kW) from the simulator KPIs, else from CHWS plant_kw."""
    if go.get("energy_kwh") is not None and go.get("avg_plant_kw") is not None:
        return float(go["energy_kwh"]), float(go["avg_plant_kw"])
    kw = frame.get(lay.system, "plant_kw") if lay.system else None
    if kw is None or not np.isfinite(kw).any():
        return None, None
    mean = float(np.nanmean(kw))
    return mean * frame.n * frame.step_s / 3600, mean


def analyse_variant(
    name: str,
    frame: Frame,
    metas: dict[str, anomaly.ParamMeta],
    rule_cfg: dict[str, dict[str, Any]],
    trip_events: list[dict[str, Any]],
) -> tuple[dict[str, Any], list[tuple[Any, ...]]]:
    """(summary additions, sim.events rows) of one variant (pure, CPU bound)."""
    lay = faults.Layout.from_aliases(frame.aliases())
    rows: list[tuple[Any, ...]] = []
    anomalies = anomaly.detect_frame(frame, metas)
    by_det: dict[str, int] = {}
    for _, _, ep in anomalies:
        by_det[ep.detector] = by_det.get(ep.detector, 0) + 1
    # the most significant anomalies become events (critical first, then by score); all are counted
    kept = sorted(anomalies, key=lambda a: (a[2].severity != "CRITICAL", -abs(a[2].score or 0.0)))[:MAX_ANOMALY_EVENTS]
    for alias, param, ep in sorted(kept, key=lambda a: a[2].start):
        details = {**ep.details, "param": param, "detector": ep.detector, "score": ep.score, "expected": ep.expected}
        rows.append((name, frame.time(ep.start), frame.time(ep.end), alias, "anomaly", ep.detector, ep.severity, f"{alias}: {ep.message}", ep.value, data.jb(details)))
    detections = faults.detect_batch(frame, rule_cfg, lay)
    found = []
    for d in detections:
        sev = rule_cfg.get(d.rule, {}).get("severity", "WARNING")
        rule = rules.RULES_BY_CODE.get(d.rule)
        details = {
            **d.evidence,
            "rule": d.rule,
            "rule_name": rule.name if rule else d.rule,
            "intensity": d.intensity,
            "confirmed_at": frame.time(d.confirmed) if d.confirmed >= 0 else None,
            "ongoing_at_end": d.ongoing,
        }
        rows.append((name, frame.time(d.start), frame.time(d.end), d.asset, "fault", d.rule, sev, d.message, d.intensity, data.jb(details)))
        found.append({"rule": d.rule, "asset": d.asset, "started_at": frame.time(d.start), "confirmed_at": details["confirmed_at"], "intensity": d.intensity})
    violations, trips = constraint_violations(frame, lay, trip_events)
    critical = [v for v in violations if v["critical"]]
    notes = []
    if found:
        notes.append(f"{len(found)} FDD fault(s): " + ", ".join(f"{f['rule']} on {f['asset']}" for f in found[:6]))
    for v in violations:
        if v["constraint"] == "duty_chiller_trip":
            notes.append(f"Duty chiller trip: {', '.join(v['assets'])}")
        else:
            notes.append(f"{v['label']} for {v['minutes']:.0f} min (longest {v['longest_min']:.0f} min, worst {v['worst']:g})")
    if anomalies:
        notes.append(f"{len(anomalies)} sensor anomalies ({', '.join(f'{k} {n}' for k, n in sorted(by_det.items()))})")
    add = {
        "anomalies": len(anomalies),
        "anomalies_by_detector": by_det,
        "anomaly_events": len(kept),
        "faults_detected": found,
        "constraint_violations": violations,
        "success": not critical,
        "notes": notes,
    }
    return add, rows


def compare(variants: dict[str, dict[str, Any]], energies: dict[str, tuple[float | None, float | None]]) -> dict[str, Any]:
    base = energies.get("baseline")
    if not base or base[0] is None:
        return {}
    out = {}
    for name, (kwh, kw) in energies.items():
        if name == "baseline" or kwh is None:
            continue
        saving = base[0] - kwh
        out[name] = {
            "energy_kwh": round(kwh, 2),
            "baseline_energy_kwh": round(base[0], 2),
            "saving_kwh": round(saving, 2),
            "saving_pct": round(saving / base[0] * 100, 3) if base[0] else None,
            "avg_plant_kw": round(kw, 2) if kw is not None else None,
            "baseline_avg_plant_kw": round(base[1], 2) if base[1] is not None else None,
            "saving_kw": round(base[1] - kw, 2) if base[1] is not None and kw is not None else None,
        }
    return out


async def _finish(job_id: int, status: str, summary: dict[str, Any], error: str | None = None) -> bool:
    n = await db.execute(
        "UPDATE sim.jobs SET status = %s, summary = %s, error = %s, progress = 1, finished_at = COALESCE(finished_at, now()), updated_at = now()"
        " WHERE id = %s AND status = 'ANALYZING'",
        (status, data.jb(summary), error, job_id),
    )
    if n:
        await db.notify("sim_jobs", str(job_id))
    return bool(n)


async def process_job(job_id: int) -> str | None:
    """Analyse one ANALYZING job; returns its final status (None when it was not ANALYZING)."""
    job = await data.load_job(job_id)
    if job is None or job["status"] != "ANALYZING":
        return None
    summary = dict(job["summary"] or {})
    if job["kind"] == "doe":
        n = int(await db.fetch_val("SELECT count(*) FROM sim.doe_samples WHERE job_id = %s", (job_id,)) or 0)
        summary |= {"samples": n, "success": n > 0, "reason": f"{n} DOE samples" if n else "the DOE produced no samples"}
        status = "SUCCEEDED" if n else "FAILED"
        await _finish(job_id, status, summary, None if n else "the DOE produced no samples")
        log.info("sim job %s (doe) %s samples=%s", job_id, status, n)
        return status
    frames = await data.sim_frames(job_id)
    if not frames:
        await _finish(job_id, "FAILED", summary | {"success": False, "reason": "no series recorded"}, "no series recorded")
        return "FAILED"
    aliases = sorted({a for f in frames.values() for a in f.aliases()})
    metas = await param_metas(aliases)
    rule_cfg = await rules.load_rules() or rules.default_rules()
    trip_rows = await db.fetch_all("SELECT variant, asset_alias, ts FROM sim.events WHERE job_id = %s AND kind = 'trip'", (job_id,))
    variants = dict(summary.get("variants") or {})
    rows: list[tuple[Any, ...]] = []
    energies: dict[str, tuple[float | None, float | None]] = {}
    for name, frame in frames.items():
        trips = [r for r in trip_rows if r["variant"] == name]
        add, vrows = await asyncio.to_thread(analyse_variant, name, frame, metas, rule_cfg, trips)
        go = dict(variants.get(name) or {})
        energies[name] = _energy(frame, faults.Layout.from_aliases(frame.aliases()), go)
        variants[name] = go | add
        rows += vrows
    await _store_events(job_id, rows)
    failing = [n for n, v in variants.items() if v.get("success") is False]
    summary["variants"] = variants
    summary["success"] = not failing
    summary["reason"] = (
        "all variants stayed within the plant constraints"
        if not failing
        else "; ".join(f"{n}: " + ", ".join(v["label"] for v in variants[n]["constraint_violations"] if v["critical"]) for n in failing)
    )
    summary["comparison"] = compare(variants, energies)
    summary["analysis"] = {"anomaly_detectors": list(anomaly.DETECTORS), "rules": sorted(rule_cfg), "events": len(rows)}
    await _finish(job_id, "SUCCEEDED", summary)
    log.info("sim job %s analysed variants=%s events=%s success=%s", job_id, list(frames), len(rows), summary["success"])
    return "SUCCEEDED"


async def _store_events(job_id: int, rows: list[tuple[Any, ...]]) -> None:
    pool = await db.get_pool()
    async with pool.connection() as conn:
        async with conn.transaction():
            await conn.execute("DELETE FROM sim.events WHERE job_id = %s AND kind IN ('anomaly', 'fault')", (job_id,))
            if rows:
                async with conn.cursor() as cur:
                    await cur.executemany(
                        "INSERT INTO sim.events (job_id, variant, ts, end_ts, asset_alias, kind, code, severity, message, value, details)"
                        " VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
                        [(job_id, *r) for r in rows],
                    )


def _lock_key(job_id: int) -> tuple[int, int]:
    return zlib.crc32(b"afdd-simpost") & 0x7FFFFFFF, int(job_id) & 0x7FFFFFFF


async def run(limit: int = 5, *, dry_run: bool = False) -> Any:
    """Analyse ANALYZING jobs (oldest first); a job failing analysis is set FAILED.

    dry_run: list the jobs waiting for analysis.
    """
    rows = await db.fetch_all("SELECT id, kind, title FROM sim.jobs WHERE status = 'ANALYZING' ORDER BY updated_at, id LIMIT %s", (limit,))
    if dry_run:
        return [dict(r) for r in rows]
    done = 0
    for r in rows:
        job_id = int(r["id"])
        pool = await db.get_pool()
        async with pool.connection() as lock_conn:
            k1, k2 = _lock_key(job_id)
            got = await (await lock_conn.execute("SELECT pg_try_advisory_lock(%s, %s) AS ok", (k1, k2))).fetchone()
            if not got or not got["ok"]:
                continue  # another worker has it
            try:
                await process_job(job_id)
                done += 1
            except Exception as err:  # noqa: BLE001 - never leave a job stuck in ANALYZING
                log.exception("post-analysis failed job=%s", job_id)
                job = await data.load_job(job_id)
                await _finish(job_id, "FAILED", dict((job or {}).get("summary") or {}) | {"success": False, "reason": "post-analysis failed"}, f"post-analysis failed: {str(err)[:300]}")
            finally:
                await lock_conn.execute("SELECT pg_advisory_unlock(%s, %s)", (k1, k2))
    return done

