"""Shared helpers of the MCP servers: bounded read-only SQL, time arguments, JSON output."""

from __future__ import annotations

import logging
import math
import re
from collections.abc import Mapping, Sequence
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
from functools import lru_cache
from typing import Any
from zoneinfo import ZoneInfo

import psycopg
from mcp.server.mcpserver.exceptions import ToolError

from ..common import db
from ..common.config import get_settings

log = logging.getLogger(__name__)

MAX_ROWS = 5000
STATEMENT_TIMEOUT = "5s"

_RELATIVE = re.compile(r"^\s*-\s*(\d+(?:\.\d+)?)\s*(m|min|h|d|w)\s*$", re.I)
_UNITS = {"m": 60, "min": 60, "h": 3600, "d": 86400, "w": 604800}


@lru_cache(maxsize=1)
def plant_tz() -> ZoneInfo:
    try:
        return ZoneInfo(get_settings().timezone)
    except Exception:  # noqa: BLE001 - unknown zone: UTC
        return ZoneInfo("UTC")


def now() -> datetime:
    return datetime.now(timezone.utc)


def parse_time(value: str | None, default: datetime, name: str) -> datetime:
    """ISO 8601 (naive = plant time), "now", or a relative "-30m" / "-6h" / "-7d" / "-2w"."""
    if value is None or not str(value).strip():
        return default
    text = str(value).strip()
    if text.lower() == "now":
        return now()
    m = _RELATIVE.match(text)
    if m:
        return now() - timedelta(seconds=float(m.group(1)) * _UNITS[m.group(2).lower()])
    try:
        dt = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        raise ToolError(f"{name} must be ISO 8601 (e.g. 2026-10-10T08:00), 'now' or relative like '-24h' / '-7d'") from None
    return dt if dt.tzinfo else dt.replace(tzinfo=plant_tz())


def window(start: str | None, end: str | None, *, default_hours: float, max_days: float) -> tuple[datetime, datetime]:
    t_end = parse_time(end, now(), "end")
    t_start = parse_time(start, t_end - timedelta(hours=default_hours), "start")
    if t_start >= t_end:
        raise ToolError("start must be before end")
    if t_end - t_start > timedelta(days=max_days):
        raise ToolError(f"The window may span at most {max_days:g} days")
    return t_start, t_end


def local_iso(dt: datetime | None) -> str | None:
    if dt is None:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(plant_tz()).isoformat(timespec="seconds")


def local_text(value: Any) -> Any:
    """A backend UTC ISO timestamp string -> plant-local ISO; anything else unchanged."""
    if not isinstance(value, str) or not value:
        return value
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return value
    return local_iso(dt)


def jsonable(value: Any, digits: int = 4) -> Any:
    """DB values -> JSON (plant-local times, rounded floats, no NaN)."""
    if value is None or isinstance(value, (bool, str, int)):
        return value
    if isinstance(value, float):
        return round(value, digits) if math.isfinite(value) else None
    if isinstance(value, Decimal):
        return jsonable(float(value), digits)
    if isinstance(value, datetime):
        return local_iso(value)
    if isinstance(value, date):
        return value.isoformat()
    if isinstance(value, Mapping):
        return {str(k): jsonable(v, digits) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [jsonable(v, digits) for v in value]
    return str(value)


def clamp(value: Any, lo: int, hi: int, default: int) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError):
        return default
    return max(lo, min(v, hi))


def norm_aliases(aliases: Sequence[str] | str | None) -> list[str] | None:
    if aliases is None:
        return None
    items = aliases.split(",") if isinstance(aliases, str) else list(aliases)
    out = [re.sub(r"[\s\-]+", "_", str(a).strip().upper()) for a in items if str(a).strip()]
    return out[:60] or None


async def ro_query(sql: str, params: Mapping[str, Any] | None = None, *, max_rows: int = MAX_ROWS) -> tuple[list[dict[str, Any]], bool]:
    """Run one read-only statement (role afdd_ai_ro, 5 s timeout); returns (rows, truncated)."""
    pool = await db.get_pool(ro=True)
    try:
        async with pool.connection() as conn:
            async with conn.transaction():
                await conn.execute("SET TRANSACTION READ ONLY")
                await conn.execute(f"SET LOCAL statement_timeout = '{STATEMENT_TIMEOUT}'")
                cur = await conn.execute(sql, params)
                rows = await cur.fetchmany(max_rows + 1) if cur.description else []
    except psycopg.errors.QueryCanceled:
        raise ToolError("The query took longer than 5 s; narrow the time range or use a larger bucket") from None
    except psycopg.Error as err:
        log.exception("read-only query failed")
        raise ToolError(f"The database query failed ({type(err).__name__})") from None
    return list(rows[:max_rows]), len(rows) > max_rows
