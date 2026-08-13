"""Chart tools: server-built, validated ECharts options from real plant data.

The model never supplies a chart option. Trend charts are built here from the
backend's own series, and `make_chart` only accepts plain numbers and labels the
model took from earlier tool results. Every option is checked by
`validate_option`: plain JSON values only (no functions, no formatter / HTML
hooks), finite numbers, sanitised text, at most `MAX_POINTS` points per series.
Tool responses carry `{"chart": {"id", "title", "option"}}`; the SSE layer
forwards that as a `chart` event and the frontend applies its theme colours.
"""

from __future__ import annotations

import json
import math
import re
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

from google.adk.tools import ToolContext

from .backend_api import Api, ToolError, error_result, rnd

MAX_POINTS = 2000
MAX_SERIES = 8
MAX_CATEGORIES = 2000
MAX_TEXT = 120
MAX_OPTION_BYTES = 400_000
CHART_TYPES = ("line", "bar", "scatter", "pie")

# Keys that would let a string become code or HTML in ECharts.
_FORBIDDEN_KEYS = frozenset({"formatter", "renderitem", "rich", "html", "tooltipformatter", "valueformatter", "labelformatter"})
_CTRL = re.compile(r"[\x00-\x1f\x7f<>`]")


def safe_text(value: Any, max_len: int = MAX_TEXT) -> str:
    """Display text without markup or control characters."""
    text = _CTRL.sub("", str(value if value is not None else "")).strip()
    return text[:max_len]


def finite(value: Any) -> float | None:
    if isinstance(value, bool) or value is None:
        return None
    try:
        f = float(value)
    except (TypeError, ValueError):
        return None
    return f if math.isfinite(f) else None


def downsample(points: Sequence[Any], max_points: int = MAX_POINTS) -> list[Any]:
    """Evenly spaced subset (first and last kept) when there are too many points."""
    n = len(points)
    if n <= max_points:
        return list(points)
    step = (n - 1) / (max_points - 1)
    return [points[round(i * step)] for i in range(max_points)]


def validate_option(option: Any) -> None:
    """Raise ValueError unless `option` is a plain, bounded, code-free ECharts option."""

    def walk(node: Any, depth: int) -> None:
        if depth > 12:
            raise ValueError("chart option nested too deeply")
        if node is None or isinstance(node, (bool, str)):
            if isinstance(node, str) and len(node) > 4000:
                raise ValueError("chart option text too long")
            return
        if isinstance(node, (int, float)):
            if isinstance(node, float) and not math.isfinite(node):
                raise ValueError("chart option has a non-finite number")
            return
        if isinstance(node, Mapping):
            for k, v in node.items():
                if not isinstance(k, str):
                    raise ValueError("chart option keys must be strings")
                if k.lower() in _FORBIDDEN_KEYS:
                    raise ValueError(f"chart option key '{k}' is not allowed")
                walk(v, depth + 1)
            return
        if isinstance(node, (list, tuple)):
            if len(node) > max(MAX_POINTS, MAX_CATEGORIES):
                raise ValueError("chart option list too long")
            for v in node:
                walk(v, depth + 1)
            return
        raise ValueError(f"chart option value of type {type(node).__name__} is not allowed")

    if not isinstance(option, Mapping):
        raise ValueError("chart option must be an object")
    walk(option, 0)
    series = option.get("series") or []
    if len(series) > MAX_SERIES:
        raise ValueError("too many series")
    if len(json.dumps(option, separators=(",", ":"))) > MAX_OPTION_BYTES:
        raise ValueError("chart option too large")


def chart_payload(title: str, option: dict[str, Any]) -> dict[str, Any]:
    validate_option(option)
    return {"id": uuid.uuid4().hex[:12], "title": safe_text(title) or "Chart", "option": option}


# ---------------------------------------------------------------------------
# Option builders
# ---------------------------------------------------------------------------


