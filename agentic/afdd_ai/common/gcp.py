"""Google credentials of the attached service account (Cloud Run metadata server).

Access tokens call Google APIs (Cloud Storage); ID tokens call private Cloud Run
services (the MCP servers), sent as `X-Serverless-Authorization` so the
application's own `Authorization` header stays free. Both are cached until
shortly before they expire.
"""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from urllib.parse import quote, urlsplit

import httpx

REFRESH_BEFORE_S = 300.0
_lock = threading.Lock()
_access: tuple[str, float] = ("", 0.0)
_ids: dict[str, tuple[str, float]] = {}


def _metadata_base() -> str:
    host = os.environ.get("GCE_METADATA_HOST", "").strip()
    return f"http://{host}" if host else "http://metadata.google.internal"


def _get(path: str) -> str:
    resp = httpx.get(f"{_metadata_base()}/computeMetadata/v1/{path}", headers={"Metadata-Flavor": "Google"}, timeout=10.0)
    resp.raise_for_status()
    return resp.text


def access_token() -> str:
    """An OAuth access token (GOOGLE_OAUTH_ACCESS_TOKEN overrides it for local tests)."""
    global _access
    override = os.environ.get("GOOGLE_OAUTH_ACCESS_TOKEN", "").strip()
    if override:
        return override
    with _lock:
        tok, exp = _access
        if tok and exp - time.time() > REFRESH_BEFORE_S:
            return tok
    data = json.loads(_get("instance/service-accounts/default/token"))
    tok = str(data["access_token"])
    with _lock:
        _access = (tok, time.time() + float(data.get("expires_in", 3600)))
    return tok


def _jwt_expiry(tok: str) -> float:
    try:
        payload = tok.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        return float(json.loads(base64.urlsafe_b64decode(payload))["exp"])
    except (IndexError, KeyError, ValueError):
        return 0.0


def id_token(audience: str) -> str:
    """A Google-signed ID token for a Cloud Run service (audience: its root URL)."""
    with _lock:
        tok, exp = _ids.get(audience, ("", 0.0))
        if tok and exp - time.time() > REFRESH_BEFORE_S:
            return tok
    tok = _get(f"instance/service-accounts/default/identity?format=full&audience={quote(audience, safe='')}").strip()
    exp = _jwt_expiry(tok)
    if not tok or not exp:
        raise RuntimeError("metadata server returned no ID token")
    with _lock:
        _ids[audience] = (tok, exp)
    return tok


def audience(url: str) -> str:
    """The root URL (scheme://host) of a service URL."""
    parts = urlsplit(url.strip())
    return f"{parts.scheme}://{parts.netloc}" if parts.netloc else url.rstrip("/")


def reset() -> None:
    """Forget cached tokens (tests)."""
    global _access
    with _lock:
        _access = ("", 0.0)
        _ids.clear()
