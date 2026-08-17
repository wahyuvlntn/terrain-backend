"""Backend tools called directly (fake backend over HTTP): request shapes, compact results, errors."""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any

import pytest

from afdd_ai.agent import charts, tools_backend
from afdd_ai.agent.backend_api import normalise_alias

from .support import TOKEN, FakeError

CTX = SimpleNamespace(state={"temp:afdd_session": TOKEN, "temp:permissions": ["aiassistant.read", "aiinsights.read"], "temp:user_id": 990001})

ORDERS = [
    {"no": 1, "order_id": "PM-10-26-001", "planned_start": "2026-10-01T01:00:00.000Z", "planned_end": None, "maintenance_type": "PREVENTIVE", "priority": "MEDIUM", "issue": None, "severity": "LOW", "building": "CUB", "floor": "L1", "room": "Chiller Plant Room", "equipment": "Chiller CH-01", "status": "Closed", "requested_by": "Sarah Lim", "technician": "Ahmad"},
    {"no": 2, "order_id": "CM-10-26-004", "planned_start": "2026-10-09T01:00:00.000Z", "planned_end": "2026-10-09T05:00:00.000Z", "maintenance_type": "CORRECTIVE", "priority": "HIGH", "issue": "THR-02 - Fouling", "severity": "HIGH", "building": "CUB", "floor": "Roof", "room": "Cooling Tower Yard", "equipment": "Cooling Tower CT-01", "status": "In Progress", "requested_by": "System", "technician": ""},
]


@pytest.fixture
def extra_handlers(fake_backend):
    h = fake_backend.handlers
    before = set(h)
    h["digital_twin/getassetsummary"] = lambda body, tok: {
        "alias": "CH_02",
        "name": "Chiller CH-02",
        "status": "IDLE",
        "status_start_time": "2026-10-10T00:00:00.000Z",
        "uptime_window": {"uptime": 0.5, "running_hours": 12.0, "total_hours": 24.0},
        "statuses": [{"value": "RUNNING", "start": "2026-10-09T06:00:00.000Z", "end": "2026-10-10T00:00:00.000Z"}],
        "alarms": [{"name": "CHL-008", "severity": "WARNING", "message": "dev", "type": "ALARM", "start": "2026-10-10T05:00:00.000Z", "end": None, "active": True}],
        "maintenances": [{"id": 9, "name": "CM-10-26-004", "type": "CORRECTIVE", "status": "IN_PROGRESS", "issue": "THR-02", "planned_start": "2026-10-09T01:00:00.000Z"}],
        "warning_counts": 2,
        "critical_counts": 0,
        "running_hours": 10234.56,
    }
    h["maintenance_order/get_maintenance_export_data"] = lambda body, tok: [o for o in ORDERS if not body.get("search") or body["search"].upper() in o["order_id"]]
    h["maintenance_order/get_maintenance_order_list"] = lambda body, tok: {"data": [{"id": 44, "name": "CM-10-26-004"}], "count": 1, "all_data_ids": [44]}
    h["maintenance_order/get_maintenance_order_detail"] = lambda body, tok: [{"id": 44, "description": "Clean the fill", "created_at": "2026-10-09T00:00:00.000Z", "planned_start": "2026-10-09T01:00:00.000Z", "planned_end": None, "requested_by": "System", "approved_by": "Sarah Lim", "estimated_time": 7200, "actual_time": 3600, "issue_relation_id": 3, "reject_reason": None, "status": "IN_PROGRESS"}]
    h["maintenance_order/get_progress"] = lambda body, tok: [{"datetime": "2026-10-09T02:00:00.000Z", "status": "IN_PROGRESS", "details": {"note": "started"}}]
    h["early_warning_diagnostics/export_early_warning_diagnostics"] = lambda body, tok: [{"order_id": "CM-10-26-004", "equipment": "Cooling Tower CT-01", "detected_issue": "THR-02 - Fouling", "planned_start": "2026-10-09T01:00:00.000Z", "severity": "HIGH", "status": "In Progress"}]
    h["maintenance_performance/get_mtbf_average"] = lambda body, tok: [{"mtbf": 812.345, "unit": "hours"}]
    h["maintenance_performance/get_mttr_average"] = lambda body, tok: [{"mttr": 3.21, "unit": "hours"}]
    h["maintenance_performance/get_paginated_mtbf_mttr"] = lambda body, tok: {"count": 1, "data": [{"id": 1, "assetName": "Chiller CH-01", "mtbf": "812.3", "mttr": "3.2"}]}
    h["ai_insights/getfaults"] = lambda body, tok: [{"id": 3, "rule_code": "CT-014", "asset_alias": "CT_01", "severity": "WARNING", "status": "ACTIVE", "message": "fouling", "intensity_estimate": 0.8123456, "started_at": "2026-10-09T00:00:00.000Z"}]
    h["ai_insights/getanomalies"] = lambda body, tok: [{"id": 5, "asset_alias": "CH_01", "param_alias": "chwst", "detector": "zscore", "severity": "WARNING", "status": "ACTIVE", "value": 7.1, "expected": 6.5, "score": 4.2, "started_at": "2026-10-10T04:00:00.000Z"}]
    yield fake_backend
    for k in set(h) - before:
        h.pop(k, None)


