"""FDD rule calibration on fault-free synthetic data (calibration.py)."""

from __future__ import annotations

import numpy as np
import pytest

from afdd_ai.analytics import calibration, faults, rules

from .synth import DP_A, DP_B, DP_C, HEAD_LOSS_C, Faults, plant_frame

START = 120


@pytest.fixture(scope="module")
def calibrated() -> dict[str, tuple[dict, dict]]:
    return calibration.calibrate_frame(plant_frame(48, seed=21))


def as_config(results: dict[str, tuple[dict, dict]]) -> dict[str, dict]:
    out = rules.default_rules()
    for code, (updates, _) in results.items():
        out[code]["params"] = {**out[code]["params"], **updates}
    return out


def test_every_rule_is_calibrated(calibrated):
    assert set(calibrated) == {r.code for r in rules.RULES}
    for code, (updates, stats) in calibrated.items():
        assert updates, (code, stats)
        assert "error" not in stats


def test_thresholds_never_drop_below_the_engineering_floor(calibrated):
    for code, (updates, _) in calibrated.items():
        d = rules.RULES_BY_CODE[code].defaults
        for key in ("threshold_k", "threshold", "mix_threshold_k", "deviation_pct", "reversals_per_h", "cmd_pp_pct", "temp_pp_k"):
            if key in updates:
                assert updates[key] >= d[key], (code, key)


def test_residual_offsets_are_small_on_fault_free_data(calibrated):
    assert abs(calibrated["CHL-011"][0]["offset_k"]) < 0.05
    assert abs(calibrated["CT-013"][0]["offset_k"]) < 0.05
    assert abs(calibrated["CHS-006"][0]["offset"]) < 0.02


def test_riser_dp_pump_curve_is_fitted(calibrated):
    updates, stats = calibrated["CHS-005"]
    m = updates["dp_model"]
    n_hz, q = np.array([42.0, 46.0, 48.0]), np.array([520.0, 560.0, 600.0])
    truth = DP_A * n_hz**2 + DP_B * n_hz * q + DP_C * q**2
    assert np.allclose(faults.dp_expected(m, n_hz, q), truth, rtol=0.02)
    assert stats["fit"]["rmse_kpa"] < 3.0
    # the pumps' own head curve (bar) for the isolation check
    hm = updates["head_model"]
    head = (truth + HEAD_LOSS_C * q**2) / 100
    assert np.allclose(faults.dp_expected(hm, n_hz, q), head, rtol=0.03)
    assert abs(updates["head_offset"]) < 0.01 and 0.035 <= updates["head_tolerance"] < 0.08


def test_tower_approach_model_is_fitted(calibrated):
    updates, stats = calibrated["CT-014"]
    m = updates["approach_model"]
    assert stats["fit"]["n"] > calibration.MIN_SAMPLES
    # the synthetic towers follow app = 2.48 qf^0.6 (100 / fan)^0.8: the fit must reproduce it
    exp = faults.approach_expected(m, np.array([1.0, 0.8]), np.array([100.0, 70.0]))
    assert exp == pytest.approx([2.48, 2.48 * 0.8**0.6 * (100 / 70) ** 0.8], rel=0.08)


def test_calibrated_rules_keep_detecting_and_stay_quiet(calibrated):
    cfg = as_config(calibrated)
    assert faults.detect_batch(plant_frame(12, seed=22), cfg) == []
    cases = [
        (Faults(chwst_bias={"CH_01": 1.0}, start_min=START), "CHL-011"),
        (Faults(cwst_bias={"CT_02": -1.0}, start_min=START), "CT-013"),
        (Faults(riser_dp_bias=0.1, start_min=START), "CHS-005"),
        (Faults(twv_leak=0.25, start_min=START), "CHS-006"),
        (Faults(twv_stuck=0.5, start_min=START), "CHS-007"),
        (Faults(cw_pi_tuning=True, start_min=START), "CHS-008"),
    ]
    for fault, code in cases:
        found = {d.rule for d in faults.detect_batch(plant_frame(8, seed=23, faults=fault), cfg)}
        assert code in found, (code, found)


