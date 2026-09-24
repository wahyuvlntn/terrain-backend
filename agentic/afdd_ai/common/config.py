"""Service configuration from environment variables (docker-compose passes them).

Every Python service (ai-agent, ai-mcp, ai-analytics) reads the same settings.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from functools import lru_cache


def _env(key: str, default: str = "") -> str:
    value = os.environ.get(key, "").strip()
    return value or default


def _env_float(key: str, default: float) -> float:
    try:
        return float(_env(key, str(default)))
    except ValueError:
        return default


def _env_int(key: str, default: int) -> int:
    try:
        return int(_env(key, str(default)))
    except ValueError:
        return default


def _env_bool(key: str, default: bool) -> bool:
    raw = _env(key, "true" if default else "false").lower()
    return raw in ("1", "true", "yes", "on")


@dataclass(frozen=True)
class Settings:
    # PostgreSQL. afdd_ai: read plant data, read-write rag/analytics/sim/agent.
    database_url: str
    # afdd_ai_ro: read-only (MCP database server).
    database_url_ro: str
    # Async URL for ADK's DatabaseSessionService (postgresql+asyncpg://...).
    agent_session_db_url: str

    # Backend API (internal network; self-signed TLS, verification off on the internal network).
    api_url: str
    org_alias: str
    project_alias: str
    api_verify_tls: bool

    # Shared secret between the backend proxy / MCP clients and the Python services.
    agent_secret: str
    # Host names the ai-agent HTTP API answers to (Host header allow-list).
    agent_allowed_hosts: tuple[str, ...]

    # Gemini (Developer API key; GOOGLE_GENAI_USE_VERTEXAI=FALSE).
    google_api_key: str
    model_fast: str
    model_pro: str
    model_lite: str
    embed_model: str
    embed_dim: int
    daily_budget_usd: float
    # Each user's own assistant spend per day (plant time zone).
    user_daily_budget_usd: float
    # Part of the daily budget kept for background analytics (fault
    # instructions, embeddings): agent turns stop at daily - reserve.
    analytics_reserve_usd: float
    max_llm_calls: int
    thinking_budget_root: int
    thinking_budget_fast: int
    thinking_budget_pro: int

    # Private bucket for RAG documents (never served from /public): MinIO, or
    # Google Cloud Storage with STORAGE_PROVIDER=gcs (GCS_DOCS_BUCKET; an
    # emulator at STORAGE_EMULATOR_HOST for tests).
    storage_provider: str
    gcs_endpoint: str
    minio_endpoint: str
    minio_access_key: str
    minio_secret_key: str
    minio_secure: bool
    docs_bucket: str

    # Site (multi-site ready) and time zone of the plant.
    site_code: str
    timezone: str

    # Analytics cadence.
    anomaly_interval_s: int
    fdd_interval_s: int
    optimizer_interval_min: int

    # MCP servers (streamable HTTP, inside the ai-mcp container).
    mcp_db_url: str
    mcp_twin_url: str
    mcp_db_port: int
    mcp_twin_port: int
    # Private Cloud Run MCP services: send a Google ID token (X-Serverless-Authorization).
    mcp_id_token: bool
    # How long start-up waits for the database (a Cloud Run instance's VPC path).
    db_connect_timeout_s: float

    log_level: str

    @property
    def gemini_configured(self) -> bool:
        return bool(self.google_api_key)


def asyncpg_url(url: str) -> str:
    """The SQLAlchemy/asyncpg form of a libpq URL: driver prefix, and asyncpg's `ssl` for libpq's `sslmode`."""
    from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

    parts = urlsplit(url)
    scheme = "postgresql+asyncpg" if parts.scheme in ("postgresql", "postgres") else parts.scheme
    query = []
    for k, v in parse_qsl(parts.query, keep_blank_values=True):
        if k == "sslmode":
            k = "ssl"
        elif k in ("pool_max_conns", "pool_min_conns"):
            continue
        query.append((k, v))
    return urlunsplit((scheme, parts.netloc, parts.path, urlencode(query), parts.fragment))


@lru_cache(maxsize=1)
def get_settings() -> Settings:
    db = _env("DATABASE_URL", "postgresql://afdd_ai:afdd-ai-dev@postgres:5432/afdd")
    return Settings(
        database_url=db,
        database_url_ro=_env("DATABASE_URL_RO", db),
        agent_session_db_url=_env("AGENT_SESSION_DB_URL", asyncpg_url(db)),
        api_url=_env("AFDD_API_URL", "https://backend:4500").rstrip("/"),
        org_alias=_env("ORG_ALIAS", "local"),
        project_alias=_env("PROJECT_ALIAS", "digital_twin_afdd"),
        api_verify_tls=_env_bool("AFDD_API_VERIFY_TLS", False),
        agent_secret=_env("AGENT_SHARED_SECRET", ""),
        agent_allowed_hosts=tuple(h.strip() for h in _env("AGENT_ALLOWED_HOSTS", "ai-agent,localhost,127.0.0.1").split(",") if h.strip()),
        google_api_key=_env("GOOGLE_API_KEY", _env("GEMINI_API_KEY", "")),
        # Gemini 2.5 is closed to new API keys ("no longer available to new
        # users"); 3.5 Flash-Lite costs what 2.5 Flash did.
        model_fast=_env("AI_MODEL_FAST", "gemini-3.5-flash-lite"),
        model_pro=_env("AI_MODEL_PRO", "gemini-3.1-pro-preview"),
        model_lite=_env("AI_MODEL_LITE", "gemini-3.5-flash-lite"),
        embed_model=_env("AI_EMBED_MODEL", "gemini-embedding-001"),
        embed_dim=_env_int("AI_EMBED_DIM", 768),
        daily_budget_usd=_env_float("AI_DAILY_BUDGET_USD", 0.50),
        user_daily_budget_usd=_env_float("AI_USER_DAILY_BUDGET_USD", 0.20),
        analytics_reserve_usd=_env_float("AI_ANALYTICS_RESERVE_USD", 0.10),
        max_llm_calls=_env_int("AI_MAX_LLM_CALLS", 10),
        thinking_budget_root=_env_int("AI_THINKING_BUDGET_ROOT", 0),
        thinking_budget_fast=_env_int("AI_THINKING_BUDGET_FAST", 512),
        thinking_budget_pro=_env_int("AI_THINKING_BUDGET_PRO", 1024),
        storage_provider=_env("STORAGE_PROVIDER", "minio").lower(),
        gcs_endpoint=_env("STORAGE_EMULATOR_HOST", ""),
        minio_endpoint=_env("MINIO_ENDPOINT", "minio:9000"),
        minio_access_key=_env("MINIO_ACCESS_KEY", "afddminio"),
        minio_secret_key=_env("MINIO_SECRET_KEY", "afddminio-secret"),
        minio_secure=_env_bool("MINIO_USE_SSL", False),
        docs_bucket=_env("GCS_DOCS_BUCKET", "") if _env("STORAGE_PROVIDER", "minio").lower() == "gcs" else _env("MINIO_DOCS_BUCKET", "afdd-docs"),
        site_code=_env("SITE_CODE", "SGF1"),
        timezone=_env("DEFAULT_TIMEZONE", "Asia/Singapore"),
        anomaly_interval_s=_env_int("ANOMALY_INTERVAL_S", 60),
        fdd_interval_s=_env_int("FDD_INTERVAL_S", 300),
        optimizer_interval_min=_env_int("OPTIMIZER_INTERVAL_MIN", 60),
        mcp_db_url=_env("MCP_DB_URL", "http://ai-mcp:8101/mcp"),
        mcp_twin_url=_env("MCP_TWIN_URL", "http://ai-mcp:8102/mcp"),
        mcp_db_port=_env_int("MCP_DB_PORT", 8101),
        mcp_twin_port=_env_int("MCP_TWIN_PORT", 8102),
        mcp_id_token=_env_bool("MCP_ID_TOKEN", False),
        db_connect_timeout_s=_env_float("DB_CONNECT_TIMEOUT_S", 60.0),
        log_level=_env("LOG_LEVEL", "INFO").upper(),
    )
