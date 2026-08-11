"""Plant surrogate model for the set-point optimizer (DESIGN 6.1).

LightGBM regressors predict `plant_kw`, `chws_temp`, `riser_dp`, `mau_valve_max`
and `mau_sat_dev_max` from the operating conditions and the supervisory set
points (`FEATURES`). Training data is a design of experiments run on the
digital twin (kind doe: Latin hypercube over set points x weather x load,
DESIGN 5.1), plus steady 15-minute windows of live operation when the plant
reports the supervisory points.

Monotone constraints encode the plant physics the optimizer must not exploit
against (more load or a warmer wet bulb never lowers plant power; a warmer CHW
set point never lowers the CHW supply temperature or the MAU valve demand; a
higher riser DP set point never lowers the riser DP and never opens the MAU
valves further). Each target is trained with early stopping on a 20 % hold-out
(metrics reported from it), then refitted on all samples with that number of
rounds. The boosters are stored as LightGBM text in
`analytics.surrogate_models.artifact` (gzip JSON) and the new model activated.

Retraining: on start when no model is active (requests a DOE job), weekly
(latest DOE + new live samples) and on drift (median optimizer prediction
error of plant power over the last runs above DRIFT_PCT: requests a new DOE).
A finished DOE too short of usable samples to train on is marked in its job
summary (`surrogate.usable = false`) and a new one is requested after the
failure back-off, instead of retrying the same job forever.
"""

from __future__ import annotations

import asyncio
import gzip
import json
import logging
import math
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

import lightgbm as lgb
import numpy as np
import pandas as pd

from ..common import db
from . import data

log = logging.getLogger(__name__)

PURPOSE = "surrogate"
ARTIFACT_FORMAT = "afdd-surrogate/1"
FEATURES = ["wet_bulb", "dry_bulb", "load_frac", "chws_sp", "approach_sp", "riser_dp_sp", "chillers", "twv_min_pos", "cw_min_temp"]
TARGETS = ["plant_kw", "chws_temp", "riser_dp", "mau_valve_max", "mau_sat_dev_max"]
DOE_BOUNDS: dict[str, list[float]] = {
    "wet_bulb": [22, 28],
    "dry_bulb_minus_wb": [1, 7],
    "load_frac": [0.5, 0.97],
    "chws_sp": [5.5, 8.0],
    "approach_sp": [2.0, 6.0],
    "riser_dp_sp": [80, 160],
    "chillers": [1, 3],
    "twv_min_pos": [0, 30],
    "cw_min_temp": [18, 24],
}
DOE_SAMPLES = 400
LOAD_BASE_KW = 2 * 3517.0  # cooling load at load_frac 1 (two duty chillers at full load)
MAU_SAT_SP = 17.0  # MAU supply air set point (AHU-003)
MIN_SAMPLES = 60
HOLDOUT = 0.2
RETRAIN_AFTER = timedelta(days=7)
DRIFT_PCT = 10.0
DRIFT_RUNS = 6
LIVE_DAYS = 30
RETRY_AFTER_FAILURE = timedelta(hours=6)

# +1 increasing, -1 decreasing, 0 free, per target and feature
MONOTONE: dict[str, dict[str, int]] = {
    "plant_kw": {"wet_bulb": 1, "load_frac": 1},
    "chws_temp": {"chws_sp": 1},
    "riser_dp": {"riser_dp_sp": 1},
    "mau_valve_max": {"chws_sp": 1, "load_frac": 1, "riser_dp_sp": -1},
    "mau_sat_dev_max": {"chws_sp": 1, "load_frac": 1, "riser_dp_sp": -1},
}

LGB_PARAMS: dict[str, Any] = {
    "objective": "regression",
    "learning_rate": 0.05,
    "num_leaves": 15,
    "min_data_in_leaf": 8,
    "feature_fraction": 0.9,
    "bagging_fraction": 0.9,
    "bagging_freq": 1,
    "lambda_l2": 1.0,
    "monotone_constraints_method": "advanced",
    "num_threads": 2,
    "deterministic": True,
    "force_row_wise": True,
    "verbose": -1,
}
MAX_ROUNDS = 2000
EARLY_STOP = 50


def doe_spec(samples: int = DOE_SAMPLES, seed: int = 0) -> dict[str, Any]:
    return {"samples": samples, "settle_min": 12, "avg_min": 5, "seed": seed, "bounds": DOE_BOUNDS, "purpose": PURPOSE}


