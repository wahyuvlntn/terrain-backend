"""Fault instructions are retried after a budget or transient Gemini failure, not given up."""

from datetime import timedelta

import httpx

from afdd_ai.analytics import data, instructions
from afdd_ai.common import gemini


def _fault(age_h: float) -> dict:
    return {"id": 7, "asset_id": 1, "rule_code": "CHS-006", "site_code": "SGF1", "severity": "WARNING", "message": "leak", "intensity_estimate": 0.5, "evidence": {}, "started_at": data.utcnow() - timedelta(hours=age_h)}


async def _run(monkeypatch, err: BaseException, age_h: float) -> tuple[str, list]:
    sets: list = []

    async def fetch_one(*_a, **_k):
        return {"alias": "CHWS_01"}

    async def retrieve(_f, _a):
        return [{"document_id": 1, "chunk_id": 2, "title": "Manual", "doc_type": "manual", "content": "x", "score": 0.9}]

    async def generate_text(*_a, **_k):
        raise err

    async def _set(*args):
        sets.append(args)

    monkeypatch.setattr(instructions.db, "fetch_one", fetch_one)
    monkeypatch.setattr(instructions, "retrieve", retrieve)
    monkeypatch.setattr(instructions.gemini, "generate_text", generate_text)
    monkeypatch.setattr(instructions, "_set", _set)
    return await instructions.generate_for(_fault(age_h)), sets


async def test_budget_and_transient_errors_keep_the_fault_pending(monkeypatch):
    for err in (gemini.BudgetExceeded("limit"), gemini.GeminiNotConfigured("no key"), httpx.ConnectError("down"), TimeoutError()):
        status, sets = await _run(monkeypatch, err, age_h=1)
        assert status == "DEFERRED" and sets == [], err


async def test_gives_up_after_the_retry_window_and_on_real_errors(monkeypatch):
    status, sets = await _run(monkeypatch, gemini.BudgetExceeded("limit"), age_h=49)
    assert status == "SKIPPED" and sets[0][1] == "SKIPPED"
    status, sets = await _run(monkeypatch, ValueError("bad request"), age_h=1)
    assert status == "FAILED" and sets[0][1] == "FAILED"


async def test_process_stops_after_a_deferral(monkeypatch):
    calls = []

    async def library_ready(_site):
        return True

    async def generate_for(f):
        calls.append(f["id"])
        return "DEFERRED"

    monkeypatch.setattr(instructions, "library_ready", library_ready)
    monkeypatch.setattr(instructions, "generate_for", generate_for)
    counts = await instructions.process([_fault(1), {**_fault(1), "id": 8}])
    assert counts == {"deferred": 1} and calls == [7]
