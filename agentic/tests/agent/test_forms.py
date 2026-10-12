"""Maintenance order tools, simulation forms and form submissions (fake backend over HTTP)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from google.adk.events import Event
from google.genai import types

from afdd_ai.agent import forms, simforms, stream, tools_maintenance
from afdd_ai.agent.backend_api import ToolError

from .support import TOKEN

STATE = {"temp:afdd_session": TOKEN, "temp:permissions": ["aiassistant.read", "maintenanceorder.create", "maintenanceorder.approve"], "temp:user_id": 990001}
CTX = SimpleNamespace(state=STATE)

DETAIL = {
    "id": 44, "equipment": "1", "maintenance_type_id": "1", "severity": "2", "priority": "1", "status": "REQUESTED",
    "planned_start": "2026-10-16T01:00:00.000Z", "planned_end": "2026-10-16T03:00:00.000Z", "description": "Clean the condenser",
    "technicians": "ahmad_faizal", "instructions": "7,9", "maintenance_id": None,
}  # fmt: skip


@pytest.fixture
def mo_backend(fake_backend):
    h = fake_backend.handlers
    before = set(h)
    h["maintenance_order/get_technicians"] = lambda body, tok: [
        {"label": "Ahmad Faizal", "value": "ahmad_faizal"},
        {"label": "Lim Kok Wai", "value": "lim_kokwai"},
    ]
    h["maintenance_order/get_maintenance_instructions"] = lambda body, tok: [
        {"label": "CHL-005 - Condenser Tube Fouling", "value": 7, "estimated_time_minute": 240, "is_diagnostic": True, "is_repair": True},
        {"label": "CHL-012 - Chiller Efficiency Loss", "value": 9, "estimated_time_minute": 60, "is_diagnostic": True, "is_repair": True},
    ]
    h["maintenance_order/submit"] = lambda body, tok: {"message": "ok", "result": [{"maintenance_id": 44}]}
    h["maintenance_order/get_maintenance_order_detail"] = lambda body, tok: [DETAIL]
    h["maintenance_order/get_maintenance_name_and_type"] = lambda body, tok: {"name": "CM-10-26-140", "is_corrective": True}
    h["maintenance_order/get_maintenance_order_list"] = lambda body, tok: {"data": [{"id": 44, "name": "CM-10-26-140"}], "count": 1}
    h["maintenance_order_work_report/close_work_report"] = lambda body, tok: "Maintenance order closed"
    h["simulation/submit"] = lambda body, tok: {"id": 301, "title": body["title"], "status": "QUEUED", "kind": "scenario"}
    h["production/submitplan"] = lambda body, tok: {"id": 302, "title": body["title"], "status": "QUEUED", "kind": "production"}
    fake_backend.calls.clear()
    yield fake_backend
    for k in set(h) - before:
        h.pop(k, None)


async def test_create_order_shows_a_prefilled_form_when_details_are_missing(mo_backend):
    out = await tools_maintenance.create_maintenance_order(CTX, equipment="ch-1", order_type="corrective")
    form = out["form"]
    assert out["status"] == "WAITING_FOR_USER" and form["kind"] == "maintenance_order.create"
    values = {f["name"]: f.get("value") for f in form["fields"]}
    assert values["asset_id"] == 1 and values["maintenance_type_id"] == 1 and values["severity_id"] is None
    assert {"severity", "planned start"} <= set(out["message"].split("missing ")[1].split(".")[0].split(", "))
    assert not mo_backend.calls_to("maintenance_order/submit")  # nothing was created


async def test_create_order_acts_when_complete(mo_backend):
    out = await tools_maintenance.create_maintenance_order(
        CTX, equipment="CH_01", order_type="corrective", severity="critical", planned_start="2026-10-16T09:00",
        technicians=["Ahmad"], issue_codes=["CHL-005"],
    )  # fmt: skip
    assert out["maintenance_order"]["name"] == "CM-10-26-140" and out["maintenance_order"]["action"] == "created"
    (call,) = mo_backend.calls_to("maintenance_order/submit")
    body = call["body"]
    assert call["origin"] == "agent"
    assert body["submit_type"] == "CREATED" and body["asset_id"] == 1 and body["maintenance_type_id"] == 1 and body["severity_id"] == 1
    assert body["planned_start"] == "2026-10-16T09:00:00+08:00" and body["technicians"] == "ahmad_faizal" and body["instructions"] == "7"


async def test_unknown_technician_is_explained_in_the_form(mo_backend):
    out = await tools_maintenance.create_maintenance_order(
        CTX, equipment="CH_01", order_type="corrective", severity="low", planned_start="2026-10-16T09:00", technicians=["Nobody"]
    )
    assert out["form"] and "cannot work on this equipment" in out["problems"][0]


async def test_actions_need_their_reason_note_or_verdicts(mo_backend):
    out = await tools_maintenance.maintenance_order_action(CTX, order="CM-10-26-140", action="reject")
    assert out["form"]["kind"] == "maintenance_order.reject"
    out = await tools_maintenance.maintenance_order_action(CTX, order="CM-10-26-140", action="close")
    checks = [f for f in out["form"]["fields"] if f["name"].startswith("valid_")]
    assert [f["name"] for f in checks] == ["valid_7", "valid_9"] and checks[0]["label"].startswith("CHL-005")
    out = await tools_maintenance.maintenance_order_action(CTX, order="CM-10-26-140", action="delete")
    assert out["form"]["danger"] and out["form"]["kind"] == "maintenance_order.delete"
    out = await tools_maintenance.maintenance_order_action(CTX, order="CM-10-26-140", action="reject", reason="Duplicate of CM-10-26-139")
    assert out["maintenance_order"]["action"] == "rejected"
    rejected = [c["body"] for c in mo_backend.calls_to("maintenance_order/submit") if c["body"]["submit_type"] == "REJECTED"]
    assert rejected == [{"submit_type": "REJECTED", "id": 44, "reject_reason": "Duplicate of CM-10-26-139"}]


async def test_approve_resends_the_current_values(mo_backend):
    await tools_maintenance.maintenance_order_action(CTX, order="CM-10-26-140", action="approve", technicians=["Lim"])
    (call,) = mo_backend.calls_to("maintenance_order/submit")
    body = call["body"]
    # submit clears what it is not sent: priority and planned end go back with the change
    assert body["submit_type"] == "APPROVED" and body["priority_id"] == 1 and body["planned_end"] == DETAIL["planned_end"]
    assert body["technicians"] == "lim_kokwai" and body["severity_id"] == 2


async def test_a_submitted_form_runs_as_the_person(mo_backend):
    result = await forms.submit("maintenance_order.close", {"valid_7": True, "valid_9": False}, {"order_id": 44, "action": "close"}, STATE)
    assert result["ok"] and result["maintenance_order"]["action"] == "closed"
    (call,) = mo_backend.calls_to("maintenance_order_work_report/close_work_report")
    assert call["origin"] is None  # no agent origin: the person submitted it
    issues = {(i["alarm_code_id"], i["instruction_type_id"]): i["is_valid"] for i in __import__("json").loads(call["body"]["issues"])}
    assert issues == {(7, 1): True, (7, 2): True, (9, 1): False, (9, 2): False}
    bad = await forms.submit("maintenance_order.reject", {"reason": ""}, {"order_id": 44}, STATE)
    assert not bad["ok"] and "reason" in bad["message"]
    unknown = await forms.submit("nope", {}, {}, STATE)
    assert not unknown["ok"]


async def test_simulation_forms_queue_the_users_own_job(mo_backend):
    out = await simforms.show_scenario_form(CTX, change="fault", fault="ct_fouling", asset="ct-2")
    names = [f["name"] for f in out["form"]["fields"]]
    assert "asset_ct_fouling" in names and "intensity_ct_fouling" in names
    preset = {f["name"]: f.get("value") for f in out["form"]["fields"]}
    assert preset["asset_ct_fouling"] == "CT_02" and preset["change"] == "fault"
    values = {"title": "Tower fouling what-if", "duration_hours": 6, "change": "fault", "fault": "ct_fouling", "asset_ct_fouling": "CT_02",
              "intensity_ct_fouling": 0.8, "at_minute": 30}  # fmt: skip
    result = await forms.submit("simulation.scenario", values, {}, STATE)
    assert result["ok"] and result["simulation"]["job_id"] == 301 and not result["simulation"]["approval_required"]
    (call,) = mo_backend.calls_to("simulation/submit")
    assert call["origin"] is None
    spec = call["body"]["spec"]
    assert spec["duration_min"] == 360 and spec["variants"][1]["actions"] == [
        {"at_min": 30.0, "kind": "fault", "asset": "CT_02", "fault": "ct_fouling", "value": 0.8}
    ]


def test_scenario_and_production_specs():
    _, spec, _ = simforms.scenario_spec({"title": "Warmer CHW", "duration_hours": 4, "change": "setpoint", "parameter": "chws_sp", "value": 7.2})
    acts = spec["variants"][1]["actions"]
    assert [a["param"] for a in acts] == ["chws_reset_mode", "chws_sp"] and acts[1]["value"] == 7.2  # the mode goes Manual first
    with pytest.raises(ToolError):
        simforms.scenario_spec({"title": "Too warm", "duration_hours": 4, "change": "setpoint", "parameter": "chws_sp", "value": 9})
    _, spec, _ = simforms.scenario_spec({"title": "Baseline", "duration_hours": 2, "change": "none"})
    assert [v["name"] for v in spec["variants"]] == ["baseline"]
    _, plan, _ = simforms.production_spec(
        {"title": "More starts", "horizon_days": 30, "starts_per_day": 5.5, "dispatch": "CR", "mcu40_share_pct": 70, "tool_down": "ETCH_02",
         "tool_down_day": 2, "tool_down_hours": 24}
    )  # fmt: skip
    alt = plan["variants"][1]
    assert alt["plan"] == {"starts_per_day": 5.5, "mix": {"MCU40": 0.7, "WSOC40": 0.3}, "dispatch": "CR"}
    assert alt["actions"][0]["kind"] == "tool_down" and alt["actions"][0]["at_day"] == 2.0


def _event(author: str, inv: str, **kw) -> Event:
    return Event(author=author, invocation_id=inv, **kw)


def test_forms_stream_and_history():
    form = forms.new_form("maintenance_order.reject", "Reject CM-10-26-140", [forms.field("reason", "Reason", "textarea", required=True)])
    reply = forms.form_reply(form, ["reason"])
    assert stream.result_items("maintenance_order_action", reply, True) == [("form", form)]
    assert stream.summarise("maintenance_order_action", reply, True, None) == "Form: Reject CM-10-26-140"
    card = {"id": 44, "name": "CM-10-26-140", "action": "rejected", "status": "REJECTED"}
    note = forms.result_note("maintenance_order.reject", form["form_id"], {"ok": True, "message": "Rejected.", "maintenance_order": card})
    content = stream.build_user_content("Submitted: reject", {"page": "maintenance-order"}, None, note)
    assert stream.user_text(content) == "Submitted: reject"
    events = [
        _event("user", "i1", content=types.UserContent(parts=[types.Part(text="Reject CM-10-26-140")])),
        _event("maintenance_agent", "i1", content=types.ModelContent(parts=[types.Part(function_call=types.FunctionCall(id="c1", name="maintenance_order_action", args={}))])),
        _event("maintenance_agent", "i1", content=types.UserContent(parts=[types.Part(function_response=types.FunctionResponse(id="c1", name="maintenance_order_action", response=reply))])),
        _event("maintenance_agent", "i1", content=types.ModelContent(parts=[types.Part(text="Fill in the reason below.")])),
        _event("user", "i2", content=content),
        _event("maintenance_agent", "i2", content=types.ModelContent(parts=[types.Part(text="CM-10-26-140 is rejected.")])),
    ]
    history = stream.history_messages(events)
    assert [m["role"] for m in history] == ["user", "assistant", "user", "assistant"]
    assert history[1]["forms"][0]["form_id"] == form["form_id"]
    assert history[1]["forms"][0]["submitted"] == {"ok": True, "message": "Rejected."}
    assert history[2]["text"] == "Submitted: reject"
    assert history[3]["orders"] == [card]
