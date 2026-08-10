"""FDD rule calibration on fault-free digital-twin data (DESIGN 6.1).

On start (when a rule was never calibrated) and weekly, the worker requests a
nominal, fault-free scenario job (kind scenario, origin analytics, 72 h,
`initial: nominal`, `stochastic: false`). When it has SUCCEEDED, the rule
statistics are computed on its measured series with exactly the gating used
for detection (`faults.py`) and written to `analytics.fault_rules`:

* residual offsets = median of the statistic on fault-free data;
* thresholds = max(engineering floor, K_SIGMA x robust sigma), K_SIGMA = 4
  (rules.K_SIGMA; robust sigma = 1.4826 MAD of the gated, smoothed statistic);
  the hunting statistics (CHS-008) use max(floor, p99 + 2) reversals and
  max(floor, 1.25 x p99) command swing, and the fault-free median for the
  leaving-water swing (a plausibility check, see cal_chs008);
* model fits: the expected tower approach (CT-014, log-linear least squares)
  and the riser DP pump curve (CHS-005, dp = a N^2 + b N q + c q^2).

The engineering defaults are floors: calibration only makes a rule less
sensitive than its default, never more. A rule edited by an administrator
(`updated_by` set) keeps its params; only `calibration` / `calibrated_at` are
written for reference. The check is part of the UPDATE, and the calibrated
keys are merged into the stored params, so an edit made while a calibration
runs is never overwritten.
"""

from __future__ import annotations

import logging
import math
from collections.abc import Callable
from datetime import datetime, timedelta
from typing import Any

import numpy as np

from ..common import db
from . import data, faults, rules
from .series import Frame, robust_center_scale

log = logging.getLogger(__name__)

PURPOSE = "calibration"
DURATION_MIN = 4320  # 72 h nominal run
FALLBACK_DURATION_MIN = 2880  # if the runner refuses runs beyond its UI limit
RECALIBRATE_AFTER = timedelta(days=7)
RETRY_AFTER_FAILURE = timedelta(hours=6)
MIN_SAMPLES = 120


def job_spec(duration_min: int = DURATION_MIN) -> dict[str, Any]:
    return {
        "start": None,
        "duration_min": duration_min,
        "initial": "nominal",
        "stochastic": False,
        "seed": 0,
        "weather": {"dry_bulb_offset": 0.0, "wet_bulb_offset": 0.0},
        "load_scale": 1.0,
        "actions": [],
        "variants": [{"name": "baseline", "actions": []}],
        "purpose": PURPOSE,
    }


# ---------------------------------------------------------------------------
# Statistics per rule (pure)


def _pool(arrays: list[np.ndarray]) -> np.ndarray:
    vals = [a[np.isfinite(a)] for a in arrays if a is not None]
    return np.concatenate(vals) if vals else np.array([])


def _summary(v: np.ndarray) -> dict[str, Any]:
    if len(v) == 0:
        return {"n": 0}
    c, s = robust_center_scale(v)
    return {
        "n": int(len(v)),
        "median": round(c, 5),
        "robust_sigma": round(s, 5),
        "mean": round(float(np.mean(v)), 5),
        "std": round(float(np.std(v)), 5),
        "p01": round(float(np.quantile(v, 0.01)), 5),
        "p99": round(float(np.quantile(v, 0.99)), 5),
    }


def _thr(floor: float, sigma: float) -> float:
    return round(max(float(floor), rules.K_SIGMA * sigma if math.isfinite(sigma) else 0.0), 4)


Result = tuple[dict[str, Any], dict[str, Any]]  # (param updates, statistics)


