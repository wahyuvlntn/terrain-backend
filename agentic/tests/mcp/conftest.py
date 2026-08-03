"""Fixtures of the MCP server tests: real server processes, fake backend, test users.

Users (bearer tokens) of the fake backend:
* TOKEN        - basic, every AI permission, all locations
* tok-plant    - basic, every AI permission, only the Chiller Plant Room (CH, CHWP, CWP, CHWS)
* tok-admin    - admin (unrestricted)
* tok-nosim    - basic, aiassistant.read only, all locations
* tok-noai     - basic, digitaltwin.read only (no assistant)
The afdd-db server reads the live database through the read-only role.
"""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator
from contextlib import asynccontextmanager
from pathlib import Path

import httpx2
import psycopg
import pytest
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

from tests.agent.support import TEST_SECRET, TOKEN, FakeBackend, McpProcess, start_mcp_server

RO_URL = "postgresql://afdd_ai_ro:afdd-ai-ro-dev@127.0.0.1:5432/afdd"
RW_URL = "postgresql://afdd_ai:afdd-ai-dev@127.0.0.1:5432/afdd"


def plant_info() -> dict[str, object]:
    with psycopg.connect(RO_URL) as conn:
        locs = [r[0] for r in conn.execute("SELECT id FROM asset.locations ORDER BY id").fetchall()]
        rows = conn.execute("SELECT alias, location_id FROM analytics.v_assets").fetchall()
    with psycopg.connect(RW_URL) as conn:
        owner = conn.execute("SELECT id FROM core.users WHERE NOT is_deleted ORDER BY id LIMIT 1").fetchone()[0]
    loc_of = dict(rows)
    room = loc_of["CH_01"]
    return {"all": locs, "plant_room": room, "plant_room_aliases": sorted(a for a, loc in rows if loc == room), "owner": owner}


@pytest.fixture(scope="session")
def plant() -> dict[str, object]:
    return plant_info()


@pytest.fixture(scope="session")
def fake_backend(plant) -> Iterator[FakeBackend]:
    fb = FakeBackend()
    fb.add_user(TOKEN, locations=plant["all"])
    fb.add_user("tok-plant", locations=[plant["plant_room"]], name="Plant Room Tech")
    # the same scope, as the user who requested the scope test's tower job (test_db_server.scoped_jobs)
    fb.add_user("tok-owner", locations=[plant["plant_room"]], name="Job Owner", user_id=int(plant["owner"]))
    fb.add_user("tok-admin", role="admin", permissions=[], name="Admin")
    fb.add_user("tok-nosim", permissions=[{"group_alias": "aiassistant", "alias": "read"}], locations=plant["all"], name="Assistant Only")
    fb.add_user("tok-noai", permissions=[{"group_alias": "digitaltwin", "alias": "read"}], locations=plant["all"], name="No AI")
    fb.start()
    yield fb
    fb.stop()


@pytest.fixture(scope="session")
def servers(fake_backend: FakeBackend, tmp_path_factory: pytest.TempPathFactory) -> Iterator[dict[str, McpProcess]]:
    log_dir: Path = tmp_path_factory.mktemp("mcp-logs")
    procs = {name: start_mcp_server(name, fake_backend.url, log_dir) for name in ("db", "twin")}
    yield procs
    for p in procs.values():
        p.stop()


@asynccontextmanager
async def mcp_session(url: str, token: str, *, secret: str = TEST_SECRET, headers: dict[str, str] | None = None) -> AsyncIterator[ClientSession]:
    """An initialised MCP client session as `token`'s user."""
    all_headers = {"X-Agent-Secret": secret, "Authorization": f"Bearer {token}", **(headers or {})}
    async with httpx2.AsyncClient(headers=all_headers, timeout=60.0) as http:
        async with streamable_http_client(url, http_client=http) as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                yield session


def text_of(result) -> str:
    return "\n".join(getattr(c, "text", "") for c in result.content)
