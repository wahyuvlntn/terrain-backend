"""LBNL FDD rules on synthetic plant series (tests/analytics/synth.py).

Every rule must find its fault at the documented LBNL intensities, estimate
the intensity, and stay quiet on healthy operation (including other faults).
"""

from __future__ import annotations

import pytest

from afdd_ai.analytics import faults, rules

from .synth import Faults, dp_model, head_model, plant_frame

START = 120  # fault onset (minute)


def cfg(**overrides: dict) -> dict[str, dict]:
    """Default rule configuration with per-rule parameter overrides."""
    out = rules.default_rules()
    for code, params in overrides.items():
        out[code]["params"] = {**out[code]["params"], **params}
    return out


CALIBRATED = cfg(**{"CHS-005": {"dp_model": dp_model(), "head_model": head_model()}})


def detections(frame, rule_cfg=CALIBRATED) -> list[faults.Detection]:
    return faults.detect_batch(frame, rule_cfg)


def only(dets: list[faults.Detection], rule: str, asset: str) -> faults.Detection:
    assert [(d.rule, d.asset) for d in dets] == [(rule, asset)], [(d.rule, d.asset, d.start, d.message) for d in dets]
    return dets[0]


@pytest.mark.parametrize("seed", [0, 1, 2])
def test_healthy_plant_has_no_faults(seed):
    assert detections(plant_frame(12, seed=seed)) == []


def test_healthy_plant_at_low_load_and_wide_approach_has_no_faults():
    assert detections(plant_frame(12, seed=5, load_scale=0.75, approach_sp=5.0)) == []


@pytest.mark.parametrize("bias", [-2.0, -1.0, 1.0, 2.0])
def test_chl011_chiller_leaving_temperature_sensor_bias(bias):
    frame = plant_frame(8, seed=3, faults=Faults(chwst_bias={"CH_02": bias}, start_min=START))
    d = only(detections(frame), "CHL-011", "CH_02")
    assert d.ongoing and START <= d.start <= START + 30  # onset: the EWMA crosses the threshold
    assert d.start + 29 <= d.confirmed <= START + 90  # raised once it persisted 30 min
    assert d.intensity == pytest.approx(bias, abs=0.25)
    assert d.evidence["threshold_k"] == 0.5
    assert "sensor reads about" in d.message


@pytest.mark.parametrize("bias", [-2.0, -1.0, 1.0, 2.0])
def test_ct013_tower_leaving_temperature_sensor_bias(bias):
    frame = plant_frame(8, seed=4, faults=Faults(cwst_bias={"CT_01": bias}, start_min=START))
    d = only(detections(frame), "CT-013", "CT_01")
    assert d.ongoing and START <= d.start <= START + 30
    assert d.start + 59 <= d.confirmed <= START + 120  # 60 min persistence
    assert d.intensity == pytest.approx(bias, abs=0.2)


def test_ct013_not_raised_by_a_real_outlet_difference():
    # fouling makes one tower's water truly warmer: the header mixes it in, so the mixing residual stays ~0
    frame = plant_frame(8, seed=4, faults=Faults(ct_fouling={"CT_02": 0.8}, start_min=START))
    assert not [d for d in detections(frame) if d.rule == "CT-013"]


@pytest.mark.parametrize("capacity", [0.5, 0.6])
def test_ct014_tower_fouling(capacity):
    # high load and a tight approach push the common fan to its limit, so the approach cannot be held
    # (the rule's LBNL conditions: excess > 1 K, fan >= 90 %, leaving water > set point + 0.5 K, for 2 h)
    fouled = Faults(ct_fouling={"CT_01": capacity, "CT_02": capacity}, start_min=START)
    frame = plant_frame(10, seed=6, load_scale=1.2, approach_sp=3.0, faults=fouled)
    found = [d for d in detections(frame) if d.rule == "CT-014"]
    assert {d.asset for d in found} == {"CT_01", "CT_02"}, [(d.rule, d.asset) for d in detections(frame)]
    for d in found:
        assert START <= d.start <= START + 40  # onset (15-min smoothing)
        assert d.confirmed >= d.start + 119  # 2 h persistence
        assert d.intensity == pytest.approx(capacity, abs=0.1)
        assert d.evidence["fan_speed_pct"] >= 90 and d.evidence["approach_excess_k"] > 1.0
    # the same operating point with clean towers: the fan holds the approach, no fault
    assert detections(plant_frame(10, seed=6, load_scale=1.2, approach_sp=3.0)) == []


