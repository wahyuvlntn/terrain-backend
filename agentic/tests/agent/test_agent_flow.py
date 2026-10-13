"""The real ADK Runner and agent tree on scripted models: routing, tools, SSE, budget, limits."""

from __future__ import annotations

import asyncio
import dataclasses
import gc
import json
import sys
import types as pytypes
from typing import Any

import anyio
import pytest
from google.adk.sessions import InMemorySessionService

from afdd_ai.agent import app as agent_app, context
from afdd_ai.agent import budget
from afdd_ai.agent.agents import AGENT_NAMES, APP_NAME, DEEP, INSIGHT, ROOT, TWIN, build_root_agent
from afdd_ai.agent.runner import AgentService, ChatRequest, ConversationBusy, ConversationNotFound
from afdd_ai.agent.state import Identity, parse_permissions
from afdd_ai.common import db, gemini
from afdd_ai.common.config import get_settings
from afdd_ai.common.gemini import sampling_temperature, thinking_config

from .support import TEST_USER_ID, TOKEN, Call, FakeError, ScriptedLlm, Slow, parse_sse, request_text

IDENTITY = Identity(
    user_id=TEST_USER_ID,
    username="sarah_lim_test",
    name="Sarah Lim",
    session_token=TOKEN,
    permissions=parse_permissions("aiassistant.read,aiinsights.read,simulation.read,digitaltwin.read"),
)


def make_service(scripts: dict[str, list[Any]], *, settings=None, session_service=None) -> tuple[AgentService, dict[str, ScriptedLlm]]:
    models = {name: ScriptedLlm(model=f"fake-{name}", script=list(scripts.get(name, []))) for name in AGENT_NAMES}
    s = settings or get_settings()
    root = build_root_agent(settings=s, models=models)
    svc = AgentService(s, root_agent=root, session_service=session_service or InMemorySessionService())
    return svc, models


async def run_turn(svc: AgentService, message: str, session_id: str | None = None, context: dict[str, Any] | None = None) -> list[tuple[str, Any]]:
    turn = await svc.prepare_chat(IDENTITY, ChatRequest(message=message, session_id=session_id, context=context))
    return [item async for item in turn.events()]


def function_response(request: Any, name: str) -> dict[str, Any]:
    """The function response `name` as the model received it."""
    for content in request.contents:
        for part in content.parts or []:
            if part.function_response and part.function_response.name == name:
                return part.function_response.response
    raise AssertionError(f"no function response {name}")


def names(events: list[tuple[str, Any]]) -> list[str]:
    return [e for e, _ in events]


def first(events: list[tuple[str, Any]], kind: str, **match: Any) -> Any:
    for e, d in events:
        if e == kind and all(d.get(k) == v for k, v in match.items()):
            return d
    raise AssertionError(f"no {kind} {match} in {events}")


