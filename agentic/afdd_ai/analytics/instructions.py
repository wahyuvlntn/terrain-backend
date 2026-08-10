"""RAG-grounded maintenance instructions for new faults (DESIGN 6.1).

For every fault with `instruction_status = 'PENDING'`: retrieve document
chunks (query = rule name + asset model + evidence; filters = the asset type
and the rule's alarm code, widened to the asset type alone when too little
matches; only documents every user who can see the fault may open: no
location, or within the subtree of the equipment's location), then Gemini
Flash writes a Markdown maintenance instruction
(safety, diagnosis steps, corrective actions, verification) **only from the
numbered sources**, citing them as [n].

Grounding rules enforced here, not only in the prompt:

* nothing indexed for the site yet -> the fault waits (PENDING, no Gemini call)
  for up to 24 h, then SKIPPED;
* no retrieved source -> SKIPPED (never write an instruction without sources);
* citations outside 1..n are removed from the text; an answer without any
  valid citation is stored as FAILED rather than shown as grounded;
* `instruction_sources` lists exactly the chunks given to the model, in [n] order,
  with `cited` flags.

On BudgetExceeded / GeminiNotConfigured or a transient Gemini error the fault
stays PENDING and is retried (the worker backs off 15 min) for up to
RETRY_FOR; only then is it set SKIPPED with the
reason in `instruction_md`.
"""

from __future__ import annotations

import logging
import re
from datetime import timedelta
from typing import Any

from ..common import db, gemini
from ..common.config import get_settings
from ..rag import search as rag_search
from . import data, rules

log = logging.getLogger(__name__)

MAX_SOURCES = 6
MIN_SOURCES = 3
SNIPPET_CHARS = 1600
BATCH = 5
WAIT_FOR_LIBRARY = timedelta(hours=24)
RETRY_FOR = timedelta(hours=48)  # budget / transient failures: keep retrying this long
TRANSIENT_CODES = {408, 429, 500, 502, 503, 504}


def transient(err: BaseException) -> bool:
    """A Gemini / network failure worth retrying later."""
    if isinstance(err, (gemini.BudgetExceeded, gemini.GeminiNotConfigured, TimeoutError)):
        return True
    try:
        from google.genai import errors as genai_errors

        if isinstance(err, genai_errors.APIError) and getattr(err, "code", None) in TRANSIENT_CODES:
            return True
    except ImportError:  # pragma: no cover
        pass
    import httpx

    return isinstance(err, httpx.TransportError)

SYSTEM = """You are a senior maintenance engineer for a semiconductor fab's central utility plant (chillers, cooling towers, pumps, plant controls).
Write a maintenance instruction for the detected fault using ONLY the numbered sources provided.
Rules:
- Every procedural statement must be supported by a source and cite it as [n] (one or more), e.g. "Isolate the tower fan [2]."
- Never invent procedures, part numbers, values, set points or safety steps that are not in the sources. If the sources do not cover something, say so briefly (e.g. "The sources do not give a torque value.").
- Use Markdown with exactly these sections in this order: "### Safety", "### Diagnosis", "### Corrective actions", "### Verification". Use numbered steps.
- Be concise and specific to the equipment and the evidence. No preamble, no closing remarks, no source list at the end (the application shows the sources)."""


def build_query(rule: rules.RuleDef | None, fault: dict[str, Any], asset: dict[str, Any]) -> str:
    ev = fault.get("evidence") or {}
    parts = [rule.name if rule else fault["rule_code"], fault["rule_code"], asset.get("model_name") or "", asset.get("type_name") or ""]
    if rule:
        parts.append(rule.lbnl_fault)
    keys = [k for k in ev if k not in ("window", "rule_version", "thresholds") and not isinstance(ev[k], dict)]
    parts.append(" ".join(k.replace("_", " ") for k in keys[:6]))
    parts.append("diagnosis repair procedure")
    return " ".join(p for p in parts if p)


def source_scope(asset: dict[str, Any]) -> list[int]:
    """Locations whose documents may source a fault's instruction: the subtree of
    the fault equipment's location (none when it has no location). Every user
    who can see the fault has that subtree in scope, so they can open each
    cited document; documents without a location are always eligible."""
    return [int(asset["location_id"])] if asset.get("location_id") is not None else []


