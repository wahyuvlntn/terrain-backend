"""Ingestion and hybrid search end to end against the host database and MinIO.

By default the embeddings are a deterministic hashed bag of words (no Gemini call):
the pipeline (PDF -> chunks -> vectors -> rag.chunks -> HNSW + keyword search,
filters, location scope, status machine) is exercised exactly as in production.

`AFDD_TEST_REAL_EMBEDDINGS=1` runs the same document through gemini-embedding-001
(about 5 chunks and 2 queries; the key comes from backend/.env). Its usage stays
in analytics.llm_usage (real spend, counted against the daily budget).
"""

from __future__ import annotations

import hashlib
import math
import os
import re
import uuid
from datetime import date, datetime, timezone

import pytest

from afdd_ai.common import db, gemini, storage
from afdd_ai.rag import chunking, ingest, search
from afdd_ai.rag.pdf import DocPDF

from .conftest import use_stack_credentials

pytestmark = pytest.mark.usefixtures("live_store")
SITE = "SGF1"
TITLE = "PYTEST plant service notes"

SECTIONS = {
    "Condenser 3-way valve leakage": (
        "Fault CHS-006: condenser water bypasses the cooling towers through the 3-way valve while it sits at its minimum position. "
        "The mixing fraction (CW supply minus tower leaving water, divided by CW return minus tower leaving water) stays above 0.08. "
        "Repair: isolate the valve, lap or replace the seat and the plug, check the actuator stroke and the positioner calibration, "
        "then verify that the mixing fraction falls below 0.02 with the valve at its minimum position. "
    ),
    "Cooling tower fan gearbox": (
        "Gear reducer service of the cooling tower fan: drain and refill the gearbox oil every 6 months, inspect the oil for metal particles, "
        "check the fan blade pitch and the drive shaft couplings, and measure the gearbox vibration at 100 % fan speed. "
        "A gearbox vibration above 7 mm/s needs a bearing inspection before the tower returns to service. "
    ),
    "Chiller oil system": (
        "Centrifugal chiller lubrication: keep the oil sump heater energised while the compressor is stopped, check the oil pressure "
        "differential at start-up and replace the oil filter when its differential pressure exceeds 1 bar. "
        "Low oil pressure differential trips the compressor to protect the bearings. "
    ),
}


def build_pdf() -> bytes:
    pdf = DocPDF(TITLE, "Unit tests of the RAG pipeline", site=SITE)
    for name, text in SECTIONS.items():
        pdf.h1(name)
        pdf.para(text * 3)
    return pdf.render()


def fake_vector(text: str, dim: int = 768) -> list[float]:
    """Hashed bag of words, L2-normalised (cosine = word overlap)."""
    v = [0.0] * dim
    for w in re.findall(r"[a-z0-9]+", text.lower()):
        if len(w) > 2:
            v[int(hashlib.md5(w.encode()).hexdigest(), 16) % dim] += 1.0
    n = math.sqrt(sum(x * x for x in v)) or 1.0
    return [x / n for x in v]


@pytest.fixture
def fake_embeddings(monkeypatch):
    calls: list[tuple[str, int]] = []

    async def embed_texts(texts, *, task_type="RETRIEVAL_DOCUMENT", titles=None, component="embeddings", batch_size=50):  # noqa: ANN001
        calls.append((task_type, len(texts)))
        return [fake_vector(t) for t in texts]

    monkeypatch.setattr(gemini, "embed_texts", embed_texts)
    return calls


@pytest.fixture
async def document():
    """A PENDING uploaded document (original in the private bucket); removed with its chunks afterwards."""
    key = f"pytest/{uuid.uuid4().hex}.pdf"
    blob = build_pdf()
    await storage.put_bytes(key, blob)
    loc = await db.fetch_val("SELECT id FROM asset.locations WHERE name = 'Chiller Plant Room'")
    row = await db.fetch_one(
        "INSERT INTO rag.documents (title, doc_type, source, object_key, file_name, size_bytes, site_code, location_id, unit, alarm_codes, doc_date, status)"
        " VALUES (%s, 'manual', 'upload', %s, 'notes.pdf', %s, %s, %s, 'CHWS_01', ARRAY['CHS-006'], '2026-09-01', 'PENDING') RETURNING id",
        (TITLE, key, len(blob), SITE, loc),
    )
    doc = {"id": int(row["id"]), "key": key, "location_id": loc}
    yield doc
    await db.execute("DELETE FROM rag.documents WHERE id = %s", (doc["id"],))
    await storage.delete(key)


def mine(results: list[dict], doc_id: int) -> list[dict]:
    return [r for r in results if r["document_id"] == doc_id]


