"""Generated corpus against the live plant records: a few real documents are rendered
to a temp dir and to the private bucket, registered in rag.documents under a test
key prefix, checked, and deleted again (rows and objects)."""

from __future__ import annotations

import os
import uuid

import pytest

from afdd_ai.common import db, storage
from afdd_ai.rag import chunking, corpus

pytestmark = pytest.mark.usefixtures("live_store")
SITE = "SGF1"


@pytest.fixture
async def prefix():
    p = f"pytest-{uuid.uuid4().hex[:8]}:"
    yield p
    rows = await db.fetch_all("SELECT id FROM rag.documents WHERE metadata ->> 'generator_key' LIKE %s", (p + "%",))
    for r in rows:
        await corpus.delete_document(int(r["id"]))


async def test_corpus_documents_are_rendered_uploaded_and_registered(prefix, tmp_path):
    order = await db.fetch_one(
        "SELECT mo.id FROM dm.maintenance_orders mo JOIN analytics.v_assets v ON v.id = mo.asset_id"
        " WHERE mo.status_id IN (5, 6) AND v.site_code = %s ORDER BY mo.completed_at DESC NULLS LAST, mo.id DESC LIMIT 1",
        (SITE,),
    )
    if order is None:
        pytest.skip("no completed maintenance order in this database")
    out = str(tmp_path)
    res = {}
    for kind, kw in (("manual", {"limit": 1}), ("fdd_reference", {}), ("maintenance_report", {"order_ids": [order["id"]]}), ("setpoint_log", {"limit": 1})):
        res[kind] = await corpus.generate(SITE, kinds=[kind], out_dir=out, key_prefix=prefix, **kw)
        assert res[kind]["failed"] == 0 and res[kind]["removed"] == 0, (kind, res[kind])
    created = sum(r["created"] for r in res.values())
    assert created >= 3  # setpoint logs exist only when control requests were made
    files = sorted(os.listdir(out))
    assert len(files) == created and all(open(os.path.join(out, f), "rb").read(5) == b"%PDF-" for f in files)

    docs = {
        r["doc_type"]: r
        for r in await db.fetch_all(
            "SELECT id, title, doc_type, source, object_key, file_name, size_bytes, sha256, site_code, location_id, asset_id, unit, asset_type,"
            " model_id, alarm_codes, doc_date, status, metadata FROM rag.documents WHERE metadata ->> 'generator_key' LIKE %s",
            (prefix + "%",),
        )
    }
    manual = docs["manual"]
    assert manual["source"] == "generated" and manual["status"] == "PENDING" and manual["site_code"] == SITE
    assert manual["title"].startswith("[PYTEST-") and manual["title"].endswith("Technical manual")
    assert manual["asset_type"] and manual["model_id"] and manual["unit"] is None and manual["alarm_codes"]
    assert manual["metadata"]["generator_key"].startswith(prefix + "manual:SGF1:") and len(manual["metadata"]["source_hash"]) == 64
    fdd = docs["fdd_reference"]
    assert fdd["alarm_codes"] == ["CHL-011", "CT-013", "CT-014", "CHS-005", "CHS-006", "CHS-007", "CHS-008"]
    report = docs["maintenance_report"]
    assert report["asset_id"] and report["unit"] and report["asset_type"] and report["model_id"] and report["doc_date"]
    assert report["location_id"] and report["metadata"]["order_id"] == order["id"]

    # the stored original is the rendered file, in the private bucket
    blob = await storage.get_bytes(report["object_key"])
    assert blob[:5] == b"%PDF-" and len(blob) == report["size_bytes"]
    assert report["object_key"].startswith(prefix.strip(":") + "/generated/maintenance_report/sgf1/")

    # the text is extractable and chunks along the generator's numbered sections
    pages = chunking.extract_pages(await storage.get_bytes(manual["object_key"]))
    text = "\n".join(pages)
    assert "1 Overview" in text and "Alarm limits" in text and "Maintenance procedures" in text
    chunks = chunking.chunk_pages(pages)
    assert len(chunks) >= 2 and chunks[0].heading and chunks[0].heading.startswith("1 Overview")
    assert all(c.token_count <= chunking.TARGET_TOKENS + 60 for c in chunks)
    report_text = "\n".join(chunking.extract_pages(blob))
    assert "Order" in report_text and "Timeline" in report_text and "Work performed" in report_text

    # idempotent: unchanged sources are not re-rendered; force re-renders and re-queues
    again = await corpus.generate(SITE, kinds=["fdd_reference"], key_prefix=prefix)
    assert again["unchanged"] == 1 and again["created"] == 0 and again["updated"] == 0
    forced = await corpus.generate(SITE, kinds=["fdd_reference"], key_prefix=prefix, force=True)
    assert forced["updated"] == 1 and forced["ids"] == [fdd["id"]]


async def test_dry_run_writes_nothing(prefix, tmp_path):
    before = await db.fetch_val("SELECT count(*) FROM rag.documents")
    res = await corpus.generate(SITE, kinds=["fdd_reference", "manual"], key_prefix=prefix, limit=1, out_dir=str(tmp_path), dry_run=True)
    assert res["would_create"] == 2 and res["created"] == 0 and res["by_kind"]["fdd_reference"] == 1
    assert len(os.listdir(tmp_path)) == 2
    assert await db.fetch_val("SELECT count(*) FROM rag.documents") == before


def test_issue_relation_flow_in_words():
    flow = {
        "nodes": [
            {"id": "s1", "type": "source", "data": {"code": "CHL-002", "name": "High condenser pressure"}},
            {"id": "p1", "type": "parameter", "data": {"name": "Condenser Approach"}},
            {"id": "c1", "type": "compound"},
            {"id": "t1", "type": "target", "data": {"code": "THR-01", "name": "Condenser tube fouling"}},
        ],
        "edges": [{"source": "s1", "target": "c1"}, {"source": "p1", "target": "c1"}, {"source": "c1", "target": "t1"}],
    }
    targets, paths = corpus.render_flow(flow)
    assert targets == ["THR-01 Condenser tube fouling"]
    assert paths == ["alarm CHL-002 High condenser pressure is active AND Condenser Approach is abnormal"]


def test_source_hash_ignores_volatile_snapshots():
    spec = corpus.DocSpec("k", "maintenance_report", "T", "o.pdf", SITE, {"order": {"id": 1}, "kpi": {"mtbf_hours": 10}}, lambda s: b"", volatile=("kpi",))
    h = spec.source_hash()
    spec.source["kpi"] = {"mtbf_hours": 12}
    assert spec.source_hash() == h
    spec.source["order"] = {"id": 2}
    assert spec.source_hash() != h
