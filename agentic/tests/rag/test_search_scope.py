"""Document search scope equals the backend's document library scope (read-only on the live corpus).

The backend (`docScopeClause`, internal/api/ai_documents.go; scope = assigned
locations and their descendants, `auth.ScopeFor`) shows a document when its
location is NULL or in scope, its equipment is located in scope and its unit is
the alias of equipment in scope. Every search result must pass the same test,
so a cited document can always be opened. Only SELECTs are run (the query
vector is given, no Gemini call).
"""

from __future__ import annotations

from typing import Any

import numpy as np
import pytest

from afdd_ai.common import db
from afdd_ai.rag import search

pytestmark = pytest.mark.usefixtures("live_db")


def subtree(parents: dict[int, int | None], roots: list[int]) -> set[int]:
    out = set(roots)
    grew = True
    while grew:
        grew = False
        for loc, parent in parents.items():
            if parent in out and loc not in out:
                out.add(loc)
                grew = True
    return out


def backend_visible(doc: dict[str, Any], scope: set[int], equipment: list[dict[str, Any]]) -> bool:
    located = {e["id"]: e["location_id"] for e in equipment}
    aliases = {e["alias"].lower() for e in equipment if e["location_id"] in scope}
    return (
        (doc["location_id"] is None or doc["location_id"] in scope)
        and (doc["asset_id"] is None or located.get(doc["asset_id"]) in scope)
        and (doc["unit"] is None or doc["unit"].lower() in aliases)
    )


def query_vector(seed: int) -> list[float]:
    v = np.random.default_rng(seed).normal(size=768)
    return list(v / np.linalg.norm(v))


async def test_search_scope_matches_the_backend_document_scope():
    docs = {r["id"]: r for r in await db.fetch_all("SELECT id, location_id, asset_id, unit, doc_type, site_code FROM rag.documents WHERE status <> 'FAILED'")}
    if not docs or not await db.fetch_val("SELECT count(*) FROM rag.chunks"):
        pytest.skip("no indexed documents in this database")
    locs = {r["name"]: r["id"] for r in await db.fetch_all("SELECT id, name FROM asset.locations")}
    parents = {r["id"]: r["parent_id"] for r in await db.fetch_all("SELECT id, parent_id FROM asset.locations")}
    equipment = await db.fetch_all("SELECT id, alias, location_id FROM asset.equipment")
    site = next(iter(docs.values()))["site_code"]
    room, building = locs.get("Chiller Plant Room"), locs.get("CUB Central Utility Building")
    if room is None or building is None:
        pytest.skip("demo locations not present")
    for i, allowed in enumerate(([room], [building], [locs.get("Cooling Tower Yard", room)], [])):
        scope = subtree(parents, allowed)
        for q in ("chiller maintenance procedure", "cooling tower fan gearbox"):
            res = await search.search(q, k=50, filters={"site_code": site}, allowed_location_ids=allowed, query_embedding=query_vector(i))
            for r in res:
                assert backend_visible(docs[r["document_id"]], scope, equipment), (allowed, r["document_id"], r["title"])
    # documents at building level (the generated manuals) are not in a room-level user's scope (no ancestors)
    building_docs = {d for d, row in docs.items() if row["location_id"] == building and row["asset_id"] is None and row["unit"] is None}
    if building_docs:
        # query with a building-level document's own title, so the lexical half matches it whatever its type
        title = await db.fetch_val("SELECT title FROM rag.documents WHERE id = %s", (min(building_docs),))
        for_room = await search.search(title, k=50, filters={"site_code": site}, allowed_location_ids=[room], query_embedding=query_vector(7))
        for_building = await search.search(title, k=50, filters={"site_code": site}, allowed_location_ids=[building], query_embedding=query_vector(7))
        assert not {r["document_id"] for r in for_room} & building_docs
        assert {r["document_id"] for r in for_building} & building_docs


def test_reindexing_documents_stay_searchable_and_failed_ones_do_not():
    where, _ = search.build_filters({"site_code": "SGF1"}, None)
    assert "d.status <> 'FAILED'" in where and "READY" not in where