async def test_routing_tools_token_and_sse(fake_backend, cleanup_usage):
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: [Call("get_asset_parameters", {"asset": "ch-1"}), "CH_01 leaves at 6.50 °C."],
        }
    )
    try:
        events = await run_turn(svc, "How is chiller 1?", context={"page": "digital-twin", "asset_alias": "CH_01"})
    finally:
        await svc.close()

    seq = names(events)
    assert seq[0] == "session" and seq[-2:] == ["usage", "done"]
    assert seq.index("agent") < seq.index("tool_call") < seq.index("tool_result") < seq.index("message")
    assert "text" in seq and "error" not in seq
    assert first(events, "agent") == {"name": INSIGHT}
    call = first(events, "tool_call", name="get_asset_parameters")
    assert call["args"] == {"asset": "ch-1"} and call["agent"] == INSIGHT
    result = first(events, "tool_result", name="get_asset_parameters")
    assert result["ok"] is True and result["id"] == call["id"] and result["summary"] == "4 parameters"
    deltas = "".join(d["delta"] for e, d in events if e == "text")
    assert deltas == "CH_01 leaves at 6.50 °C." == first(events, "message")["text"]
    usage = first(events, "usage")
    assert usage["input_tokens"] == 3 * 120 and usage["output_tokens"] == 3 * 30
    assert usage["cost_usd"] > 0 and usage["limit_usd"] in (get_settings().user_daily_budget_usd, round(get_settings().daily_budget_usd - get_settings().analytics_reserve_usd, 4))
    assert usage["today_usd"] is not None

    # The backend was called as the user, marked as agent-origin.
    (backend_call,) = fake_backend.calls_to("digital_twin/getassetparameters")
    assert backend_call["token"] == TOKEN and backend_call["origin"] == "agent" and backend_call["body"] == {"asset_id": 1}
    # The model saw compact values, the UI context and the user's name, never the token.
    params = {p["alias"]: p for p in function_response(models[INSIGHT].requests[1], "get_asset_parameters")["parameters"]}
    assert params["chwst"]["value"] == 6.5 and params["chwst"]["warning"] == "..7.5"
    assert params["run_mode"]["value"] == "Auto" and params["run_mode"]["writable"] is True
    assert "[UI context] page=digital-twin; selected asset=CH_01" in request_text(models[ROOT].requests[0])
    assert "Sarah Lim" in str(models[ROOT].requests[0].config.system_instruction)
    for llm in models.values():
        for req in llm.requests:
            assert TOKEN not in request_text(req)
    # Tool set of the insight agent: backend tools, charts, RAG, filtered afdd-db tools, deep analyst.
    tools = set(models[INSIGHT].requests[0].tools_dict)
    assert {"get_dashboard_overview", "call_app_api", "chart_compare", "search_documents", "query_parameter_history", "get_energy_summary", DEEP} <= tools
    assert not {"list_alarm_events", "list_faults", "list_anomalies"} & tools
    # Thinking budgets from the settings (root 0, flash 512).
    s = get_settings()
    assert models[ROOT].requests[0].config.thinking_config == thinking_config(s.model_fast, s.thinking_budget_root)
    assert models[INSIGHT].requests[0].config.thinking_config == thinking_config(s.model_fast, s.thinking_budget_fast)
    assert models[ROOT].requests[0].config.temperature == sampling_temperature(s.model_fast, 0.2)


async def test_followup_turn_stays_with_specialist(cleanup_usage):
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: ["First answer.", "Second answer."],
        }
    )
    try:
        first_turn = await run_turn(svc, "Hello plant")
        sid = first(first_turn, "session")["session_id"]
        second_turn = await run_turn(svc, "And now?", session_id=sid)
        session = await svc.session_service.get_session(app_name="afdd_plant_assistant", user_id=IDENTITY.username, session_id=sid)
    finally:
        await svc.close()
    assert first(second_turn, "session") == {"session_id": sid, "title": "Hello plant"}
    assert first(second_turn, "message")["text"] == "Second answer."
    assert len(models[ROOT].requests) == 1
    assert session.state["title"] == "Hello plant" and session.state["owner"] == TEST_USER_ID
    assert not any(k.startswith("temp:") for k in session.state)


