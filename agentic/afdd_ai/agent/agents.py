"""The agent tree of the plant assistant (DESIGN 6.3), on Google ADK 2.11.

    plant_assistant (root, fast model, thinking budget AI_THINKING_BUDGET_ROOT): open_in_app
    ├── insight_agent (fast, AI_THINKING_BUDGET_FAST): backend API tools, charts, RAG,
    │   McpToolset -> afdd-db, open_in_app, deep_analyst as a single-turn agent tool
    │   └── deep_analyst (pro model, AI_THINKING_BUDGET_PRO, mode single_turn)
    ├── twin_agent (fast, AI_THINKING_BUDGET_FAST): McpToolset -> afdd-twin, simulation forms, RAG, open_in_app
    └── maintenance_agent (fast, AI_THINKING_BUDGET_FAST): maintenance order tools (create, edit,
        workflow actions; forms when a request is incomplete), order and early-warning reads, open_in_app

The root and its three specialists are chat-mode agents (ADK transfers the
conversation between them). deep_analyst is a `single_turn` sub-agent of the
insight agent: ADK exposes it to its parent as an agent tool
(`_SingleTurnAgentTool`, an `AgentTool`) that runs one isolated turn on the
request text only. Every agent shares the budget callbacks (`budget.py`). The
three chat agents can open a page of the application for the user
(`navigation.open_in_app`), so the root answers a pure navigation request itself.

MCP calls carry the user's session as `Authorization: Bearer` plus the shared
`X-Agent-Secret`, read per call from `temp:` state by the header provider, so
the token never reaches the model.
"""

from __future__ import annotations

import logging
from collections.abc import Mapping
from typing import Any

from google.adk.agents import LlmAgent
from google.adk.agents.readonly_context import ReadonlyContext
from google.adk.models import Gemini
from google.adk.models.base_llm import BaseLlm
from google.adk.tools.mcp_tool import McpToolset, StreamableHTTPConnectionParams
from google.genai import types

from ..common import gcp
from ..common.config import Settings, get_settings
from ..common.gemini import sampling_temperature, thinking_config
from . import budget, charts, context, navigation, prompts, rag_tool, simforms, tools_backend, tools_maintenance
from . import state as st

log = logging.getLogger(__name__)

APP_NAME = "afdd_plant_assistant"
ROOT = "plant_assistant"
INSIGHT = "insight_agent"
TWIN = "twin_agent"
MAINTENANCE = "maintenance_agent"
DEEP = "deep_analyst"
AGENT_NAMES = (ROOT, INSIGHT, TWIN, MAINTENANCE, DEEP)
# Agents whose text is tool output for another agent, not an answer to the user.
INTERNAL_AGENTS = frozenset({DEEP})

# afdd-db tools the insight agent uses. Its search_documents, list_alarm_events, list_faults and
# list_anomalies are left out: the agent has the same data through the native RAG tool (which also
# emits citations) and the backend tools, and duplicate tool names are not allowed.
INSIGHT_DB_TOOLS = ["query_parameter_history", "get_energy_summary", "get_point_map", "list_simulation_jobs"]

AGENT_SECRET_HEADER = "X-Agent-Secret"
AGENT_SESSION_HEADER = "X-AFDD-Agent-Session"
USER_ID_HEADER = "X-AFDD-User-Id"
SERVERLESS_AUTH_HEADER = "X-Serverless-Authorization"
MCP_TIMEOUT_S = 20.0
MCP_TOOL_LIST_TTL_S = 300.0

_RETRY = types.HttpRetryOptions(attempts=3, initial_delay=1.0, max_delay=8.0, http_status_codes=[429, 500, 502, 503, 504])


def mcp_header_provider(settings: Settings):
    """Headers of every MCP request: the user's session, the shared secret, the user and agent session ids.

    The user id only widens what the user may see to the simulation jobs they requested themselves;
    the MCP servers derive permissions and location scope from the session token.
    """

    def headers(ctx: ReadonlyContext | None) -> dict[str, str]:
        out = {AGENT_SECRET_HEADER: settings.agent_secret}
        if ctx is None:
            return out
        token = st.session_token(ctx.state)
        if token:
            out["Authorization"] = f"Bearer {token}"
        user_id = st.state_user_id(ctx.state)
        if user_id is not None:
            out[USER_ID_HEADER] = str(user_id)
        try:
            out[AGENT_SESSION_HEADER] = ctx.session.id
        except (AttributeError, ValueError):
            pass
        return out

    return headers


def mcp_headers(settings: Settings, url: str):
    """`mcp_header_provider` plus, for a private Cloud Run MCP service (MCP_ID_TOKEN), a Google ID
    token of this service account (X-Serverless-Authorization; the user's session stays in Authorization)."""
    base = mcp_header_provider(settings)
    if not settings.mcp_id_token:
        return base
    aud = gcp.audience(url)

    def headers(ctx: ReadonlyContext | None) -> dict[str, str]:
        out = base(ctx)
        out[SERVERLESS_AUTH_HEADER] = f"Bearer {gcp.id_token(aud)}"
        return out

    return headers


class PlantMcpToolset(McpToolset):
    """An `McpToolset` that offers no tools while its server is unreachable.

    Without this a down ai-mcp container would fail every model call of the agent (ADK lists the
    toolset's tools before each call); this way the agent keeps its other tools and says that the
    database or simulation tools are unavailable. The listing is retried on the next model call.
    """

    def __init__(self, *, url: str, **kwargs: Any):
        super().__init__(**kwargs)
        self._afdd_url = url

    async def get_tools(self, readonly_context: ReadonlyContext | None = None) -> list[Any]:
        try:
            return await super().get_tools(readonly_context)
        except Exception as err:  # noqa: BLE001 - degrade to no tools, never fail the turn
            log.warning("MCP server unavailable url=%s: %s", self._afdd_url, err)
            return []


