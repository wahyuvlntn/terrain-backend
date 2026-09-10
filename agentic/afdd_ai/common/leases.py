"""One active instance of a continuous worker: a PostgreSQL session advisory lock.

The lock is held on a dedicated connection. A replacement instance that starts
while the old one still runs waits; when the holder dies or loses its
connection the lock is released and the waiting instance takes over.
"""

from __future__ import annotations

import asyncio
import logging

import psycopg

from .config import get_settings

log = logging.getLogger(__name__)

# pg advisory lock ids ("AFDD" + n); the Go processing worker uses ...0001
ANALYTICS_WORKER = 0x4146_4444_0002


class Lease:
    def __init__(self, conn: psycopg.AsyncConnection, key: int, name: str) -> None:
        self.conn, self.key, self.name = conn, key, name
        self.lost = asyncio.Event()
        self._monitor = asyncio.create_task(self._watch(), name=f"lease-{name}")

    async def _watch(self) -> None:
        fails = 0
        while True:
            await asyncio.sleep(5)
            try:
                cur = await self.conn.execute(
                    "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted"
                    " AND ((classid::bigint << 32) | objid::bigint) = %s)",
                    (self.key,),
                )
                row = await cur.fetchone()
                if row and row[0]:
                    fails = 0
                    continue
                raise RuntimeError("advisory lock no longer held")
            except Exception as err:  # noqa: BLE001 - any failure means the lock may be gone
                fails += 1
                log.warning("lease %s check failed (%d): %s", self.name, fails, err)
                if fails >= 2:
                    self.lost.set()
                    return

    async def release(self) -> None:
        self._monitor.cancel()
        try:
            await self.conn.execute("SELECT pg_advisory_unlock(%s)", (self.key,))
        except Exception:  # noqa: BLE001 - the connection may be gone already
            pass
        await self.conn.close()


async def acquire(key: int, name: str, stop: asyncio.Event | None = None, retry_s: float = 5.0) -> Lease | None:
    """Wait for the lock (None when `stop` is set first)."""
    waiting = False
    while stop is None or not stop.is_set():
        try:
            conn = await psycopg.AsyncConnection.connect(get_settings().database_url, autocommit=True)
            cur = await conn.execute("SELECT pg_try_advisory_lock(%s)", (key,))
            row = await cur.fetchone()
            if row and row[0]:
                log.info("lease %s acquired", name)
                return Lease(conn, key, name)
            await conn.close()
            if not waiting:
                waiting = True
                log.info("another instance holds lease %s, standing by", name)
        except Exception as err:  # noqa: BLE001 - retry until the database answers
            log.warning("lease %s: %s", name, err)
        try:
            await asyncio.wait_for(stop.wait() if stop else asyncio.sleep(retry_s), timeout=retry_s)
        except TimeoutError:
            pass
    return None
