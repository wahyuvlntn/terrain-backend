"""The chat service behind the HTTP API: sessions, one streamed turn, history.

Sessions live in PostgreSQL schema `agent` (ADK `DatabaseSessionService` over
asyncpg with `search_path=agent`; ADK creates its tables there on first use).
They are keyed by app name + username and carry the owner's user id in their
state (`owner`): a conversation whose owner is not the caller's validated user
id (a username reused after a user was deleted) does not exist for the caller,
so a user only ever sees, continues or deletes their own conversations. The
conversation title is the first user message, stored in session state.

A turn runs the ADK `Runner` in SSE streaming mode with the per-request identity
(`state.Identity.state_delta`) and the parts of the UI context the tools act on
(twin walk mode, replayed simulation job: `navigation.ui_state`) as `temp:`
state, and the model-call cap from `AI_MAX_LLM_CALLS`, and yields normalised SSE
(event, data) pairs. A request that carries a submitted form (`form`, forms.py)
first carries it out as the person (`form_result` and the result's card), then
runs the model with the outcome as a "[Form result]" part of the user message. One turn per conversation at a time: `prepare_chat` takes
the conversation's lock (409 when it is held) and the turn releases it when it
ends.

When the client leaves (`ChatTurn.abort`), the turn stops forwarding events but
lets the model call in flight finish, so its usage is recorded (Gemini bills it
anyway), starts no further model call (`budget.before_model`), and then stops
the run; DRAIN_TIMEOUT_S bounds the wait.
"""

from __future__ import annotations

import asyncio
import logging
import re
import weakref
from collections.abc import AsyncIterator
from contextlib import aclosing
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from google.adk.agents import LlmAgent
from google.adk.agents.invocation_context import LlmCallsLimitExceededError
from google.adk.agents.run_config import RunConfig, StreamingMode
from google.adk.apps import App
from google.adk.runners import Runner
from google.adk.sessions import BaseSessionService, DatabaseSessionService, Session
from pydantic import BaseModel, ConfigDict, Field

from ..common import gemini
from ..common.config import Settings, get_settings
from . import budget, convlock, forms, navigation, stream
from . import state as st
from .backend_api import plant_tz
from .agents import APP_NAME, DEEP, INSIGHT, MAINTENANCE, ROOT, TWIN, build_root_agent
from .state import Identity

log = logging.getLogger(__name__)

TITLE_MAX = 80
MAX_MESSAGE = 8000
LIST_LIMIT = 100
DRAIN_TIMEOUT_S = 30.0  # how long an aborted turn waits for its model call in flight


class ChatContext(BaseModel):
    """What the user is looking at in the application (optional)."""

    model_config = ConfigDict(extra="ignore")

    page: str | None = None
    node_id: str | int | None = None
    asset_alias: str | None = None
    replay_job_id: str | int | None = None
    view: str | None = None  # "walk" while the twin is in first-person walk mode


class FormSubmission(BaseModel):
    """A form of the conversation the user filled in and submitted (forms.py)."""

    model_config = ConfigDict(extra="ignore")

    form_id: str = Field(min_length=1, max_length=64)
    kind: str = Field(min_length=1, max_length=64)
    values: dict[str, Any] = Field(default_factory=dict)
    context: dict[str, Any] = Field(default_factory=dict)


class ChatRequest(BaseModel):
    model_config = ConfigDict(extra="ignore")

    session_id: str | None = Field(default=None, max_length=128)
    message: str = Field(min_length=1, max_length=MAX_MESSAGE)
    context: ChatContext | None = None
    form: FormSubmission | None = None


class ConversationNotFound(LookupError):
    """The session does not exist for this user."""


class ConversationBusy(RuntimeError):
    """Another reply is still being generated in this conversation."""


def make_title(message: str) -> str:
    text = re.sub(r"\s+", " ", message).strip()
    return text if len(text) <= TITLE_MAX else text[: TITLE_MAX - 1].rstrip() + "…"


