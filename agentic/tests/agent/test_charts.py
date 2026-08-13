"""Chart payload safety: plain JSON options only, bounded, no markup or code hooks."""

from __future__ import annotations

import json
import math

import pytest

from afdd_ai.agent import charts


def test_time_series_option_caps_points_and_sanitises_text():
    points = [[f"2026-10-10T00:{i % 60:02d}:00Z", float(i)] for i in range(5000)]
    option = charts.time_series_option(
        [{"name": "CH_01 <img src=x onerror=alert(1)>", "unit": "°C", "points": points}],
        thresholds=[{"name": "Warning high", "value": 7.5}, {"name": "bad", "value": float("nan")}],
    )
    charts.validate_option(option)
    series = option["series"][0]
    assert len(series["data"]) == charts.MAX_POINTS
    assert series["data"][0] == [points[0][0], 0.0] and series["data"][-1][1] == 4999.0
    assert "<" not in series["name"] and ">" not in series["name"]
    assert series["markLine"]["data"] == [{"name": "Warning high", "yAxis": 7.5}]
    assert "formatter" not in json.dumps(option)


def test_time_series_option_two_axes_and_nan_gaps():
    option = charts.time_series_option(
        [
            {"name": "chwst", "unit": "°C", "points": [["t1", 6.5], ["t2", float("nan")], ["t3", None]]},
            {"name": "power", "unit": "kW", "points": [["t1", 500], ["t2", "x"]]},
            {"name": "third unit dropped to axis 0", "unit": "%", "points": []},
        ]
    )
    charts.validate_option(option)
    assert [a["name"] for a in option["yAxis"]] == ["°C", "kW"]
    assert option["series"][1]["yAxisIndex"] == 1
    assert option["series"][0]["data"] == [["t1", 6.5], ["t2", None], ["t3", None]]
    assert option["series"][1]["data"][1] == ["t2", None]


@pytest.mark.parametrize(
    "bad",
    [
        {"series": [], "tooltip": {"formatter": "function(p){return p}"}},
        {"series": [{"data": [math.inf]}]},
        {"series": [{"renderItem": "x"}]},
        {"series": [{"data": [object()]}]},
        {"series": [{"name": f"s{i}"} for i in range(charts.MAX_SERIES + 1)]},
        {"series": [{"data": list(range(charts.MAX_POINTS + 1))}]},
        ["not", "an", "object"],
    ],
)
def test_validate_option_rejects_unsafe(bad):
    with pytest.raises(ValueError):
        charts.validate_option(bad)


def test_validate_option_rejects_callables():
    with pytest.raises(ValueError):
        charts.validate_option({"series": [{"data": [1]}], "tooltip": {"valueFormatter": lambda v: v}})


async def test_make_chart_validates_model_data():
    ok = await charts.make_chart("MTBF by unit", "bar", ["CH_01", "CH_02"], [{"name": "MTBF (h)", "data": [1200.5, None]}], "h")
    assert "chart" in ok, ok
    assert ok["chart"]["option"]["series"][0]["data"] == [1200.5, None]
    assert ok["chart"]["option"]["xAxis"]["data"] == ["CH_01", "CH_02"]
    charts.validate_option(ok["chart"]["option"])

    pie = await charts.make_chart("Orders", "pie", ["Open", "Done"], [{"name": "orders", "data": [3, 7]}])
    assert pie["chart"]["option"]["series"][0]["data"] == [{"name": "Open", "value": 3.0}, {"name": "Done", "value": 7.0}]

    assert "error" in await charts.make_chart("x", "radar", ["a"], [{"name": "s", "data": [1]}])
    assert "error" in await charts.make_chart("x", "bar", ["a", "b"], [{"name": "s", "data": [1]}])
    assert "error" in await charts.make_chart("x", "bar", ["a"], [{"name": "s", "data": ["7 kW"]}])
    assert "error" in await charts.make_chart("x", "bar", [], [{"name": "s", "data": []}])
    too_many = [{"name": str(i), "data": [1]} for i in range(charts.MAX_SERIES + 1)]
    assert "error" in await charts.make_chart("x", "line", ["a"], too_many)


def test_chart_payload_shape():
    payload = charts.chart_payload("<b>Title</b>", charts.category_option("line", ["a"], [{"name": "s", "data": [1]}]))
    assert set(payload) == {"id", "title", "option"}
    assert payload["title"] == "bTitle/b"
    assert len(payload["id"]) == 12
