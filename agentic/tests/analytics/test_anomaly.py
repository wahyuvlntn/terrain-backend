"""Sensor anomaly detectors on synthetic 1-minute series (DESIGN 6.1).

Signatures: out-of-range reading, stuck sensor (flat line), step / spike,
slow drift, sustained shift of the 15-minute means; healthy diurnal series
(including recurring process steps and saturated outputs) must stay quiet.
"""

from __future__ import annotations

from datetime import timedelta

import numpy as np
import pytest

from afdd_ai.analytics import anomaly
from afdd_ai.analytics.series import Frame

from .synth import T0, ar_noise, plant_frame, temperature_series

TEMP = anomaly.ParamMeta("chwrt", "Chilled Water Return Temp", "°C", 2)
VALVE = anomaly.ParamMeta("chw_valve", "CHW Valve Position", "%", 0)
PRESS = anomaly.ParamMeta("inlet_press", "Inlet Pressure", "mbar", 3)
DAY = 1440


def run_all(x: np.ndarray, running: np.ndarray | None = None, meta: anomaly.ParamMeta = TEMP, **kw) -> list[anomaly.Hit]:
    r = np.ones(len(x)) if running is None else running
    return anomaly.detect(x, r, meta, t0=T0, step_s=60, **kw)


def detectors(hits: list[anomaly.Hit]) -> set[str]:
    return {h.detector for h in hits}


BUFFER = anomaly.AnomalyRunner().n - 1  # the production runner's trailing buffer (minutes)


def live(x: np.ndarray, start: int, meta: anomaly.ParamMeta = TEMP, running: np.ndarray | None = None) -> list[anomaly.Hit]:
    """Production-like evaluation: the runner's trailing buffer, the samples from `start` evaluated 30 at a time."""
    hits: list[anomaly.Hit] = []
    for s in range(start, len(x), 30):
        lo = max(0, s - BUFFER)
        r = None if running is None else running[lo : s + 30]
        for h in run_all(x[lo : s + 30], r, meta, start=s - lo, end=s - lo + 29):
            h.idx += lo
            hits.append(h)
    return hits


# ---------------------------------------------------------------------------
# healthy series


@pytest.mark.parametrize("seed", range(4))
def test_healthy_diurnal_series_is_quiet_batch_and_live(seed):
    x = temperature_series(48, seed=seed)
    assert run_all(x) == []
    assert live(x, DAY) == []


def test_recurring_process_steps_are_not_anomalies():
    # a dry pump's inlet pressure dips at every chamber pump-down (about every 40 min)
    rng = np.random.default_rng(3)
    n = 2 * DAY
    x = 0.111 + rng.normal(0, 0.002, n)
    for s in range(15, n, 40):
        x[s : s + 6] = 0.019 + rng.normal(0, 0.001, min(6, n - s))
    x = np.round(x, 3)
    assert live(x, DAY, PRESS) == []


def test_saturated_output_is_not_a_stuck_sensor():
    t = np.arange(2 * DAY)
    x = np.clip(80 + 30 * np.sin(2 * np.pi * t / DAY) + ar_noise(np.random.default_rng(4), len(t), 1.0, 10), 0, 100)
    x = np.round(x, 0)
    assert "flatline" not in detectors(run_all(x, meta=VALVE))  # pinned at 100 % for hours every afternoon


def test_naturally_flat_parameter_is_not_flagged():
    # a measured value that a controller holds still (e.g. a set point echo) is flat most of the time
    x = np.full(2 * DAY, 120.0)
    x[600:640] = 118.0
    assert "flatline" not in detectors(run_all(x, meta=anomaly.ParamMeta("riser_dp_sp", "Riser DP SP", "kPa", 0)))


def test_not_running_samples_are_ignored():
    x = temperature_series(30, seed=1)
    running = np.ones(len(x))
    running[DAY + 100 : DAY + 300] = 0
    x[DAY + 100 : DAY + 300] = 150.0  # a stopped unit's sensor reads anything
    assert run_all(x, running) == []


