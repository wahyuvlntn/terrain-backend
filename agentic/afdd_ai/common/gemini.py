"""Gemini access with cost accounting and a daily budget.

The API key has a small budget, so every model call is priced and recorded in
`analytics.llm_usage`, and calls are refused once today's spend (plant time zone)
reaches `AI_DAILY_BUDGET_USD` (0 = no limit; usage is still recorded). Thinking
tokens are billed as output.

Non-agent components (fault instructions, embeddings) use the helpers here; the
ADK agent records its usage through `record_usage` from a model callback.
"""

from __future__ import annotations

import asyncio
import logging
import math
import re
from dataclasses import dataclass
from functools import lru_cache
from typing import Any

from google import genai
from google.genai import types

from . import db
from .config import get_settings

log = logging.getLogger(__name__)

# USD per 1M tokens (input, output incl. thinking, cached input). Gemini
# Developer API paid tier, text, <=200k context (ai.google.dev pricing, checked
# 2026-10-10). Prompt tokens served from Gemini's implicit context cache are
# billed at the cached rate (no storage charge for implicit caching).
# 3.8 / 3.6 Flash rise to 1.50 / 7.50 / 0.15 on 2027-01-01.
PRICES: dict[str, tuple[float, float, float]] = {
    "gemini-3.8-flash": (0.75, 3.75, 0.075),
    "gemini-3.6-flash": (0.75, 3.75, 0.075),
    "gemini-3.5-flash-lite": (0.30, 2.50, 0.03),
    "gemini-3.1-flash-lite": (0.25, 1.50, 0.025),
    "gemini-3.1-pro-preview": (2.00, 12.00, 0.20),
    "gemini-2.5-flash": (0.30, 2.50, 0.03),
    "gemini-2.5-flash-lite": (0.10, 0.40, 0.01),
    "gemini-2.5-pro": (1.25, 10.00, 0.125),
    "gemini-embedding-001": (0.15, 0.0, 0.15),
    "gemini-embedding-2": (0.20, 0.0, 0.20),
}
_DEFAULT_PRICE = (2.00, 12.00, 2.00)  # unknown model: assume the expensive tier, no cache discount


class BudgetExceeded(RuntimeError):
    """Today's Gemini spend reached the configured daily budget."""


class GeminiNotConfigured(RuntimeError):
    """No GOOGLE_API_KEY."""


@dataclass
class Usage:
    model: str
    input_tokens: int = 0  # the whole prompt, cached part included
    output_tokens: int = 0
    thinking_tokens: int = 0
    cached_tokens: int = 0  # prompt tokens served from the implicit cache

    @property
    def cost_usd(self) -> float:
        price_in, price_out, price_cached = PRICES.get(self.model, _DEFAULT_PRICE)
        cached = min(max(self.cached_tokens, 0), self.input_tokens)
        fresh = self.input_tokens - cached
        return (fresh * price_in + cached * price_cached + (self.output_tokens + self.thinking_tokens) * price_out) / 1_000_000


def usage_from_metadata(model: str, meta: Any) -> Usage:
    """Usage from a google.genai `usage_metadata` (or None)."""
    if meta is None:
        return Usage(model)
    return Usage(
        model=model,
        input_tokens=int(getattr(meta, "prompt_token_count", 0) or 0),
        output_tokens=int(getattr(meta, "candidates_token_count", 0) or 0),
        thinking_tokens=int(getattr(meta, "thoughts_token_count", 0) or 0),
        cached_tokens=int(getattr(meta, "cached_content_token_count", 0) or 0),
    )


@lru_cache(maxsize=1)
def client() -> genai.Client:
    s = get_settings()
    if not s.google_api_key:
        raise GeminiNotConfigured("GOOGLE_API_KEY is not set")
    return genai.Client(api_key=s.google_api_key)


async def spent_today_usd(user_id: int | None = None) -> float:
    """Spend since local (plant time zone) midnight: everyone's, or one user's own."""
    tz = get_settings().timezone
    sql = "SELECT COALESCE(sum(cost_usd), 0) FROM analytics.llm_usage WHERE at >= (date_trunc('day', now() AT TIME ZONE %s) AT TIME ZONE %s)"
    params: tuple[Any, ...] = (tz, tz)
    if user_id is not None:
        sql += " AND user_id = %s"
        params += (user_id,)
    val = await db.fetch_val(sql, params)
    return float(val or 0)