async def test_deep_analyst_runs_inline_and_chart_is_streamed(cleanup_usage):
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: [
                Call("get_parameter_trend", {"asset": "CH_01", "parameter": "chwst", "hours": 4}),
                Call(DEEP, {"request": "Analyse CH_01 chwst: mean 6.53 degC, 1 SPC violation."}),
                "Summary: CH_01 is stable.",
            ],
            DEEP: ["Deep finding: stable within limits."],
        }
    )
    try:
        events = await run_turn(svc, "Analyse chiller 1 leaving temperature")
    finally:
        await svc.close()
    chart = first(events, "chart")
    assert chart["title"].startswith("CH_01 CHW Leaving Temp") and len(chart["option"]["series"][0]["data"]) == 48
    assert chart["option"]["series"][0]["markLine"]["data"][0] == {"name": "Warning high", "yAxis": 7.5}
    # The model sees the trend statistics and a stub instead of the chart's data (agent/context.py).
    later = request_text(models[INSIGHT].requests[1])
    assert context.CHART_NOTE in later and "markLine" not in later
    deep = first(events, "tool_result", name=DEEP)
    assert deep["ok"] and deep["summary"].startswith("Deep finding")
    assert not any(e in ("text", "message") and "Deep finding" in json.dumps(d) for e, d in events)
    assert {"name": DEEP} not in [d for e, d in events if e == "agent"]
    assert first(events, "message")["text"] == "Summary: CH_01 is stable."
    # The analyst sees only its request (single turn, no history) and uses the pro thinking budget.
    deep_req = request_text(models[DEEP].requests[0])
    assert "1 SPC violation" in deep_req and "Analyse chiller 1 leaving temperature" not in deep_req
    s = get_settings()
    assert models[DEEP].requests[0].config.thinking_config == thinking_config(s.model_pro, s.thinking_budget_pro)


async def test_tool_errors_are_explained(fake_backend, cleanup_usage):
    def forbid(body, tok):
        raise FakeError(403, "Permission denied")

    fake_backend.handlers["digital_twin/getassetsummary"] = forbid
    svc, _ = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: [
                [Call("get_asset_summary", {"asset": "ZZ_99"}), Call("get_asset_summary", {"asset": "CH_02"}), Call("call_app_api", {"script": "maintenance_order/submit", "params": {}})],
                "Some data is unavailable.",
            ],
        }
    )
    try:
        events = await run_turn(svc, "Summaries please")
    finally:
        await svc.close()
        fake_backend.handlers.pop("digital_twin/getassetsummary", None)
    results = [d for e, d in events if e == "tool_result"]
    assert [r["ok"] for r in results] == [False, False, False]
    assert "No visible equipment matches 'ZZ_99'" in results[0]["summary"]
    assert "not allowed to access the summary of CH_02" in results[1]["summary"]
    assert "not an allowed read-only script" in results[2]["summary"]
    assert not fake_backend.calls_to("maintenance_order/submit")


async def test_rag_tool_returns_citations_with_user_scope(fake_backend, monkeypatch, cleanup_usage):
    seen: dict[str, Any] = {}

    async def fake_search(query, k=6, filters=None, allowed_location_ids=None, *, component="rag_search", query_embedding=None, ro=False):
        seen.update(query=query, k=k, filters=filters, allowed=allowed_location_ids, component=component, ro=ro)
        return [
            {"chunk_id": 1, "document_id": 11, "title": "BAC S3E manual", "doc_type": "manual", "page": 12, "heading": "Fill cleaning", "content": "Clean the fill when the approach rises...", "score": 0.88, "unit": None, "asset_type": "Cooling Tower", "doc_date": None},
            {"chunk_id": 2, "document_id": 12, "title": "CM-09-26-004 report", "doc_type": "maintenance_report", "page": 1, "heading": "Work notes", "content": "Fill brushed.", "score": 0.71, "unit": "CT_02", "asset_type": "Cooling Tower", "doc_date": "2026-09-14"},
        ]

    module = pytypes.ModuleType("afdd_ai.rag.search")
    module.search = fake_search  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "afdd_ai.rag.search", module)
    svc, models = make_service(
        {
            ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: [Call("search_documents", {"query": "tower fouling", "doc_type": "manual", "unit": "ct_02"}), "Clean the fill [1]."],
        }
    )
    try:
        events = await run_turn(svc, "How do I fix tower fouling?")
    finally:
        await svc.close()
    cites = first(events, "citations")["items"]
    assert [c["document_id"] for c in cites] == [11, 12] and cites[0]["page"] == 12 and cites[0]["title"] == "BAC S3E manual"
    assert seen["filters"] == {"site_code": get_settings().site_code, "doc_type": "manual", "unit": "CT_02"}
    assert seen["allowed"] == sorted(fake_backend.users[TOKEN]["assignedLocations"])
    assert seen["component"] == "agent_rag_search" and seen["ro"] is False
    seen_by_model = function_response(models[INSIGHT].requests[1], "search_documents")
    assert [r["ref"] for r in seen_by_model["results"]] == [1, 2] and seen_by_model["results"][1]["doc_date"] == "2026-09-14"


