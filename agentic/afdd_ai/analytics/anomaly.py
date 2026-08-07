"""Statistical sensor anomaly detection (DESIGN 6.1).

Detectors (pure functions on a regular 1-minute grid, numpy arrays):

* ``range``: value outside the physical span of its unit (sensor failure) -> CRITICAL;
* ``flatline``: max - min below the display resolution for >= 30 min while running -> WARNING;
* ``zscore``: robust modified z = (x - median) / (1.4826 MAD) over the previous 60 min
  (current point excluded); |M| > 3.5 on 3 consecutive points -> WARNING, > 6 -> CRITICAL;
* ``ewma``: EWMA (lambda 0.1) against limits mu +/- L sigma (L 3) from the 24 h running
  baseline; >= 5 of the last 10 beyond -> WARNING;
* ``spc``: 15-minute means with Phase I limits from the baseline: beyond 3 sigma, 2 of 3
  beyond 2 sigma, 7 monotonic (engine-equivalent rules) -> WARNING.

Only numeric, displayed, non-writable, non-status parameters of RUNNING units
are checked; the first minutes after a start are skipped (start-up transients).

Refinements over the textbook detectors, needed on autocorrelated 1-minute
process data with a daily load cycle, controller actions, staging and batch
process cycles (dry pumps). Checked on two days of fault-free digital twin
output evaluated like production: the textbook versions raise about 700
episodes per day over the plant's 312 parameters, these about 35, mostly
several parameters of one real plant event (CHW pumps reaching full speed, a
load step, a CHW set-point reset); a 24 h simulation run gets about 65
(`BATCH_CONFIG`). All are configurable in `AnomalyConfig`:

* independent baseline: the evaluated samples are judged in blocks of
  `block_min` against the running samples outside the block and a guard of
  `guard_min` around it (production: the trailing 24 h up to 2 h before now;
  a simulation run: the rest of the run), so an excursion never widens its own
  limits; a block without such a baseline gets no statistical judgement;
* novelty: an excursion only counts when the series did not visit that level
  more than `novelty_min` (6 h; 2 h in a simulation run) away. Daily cycles,
  recurring process steps and set-point regimes seen in the last day are not
  anomalies; a new level, spike, drift or stuck value is;
* z-score: the window scale has a floor of max(resolution, 0.25 baseline sigma,
  1.25 q / threshold), q = the `zscore_dev_quantile` (99.9 %) of the baseline's
  deviations from their trailing 60-min median, so a quiet hour or a recurring
  step does not turn normal variation into anomalies;
* EWMA sigma: by default the Phase I sigma of the EWMA statistic itself over the
  baseline (``sigma_mode="statistic"``); it accounts for autocorrelation and
  the daily cycle. ``"classic"`` uses sigma_x * sqrt(lambda/(2-lambda)(1-(1-lambda)^2t)),
  which flags every daily swing on such data. The statistic must also lie
  `ewma_novelty_sigma` sigma beyond the range it took elsewhere;
* flatline: only for parameters that are not naturally flat (in the baseline,
  excluding the current flat run, at most 5 % of 30-minute windows are flat),
  not for a percentage pinned at 0 or 100 % (a saturated output, not a stuck
  sensor), and only once the flat run is `flatline_novelty` times longer than
  any other stretch of stillness (10-minute windows) in the baseline;
* SPC: at least `spc_min_buckets` 15-minute means; the 7-monotonic run must
  move by at least 1 sigma; the bucket mean must lie `spc_novelty_sigma` beyond
  the range of the other bucket means (so a smooth daily peak is not a shift).

`AnomalyRunner` is the production loop (warm-up of a 27 h buffer: the 24 h
baseline, the guard and the new samples; incremental fetch, RUNNING gating from
ts.asset_status, open / update / clear in analytics.anomalies).
`detect_batch` / `detect_frame` serve the simulation post-analysis.
"""

from __future__ import annotations

import logging
import math
from dataclasses import dataclass, field, replace
from datetime import datetime, timedelta
from typing import Any

import numpy as np
import pandas as pd

from ..common import db
from . import data
from .series import (
    Frame,
    bucket_means,
    ewma_gated,
    merge_hits,
    n_samples,
    nanmedian_rows,
    phase_one,
    range_excluding,
    robust_center_scale,
    rolling_range,
    run_for,
    sliding_windows,
)

log = logging.getLogger(__name__)

DETECTORS = ("range", "flatline", "zscore", "ewma", "spc")


