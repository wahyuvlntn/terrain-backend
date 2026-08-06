"""Fixtures for the analytics tests (host DB access, pool cleanup between event loops)."""

from __future__ import annotations

import pytest

from afdd_ai.common import db


async def _db_reachable() -> bool:
    try:
        await db.fetch_val("SELECT 1")
        return True
    except Exception:  # noqa: BLE001 - any failure means skip DB tests
        return False
    finally:
        await db.close_pools()


@pytest.fixture
async def live_db():
    """The host database (skips when unreachable); pools are closed after the test (one event loop per test)."""
    if not await _db_reachable():
        pytest.skip("database not reachable")
    yield
    await db.close_pools()
