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


async def test_a_zero_budget_means_no_limit(monkeypatch, budget_env):
    monkeypatch.setenv("AI_DAILY_BUDGET_USD", "0")
    get_settings.cache_clear()
    ledger = Ledger(spent=100.0)
    use(monkeypatch, ledger)
    assert len(await gemini.embed_texts(TEXTS, titles=["Manual"] * len(TEXTS))) == 120
    assert ledger.calls == [50, 50, 20] and ledger.spent > 100.0  # embedded, and the usage still recorded
    await gemini.check_budget(planned_usd=1e6)


@pytest.mark.usefixtures("budget_env")
async def test_ingestion_defers_a_document_that_does_not_fit(monkeypatch):
    chunks = [Chunk(index=i, page=1, heading="H", content="y" * 4000, token_count=1000) for i in range(120)]
    ledger = Ledger(spent=0.49)
    use(monkeypatch, ledger)
    with pytest.raises(gemini.BudgetExceeded):  # ingest() turns this into Deferred (document back to PENDING)
        await ingest.embed_chunks(chunks, "Manual")
    assert ledger.calls == []


RATE_LIMIT = {"error": {"code": 429, "status": "RESOURCE_EXHAUSTED",
                        "message": "Quota exceeded for metric: embed_content_free_tier_requests, limit: 100\nPlease retry in 43.9s.",
                        "details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "43s"}]}}
DAILY_LIMIT = {"error": {"code": 429, "status": "RESOURCE_EXHAUSTED",
                         "message": "Quota exceeded for metric: embed_content_free_tier_requests, limit: 1000\nPlease retry in 14h14m33.41s.",
                         "details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "51273s"}]}}


def test_rate_limit_wait_reads_the_server_delay():
    from google.genai import errors

    assert gemini.rate_limit_wait(errors.ClientError(429, RATE_LIMIT)) == pytest.approx(43.9)
    assert gemini.rate_limit_wait(errors.ClientError(429, DAILY_LIMIT)) == 51273.0  # "retry in 14h14m33s": the RetryInfo seconds
    assert gemini.rate_limit_wait(errors.ClientError(429, {"error": {"code": 429, "status": "RESOURCE_EXHAUSTED", "message": "slow down"}})) == 60.0
    assert gemini.rate_limit_wait(errors.ClientError(400, {"error": {"code": 400, "status": "INVALID_ARGUMENT", "message": "bad"}})) is None
    assert gemini.rate_limit_wait(RuntimeError("retry in 5s")) is None


@pytest.mark.usefixtures("budget_env")
@pytest.mark.parametrize(
    ("limit", "task_type", "calls", "waits"),
    [
        (RATE_LIMIT, "RETRIEVAL_DOCUMENT", 2, [43.9]),  # ingestion waits out the minute (a 2 s retry only fails again)
        (RATE_LIMIT, "RETRIEVAL_QUERY", 2, [2.0]),  # a search keeps the short backoff
        (DAILY_LIMIT, "RETRIEVAL_DOCUMENT", 1, []),  # hours away: raised at once, the document is deferred
    ],
)
async def test_a_rate_limited_embedding_waits_as_the_server_asks(monkeypatch, limit, task_type, calls, waits):
    from types import SimpleNamespace

    from google.genai import errors, types

    sent, slept = [], []

    async def embed_content(model, contents, config):  # noqa: ANN001 - the first request is refused
        sent.append(len(contents))
        if len(sent) == 1:
            raise errors.ClientError(429, limit)
        return SimpleNamespace(embeddings=[SimpleNamespace(values=[3.0, 4.0]) for _ in contents])

    async def sleep(s):  # noqa: ANN001
        slept.append(s)

    monkeypatch.setattr(gemini, "client", lambda: SimpleNamespace(aio=SimpleNamespace(models=SimpleNamespace(embed_content=embed_content))))
    monkeypatch.setattr(gemini.asyncio, "sleep", sleep)
    call = gemini._embed_call(["a", "b"], types.EmbedContentConfig(task_type=task_type))  # noqa: SLF001
    if calls == 1:
        with pytest.raises(errors.ClientError):
            await call
    else:
        assert await call == [[0.6, 0.8], [0.6, 0.8]]
    assert len(sent) == calls and slept == [pytest.approx(w) for w in waits]


@pytest.mark.usefixtures("budget_env")
async def test_ingestion_defers_a_rate_limited_document(monkeypatch):
    from google.genai import errors

    deferred, failed = [], []

    async def get_bytes(key):  # noqa: ANN001
        return b"%PDF"

    async def same(doc):  # noqa: ANN001
        return doc

    async def rate_limited(chunks, title):  # noqa: ANN001 - quota still exhausted after the retries
        raise errors.ClientError(429, RATE_LIMIT)

    async def record(store, doc_id, reason):  # noqa: ANN001
        store.append(doc_id)

    monkeypatch.setattr(ingest.storage, "get_bytes", get_bytes)
    monkeypatch.setattr(ingest, "extract_pages", lambda blob: ["page"])
    monkeypatch.setattr(ingest, "chunk_pages", lambda pages: [Chunk(index=0, page=1, heading="H", content="text", token_count=1)])
    monkeypatch.setattr(ingest, "_resolve_asset", same)
    monkeypatch.setattr(ingest, "embed_chunks", rate_limited)
    monkeypatch.setattr(ingest, "_defer", lambda doc_id, reason: record(deferred, doc_id, reason))
    monkeypatch.setattr(ingest, "_fail", lambda doc_id, reason: record(failed, doc_id, reason))
    with pytest.raises(ingest.Deferred):
        await ingest.ingest({"id": 7, "object_key": "docs/7.pdf", "title": "Manual"})
    assert deferred == [7] and failed == []  # back to PENDING, not FAILED