@dataclass(frozen=True)
class AnomalyConfig:
    baseline_hours: float = 24.0
    min_baseline_samples: int = 120
    start_settle_min: float = 10.0
    clear_min: float = 15.0
    guard_min: float = 120.0  # samples this close to an evaluated block are not its baseline
    block_min: float = 120.0  # evaluated samples are judged in blocks of this length
    novelty: bool = True
    novelty_min: float = 360.0  # a level is "seen before" when the series visited it at least this far away
    flatline_min: float = 30.0
    flatline_max_flat_fraction: float = 0.05
    flatline_novelty: float = 2.0
    flatline_still_min: float = 10.0
    zscore_window_min: float = 60.0
    zscore_min_samples: int = 30
    zscore_threshold: float = 3.5
    zscore_critical: float = 6.0
    zscore_consecutive: int = 3
    zscore_floor_fraction: float = 0.25
    zscore_dev_quantile: float = 0.999
    zscore_dev_margin: float = 1.25  # a deviation must exceed the baseline's q by this factor to reach the threshold
    ewma_lambda: float = 0.1
    ewma_L: float = 3.0
    ewma_hits: int = 5
    ewma_of_last: int = 10
    ewma_sigma_mode: str = "statistic"  # statistic | classic
    ewma_reset_gap_min: float = 30.0
    spc_bucket_min: int = 15
    spc_min_buckets: int = 16
    spc_monotonic_min_sigma: float = 1.0
    spc_novelty_sigma: float = 1.5
    ewma_novelty_sigma: float = 2.0


DEFAULT_CONFIG = AnomalyConfig()
# Simulation runs (batch): there is no "same time yesterday" in a run of a few hours to two days,
# so a level only counts as seen before when the run visited it outside the guard of the block.
BATCH_CONFIG = AnomalyConfig(novelty_min=120.0)

# Physical spans per unit (sensor failure outside); running samples only.
UNIT_SPANS: dict[str, tuple[float, float]] = {
    "°C": (-10.0, 80.0),
    "K": (-20.0, 40.0),
    "%": (-2.0, 102.0),  # RH sensors saturate slightly above 100 %
    "kPa": (0.0, 5000.0),
    "bar": (-1.0, 40.0),
    "bar(g)": (-1.0, 40.0),
    "mbar": (0.0, 2000.0),
    "Pa": (-50.0, 5000.0),
    "A": (0.0, 5000.0),
    "Hz": (0.0, 70.0),
    "kW": (-50.0, 20000.0),
    "kW/RT": (0.0, 3.0),
    "m³/h": (-5.0, 200000.0),
    "mm/s": (0.0, 50.0),
    "µS/cm": (0.0, 10000.0),
    "l/min": (0.0, 1000.0),
    "slm": (0.0, 1000.0),
    "Nm³/min": (0.0, 500.0),
    "kWh/m³": (0.0, 5.0),
}
# Parameters whose physics leaves the generic span of their unit.
ALIAS_SPANS: dict[str, tuple[float, float]] = {
    "hp_elem_temp": (0.0, 260.0),  # compressor element outlet
    "lp_elem_temp": (0.0, 260.0),
    "dew_point": (-80.0, 30.0),  # pressure dew point of dried air
    "body_temp": (0.0, 200.0),  # dry pump body
    "discharge_temp": (-10.0, 130.0),
    "motor_temp": (-10.0, 160.0),
    "bearing_temp": (-10.0, 130.0),
    "oil_temp": (-10.0, 120.0),
    "intercooler_temp": (-10.0, 120.0),
}


@dataclass(frozen=True)
class ParamMeta:
    alias: str
    name: str
    unit: str
    scale: int | None

    @property
    def resolution(self) -> float:
        return 10.0 ** (-(self.scale if self.scale is not None else 2))

    @property
    def span(self) -> tuple[float, float] | None:
        return ALIAS_SPANS.get(self.alias) or UNIT_SPANS.get(self.unit)

    def fmt(self, v: float | None) -> str:
        if v is None or not math.isfinite(v):
            return "n/a"
        d = self.scale if self.scale is not None else 2
        return f"{v:.{max(d, 0)}f}"

    def fmtu(self, v: float | None) -> str:
        s = self.fmt(v)
        return f"{s} {self.unit}".strip() if self.unit else s


def eligible(p: data.Param) -> bool:
    """Numeric, displayed, logged, non-writable, non-status parameter."""
    return p.data_type == "number" and p.is_displayed and p.is_logged and not p.is_writable and p.alias not in data.STATUS_PARAMS


# ---------------------------------------------------------------------------
# Detectors


@dataclass
class Hit:
    detector: str
    idx: int
    severity: str
    score: float
    value: float
    expected: float | None
    details: dict[str, Any] = field(default_factory=dict)


@dataclass
class Baseline:
    """Statistics of the running samples of an evaluation block's baseline."""

    center: float
    sigma: float  # robust sigma of x
    n: int
    dev_floor: float = float("nan")  # z-score scale floor from the baseline deviations

    @classmethod
    def of(cls, x: np.ndarray, mask: np.ndarray) -> Baseline:
        v = x[mask & np.isfinite(x)]
        c, s = robust_center_scale(v)
        return cls(c, s, len(v))


def baseline_mask(n: int, lo: int, hi: int, guard: int) -> np.ndarray:
    """Samples outside [lo - guard, hi + guard]: the independent baseline of the block lo..hi."""
    mask = np.ones(n, dtype=bool)
    mask[max(0, lo - guard) : min(n, hi + guard + 1)] = False
    return mask