def test_ct014_needs_the_fan_at_its_limit():
    # the same fouling at normal load: the fan still has margin, so the approach is held (LBNL condition)
    frame = plant_frame(10, seed=6, faults=Faults(ct_fouling={"CT_01": 0.8, "CT_02": 0.8}, start_min=START))
    assert not [d for d in detections(frame) if d.rule == "CT-014"]


@pytest.mark.parametrize("bias", [-0.20, -0.10, 0.10, 0.20])
def test_chs005_riser_dp_sensor_bias(bias):
    frame = plant_frame(8, seed=7, faults=Faults(riser_dp_bias=bias, start_min=START))
    d = only(detections(frame), "CHS-005", "CHWS_01")
    assert START <= d.start <= START + 30 and d.confirmed >= d.start + 59
    assert d.intensity == pytest.approx(bias, abs=0.03)


@pytest.mark.parametrize("loss", [0.06, 0.10])
def test_chs005_not_blamed_on_the_transmitter_when_pumps_lose_head(loss):
    """A degraded pump also reads low against the healthy riser curve; its own head isolates it."""
    frame = plant_frame(8, seed=7, faults=Faults(pump_head_loss=loss, start_min=START))
    assert [d for d in detections(frame) if d.rule == "CHS-005"] == []
    # without the pump head check the transmitter would be blamed
    dp_only = cfg(**{"CHS-005": {"dp_model": dp_model()}})
    assert [d.rule for d in detections(frame, dp_only)].count("CHS-005") == 1


def test_chs005_sensor_bias_with_pump_head_isolation_reports_healthy_pumps():
    frame = plant_frame(8, seed=7, faults=Faults(riser_dp_bias=0.1, start_min=START))
    d = only(detections(frame), "CHS-005", "CHWS_01")
    assert abs(d.evidence["pump_head_deviation"]) < 0.02


def test_chs005_skipped_until_calibrated():
    frame = plant_frame(4, seed=7, faults=Faults(riser_dp_bias=0.2, start_min=START))
    lay = faults.Layout.from_aliases(frame.aliases())
    (ev,) = faults.evaluate(frame, lay, {"CHS-005": rules.default_rules()["CHS-005"]})
    assert ev.skipped and not ev.detections


@pytest.mark.parametrize("leak", [0.25, 0.50, 0.75])
def test_chs006_three_way_valve_leakage(leak):
    frame = plant_frame(6, seed=8, faults=Faults(twv_leak=leak, start_min=START))
    d = only(detections(frame), "CHS-006", "CHWS_01")
    assert START <= d.start <= START + 20 and d.confirmed >= d.start + 29
    assert d.intensity == pytest.approx(leak, abs=0.05)


def test_chs006_quiet_at_a_raised_minimum_valve_position():
    # a 20 % minimum position bypasses 0.2 of the flow by design (twin: b = max(twv_pos / 100, leak)): no leak
    frame = plant_frame(8, seed=8, twv_min_pos=20.0)
    assert detections(frame) == []
    lay = faults.Layout.from_aliases(frame.aliases())
    s = faults.chs006_stats(frame, lay, rules.default_rules()["CHS-006"]["params"])
    ok = s["at_min"] & s["gate"]
    assert abs(float(s["f"][ok].mean()) - 0.2) < 0.02 and abs(float(s["excess"][ok].mean())) < 0.02


def test_chs006_leak_beyond_a_raised_minimum_valve_position():
    frame = plant_frame(6, seed=8, faults=Faults(twv_leak=0.5, start_min=START), twv_min_pos=20.0)
    d = only(detections(frame), "CHS-006", "CHWS_01")
    assert START <= d.start <= START + 20 and d.confirmed >= d.start + 29
    assert d.intensity == pytest.approx(0.5, abs=0.05)  # the leakage fraction, not the excess
    ev = d.evidence
    assert ev["expected_fraction"] == pytest.approx(0.2, abs=0.01) and ev["excess_fraction"] == pytest.approx(0.3, abs=0.05)
    assert ev["mixing_fraction"] == pytest.approx(0.5, abs=0.05) and ev["twv_min_pos_pct"] == 20.0
    assert "20 % expected from its position" in d.message


@pytest.mark.parametrize("stuck", [0.50, 0.75])
def test_chs007_three_way_valve_stuck(stuck):
    frame = plant_frame(6, seed=9, faults=Faults(twv_stuck=stuck, start_min=START))
    found = detections(frame)
    d = next(d for d in found if d.rule == "CHS-007")
    assert d.asset == "CHWS_01" and START <= d.start <= START + 20 and d.confirmed >= d.start + 29
    assert d.intensity == pytest.approx(stuck, abs=0.01)
    # a valve stuck open also mixes return water in, but it is not at its minimum: no leakage fault
    assert not [x for x in found if x.rule == "CHS-006"]


