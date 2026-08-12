"""Test doubles shared by the agent and MCP tests (no Gemini calls, no real backend).

* `ScriptedLlm`: an ADK `BaseLlm` that replays a script of text / function calls
  (streamed like Gemini in SSE mode, with usage metadata) and records requests.
* `FakeBackend`: the AFDD script API over real HTTP (uvicorn in a thread):
  session-header auth, per-script handlers, every call recorded.
* `start_mcp_server`: one MCP server process (`python -m afdd_ai.mcp.serve db|twin`).
* `scratch_database`: a throw-away database owned by afdd_ai for ADK session tables.
"""

from __future__ import annotations

import asyncio
import json
import os
import socket
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import AsyncGenerator, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
import uvicorn
from google.adk.models._capabilities import LlmCapabilities
from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types
from pydantic import Field
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

AGENTIC_DIR = Path(__file__).resolve().parents[2]
TEST_SECRET = "test-agent-secret"
TEST_USER_ID = 990001
TOKEN = "tok-sarah-0123456789abcdef"

# ---------------------------------------------------------------------------
# Scripted LLM
# ---------------------------------------------------------------------------


@dataclass
class Call:
    """A function call step."""

    name: str
    args: dict[str, Any]


@dataclass
class Slow:
    """A step whose model call takes `seconds` (sets `started` when the call begins)."""

    seconds: float
    step: Any
    started: asyncio.Event | None = None


def usage(prompt: int = 120, output: int = 30) -> types.GenerateContentResponseUsageMetadata:
    return types.GenerateContentResponseUsageMetadata(prompt_token_count=prompt, candidates_token_count=output, total_token_count=prompt + output)


class ScriptedLlm(BaseLlm):
    """Replays `script`: str (text answer), Call, list[Call] (parallel calls), Slow(seconds, step) or
    callable(request) -> step."""

    script: list[Any] = Field(default_factory=list)
    requests: list[Any] = Field(default_factory=list)
    with_usage: bool = True

    @property
    def capabilities(self) -> LlmCapabilities:
        return LlmCapabilities(output_schema_and_tools=False)

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False) -> AsyncGenerator[LlmResponse, None]:
        self.requests.append(llm_request)
        step: Any = self.script.pop(0) if self.script else "(script exhausted)"
        if isinstance(step, Slow):
            if step.started is not None:
                step.started.set()
            await asyncio.sleep(step.seconds)
            step = step.step
        if callable(step) and not isinstance(step, Call):
            step = step(llm_request)
        meta = usage() if self.with_usage else None
        if isinstance(step, str):
            if stream:
                for i in range(0, len(step), 7):
                    yield LlmResponse(content=types.ModelContent(parts=[types.Part(text=step[i : i + 7])]), partial=True)
            yield LlmResponse(
                content=types.ModelContent(parts=[types.Part(text=step)]),
                partial=False,
                finish_reason=types.FinishReason.STOP,
                usage_metadata=meta,
                model_version=self.model,
            )
            return
        calls = step if isinstance(step, list) else [step]
        parts = [types.Part(function_call=types.FunctionCall(id=f"call-{uuid.uuid4().hex[:8]}", name=c.name, args=c.args)) for c in calls]
        yield LlmResponse(content=types.ModelContent(parts=parts), partial=False, finish_reason=types.FinishReason.STOP, usage_metadata=meta, model_version=self.model)


def request_text(req: LlmRequest) -> str:
    """Everything a model request would send (system instruction, contents, tool declarations)."""
    chunks = [str(req.config.system_instruction or "")] if req.config else []
    for c in req.contents:
        for p in c.parts or []:
            chunks.append(p.model_dump_json(exclude_none=True))
    if req.config and req.config.tools:
        for t in req.config.tools:
            chunks.append(t.model_dump_json(exclude_none=True) if hasattr(t, "model_dump_json") else str(t))
    return "\n".join(chunks)


# ---------------------------------------------------------------------------
# Fake backend (AFDD script API)
# ---------------------------------------------------------------------------


class FakeError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


