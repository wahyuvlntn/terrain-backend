"""ADK events -> the chat SSE protocol (DESIGN 6.4), and chat history from stored events.

Live (`StreamNormaliser.feed`):
* partial model text            -> `text` {delta, agent}
* final text of one model turn  -> `message` {text, agent} (replaces that turn's deltas)
* function call                 -> `tool_call` {id, name, agent, args} (args sanitised)
* function response             -> `tool_result` {id, name, agent, ok, summary}, plus
  `chart` (a tool's {"chart": …}), `citations` (search_documents results),
  `simulation` (propose_simulation result) and `navigate` (open_in_app target)
* author change / transfer      -> `agent` {name}
* model error / budget refusal  -> `error` {message}
Thought parts are never forwarded; the deep analyst's own text is internal (its
answer reaches the user through the insight agent). `session`, `usage`, `done`
and pings are added by the service / HTTP layer.

History (`history_messages`) rebuilds the same content from persisted events,
one assistant message per invocation (user turn).
"""

from __future__ import annotations

import json
import logging
import math
import re
from collections.abc import Callable, Iterable, Mapping
from datetime import datetime, timezone
from typing import Any

from google.adk.events import Event
from google.genai import types

from ..common import gemini
from . import budget
from .agents import INTERNAL_AGENTS
from .charts import validate_option
from .navigation import navigation_item
from .rag_tool import citation_items

log = logging.getLogger(__name__)

UI_CONTEXT_PREFIX = "[UI context]"
PING = ": ping\n\n"
HIDDEN_TOOLS = frozenset({"transfer_to_agent"})
SEARCH_TOOLS = frozenset({"search_documents"})
# call_app_api scripts whose result carries a stored, cited maintenance instruction
INSTRUCTION_SCRIPTS = frozenset({"ai_insights/getfault"})
PROPOSE_TOOLS = frozenset({"propose_simulation"})
NAVIGATE_TOOLS = frozenset({"open_in_app"})

_SENSITIVE_KEY = re.compile(r"token|session|secret|password|passwd|authori[sz]ation|cookie|api[_-]?key|credential", re.I)
_CONTEXT_SAFE = re.compile(r"[^A-Za-z0-9 _\-./:#@]")

_MODEL_ERRORS = {
    "MODEL_RETURNED_NO_CONTENT": "The model returned an empty answer. Please rephrase the question or try again.",
    "MAX_TOKENS": "The answer was cut off because it became too long. Ask for a shorter answer or a narrower question.",
    "SAFETY": "The answer was blocked by the model's safety filters.",
    "PROHIBITED_CONTENT": "The answer was blocked by the model's safety filters.",
    "BLOCKLIST": "The answer was blocked by the model's safety filters.",
    "RECITATION": "The answer was blocked because it repeated protected text.",
    "MALFORMED_FUNCTION_CALL": "The model produced an invalid tool call. Please try again.",
    # Error events ADK emits for exceptions (code = API status or exception class name).
    "LlmCallsLimitExceededError": "I reached the limit of model calls for one question before finishing. Please ask a narrower question.",
    "GeminiNotConfigured": budget.MSG_NOT_CONFIGURED,
    "RESOURCE_EXHAUSTED": "The AI service is busy (rate limited). Please try again in a minute.",
    "UNAVAILABLE": "The AI service is temporarily unavailable. Please try again shortly.",
    "INTERNAL": "The AI service is temporarily unavailable. Please try again shortly.",
    "DEADLINE_EXCEEDED": "The AI service took too long to answer. Please try again.",
    "PERMISSION_DENIED": "The AI service rejected the request (API key or access). Ask an administrator to check the configuration.",
    "UNAUTHENTICATED": "The AI service rejected the request (API key or access). Ask an administrator to check the configuration.",
}
GENERIC_ERROR = "Something went wrong while answering. Please try again."


def sse(event: str, data: Mapping[str, Any]) -> str:
    """One SSE frame (`event:` name + one JSON `data:` line)."""
    return f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False, separators=(',', ':'), default=str)}\n\n"


