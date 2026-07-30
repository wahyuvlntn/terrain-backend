"""PostgreSQL access (psycopg 3 async pools, dict rows, pgvector adapter).

Two pools: the read-write service role (afdd_ai) and the read-only role
(afdd_ai_ro, used by the MCP database server). Keep pools small: the server has
max_connections 100 shared with the Go backend and simulator.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable, Sequence
from typing import Any

import psycopg
from pgvector.psycopg import register_vector_async
from psycopg import sql
from psycopg.rows import dict_row
from psycopg_pool import AsyncConnectionPool

from .config import get_settings

log = logging.getLogger(__name__)

_pools: dict[str, AsyncConnectionPool] = {}
_lock = asyncio.Lock()


async def _configure(conn: psycopg.AsyncConnection) -> None:
    conn.row_factory = dict_row  # type: ignore[assignment]
    try:
        await register_vector_async(conn)
    except psycopg.ProgrammingError:
        # vector extension not installed yet (pre-migration); plain queries still work.
        log.warning("pgvector type not available on this connection")
    await conn.execute("SET TIME ZONE 'UTC'")
    await conn.commit()


async def get_pool(ro: bool = False, *, min_size: int = 1, max_size: int = 5) -> AsyncConnectionPool:
    """Shared pool (created on first use)."""
    key = "ro" if ro else "rw"
    async with _lock:
        pool = _pools.get(key)
        if pool is None:
            s = get_settings()
            pool = AsyncConnectionPool(
                conninfo=s.database_url_ro if ro else s.database_url,
                min_size=min_size,
                max_size=max_size,
                configure=_configure,
                kwargs={"autocommit": True},
                open=False,
                name=f"afdd-{key}",
            )
            await pool.open(wait=True, timeout=60)
            _pools[key] = pool
        return pool


async def close_pools() -> None:
    for pool in list(_pools.values()):
        await pool.close()
    _pools.clear()


async def fetch_all(sql: str, params: Sequence[Any] | dict[str, Any] | None = None, *, ro: bool = False) -> list[dict[str, Any]]:
    pool = await get_pool(ro)
    async with pool.connection() as conn:
        cur = await conn.execute(sql, params)
        return list(await cur.fetchall()) if cur.description else []


async def fetch_one(sql: str, params: Sequence[Any] | dict[str, Any] | None = None, *, ro: bool = False) -> dict[str, Any] | None:
    pool = await get_pool(ro)
    async with pool.connection() as conn:
        cur = await conn.execute(sql, params)
        return await cur.fetchone() if cur.description else None


async def fetch_val(sql: str, params: Sequence[Any] | dict[str, Any] | None = None, *, ro: bool = False) -> Any:
    row = await fetch_one(sql, params, ro=ro)
    return next(iter(row.values())) if row else None


async def execute(sql: str, params: Sequence[Any] | dict[str, Any] | None = None) -> int:
    """Run a statement on the read-write pool; returns the affected row count."""
    pool = await get_pool(False)
    async with pool.connection() as conn:
        cur = await conn.execute(sql, params)
        return cur.rowcount


async def notify(channel: str, payload: str = "") -> None:
    await execute("SELECT pg_notify(%s, %s)", (channel, payload))


async def listen(channels: Sequence[str], handler: Callable[[str, str], Awaitable[None]], stop: asyncio.Event | None = None) -> None:
    """LISTEN on channels with a dedicated connection, reconnecting on failure.

    handler(channel, payload) is awaited for each notification; exceptions are logged.
    """
    s = get_settings()
    while stop is None or not stop.is_set():
        try:
            async with await psycopg.AsyncConnection.connect(s.database_url, autocommit=True) as conn:
                for ch in channels:
                    await conn.execute(sql.SQL("LISTEN {}").format(sql.Identifier(ch)))
                log.info("listening channels=%s", ",".join(channels))
                async for note in conn.notifies():
                    try:
                        await handler(note.channel, note.payload)
                    except Exception:  # noqa: BLE001 - keep listening
                        log.exception("notification handler failed channel=%s", note.channel)
                    if stop is not None and stop.is_set():
                        return
        except (psycopg.OperationalError, OSError) as err:
            log.warning("listen connection lost: %s (retrying in 5 s)", err)
            await asyncio.sleep(5)
