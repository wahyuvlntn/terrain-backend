"""ai-analytics worker: `python -m afdd_ai.analytics.worker` (DESIGN 6.1).

One asyncio process runs every analytics task as an independent loop:

| task          | cadence                                   | module                         |
|---------------|-------------------------------------------|--------------------------------|
| anomaly       | ANOMALY_INTERVAL_S (60 s)                 | analytics/anomaly.py           |
| faults        | FDD_INTERVAL_S (300 s)                    | analytics/faults.py            |
| instructions  | on new fault (+120 s poll)                | analytics/instructions.py      |
| optimizer     | OPTIMIZER_INTERVAL_MIN (checked each 60 s)| analytics/optimizer.py         |
| manual_runs   | NOTIFY optimizer_runs (+30 s poll)        | analytics/optimizer.py         |
| validations   | NOTIFY sim_jobs (+60 s poll)              | analytics/optimizer.py         |
| expiry        | 60 s                                      | analytics/optimizer.py         |
| surrogate     | NOTIFY sim_jobs (+600 s poll)             | analytics/surrogate.py         |
| calibration   | NOTIFY sim_jobs (+600 s poll)             | analytics/calibration.py       |
| simpost       | NOTIFY sim_jobs (+10 s poll)              | analytics/simpost.py           |
| corpus        | on start, daily                           | rag/corpus.py                  |
| ingest        | NOTIFY rag_documents (+30 s poll)         | rag/ingest.py                  |

A failing iteration is logged and the loop continues (shorter retry delay);
each iteration has a timeout. AI_ANALYTICS_TASKS (comma-separated task names)
runs only those tasks, e.g. on a development host without the simulator. A health line with the state of every task is
logged every minute (and written to AI_HEALTH_FILE when set). SIGTERM / SIGINT
stop the loops gracefully: running iterations get SHUTDOWN_GRACE_S to finish.

Host testing: `--once <task>` (repeatable) runs one iteration of a task and
prints its result; with `--dry-run` every task computes and reports without
writing (no rows, simulation jobs, uploads or Gemini calls); `--list` lists the
tasks.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import logging
import os
import signal
import sys
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from ..common import backend, db
from ..common.aio import run as aio_run
from ..common.config import get_settings
from ..common.log import setup_logging
from ..rag import corpus, ingest
from . import anomaly, calibration, data, faults, instructions, optimizer, simpost, surrogate

log = logging.getLogger("ai-analytics")

SHUTDOWN_GRACE_S = 20.0
HEALTH_INTERVAL_S = 60.0
CHANNELS = ("sim_jobs", "rag_documents", "optimizer_runs")


@dataclass
class Task:
    name: str
    fn: Callable[[], Awaitable[Any]]
    interval_s: float
    timeout_s: float
    wake_on: tuple[str, ...] = ()
    initial_delay_s: float = 0.0
    retry_s: float | None = None
    next_delay: Callable[[Any], float | None] | None = None  # override the interval from the result
    event: asyncio.Event = field(default_factory=asyncio.Event)
    runs: int = 0
    failures: int = 0
    consecutive_failures: int = 0
    last_ok: float | None = None
    last_error: str | None = None
    last_result: Any = None
    last_duration: float | None = None
    running: bool = False

    def health(self, now: float) -> dict[str, Any]:
        return {
            "runs": self.runs,
            "failures": self.failures,
            "running": self.running,
            "last_ok_s_ago": round(now - self.last_ok, 1) if self.last_ok else None,
            "last_duration_s": round(self.last_duration, 3) if self.last_duration is not None else None,
            "last_error": self.last_error,
        }


def _brief(result: Any) -> str:
    if isinstance(result, faults.LiveResult):
        return f"evaluated={result.evaluated} opened={len(result.opened)} updated={result.updated} cleared={result.cleared}"
    if isinstance(result, dict):
        return json.dumps({k: v for k, v in result.items() if k not in ("ids", "planned")}, default=str)[:300]
    return str(result)[:300]


def enabled_tasks() -> set[str] | None:
    """Task names from AI_ANALYTICS_TASKS (comma separated; empty = every task)."""
    raw = os.environ.get("AI_ANALYTICS_TASKS", "").strip()
    if not raw:
        return None
    names = {n.strip() for n in raw.split(",") if n.strip()}
    unknown = names - set(LOOP_TASKS)
    if unknown:
        raise SystemExit(f"AI_ANALYTICS_TASKS: unknown task(s) {', '.join(sorted(unknown))}; known: {', '.join(LOOP_TASKS)}")
    return names


class Worker:
    def __init__(self, site: str | None = None):
        s = get_settings()
        self.site = site or s.site_code
        self.stop = asyncio.Event()
        self.anomaly_runner = anomaly.AnomalyRunner(self.site)
        self.tasks: dict[str, Task] = {}
        self._add(Task("anomaly", self._anomaly, s.anomaly_interval_s, 300, initial_delay_s=5))
        self._add(Task("faults", self._faults, s.fdd_interval_s, 300, initial_delay_s=20))
        self._add(Task("instructions", instructions.run, 120, 900, initial_delay_s=30, next_delay=self._ingest_delay))
        self._add(Task("optimizer", self._optimizer, 60, 900, initial_delay_s=90))
        self._add(Task("manual_runs", optimizer.run_manual, 30, 900, wake_on=("optimizer_runs",), initial_delay_s=10))
        self._add(Task("validations", optimizer.complete_validations, 60, 120, wake_on=("sim_jobs",), initial_delay_s=15))
        self._add(Task("expiry", optimizer.expire, 60, 60, initial_delay_s=25))
        self._add(Task("surrogate", self._surrogate, 600, 1800, wake_on=("sim_jobs",), initial_delay_s=40, retry_s=300))
        self._add(Task("calibration", self._calibration, 600, 900, wake_on=("sim_jobs",), initial_delay_s=45, retry_s=300))
        self._add(Task("simpost", self._simpost, 10, 1800, wake_on=("sim_jobs",), initial_delay_s=3))
        self._add(Task("corpus", self._corpus, 86400, 3600, initial_delay_s=60, retry_s=1800))
        self._add(Task("ingest", ingest.run, 30, 1800, wake_on=("rag_documents",), initial_delay_s=8, next_delay=self._ingest_delay))
        only = enabled_tasks()
        if only is not None:
            self.tasks = {n: t for n, t in self.tasks.items() if n in only}

    def _add(self, t: Task) -> None:
        self.tasks[t.name] = t

    def wake(self, *names: str) -> None:
        for n in names:
            t = self.tasks.get(n)
            if t:
                t.event.set()

    # -- task bodies -----------------------------------------------------

    async def _anomaly(self) -> dict[str, Any]:
        return await self.anomaly_runner.cycle()

    async def _faults(self) -> faults.LiveResult:
        res = await faults.run_live(site_code=self.site)
        if res.opened:
            self.wake("instructions")
        return res

    async def _optimizer(self) -> str:
        return await optimizer.run_scheduled()

    async def _surrogate(self) -> str:
        return await surrogate.run()

    async def _calibration(self) -> str:
        return await calibration.run()

    async def _simpost(self) -> int:
        n = await simpost.run()
        if n:
            self.wake("validations", "surrogate", "calibration")
        return n

    async def _corpus(self) -> dict[str, Any]:
        res = await corpus.generate(self.site)
        if res.get("created") or res.get("updated"):
            self.wake("ingest")
        return res

    @staticmethod
    def _ingest_delay(result: Any) -> float | None:
        # budget reached / Gemini not configured: try again in 15 min instead of every 30 s
        return 900.0 if isinstance(result, dict) and result.get("deferred") else None

    # -- loops -----------------------------------------------------------

    async def _wait(self, t: Task, delay: float) -> None:
        if delay <= 0 or self.stop.is_set():
            return
        waiters = [asyncio.ensure_future(self.stop.wait()), asyncio.ensure_future(t.event.wait())]
        try:
            await asyncio.wait(waiters, timeout=delay, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for w in waiters:
                w.cancel()

    async def _loop(self, t: Task) -> None:
        await self._wait(t, t.initial_delay_s)
        while not self.stop.is_set():
            t.event.clear()
            started = time.monotonic()
            t.running = True
            delay = t.interval_s
            try:
                result = await asyncio.wait_for(t.fn(), timeout=t.timeout_s)
                t.runs += 1
                t.consecutive_failures = 0
                t.last_ok = time.time()
                t.last_error = None
                t.last_result = result
                if t.next_delay:
                    delay = t.next_delay(result) or delay
                log.debug("task %s done in %.2fs: %s", t.name, time.monotonic() - started, _brief(result))
            except asyncio.CancelledError:
                raise
            except Exception as err:  # noqa: BLE001 - keep the loop alive
                t.runs += 1
                t.failures += 1
                t.consecutive_failures += 1
                t.last_error = f"{type(err).__name__}: {err}"[:300]
                log.exception("task %s failed (%s in a row)", t.name, t.consecutive_failures)
                # retry sooner, backing off up to the normal interval
                base = t.retry_s if t.retry_s is not None else min(t.interval_s, 60.0)
                delay = min(t.interval_s, base * 2 ** min(t.consecutive_failures - 1, 5))
            finally:
                t.running = False
                t.last_duration = time.monotonic() - started
            await self._wait(t, delay)

    async def _on_notify(self, channel: str, payload: str) -> None:
        for t in self.tasks.values():
            if channel in t.wake_on:
                t.event.set()

    async def _health(self) -> None:
        path = os.environ.get("AI_HEALTH_FILE", "").strip()
        while not self.stop.is_set():
            now = time.time()
            state = {name: t.health(now) for name, t in self.tasks.items()}
            failing = sorted(n for n, t in self.tasks.items() if t.consecutive_failures)
            pool = db._pools.get("rw")  # noqa: SLF001 - pool statistics for the health line
            stats = pool.get_stats() if pool else {}
            log.info(
                "health site=%s failing=%s pool=%s/%s %s",
                self.site,
                ",".join(failing) or "-",
                stats.get("pool_size", 0) - stats.get("pool_available", 0),
                stats.get("pool_size", 0),
                " ".join(f"{n}:{(s['last_ok_s_ago'] if s['last_ok_s_ago'] is not None else '-')}" for n, s in state.items()),
            )
            if path:
                with contextlib.suppress(OSError):
                    tmp = path + ".tmp"
                    with open(tmp, "w", encoding="utf-8") as fh:
                        json.dump({"at": now, "site": self.site, "failing": failing, "tasks": state}, fh)
                    os.replace(tmp, path)
            with contextlib.suppress(asyncio.TimeoutError):
                await asyncio.wait_for(self.stop.wait(), timeout=HEALTH_INTERVAL_S)

    def _install_signals(self) -> None:
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            try:
                loop.add_signal_handler(sig, self.request_stop, sig.name)
            except (NotImplementedError, RuntimeError, ValueError):
                pass  # Windows: Ctrl+C cancels the main task instead

    def request_stop(self, reason: str = "") -> None:
        if not self.stop.is_set():
            log.info("stopping (%s)", reason or "requested")
            self.stop.set()
            for t in self.tasks.values():
                t.event.set()

    async def run(self) -> None:
        self._install_signals()
        await db.get_pool(min_size=1, max_size=6)
        log.info("ai-analytics starting site=%s tasks=%s", self.site, ",".join(self.tasks))
        try:
            await optimizer.sweep_stale()  # runs a killed worker left PENDING / RUNNING
        except Exception:  # noqa: BLE001 - run_manual sweeps again
            log.exception("could not sweep stale optimizer runs")
        loops = [asyncio.create_task(self._loop(t), name=f"task-{t.name}") for t in self.tasks.values()]
        listener = asyncio.create_task(db.listen(CHANNELS, self._on_notify, self.stop), name="listen")
        health = asyncio.create_task(self._health(), name="health")
        try:
            await self.stop.wait()
        except asyncio.CancelledError:
            self.request_stop("cancelled")
        done, pending = await asyncio.wait(loops + [health], timeout=SHUTDOWN_GRACE_S)
        for p in pending:
            p.cancel()
        listener.cancel()
        await asyncio.gather(*pending, listener, return_exceptions=True)
        await backend.close_client()
        await db.close_pools()
        log.info("ai-analytics stopped")


# ---------------------------------------------------------------------------
# CLI


async def run_once(name: str, *, site: str | None, dry_run: bool) -> Any:
    """One iteration of a task. dry_run: compute and report, write nothing
    (no rows, no simulation jobs, no uploads, no Gemini calls)."""
    site = site or get_settings().site_code
    if name == "anomaly":
        runner = anomaly.AnomalyRunner(site)
        return await runner.cycle(dry_run=dry_run)
    if name == "faults":
        res = await faults.run_live(site_code=site, dry_run=dry_run)
        return {"evaluated": res.evaluated, "opened": res.opened, "updated": res.updated, "cleared": res.cleared, "skipped": res.skipped, "planned": res.planned}
    once: dict[str, Callable[[], Awaitable[Any]]] = {
        "instructions": lambda: instructions.run(dry_run=dry_run),
        "optimizer": lambda: optimizer.run_scheduled(dry_run=dry_run),
        "manual_runs": lambda: optimizer.run_manual(dry_run=dry_run),
        "validations": lambda: optimizer.complete_validations(dry_run=dry_run),
        "expiry": lambda: optimizer.expire(dry_run=dry_run),
        "surrogate": lambda: surrogate.run(dry_run=dry_run),
        "calibration": lambda: calibration.run(dry_run=dry_run),
        "simpost": lambda: simpost.run(dry_run=dry_run),
        "corpus": lambda: corpus.generate(site, dry_run=dry_run),
        "ingest": lambda: ingest.run(dry_run=dry_run),
        "seed_rules": lambda: _seed_rules(dry_run),
    }
    if name not in once:
        raise SystemExit(f"unknown task {name!r}; use --list")
    return await once[name]()


async def _seed_rules(dry_run: bool) -> list[str]:
    from . import rules

    if dry_run:
        have = {r["code"] for r in await db.fetch_all("SELECT code FROM analytics.fault_rules")}
        return [r.code for r in rules.RULES if r.code not in have]
    return await rules.seed_rules()


LOOP_TASKS = ("anomaly", "faults", "instructions", "optimizer", "manual_runs", "validations", "expiry", "surrogate", "calibration", "simpost", "corpus", "ingest")
TASK_NAMES = ("anomaly", "faults", "instructions", "optimizer", "manual_runs", "validations", "expiry", "surrogate", "calibration", "simpost", "corpus", "ingest", "seed_rules")


async def _cli(args: argparse.Namespace) -> int:
    try:
        for name in args.once:
            started = time.monotonic()
            result = await run_once(name, site=args.site, dry_run=args.dry_run)
            print(json.dumps({"task": name, "seconds": round(time.monotonic() - started, 2), "result": data.jsonable(result)}, default=str, ensure_ascii=False, indent=1))
        return 0
    finally:
        await backend.close_client()
        await db.close_pools()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="afdd_ai.analytics.worker", description="AFDD AI analytics worker")
    parser.add_argument("--once", action="append", default=[], metavar="TASK", help="run one iteration of a task and exit (repeatable)")
    parser.add_argument("--dry-run", action="store_true", help="with --once: compute and report without writing anything")
    parser.add_argument("--site", default=None, help="site code (default SITE_CODE)")
    parser.add_argument("--list", action="store_true", help="list the task names")
    args = parser.parse_args(argv)
    setup_logging("ai-analytics")
    if args.list:
        print("\n".join(TASK_NAMES))
        return 0
    if sys.platform == "win32" and hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")  # type: ignore[union-attr]
    if args.once:
        return aio_run(_cli(args))
    try:
        aio_run(Worker(args.site).run())
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
