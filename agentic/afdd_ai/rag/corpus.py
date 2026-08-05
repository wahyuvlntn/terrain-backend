"""Generated RAG corpus: PDFs built from the plant records (DESIGN 6.2).

Document kinds (all `source = 'generated'`, A4, fpdf2 + DejaVu Sans):

* ``manual`` - a technical manual per equipment model in the site: overview,
  design data, equipment list, instrument list, set points and ranges, alarm
  limits, alarm codes, diagnostics (issue relations as fault trees, alarm
  mappings), SOPs (Diagnostic / Repair / Severity instructions), PM schedule,
  LBNL point names;
* ``fdd_reference`` - the LBNL fault types, the translated detection rules
  (signals, thresholds in use, calibration) and the point map;
* ``maintenance_report`` - one per DONE / CLOSED maintenance order: header,
  people, timeline, problem, evidence (alarms before detection, parameter
  values before / after the work, downtime), work notes, instructions
  performed, KPIs; doc_date = completion date;
* ``setpoint_log`` - one per month: operator set-point changes
  (asset.control_requests) and applied optimizer recommendations.

Idempotency: every document has a unique `metadata.generator_key`; the
source data is hashed (`metadata.source_hash`) and a document is re-rendered,
re-uploaded and re-queued for indexing (PENDING + NOTIFY rag_documents) only
when its source changed. Generated documents whose source disappeared (orders
wiped by a history regeneration, removed models) are deleted with their object.

Run once on start and daily from the analytics worker, or
`python -m afdd_ai.analytics.worker --once corpus`.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
import re
from collections import defaultdict
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta, timezone
from typing import Any
from zoneinfo import ZoneInfo

from psycopg.types.json import Jsonb

from ..analytics import rules as fdd_rules
from ..common import db, storage
from ..common.config import get_settings
from .pdf import DocPDF

log = logging.getLogger(__name__)

CORPUS_VERSION = 1  # bump to re-render every generated document (layout changes)
KINDS = ("manual", "fdd_reference", "maintenance_report", "setpoint_log")
STATUS_DONE, STATUS_CLOSED = 5, 6

# Design data of the modelled equipment (simulator plant definition; not stored in the DB).
DESIGN_DATA: dict[str, list[tuple[str, str]]] = {
    "Centrifugal Chiller": [
        ("Cooling capacity", "1,000 RT (3,517 kW)"),
        ("Refrigerant", "R-134a"),
        ("Chilled water design temperatures", "6.5 °C supply / 12 °C return"),
        ("Evaporator design flow", "550 m³/h (follows the dedicated CHW pump speed)"),
        ("Condenser design flow", "680 m³/h, range about 4 K at full load"),
        ("Compressor", "Single-stage centrifugal with inlet guide vanes and VFD"),
        ("Clean-tube approaches", "Evaporator 0.9 K, condenser 1.2 K"),
        ("Leaving-water control", "Unit controller ramps its active set point to the operator set point at 0.6 K/min"),
        ("Heat balance", "Cooling load = load % / 100 x 3,517 kW = CHW flow x 1.1628 x (return - supply) kW"),
    ],
    "Cooling Tower": [
        ("Type", "BAC Series 3000 S3E-1424 induced-draft crossflow cell"),
        ("Heat rejection", "1,250 RT per cell"),
        ("Cell size", "4.27 m x 7.32 m, air inlets on both long faces"),
        ("Fan", "8-blade 3.66 m axial fan, gear reducer, VFD 30-100 %"),
        ("Fan power", "about 30 kW at 100 % speed, proportional to speed cubed"),
        ("Design wet bulb", "25.5 °C (Singapore)"),
        ("Leaving water set point", "max(wet bulb + approach set point, minimum condenser water temperature)"),
    ],
    "Chilled Water Pump": [
        ("Rating", "550 m³/h at 3.1 bar, 58 kW shaft power"),
        ("Design speed", "46 Hz (VFD), normal suction 2.6 bar"),
        ("Duty", "Variable primary: dedicated to one chiller, speed from the common riser DP controller"),
    ],
    "Condenser Water Pump": [
        ("Rating", "640 m³/h at 2.4 bar, 54 kW shaft power"),
        ("Design speed", "47 Hz (VFD), normal suction 1.6 bar"),
        ("Duty", "Dedicated to one chiller / tower pair"),
    ],
    "Process Cooling Water Pump": [
        ("Rating", "318 m³/h at 4.2 bar, 52 kW shaft power"),
        ("Design speed", "48 Hz (VFD), normal suction 2.05 bar"),
        ("Duty", "3 of 4 pumps running"),
    ],
    "Chilled Water System": [
        ("Plant", "3 x 1,000 RT centrifugal chillers, N+1 with weekly duty rotation"),
        ("Pumping", "Variable primary: CHW pump speed from a PI loop holding the measured riser DP at its set point (default 120 kPa)"),
        ("CHW supply set point", "Reset mode: 7.0 °C at 26 °C outdoor air down to 6.0 °C at 31 °C, recomputed hourly; Manual mode: operator value (5.5-8.0 °C)"),
        ("Staging", "Load mode: stage up above 95 % of n x 3,517 kW for 10 min, down below 80 % of (n-1) x 3,517 kW for 30 min; Manual: duty chillers set point"),
        ("Tower control", "Common fan speed PI on the average running tower leaving temperature against max(wet bulb + approach set point, minimum CW temperature)"),
        ("Condenser 3-way valve", "Tower bypass valve holding the CW supply at or above its minimum temperature; normally at its minimum position"),
        ("Minimum-flow bypass", "Opens when a chiller's evaporator flow falls under 60 % of design"),
    ],
    "Make-up Air Unit": [
        ("Airflow", "60,000 m³/h"),
        ("Supply air set point", "17 °C, cleanroom humidity limit 55 % RH"),
        ("Cooling coil", "Chilled water; valve demand rises with CHW supply temperature and falls with riser DP"),
    ],
}


@dataclass
class DocSpec:
    key: str
    doc_type: str
    title: str
    object_key: str
    site_code: str
    source: dict[str, Any]
    render: Callable[[dict[str, Any]], bytes]
    doc_date: date | None = None
    location_id: int | None = None
    asset_id: int | None = None
    unit: str | None = None
    asset_type: str | None = None
    model_id: int | None = None
    alarm_codes: list[str] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=dict)
    # source keys rendered but not hashed: a snapshot at first generation that must not
    # re-render (and re-embed) the document every time it changes (e.g. the unit's KPIs)
    volatile: tuple[str, ...] = ()

    @property
    def file_name(self) -> str:
        return self.object_key.rsplit("/", 1)[-1]

    def source_hash(self) -> str:
        stable = {k: v for k, v in self.source.items() if k not in self.volatile}
        blob = json.dumps({"v": CORPUS_VERSION, "title": self.title, "source": stable}, sort_keys=True, default=_json_default, ensure_ascii=False)
        return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def _json_default(v: Any) -> Any:
    if isinstance(v, (datetime, date)):
        return v.isoformat()
    if hasattr(v, "obj"):  # psycopg Jsonb
        return v.obj
    return str(v)


def slug(text: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")[:80] or "doc"


def tz() -> ZoneInfo:
    return ZoneInfo(get_settings().timezone)


def local(ts: datetime | None) -> str:
    return ts.astimezone(tz()).strftime("%Y-%m-%d %H:%M") if ts else ""


def fnum(v: Any, scale: int | None = None) -> str:
    if v is None:
        return ""
    try:
        f = float(v)
    except (TypeError, ValueError):
        return str(v)
    if scale is not None:
        return f"{f:.{max(scale, 0)}f}"
    return f"{f:g}"


def common_building(rows: Iterable[dict[str, Any]]) -> int | None:
    ids = {r.get("building_id") for r in rows}
    return ids.pop() if len(ids) == 1 else None


async def location_parents() -> dict[int, int | None]:
    rows = await db.fetch_all("SELECT id, parent_id FROM asset.locations")
    return {int(r["id"]): (int(r["parent_id"]) if r["parent_id"] is not None else None) for r in rows}


def common_location(rows: Iterable[dict[str, Any]], parents: dict[int, int | None]) -> int | None:
    """The deepest location containing every row's `location_id` (None: no common location).

    A document is filed there: the application shows it to users whose scope covers that location
    (assigned locations and their descendants), so filing it as low as possible lets everyone who can
    see the equipment it covers read it (a manual filed at the building would be hidden from a user
    scoped to the plant room).
    """
    paths: list[list[int]] = []
    for r in rows:
        loc = r.get("location_id")
        if loc is None:
            return None
        path: list[int] = []
        while loc is not None and loc not in path:
            path.append(int(loc))
            loc = parents.get(int(loc))
        paths.append(path[::-1])  # root first
    if not paths:
        return None
    common = None
    for level in zip(*paths):
        if any(x != level[0] for x in level):
            break
        common = level[0]
    return common


# ---------------------------------------------------------------------------
# Technical manuals


def render_flow(flow: dict[str, Any]) -> tuple[list[str], list[str]]:
    """Issue relation flow -> (target texts, condition paths in words)."""
    nodes = {n["id"]: n for n in (flow or {}).get("nodes", [])}
    edges = (flow or {}).get("edges", [])

    def text(nid: str) -> str:
        n = nodes.get(nid) or {}
        d = n.get("data") or {}
        if n.get("type") == "source":
            return f"alarm {d.get('code', '')} {d.get('name', '')} is active".strip()
        if n.get("type") == "parameter":
            return f"{d.get('name', 'a parameter')} is abnormal"
        if n.get("type") == "compound":
            parts = [text(e["source"]) for e in edges if e["target"] == nid]
            return " AND ".join(parts) if parts else "compound condition"
        return d.get("name", nid)

    targets, paths = [], []
    for nid, n in nodes.items():
        if n.get("type") != "target":
            continue
        d = n.get("data") or {}
        targets.append(f"{d.get('code', '')} {d.get('name', '')}".strip())
        paths += [text(e["source"]) for e in edges if e["target"] == nid]
    return targets, paths


async def collect_manuals(site: str) -> list[DocSpec]:
    models = await db.fetch_all(
        """SELECT m.id, m.name, m.description, ty.name AS type_name, b.name AS brand, s.name AS series, bm.name AS base_name,
                  m.base_model_id, m.running_hour_warning, m.running_hour_critical
           FROM asset.models m JOIN asset.types ty ON ty.id = m.type_id JOIN asset.brands b ON b.id = m.brand_id
           JOIN asset.series s ON s.id = m.series_id LEFT JOIN asset.models bm ON bm.id = m.base_model_id
           WHERE m.id IN (SELECT model_id FROM analytics.v_assets WHERE site_code = %s) ORDER BY m.id""",
        (site,),
    )
    point_map = await db.fetch_all("SELECT lbnl_name, lbnl_unit, asset_pattern, param_alias, si_unit, description FROM analytics.point_map ORDER BY lbnl_name")
    parents = await location_parents()
    out = []
    for m in models:
        ids = [m["id"]] + ([m["base_model_id"]] if m["base_model_id"] else [])
        equipment = await db.fetch_all(
            """SELECT v.id, v.alias, v.name, v.location_path, v.location_id, v.building_id, e.installed_at, e.aspect_critical, e.aspect_supporting
               FROM analytics.v_assets v JOIN asset.equipment e ON e.id = v.id WHERE v.model_id = %s AND v.site_code = %s ORDER BY v.alias""",
            (m["id"], site),
        )
        params = await db.fetch_all(
            """SELECT alias, name, unit, scale, data_type, is_writable, control_min, control_max, control_step, control_options,
                      control_mode, control_target FROM asset.model_parameters WHERE model_id = %s ORDER BY id""",
            (m["id"],),
        )
        limits = await db.fetch_all(
            """SELECT mp.alias, mp.name, mp.unit, e.alias AS unit_alias, ep.warning_lower_limit AS wl, ep.warning_upper_limit AS wu,
                      ep.critical_lower_limit AS cl, ep.critical_upper_limit AS cu
               FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
               JOIN asset.equipment e ON e.id = ep.equipment_id
               WHERE ep.equipment_id = ANY(%s) AND (ep.warning_lower_limit IS NOT NULL OR ep.warning_upper_limit IS NOT NULL
                     OR ep.critical_lower_limit IS NOT NULL OR ep.critical_upper_limit IS NOT NULL)
               ORDER BY mp.id, e.alias""",
            ([e["id"] for e in equipment],),
        )
        alarms = await db.fetch_all(
            """SELECT DISTINCT ON (lower(ma.name)) ma.name AS code, COALESCE(ac.name, ma.name) AS code_name, ma.criticality, ma.message,
                      fi.issue_code, fi.name AS issue_name, fc.name AS category, ma.is_disabled
               FROM asset.model_alarms ma LEFT JOIN dm.alarm_codes ac ON lower(ac.code) = lower(ma.name)
               LEFT JOIN asset.feedback_issues fi ON fi.id = ma.issue_id LEFT JOIN asset.feedback_categories fc ON fc.id = ma.issue_category_id
               WHERE ma.model_id = ANY(%s) ORDER BY lower(ma.name), (ma.model_id = %s) DESC""",
            (ids, m["id"]),
        )
        mappings = await db.fetch_all(
            """SELECT ac.code, ac.name, string_agg(DISTINCT mp.name, ', ') AS params
               FROM dm.alarm_mappings am JOIN dm.alarm_codes ac ON ac.id = am.alarm_code_id
               JOIN asset.model_parameters mp ON mp.id = am.model_parameter_id
               WHERE am.model_id = ANY(%s) GROUP BY 1, 2 ORDER BY 1""",
            (ids,),
        )
        relations = await db.fetch_all("SELECT name, flow FROM dm.issue_relations WHERE model_id = ANY(%s) ORDER BY name", (ids,))
        instructions = await db.fetch_all(
            """SELECT DISTINCT ON (i.id) i.id, ac.code, ac.name AS code_name, i.title, it.name AS type, i.instruction_type_id, i.duration,
                      i.description, i.content
               FROM dm.instructions i JOIN dm.instruction_models im ON im.instruction_id = i.id
               JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id JOIN dm.instruction_types it ON it.id = i.instruction_type_id
               WHERE im.model_id = ANY(%s) AND NOT i.is_deleted ORDER BY i.id""",
            (ids,),
        )
        instructions.sort(key=lambda r: (r["code"], r["instruction_type_id"], r["title"]))
        pm = await db.fetch_all(
            """SELECT ac.code, ac.name, rt.name AS repeat, max(ms.duration_sec) AS duration_sec, count(DISTINCT mo.asset_id) AS units
               FROM dm.maintenance_orders mo JOIN dm.maintenance_schedules ms ON ms.id = mo.schedule_id
               JOIN dm.repeat_types rt ON rt.id = ms.repeat_type_id
               JOIN dm.maintenance_order_issues i ON i.order_id = mo.id JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id
               WHERE mo.asset_id = ANY(%s) GROUP BY 1, 2, 3 ORDER BY 1""",
            ([e["id"] for e in equipment],),
        )
        aliases = [e["alias"] for e in equipment]
        pm_rows = [
            r for r in point_map
            if any(re.fullmatch(re.escape(r["asset_pattern"]).replace(re.escape("{n}"), r"\d+"), a) for a in aliases)
        ]  # fmt: skip
        fdd = [
            {"code": r.code, "name": r.name}
            for r in fdd_rules.RULES
            if {"chiller": "Centrifugal Chiller", "tower": "Cooling Tower", "plant": "Chilled Water System"}[r.asset_scope] == m["type_name"]
        ]
        codes = sorted(
            {a["code"] for a in alarms}
            | {i["code"] for i in instructions}
            | {p["code"] for p in pm}
            | {c.split()[0] for r in relations for c in render_flow(r["flow"])[0] if c}
            | {f["code"] for f in fdd}
        )
        source = {
            "model": m,
            "equipment": equipment,
            "params": params,
            "limits": limits,
            "alarms": alarms,
            "mappings": mappings,
            "relations": relations,
            "instructions": instructions,
            "pm": pm,
            "point_map": pm_rows,
            "fdd": fdd,
            "site": site,
        }
        out.append(
            DocSpec(
                key=f"manual:{site}:{slug(m['name'])}",
                doc_type="manual",
                title=f"{m['name']} - Technical manual",
                object_key=f"generated/manual/{site.lower()}/{slug(m['name'])}.pdf",
                site_code=site,
                source=source,
                render=render_manual,
                doc_date=None,
                location_id=common_location(equipment, parents),
                asset_type=m["type_name"],
                model_id=m["id"],
                alarm_codes=codes,
                meta={"model_name": m["name"]},
            )
        )
    return out


def _range(p: dict[str, Any]) -> str:
    if p.get("control_options"):
        return " / ".join(f"{o['value']} = {o['label']}" for o in p["control_options"])
    if p.get("control_min") is not None:
        return f"{fnum(p['control_min'])} to {fnum(p['control_max'])} {p['unit'] or ''}".strip()
    return ""


def _limit(lo: Any, hi: Any) -> str:
    parts = []
    if hi is not None:
        parts.append(f"> {fnum(hi)}")
    if lo is not None:
        parts.append(f"< {fnum(lo)}")
    return " or ".join(parts)


def render_manual(src: dict[str, Any]) -> bytes:
    m = src["model"]
    gen = datetime.now(timezone.utc)
    pdf = DocPDF(
        f"{m['name']} - Technical manual",
        f"{m['type_name']} | {m['brand']} {m['series']} | Site {src['site']} | {len(src['equipment'])} unit(s) installed",
        site=src["site"],
        generated_at=gen,
    )
    pdf.h1("Overview")
    pdf.para(m["description"])
    pdf.kv(
        [
            ("Model", m["name"]),
            ("Equipment type", m["type_name"]),
            ("Brand / series", f"{m['brand']} / {m['series']}"),
            ("Base model", m.get("base_name")),
            ("Running hours limits", f"warning {m['running_hour_warning']} h, critical {m['running_hour_critical']} h" if m["running_hour_warning"] else None),
        ]
    )
    design = DESIGN_DATA.get(m["type_name"])
    if design:
        pdf.h2("Design data")
        pdf.kv(design)
    pdf.h2("Equipment list")
    pdf.data_table(
        ["Unit", "Name", "Location", "Installed"],
        [[e["alias"], e["name"], e["location_path"], local(e["installed_at"])[:10]] for e in src["equipment"]],
        widths=(0.13, 0.27, 0.45, 0.15),
    )
    aspects = [(e["alias"], a) for e in src["equipment"] for a in (e["aspect_critical"] or [])]
    if aspects:
        pdf.h3("Critical aspects")
        pdf.bullets([f"{alias}: {a.get('aspect')} - {a.get('information')}" for alias, a in aspects])
    sup = [(e["alias"], a) for e in src["equipment"] for a in (e["aspect_supporting"] or [])]
    if sup:
        pdf.h3("Supporting aspects")
        pdf.bullets([f"{alias}: {a.get('aspect')} - {a.get('information')}" for alias, a in sup])

    pdf.h1("Instrument list")
    kinds = {}
    for p in src["params"]:
        if p["alias"] in ("running", "trip", "maint_mode"):
            kinds[p["alias"]] = "status"
        elif p["is_writable"]:
            kinds[p["alias"]] = "mode" if p.get("control_options") else "set point"
        else:
            kinds[p["alias"]] = "measured"
    pdf.data_table(
        ["Point", "Name", "Unit", "Decimals", "Kind"],
        [[p["alias"], p["name"], p["unit"], "" if p["scale"] is None else p["scale"], kinds[p["alias"]]] for p in src["params"]],
        widths=(0.2, 0.42, 0.12, 0.1, 0.16),
    )

    pdf.h1("Set points and operating modes")
    writable = [p for p in src["params"] if p["is_writable"]]
    if writable:
        pdf.para("Operator writes go through the digital twin control panel (two-way control) and are audited. A set point gated by a mode can only be written while that mode is out of automatic.")
        pdf.data_table(
            ["Point", "Name", "Range / options", "Step", "Gated by", "Controls"],
            [[p["alias"], p["name"], _range(p), fnum(p["control_step"]), p.get("control_mode") or "", p.get("control_target") or ""] for p in writable],
            widths=(0.16, 0.24, 0.26, 0.08, 0.13, 0.13),
        )
    else:
        pdf.para("This model has no operator set points.")

    pdf.h1("Alarm limits")
    grouped: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for r in src["limits"]:
        grouped[r["alias"]].append(r)
    rows = []
    for _alias, rs in grouped.items():
        sigs = {(r["wl"], r["wu"], r["cl"], r["cu"]) for r in rs}
        targets = [("all units", rs[0])] if len(sigs) == 1 else [(r["unit_alias"], r) for r in rs]
        for who, r in targets:
            rows.append([r["name"], r["unit"], _limit(r["wl"], r["wu"]), _limit(r["cl"], r["cu"]), who])
    if rows:
        pdf.data_table(["Parameter", "Unit", "Warning", "Critical", "Applies to"], rows, widths=(0.34, 0.1, 0.18, 0.18, 0.2))
    else:
        pdf.para("No parameter limits are configured for this model.")

    pdf.h1("Alarm codes")
    if src["alarms"]:
        pdf.data_table(
            ["Code", "Name", "Severity", "Condition", "Issue"],
            [
                [a["code"], a["code_name"], "Critical" if a["criticality"] == 2 else "Warning", a["message"], f"{a['issue_code'] or ''} {a['issue_name'] or ''}".strip()]
                for a in src["alarms"]
                if not a["is_disabled"]
            ],
            widths=(0.11, 0.22, 0.1, 0.39, 0.18),
        )
    if src["fdd"]:
        pdf.h2("FDD rule codes (raised by the analytics service)")
        pdf.data_table(["Code", "Fault"], [[f["code"], f["name"]] for f in src["fdd"]], widths=(0.2, 0.8))

    pdf.h1("Diagnostics")
    if src["relations"]:
        for r in src["relations"]:
            targets, paths = render_flow(r["flow"])
            pdf.h2(r["name"])
            pdf.para(f"Diagnosis: {', '.join(targets)}. Raised when any of the following holds:")
            pdf.bullets(paths, numbered=True)
    if src["mappings"]:
        pdf.h2("Alarm code to variable mapping")
        pdf.data_table(["Code", "Name", "Related variables"], [[r["code"], r["name"], r["params"]] for r in src["mappings"]], widths=(0.12, 0.33, 0.55))
    if not src["relations"] and not src["mappings"]:
        pdf.para("No diagnostic relations are configured for this model.")

    pdf.h1("Maintenance procedures")
    if src["instructions"]:
        for i in src["instructions"]:
            pdf.h2(f"{i['code']} {i['code_name']} - {i['title']}")
            pdf.para(f"Type: {i['type']}. Estimated duration: {i['duration']} min. {i['description'] or ''}".strip())
            pdf.markdown(i["content"] or "")
    else:
        pdf.para("No procedures are registered for this model yet.")

    pdf.h1("Preventive maintenance")
    if src["pm"]:
        pdf.data_table(
            ["Code", "Task", "Interval", "Planned duration", "Units"],
            [[p["code"], p["name"], (p["repeat"] or "").title(), f"{(p['duration_sec'] or 0) / 3600:g} h", p["units"]] for p in src["pm"]],
            widths=(0.12, 0.43, 0.15, 0.18, 0.12),
        )
    else:
        pdf.para("No preventive maintenance schedule is registered for this model.")

    if src["point_map"]:
        pdf.h1("LBNL point names")
        pdf.para("Point names of the LBNL chiller plant fault detection data set that map onto this equipment's tags ({n} = unit number).")
        pdf.data_table(
            ["LBNL point", "Asset", "Parameter", "Unit (LBNL / SI)", "Description"],
            [[r["lbnl_name"], r["asset_pattern"], r["param_alias"], f"{r['lbnl_unit']} / {r['si_unit']}", r["description"]] for r in src["point_map"]],
            widths=(0.22, 0.12, 0.17, 0.15, 0.34),
        )
    return pdf.render()


# ---------------------------------------------------------------------------
# FDD reference


async def collect_fdd_reference(site: str) -> list[DocSpec]:
    point_map = await db.fetch_all("SELECT lbnl_name, lbnl_unit, asset_pattern, param_alias, si_unit, description FROM analytics.point_map ORDER BY lbnl_name")
    stored = {
        r["code"]: r
        for r in await db.fetch_all("SELECT code, params, enabled, severity, calibrated_at, calibration -> 'job_id' AS job_id FROM analytics.fault_rules")
    }
    names = fdd_rules.lbnl_names(point_map)
    rules_src = []
    for r in fdd_rules.RULES:
        s = stored.get(r.code) or {}
        rules_src.append(
            {
                "code": r.code,
                "name": r.name,
                "lbnl_fault": r.lbnl_fault,
                "description": r.description,
                "scope": r.asset_scope,
                "fault_kind": r.fault_kind,
                "intensities": list(r.lbnl_intensities),
                "intensity": r.intensity,
                "signals": fdd_rules.signals_json(r, names),
                "params": fdd_rules.merged_params(r, s.get("params")),
                "enabled": s.get("enabled", True),
                "severity": s.get("severity", r.severity),
                "calibrated_at": s.get("calibrated_at"),
                "calibration_job": s.get("job_id"),
            }
        )
    source = {"rules": rules_src, "point_map": point_map, "k_sigma": fdd_rules.K_SIGMA, "site": site}
    return [
        DocSpec(
            key=f"fdd_reference:{site}:lbnl",
            doc_type="fdd_reference",
            title="LBNL chiller plant fault detection reference",
            object_key=f"generated/fdd_reference/{site.lower()}/lbnl-chiller-plant-fdd.pdf",
            site_code=site,
            source=source,
            render=render_fdd_reference,
            alarm_codes=[r.code for r in fdd_rules.RULES],
            meta={"rules": [r.code for r in fdd_rules.RULES]},
        )
    ]


def _param_rows(params: dict[str, Any]) -> list[tuple[str, str]]:
    rows = []
    for k, v in params.items():
        if k.startswith("_") or v is None:
            continue
        if isinstance(v, dict):
            v = ", ".join(f"{a} = {fnum(b)}" for a, b in v.items())
        rows.append((k.replace("_", " "), fnum(v) if isinstance(v, (int, float)) else str(v)))
    return rows


def render_fdd_reference(src: dict[str, Any]) -> bytes:
    pdf = DocPDF(
        "LBNL chiller plant fault detection reference",
        f"Fault types, translated detection rules and point map | Site {src['site']}",
        site=src["site"],
        generated_at=datetime.now(timezone.utc),
    )
    pdf.h1("Purpose and scope")
    pdf.para(
        "The analytics service detects the seven fault types of the LBNL chiller plant fault detection data set on the CUB "
        "chiller plant. Each rule raises its alarm code on the affected equipment through the evaluation engine (alarm report, "
        "condition, notifications and an early-warning corrective order with the code's Diagnostic and Repair procedures), and "
        "a maintenance instruction grounded in the plant documents is attached to the fault."
    )
    pdf.para(
        "The plant uses SI units and its own tags; the LBNL point names are kept in a mapping table (section 4). "
        "Rules are evaluated every 5 minutes on 1-minute data and on every digital twin simulation."
    )
    pdf.h1("Fault types")
    pdf.data_table(
        ["Code", "Fault", "LBNL fault", "Twin fault", "LBNL intensities"],
        [[r["code"], r["name"], r["lbnl_fault"], r["fault_kind"], ", ".join(fnum(i) for i in r["intensities"])] for r in src["rules"]],
        widths=(0.1, 0.28, 0.32, 0.14, 0.16),
    )
    pdf.h1("Detection rules")
    for r in src["rules"]:
        pdf.h2(f"{r['code']} {r['name']}")
        pdf.para(r["description"])
        pdf.para(f"Intensity estimate: {r['intensity']}. Severity {r['severity'].title()}; {'enabled' if r['enabled'] else 'disabled'}.")
        pdf.data_table(
            ["Signal", "Asset", "Parameter"],
            [[s["lbnl"] or "(no LBNL point)", s["asset"], s["param"]] for s in r["signals"]],
            widths=(0.4, 0.25, 0.35),
        )
        pdf.h3("Thresholds and windows in use")
        pdf.kv(_param_rows(r["params"]))
        if r["calibrated_at"]:
            pdf.para(f"Calibrated on fault-free twin data (simulation job {r['calibration_job']}) on {local(r['calibrated_at'])}.")
        else:
            pdf.para("Not calibrated yet: engineering defaults apply.")
        pdf.para(f"Response: follow the Diagnostic and Repair procedures for {r['code']} in the equipment manual; the fault clears once the condition has been false for {r['params'].get('clear_min', 30)} min.")
    pdf.h1("Point map")
    pdf.data_table(
        ["LBNL point", "Asset", "Parameter", "Unit (LBNL / SI)", "Description"],
        [[r["lbnl_name"], r["asset_pattern"], r["param_alias"], f"{r['lbnl_unit']} / {r['si_unit']}", r["description"]] for r in src["point_map"]],
        widths=(0.22, 0.12, 0.17, 0.15, 0.34),
    )
    pdf.h1("Methodology")
    pdf.bullets(
        [
            "Gating: every rule uses only samples where the equipment has been running for a minimum time and the relevant signals are steady.",
            "Persistence: a fault opens when at least 80 % of the known samples over the persistence window satisfy the rule, and clears after the clear window with at most 20 % of samples satisfying it.",
            "Residual rules smooth their residuals with an EWMA (lambda 0.1 per minute) and subtract the fault-free offset.",
            f"Calibration: thresholds = max(engineering floor, {src['k_sigma']:g} x robust sigma) of the rule statistic on a 72 h nominal, "
            "fault-free twin run (the loop-hunting counts above the fault-free 99th percentile); model fits for the tower approach and the "
            "riser DP pump curve. Calibration never makes a rule more sensitive than its engineering default.",
            "Sensor anomaly detectors (range, flat line, robust z-score, EWMA drift, SPC on 15-minute means) run every minute in parallel and report statistical anomalies, not faults.",
        ]
    )
    return pdf.render()


# ---------------------------------------------------------------------------
# Maintenance reports


async def collect_reports(site: str, order_ids: list[int] | None = None) -> list[DocSpec]:
    orders = await db.fetch_all(
        """SELECT mo.id, mo.name, mo.asset_id, mo.description, mo.planned_start, mo.planned_end, mo.detected_at, mo.started_at,
                  mo.completed_at, mo.closed_at, mo.created_at, mo.reject_reason, mo.from_system, mo.is_compound, mo.is_deduction,
                  mo.degradation_name, st.name AS status, mt.name AS mtype, pr.name AS priority, sv.name AS severity, ir.name AS relation,
                  ru.full_name AS requested_by, au.full_name AS approved_by, v.alias, v.name AS asset_name, v.type_name, v.model_id,
                  v.model_name, v.location_id, v.location_path, e.installed_at
           FROM dm.maintenance_orders mo JOIN analytics.v_assets v ON v.id = mo.asset_id JOIN asset.equipment e ON e.id = mo.asset_id
           JOIN dm.maintenance_statuses st ON st.id = mo.status_id JOIN dm.maintenance_types mt ON mt.id = mo.maintenance_type_id
           LEFT JOIN dm.priorities pr ON pr.id = mo.priority_id JOIN dm.severities sv ON sv.id = mo.severity_id
           LEFT JOIN dm.issue_relations ir ON ir.id = mo.issue_relation_id
           LEFT JOIN core.users ru ON ru.id = mo.requested_by LEFT JOIN core.users au ON au.id = mo.approved_by
           WHERE mo.status_id IN (%s, %s) AND v.site_code = %s AND (%s::bigint[] IS NULL OR mo.id = ANY(%s))
           ORDER BY mo.id""",
        (STATUS_DONE, STATUS_CLOSED, site, order_ids, order_ids),
    )
    if not orders:
        return []
    ids = [o["id"] for o in orders]
    techs = await _by_order("SELECT t.order_id, u.full_name, u.employee_id FROM dm.maintenance_order_technicians t JOIN core.users u ON u.id = t.user_id WHERE t.order_id = ANY(%s) ORDER BY u.full_name", ids)
    issues = await _by_order(
        """SELECT i.order_id, ac.id AS code_id, ac.code, ac.name, i.is_other, i.position, i.is_valid_diagnostic, i.is_valid_repair, i.alarm_status, i.detected_at
           FROM dm.maintenance_order_issues i JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id WHERE i.order_id = ANY(%s) ORDER BY i.position""",
        ids,
    )
    oparams = await _by_order(
        """SELECT p.order_id, mp.name, mp.unit, p.condition, p.message, p.detected_at FROM dm.maintenance_order_parameters p
           JOIN asset.equipment_parameters ep ON ep.id = p.equipment_parameter_id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
           WHERE p.order_id = ANY(%s) ORDER BY p.detected_at""",
        ids,
    )
    events = await _by_order(
        "SELECT e.order_id, e.status, e.details, e.created_at, u.full_name FROM dm.maintenance_order_events e LEFT JOIN core.users u ON u.id = e.created_by WHERE e.order_id = ANY(%s) ORDER BY e.created_at, e.id",
        ids,
    )
    notes = await _by_order(
        "SELECT w.order_id, w.note, w.created_at, u.full_name FROM dm.work_updates w LEFT JOIN core.users u ON u.id = w.created_by WHERE w.order_id = ANY(%s) ORDER BY w.created_at",
        ids,
    )
    work = {
        r["order_id"]: float(r["secs"] or 0)
        for r in await db.fetch_all(
            "SELECT order_id, sum(extract(epoch FROM (coalesce(ended_at, started_at) - started_at))) AS secs FROM dm.work_intervals WHERE order_id = ANY(%s) GROUP BY 1",
            (ids,),
        )
    }
    instr_rows = await db.fetch_all(
        """SELECT DISTINCT im.model_id, i.alarm_code_id, i.title, it.name AS type, i.instruction_type_id, i.duration
           FROM dm.instructions i JOIN dm.instruction_models im ON im.instruction_id = i.id JOIN dm.instruction_types it ON it.id = i.instruction_type_id
           WHERE NOT i.is_deleted AND im.model_id = ANY(%s)""",
        (sorted({o["model_id"] for o in orders}),),
    )
    instr: dict[tuple[int, int], list[dict[str, Any]]] = defaultdict(list)
    for r in instr_rows:
        instr[(r["model_id"], r["alarm_code_id"])].append(r)
    kpis = {
        r["asset_id"]: r
        for r in await db.fetch_all("SELECT asset_id, mtbf_hours, mttr_hours, failures, repairs FROM dm.asset_kpis WHERE asset_id = ANY(%s)", (sorted({o["asset_id"] for o in orders}),))
    }
    out = []
    for o in orders:
        oid = o["id"]
        detected = o["detected_at"] or o["created_at"]
        completed = o["completed_at"] or o["closed_at"]
        codes = [i for i in issues.get(oid, [])]
        code_ids = [i["code_id"] for i in codes]
        alarms_before = await db.fetch_all(
            "SELECT name, severity, message, start_ts, end_ts FROM ts.alarm_events WHERE asset_id = %s AND start_ts BETWEEN %s AND %s ORDER BY start_ts DESC LIMIT 12",
            (o["asset_id"], detected - timedelta(hours=24), detected),
        )
        evidence = await db.fetch_all(
            """SELECT mp.name, mp.unit, mp.scale,
                      (SELECT avg(value) FROM ts.parameter_values WHERE equipment_parameter_id = ep.id AND ts BETWEEN %(d)s - interval '24 hours' AND %(d)s) AS before,
                      (SELECT avg(value) FROM ts.parameter_values WHERE equipment_parameter_id = ep.id AND ts BETWEEN %(c)s AND %(c)s + interval '24 hours') AS after
               FROM (SELECT DISTINCT model_parameter_id FROM dm.alarm_mappings WHERE model_id = %(m)s AND alarm_code_id = ANY(%(codes)s)) am
               JOIN asset.model_parameters mp ON mp.id = am.model_parameter_id
               JOIN asset.equipment_parameters ep ON ep.model_parameter_id = mp.id AND ep.equipment_id = %(a)s
               ORDER BY mp.id""",
            {"d": detected, "c": completed or detected, "m": o["model_id"], "codes": code_ids, "a": o["asset_id"]},
        )
        down = await db.fetch_all(
            """SELECT status_id, sum(extract(epoch FROM (least(coalesce(end_ts, now()), %(c)s) - greatest(start_ts, %(s)s)))) AS secs
               FROM ts.asset_status WHERE asset_id = %(a)s AND status_id IN (3, 5) AND start_ts < %(c)s AND coalesce(end_ts, now()) > %(s)s
               GROUP BY 1""",
            {"a": o["asset_id"], "s": o["started_at"] or detected, "c": completed or detected},
        )
        performed = [i | {"code": c["code"]} for c in codes for i in instr.get((o["model_id"], c["code_id"]), [])]
        performed.sort(key=lambda r: (r["code"], r["instruction_type_id"]))
        source = {
            "order": o,
            "technicians": techs.get(oid, []),
            "issues": codes,
            "params": oparams.get(oid, []),
            "events": [{**e, "details": e["details"]} for e in events.get(oid, [])],
            "notes": notes.get(oid, []),
            "work_hours": round(work.get(oid, 0.0) / 3600, 2),
            "estimate_min": sum(int(i["duration"] or 0) for i in performed),
            "performed": performed,
            "alarms_before": alarms_before,
            "evidence": evidence,
            "downtime": {("unplanned" if r["status_id"] == 3 else "planned"): round(float(r["secs"] or 0) / 3600, 2) for r in down},
            "kpi": kpis.get(o["asset_id"]),
            "site": site,
        }
        when = (completed or o["created_at"]).astimezone(tz())
        out.append(
            DocSpec(
                key=f"maintenance_report:{site}:{oid}",
                doc_type="maintenance_report",
                title=f"Maintenance report {o['name']} - {o['asset_name']}",
                object_key=f"generated/maintenance_report/{site.lower()}/{when:%Y}/{slug(o['name'])}-o{oid}.pdf",
                site_code=site,
                source=source,
                render=render_report,
                doc_date=when.date(),
                location_id=o["location_id"],
                asset_id=o["asset_id"],
                unit=o["alias"],
                asset_type=o["type_name"],
                model_id=o["model_id"],
                alarm_codes=sorted({c["code"] for c in codes}),
                meta={"order_id": oid, "order_name": o["name"]},
                volatile=("kpi",),
            )
        )
    return out


async def _by_order(sql: str, ids: list[int]) -> dict[int, list[dict[str, Any]]]:
    out: dict[int, list[dict[str, Any]]] = defaultdict(list)
    for r in await db.fetch_all(sql, (ids,)):
        out[r["order_id"]].append(r)
    return out


def _event_text(e: dict[str, Any]) -> str:
    d = e.get("details") or {}
    if isinstance(d, dict):
        parts = []
        for k in ("Note", "Reason", "Detected", "diagnosis", "repair"):
            if d.get(k):
                parts.append(f"{k.lower()}: {d[k]}")
        status = d.get("Equipment Status")
        if status:
            parts.append(f"equipment {status}")
        return "; ".join(str(p) for p in parts)
    return str(d)


def render_report(src: dict[str, Any]) -> bytes:
    o = src["order"]
    pdf = DocPDF(
        f"Maintenance report {o['name']} - {o['asset_name']}",
        f"{o['mtype'].title()} maintenance | {o['alias']} {o['model_name']} | {o['location_path']}",
        site=src["site"],
        generated_at=datetime.now(timezone.utc),
    )
    pdf.h1("Order")
    pdf.kv(
        [
            ("Order", o["name"]),
            ("Status", o["status"]),
            ("Maintenance type", o["mtype"].title()),
            ("Severity / priority", f"{(o['severity'] or '').title()} / {(o['priority'] or '-').title()}"),
            ("Equipment", f"{o['alias']} {o['asset_name']}"),
            ("Model / type", f"{o['model_name']} / {o['type_name']}"),
            ("Location", o["location_path"]),
            ("Installed", local(o["installed_at"])[:10]),
            ("Origin", "Early-warning diagnosis (system)" if o["from_system"] else "Requested by a person"),
            ("Issue relation", f"{o['relation']}{' (compound)' if o['is_compound'] else ''}{' (deduction)' if o['is_deduction'] else ''}" if o["relation"] else None),
        ]
    )
    pdf.h1("People")
    pdf.kv(
        [
            ("Requested by", o["requested_by"] or "System"),
            ("Approved by", o["approved_by"]),
            ("Technicians", ", ".join(f"{t['full_name']} ({t['employee_id']})" if t.get("employee_id") else t["full_name"] for t in src["technicians"])),
        ]
    )
    pdf.h1("Timeline")
    pdf.kv(
        [
            ("Detected", local(o["detected_at"])),
            ("Requested", local(o["created_at"])),
            ("Planned", f"{local(o['planned_start'])} to {local(o['planned_end'])}" if o["planned_end"] else local(o["planned_start"])),
            ("Started", local(o["started_at"])),
            ("Completed", local(o["completed_at"])),
            ("Closed", local(o["closed_at"])),
            ("Actual working time", f"{src['work_hours']:g} h"),
            ("Estimated time (procedures)", f"{src['estimate_min'] / 60:g} h" if src["estimate_min"] else None),
            ("Downtime during the work", ", ".join(f"{k} {v:g} h" for k, v in src["downtime"].items()) or None),
        ]
    )
    if src["events"]:
        pdf.data_table(
            ["Time", "Event", "By", "Details"],
            [[local(e["created_at"]), e["status"], e["full_name"] or (e.get("details") or {}).get("By", ""), _event_text(e)] for e in src["events"]],
            widths=(0.18, 0.14, 0.18, 0.5),
        )
    pdf.h1("Problem")
    pdf.para(o["description"])
    if src["issues"]:
        pdf.data_table(
            ["Code", "Issue", "Alarm status", "Diagnosis valid", "Repair valid"],
            [[i["code"], i["name"], i["alarm_status"] or "", _yn(i["is_valid_diagnostic"]), _yn(i["is_valid_repair"])] for i in src["issues"]],
            widths=(0.13, 0.42, 0.15, 0.15, 0.15),
        )
    if src["params"]:
        pdf.h2("Contributing parameters")
        pdf.bullets([f"{p['message'] or p['name']} ({p['condition']}, {local(p['detected_at'])})" for p in src["params"]])
    if o.get("reject_reason"):
        pdf.para(f"Reject reason: {o['reject_reason']}")
    pdf.h1("Evidence")
    if src["alarms_before"]:
        pdf.h2("Alarms in the 24 h before detection")
        pdf.data_table(
            ["Start", "Alarm", "Severity", "Message", "End"],
            [[local(a["start_ts"]), a["name"], a["severity"].title(), a["message"], local(a["end_ts"])] for a in src["alarms_before"]],
            widths=(0.17, 0.13, 0.11, 0.42, 0.17),
        )
    ev = [e for e in src["evidence"] if e["before"] is not None or e["after"] is not None]
    if ev:
        pdf.h2("Parameter values before and after the work (24 h means)")
        pdf.data_table(
            ["Parameter", "Unit", "Before", "After", "Change"],
            [
                [e["name"], e["unit"], fnum(e["before"], e["scale"]), fnum(e["after"], e["scale"]),
                 fnum(float(e["after"]) - float(e["before"]), e["scale"]) if e["before"] is not None and e["after"] is not None else ""]
                for e in ev
            ],
            widths=(0.4, 0.12, 0.16, 0.16, 0.16),
        )  # fmt: skip
    if not src["alarms_before"] and not ev:
        pdf.para("No alarm or mapped parameter evidence was recorded around this order.")
    pdf.h1("Work performed")
    if src["notes"]:
        pdf.data_table(["Time", "Technician", "Note"], [[local(n["created_at"]), n["full_name"], n["note"]] for n in src["notes"]], widths=(0.18, 0.2, 0.62))
    else:
        pdf.para("No work notes were recorded.")
    if src["performed"]:
        pdf.h2("Procedures performed")
        pdf.data_table(
            ["Code", "Procedure", "Type", "Duration"],
            [[p["code"], p["title"], p["type"], f"{p['duration']} min"] for p in src["performed"]],
            widths=(0.13, 0.55, 0.16, 0.16),
        )
    pdf.h1("Reliability KPIs")
    k = src.get("kpi")
    if k:
        pdf.kv(
            [
                ("MTBF", f"{fnum(k['mtbf_hours'], 1)} h" if k["mtbf_hours"] is not None else "n/a"),
                ("MTTR", f"{fnum(k['mttr_hours'], 2)} h" if k["mttr_hours"] is not None else "n/a"),
                ("Failures (12 months)", k["failures"]),
                ("Repairs (12 months)", k["repairs"]),
            ]
        )
    else:
        pdf.para("No reliability KPIs are available for this equipment.")
    return pdf.render()


def _yn(v: Any) -> str:
    return "" if v is None else ("Yes" if v else "No")


# ---------------------------------------------------------------------------
# Set-point change logs


async def collect_setpoint_logs(site: str) -> list[DocSpec]:
    reqs = await db.fetch_all(
        """SELECT cr.id, cr.requested_at, cr.completed_at, cr.status, cr.reason, cr.old_value, cr.value, v.alias, v.name AS asset_name,
                  v.building_id, v.location_id, mp.alias AS param, mp.name AS param_name, mp.unit, mp.scale, mp.control_options, u.full_name AS user_name
           FROM asset.control_requests cr JOIN analytics.v_assets v ON v.id = cr.equipment_id
           JOIN asset.equipment_parameters ep ON ep.id = cr.equipment_parameter_id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
           LEFT JOIN core.users u ON u.id = cr.user_id
           WHERE v.site_code = %s ORDER BY cr.requested_at, cr.id""",
        (site,),
    )
    recs = await db.fetch_all(
        """SELECT r.id, r.decided_at, r.actions, r.savings_kw, r.savings_pct, r.rationale, u.full_name AS user_name
           FROM analytics.recommendations r LEFT JOIN core.users u ON u.id = r.decided_by
           WHERE r.status = 'APPLIED' AND r.site_code = %s AND r.decided_at IS NOT NULL ORDER BY r.decided_at""",
        (site,),
    )
    parents = await location_parents()
    months: dict[str, dict[str, list[dict[str, Any]]]] = defaultdict(lambda: {"requests": [], "recommendations": []})
    for r in reqs:
        months[r["requested_at"].astimezone(tz()).strftime("%Y-%m")]["requests"].append(r)
    for r in recs:
        months[r["decided_at"].astimezone(tz()).strftime("%Y-%m")]["recommendations"].append(r)
    out = []
    for month, content in sorted(months.items()):
        stamps = [r["requested_at"] for r in content["requests"]] + [r["decided_at"] for r in content["recommendations"]]
        last = max(stamps).astimezone(tz())
        label = datetime.strptime(month, "%Y-%m").strftime("%B %Y")
        out.append(
            DocSpec(
                key=f"setpoint_log:{site}:{month}",
                doc_type="setpoint_log",
                title=f"Set-point change log {label} - {site}",
                object_key=f"generated/setpoint_log/{site.lower()}/{month}.pdf",
                site_code=site,
                source={"month": month, "label": label, "site": site, **content},
                render=render_setpoint_log,
                doc_date=last.date(),
                location_id=common_location(content["requests"], parents) if content["requests"] else None,
                meta={"month": month},
            )
        )
    return out


def _option(value: Any, options: Any) -> str:
    for o in options or []:
        if value is not None and float(o.get("value", -1)) == float(value):
            return str(o.get("label"))
    return ""


def _fmt_value(v: Any, r: dict[str, Any]) -> str:
    label = _option(v, r.get("control_options"))
    if label:
        return label
    s = fnum(v, r.get("scale"))
    return f"{s} {r['unit'] or ''}".strip() if s else ""


def render_setpoint_log(src: dict[str, Any]) -> bytes:
    pdf = DocPDF(
        f"Set-point change log {src['label']} - {src['site']}",
        "Operator set-point and mode changes through the digital twin control panel, and applied optimizer recommendations",
        site=src["site"],
        generated_at=datetime.now(timezone.utc),
    )
    reqs = src["requests"]
    pdf.h1("Summary")
    by_status: dict[str, int] = defaultdict(int)
    by_point: dict[str, int] = defaultdict(int)
    for r in reqs:
        by_status[r["status"]] += 1
        by_point[f"{r['alias']} {r['param_name']}"] += 1
    pdf.kv(
        [
            ("Month", src["label"]),
            ("Change requests", len(reqs)),
            ("By result", ", ".join(f"{k.title()} {v}" for k, v in sorted(by_status.items()))),
            ("Applied recommendations", len(src["recommendations"])),
        ]
    )
    if by_point:
        pdf.h2("Most changed points")
        pdf.data_table(["Point", "Changes"], [[k, v] for k, v in sorted(by_point.items(), key=lambda kv: (-kv[1], kv[0]))[:15]], widths=(0.8, 0.2))
    pdf.h1("Change requests")
    if reqs:
        pdf.data_table(
            ["Time", "Equipment", "Parameter", "Change", "Result", "By"],
            [
                [local(r["requested_at"]), r["alias"], r["param_name"], f"{_fmt_value(r['old_value'], r)} → {_fmt_value(r['value'], r)}",
                 (r["status"].title() + (f": {r['reason']}" if r["reason"] else "")), r["user_name"] or ""]
                for r in reqs
            ],
            widths=(0.16, 0.1, 0.22, 0.2, 0.18, 0.14),
        )  # fmt: skip
    else:
        pdf.para("No operator changes were requested this month.")
    pdf.h1("Applied optimizer recommendations")
    if src["recommendations"]:
        for r in src["recommendations"]:
            pdf.h2(f"Recommendation {r['id']} applied {local(r['decided_at'])} by {r['user_name'] or 'unknown'}")
            pdf.para(f"Predicted saving {fnum(r['savings_kw'], 1)} kW ({fnum(r['savings_pct'], 2)} %). {r['rationale'] or ''}")
            pdf.bullets([a.get("label") or f"{a.get('asset')} {a.get('param')} = {a.get('value')}" for a in (r["actions"] or [])], numbered=True)
    else:
        pdf.para("No optimizer recommendation was applied this month.")
    return pdf.render()


# ---------------------------------------------------------------------------
# Publication


COLLECTORS: dict[str, Callable[[str], Any]] = {
    "manual": collect_manuals,
    "fdd_reference": collect_fdd_reference,
    "maintenance_report": collect_reports,
    "setpoint_log": collect_setpoint_logs,
}


async def publish(
    spec: DocSpec, *, force: bool = False, out_dir: str | None = None, key_prefix: str = "", dry_run: bool = False
) -> tuple[str, int | None]:
    """Render, upload and register one document when new or changed: ("created" | "updated" | "unchanged", id).

    dry_run: render (and write to out_dir) only; returns "would_create" / "would_update" / "unchanged".
    """
    key = key_prefix + spec.key
    obj = (f"{key_prefix.strip(':').replace(':', '-')}/" if key_prefix else "") + spec.object_key
    title = (f"[{key_prefix.strip(':').upper()}] " if key_prefix else "") + spec.title
    h = spec.source_hash()
    existing = await db.fetch_one(
        "SELECT id, status, object_key, metadata ->> 'source_hash' AS source_hash, location_id, asset_id, unit, asset_type"
        " FROM rag.documents WHERE source = 'generated' AND metadata ->> 'generator_key' = %s",
        (key,),
    )
    if existing and existing["source_hash"] == h and existing["status"] != "FAILED" and not force:
        if not dry_run:
            await _sync_placement(existing, spec)
        return "unchanged", int(existing["id"])
    pdf = await asyncio.to_thread(spec.render, spec.source)
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
        with open(os.path.join(out_dir, spec.file_name), "wb") as fh:
            fh.write(pdf)
    if dry_run:
        return ("would_update" if existing else "would_create"), (int(existing["id"]) if existing else None)
    await storage.put_bytes(obj, pdf, "application/pdf")
    meta = {"generator_key": key, "source_hash": h, "corpus_version": CORPUS_VERSION, **spec.meta}
    row = await db.fetch_one(
        """INSERT INTO rag.documents (title, doc_type, source, object_key, file_name, content_type, size_bytes, sha256, site_code,
               location_id, asset_id, unit, asset_type, model_id, alarm_codes, doc_date, status, metadata)
           VALUES (%s, %s, 'generated', %s, %s, 'application/pdf', %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, 'PENDING', %s)
           ON CONFLICT ((metadata ->> 'generator_key')) WHERE source = 'generated' DO UPDATE SET
               title = EXCLUDED.title, doc_type = EXCLUDED.doc_type, object_key = EXCLUDED.object_key, file_name = EXCLUDED.file_name,
               size_bytes = EXCLUDED.size_bytes, sha256 = EXCLUDED.sha256, site_code = EXCLUDED.site_code,
               location_id = EXCLUDED.location_id, asset_id = EXCLUDED.asset_id, unit = EXCLUDED.unit, asset_type = EXCLUDED.asset_type,
               model_id = EXCLUDED.model_id, alarm_codes = EXCLUDED.alarm_codes, doc_date = EXCLUDED.doc_date,
               status = 'PENDING', error = NULL, metadata = EXCLUDED.metadata, updated_at = now()
           RETURNING id, (xmax = 0) AS inserted""",
        (
            title, spec.doc_type, obj, spec.file_name, len(pdf), hashlib.sha256(pdf).hexdigest(), spec.site_code,
            spec.location_id, spec.asset_id, spec.unit, spec.asset_type, spec.model_id, spec.alarm_codes, spec.doc_date, Jsonb(meta),
        ),
    )  # fmt: skip
    assert row is not None
    if existing and existing["object_key"] != obj:
        await storage.delete(existing["object_key"])  # the object moved (title change): drop the old original
    await db.notify("rag_documents", str(row["id"]))
    return ("created" if row["inserted"] else "updated"), int(row["id"])


async def _sync_placement(existing: dict[str, Any], spec: DocSpec) -> None:
    """Move an unchanged document (and its chunks, which carry the same filter columns) to the
    placement the generator now gives it: no re-render and no new embeddings."""
    want = {"location_id": spec.location_id, "asset_id": spec.asset_id, "unit": spec.unit, "asset_type": spec.asset_type}
    if all(existing.get(k) == v for k, v in want.items()):
        return
    args = (spec.location_id, spec.asset_id, spec.unit, spec.asset_type, int(existing["id"]))
    await db.execute(
        "UPDATE rag.documents SET location_id = %s, asset_id = %s, unit = %s, asset_type = %s, updated_at = now() WHERE id = %s", args
    )
    await db.execute("UPDATE rag.chunks SET location_id = %s, asset_id = %s, unit = %s, asset_type = %s WHERE document_id = %s", args)
    log.info("document %s moved to location %s", existing["id"], spec.location_id)


async def delete_document(doc_id: int) -> None:
    """Delete a document row (chunks cascade) and its stored original."""
    row = await db.fetch_one("DELETE FROM rag.documents WHERE id = %s RETURNING object_key", (doc_id,))
    if row:
        await storage.delete(row["object_key"])


async def remove_stale(site: str, kinds: list[str], keep: set[str]) -> int:
    rows = await db.fetch_all(
        "SELECT id, metadata ->> 'generator_key' AS key FROM rag.documents WHERE source = 'generated' AND site_code = %s AND doc_type = ANY(%s)",
        (site, kinds),
    )
    n = 0
    for r in rows:
        key = r["key"] or ""
        if key in keep or not any(key.startswith(f"{k}:{site}:") for k in kinds):
            continue
        await delete_document(int(r["id"]))
        n += 1
    if n:
        log.info("removed %s stale generated documents site=%s", n, site)
    return n


async def generate(
    site: str | None = None,
    *,
    kinds: Iterable[str] = KINDS,
    order_ids: list[int] | None = None,
    force: bool = False,
    out_dir: str | None = None,
    key_prefix: str = "",
    limit: int | None = None,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Generate / refresh the corpus. With key_prefix (tests) nothing stale is removed.

    dry_run: collect and render every document (into out_dir when given) without
    uploading, registering or removing anything.
    """
    site = site or get_settings().site_code
    if not dry_run:
        await storage.ensure_bucket()
    summary: dict[str, Any] = {"created": 0, "updated": 0, "unchanged": 0, "failed": 0, "removed": 0, "ids": []}
    if dry_run:
        summary |= {"would_create": 0, "would_update": 0, "by_kind": {}}
    complete: list[str] = []
    keep: set[str] = set()
    for kind in kinds:
        try:
            specs = await (collect_reports(site, order_ids) if kind == "maintenance_report" else COLLECTORS[kind](site))
        except Exception:  # noqa: BLE001 - other kinds still run
            log.exception("corpus collection failed kind=%s", kind)
            summary["failed"] += 1
            continue
        ok = True
        if dry_run:
            summary["by_kind"][kind] = len(specs)
        for spec in specs[:limit] if limit else specs:
            keep.add(spec.key)
            try:
                result, doc_id = await publish(spec, force=force, out_dir=out_dir, key_prefix=key_prefix, dry_run=dry_run)
                summary[result] += 1
                if doc_id is not None:
                    summary["ids"].append(doc_id)
            except Exception:  # noqa: BLE001 - one document must not stop the corpus
                ok = False
                summary["failed"] += 1
                log.exception("corpus document failed key=%s", spec.key)
        if ok and not limit and order_ids is None:
            complete.append(kind)
    if not key_prefix and complete and not dry_run:
        summary["removed"] = await remove_stale(site, complete, keep)
    log.info("corpus site=%s %s", site, {k: v for k, v in summary.items() if k != "ids"})
    return summary