ALL_PERMS = [
    {"group_alias": g, "alias": a}
    for g, a in [
        ("aiassistant", "read"),
        ("aiinsights", "read"),
        ("simulation", "read"),
        ("simulation", "create"),
        ("digitaltwin", "read"),
        ("digitaltwin", "update"),
    ]
]

ASSETS = [
    {"id": 1, "alias": "CH_01", "name": "Chiller CH-01", "type_name": "Centrifugal Chiller", "model_name": "Daikin WSC-126", "location_id": 10, "status": "RUNNING", "status_start_time": "2026-10-10T00:00:00.000Z", "condition": "NORMAL", "condition_start_time": "2026-10-10T00:00:00.000Z"},
    {"id": 2, "alias": "CH_02", "name": "Chiller CH-02", "type_name": "Centrifugal Chiller", "model_name": "Daikin WSC-126", "location_id": 10, "status": "IDLE", "status_start_time": "2026-10-10T00:00:00.000Z", "condition": "WARNING", "condition_start_time": "2026-10-10T01:00:00.000Z"},
    {"id": 4, "alias": "CT_01", "name": "Cooling Tower CT-01", "type_name": "Cooling Tower", "model_name": "BAC S3E-1424", "location_id": 14, "status": "RUNNING", "status_start_time": "2026-10-10T00:00:00.000Z", "condition": "NORMAL", "condition_start_time": "2026-10-10T00:00:00.000Z"},
    {"id": 13, "alias": "CHWS_01", "name": "CHW Plant CHWS-01", "type_name": "Chilled Water System", "model_name": "CUB Chilled Water Plant Controller", "location_id": 10, "status": "RUNNING", "status_start_time": "2026-10-10T00:00:00.000Z", "condition": "NORMAL", "condition_start_time": "2026-10-10T00:00:00.000Z"},
]

PARAMS = [
    {"id": 101, "alias": "chwst", "name": "CHW Leaving Temp", "unit": "°C", "data_type": "number", "scale": 2, "value": 6.4987, "timestamp": "2026-10-10T06:00:00.000Z", "condition": "NORMAL", "warning": {"lower": None, "upper": 7.5}, "critical": {"lower": None, "upper": 8.5}, "spc": {}, "control": None},
    {"id": 102, "alias": "power_kw", "name": "Power", "unit": "kW", "data_type": "number", "scale": 0, "value": 512.4, "timestamp": "2026-10-10T06:00:00.000Z", "condition": "NORMAL", "warning": {}, "critical": {}, "spc": {}, "control": None},
    {"id": 103, "alias": "chwst_sp", "name": "CHWST Set Point", "unit": "°C", "data_type": "number", "scale": 1, "value": 6.5, "timestamp": "2026-10-10T06:00:00.000Z", "condition": "NORMAL", "warning": {}, "critical": {}, "spc": {}, "control": {"min": 5.5, "max": 8.0, "step": 0.1, "options": None, "mode": None, "target": "chwst"}},
    {"id": 104, "alias": "run_mode", "name": "Run Mode", "unit": "", "data_type": "number", "scale": 0, "value": 0, "timestamp": "2026-10-10T06:00:00.000Z", "condition": "NORMAL", "warning": {}, "critical": {}, "spc": {}, "control": {"options": [{"value": 0, "label": "Auto"}, {"value": 1, "label": "On"}, {"value": 2, "label": "Off"}]}},
]


def trend_payload(minutes: int) -> dict[str, Any]:
    points = [{"timestamp": f"2026-10-10T{h:02d}:{m:02d}:00.000Z", "value": 6.5 + 0.01 * ((h * 60 + m) % 7), "running": True} for h in range(4) for m in range(0, 60, 5)]
    return {
        "id": 101,
        "name": "CHW Leaving Temp",
        "unit": "°C",
        "minutes": minutes,
        "data": points,
        "warning": {"lower": None, "upper": 7.5},
        "critical": {"lower": None, "upper": 8.5},
        "spc": {"ucl": 6.6, "lcl": 6.4, "center_line": 6.5, "sigma": 0.03, "usl": None, "lsl": None, "computed": True},
        "stats": {"count": len(points), "mean": 6.53, "std_dev": 0.02, "min": 6.5, "max": 6.56, "cp": None, "cpk": None, "excluded": 0},
        "violations": [{"timestamp": "2026-10-10T03:55:00.000Z", "value": 6.56, "rules": ["beyond_3_sigma"]}],
    }