def test_chs008_pi_loop_hunting():
    frame = plant_frame(6, seed=10, faults=Faults(cw_pi_tuning=True, start_min=START))
    d = only(detections(frame), "CHS-008", "CHWS_01")
    assert START <= d.start <= START + 60  # the trailing hour shows hunting
    assert d.confirmed >= d.start + 59  # for an hour
    assert d.evidence["reversals_per_h"] > 6 and d.evidence["fan_cmd_pp_pct"] > 10


def test_fault_clears_when_the_condition_goes_away():
    frame = plant_frame(10, seed=11, faults=Faults(twv_leak=0.5, start_min=START))
    leak = frame.data["CHWS_01.cw_supply_temp"].copy()
    clean = plant_frame(10, seed=11).data["CHWS_01.cw_supply_temp"]
    leak[360:] = clean[360:]  # repaired at minute 360
    frame.data["CHWS_01.cw_supply_temp"] = leak
    d = only(detections(frame), "CHS-006", "CHWS_01")
    assert not d.ongoing and 355 <= d.end <= 362


def test_not_running_units_are_not_judged():
    frame = plant_frame(8, seed=3, faults=Faults(chwst_bias={"CH_03": 2.0}, start_min=START))
    assert detections(frame) == []  # CH_03 is the standby chiller


def test_disabled_rules_are_not_evaluated():
    frame = plant_frame(6, seed=8, faults=Faults(twv_leak=0.5, start_min=START))
    rule_cfg = {k: v for k, v in CALIBRATED.items() if k != "CHS-006"}
    assert detections(frame, rule_cfg) == []


def test_layout_from_aliases():
    lay = faults.Layout.from_aliases(["CH_01", "CH_02", "CT_01", "CHWS_01", "CHWP_01", "CWP_01", "MAU_01", "CH_01X"])
    assert lay.system == "CHWS_01" and lay.chillers == ["CH_01", "CH_02"] and lay.towers == ["CT_01"]
    assert lay.chw_pumps == ["CHWP_01"] and lay.cw_pumps == ["CWP_01"] and lay.maus == ["MAU_01"]


def test_rule_metadata_matches_the_contract():
    assert [r.code for r in rules.RULES] == ["CHL-011", "CT-013", "CT-014", "CHS-005", "CHS-006", "CHS-007", "CHS-008"]
    names = rules.lbnl_names(None)
    for r in rules.RULES:
        assert r.asset_scope in ("chiller", "tower", "plant")
        sig = rules.signals_json(r, names)
        assert all({"lbnl", "asset", "param"} <= set(s) for s in sig)
        # defaults are exactly the tunable parameters (the backend's updaterule accepts only these keys)
        assert rules.defaults_json(r) == r.defaults and not any(k.startswith("_") for k in r.defaults)
        assert {"persist_min", "clear_min"} <= set(r.defaults)
    sig = {(s["asset"], s["param"]): s["lbnl"] for s in rules.signals_json(rules.RULES_BY_CODE["CHL-011"], names)}
    assert sig[("CH_0{n}", "chwst")] == "CHL_SW_TEMP{n}"
    assert sig[("CHWS_01", "chws_temp")] == "CWL_PRI_SW_TEMP / CWL_SEC_SW_TEMP"


def test_merged_params_ignores_private_keys():
    rule = rules.RULES_BY_CODE["CHS-006"]
    p = rules.merged_params(rule, {"threshold": 0.12, "_note": "x"})
    assert p["threshold"] == 0.12 and "_note" not in p and p["persist_min"] == 30


def test_ct014_holds_while_the_fan_is_below_its_limit():
    # evening: the load drops and the fan leaves its limit; fouling is then hidden by the PI loop, which is
    # no evidence that the tower is clean: the fault stays open (stale_min 24 h) instead of clearing nightly
    fouled = Faults(ct_fouling={"CT_01": 0.5, "CT_02": 0.5}, start_min=START)
    frame = plant_frame(14, seed=6, load_scale=1.2, approach_sp=3.0, faults=fouled)
    for ct in ("CT_01", "CT_02"):
        fan = frame.data[f"{ct}.fan_speed"].copy()
        fan[600:] = 60.0
        frame.data[f"{ct}.fan_speed"] = fan
    found = [d for d in detections(frame) if d.rule == "CT-014"]
    assert {d.asset for d in found} == {"CT_01", "CT_02"}
    assert all(d.ongoing and 590 <= d.end < 600 for d in found)
    assert rules.RULES_BY_CODE["CT-014"].defaults["stale_min"] == 1440