def detect_range(x: np.ndarray, run: np.ndarray, meta: ParamMeta, idx: np.ndarray) -> list[Hit]:
    span = meta.span
    if span is None:
        return []
    lo, hi = span
    hits = []
    for i in idx:
        v = x[i]
        if not run[i] or not math.isfinite(v) or lo <= v <= hi:
            continue
        excess = v - hi if v > hi else lo - v
        hits.append(Hit("range", int(i), "CRITICAL", float(excess), float(v), None, {"lo": lo, "hi": hi}))
    return hits


def _runs(mask: np.ndarray) -> tuple[np.ndarray, list[tuple[int, int]]]:
    """Length of the true run ending at each sample, and every run as (start, end)."""
    run_len = np.zeros(len(mask), dtype=int)
    for i in np.flatnonzero(mask):
        run_len[i] = run_len[i - 1] + 1 if i > 0 else 1
    ends = [int(i) for i in np.flatnonzero(mask) if i + 1 == len(mask) or not mask[i + 1]]
    return run_len, [(e - int(run_len[e]) + 1, e) for e in ends]


def detect_flatline(x: np.ndarray, run: np.ndarray, meta: ParamMeta, idx: np.ndarray, step_s: int, cfg: AnomalyConfig) -> list[Hit]:
    w = n_samples(cfg.flatline_min, step_s)
    xr = np.where(run, x, np.nan)
    res = meta.resolution

    def flat_windows(width: int) -> np.ndarray:
        rng = rolling_range(xr, width, min_count=width)  # whole window running and valid
        ok = np.isfinite(rng) & (np.nan_to_num(rng, nan=np.inf) < res - 1e-12)
        if meta.unit == "%":
            # an output pinned at its end stop (valve shut / wide open, fan at full speed) is saturated, not stuck
            v = np.nan_to_num(xr, nan=50.0)
            ok &= (v > 0.5) & (v < 99.5)
        return ok

    evaluable = np.isfinite(rolling_range(xr, w, min_count=w))
    flat = flat_windows(w)
    # eligibility: exclude the current trailing flat run so a stuck sensor never disqualifies itself
    not_flat = np.flatnonzero(evaluable & ~flat)
    cut = int(not_flat[-1]) + 1 if len(not_flat) else 0
    base_eval = np.count_nonzero(evaluable[:cut])
    if base_eval < max(60, w):
        return []
    if np.count_nonzero(flat[:cut]) / base_eval > cfg.flatline_max_flat_fraction:
        return []
    run_len, _ = _runs(flat)
    still: list[tuple[int, int]] = []
    ws = n_samples(cfg.flatline_still_min, step_s)
    if cfg.novelty:
        # every stretch of stillness (short windows): a quantised, quiet signal sits still for a
        # while now and then; only a stillness clearly longer than any other one is new
        _, sruns = _runs(flat_windows(ws))
        still = [(s - ws + 1, e) for s, e in sruns]  # sample extents
    hits = []
    for i in idx:
        if not flat[i]:
            continue
        minutes = (run_len[i] - 1) * step_s / 60.0 + cfg.flatline_min
        if cfg.novelty:
            lo = int(i) - int(run_len[i]) - w + 2  # first sample of the current flat stretch
            longest = max(((e - s + 1) * step_s / 60.0 for s, e in still if e < lo or s > i), default=0.0)
            if minutes < cfg.flatline_novelty * longest:
                continue
        hits.append(Hit("flatline", int(i), "WARNING", float(minutes), float(x[i]), None, {"resolution": res, "minutes": round(minutes, 1)}))
    return hits


def zscores(x: np.ndarray, run: np.ndarray, idx: np.ndarray, step_s: int, base: Baseline, meta: ParamMeta, cfg: AnomalyConfig) -> tuple[np.ndarray, np.ndarray]:
    """Robust modified z at the indices (NaN where not evaluable) and the window medians."""
    w = n_samples(cfg.zscore_window_min, step_s)
    xr = np.where(run, x, np.nan)
    rows = sliding_windows(xr, w, idx, include_current=False)
    cnt = np.count_nonzero(~np.isnan(rows), axis=1)
    med = nanmedian_rows(rows)
    mad = nanmedian_rows(np.abs(rows - med[:, None]))
    floor = max(meta.resolution, cfg.zscore_floor_fraction * (base.sigma if math.isfinite(base.sigma) else 0.0))
    if math.isfinite(base.dev_floor):
        floor = max(floor, base.dev_floor)
    scale = np.maximum(1.4826 * np.nan_to_num(mad), floor)
    with np.errstate(invalid="ignore"):
        m = (xr[idx] - med) / scale
    m = np.where((cnt >= cfg.zscore_min_samples) & np.isfinite(xr[idx]), m, np.nan)
    return m, med


