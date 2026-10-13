"""Maintenance order tools of the maintenance agent (DESIGN 6.5).

Each tool acts directly when the request is complete, as the user (their permissions apply), and
otherwise returns a form prefilled with what the user said (`forms.form_reply`): a value the user
did not give is never guessed by the tool. Results carry the order card (`maintenance_order`) the
chat shows.
"""

from __future__ import annotations

from typing import Any

from google.adk.tools import ToolContext

from . import maintenance as mo
from .backend_api import Api, ToolError, error_result


async def get_maintenance_options(tool_context: ToolContext, equipment: str) -> dict[str, Any]:
    """Who can work on an equipment and its issue codes (with SOPs), to fill an order.

    Args:
      equipment: Equipment alias or name, e.g. CH_01, "ETCH-02", MAU_03.
    """
    try:
        api = Api(tool_context.state)
        asset = await api.resolve_asset(equipment)
        people = await mo.technicians(api, int(asset["id"]))
        issues = await mo.issue_options(api, int(asset["id"]))
        return {
            "equipment": asset.get("alias") or asset.get("name"),
            "technicians": [{"username": p["value"], "name": p["label"]} for p in people],
            "issue_codes": [{"code": i["label"].split(" - ")[0], "name": i["label"].split(" - ", 1)[-1], "sop_minutes": i.get("minutes")} for i in issues],
        }
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def create_maintenance_order(
    tool_context: ToolContext,
    equipment: str = "",
    order_type: str = "",
    severity: str = "",
    planned_start: str = "",
    planned_end: str = "",
    priority: str = "",
    description: str = "",
    technicians: list[str] | None = None,
    issue_codes: list[str] | None = None,
    repeat: str = "",
    repeat_until: str = "",
    approve: bool = False,
) -> dict[str, Any]:
    """Create a maintenance order (Maintenance Order page). Pass only what the user said (or asked
    you to choose); leave the rest empty: when equipment, order_type, severity or planned_start is
    missing the user gets a prefilled form in the chat instead.

    Args:
      equipment: Equipment alias or name (CH_01, CT_02, ETCH_01, MAU_03, ...).
      order_type: "corrective" (repair a problem) or "preventive" (planned service).
      severity: critical, high, medium or low.
      planned_start: Plant-local start "YYYY-MM-DDTHH:MM".
      planned_end: Optional plant-local end "YYYY-MM-DDTHH:MM".
      priority: Optional high, medium or low.
      description: Optional work description.
      technicians: Optional names or usernames of the technicians to assign.
      issue_codes: Optional issue codes, e.g. ["CHL-005"] (see get_maintenance_options).
      repeat: Preventive only, optional: daily, weekly, monthly or yearly.
      repeat_until: With repeat: plant-local end of the series "YYYY-MM-DDTHH:MM".
      approve: Also approve it right away (needs a technician and the approve permission).
    """
    try:
        api = Api(tool_context.state)
        values: dict[str, Any] = {"description": description.strip() or None, "approve": approve}
        problems: list[str] = []
        asset = None
        if equipment.strip():
            try:
                asset = await api.resolve_asset(equipment)
                values["asset_id"] = int(asset["id"])
            except ToolError as err:
                problems.append(str(err))
        for key, table, raw, what in (
            ("maintenance_type_id", mo.ORDER_TYPE, order_type, "order type"),
            ("severity_id", mo.SEVERITY, severity, "severity"),
            ("priority_id", mo.PRIORITY, priority, "priority"),
            ("repeat_type_id", mo.REPEAT, repeat, "repeat"),
        ):
            try:
                values[key] = mo.lookup(table, raw.strip() or None, what)
            except ToolError as err:
                problems.append(str(err))
        for key, raw in (("planned_start", planned_start), ("planned_end", planned_end), ("repeat_end", repeat_until)):
            if raw.strip():
                try:
                    mo.to_backend_time(raw, key.replace("_", " "))
                    values[key] = mo.form_time(raw) or raw.strip()
                except ToolError as err:
                    problems.append(str(err))
        if asset is not None:
            try:
                values["technicians"] = mo.match_people(technicians, await mo.technicians(api, int(asset["id"])))
                values["instructions"] = mo.match_issues(issue_codes, await mo.issue_options(api, int(asset["id"])))
            except ToolError as err:
                problems.append(str(err))
        missing = [n for k, n in (("asset_id", "equipment"), ("maintenance_type_id", "order type"), ("severity_id", "severity"),
                                  ("planned_start", "planned start")) if not values.get(k)]
        if missing or problems:
            reply = mo.forms.form_reply(await mo.create_form(api, {k: v for k, v in values.items() if v is not None}), missing)
            if problems:
                reply["problems"] = problems
            return reply
        return await mo.create(api, values)
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def edit_maintenance_order(
    tool_context: ToolContext,
    order: str,
    equipment: str = "",
    order_type: str = "",
    severity: str = "",
    priority: str = "",
    planned_start: str = "",
    planned_end: str = "",
    description: str = "",
    technicians: list[str] | None = None,
    issue_codes: list[str] | None = None,
) -> dict[str, Any]:
    """Change a requested or open maintenance order: its fields or the assigned technicians
    (technicians replaces the whole list). With nothing to change, the user gets the order in an
    editable form.

    Args:
      order: Order name (e.g. CM-10-26-004) or id.
      equipment: New equipment alias, optional.
      order_type: corrective or preventive, optional.
      severity: critical, high, medium or low, optional.
      priority: high, medium or low, optional.
      planned_start: New plant-local start "YYYY-MM-DDTHH:MM", optional.
      planned_end: New plant-local end "YYYY-MM-DDTHH:MM", optional.
      description: New description, optional.
      technicians: Names or usernames: the new full list of assigned technicians, optional.
      issue_codes: Issue codes: the new full list of the order's issues, optional.
    """
    try:
        api = Api(tool_context.state)
        order_id = await mo.resolve_order(api, order)
        d = await mo.order_detail(api, order_id)
        changes: dict[str, Any] = {}
        if equipment.strip():
            changes["asset_id"] = int((await api.resolve_asset(equipment))["id"])
        asset_id = changes.get("asset_id") or int(d["equipment"])
        for key, table, raw, what in (
            ("maintenance_type_id", mo.ORDER_TYPE, order_type, "order type"),
            ("severity_id", mo.SEVERITY, severity, "severity"),
            ("priority_id", mo.PRIORITY, priority, "priority"),
        ):
            if raw.strip():
                changes[key] = mo.lookup(table, raw, what)
        for key, raw in (("planned_start", planned_start), ("planned_end", planned_end)):
            if raw.strip():
                mo.to_backend_time(raw, key.replace("_", " "))
                changes[key] = raw.strip()
        if description.strip():
            changes["description"] = description.strip()
        if technicians is not None:
            changes["technicians"] = mo.match_people(technicians, await mo.technicians(api, asset_id, order_id))
        if issue_codes is not None:
            changes["instructions"] = mo.match_issues(issue_codes, await mo.issue_options(api, asset_id))
        if not changes:
            form = await mo.edit_form(api, order_id, {}, kind="maintenance_order.edit", title="Edit", submit_label="Save changes")
            return mo.forms.form_reply(form)
        return await mo.act(api, order_id, "edit", changes)
    except Exception as err:  # noqa: BLE001
        return error_result(err)


