"""Elicitation forms of the assistant and their submission (DESIGN 6.5).

A tool that cannot act because the user left out something it needs returns a form instead of
guessing (`{"form": spec, ...}`): the chat shows it as a card, prefilled with what is known. The
user fills it in and submits it; the next chat request carries the values (`ChatRequest.form`)
and the service carries the action out itself, before the model's turn (`submit`), as the person
who submitted it: no agent origin, exactly like a form of the application. The model then reads
the outcome in a "[Form result]" part of the user message and reports it.

Form spec (the `form` SSE event):
    {form_id, kind, title, description, submit_label, danger, fields: [Field], context}
Field:
    {name, label, type: text | textarea | number | select | multiselect | datetime | checkbox | info,
     required, value, options: [{value, label}] | options_source: technicians | issues (+ depends_on),
     min, max, step, unit, help, visible_if: {field, in: [...]}}
`context` carries fixed values the action needs (e.g. the order id); submitted values are merged
over it. Values are validated again on submission and the backend applies the user's permissions,
so a form spec needs no server-side storage.
"""

from __future__ import annotations

import json
import logging
import uuid
from collections.abc import Awaitable, Callable, Mapping
from typing import Any

from .backend_api import Api, ToolError

log = logging.getLogger(__name__)

FORM_RESULT_PREFIX = "[Form result]"
FORM_TYPES = frozenset({"text", "textarea", "number", "select", "multiselect", "datetime", "checkbox", "info"})
MAX_VALUES_JSON = 20_000


def field(name: str, label: str, type: str = "text", **extra: Any) -> dict[str, Any]:
    """One form field; options and limits that are None are left out."""
    assert type in FORM_TYPES, type
    out: dict[str, Any] = {"name": name, "label": label, "type": type}
    out.update({k: v for k, v in extra.items() if v is not None})
    return out


def options(pairs: Mapping[str, int] | list[tuple[Any, str]]) -> list[dict[str, Any]]:
    """Select options from {"LABEL": value} (labels title-cased) or [(value, label)]."""
    if isinstance(pairs, Mapping):
        return [{"value": v, "label": k.replace("_", " ").title()} for k, v in pairs.items()]
    return [{"value": v, "label": label} for v, label in pairs]


def new_form(
    kind: str,
    title: str,
    fields: list[dict[str, Any]],
    *,
    description: str = "",
    submit_label: str = "Submit",
    danger: bool = False,
    context: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    return {
        "form_id": f"f-{uuid.uuid4().hex[:12]}",
        "kind": kind,
        "title": title,
        "description": description,
        "submit_label": submit_label,
        "danger": danger,
        "fields": fields,
        "context": dict(context or {}),
    }


def form_reply(form: dict[str, Any], missing: list[str] | None = None) -> dict[str, Any]:
    """What a tool returns when it needs the user: the form plus a note for the model."""
    note = "A form is shown to the user in the chat"
    if missing:
        note += f" for the missing {', '.join(missing)}"
    return {
        "form": form,
        "status": "WAITING_FOR_USER",
        "message": note + ". Tell the user in one short sentence to fill in the form, then stop; do not ask for these values in text.",
    }


# ---------------------------------------------------------------------------
# Submission
# ---------------------------------------------------------------------------

Handler = Callable[[Api, dict[str, Any]], Awaitable[dict[str, Any]]]
_HANDLERS: dict[str, Handler] = {}


def handler(kind: str) -> Callable[[Handler], Handler]:
    """Register the function that carries out a submitted form of `kind`."""

    def register(fn: Handler) -> Handler:
        _HANDLERS[kind] = fn
        return fn

    return register


def known_kind(kind: str) -> bool:
    _load_handlers()
    return kind in _HANDLERS


def _load_handlers() -> None:
    # the modules that define forms register their handlers on import
    from . import maintenance, simforms  # noqa: F401


async def submit(kind: str, values: Mapping[str, Any], context: Mapping[str, Any], state: Mapping[str, Any]) -> dict[str, Any]:
    """Carry out a submitted form as the person (no agent origin).

    Returns {"ok": bool, "message": str, ...domain items ("maintenance_order", "simulation")}.
    """
    _load_handlers()
    fn = _HANDLERS.get(kind)
    if fn is None:
        return {"ok": False, "message": f"Unknown form '{kind}'."}
    if len(json.dumps(values, default=str)) > MAX_VALUES_JSON:
        return {"ok": False, "message": "The form is too large."}
    try:
        api = Api(state, origin="")
        result = await fn(api, {**dict(context), **dict(values)})
    except ToolError as err:
        return {"ok": False, "message": str(err)}
    except Exception:  # noqa: BLE001 - reported to the user, the conversation goes on
        log.exception("form submission failed kind=%s", kind)
        return {"ok": False, "message": "The form could not be carried out right now. Please try again."}
    return {"ok": True, **result}


def result_note(kind: str, form_id: str, result: Mapping[str, Any]) -> str:
    """The user-message part telling the model what the submitted form did."""
    payload = {"form_id": form_id, "kind": kind, "ok": bool(result.get("ok")), "message": result.get("message")}
    if (order := result.get("maintenance_order")) is not None:
        payload["maintenance_order"] = order
    if (sim := result.get("simulation")) is not None:
        payload["simulation"] = sim
    lead = "the application carried it out" if payload["ok"] else "it was NOT carried out"
    return f"{FORM_RESULT_PREFIX} The user submitted the form; {lead}. Report the outcome briefly. {json.dumps(payload, default=str)}"


def parse_result_note(text: str) -> dict[str, Any] | None:
    """The JSON payload of a "[Form result]" part, or None."""
    if not text.startswith(FORM_RESULT_PREFIX):
        return None
    start = text.find("{")
    if start < 0:
        return None
    try:
        payload = json.loads(text[start:])
    except ValueError:
        return None
    return payload if isinstance(payload, dict) else None