def test_chs006_calibrates_the_excess_over_the_valve_position():
    # fault-free with a 20 % minimum position: the mixing fraction is 0.2, its excess (the rule's statistic) ~0
    updates, stats = calibration.calibrate_frame(plant_frame(48, seed=21, twv_min_pos=20.0))["CHS-006"]
    assert abs(updates["offset"]) < 0.02 and updates["threshold"] >= rules.RULES_BY_CODE["CHS-006"].defaults["threshold"]
    assert stats["excess_fraction"]["n"] > calibration.MIN_SAMPLES
    cfg = rules.default_rules()
    cfg["CHS-006"]["params"] |= updates
    assert faults.detect_batch(plant_frame(8, seed=23, twv_min_pos=20.0), cfg) == []
    found = faults.detect_batch(plant_frame(8, seed=23, twv_min_pos=20.0, faults=Faults(twv_leak=0.5, start_min=START)), cfg)
    assert [d.rule for d in found] == ["CHS-006"]


class FakeRulesDb:
    """analytics.fault_rules for apply_job: an administrator edits CHS-006 while the calibration runs."""

    def __init__(self, conditional_rows: int):
        self.conditional_rows = conditional_rows
        self.statements: list[tuple[str, tuple]] = []

    async def fetch_all(self, sql, params=None):  # noqa: ANN001
        return [{"code": "CHS-006", "params": {"threshold": 0.08, "offset": 0.0, "persist_min": 30}, "updated_by": None, "version": 3}]

    async def execute(self, sql, params=None):  # noqa: ANN001
        self.statements.append((sql, params))
        return self.conditional_rows if "updated_by IS NULL" in sql else 1


@pytest.mark.parametrize("edited_meanwhile", [False, True])
async def test_calibration_merges_params_and_keeps_a_concurrent_admin_edit(monkeypatch, edited_meanwhile):
    fake = FakeRulesDb(conditional_rows=0 if edited_meanwhile else 1)

    async def sim_frames(job_id):  # noqa: ANN001
        return {"baseline": plant_frame(36, seed=51)}

    monkeypatch.setattr(calibration.data, "sim_frames", sim_frames)
    monkeypatch.setattr(calibration.db, "fetch_all", fake.fetch_all)
    monkeypatch.setattr(calibration.db, "execute", fake.execute)
    applied = await calibration.apply_job(42)
    sql, params = fake.statements[0]
    # merged into the stored params (never a full overwrite) and only while no administrator edited the rule
    assert "params = COALESCE(params, '{}'::jsonb) || %s" in sql and "WHERE code = %s AND updated_by IS NULL" in sql
    updates = applied["CHS-006"]["updates"]
    assert params[0].obj == updates and set(updates) == {"offset", "threshold"} and params[-1] == "CHS-006"
    if edited_meanwhile:
        (sql2, params2), = fake.statements[1:]
        assert "SET calibration = %s, calibrated_at = %s WHERE code = %s" in sql2 and "params" not in sql2.split("WHERE")[0]
        assert params2[0].obj["applied"] is False and "edited by an administrator" in params2[0].obj["note"]
        assert applied["CHS-006"]["applied"] is False
    else:
        assert len(fake.statements) == 1 and applied["CHS-006"]["applied"] is True


def test_too_little_running_data_changes_nothing():
    for code, (updates, stats) in calibration.calibrate_frame(plant_frame(1, seed=24)).items():
        assert updates == {}, (code, updates, stats)


def test_hunting_thresholds_keep_the_temperature_check_a_plausibility_test(calibrated):
    updates, stats = calibrated["CHS-008"]
    assert updates["temp_pp_k"] <= max(rules.RULES_BY_CODE["CHS-008"].defaults["temp_pp_k"], stats["temp_pp_k"]["median"]) + 1e-9


def test_job_spec_is_a_nominal_fault_free_72_hour_run():
    spec = calibration.job_spec()
    assert spec["duration_min"] == 4320 and spec["initial"] == "nominal" and spec["stochastic"] is False
    assert spec["actions"] == [] and spec["variants"] == [{"name": "baseline", "actions": []}]
    assert spec["purpose"] == calibration.PURPOSE


def test_stored_model_coefficients_keep_their_precision():
    from afdd_ai.analytics import data

    stored = data.jsonable({"head_model": {"a": 0.00119481, "b": -3.7e-05, "c": 9.2e-07}, "x": np.float64(1234.567891234)})
    assert stored["head_model"] == {"a": 0.00119481, "b": -3.7e-05, "c": 9.2e-07}
    assert stored["x"] == 1234.5679 and data.jsonable(float("nan")) is None