async def check_budget(planned_usd: float = 0.0) -> None:
    """Raise unless today's spend is below the daily budget and the planned
    cost of the next call(s) still fits in it (a budget of 0 = no limit)."""
    s = get_settings()
    if not s.gemini_configured:
        raise GeminiNotConfigured("Gemini is not configured (GOOGLE_API_KEY)")
    if s.daily_budget_usd <= 0:
        return
    spent = await spent_today_usd()
    if spent >= s.daily_budget_usd:
        raise BudgetExceeded(f"Daily AI budget reached (US${spent:.3f} of US${s.daily_budget_usd:.2f}); try again tomorrow")
    if spent + planned_usd > s.daily_budget_usd:
        raise BudgetExceeded(
            f"Daily AI budget would be exceeded (US${spent:.3f} spent + about US${planned_usd:.3f} needed, "
            f"of US${s.daily_budget_usd:.2f}); try again tomorrow"
        )


async def record_usage(component: str, usage: Usage, *, user_id: int | None = None, session_id: str | None = None) -> float:
    """Store one call's usage; returns its cost."""
    cost = usage.cost_usd
    try:
        await db.execute(
            "INSERT INTO analytics.llm_usage (component, model, input_tokens, output_tokens, thinking_tokens, cached_tokens, cost_usd, user_id, session_id)"
            " VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)",
            (component, usage.model, usage.input_tokens, usage.output_tokens, usage.thinking_tokens, usage.cached_tokens, round(cost, 6), user_id, session_id),
        )
    except Exception:  # noqa: BLE001 - accounting must never break a request
        log.exception("could not record llm usage")
    return cost


def is_gemini3(model: str) -> bool:
    return model.startswith("gemini-3")


def thinking_config(model: str, budget: int) -> types.ThinkingConfig | None:
    """Thinking config from a token budget.

    Gemini 3 models take a thinking level instead (a budget is legacy): budget
    0 gives the lowest level the model has (minimal on Flash-Lite, low on
    Flash and Pro, where thinking cannot be turned off), up to 4096 low, up to
    16384 medium, above that high. 2.5 Pro cannot turn thinking off (minimum
    128).
    """
    if is_gemini3(model):
        if budget <= 0 and "lite" in model:
            level = types.ThinkingLevel.MINIMAL
        elif budget <= 4096:
            level = types.ThinkingLevel.LOW
        elif budget <= 16384:
            level = types.ThinkingLevel.MEDIUM
        else:
            level = types.ThinkingLevel.HIGH
        return types.ThinkingConfig(thinking_level=level)
    if "2.5-pro" in model:
        return types.ThinkingConfig(thinking_budget=max(128, budget))
    if "2.5-flash" in model:
        return types.ThinkingConfig(thinking_budget=max(0, budget))
    return None


def sampling_temperature(model: str, temperature: float | None) -> float | None:
    """Google recommends the default temperature (1.0) for Gemini 3 models:
    lower values can make them loop or reason worse. Older models keep the
    requested value."""
    return None if is_gemini3(model) else temperature


async def generate_text(
    prompt: str | list[types.Content],
    *,
    component: str,
    model: str | None = None,
    system: str | None = None,
    max_output_tokens: int = 1200,
    thinking_budget: int | None = None,
    temperature: float = 0.2,
    user_id: int | None = None,
) -> tuple[str, Usage]:
    """One non-streaming generation (budget-checked, priced, recorded)."""
    s = get_settings()
    await check_budget()
    model = model or s.model_fast
    cfg = types.GenerateContentConfig(
        system_instruction=system,
        max_output_tokens=max_output_tokens,
        temperature=sampling_temperature(model, temperature),
        thinking_config=thinking_config(model, s.thinking_budget_fast if thinking_budget is None else thinking_budget),
    )
    resp = await client().aio.models.generate_content(model=model, contents=prompt, config=cfg)
    usage = usage_from_metadata(model, resp.usage_metadata)
    await record_usage(component, usage, user_id=user_id)
    return (resp.text or "").strip(), usage


def _normalise(vec: list[float]) -> list[float]:
    n = math.sqrt(sum(v * v for v in vec)) or 1.0
    return [v / n for v in vec]


