"""LBNL fault detection rules: evaluation on aligned plant series.

The same evaluation serves three callers:

* production (`run_live`, every FDD_INTERVAL_S): the last 6 h of 1-minute
  production values; an ongoing episode opens / updates an ACTIVE row in
  `analytics.faults` (the engine raises it as an alarm with the rule code), a
  condition that has been false for the clear window clears it;
* simulation post-analysis (`detect_batch`): every episode of a job variant;
* calibration (`rule_statistics`): the gated rule statistics on fault-free data.

Every rule is gated on RUNNING (for a minimum time) and steady state, needs its
condition to persist (`persist_min`) and clears after `clear_min` of false
samples. Thresholds come from `analytics.fault_rules.params` (rules.py defaults,
calibrated). Signed residual rules require a consistent sign over the episode.
"""

from __future__ import annotations

import logging
import math
import re
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

import numpy as np

from ..common import db
from . import data, rules
from .series import (
    Episode,
    Frame,
    ewma_gated,
    n_samples,
    persistent_episodes,
    rolling_mean,
    rolling_range,
    rolling_reversals,
    run_for,
    steady,
    tail_cleared,
)

log = logging.getLogger(__name__)

LIVE_WINDOW_MIN = 360  # longest rule needs 2 h windows + 2 h persistence + warm-up
STALE_CLEAR_MIN = 240  # default stale_min: an ACTIVE fault without any valid evidence for this long is cleared
WATER_KJ = 1.1628  # kW per (m3/h * K) of water
CHILLER_KW = 3517.0  # 1000 RT


# ---------------------------------------------------------------------------
# Plant layout


@dataclass
class Layout:
    system: str | None
    chillers: list[str] = field(default_factory=list)
    towers: list[str] = field(default_factory=list)
    chw_pumps: list[str] = field(default_factory=list)
    cw_pumps: list[str] = field(default_factory=list)
    maus: list[str] = field(default_factory=list)

    @classmethod
    def from_aliases(cls, aliases: Iterable[str]) -> Layout:
        al = sorted(set(aliases))

        def pick(prefix: str) -> list[str]:
            return [a for a in al if re.fullmatch(prefix + r"\d+", a)]

        systems = pick("CHWS_")
        return cls(
            system=systems[0] if systems else None,
            chillers=pick("CH_"),
            towers=pick("CT_"),
            chw_pumps=pick("CHWP_"),
            cw_pumps=pick("CWP_"),
            maus=pick("MAU_"),
        )

    @classmethod
    def from_plant(cls, plant: data.Plant) -> Layout:
        systems = [a.alias for a in plant.by_role("chws")]
        return cls(
            system=systems[0] if systems else None,
            chillers=[a.alias for a in plant.by_role("chiller")],
            towers=[a.alias for a in plant.by_role("tower")],
            chw_pumps=[a.alias for a in plant.by_role("chwp")],
            cw_pumps=[a.alias for a in plant.by_role("cwp")],
            maus=[a.alias for a in plant.by_role("mau")],
        )

    def required_params(self) -> dict[str, list[str]]:
        """Parameters the rules read, per alias."""
        out: dict[str, list[str]] = {}
        for c in self.chillers:
            out[c] = ["running", "chwst", "chwrt", "load_pct", "chw_flow", "power_kw"]
        for t in self.towers:
            out[t] = ["running", "cwst", "fan_speed", "wet_bulb", "fan_mode"]
        for p in self.chw_pumps:
            out[p] = ["running", "speed", "suction_press", "discharge_press"]
        if self.system:
            out[self.system] = [
                "running", "chws_temp", "cooling_load", "wet_bulb", "cwst_sp", "tower_supply_temp", "riser_dp",
                "riser_dp_sp", "plant_flow", "chwp_speed_cmd", "twv_pos", "twv_cmd", "twv_min_pos", "cw_supply_temp",
                "cw_return_temp", "cw_flow", "fan_speed_cmd", "chillers_running",
            ]  # fmt: skip
        return out


# ---------------------------------------------------------------------------
# Results


@dataclass
class Detection:
    rule: str
    asset: str
    start: int
    end: int
    ongoing: bool
    intensity: float | None
    evidence: dict[str, Any]
    message: str
    confirmed: int = -1  # sample where the persistence window was met (the fault is raised from here)


@dataclass
class RuleEval:
    """One rule on one asset: its statistic, condition and episodes."""

    rule: str
    asset: str
    metric: np.ndarray
    cond: np.ndarray  # combined condition (1 true, 0 false, NaN unknown)
    detections: list[Detection]
    step_s: int
    clear_min: float
    skipped: str | None = None  # reason the rule could not be evaluated
    stale_min: float = STALE_CLEAR_MIN

    def last_valid(self) -> int | None:
        idx = np.flatnonzero(~np.isnan(self.cond))
        return int(idx[-1]) if len(idx) else None

    def tail_false(self) -> bool:
        """The trailing clear window is mostly false (fault cleared)."""
        return tail_cleared(self.cond, self.step_s, self.clear_min)


def _gate(cond: np.ndarray, gate: np.ndarray) -> np.ndarray:
    """Float condition with NaN where the gate is closed or the condition unknown."""
    out = np.where(cond, 1.0, 0.0)
    return np.where(gate, out, np.nan)


def _combine(*conds: np.ndarray) -> np.ndarray:
    """1 where any is true, 0 where all known and false, NaN where none is known."""
    stack = np.vstack(conds)
    anytrue = np.any(stack >= 0.5, axis=0)
    known = np.any(~np.isnan(stack), axis=0)
    return np.where(anytrue, 1.0, np.where(known, 0.0, np.nan))


