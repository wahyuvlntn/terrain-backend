"""Client for the AFDD backend script API, acting as the end user.

The agent and the MCP servers always call the backend with the **user's own
session token**, so permissions and location scope apply to the LLM exactly as
to the user. Every call carries `X-AFDD-Origin: agent`; the backend refuses
human-only actions (plant writes, approvals) for such calls.
"""

from __future__ import annotations

import logging
from typing import Any

import httpx

from .config import get_settings

log = logging.getLogger(__name__)

SESSION_HEADER = "X-AFDD-Session-Id"
ORIGIN_HEADER = "X-AFDD-Origin"


class BackendError(Exception):
    """A backend call failed (status = HTTP status, message = backend error text)."""

    def __init__(self, status: int, message: str):
        super().__init__(f"{status}: {message}")
        self.status = status
        self.message = message


_client: httpx.AsyncClient | None = None


def _http() -> httpx.AsyncClient:
    global _client
    if _client is None:
        s = get_settings()
        _client = httpx.AsyncClient(
            base_url=s.api_url,
            verify=s.api_verify_tls,
            timeout=httpx.Timeout(30.0, connect=10.0),
            limits=httpx.Limits(max_connections=20, max_keepalive_connections=10),
        )
    return _client


async def close_client() -> None:
    global _client
    if _client is not None:
        await _client.aclose()
        _client = None


class BackendClient:
    """Calls `/api/js-service/exposed/execute/{org}/{project}/{script}` as one user."""

    def __init__(self, session_token: str, *, origin: str = "agent", timeout: float = 30.0):
        if not session_token:
            raise BackendError(401, "No user session")
        self._token = session_token
        self._origin = origin
        self._timeout = timeout

    async def call(self, script: str, data: dict[str, Any] | None = None) -> Any:
        """POST a JSON body to a script and return the unwrapped `data`."""
        s = get_settings()
        path = f"/api/js-service/exposed/execute/{s.org_alias}/{s.project_alias}/{script.strip('/')}"
        try:
            resp = await _http().post(
                path,
                json=data or {},
                headers={SESSION_HEADER: self._token, ORIGIN_HEADER: self._origin},
                timeout=self._timeout,
            )
        except httpx.HTTPError as err:
            raise BackendError(503, f"backend unreachable: {err}") from err
        try:
            body = resp.json()
        except ValueError:
            raise BackendError(resp.status_code, resp.text[:300]) from None
        if resp.status_code >= 400 or (isinstance(body, dict) and body.get("error")):
            msg = body.get("error") or body.get("message") if isinstance(body, dict) else str(body)
            raise BackendError(resp.status_code, str(msg or "error"))
        return body.get("data") if isinstance(body, dict) and "data" in body else body

    async def user_details(self) -> dict[str, Any]:
        """`user/getuserdetails`: {full_name, role, assignedLocations, permissions[{alias, group_alias}]}."""
        return await self.call("user/getuserdetails")


def has_permission(details: dict[str, Any], group: str, alias: str) -> bool:
    """Permission check on a user-details payload (admins and superusers have everything)."""
    if details.get("role") in ("admin", "superuser"):
        return True
    return any(p.get("group_alias") == group and p.get("alias") == alias for p in details.get("permissions") or [])
