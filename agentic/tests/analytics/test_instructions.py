"""Fault instruction sources: only documents every user who can see the fault can open."""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from afdd_ai.analytics import instructions


async def test_retrieval_is_limited_to_the_fault_equipments_location_subtree(monkeypatch):
    seen: list[Any] = []

    async def embed_texts(texts, **kw):  # noqa: ANN001, ANN003
        return [[0.1] * 768 for _ in texts]

    async def search(query, k=6, filters=None, allowed_location_ids=None, **kw):  # noqa: ANN001, ANN003
        seen.append(allowed_location_ids)
        return []

    monkeypatch.setattr(instructions.gemini, "embed_texts", embed_texts)
    monkeypatch.setattr(instructions.rag_search, "search", search)
    fault = {"rule_code": "CHS-006", "site_code": "SGF1", "evidence": {}, "started_at": datetime(2026, 10, 10, tzinfo=timezone.utc)}
    plant = {"alias": "CHWS_01", "type_name": "Chilled Water System", "model_name": "CUB plant", "location_id": 10}
    assert await instructions.retrieve(fault, plant) == []
    assert seen == [[10], [10]]  # alarm code + type, then type alone: both within the subtree of location 10
    seen.clear()
    await instructions.retrieve(fault, {**plant, "location_id": None})
    assert seen == [[], []]  # equipment without a location: documents without a location only