def test_normalise_alias():
    assert [normalise_alias(x) for x in ("ch-1", "CH 01", "chws_1", "ct3", "CHWP-02", "Chiller 1")] == ["CH_01", "CH_01", "CHWS_01", "CT_03", "CHWP_02", "CHILLER_01"]


async def test_dashboard_overview(fake_backend):
    out = await tools_backend.get_dashboard_overview(tool_context=CTX)
    assert out["equipment"] == 4 and out["by_status"] == {"RUNNING": 3, "IDLE": 1}
    assert [a["alias"] for a in out["attention"]] == ["CH_02"]
    plant = out["plant"]["values"]
    assert plant["plant_kw"] == 1450.2 and plant["cooling_load_rt"] == round(5200 / 3.517, 1)
    assert out["alarms_24h"]["totalAlarm"] == 6 and out["ai_insights"]["faults"]["active"] == 1
    (count,) = fake_backend.calls_to("alarm/getalarmcount")
    assert count["body"]["asset_ids"] == [1, 2, 4, 13] and count["body"]["start_ts"].endswith("Z")


async def test_asset_summary_and_lists(extra_handlers):
    s = await tools_backend.get_asset_summary(tool_context=CTX, asset="ch2", hours=500)
    assert s["status"] == "IDLE" and s["window_hours"] == 168 and s["status_since"] == "2026-10-10T08:00+08:00"
    assert s["recent_alarms"][0]["active"] is True and s["maintenance"][0]["order"] == "CM-10-26-004"
    assert extra_handlers.calls_to("digital_twin/getassetsummary")[0]["body"] == {"asset_id": 2, "hours": 168}

    lst = await tools_backend.list_assets(tool_context=CTX, asset_type="chiller", status="running")
    assert [a["alias"] for a in lst["assets"]] == ["CH_01"]

    alarms = await tools_backend.get_alarm_events(tool_context=CTX, assets=["CH_02"], hours=48, alarm_type="alarm")
    assert alarms["total"] == 1 and alarms["events"][0]["time"] == "2026-10-10T13:00+08:00"
    body = extra_handlers.calls_to("alarm/getalarmbyassetid")[0]["body"]
    assert body["asset_ids"] == [2] and body["alarm_type_id"] == 1 and body["limit"] == 50


async def test_maintenance_tools(extra_handlers):
    orders = await tools_backend.get_maintenance_orders(tool_context=CTX, assets=["CT_01"], status="open, in progress", order_type="corrective")
    assert [o["order"] for o in orders["orders"]] == ["CM-10-26-004", "PM-10-26-001"]
    body = extra_handlers.calls_to("maintenance_order/get_maintenance_export_data")[0]["body"]
    assert body["filter"] == {"locationIds": [], "assetIds": [4], "maintenanceStatusIds": [3, 4], "maintenanceTypeIds": [1]}
    bad = await tools_backend.get_maintenance_orders(tool_context=CTX, status="finished")
    assert "Unknown order status" in bad["error"]

    detail = await tools_backend.get_maintenance_order_detail(tool_context=CTX, order="cm-10-26-004")
    assert detail["id"] == 44 and detail["issue"] == "THR-02 - Fouling" and detail["from_early_warning"] is True
    assert detail["history"][0]["details"] == {"note": "started"}
    missing = await tools_backend.get_maintenance_order_detail(tool_context=CTX, order="CM-99-99-999")
    assert "No visible maintenance order" in missing["error"]

    ew = await tools_backend.get_early_warnings(tool_context=CTX)
    assert ew["early_warnings"][0]["detected_issue"] == "THR-02 - Fouling"
    kpi = await tools_backend.get_reliability_kpis(tool_context=CTX, assets=["CH_01"], days=30)
    assert kpi["average_mtbf_h"] == 812.3 and kpi["per_unit_trailing_year"][0]["mtbf_h"] == "812.3"
    assert extra_handlers.calls_to("maintenance_performance/get_mtbf_average")[0]["body"]["asset"] == [1]