async def test_budget_refused_before_the_turn(monkeypatch):
    async def spent() -> float:
        return 999.0

    monkeypatch.setattr(gemini, "spent_today_usd", spent)
    svc, models = make_service({ROOT: ["never"]})
    try:
        events = await run_turn(svc, "Hi")
        listed = await svc.list_sessions(IDENTITY)
    finally:
        await svc.close()
    assert names(events) == ["error", "done"]
    assert "budget is used up" in events[0][1]["message"]
    assert listed == [] and models[ROOT].requests == []


async def test_budget_refused_by_model_callback(monkeypatch):
    calls = {"n": 0}

    async def refusal(user_id=None):  # noqa: ANN001
        assert user_id == TEST_USER_ID  # the per-user allowance is checked for the turn's user
        calls["n"] += 1
        return None if calls["n"] == 1 else (budget.BUDGET_EXCEEDED, budget.budget_message(0.5, 0.5))

    monkeypatch.setattr(budget, "budget_refusal", refusal)
    svc, models = make_service({ROOT: ["never"]})
    try:
        events = await run_turn(svc, "Hi")
    finally:
        await svc.close()
    assert names(events) == ["session", "agent", "error", "usage", "done"]
    assert "budget is used up" in first(events, "error")["message"]
    assert models[ROOT].requests == []


async def test_not_configured_refuses(monkeypatch):
    monkeypatch.setenv("GOOGLE_API_KEY", "")
    get_settings.cache_clear()
    try:
        svc, models = make_service({ROOT: ["never"]})
        try:
            events = await run_turn(svc, "Hi")
        finally:
            await svc.close()
    finally:
        monkeypatch.undo()
        get_settings.cache_clear()
    assert names(events) == ["error", "done"] and events[0][1]["message"] == budget.MSG_NOT_CONFIGURED


async def test_llm_call_limit_ends_turn_cleanly(cleanup_usage):
    settings = dataclasses.replace(get_settings(), max_llm_calls=2)
    svc, models = make_service(
        {ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})], INSIGHT: [Call("list_assets", {}), Call("list_assets", {}), "late"]},
        settings=settings,
    )
    try:
        events = await run_turn(svc, "Loop")
    finally:
        await svc.close()
    assert "limit of model calls" in first(events, "error")["message"]
    assert names(events)[-2:] == ["usage", "done"]


async def test_twin_agent_routing(cleanup_usage):
    svc, models = make_service({ROOT: [Call("transfer_to_agent", {"agent_name": TWIN})], TWIN: ["I can propose a simulation."]})
    try:
        events = await run_turn(svc, "What if we raise the CHW set point tomorrow?")
    finally:
        await svc.close()
    assert first(events, "agent") == {"name": TWIN}
    assert first(events, "message") == {"text": "I can propose a simulation.", "agent": TWIN}
    twin_tools = set(models[TWIN].requests[0].tools_dict)
    assert {"get_plant_state", "propose_simulation", "get_simulation_status", "get_simulation_results", "search_documents"} <= twin_tools


