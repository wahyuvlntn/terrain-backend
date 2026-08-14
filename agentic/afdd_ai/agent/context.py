"""What the model sees of earlier tool results.

Chart tools return the full ECharts option (up to 2000 points per series) so the
SSE layer can draw it; the model only needs the statistics returned next to it.
Kept in the conversation, one chart can add 20k tokens to every later model call.
`before_model` replaces each chart option in the request's function responses with
a short stub. The replacement is deterministic, so the cached prompt prefix stays
the same from call to call; the stored events keep the full chart for the history.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse

CHART_NOTE = "drawn for the user as a chart; refer to it instead of repeating its data"
MAX_SERIES_NAMES = 12


def _chart_stub(chart: Mapping[str, Any]) -> dict[str, Any]:
    option = chart.get("option")
    series = option.get("series") if isinstance(option, Mapping) else None
    names = [s.get("name") for s in series if isinstance(s, Mapping) and s.get("name")] if isinstance(series, list) else []
    return {"title": chart.get("title"), "series": names[:MAX_SERIES_NAMES], "note": CHART_NOTE}


def slim_response(response: Any) -> Any:
    """The response with every chart option stubbed (the same object when nothing changes)."""
    if not isinstance(response, Mapping):
        return response
    out: dict[str, Any] = {}
    changed = False
    for key, value in response.items():
        if key == "chart" and isinstance(value, Mapping) and "option" in value:
            out[key] = _chart_stub(value)
            changed = True
        elif isinstance(value, Mapping):  # ADK wraps non-dict tool results as {"result": ...}
            slim = slim_response(value)
            changed |= slim is not value
            out[key] = slim
        else:
            out[key] = value
    return out if changed else response


def slim_request(llm_request: LlmRequest) -> int:
    """Stub chart options in the request's function responses; returns how many were stubbed.

    Contents are replaced by copies, never edited in place, so the session's events
    keep the full charts.
    """
    stubbed = 0
    for i, content in enumerate(llm_request.contents or []):
        if not content.parts:
            continue
        parts = []
        touched = False
        for part in content.parts:
            fr = part.function_response
            if fr is not None and isinstance(fr.response, Mapping):
                slim = slim_response(fr.response)
                if slim is not fr.response:
                    part = part.model_copy(update={"function_response": fr.model_copy(update={"response": slim})})
                    touched = True
                    stubbed += 1
            parts.append(part)
        if touched:
            llm_request.contents[i] = content.model_copy(update={"parts": parts})
    return stubbed


async def before_model(callback_context: CallbackContext, llm_request: LlmRequest) -> LlmResponse | None:
    """`before_model_callback`: slim the request; never short-circuits the call."""
    del callback_context
    slim_request(llm_request)
    return None