def detect_zscore(x: np.ndarray, run: np.ndarray, meta: ParamMeta, idx: np.ndarray, step_s: int, base: Baseline, cfg: AnomalyConfig) -> list[Hit]:
    k = cfg.zscore_consecutive
    if len(idx) == 0:
        return []
    ext = np.arange(max(0, int(idx[0]) - (k - 1)), int(idx[-1]) + 1)
    m, med = zscores(x, run, ext, step_s, base, meta, cfg)
    pos = {int(i): j for j, i in enumerate(ext)}
    hits = []
    for i in idx:
        j = pos[int(i)]
        if j < k - 1:
            continue
        win = m[j - k + 1 : j + 1]
        if np.any(np.isnan(win)):
            continue
        if not (np.all(win > cfg.zscore_threshold) or np.all(win < -cfg.zscore_threshold)):
            continue
        sev = "CRITICAL" if np.min(np.abs(win)) > cfg.zscore_critical else "WARNING"
        hits.append(Hit("zscore", int(i), sev, float(m[j]), float(x[i]), float(med[j]), {"window_min": cfg.zscore_window_min, "median": float(med[j])}))
    return hits


def ewma_limits(z: np.ndarray, run: np.ndarray, mask: np.ndarray, base: Baseline, step_s: int, meta: ParamMeta, cfg: AnomalyConfig) -> tuple[float, np.ndarray]:
    """Center and half-width (per sample) of the EWMA limits from the block's baseline."""
    lam = cfg.ewma_lambda
    floor = meta.resolution
    if cfg.ewma_sigma_mode == "classic":
        sigma = max(base.sigma, floor) if math.isfinite(base.sigma) else floor
        # samples since the statistic (re)started
        valid = ~np.isnan(z)
        t = np.zeros(len(z))
        count, last = 0, -(10**9)
        gap = n_samples(cfg.ewma_reset_gap_min, step_s)
        for i in np.flatnonzero(valid):
            count = count + 1 if i - last <= gap else 1
            t[i] = count
            last = i
        return base.center, cfg.ewma_L * sigma * np.sqrt(lam / (2 - lam) * (1 - (1 - lam) ** (2 * t)))
    # Phase I (3-sigma trimmed) mean and sample sigma of the statistic over the baseline,
    # like the twin's SPC: excursions do not inflate the limits, multi-modal operation is covered
    base_z = phase_one(z[mask & run & np.isfinite(z)])
    if len(base_z) >= 2:
        zc, zs = float(np.mean(base_z)), float(np.std(base_z, ddof=1))
    else:
        zc, zs = base.center, float("nan")
    zs = max(zs, floor) if math.isfinite(zs) else floor
    return (zc if math.isfinite(zc) else base.center), np.full(len(z), cfg.ewma_L * zs)


def detect_ewma(
    x: np.ndarray, z: np.ndarray, run: np.ndarray, mask: np.ndarray, meta: ParamMeta, idx: np.ndarray, step_s: int, base: Baseline, cfg: AnomalyConfig
) -> list[Hit]:
    if base.n < cfg.min_baseline_samples or not math.isfinite(base.center) or len(idx) == 0:
        return []
    center, half = ewma_limits(z, run, mask, base, step_s, meta, cfg)
    lo_i = max(0, int(idx[0]) - 4 * cfg.ewma_of_last)
    beyond = np.full(len(z), np.nan)
    seg = slice(lo_i, int(idx[-1]) + 1)
    beyond[seg] = np.where(np.isnan(z[seg]), np.nan, (np.abs(z[seg] - center) > half[seg]).astype(float))
    new: dict[int, bool] = {}
    if cfg.novelty:
        # the statistic must leave the range it took elsewhere in the baseline
        z_lo, z_hi = range_excluding(np.where(run, z, np.nan), idx, n_samples(cfg.novelty_min, step_s))
        m = cfg.ewma_novelty_sigma * half / cfg.ewma_L
        for j, i in enumerate(idx):
            if np.isnan(z[i]):
                continue
            if z[i] > center:
                new[int(i)] = bool(np.isnan(z_hi[j]) or z[i] > z_hi[j] + m[i])
            else:
                new[int(i)] = bool(np.isnan(z_lo[j]) or z[i] < z_lo[j] - m[i])
    hits = []
    for i in idx:
        if np.isnan(beyond[i]) or beyond[i] < 0.5:
            continue
        recent = beyond[max(0, i - 4 * cfg.ewma_of_last) : i + 1]
        recent = recent[~np.isnan(recent)][-cfg.ewma_of_last :]
        if len(recent) < cfg.ewma_of_last or np.count_nonzero(recent >= 0.5) < cfg.ewma_hits:
            continue
        if cfg.novelty and not new.get(int(i), False):
            continue
        score = float((z[i] - center) / (half[i] / cfg.ewma_L)) if half[i] > 0 else 0.0
        hits.append(
            Hit("ewma", int(i), "WARNING", score, float(x[i]), float(center),
                {"ewma": float(z[i]), "center": float(center), "half_width": float(half[i]), "lambda": cfg.ewma_lambda, "L": cfg.ewma_L, "sigma_mode": cfg.ewma_sigma_mode})  # fmt: skip
        )
    return hits


