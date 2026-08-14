"""`search_documents`: RAG retrieval over the plant document library, with citations.

Retrieval (`afdd_ai.rag.search.search`, DESIGN 6.2) embeds the query and runs a
hybrid vector + keyword search over `rag.chunks`, restricted to the user's
location scope. The tool returns results with a `ref` number; the agent cites
them as [ref] and the SSE layer turns the same results into a `citations` event
(ref, document id, title, type, page, snippet, score) for the PDF viewer.

Refs are numbered per answer (one ADK invocation), across every search and
relayed fault instruction in it: a later search continues the numbering and a
passage cited before keeps its number, so the markers in the text always match
the source chips. They live in `temp:` state, which ADK drops after the turn.
"""

from __future__ import annotations

import inspect
from datetime import date
import json
import logging
import re
from collections.abc import Mapping, MutableMapping
from typing import Any

from google.adk.tools import ToolContext

from ..common.config import get_settings
from ..common.gemini import BudgetExceeded, GeminiNotConfigured
from .backend_api import Api, ToolError, cache_get, cache_put, error_result, rnd

log = logging.getLogger(__name__)

DOC_TYPES = ("manual", "sop", "maintenance_report", "setpoint_log", "fdd_reference", "other")
SCOPE_CACHE_S = 60.0
MAX_CONTENT = 1400
SNIPPET = 240
REFS_KEY = "temp:citation_refs"


def source_key(r: Mapping[str, Any]) -> str:
    """Identity of a cited passage: its chunk, else document and page."""
    if r.get("chunk_id") is not None:
        return f"chunk:{r.get('chunk_id')}"
    return f"doc:{r.get('document_id')}#{r.get('page') or ''}"


def citation_ref(state: MutableMapping[str, Any], key: str) -> int:
    """The answer-wide reference number of a passage (assigned on first use)."""
    refs = dict(state.get(REFS_KEY) or {})
    if key not in refs:
        refs[key] = len(refs) + 1
        state[REFS_KEY] = refs  # assign a new dict so the state delta records it
    return int(refs[key])


_MARKER = re.compile(r"\[(\d+(?:\s*[,\u2013-]\s*\d+)*)\]")


def _marker_numbers(text: str) -> list[int]:
    """[1, 3-5] -> [1, 3, 4, 5]; [] when the marker is not a plain list or a short range."""
    out: list[int] = []
    for part in re.split(r"\s*,\s*", text.strip()):
        rng = re.fullmatch(r"(\d+)\s*[\u2013-]\s*(\d+)", part)
        if rng:
            lo, hi = int(rng.group(1)), int(rng.group(2))
            if not 0 <= hi - lo <= 20:
                return []
            out += list(range(lo, hi + 1))
        elif part.isdigit():
            out.append(int(part))
        else:
            return []
    return out


def renumber_instruction(state: MutableMapping[str, Any], data: Any) -> Any:
    """Give a stored fault instruction's sources answer-wide refs and rewrite its
    [n] markers to them (ai_insights/getfault relayed through call_app_api)."""
    if not isinstance(data, dict):
        return data
    sources = data.get("instruction_sources")
    if isinstance(sources, str):
        try:
            sources = json.loads(sources)
        except ValueError:
            return data
    if not isinstance(sources, list):
        return data
    rows = sorted((dict(s) for s in sources if isinstance(s, Mapping)), key=lambda s: s.get("n") or 0)
    mapping: dict[int, int] = {}
    for s in rows:
        s["ref"] = citation_ref(state, source_key(s))
        if isinstance(s.get("n"), int):
            mapping[s["n"]] = s["ref"]
    md = data.get("instruction_md")
    if isinstance(md, str) and mapping:

        def repl(m: re.Match[str]) -> str:
            nums = _marker_numbers(m.group(1))
            if not nums or any(n not in mapping for n in nums):
                return m.group(0)
            return "[" + ", ".join(str(mapping[n]) for n in dict.fromkeys(nums)) + "]"

        md = _MARKER.sub(repl, md)
    return {**data, "instruction_sources": rows, "instruction_md": md}


async def allowed_location_ids(api: Api) -> list[int] | None:
    """The user's visible locations (descendants included); None = unrestricted (admin)."""
    cached = cache_get(api.token, "scope")
    if cached is not None:
        return None if cached == "all" else list(cached)
    details = await api.call("user/getuserdetails", {}, what="the user's location access") or {}
    if details.get("role") in ("admin", "superuser"):
        cache_put(api.token, "scope", "all", SCOPE_CACHE_S)
        return None
    ids = sorted({int(x) for x in (details.get("assignedLocations") or []) if str(x).lstrip("-").isdigit()})
    cache_put(api.token, "scope", tuple(ids), SCOPE_CACHE_S)
    return ids


