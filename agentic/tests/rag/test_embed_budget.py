"""Embedding budget: a document starts only when its estimated cost fits, and every batch is checked.

No Gemini call and no database: the spend, the embedding call and the usage
recording are replaced by an in-memory ledger.
"""

from __future__ import annotations

from collections.abc import Iterator

import pytest

from afdd_ai.common import gemini
from afdd_ai.common.config import get_settings
from afdd_ai.rag import ingest
from afdd_ai.rag.chunking import Chunk


class Ledger:
    def __init__(self, spent: float) -> None:
        self.spent = spent
        self.calls: list[int] = []

    async def spent_today_usd(self, user_id: int | None = None) -> float:
        return self.spent

    async def embed_call(self, texts, cfg, attempts=4):  # noqa: ANN001
        self.calls.append(len(texts))
        return [[1.0] + [0.0] * 767 for _ in texts]

    async def record_usage(self, component, usage, **kw):  # noqa: ANN001, ANN003
        self.spent += usage.cost_usd
        return usage.cost_usd


@pytest.fixture
def budget_env(monkeypatch) -> Iterator[None]:
    monkeypatch.setenv("GOOGLE_API_KEY", "test-key-never-used")
    monkeypatch.setenv("AI_DAILY_BUDGET_USD", "0.50")
    get_settings.cache_clear()
    yield
    monkeypatch.undo()
    get_settings.cache_clear()


def use(monkeypatch, ledger: Ledger) -> None:
    monkeypatch.setattr(gemini, "spent_today_usd", ledger.spent_today_usd)
    monkeypatch.setattr(gemini, "_embed_call", ledger.embed_call)
    monkeypatch.setattr(gemini, "record_usage", ledger.record_usage)


TEXTS = ["x" * 4000] * 120  # 1,000 tokens each: 3 batches of 50 / 50 / 20


@pytest.mark.usefixtures("budget_env")
async def test_a_document_that_does_not_fit_is_not_started(monkeypatch):
    cost = gemini.estimate_embedding_cost(TEXTS)
    assert cost == pytest.approx(120 * 1000 * 0.15 / 1e6)
    ledger = Ledger(spent=0.50 - cost / 2)
    use(monkeypatch, ledger)
    with pytest.raises(gemini.BudgetExceeded, match="would be exceeded"):
        await gemini.embed_texts(TEXTS, titles=["Manual"] * len(TEXTS))
    assert ledger.calls == []  # nothing embedded (and nothing paid) for a document that cannot finish today


@pytest.mark.usefixtures("budget_env")
async def test_every_batch_checks_the_budget(monkeypatch):
    ledger = Ledger(spent=0.0)
    use(monkeypatch, ledger)
    assert len(await gemini.embed_texts(TEXTS, titles=["Manual"] * len(TEXTS))) == 120
    assert ledger.calls == [50, 50, 20]

    # other spending uses the budget up while a document is embedded: it stops before the next batch
    ledger = Ledger(spent=0.0)
    use(monkeypatch, ledger)

    async def embed_and_spend(texts, cfg, attempts=4):  # noqa: ANN001
        ledger.calls.append(len(texts))
        ledger.spent = 0.499  # e.g. fault instructions meanwhile
        return [[1.0] + [0.0] * 767 for _ in texts]

    monkeypatch.setattr(gemini, "_embed_call", embed_and_spend)
    with pytest.raises(gemini.BudgetExceeded):
        await gemini.embed_texts(TEXTS, titles=["Manual"] * len(TEXTS))
    assert ledger.calls == [50]


@pytest.mark.usefixtures("budget_env")
async def test_ingestion_defers_a_document_that_does_not_fit(monkeypatch):
    chunks = [Chunk(index=i, page=1, heading="H", content="y" * 4000, token_count=1000) for i in range(120)]
    ledger = Ledger(spent=0.49)
    use(monkeypatch, ledger)
    with pytest.raises(gemini.BudgetExceeded):  # ingest() turns this into Deferred (document back to PENDING)
        await ingest.embed_chunks(chunks, "Manual")
    assert ledger.calls == []
