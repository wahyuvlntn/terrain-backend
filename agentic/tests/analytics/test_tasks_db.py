"""Production tasks against the host database (rows created here are removed afterwards).

Covers rule seeding, the live FDD pass (open / update / clear of analytics.faults),
fault instructions (RAG + Gemini mocked: no model call is ever made), the
anomaly runner's open / update / clear, rule calibration from a nominal job,
surrogate storage and the optimizer's recommendation / validation / expiry cycle.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

import numpy as np
import pytest
from psycopg.types.json import Jsonb

from afdd_ai.analytics import anomaly, calibration, data, faults, instructions, optimizer, rules, surrogate
from afdd_ai.analytics.series import Frame
from afdd_ai.common import db, gemini

from .synth import Faults, plant_frame
from .test_optimizer import doe_rows

pytestmark = pytest.mark.usefixtures("live_db")


@pytest.fixture
async def seeded_rules():
    """The 7 rules in analytics.fault_rules; removed afterwards when this test created them."""
    existed = await db.fetch_val("SELECT count(*) FROM analytics.fault_rules")
    inserted = await rules.seed_rules()
    yield inserted
    if not existed:
        await db.execute("DELETE FROM analytics.fault_rules WHERE code = ANY(%s)", ([r.code for r in rules.RULES],))


@pytest.fixture
async def created():
    """Ids of rows to remove after the test, per table."""
    rows: dict[str, list[int]] = {"faults": [], "anomalies": [], "jobs": [], "recs": [], "runs": [], "models": []}
    yield rows
    await db.execute("DELETE FROM analytics.recommendations WHERE id = ANY(%s) OR run_id = ANY(%s)", (rows["recs"], rows["runs"]))
    await db.execute("DELETE FROM analytics.optimizer_runs WHERE id = ANY(%s)", (rows["runs"],))
    await db.execute("DELETE FROM sim.jobs WHERE id = ANY(%s)", (rows["jobs"],))
    await db.execute("DELETE FROM analytics.faults WHERE id = ANY(%s)", (rows["faults"],))
    await db.execute("DELETE FROM analytics.anomalies WHERE id = ANY(%s)", (rows["anomalies"],))
    await db.execute("DELETE FROM analytics.surrogate_models WHERE id = ANY(%s)", (rows["models"],))


def utc_minute() -> datetime:
    return datetime.now(timezone.utc).replace(second=0, microsecond=0)


# ---------------------------------------------------------------------------
# rules


async def test_seed_rules_is_idempotent_and_keeps_tuned_params(seeded_rules):
    rows = {r["code"]: r for r in await db.fetch_all("SELECT code, params, defaults, signals, version FROM analytics.fault_rules")}
    assert set(rows) >= {r.code for r in rules.RULES}
    leak = rows["CHS-006"]
    assert leak["defaults"] == rules.RULES_BY_CODE["CHS-006"].defaults
    assert {"lbnl": "TWV_CTRL", "asset": "CHWS_01", "param": "twv_pos"} in leak["signals"]
    before = leak["params"]
    try:
        # an admin tunes a threshold and a parameter key disappears: seeding keeps the value and restores the key
        await db.execute(
            "UPDATE analytics.fault_rules SET params = (params - 'persist_min') || '{\"threshold\": 0.12}' WHERE code = 'CHS-006'"
        )
        assert await rules.seed_rules() == []
        p = await db.fetch_val("SELECT params FROM analytics.fault_rules WHERE code = 'CHS-006'")
        assert p["threshold"] == 0.12 and p["persist_min"] == 30
        loaded = await rules.load_rules()
        assert loaded["CHS-006"]["params"]["threshold"] == 0.12
    finally:
        await db.execute("UPDATE analytics.fault_rules SET params = %s WHERE code = 'CHS-006'", (Jsonb(before),))


# ---------------------------------------------------------------------------
# live FDD pass


def fake_live_frame(make: Any):
    async def live_frame(plant, wanted, start, end, *, step_s=60):  # noqa: ANN001 - data.live_frame signature
        n = int((end - start).total_seconds() // step_s) + 1
        src = make(n)
        frame = Frame(t0=start, step_s=step_s, n=n)
        frame.data = {k: v[-n:] for k, v in src.data.items()}
        return frame

    return live_frame


async def test_live_fdd_opens_updates_and_clears_a_fault(seeded_rules, created, monkeypatch):
    plant = await data.load_plant()
    if plant.get("CHWS_01") is None:
        pytest.skip("CHWS_01 not in this database")
    if await db.fetch_val("SELECT count(*) FROM analytics.faults WHERE status = 'ACTIVE'"):
        pytest.skip("live ACTIVE faults present: the pass would judge them too")
    now = utc_minute()
    leaky = lambda n: plant_frame(n / 60, seed=41, faults=Faults(twv_leak=0.5, start_min=60))  # noqa: E731
    monkeypatch.setattr(faults.data, "live_frame", fake_live_frame(leaky))
    res = await faults.run_live(now=now)
    assert len(res.opened) == 1
    created["faults"] += res.opened
    row = await db.fetch_one("SELECT * FROM analytics.faults WHERE id = %s", (res.opened[0],))
    assert row["rule_code"] == "CHS-006" and row["asset_id"] == plant.get("CHWS_01").id and row["status"] == "ACTIVE"
    assert row["severity"] == "WARNING" and row["instruction_status"] == "PENDING"
    assert row["intensity_estimate"] == pytest.approx(0.5, abs=0.05)
    assert row["evidence"]["mixing_fraction"] == pytest.approx(0.5, abs=0.05) and row["evidence"]["confirmed_at"]
    assert now - timedelta(minutes=320) <= row["started_at"] <= now - timedelta(minutes=290)

    # the next pass updates the same row
    res2 = await faults.run_live(now=now + timedelta(minutes=5))
    assert res2.opened == [] and res2.updated >= 1
    again = await db.fetch_one("SELECT status, started_at, last_seen_at FROM analytics.faults WHERE id = %s", (row["id"],))
    assert again["status"] == "ACTIVE" and again["started_at"] == row["started_at"] and again["last_seen_at"] > row["last_seen_at"]

    # repaired: the condition has been false for the clear window
    repaired = lambda n: plant_frame(n / 60, seed=42)  # noqa: E731
    monkeypatch.setattr(faults.data, "live_frame", fake_live_frame(repaired))
    res3 = await faults.run_live(now=now + timedelta(minutes=10))
    assert res3.cleared >= 1
    cleared = await db.fetch_one("SELECT status, ended_at, evidence FROM analytics.faults WHERE id = %s", (row["id"],))
    assert cleared["status"] == "CLEARED" and cleared["ended_at"] is not None and cleared["evidence"]["clear_reason"] == "condition cleared"


# ---------------------------------------------------------------------------
# instructions (RAG + Gemini mocked)


async def insert_fault(created: dict, rule: str = "CHS-006", alias: str = "CHWS_01", started: datetime | None = None) -> dict:
    plant = await data.load_plant()
    asset = plant.get(alias)
    if asset is None:
        pytest.skip(f"{alias} not in this database")
    started = started or utc_minute() - timedelta(hours=1)
    row = await db.fetch_one(
        "INSERT INTO analytics.faults (rule_code, asset_id, severity, message, intensity_estimate, evidence, started_at, last_seen_at, status)"
        " VALUES (%s, %s, 'WARNING', 'pytest fault', 0.5, '{\"mixing_fraction\": 0.5}', %s, %s, 'CLEARED') RETURNING *",
        (rule, asset.id, started, started + timedelta(minutes=30)),
    )
    created["faults"].append(row["id"])
    return row


SOURCES = [
    {"chunk_id": 1, "document_id": 10, "title": "CUB plant controller manual", "doc_type": "manual", "page": 4, "heading": "CHS-006 Repair",
     "content": "Close the tower bypass valve and check the seat for wear.", "score": 0.81},
    {"chunk_id": 2, "document_id": 11, "title": "Maintenance report MO-1", "doc_type": "maintenance_report", "page": 1, "heading": "Work performed",
     "content": "Replaced the 3-way valve actuator; verified 0 % mixing at minimum position.", "score": 0.74},
]  # fmt: skip


async def test_instruction_is_written_from_sources_only(seeded_rules, created, monkeypatch):
    fault = await insert_fault(created)

    async def retrieve(f, asset):  # noqa: ANN001
        return SOURCES

    prompts: list[str] = []

    async def generate_text(prompt, **kw):  # noqa: ANN001
        prompts.append(prompt)
        return "### Safety\n1. Isolate the valve [1].\n### Diagnosis\n1. Check the seat [1][7].\n### Corrective actions\n1. Replace the actuator [2].\n### Verification\n1. Mixing at 0 % [2-3].", gemini.Usage("m")

    monkeypatch.setattr(instructions, "retrieve", retrieve)
    monkeypatch.setattr(instructions, "library_ready", lambda site: _true())
    monkeypatch.setattr(instructions.gemini, "generate_text", generate_text)
    assert await instructions.generate_for(fault) == "READY"
    row = await db.fetch_one("SELECT instruction_status, instruction_md, instruction_sources, instruction_model FROM analytics.faults WHERE id = %s", (fault["id"],))
    assert row["instruction_status"] == "READY"
    assert "[7]" not in row["instruction_md"] and "[3]" not in row["instruction_md"] and "[2]" in row["instruction_md"]
    assert [s["n"] for s in row["instruction_sources"]] == [1, 2] and all(s["cited"] for s in row["instruction_sources"])
    assert row["instruction_sources"][0]["document_id"] == 10
    assert "[1] CUB plant controller manual" in prompts[0] and "Condenser 3-Way Valve Leakage" in prompts[0]


async def _true() -> bool:
    return True


async def test_instruction_budget_and_missing_library(seeded_rules, created, monkeypatch):
    fault = await insert_fault(created)

    async def over_budget(f, asset):  # noqa: ANN001
        raise gemini.BudgetExceeded("Daily AI budget reached")

    monkeypatch.setattr(instructions, "retrieve", over_budget)
    assert await instructions.generate_for(fault) == "SKIPPED"
    md = await db.fetch_val("SELECT instruction_md FROM analytics.faults WHERE id = %s", (fault["id"],))
    assert "Daily AI budget reached" in md

    # no indexed document yet: a recent fault waits (no model call), an old one is skipped
    async def not_ready(site: str) -> bool:
        return False

    monkeypatch.setattr(instructions, "library_ready", not_ready)
    recent = await insert_fault(created, started=utc_minute() - timedelta(hours=1))
    old = await insert_fault(created, rule="CHS-007", started=utc_minute() - timedelta(hours=30))
    out = await instructions.process([recent, old])
    assert out == {"waiting": 1, "SKIPPED": 1}
    st = {r["id"]: r["instruction_status"] for r in await db.fetch_all("SELECT id, instruction_status FROM analytics.faults WHERE id = ANY(%s)", ([recent["id"], old["id"]],))}
    assert st == {recent["id"]: "PENDING", old["id"]: "SKIPPED"}


def test_citation_check():
    text, cited = instructions.check_citations("A [1], B [2, 3], C [4-6], D [9], E [1–2]", 5)
    assert text == "A [1], B [2][3], C [4][5], D , E [1][2]" and cited == {1, 2, 3, 4, 5}


# ---------------------------------------------------------------------------
# anomaly runner: open / update / clear


async def test_anomaly_rows_open_update_and_clear(created):
    plant = await data.load_plant()
    asset = plant.get("CH_01")
    if asset is None:
        pytest.skip("CH_01 not in this database")
    p = next(p for p in await data.load_params([asset.id]) if p.alias == "chwrt")
    runner = anomaly.AnomalyRunner()
    runner.t0 = utc_minute() - timedelta(minutes=runner.n - 1)
    tr = anomaly._Tracked(p, asset, anomaly.ParamMeta(p.alias, p.name, p.unit, p.scale))  # noqa: SLF001
    if await db.fetch_val("SELECT count(*) FROM analytics.anomalies WHERE status = 'ACTIVE'"):
        pytest.skip("live anomalies present: the clear step would touch them")
    hit = anomaly.Hit("zscore", runner.n - 3, "WARNING", 4.2, 14.0, 10.6, {"window_min": 60, "median": 10.6})
    first, last = runner._time(runner.n - 5), runner._time(runner.n - 3)  # noqa: SLF001
    assert await runner._upsert(tr, hit, first, last) is True  # noqa: SLF001
    row = await db.fetch_one("SELECT * FROM analytics.anomalies WHERE equipment_parameter_id = %s AND status = 'ACTIVE'", (p.id,))
    created["anomalies"].append(row["id"])
    assert row["detector"] == "zscore" and row["severity"] == "WARNING" and row["param_alias"] == "chwrt"
    assert row["details"]["asset_alias"] == "CH_01" and "against a 60-min median" in row["message"]
    crit = anomaly.Hit("zscore", runner.n - 1, "CRITICAL", 7.5, 16.0, 10.6, {"window_min": 60, "median": 10.6})
    assert await runner._upsert(tr, crit, first, runner._time(runner.n - 1)) is False  # noqa: SLF001
    row2 = await db.fetch_one("SELECT severity, score, started_at, last_seen_at FROM analytics.anomalies WHERE id = %s", (row["id"],))
    assert row2["severity"] == "CRITICAL" and row2["score"] == 7.5 and row2["started_at"] == row["started_at"] and row2["last_seen_at"] > row["last_seen_at"]
    assert await runner._clear(utc_minute() + timedelta(minutes=10)) == 0  # 15 min without a hit not reached  # noqa: SLF001
    assert await runner._clear(utc_minute() + timedelta(minutes=20)) == 1  # noqa: SLF001
    row3 = await db.fetch_one("SELECT status, ended_at, last_seen_at FROM analytics.anomalies WHERE id = %s", (row["id"],))
    assert row3["status"] == "CLEARED" and row3["ended_at"] == row3["last_seen_at"]


async def test_anomaly_runner_dry_run_cycle():
    runner = anomaly.AnomalyRunner()
    out = await runner.cycle(dry_run=True)
    assert out["params"] > 0 and "planned" in out
    second = await runner.cycle(dry_run=True)  # incremental: nothing new to evaluate within the same minute
    assert second["params"] == out["params"]


# ---------------------------------------------------------------------------
# calibration from a nominal job


async def test_calibration_applies_a_finished_nominal_job(seeded_rules, created):
    frame = plant_frame(36, seed=51)
    row = await db.fetch_one(
        "INSERT INTO sim.jobs (kind, title, origin, status, spec, summary, finished_at) VALUES ('scenario', 'pytest calibration', 'analytics', 'SUCCEEDED', %s, '{}', now()) RETURNING id",
        (Jsonb(calibration.job_spec() | {"purpose": "pytest-calibration"}),),
    )
    job_id = int(row["id"])
    created["jobs"].append(job_id)
    pool = await db.get_pool()
    async with pool.connection() as conn, conn.cursor() as cur:
        await cur.executemany(
            "INSERT INTO sim.series (job_id, variant, asset_alias, param_alias, t0, step_s, measured) VALUES (%s, 'baseline', %s, %s, %s, 60, %s)",
            [(job_id, *k.split(".", 1), frame.t0, [float(v) for v in arr]) for k, arr in frame.data.items()],
        )
    before = {r["code"]: r for r in await db.fetch_all("SELECT code, params, calibration, calibrated_at, version, updated_by FROM analytics.fault_rules")}
    try:
        applied = await calibration.apply_job(job_id)
        assert set(applied) == {r.code for r in rules.RULES}
        after = {r["code"]: r for r in await db.fetch_all("SELECT code, params, calibration, calibrated_at, version FROM analytics.fault_rules")}
        dp = after["CHS-005"]
        assert dp["params"]["dp_model"] and dp["calibration"]["job_id"] == job_id and dp["calibration"]["k_sigma"] == rules.K_SIGMA
        assert dp["calibrated_at"] is not None and dp["version"] == before["CHS-005"]["version"] + 1
        assert after["CHL-011"]["params"]["threshold_k"] >= 0.5
    finally:
        for code, r in before.items():
            await db.execute(
                "UPDATE analytics.fault_rules SET params = %s, calibration = %s, calibrated_at = %s, version = %s WHERE code = %s",
                (Jsonb(r["params"]), Jsonb(r["calibration"]) if r["calibration"] is not None else None, r["calibrated_at"], r["version"], code),
            )


# ---------------------------------------------------------------------------
# surrogate and optimizer cycle


async def test_surrogate_is_stored_activated_and_loaded(created):
    if await db.fetch_val("SELECT count(*) FROM analytics.surrogate_models"):
        pytest.skip("a surrogate already exists: training here would deactivate it")
    row = await db.fetch_one(
        "INSERT INTO sim.jobs (kind, title, origin, status, spec, finished_at) VALUES ('doe', 'pytest DOE', 'analytics', 'SUCCEEDED', %s, now()) RETURNING id",
        (Jsonb(surrogate.doe_spec() | {"purpose": "pytest-surrogate"}),),
    )
    job_id = int(row["id"])
    created["jobs"].append(job_id)
    pool = await db.get_pool()
    async with pool.connection() as conn, conn.cursor() as cur:
        await cur.executemany(
            "INSERT INTO sim.doe_samples (job_id, idx, inputs, outputs) VALUES (%s, %s, %s, %s)",
            [(job_id, r["idx"], Jsonb(r["inputs"]), Jsonb(r["outputs"])) for r in doe_rows(200, seed=5)],
        )
    model_id = await surrogate.train_and_store(job_id, include_live=False)
    created["models"].append(model_id)
    m = await surrogate.load_active()
    assert m is not None and m.id == model_id and m.version.endswith(f"doe{job_id}") and m.n_samples == 200
    assert m.metrics["doe_samples"] == 200 and m.metrics["targets"]["plant_kw"]["r2"] > 0.9
    pred = m.predict_one({"wet_bulb": 25.5, "dry_bulb": 29, "load_frac": 0.74, "chws_sp": 6.5, "approach_sp": 3.5, "riser_dp_sp": 120, "chillers": 2, "twv_min_pos": 0, "cw_min_temp": 20})  # noqa: E501
    assert 600 < pred["plant_kw"] < 1600


async def test_optimizer_run_skips_without_a_model(created, monkeypatch):
    async def no_model():
        return None

    monkeypatch.setattr(optimizer.surrogate, "load_active", no_model)
    row = await db.fetch_one("INSERT INTO analytics.optimizer_runs (trigger, status) VALUES ('manual', 'PENDING') RETURNING id")
    created["runs"].append(int(row["id"]))
    if await db.fetch_val("SELECT count(*) FROM analytics.optimizer_runs WHERE status = 'PENDING' AND id <> %s", (row["id"],)):
        pytest.skip("other pending optimizer runs exist")
    assert await optimizer.run_manual() == 1
    run = await db.fetch_one("SELECT status, error, started_at, finished_at FROM analytics.optimizer_runs WHERE id = %s", (row["id"],))
    assert run["status"] == "SKIPPED" and "no surrogate" in run["error"] and run["started_at"] and run["finished_at"]


async def test_stale_optimizer_runs_are_failed(created):
    old = await db.fetch_one(
        "INSERT INTO analytics.optimizer_runs (trigger, status, created_at, started_at) VALUES ('manual', 'RUNNING', now() - interval '31 minutes', now() - interval '30 minutes') RETURNING id"
    )
    fresh = await db.fetch_one("INSERT INTO analytics.optimizer_runs (trigger, status, started_at) VALUES ('manual', 'RUNNING', now()) RETURNING id")
    created["runs"] += [int(old["id"]), int(fresh["id"])]
    assert await optimizer.sweep_stale() >= 1
    rows = {r["id"]: r for r in await db.fetch_all("SELECT id, status, error, finished_at FROM analytics.optimizer_runs WHERE id = ANY(%s)", (created["runs"],))}
    assert rows[old["id"]]["status"] == "FAILED" and rows[old["id"]]["error"] == "interrupted (stale)" and rows[old["id"]]["finished_at"]
    assert rows[fresh["id"]]["status"] == "RUNNING"


async def test_recommendation_validation_and_expiry(created, monkeypatch):
    from .test_optimizer import conditions

    if await db.fetch_val("SELECT count(*) FROM analytics.recommendations WHERE status IN ('PROPOSED', 'VALIDATING', 'VALIDATED')"):
        pytest.skip("open recommendations present: a new one would supersede them")

    df = surrogate.doe_frame(doe_rows(300, seed=6))
    boosters, metrics = surrogate.train(df, seed=2)
    model = surrogate.Surrogate(boosters=boosters, ranges=surrogate.ranges_of(df), metrics=metrics, version="pytest", n_samples=len(df))
    cond = conditions()
    res = optimizer.optimize(model, cond, n_trials=120, seed=1)
    actions = optimizer.build_actions(cond, res.best)
    assert actions and actions[0]["param"] == "chws_reset_mode"

    async def insert_job(**kw):  # created PROPOSED, not QUEUED: a running simulator must not pick it up
        row = await db.fetch_one(
            "INSERT INTO sim.jobs (kind, title, origin, status, spec, rationale, recommendation_id) VALUES (%s, %s, %s, 'PROPOSED', %s, %s, %s) RETURNING id",
            (kw["kind"], kw["title"], kw["origin"], Jsonb(kw["spec"]), kw["rationale"], kw.get("recommendation_id")),
        )
        created["jobs"].append(int(row["id"]))
        return int(row["id"])

    monkeypatch.setattr(optimizer.data, "insert_job", insert_job)
    run = await db.fetch_one("INSERT INTO analytics.optimizer_runs (trigger, status, started_at) VALUES ('manual', 'RUNNING', now()) RETURNING id")
    created["runs"].append(int(run["id"]))
    kw, pct, _ = optimizer.saving(res.predicted_current["plant_kw"], res.predicted_best["plant_kw"])
    rec_id, job_id = await optimizer.recommend(int(run["id"]), "SGF1", model, cond, res, actions, kw, pct)
    created["recs"].append(rec_id)
    rec = await db.fetch_one("SELECT * FROM analytics.recommendations WHERE id = %s", (rec_id,))
    assert rec["status"] == "VALIDATING" and rec["validation_job_id"] == job_id
    assert rec["actions"] == actions and rec["current"]["setpoints"]["chws_sp"] == 6.5
    assert {c["name"] for c in rec["constraints"]} == {"chws_temp", "mau_valve_max", "riser_dp"} and all(c["ok"] for c in rec["constraints"])
    assert rec["valid_until"] > datetime.now(timezone.utc) + timedelta(minutes=100)
    job = await data.load_job(job_id)
    assert job["origin"] == "optimizer" and job["recommendation_id"] == rec_id and job["spec"]["variants"][1]["name"] == "recommended"

    # the twin confirms the saving
    summary = {"success": True, "variants": {"baseline": {"avg_plant_kw": 1000, "success": True}, "recommended": {"avg_plant_kw": 970, "success": True}},
               "comparison": {"recommended": {"saving_kw": 30.0, "saving_kwh": 60.0, "saving_pct": 3.0}}}  # fmt: skip
    await db.execute("UPDATE sim.jobs SET status = 'SUCCEEDED', summary = %s, finished_at = now() WHERE id = %s", (Jsonb(summary), job_id))
    assert await optimizer.complete_validations() >= 1
    rec = await db.fetch_one("SELECT status, validation FROM analytics.recommendations WHERE id = %s", (rec_id,))
    assert rec["status"] == "VALIDATED" and rec["validation"]["saving_kw"] == 30.0

    # past valid_until it expires
    await db.execute("UPDATE analytics.recommendations SET valid_until = now() - interval '1 minute' WHERE id = %s", (rec_id,))
    assert await optimizer.expire() >= 1
    rec = await db.fetch_one("SELECT status, validation FROM analytics.recommendations WHERE id = %s", (rec_id,))
    assert rec["status"] == "EXPIRED" and rec["validation"]["expired_reason"] == "valid_until passed"


async def test_current_conditions_reports_missing_supervisory_points():
    cond, reason = await optimizer.current_conditions()
    if cond is None:
        assert reason  # e.g. the plant does not report the supervisory points yet
    else:
        assert 0 < cond.load_frac < 2 and cond.setpoints["chws_sp"] > 0


def test_frame_from_numpy_is_consistent():
    f = plant_frame(1)
    assert isinstance(f.data["CH_01.chwst"], np.ndarray) and f.n == 60


# ---------------------------------------------------------------------------
# job requests of calibration and surrogate (the job insert is captured: no QUEUED row is created)


class Jobs:
    def __init__(self, latest: dict[str, dict | None] | None = None):
        self.latest = latest or {}
        self.inserted: list[dict] = []

    async def latest_job(self, purpose: str, statuses=None):  # noqa: ANN001
        job = self.latest.get(purpose)
        if job is None or (statuses and job["status"] not in statuses):
            return None
        return job

    async def insert_job(self, **kw):  # noqa: ANN003
        self.inserted.append(kw)
        return 999_000 + len(self.inserted)


async def test_calibration_requests_a_nominal_run_then_waits(seeded_rules, monkeypatch):
    if await db.fetch_val("SELECT count(*) FROM analytics.fault_rules WHERE calibrated_at IS NOT NULL"):
        pytest.skip("rules already calibrated in this database")
    jobs = Jobs()
    monkeypatch.setattr(calibration.data, "latest_job", jobs.latest_job)
    monkeypatch.setattr(calibration.data, "insert_job", jobs.insert_job)
    assert await calibration.run() == "requested job 999001"
    (req,) = jobs.inserted
    assert req["kind"] == "scenario" and req["origin"] == "analytics" and req["spec"] == calibration.job_spec()
    jobs.latest["calibration"] = {"id": 5, "status": "RUNNING", "spec": req["spec"], "finished_at": None}
    assert await calibration.run() == "waiting for job 5 (RUNNING)"
    # a failed run backs off, then retries with the runner's limit when the 72 h run was refused
    jobs.latest["calibration"] = {"id": 5, "status": "FAILED", "spec": req["spec"], "finished_at": datetime.now(timezone.utc)}
    assert await calibration.run() == "backing off after failed job 5"
    jobs.latest["calibration"]["finished_at"] = datetime.now(timezone.utc) - timedelta(hours=7)
    assert await calibration.run() == "requested job 999002"
    assert jobs.inserted[-1]["spec"]["duration_min"] == calibration.FALLBACK_DURATION_MIN


async def test_surrogate_requests_a_doe_and_trains_on_it(monkeypatch):
    if await db.fetch_val("SELECT count(*) FROM analytics.surrogate_models"):
        pytest.skip("a surrogate already exists in this database")
    jobs = Jobs()
    trained: list[int] = []

    async def train_and_store(job_id, *, include_live=True):  # noqa: ANN001
        trained.append(job_id)
        return 77

    monkeypatch.setattr(surrogate.data, "latest_job", jobs.latest_job)
    monkeypatch.setattr(surrogate.data, "insert_job", jobs.insert_job)
    monkeypatch.setattr(surrogate, "train_and_store", train_and_store)
    assert await surrogate.run() == "requested DOE job 999001 (no active surrogate)"
    (req,) = jobs.inserted
    assert req["kind"] == "doe" and req["origin"] == "analytics"
    assert req["spec"]["samples"] == 400 and req["spec"]["bounds"] == surrogate.DOE_BOUNDS and req["spec"]["purpose"] == "surrogate"
    jobs.latest["surrogate"] = {"id": 6, "status": "QUEUED", "finished_at": None}
    assert await surrogate.run() == "waiting for DOE job 6 (QUEUED)"
    jobs.latest["surrogate"] = {"id": 6, "status": "SUCCEEDED", "finished_at": datetime.now(timezone.utc)}
    assert await surrogate.run() == "trained model 77 on DOE job 6" and trained == [6]
