"""Run the MCP servers of the ai-mcp service.

    python -m afdd_ai.mcp.serve          # both servers, one process each (container entry point)
    python -m afdd_ai.mcp.serve db       # afdd-db only   (MCP_DB_PORT, default 8101)
    python -m afdd_ai.mcp.serve twin     # afdd-twin only (MCP_TWIN_PORT, default 8102)

Streamable HTTP on `/mcp`, stateless, JSON responses, bound to 0.0.0.0. MCP 2.x
validates the Host header against DNS rebinding; besides localhost the allowed
hosts are the compose service name `ai-mcp` and any extra names in
`MCP_ALLOWED_HOSTS` (comma separated, `host` or `host:port`). `GET /healthz`
answers without authentication; everything else goes through `McpAuthMiddleware`.
When one server process exits the other is stopped too, so the container
restarts as a whole.
"""

from __future__ import annotations

import json
import logging
import multiprocessing
import os
import re
import signal
import sys
import time
from typing import Any

import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.transport_security import TransportSecuritySettings

from ..common import aio, backend, db
from ..common.config import get_settings
from ..common.log import setup_logging
from .auth import McpAuthMiddleware

log = logging.getLogger(__name__)

HOST = "0.0.0.0"
DEFAULT_HOSTS = ("ai-mcp", "localhost", "127.0.0.1", "[::1]")


def allowed_hosts() -> list[str]:
    names = list(DEFAULT_HOSTS) + [h.strip() for h in os.environ.get("MCP_ALLOWED_HOSTS", "").split(",") if h.strip()]
    out: list[str] = []
    for name in names:
        # "host:port" / "host:*" are used as given; a bare host is allowed with and without a port.
        out += [name] if re.search(r":(\d+|\*)$", name) else [name, f"{name}:*"]
    return list(dict.fromkeys(out))


def server_for(name: str) -> tuple[MCPServer, int]:
    s = get_settings()
    if name == "db":
        from .db_server import mcp as db_mcp

        return db_mcp, s.mcp_db_port
    if name == "twin":
        from .twin_server import mcp as twin_mcp

        return twin_mcp, s.mcp_twin_port
    raise ValueError(f"unknown MCP server '{name}' (db or twin)")


class _Health:
    """Answers GET /healthz; passes everything else on."""

    def __init__(self, app: Any, name: str):
        self.app = app
        self.body = json.dumps({"status": "ok", "server": name}).encode()

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        if scope.get("type") == "http" and scope.get("path") == "/healthz":
            await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"application/json")]})
            await send({"type": "http.response.body", "body": self.body})
            return
        await self.app(scope, receive, send)


def build_app(server: MCPServer, name: str) -> Any:
    """The ASGI app of one server: health, authentication, MCP streamable HTTP."""
    hosts = allowed_hosts()
    security = TransportSecuritySettings(
        enable_dns_rebinding_protection=True,
        allowed_hosts=hosts,
        allowed_origins=[f"http://{h}" for h in hosts],
    )
    starlette = server.streamable_http_app(
        streamable_http_path="/mcp",
        json_response=True,
        stateless_http=True,
        transport_security=security,
        host=HOST,
    )
    return _Health(McpAuthMiddleware(starlette), name)


async def _serve(name: str, port: int) -> None:
    server, _ = server_for(name)
    config = uvicorn.Config(
        build_app(server, name),
        host=HOST,
        port=port,
        log_level=get_settings().log_level.lower(),
        access_log=False,
        lifespan="on",
        timeout_graceful_shutdown=10,
    )
    try:
        await uvicorn.Server(config).serve()
    finally:
        await backend.close_client()
        await db.close_pools()


def run_one(name: str, port: int | None = None) -> None:
    setup_logging(f"ai-mcp-{name}")
    _, default_port = server_for(name)
    s = get_settings()
    if not s.agent_secret:
        log.error("AGENT_SHARED_SECRET is not set: every request will be rejected")
    log.info("starting MCP server afdd-%s on %s:%s", name, HOST, port or default_port)
    aio.run(_serve(name, port or default_port))


def run_all() -> int:
    setup_logging("ai-mcp")
    ctx = multiprocessing.get_context("spawn")
    procs = {name: ctx.Process(target=run_one, args=(name,), name=f"afdd-{name}", daemon=False) for name in ("db", "twin")}
    for p in procs.values():
        p.start()
    stopping = False

    def stop(signum: int, _frame: Any) -> None:
        nonlocal stopping
        stopping = True
        log.info("signal %s: stopping MCP servers", signum)
        for p in procs.values():
            if p.is_alive():
                p.terminate()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    code = 0
    try:
        while all(p.is_alive() for p in procs.values()):
            time.sleep(1.0)
    finally:
        for name, p in procs.items():
            if not p.is_alive() and p.exitcode not in (0, None) and not stopping:
                log.error("MCP server afdd-%s exited with code %s", name, p.exitcode)
                code = p.exitcode or 1
            if p.is_alive():
                p.terminate()
        for p in procs.values():
            p.join(timeout=15)
            if p.is_alive():
                p.kill()
    return 0 if stopping else (code or 1)


def main(argv: list[str]) -> int:
    which = argv[1] if len(argv) > 1 else "all"
    if which == "all":
        return run_all()
    run_one(which)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
