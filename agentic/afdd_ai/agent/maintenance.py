"""Maintenance orders for the assistant: lookups, the order card, the actions (create, edit,
approve, reject, start, pause, resume, complete, close, work note, delete) and their forms.

The actions call the scripts of the Maintenance Order page (`maintenance_order/submit` and the
work report scripts) with the user's session, so the backend applies the user's permissions
(create / update / approve / report / delete, assigned technician) and location scope.
`maintenance_order/submit` clears the priority and the planned end it is not sent, so edits and
approvals resend the order's current values with the changes.
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from datetime import datetime
from typing import Any

from . import forms
from .backend_api import Api, ToolError, parse_ts, plant_tz

SEVERITY = {"CRITICAL": 1, "HIGH": 2, "MEDIUM": 3, "LOW": 4}
PRIORITY = {"HIGH": 1, "MEDIUM": 2, "LOW": 3}
ORDER_TYPE = {"CORRECTIVE": 1, "PREVENTIVE": 2}
REPEAT = {"DAILY": 1, "WEEKLY": 2, "MONTHLY": 3, "YEARLY": 4}
ACTIONS = ("approve", "reject", "start", "pause", "resume", "complete", "close", "add_note", "delete")
WORK = "maintenance_order_work_report/"
DIAGNOSTIC, REPAIR = 1, 2

_NAME_RE = re.compile(r"^[A-Z]{2}-\d{2}-\d{2}-\d{3,}$")


def label_of(table: Mapping[str, int], value: Any) -> str | None:
    for k, v in table.items():
        if v == value:
            return k.replace("_", " ").title()
    return None


def lookup(table: Mapping[str, int], ref: Any, what: str) -> int | None:
    """An id of `table` from its id or name ("critical", "High", 2); None when empty."""
    if ref is None or ref == "":
        return None
    if isinstance(ref, bool):
        raise ToolError(f"Invalid {what}.")
    if isinstance(ref, (int, float)) or (isinstance(ref, str) and ref.strip().isdigit()):
        v = int(ref)
        if v in table.values():
            return v
    else:
        key = str(ref).strip().upper().replace(" ", "_")
        if key in table:
            return table[key]
    raise ToolError(f"Unknown {what} '{ref}'. Use {', '.join(k.lower() for k in table)}.")


def to_backend_time(value: Any, what: str) -> str | None:
    """A plant-local "YYYY-MM-DDTHH:MM" (or ISO with offset) as ISO with offset; None when empty."""
    if value is None or value == "":
        return None
    try:
        dt = datetime.fromisoformat(str(value).strip().replace("Z", "+00:00"))
    except ValueError:
        raise ToolError(f"The {what} must be a date and time like 2026-10-16T09:00 (plant time), not '{value}'.") from None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=plant_tz())
    return dt.isoformat(timespec="seconds")


def form_time(value: Any) -> str | None:
    """A backend time as the plant-local "YYYY-MM-DDTHH:MM" of a datetime field."""
    if not value:
        return None
    dt = parse_ts(value) if isinstance(value, str) and ("Z" in value or "+" in value[10:]) else None
    if dt is None:
        try:
            dt = datetime.fromisoformat(str(value))
        except ValueError:
            return None
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=plant_tz())
    return dt.astimezone(plant_tz()).strftime("%Y-%m-%dT%H:%M")


def _ids(csv: Any) -> list[int]:
    out = []
    for part in str(csv or "").split(","):
        part = part.strip()
        if part.isdigit():
            out.append(int(part))
    return out


def _names(csv: Any) -> list[str]:
    return [p.strip() for p in str(csv or "").split(",") if p.strip()]


# ---------------------------------------------------------------------------
# Lookups
# ---------------------------------------------------------------------------


async def resolve_order(api: Api, ref: Any) -> int:
    """The id of an order from its name (CM-10-26-004) or id."""
    text = str(ref or "").strip()
    if not text:
        raise ToolError("Which maintenance order? Give its name (e.g. CM-10-26-004) or id.")
    if text.isdigit():
        return int(text)
    listing = await api.call("maintenance_order/get_maintenance_order_list", {"filter": {}, "search": text}, what=f"order {text}")
    for row in (listing or {}).get("data") or []:
        if str(row.get("name") or "").upper() == text.upper():
            return int(row["id"])
    if not _NAME_RE.match(text.upper()):
        raise ToolError(f"'{text}' is not an order name; give one like CM-10-26-004 (find it with get_maintenance_orders).")
    raise ToolError(f"No visible maintenance order is named '{text}'.")


async def order_detail(api: Api, order_id: int) -> dict[str, Any]:
    rows = await api.call("maintenance_order/get_maintenance_order_detail", {"order_id": order_id}, what=f"maintenance order {order_id}")
    if not rows:
        raise ToolError(f"Maintenance order {order_id} was not found (or you cannot see it).")
    return dict(rows[0])


async def technicians(api: Api, equipment_id: int, order_id: int | None = None) -> list[dict[str, Any]]:
    """[{value: username, label: name}] of the people who may work on the equipment."""
    body = {"equipment_id": equipment_id, "building_id": None, "planned_start": None, "planned_end": None, "maintenance_id": order_id or 0}
    rows = await api.call("maintenance_order/get_technicians", body, what="the technicians") or []
    return [{"value": str(r.get("value")), "label": str(r.get("label") or r.get("value"))} for r in rows if r.get("value")]


async def issue_options(api: Api, equipment_id: int) -> list[dict[str, Any]]:
    """[{value: alarm_code_id, label: "CODE - name", minutes}] of the equipment's issue codes with SOPs."""
    rows = await api.call("maintenance_order/get_maintenance_instructions", {"asset_id": equipment_id}, what="the issue codes") or []
    return [{"value": int(r["value"]), "label": str(r.get("label")), "minutes": r.get("estimated_time_minute")} for r in rows if r.get("value") is not None]