def test_start_up_transient_is_skipped():
    x = temperature_series(30, seed=2)
    running = np.ones(len(x))
    running[DAY : DAY + 60] = 0
    x[DAY + 60 : DAY + 68] += np.linspace(5, 0.5, 8)  # warm water flushed through after a start
    assert "zscore" not in detectors(live(x, DAY, running=running))


# ---------------------------------------------------------------------------
# fault signatures


def test_range_out_of_physical_span_is_critical():
    x = temperature_series(26, seed=3)
    x[DAY + 50 : DAY + 55] = 85.0  # beyond the -10..80 °C span of a temperature sensor
    hits = [h for h in run_all(x) if h.detector == "range"]
    assert [h.idx for h in hits] == list(range(DAY + 50, DAY + 55))
    assert all(h.severity == "CRITICAL" for h in hits)
    assert "outside the physical range" in anomaly.describe(hits[0], TEMP)


def test_flatline_stuck_sensor():
    x = temperature_series(28, seed=4)
    x[DAY + 60 :] = x[DAY + 60]  # the reading freezes
    hits = [h for h in live(x, DAY) if h.detector == "flatline"]
    assert hits and hits[0].idx >= DAY + 60 + 29  # after 30 min at the earliest
    assert hits[0].severity == "WARNING"
    assert "has not changed" in anomaly.describe(hits[0], TEMP)


def test_zscore_step_is_flagged_but_a_single_spike_is_not():
    x = temperature_series(26, seed=5)
    x[DAY + 40] += 3.0  # one bad sample: needs 3 consecutive
    assert "zscore" not in detectors(live(x, DAY))
    x[DAY + 90 : DAY + 110] += 3.0  # a step of 3 K for 20 min
    hits = [h for h in live(x, DAY) if h.detector == "zscore"]
    assert hits and DAY + 92 <= hits[0].idx <= DAY + 95
    assert hits[0].severity == "CRITICAL" and hits[0].score > 6
    assert hits[0].expected == pytest.approx(x[DAY + 60], abs=0.5)


def test_ewma_slow_drift():
    x = temperature_series(30, seed=6)
    x[DAY + 60 :] += np.linspace(0, 2.0, len(x) - DAY - 60)  # sensor drifting 2 K over 5 h
    hits = [h for h in live(x, DAY) if h.detector == "ewma"]
    assert hits and hits[0].score > 0
    assert "drifted" in anomaly.describe(hits[0], TEMP)


def test_spc_sustained_shift_of_the_15_minute_means():
    x = temperature_series(30, seed=7)
    x[DAY + 120 :] += 3.0  # a sensor offset appears and stays (about 5 sigma of the 15-min means)
    hits = [h for h in live(x, DAY) if h.detector == "spc"]
    assert hits and DAY + 120 <= hits[0].idx <= DAY + 180
    assert {"SPC_OUT_OF_CONTROL", "SPC_2_OUT_OF_3_POINTS_ZONE_A_OR_BEYOND"} & set(hits[0].details["rules"])
    assert (hits[0].idx + 1) % 15 == 0  # evaluated at bucket ends
    assert "15-min mean" in anomaly.describe(hits[0], TEMP)


def day_night(hours: float, seed: int) -> np.ndarray:
    """A process with a day shift: +1.5 K from 08:00 to 20:00 with 10-min ramps, plus noise."""
    n = int(hours * 60)
    t = np.arange(n) % DAY
    level = np.interp(t, [0, 480, 490, 1200, 1210, DAY], [0, 0, 1.5, 1.5, 0, 0])
    return np.round(11.0 + level + ar_noise(np.random.default_rng(seed), n, 0.05, 15), 2)


