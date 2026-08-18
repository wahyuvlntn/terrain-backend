"""ai-agent HTTP hardening: Host header allow-list and the development-secret warning."""

from __future__ import annotations

import logging

import httpx

from afdd_ai.agent import app as agent_app
from afdd_ai.common.config import get_settings


async def test_only_allowed_host_names_are_served():
    transport = httpx.ASGITransport(app=agent_app.app)
    for base, status in (("http://ai-agent:8000", 200), ("http://127.0.0.1:8000", 200), ("http://localhost", 200), ("http://evil.example", 400)):
        async with httpx.AsyncClient(transport=transport, base_url=base) as c:
            assert (await c.get("/healthz")).status_code == status, base


class _Service:
    async def prepare(self) -> None:
        return None


async def test_the_development_secret_is_warned_about_at_startup(monkeypatch, caplog):
    monkeypatch.setenv("AGENT_SHARED_SECRET", agent_app.DEV_SECRET)
    get_settings.cache_clear()
    agent_app.set_service(_Service())  # type: ignore[arg-type]
    try:
        with caplog.at_level(logging.WARNING):
            async with agent_app.lifespan(agent_app.app):
                pass
        assert any(r.levelno == logging.WARNING and "development default" in r.getMessage() for r in caplog.records)
    finally:
        agent_app.set_service(None)
        monkeypatch.undo()
        get_settings.cache_clear()
