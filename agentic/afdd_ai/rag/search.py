"""Hybrid document search over `rag.chunks` (DESIGN 6.2).

`search()` embeds the query (gemini-embedding-001, RETRIEVAL_QUERY, 768 d),
takes the nearest chunks by cosine distance (HNSW, iterative scan so metadata
filters do not starve the result) plus the best full-text matches (all query
words, `websearch_to_tsquery`), and ranks them by

    score = cosine similarity + KEYWORD_WEIGHT x ts_rank_cd / best ts_rank_cd of the candidates

so exact tags and alarm codes ("CHL-011", "CT_02") surface even when their
embedding is vague, while a long natural-language question (whose words rarely
all occur in one chunk) is ranked by meaning alone.

Used by the analytics fault instructions, the agent and the MCP server, so it
is importable without side effects (no DB or Gemini access at import).

Filters (all optional):

* site_code: exact.
* asset_type, unit: the value or general documents (NULL) unless `strict`.
* doc_type: one type or a list.
* alarm_codes: chunks of documents that cover any of the codes.
* date_from / date_to: doc_date range (ISO date or date); documents without a
  date are excluded when a range is given.
* strict: exact asset_type / unit match only.

Searchable are the chunks of every document that has them except FAILED ones:
a document being re-indexed (PENDING / INDEXING) keeps its previous chunks
until the new ones replace them in one transaction, so it stays findable.

`allowed_location_ids` scopes results to a user's locations exactly like the
backend's document library (`docScopeClause`, internal/api/ai_documents.go):
the scope is the allowed locations and all their descendants, and a document
is visible when its location is NULL or in the scope, its equipment (if any)
is located in the scope, and its unit (if any) is the alias of equipment in
the scope. None = no location restriction (admins, system callers).
"""

from __future__ import annotations

import logging
from datetime import date, datetime
from typing import Any

import numpy as np

from ..common import db, gemini

log = logging.getLogger(__name__)

KEYWORD_WEIGHT = 0.2  # boost of the best full-text match (others in proportion to their rank)
CANDIDATES_MIN = 40


def _date(v: Any) -> date | None:
    if v is None or v == "":
        return None
    if isinstance(v, datetime):
        return v.date()
    if isinstance(v, date):
        return v
    return date.fromisoformat(str(v)[:10])


def _as_list(v: Any) -> list[str] | None:
    if v is None or v == "" or v == []:
        return None
    if isinstance(v, str):
        return [s.strip() for s in v.split(",") if s.strip()]
    return [str(s) for s in v]


def build_filters(filters: dict[str, Any] | None, allowed_location_ids: list[int] | None) -> tuple[str, list[Any]]:
    """SQL WHERE fragment (on alias c = rag.chunks) and its parameters."""
    f = filters or {}
    strict = bool(f.get("strict"))
    where = ["d.status <> 'FAILED'"]  # chunks exist (join); re-indexing documents keep their old ones
    params: list[Any] = []
    if f.get("site_code"):
        where.append("c.site_code = %s")
        params.append(f["site_code"])
    for key in ("asset_type", "unit"):
        val = f.get(key)
        if val:
            vals = _as_list(val)
            if key == "asset_type":
                # types are stored as model types ("Centrifugal Chiller"): match words case-insensitively
                cond = "EXISTS (SELECT 1 FROM unnest(%s::text[]) v WHERE lower(c.asset_type) LIKE '%%' || lower(v) || '%%')"
            else:
                cond = f"c.{key} = ANY(%s)"
            where.append(f"({cond}{'' if strict else f' OR c.{key} IS NULL'})")
            params.append(vals)
    doc_types = _as_list(f.get("doc_type"))
    if doc_types:
        where.append("c.doc_type = ANY(%s)")
        params.append(doc_types)
    codes = _as_list(f.get("alarm_codes"))
    if codes:
        where.append("c.alarm_codes && %s::text[]")
        params.append([c.upper() for c in codes])
    d_from, d_to = _date(f.get("date_from")), _date(f.get("date_to"))
    if d_from:
        where.append("c.doc_date >= %s")
        params.append(d_from)
    if d_to:
        where.append("c.doc_date <= %s")
        params.append(d_to)
    if allowed_location_ids is not None:
        where.append(SCOPE_WHERE)
    return " AND ".join(where), params


