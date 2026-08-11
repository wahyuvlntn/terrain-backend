"""Supervisory set-point optimizer (DESIGN 6.1).

Every OPTIMIZER_INTERVAL_MIN (and for manual runs: `analytics.optimizer_runs`
PENDING rows, NOTIFY optimizer_runs) the optimizer

1. reads the current conditions (15-minute means of live data: wet bulb, dry
   bulb, cooling load, staging, set points, modes, measured plant power);
2. searches the set points with Optuna TPE (300 trials) on the active surrogate:
   `chws_sp` 5.5-8.0, `approach_sp` 2.0-6.0, `riser_dp_sp` 80-160, `chillers`
   (feasible staging n x 3517 x 0.95 >= load), `twv_min_pos` 0-30, each within
   its per-run step limit around the current value (chws_sp +/-0.5 K,
   approach_sp +/-1 K, riser_dp_sp +/-20 kPa); constraints via
   `trial.set_constraint`: chws_temp <= 7.5 C, mau_valve_max <= 95 %,
   riser_dp >= 95 kPa, each backed off by the surrogate's hold-out RMSE of
   that output (at most 5 % of the limit);
3. recommends only when the predicted saving >= max(1 %, 15 kW): writes an
   `analytics.recommendations` row with the actions (modes to Manual first,
   then set points) and queues a 2-variant validation job on the twin
   (baseline / recommended, 2 h, same seed, initial live, origin optimizer);
4. turns VALIDATING into VALIDATED (twin saving > 0 and job success) or
   REJECTED when the job finishes (FAILED when the job failed), and expires
   open recommendations after `valid_until`.
"""

from __future__ import annotations

import asyncio
import logging
import math
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

import numpy as np
import optuna

from ..common import db
from ..common.config import get_settings
from . import data, surrogate
from .surrogate import LOAD_BASE_KW, MAU_SAT_SP, Surrogate

log = logging.getLogger(__name__)
optuna.logging.set_verbosity(optuna.logging.WARNING)

N_TRIALS = 300
CHILLER_KW = 3517.0
STAGING_MARGIN = 0.95
MIN_SAVING_PCT = 1.0
MIN_SAVING_KW = 15.0
VALIDATION_MIN = 120
CONDITION_WINDOW_MIN = 15
STALE_DATA_MIN = 5
MAX_MARGIN_FRACTION = 0.05  # a constraint back-off never exceeds 5 % of its limit


@dataclass(frozen=True)
class Decision:
    key: str  # set point key in recommendations (setpoints)
    param: str  # CHWS_01 parameter written
    lo: float
    hi: float
    step: float
    max_step: float | None  # per-run change limit
    unit: str
    label: str
    mode: str | None = None  # gating mode parameter (option 1 = Manual)
    mode_label: str = ""


DECISIONS: tuple[Decision, ...] = (
    Decision("chws_sp", "chws_sp", 5.5, 8.0, 0.1, 0.5, "°C", "CHW Supply Set Point", "chws_reset_mode", "CHW Set Point Mode"),
    Decision("approach_sp", "approach_sp", 2.0, 6.0, 0.1, 1.0, "K", "Tower Approach Set Point"),
    Decision("riser_dp_sp", "riser_dp_sp", 80.0, 160.0, 1.0, 20.0, "kPa", "Riser DP Set Point"),
    Decision("chillers", "chillers_sp", 1.0, 3.0, 1.0, None, "", "Duty Chillers Set Point", "staging_mode", "Chiller Staging Mode"),
    Decision("twv_min_pos", "twv_min_pos", 0.0, 30.0, 1.0, None, "%", "3-Way Valve Min Position"),
)
CONSTRAINTS: tuple[tuple[str, str, float], ...] = (
    ("chws_temp", "<=", 7.5),
    ("mau_valve_max", "<=", 95.0),
    ("riser_dp", ">=", 95.0),
)
CONSTRAINT_INFO: dict[str, tuple[str, str]] = {  # label, unit
    "chws_temp": ("CHW supply header temperature", "°C"),
    "mau_valve_max": ("Highest MAU CHW valve position", "%"),
    "riser_dp": ("Riser differential pressure", "kPa"),
}
MANUAL = 1.0


