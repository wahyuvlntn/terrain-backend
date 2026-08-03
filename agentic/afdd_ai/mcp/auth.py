"""Authentication of the MCP servers (afdd-db, afdd-twin).

Callers (the ai-agent's McpToolsets) send the end user's session as
`Authorization: Bearer <token>` and the shared `X-Agent-Secret`. The ASGI
middleware rejects a request before it reaches MCP unless

* the secret matches (constant-time compare; an unset secret rejects everything),
* the token is a live backend session (`user/getuserdetails`, cached 60 s), and
* the user holds `aiassistant.read`.

Tools then read the same identity with `current_user(ctx)` (a cache hit) and get
its location scope: the assigned locations with all their descendants (from the
backend), or None for admins (unrestricted).
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import logging
import time
from collections import OrderedDict
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from typing import Any

from mcp.server.mcpserver import Context
from mcp.server.mcpserver.exceptions import ToolError

from ..common.backend import BackendClient, BackendError
from ..common.config import get_settings

log = logging.getLogger(__name__)

SECRET_HEADER = "x-agent-secret"
AGENT_SESSION_HEADER = "x-afdd-agent-session"
USER_ID_HEADER = "x-afdd-user-id"
REQUIRED_PERMISSION = "aiassistant.read"
USER_CACHE_S = 60.0
NEGATIVE_CACHE_S = 5.0
CACHE_MAX = 1024
PUBLIC_PATHS = frozenset({"/healthz"})


class AuthError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


@dataclass(frozen=True)
class McpUser:
    """The end user an MCP request acts for."""

    token: str = field(repr=False)
    role: str
    full_name: str
    permissions: frozenset[str]
    # Visible location ids (descendants included); None = unrestricted (admin / superuser).
    locations: tuple[int, ...] | None
    # From user/getuserdetails (None with a backend that does not send them).
    user_id: int | None = None
    username: str | None = None

    @property
    def is_admin(self) -> bool:
        return self.role in ("admin", "superuser")

    def can(self, permission: str) -> bool:
        return self.is_admin or permission in self.permissions

    def location_list(self) -> list[int] | None:
        """Scope as a SQL array parameter (None = no restriction)."""
        return None if self.locations is None else list(self.locations)


def user_from_details(token: str, details: Mapping[str, Any]) -> McpUser:
    role = str(details.get("role") or "basic")
    perms = frozenset(
        f"{p.get('group_alias')}.{p.get('alias')}".lower()
        for p in (details.get("permissions") or [])
        if isinstance(p, Mapping) and p.get("group_alias") and p.get("alias")
    )
    locations: tuple[int, ...] | None
    if role in ("admin", "superuser"):
        locations = None
    else:
        locations = tuple(sorted({int(x) for x in (details.get("assignedLocations") or []) if str(x).lstrip("-").isdigit()}))
    raw_id = details.get("id")
    user_id = int(raw_id) if isinstance(raw_id, (int, float, str)) and str(raw_id).isdigit() and int(raw_id) > 0 else None
    username = str(details.get("username") or "").strip() or None
    return McpUser(
        token=token, role=role, full_name=str(details.get("full_name") or ""), permissions=perms, locations=locations,
        user_id=user_id, username=username,
    )  # fmt: skip


async def fetch_user_details(token: str) -> dict[str, Any]:
    """`user/getuserdetails` as the token's user (tests replace this)."""
    details = await BackendClient(token, timeout=10.0).user_details()
    if not isinstance(details, dict):
        raise BackendError(502, "unexpected user details payload")
    return details


# token hash -> (expires, user or AuthError)
_cache: OrderedDict[str, tuple[float, McpUser | AuthError]] = OrderedDict()
_locks: dict[str, asyncio.Lock] = {}


def clear_cache() -> None:
    _cache.clear()
    _locks.clear()