def detect_spc(ends: np.ndarray, means: np.ndarray, mask: np.ndarray, idx: np.ndarray, step_s: int, cfg: AnomalyConfig) -> list[Hit]:
    """Engine-equivalent rules on the 15-minute means ending in idx, Phase I limits from the baseline buckets."""
    if len(idx) == 0 or len(means) == 0:
        return []
    base = phase_one(means[mask[ends]])
    if len(base) < cfg.spc_min_buckets:
        return []
    cl, sd = float(np.mean(base)), float(np.std(base, ddof=1))
    if not sd > 0:
        return []
    lo_idx, hi_idx = int(idx[0]), int(idx[-1])
    evaluated = np.flatnonzero((ends >= lo_idx) & (ends <= hi_idx))
    if len(evaluated) == 0:
        return []
    guard = max(1, int(math.ceil(cfg.novelty_min / cfg.spc_bucket_min)))
    other_lo, other_hi = range_excluding(means, evaluated, guard)
    margin = cfg.spc_novelty_sigma * sd
    hits = []
    for k, j in enumerate(evaluated):
        v = means[j]
        z = (v - cl) / sd

        def novel(up: bool, k: int = k, v: float = v) -> bool:
            if not cfg.novelty:
                return True
            edge = other_hi[k] if up else other_lo[k]
            return bool(np.isnan(edge) or (v > edge + margin if up else v < edge - margin))

        fired = []
        if abs(z) > 3 and novel(z > 0):
            fired.append("SPC_OUT_OF_CONTROL")
        if j >= 2:
            w = (means[j - 2 : j + 1] - cl) / sd
            if (np.count_nonzero(w > 2) >= 2 and z > 2 and novel(True)) or (np.count_nonzero(w < -2) >= 2 and z < -2 and novel(False)):
                fired.append("SPC_2_OUT_OF_3_POINTS_ZONE_A_OR_BEYOND")
        if j >= 6:
            w = means[j - 6 : j + 1]
            d = np.diff(w)
            rising, falling = bool(np.all(d > 0)), bool(np.all(d < 0))
            if (rising or falling) and abs(w[-1] - w[0]) >= cfg.spc_monotonic_min_sigma * sd and novel(rising):
                fired.append("SPC_7_POINTS_MONOTONIC")
        if fired:
            hits.append(
                Hit("spc", int(ends[j]), "WARNING", float(z), float(v), cl,
                    {"rules": fired, "cl": cl, "sigma": sd, "ucl": cl + 3 * sd, "lcl": cl - 3 * sd, "bucket_min": cfg.spc_bucket_min, "bucket_mean": float(v)})  # fmt: skip
            )
    return hits


def gate_running(running: np.ndarray, step_s: int, cfg: AnomalyConfig) -> np.ndarray:
    """Running and past the start-up settle time."""
    settle = n_samples(cfg.start_settle_min, step_s) if cfg.start_settle_min > 0 else 1
    return run_for(running, settle)


def detect(
    x: np.ndarray,
    running: np.ndarray,
    meta: ParamMeta,
    *,
    t0: datetime,
    step_s: int,
    start: int = 0,
    end: int | None = None,
    cfg: AnomalyConfig = DEFAULT_CONFIG,
    detectors: tuple[str, ...] = DETECTORS,
) -> list[Hit]:
    """All detector hits at indices start..end of a series (x includes the baseline).

    running: 1.0 / 0.0 per sample (RUNNING status); the start-up settle time is applied here.
    The evaluated samples are taken in blocks of `block_min`; the statistical
    detectors judge each block against the running samples outside it and its
    guard (in production: the trailing 24 h up to 2 h before; on a simulation
    series: the rest of the run), so an excursion never sets its own limits.
    A block without `min_baseline_samples` such samples gets no statistical
    judgement (range and flatline still apply).
    """
    n = len(x)
    end = n - 1 if end is None else min(end, n - 1)
    if end < start or n == 0:
        return []
    idx = np.arange(max(0, start), end + 1)
    run = gate_running(running, step_s, cfg)
    xr = np.where(run, x, np.nan)
    hits: list[Hit] = []
    if "range" in detectors:
        hits += detect_range(x, run, meta, idx)
    if "flatline" in detectors:
        hits += detect_flatline(x, run, meta, idx, step_s, cfg)
    want = [d for d in ("zscore", "ewma", "spc") if d in detectors]
    finite_run = run & np.isfinite(x)
    if not want or np.count_nonzero(finite_run) < cfg.min_baseline_samples:
        return hits
    guard = n_samples(cfg.guard_min, step_s)
    block = n_samples(cfg.block_min, step_s)
    dev = z = ends = means = None
    if "zscore" in want and cfg.novelty and cfg.zscore_dev_quantile > 0:
        w = n_samples(cfg.zscore_window_min, step_s)
        dev = np.abs(xr - pd.Series(xr).rolling(w, min_periods=cfg.zscore_min_samples).median().shift(1).to_numpy())
    if "ewma" in want:
        start_value = robust_center_scale(x[finite_run])[0]
        z = ewma_gated(xr, cfg.ewma_lambda, init=start_value, reset_gap=n_samples(cfg.ewma_reset_gap_min, step_s))
    if "spc" in want:
        ends, means = bucket_means(x, finite_run, t0, step_s, cfg.spc_bucket_min)
    for b0 in range(0, len(idx), block):
        bidx = idx[b0 : b0 + block]
        mask = baseline_mask(n, int(bidx[0]), int(bidx[-1]), guard)
        base = Baseline.of(x, mask & run)
        if base.n < cfg.min_baseline_samples:
            continue  # no independent baseline for this block
        if "zscore" in want:
            if dev is not None:
                d = dev[mask & np.isfinite(dev)]
                if len(d) >= cfg.min_baseline_samples:
                    base.dev_floor = cfg.zscore_dev_margin * float(np.quantile(d, cfg.zscore_dev_quantile)) / cfg.zscore_threshold
            hits += detect_zscore(x, run, meta, bidx, step_s, base, cfg)
        if z is not None:
            hits += detect_ewma(x, z, run, mask, meta, bidx, step_s, base, cfg)
        if ends is not None and means is not None:
            hits += detect_spc(ends, means, mask, bidx, step_s, cfg)
    return hits