class FakeBackend:
    """The AFDD script API over HTTP, for tests (runs uvicorn in a thread)."""

    def __init__(self) -> None:
        self.users: dict[str, dict[str, Any]] = {}
        self.calls: list[dict[str, Any]] = []
        self.handlers: dict[str, Callable[[dict[str, Any], str], Any]] = {}
        self.jobs: dict[int, dict[str, Any]] = {}
        self.port = free_port()
        self._server: uvicorn.Server | None = None
        self._thread: threading.Thread | None = None
        self._install_defaults()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def add_user(
        self, token: str, *, role: str = "basic", permissions: list[dict[str, str]] | None = None, locations: list[int] | None = None,
        name: str = "Sarah Lim", username: str | None = None, user_id: int | None = None,
    ) -> None:  # fmt: skip
        self.users[token] = {
            "id": user_id or (TEST_USER_ID if token == TOKEN else TEST_USER_ID + 100 + len(self.users)),
            "username": username or ("sarah_lim_test" if token == TOKEN else token.removeprefix("tok-") + "_test"),
            "full_name": name,
            "employee_id": None,
            "photo": "",
            "role": role,
            "assignedLocations": locations or [],
            "permissions": ALL_PERMS if permissions is None else permissions,
        }

    def calls_to(self, script: str) -> list[dict[str, Any]]:
        return [c for c in self.calls if c["script"] == script]

    def _install_defaults(self) -> None:
        h = self.handlers
        h["user/getuserdetails"] = lambda body, tok: self.users[tok]
        h["digital_twin/getassets"] = lambda body, tok: ASSETS
        h["digital_twin/getassetparameters"] = lambda body, tok: PARAMS if int(body.get("asset_id", 0)) in (1, 2) else PARAMS[:2]
        h["digital_twin/getparametertrend"] = lambda body, tok: trend_payload(int(body.get("minutes", 60)))
        h["digital_twin/getliveoverview"] = lambda body, tok: [
            {
                **{k: a[k] for k in ("id", "alias", "name", "type_name", "status", "status_start_time", "condition", "condition_start_time")},
                "params": {
                    "chws_temp": {"value": 6.56, "unit": "°C", "scale": 2, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                    "plant_kw": {"value": 1450.2, "unit": "kW", "scale": 0, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                    "cooling_load": {"value": 5200.0, "unit": "kW", "scale": 0, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                    "chws_reset_mode": {"value": 1, "unit": "", "scale": 0, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                    "run_mode": {"value": 0, "unit": "", "scale": 0, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                    "chwst": {"value": 6.5, "unit": "°C", "scale": 2, "condition": "NORMAL", "timestamp": "2026-10-10T06:00:00.000Z"},
                },
            }
            for a in ASSETS
            if a["alias"] in [x.upper() for x in (body.get("aliases") or [])]
        ]
        h["digital_twin/getactivefaults"] = lambda body, tok: [{"alias": "CT_02", "kind": "ct_fouling", "value": 0.8, "sev": 0.2, "start": "2026-10-10T00:00:00Z"}]
        h["alarm/getalarmcount"] = lambda body, tok: {"totalWarning": 3, "totalCritical": 1, "totalAbnormal": 2, "totalAlarm": 6}
        h["alarm/getalarmbyassetid"] = lambda body, tok: {"total": 1, "data": [{"assetName": "Chiller CH-02", "timestamp": "2026-10-10T05:00:00.000Z", "alarmType": "ALARM", "alarmStatus": "WARNING", "alarmMessage": "CHL-008 - CHWST deviation"}]}
        h["ai_insights/getoverview"] = lambda body, tok: {"recommendation": None, "faults": {"active": 1, "last24h": 2}}
        h["ai_insights/getrecommendations"] = lambda body, tok: [{"id": 7, "status": "VALIDATED", "current": {"setpoints": {"chws_sp": 6.5}}, "recommended": {"setpoints": {"chws_sp": 7.0}}, "savings_kw": 42.0, "savings_pct": 2.9, "actions": [], "valid_until": "2026-10-10T08:00:00.000Z"}]
        h["simulation/getfaultcatalog"] = lambda body, tok: [{"kind": "ct_fouling", "asset_kind": "tower", "assets": ["CT_01", "CT_02", "CT_03"], "intensities": [0.95, 0.8, 0.65], "unit": "", "label": "Tower fouling", "code": "CT-014"}]
        h["simulation/propose"] = self._propose
        h["simulation/getjob"] = self._getjob
        h["simulation/listjobs"] = lambda body, tok: {"total": len(self.jobs), "jobs": list(self.jobs.values())}
        h["simulation/getevents"] = lambda body, tok: {"total": 1, "events": [{"id": 1, "variant": "baseline", "ts": "2026-10-10T07:00:00.000Z", "asset_alias": "CH_01", "kind": "alarm", "code": "CHL-008", "severity": "WARNING", "message": "deviation", "value": 1.2, "details": {}}]}

    def _propose(self, body: dict[str, Any], tok: str) -> dict[str, Any]:
        if "simulation.read" not in {f"{p['group_alias']}.{p['alias']}" for p in self.users[tok]["permissions"]}:
            raise FakeError(403, "Permission denied")
        job_id = 500 + len(self.jobs) + 1
        job = {
            "id": job_id,
            "kind": "scenario",
            "title": body["title"],
            "origin": "agent",
            "status": "PROPOSED",
            "progress": 0,
            "rationale": body.get("rationale"),
            "requested_by": TEST_USER_ID,
            "requested_by_name": self.users[tok]["full_name"],
            "approved_by_name": None,
            "approved_at": None,
            "created_at": "2026-10-10T06:00:00.000Z",
            "started_at": None,
            "finished_at": None,
            "sim_start": None,
            "sim_end": None,
            "step_s": 60,
            "recommendation_id": None,
            "error": None,
            "variants": [v.get("name") for v in body["spec"].get("variants") or []],
            "summary": {},
        }
        self.jobs[job_id] = job
        return job

    def _getjob(self, body: dict[str, Any], tok: str) -> dict[str, Any]:
        job = self.jobs.get(int(body.get("job_id", 0)))
        if job is None:
            raise FakeError(404, "Job not found")
        return job

    def app(self) -> Starlette:
        async def execute(request: Request) -> JSONResponse:
            script = request.path_params["script"].strip("/").lower()
            token = request.headers.get("nodelinx-exposed-session-id", "")
            try:
                body = await request.json()
            except (ValueError, json.JSONDecodeError):
                body = {}
            self.calls.append({"script": script, "token": token, "origin": request.headers.get("x-afdd-origin"), "body": body})
            if token not in self.users:
                return JSONResponse({"data": None, "error": "Unauthorized, please re-login", "message": "Unauthorized, please re-login"}, status_code=401)
            handler = self.handlers.get(script)
            if handler is None:
                return JSONResponse({"data": None, "error": f"unknown script {script}"}, status_code=404)
            try:
                return JSONResponse({"data": handler(body, token), "error": None})
            except FakeError as err:
                return JSONResponse({"data": None, "error": err.message, "message": err.message}, status_code=err.status)

        return Starlette(routes=[Route("/api/js-service/exposed/execute/{org}/{project}/{script:path}", execute, methods=["POST", "GET"])])

    def start(self) -> None:
        config = uvicorn.Config(self.app(), host="127.0.0.1", port=self.port, log_level="warning", lifespan="off")
        self._server = uvicorn.Server(config)

        def run() -> None:
            loop = asyncio.SelectorEventLoop() if sys.platform == "win32" else asyncio.new_event_loop()
            asyncio.set_event_loop(loop)
            loop.run_until_complete(self._server.serve())  # type: ignore[union-attr]
            loop.close()

        self._thread = threading.Thread(target=run, name="fake-backend", daemon=True)
        self._thread.start()
        deadline = time.monotonic() + 15
        while not self._server.started:
            if time.monotonic() > deadline:
                raise RuntimeError("fake backend did not start")
            time.sleep(0.05)

    def stop(self) -> None:
        if self._server is not None:
            self._server.should_exit = True
        if self._thread is not None:
            self._thread.join(timeout=10)


# ---------------------------------------------------------------------------
# MCP server processes
# ---------------------------------------------------------------------------


@dataclass
class McpProcess:
    name: str
    port: int
    proc: subprocess.Popen[bytes]
    log_path: Path

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/mcp"

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def mcp_env(backend_url: str, ports: dict[str, int]) -> dict[str, str]:
    env = dict(os.environ)
    env.update(
        {
            "AFDD_API_URL": backend_url,
            "AGENT_SHARED_SECRET": TEST_SECRET,
            "DATABASE_URL": os.environ.get("DATABASE_URL", "postgresql://afdd_ai:afdd-ai-dev@127.0.0.1:5432/afdd"),
            "DATABASE_URL_RO": os.environ.get("DATABASE_URL_RO", "postgresql://afdd_ai_ro:afdd-ai-ro-dev@127.0.0.1:5432/afdd"),
            "MCP_DB_PORT": str(ports.get("db", 0)),
            "MCP_TWIN_PORT": str(ports.get("twin", 0)),
            "GOOGLE_API_KEY": "",
            "GEMINI_API_KEY": "",
            "LOG_LEVEL": "WARNING",
            "PYTHONUNBUFFERED": "1",
        }
    )
    return env


def start_mcp_server(name: str, backend_url: str, log_dir: Path) -> McpProcess:
    port = free_port()
    log_path = log_dir / f"mcp-{name}.log"
    log_file = open(log_path, "wb")  # noqa: SIM115 - closed with the process
    flags = subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0
    proc = subprocess.Popen(
        [sys.executable, "-m", "afdd_ai.mcp.serve", name],
        cwd=AGENTIC_DIR,
        env=mcp_env(backend_url, {name: port}),
        stdout=log_file,
        stderr=subprocess.STDOUT,
        creationflags=flags,
    )
    deadline = time.monotonic() + 60
    while True:
        if proc.poll() is not None:
            raise RuntimeError(f"MCP server {name} exited: {log_path.read_text(errors='replace')[-2000:]}")
        try:
            if httpx.get(f"http://127.0.0.1:{port}/healthz", timeout=1.0).status_code == 200:
                break
        except httpx.HTTPError:
            pass
        if time.monotonic() > deadline:
            proc.kill()
            raise RuntimeError(f"MCP server {name} did not start: {log_path.read_text(errors='replace')[-2000:]}")
        time.sleep(0.3)
    return McpProcess(name, port, proc, log_path)


# ---------------------------------------------------------------------------
# Scratch database for ADK session tables
# ---------------------------------------------------------------------------

ADMIN_DB_URL = os.environ.get("AFDD_TEST_ADMIN_DB_URL", "postgresql://afdd:afdd@127.0.0.1:5432/postgres")
AI_ROLE_URL = "postgresql://afdd_ai:afdd-ai-dev@127.0.0.1:5432/{db}"


def create_scratch_database() -> str:
    """Create a database owned by afdd_ai with schema `agent`; returns its name."""
    import psycopg

    name = f"afdd_agent_test_{uuid.uuid4().hex[:8]}"
    with psycopg.connect(ADMIN_DB_URL, autocommit=True) as conn:
        conn.execute(f'CREATE DATABASE "{name}" OWNER afdd_ai')
    with psycopg.connect(AI_ROLE_URL.format(db=name), autocommit=True) as conn:
        conn.execute("CREATE SCHEMA agent")
    return name


def drop_scratch_database(name: str) -> None:
    import psycopg

    with psycopg.connect(ADMIN_DB_URL, autocommit=True) as conn:
        conn.execute(f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')


def parse_sse(raw: str) -> list[tuple[str, Any]]:
    """[(event, data)] of an SSE body; ping comments become ("ping", None)."""
    out: list[tuple[str, Any]] = []
    for frame in raw.split("\n\n"):
        if not frame.strip():
            continue
        if frame.startswith(":"):
            out.append(("ping", None))
            continue
        event, data = "message", None
        for line in frame.split("\n"):
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: "):
                data = json.loads(line[6:])
        out.append((event, data))
    return out