def match_people(refs: list[str] | None, people: list[dict[str, Any]]) -> list[str]:
    """Usernames for names / usernames the user gave (case-insensitive, first-name match allowed)."""
    out: list[str] = []
    for ref in refs or []:
        want = str(ref).strip().lower()
        if not want:
            continue
        hit = next((p for p in people if p["value"].lower() == want or p["label"].lower() == want), None)
        if hit is None:
            hits = [p for p in people if want in p["label"].lower() or want in p["value"].lower()]
            if len(hits) == 1:
                hit = hits[0]
            elif hits:
                raise ToolError(f"'{ref}' matches several people ({', '.join(p['label'] for p in hits[:6])}); be more specific.")
        if hit is None:
            known = ", ".join(p["label"] for p in people[:20]) or "nobody (no technician covers this equipment)"
            raise ToolError(f"'{ref}' cannot work on this equipment. Technicians who can: {known}.")
        if hit["value"] not in out:
            out.append(hit["value"])
    return out


def match_issues(refs: list[Any] | None, issues: list[dict[str, Any]]) -> list[int]:
    """Alarm code ids for codes ("CHL-005"), ids or names the user gave."""
    out: list[int] = []
    for ref in refs or []:
        if isinstance(ref, int) or str(ref).strip().isdigit():
            want_id = int(ref)
            if any(i["value"] == want_id for i in issues):
                out.append(want_id)
                continue
        text = str(ref).strip().lower()
        hit = next((i for i in issues if i["label"].lower().split(" - ")[0] == text or i["label"].lower() == text), None)
        if hit is None:
            hits = [i for i in issues if text in i["label"].lower()]
            hit = hits[0] if len(hits) == 1 else None
        if hit is None:
            known = ", ".join(i["label"].split(" - ")[0] for i in issues[:30])
            raise ToolError(f"Unknown issue code '{ref}' for this equipment. Its codes: {known}.")
        if hit["value"] not in out:
            out.append(hit["value"])
    return out


async def equipment_options(api: Api) -> list[dict[str, Any]]:
    rows = await api.assets()
    out = [
        {"value": int(a["id"]), "label": f"{str(a.get('alias') or '').replace('_', '-')} · {a.get('name') or ''}".strip(" ·")}
        for a in rows
        if a.get("id") is not None
    ]
    return sorted(out, key=lambda o: o["label"])