def _merge_episodes(eps: list[Episode], gap: int) -> list[Episode]:
    eps = sorted(eps, key=lambda e: e.start)
    out: list[Episode] = []
    for e in eps:
        if out and e.start - out[-1].end <= gap:
            last = out[-1]
            out[-1] = Episode(last.start, max(last.end, e.end), min(last.confirmed, e.confirmed), last.ongoing or e.ongoing)
        else:
            out.append(e)
    return out


def _median(x: np.ndarray, ep: Episode, mask: np.ndarray | None = None) -> float | None:
    w = x[ep.start : ep.end + 1]
    if mask is not None:
        w = w[mask[ep.start : ep.end + 1]]
    w = w[np.isfinite(w)]
    return float(np.median(w)) if len(w) else None


def _mean_over(frame: Frame, alias: str | None, param: str, ep: Episode) -> float | None:
    if not alias:
        return None
    v = frame.get(alias, param)
    if v is None:
        return None
    w = v[ep.start : ep.end + 1]
    w = w[np.isfinite(w)]
    return round(float(np.mean(w)), 3) if len(w) else None


def stale_of(p: dict[str, Any]) -> float:
    """Minutes without any valid evidence after which an episode / ACTIVE fault ends (rule param stale_min)."""
    try:
        return float(p.get("stale_min") or STALE_CLEAR_MIN)
    except (TypeError, ValueError):
        return float(STALE_CLEAR_MIN)


def _r(v: float | None, d: int = 3) -> float | None:
    return None if v is None or not math.isfinite(v) else round(float(v), d)


def _running(frame: Frame, alias: str) -> np.ndarray:
    r = frame.get(alias, "running")
    return r if r is not None else frame.nan()


def _signed_episodes(e: np.ndarray, gate: np.ndarray, thr: float, extra_pos: np.ndarray, extra_neg: np.ndarray, frame: Frame, p: dict[str, Any]) -> tuple[list[Episode], np.ndarray, np.ndarray]:
    """Episodes of e > thr and e < -thr (consistent sign), with extra conditions."""
    cpos = _gate((e > thr) & extra_pos, gate & np.isfinite(e))
    cneg = _gate((e < -thr) & extra_neg, gate & np.isfinite(e))
    eps = []
    for c in (cpos, cneg):
        eps += persistent_episodes(c, frame.step_s, p["persist_min"], p["clear_min"], stale_min=stale_of(p))
    return sorted(eps, key=lambda x: x.start), cpos, cneg


# ---------------------------------------------------------------------------
# Rule statistics (shared with calibration)


def chl011_stats(frame: Frame, lay: Layout, p: dict[str, Any]) -> dict[str, dict[str, np.ndarray]]:
    """Per chiller: gated heat-balance residual r, its EWMA, and the plant mixing residual EWMA."""
    lam = float(p["ewma_lambda"])
    run_n = n_samples(p["run_min"], frame.step_s)
    st_n = n_samples(p["steady_min"], frame.step_s)
    chws = frame.get(lay.system, "chws_temp") if lay.system else None
    num = np.zeros(frame.n)
    den = np.zeros(frame.n)
    for c in lay.chillers:
        run = _running(frame, c) >= 0.5
        f, t = frame.get_or_nan(c, "chw_flow"), frame.get_or_nan(c, "chwst")
        ok = run & np.isfinite(f) & np.isfinite(t) & (f > 0)
        num += np.where(ok, f * t, 0.0)
        den += np.where(ok, f, 0.0)
    if chws is not None:
        with np.errstate(invalid="ignore", divide="ignore"):
            r_mix = np.where(den > 0, num / np.where(den > 0, den, 1) - (chws - 0.06), np.nan)
        e_mix = ewma_gated(r_mix, lam) - float(p.get("mix_offset_k", 0.0))
    else:
        r_mix = e_mix = frame.nan()
    out: dict[str, dict[str, np.ndarray]] = {}
    for c in lay.chillers:
        if not (frame.has(c, "chwst") and frame.has(c, "chwrt") and frame.has(c, "load_pct") and frame.has(c, "chw_flow")):
            continue
        chwst, chwrt = frame.get_or_nan(c, "chwst"), frame.get_or_nan(c, "chwrt")
        load, flow = frame.get_or_nan(c, "load_pct"), frame.get_or_nan(c, "chw_flow")
        with np.errstate(invalid="ignore", divide="ignore"):
            r_hb = (chwrt - chwst) - load / 100.0 * CHILLER_KW / (flow * WATER_KJ)
        gate = (
            run_for(_running(frame, c), run_n)
            & (np.nan_to_num(flow) > float(p["min_flow_m3h"]))
            & steady(load, st_n, float(p["load_steady_pct"]))
            & steady(chwst, st_n, float(p["chwst_steady_k"]))
            & np.isfinite(r_hb)
        )
        r = np.where(gate, r_hb, np.nan)
        out[c] = {"r": r, "ewma": ewma_gated(r, lam), "gate": gate, "mix": r_mix, "mix_ewma": e_mix}
    return out