async def retrieve(fault: dict[str, Any], asset: dict[str, Any]) -> list[dict[str, Any]]:
    """Chunks for a fault: by alarm code and asset type first, then by asset type only."""
    rule = rules.RULES_BY_CODE.get(fault["rule_code"])
    query = build_query(rule, fault, asset)
    site = fault.get("site_code") or get_settings().site_code
    found: dict[int, dict[str, Any]] = {}
    scope = source_scope(asset)
    vec = (await gemini.embed_texts([query], task_type="RETRIEVAL_QUERY", component="instructions"))[0]
    for filters in (
        {"site_code": site, "asset_type": asset.get("type_name"), "alarm_codes": [fault["rule_code"]]},
        {"site_code": site, "asset_type": asset.get("type_name")},
    ):
        for r in await rag_search.search(query, k=MAX_SOURCES, filters=filters, allowed_location_ids=scope, query_embedding=vec):
            found.setdefault(r["chunk_id"], r)
        if len(found) >= MIN_SOURCES:
            break
    return sorted(found.values(), key=lambda r: r["score"], reverse=True)[:MAX_SOURCES]


def build_prompt(fault: dict[str, Any], asset: dict[str, Any], sources: list[dict[str, Any]]) -> str:
    rule = rules.RULES_BY_CODE.get(fault["rule_code"])
    ev = {k: v for k, v in (fault.get("evidence") or {}).items() if k not in ("window", "rule_version")}
    lines = [
        f"Fault: {fault['rule_code']} {rule.name if rule else ''}".strip(),
        f"LBNL fault type: {rule.lbnl_fault}" if rule else "",
        f"Equipment: {asset.get('alias')} {asset.get('name')} ({asset.get('model_name')}, {asset.get('type_name')})",
        f"Location: {asset.get('location_path') or 'n/a'}",
        f"Detected: {fault['started_at']:%Y-%m-%d %H:%M} UTC; severity {fault['severity']}",
        f"Detector message: {fault['message']}",
        f"Estimated intensity: {fault.get('intensity_estimate')} ({rule.intensity if rule else ''})",
        f"Evidence: {data.dumps(ev)}",
        "",
        "Sources:",
    ]
    for i, s in enumerate(sources, 1):
        where = f"page {s['page']}" if s.get("page") else ""
        head = f", section \"{s['heading']}\"" if s.get("heading") else ""
        lines.append(f"[{i}] {s['title']} ({s['doc_type']}{', ' + where if where else ''}{head})")
        lines.append(s["content"][:SNIPPET_CHARS])
        lines.append("")
    lines.append("Write the maintenance instruction now.")
    return "\n".join(x for x in lines if x is not None)


_CITE = re.compile(r"\[(\d+(?:\s*[,–-]\s*\d+)*)\]")


def check_citations(text: str, n_sources: int) -> tuple[str, set[int]]:
    """Remove citations outside 1..n; return the cleaned text and the cited source numbers."""
    cited: set[int] = set()

    def fix(m: re.Match[str]) -> str:
        nums: list[int] = []
        for part in re.split(r"\s*,\s*", m.group(1)):
            rng = re.split(r"\s*[–-]\s*", part)
            if len(rng) == 2 and rng[0].isdigit() and rng[1].isdigit():
                lo, hi = int(rng[0]), int(rng[1])
                nums.extend(range(lo, hi + 1) if hi - lo < 20 else [])
            elif part.strip().isdigit():
                nums.append(int(part))
        ok = sorted({x for x in nums if 1 <= x <= n_sources})
        cited.update(ok)
        return "".join(f"[{x}]" for x in ok)

    return _CITE.sub(fix, text), cited


def sources_json(sources: list[dict[str, Any]], cited: set[int]) -> list[dict[str, Any]]:
    return [
        {
            "n": i,
            "document_id": s["document_id"],
            "chunk_id": s["chunk_id"],
            "title": s["title"],
            "doc_type": s["doc_type"],
            "page": s.get("page"),
            "heading": s.get("heading"),
            "snippet": s["content"][:300],
            "score": s["score"],
            "cited": i in cited,
        }
        for i, s in enumerate(sources, 1)
    ]


async def _set(fault_id: int, status: str, md: str | None, sources: list[dict[str, Any]], model: str | None) -> None:
    await db.execute(
        "UPDATE analytics.faults SET instruction_status = %s, instruction_md = %s, instruction_sources = %s,"
        " instruction_model = %s, instruction_at = now() WHERE id = %s",
        (status, md, data.jb(sources), model, fault_id),
    )