# ---------------------------------------------------------------------------
# Model


@dataclass
class Surrogate:
    boosters: dict[str, lgb.Booster]
    ranges: dict[str, list[float]]
    metrics: dict[str, Any] = field(default_factory=dict)
    features: list[str] = field(default_factory=lambda: list(FEATURES))
    id: int | None = None
    version: str = ""
    n_samples: int = 0
    trained_at: datetime | None = None

    def predict(self, X: pd.DataFrame) -> dict[str, np.ndarray]:
        arr = X[self.features].to_numpy(dtype=float)
        return {t: b.predict(arr) for t, b in self.boosters.items()}

    def predict_one(self, row: dict[str, float]) -> dict[str, float]:
        arr = np.array([[float(row[f]) for f in self.features]])
        return {t: float(b.predict(arr)[0]) for t, b in self.boosters.items()}

    def out_of_range(self, row: dict[str, float], tolerance: float = 0.05) -> list[str]:
        """Features of a point outside the training range (by more than tolerance x span)."""
        out = []
        for f in self.features:
            lo, hi = self.ranges.get(f, [-math.inf, math.inf])
            span = (hi - lo) or 1.0
            v = float(row[f])
            if v < lo - tolerance * span or v > hi + tolerance * span:
                out.append(f)
        return out

    def to_artifact(self) -> bytes:
        doc = {
            "format": ARTIFACT_FORMAT,
            "features": self.features,
            "targets": list(self.boosters),
            "ranges": self.ranges,
            "monotone": MONOTONE,
            "boosters": {t: b.model_to_string() for t, b in self.boosters.items()},
        }
        return gzip.compress(json.dumps(doc).encode("utf-8"), compresslevel=6)

    @classmethod
    def from_artifact(cls, blob: bytes, **meta: Any) -> Surrogate:
        raw = gzip.decompress(blob) if blob[:2] == b"\x1f\x8b" else blob
        doc = json.loads(raw.decode("utf-8"))
        if doc.get("format") != ARTIFACT_FORMAT:
            raise ValueError(f"unknown surrogate artifact format {doc.get('format')!r}")
        boosters = {t: lgb.Booster(model_str=s) for t, s in doc["boosters"].items()}
        return cls(boosters=boosters, ranges=doc["ranges"], features=doc["features"], **meta)


# ---------------------------------------------------------------------------
# Training data


def doe_frame(rows: list[dict[str, Any]]) -> pd.DataFrame:
    """sim.doe_samples rows -> features + targets (rows with missing values dropped)."""
    recs = []
    for r in rows:
        i, o = r.get("inputs") or {}, r.get("outputs") or {}
        wb = i.get("wet_bulb")
        db_ = i.get("dry_bulb")
        if db_ is None and wb is not None and i.get("dry_bulb_minus_wb") is not None:
            db_ = float(wb) + float(i["dry_bulb_minus_wb"])
        rec = {
            "wet_bulb": wb,
            "dry_bulb": db_,
            "load_frac": i.get("load_frac"),
            "chws_sp": i.get("chws_sp"),
            "approach_sp": i.get("approach_sp"),
            "riser_dp_sp": i.get("riser_dp_sp"),
            "chillers": i.get("chillers"),
            "twv_min_pos": i.get("twv_min_pos"),
            "cw_min_temp": i.get("cw_min_temp"),
        }
        rec |= {t: o.get(t) for t in TARGETS}
        recs.append(rec)
    df = pd.DataFrame(recs, columns=FEATURES + TARGETS).apply(pd.to_numeric, errors="coerce")
    return df.dropna().reset_index(drop=True)


def _monotone_vector(target: str) -> list[int]:
    m = MONOTONE.get(target, {})
    return [int(m.get(f, 0)) for f in FEATURES]


