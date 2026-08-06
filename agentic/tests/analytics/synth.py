"""Synthetic chiller-plant series that mimic the digital twin's signatures.

A small, deterministic re-implementation of the twin's plant points (DESIGN 2.1-2.4)
on a 1-minute grid: 2 duty chillers + 1 standby, 2 duty towers + 1 standby,
variable-primary CHW pumps holding the riser DP, the condenser 3-way valve and
the tower fan PI. LBNL faults can be injected with their documented intensities
from a start minute. Values are rounded to the twin's display scales.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone

import numpy as np

from afdd_ai.analytics.series import Frame

T0 = datetime(2026, 10, 1, 0, 0, tzinfo=timezone.utc)

# riser DP hydraulics: dp = A N^2 + B N q + C q^2 (N Hz, q m3/h per pump)
DP_A, DP_B, DP_C = 0.0835, 0.0005, -0.0002
# pump head (bar, discharge - suction) = riser DP + evaporator / branch loss L q^2, in kPa / 100
HEAD_LOSS_C = 0.00012
SUCTION_BAR = 2.0
APP0 = 2.48  # tower approach at 100 % fan and design heat rejection
Q_DESIGN = 3036.0


@dataclass
class Faults:
    chwst_bias: dict[str, float] = field(default_factory=dict)  # CH_0n -> K
    cwst_bias: dict[str, float] = field(default_factory=dict)  # CT_0n -> K
    ct_fouling: dict[str, float] = field(default_factory=dict)  # CT_0n -> capacity
    riser_dp_bias: float = 0.0
    pump_head_loss: float = 0.0  # fraction of head the running CHW pumps lose (impeller wear, cavitation)
    twv_leak: float = 0.0
    twv_stuck: float | None = None
    cw_pi_tuning: bool = False
    start_min: int = 0


def ar_noise(rng: np.random.Generator, n: int, sigma: float, tau_min: float) -> np.ndarray:
    """AR(1) noise with stationary sigma and time constant tau (minutes)."""
    phi = np.exp(-1.0 / max(tau_min, 1e-6))
    e = rng.normal(0, sigma * np.sqrt(1 - phi**2), n)
    out = np.empty(n)
    acc = rng.normal(0, sigma)
    for i in range(n):
        acc = phi * acc + e[i]
        out[i] = acc
    return out


def plant_frame(
    hours: float = 8.0,
    *,
    seed: int = 0,
    faults: Faults | None = None,
    t0: datetime = T0,
    load_scale: float = 1.0,
    approach_sp: float = 3.5,
    twv_min_pos: float = 0.0,
) -> Frame:
    """twv_min_pos: minimum position of the condenser 3-way valve (%); as on the
    twin, the valve then sits there and bypasses that fraction of the flow."""
    f = faults or Faults()
    n = int(hours * 60)
    rng = np.random.default_rng(seed)
    t = np.arange(n, dtype=float)
    active = t >= f.start_min
    tod = (t0.hour * 60 + t0.minute + t) % 1440

    wb = 25.5 + 0.8 * np.sin(2 * np.pi * (tod - 540) / 1440) + ar_noise(rng, n, 0.08, 60)
    oa = wb + 3.5 + 1.5 * np.sin(2 * np.pi * (tod - 540) / 1440)
    load_frac = (0.74 + 0.08 * np.sin(2 * np.pi * (tod - 600) / 1440) + ar_noise(rng, n, 0.01, 120)) * load_scale
    cooling_load = load_frac * 2 * 3517

    fr = Frame(t0=t0, step_s=60, n=n)
    put = fr.data.__setitem__

    # chillers: CH_01 / CH_02 duty, CH_03 standby
    flows, trues = [], []
    for c in ("CH_01", "CH_02", "CH_03"):
        if c == "CH_03":
            put(f"{c}.running", np.zeros(n))
            put(f"{c}.chwst", np.round(11.5 + ar_noise(rng, n, 0.05, 60), 2))
            put(f"{c}.chwrt", np.round(fr.data[f"{c}.chwst"] + 0.1, 2))
            for k in ("load_pct", "chw_flow", "power_kw"):
                put(f"{c}.{k}", np.zeros(n))
            continue
        put(f"{c}.running", np.ones(n))
        flow = 550 + ar_noise(rng, n, 4, 10)
        load = np.clip(load_frac * 100 + ar_noise(rng, n, 1.2, 10), 20, 100)
        bias = np.where(active, f.chwst_bias.get(c, 0.0), 0.0)
        noise = ar_noise(rng, n, 0.06, 20)
        meas = 6.5 + noise
        true = meas - bias
        dtw = load / 100 * 3517 / (flow * 1.1628)
        put(f"{c}.chwst", np.round(meas, 2))
        put(f"{c}.chwrt", np.round(true + dtw, 2))
        put(f"{c}.load_pct", np.round(load, 1))
        put(f"{c}.chw_flow", np.round(flow, 0))
        put(f"{c}.power_kw", np.round(load / 100 * 3517 / 6.4, 1))
        flows.append(flow)
        trues.append(true)
    chws = sum(fl * tr for fl, tr in zip(flows, trues, strict=True)) / sum(flows) + 0.06 + ar_noise(rng, n, 0.03, 2)
    plant_flow = sum(flows)

    # towers: CT_01 / CT_02 duty with a common fan speed, CT_03 standby
    qf = cooling_load * 1.17 / 2 / Q_DESIGN
    if f.cw_pi_tuning:
        hunt = np.where(active, 12 * np.sin(2 * np.pi * t / 8.0), 0.0)
    else:
        hunt = np.zeros(n)
    caps = {ct: np.where(active, f.ct_fouling.get(ct, 1.0), 1.0) for ct in ("CT_01", "CT_02")}
    # steady state of the common fan PI: the average approach meets approach_sp
    avg_factor = (caps["CT_01"] ** -0.7 + caps["CT_02"] ** -0.7) / 2
    ratio = approach_sp / (APP0 * qf**0.6 * avg_factor)  # = (100 / speed)^0.8
    fan = np.clip(100 / np.maximum(ratio, 1e-3) ** (1 / 0.8) + ar_noise(rng, n, 0.8, 15) + hunt, 30, 100)
    tower_true = {}
    for ct in ("CT_01", "CT_02", "CT_03"):
        if ct == "CT_03":
            put(f"{ct}.running", np.zeros(n))
            put(f"{ct}.cwst", np.round(wb + 2.0 + ar_noise(rng, n, 0.05, 60), 2))
            put(f"{ct}.fan_speed", np.zeros(n))
            put(f"{ct}.wet_bulb", np.round(wb, 2))
            put(f"{ct}.fan_mode", np.zeros(n))
            continue
        app = APP0 * qf**0.6 * (100 / fan) ** 0.8 / caps[ct] ** 0.7 + ar_noise(rng, n, 0.04, 10)
        true = wb + app + (np.where(active, 0.25 * np.sin(2 * np.pi * t / 8.0 + 0.5), 0.0) if f.cw_pi_tuning else 0.0)
        tower_true[ct] = true
        bias = np.where(active, f.cwst_bias.get(ct, 0.0), 0.0)
        put(f"{ct}.running", np.ones(n))
        put(f"{ct}.cwst", np.round(true + bias + ar_noise(rng, n, 0.03, 2), 2))
        put(f"{ct}.fan_speed", np.round(fan, 1))
        put(f"{ct}.wet_bulb", np.round(wb, 2))
        put(f"{ct}.fan_mode", np.zeros(n))
    tower_supply = (tower_true["CT_01"] + tower_true["CT_02"]) / 2

    # condenser loop and 3-way valve
    cw_return = tower_supply + 5.0 * load_frac / 0.74
    twv_cmd = np.full(n, float(twv_min_pos))  # the PI output (0 here) raised to the minimum position
    twv_pos = twv_cmd.copy()
    if f.twv_stuck is not None:
        twv_pos = np.where(active, f.twv_stuck * 100, twv_pos)
    b = twv_pos / 100
    if f.twv_leak:
        b = np.where(active, np.maximum(b, f.twv_leak), b)
    cw_supply = (1 - b) * tower_supply + b * cw_return

    # variable-primary riser DP: the PI holds the measured DP, a bias moves the true one
    q = plant_flow / 2
    dp_sp = 120.0
    v = np.where(active, f.riser_dp_bias, 0.0)
    h = np.where(active, f.pump_head_loss, 0.0)
    dp_true = dp_sp / (1 + v)
    # solve (1 - h) (A N^2 + B q N) + C q^2 = dp_true for N (a worn pump delivers less of its curve)
    a_, b_ = DP_A * (1 - h), DP_B * (1 - h) * q
    disc = b_**2 - 4 * a_ * (DP_C * q**2 - dp_true)
    speed = (-b_ + np.sqrt(disc)) / (2 * a_) + ar_noise(rng, n, 0.1, 5)
    dp_now = (1 - h) * (DP_A * speed**2 + DP_B * speed * q) + DP_C * q**2
    dp_meas = dp_now * (1 + v) + ar_noise(rng, n, 1.2, 1)
    head_bar = (dp_now + HEAD_LOSS_C * q**2) / 100

    for p in ("CHWP_01", "CHWP_02"):
        put(f"{p}.running", np.ones(n))
        put(f"{p}.speed", np.round(speed, 1))
        suction = SUCTION_BAR + ar_noise(rng, n, 0.01, 3)
        put(f"{p}.suction_press", np.round(suction, 2))
        put(f"{p}.discharge_press", np.round(suction + head_bar + ar_noise(rng, n, 0.01, 1), 2))
    put("CHWP_03.running", np.zeros(n))
    put("CHWP_03.speed", np.zeros(n))

    s = "CHWS_01"
    put(f"{s}.running", np.ones(n))
    put(f"{s}.chillers_running", np.full(n, 2.0))
    put(f"{s}.chws_temp", np.round(chws, 2))
    put(f"{s}.cooling_load", np.round(cooling_load, 0))
    put(f"{s}.wet_bulb", np.round(wb, 2))
    put(f"{s}.oa_temp", np.round(oa, 1))
    put(f"{s}.cwst_sp", np.round(wb + approach_sp, 2))
    put(f"{s}.tower_supply_temp", np.round(tower_supply + ar_noise(rng, n, 0.02, 2), 2))
    put(f"{s}.cw_return_temp", np.round(cw_return + ar_noise(rng, n, 0.02, 2), 2))
    put(f"{s}.cw_supply_temp", np.round(cw_supply + ar_noise(rng, n, 0.02, 2), 2))
    put(f"{s}.cw_flow", np.full(n, 1300.0))
    put(f"{s}.twv_cmd", np.round(twv_cmd, 1))
    put(f"{s}.twv_pos", np.round(twv_pos, 1))
    put(f"{s}.twv_min_pos", np.full(n, float(twv_min_pos)))
    put(f"{s}.riser_dp", np.round(dp_meas, 1))
    put(f"{s}.riser_dp_sp", np.full(n, dp_sp))
    put(f"{s}.plant_flow", np.round(plant_flow, 0))
    put(f"{s}.chwp_speed_cmd", np.round(speed, 1))
    put(f"{s}.fan_speed_cmd", np.round(fan, 1))
    return fr


def dp_model() -> dict[str, float]:
    return {"a": DP_A, "b": DP_B, "c": DP_C}


def head_model() -> dict[str, float]:
    """Healthy pump head curve in bar (discharge - suction)."""
    return {"a": DP_A / 100, "b": DP_B / 100, "c": (DP_C + HEAD_LOSS_C) / 100}


def temperature_series(hours: float = 24.0, *, seed: int = 0, base: float = 11.0, amp: float = 0.8, noise: float = 0.05, scale: int = 2) -> np.ndarray:
    """A healthy diurnal process temperature with autocorrelated noise."""
    n = int(hours * 60)
    rng = np.random.default_rng(seed)
    t = np.arange(n)
    x = base + amp * np.sin(2 * np.pi * (t - 300) / 1440) + ar_noise(rng, n, noise, 15) + ar_noise(rng, n, noise * 0.5, 1)
    return np.round(x, scale)