async def order_card(api: Api, order_id: int, action: str, extra: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """What the chat shows of an order after an action (the `maintenance_order` event)."""
    d = await order_detail(api, order_id)
    head = await api.call("maintenance_order/get_maintenance_name_and_type", {"maintenance_id": order_id}, what="the order name") or {}
    equipment_id = int(d["equipment"]) if str(d.get("equipment") or "").isdigit() else None
    asset = next((a for a in await api.assets() if a.get("id") == equipment_id), {}) if equipment_id else {}
    usernames = _names(d.get("technicians"))
    names = usernames
    if usernames and equipment_id:
        try:
            people = {p["value"]: p["label"] for p in await technicians(api, equipment_id, order_id)}
            names = [people.get(u, u) for u in usernames]
        except ToolError:
            pass
    return {
        "id": order_id,
        "name": head.get("name") or f"#{order_id}",
        "action": action,
        "status": d.get("status"),
        "type": label_of(ORDER_TYPE, int(d["maintenance_type_id"])) if str(d.get("maintenance_type_id") or "").isdigit() else None,
        "severity": label_of(SEVERITY, int(d["severity"])) if str(d.get("severity") or "").isdigit() else None,
        "priority": label_of(PRIORITY, int(d["priority"])) if str(d.get("priority") or "").isdigit() else None,
        "equipment": asset.get("alias"),
        "equipment_name": asset.get("name"),
        "planned_start": d.get("planned_start"),
        "planned_end": d.get("planned_end"),
        "technicians": names,
        "description": str(d.get("description") or "")[:400],
        **dict(extra or {}),
    }


# ---------------------------------------------------------------------------
# Actions (shared by the tools and the submitted forms)
# ---------------------------------------------------------------------------


async def create(api: Api, v: Mapping[str, Any]) -> dict[str, Any]:
    """New order(s) (a repeating preventive order creates its series) from resolved values."""
    asset = await api.resolve_asset(v["asset_id"])
    type_id = lookup(ORDER_TYPE, v.get("maintenance_type_id"), "order type")
    severity = lookup(SEVERITY, v.get("severity_id"), "severity")
    start = to_backend_time(v.get("planned_start"), "planned start")
    if type_id is None or severity is None or start is None:
        raise ToolError("Equipment, order type, severity and planned start are required.")
    end = to_backend_time(v.get("planned_end"), "planned end")
    techs = [str(t) for t in v.get("technicians") or [] if str(t).strip()]
    issues = [int(i) for i in v.get("instructions") or [] if str(i).strip().isdigit()]
    repeat = lookup(REPEAT, v.get("repeat_type_id"), "repeat") if type_id == ORDER_TYPE["PREVENTIVE"] else None
    body: dict[str, Any] = {
        "submit_type": "CREATED",
        "asset_id": int(asset["id"]),
        "maintenance_type_id": type_id,
        "severity_id": severity,
        "priority_id": lookup(PRIORITY, v.get("priority_id"), "priority"),
        "planned_start": start,
        "planned_end": end,
        "description": str(v.get("description") or "").strip()[:2000],
        "repeat_type_id": repeat,
        "repeat_end": to_backend_time(v.get("repeat_end"), "repeat end") if repeat else None,
    }
    if techs:
        body["technicians"] = ",".join(techs)
    if issues:
        body["instructions"] = ",".join(str(i) for i in issues)
    res = await api.call("maintenance_order/submit", body, what="the new maintenance order") or {}
    ids = [int(r["maintenance_id"]) for r in res.get("result") or [] if r.get("maintenance_id")]
    if not ids:
        raise ToolError(str(res.get("message") or "The maintenance order was not created."))
    action = "created"
    if v.get("approve"):
        await submit_update(api, ids[0], {}, "APPROVED")
        action = "created and approved"
    card = await order_card(api, ids[0], action, {"occurrences": len(ids)} if len(ids) > 1 else None)
    series = f" (a series of {len(ids)} orders)" if len(ids) > 1 else ""
    return {"message": f"Maintenance order {card['name']} {action}{series}.", "maintenance_order": card}


async def submit_update(api: Api, order_id: int, changes: Mapping[str, Any], submit_type: str = "SAVED") -> None:
    """maintenance_order/submit SAVED / APPROVED with the order's current values plus `changes`."""
    d = await order_detail(api, order_id)
    body: dict[str, Any] = {
        "submit_type": submit_type,
        "id": order_id,
        "asset_id": int(d["equipment"]),
        "maintenance_type_id": int(d["maintenance_type_id"]),
        "severity_id": int(d["severity"]),
        "priority_id": int(d["priority"]) if str(d.get("priority") or "").isdigit() else None,
        "planned_start": d.get("planned_start"),
        "planned_end": d.get("planned_end"),
        "description": d.get("description") or "",
        "technicians": d.get("technicians") or "",
        "is_recurring": False,
    }
    for key in ("asset_id", "maintenance_type_id", "severity_id", "priority_id"):
        if changes.get(key) not in (None, ""):
            table = {"maintenance_type_id": ORDER_TYPE, "severity_id": SEVERITY, "priority_id": PRIORITY}.get(key)
            body[key] = lookup(table, changes[key], key.replace("_id", "").replace("_", " ")) if table else int(changes[key])
    for key in ("planned_start", "planned_end"):
        if changes.get(key):
            body[key] = to_backend_time(changes[key], key.replace("_", " "))
    if changes.get("description") is not None and str(changes.get("description")).strip():
        body["description"] = str(changes["description"]).strip()[:2000]
    if changes.get("technicians") is not None:
        body["technicians"] = ",".join(str(t) for t in changes["technicians"] if str(t).strip())
    if changes.get("instructions") is not None:
        body["instructions"] = ",".join(str(int(i)) for i in changes["instructions"] if str(i).strip().isdigit())
    await api.call("maintenance_order/submit", body, what=f"maintenance order {order_id}")


async def act(api: Api, order_id: int, action: str, v: Mapping[str, Any]) -> dict[str, Any]:
    """A workflow action on an existing order; returns the message and the order card."""
    action = action.strip().lower()
    if action not in ACTIONS + ("edit",):
        raise ToolError(f"Unknown action '{action}'. Use {', '.join(ACTIONS)}.")
    done = {
        "edit": "updated",
        "approve": "approved",
        "reject": "rejected",
        "start": "started",
        "pause": "paused",
        "resume": "resumed",
        "complete": "completed",
        "close": "closed",
        "add_note": "work note added",
        "delete": "deleted",
    }[action]
    if action == "edit":
        await submit_update(api, order_id, v)
    elif action == "approve":
        await submit_update(api, order_id, {k: v[k] for k in ("technicians", "planned_start", "planned_end") if v.get(k)}, "APPROVED")
    elif action == "reject":
        reason = str(v.get("reason") or "").strip()
        if not reason:
            raise ToolError("A reason is required to reject an order.")
        await api.call("maintenance_order/submit", {"submit_type": "REJECTED", "id": order_id, "reject_reason": reason[:1000]}, what=f"order {order_id}")
    elif action == "start":
        await api.call(WORK + "start_work_report", {"maintenance_id": order_id, "is_status_change": bool(v.get("equipment_down"))}, what=f"order {order_id}")
    elif action == "pause":
        reason = str(v.get("reason") or "").strip()
        if not reason:
            raise ToolError("A reason is required to pause the work.")
        await api.call(WORK + "pause_work_report", {"maintenance_id": order_id, "reason": reason[:1000]}, what=f"order {order_id}")
    elif action == "resume":
        await api.call(WORK + "resume_work_report", {"maintenance_id": order_id}, what=f"order {order_id}")
    elif action in ("complete", "close"):
        issues = await validities(api, order_id, v)
        script = "set_work_report_status_to_done" if action == "complete" else "close_work_report"
        await api.call(WORK + script, {"maintenance_id": order_id, "issues": json.dumps(issues)}, what=f"order {order_id}")
        if str(v.get("note") or "").strip():
            await api.call(WORK + "add_work_update", {"maintenance_id": order_id, "note": str(v["note"]).strip()[:1000], "image_urls": [], "issue_ids": []}, what=f"order {order_id}")
    elif action == "add_note":
        note = str(v.get("note") or "").strip()
        if not note:
            raise ToolError("The work note is empty.")
        d = await order_detail(api, order_id)
        await api.call(WORK + "add_work_update", {"maintenance_id": order_id, "note": note[:1000], "image_urls": [], "issue_ids": _ids(d.get("instructions"))}, what=f"order {order_id}")
    elif action == "delete":
        head = await api.call("maintenance_order/get_maintenance_name_and_type", {"maintenance_id": order_id}, what="the order name") or {}
        await api.call("maintenance_order/delete_maintenance_order", {"id": order_id, "is_recurring": bool(v.get("is_recurring"))}, what=f"order {order_id}")
        name = head.get("name") or f"#{order_id}"
        return {"message": f"Maintenance order {name} deleted.", "maintenance_order": {"id": order_id, "name": name, "action": "deleted", "status": "DELETED"}}
    card = await order_card(api, order_id, done)
    return {"message": f"Maintenance order {card['name']} {done}.", "maintenance_order": card}


async def validities(api: Api, order_id: int, v: Mapping[str, Any]) -> list[dict[str, Any]]:
    """Diagnosis / repair verdicts of the order's issues: `valid_<id>` fields of a form, else `all_valid`."""
    d = await order_detail(api, order_id)
    out = []
    for code_id in _ids(d.get("instructions")):
        own = v.get(f"valid_{code_id}")
        valid = bool(own) if own is not None else v.get("all_valid", True) is not False
        out.append({"alarm_code_id": code_id, "instruction_type_id": DIAGNOSTIC, "is_valid": valid})
        out.append({"alarm_code_id": code_id, "instruction_type_id": REPAIR, "is_valid": valid})
    return out


# ---------------------------------------------------------------------------
# Forms
# ---------------------------------------------------------------------------


def _common_fields(values: Mapping[str, Any], equipment: list[dict[str, Any]] | None, *, preventive_extras: bool) -> list[dict[str, Any]]:
    fields = []
    if equipment is not None:
        fields.append(forms.field("asset_id", "Equipment", "select", required=True, options=equipment, value=values.get("asset_id")))
    fields += [
        forms.field("maintenance_type_id", "Order type", "select", required=True, options=forms.options(ORDER_TYPE), value=values.get("maintenance_type_id")),
        forms.field("severity_id", "Severity", "select", required=True, options=forms.options(SEVERITY), value=values.get("severity_id")),
        forms.field("priority_id", "Priority", "select", options=forms.options(PRIORITY), value=values.get("priority_id")),
        forms.field("planned_start", "Planned start", "datetime", required=True, value=values.get("planned_start"), help="Plant time"),
        forms.field("planned_end", "Planned end", "datetime", value=values.get("planned_end")),
    ]
    if preventive_extras:
        fields += [
            forms.field(
                "repeat_type_id", "Repeat", "select", options=forms.options(REPEAT), value=values.get("repeat_type_id"),
                visible_if={"field": "maintenance_type_id", "in": [ORDER_TYPE["PREVENTIVE"]]},
            ),
            forms.field(
                "repeat_end", "Repeat until", "datetime", value=values.get("repeat_end"),
                visible_if={"field": "repeat_type_id", "in": list(REPEAT.values())},
            ),
        ]
    fields += [
        forms.field("instructions", "Issues", "multiselect", options_source="issues", depends_on="asset_id", value=values.get("instructions") or [],
                    help="Issue codes with their diagnostic and repair SOPs"),
        forms.field("technicians", "Technicians", "multiselect", options_source="technicians", depends_on="asset_id", value=values.get("technicians") or []),
        forms.field("description", "Description", "textarea", value=values.get("description")),
    ]
    return fields


async def create_form(api: Api, values: Mapping[str, Any]) -> dict[str, Any]:
    v = dict(values)
    approve = bool(v.pop("approve", False))
    corrective = v.get("maintenance_type_id") != ORDER_TYPE["PREVENTIVE"]
    fields = _common_fields(v, await equipment_options(api), preventive_extras=True)
    if api.can("maintenanceorder.approve"):
        fields.append(forms.field("approve", "Approve right away (needs a technician)", "checkbox", value=approve))
    title = "New maintenance order" if v.get("maintenance_type_id") is None else f"New {'corrective' if corrective else 'preventive'} maintenance order"
    return forms.new_form("maintenance_order.create", title, fields, description="Fill in what is missing, then create the order.", submit_label="Create order")


async def edit_form(api: Api, order_id: int, changes: Mapping[str, Any], *, kind: str, title: str, submit_label: str) -> dict[str, Any]:
    """The order's current values (with any changes the user named) in an editable form."""
    d = await order_detail(api, order_id)
    head = await api.call("maintenance_order/get_maintenance_name_and_type", {"maintenance_id": order_id}, what="the order name") or {}
    current = {
        "asset_id": int(d["equipment"]),
        "maintenance_type_id": int(d["maintenance_type_id"]),
        "severity_id": int(d["severity"]),
        "priority_id": int(d["priority"]) if str(d.get("priority") or "").isdigit() else None,
        "planned_start": form_time(d.get("planned_start")),
        "planned_end": form_time(d.get("planned_end")),
        "description": d.get("description") or "",
        "technicians": _names(d.get("technicians")),
        "instructions": _ids(d.get("instructions")),
    }
    current.update({k: v for k, v in changes.items() if v not in (None, "", [])})
    name = head.get("name") or f"#{order_id}"
    if kind == "maintenance_order.approve":
        fields = [
            forms.field("order", "Order", "info", value=f"{name} · {d.get('status')}"),
            forms.field("technicians", "Technicians", "multiselect", required=True, options_source="technicians", depends_on="asset_id",
                        value=current["technicians"]),
            forms.field("planned_start", "Planned start", "datetime", required=True, value=current["planned_start"]),
            forms.field("planned_end", "Planned end", "datetime", value=current["planned_end"]),
        ]
        return forms.new_form(kind, f"{title} {name}", fields, submit_label=submit_label,
                              context={"order_id": order_id, "asset_id": current["asset_id"], "maintenance_id": order_id})
    fields = [forms.field("order", "Order", "info", value=f"{name} · {d.get('status')}")] + _common_fields(current, None, preventive_extras=False)
    return forms.new_form(kind, f"{title} {name}", fields, submit_label=submit_label,
                          context={"order_id": order_id, "asset_id": current["asset_id"], "maintenance_id": order_id})


async def action_form(api: Api, order_id: int, action: str, values: Mapping[str, Any]) -> dict[str, Any]:
    """The form of a workflow action that needs the user (a reason, verdicts, a confirmation)."""
    d = await order_detail(api, order_id)
    head = await api.call("maintenance_order/get_maintenance_name_and_type", {"maintenance_id": order_id}, what="the order name") or {}
    name = head.get("name") or f"#{order_id}"
    ctx = {"order_id": order_id, "action": action, "asset_id": int(d["equipment"]), "maintenance_id": order_id}
    info = forms.field("order", "Order", "info", value=f"{name} · {d.get('status')}")
    if action in ("reject", "pause"):
        label = "Reason for rejecting" if action == "reject" else "Reason for pausing"
        return forms.new_form(f"maintenance_order.{action}", f"{action.title()} {name}",
                              [info, forms.field("reason", label, "textarea", required=True, value=values.get("reason"))],
                              submit_label=action.title(), danger=action == "reject", context=ctx)
    if action == "add_note":
        return forms.new_form("maintenance_order.add_note", f"Work note on {name}",
                              [info, forms.field("note", "Note", "textarea", required=True, value=values.get("note"))],
                              submit_label="Add note", context=ctx)
    if action in ("complete", "close"):
        labels = {i["value"]: i["label"] for i in await issue_options(api, int(d["equipment"]))}
        fields = [info]
        for code_id in _ids(d.get("instructions")):
            fields.append(forms.field(f"valid_{code_id}", f"{labels.get(code_id, f'Issue {code_id}')}: diagnosis confirmed and repaired", "checkbox", value=True))
        fields.append(forms.field("note", "Closing note", "textarea", value=values.get("note")))
        verb = "Complete" if action == "complete" else "Close"
        return forms.new_form(f"maintenance_order.{action}", f"{verb} {name}", fields, submit_label=verb, context=ctx)
    if action == "delete":
        return forms.new_form(
            "maintenance_order.delete", f"Delete {name}?",
            [info, forms.field("warning", "", "info", value="The order and its history are deleted. This cannot be undone.")],
            submit_label="Delete order", danger=True, context=ctx,
        )
    raise ToolError(f"The action '{action}' needs no form.")


# ---------------------------------------------------------------------------
# Submitted forms
# ---------------------------------------------------------------------------


@forms.handler("maintenance_order.create")
async def _submit_create(api: Api, v: dict[str, Any]) -> dict[str, Any]:
    return await create(api, v)


@forms.handler("maintenance_order.edit")
async def _submit_edit(api: Api, v: dict[str, Any]) -> dict[str, Any]:
    return await act(api, int(v["order_id"]), "edit", v)


@forms.handler("maintenance_order.approve")
async def _submit_approve(api: Api, v: dict[str, Any]) -> dict[str, Any]:
    if not v.get("technicians"):
        raise ToolError("Choose at least one technician to approve the order.")
    return await act(api, int(v["order_id"]), "approve", v)


def _action_handler(action: str) -> None:
    async def run(api: Api, v: dict[str, Any]) -> dict[str, Any]:
        return await act(api, int(v["order_id"]), action, v)

    forms.handler(f"maintenance_order.{action}")(run)


for _a in ("reject", "pause", "complete", "close", "add_note", "delete"):
    _action_handler(_a)