async def generate_for(fault: dict[str, Any]) -> str:
    """Write the instruction of one fault; returns the resulting instruction_status."""
    asset = await db.fetch_one(
        "SELECT id, alias, name, type_name, model_name, location_id, location_path FROM analytics.v_assets WHERE id = %s",
        (fault["asset_id"],),
    ) or {"alias": str(fault["asset_id"])}
    s = get_settings()
    try:
        sources = await retrieve(fault, asset)
        if not sources:
            await _set(fault["id"], "SKIPPED", "_Maintenance instruction not generated: no indexed document matches this fault yet._", [], None)
            return "SKIPPED"
        text, _ = await gemini.generate_text(
            build_prompt(fault, asset, sources),
            component="instructions",
            model=s.model_fast,
            system=SYSTEM,
            max_output_tokens=1400,
            thinking_budget=0,
            temperature=0.1,
        )
    except Exception as err:  # noqa: BLE001 - recorded on the fault
        if transient(err):
            if data.utcnow() - fault["started_at"] < RETRY_FOR:
                log.info("instruction deferred fault=%s: %s", fault["id"], err)
                return "DEFERRED"  # stays PENDING: retried after the worker's back-off
            await _set(fault["id"], "SKIPPED", f"_Maintenance instruction not generated: {str(err)[:300]}._", [], None)
            return "SKIPPED"
        log.exception("instruction generation failed fault=%s", fault["id"])
        await _set(fault["id"], "FAILED", f"_Maintenance instruction generation failed: {str(err)[:300]}_", [], None)
        return "FAILED"
    cleaned, cited = check_citations(text, len(sources))
    if not cleaned.strip() or not cited:
        await _set(fault["id"], "FAILED", "_The generated instruction cited no source, so it was discarded._", sources_json(sources, set()), s.model_fast)
        log.warning("instruction discarded (no valid citation) fault=%s", fault["id"])
        return "FAILED"
    await _set(fault["id"], "READY", cleaned.strip(), sources_json(sources, cited), s.model_fast)
    log.info("instruction ready fault=%s sources=%s cited=%s", fault["id"], len(sources), sorted(cited))
    return "READY"


async def library_ready(site_code: str) -> bool:
    """At least one searchable document of the site (no embedding call is spent before that)."""
    return bool(
        await db.fetch_val(
            "SELECT EXISTS (SELECT 1 FROM rag.documents d WHERE d.site_code = %s AND d.status <> 'FAILED'"
            " AND EXISTS (SELECT 1 FROM rag.chunks c WHERE c.document_id = d.id))",
            (site_code,),
        )
    )


async def run(limit: int = BATCH, *, dry_run: bool = False) -> dict[str, Any]:
    """Generate instructions for PENDING faults (oldest first).

    While the site's document library has nothing indexed yet (corpus being
    generated or ingestion waiting for the budget), faults stay PENDING for up
    to WAIT_FOR_LIBRARY and are then SKIPPED; no Gemini call is made meanwhile.
    """
    faults = await db.fetch_all(
        "SELECT id, site_code, rule_code, asset_id, severity, message, intensity_estimate, evidence, started_at"
        " FROM analytics.faults WHERE instruction_status = 'PENDING' ORDER BY started_at LIMIT %s",
        (limit,),
    )
    return await process(faults, dry_run=dry_run)


async def process(faults: list[dict[str, Any]], *, dry_run: bool = False) -> dict[str, Any]:
    """Write (or defer / skip) the instructions of the given PENDING faults; counts per outcome."""
    counts: dict[str, Any] = {}
    ready: dict[str, bool] = {}
    for f in faults:
        site = f["site_code"] or get_settings().site_code
        if site not in ready:
            ready[site] = await library_ready(site)
        if dry_run:
            counts.setdefault("planned", []).append({"fault": f["id"], "rule": f["rule_code"], "library_ready": ready[site]})
            continue
        if not ready[site]:
            if data.utcnow() - f["started_at"] < WAIT_FOR_LIBRARY:
                counts["waiting"] = counts.get("waiting", 0) + 1
                continue
            await _set(f["id"], "SKIPPED", "_Maintenance instruction not generated: the document library had no indexed document._", [], None)
            st = "SKIPPED"
        else:
            st = await generate_for(f)
        if st == "DEFERRED":
            counts["deferred"] = counts.get("deferred", 0) + 1
            break  # budget / Gemini unavailable: do not try the other faults now
        counts[st] = counts.get(st, 0) + 1
    return counts