def clear_minutes(detector: str, cfg: AnomalyConfig = DEFAULT_CONFIG) -> float:
    """Minutes without a hit before an anomaly clears (SPC hits come once per bucket)."""
    return max(cfg.clear_min, 2 * cfg.spc_bucket_min) if detector == "spc" else cfg.clear_min


def describe(hit: Hit, meta: ParamMeta) -> str:
    name = meta.name
    d = hit.details
    if hit.detector == "range":
        return f"{name} reads {meta.fmtu(hit.value)}, outside the physical range {d['lo']:g} to {d['hi']:g} {meta.unit}".rstrip()
    if hit.detector == "flatline":
        return f"{name} has not changed ({meta.fmtu(hit.value)}) for {d['minutes']:.0f} min while running"
    if hit.detector == "zscore":
        return f"{name} at {meta.fmtu(hit.value)} against a 60-min median of {meta.fmtu(hit.expected)} (robust z {hit.score:+.1f})"
    if hit.detector == "ewma":
        return f"{name} drifted: EWMA {meta.fmtu(d['ewma'])} outside {meta.fmt(d['center'])} ± {meta.fmtu(d['half_width'])}"
    if hit.detector == "spc":
        rules = ", ".join(r.removeprefix("SPC_").replace("_", " ").lower() for r in d["rules"])
        return f"{name} 15-min mean {meta.fmtu(hit.value)}: {rules} (CL {meta.fmt(d['cl'])}, UCL {meta.fmt(d['ucl'])}, LCL {meta.fmt(d['lcl'])})"
    return name


# ---------------------------------------------------------------------------
# Batch API (simulation post-analysis)


@dataclass
class AnomalyEpisode:
    detector: str
    start: int
    end: int
    severity: str
    score: float
    value: float
    expected: float | None
    message: str
    details: dict[str, Any]


def detect_batch(x: np.ndarray, running: np.ndarray, meta: ParamMeta, *, t0: datetime, step_s: int, cfg: AnomalyConfig = BATCH_CONFIG) -> list[AnomalyEpisode]:
    """Episodes of every detector over a whole series (each block judged against the rest of the series)."""
    hits = detect(x, running, meta, t0=t0, step_s=step_s, cfg=cfg)
    out: list[AnomalyEpisode] = []
    for det in DETECTORS:
        dh = sorted((h for h in hits if h.detector == det), key=lambda h: h.idx)
        if not dh:
            continue
        gap = n_samples(clear_minutes(det, cfg), step_s)
        by_idx = {h.idx: h for h in dh}
        for a, b in merge_hits([h.idx for h in dh], gap):
            group = [by_idx[i] for i in sorted(by_idx) if a <= i <= b]
            peak = max(group, key=lambda h: abs(h.score))
            sev = "CRITICAL" if any(h.severity == "CRITICAL" for h in group) else "WARNING"
            out.append(AnomalyEpisode(det, a, b, sev, peak.score, peak.value, peak.expected, describe(peak, meta), {**peak.details, "hits": len(group)}))
    return sorted(out, key=lambda e: e.start)


