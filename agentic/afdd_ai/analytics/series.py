"""Regular-grid time-series helpers shared by the anomaly detectors, the FDD
rules, rule calibration and the simulation post-analysis.

Every analytics computation works on a regular grid (one sample per `step_s`,
NaN = missing or gated out): production values are resampled to whole minutes,
simulation series already are regular. Conditions are float arrays holding
1.0 (true), 0.0 (false) or NaN (unknown: not running, not steady, no data).
"""

from __future__ import annotations

import math
import warnings
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

import numpy as np
import pandas as pd
from scipy.signal import lfilter


def as_array(values: Iterable[float | None] | np.ndarray) -> np.ndarray:
    """float64 array with None -> NaN."""
    if isinstance(values, np.ndarray):
        return values.astype(float, copy=False)
    return np.array([np.nan if v is None else float(v) for v in values], dtype=float)


def n_samples(minutes: float, step_s: float) -> int:
    """Number of grid samples spanning `minutes` (at least 1)."""
    return max(1, int(math.ceil(minutes * 60.0 / step_s - 1e-9)))


# ---------------------------------------------------------------------------
# Frames: aligned series of several assets on one grid


@dataclass
class Frame:
    """Aligned series of several assets on a regular UTC grid.

    `data` maps "ALIAS.param" to float arrays of len(times); `truth` optionally
    holds simulation ground truth for the same keys.
    """

    t0: datetime
    step_s: int
    n: int
    data: dict[str, np.ndarray] = field(default_factory=dict)

    def key(self, alias: str, param: str) -> str:
        return f"{alias}.{param}"

    def get(self, alias: str, param: str) -> np.ndarray | None:
        return self.data.get(f"{alias}.{param}")

    def has(self, alias: str, param: str) -> bool:
        return f"{alias}.{param}" in self.data

    def nan(self) -> np.ndarray:
        return np.full(self.n, np.nan)

    def get_or_nan(self, alias: str, param: str) -> np.ndarray:
        v = self.get(alias, param)
        return v if v is not None else self.nan()

    def time(self, i: int) -> datetime:
        return self.t0 + timedelta(seconds=self.step_s * int(i))

    @property
    def times(self) -> pd.DatetimeIndex:
        return pd.date_range(self.t0, periods=self.n, freq=f"{self.step_s}s", tz="UTC")

    @property
    def end(self) -> datetime:
        return self.time(self.n - 1) if self.n else self.t0

    def aliases(self) -> list[str]:
        return sorted({k.split(".", 1)[0] for k in self.data})

    def params_of(self, alias: str) -> list[str]:
        prefix = alias + "."
        return sorted(k[len(prefix) :] for k in self.data if k.startswith(prefix))


def floor_minute(t: datetime) -> datetime:
    return t.replace(second=0, microsecond=0)


