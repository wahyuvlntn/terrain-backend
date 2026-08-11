"""Optimizer runs never stay RUNNING: interrupted runs are failed, stale ones are swept.

No row is written: the run bookkeeping is replaced by recorders, and the new SQL
statements are checked against the live schema with EXPLAIN (planned, not run).
"""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from afdd_ai.analytics import calibration, optimizer, surrogate, worker
from afdd_ai.common import db

from .synth import plant_frame

INTERRUPTED = "interrupted (CancelledError)"


class Finished:
    """Records optimizer._finish calls (run id, status, error)."""

    def __init__(self) -> None:
        self.calls: list[tuple[int, str, str | None]] = []

    async def __call__(self, run_id: int, status: str, **kw: Any) -> None:
        self.calls.append((run_id, status, kw.get("error")))


@pytest.fixture
def finished(monkeypatch) -> Finished:
    rec = Finished()
    monkeypatch.setattr(optimizer, "_finish", rec)
    return rec


def hanging_evaluate(monkeypatch) -> asyncio.Event:
    started = asyncio.Event()

    async def evaluate(site, *, seed=0):  # noqa: ANN001
        started.set()
        await asyncio.sleep(3600)

    monkeypatch.setattr(optimizer, "evaluate", evaluate)
    return started


async def test_a_run_cut_by_a_timeout_is_failed(monkeypatch, finished):
    hanging_evaluate(monkeypatch)
    with pytest.raises(TimeoutError):
        await asyncio.wait_for(optimizer.execute(7, site="SGF1"), timeout=0.2)
    assert finished.calls == [(7, "FAILED", INTERRUPTED)]


async def test_a_cancelled_run_is_failed_and_the_cancellation_propagates(monkeypatch, finished):
    started = hanging_evaluate(monkeypatch)
    task = asyncio.create_task(optimizer.execute(8, site="SGF1"))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert finished.calls == [(8, "FAILED", INTERRUPTED)]


async def test_the_worker_iteration_timeout_fails_the_run(monkeypatch, finished):
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "expiry")
    w = worker.Worker()
    hanging_evaluate(monkeypatch)

    async def manual_run() -> str:
        w.request_stop("test done")
        return await optimizer.execute(9, site="SGF1")

    t = worker.Task("manual_runs", manual_run, interval_s=0.05, timeout_s=0.2)
    await asyncio.wait_for(w._loop(t), timeout=5)  # noqa: SLF001
    assert t.failures == 1 and "TimeoutError" in (t.last_error or "")
    assert finished.calls == [(9, "FAILED", INTERRUPTED)]


async def test_the_worker_sweeps_at_start_and_fails_runs_cut_by_a_shutdown(monkeypatch, finished):
    swept: list[int] = []

    async def sweep() -> int:
        swept.append(1)
        return 0

    async def get_pool(*args: Any, **kwargs: Any) -> None:
        return None

    async def listen(channels, handler, stop):  # noqa: ANN001
        await stop.wait()

    async def nothing() -> None:
        return None

    monkeypatch.setattr(optimizer, "sweep_stale", sweep)
    monkeypatch.setattr(worker.db, "get_pool", get_pool)
    monkeypatch.setattr(worker.db, "listen", listen)
    monkeypatch.setattr(worker.db, "close_pools", nothing)
    monkeypatch.setattr(worker.backend, "close_client", nothing)
    monkeypatch.setattr(worker, "SHUTDOWN_GRACE_S", 0.2)
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "expiry")
    started = hanging_evaluate(monkeypatch)
    w = worker.Worker()
    w.tasks = {"manual_runs": worker.Task("manual_runs", lambda: optimizer.execute(10, site="SGF1"), interval_s=60, timeout_s=900)}
    service = asyncio.create_task(w.run())
    await asyncio.wait_for(started.wait(), timeout=5)
    w.request_stop("test")  # the run is still evaluating: cancelled after the grace period
    await asyncio.wait_for(service, timeout=5)
    assert swept == [1] and finished.calls == [(10, "FAILED", INTERRUPTED)]


async def test_manual_runs_sweep_stale_runs_before_claiming(monkeypatch):
    order: list[str] = []

    async def sweep() -> int:
        order.append("sweep")
        return 1

    async def claim() -> None:
        order.append("claim")

    monkeypatch.setattr(optimizer, "sweep_stale", sweep)
    monkeypatch.setattr(optimizer, "claim_manual", claim)
    assert await optimizer.run_manual() == 0 and order == ["sweep", "claim"]


@pytest.mark.usefixtures("live_db")
async def test_new_statements_are_valid_sql(monkeypatch):
    """The stale-run sweep, the calibration merge and the unusable-DOE marker,
    planned by PostgreSQL on the live schema (EXPLAIN: nothing is executed)."""
    real_fetch_all = db.fetch_all
    planned: list[str] = []

    async def explain(sql: str, params: Any = None) -> int:
        rows = await real_fetch_all("EXPLAIN " + sql, params)
        assert rows, sql
        planned.append(sql.split()[1])
        return 0  # as if nothing matched: apply_job then writes the calibration only

    async def sim_frames(job_id: int) -> dict[str, Any]:
        return {"baseline": plant_frame(24, seed=51)}

    monkeypatch.setattr(db, "execute", explain)
    monkeypatch.setattr(calibration.data, "sim_frames", sim_frames)
    assert await optimizer.sweep_stale() == 0
    await surrogate.mark_unusable(-1, "pytest: not usable")
    if not await real_fetch_all("SELECT code FROM analytics.fault_rules WHERE updated_by IS NULL LIMIT 1"):
        pytest.skip("no calibratable rule in this database")
    await calibration.apply_job(-1)
    assert planned[:2] == ["analytics.optimizer_runs", "sim.jobs"] and "analytics.fault_rules" in planned[2:]