@dataclass
class Conditions:
    wet_bulb: float
    dry_bulb: float
    load_frac: float
    cooling_load_kw: float
    chillers_running: int
    setpoints: dict[str, float]  # chws_sp, approach_sp, riser_dp_sp, chillers, twv_min_pos, cw_min_temp
    modes: dict[str, float] = field(default_factory=dict)  # chws_reset_mode, staging_mode
    measured: dict[str, float | None] = field(default_factory=dict)
    window_start: datetime | None = None
    window_end: datetime | None = None
    system_alias: str = "CHWS_01"

    def features(self, sp: dict[str, float]) -> dict[str, float]:
        return {
            "wet_bulb": self.wet_bulb,
            "dry_bulb": self.dry_bulb,
            "load_frac": self.load_frac,
            "chws_sp": sp["chws_sp"],
            "approach_sp": sp["approach_sp"],
            "riser_dp_sp": sp["riser_dp_sp"],
            "chillers": sp["chillers"],
            "twv_min_pos": sp["twv_min_pos"],
            "cw_min_temp": self.setpoints["cw_min_temp"],
        }

    def as_json(self) -> dict[str, Any]:
        return {
            "wet_bulb": round(self.wet_bulb, 2),
            "dry_bulb": round(self.dry_bulb, 2),
            "load_frac": round(self.load_frac, 4),
            "cooling_load_kw": round(self.cooling_load_kw, 1),
            "chillers_running": self.chillers_running,
            "cw_min_temp": self.setpoints.get("cw_min_temp"),
            "modes": self.modes,
            "measured": self.measured,
            "window": {"start": self.window_start, "end": self.window_end},
        }


@dataclass
class OptResult:
    best: dict[str, float] | None
    predicted_best: dict[str, float] | None
    predicted_current: dict[str, float]
    n_trials: int
    n_feasible: int
    bounds: dict[str, list[float]]
    feasible_chillers: list[int]
    seconds: float
    margins: dict[str, float] = field(default_factory=dict)


def _grid(lo: float, hi: float, step: float) -> tuple[float, float]:
    """Bounds aligned to the step grid (Optuna requires (high - low) to be a multiple of step)."""
    a = math.ceil(round(lo / step, 6)) * step
    b = math.floor(round(hi / step, 6)) * step
    return round(a, 6), round(max(a, b), 6)


def feasible_chillers(cooling_load_kw: float) -> list[int]:
    ok = [n for n in (1, 2, 3) if n * CHILLER_KW * STAGING_MARGIN >= cooling_load_kw]
    return ok or [3]


def decision_bounds(cond: Conditions) -> dict[str, tuple[float, float]]:
    """Search range per set point: the decision range within the per-run step
    limit around the current value. A current value further outside the range
    than one step limit moves one step limit toward it (on the step grid, never
    further), so no recommended change exceeds max_step."""
    out = {}
    for d in DECISIONS:
        if d.key == "chillers":
            continue
        cur = cond.setpoints[d.key]
        lo, hi = d.lo, d.hi
        if d.max_step is not None:
            lo, hi = max(lo, cur - d.max_step), min(hi, cur + d.max_step)
            if lo > hi:  # current value more than max_step outside the range
                n, k = cur / d.step, d.max_step / d.step
                v = (math.ceil(round(n - k, 6)) if cur > d.hi else math.floor(round(n + k, 6))) * d.step
                out[d.key] = (round(v, 6), round(v, 6))
                continue
        out[d.key] = _grid(lo, hi, d.step)
    return out


def _violation(pred: dict[str, float], margins: dict[str, float] | None = None) -> dict[str, float]:
    """Constraint values (<= 0 feasible), each backed off by its margin (surrogate uncertainty)."""
    out = {}
    for name, op, limit in CONSTRAINTS:
        v, m = pred[name], (margins or {}).get(name, 0.0)
        out[name] = v + m - limit if op == "<=" else limit - (v - m)
    return out


def constraint_margins(model: Surrogate) -> dict[str, float]:
    """Back-off per constraint = the surrogate's hold-out RMSE of that output (capped), so a
    recommendation stays feasible on the plant when the model is off by a typical error."""
    targets = (model.metrics or {}).get("targets") or {}
    out = {}
    for name, _, limit in CONSTRAINTS:
        rmse = (targets.get(name) or {}).get("rmse")
        out[name] = round(min(float(rmse), MAX_MARGIN_FRACTION * abs(limit)), 4) if isinstance(rmse, (int, float)) and math.isfinite(rmse) else 0.0
    return out