def iso(ts: float | None) -> str | None:
    if ts is None:
        return None
    return datetime.fromtimestamp(ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


# ---------------------------------------------------------------------------
# User message and UI context
# ---------------------------------------------------------------------------


def _ctx_value(value: Any, max_len: int) -> str | None:
    if value is None:
        return None
    text = _CONTEXT_SAFE.sub("", str(value)).strip()[:max_len]
    return text or None


def format_context(context: Mapping[str, Any] | None, now: datetime | None = None) -> str | None:
    """The `[UI context]` line prepended to the user message (sanitised, no free text).

    `now` (plant-local) adds the current plant time: it lives here, in the
    newest message, rather than in the system instruction, so the cached
    prompt prefix stays the same from turn to turn.
    """
    context = context or {}
    fields = [
        ("page", _ctx_value(context.get("page"), 40)),
        ("selected node", _ctx_value(context.get("node_id"), 80)),
        ("selected asset", _ctx_value(context.get("asset_alias"), 40)),
        ("view", _ctx_value(context.get("view"), 10)),
        ("replay simulation job", _ctx_value(context.get("replay_job_id"), 20)),
        ("plant time", f"{now:%Y-%m-%d %H:%M} (UTC{now:%z})" if now is not None else None),
    ]
    items = [f"{k}={v}" for k, v in fields if v]
    return f"{UI_CONTEXT_PREFIX} " + "; ".join(items) if items else None


def build_user_content(message: str, context: Mapping[str, Any] | None, now: datetime | None = None) -> types.Content:
    parts = []
    line = format_context(context, now)
    if line:
        parts.append(types.Part(text=line))
    parts.append(types.Part(text=message))
    return types.UserContent(parts=parts)


def user_text(content: types.Content | None) -> str:
    """The user's own text (without the UI context line)."""
    if content is None or not content.parts:
        return ""
    texts = [p.text for p in content.parts if p.text and not p.thought and not p.text.startswith(UI_CONTEXT_PREFIX)]
    return "\n".join(texts).strip()


# ---------------------------------------------------------------------------
# Tool calls and results
# ---------------------------------------------------------------------------


def sanitize_args(value: Any, depth: int = 0) -> Any:
    """Tool-call args for display: no secrets, bounded size."""
    if depth > 5:
        return "…"
    if isinstance(value, Mapping):
        out = {}
        for i, (k, v) in enumerate(value.items()):
            if i >= 30:
                out["…"] = "more fields not shown"
                break
            key = str(k)
            out[key] = "[redacted]" if _SENSITIVE_KEY.search(key) else sanitize_args(v, depth + 1)
        return out
    if isinstance(value, (list, tuple)):
        items = [sanitize_args(v, depth + 1) for v in list(value)[:20]]
        if len(value) > 20:
            items.append(f"… {len(value) - 20} more")
        return items
    if isinstance(value, str):
        return value if len(value) <= 300 else value[:299] + "…"
    if isinstance(value, float) and not math.isfinite(value):
        return None
    return value


def unwrap_response(response: Any) -> tuple[Any, bool, str | None]:
    """(payload, ok, error_text) of a function response (plain tool, ADK-wrapped value or MCP result)."""
    if not isinstance(response, Mapping):
        return response, True, None
    if isinstance(response.get("content"), list) and any(k in response for k in ("isError", "is_error", "structuredContent", "structured_content")):
        is_error = bool(response.get("isError") or response.get("is_error"))
        texts = [str(c.get("text") or "") for c in response["content"] if isinstance(c, Mapping) and c.get("type") == "text"]
        text = "\n".join(t for t in texts if t)
        structured = response.get("structuredContent") or response.get("structured_content")
        payload: Any
        if isinstance(structured, Mapping):
            payload = structured
        else:
            try:
                payload = json.loads(text) if text else {}
            except ValueError:
                payload = {"text": text}
        if is_error:
            return payload, False, text or "The tool failed."
        if isinstance(payload, Mapping) and payload.get("error"):
            return payload, False, str(payload["error"])
        return payload, True, None
    if response.get("error"):
        return response, False, str(response["error"])
    return response, True, None


def summarise(name: str, payload: Any, ok: bool, error: str | None) -> str:
    if not ok:
        return (error or "failed")[:240]
    if isinstance(payload, Mapping):
        chart = payload.get("chart")
        if isinstance(chart, Mapping):
            return f"Chart: {chart.get('title')}"[:240]
        if name in PROPOSE_TOOLS and payload.get("job_id") is not None:
            return f"Simulation #{payload.get('job_id')} {payload.get('status') or ''}".strip()
        if name in NAVIGATE_TOOLS and (nav := navigation_item(payload)) is not None:
            return f"Opened: {nav['label']}"[:240]
        if name in SEARCH_TOOLS and isinstance(payload.get("results"), list):
            return f"{len(payload['results'])} document passages"
        for key in ("count", "total"):
            if isinstance(payload.get(key), int):
                return f"{payload[key]} results"
        for key, value in payload.items():
            if isinstance(value, list):
                return f"{len(value)} {key}"
        if "result" in payload and isinstance(payload["result"], str):
            return payload["result"][:120] + ("…" if len(payload["result"]) > 120 else "")
    return "done"


def _chart(payload: Any) -> dict[str, Any] | None:
    if not isinstance(payload, Mapping):
        return None
    chart = payload.get("chart")
    if not isinstance(chart, Mapping) or not isinstance(chart.get("option"), Mapping):
        return None
    try:
        validate_option(chart["option"])
    except ValueError as err:
        log.warning("dropping invalid chart from tool output: %s", err)
        return None
    return {"id": str(chart.get("id") or ""), "title": str(chart.get("title") or "Chart"), "option": chart["option"]}


def _simulation(payload: Any) -> dict[str, Any] | None:
    if not isinstance(payload, Mapping) or payload.get("job_id") is None:
        return None
    return {
        "job_id": payload.get("job_id"),
        "title": payload.get("title"),
        "status": payload.get("status"),
        "approval_required": bool(payload.get("approval_required", payload.get("status") == "PROPOSED")),
        "rationale": payload.get("rationale"),
        "spec_summary": payload.get("spec_summary"),
    }


def result_items(name: str, payload: Any, ok: bool) -> list[tuple[str, dict[str, Any]]]:
    """`chart` / `citations` / `simulation` / `navigate` events carried by one tool result."""
    if not ok:
        return []
    out: list[tuple[str, dict[str, Any]]] = []
    if (chart := _chart(payload)) is not None:
        out.append(("chart", chart))
    if name in SEARCH_TOOLS and isinstance(payload, Mapping) and isinstance(payload.get("results"), list):
        items = citation_items(payload["results"])
        if items:
            out.append(("citations", {"items": items}))
    if name == "call_app_api" and (items := instruction_source_items(payload)):
        out.append(("citations", {"items": items}))
    if name in PROPOSE_TOOLS and (sim := _simulation(payload)) is not None:
        out.append(("simulation", sim))
    if name in NAVIGATE_TOOLS and (nav := navigation_item(payload)) is not None:
        out.append(("navigate", nav))
    return out


def instruction_source_items(payload: Any) -> list[dict[str, Any]]:
    """Citation items of a stored fault instruction relayed through call_app_api,
    in its [n] order, so the markers the agent keeps match the chips."""
    if not isinstance(payload, Mapping) or payload.get("script") not in INSTRUCTION_SCRIPTS:
        return []
    data = payload.get("data")
    sources = data.get("instruction_sources") if isinstance(data, Mapping) else None
    if isinstance(sources, str):
        try:
            sources = json.loads(sources)
        except ValueError:
            return []
    if not isinstance(sources, list):
        return []
    rows = sorted((s for s in sources if isinstance(s, Mapping)), key=lambda s: s.get("ref") or s.get("n") or 0)
    return citation_items([{**s, "ref": s.get("ref") or s.get("n"), "content": s.get("snippet") or ""} for s in rows])


def error_text(event: Event) -> str | None:
    """User-facing text of a model error / refusal event, else None."""
    code = event.error_code
    if not code and not event.error_message:
        return None
    if code in budget.REFUSAL_CODES:
        return event.error_message or "The assistant is unavailable."
    name = str(code.value) if hasattr(code, "value") else str(code or "")  # FinishReason enum or text
    if name in _MODEL_ERRORS:
        return _MODEL_ERRORS[name]
    if name == "BudgetExceeded":
        return f"{event.error_message or 'Daily AI budget reached'}."
    if not name or name.endswith(("Error", "Exception", "Exceeded")):
        return GENERIC_ERROR
    return f"The model reported an error ({name}). Please try again."


def _texts(content: types.Content | None) -> str:
    if content is None or not content.parts:
        return ""
    return "".join(p.text for p in content.parts if p.text and not p.thought)


def is_internal(event: Event) -> bool:
    if event.author in INTERNAL_AGENTS:
        return True
    branch = event.branch or ""
    return any(seg.split("@", 1)[0] in INTERNAL_AGENTS for seg in branch.split(".") if seg)


# ---------------------------------------------------------------------------
# Live normaliser
# ---------------------------------------------------------------------------


class StreamNormaliser:
    """Stateful converter of one turn's ADK events into SSE (event, data) pairs."""

    def __init__(self, model_of: Callable[[str], str]):
        self._model_of = model_of
        self.agent: str | None = None
        self.input_tokens = 0
        self.cached_tokens = 0
        self.output_tokens = 0
        self.cost_usd = 0.0
        self.error_sent = False

    def _set_agent(self, name: str | None) -> list[tuple[str, dict[str, Any]]]:
        if not name or name == "user" or name == self.agent or name in INTERNAL_AGENTS:
            return []
        self.agent = name
        return [("agent", {"name": name})]

    def _account(self, event: Event) -> None:
        if event.partial or event.usage_metadata is None:
            return
        usage = gemini.usage_from_metadata(self._model_of(event.author) or event.model_version or "", event.usage_metadata)
        self.input_tokens += usage.input_tokens
        self.cached_tokens += usage.cached_tokens
        self.output_tokens += usage.output_tokens + usage.thinking_tokens
        self.cost_usd += usage.cost_usd

    def feed(self, event: Event) -> list[tuple[str, dict[str, Any]]]:
        self._account(event)
        if event.author == "user" or is_internal(event):
            return []
        out: list[tuple[str, dict[str, Any]]] = []
        content = event.content
        is_model = content is not None and content.role == "model"
        text = _texts(content) if is_model else ""
        calls = [fc for fc in event.get_function_calls() if fc.name not in HIDDEN_TOOLS] if not event.partial else []
        if text or calls:
            out += self._set_agent(event.author)
        agent = self.agent or event.author

        if event.partial:
            if text:
                out.append(("text", {"delta": text, "agent": agent}))
            return out

        err = error_text(event)
        if err is not None:
            if text and event.error_code not in budget.REFUSAL_CODES:
                out.append(("message", {"text": text, "agent": agent}))
            out.append(("error", {"message": err}))
            self.error_sent = True
            return out

        if text:
            out.append(("message", {"text": text, "agent": agent}))
        for fc in calls:
            out.append(("tool_call", {"id": fc.id, "name": fc.name, "agent": agent, "args": sanitize_args(dict(fc.args or {}))}))
        for fr in event.get_function_responses():
            if fr.name in HIDDEN_TOOLS:
                continue
            payload, ok, err_text = unwrap_response(fr.response)
            out.append(("tool_result", {"id": fr.id, "name": fr.name, "agent": event.author, "ok": ok, "summary": summarise(fr.name or "", payload, ok, err_text)}))
            out += result_items(fr.name or "", payload, ok)
        if event.actions and event.actions.transfer_to_agent:
            out += self._set_agent(event.actions.transfer_to_agent)
        return out

    def usage(self) -> dict[str, Any]:
        return {"input_tokens": self.input_tokens, "cached_tokens": self.cached_tokens, "output_tokens": self.output_tokens, "cost_usd": round(self.cost_usd, 6)}


# ---------------------------------------------------------------------------
# History
# ---------------------------------------------------------------------------


def _new_message(event: Event, role: str) -> dict[str, Any]:
    return {
        "id": event.id,
        "role": role,
        "text": "",
        "agent": None,
        "charts": [],
        "citations": [],
        "simulations": [],
        "navigations": [],
        "tool_calls": [],
        "created_at": iso(event.timestamp),
    }


def history_messages(events: Iterable[Event]) -> list[dict[str, Any]]:
    """Chat messages from a session's stored events (one assistant message per invocation)."""
    messages: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    current_inv: str | None = None
    texts: list[str] = []
    calls: dict[str, dict[str, Any]] = {}

    def close() -> None:
        nonlocal current, current_inv, texts, calls
        if current is not None:
            current["text"] = "\n\n".join(t for t in texts if t.strip())
            if current["text"] or current["tool_calls"] or current["charts"] or current["simulations"]:
                messages.append(current)
        current, current_inv, texts, calls = None, None, [], {}

    for ev in events:
        if ev.partial:
            continue
        if ev.author == "user":
            text = user_text(ev.content)
            if text:
                close()
                msg = _new_message(ev, "user")
                msg["text"] = text
                messages.append(msg)
            continue
        if is_internal(ev):
            continue
        if current is None or ev.invocation_id != current_inv:
            close()
            current, current_inv = _new_message(ev, "assistant"), ev.invocation_id
        assert current is not None
        content = ev.content
        err = error_text(ev)
        text = _texts(content) if content is not None and content.role == "model" else ""
        if err is not None:
            texts.append(err if ev.error_code in budget.REFUSAL_CODES or not text else f"{text}\n\n{err}")
            current["agent"] = current["agent"] or ev.author
            continue
        if text:
            texts.append(text)
            current["agent"] = ev.author
        for fc in ev.get_function_calls():
            if fc.name in HIDDEN_TOOLS:
                continue
            call = {"id": fc.id, "name": fc.name, "agent": ev.author, "args": sanitize_args(dict(fc.args or {})), "ok": None, "summary": None}
            calls[fc.id or f"call{len(calls)}"] = call
            current["tool_calls"].append(call)
            current["agent"] = current["agent"] or ev.author
        for fr in ev.get_function_responses():
            if fr.name in HIDDEN_TOOLS:
                continue
            payload, ok, err_text = unwrap_response(fr.response)
            call = calls.get(fr.id or "")
            if call is not None:
                call["ok"] = ok
                call["summary"] = summarise(fr.name or "", payload, ok, err_text)
            for kind, data in result_items(fr.name or "", payload, ok):
                if kind == "chart":
                    current["charts"].append(data)
                elif kind == "citations":
                    current["citations"].extend(data["items"])
                elif kind == "simulation":
                    current["simulations"].append(data)
                elif kind == "navigate":
                    current["navigations"].append(data)
    close()
    return messages
