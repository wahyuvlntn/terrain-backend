"""Per-request identity and the session-state keys shared by the agent modules.

The backend proxy (`/api/agent/*`) authenticates the end user and forwards the
identity in `X-AFDD-*` headers. Each chat turn passes it to ADK as `temp:`
state, which ADK applies to the in-memory session (so tools and callbacks can
read it) and strips before anything is persisted. The user's session token is
therefore never stored and never appears in a prompt or a tool declaration.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from typing import Any

# temp: keys (per request, never persisted).
SESSION_TOKEN = "temp:afdd_session"
USER_ID = "temp:user_id"
PERMISSIONS = "temp:permissions"
USERNAME = "temp:username"
USER_NAME = "temp:user_name"

# Persisted session state.
TITLE = "title"
OWNER = "owner"

# Every agent user needs this permission (the proxy checks it too).
REQUIRED_PERMISSION = "aiassistant.read"
# What the proxy sends instead of a permission list for admins and superusers (they hold every permission).
ADMIN = "admin"


@dataclass(frozen=True)
class Identity:
    """The end user a request acts for (from the backend proxy headers)."""

    user_id: int
    username: str
    name: str
    session_token: str = field(repr=False)
    permissions: frozenset[str] = frozenset()

    @property
    def is_admin(self) -> bool:
        return ADMIN in self.permissions

    def can(self, permission: str) -> bool:
        """`permission` is "group.alias"; admins can do everything."""
        return has_permission(self.permissions, permission)

    def state_delta(self) -> dict[str, Any]:
        """The per-request temp state handed to ADK on every run."""
        return {
            SESSION_TOKEN: self.session_token,
            USER_ID: self.user_id,
            PERMISSIONS: sorted(self.permissions),
            USERNAME: self.username,
            USER_NAME: self.name,
        }


def parse_permissions(raw: str | Iterable[str] | None) -> frozenset[str]:
    """CSV (`group.alias,group.alias`) or an iterable to a normalised set."""
    if raw is None:
        return frozenset()
    items = raw.split(",") if isinstance(raw, str) else list(raw)
    return frozenset(p.strip().lower() for p in items if p and p.strip())


def has_permission(permissions: frozenset[str], permission: str) -> bool:
    """`permission` ("group.alias") is held, or the user is an admin (the proxy sends just "admin")."""
    return ADMIN in permissions or permission.lower() in permissions


def session_token(state: Mapping[str, Any]) -> str:
    return str(state.get(SESSION_TOKEN) or "")


def state_user_id(state: Mapping[str, Any]) -> int | None:
    value = state.get(USER_ID)
    try:
        return int(value) if value is not None else None
    except (TypeError, ValueError):
        return None


def state_permissions(state: Mapping[str, Any]) -> frozenset[str]:
    return parse_permissions(state.get(PERMISSIONS) or [])