async def test_sse_wrapper_pings_and_aborts_on_disconnect(fake_backend, monkeypatch, cleanup_usage):
    import time as _time

    def slow(body, tok):
        _time.sleep(0.6)
        return []

    fake_backend.handlers["digital_twin/getassets"] = slow
    monkeypatch.setattr(agent_app, "PING_INTERVAL_S", 0.1)
    svc, _ = make_service({ROOT: [Call("transfer_to_agent", {"agent_name": INSIGHT})], INSIGHT: [Call("list_assets", {}), "done"]})
    try:
        turn = await svc.prepare_chat(IDENTITY, ChatRequest(message="slow one"))
        frames = [f async for f in agent_app._sse(turn)]
        parsed = parse_sse("".join(frames))
        assert ("ping", None) in parsed and parsed[-1] == ("done", {})

        # Client disconnect: the response generator is closed after the first frame.
        turn2 = await svc.prepare_chat(IDENTITY, ChatRequest(message="slow two", session_id=turn.session_id))
        gen = agent_app._sse(turn2)
        assert (await gen.__anext__()).startswith("event: session")
        await gen.aclose()
        assert turn2.abort.is_set()
        assert turn.session_id not in svc._locks or not svc._locks[turn.session_id].locked()
    finally:
        fake_backend.handlers.pop("digital_twin/getassets", None)
        fake_backend._install_defaults()
        await svc.close()


@pytest.mark.parametrize("disconnect", ["close", "cancel"])
async def test_client_disconnect_lets_the_model_call_finish_and_records_its_usage(cleanup_usage, disconnect):
    started = asyncio.Event()
    svc, models = make_service({ROOT: [Slow(0.8, Call("transfer_to_agent", {"agent_name": INSIGHT}), started)], INSIGHT: ["never"]})
    try:
        turn = await svc.prepare_chat(IDENTITY, ChatRequest(message="slow question"))
        if disconnect == "close":  # the response generator is closed
            gen = agent_app._sse(turn)
            assert (await gen.__anext__()).startswith("event: session")
            await asyncio.wait_for(started.wait(), timeout=5)
            await gen.aclose()
        else:  # Starlette cancels the response's task group (level-triggered anyio cancellation)
            frames: list[str] = []

            async def respond() -> None:
                async for frame in agent_app._sse(turn):
                    frames.append(frame)

            async with anyio.create_task_group() as tg:
                tg.start_soon(respond)
                await asyncio.wait_for(started.wait(), timeout=5)
                tg.cancel_scope.cancel()
            assert frames and frames[0].startswith("event: session")
        assert turn.abort.is_set()
        rows = await db.fetch_all(
            "SELECT component, model, input_tokens, output_tokens, user_id FROM analytics.llm_usage WHERE session_id = %s", (turn.session_id,)
        )
        # the call Gemini bills anyway is recorded; no further call was made after the abort
        assert rows == [{"component": "agent", "model": get_settings().model_fast, "input_tokens": 120, "output_tokens": 30, "user_id": TEST_USER_ID}]
        assert len(models[ROOT].requests) == 1 and models[INSIGHT].requests == []
        assert turn.session_id not in svc._locks and turn.session_id not in budget._TURNS
    finally:
        await svc.close()


async def test_no_model_call_starts_after_the_client_left():
    abort = asyncio.Event()
    guard = budget.track_turn("pytest-session", abort)
    ctx = pytypes.SimpleNamespace(session=pytypes.SimpleNamespace(id="pytest-session"), state={}, agent_name=ROOT)
    try:
        abort.set()
        resp = await budget.before_model(ctx, None)  # type: ignore[arg-type]
        assert resp is not None and resp.error_code == budget.ABORTED and guard.in_flight == 0 and guard.idle.is_set()
    finally:
        budget.untrack_turn("pytest-session", guard)
    assert "pytest-session" not in budget._TURNS


