"""Database access shared by the analytics tasks: equipment and parameter
metadata, production values on a 1-minute grid, status intervals,
simulation series and system simulation jobs.

All reads go through the afdd_ai role (read asset / ts / dm, read-write
rag / analytics / sim). Bulk numeric reads use tuple rows (the shared pool
returns dict rows) to keep the warm-up of 24 h buffers cheap.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any

import numpy as np
from psycopg.rows import tuple_row
from psycopg.types.json import Jsonb

from ..common import db
from ..common.config import get_settings
from .series import Frame, as_array, frame_from_rows

log = logging.getLogger(__name__)

STATUS_RUNNING = 1
STATUS_PARAMS = frozenset({"running", "trip", "maint_mode"})

# asset.types.name -> plant role (FDD layout and model physics keys)
TYPE_ROLE = {
    "Centrifugal Chiller": "chiller",
    "Cooling Tower": "tower",
    "Chilled Water Pump": "chwp",
    "Condenser Water Pump": "cwp",
    "Process Cooling Water Pump": "pcw",
    "Air Compressor": "cda",
    "Make-up Air Unit": "mau",
    "Dry Vacuum Pump": "vac",
    "Chilled Water System": "chws",
}


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


# ---------------------------------------------------------------------------
# Metadata


@dataclass
class Asset:
    id: int
    alias: str
    name: str
    type_name: str
    model_id: int
    model_name: str
    location_id: int | None
    location_path: str | None
    building_id: int | None
    site_code: str

    @property
    def role(self) -> str:
        return TYPE_ROLE.get(self.type_name, "other")


@dataclass
class Param:
    """An equipment parameter (asset.equipment_parameters + model parameter)."""

    id: int
    asset_id: int
    alias: str
    name: str
    unit: str
    data_type: str
    scale: int | None
    is_writable: bool
    is_displayed: bool
    is_logged: bool
    warn_lower: float | None = None
    warn_upper: float | None = None
    crit_lower: float | None = None
    crit_upper: float | None = None

    @property
    def resolution(self) -> float:
        """Display resolution (values are logged rounded to `scale` decimals)."""
        return 10.0 ** (-(self.scale if self.scale is not None else 2))


@dataclass
class Plant:
    """Equipment of one site, by alias."""

    assets: dict[str, Asset] = field(default_factory=dict)

    def by_role(self, role: str) -> list[Asset]:
        return sorted((a for a in self.assets.values() if a.role == role), key=lambda a: a.alias)

    def get(self, alias: str) -> Asset | None:
        return self.assets.get(alias)


async def load_assets(site_code: str | None = None) -> list[Asset]:
    rows = await db.fetch_all(
        "SELECT id, alias, name, type_name, model_id, model_name, location_id, location_path, building_id, site_code"
        " FROM analytics.v_assets WHERE (%s::text IS NULL OR site_code = %s) ORDER BY id",
        (site_code, site_code),
    )
    return [Asset(**r) for r in rows]


async def load_plant(site_code: str | None = None) -> Plant:
    return Plant({a.alias: a for a in await load_assets(site_code)})


async def load_params(asset_ids: Sequence[int]) -> list[Param]:
    if not asset_ids:
        return []
    rows = await db.fetch_all(
        """SELECT ep.id, ep.equipment_id AS asset_id, mp.alias, mp.name, mp.unit, mp.data_type, mp.scale,
                  mp.is_writable, ep.is_displayed, ep.is_logged,
                  ep.warning_lower_limit AS warn_lower, ep.warning_upper_limit AS warn_upper,
                  ep.critical_lower_limit AS crit_lower, ep.critical_upper_limit AS crit_upper
           FROM asset.equipment_parameters ep
           JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
           WHERE ep.equipment_id = ANY(%s)
           ORDER BY ep.equipment_id, mp.alias""",
        (list(asset_ids),),
    )
    return [Param(**r) for r in rows]


# ---------------------------------------------------------------------------
# Production values


async def fetch_values(param_ids: Sequence[int], since: datetime, until: datetime | None = None) -> list[tuple[int, datetime, float | None]]:
    """(equipment_parameter_id, ts, value) rows with since < ts <= until, ordered by ts."""
    if not param_ids:
        return []
    pool = await db.get_pool()
    async with pool.connection() as conn:
        cur = conn.cursor(row_factory=tuple_row)
        await cur.execute(
            "SELECT equipment_parameter_id, ts, value FROM ts.parameter_values"
            " WHERE equipment_parameter_id = ANY(%s) AND ts > %s AND (%s::timestamptz IS NULL OR ts <= %s)"
            " ORDER BY ts",
            (list(param_ids), since, until, until),
        )
        return list(await cur.fetchall())


async def live_frame(
    plant: Plant,
    wanted: dict[str, Sequence[str]],
    start: datetime,
    end: datetime,
    *,
    step_s: int = 60,
) -> Frame:
    """Production values of {alias: [param, ...]} on a regular grid [start, end]."""
    aliases = [a for a in wanted if a in plant.assets]
    params = await load_params([plant.assets[a].id for a in aliases])
    by_asset = {a.id: a.alias for a in plant.assets.values()}
    keys: dict[int, str] = {}
    for p in params:
        alias = by_asset.get(p.asset_id)
        if alias and p.alias in wanted.get(alias, ()):
            keys[p.id] = f"{alias}.{p.alias}"
    rows = await fetch_values(list(keys), start - timedelta(minutes=3), end)
    return frame_from_rows(((keys[pid], ts, v) for pid, ts, v in rows), start, end, step_s=step_s)


@dataclass
class StatusSpan:
    status_id: int
    start: datetime
    end: datetime | None


async def fetch_status(asset_ids: Sequence[int], since: datetime, until: datetime | None = None) -> dict[int, list[StatusSpan]]:
    """Status intervals overlapping [since, until] per asset (ordered by start)."""
    out: dict[int, list[StatusSpan]] = {}
    if not asset_ids:
        return out
    rows = await db.fetch_all(
        "SELECT asset_id, status_id, start_ts, end_ts FROM ts.asset_status"
        " WHERE asset_id = ANY(%s) AND (end_ts IS NULL OR end_ts > %s) AND (%s::timestamptz IS NULL OR start_ts < %s)"
        " ORDER BY asset_id, start_ts",
        (list(asset_ids), since, until, until),
    )
    for r in rows:
        out.setdefault(r["asset_id"], []).append(StatusSpan(r["status_id"], r["start_ts"], r["end_ts"]))
    return out


def running_mask(spans: list[StatusSpan] | None, t0: datetime, step_s: int, n: int) -> np.ndarray:
    """1.0 where the grid sample lies in a RUNNING interval, else 0.0.

    An equipment without status rows in the window counts as running (like
    the twin's SPC), so units without status scripts are not excluded.
    """
    if not spans:
        return np.ones(n)
    mask = np.zeros(n)
    base = t0.timestamp()
    for sp in spans:
        if sp.status_id != STATUS_RUNNING:
            continue
        # compare truncated to the second (the engine stamps changes with the exact time)
        s = int((int(sp.start.timestamp()) - base + step_s - 1) // step_s)
        e = n if sp.end is None else int((int(sp.end.timestamp()) - base + step_s - 1) // step_s)
        s, e = max(s, 0), min(e, n)
        if e > s:
            mask[s:e] = 1.0
    return mask


# ---------------------------------------------------------------------------
# Simulation jobs


async def insert_job(
    *,
    kind: str,
    title: str,
    origin: str,
    spec: dict[str, Any],
    rationale: str,
    recommendation_id: int | None = None,
    step_s: int = 60,
) -> int:
    """Queue a system simulation job (status QUEUED, no approval needed) and NOTIFY sim_jobs."""
    row = await db.fetch_one(
        "INSERT INTO sim.jobs (kind, title, origin, status, spec, rationale, recommendation_id, step_s)"
        " VALUES (%s, %s, %s, 'QUEUED', %s, %s, %s, %s) RETURNING id",
        (kind, title, origin, Jsonb(spec), rationale, recommendation_id, step_s),
    )
    assert row is not None
    job_id = int(row["id"])
    await db.notify("sim_jobs", str(job_id))
    log.info("queued simulation job id=%s kind=%s origin=%s title=%r", job_id, kind, origin, title)
    return job_id


async def load_job(job_id: int) -> dict[str, Any] | None:
    return await db.fetch_one(
        "SELECT id, kind, title, origin, status, spec, summary, error, recommendation_id, sim_start, sim_end, step_s,"
        " created_at, started_at, finished_at FROM sim.jobs WHERE id = %s",
        (job_id,),
    )


async def latest_job(purpose: str, statuses: Sequence[str] | None = None) -> dict[str, Any] | None:
    """Latest analytics system job of a purpose (spec.purpose), optionally by status."""
    return await db.fetch_one(
        "SELECT id, kind, status, spec, summary, error, created_at, finished_at FROM sim.jobs"
        " WHERE origin = 'analytics' AND spec ->> 'purpose' = %s AND (%s::text[] IS NULL OR status = ANY(%s))"
        " ORDER BY id DESC LIMIT 1",
        (purpose, list(statuses) if statuses else None, list(statuses) if statuses else None),
    )


async def sim_frames(job_id: int, variants: Sequence[str] | None = None) -> dict[str, Frame]:
    """Measured series of a job per variant as Frames (asset.param keys)."""
    pool = await db.get_pool()
    async with pool.connection() as conn:
        cur = conn.cursor(row_factory=tuple_row)
        await cur.execute(
            "SELECT variant, asset_alias, param_alias, t0, step_s, measured FROM sim.series"
            " WHERE job_id = %s AND (%s::text[] IS NULL OR variant = ANY(%s)) ORDER BY variant, asset_alias, param_alias",
            (job_id, list(variants) if variants else None, list(variants) if variants else None),
        )
        rows = await cur.fetchall()
    frames: dict[str, Frame] = {}
    for variant, alias, param, t0, step_s, measured in rows:
        arr = as_array(measured or [])
        f = frames.get(variant)
        if f is None:
            f = frames[variant] = Frame(t0=t0.astimezone(timezone.utc), step_s=int(step_s), n=len(arr))
        if len(arr) != f.n:  # defensive: align to the variant grid
            fixed = np.full(f.n, np.nan)
            k = min(f.n, len(arr))
            fixed[:k] = arr[:k]
            arr = fixed
        f.data[f"{alias}.{param}"] = arr
    return frames


def jsonable(value: Any) -> Any:
    """Plain JSON value (numpy scalars and NaN cleaned) for JSONB columns."""
    if isinstance(value, dict):
        return {str(k): jsonable(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [jsonable(v) for v in value]
    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, float):
        # significant digits, not decimals: fitted model coefficients can be ~1e-6
        return None if not np.isfinite(value) else float(f"{value:.8g}")
    if isinstance(value, datetime):
        return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
    return value


def jb(value: Any) -> Jsonb:
    return Jsonb(jsonable(value))


def dumps(value: Any) -> str:
    return json.dumps(jsonable(value), ensure_ascii=False)


def settings_site() -> str:
    return get_settings().site_code