def estimate_tokens(text: str) -> int:
    """Rough token estimate (embedding responses carry no usage)."""
    return max(1, len(text) // 4)


def estimate_embedding_cost(texts: list[str]) -> float:
    """Estimated cost (USD) of embedding texts with the configured model."""
    return Usage(get_settings().embed_model, input_tokens=sum(estimate_tokens(t) for t in texts)).cost_usd


async def embed_texts(
    texts: list[str],
    *,
    task_type: str = "RETRIEVAL_DOCUMENT",
    titles: list[str] | None = None,
    component: str = "embeddings",
    batch_size: int = 50,
) -> list[list[float]]:
    """Embed texts with the configured model and dimension (L2-normalised).

    task_type: RETRIEVAL_DOCUMENT for chunks (title applies), RETRIEVAL_QUERY for searches.

    The budget is checked before every batch against the estimated cost of
    everything still to embed, so a document (one call with all its chunks) is
    only started when it fits into today's budget as a whole, and stops early
    when other spending uses the budget up meanwhile (raises BudgetExceeded).
    """
    s = get_settings()
    out: list[list[float]] = []
    for start in range(0, len(texts), batch_size):
        await check_budget(estimate_embedding_cost(texts[start:]))
        batch = texts[start : start + batch_size]
        cfgs: list[types.EmbedContentConfig]
        if titles and task_type == "RETRIEVAL_DOCUMENT":
            # title is per request; embed one by one only when titles differ within the batch
            batch_titles = titles[start : start + batch_size]
            if len(set(batch_titles)) == 1:
                cfgs = [types.EmbedContentConfig(task_type=task_type, title=batch_titles[0], output_dimensionality=s.embed_dim)]
            else:
                vecs = []
                for text, title in zip(batch, batch_titles, strict=True):
                    r = await _embed_call([text], types.EmbedContentConfig(task_type=task_type, title=title, output_dimensionality=s.embed_dim))
                    vecs.extend(r)
                out.extend(vecs)
                await record_usage(component, Usage(s.embed_model, input_tokens=sum(estimate_tokens(t) for t in batch)))
                continue
        else:
            cfgs = [types.EmbedContentConfig(task_type=task_type, output_dimensionality=s.embed_dim)]
        out.extend(await _embed_call(batch, cfgs[0]))
        await record_usage(component, Usage(s.embed_model, input_tokens=sum(estimate_tokens(t) for t in batch)))
    return out


_RETRY_DELAY = re.compile(r"retry(?:Delay': '| in )(\d+(?:\.\d+)?)s")


def rate_limit_wait(err: Exception) -> float | None:
    """Seconds a 429 asks to wait (RetryInfo); None when err is not a rate limit. The free tier
    answers about a minute for its 100 embed requests/min and hours for its 1,000 a day."""
    if getattr(err, "code", None) != 429:
        return None
    m = _RETRY_DELAY.search(str(err))
    return float(m.group(1)) if m else 60.0


async def _embed_call(texts: list[str], cfg: types.EmbedContentConfig, attempts: int = 4) -> list[list[float]]:
    s = get_settings()
    delay = 2.0
    for attempt in range(attempts):
        try:
            resp = await client().aio.models.embed_content(model=s.embed_model, contents=texts, config=cfg)
            vecs = [list(e.values or []) for e in (resp.embeddings or [])]
            if len(vecs) != len(texts):
                raise RuntimeError(f"embedding count {len(vecs)} != {len(texts)}")
            return [_normalise(v) for v in vecs]
        except Exception as err:  # noqa: BLE001 - retry rate limits / transient errors
            wait = rate_limit_wait(err)
            if attempt == attempts - 1 or (wait is not None and wait > 90):
                raise  # out of attempts, or a daily quota (hours away): the caller defers or fails fast
            # A per-minute quota names its own wait: background document embedding waits that long
            # (sooner only fails again); a search query keeps the short backoff.
            pause = max(delay, wait) if wait is not None and cfg is not None and cfg.task_type == "RETRIEVAL_DOCUMENT" else delay
            log.warning("embed retry %s in %.0f s: %s", attempt + 1, pause, err)
            await asyncio.sleep(pause)
            delay *= 2
    return []
