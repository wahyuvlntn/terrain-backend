"""Simulation forms of the twin agent (DESIGN 6.5): a chiller plant what-if scenario and a FAB1
production plan, for requests that don't say what to simulate.

The user fills the form in the chat; submitting it queues the simulation at once as theirs
(`simulation/submit`, `production/submitplan`: a person's request, like the Simulation page's
builder; the backend applies their `simulation.create` permission). Specs the agent writes itself
still go through propose_simulation / propose_production_plan and a person's approval.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from google.adk.tools import ToolContext

from . import forms
from .backend_api import Api, ToolError, error_result, normalise_alias

# writable CHWS_01 supervisory set points: label, unit, min, max, step, mode that must be Manual
SETPOINTS: dict[str, tuple[str, str, float, float, float, str | None]] = {
    "chws_sp": ("CHW supply set point", "°C", 5.5, 8.0, 0.1, "chws_reset_mode"),
    "approach_sp": ("Tower approach set point", "K", 2.0, 8.0, 0.1, None),
    "riser_dp_sp": ("Riser DP set point", "kPa", 80, 160, 1, None),
    "chillers_sp": ("Duty chillers", "", 1, 3, 1, "staging_mode"),
    "cw_min_temp": ("Minimum condenser water temperature", "°C", 15, 28, 0.5, None),
    "twv_min_pos": ("Condenser 3-way valve minimum position", "%", 0, 50, 1, None),
}
CHANGES = [("setpoint", "Change a set point"), ("fault", "Inject a fault"), ("trip", "Trip a unit"), ("none", "No change (baseline only)")]
TRIP_PREFIXES = ("CH_", "CT_", "CHWP_", "CWP_", "PCWP_")
PRODUCTS = ("MCU40", "WSOC40")


def _num(v: Any) -> float | None:
    if v is None or v == "" or isinstance(v, bool):
        return None
    try:
        return float(v)
    except (TypeError, ValueError):
        raise ToolError(f"'{v}' is not a number.") from None


def _variant_name(text: Any, fallback: str) -> str:
    name = "".join(ch for ch in str(text or "").strip() if ch.isalnum() or ch in " _.-")[:40].strip()
    return name if name and name[0].isalnum() else fallback


async def _catalog(api: Api) -> list[dict[str, Any]]:
    rows = await api.call("simulation/getfaultcatalog", {}, what="the fault catalog") or []
    return [r for r in rows if isinstance(r, Mapping) and r.get("kind")]


def _sim_card(row: Mapping[str, Any], rationale: str) -> dict[str, Any]:
    return {
        "job_id": row.get("id") or row.get("job_id"),
        "title": row.get("title"),
        "status": row.get("status") or "QUEUED",
        "approval_required": False,
        "rationale": rationale,
        "spec_summary": None,
    }


# ---------------------------------------------------------------------------
# Chiller plant scenario
# ---------------------------------------------------------------------------


async def scenario_form(api: Api, v: Mapping[str, Any]) -> dict[str, Any]:
    catalog = await _catalog(api)
    assets = await api.assets()
    fields = [
        forms.field("title", "Title", "text", required=True, value=v.get("title")),
        forms.field("start", "Start", "datetime", value=v.get("start"), help="Plant time; empty = now"),
        forms.field("duration_hours", "Duration", "number", required=True, min=0.25, max=48, step=0.25, unit="h", value=v.get("duration_hours", 4)),
        forms.field("initial", "Starting state", "select", required=True, value=v.get("initial") or "live",
                    options=[{"value": "live", "label": "The plant as it is now"}, {"value": "nominal", "label": "Nominal design state"}]),
        forms.field("change", "What to compare with the baseline", "select", required=True, value=v.get("change"), options=forms.options(CHANGES)),
        forms.field("parameter", "Set point", "select", value=v.get("parameter"), visible_if={"field": "change", "in": ["setpoint"]},
                    options=[{"value": k, "label": f"{lab} ({lo:g}-{hi:g}{(' ' + u) if u else ''})"} for k, (lab, u, lo, hi, _, _) in SETPOINTS.items()]),
        forms.field("value", "New value", "number", value=v.get("value"), visible_if={"field": "change", "in": ["setpoint"]}),
        forms.field("fault", "Fault", "select", value=v.get("fault"), visible_if={"field": "change", "in": ["fault"]},
                    options=[{"value": f["kind"], "label": f"{f['label']} ({f.get('code')})"} for f in catalog]),
    ]
    for f in catalog:
        unit = f" {f['unit']}" if f.get("unit") else ""
        fields.append(forms.field(
            f"asset_{f['kind']}", "Equipment", "select", visible_if={"field": "fault", "in": [f["kind"]]},
            value=v.get("asset") if v.get("fault") == f["kind"] else None,
            options=[{"value": a, "label": a.replace("_", "-")} for a in f.get("assets") or []],
        ))
        fields.append(forms.field(
            f"intensity_{f['kind']}", "Intensity (documented values)", "select", visible_if={"field": "fault", "in": [f["kind"]]},
            value=v.get("intensity") if v.get("fault") == f["kind"] else None,
            options=[{"value": x, "label": f"{x:g}{unit}"} for x in f.get("intensities") or []],
        ))
    fields += [
        forms.field("trip_asset", "Unit to trip", "select", value=v.get("asset") if v.get("change") == "trip" else None,
                    visible_if={"field": "change", "in": ["trip"]},
                    options=[{"value": a["alias"], "label": f"{a['alias'].replace('_', '-')} · {a.get('name') or ''}"}
                             for a in sorted(assets, key=lambda a: str(a.get("alias"))) if str(a.get("alias") or "").startswith(TRIP_PREFIXES)]),
        forms.field("at_minute", "At minute", "number", min=0, step=5, value=v.get("at_minute", 0), visible_if={"field": "change", "in": ["setpoint", "fault", "trip"]},
                    help="Minutes after the start"),
        forms.field("variant", "Name of the what-if run", "text", value=v.get("variant"), visible_if={"field": "change", "in": ["setpoint", "fault", "trip"]}),
        forms.field("load_pct", "Cooling load", "number", min=50, max=130, step=5, unit="%", value=v.get("load_pct", 100), help="Of today's load, both runs"),
        forms.field("rationale", "Why (optional)", "textarea", value=v.get("rationale")),
    ]
    return forms.new_form("simulation.scenario", "New chiller plant simulation", fields,
                          description="Compares a baseline run with your change on the digital twin. Submitting queues it now.",
                          submit_label="Run simulation")


def scenario_spec(v: Mapping[str, Any]) -> tuple[str, dict[str, Any], str]:
    """(title, spec, rationale) of a submitted scenario form."""
    from .maintenance import to_backend_time

    title = str(v.get("title") or "").strip()
    if len(title) < 3:
        raise ToolError("Give the simulation a title (at least 3 characters).")
    hours = _num(v.get("duration_hours"))
    if hours is None or not 10 <= hours * 60 <= 2880:
        raise ToolError("The duration must be between 10 minutes and 48 hours.")
    at = _num(v.get("at_minute")) or 0.0
    if not 0 <= at < hours * 60:
        raise ToolError("The change must happen within the simulated time.")
    change = str(v.get("change") or "").strip()
    actions: list[dict[str, Any]] = []
    name = "baseline only"
    if change == "setpoint":
        param = str(v.get("parameter") or "")
        if param not in SETPOINTS:
            raise ToolError("Choose the set point to change.")
        label, unit, lo, hi, _, mode = SETPOINTS[param]
        value = _num(v.get("value"))
        if value is None or not lo <= value <= hi:
            raise ToolError(f"The new {label.lower()} must be between {lo:g} and {hi:g}{(' ' + unit) if unit else ''}.")
        if mode:
            actions.append({"at_min": at, "kind": "control", "asset": "CHWS_01", "param": mode, "value": 1})
        actions.append({"at_min": at, "kind": "control", "asset": "CHWS_01", "param": param, "value": value})
        name = f"{label} {value:g}{(' ' + unit) if unit else ''}"
    elif change == "fault":
        kind = str(v.get("fault") or "")
        asset, intensity = v.get(f"asset_{kind}"), _num(v.get(f"intensity_{kind}"))
        if not kind or not asset or intensity is None:
            raise ToolError("Choose the fault, its equipment and one of its documented intensities.")
        actions.append({"at_min": at, "kind": "fault", "asset": str(asset), "fault": kind, "value": intensity})
        name = f"{kind} {asset}"
    elif change == "trip":
        asset = str(v.get("trip_asset") or "")
        if not asset:
            raise ToolError("Choose the unit to trip.")
        actions.append({"at_min": at, "kind": "trip", "asset": asset})
        name = f"{asset} trip"
    elif change != "none":
        raise ToolError("Choose what to compare with the baseline.")
    variants: list[dict[str, Any]] = [{"name": "baseline", "actions": []}]
    if actions:
        variants.append({"name": _variant_name(v.get("variant"), _variant_name(name, "what-if")), "actions": actions})
    load = _num(v.get("load_pct")) or 100.0
    spec = {
        "start": to_backend_time(v.get("start"), "start") if v.get("start") else None,
        "duration_min": round(hours * 60),
        "initial": "nominal" if v.get("initial") == "nominal" else "live",
        "stochastic": False,
        "seed": 0,
        "weather": {"dry_bulb_offset": 0.0, "wet_bulb_offset": 0.0},
        "load_scale": max(0.5, min(1.3, load / 100)),
        "actions": [],
        "variants": variants,
    }
    rationale = str(v.get("rationale") or "").strip() or "Submitted from LEVO's simulation form."
    return title, spec, rationale


@forms.handler("simulation.scenario")
async def _submit_scenario(api: Api, v: dict[str, Any]) -> dict[str, Any]:
    title, spec, rationale = scenario_spec(v)
    row = await api.call("simulation/submit", {"title": title, "rationale": rationale[:4000], "spec": spec}, what="the simulation") or {}
    card = _sim_card(row, rationale)
    return {"message": f"Simulation #{card['job_id']} '{title}' is queued and runs now.", "simulation": card}


# ---------------------------------------------------------------------------
# FAB1 production plan
# ---------------------------------------------------------------------------


def _tool_options(assets: list[dict[str, Any]]) -> list[dict[str, Any]]:
    tools = [a for a in assets if a.get("alias") and str(a.get("type_name") or "").lower().endswith(
        ("tool", "scanner", "track", "furnace", "implanter", "station", "asher", "system"))
        and not str(a.get("alias")).startswith(("CHWS", "SCR", "PCH", "DP_", "MAU", "CDA"))]
    return [{"value": a["alias"], "label": f"{a['alias'].replace('_', '-')} · {a.get('name') or ''}"} for a in sorted(tools, key=lambda a: a["alias"])]


async def production_form(api: Api, v: Mapping[str, Any]) -> dict[str, Any]:
    tools = _tool_options(await api.assets())
    fields = [
        forms.field("title", "Title", "text", required=True, value=v.get("title")),
        forms.field("horizon_days", "Horizon", "number", required=True, min=1, max=90, step=1, unit="days", value=v.get("horizon_days", 30),
                    help="A lot takes about three weeks: 30-60 days show the outs"),
        forms.field("variant", "Name of the alternative plan", "text", value=v.get("variant")),
        forms.field("starts_per_day", "Lot starts per day", "number", min=0, max=12, step=0.5, value=v.get("starts_per_day"), help="Empty = current plan"),
        forms.field("mcu40_share_pct", "MCU40 share of the starts", "number", min=0, max=100, step=5, unit="%", value=v.get("mcu40_share_pct"),
                    help="The rest is WSOC40; empty = current mix"),
        forms.field("hot_share_pct", "Hot lots", "number", min=0, max=50, step=1, unit="%", value=v.get("hot_share_pct")),
        forms.field("dispatch", "Dispatch rule", "select", value=v.get("dispatch"),
                    options=[{"value": "FIFO", "label": "FIFO (first in, first out)"}, {"value": "CR", "label": "Critical ratio (due date)"}]),
        forms.field("furnace_wait_h", "Furnace waits for a full boat up to", "number", min=0, max=48, step=1, unit="h", value=v.get("furnace_wait_h")),
        forms.field("furnace_min_lots", "Furnace minimum lots per batch", "number", min=1, max=4, step=1, value=v.get("furnace_min_lots")),
        forms.field("vehicles", "OHT vehicles", "number", min=1, max=30, step=1, value=v.get("vehicles")),
        forms.field("tool_down", "Take a tool down", "select", options=tools, value=v.get("tool_down")),
        forms.field("tool_down_day", "From day", "number", min=0, max=89, step=0.5, value=v.get("tool_down_day", 1), visible_if={"field": "tool_down", "in": [t["value"] for t in tools]}),
        forms.field("tool_down_hours", "For", "number", min=1, max=2160, step=1, unit="h", value=v.get("tool_down_hours", 24), visible_if={"field": "tool_down", "in": [t["value"] for t in tools]}),
        forms.field("add_tool", "Add tools like", "select", options=tools, value=v.get("add_tool")),
        forms.field("add_count", "How many", "number", min=1, max=3, step=1, value=v.get("add_count", 1), visible_if={"field": "add_tool", "in": [t["value"] for t in tools]}),
        forms.field("rationale", "Why (optional)", "textarea", value=v.get("rationale")),
    ]
    return forms.new_form("simulation.production", "New production plan", fields,
                          description="Runs the current plan and your alternative on the FAB1 production model. Submitting queues it now.",
                          submit_label="Run plan")


def production_spec(v: Mapping[str, Any]) -> tuple[str, dict[str, Any], str]:
    title = str(v.get("title") or "").strip()
    if len(title) < 3:
        raise ToolError("Give the plan a title (at least 3 characters).")
    days = _num(v.get("horizon_days"))
    if days is None or not 1 <= days <= 90:
        raise ToolError("The horizon must be 1 to 90 days.")
    plan: dict[str, Any] = {}
    if (x := _num(v.get("starts_per_day"))) is not None:
        plan["starts_per_day"] = x
    if (x := _num(v.get("mcu40_share_pct"))) is not None:
        share = max(0.0, min(100.0, x)) / 100
        plan["mix"] = {PRODUCTS[0]: round(share, 3), PRODUCTS[1]: round(1 - share, 3)}
    if (x := _num(v.get("hot_share_pct"))) is not None:
        plan["hot_share"] = max(0.0, min(50.0, x)) / 100
    if v.get("dispatch") in ("FIFO", "CR"):
        plan["dispatch"] = v["dispatch"]
    if (x := _num(v.get("furnace_wait_h"))) is not None:
        plan["furnace_wait_h"] = x
    if (x := _num(v.get("furnace_min_lots"))) is not None:
        plan["furnace_min_lots"] = int(x)
    if (x := _num(v.get("vehicles"))) is not None:
        plan["vehicles"] = int(x)
    actions: list[dict[str, Any]] = []
    if v.get("tool_down"):
        actions.append({"kind": "tool_down", "asset": str(v["tool_down"]), "at_day": _num(v.get("tool_down_day")) or 0.0,
                        "duration_h": _num(v.get("tool_down_hours")) or 24.0, "reason": "What-if from LEVO"})
    if v.get("add_tool"):
        actions.append({"kind": "add_tool", "asset": str(v["add_tool"]), "count": int(_num(v.get("add_count")) or 1)})
    variants: list[dict[str, Any]] = [{"name": "Current plan", "plan": {}, "actions": []}]
    if plan or actions:
        variants.append({"name": _variant_name(v.get("variant"), "Alternative"), "plan": plan, "actions": actions})
    spec = {"kind": "production", "horizon_days": days, "initial": "live", "seed": 0, "breakdowns": True, "plan": {}, "actions": [], "variants": variants}
    rationale = str(v.get("rationale") or "").strip() or "Submitted from LEVO's production plan form."
    return title, spec, rationale


@forms.handler("simulation.production")
async def _submit_production(api: Api, v: dict[str, Any]) -> dict[str, Any]:
    title, spec, rationale = production_spec(v)
    row = await api.call("production/submitplan", {"title": title, "rationale": rationale[:4000], "spec": spec}, what="the production plan") or {}
    card = _sim_card(row, rationale)
    return {"message": f"Production plan #{card['job_id']} '{title}' is queued and runs now.", "simulation": card}


# ---------------------------------------------------------------------------
# Tools of the twin agent
# ---------------------------------------------------------------------------


async def show_scenario_form(
    tool_context: ToolContext,
    title: str = "",
    change: str = "",
    parameter: str = "",
    value: float | None = None,
    fault: str = "",
    asset: str = "",
    intensity: float | None = None,
    duration_hours: float | None = None,
    start: str = "",
    initial: str = "",
    at_minute: float | None = None,
) -> dict[str, Any]:
    """Show the chiller plant simulation form, prefilled with what the user said, when they want a
    simulation but did not say what to change or compare. Submitting it runs the simulation as
    theirs (no approval). Leave unknown arguments empty.

    Args:
      title: Simulation title.
      change: setpoint, fault, trip or none.
      parameter: setpoint: chws_sp, approach_sp, riser_dp_sp, chillers_sp, cw_min_temp or twv_min_pos.
      value: setpoint: the new value.
      fault: fault: an LBNL fault kind from get_fault_catalog (e.g. ct_fouling).
      asset: fault or trip: the equipment alias (e.g. CT_01, CH_02).
      intensity: fault: one of the documented intensities.
      duration_hours: Simulated hours (0.25-48).
      start: Plant-local start "YYYY-MM-DDTHH:MM" (empty = now).
      initial: live (plant as it is) or nominal.
      at_minute: Minutes after the start when the change happens.
    """
    try:
        api = Api(tool_context.state)
        v = {k: x for k, x in {
            "title": title.strip(), "change": change.strip().lower(), "parameter": parameter.strip(), "value": value, "fault": fault.strip(),
            "asset": normalise_alias(asset) if asset.strip() else "", "intensity": intensity, "duration_hours": duration_hours, "start": start.strip(),
            "initial": initial.strip().lower(), "at_minute": at_minute,
        }.items() if x not in (None, "")}
        return forms.form_reply(await scenario_form(api, v))
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def show_production_plan_form(
    tool_context: ToolContext,
    title: str = "",
    horizon_days: float | None = None,
    starts_per_day: float | None = None,
    mcu40_share_pct: float | None = None,
    hot_share_pct: float | None = None,
    dispatch: str = "",
    furnace_wait_h: float | None = None,
    vehicles: int | None = None,
    tool_down: str = "",
    tool_down_day: float | None = None,
    tool_down_hours: float | None = None,
    add_tool: str = "",
) -> dict[str, Any]:
    """Show the FAB1 production plan form, prefilled with what the user said, when they want a
    production plan simulation but did not say what to change. Submitting it runs the plan as
    theirs (no approval). Leave unknown arguments empty.

    Args:
      title: Plan title.
      horizon_days: Simulated days (1-90).
      starts_per_day: Lot starts per day of the alternative plan (0-12).
      mcu40_share_pct: MCU40 share of the starts in % (the rest WSOC40).
      hot_share_pct: Hot lot share in % (0-50).
      dispatch: FIFO or CR (critical ratio).
      furnace_wait_h: Hours a furnace waits for a full boat (0-48).
      vehicles: OHT vehicles (1-30).
      tool_down: Tool alias taken down in the alternative (e.g. ETCH_02).
      tool_down_day: Day the tool goes down.
      tool_down_hours: Hours it stays down.
      add_tool: Tool alias of which a copy is added.
    """
    try:
        api = Api(tool_context.state)
        v = {k: x for k, x in {
            "title": title.strip(), "horizon_days": horizon_days, "starts_per_day": starts_per_day, "mcu40_share_pct": mcu40_share_pct,
            "hot_share_pct": hot_share_pct, "dispatch": dispatch.strip().upper(), "furnace_wait_h": furnace_wait_h, "vehicles": vehicles,
            "tool_down": normalise_alias(tool_down) if tool_down.strip() else "", "tool_down_day": tool_down_day,
            "tool_down_hours": tool_down_hours, "add_tool": normalise_alias(add_tool) if add_tool.strip() else "",
        }.items() if x not in (None, "")}
        return forms.form_reply(await production_form(api, v))
    except Exception as err:  # noqa: BLE001
        return error_result(err)


SIM_FORM_TOOLS = [show_scenario_form, show_production_plan_form]
