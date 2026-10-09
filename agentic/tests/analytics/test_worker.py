"""The analytics worker: task selection, resilient loops, NOTIFY wake-up, --once dry runs."""

from __future__ import annotations

import asyncio

import pytest

from afdd_ai.analytics import worker
from afdd_ai.common import db


def test_task_selection(monkeypatch):
    monkeypatch.delenv("AI_ANALYTICS_TASKS", raising=False)
    assert list(worker.Worker().tasks) == list(worker.LOOP_TASKS)
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "anomaly, faults")
    assert set(worker.Worker().tasks) == {"anomaly", "faults"}
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "anomaly,bogus")
    with pytest.raises(SystemExit):
        worker.Worker()


def test_every_design_task_is_scheduled(monkeypatch):
    monkeypatch.delenv("AI_ANALYTICS_TASKS", raising=False)
    w = worker.Worker()
    t = w.tasks
    assert t["anomaly"].interval_s == 60 and t["faults"].interval_s == 300
    assert "sim_jobs" in t["simpost"].wake_on and t["simpost"].interval_s == 10
    assert "rag_documents" in t["ingest"].wake_on and t["ingest"].interval_s == 30
    assert "optimizer_runs" in t["manual_runs"].wake_on
    assert t["corpus"].interval_s == 86400 and "rag_corpus" in t["corpus"].wake_on and t["expiry"].interval_s == 60
    assert set(worker.CHANNELS) == {"sim_jobs", "rag_documents", "optimizer_runs", "rag_corpus"}


async def test_a_failing_iteration_does_not_stop_the_loop(monkeypatch):
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "expiry")
    w = worker.Worker()
    calls = {"n": 0}

    async def flaky():
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("database hiccup")
        if calls["n"] >= 3:
            w.request_stop("test done")
        return {"ok": calls["n"]}

    t = worker.Task("flaky", flaky, interval_s=0.05, timeout_s=5, retry_s=0.01)
    await asyncio.wait_for(w._loop(t), timeout=10)  # noqa: SLF001
    assert calls["n"] == 3 and t.runs == 3 and t.failures == 1 and t.consecutive_failures == 0
    assert t.last_error is None and t.last_result == {"ok": 3}
    assert t.health(t.last_ok + 1)["last_ok_s_ago"] == 1.0


async def test_a_hung_iteration_times_out(monkeypatch):
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "expiry")
    w = worker.Worker()

    async def hang():
        w.request_stop("test done")
        await asyncio.sleep(30)

    t = worker.Task("hang", hang, interval_s=0.05, timeout_s=0.1)
    await asyncio.wait_for(w._loop(t), timeout=5)  # noqa: SLF001
    assert t.failures == 1 and "TimeoutError" in (t.last_error or "")


async def test_notify_wakes_the_listening_tasks(monkeypatch):
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "simpost,ingest")
    w = worker.Worker()
    seen: list[str] = []

    async def body():
        seen.append("run")
        if len(seen) == 2:
            w.request_stop("test done")

    t = worker.Task("woken", body, interval_s=3600, timeout_s=5, wake_on=("sim_jobs",))
    w.tasks = {"woken": t}
    loop = asyncio.create_task(w._loop(t))  # noqa: SLF001
    await asyncio.sleep(0.05)
    assert seen == ["run"]  # now waiting an hour unless woken
    await w._on_notify("rag_documents", "1")  # noqa: SLF001 - not its channel
    await asyncio.sleep(0.05)
    assert seen == ["run"]
    await w._on_notify("sim_jobs", "42")  # noqa: SLF001
    await asyncio.wait_for(loop, timeout=5)
    assert seen == ["run", "run"]


@pytest.mark.usefixtures("live_db")
async def test_service_starts_listens_and_stops_gracefully(monkeypatch):
    monkeypatch.setenv("AI_ANALYTICS_TASKS", "expiry")
    monkeypatch.setenv("AI_WORKER_LEASE", "false")  # the running ai-analytics container holds the lease
    w = worker.Worker()
    ticks: list[float] = []

    async def tick():
        ticks.append(asyncio.get_running_loop().time())

    w.tasks = {"tick": worker.Task("tick", tick, interval_s=0.2, timeout_s=5, wake_on=("sim_jobs",))}
    service = asyncio.create_task(w.run())
    await asyncio.sleep(1.5)  # pool open, LISTEN connection up, a few iterations
    await db.notify("sim_jobs", "0")  # wakes the task through the listener
    await asyncio.sleep(0.3)
    w.request_stop("test")
    await asyncio.wait_for(service, timeout=worker.SHUTDOWN_GRACE_S + 5)
    assert len(ticks) >= 5 and w.stop.is_set()
    assert db._pools == {}  # noqa: SLF001 - closed on the way out


def test_brief_result_lines():
    assert worker._brief({"a": 1, "ids": [1, 2], "planned": [3]}) == '{"a": 1}'  # noqa: SLF001
    assert worker._brief("x" * 400) == "x" * 300  # noqa: SLF001


@pytest.mark.usefixtures("live_db")
async def test_once_dry_run_of_every_task_writes_nothing():
    tables = ("rag.documents", "analytics.anomalies", "analytics.faults", "analytics.fault_rules", "analytics.optimizer_runs", "analytics.recommendations", "analytics.surrogate_models", "sim.jobs")  # noqa: E501
    before = {t: await db.fetch_val(f"SELECT count(*) FROM {t}") for t in tables}
    for name in worker.TASK_NAMES:
        if name == "corpus":
            continue  # renders every document of the site (about a minute); covered by tests/rag/test_corpus.py
        await worker.run_once(name, site=None, dry_run=True)
    after = {t: await db.fetch_val(f"SELECT count(*) FROM {t}") for t in tables}
    assert after == before
    with pytest.raises(SystemExit):
        await worker.run_once("nonsense", site=None, dry_run=True)