def test_recurring_daily_steps_are_learnt_from_yesterday():
    x = day_night(48, seed=8)
    textbook = anomaly.with_config(novelty=False, spc_min_buckets=8)
    plain = [
        h
        for s in range(DAY, len(x), 30)
        for h in anomaly.detect(x[s - DAY : s + 30], np.ones(DAY + 30), TEMP, t0=T0, step_s=60, start=DAY, end=DAY + 29, cfg=textbook)
    ]
    assert "zscore" in detectors(plain)  # the textbook z-score flags the morning step every day
    assert live(x, DAY) == []  # yesterday's steps are in the baseline: not new
    x[DAY + 700 : DAY + 760] += 3.0  # a new excursion on day 2 still is
    assert "zscore" in detectors(live(x, DAY))


# ---------------------------------------------------------------------------
# batch API (simulation post-analysis)


def test_detect_batch_merges_hits_into_episodes():
    x = temperature_series(30, seed=9)
    x[DAY + 200 : DAY + 230] += 4.0
    eps = anomaly.detect_batch(x, np.ones(len(x)), TEMP, t0=T0, step_s=60)
    z = [e for e in eps if e.detector == "zscore"]
    assert len(z) == 1 and z[0].details["hits"] >= 3
    assert DAY + 200 <= z[0].start and z[0].end < DAY + 240
    assert z[0].severity == "CRITICAL" and z[0].message.startswith("Chilled Water Return Temp at")


def test_detect_frame_on_a_healthy_plant_is_quiet():
    frame = plant_frame(26, seed=12)
    metas = {
        "CH_01.chwrt": TEMP,
        "CH_01.chw_flow": anomaly.ParamMeta("chw_flow", "Chilled Water Flow", "m³/h", 0),
        "CT_01.cwst": anomaly.ParamMeta("cwst", "Cold Water Temp", "°C", 2),
        "CHWS_01.riser_dp": anomaly.ParamMeta("riser_dp", "Riser DP", "kPa", 0),
    }
    assert anomaly.detect_frame(frame, metas) == []


def test_detect_frame_gates_on_each_assets_running_series():
    frame = Frame(t0=T0, step_s=60, n=2 * DAY)
    x = temperature_series(48, seed=13)
    x[DAY + 300 : DAY + 340] = 150.0
    frame.data["CH_03.chwrt"] = x
    frame.data["CH_03.running"] = np.where((np.arange(2 * DAY) >= DAY + 290) & (np.arange(2 * DAY) < DAY + 350), 0.0, 1.0)
    assert anomaly.detect_frame(frame, {"CH_03.chwrt": TEMP}) == []
    frame.data["CH_03.running"] = np.ones(2 * DAY)
    found = anomaly.detect_frame(frame, {"CH_03.chwrt": TEMP})
    assert {e.detector for _, _, e in found} >= {"range"}


def test_eligibility_and_param_meta():
    from afdd_ai.analytics.data import Param

    def p(**kw) -> Param:
        base = dict(id=1, asset_id=1, alias="chwst", name="CHWST", unit="°C", data_type="number", scale=2, is_writable=False, is_displayed=True, is_logged=True)
        return Param(**(base | kw))

    assert anomaly.eligible(p())
    assert not anomaly.eligible(p(is_writable=True))
    assert not anomaly.eligible(p(alias="running"))
    assert not anomaly.eligible(p(data_type="boolean"))
    assert not anomaly.eligible(p(is_displayed=False))
    assert anomaly.ParamMeta("dew_point", "Dew point", "°C", 1).span == (-80.0, 30.0)
    assert anomaly.ParamMeta("x", "x", "°C", 1).span == (-10.0, 80.0)
    assert TEMP.resolution == 0.01 and TEMP.fmtu(6.5) == "6.50 °C"


def test_clear_window_is_longer_for_spc():
    assert anomaly.clear_minutes("zscore") == 15
    assert anomaly.clear_minutes("spc") == 30


def test_live_evaluation_uses_only_the_new_samples():
    x = temperature_series(26, seed=14)
    x[100:130] += 5.0  # an old excursion inside the baseline, already reported
    hits = run_all(x, start=len(x) - 5)
    assert all(h.idx >= len(x) - 5 for h in hits) and hits == []
    assert anomaly.AnomalyRunner.META_REFRESH == timedelta(minutes=10)