def frame_from_rows(
    rows: Iterable[tuple[str, datetime, float | None]],
    start: datetime,
    end: datetime,
    *,
    step_s: int = 60,
    ffill_limit: int = 2,
) -> Frame:
    """Resample (key, ts, value) rows onto a regular grid [start, end].

    Samples are assigned to the grid slot that contains them (floor), averaged
    when several fall into one slot, and short gaps are forward-filled
    (production values are logged every 60-70 s, so some minutes are empty).
    """
    start = start.astimezone(timezone.utc)
    end = end.astimezone(timezone.utc)
    t0 = datetime.fromtimestamp(math.floor(start.timestamp() / step_s) * step_s, tz=timezone.utc)
    n = int((end - t0).total_seconds() // step_s) + 1
    frame = Frame(t0=t0, step_s=step_s, n=max(n, 0))
    if frame.n <= 0:
        return frame
    acc: dict[str, tuple[list[int], list[float]]] = {}
    base = t0.timestamp()
    for key, ts, value in rows:
        if value is None:
            continue
        v = float(value)
        if not math.isfinite(v):
            continue
        i = int((ts.timestamp() - base) // step_s)
        if 0 <= i < frame.n:
            idx, vals = acc.setdefault(key, ([], []))
            idx.append(i)
            vals.append(v)
    for key, (idx, vals) in acc.items():
        sums = np.bincount(idx, weights=vals, minlength=frame.n)
        counts = np.bincount(idx, minlength=frame.n)
        with np.errstate(invalid="ignore", divide="ignore"):
            arr = np.where(counts > 0, sums / np.maximum(counts, 1), np.nan)
        if ffill_limit > 0:
            arr = pd.Series(arr).ffill(limit=ffill_limit).to_numpy()
        frame.data[key] = arr
    return frame


# ---------------------------------------------------------------------------
# Rolling statistics (trailing windows that include the current sample)


def rolling_max(x: np.ndarray, window: int, min_count: int = 1) -> np.ndarray:
    return pd.Series(x).rolling(window, min_periods=min_count).max().to_numpy()


def rolling_min(x: np.ndarray, window: int, min_count: int = 1) -> np.ndarray:
    return pd.Series(x).rolling(window, min_periods=min_count).min().to_numpy()


def rolling_mean(x: np.ndarray, window: int, min_count: int = 1) -> np.ndarray:
    return pd.Series(x).rolling(window, min_periods=min_count).mean().to_numpy()


def rolling_range(x: np.ndarray, window: int, min_count: int = 1) -> np.ndarray:
    """max - min over the trailing window (NaN when fewer than min_count valid samples)."""
    s = pd.Series(x).rolling(window, min_periods=min_count)
    return (s.max() - s.min()).to_numpy()


def rolling_count(mask: np.ndarray, window: int) -> np.ndarray:
    """Number of true samples in the trailing window."""
    return pd.Series(mask.astype(float)).rolling(window, min_periods=1).sum().to_numpy()


def run_for(running: np.ndarray, window: int) -> np.ndarray:
    """True where the unit has been running for the whole trailing window."""
    r = np.where(np.isnan(running), 0.0, (running >= 0.5).astype(float))
    return pd.Series(r).rolling(window, min_periods=window).min().to_numpy() >= 0.5


def steady(x: np.ndarray, window: int, tol: float, min_fraction: float = 0.8) -> np.ndarray:
    """True where max - min of x over the trailing window stays within tol."""
    rng = rolling_range(x, window, min_count=max(1, int(window * min_fraction)))
    with np.errstate(invalid="ignore"):
        return np.nan_to_num(rng, nan=np.inf) <= tol


def sliding_windows(x: np.ndarray, window: int, idx: np.ndarray, *, include_current: bool) -> np.ndarray:
    """Rows of the trailing windows ending at each index in idx (NaN padded at the start).

    include_current=False gives the `window` samples strictly before the index.
    """
    pad = np.concatenate([np.full(window + 1, np.nan), x])
    view = np.lib.stride_tricks.sliding_window_view(pad, window)
    # view[k] covers pad[k : k+window]; the sample x[i] sits at pad[i+window+1]
    offset = 2 if include_current else 1
    return view[idx + offset]


def nanmedian_rows(w: np.ndarray) -> np.ndarray:
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", RuntimeWarning)
        return np.nanmedian(w, axis=1)


def range_excluding(x: np.ndarray, idx: np.ndarray, guard: int) -> tuple[np.ndarray, np.ndarray]:
    """(min, max) of x outside [i - guard, i + guard] for each i in idx (NaN when nothing is left).

    The "rest of the baseline" of a sample: used to tell a new excursion from a
    level the series already visited elsewhere in its window. O(n) with prefix /
    suffix extremes; NaN samples are ignored.
    """
    n = len(x)
    idx = np.asarray(idx, dtype=int)
    if n == 0 or len(idx) == 0:
        return np.full(len(idx), np.nan), np.full(len(idx), np.nan)
    hi = np.where(np.isnan(x), -np.inf, x)
    lo = np.where(np.isnan(x), np.inf, x)
    pre_max, pre_min = np.maximum.accumulate(hi), np.minimum.accumulate(lo)
    suf_max, suf_min = np.maximum.accumulate(hi[::-1])[::-1], np.minimum.accumulate(lo[::-1])[::-1]
    b, a = idx - guard - 1, idx + guard + 1
    has_b, has_a = b >= 0, a < n
    bi, ai = np.clip(b, 0, n - 1), np.clip(a, 0, n - 1)
    mx = np.maximum(np.where(has_b, pre_max[bi], -np.inf), np.where(has_a, suf_max[ai], -np.inf))
    mn = np.minimum(np.where(has_b, pre_min[bi], np.inf), np.where(has_a, suf_min[ai], np.inf))
    return np.where(np.isfinite(mn), mn, np.nan), np.where(np.isfinite(mx), mx, np.nan)


# ---------------------------------------------------------------------------
# EWMA with gating


def ewma_gated(x: np.ndarray, lam: float, *, init: float | None = None, reset_gap: int | None = 30) -> np.ndarray:
    """EWMA z_t = lam x_t + (1 - lam) z_{t-1} over the valid (non-NaN) samples only.

    Invalid samples hold the state and return NaN. After a gap longer than
    `reset_gap` samples the statistic restarts at the next valid sample (or
    at `init` when given). Vectorised with lfilter per contiguous segment.
    """
    out = np.full(len(x), np.nan)
    valid = np.flatnonzero(~np.isnan(x))
    if len(valid) == 0:
        return out
    # split into segments at long gaps
    if reset_gap is not None and len(valid) > 1:
        breaks = np.flatnonzero(np.diff(valid) > reset_gap) + 1
        segments = np.split(valid, breaks)
    else:
        segments = [valid]
    b, a = [lam], [1.0, -(1.0 - lam)]
    for seg in segments:
        xs = x[seg]
        z0 = xs[0] if init is None else init
        zi = [(1.0 - lam) * z0]
        z, _ = lfilter(b, a, xs, zi=zi)
        out[seg] = z
    return out


# ---------------------------------------------------------------------------
# Persistence: condition series -> episodes


@dataclass
class Episode:
    """A persistent condition: samples start..end (end = last true sample)."""

    start: int
    end: int
    confirmed: int  # sample where persistence was reached
    ongoing: bool  # still active at the end of the series (not cleared)


TRUE_FRACTION = 0.8  # share of known samples that must be true over the persistence window
MIN_COVERAGE = 0.6  # share of the window that must be known (gated in)


def _window_counts(cond: np.ndarray, window: int) -> tuple[np.ndarray, np.ndarray]:
    known = (~np.isnan(cond)).astype(float)
    true = np.where(np.isnan(cond), 0.0, (cond >= 0.5).astype(float))
    k = pd.Series(known).rolling(window, min_periods=1).sum().to_numpy()
    t = pd.Series(true).rolling(window, min_periods=1).sum().to_numpy()
    return k, t


def persistent_episodes(
    cond: np.ndarray,
    step_s: float,
    persist_min: float,
    clear_min: float,
    *,
    true_fraction: float = TRUE_FRACTION,
    min_coverage: float = MIN_COVERAGE,
    stale_min: float | None = None,
) -> list[Episode]:
    """Episodes where `cond` persists for `persist_min` and that end after `clear_min` of mostly false.

    cond: 1.0 true, 0.0 false, NaN unknown (gated out). An episode starts once
    the trailing `persist_min` window is known for at least `min_coverage` of
    its samples and at least `true_fraction` of the known samples are true (so
    noise around a threshold does not reset a real fault); its start is the
    first true sample of that window. It ends when the trailing `clear_min`
    window is known for `min_coverage` and at most 1 - `true_fraction` of it is
    true, or after `stale_min` without a known sample (no evidence either way);
    its end is the last true sample.
    """
    n = len(cond)
    persist_n = n_samples(persist_min, step_s)
    clear_n = n_samples(clear_min, step_s)
    stale_n = n_samples(stale_min, step_s) if stale_min else None
    kp, tp = _window_counts(cond, persist_n)
    kc, tc = _window_counts(cond, clear_n)
    with np.errstate(invalid="ignore", divide="ignore"):
        fire = (kp >= max(1.0, min_coverage * persist_n)) & (tp >= true_fraction * np.maximum(kp, 1)) & (tp > 0)
        settle = (kc >= max(1.0, min_coverage * clear_n)) & (tc <= (1 - true_fraction) * kc)
    is_true = np.where(np.isnan(cond), False, cond >= 0.5)
    known = ~np.isnan(cond)
    # the condition must already hold at the start of the window: it has persisted for the whole window
    starts = np.zeros(n, dtype=bool)
    if n >= persist_n:
        starts[persist_n - 1 :] = is_true[: n - persist_n + 1]
    fire &= starts
    out: list[Episode] = []
    on = False
    ep_start = confirmed = last_true = last_known = 0
    for i in range(n):
        if known[i]:
            last_known = i
        if not on:
            if fire[i] and is_true[i]:
                lo = max(0, i - persist_n + 1)
                first = lo + int(np.argmax(is_true[lo : i + 1]))
                on, ep_start, confirmed, last_true = True, first, i, i
            continue
        if is_true[i]:
            last_true = i
        if settle[i] or (stale_n is not None and i - last_known >= stale_n and not known[i]):
            out.append(Episode(ep_start, last_true, confirmed, False))
            on = False
    if on:
        out.append(Episode(ep_start, last_true, confirmed, True))
    return out


def tail_cleared(cond: np.ndarray, step_s: float, clear_min: float, *, true_fraction: float = TRUE_FRACTION, min_coverage: float = MIN_COVERAGE) -> bool:
    """The trailing clear window is known enough and mostly false (same rule as an episode end)."""
    clear_n = n_samples(clear_min, step_s)
    w = cond[-clear_n:]
    k = np.count_nonzero(~np.isnan(w))
    t = np.count_nonzero(np.nan_to_num(w, nan=0.0) >= 0.5)
    return k >= max(1.0, min_coverage * clear_n) and t <= (1 - true_fraction) * k


def merge_hits(hit_idx: Sequence[int], gap: int) -> list[tuple[int, int]]:
    """Group sorted hit indices into (first, last) runs; hits closer than `gap` samples join."""
    runs: list[tuple[int, int]] = []
    for i in hit_idx:
        if runs and i - runs[-1][1] <= gap:
            runs[-1] = (runs[-1][0], int(i))
        else:
            runs.append((int(i), int(i)))
    return runs


# ---------------------------------------------------------------------------
# Robust statistics


def robust_center_scale(x: np.ndarray) -> tuple[float, float]:
    """(median, 1.4826 * MAD) of the finite values; (nan, nan) when empty."""
    v = x[np.isfinite(x)]
    if len(v) == 0:
        return float("nan"), float("nan")
    med = float(np.median(v))
    return med, float(1.4826 * np.median(np.abs(v - med)))


def phase_one(values: np.ndarray, passes: int = 5, min_points: int = 8) -> np.ndarray:
    """Phase I revised sample: drop points beyond 3 sigma iteratively (like the twin's SPC)."""
    base = values[np.isfinite(values)]
    for _ in range(passes):
        if len(base) < 2:
            break
        mean, sd = float(np.mean(base)), float(np.std(base, ddof=1))
        if sd <= 0:
            break
        kept = base[np.abs(base - mean) <= 3 * sd]
        if len(kept) == len(base) or len(kept) < min_points:
            break
        base = kept
    return base


def bucket_means(x: np.ndarray, valid: np.ndarray, t0: datetime, step_s: int, bucket_min: int, min_fraction: float = 0.67) -> tuple[np.ndarray, np.ndarray]:
    """Means over absolute-aligned buckets of `bucket_min` minutes.

    Returns (bucket_end_index, mean) for complete buckets where at least
    `min_fraction` of the samples are valid; bucket_end_index is the last
    sample index of the bucket.
    """
    per = n_samples(bucket_min, step_s)
    offset = int((t0.timestamp() // step_s) % per)  # samples already elapsed in the first bucket
    first = (per - offset) % per  # first sample index starting a bucket
    ends, means = [], []
    xv = np.where(valid, x, np.nan)
    for s in range(first, len(x) - per + 1, per):
        w = xv[s : s + per]
        k = np.count_nonzero(~np.isnan(w))
        if k >= per * min_fraction:
            ends.append(s + per - 1)
            means.append(float(np.nanmean(w)))
    return np.array(ends, dtype=int), np.array(means, dtype=float)


def count_reversals(x: np.ndarray, deadband: float) -> int:
    """Direction reversals of a signal with a deadband (NaN samples skipped)."""
    v = x[~np.isnan(x)]
    if len(v) < 3:
        return 0
    direction = 0  # 0 until the first move beyond the deadband
    lo = hi = ref = float(v[0])
    reversals = 0
    for val in v[1:]:
        if direction == 0:
            lo, hi = min(lo, val), max(hi, val)
            if val - lo > deadband:
                direction, ref = 1, val
            elif hi - val > deadband:
                direction, ref = -1, val
        elif direction == 1:
            if val > ref:
                ref = val
            elif ref - val > deadband:
                reversals += 1
                direction, ref = -1, val
        else:
            if val < ref:
                ref = val
            elif val - ref > deadband:
                reversals += 1
                direction, ref = 1, val
    return reversals


def rolling_reversals(x: np.ndarray, window: int, deadband: float, idx: Iterable[int]) -> np.ndarray:
    """Reversal counts of the trailing window ending at each index in idx."""
    out = np.full(len(x), np.nan)
    for i in idx:
        lo = max(0, i - window + 1)
        out[i] = count_reversals(x[lo : i + 1], deadband)
    return out
