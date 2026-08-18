-- Gemini implicit context caching (docs/agentic/DESIGN.md 6.3): prompt tokens
-- served from the cache are billed at the cache rate (10 % of the input price),
-- so they are counted apart. input_tokens keeps the full prompt size; the cached
-- part is included in it.

ALTER TABLE analytics.llm_usage ADD COLUMN cached_tokens integer NOT NULL DEFAULT 0;