def citation_items(results: list[Mapping[str, Any]]) -> list[dict[str, Any]]:
    """`citations` SSE items from search results."""
    items = []
    for r in results:
        if not isinstance(r, Mapping) or r.get("document_id") is None:
            continue
        content = str(r.get("content") or "")
        items.append(
            {
                "ref": r.get("ref"),
                "document_id": r.get("document_id"),
                "title": r.get("title"),
                "doc_type": r.get("doc_type"),
                "page": r.get("page"),
                "snippet": content[:SNIPPET] + ("…" if len(content) > SNIPPET else ""),
                "score": rnd(r.get("score"), 3),
            }
        )
    return items


async def search_documents(
    tool_context: ToolContext,
    query: str,
    doc_type: str = "",
    asset_type: str = "",
    unit: str = "",
    alarm_code: str = "",
    date_from: str = "",
    date_to: str = "",
    k: int = 6,
) -> dict[str, Any]:
    """Search the plant document library (equipment manuals, SOPs, maintenance reports of completed orders,
    set-point change logs, the LBNL fault-detection reference). Use it for procedures, design data, alarm
    meanings, past repairs and history; cite the results as [n] with title and page.

    Args:
      query: What to look for, in plain words (e.g. "cooling tower fouling corrective action").
      doc_type: Optional: manual, sop, maintenance_report, setpoint_log, fdd_reference, other.
      asset_type: Optional equipment type, e.g. "Chiller", "Cooling Tower".
      unit: Optional equipment alias, e.g. "CT_02" (documents of that unit or of all units of its type).
      alarm_code: Optional alarm / rule code, e.g. "CT-014".
      date_from: Optional ISO date (YYYY-MM-DD): only documents dated on or after it (maintenance reports,
        set-point logs; undated documents such as manuals are then left out).
      date_to: Optional ISO date (YYYY-MM-DD): only documents dated on or before it.
      k: Number of passages (1-10, default 6).
    """
    try:
        q = (query or "").strip()
        if not q:
            raise ToolError("Give a search query.")
        if len(q) > 500:
            q = q[:500]
        dt = doc_type.strip().lower()
        if dt and dt not in DOC_TYPES:
            raise ToolError(f"doc_type must be one of {', '.join(DOC_TYPES)}.")
        api = Api(tool_context.state)
        if not (api.can("aiassistant.read") or api.can("aiinsights.read")):
            raise ToolError("The user is not allowed to read the document library.")
        filters: dict[str, Any] = {"site_code": get_settings().site_code}
        if dt:
            filters["doc_type"] = dt
        if asset_type.strip():
            filters["asset_type"] = asset_type.strip()
        if unit.strip():
            filters["unit"] = unit.strip().upper()
        if alarm_code.strip():
            filters["alarm_codes"] = [alarm_code.strip().upper()]
        for key, value in (("date_from", date_from), ("date_to", date_to)):
            value = (value or "").strip()[:10]
            if value:
                try:
                    date.fromisoformat(value)
                except ValueError:
                    raise ToolError(f"{key} must be an ISO date (YYYY-MM-DD).") from None
                filters[key] = value
        try:
            kk = max(1, min(int(k), 10))
        except (TypeError, ValueError):
            kk = 6
        locations = await allowed_location_ids(api)
        try:
            from ..rag.search import search as rag_search
        except ImportError:
            log.error("afdd_ai.rag.search is not available")
            raise ToolError("Document search is not available on this server yet.") from None
        try:
            found = rag_search(q, k=kk, filters=filters, allowed_location_ids=locations, component="agent_rag_search")
            if inspect.isawaitable(found):
                found = await found
        except (BudgetExceeded, GeminiNotConfigured) as err:
            raise ToolError(f"Document search is unavailable: {err}") from None
        results = []
        for r in found or []:
            if not isinstance(r, Mapping):
                continue
            content = str(r.get("content") or "")
            results.append(
                {
                    "ref": citation_ref(tool_context.state, source_key(r)),
                    "document_id": r.get("document_id"),
                    "chunk_id": r.get("chunk_id"),
                    "title": r.get("title"),
                    "doc_type": r.get("doc_type"),
                    "page": r.get("page"),
                    "heading": r.get("heading"),
                    "unit": r.get("unit"),
                    "asset_type": r.get("asset_type"),
                    "doc_date": str(r["doc_date"]) if r.get("doc_date") is not None else None,
                    "score": rnd(r.get("score"), 3),
                    "content": content[:MAX_CONTENT] + ("…" if len(content) > MAX_CONTENT else ""),
                }
            )
        out: dict[str, Any] = {"query": q, "count": len(results), "results": results}
        if not results:
            out["note"] = "No matching documents. Say that the library has nothing on this; do not answer from memory as if it were documented."
        return out
    except Exception as err:  # noqa: BLE001
        return error_result(err)