async def test_ai_tools_and_app_api(extra_handlers):
    faults = await tools_backend.get_faults(tool_context=CTX, rule_codes=["ct-014"], assets=["CT_01"])
    assert faults["faults"][0]["intensity_estimate"] == 0.812
    assert extra_handlers.calls_to("ai_insights/getfaults")[0]["body"]["rule_codes"] == ["CT-014"]
    anomalies = await tools_backend.get_anomalies(tool_context=CTX, status="")
    assert anomalies["anomalies"][0]["parameter"] == "chwst" and "status" not in extra_handlers.calls_to("ai_insights/getanomalies")[0]["body"]
    recs = await tools_backend.get_recommendations(tool_context=CTX)
    assert recs["recommendations"][0]["recommended_setpoints"] == {"chws_sp": 7.0}

    live = await tools_backend.call_app_api(tool_context=CTX, script="/digital_twin/getliveoverview", params={"aliases": "CH_01,CT_01"})
    assert [r["alias"] for r in live["data"]] == ["CH_01", "CT_01"]
    assert "not accepted" in (await tools_backend.call_app_api(tool_context=CTX, script="digital_twin/getliveoverview", params={"sql": "x"}))["error"]
    assert "must be of type int" in (await tools_backend.call_app_api(tool_context=CTX, script="ai_insights/getfault", params={"id": "abc"}))["error"]
    for script in ("digital_twin/writeparameter", "simulation/approve", "ai_insights/applyrecommendation", "maintenance_order/submit"):
        assert "not an allowed" in (await tools_backend.call_app_api(tool_context=CTX, script=script, params={}))["error"]


async def test_fault_instruction_is_passed_whole(extra_handlers):
    md = "### Safety\n" + "1. Isolate the valve and lock out the actuator [1].\n" * 120  # ~6,300 characters
    extra_handlers.handlers["ai_insights/getfault"] = lambda body, tok: {
        "id": 43,
        "message": "m" * 3000,
        "instruction_md": md,
        "instruction_sources": [{"n": 1, "document_id": 5, "title": "Manual", "page": 3}],
    }
    out = await tools_backend.call_app_api(tool_context=CTX, script="ai_insights/getfault", params={"id": 43})
    data = out["data"]
    assert 6000 < len(md) <= tools_backend.INSTRUCTION_MAX_STR
    assert data["instruction_md"].count("Isolate the valve") == 120 and not data["instruction_md"].endswith("…")
    assert len(data["message"]) == 1500 and data["message"].endswith("…")  # other strings stay compact
    assert data["instruction_sources"][0]["document_id"] == 5


async def test_chart_tools(fake_backend):
    trend = await charts.chart_parameter_trend(tool_context=CTX, asset="CH_01", params=["chwst", "power_kw"], hours=6)
    assert [s["param"] for s in trend["series"]] == ["chwst", "power_kw"]
    option = trend["chart"]["option"]
    assert [a["name"] for a in option["yAxis"]] == ["°C", "kW"] and option["series"][1]["yAxisIndex"] == 1
    assert [c["body"]["minutes"] for c in fake_backend.calls_to("digital_twin/getparametertrend")] == [360, 360]

    cmp_ = await charts.chart_compare(tool_context=CTX, assets=["CH_01", "CH_02"], param="chwst", hours=2, mode="bar")
    assert cmp_["chart"]["option"]["xAxis"]["data"] == ["CH_01", "CH_02"]
    missing = await charts.chart_compare(tool_context=CTX, assets=["CH_01", "CT_01"], param="run_mode")
    assert "CT_01 has no parameter 'run_mode'" in missing["error"]


async def test_session_errors(fake_backend):
    expired = SimpleNamespace(state={"temp:afdd_session": "expired-token"})
    out = await tools_backend.list_assets(tool_context=expired)
    assert "session has expired" in out["error"]
    none = await tools_backend.list_assets(tool_context=SimpleNamespace(state={}))
    assert "No user session" in none["error"]

    def boom(body: dict[str, Any], tok: str):
        raise FakeError(500, "err_something_went_wrong")

    fake_backend.handlers["digital_twin/getassets"] = boom
    try:
        out = await tools_backend.list_assets(tool_context=CTX)
        assert "internal backend error" in out["error"]
    finally:
        fake_backend._install_defaults()