def train(df: pd.DataFrame, *, seed: int = 0) -> tuple[dict[str, lgb.Booster], dict[str, Any]]:
    """Train one booster per target; metrics from a seeded 20 % hold-out."""
    if len(df) < MIN_SAMPLES:
        raise ValueError(f"not enough training samples ({len(df)} < {MIN_SAMPLES})")
    rng = np.random.default_rng(seed)
    idx = rng.permutation(len(df))
    n_hold = max(1, int(round(len(df) * HOLDOUT)))
    hold, tr = idx[:n_hold], idx[n_hold:]
    X = df[FEATURES].to_numpy(dtype=float)
    boosters: dict[str, lgb.Booster] = {}
    metrics: dict[str, Any] = {"n_train": int(len(tr)), "n_holdout": int(n_hold), "targets": {}}
    for t in TARGETS:
        y = df[t].to_numpy(dtype=float)
        params = {**LGB_PARAMS, "seed": seed, "monotone_constraints": _monotone_vector(t)}
        dtr = lgb.Dataset(X[tr], y[tr], feature_name=FEATURES, free_raw_data=False)
        dva = lgb.Dataset(X[hold], y[hold], reference=dtr, free_raw_data=False)
        b = lgb.train(params, dtr, num_boost_round=MAX_ROUNDS, valid_sets=[dva], callbacks=[lgb.early_stopping(EARLY_STOP, verbose=False)])
        rounds = max(1, int(b.best_iteration or MAX_ROUNDS))
        pred = b.predict(X[hold], num_iteration=rounds)
        err = pred - y[hold]
        ss_tot = float(np.sum((y[hold] - np.mean(y[hold])) ** 2)) or 1e-12
        tm = {
            "mae": round(float(np.mean(np.abs(err))), 4),
            "rmse": round(float(np.sqrt(np.mean(err**2))), 4),
            "r2": round(1 - float(np.sum(err**2)) / ss_tot, 4),
            "rounds": rounds,
        }
        nz = np.abs(y[hold]) > 1e-6
        if t == "plant_kw" and np.any(nz):
            tm["mape_pct"] = round(float(np.mean(np.abs(err[nz] / y[hold][nz]))) * 100, 3)
        metrics["targets"][t] = tm
        # refit on everything with the validated number of rounds
        full = lgb.Dataset(X, y, feature_name=FEATURES, free_raw_data=False)
        boosters[t] = lgb.train(params, full, num_boost_round=rounds)
    return boosters, metrics


def ranges_of(df: pd.DataFrame) -> dict[str, list[float]]:
    return {f: [round(float(df[f].min()), 4), round(float(df[f].max()), 4)] for f in FEATURES}


# ---------------------------------------------------------------------------
# Live samples (15-minute steady windows)

SYSTEM_PARAMS = ["wet_bulb", "oa_temp", "cooling_load", "chillers_running", "chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos", "cw_min_temp", "plant_kw", "chws_temp", "riser_dp"]
HELD = ["chws_sp", "approach_sp", "riser_dp_sp", "twv_min_pos", "cw_min_temp", "chillers_running"]