def time_series_option(series: list[dict[str, Any]], *, thresholds: list[dict[str, Any]] | None = None) -> dict[str, Any]:
    """Line chart on a time axis.

    series: [{name, unit, points: [[iso_ts, value|None], ...]}] (unit picks the y axis; at most 2 axes).
    thresholds: [{name, value}] drawn as dashed lines on the first axis.
    """
    units: list[str] = []
    for s in series[:MAX_SERIES]:
        u = safe_text(s.get("unit") or "", 20)
        if u not in units:
            units.append(u)
    if len(units) > 2:
        units = units[:2]
    y_axes = [{"type": "value", "name": u, "scale": True} for u in units] or [{"type": "value", "scale": True}]
    out_series: list[dict[str, Any]] = []
    for s in series[:MAX_SERIES]:
        u = safe_text(s.get("unit") or "", 20)
        axis = units.index(u) if u in units else 0
        data = []
        for p in downsample(s.get("points") or []):
            if not isinstance(p, (list, tuple)) or len(p) != 2:
                continue
            data.append([safe_text(p[0], 40), finite(p[1])])
        item: dict[str, Any] = {"name": safe_text(s.get("name")), "type": "line", "showSymbol": False, "connectNulls": False, "data": data}
        if axis:
            item["yAxisIndex"] = axis
        out_series.append(item)
    if thresholds and out_series:
        lines = [
            {"name": safe_text(t.get("name"), 40), "yAxis": finite(t.get("value"))}
            for t in thresholds[:6]
            if finite(t.get("value")) is not None
        ]
        if lines:
            out_series[0]["markLine"] = {"silent": True, "symbol": "none", "lineStyle": {"type": "dashed"}, "data": lines}
    return {
        "tooltip": {"trigger": "axis"},
        "legend": {"type": "scroll", "data": [s["name"] for s in out_series]},
        "grid": {"left": 56, "right": 56 if len(y_axes) > 1 else 24, "top": 40, "bottom": 48},
        "xAxis": {"type": "time"},
        "yAxis": y_axes,
        "dataZoom": [{"type": "inside"}, {"type": "slider", "height": 16, "bottom": 8}],
        "series": out_series,
    }


def category_option(chart_type: str, x: list[Any], series: list[dict[str, Any]], y_name: str = "") -> dict[str, Any]:
    """Bar / line / scatter over categories, or a pie of the first series."""
    labels = [safe_text(v, 60) for v in x[:MAX_CATEGORIES]]
    if chart_type == "pie":
        first = series[0] if series else {"name": "", "data": []}
        data = [
            {"name": labels[i], "value": finite(v)}
            for i, v in enumerate((first.get("data") or [])[: len(labels)])
            if finite(v) is not None
        ]
        return {
            "tooltip": {"trigger": "item"},
            "legend": {"type": "scroll", "bottom": 0},
            "series": [{"name": safe_text(first.get("name")), "type": "pie", "radius": ["35%", "65%"], "data": data}],
        }
    out_series = []
    for s in series[:MAX_SERIES]:
        values = [finite(v) for v in (s.get("data") or [])[: len(labels)]]
        out_series.append({"name": safe_text(s.get("name")), "type": chart_type, "data": values})
    return {
        "tooltip": {"trigger": "axis" if chart_type != "scatter" else "item"},
        "legend": {"type": "scroll", "data": [s["name"] for s in out_series]},
        "grid": {"left": 56, "right": 24, "top": 40, "bottom": 56},
        "xAxis": {"type": "category", "data": labels},
        "yAxis": {"type": "value", "name": safe_text(y_name, 40), "scale": chart_type != "bar"},
        "series": out_series,
    }


def series_stats(points: list[list[Any]]) -> dict[str, Any]:
    values = [v for _, v in points if finite(v) is not None]
    if not values:
        return {"count": 0}
    return {
        "count": len(values),
        "min": rnd(min(values)),
        "max": rnd(max(values)),
        "mean": rnd(sum(values) / len(values)),
        "first": rnd(values[0]),
        "last": rnd(values[-1]),
    }


def trend_points(trend: Mapping[str, Any]) -> list[list[Any]]:
    """[[ts, value], …] from a digital_twin/getparametertrend payload."""
    return [[p.get("timestamp"), finite(p.get("value"))] for p in (trend.get("data") or []) if isinstance(p, Mapping) and p.get("timestamp")]


def limit_lines(trend: Mapping[str, Any]) -> list[dict[str, Any]]:
    lines = []
    for kind in ("warning", "critical"):
        band = trend.get(kind) or {}
        if isinstance(band, Mapping):
            if finite(band.get("upper")) is not None:
                lines.append({"name": f"{kind.title()} high", "value": band.get("upper")})
            if finite(band.get("lower")) is not None:
                lines.append({"name": f"{kind.title()} low", "value": band.get("lower")})
    return lines


def _hours(hours: Any, default: int = 24) -> int:
    try:
        h = int(hours)
    except (TypeError, ValueError):
        h = default
    return max(1, min(h, 168))


# ---------------------------------------------------------------------------
# Tools
# ---------------------------------------------------------------------------


