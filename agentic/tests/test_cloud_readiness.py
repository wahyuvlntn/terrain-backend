"""Cloud Run readiness of the AI services: GCS document store, metadata-server tokens, TLS URL
mapping for asyncpg, ID tokens on MCP calls, conversation leases across instances."""

from __future__ import annotations

import base64
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlsplit

import pytest

from afdd_ai.common import config, db, gcp, storage


class _FakeGCS(BaseHTTPRequestHandler):
    """The part of the GCS JSON API the document store uses (plus a metadata server)."""

    buckets: set[str] = set()
    objects: dict[str, tuple[bytes, str]] = {}
    calls: list[str] = []

    def log_message(self, *args):  # noqa: ANN002 - quiet
        pass

    def _send(self, code: int, body: bytes = b"", ctype: str = "application/json") -> None:
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802
        u = urlsplit(self.path)
        q = parse_qs(u.query)
        if u.path.startswith("/computeMetadata/v1/"):
            if self.headers.get("Metadata-Flavor") != "Google":
                return self._send(403)
            self.calls.append(u.path)
            if u.path.endswith("/token"):
                return self._send(200, json.dumps({"access_token": "ya29.t", "expires_in": 3600}).encode())
            claims = base64.urlsafe_b64encode(json.dumps({"exp": time.time() + 3600, "aud": q["audience"][0]}).encode()).rstrip(b"=")
            return self._send(200, b"e30." + claims + b".sig", "text/plain")
        parts = u.path.split("/")
        bucket = unquote(parts[4])
        if bucket not in self.buckets:
            return self._send(404, b'{"error":{"message":"no bucket"}}')
        if u.path.endswith("/o"):
            return self._send(200, b'{"kind":"storage#objects"}')
        key = unquote(parts[6])
        obj = self.objects.get(f"{bucket}/{key}")
        if obj is None:
            return self._send(404, b'{"error":{"message":"not found"}}')
        return self._send(200, obj[0], obj[1])

    def do_POST(self):  # noqa: N802
        u = urlsplit(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if u.path == "/storage/v1/b":
            self.buckets.add(json.loads(body)["name"])
            return self._send(200, b"{}")
        bucket = unquote(u.path.split("/")[5])
        name = parse_qs(u.query)["name"][0]
        self.objects[f"{bucket}/{name}"] = (body, self.headers.get("Content-Type", ""))
        return self._send(200, json.dumps({"size": str(len(body))}).encode())

    def do_DELETE(self):  # noqa: N802
        parts = urlsplit(self.path).path.split("/")
        self.objects.pop(f"{unquote(parts[4])}/{unquote(parts[6])}", None)
        return self._send(204)


@pytest.fixture
def fake_gcs(monkeypatch):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _FakeGCS)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    host = f"127.0.0.1:{srv.server_address[1]}"
    _FakeGCS.buckets, _FakeGCS.objects, _FakeGCS.calls = set(), {}, []
    monkeypatch.setenv("STORAGE_PROVIDER", "gcs")
    monkeypatch.setenv("GCS_DOCS_BUCKET", "dt-docs")
    monkeypatch.setenv("STORAGE_EMULATOR_HOST", host)
    monkeypatch.setenv("GCE_METADATA_HOST", host)
    config.get_settings.cache_clear()
    gcp.reset()
    yield host
    srv.shutdown()
    config.get_settings.cache_clear()
    gcp.reset()


async def test_gcs_document_store(fake_gcs):
    await storage.ensure_bucket()
    assert _FakeGCS.buckets == {"dt-docs"}
    await storage.put_bytes("rag/2026/a b.pdf", b"%PDF-1", "application/pdf")
    assert await storage.get_bytes("rag/2026/a b.pdf") == b"%PDF-1"
    assert _FakeGCS.objects["dt-docs/rag/2026/a b.pdf"][1] == "application/pdf"
    await storage.delete("rag/2026/a b.pdf")
    await storage.delete("rag/2026/a b.pdf")  # a missing object is fine
    with pytest.raises(storage.ObjectNotFound):
        await storage.get_bytes("rag/2026/a b.pdf")


def test_metadata_tokens_are_cached(fake_gcs, monkeypatch):
    monkeypatch.delenv("GOOGLE_OAUTH_ACCESS_TOKEN", raising=False)
    assert gcp.access_token() == "ya29.t" == gcp.access_token()
    aud = gcp.audience("https://dt-mcp-db-abc.a.run.app/mcp")
    assert aud == "https://dt-mcp-db-abc.a.run.app"
    tok = gcp.id_token(aud)
    assert tok.count(".") == 2 and gcp.id_token(aud) == tok
    assert sum(c.endswith("/token") for c in _FakeGCS.calls) == 1
    assert sum(c.endswith("/identity") for c in _FakeGCS.calls) == 1


def test_asyncpg_url_maps_libpq_tls():
    url = config.asyncpg_url("postgresql://u:p%40ss@10.1.2.3:5432/afdd?sslmode=require&pool_max_conns=10")
    assert url == "postgresql+asyncpg://u:p%40ss@10.1.2.3:5432/afdd?ssl=require"
    assert config.asyncpg_url("postgres://u@h/db") == "postgresql+asyncpg://u@h/db"


def test_mcp_headers_add_a_cloud_run_id_token(monkeypatch):
    from afdd_ai.agent import agents

    s = config.get_settings()
    plain = agents.mcp_headers(s, "https://dt-mcp-db-abc.a.run.app/mcp")
    assert agents.SERVERLESS_AUTH_HEADER not in plain(None)
    monkeypatch.setattr(gcp, "id_token", lambda aud: f"tok-for-{aud}")
    cloud = agents.mcp_headers(s.__class__(**{**s.__dict__, "mcp_id_token": True}), "https://dt-mcp-db-abc.a.run.app/mcp")
    h = cloud(None)
    assert h[agents.SERVERLESS_AUTH_HEADER] == "Bearer tok-for-https://dt-mcp-db-abc.a.run.app"
    assert h[agents.AGENT_SECRET_HEADER] == s.agent_secret


async def _db_reachable() -> bool:
    try:
        await db.fetch_val("SELECT 1")
        return True
    except Exception:  # noqa: BLE001
        return False


async def test_conversation_lease_across_instances():
    from afdd_ai.agent import convlock

    if not await _db_reachable():
        await db.close_pools()
        pytest.skip("database not reachable")
    sid = f"test-lease-{time.time_ns()}"
    try:
        assert await convlock.acquire(sid)
        assert not await convlock.acquire(sid)  # held (by this or another instance)
        # another instance holds it
        await db.execute("UPDATE agent.conversation_locks SET holder = 'other' WHERE session_id = %s", (sid,))
        assert not await convlock.acquire(sid)
        await convlock.release(sid)  # not ours: stays
        assert await db.fetch_val("SELECT count(*) FROM agent.conversation_locks WHERE session_id = %s", (sid,)) == 1
        # it died: the lease expires and this instance takes it
        await db.execute("UPDATE agent.conversation_locks SET expires_at = now() - interval '1 s' WHERE session_id = %s", (sid,))
        assert await convlock.acquire(sid)
        await convlock.release(sid)
        assert await convlock.acquire(sid)
    finally:
        await db.execute("DELETE FROM agent.conversation_locks WHERE session_id = %s", (sid,))
        await db.close_pools()