def ct013_stats(frame: Frame, lay: Layout, p: dict[str, Any]) -> dict[str, dict[str, np.ndarray]]:
    """Per tower: gated header, peer and mixing residuals and their EWMAs."""
    lam = float(p["ewma_lambda"])
    run_n = n_samples(p["run_min"], frame.step_s)
    tsup = frame.get(lay.system, "tower_supply_temp") if lay.system else None
    temps = {t: frame.get_or_nan(t, "cwst") for t in lay.towers if frame.has(t, "cwst")}
    runs = {t: (_running(frame, t) >= 0.5) & np.isfinite(temps[t]) for t in temps}
    if not temps:
        return {}
    tmat = np.vstack([np.where(runs[t], temps[t], np.nan) for t in temps])
    nrun = np.sum(~np.isnan(tmat), axis=0)
    tsum = np.nansum(tmat, axis=0)
    with np.errstate(invalid="ignore", divide="ignore"):
        mean_all = np.where(nrun > 0, tsum / np.maximum(nrun, 1), np.nan)
    if tsup is not None:
        r_mix = mean_all - tsup
        e_mix = ewma_gated(r_mix, lam) - float(p.get("mix_offset_k", 0.0))
    else:
        r_mix = e_mix = frame.nan()
    out: dict[str, dict[str, np.ndarray]] = {}
    for t, cw in temps.items():
        gate = run_for(_running(frame, t), run_n) & np.isfinite(cw)
        others_n = nrun - runs[t].astype(int)
        with np.errstate(invalid="ignore", divide="ignore"):
            others_mean = np.where(others_n > 0, (tsum - np.where(runs[t], cw, 0.0)) / np.maximum(others_n, 1), np.nan)
        r_peer = np.where(gate & (others_n >= 1), cw - others_mean, np.nan)
        r_hdr = np.where(gate, cw - tsup, np.nan) if tsup is not None else frame.nan()
        out[t] = {
            "gate": gate,
            "hdr": r_hdr,
            "hdr_ewma": ewma_gated(r_hdr, lam),
            "peer": r_peer,
            "peer_ewma": ewma_gated(r_peer, lam),
            "mix": np.where(gate, r_mix, np.nan),
            "mix_ewma": np.where(gate, e_mix, np.nan),
        }
    return out


def ct014_features(frame: Frame, lay: Layout, p: dict[str, Any]) -> dict[str, dict[str, np.ndarray]]:
    """Per tower: gated approach, fan speed and heat-rejection fraction (for the fit and the rule)."""
    run_n = n_samples(p["run_min"], frame.step_s)
    st_n = n_samples(p["steady_min"], frame.step_s)
    wb_sys = frame.get(lay.system, "wet_bulb") if lay.system else None
    load = frame.get(lay.system, "cooling_load") if lay.system else None
    if load is None:
        return {}
    chiller_kw = np.zeros(frame.n)
    have_kw = False
    for c in lay.chillers:
        kw = frame.get(c, "power_kw")
        if kw is not None:
            have_kw = True
            chiller_kw += np.where(_running(frame, c) >= 0.5, np.nan_to_num(kw), 0.0)
    q_rej = load + chiller_kw if have_kw else load * 1.17
    nrun = np.zeros(frame.n)
    for t in lay.towers:
        nrun += (_running(frame, t) >= 0.5).astype(float)
    out: dict[str, dict[str, np.ndarray]] = {}
    for t in lay.towers:
        cw, fan = frame.get(t, "cwst"), frame.get(t, "fan_speed")
        if cw is None or fan is None:
            continue
        wb = wb_sys if wb_sys is not None else frame.get(t, "wet_bulb")
        if wb is None:
            continue
        with np.errstate(invalid="ignore", divide="ignore"):
            qf = q_rej / np.maximum(nrun, 1) / float(p["q_design_kw"])
        gate = (
            run_for(_running(frame, t), run_n)
            & steady(fan, st_n, float(p["fan_steady_pct"]))
            & (np.nan_to_num(fan) >= 10)
            & (np.nan_to_num(qf) > 0.05)
            & np.isfinite(cw)
            & np.isfinite(wb)
        )
        approach = cw - wb
        gate &= np.nan_to_num(approach, nan=-1) > 0.05
        out[t] = {"gate": gate, "approach": np.where(gate, approach, np.nan), "fan": fan, "qf": qf, "cwst": cw}
    return out


def approach_expected(model: dict[str, float], qf: np.ndarray, fan: np.ndarray) -> np.ndarray:
    with np.errstate(invalid="ignore", divide="ignore"):
        return np.exp(model["b0"] + model["b1"] * np.log(np.maximum(qf, 1e-3)) + model["b2"] * np.log(np.maximum(fan, 1.0) / 100.0))


def chs005_features(frame: Frame, lay: Layout, p: dict[str, Any]) -> dict[str, np.ndarray] | None:
    """Plant: mean running CHW pump speed N, flow per pump q, measured DP,
    mean running pump head (discharge - suction, in the pumps' own unit; NaN
    without pump pressures) and the gate."""
    if not lay.system:
        return None
    dp, flow = frame.get(lay.system, "riser_dp"), frame.get(lay.system, "plant_flow")
    if dp is None or flow is None:
        return None
    run_n = n_samples(p["run_min"], frame.step_s)
    st_n = n_samples(p["steady_min"], frame.step_s)
    spd_sum = np.zeros(frame.n)
    nrun = np.zeros(frame.n)
    head_sum = np.zeros(frame.n)
    head_ok = np.ones(frame.n, dtype=bool)
    for pm in lay.chw_pumps:
        r = (_running(frame, pm) >= 0.5) & np.isfinite(frame.get_or_nan(pm, "speed"))
        spd_sum += np.where(r, frame.get_or_nan(pm, "speed"), 0.0)
        nrun += r
        h = frame.get_or_nan(pm, "discharge_press") - frame.get_or_nan(pm, "suction_press")
        head_sum += np.where(r, np.nan_to_num(h), 0.0)
        head_ok &= ~r | np.isfinite(h)
    with np.errstate(invalid="ignore", divide="ignore"):
        n_hz = np.where(nrun > 0, spd_sum / np.maximum(nrun, 1), np.nan)
        q = np.where(nrun > 0, flow / np.maximum(nrun, 1), np.nan)
        head = np.where((nrun > 0) & head_ok, head_sum / np.maximum(nrun, 1), np.nan)
    if not lay.chw_pumps:  # fall back to the common command
        n_hz = frame.get_or_nan(lay.system, "chwp_speed_cmd")
        q = flow
        head = frame.nan()
    sys_run = _running(frame, lay.system)
    if np.all(np.isnan(sys_run)):
        sys_run = (np.nan_to_num(frame.get_or_nan(lay.system, "chillers_running")) > 0).astype(float)
    gate = run_for(sys_run, run_n) & steady(n_hz, st_n, float(p["speed_steady_hz"])) & (np.nan_to_num(q) > 0) & np.isfinite(dp)
    sp = frame.get(lay.system, "riser_dp_sp")
    if sp is not None:
        gate &= steady(sp, st_n, 0.5)
    # pumps starting / stopping change the per-pump flow abruptly
    gate &= steady(nrun.astype(float), st_n, 0.0)
    return {"n": n_hz, "q": q, "dp": dp, "head": head, "gate": gate}


