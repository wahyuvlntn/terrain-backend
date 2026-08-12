"""Fixtures of the agent tests: fake backend, MCP server processes, scripted models.

No test calls Gemini: every agent runs on `ScriptedLlm`. The backend API is the
in-process `FakeBackend` (real HTTP), the MCP servers are real processes
(`python -m afdd_ai.mcp.serve db|twin`) pointed at it, and the afdd-db server
reads the live database read-only.
"""

from __future__ import annotations

import os
from collections.abc import AsyncIterator, Iterator
from pathlib import Path

import pytest

from afdd_ai.agent import backend_api
from afdd_ai.mcp import auth as mcp_auth
from afdd_ai.common import backend, db
from afdd_ai.common.config import get_settings

from .support import TEST_SECRET, TEST_USER_ID, TOKEN, FakeBackend, McpProcess, start_mcp_server

ENV_KEYS = ("AGENT_SHARED_SECRET", "GOOGLE_API_KEY", "GEMINI_API_KEY", "AFDD_API_URL", "MCP_DB_URL", "MCP_TWIN_URL")


def all_location_ids() -> list[int]:
    import psycopg

    url = os.environ.get("DATABASE_URL_RO", "postgresql://afdd_ai_ro:afdd-ai-ro-dev@127.0.0.1:5432/afdd")
    with psycopg.connect(url) as conn:
        return [r[0] for r in conn.execute("SELECT id FROM asset.locations ORDER BY id").fetchall()]


@pytest.fixture(scope="session")
def fake_backend() -> Iterator[FakeBackend]:
    fb = FakeBackend()
    fb.add_user(TOKEN, locations=all_location_ids())
    fb.add_user("tok-admin", role="admin", permissions=[], name="Admin")
    fb.add_user("tok-noai", permissions=[{"group_alias": "digitaltwin", "alias": "read"}], name="No AI")
    fb.add_user("tok-jose", role="admin", permissions=[], name="José Tan")
    fb.start()
    yield fb
    fb.stop()


@pytest.fixture(scope="session")
def mcp_servers(fake_backend: FakeBackend, tmp_path_factory: pytest.TempPathFactory) -> Iterator[dict[str, McpProcess]]:
    log_dir: Path = tmp_path_factory.mktemp("mcp-logs")
    servers = {name: start_mcp_server(name, fake_backend.url, log_dir) for name in ("db", "twin")}
    yield servers
    for s in servers.values():
        s.stop()


@pytest.fixture(scope="session", autouse=True)
def agent_env(fake_backend: FakeBackend, mcp_servers: dict[str, McpProcess]) -> Iterator[None]:
    """Point the settings at the fakes (restored afterwards)."""
    saved = {k: os.environ.get(k) for k in ENV_KEYS}
    os.environ.update(
        {
            "AGENT_SHARED_SECRET": TEST_SECRET,
            "GOOGLE_API_KEY": "test-key-never-used",
            "GEMINI_API_KEY": "",
            "AFDD_API_URL": fake_backend.url,
            "MCP_DB_URL": mcp_servers["db"].url,
            "MCP_TWIN_URL": mcp_servers["twin"].url,
        }
    )
    get_settings.cache_clear()
    yield
    for k, v in saved.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v
    get_settings.cache_clear()


@pytest.fixture(autouse=True)
async def _reset_clients(fake_backend: FakeBackend) -> AsyncIterator[None]:
    """Pools and HTTP clients are bound to one event loop: recreate them per test."""
    backend_api.clear_cache()
    mcp_auth.clear_cache()
    fake_backend.calls.clear()
    yield
    await backend.close_client()
    await db.close_pools()


@pytest.fixture
async def cleanup_usage() -> AsyncIterator[None]:
    """Remove the llm_usage rows the scripted models' usage callbacks recorded."""
    yield
    await db.execute("DELETE FROM analytics.llm_usage WHERE user_id = %s", (TEST_USER_ID,))
