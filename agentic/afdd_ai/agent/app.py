"""HTTP API of the ai-agent service (`uvicorn afdd_ai.agent.app:app`, DESIGN 6.3 / 6.4).

Only the backend proxy (`/api/agent/*`) talks to this service. Every route
except `/healthz` requires the shared `X-Agent-Secret` (constant-time compare;
an unset secret rejects everything) and the end user's identity in the
`X-AFDD-User-Id`, `X-AFDD-Username`, `X-AFDD-User-Name`, `X-AFDD-Session` and
`X-AFDD-Permissions` headers set by the proxy. Requests must name one of the
AGENT_ALLOWED_HOSTS in their Host header (default ai-agent, localhost,
127.0.0.1; the container healthcheck calls 127.0.0.1).

POST /chat streams Server-Sent Events: `session`, then `agent` / `text` /
`message` / `tool_call` / `tool_result` / `chart` / `citations` / `simulation` /
`error`, then `usage` and `done`, with a `: ping` comment every 15 s. A client
disconnect aborts the run: nothing more is sent, the model call in flight is
allowed to finish so its cost is recorded, and no further call is made.
"""

from __future__ import annotations

import asyncio
import hmac
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager, suppress
from typing import Annotated, Any

import anyio
from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, StreamingResponse
from starlette.exceptions import HTTPException as StarletteHTTPException
from starlette.middleware.trustedhost import TrustedHostMiddleware

from ..common import backend, db
from ..common.config import get_settings
from ..common.log import setup_logging
from ..mcp import auth as mcp_auth
from . import stream
from .runner import DRAIN_TIMEOUT_S, AgentService, ChatRequest, ChatTurn, ConversationBusy, ConversationNotFound
from .state import REQUIRED_PERMISSION, Identity, has_permission, parse_permissions

log = setup_logging("ai-agent")

PING_INTERVAL_S = 15.0
SSE_HEADERS = {"Cache-Control": "no-cache, no-transform", "X-Accel-Buffering": "no", "Connection": "keep-alive"}
DEV_SECRET = "afdd-agent-dev-secret"  # docker-compose's local demo default
DRAIN_WAIT_S = DRAIN_TIMEOUT_S + 10.0  # an aborted turn stops itself within DRAIN_TIMEOUT_S

_service: AgentService | None = None
# Aborted turns still finishing their model call after their response was closed.
_draining: set[asyncio.Task[None]] = set()


def get_service() -> AgentService:
    if _service is None:
        raise HTTPException(status_code=503, detail="The assistant is starting")
    return _service


def set_service(service: AgentService | None) -> None:
    """Install the service (the lifespan does it; tests inject their own)."""
    global _service
    _service = service


@asynccontextmanager
async def lifespan(_: FastAPI) -> AsyncIterator[None]:
    s = get_settings()
    if not s.agent_secret:
        log.error("AGENT_SHARED_SECRET is not set: every request will be rejected")
    elif s.agent_secret == DEV_SECRET:
        log.warning("AGENT_SHARED_SECRET is the development default: set a private secret outside a local demo")
    if not s.gemini_configured:
        log.warning("GOOGLE_API_KEY is not set: the assistant will refuse to answer")
    created = False
    if _service is None:
        set_service(AgentService(s))
        created = True
    try:
        await get_service().prepare()
    except Exception:  # noqa: BLE001 - the database may come up later; sessions retry on use
        log.exception("could not prepare the agent session tables")
    log.info("ai-agent ready model_fast=%s model_pro=%s", s.model_fast, s.model_pro)
    try:
        yield
    finally:
        if created and _service is not None:
            with suppress(Exception):
                await _service.close()
            set_service(None)
        with suppress(Exception):
            await backend.close_client()
        with suppress(Exception):
            await db.close_pools()


app = FastAPI(title="AFDD plant assistant", docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)
app.add_middleware(TrustedHostMiddleware, allowed_hosts=list(get_settings().agent_allowed_hosts))


@app.exception_handler(StarletteHTTPException)
async def _http_error(_, exc: StarletteHTTPException) -> JSONResponse:
    return JSONResponse(status_code=exc.status_code, content={"error": exc.detail})


@app.exception_handler(RequestValidationError)
async def _invalid_request(_, exc: RequestValidationError) -> JSONResponse:
    problems = "; ".join(f"{'.'.join(str(p) for p in e.get('loc', ())[1:]) or 'body'}: {e.get('msg')}" for e in exc.errors()[:5])
    return JSONResponse(status_code=422, content={"error": f"Invalid request ({problems})"})


@app.exception_handler(Exception)
async def _unexpected(_, exc: Exception) -> JSONResponse:
    log.error("request failed: %s", exc, exc_info=exc)
    return JSONResponse(status_code=500, content={"error": "The assistant could not handle the request. Please try again."})


def _header_text(value: str | None) -> str:
    """Header text from the Go proxy: raw UTF-8 bytes, which Starlette decodes as latin-1."""
    if not value:
        return ""
    try:
        return value.encode("latin-1").decode("utf-8")
    except (UnicodeEncodeError, UnicodeDecodeError):
        return value


