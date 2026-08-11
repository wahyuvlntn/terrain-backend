"""Surrogate training and the set-point optimizer on synthetic DOE data.

`true_plant` is a small physical model of the CUB plant with the trade-offs the
optimizer must find: a warmer CHW set point and a wider tower approach save
chiller / fan energy but open the MAU coil valves and warm the supply; a lower
riser DP saves pump energy but also opens the valves.
"""

from __future__ import annotations

import math
from datetime import datetime, timezone

import numpy as np
import pandas as pd
import pytest
from scipy.stats import qmc

from afdd_ai.analytics import optimizer, surrogate
from afdd_ai.analytics.optimizer import Conditions

CHILLER_KW = 3517.0


def true_plant(wet_bulb, dry_bulb, load_frac, chws_sp, approach_sp, riser_dp_sp, chillers, twv_min_pos, cw_min_temp, rng=None):
    """Steady-state plant outputs (vectorised over numpy arrays)."""
    load = load_frac * 2 * CHILLER_KW
    cwst = np.maximum(wet_bulb + approach_sp, cw_min_temp) + 0.02 * twv_min_pos
    lift = cwst + 4.0 - chws_sp
    plr = load / (chillers * CHILLER_KW)
    kw_per_kw = (0.105 + 0.0045 * (lift - 22.0)) * (1 + 0.6 * (plr - 0.75) ** 2)
    chiller_kw = load * kw_per_kw
    qf = load * 1.17 / np.maximum(chillers, 1) / 3036.0
    fan = np.clip(100.0 * (2.48 * qf**0.6 / approach_sp) ** 1.25, 20, 100)
    tower_kw = chillers * 30.0 * (fan / 100.0) ** 3
    pump_kw = chillers * (40.0 * (riser_dp_sp / 120.0) ** 1.5 + 45.0)
    demand = 70.0 * load_frac / 0.74 * (1 + 0.22 * (chws_sp - 6.5)) * np.sqrt(120.0 / riser_dp_sp)
    out = {
        "plant_kw": chiller_kw + tower_kw + pump_kw,
        "chws_temp": chws_sp + 0.06 + 0.4 * np.maximum(0, plr - 0.95),
        "riser_dp": riser_dp_sp - 2.0 * np.maximum(0, demand - 100) / 10,
        "mau_valve_max": np.minimum(demand, 100.0),
        "mau_sat_dev_max": 0.6 * np.maximum(0, demand - 100) / 10,
    }
    if rng is not None:
        out["plant_kw"] = out["plant_kw"] * (1 + rng.normal(0, 0.004, np.shape(load_frac)))
    return out


def doe_rows(n: int = 400, seed: int = 0) -> list[dict]:
    names = list(surrogate.DOE_BOUNDS)
    sample = qmc.LatinHypercube(d=len(names), seed=seed).random(n)
    lo = np.array([surrogate.DOE_BOUNDS[k][0] for k in names])
    hi = np.array([surrogate.DOE_BOUNDS[k][1] for k in names])
    vals = lo + sample * (hi - lo)
    rng = np.random.default_rng(seed)
    rows = []
    for i, v in enumerate(vals):
        inp = dict(zip(names, v, strict=True))
        inp["chillers"] = float(round(inp["chillers"]))
        outs = true_plant(
            inp["wet_bulb"], inp["wet_bulb"] + inp["dry_bulb_minus_wb"], inp["load_frac"], inp["chws_sp"], inp["approach_sp"],
            inp["riser_dp_sp"], inp["chillers"], inp["twv_min_pos"], inp["cw_min_temp"], rng,
        )  # fmt: skip
        rows.append({"idx": i, "inputs": inp, "outputs": {k: float(np.asarray(x)) for k, x in outs.items()}})
    return rows


@pytest.fixture(scope="module")
def model() -> surrogate.Surrogate:
    df = surrogate.doe_frame(doe_rows())
    boosters, metrics = surrogate.train(df, seed=1)
    return surrogate.Surrogate(boosters=boosters, ranges=surrogate.ranges_of(df), metrics=metrics, version="test", n_samples=len(df))


