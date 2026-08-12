"""Model-specific generation settings (Gemini 3 thinking levels, temperature, prices)."""
from google.genai import types

from afdd_ai.common import gemini


def test_gemini3_uses_thinking_levels():
    lvl = lambda m, b: gemini.thinking_config(m, b).thinking_level  # noqa: E731
    assert lvl("gemini-3.5-flash-lite", 0) == types.ThinkingLevel.MINIMAL
    assert lvl("gemini-3.8-flash", 0) == types.ThinkingLevel.LOW  # Flash has no minimal level
    assert lvl("gemini-3.5-flash-lite", 512) == types.ThinkingLevel.LOW
    assert lvl("gemini-3.1-pro-preview", 1024) == types.ThinkingLevel.LOW
    assert lvl("gemini-3.1-pro-preview", 8192) == types.ThinkingLevel.MEDIUM
    assert lvl("gemini-3.1-pro-preview", 20000) == types.ThinkingLevel.HIGH
    for m in ("gemini-3.5-flash-lite", "gemini-3.1-pro-preview"):
        assert gemini.thinking_config(m, 512).thinking_budget is None  # never both (400)


def test_gemini25_keeps_budgets():
    assert gemini.thinking_config("gemini-2.5-flash", 0).thinking_budget == 0
    assert gemini.thinking_config("gemini-2.5-pro", 0).thinking_budget == 128


def test_temperature_default_for_gemini3():
    assert gemini.sampling_temperature("gemini-3.5-flash-lite", 0.2) is None
    assert gemini.sampling_temperature("gemini-2.5-flash", 0.2) == 0.2


def test_prices_known_for_default_models():
    from afdd_ai.common.config import get_settings

    s = get_settings()
    for m in (s.model_fast, s.model_pro, s.model_lite, s.embed_model):
        assert m in gemini.PRICES, m
    u = gemini.Usage("gemini-3.5-flash-lite", input_tokens=1000, output_tokens=100, thinking_tokens=50)
    assert abs(u.cost_usd - (1000 * 0.30 + 150 * 2.50) / 1e6) < 1e-12


def test_cached_prompt_tokens_are_billed_at_the_cache_rate():
    meta = type("Meta", (), {"prompt_token_count": 57157, "candidates_token_count": 118, "thoughts_token_count": 0, "cached_content_token_count": 55066})()
    u = gemini.usage_from_metadata("gemini-3.5-flash-lite", meta)
    assert (u.input_tokens, u.cached_tokens) == (57157, 55066)
    assert abs(u.cost_usd - (2091 * 0.30 + 55066 * 0.03 + 118 * 2.50) / 1e6) < 1e-12
    # never more cached than prompt tokens, unknown models get no discount
    assert gemini.Usage("gemini-3.5-flash-lite", input_tokens=10, cached_tokens=50).cost_usd == 10 * 0.03 / 1e6
    assert gemini.Usage("some-new-model", input_tokens=1000, cached_tokens=1000).cost_usd == 1000 * 2.00 / 1e6
