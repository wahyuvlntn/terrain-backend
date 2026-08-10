"""Unit tests of the grid helpers (persistence, EWMA, reversals, resampling)."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import numpy as np

from afdd_ai.analytics import series as s


def test_persistent_episode_needs_persistence_and_clears():
    cond = np.zeros(200)
    cond[50:120] = 1.0
    eps = s.persistent_episodes(cond, 60, persist_min=30, clear_min=20)
    assert len(eps) == 1
    ep = eps[0]
    assert ep.start == 50 and ep.end == 119 and not ep.ongoing
    assert ep.confirmed >= 50 + 29


def test_persistent_episode_too_short_is_ignored():
    cond = np.zeros(200)
    cond[50:70] = 1.0
    assert s.persistent_episodes(cond, 60, persist_min=30, clear_min=20) == []


def test_persistence_tolerates_noise_and_unknown_samples():
    rng = np.random.default_rng(0)
    cond = np.zeros(300)
    cond[100:] = (rng.random(200) < 0.9).astype(float)  # 90 % true with noise
    cond[150:160] = np.nan  # gated out
    eps = s.persistent_episodes(cond, 60, persist_min=30, clear_min=30)
    assert len(eps) == 1 and eps[0].ongoing and 100 <= eps[0].start <= 105


def test_stale_episode_closes_without_evidence():
    cond = np.full(400, np.nan)
    cond[0:60] = 1.0
    eps = s.persistent_episodes(cond, 60, persist_min=30, clear_min=30, stale_min=120)
    assert len(eps) == 1 and not eps[0].ongoing and eps[0].end == 59


def test_tail_cleared():
    cond = np.ones(100)
    assert not s.tail_cleared(cond, 60, 30)
    cond[-40:] = 0.0
    assert s.tail_cleared(cond, 60, 30)


def test_ewma_gated_holds_and_resets():
    x = np.array([1.0, 1.0, np.nan, 3.0] + [np.nan] * 40 + [10.0])
    z = s.ewma_gated(x, 0.5, init=0.0, reset_gap=30)
    assert np.isnan(z[2])
    assert z[0] == 0.5 and z[1] == 0.75 and z[3] == 0.75 * 0.5 + 1.5
    assert z[-1] == 0.5 * 0.0 + 0.5 * 10.0  # restarted from init after the long gap


def test_count_reversals_with_deadband():
    t = np.arange(120)
    hunting = 10 * np.sin(2 * np.pi * t / 10)  # 12 cycles: about 23 reversals
    assert s.count_reversals(hunting, 2.0) >= 20
    noise = np.random.default_rng(1).normal(0, 0.3, 120)
    assert s.count_reversals(noise, 2.0) <= 2
    assert s.count_reversals(np.linspace(0, 50, 120), 2.0) == 0


def test_frame_from_rows_resamples_and_forward_fills():
    t0 = datetime(2026, 10, 1, tzinfo=timezone.utc)
    rows = [("A.x", t0 + timedelta(seconds=65 * i), float(i)) for i in range(10)]
    f = s.frame_from_rows(rows, t0, t0 + timedelta(minutes=12))
    x = f.get("A", "x")
    assert f.n == 13 and x is not None
    assert np.isfinite(x[:11]).all()  # 70 s logging leaves empty minutes, forward-filled
    assert x[0] == 0.0


def test_bucket_means_are_absolute_aligned():
    t0 = datetime(2026, 10, 1, 0, 7, tzinfo=timezone.utc)
    x = np.arange(60, dtype=float)
    ends, means = s.bucket_means(x, np.ones(60, dtype=bool), t0, 60, 15)
    # first complete bucket is 00:15-00:29 -> samples 8..22
    assert ends[0] == 22 and means[0] == np.mean(np.arange(8, 23))


def test_phase_one_trims_outliers():
    rng = np.random.default_rng(2)
    v = np.concatenate([rng.normal(10, 1, 200), [60, 70, 80]])
    base = s.phase_one(v)
    assert len(base) == 200 and abs(np.mean(base) - 10) < 0.3


def test_range_excluding_leaves_out_the_guard():
    x = np.array([5.0, 1.0, np.nan, 9.0, 2.0, 3.0, 7.0])
    lo, hi = s.range_excluding(x, np.array([3, 0, 6]), guard=1)
    # around 3: samples 2..4 excluded -> {5, 1, 3, 7}; around 0: {9, 2, 3, 7} (NaN ignored); around 6: {5, 1, 9, 2}
    assert lo.tolist() == [1.0, 2.0, 1.0] and hi.tolist() == [7.0, 9.0, 9.0]
    lo, hi = s.range_excluding(x, np.array([3]), guard=10)
    assert np.isnan(lo[0]) and np.isnan(hi[0])  # nothing left outside the guard
