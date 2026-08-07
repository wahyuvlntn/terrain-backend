"""LBNL chiller-plant fault detection rules: metadata and engineering defaults.

The rule code is the alarm code the engine raises for an ACTIVE fault
(DESIGN 2.5 / 6.1). Thresholds and windows live in `analytics.fault_rules.params`
(seeded from `defaults` below, calibrated on fault-free twin data by
`calibration.py`, editable by an admin). The evaluation itself is in
`faults.py`; this module has no numeric dependencies so the backend-facing
metadata can be imported anywhere.

Signals are listed as (asset pattern, parameter); `{n}` is the unit index.
Their LBNL point names come from `analytics.point_map` at seed time
(`STATIC_LBNL` is the same mapping, used when the table is unreachable).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any

from psycopg.types.json import Jsonb

from ..common import db

log = logging.getLogger(__name__)

# Calibration: threshold = max(engineering floor, offset + K_SIGMA * robust sigma)
# of the rule statistic on fault-free twin data. Robust sigma = 1.4826 * MAD;
# 4 sigma keeps the per-sample false-alarm rate below 1e-4 for Gaussian noise,
# and the persistence windows cut it further for autocorrelated residuals.
K_SIGMA = 4.0


@dataclass(frozen=True)
class RuleDef:
    code: str
    name: str
    lbnl_fault: str
    description: str
    asset_scope: str  # chiller | tower | plant
    signals: tuple[tuple[str, str], ...]
    defaults: dict[str, Any] = field(default_factory=dict)
    severity: str = "WARNING"
    intensity: str = ""  # what intensity_estimate means
    lbnl_intensities: tuple[float, ...] = ()
    fault_kind: str = ""  # simulator fault kind (DESIGN 2.4)


RULES: tuple[RuleDef, ...] = (
    RuleDef(
        code="CHL-011",
        name="Chiller CHW Leaving Temperature Sensor Bias",
        lbnl_fault="Chiller CHW leaving temperature sensor bias",
        description=(
            "Per running chiller, analytical redundancy for the leaving-water sensor that the unit controls on: "
            "heat-balance residual r = (chwrt - chwst) - load_pct/100 * 3517 / (chw_flow * 1.1628) K, and the header "
            "mixing residual sum(f * chwst) / sum(f) - (chws_temp - 0.06) K. A fault is raised when |EWMA(r)| exceeds "
            "the threshold for 30 min and the mixing residual has the consistent (opposite) sign. "
            "Gated on RUNNING for 15 min, steady load and leaving temperature."
        ),
        asset_scope="chiller",
        signals=(
            ("CH_0{n}", "chwst"),
            ("CH_0{n}", "chwrt"),
            ("CH_0{n}", "load_pct"),
            ("CH_0{n}", "chw_flow"),
            ("CH_0{n}", "running"),
            ("CHWS_01", "chws_temp"),
        ),
        defaults={
            "threshold_k": 0.5,
            "offset_k": 0.0,
            "mix_threshold_k": 0.15,
            "mix_offset_k": 0.0,
            "ewma_lambda": 0.1,
            "persist_min": 30,
            "clear_min": 30,
            "stale_min": 240,
            "run_min": 15,
            "steady_min": 15,
            "load_steady_pct": 10.0,
            "chwst_steady_k": 1.0,
            "min_flow_m3h": 100.0,
        },
        intensity="Estimated sensor bias v = measured - true (K), approximately -r",
        lbnl_intensities=(-2.0, -1.0, 1.0, 2.0),
        fault_kind="chwst_bias",
    ),
    RuleDef(
        code="CT-013",
        name="Tower Leaving Water Temperature Sensor Bias",
        lbnl_fault="Cooling tower CW leaving temperature sensor bias",
        description=(
            "Per running tower: residual of its leaving-water sensor against the tower leaving header "
            "(cwst_n - tower_supply_temp) and against its running peers (cwst_n - mean(cwst_others), 2 or more running). "
            "The flow-weighted mean of the tower sensors minus the header (mixing residual) separates a sensor bias "
            "(non-zero) from a real temperature difference such as fouling (zero). Fault when |EWMA| > 0.7 K for 60 min "
            "with consistent signs."
        ),
        asset_scope="tower",
        signals=(
            ("CT_0{n}", "cwst"),
            ("CT_0{n}", "running"),
            ("CHWS_01", "tower_supply_temp"),
        ),
        defaults={
            "threshold_k": 0.7,
            "offset_k": 0.0,
            "peer_offset_k": 0.0,
            "mix_threshold_k": 0.2,
            "mix_offset_k": 0.0,
            "ewma_lambda": 0.1,
            "persist_min": 60,
            "clear_min": 30,
            "stale_min": 240,
            "run_min": 15,
        },
        intensity="Estimated sensor bias v = measured - true (K)",
        lbnl_intensities=(-2.0, -1.0, 1.0, 2.0),
        fault_kind="cwst_bias",
    ),
    RuleDef(
        code="CT-014",
        name="Cooling Tower Heat Exchanger Fouling",
        lbnl_fault="Cooling tower fouling (reduced heat transfer)",
        description=(
            "Per running tower: approach (cwst - wet_bulb) minus the expected approach at the current fan speed and "
            "heat rejection, from a fault-free fit ln(approach) = b0 + b1 ln(Q/Q_design) + b2 ln(fan/100). "
            "Fault when the residual exceeds 1.0 K while the fan runs at 90 % or more and the tower leaving water is "
            "more than 0.5 K above the plant set point cwst_sp, for 2 h. Below 90 % fan the controller hides fouling: "
            "those samples count as no evidence, so the fault holds until a high-load period shows the tower clean "
            "again (or 24 h without evidence)."
        ),
        asset_scope="tower",
        signals=(
            ("CT_0{n}", "cwst"),
            ("CT_0{n}", "fan_speed"),
            ("CT_0{n}", "running"),
            ("CHWS_01", "wet_bulb"),
            ("CHWS_01", "cwst_sp"),
            ("CHWS_01", "cooling_load"),
            ("CH_0{n}", "power_kw"),
        ),
        defaults={
            "threshold_k": 1.0,
            "fan_min_pct": 90.0,
            "sp_dev_k": 0.5,
            "persist_min": 120,
            "clear_min": 30,
            "stale_min": 1440,  # only judged while the fan is near its limit: hold through the night
            "run_min": 30,
            "steady_min": 15,
            "fan_steady_pct": 10.0,
            "smooth_min": 15,
            "q_design_kw": 3036.0,
            # ln(approach) = b0 + b1 ln(Q/Q_design) + b2 ln(fan/100); twin physics prior:
            # approach 3.5 K at 65 % fan and design heat rejection
            "approach_model": {"b0": 0.908, "b1": 0.6, "b2": -0.8},
        },
        intensity=(
            "Estimated heat-transfer capacity multiplier (1 = clean) = (expected / actual approach)^(1/0.7); "
            "evidence holds the approach excess (K)"
        ),
        lbnl_intensities=(0.95, 0.80, 0.65),
        fault_kind="ct_fouling",
    ),
    RuleDef(
        code="CHS-005",
        name="Riser Differential Pressure Sensor Bias",
        lbnl_fault="Secondary loop differential pressure sensor bias",
        description=(
            "The riser DP loop holds the measured value at its set point, so a biased transmitter shows as a mismatch "
            "between the measured DP and the DP the CHW pumps can deliver: ratio riser_dp / dp_expected(speed, flow) - 1, "
            "with dp_expected = a N^2 + b N q + c q^2 fitted on fault-free data (N mean running CHW pump speed in Hz, "
            "q plant flow per running pump). Fault when |ratio - 1| > 0.07 for 60 min. Isolation: only while the "
            "running pumps' own head (discharge - suction) stays within head_tolerance of its fault-free curve, so a "
            "degraded pump (impeller wear, cavitation) is not blamed on the transmitter. Needs calibration."
        ),
        asset_scope="plant",
        signals=(
            ("CHWS_01", "riser_dp"),
            ("CHWS_01", "riser_dp_sp"),
            ("CHWS_01", "plant_flow"),
            ("CHWS_01", "chwp_speed_cmd"),
            ("CHWP_0{n}", "speed"),
            ("CHWP_0{n}", "running"),
            ("CHWP_0{n}", "suction_press"),
            ("CHWP_0{n}", "discharge_press"),
        ),
        defaults={
            "threshold": 0.07,
            "offset": 0.0,
            "ewma_lambda": 0.1,
            "persist_min": 60,
            "clear_min": 30,
            "stale_min": 240,
            "run_min": 15,
            "steady_min": 15,
            "speed_steady_hz": 3.0,
            "dp_model": None,  # {"a", "b", "c"}: fitted by calibration
            "head_model": None,  # pump head curve {"a", "b", "c"}: fitted by calibration
            "head_offset": 0.0,
            "head_tolerance": 0.035,
        },
        intensity="Relative bias of the measured riser DP (fraction, measured/true - 1)",
        lbnl_intensities=(-0.20, -0.10, 0.10, 0.20),
        fault_kind="riser_dp_bias",
    ),
    RuleDef(
        code="CHS-006",
        name="Condenser 3-Way Valve Leakage",
        lbnl_fault="Condenser water 3-way bypass valve leakage",
        description=(
            "With the tower bypass valve at its minimum position (twv_pos <= twv_min_pos + 2 %), the mixing fraction "
            "f = (cw_supply_temp - tower_supply_temp) / (cw_return_temp - tower_supply_temp) should equal the valve's "
            "own bypass twv_pos / 100 (zero when closed, 0.2 at a 20 % minimum position). Fault when the excess "
            "f - twv_pos / 100 > 0.08 for 30 min, valid when cw_return_temp - tower_supply_temp > 2 K. "
            "Intensity ~ f (the leakage fraction)."
        ),
        asset_scope="plant",
        signals=(
            ("CHWS_01", "twv_pos"),
            ("CHWS_01", "twv_min_pos"),
            ("CHWS_01", "cw_supply_temp"),
            ("CHWS_01", "cw_return_temp"),
            ("CHWS_01", "tower_supply_temp"),
            ("CHWS_01", "cw_flow"),
        ),
        defaults={
            "threshold": 0.08,
            "offset": 0.0,
            "min_pos_margin_pct": 2.0,
            "min_range_k": 2.0,
            "persist_min": 30,
            "clear_min": 30,
            "stale_min": 240,
            "steady_min": 15,
            "pos_steady_pct": 2.0,
        },
        intensity="Leakage fraction f (0-1)",
        lbnl_intensities=(0.25, 0.50, 0.75),
        fault_kind="twv_leak",
    ),
    RuleDef(
        code="CHS-007",
        name="Condenser 3-Way Valve Stuck",
        lbnl_fault="Condenser water 3-way bypass valve stuck",
        description=(
            "Valve position disagrees with its command (|twv_pos - twv_cmd| > 10 % for 30 min), or the position "
            "moves less than 1 % over 2 h while the command spans more than 10 %."
        ),
        asset_scope="plant",
        signals=(
            ("CHWS_01", "twv_pos"),
            ("CHWS_01", "twv_cmd"),
            ("CHWS_01", "cw_flow"),
        ),
        defaults={
            "deviation_pct": 10.0,
            "persist_min": 30,
            "clear_min": 30,
            "stale_min": 240,
            "window_min": 120,
            "pos_range_pct": 1.0,
            "cmd_range_pct": 10.0,
        },
        intensity="Stuck position (fraction of full open)",
        lbnl_intensities=(0.50, 0.75),
        fault_kind="twv_stuck",
    ),
    RuleDef(
        code="CHS-008",
        name="CW Supply Temperature Loop Hunting (PI Tuning)",
        lbnl_fault="Condenser water supply temperature PI loop poorly tuned",
        description=(
            "Hunting of the tower fan PI loop: over a trailing hour, reversals of fan_speed_cmd (2 % deadband) exceed "
            "6, the command peak-to-peak exceeds 10 % and the tower leaving header temperature peak-to-peak exceeds "
            "0.3 K; fault when this holds for 1 h."
        ),
        asset_scope="plant",
        signals=(
            ("CHWS_01", "fan_speed_cmd"),
            ("CHWS_01", "tower_supply_temp"),
            ("CHWS_01", "cwst_sp"),
            ("CT_0{n}", "running"),
        ),
        defaults={
            "window_min": 60,
            "deadband_pct": 2.0,
            "reversals_per_h": 6,
            "cmd_pp_pct": 10.0,
            "temp_pp_k": 0.3,
            "persist_min": 60,
            "clear_min": 30,
            "stale_min": 240,
        },
        intensity="1 = loop hunting (LBNL fault present); evidence holds reversals per hour and amplitudes",
        lbnl_intensities=(1.0,),
        fault_kind="cw_pi_tuning",
    ),
)

RULES_BY_CODE: dict[str, RuleDef] = {r.code: r for r in RULES}

# Same mapping as the analytics.point_map seed (migration 0005), for offline use.
STATIC_LBNL: dict[tuple[str, str], str] = {
    ("CHWS_01", "cooling_load"): "CWL_SEC_LOAD",
    ("CHWS_01", "chwr_temp"): "CWL_PRI_RW_TEMP / CWL_SEC_RW_TEMP",
    ("CHWS_01", "chws_temp"): "CWL_PRI_SW_TEMP / CWL_SEC_SW_TEMP",
    ("CHWS_01", "fab_flow"): "CWL_SEC_CW_FLOW",
    ("CHWS_01", "plant_flow"): "CWL_PRI_CW_FLOW",
    ("CHWS_01", "chws_sp"): "CWL_PRI_SW_TEMPSPT",
    ("CHWS_01", "twv_pos"): "TWV_CTRL",
    ("CHWS_01", "cwst_sp"): "CT_SW_TEMPSPT",
    ("CHWS_01", "cw_supply_temp"): "CDWL_SW_TEMP",
    ("CHWS_01", "cw_return_temp"): "CDWL_RW_TEMP",
    ("CHWS_01", "cw_flow"): "CDWL_CW_FLOW",
    ("CHWS_01", "oa_temp"): "OA_TEMP",
    ("CHWS_01", "wet_bulb"): "OA_TEMP_WB",
    ("CHWS_01", "riser_dp_sp"): "CWL_SEC_DPSPTS",
    ("CHWS_01", "riser_dp"): "CWL_SEC_DP",
    ("CHWS_01", "fan_speed_cmd"): "CT_FAN_SPD_CTRL{n}",
    ("CT_0{n}", "running"): "CT_STA{n}",
    ("CT_0{n}", "cwrt"): "CT_RW_TEMP{n}",
    ("CT_0{n}", "cwst"): "CT_SW_TEMP{n}",
    ("CT_0{n}", "fan_speed"): "CT_FAN_SPD{n}",
    ("CT_0{n}", "fan_power"): "CT_POW{n}",
    ("CH_0{n}", "running"): "CHL_STA{n}",
    ("CH_0{n}", "load_pct"): "CHL_COMP_SPD_CTRL{n}",
    ("CH_0{n}", "chw_flow"): "CHL_CW_FLOW{n}",
    ("CH_0{n}", "cond_flow"): "CHL_CD_FLOW{n}",
    ("CH_0{n}", "chwrt"): "CHL_RW_TEMP{n}",
    ("CH_0{n}", "chwst"): "CHL_SW_TEMP{n}",
    ("CH_0{n}", "cwlt"): "CHL_SWCD_TEMP{n}",
    ("CH_0{n}", "cwet"): "CHL_RWCD_TEMP{n}",
    ("CH_0{n}", "power_kw"): "CHL_POW{n}",
    ("CWP_0{n}", "power_kw"): "CDWL_PM_POW{n}",
    ("CHWP_0{n}", "power_kw"): "CWL_PRI_PM_POW{n} / CWL_SEC_PM_POW{n}",
    ("CHWP_0{n}", "speed"): "CWL_SEC_PM_SPD{n}",
    ("CHWP_0{n}", "running"): "CWL_SEC_PM_STA{n}",
}


def lbnl_names(point_map: list[dict[str, Any]] | None) -> dict[tuple[str, str], str]:
    """(asset_pattern, param) -> LBNL name(s) from analytics.point_map rows (static fallback)."""
    if not point_map:
        return dict(STATIC_LBNL)
    out: dict[tuple[str, str], list[str]] = {}
    for r in point_map:
        out.setdefault((r["asset_pattern"], r["param_alias"]), []).append(r["lbnl_name"])
    # primary-loop names first (variable primary: one loop)
    return {k: " / ".join(sorted(v, key=lambda n: (0 if "_PRI_" in n else 1, n))) for k, v in out.items()}


def signals_json(rule: RuleDef, names: dict[tuple[str, str], str]) -> list[dict[str, Any]]:
    return [{"lbnl": names.get((asset, param)), "asset": asset, "param": param} for asset, param in rule.signals]


def defaults_json(rule: RuleDef) -> dict[str, Any]:
    """`fault_rules.defaults`: exactly the tunable parameters (the backend's updaterule
    accepts only these keys, keeping each value's JSON type; the UI lists them)."""
    return dict(rule.defaults)


def merged_params(rule: RuleDef, params: dict[str, Any] | None) -> dict[str, Any]:
    """Rule parameters in use: defaults overlaid with the stored params."""
    out = dict(rule.defaults)
    for k, v in (params or {}).items():
        if not k.startswith("_"):
            out[k] = v
    return out


async def seed_rules() -> list[str]:
    """Insert missing rules; refresh metadata (name, description, signals, defaults) of existing ones.

    Never touches params values set by calibration or an admin, `enabled`,
    `severity` or `calibration`; params only gain keys new in the defaults
    (`defaults || params` in one statement, so a concurrent admin edit is never
    lost). Rows already up to date are not written. Returns the inserted codes.
    """
    try:
        pm = await db.fetch_all("SELECT lbnl_name, asset_pattern, param_alias FROM analytics.point_map")
    except Exception:  # noqa: BLE001 - metadata only; fall back to the static copy
        log.warning("analytics.point_map unreadable; using the static LBNL names")
        pm = None
    names = lbnl_names(pm)
    inserted: list[str] = []
    for rule in RULES:
        signals, defaults = Jsonb(signals_json(rule, names)), Jsonb(defaults_json(rule))
        res = await db.fetch_one(
            "INSERT INTO analytics.fault_rules (code, name, lbnl_fault, description, asset_scope, signals, params, defaults, severity)"
            " VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) ON CONFLICT (code) DO NOTHING RETURNING code",
            (rule.code, rule.name, rule.lbnl_fault, rule.description, rule.asset_scope, signals, defaults, defaults, rule.severity),
        )
        if res:
            inserted.append(rule.code)
            continue
        await db.execute(
            """UPDATE analytics.fault_rules SET name = %(name)s, lbnl_fault = %(lbnl)s, description = %(desc)s, asset_scope = %(scope)s,
                   signals = %(signals)s, defaults = %(defaults)s, params = %(defaults)s || params
               WHERE code = %(code)s AND (name IS DISTINCT FROM %(name)s OR lbnl_fault IS DISTINCT FROM %(lbnl)s
                     OR description IS DISTINCT FROM %(desc)s OR asset_scope IS DISTINCT FROM %(scope)s
                     OR signals IS DISTINCT FROM %(signals)s OR defaults IS DISTINCT FROM %(defaults)s
                     OR NOT params ?& ARRAY(SELECT jsonb_object_keys(%(defaults)s)))""",
            {
                "code": rule.code, "name": rule.name, "lbnl": rule.lbnl_fault, "desc": rule.description,
                "scope": rule.asset_scope, "signals": signals, "defaults": defaults,
            },
        )  # fmt: skip
    if inserted:
        log.info("seeded fault rules %s", ",".join(inserted))
    return inserted


async def load_rules(*, enabled_only: bool = True) -> dict[str, dict[str, Any]]:
    """Stored rules by code: {code: {params (merged with defaults), severity, enabled, version, calibrated_at}}."""
    rows = await db.fetch_all(
        "SELECT code, params, severity, enabled, version, calibrated_at, updated_by FROM analytics.fault_rules"
    )
    out: dict[str, dict[str, Any]] = {}
    for r in rows:
        rule = RULES_BY_CODE.get(r["code"])
        if rule is None or (enabled_only and not r["enabled"]):
            continue
        out[r["code"]] = {
            "params": merged_params(rule, r["params"]),
            "severity": r["severity"],
            "enabled": r["enabled"],
            "version": r["version"],
            "calibrated_at": r["calibrated_at"],
            "updated_by": r["updated_by"],
        }
    return out


def default_rules() -> dict[str, dict[str, Any]]:
    """Rules with engineering defaults (no DB), same shape as load_rules."""
    return {
        r.code: {"params": dict(r.defaults), "severity": r.severity, "enabled": True, "version": 0, "calibrated_at": None, "updated_by": None}
        for r in RULES
    }
