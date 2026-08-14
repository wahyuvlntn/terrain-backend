"""Chart options are kept out of the model's view of earlier tool results."""

from google.adk.models.llm_request import LlmRequest
from google.genai import types

from afdd_ai.agent import context

OPTION = {"xAxis": {"type": "time"}, "yAxis": {"type": "value"}, "series": [{"name": "CH_01 chwst", "type": "line", "data": [[1, 6.5]] * 500}]}


def _request(response: dict) -> LlmRequest:
    call = types.Part(function_call=types.FunctionCall(id="c1", name="chart_trend", args={}))
    result = types.Part(function_response=types.FunctionResponse(id="c1", name="chart_trend", response=response))
    return LlmRequest(contents=[types.UserContent(parts=[types.Part(text="Trend CH_01")]), types.ModelContent(parts=[call]), types.Content(role="user", parts=[result])])


def test_chart_options_are_stubbed_without_touching_the_original():
    original = {"chart": {"id": "abc", "title": "CH_01 CHWST", "option": OPTION}, "series": [{"name": "CH_01 chwst", "mean": 6.5}], "hours": 24}
    req = _request(original)
    stored = req.contents[2]
    assert context.slim_request(req) == 1
    sent = req.contents[2].parts[0].function_response.response
    assert sent == {"chart": {"title": "CH_01 CHWST", "series": ["CH_01 chwst"], "note": context.CHART_NOTE}, "series": [{"name": "CH_01 chwst", "mean": 6.5}], "hours": 24}
    # the session's content object (and the stored event) keeps the full chart
    assert stored.parts[0].function_response.response["chart"]["option"] is OPTION
    # deterministic: the same request always gives the same stub (stable cached prefix)
    again = _request(original)
    context.slim_request(again)
    assert again.contents[2].model_dump() == req.contents[2].model_dump()


def test_wrapped_and_chartless_results_are_left_alone():
    wrapped = _request({"result": {"chart": {"title": "x", "option": OPTION}}})
    assert context.slim_request(wrapped) == 1
    assert "option" not in wrapped.contents[2].parts[0].function_response.response["result"]["chart"]
    plain = _request({"count": 3, "rows": [1, 2, 3]})
    before = plain.contents[2]
    assert context.slim_request(plain) == 0 and plain.contents[2] is before