def optimize(model: Surrogate, cond: Conditions, *, n_trials: int = N_TRIALS, seed: int = 0) -> OptResult:
    """TPE search of the supervisory set points on the surrogate (pure)."""
    t0 = time.monotonic()
    bounds = decision_bounds(cond)
    chillers = feasible_chillers(cond.cooling_load_kw)
    current_sp = {k: cond.setpoints[k] for k in ("chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos")}
    current_sp["chillers"] = float(cond.chillers_running)
    pred_current = model.predict_one(cond.features(current_sp))
    steps = {d.key: d.step for d in DECISIONS}
    margins = constraint_margins(model)

    def objective(trial: optuna.Trial) -> float:
        sp = {k: trial.suggest_float(k, lo, hi, step=steps[k]) if hi > lo else lo for k, (lo, hi) in bounds.items()}
        sp["chillers"] = float(trial.suggest_categorical("chillers", chillers) if len(chillers) > 1 else chillers[0])
        pred = model.predict_one(cond.features(sp))
        for name, v in _violation(pred, margins).items():
            trial.set_constraint(name, v)
        trial.set_user_attr("pred", pred)
        trial.set_user_attr("sp", sp)
        return pred["plant_kw"]

    sampler = optuna.samplers.TPESampler(seed=seed, n_startup_trials=30, multivariate=True)
    study = optuna.create_study(direction="minimize", sampler=sampler)
    start = {k: float(min(max(current_sp[k], lo), hi)) for k, (lo, hi) in bounds.items() if hi > lo}
    if len(chillers) > 1:
        start["chillers"] = int(cond.chillers_running) if cond.chillers_running in chillers else chillers[-1]
    study.enqueue_trial(start)
    study.optimize(objective, n_trials=n_trials, show_progress_bar=False)
    feasible = [t for t in study.trials if t.value is not None and all(v <= 0 for v in t.constraints.values())]
    best = min(feasible, key=lambda t: t.value) if feasible else None
    return OptResult(
        best={k: round(float(v), 4) for k, v in best.user_attrs["sp"].items()} if best else None,
        predicted_best=best.user_attrs["pred"] if best else None,
        predicted_current=pred_current,
        n_trials=len(study.trials),
        n_feasible=len(feasible),
        bounds={k: list(v) for k, v in bounds.items()},
        feasible_chillers=chillers,
        seconds=round(time.monotonic() - t0, 3),
        margins=margins,
    )


def saving(pred_current_kw: float, pred_best_kw: float) -> tuple[float, float, bool]:
    """(saving kW, saving %, worth recommending)."""
    kw = pred_current_kw - pred_best_kw
    pct = kw / pred_current_kw * 100 if pred_current_kw > 0 else 0.0
    return kw, pct, kw >= max(MIN_SAVING_PCT / 100 * pred_current_kw, MIN_SAVING_KW)


def build_actions(cond: Conditions, best: dict[str, float]) -> list[dict[str, Any]]:
    """Writes to apply, in order: modes to Manual where needed, then set points."""
    modes: list[dict[str, Any]] = []
    sps: list[dict[str, Any]] = []
    for d in DECISIONS:
        cur = float(cond.chillers_running) if d.key == "chillers" else cond.setpoints[d.key]
        new = best[d.key]
        if abs(new - cur) < d.step / 2:
            continue
        if d.mode and cond.modes.get(d.mode, 0.0) != MANUAL:
            modes.append({"asset": cond.system_alias, "param": d.mode, "value": MANUAL, "unit": "", "label": f"{d.mode_label}: Manual"})
        fmt = "{:.0f}" if d.step >= 1 else "{:.1f}"
        new_v = round(new) if d.step >= 1 else round(new, 1)
        sps.append(
            {
                "asset": cond.system_alias,
                "param": d.param,
                "value": new_v,
                "unit": d.unit,
                "label": f"{d.label}: {fmt.format(cur)} → {fmt.format(new_v)} {d.unit}".rstrip(),
            }
        )
    return modes + sps


def _kw_per_rt(plant_kw: float, cooling_load_kw: float) -> float | None:
    return round(plant_kw / (cooling_load_kw / 3.517), 4) if cooling_load_kw > 0 else None