async def test_ingest_and_search(document, fake_embeddings):
    doc_id = document["id"]
    assert await ingest.ingest_document(doc_id) == "READY"
    d = await db.fetch_one("SELECT status, error, pages, chunks, sha256, indexed_at, asset_id, asset_type, model_id FROM rag.documents WHERE id = %s", (doc_id,))
    assert d["status"] == "READY" and d["error"] is None and d["pages"] >= 1 and 3 <= d["chunks"] <= 8 and d["indexed_at"]
    assert d["asset_type"] == "Chilled Water System" and d["asset_id"] and d["model_id"]  # resolved from the unit alias
    assert fake_embeddings[0] == ("RETRIEVAL_DOCUMENT", d["chunks"])
    chunks = await db.fetch_all("SELECT chunk_index, page, heading, token_count, site_code, unit, asset_type, doc_type, doc_date, alarm_codes, location_id FROM rag.chunks WHERE document_id = %s ORDER BY chunk_index", (doc_id,))  # noqa: E501
    assert [c["chunk_index"] for c in chunks] == list(range(d["chunks"]))
    assert all(c["site_code"] == SITE and c["unit"] == "CHWS_01" and c["asset_type"] == "Chilled Water System" and c["doc_type"] == "manual" for c in chunks)
    assert all(c["alarm_codes"] == ["CHS-006"] and c["doc_date"] == date(2026, 9, 1) and c["location_id"] == document["location_id"] for c in chunks)
    assert {c["heading"].split(" > ")[0] for c in chunks if c["heading"]} == {"1 Condenser 3-way valve leakage", "2 Cooling tower fan gearbox", "3 Chiller oil system"}

    res = await search.search("3-way valve leakage repair seat actuator", k=3, filters={"site_code": SITE})
    assert res and res[0]["document_id"] == doc_id and res[0]["heading"].startswith("1 Condenser 3-way valve")
    assert set(res[0]) == {"chunk_id", "document_id", "title", "doc_type", "page", "heading", "content", "score", "unit", "asset_type", "doc_date"}
    assert res[0]["doc_date"] == "2026-09-01" and res[0]["title"] == TITLE
    assert [r["score"] for r in res] == sorted((r["score"] for r in res), reverse=True)
    gear = await search.search("cooling tower fan gearbox oil vibration", k=2, filters={"site_code": SITE})
    assert gear[0]["heading"].startswith("2 Cooling tower fan gearbox")

    # keyword boost: the exact alarm code lifts its chunk
    exact = await search.search("CHS-006", k=2, filters={"site_code": SITE})
    assert exact[0]["heading"].startswith("1 Condenser")

    # metadata filters
    q = "oil pressure differential filter"
    assert mine(await search.search(q, filters={"site_code": SITE, "alarm_codes": ["CHS-006"]}), doc_id)
    assert not mine(await search.search(q, filters={"site_code": SITE, "alarm_codes": "CHL-011"}), doc_id)
    assert mine(await search.search(q, filters={"unit": "CHWS_01", "doc_type": ["manual", "sop"]}), doc_id)
    assert not mine(await search.search(q, filters={"unit": "CH_02", "strict": True}), doc_id)
    assert not mine(await search.search(q, filters={"asset_type": "Cooling Tower"}), doc_id)
    assert mine(await search.search(q, filters={"date_from": "2026-08-01", "date_to": datetime(2026, 12, 31, tzinfo=timezone.utc)}), doc_id)
    assert not mine(await search.search(q, filters={"date_from": "2026-10-01"}), doc_id)
    assert not mine(await search.search(q, filters={"site_code": "XX01"}), doc_id)

    # location scope (the backend's): own room, its building (the room is a descendant) and none -> visible;
    # other buildings / rooms -> hidden
    ids = {r["name"]: r["id"] for r in await db.fetch_all("SELECT id, name FROM asset.locations")}
    for allowed, visible in (
        ([ids["Chiller Plant Room"]], True),
        ([ids["CUB Central Utility Building"]], True),
        (None, True),
        ([ids["FAB1 Wafer Fab"]], False),
        ([ids["Cooling Tower Yard"]], False),
        ([], False),
    ):
        assert bool(mine(await search.search(q, filters={"site_code": SITE}, allowed_location_ids=allowed), doc_id)) is visible, allowed
    assert await search.search("   ") == []

    # a document being re-indexed keeps its old chunks searchable until they are replaced; a failed one is hidden
    for status, visible in (("PENDING", True), ("INDEXING", True), ("FAILED", False)):
        await db.execute("UPDATE rag.documents SET status = %s WHERE id = %s", (status, doc_id))
        assert bool(mine(await search.search(q, filters={"site_code": SITE}), doc_id)) is visible, status

    # reindex replaces the chunks
    old_ids = {r["id"] for r in await db.fetch_all("SELECT id FROM rag.chunks WHERE document_id = %s", (doc_id,))}
    await db.execute("UPDATE rag.documents SET status = 'PENDING' WHERE id = %s", (doc_id,))
    assert await ingest.ingest_document(doc_id) == "READY"
    new_ids = {r["id"] for r in await db.fetch_all("SELECT id FROM rag.chunks WHERE document_id = %s", (doc_id,))}
    assert len(new_ids) == len(old_ids) and not new_ids & old_ids


