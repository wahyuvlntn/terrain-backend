"""afdd-twin: backend calls as the user, proposal only (never approval), status / results, climatology."""

from __future__ import annotations

from tests.agent.support import TOKEN

from .conftest import mcp_session, text_of

SPEC = {
    "duration_min": 120,
    "initial": "live",
    "variants": [{"name": "baseline", "actions": []}, {"name": "recommended", "actions": [{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 3.0}]}],
}


async def call(servers, token: str, tool: str, args: dict, headers: dict | None = None):
    async with mcp_session(servers["twin"].url, token, headers=headers) as s:
        return await s.call_tool(tool, args)


async def test_plant_state_and_catalog(servers, fake_backend):
    res = await call(servers, TOKEN, "get_plant_state", {})
    assert not res.is_error, text_of(res)
    data = res.structured_content
    assert data["plant_controller"]["points"]["chws_reset_mode"] == "Manual"
    assert {u["alias"] for u in data["units"]} == {"CH_01", "CH_02", "CT_01"}
    assert data["units"][0]["values"]["run_mode"] == "Auto"
    assert data["active_faults"][0]["kind"] == "ct_fouling"
    (overview,) = fake_backend.calls_to("digital_twin/getliveoverview")[-1:]
    assert overview["token"] == TOKEN and overview["origin"] == "agent" and "CHWS_01" in overview["body"]["aliases"]

    cat = await call(servers, TOKEN, "get_fault_catalog", {})
    assert cat.structured_content["faults"][0]["intensities"] == [0.95, 0.8, 0.65]


async def test_propose_status_results(servers, fake_backend):
    res = await call(
        servers,
        TOKEN,
        "propose_simulation",
        {"title": "Approach 3.0 K for 2 h", "rationale": "Optimiser suggests a closer approach.", "spec": SPEC},
        headers={"X-AFDD-Agent-Session": "session-abc"},
    )
    assert not res.is_error, text_of(res)
    prop = res.structured_content
    assert prop["status"] == "PROPOSED" and prop["approval_required"] is True
    assert prop["spec_summary"].startswith("120 min; from now; live initial state") and "recommended (1 action(s))" in prop["spec_summary"]
    job_id = prop["job_id"]
    call_ = fake_backend.calls_to("simulation/propose")[-1]
    assert call_["body"]["agent_session_id"] == "session-abc" and call_["origin"] == "agent"
    assert not fake_backend.calls_to("simulation/approve")

    status = (await call(servers, TOKEN, "get_simulation_status", {"job_id": job_id})).structured_content
    assert status["status"] == "PROPOSED" and "has not run" in status["meaning"]
    assert status["created_at"] == "2026-10-10T14:00:00+08:00"
    pending = (await call(servers, TOKEN, "get_simulation_results", {"job_id": job_id})).structured_content
    assert "No results" in pending["note"] and "summary" not in pending

    fake_backend.jobs[job_id].update(status="SUCCEEDED", summary={"success": True, "variants": {"baseline": {"energy_kwh": 2900.0}, "recommended": {"energy_kwh": 2850.0}}})
    done = (await call(servers, TOKEN, "get_simulation_results", {"job_id": job_id})).structured_content
    assert done["summary"]["variants"]["recommended"]["energy_kwh"] == 2850.0
    assert done["events"][0]["code"] == "CHL-008" and done["events_total"] == 1
    assert done["events"][0]["time"] == "2026-10-10T15:00:00+08:00"

    listed = (await call(servers, TOKEN, "list_simulations", {"limit": 5})).structured_content
    assert job_id in [j["job_id"] for j in listed["jobs"]]
    missing = await call(servers, TOKEN, "get_simulation_status", {"job_id": 999999})
    assert missing.is_error and "not found" in text_of(missing)


async def test_proposal_validation_and_permission(servers, fake_backend):
    before = len(fake_backend.calls_to("simulation/propose"))
    for spec, msg in (
        ({**SPEC, "duration_min": 3000}, "duration_min"),
        ({**SPEC, "variants": [{"name": f"v{i}", "actions": []} for i in range(4)]}, "at most 3 variants"),
        ({**SPEC, "kind": "doe"}, "Only scenario"),
        ({**SPEC, "actions": [{"kind": "control"}]}, "kind and asset"),
    ):
        res = await call(servers, TOKEN, "propose_simulation", {"title": "Bad spec", "rationale": "r", "spec": spec})
        assert res.is_error and msg in text_of(res)
    assert len(fake_backend.calls_to("simulation/propose")) == before

    denied = await call(servers, "tok-nosim", "propose_simulation", {"title": "No permission", "rationale": "r", "spec": SPEC})
    assert denied.is_error and "not allowed" in text_of(denied)


async def test_latest_recommendation_and_forecast(servers):
    rec = (await call(servers, TOKEN, "get_latest_recommendation", {})).structured_content
    assert rec["recommendation"]["id"] == 7 and rec["recommendation"]["recommended"]["setpoints"]["chws_sp"] == 7.0

    res = await call(servers, "tok-admin", "get_forecast_conditions", {"hours": 3})
    assert not res.is_error, text_of(res)
    data = res.structured_content
    assert len(data["hours"]) == 3 and "climatology" in data["method"]
    assert data["sources"]["wet_bulb_c"] in ("CHWS_01.wet_bulb", "CT.wet_bulb")
    first = data["hours"][0]
    assert first["time"].endswith("+08:00") and first["wet_bulb_c"]["p10"] <= first["wet_bulb_c"]["mean"] <= first["wet_bulb_c"]["p90"]
    assert "cooling_load_kw" in first and 1 <= first["chillers_needed"] <= 3

    scoped = await call(servers, "tok-plant", "get_forecast_conditions", {"hours": 1})
    assert not scoped.is_error and scoped.structured_content["sources"]["cooling_load_kw"] == "CHWS_01.cooling_load"
