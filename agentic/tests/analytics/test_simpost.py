"""Simulation post-analysis (DESIGN 5.3) on synthetic jobs inserted into sim.* (removed afterwards)."""

from __future__ import annotations

from datetime import timedelta

import numpy as np
import pytest
from psycopg.types.json import Jsonb

from afdd_ai.analytics import data, faults, simpost
from afdd_ai.analytics.series import Frame
from afdd_ai.common import db

from .synth import Faults, plant_frame

pytestmark = pytest.mark.usefixtures("live_db")

HOURS = 8


async def insert_job(kind: str, summary: dict, spec: dict | None = None) -> int:
    row = await db.fetch_one(
        "INSERT INTO sim.jobs (kind, title, origin, status, spec, summary, sim_start, sim_end, started_at)"
        " VALUES (%s, %s, 'analytics', 'ANALYZING', %s, %s, %s, %s, now()) RETURNING id",
        (kind, "pytest simpost", Jsonb(spec or {"purpose": "pytest"}), Jsonb(summary), plant_frame(0.1).t0, plant_frame(0.1).t0 + timedelta(hours=HOURS)),
    )
    assert row is not None
    return int(row["id"])


async def insert_series(job_id: int, variant: str, frame: Frame) -> None:
    pool = await db.get_pool()
    async with pool.connection() as conn:
        async with conn.cursor() as cur:
            await cur.executemany(
                "INSERT INTO sim.series (job_id, variant, asset_alias, param_alias, t0, step_s, measured) VALUES (%s, %s, %s, %s, %s, %s, %s)",
                [(job_id, variant, *key.split(".", 1), frame.t0, frame.step_s, [float(v) for v in arr]) for key, arr in frame.data.items()],
            )


@pytest.fixture
async def cleanup():
    ids: list[int] = []
    yield ids
    if ids:
        await db.execute("DELETE FROM sim.jobs WHERE id = ANY(%s)", (ids,))


async def test_scenario_job_is_analysed(cleanup):
    baseline = plant_frame(HOURS, seed=31)
    rec = plant_frame(HOURS, seed=31, faults=Faults(twv_leak=0.5, start_min=120))
    dp = rec.data["CHWS_01.riser_dp"].copy()
    dp[300:325] = 70.0  # riser DP below 80 kPa for 25 min: a critical violation
    rec.data["CHWS_01.riser_dp"] = dp
    summary = {
        "variants": {
            "baseline": {"energy_kwh": 8000.0, "avg_plant_kw": 1000.0, "avg_kw_per_rt": 0.7},
            "recommended": {"energy_kwh": 7600.0, "avg_plant_kw": 950.0, "avg_kw_per_rt": 0.66},
        },
        "runtime_ms": 1234,
    }
    job_id = await insert_job("scenario", summary)
    cleanup.append(job_id)
    await insert_series(job_id, "baseline", baseline)
    await insert_series(job_id, "recommended", rec)

    frames = await data.sim_frames(job_id)
    assert set(frames) == {"baseline", "recommended"} and frames["baseline"].n == HOURS * 60
    assert np.allclose(frames["baseline"].data["CH_01.chwst"], baseline.data["CH_01.chwst"], atol=1e-4)

    assert await simpost.process_job(job_id) == "SUCCEEDED"
    job = await data.load_job(job_id)
    assert job is not None and job["status"] == "SUCCEEDED" and job["finished_at"] is not None
    s = job["summary"]
    assert s["runtime_ms"] == 1234  # the simulator's summary is kept
    base, recv = s["variants"]["baseline"], s["variants"]["recommended"]
    assert base["success"] is True and base["faults_detected"] == [] and base["constraint_violations"] == []
    assert base["energy_kwh"] == 8000.0  # Go KPIs kept
    assert [f["rule"] for f in recv["faults_detected"]] == ["CHS-006"] and recv["faults_detected"][0]["asset"] == "CHWS_01"
    assert recv["faults_detected"][0]["confirmed_at"] is not None
    (viol,) = recv["constraint_violations"]
    assert viol["constraint"] == "riser_dp_low" and viol["critical"] and viol["longest_min"] == 25 and viol["worst"] == 70.0
    assert recv["success"] is False and s["success"] is False
    assert s["reason"] == "recommended: Riser DP below 80 kPa"
    assert s["comparison"]["recommended"]["saving_kwh"] == 400.0 and s["comparison"]["recommended"]["saving_kw"] == 50.0
    assert any("FDD fault" in n for n in recv["notes"])

    events = await db.fetch_all("SELECT variant, kind, code, severity, asset_alias, message, details FROM sim.events WHERE job_id = %s ORDER BY ts", (job_id,))
    fault_events = [e for e in events if e["kind"] == "fault"]
    assert [(e["variant"], e["code"], e["asset_alias"]) for e in fault_events] == [("recommended", "CHS-006", "CHWS_01")]
    assert fault_events[0]["severity"] == "WARNING" and fault_events[0]["details"]["rule_name"] == "Condenser 3-Way Valve Leakage"
    assert len([e for e in events if e["kind"] == "anomaly"]) == base["anomaly_events"] + recv["anomaly_events"]

    # analysing again is a no-op (the job is no longer ANALYZING)
    assert await simpost.process_job(job_id) is None