async def require_identity(
    x_agent_secret: Annotated[str | None, Header()] = None,
    x_afdd_user_id: Annotated[str | None, Header()] = None,
    x_afdd_username: Annotated[str | None, Header()] = None,
    x_afdd_user_name: Annotated[str | None, Header()] = None,
    x_afdd_session: Annotated[str | None, Header()] = None,
    x_afdd_permissions: Annotated[str | None, Header()] = None,
) -> Identity:
    """The user a request acts for.

    The shared secret proves the request comes from the backend proxy, but the
    identity is taken from the session token itself (user/getuserdetails, cached
    60 s): whoever knows the secret still cannot act as another user or with
    permissions the session does not have. The identity headers must agree.
    """
    secret = get_settings().agent_secret
    if not secret or not x_agent_secret or not hmac.compare_digest(x_agent_secret.encode(), secret.encode()):
        raise HTTPException(status_code=401, detail="Unauthorized")
    token = (x_afdd_session or "").strip()
    if not token:
        raise HTTPException(status_code=401, detail="Missing user identity")
    try:
        user = await mcp_auth.authenticate(token)
    except mcp_auth.AuthError as err:
        raise HTTPException(status_code=err.status, detail=err.message) from None
    header_username = _header_text(x_afdd_username).strip()
    header_id = (x_afdd_user_id or "").strip()
    if user.user_id is None or not user.username:
        raise HTTPException(status_code=503, detail="Cannot validate the user session right now")
    if (header_username and header_username != user.username) or (header_id and header_id != str(user.user_id)):
        log.warning("identity headers do not match the session user=%s", user.username)
        raise HTTPException(status_code=401, detail="Unauthorized")
    permissions = ["admin"] if user.is_admin else sorted(user.permissions)
    if not has_permission(permissions, REQUIRED_PERMISSION):
        raise HTTPException(status_code=403, detail="The AI assistant is not enabled for this user")
    del x_afdd_user_name, x_afdd_permissions  # informational only: the session is the source of truth
    return Identity(user_id=user.user_id, username=user.username, name=user.full_name or user.username, session_token=token, permissions=permissions)


IdentityDep = Annotated[Identity, Depends(require_identity)]


@app.get("/healthz")
async def healthz() -> dict[str, Any]:
    return {"status": "ok" if _service is not None else "starting"}


async def _sse(turn: ChatTurn) -> AsyncIterator[str]:
    """Run the turn in a producer task; forward its frames, ping while idle, abort on disconnect.

    On disconnect the producer is not cancelled: the turn stops by itself once
    its model call in flight has returned (so the call's cost is recorded),
    within DRAIN_TIMEOUT_S; this waits for that (shielded from the request's
    cancellation, at most DRAIN_WAIT_S) and only then cancels a turn that did
    not stop.
    """
    queue: asyncio.Queue[str | None] = asyncio.Queue()

    async def produce() -> None:
        try:
            async for name, data in turn.events():
                await queue.put(stream.sse(name, data))
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001 - never leave the client without an ending
            log.exception("chat stream failed")
            await queue.put(stream.sse("error", {"message": "Something went wrong while answering. Please try again."}))
            await queue.put(stream.sse("done", {}))
        finally:
            queue.put_nowait(None)

    task = asyncio.create_task(produce(), name=f"chat-{turn.session_id or 'new'}")
    try:
        while True:
            try:
                frame = await asyncio.wait_for(queue.get(), timeout=PING_INTERVAL_S)
            except TimeoutError:
                yield stream.PING
                continue
            if frame is None:
                break
            yield frame
    finally:
        turn.abort.set()
        if not task.done():
            _draining.add(task)  # keeps it alive should this response be torn down first
            task.add_done_callback(_draining.discard)
            with anyio.CancelScope(shield=True):
                try:
                    await asyncio.wait_for(asyncio.shield(task), timeout=DRAIN_WAIT_S)
                except TimeoutError:
                    log.warning("chat turn session=%s did not stop after the client left; cancelling it", turn.session_id)
                    task.cancel()
                    await asyncio.wait({task}, timeout=5.0)
                except asyncio.CancelledError:
                    if not task.cancelled():
                        raise  # this request is being cancelled: the turn finishes in the background
                except Exception:  # noqa: BLE001 - the producer reports its own failures
                    pass


@app.post("/chat")
async def chat(body: ChatRequest, identity: IdentityDep) -> StreamingResponse:
    service = get_service()
    try:
        turn = await service.prepare_chat(identity, body)
    except ConversationNotFound:
        raise HTTPException(status_code=404, detail="Conversation not found") from None
    except ConversationBusy:
        raise HTTPException(status_code=409, detail="A reply is still being written in this conversation") from None
    except ValueError as err:
        raise HTTPException(status_code=400, detail=str(err)) from None
    return StreamingResponse(_sse(turn), media_type="text/event-stream", headers=SSE_HEADERS)


@app.get("/sessions")
async def list_sessions(identity: IdentityDep) -> list[dict[str, Any]]:
    return await get_service().list_sessions(identity)


@app.get("/sessions/{session_id}")
async def get_session(session_id: str, identity: IdentityDep) -> dict[str, Any]:
    try:
        return await get_service().get_history(identity, session_id)
    except ConversationNotFound:
        raise HTTPException(status_code=404, detail="Conversation not found") from None


@app.delete("/sessions/{session_id}")
async def delete_session(session_id: str, identity: IdentityDep) -> dict[str, Any]:
    try:
        await get_service().delete_session(identity, session_id)
    except ConversationNotFound:
        raise HTTPException(status_code=404, detail="Conversation not found") from None
    except ConversationBusy:
        raise HTTPException(status_code=409, detail="A reply is still being written in this conversation") from None
    return {"ok": True}