def _pred_json(pred: dict[str, float], cond: Conditions) -> dict[str, Any]:
    out = {k: round(float(v), 3) for k, v in pred.items()}
    out["kw_per_rt"] = _kw_per_rt(pred["plant_kw"], cond.cooling_load_kw)
    return out


# ---------------------------------------------------------------------------
# Live conditions

SYSTEM_POINTS = [
    "wet_bulb", "oa_temp", "cooling_load", "chillers_running", "chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos",
    "cw_min_temp", "chws_reset_mode", "staging_mode", "plant_kw", "chws_temp", "riser_dp",
]  # fmt: skip
REQUIRED = ["wet_bulb", "oa_temp", "cooling_load", "chillers_running", "chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos", "cw_min_temp"]


async def current_conditions(site: str | None = None, now: datetime | None = None) -> tuple[Conditions | None, str]:
    """15-minute means of the live plant (or None with the reason)."""
    plant = await data.load_plant(site or data.settings_site())
    systems = plant.by_role("chws")
    if not systems:
        return None, "no chilled water plant controller in this site"
    sys_ = systems[0].alias
    end = (now or data.utcnow()).replace(second=0, microsecond=0)
    start = end - timedelta(minutes=CONDITION_WINDOW_MIN)
    wanted: dict[str, list[str]] = {sys_: SYSTEM_POINTS}
    for m in plant.by_role("mau"):
        wanted[m.alias] = ["chw_valve", "sat", "running"]
    frame = await data.live_frame(plant, wanted, start, end)

    def mean(alias: str, p: str) -> float | None:
        v = frame.get(alias, p)
        if v is None:
            return None
        w = v[np.isfinite(v)]
        return float(np.mean(w)) if len(w) >= 5 else None

    vals = {p: mean(sys_, p) for p in SYSTEM_POINTS}
    missing = [p for p in REQUIRED if vals[p] is None]
    if missing:
        return None, f"no recent live data for {sys_}: {', '.join(missing)}"
    latest = max((int(np.flatnonzero(np.isfinite(v))[-1]) for v in frame.data.values() if np.isfinite(v).any()), default=-1)
    if latest < 0 or (frame.n - 1 - latest) * frame.step_s / 60 > STALE_DATA_MIN:
        return None, "live data is stale"
    valves, devs = [], []
    for m in plant.by_role("mau"):
        run = mean(m.alias, "running")
        if run is not None and run < 0.5:
            continue
        v, s = mean(m.alias, "chw_valve"), mean(m.alias, "sat")
        if v is not None:
            valves.append(v)
        if s is not None:
            devs.append(abs(s - MAU_SAT_SP))
    load = float(vals["cooling_load"])
    cond = Conditions(
        wet_bulb=float(vals["wet_bulb"]),
        dry_bulb=float(vals["oa_temp"]),
        load_frac=load / LOAD_BASE_KW,
        cooling_load_kw=load,
        chillers_running=int(round(float(vals["chillers_running"]))),
        setpoints={k: round(float(vals[k]), 3) for k in ("chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos", "cw_min_temp")}
        | {"chillers": float(round(float(vals["chillers_running"])))},
        modes={k: float(round(vals[k])) for k in ("chws_reset_mode", "staging_mode") if vals[k] is not None},
        measured={
            "plant_kw": _r(vals["plant_kw"]),
            "chws_temp": _r(vals["chws_temp"]),
            "riser_dp": _r(vals["riser_dp"]),
            "mau_valve_max": _r(max(valves)) if valves else None,
            "mau_sat_dev_max": _r(max(devs)) if devs else None,
        },
        window_start=start,
        window_end=end,
        system_alias=sys_,
    )
    return cond, ""


def _r(v: float | None, d: int = 3) -> float | None:
    return None if v is None or not math.isfinite(v) else round(float(v), d)


# ---------------------------------------------------------------------------
# Runs


async def _finish(run_id: int, status: str, *, metrics: dict[str, Any] | None = None, error: str | None = None, model_id: int | None = None, n_trials: int | None = None) -> None:
    await db.execute(
        "UPDATE analytics.optimizer_runs SET status = %s, metrics = %s, error = %s, model_id = COALESCE(%s, model_id),"
        " n_trials = COALESCE(%s, n_trials), finished_at = now() WHERE id = %s",
        (status, data.jb(metrics or {}), error, model_id, n_trials, run_id),
    )