async def test_budget_reserve_and_per_user_allowance(monkeypatch):
    spend = {"total": 0.0, "user": 0.0}

    async def spent(user_id=None):  # noqa: ANN001
        if user_id is None:
            return spend["total"]
        return spend["user"] if user_id == TEST_USER_ID else 0.0

    monkeypatch.setattr(gemini, "spent_today_usd", spent)
    monkeypatch.setenv("AI_DAILY_BUDGET_USD", "0.50")
    monkeypatch.setenv("AI_ANALYTICS_RESERVE_USD", "0.10")
    monkeypatch.setenv("AI_USER_DAILY_BUDGET_USD", "0.20")
    get_settings.cache_clear()
    try:
        spend.update(total=0.39, user=0.19)
        assert await budget.budget_refusal(TEST_USER_ID) is None
        # agent turns stop at the daily budget minus the analytics reserve
        spend.update(total=0.40)
        code, msg = await budget.budget_refusal(TEST_USER_ID)
        assert code == budget.BUDGET_EXCEEDED and "US$0.40 of US$0.40" in msg and "kept for the automatic plant analytics" in msg
        spend.update(total=0.50)
        assert "budget is used up (US$0.50 of US$0.50)" in (await budget.budget_refusal(TEST_USER_ID))[1]
        # each user's own allowance; other users are not affected
        spend.update(total=0.10, user=0.20)
        code, msg = await budget.budget_refusal(TEST_USER_ID)
        assert code == budget.USER_BUDGET_EXCEEDED and "personal LEVO allowance for today (US$0.20 of US$0.20)" in msg
        assert await budget.budget_refusal(TEST_USER_ID + 1) is None and await budget.budget_refusal(None) is None
        svc, models = make_service({ROOT: ["never"]})
        try:
            events = await run_turn(svc, "Hi")
        finally:
            await svc.close()
        assert names(events) == ["error", "done"] and "personal LEVO allowance" in events[0][1]["message"]
        assert models[ROOT].requests == []
    finally:
        monkeypatch.undo()
        get_settings.cache_clear()


async def test_budget_zero_means_no_limit(monkeypatch):
    spend = {"total": 100.0, "user": 100.0}
    recorded: list[tuple[str, int | None]] = []

    async def spent(user_id=None):  # noqa: ANN001
        return spend["total"] if user_id is None else spend["user"]

    async def record(component, usage, *, user_id=None, session_id=None):  # noqa: ANN001
        recorded.append((component, user_id))
        return usage.cost_usd

    async def turn() -> list[tuple[str, Any]]:
        svc, _ = make_service({ROOT: ["Hello."]})
        try:
            return await run_turn(svc, "Hi")
        finally:
            await svc.close()

    monkeypatch.setattr(gemini, "spent_today_usd", spent)
    monkeypatch.setattr(gemini, "record_usage", record)
    monkeypatch.setenv("AI_DAILY_BUDGET_USD", "0")
    monkeypatch.setenv("AI_ANALYTICS_RESERVE_USD", "0.10")  # irrelevant without a daily budget
    monkeypatch.setenv("AI_USER_DAILY_BUDGET_USD", "0")
    get_settings.cache_clear()
    try:
        assert await budget.budget_refusal(TEST_USER_ID) is None
        events = await turn()
        assert "error" not in names(events) and first(events, "message")["text"] == "Hello."
        usage = first(events, "usage")
        assert usage["limit_usd"] == 0 and usage["today_usd"] == 100.0  # the UI shows only the spend
        assert recorded == [(budget.COMPONENT, TEST_USER_ID)]  # usage is still recorded
        # the per-user allowance alone still applies, and is the reported limit
        monkeypatch.setenv("AI_USER_DAILY_BUDGET_USD", "0.20")
        get_settings.cache_clear()
        assert (await budget.budget_refusal(TEST_USER_ID))[0] == budget.USER_BUDGET_EXCEEDED
        spend.update(user=0.05)
        assert await budget.budget_refusal(TEST_USER_ID) is None
        assert first(await turn(), "usage")["limit_usd"] == 0.20
        # a positive daily budget still refuses
        monkeypatch.setenv("AI_DAILY_BUDGET_USD", "0.50")
        get_settings.cache_clear()
        assert (await budget.budget_refusal(TEST_USER_ID))[0] == budget.BUDGET_EXCEEDED
    finally:
        monkeypatch.undo()
        get_settings.cache_clear()