async def maintenance_order_action(
    tool_context: ToolContext,
    order: str,
    action: str,
    reason: str = "",
    note: str = "",
    technicians: list[str] | None = None,
    all_valid: bool | None = None,
    equipment_down: bool = False,
) -> dict[str, Any]:
    """Move a maintenance order through its workflow, as on the Maintenance Order page.

    Actions: approve (REQUESTED -> OPEN, needs at least one technician: pass technicians or keep the
    assigned ones), reject (needs reason), start (OPEN -> IN_PROGRESS; equipment_down marks the unit
    down while the work runs), pause (needs reason), resume, complete (IN_PROGRESS -> DONE), close
    (DONE -> CLOSED), add_note (work update, needs note), delete (always confirmed by the user in a
    form). Without a required reason, note, technician or the issue verdicts (all_valid) the user
    gets a form instead.

    Args:
      order: Order name (e.g. CM-10-26-004) or id.
      action: approve, reject, start, pause, resume, complete, close, add_note or delete.
      reason: Reason for reject or pause.
      note: Work note (add_note) or closing note (complete / close).
      technicians: approve: names or usernames to assign (replaces the list).
      all_valid: complete / close: true when every issue's diagnosis was confirmed and repaired, false
        when not; leave empty to let the user tick each issue in a form.
      equipment_down: start: mark the equipment down (planned maintenance) during the work.
    """
    try:
        api = Api(tool_context.state)
        action = (action or "").strip().lower()
        if action not in mo.ACTIONS:
            raise ToolError(f"Unknown action '{action}'. Use {', '.join(mo.ACTIONS)}.")
        order_id = await mo.resolve_order(api, order)
        values: dict[str, Any] = {"reason": reason.strip(), "note": note.strip(), "equipment_down": equipment_down}
        if action == "delete":
            return mo.forms.form_reply(await mo.action_form(api, order_id, "delete", values))
        if action in ("reject", "pause") and not values["reason"]:
            return mo.forms.form_reply(await mo.action_form(api, order_id, action, values), ["reason"])
        if action == "add_note" and not values["note"]:
            return mo.forms.form_reply(await mo.action_form(api, order_id, action, values), ["note"])
        if action in ("complete", "close"):
            if all_valid is None:
                return mo.forms.form_reply(await mo.action_form(api, order_id, action, values), ["issue verdicts"])
            values["all_valid"] = all_valid
        if action == "approve":
            d = await mo.order_detail(api, order_id)
            if technicians:
                values["technicians"] = mo.match_people(technicians, await mo.technicians(api, int(d["equipment"]), order_id))
            elif not str(d.get("technicians") or "").strip():
                form = await mo.edit_form(api, order_id, {}, kind="maintenance_order.approve", title="Approve", submit_label="Approve")
                return mo.forms.form_reply(form, ["technicians"])
        return await mo.act(api, order_id, action, values)
    except Exception as err:  # noqa: BLE001
        return error_result(err)


MAINTENANCE_TOOLS = [get_maintenance_options, create_maintenance_order, edit_maintenance_order, maintenance_order_action]