async def chart_parameter_trend(tool_context: ToolContext, asset: str, params: list[str], hours: int = 24) -> dict[str, Any]:
    """Plot one or more parameters of one equipment over the last hours (shown to the user as a chart).

    Args:
      asset: Equipment alias, e.g. "CH_01", "CT_02", "CHWP_01", "CHWS_01".
      params: Parameter aliases of that equipment, e.g. ["chwst", "chwrt"] (at most 6; units should be comparable).
      hours: Window length in hours (1-168).

    Returns:
      {"chart": {...}} for the user plus per-series statistics (min / max / mean / last) for your answer.
    """
    try:
        api = Api(tool_context.state)
        if not params:
            raise ToolError("Give at least one parameter alias.")
        unit = await api.resolve_asset(asset)
        all_params = await api.parameters(unit)
        minutes = _hours(hours) * 60
        series, stats, thresholds = [], [], []
        for ref in params[:6]:
            p = await api.resolve_parameter(unit, ref, all_params)
            trend = await api.trend(int(p["id"]), minutes, f"the trend of {unit.get('alias')} {p.get('alias')}")
            pts = trend_points(trend)
            series.append({"name": f"{unit.get('alias')} {p.get('name') or p.get('alias')}", "unit": p.get("unit") or "", "points": pts})
            stats.append({"param": p.get("alias"), "unit": p.get("unit"), **series_stats(pts)})
            if len(params) == 1:
                thresholds = limit_lines(trend)
        title = f"{unit.get('alias')}: {', '.join(s['param'] for s in stats)} (last {minutes // 60} h)"
        chart = chart_payload(title, time_series_option(series, thresholds=thresholds))
        return {"chart": chart, "asset": unit.get("alias"), "hours": minutes // 60, "series": stats}
    except Exception as err:  # noqa: BLE001 - reported to the model as an error result
        return error_result(err)


async def chart_compare(tool_context: ToolContext, assets: list[str], param: str, hours: int = 24, mode: str = "line") -> dict[str, Any]:
    """Compare the same parameter across several units, e.g. chwst of CH_01..CH_03 (shown as a chart).

    Args:
      assets: Equipment aliases of the same type, e.g. ["CH_01", "CH_02", "CH_03"] (at most 8).
      param: Parameter alias present on every unit, e.g. "power_kw", "chwst", "fan_speed".
      hours: Window length in hours (1-168).
      mode: "line" for trends over time, "bar" for the window mean per unit.

    Returns:
      {"chart": {...}} plus per-unit statistics.
    """
    try:
        api = Api(tool_context.state)
        units = await api.resolve_assets(assets)
        if not units:
            raise ToolError("Give at least one equipment alias.")
        minutes = _hours(hours) * 60
        series, stats = [], []
        unit_label = ""
        for u in units[:MAX_SERIES]:
            p = await api.resolve_parameter(u, param)
            unit_label = p.get("unit") or unit_label
            trend = await api.trend(int(p["id"]), minutes, f"the trend of {u.get('alias')} {p.get('alias')}")
            pts = trend_points(trend)
            series.append({"name": str(u.get("alias") or u.get("name")), "unit": p.get("unit") or "", "points": pts})
            stats.append({"asset": u.get("alias"), "unit": p.get("unit"), **series_stats(pts)})
        title = f"{param} by unit (last {minutes // 60} h)"
        if (mode or "line").lower() == "bar":
            option = category_option("bar", [s["asset"] for s in stats], [{"name": f"Mean {param}", "data": [s.get("mean") for s in stats]}], unit_label)
            title = f"Mean {param} by unit (last {minutes // 60} h)"
        else:
            option = time_series_option(series)
        return {"chart": chart_payload(title, option), "param": param, "hours": minutes // 60, "units": stats}
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def make_chart(title: str, chart_type: str, x: list[str], series: list[dict[str, Any]], y_name: str = "") -> dict[str, Any]:
    """Draw a simple chart from numbers you already received from other tools (never invent or estimate values).

    Args:
      title: Short chart title.
      chart_type: "line", "bar", "scatter" or "pie".
      x: Category labels (for example unit aliases or dates); for pie, the slice names.
      series: [{"name": "MTBF (h)", "data": [number, ...]}] - one number per label, at most 8 series (pie uses the first).
      y_name: Y axis name with unit, e.g. "kW".

    Returns:
      {"chart": {...}} when the data is valid, else {"error": ...}.
    """
    try:
        kind = (chart_type or "").strip().lower()
        if kind not in CHART_TYPES:
            raise ToolError(f"chart_type must be one of {', '.join(CHART_TYPES)}.")
        if not isinstance(x, list) or not x:
            raise ToolError("x must be a non-empty list of labels.")
        if len(x) > MAX_CATEGORIES:
            raise ToolError(f"At most {MAX_CATEGORIES} labels.")
        if not isinstance(series, list) or not series:
            raise ToolError("series must be a non-empty list of {name, data}.")
        if len(series) > MAX_SERIES:
            raise ToolError(f"At most {MAX_SERIES} series.")
        clean = []
        for s in series:
            if not isinstance(s, Mapping):
                raise ToolError("Each series must be an object {name, data}.")
            data = s.get("data")
            if not isinstance(data, list) or len(data) != len(x):
                raise ToolError(f"Series '{safe_text(s.get('name'))}' must have exactly one value per label ({len(x)}).")
            bad = [v for v in data if v is not None and finite(v) is None]
            if bad:
                raise ToolError("Series values must be numbers (or null for a gap).")
            clean.append({"name": s.get("name"), "data": data})
        option = category_option(kind, x, clean, y_name)
        return {"chart": chart_payload(title, option), "points": len(x), "series": len(clean)}
    except Exception as err:  # noqa: BLE001
        return error_result(err)
