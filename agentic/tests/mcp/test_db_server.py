"""afdd-db against the live database (read-only role): data, scope filtering, permissions, limits."""

from __future__ import annotations

import uuid
from collections.abc import Iterator

import psycopg
import pytest
from psycopg.types.json import Jsonb

from tests.agent.support import TOKEN

from .conftest import mcp_session, text_of


async def call(servers, token: str, tool: str, args: dict):
    async with mcp_session(servers["db"].url, token) as s:
        return await s.call_tool(tool, args)


async def test_parameter_history_and_scope(servers, plant):
    res = await call(servers, "tok-plant", "query_parameter_history", {"asset_alias": "ch-01", "param_aliases": ["chwst", "load_pct"], "start": "-6h", "bucket_minutes": 30})
    assert not res.is_error, text_of(res)
    data = res.structured_content
    assert data["asset"] == "CH_01" and data["bucket_minutes"] == 30 and set(data["series"]) == {"chwst", "load_pct"}
    pts = data["series"]["chwst"]["points"]
    assert pts and len(pts) <= 13 and pts[0][0].endswith("+08:00")
    assert data["series"]["chwst"]["unit"] == "°C"

    # A tower is outside the plant-room user's scope; the admin sees it.
    denied = await call(servers, "tok-plant", "query_parameter_history", {"asset_alias": "CT_01", "param_aliases": ["cwst"]})
    assert denied.is_error and "No visible equipment with alias 'CT_01'" in text_of(denied)
    allowed = await call(servers, "tok-admin", "query_parameter_history", {"asset_alias": "CT_01", "param_aliases": ["cwst"], "start": "-2h"})
    assert not allowed.is_error and allowed.structured_content["series"]["cwst"]["points"]


async def test_parameter_history_limits_and_errors(servers):
    res = await call(servers, TOKEN, "query_parameter_history", {"asset_alias": "CH_01", "param_aliases": ["chwst", "chwrt"], "start": "-30d", "bucket_minutes": 1})
    assert not res.is_error, text_of(res)
    data = res.structured_content
    assert data["bucket_minutes"] >= 18 and "bucket enlarged" in data["note"]
    assert sum(len(s["points"]) for s in data["series"].values()) <= 5000

    unknown = await call(servers, TOKEN, "query_parameter_history", {"asset_alias": "CH_01", "param_aliases": ["nope"]})
    assert unknown.is_error and "has no parameter nope" in text_of(unknown) and "chwst" in text_of(unknown)
    too_long = await call(servers, TOKEN, "query_parameter_history", {"asset_alias": "CH_01", "param_aliases": ["chwst"], "start": "-200d"})
    assert too_long.is_error and "at most 92 days" in text_of(too_long)
    bad_agg = await call(servers, TOKEN, "query_parameter_history", {"asset_alias": "CH_01", "param_aliases": ["chwst"], "agg": "median"})
    assert bad_agg.is_error and "agg must be one of" in text_of(bad_agg)
    bad_time = await call(servers, TOKEN, "query_parameter_history", {"asset_alias": "CH_01", "param_aliases": ["chwst"], "start": "yesterday"})
    assert bad_time.is_error and "ISO 8601" in text_of(bad_time)


async def test_alarm_events_are_scope_filtered(servers, plant):
    res = await call(servers, "tok-plant", "list_alarm_events", {"start": "-30d", "limit": 2000})
    assert not res.is_error, text_of(res)
    aliases = {e["alias"] for e in res.structured_content["events"]}
    assert aliases <= set(plant["plant_room_aliases"])
    admin = await call(servers, "tok-admin", "list_alarm_events", {"start": "-30d", "limit": 2000})
    assert admin.structured_content["count"] >= res.structured_content["count"]
    active = await call(servers, "tok-admin", "list_alarm_events", {"start": "-30d", "active_only": True, "limit": 50})
    assert all(e["active"] and e["end_ts"] is None for e in active.structured_content["events"])


async def test_point_map_and_energy(servers):
    res = await call(servers, TOKEN, "get_point_map", {})
    assert not res.is_error
    names = {p["lbnl_name"]: p for p in res.structured_content["points"]}
    assert names["CHL_SW_TEMP{n}"]["asset_pattern"] == "CH_0{n}" and names["CHL_SW_TEMP{n}"]["param_alias"] == "chwst"

    energy = await call(servers, TOKEN, "get_energy_summary", {"start": "-2d", "bucket": "day"})
    assert not energy.is_error, text_of(energy)
    data = energy.structured_content
    assert data["bucket"] == "day" and 2 <= len(data["periods"]) <= 3
    assert data["totals"].get("chillers_kwh", 0) > 0
    assert all(p["period_start"].endswith("+08:00") for p in data["periods"])
    bad = await call(servers, TOKEN, "get_energy_summary", {"bucket": "minute"})
    assert bad.is_error


