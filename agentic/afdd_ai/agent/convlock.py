"""One running turn per conversation across agent instances (Cloud Run replicas).

A lease row per conversation in `agent.conversation_locks`: taken when a turn
starts (only when free or expired), renewed while the turn streams and deleted
when it ends. An instance that dies mid-turn frees the conversation when the
lease expires (TTL_S). The in-process lock of the runner stays in front of it.
"""

from __future__ import annotations

import asyncio
import logging
import uuid

from ..common import db

log = logging.getLogger(__name__)

TTL_S = 120
RENEW_S = 30
HOLDER = uuid.uuid4().hex  # this process

_DDL = """
CREATE TABLE IF NOT EXISTS agent.conversation_locks (
    session_id TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
)"""
_ready = False


async def prepare() -> None:
    global _ready
    if not _ready:
        await db.execute(_DDL)
        _ready = True


async def acquire(session_id: str) -> bool:
    """Take the conversation's lease; False while another turn holds it."""
    await prepare()
    row = await db.fetch_one(
        """INSERT INTO agent.conversation_locks (session_id, holder, expires_at)
           VALUES (%s, %s, now() + make_interval(secs => %s))
           ON CONFLICT (session_id) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
           WHERE agent.conversation_locks.expires_at < now()
           RETURNING holder""",
        (session_id, HOLDER, TTL_S),
    )
    return row is not None


async def renew(session_id: str) -> None:
    await db.execute(
        "UPDATE agent.conversation_locks SET expires_at = now() + make_interval(secs => %s) WHERE session_id = %s AND holder = %s",
        (TTL_S, session_id, HOLDER),
    )


async def release(session_id: str) -> None:
    await db.execute("DELETE FROM agent.conversation_locks WHERE session_id = %s AND holder = %s", (session_id, HOLDER))


class Held:
    """A held conversation: the in-process lock plus the shared lease (renewed until released)."""

    def __init__(self, session_id: str, local: asyncio.Lock, shared: bool) -> None:
        self.session_id, self.local, self.shared = session_id, local, shared
        self._renew = asyncio.create_task(self._renewer(), name=f"lease-{session_id}") if shared else None

    async def _renewer(self) -> None:
        while True:
            await asyncio.sleep(RENEW_S)
            try:
                await renew(self.session_id)
            except Exception as err:  # noqa: BLE001 - the lease expires on its own
                log.warning("renew conversation lease %s: %s", self.session_id, err)

    def locked(self) -> bool:
        return self.local.locked()

    def release(self) -> None:
        """Synchronous (also from a finalizer): the shared lease is deleted in the background."""
        if self._renew is not None:
            self._renew.cancel()
            self._renew = None
            try:
                asyncio.get_running_loop().create_task(_release_quietly(self.session_id))
            except RuntimeError:
                pass  # no loop (interpreter shutdown): the lease expires
        if self.local.locked():
            self.local.release()


async def _release_quietly(session_id: str) -> None:
    try:
        await release(session_id)
    except Exception as err:  # noqa: BLE001 - the lease expires on its own
        log.warning("release conversation lease %s: %s", session_id, err)