@dataclass
class Outcome:
    """Result of one optimizer evaluation (before anything is written)."""

    status: str  # SUCCEEDED | SKIPPED
    metrics: dict[str, Any] = field(default_factory=dict)
    error: str | None = None
    model: Surrogate | None = None
    cond: Conditions | None = None
    res: OptResult | None = None
    actions: list[dict[str, Any]] = field(default_factory=list)
    saving_kw: float = 0.0
    saving_pct: float = 0.0
    recommend: bool = False


async def evaluate(site: str, *, seed: int = 0) -> Outcome:
    """Conditions, surrogate search and the recommend decision (reads only)."""
    model = await surrogate.load_active()
    if model is None:
        return Outcome("SKIPPED", error="no surrogate model yet (waiting for the DOE job on the twin)")
    cond, reason = await current_conditions(site)
    if cond is None:
        return Outcome("SKIPPED", error=reason, model=model)
    cur_feats = cond.features({**cond.setpoints, "chillers": float(cond.chillers_running)})
    outside = model.out_of_range(cur_feats)
    metrics: dict[str, Any] = {"conditions": cond.as_json(), "model_version": model.version}
    if outside:
        metrics["outside_training_range"] = outside
        return Outcome("SKIPPED", metrics, f"conditions outside the surrogate training range: {', '.join(outside)}", model, cond)
    res = await asyncio.to_thread(optimize, model, cond, seed=seed)
    measured_kw = cond.measured.get("plant_kw")
    pred_cur_kw = res.predicted_current["plant_kw"]
    metrics |= {
        "n_feasible": res.n_feasible,
        "bounds": res.bounds,
        "feasible_chillers": res.feasible_chillers,
        "constraint_margins": res.margins,
        "seconds": res.seconds,
        "predicted_current": _pred_json(res.predicted_current, cond),
    }
    if measured_kw:
        metrics["model_error_pct"] = round((pred_cur_kw - measured_kw) / measured_kw * 100, 3)
    out = Outcome("SUCCEEDED", metrics, None, model, cond, res)
    if res.best is None or res.predicted_best is None:
        metrics |= {"recommended": False, "reason": "no feasible set points satisfy the constraints"}
        return out
    kw, pct, worth = saving(pred_cur_kw, res.predicted_best["plant_kw"])
    metrics |= {"best": res.best, "predicted_best": _pred_json(res.predicted_best, cond), "saving_kw": round(kw, 2), "saving_pct": round(pct, 3)}
    actions = build_actions(cond, res.best)
    out.actions, out.saving_kw, out.saving_pct = actions, kw, pct
    if not worth or not actions:
        why = f"predicted saving {kw:.1f} kW ({pct:.2f} %) below max(1 %, 15 kW)" if actions else "current set points are already optimal"
        metrics |= {"recommended": False, "reason": why}
        return out
    out.recommend = True
    return out


async def execute(run_id: int, *, site: str | None = None) -> str:
    """Execute one claimed (RUNNING) optimizer run; returns its final status.

    A run interrupted by the task timeout or a shutdown (cancellation) is
    marked FAILED before the cancellation propagates; one lost to a killed
    process is failed later by `sweep_stale`.
    """
    site = site or data.settings_site()
    try:
        o = await evaluate(site, seed=run_id)
        model_id = o.model.id if o.model else None
        n_trials = o.res.n_trials if o.res else None
        if o.recommend:
            assert o.model is not None and o.cond is not None and o.res is not None
            rec_id, job_id = await recommend(run_id, site, o.model, o.cond, o.res, o.actions, o.saving_kw, o.saving_pct)
            o.metrics |= {"recommended": True, "recommendation_id": rec_id, "validation_job_id": job_id}
        await _finish(run_id, o.status, metrics=o.metrics, error=o.error, model_id=model_id, n_trials=n_trials)
        return o.status
    except Exception as err:  # noqa: BLE001 - recorded on the run
        log.exception("optimizer run failed id=%s", run_id)
        await _finish(run_id, "FAILED", error=str(err)[:500])
        return "FAILED"
    except BaseException as err:  # cancelled (iteration timeout, shutdown): never leave the run RUNNING
        log.warning("optimizer run interrupted id=%s (%s)", run_id, type(err).__name__)
        try:
            await asyncio.wait_for(asyncio.shield(_finish(run_id, "FAILED", error=f"interrupted ({type(err).__name__})")), timeout=10)
        except BaseException:  # noqa: BLE001 - best effort; sweep_stale fails it later
            log.warning("could not mark interrupted optimizer run id=%s", run_id, exc_info=True)
        raise