async def test_document_replaced_while_indexing_keeps_its_new_state(document, fake_embeddings):
    doc = await ingest.claim(document["id"])
    assert doc is not None
    await db.execute("UPDATE rag.documents SET status = 'PENDING', updated_at = now() + interval '1 second' WHERE id = %s", (document["id"],))
    assert await ingest.ingest(doc) == -1
    row = await db.fetch_one("SELECT status, chunks FROM rag.documents WHERE id = %s", (document["id"],))
    assert row["status"] == "PENDING" and row["chunks"] is None
    assert await db.fetch_val("SELECT count(*) FROM rag.chunks WHERE document_id = %s", (document["id"],)) == 0


async def test_unreadable_pdf_fails_with_the_reason(document, fake_embeddings):
    await storage.put_bytes(document["key"], b"not a pdf at all")
    assert await ingest.ingest_document(document["id"]) == "FAILED"
    err = await db.fetch_val("SELECT error FROM rag.documents WHERE id = %s", (document["id"],))
    assert err.startswith("not a readable PDF")
    assert fake_embeddings == []


async def test_missing_original_fails(document, fake_embeddings):
    await storage.delete(document["key"])
    assert await ingest.ingest_document(document["id"]) == "FAILED"
    assert "not found" in await db.fetch_val("SELECT error FROM rag.documents WHERE id = %s", (document["id"],))


async def test_budget_defers_ingestion(document, monkeypatch):
    async def over_budget(*a, **kw):  # noqa: ANN002, ANN003
        raise gemini.BudgetExceeded("Daily AI budget reached (US$0.500 of US$0.50)")

    monkeypatch.setattr(gemini, "embed_texts", over_budget)
    assert await ingest.ingest_document(document["id"]) == "PENDING"
    row = await db.fetch_one("SELECT status, error FROM rag.documents WHERE id = %s", (document["id"],))
    assert row["status"] == "PENDING" and "budget" in row["error"]


async def test_ingest_dry_run_counts(document):
    out = await ingest.run(dry_run=True)
    assert out["dry_run"] is True and out.get("pending", 0) >= 1


@pytest.mark.skipif(os.environ.get("AFDD_TEST_REAL_EMBEDDINGS") != "1", reason="set AFDD_TEST_REAL_EMBEDDINGS=1 to spend real embeddings (about 7 calls)")
async def test_real_embeddings_end_to_end(document):
    use_stack_credentials("GOOGLE_API_KEY")
    gemini.client.cache_clear()
    if not gemini.get_settings().gemini_configured:
        pytest.skip("no GOOGLE_API_KEY in backend/.env")
    started = datetime.now(timezone.utc)
    pages = chunking.extract_pages(build_pdf())
    n_chunks = len(chunking.chunk_pages(pages))
    assert n_chunks <= 8  # the task allows at most 20 embedded chunks in total
    assert await ingest.ingest_document(document["id"]) == "READY"
    v = await db.fetch_one("SELECT vector_dims(embedding) AS d, vector_norm(embedding) AS n FROM rag.chunks WHERE document_id = %s LIMIT 1", (document["id"],))
    assert v["d"] == 768 and abs(v["n"] - 1.0) < 1e-3
    valve = await search.search("How do I repair a leaking condenser bypass valve?", k=3, filters={"site_code": SITE}, component="pytest_rag_search")
    assert mine(valve, document["id"]) and valve[0]["heading"].startswith("1 Condenser 3-way valve")
    gear = await search.search("tower gear reducer oil change and vibration limit", k=3, filters={"site_code": SITE}, component="pytest_rag_search")
    assert gear[0]["heading"].startswith("2 Cooling tower fan gearbox")
    usage = await db.fetch_all(
        "SELECT id, component, model, input_tokens, cost_usd FROM analytics.llm_usage WHERE at >= %s AND component IN ('embeddings', 'pytest_rag_search') ORDER BY id",
        (started,),
    )
    assert usage and sum(u["input_tokens"] for u in usage) > 0
    print("embedding usage rows:", [(u["id"], u["component"], u["model"], u["input_tokens"], float(u["cost_usd"])) for u in usage])