def friendly_error(err: BaseException) -> str:
    """User-facing text for an exception that ended a turn."""
    if isinstance(err, LlmCallsLimitExceededError):
        return "I reached the limit of model calls for one question before finishing. Please ask a narrower question."
    if isinstance(err, gemini.BudgetExceeded):
        return f"{err}."
    if isinstance(err, gemini.GeminiNotConfigured):
        return budget.MSG_NOT_CONFIGURED
    status = getattr(err, "code", None) or getattr(err, "status_code", None)
    if status == 429:
        return "The AI service is busy (rate limited). Please try again in a minute."
    if isinstance(status, int) and status >= 500:
        return "The AI service is temporarily unavailable. Please try again shortly."
    return "Something went wrong while answering. Please try again."


def _release_lock(lock: Any) -> None:
    if lock.locked():
        lock.release()


@dataclass
class ChatTurn:
    """One prepared chat turn; `events()` streams it.

    A turn that may run holds its conversation's lock from `prepare_chat` until
    it ends (`release`, idempotent). A turn whose stream never starts (the
    client left before the response began) releases it when it is collected.
    """

    service: AgentService
    identity: Identity
    session_id: str | None
    title: str
    message: str
    context: dict[str, Any] | None
    refusal: str | None = None
    abort: asyncio.Event = field(default_factory=asyncio.Event)
    lock: Any = None  # convlock.Held: this instance's lock plus the shared lease
    form: dict[str, Any] | None = None  # a submitted form (FormSubmission) to carry out first

    def __post_init__(self) -> None:
        self._unlock = weakref.finalize(self, _release_lock, self.lock) if self.lock is not None else None

    def release(self) -> None:
        """Release the conversation lock (once) and drop it from the service when unused."""
        if self._unlock is not None:
            self._unlock()
        if self.session_id:
            self.service._prune_lock(self.session_id)

    async def events(self) -> AsyncIterator[tuple[str, dict[str, Any]]]:
        try:
            async for item in self._events():
                yield item
        finally:
            self.release()

    async def _drain_on_abort(self, guard: budget.TurnGuard, stop: asyncio.Event) -> None:
        """When the client leaves, wait (bounded) for the model call in flight to
        return its final response, whose usage is then recorded, and stop the run."""
        await self.abort.wait()
        if not guard.idle.is_set():
            try:
                await asyncio.wait_for(guard.idle.wait(), timeout=DRAIN_TIMEOUT_S)
            except TimeoutError:
                log.warning("model call still running %.0f s after the client left session=%s; stopping it", DRAIN_TIMEOUT_S, self.session_id)
        stop.set()

    async def _events(self) -> AsyncIterator[tuple[str, dict[str, Any]]]:
        svc = self.service
        if self.session_id:
            yield "session", {"session_id": self.session_id, "title": self.title}
        form_note = None
        if self.form is not None and self.session_id and not self.abort.is_set():
            f = self.form
            result = await forms.submit(f["kind"], f.get("values") or {}, f.get("context") or {}, self.identity.state_delta())
            log.info("form submitted session=%s kind=%s ok=%s", self.session_id, f["kind"], result.get("ok"))
            yield "form_result", {"form_id": f["form_id"], "ok": bool(result.get("ok")), "message": result.get("message")}
            for item in stream.form_result_items(result):
                yield item
            form_note = forms.result_note(f["kind"], f["form_id"], result)
        if self.refusal is not None:
            yield "error", {"message": self.refusal}
            yield "done", {}
            return
        assert self.session_id is not None
        if self.abort.is_set():  # the client left before the turn started
            return
        normaliser = stream.StreamNormaliser(model_of=svc.model_of)
        run_config = RunConfig(streaming_mode=StreamingMode.SSE, max_llm_calls=svc.settings.max_llm_calls)
        stop = asyncio.Event()  # ADK's abort signal: set once nothing is left to drain
        guard = budget.track_turn(self.session_id, self.abort)
        drain = asyncio.create_task(self._drain_on_abort(guard, stop), name=f"drain-{self.session_id}")
        try:
            async with aclosing(
                svc.runner.run_async(
                    user_id=self.identity.username,
                    session_id=self.session_id,
                    new_message=stream.build_user_content(self.message, self.context, datetime.now(plant_tz()), form_note),
                    state_delta={**self.identity.state_delta(), **navigation.ui_state(self.context)},
                    run_config=run_config,
                    abort_signal=stop,
                )
            ) as agen:
                async for event in agen:
                    if self.abort.is_set():
                        continue  # nobody listens any more: only let the model call in flight finish
                    for item in normaliser.feed(event):
                        yield item
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - reported to the client as an SSE error
            if self.abort.is_set():
                log.info("aborted turn ended session=%s: %s", self.session_id, err)
                return
            if isinstance(err, (LlmCallsLimitExceededError, gemini.BudgetExceeded, gemini.GeminiNotConfigured)):
                log.warning("turn ended session=%s user=%s: %s", self.session_id, self.identity.username, err)
            else:
                log.exception("turn failed session=%s user=%s", self.session_id, self.identity.username)
            if not normaliser.error_sent:
                yield "error", {"message": friendly_error(err)}
        finally:
            drain.cancel()
            budget.untrack_turn(self.session_id, guard)
            await asyncio.gather(drain, return_exceptions=True)
        if self.abort.is_set():
            log.info("turn stopped after the client left session=%s user=%s", self.session_id, self.identity.username)
            return
        usage = normaliser.usage()
        today, limit = None, svc.settings.daily_budget_usd
        try:
            # report the limit that applies to this user's next turn: the assistant's share of the
            # daily budget (minus the analytics reserve) or the user's own allowance, whichever is closer;
            # a limit of 0 is off, and with both off the limit is 0 (the UI shows only the spend)
            s = svc.settings
            total = await gemini.spent_today_usd()
            mine = await gemini.spent_today_usd(self.identity.user_id)
            pairs = []
            if s.daily_budget_usd > 0:
                pairs.append((total, max(0.0, s.daily_budget_usd - s.analytics_reserve_usd)))
            if s.user_daily_budget_usd > 0:
                pairs.append((mine, s.user_daily_budget_usd))
            today, limit = min(pairs, key=lambda p: p[1] - p[0]) if pairs else (total, 0.0)
        except Exception:  # noqa: BLE001 - usage is informational
            log.warning("could not read today's AI spend", exc_info=True)
        yield "usage", {**usage, "today_usd": None if today is None else round(today, 4), "limit_usd": round(limit, 4)}
        yield "done", {}


