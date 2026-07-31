"""Test setup for the AFDD AI services.

Host-side tests reach the dockerised Postgres / MinIO through the localhost-only
ports published by docker-compose (127.0.0.1:5432, 127.0.0.1:9000). Run:

    cd backend/agentic
    .venv/Scripts/python -m pytest tests            (Windows host)

DB-backed tests are skipped when the database is unreachable. Tests that put
work into the live queues (sim jobs, optimizer runs, faults, documents) are
skipped while the ai-analytics container runs, because the live worker would
pick that work up (and spend Gemini budget on it); set AFDD_TEST_ALLOW_LIVE=1
to run them anyway, or stop the container first.
"""

from __future__ import annotations

import asyncio
import os
import shutil
import subprocess
import sys
from functools import lru_cache

import pytest

# Defaults for host-side runs (container env overrides them).
os.environ.setdefault("DATABASE_URL", "postgresql://afdd_ai:afdd-ai-dev@127.0.0.1:5432/afdd")
os.environ.setdefault("DATABASE_URL_RO", "postgresql://afdd_ai_ro:afdd-ai-ro-dev@127.0.0.1:5432/afdd")
os.environ.setdefault("AFDD_API_URL", "https://localhost:4500")
os.environ.setdefault("MINIO_ENDPOINT", "127.0.0.1:9000")

if sys.platform == "win32":
    # psycopg async needs a selector loop on Windows.
    asyncio.set_event_loop_policy(asyncio.WindowsSelectorEventLoopPolicy())  # type: ignore[attr-defined]


# Modules whose tests insert rows the live analytics worker acts on.
LIVE_QUEUE_WRITERS = {
    "test_simpost.py",
    "test_tasks_db.py",
    "test_worker.py",
    "test_corpus.py",
    "test_ingest_search.py",
}


@lru_cache(maxsize=1)
def live_worker_running() -> bool:
    if os.environ.get("AFDD_TEST_ALLOW_LIVE") == "1" or not shutil.which("docker"):
        return False
    try:
        out = subprocess.run(
            ["docker", "ps", "--filter", "name=ai-analytics", "--filter", "status=running", "-q"],
            capture_output=True, text=True, timeout=10,
        )  # fmt: skip
    except (OSError, subprocess.SubprocessError):
        return False
    return bool(out.stdout.strip())


def pytest_collection_modifyitems(config, items):
    if not live_worker_running():
        return
    skip = pytest.mark.skip(reason="the live ai-analytics container would act on this test's rows; stop it or set AFDD_TEST_ALLOW_LIVE=1")
    for item in items:
        if item.path.name in LIVE_QUEUE_WRITERS:
            item.add_marker(skip)
