"""HTTP API with ADK's DatabaseSessionService on PostgreSQL (scratch database, schema `agent`).

The scratch database is owned by afdd_ai and dropped afterwards; the service is
built exactly as in production (`AgentService` from settings with
`search_path=agent`), only the models are scripted.
"""

from __future__ import annotations

import dataclasses
import json
from collections.abc import AsyncIterator, Iterator
from typing import Any

import asyncpg
import httpx
import pytest

from afdd_ai.agent import app as agent_app
from afdd_ai.agent.agents import AGENT_NAMES, DEEP, INSIGHT, ROOT, build_root_agent
from afdd_ai.agent.runner import AgentService
from afdd_ai.common.config import get_settings

from .support import AI_ROLE_URL, TEST_SECRET, TEST_USER_ID, TOKEN, Call, ScriptedLlm, create_scratch_database, drop_scratch_database, parse_sse

HEADERS = {
    "X-Agent-Secret": TEST_SECRET,
    "X-AFDD-User-Id": str(TEST_USER_ID),
    "X-AFDD-Username": "sarah_lim_test",
    "X-AFDD-User-Name": "Sarah Lim",
    "X-AFDD-Session": TOKEN,
    "X-AFDD-Permissions": "aiassistant.read,aiinsights.read,digitaltwin.read",
}


@pytest.fixture(scope="module")
def scratch_db() -> Iterator[str]:
    try:
        name = create_scratch_database()
    except Exception as err:  # noqa: BLE001 - no admin access: skip, do not touch the live database
        pytest.skip(f"cannot create a scratch database: {err}")
    yield name
    drop_scratch_database(name)


@pytest.fixture
async def client(scratch_db: str, cleanup_usage) -> AsyncIterator[tuple[httpx.AsyncClient, dict[str, ScriptedLlm], AgentService]]:
    url = AI_ROLE_URL.format(db=scratch_db).replace("postgresql://", "postgresql+asyncpg://")
    settings = dataclasses.replace(get_settings(), agent_session_db_url=url)
    models = {name: ScriptedLlm(model=f"fake-{name}") for name in AGENT_NAMES}
    svc = AgentService(settings, root_agent=build_root_agent(settings=settings, models=models))
    await svc.prepare()
    agent_app.set_service(svc)
    transport = httpx.ASGITransport(app=agent_app.app)
    async with httpx.AsyncClient(transport=transport, base_url="http://ai-agent") as c:
        yield c, models, svc
    agent_app.set_service(None)
    await svc.close()


async def chat(c: httpx.AsyncClient, body: dict[str, Any], headers: dict[str, str] | None = None) -> list[tuple[str, Any]]:
    resp = await c.post("/chat", json=body, headers=headers or HEADERS)
    assert resp.status_code == 200, resp.text
    assert resp.headers["content-type"].startswith("text/event-stream")
    return parse_sse(resp.text)


async def test_auth_on_every_route_but_healthz(client):
    c, _, _ = client
    assert (await c.get("/healthz")).json() == {"status": "ok"}
    for method, path in (("GET", "/sessions"), ("GET", "/sessions/x"), ("DELETE", "/sessions/x")):
        assert (await c.request(method, path)).status_code == 401
        assert (await c.request(method, path, headers={**HEADERS, "X-Agent-Secret": "wrong"})).status_code == 401
    assert (await c.post("/chat", json={"message": "hi"}, headers={"X-Agent-Secret": TEST_SECRET})).status_code == 401
    # permissions come from the session, not from the headers
    no_ai = {**HEADERS, "X-AFDD-Session": "tok-noai", "X-AFDD-Username": "noai_test", "X-AFDD-User-Id": "", "X-AFDD-Permissions": "admin"}
    resp = await c.get("/sessions", headers=no_ai)
    assert resp.status_code == 403 and resp.json() == {"error": "The AI assistant is not enabled for this user"}
    # whoever knows the secret cannot act as another user: headers must match the session
    for forged in ({"X-AFDD-Username": "someone_else"}, {"X-AFDD-User-Id": "1"}):
        assert (await c.get("/sessions", headers={**HEADERS, **forged})).status_code == 401
    assert (await c.get("/sessions", headers={**HEADERS, "X-AFDD-Session": "not-a-session"})).status_code == 401
    assert (await c.get("/sessions/does-not-exist", headers=HEADERS)).status_code == 404
    assert (await c.post("/chat", json={"message": "hi", "session_id": "does-not-exist"}, headers=HEADERS)).status_code == 404
    assert (await c.post("/chat", json={"message": ""}, headers=HEADERS)).status_code == 422