def _key(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


async def authenticate(token: str) -> McpUser:
    """Validate a user session token (cached); raises AuthError."""
    if not token:
        raise AuthError(401, "Missing bearer token")
    key = _key(token)
    hit = _cache.get(key)
    if hit and hit[0] > time.monotonic():
        if isinstance(hit[1], AuthError):
            raise hit[1]
        return hit[1]
    lock = _locks.setdefault(key, asyncio.Lock())
    async with lock:
        hit = _cache.get(key)
        if hit and hit[0] > time.monotonic():
            if isinstance(hit[1], AuthError):
                raise hit[1]
            return hit[1]
        try:
            details = await fetch_user_details(token)
        except BackendError as err:
            if err.status in (401, 403):
                failure = AuthError(401, "Invalid or expired user session")
                _store(key, failure, NEGATIVE_CACHE_S)
                raise failure from None
            log.warning("user validation failed status=%s msg=%s", err.status, err.message)
            raise AuthError(503, "Cannot validate the user session right now") from None
        finally:
            _locks.pop(key, None)
        user = user_from_details(token, details)
        _store(key, user, USER_CACHE_S)
        return user


def _store(key: str, value: McpUser | AuthError, ttl: float) -> None:
    _cache[key] = (time.monotonic() + ttl, value)
    _cache.move_to_end(key)
    while len(_cache) > CACHE_MAX:
        _cache.popitem(last=False)


def secret_ok(value: str | None) -> bool:
    secret = get_settings().agent_secret
    return bool(secret) and bool(value) and hmac.compare_digest(str(value).encode(), secret.encode())


def bearer(value: str | None) -> str:
    if not value:
        return ""
    scheme, _, token = value.strip().partition(" ")
    return token.strip() if scheme.lower() == "bearer" else ""


async def authorize(headers: Mapping[str, str]) -> McpUser:
    """Secret + session + required permission; raises AuthError."""
    if not secret_ok(headers.get(SECRET_HEADER)):
        raise AuthError(401, "Invalid agent secret")
    user = await authenticate(bearer(headers.get("authorization")))
    if not user.can(REQUIRED_PERMISSION):
        raise AuthError(403, "The AI assistant is not enabled for this user")
    return user


async def current_user(ctx: Context) -> McpUser:
    """The authenticated user of a tool call (the middleware has validated it already)."""
    headers = ctx.headers
    if headers is None:
        raise ToolError("This server requires the streamable HTTP transport")
    try:
        return await authorize(headers)
    except AuthError as err:
        raise ToolError(err.message) from None


def agent_session_id(ctx: Context) -> str | None:
    headers = ctx.headers or {}
    value = (headers.get(AGENT_SESSION_HEADER) or "").strip()
    return value[:128] or None


def request_user_id(ctx: Context) -> int | None:
    """The user id the agent sends in `X-AFDD-User-Id` (from the backend proxy), if any.

    `user/getuserdetails` does not return the id. It is only used to show users the simulation
    jobs they requested themselves; permissions and scope always come from the session token.
    """
    value = ((ctx.headers or {}).get(USER_ID_HEADER) or "").strip()
    return int(value) if value.isdigit() and 0 < int(value) < 2**63 else None


ASGIApp = Callable[[dict[str, Any], Callable[[], Awaitable[dict[str, Any]]], Callable[[dict[str, Any]], Awaitable[None]]], Awaitable[None]]


class McpAuthMiddleware:
    """Rejects unauthenticated HTTP requests before they reach the MCP transport."""

    def __init__(self, app: ASGIApp):
        self.app = app

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        if scope.get("type") != "http" or scope.get("path") in PUBLIC_PATHS:
            await self.app(scope, receive, send)
            return
        headers = {k.decode("latin-1").lower(): v.decode("latin-1") for k, v in scope.get("headers") or []}
        try:
            await authorize(headers)
        except AuthError as err:
            body = json.dumps({"error": err.message}).encode()
            extra = [(b"www-authenticate", b"Bearer")] if err.status == 401 else []
            await send(
                {
                    "type": "http.response.start",
                    "status": err.status,
                    "headers": [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode()), *extra],
                }
            )
            await send({"type": "http.response.body", "body": body})
            return
        await self.app(scope, receive, send)