def detect_frame(frame: Frame, metas: dict[str, ParamMeta], cfg: AnomalyConfig = BATCH_CONFIG) -> list[tuple[str, str, AnomalyEpisode]]:
    """Episodes for every (alias, param) of a frame that has metadata; gating from each asset's `running` series."""
    out: list[tuple[str, str, AnomalyEpisode]] = []
    for key, x in frame.data.items():
        alias, param = key.split(".", 1)
        meta = metas.get(key)
        if meta is None:
            continue
        running = frame.get(alias, "running")
        run = np.ones(frame.n) if running is None else np.where(np.isnan(running), 0.0, (running >= 0.5).astype(float))
        for ep in detect_batch(x, run, meta, t0=frame.t0, step_s=frame.step_s, cfg=cfg):
            out.append((alias, param, ep))
    return out


# ---------------------------------------------------------------------------
# Production runner


@dataclass
class _Tracked:
    param: data.Param
    asset: data.Asset
    meta: ParamMeta


class AnomalyRunner:
    """Incremental production detection with in-memory 27 h buffers per parameter (24 h baseline + guard + new samples)."""

    META_REFRESH = timedelta(minutes=10)
    FETCH_OVERLAP = timedelta(minutes=3)

    def __init__(self, site_code: str | None = None, cfg: AnomalyConfig = DEFAULT_CONFIG):
        self.site = site_code or data.settings_site()
        self.cfg = cfg
        self.step_s = 60
        # 24 h of baseline before the guard, plus the guard and an hour of new samples: an event
        # that recurs at the same time every day is then inside its own baseline (not at the edge)
        self.n = int(cfg.baseline_hours * 60 + cfg.guard_min + 60) + 1
        self.t0: datetime | None = None
        self.buf: dict[int, np.ndarray] = {}
        self.tracked: dict[int, _Tracked] = {}
        self.meta_at: datetime | None = None
        self.fetched_until: datetime | None = None
        self.evaluated_until: datetime | None = None

    # -- buffers ---------------------------------------------------------

    def _index(self, ts: datetime) -> int:
        assert self.t0 is not None
        return int((ts.timestamp() - self.t0.timestamp()) // self.step_s)

    def _shift(self, new_t0: datetime) -> None:
        if self.t0 is None:
            self.t0 = new_t0
            return
        k = int((new_t0 - self.t0).total_seconds() // self.step_s)
        if k <= 0:
            return
        for pid, arr in self.buf.items():
            if k >= self.n:
                arr[:] = np.nan
            else:
                arr[:-k] = arr[k:]
                arr[-k:] = np.nan
        self.t0 = new_t0

    def _store(self, rows: list[tuple[int, datetime, float | None]]) -> None:
        for pid, ts, v in rows:
            arr = self.buf.get(pid)
            if arr is None or v is None:
                continue
            i = self._index(ts)
            if 0 <= i < self.n and math.isfinite(v):
                arr[i] = v  # last value of the slot wins (re-fetched overlap is idempotent)

    async def _refresh_meta(self, now: datetime) -> list[int]:
        assets = {a.id: a for a in await data.load_assets(self.site)}
        params = [p for p in await data.load_params(list(assets)) if eligible(p)]
        new_ids = []
        tracked = {}
        for p in params:
            tracked[p.id] = _Tracked(p, assets[p.asset_id], ParamMeta(p.alias, p.name, p.unit, p.scale))
            if p.id not in self.buf:
                self.buf[p.id] = np.full(self.n, np.nan)
                new_ids.append(p.id)
        for pid in set(self.buf) - set(tracked):
            del self.buf[pid]
        self.tracked = tracked
        self.meta_at = now
        return new_ids

    async def warm_up(self, now: datetime | None = None) -> None:
        now = (now or data.utcnow()).replace(second=0, microsecond=0)
        self._shift(now - timedelta(minutes=self.n - 1))
        await self._refresh_meta(now)
        rows = await data.fetch_values(list(self.buf), self.t0 - timedelta(seconds=1), now + timedelta(minutes=1))
        self._store(rows)
        self.fetched_until = max((r[1] for r in rows), default=now)
        # detection starts with the latest complete minutes, not the warm-up history
        self.evaluated_until = now - timedelta(minutes=3)
        log.info("anomaly warm-up site=%s params=%s rows=%s", self.site, len(self.buf), len(rows))

    # -- cycle -----------------------------------------------------------

    async def cycle(self, now: datetime | None = None, *, dry_run: bool = False) -> dict[str, Any]:
        now = (now or data.utcnow()).replace(second=0, microsecond=0)
        if self.t0 is None:
            await self.warm_up(now)
        assert self.t0 is not None and self.fetched_until is not None and self.evaluated_until is not None
        self._shift(now - timedelta(minutes=self.n - 1))
        if self.meta_at is None or now - self.meta_at >= self.META_REFRESH:
            new_ids = await self._refresh_meta(now)
            if new_ids:
                self._store(await data.fetch_values(new_ids, self.t0 - timedelta(seconds=1), now + timedelta(minutes=1)))
        rows = await data.fetch_values(list(self.buf), self.fetched_until - self.FETCH_OVERLAP, now + timedelta(minutes=1))
        self._store(rows)
        if rows:
            self.fetched_until = max(self.fetched_until, max(r[1] for r in rows))
        # evaluate complete minutes after the last evaluated one
        last_complete = now - timedelta(minutes=1)
        start_i = max(0, self._index(self.evaluated_until) + 1)
        end_i = min(self.n - 1, self._index(last_complete))
        summary: dict[str, Any] = {"params": len(self.tracked), "rows": len(rows), "hits": 0, "opened": 0, "updated": 0, "cleared": 0}
        if end_i < start_i:
            return summary
        status = await data.fetch_status(sorted({t.asset.id for t in self.tracked.values()}), self.t0, now + timedelta(minutes=1))
        masks = {aid: data.running_mask(spans, self.t0, self.step_s, self.n) for aid, spans in status.items()}
        ones = np.ones(self.n)
        found: list[tuple[_Tracked, list[Hit]]] = []
        for pid, tr in self.tracked.items():
            x = pd.Series(self.buf[pid]).ffill(limit=2).to_numpy()
            hits = detect(x, masks.get(tr.asset.id, ones), tr.meta, t0=self.t0, step_s=self.step_s, start=start_i, end=end_i, cfg=self.cfg)
            if hits:
                found.append((tr, hits))
        self.evaluated_until = last_complete
        summary["hits"] = sum(len(h) for _, h in found)
        if dry_run:
            summary["planned"] = [
                {"asset": tr.asset.alias, "param": tr.param.alias, "detector": h.detector, "severity": h.severity, "message": describe(h, tr.meta)}
                for tr, hits in found
                for h in self._worst_per_detector(hits)
            ]
            return summary
        for tr, hits in found:
            for h in self._worst_per_detector(hits):
                first = min(x.idx for x in hits if x.detector == h.detector)
                last = max(x.idx for x in hits if x.detector == h.detector)
                inserted = await self._upsert(tr, h, self._time(first), self._time(last))
                summary["opened" if inserted else "updated"] += 1
        summary["cleared"] = await self._clear(now)
        return summary

    def _time(self, i: int) -> datetime:
        assert self.t0 is not None
        return self.t0 + timedelta(seconds=i * self.step_s)

    @staticmethod
    def _worst_per_detector(hits: list[Hit]) -> list[Hit]:
        best: dict[str, Hit] = {}
        for h in hits:
            cur = best.get(h.detector)
            rank = (h.severity == "CRITICAL", abs(h.score), h.idx)
            if cur is None or rank > (cur.severity == "CRITICAL", abs(cur.score), cur.idx):
                best[h.detector] = h
        return list(best.values())

    async def _upsert(self, tr: _Tracked, h: Hit, first: datetime, last: datetime) -> bool:
        details = {**h.details, "param_name": tr.param.name, "unit": tr.param.unit, "asset_alias": tr.asset.alias}
        row = await db.fetch_one(
            """INSERT INTO analytics.anomalies (site_code, asset_id, equipment_parameter_id, param_alias, detector, severity,
                   score, value, expected, message, details, started_at, last_seen_at)
               VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
               ON CONFLICT (equipment_parameter_id, detector) WHERE status = 'ACTIVE' DO UPDATE SET
                 last_seen_at = GREATEST(analytics.anomalies.last_seen_at, EXCLUDED.last_seen_at),
                 severity = CASE WHEN EXCLUDED.severity = 'CRITICAL' THEN 'CRITICAL' ELSE analytics.anomalies.severity END,
                 score = EXCLUDED.score, value = EXCLUDED.value, expected = EXCLUDED.expected,
                 message = EXCLUDED.message, details = EXCLUDED.details
               RETURNING (xmax = 0) AS inserted""",
            (
                self.site, tr.asset.id, tr.param.id, tr.param.alias, h.detector, h.severity, _f(h.score), _f(h.value),
                _f(h.expected), describe(h, tr.meta), data.jb(details), first, last,
            ),
        )  # fmt: skip
        if row and row["inserted"]:
            log.info("anomaly opened asset=%s param=%s detector=%s %s", tr.asset.alias, tr.param.alias, h.detector, describe(h, tr.meta))
            return True
        return False

    async def _clear(self, now: datetime) -> int:
        n = 0
        for det in DETECTORS:
            n += await db.execute(
                "UPDATE analytics.anomalies SET status = 'CLEARED', ended_at = last_seen_at"
                " WHERE status = 'ACTIVE' AND site_code = %s AND detector = %s AND last_seen_at < %s",
                (self.site, det, now - timedelta(minutes=clear_minutes(det, self.cfg))),
            )
        if n:
            log.info("anomalies cleared site=%s count=%s", self.site, n)
        return n


def _f(v: float | None) -> float | None:
    return None if v is None or not math.isfinite(v) else float(v)


def with_config(**overrides: Any) -> AnomalyConfig:
    return replace(DEFAULT_CONFIG, **overrides)