async def test_sessions_persist_in_agent_schema_and_history_is_rebuilt(client, scratch_db):
    c, models, _ = client
    models[ROOT].script.extend([Call("transfer_to_agent", {"agent_name": INSIGHT})])
    models[INSIGHT].script.extend(
        [
            Call("get_parameter_trend", {"asset": "CH_01", "parameter": "chwst", "hours": 4}),
            Call(DEEP, {"request": "Summarise: mean 6.53"}),
            "CH_01 chwst is stable around 6.5 °C.",
            "Yes, within limits.",
        ]
    )
    models[DEEP].script.append("Deep: stable.")

    first = await chat(c, {"message": "  Show   CH_01 leaving temperature  ", "context": {"page": "digital-twin", "asset_alias": "CH_01"}})
    assert first[0][0] == "session"
    sid = first[0][1]["session_id"]
    assert first[0][1]["title"] == "Show CH_01 leaving temperature"
    kinds = [e for e, _ in first]
    assert {"agent", "tool_call", "tool_result", "chart", "text", "message", "usage"} <= set(kinds) and kinds[-1] == "done"

    second = await chat(c, {"message": "Within limits?", "session_id": sid})
    assert second[0] == ("session", {"session_id": sid, "title": "Show CH_01 leaving temperature"})
    assert ("message", {"text": "Yes, within limits.", "agent": INSIGHT}) in second

    listed = (await c.get("/sessions", headers=HEADERS)).json()
    assert [s["id"] for s in listed] == [sid] and listed[0]["title"] == "Show CH_01 leaving temperature"
    assert listed[0]["updated_at"].endswith("Z")

    history = (await c.get(f"/sessions/{sid}", headers=HEADERS)).json()
    msgs = history["messages"]
    assert [(m["role"], m["text"]) for m in msgs] == [
        ("user", "Show   CH_01 leaving temperature"),
        ("assistant", "CH_01 chwst is stable around 6.5 °C."),
        ("user", "Within limits?"),
        ("assistant", "Yes, within limits."),
    ]
    a = msgs[1]
    assert a["agent"] == INSIGHT and len(a["charts"]) == 1 and a["charts"][0]["option"]["series"][0]["data"]
    assert [t["name"] for t in a["tool_calls"]] == ["get_parameter_trend", DEEP] and all(t["ok"] for t in a["tool_calls"])
    assert set(a) == {"id", "role", "text", "agent", "charts", "citations", "simulations", "navigations", "tool_calls", "created_at"}
    assert a["navigations"] == []

    # Another user cannot see, continue or delete it.
    other = {**HEADERS, "X-AFDD-Session": "tok-admin", "X-AFDD-Username": "admin_test", "X-AFDD-User-Id": ""}
    assert (await c.get("/sessions", headers=other)).json() == []
    assert (await c.get(f"/sessions/{sid}", headers=other)).status_code == 404
    assert (await c.post("/chat", json={"message": "hi", "session_id": sid}, headers=other)).status_code == 404
    assert (await c.delete(f"/sessions/{sid}", headers=other)).status_code == 404

    # ADK created its tables in schema agent (not public); the session token was never stored.
    conn = await asyncpg.connect(AI_ROLE_URL.format(db=scratch_db))
    try:
        tables = {r["table_name"]: r["table_schema"] for r in await conn.fetch("SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema IN ('agent', 'public')")}
        assert {"sessions", "events", "app_states", "user_states"} <= set(tables) and set(tables.values()) == {"agent"}
        dump = json.dumps([dict(r) for r in await conn.fetch("SELECT row_to_json(e)::text AS j FROM agent.events e")])
        dump += json.dumps([dict(r) for r in await conn.fetch("SELECT row_to_json(s)::text AS j FROM agent.sessions s")])
        assert TOKEN not in dump and "temp:afdd_session" not in dump
        assert "[UI context] page=digital-twin; selected asset=CH_01" in dump
    finally:
        await conn.close()

    assert (await c.delete(f"/sessions/{sid}", headers=HEADERS)).json() == {"ok": True}
    assert (await c.get(f"/sessions/{sid}", headers=HEADERS)).status_code == 404
    assert (await c.get("/sessions", headers=HEADERS)).json() == []


async def test_non_ascii_user_name_and_admin_headers(client):
    c, models, _ = client
    models[ROOT].script.append("Hello José.")
    # The Go proxy sends names as raw UTF-8 and "admin" instead of a permission list for admins;
    # the identity itself comes from the session (user/getuserdetails).
    admin = {**HEADERS, "X-AFDD-Session": "tok-jose", "X-AFDD-Username": "jose_test", "X-AFDD-User-Id": "",
             "X-AFDD-User-Name": "José Tan".encode(), "X-AFDD-Permissions": "admin"}  # fmt: skip
    events = await chat(c, {"message": "hi"}, headers=admin)
    assert ("message", {"text": "Hello José.", "agent": ROOT}) in events
    instruction = str(models[ROOT].requests[0].config.system_instruction)
    assert "helping José Tan, who can view AI insights, view and propose simulations, operate the plant" in instruction
    sid = events[0][1]["session_id"]
    assert [s["id"] for s in (await c.get("/sessions", headers=admin)).json()] == [sid]
    assert (await c.get(f"/sessions/{sid}", headers=HEADERS)).status_code == 404  # another user's conversation
    assert (await c.delete(f"/sessions/{sid}", headers=admin)).json() == {"ok": True}