def conditions(**sp) -> Conditions:
    setpoints = {"chws_sp": 6.5, "approach_sp": 3.5, "riser_dp_sp": 120.0, "twv_min_pos": 0.0, "cw_min_temp": 20.0, "chillers": 2.0} | sp
    return Conditions(
        wet_bulb=25.5, dry_bulb=29.0, load_frac=0.74, cooling_load_kw=0.74 * 2 * CHILLER_KW, chillers_running=int(setpoints["chillers"]),
        setpoints=setpoints, modes={"chws_reset_mode": 0.0, "staging_mode": 0.0}, measured={"plant_kw": 1000.0},
        window_start=datetime(2026, 10, 10, 6, 0, tzinfo=timezone.utc), window_end=datetime(2026, 10, 10, 6, 15, tzinfo=timezone.utc),
    )  # fmt: skip


def truth(cond: Conditions, sp: dict) -> dict:
    return {
        k: float(np.asarray(v))
        for k, v in true_plant(
            cond.wet_bulb, cond.dry_bulb, cond.load_frac, sp["chws_sp"], sp["approach_sp"], sp["riser_dp_sp"], sp["chillers"],
            sp["twv_min_pos"], cond.setpoints["cw_min_temp"],
        ).items()  # fmt: skip
    }


def test_surrogate_learns_the_plant(model):
    t = model.metrics["targets"]
    assert t["plant_kw"]["r2"] > 0.95 and t["plant_kw"]["mape_pct"] < 3
    assert t["chws_temp"]["r2"] > 0.95 and t["riser_dp"]["r2"] > 0.9 and t["mau_valve_max"]["r2"] > 0.9
    assert model.metrics["n_holdout"] == 80 and model.metrics["n_train"] == 320


def test_surrogate_artifact_round_trip(model):
    blob = model.to_artifact()
    assert blob[:2] == b"\x1f\x8b"
    back = surrogate.Surrogate.from_artifact(blob, version="x")
    row = conditions().features(conditions().setpoints)
    assert back.predict_one(row) == pytest.approx(model.predict_one(row))
    assert back.features == surrogate.FEATURES


def test_out_of_range_conditions_are_flagged(model):
    row = conditions().features(conditions().setpoints)
    assert model.out_of_range(row) == []
    assert model.out_of_range({**row, "wet_bulb": 35.0}) == ["wet_bulb"]


def test_optimizer_finds_a_better_feasible_point_within_the_step_limits(model):
    cond = conditions()
    res = optimizer.optimize(model, cond, n_trials=300, seed=3)
    assert res.best is not None and res.n_feasible > 50
    best = res.best
    # step limits per run
    assert abs(best["chws_sp"] - 6.5) <= 0.5 + 1e-9
    assert abs(best["approach_sp"] - 3.5) <= 1.0 + 1e-9
    assert abs(best["riser_dp_sp"] - 120) <= 20 + 1e-9
    assert 0 <= best["twv_min_pos"] <= 30
    # feasible staging: n x 3517 x 0.95 >= load
    assert best["chillers"] * CHILLER_KW * 0.95 >= cond.cooling_load_kw
    # predicted constraints and the true plant's outcome
    for name, op, limit in optimizer.CONSTRAINTS:
        v = res.predicted_best[name]
        assert (v <= limit) if op == "<=" else (v >= limit)
    cur, new = truth(cond, cond.setpoints), truth(cond, best)
    assert new["plant_kw"] < cur["plant_kw"] * 0.99  # a real saving of more than 1 %
    assert new["chws_temp"] <= 7.5 + 0.1 and new["mau_valve_max"] <= 95 + 3 and new["riser_dp"] >= 95 - 3
    kw, pct, worth = optimizer.saving(res.predicted_current["plant_kw"], res.predicted_best["plant_kw"])
    assert worth and kw > 15 and pct > 1