STALE_RUN_MIN = 20  # longer than the optimizer task timeout (15 min)


async def sweep_stale() -> int:
    """Fail PENDING / RUNNING runs older than STALE_RUN_MIN (lost to a killed worker)."""
    n = await db.execute(
        "UPDATE analytics.optimizer_runs SET status = 'FAILED', error = 'interrupted (stale)', finished_at = now()"
        " WHERE status IN ('PENDING', 'RUNNING') AND COALESCE(started_at, created_at) < now() - make_interval(mins => %s)",
        (STALE_RUN_MIN,),
    )
    if n:
        log.warning("stale optimizer runs failed count=%s", n)
    return n


async def preview(site: str | None = None) -> dict[str, Any]:
    """What a run would do now, without writing anything (worker --dry-run)."""
    o = await evaluate(site or data.settings_site())
    return {"status": o.status, "error": o.error, "would_recommend": o.recommend, "actions": o.actions, "metrics": o.metrics}


async def recommend(run_id: int, site: str, model: Surrogate, cond: Conditions, res: OptResult, actions: list[dict[str, Any]], kw: float, pct: float) -> tuple[int, int]:
    """Store the recommendation (PROPOSED), supersede older open ones and queue its twin validation."""
    assert res.best is not None and res.predicted_best is not None
    cur_sp = {**{k: cond.setpoints[k] for k in ("chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos")}, "chillers": cond.chillers_running}
    constraints = []
    for name, op, limit in CONSTRAINTS:
        label, unit = CONSTRAINT_INFO[name]
        constraints.append(
            {
                "name": name,
                "label": label,
                "op": op,
                "limit": limit,
                "unit": unit,
                "predicted": round(res.predicted_best[name], 3),
                "current": round(res.predicted_current[name], 3),
                "recommended": round(res.predicted_best[name], 3),
                "margin": res.margins.get(name, 0.0),
                "ok": _violation(res.predicted_best, res.margins)[name] <= 0,
            }
        )
    changes = "; ".join(a["label"] for a in actions if a["param"] not in ("chws_reset_mode", "staging_mode"))
    rationale = (
        f"At wet bulb {cond.wet_bulb:.1f} °C and {cond.cooling_load_kw:.0f} kW cooling load the surrogate "
        f"({model.version}) predicts {res.predicted_best['plant_kw']:.0f} kW plant power with: {changes}, against "
        f"{res.predicted_current['plant_kw']:.0f} kW at the current set points (saving {kw:.0f} kW, {pct:.1f} %), "
        f"with CHW supply {res.predicted_best['chws_temp']:.2f} °C, riser DP {res.predicted_best['riser_dp']:.0f} kPa "
        f"and MAU valves up to {res.predicted_best['mau_valve_max']:.0f} %."
    )
    interval = get_settings().optimizer_interval_min
    valid_until = data.utcnow() + timedelta(minutes=max(2 * interval, VALIDATION_MIN))
    row = await db.fetch_one(
        """INSERT INTO analytics.recommendations (run_id, site_code, status, conditions, current, recommended, actions,
               savings_kw, savings_pct, constraints, model_id, rationale, valid_until)
           VALUES (%s, %s, 'PROPOSED', %s, %s, %s, %s, %s, %s, %s, %s, %s, %s) RETURNING id""",
        (
            run_id, site, data.jb(cond.as_json()),
            data.jb({"setpoints": cur_sp, "predicted": _pred_json(res.predicted_current, cond)}),
            data.jb({"setpoints": res.best, "predicted": _pred_json(res.predicted_best, cond)}),
            data.jb(actions), round(kw, 2), round(pct, 3), data.jb(constraints), model.id, rationale, valid_until,
        ),
    )  # fmt: skip
    assert row is not None
    rec_id = int(row["id"])
    await db.execute(
        "UPDATE analytics.recommendations SET status = 'EXPIRED',"
        " validation = COALESCE(validation, '{}'::jsonb) || jsonb_build_object('expired_reason', %s::text)"
        " WHERE site_code = %s AND id <> %s AND status IN ('PROPOSED', 'VALIDATING', 'VALIDATED')",
        (f"superseded by recommendation {rec_id}", site, rec_id),
    )
    spec = validation_spec(rec_id, actions)
    job_id = await data.insert_job(
        kind="scenario",
        title=f"Validate recommendation #{rec_id} (baseline vs recommended, 2 h)",
        origin="optimizer",
        spec=spec,
        rationale=rationale,
        recommendation_id=rec_id,
    )
    await db.execute(
        "UPDATE analytics.recommendations SET status = 'VALIDATING', validation_job_id = %s WHERE id = %s AND status = 'PROPOSED'",
        (job_id, rec_id),
    )
    log.info("recommendation id=%s saving=%.1f kW (%.2f %%) validation job=%s", rec_id, kw, pct, job_id)
    return rec_id, job_id