async def live_samples(site: str | None = None, days: int = LIVE_DAYS) -> pd.DataFrame:
    """Steady 15-minute windows of live operation (set points and staging unchanged)."""
    plant = await data.load_plant(site or data.settings_site())
    systems = plant.by_role("chws")
    if not systems:
        return pd.DataFrame(columns=FEATURES + TARGETS)
    sys_ = systems[0]
    params = await data.load_params([sys_.id] + [m.id for m in plant.by_role("mau")])
    keys: dict[int, tuple[str, str]] = {}
    for p in params:
        if p.asset_id == sys_.id and p.alias in SYSTEM_PARAMS:
            keys[p.id] = ("sys", p.alias)
        elif p.asset_id != sys_.id and p.alias in ("chw_valve", "sat", "running"):
            keys[p.id] = (str(p.asset_id), p.alias)
    if {a for k, a in keys.values() if k == "sys"} != set(SYSTEM_PARAMS):
        return pd.DataFrame(columns=FEATURES + TARGETS)  # plant without the supervisory points
    rows = await db.fetch_all(
        """SELECT equipment_parameter_id AS pid, date_bin('15 minutes', ts, TIMESTAMPTZ '2000-01-01') AS b,
                  avg(value) AS mean, min(value) AS lo, max(value) AS hi, count(*) AS n
           FROM ts.parameter_values
           WHERE equipment_parameter_id = ANY(%s) AND ts > now() - make_interval(days => %s)
           GROUP BY 1, 2""",
        (list(keys), days),
    )
    buckets: dict[datetime, dict[tuple[str, str], dict[str, float]]] = {}
    for r in rows:
        buckets.setdefault(r["b"], {})[keys[r["pid"]]] = {"mean": r["mean"], "lo": r["lo"], "hi": r["hi"], "n": r["n"]}
    recs = []
    for b, vals in buckets.items():
        sysv = {a: v for (k, a), v in vals.items() if k == "sys"}
        if set(sysv) != set(SYSTEM_PARAMS) or any(v["n"] < 10 or v["mean"] is None for v in sysv.values()):
            continue
        if any(sysv[h]["hi"] - sysv[h]["lo"] > 1e-6 for h in HELD) or sysv["chillers_running"]["mean"] < 0.5:
            continue
        maus = {}
        for (k, a), v in vals.items():
            if k != "sys":
                maus.setdefault(k, {})[a] = v
        running = [m for m in maus.values() if "chw_valve" in m and "sat" in m and (m.get("running") or {}).get("mean", 1) >= 0.99]
        if not running:
            continue
        recs.append(
            {
                "wet_bulb": sysv["wet_bulb"]["mean"],
                "dry_bulb": sysv["oa_temp"]["mean"],
                "load_frac": sysv["cooling_load"]["mean"] / LOAD_BASE_KW,
                "chws_sp": sysv["chws_sp"]["mean"],
                "approach_sp": sysv["approach_sp"]["mean"],
                "riser_dp_sp": sysv["riser_dp_sp"]["mean"],
                "chillers": round(sysv["chillers_running"]["mean"]),
                "twv_min_pos": sysv["twv_min_pos"]["mean"],
                "cw_min_temp": sysv["cw_min_temp"]["mean"],
                "plant_kw": sysv["plant_kw"]["mean"],
                "chws_temp": sysv["chws_temp"]["mean"],
                "riser_dp": sysv["riser_dp"]["mean"],
                "mau_valve_max": max(m["chw_valve"]["mean"] for m in running),
                "mau_sat_dev_max": max(abs(m["sat"]["mean"] - MAU_SAT_SP) for m in running),
            }
        )
    return pd.DataFrame(recs, columns=FEATURES + TARGETS).dropna().reset_index(drop=True)


# ---------------------------------------------------------------------------
# Store / load


class UnusableDoe(ValueError):
    """A finished DOE job without enough usable samples to train on."""


async def train_and_store(doe_job_id: int, *, include_live: bool = True) -> int:
    rows = await db.fetch_all("SELECT inputs, outputs FROM sim.doe_samples WHERE job_id = %s ORDER BY idx", (doe_job_id,))
    doe = doe_frame(rows)
    live = await live_samples() if include_live else pd.DataFrame(columns=FEATURES + TARGETS)
    df = pd.concat([doe, live], ignore_index=True) if len(live) else doe
    if len(df) < MIN_SAMPLES:
        raise UnusableDoe(f"DOE job {doe_job_id} has {len(doe)} usable samples of {len(rows)} (+{len(live)} live), {MIN_SAMPLES} needed")
    boosters, metrics = await asyncio.to_thread(train, df, seed=doe_job_id)
    metrics |= {"doe_samples": int(len(doe)), "live_samples": int(len(live)), "doe_job_id": doe_job_id}
    model = Surrogate(boosters=boosters, ranges=ranges_of(df), metrics=metrics, n_samples=len(df))
    now = data.utcnow()
    version = f"lgbm-{now:%Y%m%dT%H%M%SZ}-doe{doe_job_id}"
    pool = await db.get_pool()
    async with pool.connection() as conn:
        async with conn.transaction():
            await conn.execute("UPDATE analytics.surrogate_models SET is_active = FALSE WHERE is_active")
            cur = await conn.execute(
                "INSERT INTO analytics.surrogate_models (version, algorithm, features, targets, n_samples, metrics, doe_job_id, artifact, is_active)"
                " VALUES (%s, 'lightgbm', %s, %s, %s, %s, %s, %s, TRUE) RETURNING id",
                (version, data.jb(FEATURES), data.jb(TARGETS), len(df), data.jb({**metrics, "ranges": model.ranges}), doe_job_id, model.to_artifact()),
            )
            row = await cur.fetchone()
    assert row is not None
    log.info("surrogate trained id=%s version=%s samples=%s metrics=%s", row["id"], version, len(df), {t: m["r2"] for t, m in metrics["targets"].items()})
    return int(row["id"])


