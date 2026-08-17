"""Twin agent -> McpToolset -> afdd-twin MCP server process -> backend: propose a simulation.

The MCP server is a real process; the header provider sends the user's session
as a bearer token plus the shared secret and the agent session id. The proposal
reaches the (fake) backend as the user, marked agent-origin, and the SSE stream
carries a `simulation` event for the approval card.
"""

from __future__ import annotations

from types import SimpleNamespace

from google.adk.sessions import InMemorySessionService

from afdd_ai.agent.agents import AGENT_NAMES, INSIGHT, TWIN, build_root_agent, mcp_header_provider
from afdd_ai.agent.runner import AgentService
from afdd_ai.common.config import get_settings

from .support import TOKEN, Call, ScriptedLlm, free_port, request_text
from .test_agent_flow import IDENTITY, ROOT, first, make_service, names, run_turn

SPEC = {
    "start": None,
    "duration_min": 240,
    "initial": "live",
    "stochastic": False,
    "seed": 0,
    "weather": {"dry_bulb_offset": 0.0, "wet_bulb_offset": 0.0},
    "load_scale": 1.0,
    "actions": [],
    "variants": [
        {"name": "baseline", "actions": []},
        {
            "name": "chws_7",
            "actions": [
                {"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1},
                {"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7.0},
            ],
        },
    ],
}


async def test_twin_agent_proposes_simulation_through_mcp(fake_backend, cleanup_usage):
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": TWIN})],
            TWIN: [
                Call("get_plant_state", {}),
                Call("propose_simulation", {"title": "CHW supply 7.0 degC for 4 h", "rationale": "Recommendation #7 predicts a 2.9 % saving.", "spec": SPEC}),
                "Proposed simulation; it waits for your approval.",
            ],
        }
    )
    try:
        events = await run_turn(svc, "What if we raise the CHW supply set point to 7 degC?")
        sid = first(events, "session")["session_id"]
        history = await svc.get_history(IDENTITY, sid)
    finally:
        await svc.close()

    seq = names(events)
    assert "error" not in seq, events
    state = first(events, "tool_result", name="get_plant_state")
    assert state["ok"] is True
    proposal = first(events, "tool_result", name="propose_simulation")
    assert proposal["ok"] is True and proposal["summary"].startswith("Simulation #")
    sim = first(events, "simulation")
    assert sim["status"] == "PROPOSED" and sim["approval_required"] is True
    assert sim["title"] == "CHW supply 7.0 degC for 4 h" and sim["rationale"].startswith("Recommendation #7")
    assert "240 min" in sim["spec_summary"] and "chws_7 (2 action(s))" in sim["spec_summary"]
    assert seq.index("simulation") < seq.index("message")

    (call,) = fake_backend.calls_to("simulation/propose")
    assert call["token"] == TOKEN and call["origin"] == "agent"
    assert call["body"]["agent_session_id"] == sid and call["body"]["spec"]["variants"][1]["name"] == "chws_7"
    assert sim["job_id"] == max(fake_backend.jobs)
    # The stored conversation rebuilds the approval card and the tool calls.
    reply = history["messages"][-1]
    assert reply["role"] == "assistant" and reply["agent"] == TWIN and reply["simulations"] == [sim]
    assert [t["name"] for t in reply["tool_calls"]] == ["get_plant_state", "propose_simulation"] and all(t["ok"] for t in reply["tool_calls"])
    # The plant state reached the model with mode labels; the token never did.
    assert "Manual" in request_text(models[TWIN].requests[1])
    for llm in models.values():
        for req in llm.requests:
            assert TOKEN not in request_text(req)


async def test_mcp_tool_error_reaches_the_model(cleanup_usage):
    bad = dict(SPEC, duration_min=5)
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": TWIN})],
            TWIN: [Call("propose_simulation", {"title": "Too short", "rationale": "test", "spec": bad}), "The spec was rejected."],
        }
    )
    try:
        events = await run_turn(svc, "Simulate 5 minutes")
    finally:
        await svc.close()
    result = first(events, "tool_result", name="propose_simulation")
    assert result["ok"] is False and "duration_min" in result["summary"]
    assert "simulation" not in names(events)
    assert "duration_min must be a number from 10 to 2880" in request_text(models[TWIN].requests[1])


async def test_mcp_server_down_keeps_other_tools(fake_backend, cleanup_usage):
    """An unreachable afdd-db only removes its tools; the insight agent still answers with backend tools."""
    models = {name: ScriptedLlm(model=f"fake-{name}") for name in AGENT_NAMES}
    models[ROOT].script.append(Call("transfer_to_agent", {"agent_name": INSIGHT}))
    models[INSIGHT].script.extend([Call("list_assets", {}), "4 units are visible."])
    dead = f"http://127.0.0.1:{free_port()}/mcp"
    root = build_root_agent(settings=get_settings(), models=models, mcp_db_url=dead, mcp_twin_url=dead)
    svc = AgentService(get_settings(), root_agent=root, session_service=InMemorySessionService())
    try:
        events = await run_turn(svc, "Which units are there?")
    finally:
        await svc.close()
    assert "error" not in names(events), events
    assert first(events, "tool_result", name="list_assets")["ok"] is True
    assert first(events, "message")["text"] == "4 units are visible."
    tools = set(models[INSIGHT].requests[0].tools_dict)
    assert "list_assets" in tools and "query_parameter_history" not in tools


def test_mcp_headers_carry_identity_not_to_the_model():
    provide = mcp_header_provider(get_settings())
    ctx = SimpleNamespace(state=IDENTITY.state_delta(), session=SimpleNamespace(id="sess-1"))
    assert provide(ctx) == {
        "X-Agent-Secret": get_settings().agent_secret,
        "Authorization": f"Bearer {TOKEN}",
        "X-AFDD-User-Id": str(IDENTITY.user_id),
        "X-AFDD-Agent-Session": "sess-1",
    }
    assert provide(None) == {"X-Agent-Secret": get_settings().agent_secret}
