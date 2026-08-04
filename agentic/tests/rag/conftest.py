"""Fixtures for the RAG tests (host database and MinIO; pools closed between event loops).

On the host the MinIO credentials of the running stack come from backend/.env
(the compose file passes the same values to MinIO); values already in the
environment win. Secrets are only placed in this process's environment, never
printed or written anywhere.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from afdd_ai.common import config, db, storage

BACKEND_ENV = Path(__file__).resolve().parents[3] / ".env"


def dotenv(keys: tuple[str, ...]) -> dict[str, str]:
    """Selected KEY=VALUE pairs of backend/.env (empty values skipped)."""
    out: dict[str, str] = {}
    try:
        lines = BACKEND_ENV.read_text(encoding="utf-8").splitlines()
    except OSError:
        return out
    for line in lines:
        key, sep, value = line.partition("=")
        key, value = key.strip(), value.strip().strip('"').strip("'")
        if sep and key in keys and value:
            out[key] = value
    return out


def use_stack_credentials(*keys: str) -> None:
    """Put the stack's values of `keys` into the environment and drop cached settings / clients."""
    for k, v in dotenv(keys).items():
        os.environ.setdefault(k, v)
    config.get_settings.cache_clear()
    storage._client.cache_clear()  # noqa: SLF001 - rebuilt with the new credentials


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


@pytest.fixture
async def live_store(live_db):
    """The host database and the private MinIO docs bucket (skips when MinIO is unreachable)."""
    use_stack_credentials("MINIO_ACCESS_KEY", "MINIO_SECRET_KEY")
    try:
        await storage.ensure_bucket()
    except Exception as err:  # noqa: BLE001 - any failure means skip
        pytest.skip(f"MinIO not reachable: {type(err).__name__}")
    yield
