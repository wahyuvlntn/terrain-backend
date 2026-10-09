"""Gemini cost control for the agents (ADK model callbacks).

* `before_model` refuses a model call when Gemini is not configured, when
  today's spend (plant time zone, `analytics.llm_usage`) has reached
  `AI_DAILY_BUDGET_USD` minus `AI_ANALYTICS_RESERVE_USD` (the reserve keeps
  background analytics such as fault instructions and embeddings running), or
  when the user's own spend today has reached `AI_USER_DAILY_BUDGET_USD`. A
  budget of 0 means no limit (the daily one also lifts the reserve). The
  refusal is an `LlmResponse` carrying an error code, so the turn ends cleanly
  and the SSE layer reports it as `error`.
* `usage_recorder(model)` builds the `after_model_callback` that prices and
  records each completed model call (streamed partial chunks are skipped; the
  aggregated final response carries the usage).
* `TurnGuard` (one per running chat turn, `track_turn`) supports stopping a
  turn whose client left: once its abort flag is set `before_model` starts no
  further model call, and `idle` tells when the call in flight has returned
  its final response (and its usage is recorded), so the runner can stop the
  run without losing the cost of a call Gemini bills anyway.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from typing import Any

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types

from ..common import gemini
from ..common.config import get_settings
from . import state as st

log = logging.getLogger(__name__)

COMPONENT = "agent"

# error_code values of refusals (read by the SSE layer).
BUDGET_EXCEEDED = "AFDD_BUDGET_EXCEEDED"
USER_BUDGET_EXCEEDED = "AFDD_USER_BUDGET_EXCEEDED"
NOT_CONFIGURED = "AFDD_AI_NOT_CONFIGURED"
BUDGET_UNKNOWN = "AFDD_BUDGET_UNAVAILABLE"
ABORTED = "AFDD_TURN_ABORTED"
REFUSAL_CODES = frozenset({BUDGET_EXCEEDED, USER_BUDGET_EXCEEDED, NOT_CONFIGURED, BUDGET_UNKNOWN, ABORTED})

MSG_NOT_CONFIGURED = "LEVO is not configured on this server (no Gemini API key). Ask an administrator to set it up."
MSG_BUDGET_UNKNOWN = "I cannot check today's AI budget right now, so I will not call the model. Please try again in a moment."
MSG_ABORTED = "This reply was stopped because the conversation was closed before it finished."


def budget_message(spent: float, limit: float) -> str:
    return (
        f"Today's AI budget is used up (US${spent:.2f} of US${limit:.2f}). "
        "LEVO will be available again after midnight plant time."
    )


def assistant_budget_message(spent: float, limit: float) -> str:
    return (
        f"Today's AI budget for LEVO is used up (US${spent:.2f} of US${limit:.2f}; the rest is kept for "
        "the automatic plant analytics). LEVO will be available again after midnight plant time."
    )


def user_budget_message(spent: float, limit: float) -> str:
    return (
        f"You have used your personal LEVO allowance for today (US${spent:.2f} of US${limit:.2f}). "
        "It is renewed at midnight plant time."
    )


async def budget_refusal(user_id: int | None = None) -> tuple[str, str] | None:
    """(error_code, message) when an agent model call must not be made, else None.

    Agent turns stop at the daily budget minus the analytics reserve, and at the
    user's own daily allowance (`user_id`; None skips the per-user check).
    A limit of 0 is not checked (no limit).
    """
    s = get_settings()
    if not s.gemini_configured:
        return NOT_CONFIGURED, MSG_NOT_CONFIGURED
    try:
        if s.daily_budget_usd > 0:
            spent = await gemini.spent_today_usd()
            if spent >= s.daily_budget_usd:
                return BUDGET_EXCEEDED, budget_message(spent, s.daily_budget_usd)
            agent_limit = max(0.0, s.daily_budget_usd - s.analytics_reserve_usd)
            if spent >= agent_limit:
                return BUDGET_EXCEEDED, assistant_budget_message(spent, agent_limit)
        if user_id is not None and s.user_daily_budget_usd > 0:
            mine = await gemini.spent_today_usd(user_id)
            if mine >= s.user_daily_budget_usd:
                return USER_BUDGET_EXCEEDED, user_budget_message(mine, s.user_daily_budget_usd)
    except Exception:  # noqa: BLE001 - fail closed: no budget check, no spend
        log.exception("could not read today's AI spend")
        return BUDGET_UNKNOWN, MSG_BUDGET_UNKNOWN
    return None


def refusal_response(code: str, message: str) -> LlmResponse:
    """A model response that ends the turn with `message` (no model call made)."""
    return LlmResponse(
        content=types.ModelContent(parts=[types.Part(text=message)]),
        error_code=code,
        error_message=message,
        turn_complete=True,
        custom_metadata={"afdd_refusal": code},
    )


class TurnGuard:
    """Model calls of one running chat turn (see the module docstring)."""

    def __init__(self, abort: asyncio.Event) -> None:
        self.abort = abort
        self.in_flight = 0
        self.idle = asyncio.Event()
        self.idle.set()

    def started(self) -> None:
        self.in_flight += 1
        self.idle.clear()

    def finished(self) -> None:
        self.in_flight = max(0, self.in_flight - 1)
        if self.in_flight == 0:
            self.idle.set()


# Running turns by conversation (session) id: one turn per conversation at a
# time (runner lock); the deep analyst runs inside its caller's session.
_TURNS: dict[str, TurnGuard] = {}


def track_turn(session_id: str, abort: asyncio.Event) -> TurnGuard:
    guard = _TURNS[session_id] = TurnGuard(abort)
    return guard


def untrack_turn(session_id: str, guard: TurnGuard) -> None:
    if _TURNS.get(session_id) is guard:
        del _TURNS[session_id]


def _guard(callback_context: CallbackContext) -> TurnGuard | None:
    session = getattr(callback_context, "session", None)
    return _TURNS.get(session.id) if session is not None else None


async def before_model(callback_context: CallbackContext, llm_request: LlmRequest) -> LlmResponse | None:
    """`before_model_callback`: refuse when the turn was aborted, over budget or not configured."""
    del llm_request
    guard = _guard(callback_context)
    if guard is not None and guard.abort.is_set():
        log.info("model call skipped (client left) agent=%s", callback_context.agent_name)
        return refusal_response(ABORTED, MSG_ABORTED)
    refusal = await budget_refusal(st.state_user_id(callback_context.state))
    if refusal is not None:
        code, message = refusal
        log.warning("model call refused agent=%s code=%s", callback_context.agent_name, code)
        return refusal_response(code, message)
    if guard is not None:
        if guard.abort.is_set():  # the client left while the budget was read
            return refusal_response(ABORTED, MSG_ABORTED)
        guard.started()
    return None


def usage_recorder(model_name: str) -> Callable[[CallbackContext, LlmResponse], Awaitable[None]]:
    """`after_model_callback` that records the usage of one agent's model calls.

    `model_name` is the configured model id (pricing key); the response's own
    `model_version` is only a fallback. A final (non-partial) response ends the
    model call for the turn guard, after its usage has been recorded.
    """

    async def after_model(callback_context: CallbackContext, llm_response: LlmResponse) -> None:
        if llm_response.partial:
            return None
        try:
            if llm_response.usage_metadata is None:
                return None
            usage = gemini.usage_from_metadata(model_name or llm_response.model_version or "", llm_response.usage_metadata)
            if not (usage.input_tokens or usage.output_tokens or usage.thinking_tokens):
                return None
            state: Any = callback_context.state
            await gemini.record_usage(
                COMPONENT,
                usage,
                user_id=st.state_user_id(state),
                session_id=callback_context.session.id,
            )
            return None
        finally:
            guard = _guard(callback_context)
            if guard is not None:
                guard.finished()

    return after_model