def validation_spec(rec_id: int, actions: list[dict[str, Any]]) -> dict[str, Any]:
    """Baseline keeps the live set points; recommended applies the actions at t = 0 (same seed)."""
    return {
        "start": None,
        "duration_min": VALIDATION_MIN,
        "initial": "live",
        "stochastic": False,
        "seed": rec_id,
        "weather": {"dry_bulb_offset": 0.0, "wet_bulb_offset": 0.0},
        "load_scale": 1.0,
        "actions": [],
        "variants": [
            {"name": "baseline", "actions": []},
            {
                "name": "recommended",
                "actions": [{"at_min": 0, "kind": "control", "asset": a["asset"], "param": a["param"], "value": a["value"]} for a in actions],
            },
        ],
        "purpose": "validation",
    }


async def complete_validations(*, dry_run: bool = False) -> Any:
    """VALIDATING recommendations whose validation job finished -> VALIDATED / REJECTED / FAILED."""
    rows = await db.fetch_all(
        """SELECT r.id, r.validation_job_id, r.savings_kw, j.status AS job_status, j.summary, j.error
           FROM analytics.recommendations r JOIN sim.jobs j ON j.id = r.validation_job_id
           WHERE r.status = 'VALIDATING' AND j.status IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'REJECTED')"""
    )
    if dry_run:
        return [{"recommendation": r["id"], "verdict": judge(r["validation_job_id"], r["job_status"], r["summary"] or {}, r["error"])} for r in rows]
    n = 0
    for r in rows:
        status, validation = judge(r["validation_job_id"], r["job_status"], r["summary"] or {}, r["error"])
        changed = await db.execute(
            "UPDATE analytics.recommendations SET status = %s, validation = %s WHERE id = %s AND status = 'VALIDATING'",
            (status, data.jb(validation), r["id"]),
        )
        if changed:
            n += 1
            log.info("recommendation id=%s %s (%s)", r["id"], status, validation.get("reason"))
    return n


WORSE_TOLERANCE_MIN = 5.0  # a recommendation may extend a pre-existing violation by this much (sampling noise)