def test_optimizer_respects_a_binding_constraint(model):
    # a hot afternoon at high load: the MAU valves limit how warm the CHW may go
    cond = conditions()
    cond.load_frac, cond.cooling_load_kw, cond.wet_bulb, cond.dry_bulb = 0.92, 0.92 * 2 * CHILLER_KW, 27.5, 32.0
    res = optimizer.optimize(model, cond, n_trials=300, seed=4)
    assert res.best is not None
    assert truth(cond, res.best)["mau_valve_max"] <= 95 + 3
    assert res.best["chillers"] >= 2  # one chiller cannot carry 6.5 MW


def test_decision_bounds_follow_the_current_set_points():
    b = optimizer.decision_bounds(conditions(chws_sp=7.8, riser_dp_sp=85.0))
    assert b["chws_sp"] == (7.3, 8.0)  # +-0.5 K clipped to the range
    assert b["riser_dp_sp"] == (80.0, 105.0)
    assert b["approach_sp"] == (2.5, 4.5) and b["twv_min_pos"] == (0.0, 30.0)
    assert optimizer.feasible_chillers(5000) == [2, 3] and optimizer.feasible_chillers(12000) == [3]


@pytest.mark.parametrize(
    ("key", "current", "expected"),
    [
        ("chws_sp", 9.23, (8.8, 8.8)),  # 1.23 K above the range: one 0.5 K step toward it, on the 0.1 grid
        ("chws_sp", 9.0, (8.5, 8.5)),
        ("chws_sp", 4.87, (5.3, 5.3)),
        ("chws_sp", 8.3, (7.8, 8.0)),  # outside by less than a step: the range part within the step
        ("riser_dp_sp", 50.0, (70.0, 70.0)),
        ("riser_dp_sp", 59.7, (79.0, 79.0)),
        ("riser_dp_sp", 185.0, (165.0, 165.0)),
        ("approach_sp", 8.0, (7.0, 7.0)),
    ],
)
def test_no_recommended_change_exceeds_the_step_limit(key, current, expected):
    b = optimizer.decision_bounds(conditions(**{key: current}))
    assert b[key] == pytest.approx(expected)
    max_step = next(d.max_step for d in optimizer.DECISIONS if d.key == key)
    assert max(abs(b[key][0] - current), abs(b[key][1] - current)) <= max_step + 1e-9


def test_optimizer_moves_an_out_of_range_set_point_by_one_step_limit(model):
    cond = conditions(chws_sp=9.2)
    res = optimizer.optimize(model, cond, n_trials=40, seed=2)
    assert res.bounds["chws_sp"] == pytest.approx([8.7, 8.7])
    if res.best is not None:
        assert res.best["chws_sp"] == pytest.approx(8.7)
        assert [a["value"] for a in optimizer.build_actions(cond, res.best) if a["param"] == "chws_sp"] == [8.7]


def test_actions_switch_modes_to_manual_before_the_set_points():
    cond = conditions()
    best = {"chws_sp": 7.0, "approach_sp": 4.1, "riser_dp_sp": 110.0, "chillers": 3.0, "twv_min_pos": 0.0}
    actions = optimizer.build_actions(cond, best)
    params = [a["param"] for a in actions]
    assert params == ["chws_reset_mode", "staging_mode", "chws_sp", "approach_sp", "riser_dp_sp", "chillers_sp"]
    assert actions[0] == {"asset": "CHWS_01", "param": "chws_reset_mode", "value": 1.0, "unit": "", "label": "CHW Set Point Mode: Manual"}
    assert next(a for a in actions if a["param"] == "chws_sp")["value"] == 7.0
    assert next(a for a in actions if a["param"] == "chillers_sp")["value"] == 3
    assert all(set(a) == {"asset", "param", "value", "unit", "label"} for a in actions)
    # already in Manual: no mode write; unchanged set points: no write
    cond.modes = {"chws_reset_mode": 1.0, "staging_mode": 1.0}
    assert [a["param"] for a in optimizer.build_actions(cond, {**best, "chillers": 2.0})] == ["chws_sp", "approach_sp", "riser_dp_sp"]
    assert optimizer.build_actions(cond, {"chws_sp": 6.5, "approach_sp": 3.5, "riser_dp_sp": 120.0, "chillers": 2.0, "twv_min_pos": 0.0}) == []


