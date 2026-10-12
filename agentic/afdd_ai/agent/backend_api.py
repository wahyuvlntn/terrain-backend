"""Backend access for agent tools: the user's own session, clear errors, name resolution.

Every tool call goes to the AFDD backend with the end user's session token (read
from `temp:` state, never from the model) and `X-AFDD-Origin: agent`, so the
user's permissions and location scope apply to the model exactly as to the user.
Backend failures become `ToolError`s whose text is written for the model (and
the person reading the answer), e.g. "the user is not allowed to …".
"""

from __future__ import annotations

import hashlib
import logging
import math
import re
import time
from collections import OrderedDict
from collections.abc import Mapping
from datetime import datetime, timezone
from functools import lru_cache
from typing import Any
from zoneinfo import ZoneInfo

from ..common.backend import BackendClient, BackendError
from ..common.config import get_settings
from . import state as st

log = logging.getLogger(__name__)


class ToolError(Exception):
    """A tool failure explained for the model; tools return it as {"error": text}."""


def error_result(err: Exception) -> dict[str, Any]:
    if isinstance(err, ToolError):
        return {"error": str(err)}
    log.exception("agent tool failed", exc_info=err)
    return {"error": "The tool failed unexpectedly; tell the user the data could not be retrieved right now."}


# Translation keys the backend returns as error text.
_BACKEND_MESSAGES = {
    "err_asset_not_found": "equipment not found",
    "err_something_went_wrong": "internal backend error",
    "unauthorized, please re-login": "the user's session has expired",
}


def _backend_text(message: str) -> str:
    msg = (message or "").strip()
    return _BACKEND_MESSAGES.get(msg.lower(), msg) or "no details"


def map_backend_error(err: BackendError, what: str) -> ToolError:
    msg = _backend_text(err.message)
    if err.status == 401:
        return ToolError("The user's session has expired or is invalid. Ask the user to sign in to the application again.")
    if err.status == 403:
        return ToolError(f"The user is not allowed to access {what} ({msg}). Do not retry; tell the user which permission or location access is missing.")
    if err.status == 404:
        return ToolError(f"Not found: {what} ({msg}). The data may not exist, or this feature is not available on the server.")
    if err.status in (400, 409, 422):
        return ToolError(f"The application rejected the request for {what}: {msg}. Check the arguments.")
    if err.status in (499, 503, 502, 504):
        return ToolError(f"The application backend is unavailable right now ({msg}); {what} could not be retrieved.")
    return ToolError(f"The application could not return {what} (HTTP {err.status}: {msg}).")


# ---------------------------------------------------------------------------
# Small per-user TTL cache (keyed by a hash of the session token).
# ---------------------------------------------------------------------------

_CACHE_MAX = 512
_cache: OrderedDict[tuple[str, str], tuple[float, Any]] = OrderedDict()