def pump_head_deviation(f: dict[str, np.ndarray], p: dict[str, Any]) -> np.ndarray | None:
    """EWMA of the running pumps' measured head against their fault-free curve
    (fraction, offset removed), or None without a head model / pump pressures."""
    model = p.get("head_model")
    if not model or not np.any(np.isfinite(f["head"])):
        return None
    exp = dp_expected(model, f["n"], f["q"])
    ok = f["gate"] & np.isfinite(f["head"]) & (np.nan_to_num(exp) > 0)
    with np.errstate(invalid="ignore", divide="ignore"):
        ratio = np.where(ok, f["head"] / exp - 1.0, np.nan)
    return ewma_gated(ratio, float(p["ewma_lambda"])) - float(p.get("head_offset") or 0.0)


def dp_expected(model: dict[str, float], n_hz: np.ndarray, q: np.ndarray) -> np.ndarray:
    return model["a"] * n_hz**2 + model["b"] * n_hz * q + model["c"] * q**2


def chs006_stats(frame: Frame, lay: Layout, p: dict[str, Any]) -> dict[str, np.ndarray] | None:
    """Plant: mixing fraction f where valid, the part of it the valve position
    explains (an open valve bypasses twv_pos / 100 of the flow by design, also
    at a minimum position above 0), the excess over it, and whether the valve
    sits at its minimum."""
    if not lay.system:
        return None
    s = lay.system
    pos, cws, cwr, tsup = (frame.get(s, k) for k in ("twv_pos", "cw_supply_temp", "cw_return_temp", "tower_supply_temp"))
    if pos is None or cws is None or cwr is None or tsup is None:
        return None
    minp = frame.get(s, "twv_min_pos")
    minp = np.nan_to_num(minp, nan=0.0) if minp is not None else np.zeros(frame.n)
    flow = frame.get(s, "cw_flow")
    rng = cwr - tsup
    st_n = n_samples(p["steady_min"], frame.step_s)
    gate = (np.nan_to_num(rng) > float(p["min_range_k"])) & steady(pos, st_n, float(p["pos_steady_pct"])) & np.isfinite(cws)
    if flow is not None:
        gate &= np.nan_to_num(flow) > 0
    with np.errstate(invalid="ignore", divide="ignore"):
        f = np.where(gate, (cws - tsup) / np.where(rng != 0, rng, np.nan), np.nan)
    expected = np.where(gate, np.clip(pos / 100.0, 0.0, 1.0), np.nan)
    at_min = np.nan_to_num(pos, nan=np.inf) <= minp + float(p["min_pos_margin_pct"])
    return {"f": f, "expected": expected, "excess": f - expected, "gate": gate, "at_min": at_min, "pos": pos}


def chs008_stats(frame: Frame, lay: Layout, p: dict[str, Any], idx: Sequence[int] | None = None) -> dict[str, np.ndarray] | None:
    """Plant: per-sample trailing-window reversal rate and peak-to-peak amplitudes."""
    if not lay.system:
        return None
    cmd, tsup = frame.get(lay.system, "fan_speed_cmd"), frame.get(lay.system, "tower_supply_temp")
    if cmd is None or tsup is None:
        return None
    win = n_samples(p["window_min"], frame.step_s)
    any_tower = np.zeros(frame.n, dtype=bool)
    for t in lay.towers:
        any_tower |= _running(frame, t) >= 0.5
    if not lay.towers:
        any_tower[:] = True
    min_cnt = max(2, int(win * 0.8))
    cmd_pp = rolling_range(cmd, win, min_cnt)
    t_pp = rolling_range(tsup, win, min_cnt)
    gate = any_tower & np.isfinite(cmd_pp) & np.isfinite(t_pp)
    eval_idx = np.flatnonzero(gate) if idx is None else np.array([i for i in idx if gate[i]], dtype=int)
    rev = rolling_reversals(cmd, win, float(p["deadband_pct"]), eval_idx)
    rev_h = rev * 60.0 / float(p["window_min"])
    return {"rev_h": rev_h, "cmd_pp": cmd_pp, "t_pp": t_pp, "gate": gate}


# ---------------------------------------------------------------------------
# Rules