def test_saving_threshold_is_max_of_1_percent_and_15_kw():
    assert optimizer.saving(1000, 985) == (15, 1.5, True)
    assert optimizer.saving(1000, 991)[2] is False  # 9 kW
    assert optimizer.saving(3000, 2980)[2] is False  # 20 kW but 0.67 %


def test_validation_spec_runs_baseline_and_recommended_from_live():
    actions = [{"asset": "CHWS_01", "param": "chws_reset_mode", "value": 1.0}, {"asset": "CHWS_01", "param": "chws_sp", "value": 7.0}]
    spec = optimizer.validation_spec(42, actions)
    assert spec["duration_min"] == 120 and spec["initial"] == "live" and spec["seed"] == 42 and spec["stochastic"] is False
    assert [v["name"] for v in spec["variants"]] == ["baseline", "recommended"]
    assert spec["variants"][0]["actions"] == []
    rec = spec["variants"][1]["actions"]
    assert [a["param"] for a in rec] == ["chws_reset_mode", "chws_sp"] and all(a["at_min"] == 0 and a["kind"] == "control" for a in rec)


def test_validation_verdicts():
    ok = {"success": True, "variants": {"baseline": {"avg_plant_kw": 1000, "success": True}, "recommended": {"avg_plant_kw": 960, "success": True}},
          "comparison": {"recommended": {"saving_kw": 40.0, "saving_kwh": 80.0, "saving_pct": 4.0}}}  # fmt: skip
    status, v = optimizer.judge(7, "SUCCEEDED", ok, None)
    assert status == "VALIDATED" and v["saving_kw"] == 40.0 and v["job_id"] == 7
    worse = ok | {"comparison": {"recommended": {"saving_kw": -5.0}}}
    assert optimizer.judge(7, "SUCCEEDED", worse, None)[0] == "REJECTED"
    violating = ok | {"success": False, "reason": "recommended: Riser DP below 80 kPa"}
    status, v = optimizer.judge(7, "SUCCEEDED", violating, None)
    assert status == "REJECTED" and "Riser DP" in v["reason"]

    def viol(name, label, minutes):
        return {"constraint": name, "label": label, "minutes": minutes, "longest_min": minutes, "critical": True}

    valve = viol("mau_valve_saturated", "MAU CHW valve at 100 %", 40.0)
    riser = viol("riser_dp_low", "Riser DP below 80 kPa", 20.0)
    both = {"success": False, "variants": {"baseline": {"avg_plant_kw": 1000, "success": False, "constraint_violations": [valve]},
            "recommended": {"avg_plant_kw": 960, "success": False, "constraint_violations": [valve | {"minutes": 43.0}]}},
            "comparison": {"recommended": {"saving_kw": 40.0}}}  # fmt: skip
    status, v = optimizer.judge(7, "SUCCEEDED", both, None)
    assert status == "VALIDATED" and v["success"] is True and v["violations"] == []
    assert "also in the baseline run" in v["note"] and v["baseline_violations"][0]["constraint"] == "mau_valve_saturated"
    longer = both | {"variants": both["variants"] | {"recommended": {"avg_plant_kw": 960, "constraint_violations": [valve | {"minutes": 60.0}]}}}
    status, v = optimizer.judge(7, "SUCCEEDED", longer, None)
    assert status == "REJECTED" and v["violations"][0]["minutes"] == 60.0
    caused = both | {"variants": both["variants"] | {"recommended": {"avg_plant_kw": 960, "constraint_violations": [valve, riser]}}}
    status, v = optimizer.judge(7, "SUCCEEDED", caused, None)
    assert status == "REJECTED" and [x["constraint"] for x in v["violations"]] == ["riser_dp_low"] and "Riser DP" in v["reason"]
    assert optimizer.judge(7, "FAILED", {}, "boom")[0] == "FAILED"


def test_doe_frame_reads_the_simulator_rows():
    rows = doe_rows(5)
    df = surrogate.doe_frame(rows + [{"inputs": {"wet_bulb": 25}, "outputs": {}}])
    assert len(df) == 5 and list(df.columns) == surrogate.FEATURES + surrogate.TARGETS
    assert df["dry_bulb"].iloc[0] == pytest.approx(rows[0]["inputs"]["wet_bulb"] + rows[0]["inputs"]["dry_bulb_minus_wb"])