def caused_violations(base: dict[str, Any], rec: dict[str, Any]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """(caused, pre-existing) critical violations of the recommended variant.

    A violation the baseline run has too (for example a MAU valve saturated on
    a hot afternoon whatever the plant set points) is pre-existing unless the
    recommendation makes it clearly longer; anything else was caused by it.
    """
    baseline = {v.get("constraint"): v for v in base.get("constraint_violations") or [] if v.get("critical")}
    caused: list[dict[str, Any]] = []
    existing: list[dict[str, Any]] = []
    for v in rec.get("constraint_violations") or []:
        if not v.get("critical"):
            continue
        b = baseline.get(v.get("constraint"))
        if b is not None and float(v.get("minutes") or 0) <= float(b.get("minutes") or 0) + WORSE_TOLERANCE_MIN:
            existing.append(v)
        else:
            caused.append(v)
    return caused, existing


def judge(job_id: int, job_status: str, summary: dict[str, Any], error: str | None) -> tuple[str, dict[str, Any]]:
    """Validation verdict from a finished 2-variant job.

    The recommendation is judged against the baseline run of the same plant
    state: it is rejected for constraint violations it causes or worsens, not
    for ones the plant has anyway.
    """
    if job_status != "SUCCEEDED":
        return "FAILED", {"job_id": job_id, "status": job_status, "error": error, "reason": f"validation job {job_status.lower()}"}
    variants = summary.get("variants") or {}
    base, rec = variants.get("baseline") or {}, variants.get("recommended") or {}
    comp = (summary.get("comparison") or {}).get("recommended") or {}
    saving_kw = comp.get("saving_kw")
    if saving_kw is None and base.get("avg_plant_kw") is not None and rec.get("avg_plant_kw") is not None:
        saving_kw = float(base["avg_plant_kw"]) - float(rec["avg_plant_kw"])
    saving_kwh = comp.get("saving_kwh")
    caused, existing = caused_violations(base, rec)
    if rec.get("constraint_violations") is None and (summary.get("success") is False or rec.get("success") is False):
        # no per-variant detail: fall back to the job's overall verdict
        caused = [{"label": summary.get("reason") or "plant constraint"}]
    success = not caused
    out = {
        "job_id": job_id,
        "status": job_status,
        "success": success,
        "saving_kw": saving_kw,
        "saving_kwh": saving_kwh,
        "saving_pct": comp.get("saving_pct"),
        "baseline": {k: base.get(k) for k in ("energy_kwh", "avg_plant_kw", "avg_kw_per_rt", "max_chws_temp", "min_riser_dp", "max_mau_valve", "success")},
        "recommended": {k: rec.get(k) for k in ("energy_kwh", "avg_plant_kw", "avg_kw_per_rt", "max_chws_temp", "min_riser_dp", "max_mau_valve", "success")},
        "violations": caused,
        "baseline_violations": existing,
    }
    if existing:
        out["note"] = (
            ", ".join(v.get("label", "constraint") for v in existing)
            + " also in the baseline run (the plant as it is); the recommendation does not make it worse"
        )
    if not success:
        out["reason"] = "the recommended set points caused: " + ", ".join(v.get("label", "constraint") for v in caused)
        return "REJECTED", out
    if saving_kw is None or saving_kw <= 0:
        out["reason"] = "no energy saving on the twin"
        return "REJECTED", out
    out["reason"] = f"twin saving {saving_kw:.1f} kW over 2 h"
    return "VALIDATED", out


async def expire(*, dry_run: bool = False) -> int:
    if dry_run:
        return int(
            await db.fetch_val(
                "SELECT count(*) FROM analytics.recommendations WHERE status IN ('PROPOSED', 'VALIDATING', 'VALIDATED') AND valid_until < now()"
            )
            or 0
        )
    n = await db.execute(
        "UPDATE analytics.recommendations SET status = 'EXPIRED',"
        " validation = COALESCE(validation, '{}'::jsonb) || jsonb_build_object('expired_reason', 'valid_until passed')"
        " WHERE status IN ('PROPOSED', 'VALIDATING', 'VALIDATED') AND valid_until < now()"
    )
    if n:
        log.info("recommendations expired count=%s", n)
    return n


async def claim_manual() -> int | None:
    row = await db.fetch_one(
        "UPDATE analytics.optimizer_runs SET status = 'RUNNING', started_at = now()"
        " WHERE id = (SELECT id FROM analytics.optimizer_runs WHERE status = 'PENDING' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)"
        " RETURNING id"
    )
    return int(row["id"]) if row else None


async def run_manual(*, dry_run: bool = False) -> int:
    """Execute pending manual / agent runs (dry run: count them)."""
    if dry_run:
        return int(await db.fetch_val("SELECT count(*) FROM analytics.optimizer_runs WHERE status = 'PENDING'") or 0)
    await sweep_stale()
    n = 0
    while (run_id := await claim_manual()) is not None:
        await execute(run_id)
        n += 1
    return n


async def run_scheduled(now: datetime | None = None, *, dry_run: bool = False) -> Any:
    """Start a scheduled run when the interval has elapsed since the last one (dry run: preview it)."""
    now = now or data.utcnow()
    interval = timedelta(minutes=max(1, get_settings().optimizer_interval_min))
    last = await db.fetch_val("SELECT max(created_at) FROM analytics.optimizer_runs WHERE trigger = 'schedule'")
    due = last is None or now - last >= interval
    if dry_run:
        return {"due": due, "last_scheduled_run": last, **(await preview())}
    if not due:
        return "not due"
    row = await db.fetch_one(
        "INSERT INTO analytics.optimizer_runs (trigger, status, started_at) VALUES ('schedule', 'RUNNING', now()) RETURNING id"
    )
    assert row is not None
    status = await execute(int(row["id"]))
    return f"run {row['id']} {status}"