def cal_chl011(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    st = faults.chl011_stats(frame, lay, {**p, "offset_k": 0.0, "mix_offset_k": 0.0})
    e = _pool([s["ewma"] for s in st.values()])
    m = _pool([np.where(s["gate"], s["mix_ewma"], np.nan) for s in st.values()])
    se, sm = _summary(e), _summary(m)
    if se["n"] < MIN_SAMPLES:
        return {}, {"residual": se, "mixing": sm, "note": "not enough running samples"}
    upd = {"offset_k": se["median"], "threshold_k": _thr(d["threshold_k"], se["robust_sigma"])}
    if sm["n"] >= MIN_SAMPLES:
        upd |= {"mix_offset_k": sm["median"], "mix_threshold_k": _thr(d["mix_threshold_k"], sm["robust_sigma"])}
    return upd, {"residual": se, "mixing": sm}


def cal_ct013(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    st = faults.ct013_stats(frame, lay, p)
    hdr, peer, mix = (_summary(_pool([s[k] for s in st.values()])) for k in ("hdr_ewma", "peer_ewma", "mix_ewma"))
    upd: dict[str, Any] = {}
    sig = []
    if hdr["n"] >= MIN_SAMPLES:
        upd["offset_k"] = hdr["median"]
        sig.append(hdr["robust_sigma"])
    if peer["n"] >= MIN_SAMPLES:
        upd["peer_offset_k"] = peer["median"]
        sig.append(peer["robust_sigma"])
    if sig:
        upd["threshold_k"] = _thr(d["threshold_k"], max(sig))
    if mix["n"] >= MIN_SAMPLES:
        upd |= {"mix_offset_k": mix["median"], "mix_threshold_k": _thr(d["mix_threshold_k"], mix["robust_sigma"])}
    return upd, {"header": hdr, "peer": peer, "mixing": mix}


def fit_approach_model(app: np.ndarray, qf: np.ndarray, fan: np.ndarray, prior: dict[str, float]) -> tuple[dict[str, float], dict[str, Any]]:
    """ln(approach) = b0 + b1 ln(qf) + b2 ln(fan/100); a regressor without spread keeps its prior slope."""
    ok = np.isfinite(app) & np.isfinite(qf) & np.isfinite(fan) & (app > 0.05) & (qf > 0.05) & (fan >= 10)
    y = np.log(app[ok])
    xq, xf = np.log(qf[ok]), np.log(fan[ok] / 100.0)
    coefs = dict(prior)
    cols, names = [], []
    for name, x in (("b1", xq), ("b2", xf)):
        if len(x) and np.std(x) >= 0.03:
            cols.append(x)
            names.append(name)
        else:
            y = y - prior[name] * x  # spread too small to identify: keep the physics prior
    if len(y) < MIN_SAMPLES:
        return coefs, {"n": int(len(y)), "note": "not enough steady running samples"}
    X = np.column_stack([np.ones(len(y)), *cols])
    beta, *_ = np.linalg.lstsq(X, y, rcond=None)
    coefs["b0"] = float(beta[0])
    for name, b in zip(names, beta[1:], strict=True):
        coefs[name] = float(b)
    pred = X @ beta
    ss_res = float(np.sum((y - pred) ** 2))
    ss_tot = float(np.sum((y - np.mean(y)) ** 2)) or 1e-12
    return {k: round(v, 5) for k, v in coefs.items()}, {"n": int(len(y)), "fitted": names, "r2_log": round(1 - ss_res / ss_tot, 4)}


def cal_ct014(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    feats = faults.ct014_features(frame, lay, p)
    if not feats:
        return {}, {"note": "tower signals missing"}
    app = _pool([f["approach"] for f in feats.values()])
    qf = _pool([np.where(np.isfinite(f["approach"]), f["qf"], np.nan) for f in feats.values()])
    fan = _pool([np.where(np.isfinite(f["approach"]), f["fan"], np.nan) for f in feats.values()])
    if not (len(app) == len(qf) == len(fan)):
        return {}, {"note": "misaligned tower samples"}
    model, fit = fit_approach_model(app, qf, fan, d["approach_model"])
    if fit.get("n", 0) < MIN_SAMPLES:
        return {}, {"fit": fit}
    resid = app - faults.approach_expected(model, qf, fan)
    rs = _summary(resid)
    return {"approach_model": model, "threshold_k": _thr(d["threshold_k"], rs["robust_sigma"])}, {"fit": fit, "residual": rs}


def fit_dp_model(dp: np.ndarray, n_hz: np.ndarray, q: np.ndarray) -> tuple[dict[str, float] | None, dict[str, Any]]:
    ok = np.isfinite(dp) & np.isfinite(n_hz) & np.isfinite(q) & (n_hz > 5) & (q > 0)
    if np.count_nonzero(ok) < MIN_SAMPLES:
        return None, {"n": int(np.count_nonzero(ok)), "note": "not enough steady samples"}
    y, N, Q = dp[ok], n_hz[ok], q[ok]
    X = np.column_stack([N**2, N * Q, Q**2])
    beta, *_ = np.linalg.lstsq(X, y, rcond=None)
    pred = X @ beta
    ss_res = float(np.sum((y - pred) ** 2))
    ss_tot = float(np.sum((y - np.mean(y)) ** 2)) or 1e-12
    rmse = math.sqrt(ss_res / len(y))
    stats = {"n": int(len(y)), "rmse_kpa": round(rmse, 3), "r2": round(1 - ss_res / ss_tot, 4), "speed_range_hz": [round(float(N.min()), 2), round(float(N.max()), 2)]}
    # a fit that cannot explain the DP is worse than no rule
    if rmse > 0.05 * float(np.mean(y)):
        return None, {**stats, "note": "pump curve fit too poor (rmse > 5 % of DP)"}
    return {"a": float(beta[0]), "b": float(beta[1]), "c": float(beta[2])}, stats


def cal_chs005(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    f = faults.chs005_features(frame, lay, p)
    if f is None:
        return {}, {"note": "riser DP signals missing"}
    g = f["gate"]
    model, fit = fit_dp_model(np.where(g, f["dp"], np.nan), f["n"], f["q"])
    if model is None:
        return {}, {"fit": fit}
    exp_dp = faults.dp_expected(model, f["n"], f["q"])
    with np.errstate(invalid="ignore", divide="ignore"):
        ratio = np.where(g & (exp_dp > 10), f["dp"] / exp_dp - 1.0, np.nan)
    e = faults.ewma_gated(ratio, float(p["ewma_lambda"]))
    rs = _summary(e[np.isfinite(e)])
    updates: dict[str, Any] = {"dp_model": {k: float(f"{v:.8g}") for k, v in model.items()}, "offset": rs["median"], "threshold": _thr(d["threshold"], rs["robust_sigma"])}
    stats: dict[str, Any] = {"fit": fit, "ratio": rs}
    # the pumps' own head curve, for isolating pump degradation from a transmitter bias
    head_model, head_fit = fit_dp_model(np.where(g, f["head"], np.nan), f["n"], f["q"])
    stats["head_fit"] = head_fit
    if head_model is not None:
        hm = {k: float(f"{v:.8g}") for k, v in head_model.items()}
        hd = faults.pump_head_deviation(f, {**p, "head_model": hm, "head_offset": 0.0})
        hs = _summary(hd[np.isfinite(hd)]) if hd is not None else {"median": 0.0, "robust_sigma": float("nan")}
        updates |= {"head_model": hm, "head_offset": hs["median"], "head_tolerance": _thr(d["head_tolerance"], hs["robust_sigma"])}
        stats["head_ratio"] = hs
    return updates, stats


def cal_chs006(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    s = faults.chs006_stats(frame, lay, p)
    if s is None:
        return {}, {"note": "3-way valve signals missing"}
    # the rule tests the excess over the valve's own bypass (f - twv_pos / 100): its offset and spread
    xs = _summary(s["excess"][s["at_min"] & np.isfinite(s["excess"])])
    if xs["n"] < MIN_SAMPLES:
        return {}, {"excess_fraction": xs, "note": "valve rarely at its minimum with a usable range"}
    return {"offset": xs["median"], "threshold": _thr(d["threshold"], xs["robust_sigma"])}, {"excess_fraction": xs}


def cal_chs007(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    if not lay.system:
        return {}, {}
    pos, cmd = frame.get(lay.system, "twv_pos"), frame.get(lay.system, "twv_cmd")
    if pos is None or cmd is None:
        return {}, {"note": "3-way valve signals missing"}
    ds = _summary(np.abs(pos - cmd))
    if ds["n"] < MIN_SAMPLES:
        return {}, {"deviation": ds}
    return {"deviation_pct": round(max(float(d["deviation_pct"]), ds["median"] + rules.K_SIGMA * ds["robust_sigma"]), 3)}, {"deviation": ds}


def cal_chs008(frame: Frame, lay: faults.Layout, p: dict[str, Any], d: dict[str, Any]) -> Result:
    s = faults.chs008_stats(frame, lay, p)
    if s is None:
        return {}, {"note": "tower loop signals missing"}
    rev = s["rev_h"][np.isfinite(s["rev_h"])]
    cpp = s["cmd_pp"][s["gate"] & np.isfinite(s["cmd_pp"])]
    tpp = s["t_pp"][s["gate"] & np.isfinite(s["t_pp"])]
    stats = {"reversals_per_h": _summary(rev), "cmd_pp_pct": _summary(cpp), "temp_pp_k": _summary(tpp)}
    if len(rev) < MIN_SAMPLES:
        return {}, {**stats, "note": "not enough samples"}
    # The reversal rate and the command swing separate hunting from normal operation (on the twin:
    # nominal p99 about 7 / h and 15 %, hunting about 30 / h and 70 %); their thresholds sit above
    # the fault-free p99. The leaving-water swing is only a plausibility check: over an hour it is
    # dominated by load and weather trends (nominal p99 about 1 K, hunting about 1.3 K), so its
    # threshold is the fault-free median, not a tail quantile that would hide the fault.
    return {
        "reversals_per_h": max(int(d["reversals_per_h"]), int(math.ceil(float(np.quantile(rev, 0.99)))) + 2),
        "cmd_pp_pct": round(max(float(d["cmd_pp_pct"]), 1.25 * float(np.quantile(cpp, 0.99))), 3) if len(cpp) else d["cmd_pp_pct"],
        "temp_pp_k": round(max(float(d["temp_pp_k"]), float(np.median(tpp))), 4) if len(tpp) else d["temp_pp_k"],
    }, stats


CALIBRATORS: dict[str, Callable[[Frame, faults.Layout, dict[str, Any], dict[str, Any]], Result]] = {
    "CHL-011": cal_chl011,
    "CT-013": cal_ct013,
    "CT-014": cal_ct014,
    "CHS-005": cal_chs005,
    "CHS-006": cal_chs006,
    "CHS-007": cal_chs007,
    "CHS-008": cal_chs008,
}


def calibrate_frame(frame: Frame, params: dict[str, dict[str, Any]] | None = None) -> dict[str, Result]:
    """Calibration results per rule code for a fault-free frame (pure)."""
    lay = faults.Layout.from_aliases(frame.aliases())
    out: dict[str, Result] = {}
    for code, fn in CALIBRATORS.items():
        rule = rules.RULES_BY_CODE[code]
        p = rules.merged_params(rule, (params or {}).get(code))
        try:
            out[code] = fn(frame, lay, p, rule.defaults)
        except Exception as err:  # noqa: BLE001 - one rule must not stop the others
            log.exception("calibration failed code=%s", code)
            out[code] = ({}, {"error": str(err)})
    return out


# ---------------------------------------------------------------------------
# Worker task


async def apply_job(job_id: int) -> dict[str, Any]:
    """Compute and store the calibration from a SUCCEEDED nominal job."""
    frames = await data.sim_frames(job_id)
    if not frames:
        raise RuntimeError(f"calibration job {job_id} has no series")
    frame = frames.get("baseline") or next(iter(frames.values()))
    rows = {
        r["code"]: r
        for r in await db.fetch_all("SELECT code, params, updated_by, version FROM analytics.fault_rules")
    }
    results = calibrate_frame(frame, {c: r["params"] for c, r in rows.items()})
    now = data.utcnow()
    applied: dict[str, Any] = {}
    for code, (updates, stats) in results.items():
        row = rows.get(code)
        if row is None:
            continue
        admin_edited = row["updated_by"] is not None
        cal = {
            "job_id": job_id,
            "samples": frame.n,
            "hours": round(frame.n * frame.step_s / 3600, 2),
            "k_sigma": rules.K_SIGMA,
            "stats": stats,
            "updates": updates,
            "applied": bool(updates) and not admin_edited,
            "note": "params kept: edited by an administrator" if admin_edited and updates else None,
            "computed_at": now,
        }
        written = 0
        if updates and not admin_edited:
            # merged into the stored params, and only while no administrator has edited the rule meanwhile
            written = await db.execute(
                "UPDATE analytics.fault_rules SET params = COALESCE(params, '{}'::jsonb) || %s, calibration = %s, calibrated_at = %s,"
                " version = version + 1, updated_at = now() WHERE code = %s AND updated_by IS NULL",
                (data.jb(updates), data.jb(cal), now, code),
            )
            if not written:
                cal |= {"applied": False, "note": "params kept: edited by an administrator"}
        if not written:
            await db.execute(
                "UPDATE analytics.fault_rules SET calibration = %s, calibrated_at = %s WHERE code = %s",
                (data.jb(cal), now, code),
            )
        applied[code] = {"applied": cal["applied"], "updates": updates}
    log.info("rules calibrated from job=%s %s", job_id, {k: v["applied"] for k, v in applied.items()})
    return applied


async def run(now: datetime | None = None, *, dry_run: bool = False) -> str:
    """One calibration check: request a nominal job when due, apply a finished one.

    dry_run: report the decision without seeding, calibrating or queuing anything.
    """
    now = now or data.utcnow()
    if not dry_run:
        await rules.seed_rules()
    rows = await db.fetch_all("SELECT code, enabled, calibrated_at, calibration ->> 'job_id' AS job_id FROM analytics.fault_rules")
    if not rows:
        return "no rules (seeded on the first FDD or calibration pass)" if dry_run else "no rules"
    pending = await data.latest_job(PURPOSE, ("QUEUED", "RUNNING", "ANALYZING"))
    if pending:
        return f"waiting for job {pending['id']} ({pending['status']})"
    done = await data.latest_job(PURPOSE, ("SUCCEEDED",))
    applied_jobs = {int(r["job_id"]) for r in rows if r["job_id"]}
    if done and int(done["id"]) not in applied_jobs:
        if dry_run:
            return f"would calibrate from job {done['id']}"
        await apply_job(int(done["id"]))
        return f"calibrated from job {done['id']}"
    enabled = [r for r in rows if r["enabled"]]
    oldest = min((r["calibrated_at"] for r in enabled if r["calibrated_at"]), default=None)
    due = any(r["calibrated_at"] is None for r in enabled) or (oldest is not None and now - oldest >= RECALIBRATE_AFTER)
    if not due:
        return "up to date"
    last = await data.latest_job(PURPOSE)
    duration = DURATION_MIN
    if last and last["status"] in ("FAILED", "CANCELLED"):
        if last["finished_at"] and now - last["finished_at"] < RETRY_AFTER_FAILURE:
            return f"backing off after failed job {last['id']}"
        duration = FALLBACK_DURATION_MIN if int((last["spec"] or {}).get("duration_min", 0)) > FALLBACK_DURATION_MIN else DURATION_MIN
    if dry_run:
        return f"would request a nominal {duration // 60} h calibration job"
    job_id = await data.insert_job(
        kind="scenario",
        title=f"FDD rule calibration (nominal, fault-free {duration // 60} h)",
        origin="analytics",
        spec=job_spec(duration),
        rationale="Fault-free baseline for the LBNL FDD rule thresholds and model fits (analytics.fault_rules).",
    )
    return f"requested job {job_id}"