async def test_duty_chiller_trip_fails_the_run(cleanup):
    frame = plant_frame(4, seed=32)
    trip = np.zeros(frame.n)
    trip[100:140] = 1.0
    frame.data["CH_01.trip"] = trip
    run = frame.data["CH_01.running"].copy()
    run[100:140] = 0.0
    frame.data["CH_01.running"] = run
    job_id = await insert_job("scenario", {"variants": {"baseline": {}}})
    cleanup.append(job_id)
    await insert_series(job_id, "baseline", frame)
    assert await simpost.process_job(job_id) == "SUCCEEDED"
    s = (await data.load_job(job_id))["summary"]
    v = s["variants"]["baseline"]
    trip_v = [c for c in v["constraint_violations"] if c["constraint"] == "duty_chiller_trip"]
    assert trip_v and trip_v[0]["assets"] == ["CH_01"] and v["success"] is False
    assert s["comparison"] == {}  # a single variant: nothing to compare against the baseline
    assert s["reason"] == "baseline: Duty chiller trip"


async def test_doe_job_counts_its_samples(cleanup):
    job_id = await insert_job("doe", {"samples": 2})
    cleanup.append(job_id)
    await db.execute(
        "INSERT INTO sim.doe_samples (job_id, idx, inputs, outputs) VALUES (%s, 0, '{}', '{}'), (%s, 1, '{}', '{}')",
        (job_id, job_id),
    )
    assert await simpost.process_job(job_id) == "SUCCEEDED"
    s = (await data.load_job(job_id))["summary"]
    assert s["samples"] == 2 and s["success"] is True


async def test_empty_doe_job_fails(cleanup):
    job_id = await insert_job("doe", {})
    cleanup.append(job_id)
    assert await simpost.process_job(job_id) == "FAILED"
    job = await data.load_job(job_id)
    assert job["status"] == "FAILED" and job["error"] == "the DOE produced no samples"


async def test_scenario_without_series_fails(cleanup):
    job_id = await insert_job("scenario", {})
    cleanup.append(job_id)
    assert await simpost.process_job(job_id) == "FAILED"


def test_constraint_violation_runs():
    frame = plant_frame(2, seed=33)
    chws = frame.data["CHWS_01.chws_temp"].copy()
    chws[10:18] = 9.0  # 8 min above 8.5: reported, not critical
    chws[60:75] = 9.2  # 15 min: critical
    frame.data["CHWS_01.chws_temp"] = chws
    viol, trips = simpost.constraint_violations(frame, faults.Layout.from_aliases(frame.aliases()), [])
    (v,) = viol
    assert v["constraint"] == "chws_temp_high" and v["minutes"] == 23 and v["longest_min"] == 15 and v["critical"] and v["worst"] == 9.2
    assert trips == []


def test_mau_valve_saturation():
    frame = plant_frame(2, seed=34)
    valve = np.full(frame.n, 80.0)
    valve[30:50] = 100.0
    frame.data["MAU_01.chw_valve"] = valve
    frame.data["MAU_02.chw_valve"] = np.full(frame.n, 60.0)
    viol, _ = simpost.constraint_violations(frame, faults.Layout.from_aliases(frame.aliases()), [])
    assert [(v["constraint"], v["longest_min"], v["critical"]) for v in viol] == [("mau_valve_saturated", 20, True)]