class AgentService:
    """Owns the ADK runner and the session store."""

    def __init__(
        self,
        settings: Settings | None = None,
        *,
        root_agent: LlmAgent | None = None,
        session_service: BaseSessionService | None = None,
    ):
        self.settings = settings or get_settings()
        self.session_service = session_service or DatabaseSessionService(
            self.settings.agent_session_db_url,
            connect_args={"server_settings": {"search_path": "agent"}},
            pool_size=5,
            max_overflow=5,
        )
        self.root_agent = root_agent or build_root_agent(settings=self.settings)
        self.runner = Runner(app=App(name=APP_NAME, root_agent=self.root_agent), session_service=self.session_service)
        self._models = {
            ROOT: self.settings.model_fast,
            INSIGHT: self.settings.model_fast,
            TWIN: self.settings.model_fast,
            MAINTENANCE: self.settings.model_fast,
            DEEP: self.settings.model_pro,
        }
        self._locks: dict[str, asyncio.Lock] = {}

    def model_of(self, agent_name: str) -> str:
        return self._models.get(agent_name, "")

    async def prepare(self) -> None:
        """Create the ADK tables (schema `agent`) ahead of the first request."""
        prepare = getattr(self.session_service, "prepare_tables", None)
        if prepare is not None:
            await prepare()

    async def close(self) -> None:
        await self.runner.close()
        close = getattr(self.session_service, "close", None)
        if close is not None:
            await close()

    # -- sessions ----------------------------------------------------------

    async def _get(self, identity: Identity, session_id: str) -> Session:
        session = await self.session_service.get_session(app_name=APP_NAME, user_id=identity.username, session_id=session_id)
        if session is None or not owned_by(session, identity):
            raise ConversationNotFound(session_id)
        return session

    def _lock(self, session_id: str) -> asyncio.Lock:
        lock = self._locks.get(session_id)
        if lock is None:
            lock = self._locks[session_id] = asyncio.Lock()
        return lock

    async def _acquire(self, session_id: str) -> convlock.Held:
        """The conversation's lock, held by the caller; ConversationBusy when a turn holds it, here or on
        another instance (the shared lease of convlock)."""
        lock = self._lock(session_id)
        if lock.locked():
            raise ConversationBusy(session_id)
        await lock.acquire()  # free: taken at once, nothing else runs between the check and here
        try:
            shared = await convlock.acquire(session_id)
        except Exception as err:  # noqa: BLE001 - without the database only this instance's lock guards it
            log.warning("conversation lease unavailable session=%s: %s", session_id, err)
            return convlock.Held(session_id, lock, False)
        if not shared:
            lock.release()
            self._prune_lock(session_id)
            raise ConversationBusy(session_id)
        return convlock.Held(session_id, lock, True)

    def _prune_lock(self, session_id: str) -> None:
        """Forget a lock nobody holds (every user of a lock holds it, so this never drops one in use)."""
        lock = self._locks.get(session_id)
        if lock is not None and not lock.locked():
            self._locks.pop(session_id, None)

    async def prepare_chat(self, identity: Identity, req: ChatRequest) -> ChatTurn:
        """Validate and open (or create) the conversation; raises ConversationNotFound / ConversationBusy.

        A turn that may run holds the conversation's lock from here until it ends.
        """
        message = req.message.strip()
        if not message:
            raise ValueError("Empty message")
        context = req.context.model_dump(exclude_none=True) if req.context else None
        form = req.form.model_dump() if req.form else None
        refusal = await budget.budget_refusal(identity.user_id)
        if req.session_id:
            session = await self._get(identity, req.session_id)
            title = str(session.state.get(st.TITLE) or "")
            if refusal is not None:
                return ChatTurn(self, identity, session.id, title, message, context, refusal=refusal[1], form=form)
            lock = await self._acquire(session.id)
            return ChatTurn(self, identity, session.id, title, message, context, lock=lock, form=form)
        if refusal is not None:
            # No conversation is created for a turn that cannot run.
            return ChatTurn(self, identity, None, "", message, context, refusal=refusal[1])
        title = make_title(message)
        session = await self.session_service.create_session(
            app_name=APP_NAME,
            user_id=identity.username,
            state={st.TITLE: title, st.OWNER: identity.user_id},
        )
        lock = await self._acquire(session.id)
        log.info("conversation created session=%s user=%s", session.id, identity.username)
        return ChatTurn(self, identity, session.id, title, message, context, lock=lock)

    async def list_sessions(self, identity: Identity) -> list[dict[str, Any]]:
        resp = await self.session_service.list_sessions(app_name=APP_NAME, user_id=identity.username)
        mine = [s for s in resp.sessions if owned_by(s, identity)]
        sessions = sorted(mine, key=lambda s: s.last_update_time or 0, reverse=True)[:LIST_LIMIT]
        return [{"id": s.id, "title": s.state.get(st.TITLE) or "Conversation", "updated_at": stream.iso(s.last_update_time)} for s in sessions]

    async def get_history(self, identity: Identity, session_id: str) -> dict[str, Any]:
        session = await self._get(identity, session_id)
        return {"id": session.id, "title": session.state.get(st.TITLE) or "Conversation", "messages": stream.history_messages(session.events)}

    async def delete_session(self, identity: Identity, session_id: str) -> None:
        await self._get(identity, session_id)
        lock = await self._acquire(session_id)  # 409 while a turn runs; no turn can start meanwhile
        try:
            await self.session_service.delete_session(app_name=APP_NAME, user_id=identity.username, session_id=session_id)
        finally:
            lock.release()
            self._prune_lock(session_id)
        log.info("conversation deleted session=%s user=%s", session_id, identity.username)


def owned_by(session: Session, identity: Identity) -> bool:
    """The conversation belongs to this user id (sessions are keyed by username,
    which a new user may reuse after the old one was deleted)."""
    try:
        return int(session.state.get(st.OWNER)) == int(identity.user_id)
    except (TypeError, ValueError):
        return False