# The allowed locations and their descendants (the backend's auth.ScopeFor).
SCOPE_CTE = """scope AS (
    WITH RECURSIVE down AS (
        SELECT id FROM asset.locations WHERE id = ANY(%s)
        UNION SELECT l.id FROM asset.locations l JOIN down ON l.parent_id = down.id
    )
    SELECT id FROM down
)"""
# The backend's docScopeClause on the document (alias d), not on the chunks' copies of its metadata.
SCOPE_WHERE = """(d.location_id IS NULL OR d.location_id IN (SELECT id FROM scope))
    AND (d.asset_id IS NULL OR EXISTS (SELECT 1 FROM asset.equipment se WHERE se.id = d.asset_id AND se.location_id IN (SELECT id FROM scope)))
    AND (d.unit IS NULL OR lower(d.unit) IN (SELECT lower(se.alias) FROM asset.equipment se WHERE se.location_id IN (SELECT id FROM scope)))"""


async def search(
    query: str,
    k: int = 6,
    filters: dict[str, Any] | None = None,
    allowed_location_ids: list[int] | None = None,
    *,
    component: str = "rag_search",
    query_embedding: list[float] | None = None,
    ro: bool = False,
) -> list[dict[str, Any]]:
    """Top-k chunks for a query: [{chunk_id, document_id, title, doc_type, page, heading, content,
    score, unit, asset_type, doc_date}] ordered by score (best first).

    Raises gemini.BudgetExceeded / GeminiNotConfigured when the query cannot be embedded.
    `ro=True` uses the read-only pool (MCP server).
    """
    query = (query or "").strip()
    if not query:
        return []
    k = max(1, min(int(k), 50))
    vec = query_embedding
    if vec is None:
        vec = (await gemini.embed_texts([query], task_type="RETRIEVAL_QUERY", component=component))[0]
    where, wparams = build_filters(filters, allowed_location_ids)
    pool_n = max(CANDIDATES_MIN, 8 * k)
    scope_params: list[Any] = []
    with_sql = ""
    if allowed_location_ids is not None:
        ids = [int(i) for i in allowed_location_ids]
        with_sql = "WITH " + SCOPE_CTE + ", "
        scope_params = [ids]
    else:
        with_sql = "WITH "
    sql = f"""{with_sql}
q AS (SELECT %s::vector AS v, websearch_to_tsquery('english', %s) AS tq),
vec AS (
    SELECT c.id FROM rag.chunks c JOIN rag.documents d ON d.id = c.document_id
    WHERE {where}
    ORDER BY c.embedding <=> (SELECT v FROM q) LIMIT %s
),
kw AS (
    SELECT c.id FROM rag.chunks c JOIN rag.documents d ON d.id = c.document_id, q
    WHERE {where} AND c.tsv @@ q.tq
    ORDER BY ts_rank_cd(c.tsv, q.tq, 32) DESC LIMIT %s
),
cand AS (SELECT id FROM vec UNION SELECT id FROM kw)
SELECT c.id AS chunk_id, c.document_id, d.title, c.doc_type, c.page, c.heading, c.content, c.unit, c.asset_type,
       c.doc_date, 1 - (c.embedding <=> q.v) AS similarity, ts_rank_cd(c.tsv, q.tq, 32) AS keyword
FROM cand JOIN rag.chunks c ON c.id = cand.id JOIN rag.documents d ON d.id = c.document_id, q"""
    params = [*scope_params, np.asarray(vec, dtype=np.float32), query, *wparams, pool_n, *wparams, pool_n]
    pool = await db.get_pool(ro)
    async with pool.connection() as conn:
        async with conn.transaction():
            await conn.execute("SET LOCAL hnsw.ef_search = 100")
            await conn.execute("SET LOCAL hnsw.iterative_scan = relaxed_order")
            cur = await conn.execute(sql, params)
            rows = await cur.fetchall()
    best_kw = max((float(r["keyword"] or 0) for r in rows), default=0.0)
    out = []
    for r in rows:
        kw = float(r["keyword"] or 0) / best_kw if best_kw > 0 else 0.0
        score = float(r["similarity"] or 0) + KEYWORD_WEIGHT * kw
        out.append(
            {
                "chunk_id": r["chunk_id"],
                "document_id": r["document_id"],
                "title": r["title"],
                "doc_type": r["doc_type"],
                "page": r["page"],
                "heading": r["heading"],
                "content": r["content"],
                "score": round(score, 4),
                "unit": r["unit"],
                "asset_type": r["asset_type"],
                "doc_date": r["doc_date"].isoformat() if r["doc_date"] else None,
            }
        )
    out.sort(key=lambda x: x["score"], reverse=True)
    return out[:k]