async def load_active() -> Surrogate | None:
    row = await db.fetch_one(
        "SELECT id, version, n_samples, metrics, artifact, trained_at FROM analytics.surrogate_models WHERE is_active ORDER BY id DESC LIMIT 1"
    )
    if row is None:
        return None
    return await asyncio.to_thread(
        Surrogate.from_artifact,
        bytes(row["artifact"]),
        id=row["id"],
        version=row["version"],
        n_samples=row["n_samples"],
        metrics=row["metrics"],
        trained_at=row["trained_at"],
    )


async def _drifting(model_id: int) -> float | None:
    rows = await db.fetch_all(
        "SELECT (metrics ->> 'model_error_pct')::float AS err FROM analytics.optimizer_runs"
        " WHERE model_id = %s AND status = 'SUCCEEDED' AND metrics ? 'model_error_pct' ORDER BY id DESC LIMIT %s",
        (model_id, DRIFT_RUNS),
    )
    errs = [abs(r["err"]) for r in rows if r["err"] is not None]
    if len(errs) < DRIFT_RUNS:
        return None
    return float(np.median(errs))


def unusable(job: dict[str, Any] | None) -> bool:
    """A finished DOE job marked as not usable for training (`summary.surrogate.usable` false)."""
    return bool(job) and ((job.get("summary") or {}).get("surrogate") or {}).get("usable") is False


async def mark_unusable(job_id: int, reason: str) -> None:
    """Remember that a DOE job cannot be trained on, so it is not retried forever."""
    await db.execute(
        "UPDATE sim.jobs SET summary = COALESCE(summary, '{}'::jsonb) || jsonb_build_object('surrogate', %s::jsonb) WHERE id = %s",
        (data.jb({"usable": False, "reason": reason[:300], "checked_at": data.utcnow()}), job_id),
    )
    log.warning("DOE job %s is not usable for the surrogate: %s", job_id, reason)


async def run(now: datetime | None = None, *, dry_run: bool = False) -> str:
    """One surrogate check: request DOE, train on a finished DOE, weekly retrain, drift.

    A finished DOE without enough usable samples is marked (`unusable`) and
    treated like a failed one: a new DOE is requested after RETRY_AFTER_FAILURE.

    dry_run: report the decision without training or queuing anything.
    """
    now = now or data.utcnow()
    active = await db.fetch_one("SELECT id, trained_at, doe_job_id FROM analytics.surrogate_models WHERE is_active LIMIT 1")
    pending = await data.latest_job(PURPOSE, ("QUEUED", "RUNNING", "ANALYZING"))
    done = await data.latest_job(PURPOSE, ("SUCCEEDED",))
    if done and unusable(done):
        done = None
    if done:
        trained = await db.fetch_val("SELECT count(*) FROM analytics.surrogate_models WHERE doe_job_id = %s", (done["id"],))
        if not trained:
            if dry_run:
                return f"would train a model on DOE job {done['id']}"
            try:
                model_id = await train_and_store(int(done["id"]))
                return f"trained model {model_id} on DOE job {done['id']}"
            except UnusableDoe as err:
                await mark_unusable(int(done["id"]), str(err))
                done = None
    if pending:
        return f"waiting for DOE job {pending['id']} ({pending['status']})"

    async def request(reason: str) -> str:
        last = await data.latest_job(PURPOSE)
        if last and (last["status"] in ("FAILED", "CANCELLED") or unusable(last)) and last["finished_at"] and now - last["finished_at"] < RETRY_AFTER_FAILURE:
            return f"backing off after {'unusable' if unusable(last) else 'failed'} DOE job {last['id']}"
        if dry_run:
            return f"would request a DOE job ({reason})"
        job_id = await data.insert_job(
            kind="doe",
            title=f"Surrogate DOE ({DOE_SAMPLES} samples)",
            origin="analytics",
            spec=doe_spec(),
            rationale=f"Training data for the set-point optimizer surrogate ({reason}).",
        )
        return f"requested DOE job {job_id} ({reason})"

    if active is None:
        return await request("no active surrogate")
    age = now - active["trained_at"]
    if age >= RETRAIN_AFTER and done:
        if dry_run:
            return f"would retrain (weekly) on DOE job {done['id']} and recent live samples"
        model_id = await train_and_store(int(done["id"]))
        return f"weekly retrain: model {model_id}"
    err = await _drifting(int(active["id"]))
    if err is not None and err > DRIFT_PCT and age > timedelta(hours=24):
        return await request(f"drift: median plant power error {err:.1f} %")
    return "up to date"