def mcp_toolset(url: str, settings: Settings, tool_filter: list[str] | None = None) -> McpToolset:
    return PlantMcpToolset(
        url=url,
        connection_params=StreamableHTTPConnectionParams(url=url, timeout=MCP_TIMEOUT_S, sse_read_timeout=120.0),
        tool_filter=tool_filter,
        tool_list_cache_ttl_seconds=MCP_TOOL_LIST_TTL_S,
        header_provider=mcp_headers(settings, url),
    )


def _gemini(model_name: str, settings: Settings) -> Gemini:
    kwargs: dict[str, Any] = {"model": model_name, "retry_options": _RETRY}
    if settings.google_api_key:
        kwargs["client_kwargs"] = {"api_key": settings.google_api_key}
    return Gemini(**kwargs)


def _config(model_name: str, thinking_budget: int, max_output_tokens: int) -> types.GenerateContentConfig:
    return types.GenerateContentConfig(
        temperature=sampling_temperature(model_name, 0.2),
        max_output_tokens=max_output_tokens,
        thinking_config=thinking_config(model_name, thinking_budget),
    )


def build_root_agent(
    *,
    settings: Settings | None = None,
    models: Mapping[str, str | BaseLlm] | None = None,
    mcp_db_url: str | None = None,
    mcp_twin_url: str | None = None,
) -> LlmAgent:
    """Build a fresh agent tree.

    models: optional per-agent model override (agent name -> model id or BaseLlm, used by tests).
    mcp_db_url / mcp_twin_url: override the afdd-db / afdd-twin endpoints (default from settings).
    """
    s = settings or get_settings()
    overrides = dict(models or {})

    def model_for(agent: str, model_name: str) -> str | BaseLlm:
        chosen = overrides.get(agent, model_name)
        return chosen if isinstance(chosen, BaseLlm) else _gemini(str(chosen), s)

    def callbacks(model_name: str) -> dict[str, Any]:
        # budget first (it may refuse the call), then keep chart data out of the prompt
        return {"before_model_callback": [budget.before_model, context.before_model], "after_model_callback": budget.usage_recorder(model_name)}

    deep = LlmAgent(
        name=DEEP,
        description=(
            "Senior plant engineer for long summaries and complex analysis. Pass every number, time and "
            "observation it needs in `request`; it has no tools and sees nothing else."
        ),
        model=model_for(DEEP, s.model_pro),
        mode="single_turn",
        include_contents="none",
        instruction=prompts.deep_instruction,
        generate_content_config=_config(s.model_pro, s.thinking_budget_pro, 6144),
        **callbacks(s.model_pro),
    )

    insight = LlmAgent(
        name=INSIGHT,
        description=(
            "Plant data analyst: live and historical equipment data, alarms, anomalies and detected faults, "
            "maintenance orders, early warnings, reliability KPIs, energy, documents and procedures, charts."
        ),
        model=model_for(INSIGHT, s.model_fast),
        instruction=prompts.insight_instruction,
        tools=[
            *tools_backend.BACKEND_TOOLS,
            charts.chart_parameter_trend,
            charts.chart_compare,
            charts.make_chart,
            rag_tool.search_documents,
            navigation.open_in_app,
            mcp_toolset(mcp_db_url or s.mcp_db_url, s, INSIGHT_DB_TOOLS),
        ],
        sub_agents=[deep],
        generate_content_config=_config(s.model_fast, s.thinking_budget_fast, 4096),
        **callbacks(s.model_fast),
    )

    twin = LlmAgent(
        name=TWIN,
        description=(
            "Digital-twin planner: what-if questions and future scenarios, proposes simulations for human "
            "approval, reports simulation status and results, optimisation recommendations to test."
        ),
        model=model_for(TWIN, s.model_fast),
        instruction=prompts.twin_instruction,
        tools=[
            mcp_toolset(mcp_twin_url or s.mcp_twin_url, s),
            *simforms.SIM_FORM_TOOLS,
            rag_tool.search_documents,
            navigation.open_in_app,
        ],
        generate_content_config=_config(s.model_fast, s.thinking_budget_fast, 4096),
        **callbacks(s.model_fast),
    )

    maintenance = LlmAgent(
        name=MAINTENANCE,
        description=(
            "Maintenance planner: creates, edits, assigns, approves, rejects, starts, pauses, resumes, "
            "completes, closes and deletes maintenance orders for the user, with a form when details are missing."
        ),
        model=model_for(MAINTENANCE, s.model_fast),
        instruction=prompts.maintenance_instruction,
        tools=[
            tools_backend.get_maintenance_orders,
            tools_backend.get_maintenance_order_detail,
            tools_backend.get_early_warnings,
            tools_backend.list_assets,
            *tools_maintenance.MAINTENANCE_TOOLS,
            navigation.open_in_app,
        ],
        generate_content_config=_config(s.model_fast, s.thinking_budget_fast, 4096),
        **callbacks(s.model_fast),
    )

    return LlmAgent(
        name=ROOT,
        description="Plant assistant: greets, opens pages of the application and routes questions to the specialists.",
        model=model_for(ROOT, s.model_fast),
        instruction=prompts.root_instruction,
        tools=[navigation.open_in_app],
        sub_agents=[insight, twin, maintenance],
        generate_content_config=_config(s.model_fast, s.thinking_budget_root, 1024),
        **callbacks(s.model_fast),
    )
