"""MCP server authentication: shared secret, user session, assistant permission, Host check."""

from __future__ import annotations

import httpx
import pytest

from tests.agent.support import TEST_SECRET, TOKEN

from .conftest import mcp_session

INIT = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "test", "version": "0"}},
}
MCP_HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}


@pytest.mark.parametrize("name", ["db", "twin"])
def test_healthz_is_public(servers, name):
    resp = httpx.get(f"http://127.0.0.1:{servers[name].port}/healthz")
    assert resp.status_code == 200 and resp.json() == {"status": "ok", "server": name}


@pytest.mark.parametrize("name", ["db", "twin"])
@pytest.mark.parametrize(
    ("headers", "status", "error"),
    [
        ({}, 401, "Invalid agent secret"),
        ({"X-Agent-Secret": "wrong", "Authorization": f"Bearer {TOKEN}"}, 401, "Invalid agent secret"),
        ({"X-Agent-Secret": TEST_SECRET}, 401, "Missing bearer token"),
        ({"X-Agent-Secret": TEST_SECRET, "Authorization": "Bearer not-a-session"}, 401, "Invalid or expired user session"),
        ({"X-Agent-Secret": TEST_SECRET, "Authorization": f"Basic {TOKEN}"}, 401, "Missing bearer token"),
        ({"X-Agent-Secret": TEST_SECRET, "Authorization": "Bearer tok-noai"}, 403, "The AI assistant is not enabled for this user"),
    ],
)
def test_rejects_before_mcp(servers, name, headers, status, error):
    resp = httpx.post(servers[name].url, json=INIT, headers={**MCP_HEADERS, **headers})
    assert resp.status_code == status
    assert resp.json() == {"error": error}


def test_dns_rebinding_host_check(servers):
    ok = {**MCP_HEADERS, "X-Agent-Secret": TEST_SECRET, "Authorization": f"Bearer {TOKEN}"}
    url = servers["db"].url
    assert httpx.post(url, json=INIT, headers=ok).status_code == 200
    assert httpx.post(url, json=INIT, headers={**ok, "Host": f"ai-mcp:{servers['db'].port}"}).status_code == 200
    assert httpx.post(url, json=INIT, headers={**ok, "Host": "evil.example"}).status_code == 421


async def test_valid_session_lists_tools(servers, fake_backend):
    async with mcp_session(servers["db"].url, TOKEN) as s:
        tools = {t.name for t in (await s.list_tools()).tools}
    assert tools == {
        "query_parameter_history",
        "get_energy_summary",
        "list_alarm_events",
        "list_anomalies",
        "list_faults",
        "get_point_map",
        "search_documents",
        "list_simulation_jobs",
    }
    async with mcp_session(servers["twin"].url, TOKEN) as s:
        tools = {t.name for t in (await s.list_tools()).tools}
    assert tools == {
        "get_plant_state",
        "get_fault_catalog",
        "get_forecast_conditions",
        "propose_simulation",
        "propose_production_plan",
        "get_production_planning_options",
        "get_simulation_status",
        "get_simulation_results",
        "list_simulations",
        "get_latest_recommendation",
    }
    assert any(c["token"] == TOKEN for c in fake_backend.calls_to("user/getuserdetails"))