def test_too_few_samples_refuse_training():
    with pytest.raises(ValueError):
        surrogate.train(surrogate.doe_frame(doe_rows(20)))


def test_monotone_constraints_hold(model):
    base = conditions().features(conditions().setpoints)
    grid = pd.DataFrame([{**base, "load_frac": lf} for lf in np.linspace(0.5, 0.97, 12)])
    kw = model.predict(grid)["plant_kw"]
    assert np.all(np.diff(kw) >= -1e-9)
    grid = pd.DataFrame([{**base, "chws_sp": sp} for sp in np.linspace(5.5, 8.0, 12)])
    assert np.all(np.diff(model.predict(grid)["chws_temp"]) >= -1e-9)
    assert math.isfinite(model.predict_one(base)["mau_sat_dev_max"])


class DoeJobs:
    """sim.jobs of the surrogate as the worker sees them (no database)."""

    def __init__(self, done: dict) -> None:
        self.done = done
        self.inserted: list[dict] = []
        self.marked: list[tuple[int, str]] = []
        self.trained: list[int] = []

    async def latest_job(self, purpose, statuses=None):  # noqa: ANN001
        job = self.done
        return job if not statuses or job["status"] in statuses else None

    async def insert_job(self, **kw):  # noqa: ANN003
        self.inserted.append(kw)
        return 900 + len(self.inserted)

    async def mark_unusable(self, job_id, reason):  # noqa: ANN001
        self.marked.append((job_id, reason))
        self.done["summary"] = {"surrogate": {"usable": False, "reason": reason}}

    async def train_and_store(self, job_id, *, include_live=True):  # noqa: ANN001
        self.trained.append(job_id)
        raise surrogate.UnusableDoe(f"DOE job {job_id} has 3 usable samples of 400 (+0 live), 60 needed")


async def test_an_unusable_doe_is_marked_and_replaced_after_a_back_off(monkeypatch):
    from datetime import timedelta

    now = datetime(2026, 10, 10, 12, 0, tzinfo=timezone.utc)
    jobs = DoeJobs({"id": 41, "status": "SUCCEEDED", "summary": {}, "finished_at": now - timedelta(hours=1)})

    async def fetch_one(sql, params=None):  # noqa: ANN001
        return None  # no active surrogate

    async def fetch_val(sql, params=None):  # noqa: ANN001
        return 0  # no model trained on the DOE yet

    monkeypatch.setattr(surrogate.db, "fetch_one", fetch_one)
    monkeypatch.setattr(surrogate.db, "fetch_val", fetch_val)
    monkeypatch.setattr(surrogate.data, "latest_job", jobs.latest_job)
    monkeypatch.setattr(surrogate.data, "insert_job", jobs.insert_job)
    monkeypatch.setattr(surrogate, "mark_unusable", jobs.mark_unusable)
    monkeypatch.setattr(surrogate, "train_and_store", jobs.train_and_store)
    # first pass: training fails for lack of usable rows -> marked, and the failure back-off applies
    assert await surrogate.run(now) == "backing off after unusable DOE job 41"
    assert jobs.trained == [41] and jobs.marked[0][0] == 41 and surrogate.unusable(jobs.done)
    # later passes never retrain the marked DOE; after the back-off a new DOE is requested
    assert await surrogate.run(now + timedelta(hours=2)) == "backing off after unusable DOE job 41"
    assert await surrogate.run(now + timedelta(hours=6)) == "requested DOE job 901 (no active surrogate)"
    assert jobs.trained == [41] and len(jobs.marked) == 1 and jobs.inserted[0]["kind"] == "doe"


async def test_too_few_usable_doe_rows_raise_unusable_doe(monkeypatch):
    async def fetch_all(sql, params=None):  # noqa: ANN001
        return doe_rows(5)

    monkeypatch.setattr(surrogate.db, "fetch_all", fetch_all)
    with pytest.raises(surrogate.UnusableDoe, match="5 usable samples of 5"):
        await surrogate.train_and_store(41, include_live=False)