def eval_chl011(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    stats = chl011_stats(frame, lay, p)
    thr, mthr = float(p["threshold_k"]), float(p["mix_threshold_k"])
    clear_min = float(p["clear_min"])
    out = []
    for c, s in stats.items():
        e = s["ewma"] - float(p["offset_k"])
        m = s["mix_ewma"]
        no_mix = np.isnan(m)
        # bias v ~ -e moves the header mixing residual by +f_n v / sum(f): opposite sign to e
        eps, cpos, cneg = _signed_episodes(e, s["gate"], thr, no_mix | (m < -mthr), no_mix | (m > mthr), frame, p)
        dets = []
        for ep in eps:
            med = _median(e, ep)
            if med is None:
                continue
            bias = -med
            mix = _median(m, ep)
            dets.append(
                Detection(
                    "CHL-011", c, ep.start, ep.end, ep.ongoing, _r(bias, 2),
                    {
                        "residual_ewma_k": _r(med), "mixing_residual_k": _r(mix), "threshold_k": thr,
                        "chwst_c": _mean_over(frame, c, "chwst", ep), "chwrt_c": _mean_over(frame, c, "chwrt", ep),
                        "load_pct": _mean_over(frame, c, "load_pct", ep), "chw_flow_m3h": _mean_over(frame, c, "chw_flow", ep),
                        "chws_temp_c": _mean_over(frame, lay.system, "chws_temp", ep),
                    },
                    f"{c}: CHW leaving temperature sensor reads about {bias:+.1f} K off "
                    f"(heat-balance residual {med:+.2f} K, header mixing residual {(mix or 0):+.2f} K)",
                    confirmed=ep.confirmed,
                )  # fmt: skip
            )
        out.append(RuleEval("CHL-011", c, e, _combine(cpos, cneg), dets, frame.step_s, clear_min))
    return out


def eval_ct013(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    stats = ct013_stats(frame, lay, p)
    thr, mthr = float(p["threshold_k"]), float(p["mix_threshold_k"])
    clear_min = float(p["clear_min"])
    out = []
    for t, s in stats.items():
        hdr = s["hdr_ewma"] - float(p["offset_k"])
        peer = s["peer_ewma"] - float(p["peer_offset_k"])
        mix = s["mix_ewma"]
        have_hdr = not np.all(np.isnan(hdr))
        e = hdr if have_hdr else peer
        no_peer, no_mix = np.isnan(peer), np.isnan(mix) | (not have_hdr)
        # a bias moves the tower sensor, its peer residual and the mixing residual the same way;
        # a real outlet difference (fouling) leaves the mixing residual at zero
        pos_ok = (no_peer | (peer > thr)) & (no_mix | (mix > mthr))
        neg_ok = (no_peer | (peer < -thr)) & (no_mix | (mix < -mthr))
        eps, cpos, cneg = _signed_episodes(e, s["gate"], thr, pos_ok, neg_ok, frame, p)
        dets = []
        for ep in eps:
            med = _median(e, ep)
            if med is None:
                continue
            dets.append(
                Detection(
                    "CT-013", t, ep.start, ep.end, ep.ongoing, _r(med, 2),
                    {
                        "header_residual_k": _r(_median(hdr, ep)), "peer_residual_k": _r(_median(peer, ep)),
                        "mixing_residual_k": _r(_median(mix, ep)), "threshold_k": thr,
                        "cwst_c": _mean_over(frame, t, "cwst", ep),
                        "tower_supply_temp_c": _mean_over(frame, lay.system, "tower_supply_temp", ep),
                    },
                    f"{t}: leaving water temperature sensor reads about {med:+.1f} K off "
                    f"(vs header {(_median(hdr, ep) or 0):+.2f} K, vs peers {(_median(peer, ep) or 0):+.2f} K)",
                    confirmed=ep.confirmed,
                )  # fmt: skip
            )
        out.append(RuleEval("CT-013", t, e, _combine(cpos, cneg), dets, frame.step_s, clear_min))
    return out


def eval_ct014(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    feats = ct014_features(frame, lay, p)
    model = p.get("approach_model") or rules.RULES_BY_CODE["CT-014"].defaults["approach_model"]
    thr = float(p["threshold_k"])
    sp = frame.get(lay.system, "cwst_sp") if lay.system else None
    clear_min = float(p["clear_min"])
    out = []
    sm_n = n_samples(p.get("smooth_min", 15), frame.step_s)
    sm_min = max(1, sm_n // 2)
    for t, f in feats.items():
        exp_app = approach_expected(model, f["qf"], f["fan"])
        resid = f["approach"] - exp_app
        # a 2-hour trend rule: compare 15-minute means, not single noisy samples
        res_s = rolling_mean(resid, sm_n, sm_min)
        fan_s = rolling_mean(np.where(f["gate"], f["fan"], np.nan), sm_n, sm_min)
        # LBNL: fouling shows when the fan runs near its limit; below that the PI loop hides it, so those
        # samples are no evidence either way (unknown), and the fault holds through the night (stale_min)
        applicable = f["gate"] & np.isfinite(resid) & np.isfinite(res_s) & (np.nan_to_num(fan_s) >= float(p["fan_min_pct"]))
        cond = np.nan_to_num(res_s, nan=-np.inf) > thr
        if sp is not None:
            dev_s = rolling_mean(np.where(f["gate"], f["cwst"] - sp, np.nan), sm_n, sm_min)
            cond &= np.nan_to_num(dev_s, nan=-np.inf) > float(p["sp_dev_k"])
        c = _gate(cond, applicable)
        eps = persistent_episodes(c, frame.step_s, p["persist_min"], p["clear_min"], stale_min=stale_of(p))
        dets = []
        for ep in eps:
            med = _median(resid, ep, applicable)
            app = _median(f["approach"], ep, applicable)
            ex = _median(exp_app, ep, applicable)
            fan_at = _median(f["fan"], ep, applicable)
            if med is None or app is None or ex is None or app <= 0:
                continue
            capacity = (ex / app) ** (1 / 0.7)
            dets.append(
                Detection(
                    "CT-014", t, ep.start, ep.end, ep.ongoing, _r(capacity, 2),
                    {
                        "approach_k": _r(app), "expected_approach_k": _r(ex), "approach_excess_k": _r(med), "threshold_k": thr,
                        "fan_speed_pct": _r(fan_at, 1), "cwst_c": _mean_over(frame, t, "cwst", ep),
                        "cwst_sp_c": _mean_over(frame, lay.system, "cwst_sp", ep), "wet_bulb_c": _mean_over(frame, lay.system, "wet_bulb", ep),
                        "capacity_estimate": _r(capacity),
                    },
                    f"{t}: approach {app:.1f} K is {med:.1f} K above the clean-tower expectation at "
                    f"{(fan_at or 0):.0f} % fan (estimated capacity {capacity:.2f})",
                    confirmed=ep.confirmed,
                )  # fmt: skip
            )
        out.append(RuleEval("CT-014", t, np.where(applicable, resid, np.nan), c, dets, frame.step_s, clear_min, stale_min=stale_of(p)))
    return out


def eval_chs005(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    if not lay.system:
        return []
    clear_min = float(p["clear_min"])
    model = p.get("dp_model")
    if not model:
        return [RuleEval("CHS-005", lay.system, frame.nan(), frame.nan(), [], frame.step_s, clear_min, skipped="riser DP model not calibrated yet")]
    f = chs005_features(frame, lay, p)
    if f is None:
        return [RuleEval("CHS-005", lay.system, frame.nan(), frame.nan(), [], frame.step_s, clear_min, skipped="riser DP signals missing")]
    exp_dp = dp_expected(model, f["n"], f["q"])
    gate = f["gate"] & (np.nan_to_num(exp_dp) > 10)
    with np.errstate(invalid="ignore", divide="ignore"):
        ratio = np.where(gate, f["dp"] / exp_dp - 1.0, np.nan)
    e = ewma_gated(ratio, float(p["ewma_lambda"])) - float(p["offset"])
    thr = float(p["threshold"])
    # Isolation: a riser DP below (or above) what the pumps should deliver is
    # only the transmitter's fault when the pumps' own head (their discharge -
    # suction transmitters) is on its fault-free curve. A degraded pump
    # (impeller wear, cavitation) lowers both and is left to the pump's alarms.
    head_dev = pump_head_deviation(f, p)
    pump_ok = np.ones(frame.n, dtype=bool)
    if head_dev is not None:
        pump_ok = ~(np.abs(np.nan_to_num(head_dev, nan=0.0)) > float(p.get("head_tolerance") or 0.035))
    ones = np.ones(frame.n, dtype=bool)
    eps, cpos, cneg = _signed_episodes(e, gate & pump_ok, thr, ones, ones, frame, p)
    dets = []
    for ep in eps:
        med = _median(e, ep)
        if med is None:
            continue
        dets.append(
            Detection(
                "CHS-005", lay.system, ep.start, ep.end, ep.ongoing, _r(med, 3),
                {
                    "ratio_minus_1": _r(med, 4), "threshold": thr, "riser_dp_kpa": _mean_over(frame, lay.system, "riser_dp", ep),
                    "expected_dp_kpa": _r(_median(exp_dp, ep), 1), "pump_speed_hz": _r(_median(f["n"], ep), 2),
                    "flow_per_pump_m3h": _r(_median(f["q"], ep), 1), "riser_dp_sp_kpa": _mean_over(frame, lay.system, "riser_dp_sp", ep),
                    "pump_head_deviation": _r(_median(head_dev, ep), 4) if head_dev is not None else None,
                },
                f"{lay.system}: riser DP reads {med * 100:+.0f} % against what the CHW pumps deliver "
                f"({(_mean_over(frame, lay.system, 'riser_dp', ep) or 0):.0f} kPa measured vs {(_median(exp_dp, ep) or 0):.0f} kPa expected)",
                confirmed=ep.confirmed,
            )  # fmt: skip
        )
    return [RuleEval("CHS-005", lay.system, e, _combine(cpos, cneg), dets, frame.step_s, clear_min)]


def eval_chs006(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    if not lay.system:
        return []
    clear_min = float(p["clear_min"])
    s = chs006_stats(frame, lay, p)
    if s is None:
        return [RuleEval("CHS-006", lay.system, frame.nan(), frame.nan(), [], frame.step_s, clear_min, skipped="3-way valve signals missing")]
    off = float(p["offset"])
    # detected on the bypass the valve position does not explain (a minimum position of 20 % mixes 0.2 by design)
    x = s["excess"] - off
    f = s["f"] - off
    thr = float(p["threshold"])
    # not applicable (NaN) while the valve is legitimately open above its minimum
    c = _gate(x > thr, s["gate"] & s["at_min"] & np.isfinite(x))
    eps = persistent_episodes(c, frame.step_s, p["persist_min"], p["clear_min"], stale_min=stale_of(p))
    dets = []
    for ep in eps:
        med, med_x, med_e = _median(f, ep), _median(x, ep), _median(s["expected"], ep)
        if med is None or med_x is None:
            continue
        dets.append(
            Detection(
                "CHS-006", lay.system, ep.start, ep.end, ep.ongoing, _r(med, 2),
                {
                    "mixing_fraction": _r(med), "expected_fraction": _r(med_e), "excess_fraction": _r(med_x), "threshold": thr,
                    "twv_pos_pct": _mean_over(frame, lay.system, "twv_pos", ep),
                    "twv_min_pos_pct": _mean_over(frame, lay.system, "twv_min_pos", ep),
                    "cw_supply_temp_c": _mean_over(frame, lay.system, "cw_supply_temp", ep),
                    "cw_return_temp_c": _mean_over(frame, lay.system, "cw_return_temp", ep),
                    "tower_supply_temp_c": _mean_over(frame, lay.system, "tower_supply_temp", ep),
                },
                f"{lay.system}: about {med * 100:.0f} % of the condenser return bypasses the towers while the 3-way "
                f"valve is at its minimum position ({(med_e or 0) * 100:.0f} % expected from its position)",
                confirmed=ep.confirmed,
            )  # fmt: skip
        )
    return [RuleEval("CHS-006", lay.system, np.where(s["gate"] & s["at_min"], x, np.nan), c, dets, frame.step_s, clear_min)]


def eval_chs007(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    if not lay.system:
        return []
    clear_min = float(p["clear_min"])
    pos, cmd = frame.get(lay.system, "twv_pos"), frame.get(lay.system, "twv_cmd")
    if pos is None or cmd is None:
        return [RuleEval("CHS-007", lay.system, frame.nan(), frame.nan(), [], frame.step_s, clear_min, skipped="3-way valve signals missing")]
    flow = frame.get(lay.system, "cw_flow")
    valid = np.isfinite(pos) & np.isfinite(cmd)
    if flow is not None:
        valid &= np.nan_to_num(flow) > 0
    dev = np.abs(pos - cmd)
    ca = _gate(dev > float(p["deviation_pct"]), valid)
    win = n_samples(p["window_min"], frame.step_s)
    pr = rolling_range(np.where(valid, pos, np.nan), win, max(2, int(win * 0.8)))
    cr = rolling_range(np.where(valid, cmd, np.nan), win, max(2, int(win * 0.8)))
    cb = _gate((pr < float(p["pos_range_pct"])) & (cr > float(p["cmd_range_pct"])), np.isfinite(pr) & np.isfinite(cr))
    eps = persistent_episodes(ca, frame.step_s, p["persist_min"], p["clear_min"], stale_min=stale_of(p))
    eps += persistent_episodes(cb, frame.step_s, frame.step_s / 60.0, p["clear_min"], stale_min=stale_of(p))
    eps = _merge_episodes(eps, n_samples(clear_min, frame.step_s))
    dets = []
    for ep in eps:
        stuck = _median(pos, ep)
        if stuck is None:
            continue
        d = _median(dev, ep)
        dets.append(
            Detection(
                "CHS-007", lay.system, ep.start, ep.end, ep.ongoing, _r(stuck / 100.0, 2),
                {
                    "twv_pos_pct": _r(stuck, 1), "twv_cmd_pct": _mean_over(frame, lay.system, "twv_cmd", ep),
                    "deviation_pct": _r(d, 1), "threshold_pct": float(p["deviation_pct"]),
                },
                f"{lay.system}: condenser 3-way valve stays at {stuck:.0f} % while commanded "
                f"{(_mean_over(frame, lay.system, 'twv_cmd', ep) or 0):.0f} %",
                confirmed=ep.confirmed,
            )  # fmt: skip
        )
    return [RuleEval("CHS-007", lay.system, dev, _combine(ca, cb), dets, frame.step_s, clear_min)]


def eval_chs008(frame: Frame, lay: Layout, p: dict[str, Any]) -> list[RuleEval]:
    if not lay.system:
        return []
    clear_min = float(p["clear_min"])
    s = chs008_stats(frame, lay, p)
    if s is None:
        return [RuleEval("CHS-008", lay.system, frame.nan(), frame.nan(), [], frame.step_s, clear_min, skipped="tower loop signals missing")]
    cond = (np.nan_to_num(s["rev_h"]) > float(p["reversals_per_h"])) & (s["cmd_pp"] > float(p["cmd_pp_pct"])) & (s["t_pp"] > float(p["temp_pp_k"]))
    c = _gate(cond, s["gate"] & np.isfinite(s["rev_h"]))
    eps = persistent_episodes(c, frame.step_s, p["persist_min"], p["clear_min"], stale_min=stale_of(p))
    dets = []
    for ep in eps:
        rev = _median(s["rev_h"], ep)
        cpp = _median(s["cmd_pp"], ep)
        tpp = _median(s["t_pp"], ep)
        dets.append(
            Detection(
                "CHS-008", lay.system, ep.start, ep.end, ep.ongoing, 1.0,
                {"reversals_per_h": _r(rev, 1), "fan_cmd_pp_pct": _r(cpp, 1), "tower_supply_pp_k": _r(tpp, 2),
                 "thresholds": {"reversals_per_h": p["reversals_per_h"], "cmd_pp_pct": p["cmd_pp_pct"], "temp_pp_k": p["temp_pp_k"]}},
                f"{lay.system}: tower fan PI loop is hunting ({(rev or 0):.0f} reversals per hour, fan command "
                f"{(cpp or 0):.0f} % peak-to-peak, leaving water {(tpp or 0):.2f} K peak-to-peak)",
                confirmed=ep.confirmed,
            )  # fmt: skip
        )
    return [RuleEval("CHS-008", lay.system, s["rev_h"], c, dets, frame.step_s, clear_min)]


EVALUATORS: dict[str, Callable[[Frame, Layout, dict[str, Any]], list[RuleEval]]] = {
    "CHL-011": eval_chl011,
    "CT-013": eval_ct013,
    "CT-014": eval_ct014,
    "CHS-005": eval_chs005,
    "CHS-006": eval_chs006,
    "CHS-007": eval_chs007,
    "CHS-008": eval_chs008,
}


def evaluate(frame: Frame, lay: Layout, rule_cfg: dict[str, dict[str, Any]]) -> list[RuleEval]:
    """Evaluate every configured rule; a failing rule is logged and skipped."""
    out: list[RuleEval] = []
    for code, cfg in rule_cfg.items():
        fn = EVALUATORS.get(code)
        if fn is None or frame.n == 0:
            continue
        try:
            out.extend(fn(frame, lay, cfg["params"]))
        except Exception:  # noqa: BLE001 - one rule must not stop the others
            log.exception("rule evaluation failed code=%s", code)
    return out


def detect_batch(frame: Frame, rule_cfg: dict[str, dict[str, Any]] | None = None, lay: Layout | None = None) -> list[Detection]:
    """All fault episodes in a frame (simulation post-analysis)."""
    lay = lay or Layout.from_aliases(frame.aliases())
    cfg = rule_cfg if rule_cfg is not None else rules.default_rules()
    return [d for ev in evaluate(frame, lay, cfg) for d in ev.detections]


# ---------------------------------------------------------------------------
# Production runner


@dataclass
class LiveResult:
    evaluated: int = 0
    opened: list[int] = field(default_factory=list)
    updated: int = 0
    cleared: int = 0
    skipped: dict[str, str] = field(default_factory=dict)
    planned: list[dict[str, Any]] = field(default_factory=list)  # dry run


async def run_live(*, site_code: str | None = None, now: datetime | None = None, dry_run: bool = False) -> LiveResult:
    """One FDD pass over the last LIVE_WINDOW_MIN of production data."""
    site = site_code or data.settings_site()
    res = LiveResult()
    if not dry_run:
        await rules.seed_rules()
    cfg = await rules.load_rules() if not dry_run else (await rules.load_rules() or rules.default_rules())
    plant = await data.load_plant(site)
    lay = Layout.from_plant(plant)
    end = (now or data.utcnow()).replace(second=0, microsecond=0)
    start = end - timedelta(minutes=LIVE_WINDOW_MIN)
    evals: list[RuleEval] = []
    if cfg and (lay.system or lay.chillers or lay.towers):
        frame = await data.live_frame(plant, lay.required_params(), start, end)
        evals = evaluate(frame, lay, cfg)
    else:
        frame = None
    res.evaluated = len(evals)
    active = {
        (r["rule_code"], r["asset_id"]): r
        for r in await db.fetch_all(
            "SELECT id, rule_code, asset_id, started_at, last_seen_at FROM analytics.faults WHERE status = 'ACTIVE' AND site_code = %s",
            (site,),
        )
    }
    seen: set[tuple[str, int]] = set()
    for ev in evals:
        asset = plant.get(ev.asset)
        if asset is None:
            continue
        key = (ev.rule, asset.id)
        seen.add(key)
        if ev.skipped:
            res.skipped[f"{ev.rule}:{ev.asset}"] = ev.skipped
        ongoing = next((d for d in reversed(ev.detections) if d.ongoing), None)
        current = active.get(key)
        assert frame is not None
        if ongoing is not None:
            started = frame.time(ongoing.start)
            last = frame.time(ongoing.end)
            sev = cfg[ev.rule]["severity"]
            evidence = {
                **ongoing.evidence,
                "window": {"start": frame.time(ongoing.start), "end": last},
                "confirmed_at": frame.time(ongoing.confirmed) if ongoing.confirmed >= 0 else None,
                "rule_version": cfg[ev.rule]["version"],
            }
            if dry_run:
                res.planned.append({"action": "update" if current else "open", "rule": ev.rule, "asset": ev.asset, "message": ongoing.message, "intensity": ongoing.intensity})
                continue
            row = await db.fetch_one(
                """INSERT INTO analytics.faults (site_code, rule_code, asset_id, severity, message, intensity_estimate, evidence, started_at, last_seen_at)
                   VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
                   ON CONFLICT (rule_code, asset_id) WHERE status = 'ACTIVE' DO UPDATE SET
                     last_seen_at = GREATEST(analytics.faults.last_seen_at, EXCLUDED.last_seen_at),
                     message = EXCLUDED.message, intensity_estimate = EXCLUDED.intensity_estimate,
                     evidence = EXCLUDED.evidence, severity = EXCLUDED.severity
                   RETURNING id, (xmax = 0) AS inserted""",
                (site, ev.rule, asset.id, sev, ongoing.message, ongoing.intensity, data.jb(evidence), current["started_at"] if current else started, last),
            )
            if row and row["inserted"]:
                res.opened.append(int(row["id"]))
                log.info("fault opened id=%s rule=%s asset=%s %s", row["id"], ev.rule, ev.asset, ongoing.message)
            else:
                res.updated += 1
            continue
        if current is None:
            continue
        reason = None
        stale = timedelta(minutes=stale_of(cfg[ev.rule]["params"]))
        if not ev.skipped and ev.tail_false():
            reason = "condition cleared"
        elif current["last_seen_at"] < end - stale:
            reason = "no valid evidence (unit not running, rule not applicable or data missing)"
        if reason:
            await _clear(current["id"], reason, dry_run, res, ev.rule, ev.asset)
    # ACTIVE faults whose rule is disabled or whose asset vanished from the evaluation
    for key, row in active.items():
        if key in seen:
            continue
        code = key[0]
        if code not in cfg:
            await _clear(row["id"], "rule disabled", dry_run, res, code, str(key[1]))
        elif row["last_seen_at"] < end - timedelta(minutes=stale_of(cfg[code]["params"])):
            await _clear(row["id"], "no valid evidence (unit not running, rule not applicable or data missing)", dry_run, res, code, str(key[1]))
    return res


async def _clear(fault_id: int, reason: str, dry_run: bool, res: LiveResult, rule: str, asset: str) -> None:
    if dry_run:
        res.planned.append({"action": "clear", "rule": rule, "asset": asset, "reason": reason})
        return
    n = await db.execute(
        "UPDATE analytics.faults SET status = 'CLEARED', ended_at = last_seen_at,"
        " evidence = evidence || jsonb_build_object('clear_reason', %s::text) WHERE id = %s AND status = 'ACTIVE'",
        (reason, fault_id),
    )
    if n:
        res.cleared += 1
        log.info("fault cleared id=%s rule=%s asset=%s reason=%s", fault_id, rule, asset, reason)