async def test_permission_gated_tools(servers):
    for tool in ("list_faults", "list_anomalies"):
        res = await call(servers, "tok-nosim", tool, {})
        assert res.is_error and "needs aiinsights.read" in text_of(res)
    res = await call(servers, "tok-nosim", "list_simulation_jobs", {})
    assert res.is_error and "needs simulation.read" in text_of(res)
    ok = await call(servers, "tok-plant", "list_simulation_jobs", {"limit": 5})
    assert not ok.is_error and isinstance(ok.structured_content["jobs"], list)
    docs = await call(servers, TOKEN, "search_documents", {"query": "tower fouling", "filters": {"doc_type": "manual"}})
    assert docs.is_error and "Document search is unavailable" in text_of(docs)  # the test server has no Gemini key
    bad_filter = await call(servers, TOKEN, "search_documents", {"query": "x", "filters": {"sql": "1"}})
    assert bad_filter.is_error and "Unknown filter 'sql'" in text_of(bad_filter)
    faults = await call(servers, "tok-plant", "list_faults", {"start": "-30d"})
    assert not faults.is_error and isinstance(faults.structured_content["faults"], list)


RW_URL = "postgresql://afdd_ai:afdd-ai-dev@127.0.0.1:5432/afdd"


@pytest.fixture
def scoped_jobs() -> Iterator[dict[str, int]]:
    """Three finished-state jobs (never picked up by the simulator), removed afterwards."""
    prefix = f"mcp-scope-test-{uuid.uuid4().hex[:8]}"
    specs = {
        "plant": ({"duration_min": 60, "actions": [{"at_min": 0, "kind": "control", "asset": "CH_01", "param": "chwst_sp", "value": 7.0}]}, None),
        "tower": ({"duration_min": 60, "variants": [{"name": "baseline", "actions": []}, {"name": "x", "actions": [{"at_min": 0, "kind": "fault", "asset": "CT_01", "fault": "ct_fouling", "value": 0.8}]}]}, "owner"),
        "none": ({"duration_min": 60, "actions": []}, None),
    }
    with psycopg.connect(RW_URL, autocommit=True) as conn:
        owner = conn.execute("SELECT id FROM core.users WHERE NOT is_deleted ORDER BY id LIMIT 1").fetchone()[0]
        ids = {"owner": owner}
        try:
            for key, (spec, requested) in specs.items():
                ids[key] = conn.execute(
                    "INSERT INTO sim.jobs (kind, title, origin, status, spec, requested_by) VALUES ('scenario', %s, 'ui', 'REJECTED', %s, %s) RETURNING id",
                    (f"{prefix} {key}", Jsonb(spec), owner if requested else None),
                ).fetchone()[0]
            yield ids
        finally:
            conn.execute("DELETE FROM sim.jobs WHERE title LIKE %s", (f"{prefix} %",))


async def test_simulation_jobs_are_scope_filtered(servers, scoped_jobs):
    mine = {scoped_jobs[k] for k in ("plant", "tower", "none")}

    async def visible(token: str, headers: dict[str, str] | None = None) -> set[int]:
        async with mcp_session(servers["db"].url, token, headers=headers) as s:
            res = await s.call_tool("list_simulation_jobs", {"status": "REJECTED", "limit": 200})
        assert not res.is_error, text_of(res)
        return {j["id"] for j in res.structured_content["jobs"]} & mine

    # Plant-room user: the tower job touches CT_01 (out of scope) and is hidden ...
    assert await visible("tok-plant") == {scoped_jobs["plant"], scoped_jobs["none"]}
    # ... unless the user requested it (user id from the validated session) ...
    assert await visible("tok-owner") == mine
    # ... and a forged X-AFDD-User-Id header does not widen it.
    assert await visible("tok-plant", {"X-AFDD-User-Id": str(scoped_jobs["owner"])}) == {scoped_jobs["plant"], scoped_jobs["none"]}
    assert await visible("tok-admin") == mine