def test_budget_settings_defaults(monkeypatch):
    for key in ("AI_USER_DAILY_BUDGET_USD", "AI_ANALYTICS_RESERVE_USD", "AGENT_ALLOWED_HOSTS"):
        monkeypatch.delenv(key, raising=False)
    get_settings.cache_clear()
    try:
        s = get_settings()
        assert s.user_daily_budget_usd == 0.20 and s.analytics_reserve_usd == 0.10
        assert s.agent_allowed_hosts == ("ai-agent", "localhost", "127.0.0.1")
        monkeypatch.setenv("AGENT_ALLOWED_HOSTS", " agent.internal , localhost ,")
        get_settings.cache_clear()
        assert get_settings().agent_allowed_hosts == ("agent.internal", "localhost")
    finally:
        monkeypatch.undo()
        get_settings.cache_clear()


async def test_a_reused_username_does_not_inherit_conversations():
    svc, _ = make_service({})
    try:
        session = await svc.session_service.create_session(app_name=APP_NAME, user_id=IDENTITY.username, state={"title": "Old chat", "owner": TEST_USER_ID})
        newcomer = dataclasses.replace(IDENTITY, user_id=TEST_USER_ID + 500)  # same username, a new account
        assert await svc.list_sessions(newcomer) == []
        with pytest.raises(ConversationNotFound):
            await svc.get_history(newcomer, session.id)
        with pytest.raises(ConversationNotFound):
            await svc.delete_session(newcomer, session.id)
        with pytest.raises(ConversationNotFound):
            await svc.prepare_chat(newcomer, ChatRequest(message="hi", session_id=session.id))
        assert [s["id"] for s in await svc.list_sessions(IDENTITY)] == [session.id]
        assert (await svc.get_history(IDENTITY, session.id))["title"] == "Old chat"
        # a conversation without an owner belongs to nobody
        orphan = await svc.session_service.create_session(app_name=APP_NAME, user_id=IDENTITY.username, state={"title": "No owner"})
        with pytest.raises(ConversationNotFound):
            await svc.get_history(IDENTITY, orphan.id)
    finally:
        await svc.close()


async def test_one_turn_per_conversation_from_prepare_until_it_ends(monkeypatch, cleanup_usage):
    svc, _ = make_service({ROOT: ["First.", "Second."]})
    try:
        first_turn = await svc.prepare_chat(IDENTITY, ChatRequest(message="One"))
        sid = first_turn.session_id
        # a concurrent request for the same conversation is refused (409) before the first turn even started
        with pytest.raises(ConversationBusy):
            await svc.prepare_chat(IDENTITY, ChatRequest(message="Two", session_id=sid))
        with pytest.raises(ConversationBusy):
            await svc.delete_session(IDENTITY, sid)
        # a refused turn in the same conversation does not drop the lock in use
        real_refusal = budget.budget_refusal

        async def refuse(user_id=None):  # noqa: ANN001
            return budget.BUDGET_EXCEEDED, budget.budget_message(0.5, 0.5)

        monkeypatch.setattr(budget, "budget_refusal", refuse)
        refused = await svc.prepare_chat(IDENTITY, ChatRequest(message="Three", session_id=sid))
        assert names([item async for item in refused.events()]) == ["session", "error", "done"]
        monkeypatch.setattr(budget, "budget_refusal", real_refusal)
        assert svc._locks[sid].locked()
        with pytest.raises(ConversationBusy):
            await svc.prepare_chat(IDENTITY, ChatRequest(message="Four", session_id=sid))
        assert first([item async for item in first_turn.events()], "message")["text"] == "First."
        assert sid not in svc._locks  # released when the turn ended
        second_turn = await svc.prepare_chat(IDENTITY, ChatRequest(message="Five", session_id=sid))
        assert first([item async for item in second_turn.events()], "message")["text"] == "Second."
        # a prepared turn whose stream never starts releases the conversation when it is collected
        orphan = await svc.prepare_chat(IDENTITY, ChatRequest(message="Six", session_id=sid))
        del orphan
        gc.collect()
        assert not svc._locks[sid].locked()
        await svc.delete_session(IDENTITY, sid)
        assert sid not in svc._locks
    finally:
        await svc.close()
