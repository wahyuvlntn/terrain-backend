"""Document ingestion: PDF -> chunks -> embeddings -> rag.chunks (DESIGN 6.2).

Status machine of `rag.documents`: PENDING -> INDEXING -> READY | FAILED.
A worker claims one PENDING document at a time (FOR UPDATE SKIP LOCKED),
reads the original from the private bucket, extracts the page text (pypdf),
chunks it (heading-aware, ~700 tokens, 80 overlap), embeds the chunks with
gemini-embedding-001 (RETRIEVAL_DOCUMENT with the document title, 768 d,
L2-normalised, batches of 50) and replaces the document's chunks in one
transaction with the document's metadata denormalised onto every chunk.

* Unreadable / encrypted / image-only PDFs and missing objects -> FAILED with the reason.
* Daily budget reached, the document's estimated embedding cost not fitting
  into what is left of it (checked before the first and every further batch,
  `gemini.embed_texts`), or Gemini not configured -> the document goes back to
  PENDING (error holds the reason) and ingestion pauses (`Deferred`), so the
  library is indexed once the budget allows.
* A Gemini rate limit (429) still hit after the retries defers like the budget;
  documents an older build marked FAILED for it are re-queued.
* INDEXING rows left behind by a crash are re-queued after STALE_INDEXING.
* A document replaced or deleted while it was being embedded keeps its new
  state: the result is dropped (claim token = updated_at).
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
from datetime import timedelta
from typing import Any

import numpy as np

from ..common import db, gemini, storage
from ..common.config import get_settings
from .chunking import Chunk, chunk_pages, embed_input, extract_pages

log = logging.getLogger(__name__)

STALE_INDEXING = timedelta(minutes=15)
EMBED_BATCH = 50
MAX_CHUNKS = 2000


class Deferred(Exception):
    """Ingestion cannot proceed now (budget / configuration); the document stays PENDING."""


async def requeue_stale() -> int:
    return await db.execute(
        "UPDATE rag.documents SET status = 'PENDING', updated_at = now()"
        " WHERE (status = 'INDEXING' AND updated_at < now() - %s) OR (status = 'FAILED' AND error LIKE 'embedding failed: 429%%')",
        (STALE_INDEXING,),
    )


async def claim(document_id: int | None = None) -> dict[str, Any] | None:
    """Move one PENDING document (or the given one) to INDEXING."""
    return await db.fetch_one(
        """UPDATE rag.documents SET status = 'INDEXING', error = NULL, updated_at = now()
           WHERE id = (SELECT id FROM rag.documents WHERE status = 'PENDING' AND (%s::bigint IS NULL OR id = %s)
                       ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
           RETURNING id, title, doc_type, object_key, site_code, location_id, asset_id, unit, asset_type, model_id,
                     alarm_codes, doc_date, sha256, size_bytes, updated_at""",
        (document_id, document_id),
    )


async def _resolve_asset(doc: dict[str, Any]) -> dict[str, Any]:
    """Fill asset_id / location_id / asset_type from the unit alias when the upload only named the unit."""
    if not doc.get("unit") or (doc.get("asset_id") and doc.get("location_id") and doc.get("asset_type")):
        return doc
    row = await db.fetch_one(
        "SELECT id, location_id, type_name, model_id FROM analytics.v_assets WHERE lower(alias) = lower(%s)",
        (doc["unit"],),
    )
    if row:
        doc = {
            **doc,
            "asset_id": doc.get("asset_id") or row["id"],
            "location_id": doc.get("location_id") or row["location_id"],
            "asset_type": doc.get("asset_type") or row["type_name"],
            "model_id": doc.get("model_id") or row["model_id"],
        }
        await db.execute(
            "UPDATE rag.documents SET asset_id = %s, location_id = %s, asset_type = %s, model_id = %s WHERE id = %s",
            (doc["asset_id"], doc["location_id"], doc["asset_type"], doc["model_id"], doc["id"]),
        )
    return doc


async def _fail(doc_id: int, reason: str) -> None:
    await db.execute("UPDATE rag.documents SET status = 'FAILED', error = %s, updated_at = now() WHERE id = %s", (reason[:500], doc_id))
    log.warning("document %s failed: %s", doc_id, reason)


async def _defer(doc_id: int, reason: str) -> None:
    await db.execute("UPDATE rag.documents SET status = 'PENDING', error = %s, updated_at = now() WHERE id = %s", (reason[:500], doc_id))


async def embed_chunks(chunks: list[Chunk], title: str) -> list[np.ndarray]:
    texts = [embed_input(c) for c in chunks]
    vecs = await gemini.embed_texts(texts, task_type="RETRIEVAL_DOCUMENT", titles=[title] * len(texts), component="embeddings", batch_size=EMBED_BATCH)
    return [np.asarray(v, dtype=np.float32) for v in vecs]


async def ingest(doc: dict[str, Any]) -> int:
    """Index one claimed (INDEXING) document; returns its chunk count (0 = failed, -1 = superseded).

    Raises Deferred when embeddings cannot be computed now; other failures
    mark the document FAILED.
    """
    doc_id = int(doc["id"])
    try:
        blob = await storage.get_bytes(doc["object_key"])
    except storage.ObjectNotFound as err:
        await _fail(doc_id, f"original not found in the document store ({err.code})")
        return 0
    try:
        pages = await asyncio.to_thread(extract_pages, blob)
    except Exception as err:  # noqa: BLE001 - any parser error means an unreadable PDF
        await _fail(doc_id, f"not a readable PDF: {err}")
        return 0
    chunks = await asyncio.to_thread(chunk_pages, pages)
    if not chunks:
        await _fail(doc_id, "no extractable text (scanned PDF? OCR is not supported)")
        return 0
    if len(chunks) > MAX_CHUNKS:
        await _fail(doc_id, f"document too large ({len(chunks)} chunks, limit {MAX_CHUNKS})")
        return 0
    doc = await _resolve_asset(doc)
    try:
        vecs = await embed_chunks(chunks, doc["title"])
    except (gemini.BudgetExceeded, gemini.GeminiNotConfigured) as err:
        await _defer(doc_id, f"waiting to index: {err}")
        raise Deferred(str(err)) from err
    except Exception as err:  # noqa: BLE001 - recorded on the document
        if gemini.rate_limit_wait(err) is not None:
            await _defer(doc_id, f"waiting to index: Gemini rate limit ({str(err)[:200]})")
            raise Deferred("Gemini rate limit") from err
        await _fail(doc_id, f"embedding failed: {str(err)[:300]}")
        return 0
    sha = hashlib.sha256(blob).hexdigest()
    rows = [
        (
            doc_id, c.index, c.page, c.heading, c.content, c.token_count, v, doc["site_code"], doc.get("location_id"),
            doc.get("asset_id"), doc.get("unit"), doc.get("asset_type"), doc["doc_type"], doc.get("doc_date"),
            list(doc.get("alarm_codes") or []),
        )
        for c, v in zip(chunks, vecs, strict=True)
    ]  # fmt: skip
    pool = await db.get_pool()
    async with pool.connection() as conn:
        async with conn.transaction():
            # the document may have been replaced (corpus refresh, reindex) or deleted while it was embedded
            cur = await conn.execute(
                "SELECT 1 FROM rag.documents WHERE id = %s AND status = 'INDEXING' AND updated_at = %s FOR UPDATE",
                (doc_id, doc["updated_at"]),
            )
            if await cur.fetchone() is None:
                log.info("document %s changed while it was being indexed; result dropped", doc_id)
                return -1
            await conn.execute("DELETE FROM rag.chunks WHERE document_id = %s", (doc_id,))
            async with conn.cursor() as cur:
                await cur.executemany(
                    """INSERT INTO rag.chunks (document_id, chunk_index, page, heading, content, token_count, embedding,
                           site_code, location_id, asset_id, unit, asset_type, doc_type, doc_date, alarm_codes)
                       VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)""",
                    rows,
                )
            await conn.execute(
                """UPDATE rag.documents SET status = 'READY', error = NULL, pages = %s, chunks = %s, sha256 = %s,
                       size_bytes = %s, indexed_at = now(), updated_at = now() WHERE id = %s""",
                (len(pages), len(chunks), sha, len(blob), doc_id),
            )
    log.info("document %s indexed pages=%s chunks=%s title=%r", doc_id, len(pages), len(chunks), doc["title"])
    return len(chunks)


async def ingest_document(document_id: int) -> str:
    """Claim and index one specific PENDING document; returns its final status."""
    doc = await claim(document_id)
    if doc is None:
        return "NOT_PENDING"
    try:
        await ingest(doc)
    except Deferred:
        return "PENDING"
    except Exception as err:  # noqa: BLE001 - never leave a document INDEXING
        log.exception("ingestion failed document=%s", document_id)
        await _fail(int(doc["id"]), f"ingestion failed: {str(err)[:300]}")
    return str(await db.fetch_val("SELECT status FROM rag.documents WHERE id = %s", (document_id,)))


async def run(limit: int = 20, *, dry_run: bool = False) -> dict[str, Any]:
    """Index pending documents (oldest first) until none is left, the limit or a deferral.

    dry_run: count the documents per status, embed nothing.
    """
    if dry_run:
        rows = await db.fetch_all("SELECT status, count(*) AS n FROM rag.documents GROUP BY 1")
        return {"dry_run": True, "gemini_configured": get_settings().gemini_configured, **{r["status"].lower(): int(r["n"]) for r in rows}}
    await requeue_stale()
    if not get_settings().gemini_configured:
        pending = await db.fetch_val("SELECT count(*) FROM rag.documents WHERE status = 'PENDING'")
        return {"indexed": 0, "deferred": "Gemini is not configured", "pending": int(pending or 0)}
    await storage.ensure_bucket()
    out: dict[str, Any] = {"indexed": 0, "failed": 0}
    for _ in range(limit):
        doc = await claim()
        if doc is None:
            break
        try:
            n = await ingest(doc)
            key = "indexed" if n > 0 else "failed" if n == 0 else "superseded"
            out[key] = out.get(key, 0) + 1
        except Deferred as err:
            out["deferred"] = str(err)
            break
        except Exception as err:  # noqa: BLE001 - never leave a document INDEXING
            log.exception("ingestion failed document=%s", doc["id"])
            await _fail(int(doc["id"]), f"ingestion failed: {str(err)[:300]}")
            out["failed"] += 1
    return out