def _token_key(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()[:32]


def cache_get(token: str, key: str) -> Any | None:
    k = (_token_key(token), key)
    hit = _cache.get(k)
    if hit is None:
        return None
    expires, value = hit
    if expires < time.monotonic():
        _cache.pop(k, None)
        return None
    _cache.move_to_end(k)
    return value


def cache_put(token: str, key: str, value: Any, ttl_s: float) -> None:
    _cache[(_token_key(token), key)] = (time.monotonic() + ttl_s, value)
    _cache.move_to_end((_token_key(token), key))
    while len(_cache) > _CACHE_MAX:
        _cache.popitem(last=False)


def clear_cache() -> None:
    _cache.clear()


# ---------------------------------------------------------------------------
# Formatting helpers
# ---------------------------------------------------------------------------


@lru_cache(maxsize=1)
def plant_tz() -> ZoneInfo:
    try:
        return ZoneInfo(get_settings().timezone)
    except Exception:  # noqa: BLE001 - unknown zone name: fall back to UTC
        return ZoneInfo("UTC")


def parse_ts(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)


def local_ts(value: Any) -> Any:
    """Backend UTC ISO time -> plant-local ISO minute with offset (for the model)."""
    dt = parse_ts(value)
    if dt is None:
        return value
    return dt.astimezone(plant_tz()).isoformat(timespec="minutes")


def iso_utc(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def now_utc() -> datetime:
    return datetime.now(timezone.utc)


def rnd(value: Any, digits: int = 3) -> Any:
    """Round floats for compact output (non-finite numbers become None)."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            return None
        return round(value, digits)
    return value


def compact(value: Any, *, max_items: int = 50, max_str: int = 600, depth: int = 0) -> Any:
    """Bounded copy of a JSON-like value (lists truncated, long text cut, floats rounded)."""
    if depth > 6:
        return "…"
    if isinstance(value, Mapping):
        return {str(k): compact(v, max_items=max_items, max_str=max_str, depth=depth + 1) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        items = [compact(v, max_items=max_items, max_str=max_str, depth=depth + 1) for v in list(value)[:max_items]]
        if len(value) > max_items:
            items.append(f"… {len(value) - max_items} more not shown")
        return items
    if isinstance(value, str):
        return value if len(value) <= max_str else value[: max_str - 1] + "…"
    return rnd(value)


_ALIAS_RE = re.compile(r"^([A-Z]+)_?0*(\d{1,3})$")


def normalise_alias(ref: str) -> str:
    """'ch-1', 'CH 01', 'ch_1' -> 'CH_01'; other references are upper-cased with '_' separators."""
    s = re.sub(r"[\s\-]+", "_", (ref or "").strip().upper())
    m = _ALIAS_RE.match(s)
    if m:
        return f"{m.group(1)}_{int(m.group(2)):02d}"
    return s


# ---------------------------------------------------------------------------
# API wrapper bound to one tool call's user
# ---------------------------------------------------------------------------

ASSET_CACHE_S = 30.0


class Api:
    """Backend calls as the user of the current tool call.

    origin "agent" (default) marks the call as the assistant's; "" as the person's own (a form they
    submitted in the chat, carried out by the service).
    """

    def __init__(self, state: Mapping[str, Any], origin: str = "agent"):
        token = st.session_token(state)
        if not token:
            raise ToolError("No user session is available for this request, so no plant data can be read. Ask the user to sign in again.")
        self.token = token
        self.permissions = st.state_permissions(state)
        self._client = BackendClient(token, origin=origin)

    def can(self, permission: str) -> bool:
        return st.has_permission(self.permissions, permission)

    async def call(self, script: str, data: dict[str, Any] | None = None, *, what: str | None = None) -> Any:
        try:
            return await self._client.call(script, data or {})
        except BackendError as err:
            log.info("backend call failed script=%s status=%s msg=%s", script, err.status, err.message)
            raise map_backend_error(err, what or script) from None

    # -- equipment ---------------------------------------------------------

    async def assets(self) -> list[dict[str, Any]]:
        """Visible equipment [{id, alias, name, type_name, model_name, location_id, status, condition, …}]."""
        cached = cache_get(self.token, "assets")
        if cached is not None:
            return cached
        try:
            rows = await self._client.call("digital_twin/getassets", {})
        except BackendError as err:
            if err.status != 403:
                raise map_backend_error(err, "the equipment list") from None
            rows = await self._assets_fallback()
        rows = [r for r in (rows or []) if isinstance(r, dict)]
        cache_put(self.token, "assets", rows, ASSET_CACHE_S)
        return rows

    async def _assets_fallback(self) -> list[dict[str, Any]]:
        """Without digitaltwin.read: equipment names from the location filter tree (no alias / status)."""
        tree = await self.call("filter/getalllocationsandassets", {}, what="the equipment list")
        out: list[dict[str, Any]] = []
        for loc in tree or []:
            for a in loc.get("assets") or []:
                out.append({"id": a.get("id"), "alias": None, "name": a.get("name"), "location_id": loc.get("id")})
        return out

    async def resolve_asset(self, ref: str | int) -> dict[str, Any]:
        """Equipment by alias ('CH_01', 'ch-1'), id, or (unique) name."""
        assets = await self.assets()
        if isinstance(ref, int) or (isinstance(ref, str) and ref.strip().isdigit()):
            want_id = int(ref)
            for a in assets:
                if a.get("id") == want_id:
                    return a
        text = str(ref or "").strip()
        if not text:
            raise ToolError("An equipment alias is required (for example CH_01, CT_02, CHWP_01, CHWS_01).")
        alias = normalise_alias(text)
        for a in assets:
            if (a.get("alias") or "").upper() == alias:
                return a
        lowered = text.lower()
        exact = [a for a in assets if (a.get("name") or "").lower() == lowered]
        if len(exact) == 1:
            return exact[0]
        partial = [a for a in assets if lowered in (a.get("name") or "").lower()]
        if len(partial) == 1:
            return partial[0]
        known = ", ".join(sorted(str(a.get("alias") or a.get("name")) for a in assets)[:60])
        if len(partial) > 1:
            raise ToolError(f"'{text}' matches several units ({', '.join(str(a.get('alias') or a.get('name')) for a in partial[:10])}); use the alias.")
        raise ToolError(f"No visible equipment matches '{text}'. Visible units: {known or 'none (no location access)'}.")

    async def resolve_assets(self, refs: list[str] | None) -> list[dict[str, Any]]:
        if not refs:
            return []
        out: list[dict[str, Any]] = []
        seen: set[Any] = set()
        for ref in refs[:60]:
            a = await self.resolve_asset(ref)
            if a.get("id") not in seen:
                seen.add(a.get("id"))
                out.append(a)
        return out

    # -- parameters --------------------------------------------------------

    async def parameters(self, asset: Mapping[str, Any]) -> list[dict[str, Any]]:
        label = asset.get("alias") or asset.get("name")
        rows = await self.call("digital_twin/getassetparameters", {"asset_id": asset.get("id")}, what=f"the parameters of {label}")
        return [r for r in (rows or []) if isinstance(r, dict)]

    async def resolve_parameter(self, asset: Mapping[str, Any], ref: str, params: list[dict[str, Any]] | None = None) -> dict[str, Any]:
        """Parameter of an equipment by alias ('chwst') or name ('CHW Leaving Temp')."""
        params = params if params is not None else await self.parameters(asset)
        text = (ref or "").strip()
        if not text:
            raise ToolError("A parameter alias is required (for example chwst, power_kw, fan_speed).")
        want = text.lower().replace(" ", "_")
        for p in params:
            if (p.get("alias") or "").lower() == want:
                return p
        lowered = text.lower()
        exact = [p for p in params if (p.get("name") or "").lower() == lowered]
        if len(exact) == 1:
            return exact[0]
        partial = [p for p in params if lowered in (p.get("name") or "").lower()]
        if len(partial) == 1:
            return partial[0]
        known = ", ".join(str(p.get("alias")) for p in params)
        raise ToolError(f"{asset.get('alias') or asset.get('name')} has no parameter '{text}'. Its parameters: {known}.")

    async def trend(self, parameter_id: int, minutes: int, what: str) -> dict[str, Any]:
        data = await self.call("digital_twin/getparametertrend", {"parameter_id": parameter_id, "minutes": minutes}, what=what)
        return data if isinstance(data, dict) else {}
