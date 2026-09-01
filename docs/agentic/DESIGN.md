# Agentic AI for the CUB chiller plant: design and contracts

Status: implementation contract for branch `feat/agentic_implementation` (backend and frontend), 2026-10-10.
Source requirement: `agentic_requirement/HVAC Chiller Plant Asset & Equipment Monitoring System Project Prompt (2).md`
and the LBNL FDD Data Sets (chiller plant) reference.

This document is the single source of truth for every implementer. Names, columns, endpoints, JSON
shapes and event types below are **contracts**: do not rename them. If a contract is impossible to
honour, stop and report it rather than inventing a variant.

---

## 0. Decisions and improvements over the brief

| Brief | Decision | Why |
|---|---|---|
| LBNL sensor names, °F / GPM / inH2O | Keep the plant in **SI** and our own tags; LBNL names live in `analytics.point_map` and in the docs/agent prompts | The live plant, history, thresholds and UI are SI (Singapore fab). A mapping layer is the industry pattern (Brick / Haystack style) and keeps both vocabularies |
| LBNL plant (secondary loop, 3-way valve) | **Extend the simulated plant**: plant supervisory set points, variable-primary riser DP control, condenser 3-way tower bypass valve, tower CW leaving-temperature PI loop, load-based staging, MAU coil coupling, injectable LBNL faults | Every LBNL fault type then maps onto real tags of our plant (user decision) |
| Time-partitioned sensor tables | `ts.parameter_values` becomes **monthly range partitions**; retention drops partitions | Spec; the old `ctid` retention delete is unsafe on partitions and is replaced |
| pgvector | Same Alpine Postgres 16.15 image + pgvector **0.8.6** compiled in (`OPTFLAGS=""`), same data volume | Debian images change collation behaviour on the existing musl cluster |
| Surrogate trained on history | Surrogate trained mainly on a **design of experiments run on the digital twin** (Latin hypercube over set points × weather × load), plus recent live samples | History has almost no set-point variation; physics-generated data is the standard way to train plant surrogates |
| "Runs every 1–2 hours" | Optimizer every **60 min** (configurable 60–120), surrogate retrained weekly or on drift | Spec; literature shows cadence beyond ~1 h adds little |
| Candidate validated on the twin | Every recommendation is **validated by a 2-variant twin scenario** (baseline vs recommended, 2 h ahead, same seed) before it is shown as VALIDATED | Spec, made deterministic and auditable |
| Each fault produces a maintenance instruction (RAG) | Fault → **engine alarm with an LBNL alarm code** (alarm report, condition, notifications, early-warning work order with the code's SOPs) **plus** a RAG-grounded instruction written by Gemini Flash with citations | Reuses the existing CMMS chain; RAG text is grounded and cited |
| Agent tools via the app API | The agent calls the backend **with the end user's own session**, so permissions and location scope apply to the LLM exactly as to the user. Human-only actions (writes, approvals) are refused for agent-origin calls | Least privilege; prevents prompt-injection escalation |
| HITL for simulations | Agent can only **propose** (`PROPOSED`); a human approves in the UI (`QUEUED`). Approval endpoint refuses agent-origin calls | Spec requirement, enforced server side, auditable |
| MCP for DB and simulator | Two MCP servers (streamable HTTP): `afdd-db` (parameterised, scoped, read-only role, row limits, no free SQL) and `afdd-twin` (simulation job API) | Spec; parameterised tools instead of raw SQL avoid data exfiltration |
| Gemini 3.x (was 2.5 Flash / Pro) | Model ids from config: `gemini-3.5-flash-lite` (fast; same price as 2.5 Flash), `gemini-3.1-pro-preview` (deep analysis), embeddings `gemini-embedding-001` @768d. Gemini 2.5 answers "no longer available to new users" for this key (checked 2026-10-10). Gemini 3 uses thinking levels (budget mapped to minimal / low / medium / high) and the default temperature. Per-turn LLM-call cap, **daily USD budget** with a per-user quota and an analytics reserve; usage accounting prices implicit-cache hits at the cache rate. Volatile prompt parts (plant time) go in the newest message so the cached prefix stays stable | API key budget is small (about US$4) |
| Session memory | ADK `DatabaseSessionService` on PostgreSQL schema `agent` (asyncpg) | Spec |
| Charts | Tools return **validated ECharts options** (server-built from real data); the LLM never invents data series | Prevents hallucinated numbers |

Multi-site / multi-unit: a `core.sites` table, `site_code` on every analytics / RAG row, unit = equipment alias,
asset type = `asset.types.name`. One site today: `SGF1` "Singapore Fab 1 campus".

---

## 1. Architecture

```
                     browser (nlam)                      
  floating AI assistant · AI Insights page · Simulation page · Digital twin (live + replay)
          │ /api/js-service/... (JSON)        │ /api/agent/* (SSE)   │ /api/ai/documents/{id}/file
          ▼                                   ▼                      ▼
  ┌──────────────── backend (Go, afdd-backend) ────────────────────────────────────────┐
  │ auth/session · permissions · location scope · human-only guard (X-AFDD-Origin)    │
  │ ai_insights/* · ai_documents/* · simulation/* · digital_twin/injectfault          │
  │ /api/agent/* reverse proxy (SSE, X-Agent-Secret + user identity + user session)   │
  │ engine: + external FDD alarms from analytics.faults (codes → alarm events, orders)│
  │ scheduler: + partition maintenance, partition-drop retention                        │
  └───────────────┬───────────────────────────────┬────────────────────────────────────┘
        MQTT cmd   │                               │ HTTP (user session)
  ┌──────────────▼────────────┐       ┌─────────────▼─────────────────────────────────┐
  │ simulator (Go)             │       │ ai-agent (Python, ADK 2.11, FastAPI)            │
  │ live plant (+ supervisory  │       │ root → insight_agent, twin_agent (+ analyst)    │
  │ loops, TWV, LBNL faults)   │       │ tools: backend API (user token), RAG, charts    │
  │ sim job runner (clones)    │       │ McpToolset → ai-mcp (header_provider = token)   │
  │ writes sim.* only for jobs │       │ sessions: agent schema (asyncpg)                │
  └──────────────┬────────────┘       └─────────────┬─────────────────────────────────┘
                 │ LISTEN sim_jobs                   │ streamable HTTP MCP
  ┌──────────────▼──────────────────────┐  ┌────────▼──────────────────────────────┐
  │ ai-analytics (Python worker)          │  │ ai-mcp (Python): afdd-db :8101,         │
  │ anomaly detection (1 min)             │  │ afdd-twin :8102 (validate token via     │
  │ LBNL fault rules (5 min) + RAG instr. │  │ backend, scope SQL, read-only role)     │
  │ optimizer (60 min) + twin validation  │  └─────────────────────────────────────────┘
  │ RAG corpus generation + ingestion     │
  │ sim post-analysis (ANALYZING→done)    │
  └───────────────────────────────────────┘
            PostgreSQL 16 + pgvector (schemas core asset dh dm ts + rag analytics sim agent) · MinIO (afdd, afdd-docs)
```

Compose services added: `ai-agent`, `ai-mcp`, `ai-analytics` (one image `afdd-ai:latest`, built from
`backend/agentic`), and `postgres` is now built from `backend/docker/postgres`. Internal ports, not
published: ai-agent 8000, ai-mcp 8101/8102. Python services connect to Postgres as role `afdd_ai`
(MCP DB server as `afdd_ai_ro`).

---

## 2. Plant extension (simulator, `cmd/simulator`)

All plant-level supervisory points live on **CHWS_01** (the BMS plant controller now covers CHW and CW).
Model name stays "CUB Chilled Water Plant Controller" (seeder looks models up by name); its
description changes to "Chiller plant controller (CHW and CW loops)".

### 2.1 New parameters

CHWS_01 (`chwSystemModel`) — every one must be written to `v` in **all** branches of `chwSystem`:

| alias | name | unit | scale | writable / notes |
|---|---|---|---|---|
| `oa_temp` | Outdoor Air Temp | °C | 1 | LBNL OA_TEMP |
| `wet_bulb` | Outdoor Wet Bulb Temp | °C | 2 | LBNL OA_TEMP_WB |
| `chws_reset_mode` | CHW Set Point Mode | | 0 | `.Modes("chws_temp","Reset","Manual")` |
| `chws_sp` | CHW Supply Set Point | °C | 1 | `.SP(5.5, 8.0, 0.1, "chws_reset_mode", "chws_temp")`; in Reset mode computed hourly (2.2) |
| `staging_mode` | Chiller Staging Mode | | 0 | `.Modes("chillers_running","Load","Manual")` |
| `chillers_sp` | Duty Chillers Set Point | | 0 | `.SP(1, 3, 1, "staging_mode", "chillers_running").Def(2)` |
| `approach_sp` | Tower Approach Set Point | K | 1 | `.SP(2.0, 8.0, 0.1, "", "cwst_sp").Def(3.5)`; default 3.5 |
| `cw_min_temp` | Min Condenser Water Temp | °C | 1 | `.SP(15.0, 28.0, 0.5, "", "cw_supply_temp").Def(20.0)` |
| `cwst_sp` | Tower Leaving Water Set Point | °C | 2 | read-only = max(wet_bulb + approach_sp, cw_min_temp) (LBNL eq. 3) |
| `tower_supply_temp` | Tower Leaving Water Header Temp | °C | 2 | flow-weighted **true** outlet of running towers (header sensor upstream of the valve) |
| `cw_supply_temp` | CW Supply Temp | °C | 2 | after the 3-way valve mixing, entering chillers (LBNL CDWL_SW_TEMP) |
| `cw_return_temp` | CW Return Temp | °C | 2 | flow-weighted chiller condenser leaving (CDWL_RW_TEMP) |
| `cw_flow` | Condenser Water Flow | m³/h | 0 | sum of running CW pump flows |
| `fan_speed_cmd` | Tower Fan Speed Command | % | 1 | common PI output to all Auto towers (CT_FAN_SPD_CTRL) |
| `twv_min_pos` | 3-Way Valve Min Position | % | 0 | `.SP(0, 50, 1, "", "twv_pos").Def(0)` |
| `twv_cmd` | 3-Way Valve Command | % | 1 | TWV controller output (max(min_pos, PI to keep cw_supply ≥ cw_min_temp)) |
| `twv_pos` | 3-Way Valve Position | % | 1 | actual position (= cmd unless stuck); LBNL TWV_CTRL |
| `riser_dp_sp` | Riser DP Set Point | kPa | 0 | `.SP(80, 160, 1, "", "riser_dp").Def(120)` (LBNL CWL_SEC_DPSPTS analog) |
| `chwp_speed_cmd` | CHW Pump Speed Command | Hz | 1 | common riser-DP PI output to Auto CHW pumps |

Existing CHWS_01 points keep their meaning; `riser_dp` becomes the **measured** (possibly biased) DP.

Other models:

| model | new alias | name | unit | scale |
|---|---|---|---|---|
| chiller | `cond_flow` | Condenser Water Flow | m³/h | 0 |
| tower | `fan_power` | Fan Power | kW | 1 |
| CHW / CW pump (`pumpModel`) | `power_kw` | Input Power | kW | 1 |

### 2.2 Supervisory control (new `Plant.supervise`, called in `Step` after `updateDuty`/weather, before `Values`)

Controller state is in a new `Asset.Cmd map[string]float64` (commands from the plant controller), kept
separate from operator `Ctl`. PI integrators live in `a.smooth` (velocity form, anti-windup).

* **CHWST reset** (LBNL eq. 2, adapted to the tropics): in Reset mode `chws_sp = clamp(7.0 − (oa − 26)/(31 − 26) × 1.0, 6.0, 7.0)`, recomputed **once per simulated hour** (held between). In Manual the operator value. When the plant set point changes it is **pushed to every chiller's `Ctl["chwst_sp"]`** (supervisory write; a local operator write to one chiller lasts until the next push).
* **Staging**: Load mode stages up when cooling load > n × 3517 kW × 0.95 for 10 min, down when load < (n − 1) × 3517 × 0.80 for 30 min (min 1, max 3; LBNL ξ = 0.95, wait 30 min; shorter up-wait for safety). Manual mode uses `chillers_sp`. The weekly N+1 rotation still chooses **which** units; `run_mode` On/Off keeps priority. Default `staging_mode` = Load.
* **Tower fan PI** on `cwst_sp` vs the **average of the running towers' measured `cwst`** (so a tower sensor bias moves the loop, as in LBNL). All Auto towers run at `fan_speed_cmd` (LBNL: same speed). Manual towers keep `fan_speed_sp`. The CW loop (PI + tower + valve) runs in **internal sub-steps of 5 s** inside `Step(dt)` so behaviour does not depend on dt (history uses dt = 60 s).
* **Tower physics**: approach from heat rejection and airflow, e.g. `app = app0 × (Q_rej/Q_design)^0.6 × (100/max(speed,10))^0.8 / capacity^0.7` with `capacity` = 1 (or the `ct_fouling` intensity), calibrated so that at design (wb 25.5, load 0.74, 2 chillers) the PI settles near 60–70 % fan speed with approach_sp 3.5 K. Tower inlet = `cw_return_temp` (closed loop, no more open-loop `cwrt`). Fan power ∝ speed³ (rated 30 kW at 100 %); `fan_current` keeps its existing scale relation to speed.
* **3-way valve**: `twv_cmd = max(twv_min_pos, PI)` holding `cw_supply_temp ≥ cw_min_temp`; mixing `b = pos/100` (+ leak), `cw_supply = (1 − b) × tower_supply + b × cw_return`. Chillers' `cwet` = `cw_supply_temp` (replaces `env.CWS` mean of tower cwst). In Singapore the valve normally sits at its minimum.
* **Variable primary riser DP**: Auto CHW pumps run at `chwp_speed_cmd` from a PI holding **measured** `riser_dp` at `riser_dp_sp`. Hydraulics: `dp_true ≈ dp0 × k² × g(coil valves) − r × Q²` (monotonic: rises with speed², falls as coil valves open), calibrated so design operation settles near 44–46 Hz at 120 kPa. Chiller CHW flow follows its pump (existing `ChwK`). The min-flow bypass opens when a chiller's flow falls under 60 % of design. Re-threshold PMP-004 for CHW pumps to a flow that normal VPF operation never crosses (≈ 330 m³/h).
* **MAU coupling** (gives the optimizer a real trade-off): MAU `chw_valve = need × (1+coil) × (1 + 0.22 × (chws − 6.5)) × sqrt(120 / max(riser_dp, 40))`; if the uncapped demand exceeds 100 % the valve saturates and `sat` rises by `0.6 K` per 10 % of unmet demand (AHU-003 then fires). `env` gets last step's `chws_temp` and `riser_dp`.

### 2.3 Sensor reading vs truth

`Values`/`chwSystem` compute **truth**; a new `sense` step applies sensor faults to produce the
**measured** map that is published, evaluated, logged and fed to the controllers. Truth keeps feeding
the physics (`a.last` stays truth, add `a.meas`). `Step` returns measured; `p.Truth` holds truth (used as
labels in simulation jobs).

### 2.4 LBNL faults (fixed intensity, `Fault.Value`)

`Fault` gets `Value float64 json:"value,omitempty"`; `FaultSpec` gets `Manual bool` (never injected
randomly live, never ramped by `doRun`'s min-rate bump) and `Intensities []float64` (documented values).

| kind | asset | intensities (LBNL) | effect |
|---|---|---|---|
| `chwst_bias` | CH_0n | −2, −1, +1, +2 K | measured `chwst` = true + v; unit controls its sensor, so true = asp − v |
| `cwst_bias` | CT_0n | −2, −1, +1, +2 K | measured tower `cwst` = true + v (moves the fan PI average) |
| `ct_fouling` | CT_0n | 0.95, 0.80, 0.65 | tower heat-transfer capacity multiplier |
| `riser_dp_bias` | CHWS_01 | −0.20, −0.10, +0.10, +0.20 | measured `riser_dp` = true × (1 + v) |
| `twv_leak` | CHWS_01 | 0.25, 0.50, 0.75 | effective valve opening = max(pos, v) while `twv_pos` reports the command |
| `twv_stuck` | CHWS_01 | 0.50, 0.75 | `twv_pos` fixed at v × 100 regardless of `twv_cmd` |
| `cw_pi_tuning` | CHWS_01 | 1 (faulted) | tower PI gains detuned (Kp × 6, Ti ÷ 4) → sustained hunting (fan cmd reversals > 6/h, CWST p-p > 0.5 K) |

On-demand injection: MQTT command payload `{"request_id", "fault": kind, "value": v, "action": "inject"|"clear", "user"}`
on the existing `…/cmd/<ALIAS>` topic, handled by `Live.applyCommand`; the next publish shows the effect.
Random live injection and history planning only use non-Manual faults, **except** history plans 1–2
labelled episodes per LBNL kind (2.6).

### 2.5 Alarm codes, SOPs, relations (seeded; `seedVersion` 6)

New issue category "Sensors & Controls": `SEN-01` Sensor Bias / Calibration, `CTL-01` Control Loop Tuning,
`VLV-01` Valve Leakage / Stuck. New alarm codes (FDD rule codes, see 6.3):

| code | name | issue |
|---|---|---|
| CHL-011 | Chiller CHW Leaving Temperature Sensor Bias | SEN-01 |
| CT-013 | Tower Leaving Water Temperature Sensor Bias | SEN-01 |
| CT-014 | Cooling Tower Heat Exchanger Fouling | THR-02 |
| CHS-005 | Riser Differential Pressure Sensor Bias | SEN-01 |
| CHS-006 | Condenser 3-Way Valve Leakage | VLV-01 |
| CHS-007 | Condenser 3-Way Valve Stuck | VLV-01 |
| CHS-008 | CW Supply Temperature Loop Hunting (PI Tuning) | CTL-01 |

Each code gets a Diagnostic and a Repair SOP in `content.go` (Markdown, no `< > { }`), mapped to the
model (`instructionModels`). Each gets a self issue relation ("<name> (FDD)" source code → target same
code) on its model so an external FDD alarm creates a corrective early-warning order with the SOPs.
`FaultSpec.Codes` of each LBNL fault include its code so completing the order repairs it.

### 2.6 History (`historyVersion` 4 — regenerates all demo history; tell the user)

* Physics above is used for the whole window.
* 1–2 episodes per LBNL fault kind (non-overlapping per asset, 6 h – 3 days), each followed by an FDD
  corrective order (issue = rule code, detected 1–4 h after onset) that is worked, noted and closed; the
  fault clears on completion.
* Occasional operator set-point changes (`chws_sp`, `approach_sp`, `riser_dp_sp`, `chillers_sp`, modes;
  ~2 per week) applied at their time **and** stored as `asset.control_requests` rows (APPLIED, by demo
  users) — "historical parameter settings".
* Before COPY, call `SELECT ts.ensure_parameter_values_partitions($from, $to)` for the window.

### 2.7 LBNL point map (seeded into `analytics.point_map` by migration 0005)

`asset_pattern` uses `{n}` for the unit index.

| LBNL | asset | param | LBNL unit | SI unit |
|---|---|---|---|---|
| CWL_SEC_LOAD | CHWS_01 | cooling_load | W | kW |
| CWL_SEC_RW_TEMP / CWL_PRI_RW_TEMP | CHWS_01 | chwr_temp | °F | °C |
| CWL_SEC_SW_TEMP / CWL_PRI_SW_TEMP | CHWS_01 | chws_temp | °F | °C |
| CWL_SEC_CW_FLOW | CHWS_01 | fab_flow | GPM | m³/h |
| CWL_PRI_CW_FLOW | CHWS_01 | plant_flow | GPM | m³/h |
| CWL_PRI_SW_TEMPSPT | CHWS_01 | chws_sp | °F | °C |
| TWV_CTRL | CHWS_01 | twv_pos | 0-1 | % |
| CT_SW_TEMPSPT | CHWS_01 | cwst_sp | °F | °C |
| CDWL_SW_TEMP | CHWS_01 | cw_supply_temp | °F | °C |
| CDWL_RW_TEMP | CHWS_01 | cw_return_temp | °F | °C |
| CDWL_CW_FLOW | CHWS_01 | cw_flow | GPM | m³/h |
| OA_TEMP | CHWS_01 | oa_temp | °F | °C |
| OA_TEMP_WB | CHWS_01 | wet_bulb | °F | °C |
| CWL_SEC_DPSPTS | CHWS_01 | riser_dp_sp | inH2O | kPa |
| CWL_SEC_DP | CHWS_01 | riser_dp | inH2O | kPa |
| CT_STA{n} | CT_0{n} | running | 0/1 | 0/1 |
| CT_RW_TEMP{n} | CT_0{n} | cwrt | °F | °C |
| CT_SW_TEMP{n} | CT_0{n} | cwst | °F | °C |
| CT_FAN_SPD{n} | CT_0{n} | fan_speed | 0-1 | % |
| CT_FAN_SPD_CTRL{n} | CHWS_01 | fan_speed_cmd | 0-1 | % |
| CT_POW{n} | CT_0{n} | fan_power | kW | kW |
| CHL_STA{n} | CH_0{n} | running | 0/1 | 0/1 |
| CHL_COMP_SPD_CTRL{n} | CH_0{n} | load_pct | 0-1 | % |
| CHL_CW_FLOW{n} | CH_0{n} | chw_flow | GPM | m³/h |
| CHL_CD_FLOW{n} | CH_0{n} | cond_flow | GPM | m³/h |
| CHL_RW_TEMP{n} | CH_0{n} | chwrt | °F | °C |
| CHL_SW_TEMP{n} | CH_0{n} | chwst | °F | °C |
| CHL_SWCD_TEMP{n} | CH_0{n} | cwlt | °F | °C |
| CHL_RWCD_TEMP{n} | CH_0{n} | cwet | °F | °C |
| CHL_POW{n} | CH_0{n} | power_kw | kW | kW |
| CDWL_PM_POW{n} | CWP_0{n} | power_kw | kW | kW |
| CWL_PRI_PM_POW{n} | CHWP_0{n} | power_kw | kW | kW |
| CWL_SEC_PM_SPD{n} | CHWP_0{n} | speed | 0-1 | Hz (VPF: the CHW pumps also do the secondary duty) |
| CWL_SEC_PM_STA{n} | CHWP_0{n} | running | 0/1 | 0/1 |
| CWL_SEC_PM_POW{n} | CHWP_0{n} | power_kw | kW | kW |

---

## 3. Data model

Contract migrations (Go runner, one transaction per file):

* `0005_agentic.sql` — **written already** (see the file): pgvector, `core.sites`, schemas `rag`,
  `analytics`, `sim`, `agent`, all tables below, `analytics.point_map` seed, permission groups, service
  roles `afdd_ai` / `afdd_ai_ro` with grants.
* `0006_ts_partitioning.sql` — monthly range partitioning of `ts.parameter_values` (infra agent).

Tables are documented in `0005_agentic.sql` comments. Key status machines:

* `sim.jobs.status`: `PROPOSED → QUEUED` (human approval) `→ RUNNING → ANALYZING → SUCCEEDED | FAILED`;
  `PROPOSED → REJECTED`; `PROPOSED | QUEUED → CANCELLED`. UI submissions and system jobs (optimizer,
  analytics) start at `QUEUED`. Agent proposals always start at `PROPOSED`.
* `analytics.recommendations.status`: `PROPOSED → VALIDATING → VALIDATED | REJECTED | FAILED`;
  `VALIDATED → APPLIED | DISMISSED | EXPIRED` (after `valid_until`).
* `rag.documents.status`: `PENDING → INDEXING → READY | FAILED`.

NOTIFY channels: `sim_jobs` (payload job id; sent on insert/approve and on status changes),
`rag_documents` (payload document id), `optimizer_runs` (payload run id).

Permission groups (migration 0005; frontend enum **must** add them in the same release):
`aiassistant` (read), `aiinsights` (read, update), `simulation` (read, create). Grants in the simulator
`userGroups`: Facility Management all; Control Room aiassistant.read, aiinsights.read, simulation.read +
create; technicians aiassistant.read, aiinsights.read, simulation.read.

Service roles: `afdd_ai` (read asset/ts/dm/selected core columns, read-write rag/analytics/sim/agent) and
`afdd_ai_ro` (read-only, used by the MCP DB server). Passwords are set by the backend at start-up from
`AI_DB_PASSWORD` / `AI_DB_RO_PASSWORD` (`ALTER ROLE … LOGIN PASSWORD`), so no secret is in SQL.

---

## 4. Backend (Go) contracts

All script routes go through `s.handle`. Inputs are JSON/multipart form fields; outputs inside the usual
`{data, error}` envelope; timestamps UTC ISO (`httpx.TS`).

### 4.1 Human-only guard

The agent service sends `X-AFDD-Origin: agent` on every backend call. New helper `c.RequireHuman()`
returns 403 `"This action needs a person: approve it in the application"` when that header is present.
Apply it to: `digital_twin/writeparameter`, `digital_twin/injectfault`, `simulation/approve`,
`simulation/reject`, `ai_insights/applyrecommendation`, `ai_insights/dismissrecommendation`,
`ai_insights/updaterule`, `ai_documents/upload`, `ai_documents/delete`.

### 4.2 Simulation (`internal/api/simulation.go`)

| script | perm | input | output |
|---|---|---|---|
| `simulation/listjobs` | simulation.read | `status?` (CSV), `origin?`, `limit` (≤100, 20), `offset` | `{total, jobs:[JobRow]}` |
| `simulation/getjob` | simulation.read | `job_id` | `JobRow` + `spec`, `summary` |
| `simulation/submit` | simulation.create | `title`, `spec` (JSON), `rationale?` | `JobRow` (status QUEUED, origin ui) |
| `simulation/propose` | simulation.read, **agent origin only** (`X-AFDD-Origin: agent`; people use `submit`) | `title`, `spec`, `rationale`, `agent_session_id?` | `JobRow` (status PROPOSED, origin agent) |
| `simulation/approve` | simulation.create + human | `job_id` | `JobRow` (QUEUED, approved_by/at) |
| `simulation/reject` | simulation.create + human | `job_id`, `reason?` | `JobRow` |
| `simulation/cancel` | simulation.create, human only; the requester or an admin (optimizer / analytics jobs: admins only) | `job_id` | `JobRow` |
| `simulation/getevents` | simulation.read | `job_id`, `variant?`, `kinds?` CSV, `asset?`, `limit` (≤1000, 200), `offset` | `{total, events:[{id, variant, ts, end_ts, asset_alias, kind, code, severity, message, value, details}]}` |
| `simulation/getframes` | simulation.read | `job_id`, `variant?` (default first), `aliases?` CSV | `Frames` (below) |
| `simulation/gettrend` | simulation.read | `job_id`, `variant`, `asset_alias`, `param_alias`, `at` (ISO, default end), `minutes` (5..1440, 60) | same shape as `digital_twin/getparametertrend` |
| `simulation/getfaultcatalog` | simulation.read | — | `[{kind, asset_kind, assets:[alias], intensities:[], unit, label, code}]` |

`JobRow = {id, kind, title, origin, status, progress, rationale, requested_by, requested_by_name, approved_by_name, approved_at, created_at, started_at, finished_at, sim_start, sim_end, step_s, recommendation_id, error, variants:[name]}`.

Spec validation in `submit`/`propose`: kind scenario; duration 10..2880 min; ≤ 3 variants; ≤ 200 actions;
actions reference existing aliases/params/fault kinds (catalog); `start` ≥ now − 5 min and ≤ now + 14 d.
A user may have at most 3 jobs in QUEUED/RUNNING and at most 5 PROPOSED jobs (counted per requesting user). Actions due at the same minute run in this order: set-point reverts, other reverts, mode writes, then the rest in spec order; a set point must come after the mode write that enables it.

`Frames = {job_id, variant, t0, step_s, steps, assets:[{alias, id, name, type_name, model_name}], series:{alias:{param:[number|null]}}, states:[{alias, kind, value, start, end}], events:[event]}`.

`simulation/gettrend` reuses the live trend/SPC logic (Phase I limits, running flags from the sim status
intervals) on the sim series window ending at `at`.

### 4.3 AI insights (`internal/api/ai_insights.go`)

| script | perm | input | output |
|---|---|---|---|
| `ai_insights/getoverview` | aiinsights.read | — | `{recommendation: Rec|null, faults:{active, last24h}, anomalies:{active, last24h}, documents:{ready, pending, failed}, optimizer:{last_run_at, status, next_run_at}, budget:{today_usd, limit_usd}}` |
| `ai_insights/getrecommendations` | aiinsights.read | `status?`, `limit` (≤50, 10) | `[Rec]` |
| `ai_insights/getrecommendation` | aiinsights.read | `id` | `Rec` |
| `ai_insights/applyrecommendation` | aiinsights.update + digitaltwin.update + human | `id` | `{recommendation: Rec, results:[control request rows]}` — applies each action through the same code path as `writeparameter` (mode first, then set points), status APPLIED if all APPLIED |
| `ai_insights/dismissrecommendation` | aiinsights.update + human | `id`, `note?` | `Rec` |
| `ai_insights/runoptimizer` | aiinsights.update | — | `{run_id}` (inserts `analytics.optimizer_runs` PENDING trigger manual + NOTIFY) |
| `ai_insights/getfaults` | aiinsights.read | `status?`, `asset_ids?`, `rule_codes?`, `from?`, `to?`, `limit` (≤200, 50) | `[Fault]` (scope-filtered) |
| `ai_insights/getfault` | aiinsights.read | `id` | `Fault` + `instruction_md`, `instruction_sources`, `evidence` |
| `ai_insights/getanomalies` | aiinsights.read | `status?`, `asset_ids?`, `detectors?`, `from?`, `to?`, `limit` (≤500, 100) | `[Anomaly]` |
| `ai_insights/getrules` | aiinsights.read | — | `[Rule]` |
| `ai_insights/updaterule` | admin + human | `code`, `enabled?`, `params?` (JSON) | `Rule` (version+1) |
| `ai_insights/getpointmap` | aiinsights.read | — | `[point_map rows]` |

`Rec`, `Fault`, `Anomaly`, `Rule` = the table rows (snake_case) plus `asset_alias`, `asset_name` where an
asset is referenced, and for Rec `validation_job` summary `{id, status}`.

### 4.4 Documents (`internal/api/ai_documents.go`)

| script / route | perm | notes |
|---|---|---|
| `ai_documents/list` | aiinsights.read | `doc_type?`, `status?`, `asset_type?`, `unit?`, `search?`, `limit`, `offset` → `{total, documents:[Doc]}` |
| `ai_documents/get` | aiinsights.read | `id` → `Doc` |
| `ai_documents/upload` | aiinsights.update + human | multipart `file` (PDF ≤ 50 MB), `title`, `doc_type`, `asset_type?`, `unit?`, `doc_date?` → `Doc` (PENDING, NOTIFY `rag_documents`) |
| `ai_documents/delete` | aiinsights.update + human | `id` |
| `ai_documents/reindex` | aiinsights.update | `id` → status PENDING + NOTIFY |
| `GET /api/ai/documents/{id}/file` (raw mux route) | session + (aiinsights.read or aiassistant.read) | streams the PDF from the private bucket `afdd-docs` (never `/public`) |

### 4.5 Live fault injection (`internal/api/digital_twin_faults.go`)

`digital_twin/injectfault` (admin + digitaltwin.update + human): `asset_id`, `fault`, `value`, `action`
(inject|clear) → validated against the catalog → MQTT command (2.4) → `{status: "SENT"}`.
`digital_twin/getactivefaults` (digitaltwin.read): reads the simulator state setting
`demo_simulator_state` → `[{alias, kind, value, sev, start}]`.

### 4.6 Agent proxy (raw mux route `/api/agent/`)

Authenticate `Nodelinx-Exposed-Session-Id`; require `aiassistant.read`; reverse-proxy to `AGENT_URL`
(default `http://ai-agent:8000`) stripping `/api/agent`, with headers `X-Agent-Secret`,
`X-AFDD-User-Id`, `X-AFDD-Username`, `X-AFDD-User-Name`, `X-AFDD-Session` (the user's token), and
`X-AFDD-Permissions` (CSV `group.alias`). `httputil.ReverseProxy{FlushInterval: -1}`; lift the write
deadline (`http.NewResponseController(w).SetWriteDeadline(time.Time{})`). Strip any client-supplied
`X-AFDD-*`/`X-Agent-*` headers before adding ours.

### 4.7 Engine: external FDD alarms

Every 30 s the engine loads `SELECT rule_code, asset_id, severity, message FROM analytics.faults WHERE
status='ACTIVE'` (ignore errors when the table is missing). In `evaluateEquipment`, when the unit is not
offline, append `ActiveAlarm{Key: "ext:fdd:"+rule_code, Type: "ALARM", Name: rule_code, Severity, Message}`
to `res.Alarms` → alarm events with `alarm_code_id`, notifications, condition, early-warning orders.

### 4.8 Scheduler / storage / config

* `partition-maintenance` job (24 h, also once at start-up before the engine): ensure partitions from the
  first month of retention to now + 3 months; retention drops whole monthly partitions older than
  `RETENTION_DAYS` (replaces the `ctid` delete for `parameter_values`).
* Docs bucket store: `MINIO_DOCS_BUCKET` (default `afdd-docs`).
* New config: `AGENT_URL`, `AGENT_SHARED_SECRET`, `MINIO_DOCS_BUCKET`, `AI_DB_PASSWORD`, `AI_DB_RO_PASSWORD`.

---

## 5. Simulation jobs (simulator process)

### 5.1 Spec (contract)

```json
{
  "start": "2026-10-11T06:00:00Z",      // null = now; future runs start from the cloned live state
  "duration_min": 240,                  // 10..2880
  "initial": "live",                    // "live" | "nominal"
  "stochastic": false,                  // random faults / trips / comm loss during the run
  "seed": 0,                            // 0 = derived from job id
  "weather": {"dry_bulb_offset": 0.0, "wet_bulb_offset": 0.0},
  "load_scale": 1.0,                    // 0.5..1.3 × plant cooling load
  "actions": [Action],                  // common to every variant
  "variants": [{"name": "baseline", "actions": [Action]}, {"name": "recommended", "actions": [Action]}]
}
Action = {"at_min": 0, "kind": "control"|"fault"|"clear_fault"|"trip"|"offline"|"maintenance"|"release",
          "asset": "CHWS_01", "param": "chws_sp", "fault": "ct_fouling", "value": 7.0, "duration_min": 60}
```

DOE spec (`kind: "doe"`): `{"samples": 400, "settle_min": 12, "avg_min": 5, "seed": 0, "bounds": {"wet_bulb":[22,28],
"dry_bulb_minus_wb":[1,7], "load_frac":[0.5,0.97], "chws_sp":[5.5,8.0], "approach_sp":[2.0,6.0],
"riser_dp_sp":[80,160], "chillers":[1,3], "twv_min_pos":[0,30], "cw_min_temp":[18,24]}}` → one
`sim.doe_samples` row per sample: `inputs` = the sampled values; `outputs` = averages over the last
`avg_min` of `{plant_kw, chiller_kw, chwp_kw, cwp_kw, tower_kw, cooling_load_kw, kw_per_rt, chws_temp,
riser_dp, mau_valve_max, mau_sat_dev_max, cw_supply_temp, fan_speed_cmd, twv_pos, chwp_speed_cmd,
chillers_running, capacity_ok (0/1: chws_temp ≤ chws_sp + 0.5 and mau_valve_max < 98), alarms_critical}`.
Sampling: Latin hypercube from the job seed; weather and load **fixed** per sample (env hook), noise on.

### 5.2 Runner (`cmd/simulator/simjobs.go`)

* Goroutine started from `doRun`; `LISTEN sim_jobs` + poll every 5 s; claim one `QUEUED` job with
  `UPDATE … SET status='RUNNING' … WHERE id = (SELECT id … FOR UPDATE SKIP LOCKED LIMIT 1)`.
* `initial: live` → `Live.Snapshot(seed)` returns a deep `Plant.Clone(seed)` taken **on the Live run
  goroutine** (channel request). `nominal` → `newPlant(seed)` with default controls, no faults.
* Per variant (fresh clone each), step 60 s from `sim_start` to `sim_end`: apply due actions
  (`applyControl`, faults with `Fault{Value}`, trips via `SpikeUntil`, offline, maintenance), evolve
  faults / trips with simulated time, `Step`, `evaluate`, record:
  * `sim.series` (measured arrays for every asset/param; `truth` arrays where truth ≠ measured),
  * `sim.states` status / condition intervals,
  * `sim.events`: status, condition, alarm / abnormal (2-hit open, 4-miss close debounce), diagnosis,
    control, fault_injected / fault_cleared, trip.
* Progress updated at most every 2 s. Never writes `ts.*`, `dm.*`, `core.notifications` or MQTT.
* On success: `summary.variants[v]` physical KPIs (Go part): `energy_kwh, avg_plant_kw, avg_kw_per_rt,
  peak_load_kw, avg_chws_temp, max_chws_temp, min_riser_dp, max_mau_valve, alarm_counts{warning,
  critical}, status_changes, trips`; status `ANALYZING`; NOTIFY `sim_jobs`. On error: `FAILED` + `error`.
* Runtime target: 24 h × 1 variant < 10 s.

### 5.3 Post-analysis (Python `ai-analytics`)

Claims `ANALYZING` jobs, runs the **same** anomaly detectors and FDD rules (batch mode) on the sim
series, inserts `sim.events` (kind `anomaly` / `fault`), adds `summary.variants[v].{anomalies,
faults_detected:[{rule, asset, started_at}], constraint_violations:[{constraint, minutes, worst}],
success, notes}` and job-level `summary.{success, reason, comparison}` (energy saving of non-baseline
variants vs `baseline`), then `SUCCEEDED`. Success = run finished and no critical violation:
`chws_temp > 8.5 °C` for > 10 min, `riser_dp < 80 kPa` for > 10 min, any MAU valve at 100 % for > 15 min,
any trip of a duty chiller.

---

## 6. Python services (`backend/agentic`, image `afdd-ai`)

Package `afdd_ai`. `afdd_ai/common/*` is **written already** (config, db, backend client, Gemini wrapper
with pricing / usage / daily budget, MinIO storage, logging). Use it; do not duplicate it.

### 6.1 `ai-analytics` worker (`python -m afdd_ai.analytics.worker`)

Asyncio service, one process. Tasks:

| task | cadence | module |
|---|---|---|
| anomaly detection | 60 s | `analytics/anomaly.py` |
| FDD rules | 300 s | `analytics/faults.py` + `analytics/rules.py` (defaults) |
| fault instructions (RAG + Gemini Flash) | on new fault | `analytics/instructions.py` |
| optimizer | `OPTIMIZER_INTERVAL_MIN` (60), + manual runs (`optimizer_runs` PENDING) | `analytics/optimizer.py` |
| surrogate training | on start if none, weekly, on drift | `analytics/surrogate.py` (requests a DOE job) |
| rule calibration | on start if not calibrated, weekly | `analytics/calibration.py` (requests a nominal fault-free 72 h scenario job, kind scenario, origin analytics) |
| sim post-analysis | NOTIFY `sim_jobs` / 10 s poll | `analytics/simpost.py` |
| RAG corpus generation | on start (idempotent), daily | `rag/corpus.py` |
| RAG ingestion | NOTIFY `rag_documents` / 30 s poll | `rag/ingest.py` |
| recommendation expiry | 60 s | `analytics/optimizer.py` |

Anomaly detection (production data, `ts.parameter_values`, 1-min): in-memory rolling buffers per
equipment parameter (24 h warm-up from DB, then incremental). Numeric, displayed, non-writable,
non-status params of RUNNING units only. Detectors (config in `analytics/anomaly.py`):
* `range`: outside physical span per unit (°C −10..80, %, kPa ≥ 0 …) → CRITICAL;
* `flatline`: max − min < resolution for ≥ 30 min while running → WARNING;
* `zscore`: robust modified z (median / MAD over 60 min, excluding the current point), |M| > 3.5 for 3
  consecutive points → WARNING (|M| > 6 CRITICAL);
* `ewma`: λ 0.1, L 3, μ/σ from the 24 h running baseline, ≥ 5 of the last 10 beyond limits → WARNING;
* `spc`: `engine`-equivalent rules (beyond 3σ, 2-of-3 beyond 2σ, 7 monotonic) on 15-min means with
  Phase I limits → WARNING.
Open / update / clear rows in `analytics.anomalies` (one ACTIVE row per parameter+detector; clear after
15 min without a hit).

FDD rules (`analytics/rules.py` defaults; thresholds live in `analytics.fault_rules.params` and are
calibrated): steady-state and RUNNING gating everywhere; persistence windows; intensity estimates.

| code | rule (our tags) |
|---|---|
| CHL-011 | per running chiller: heat-balance residual `r = (chwrt − chwst) − load_pct/100·3517/(chw_flow·1.1628)`; and header mixing residual `Σ(f·chwst)/Σf − (chws_temp − 0.06)`; fault if \|EWMA(r)\| > thr (≈0.5 K) for 30 min with consistent sign; intensity ≈ r |
| CT-013 | per running tower: peer residual `cwst_n − mean(cwst_others)` (≥ 2 running) and residual vs `tower_supply_temp`; \|EWMA\| > 0.7 K for 60 min |
| CT-014 | per running tower: approach `cwst − wet_bulb` minus expected approach at the current fan speed and load (fault-free fit) > 1.0 K, fan ≥ 90 %, `cwst − cwst_sp` > 0.5 K, for 2 h |
| CHS-005 | `riser_dp / dp_expected(chwp_speed, plant_flow) − 1`, \|.\| > 0.07 for 60 min (dp_expected = quadratic fit on fault-free data) |
| CHS-006 | `twv_pos ≤ twv_min_pos + 2` and the bypass in excess of the valve position, `f − twv_pos/100` with mixing fraction `f = (cw_supply − tower_supply)/(cw_return − tower_supply)`, > 0.08 for 30 min (valid when cw_return − tower_supply > 2 K); intensity ≈ f (a valve held at a minimum position bypasses that fraction by design) |
| CHS-007 | \|`twv_pos − twv_cmd`\| > 10 % for 30 min, or `twv_pos` range < 1 % over 2 h while `twv_cmd` range > 10 % |
| CHS-008 | reversals of `fan_speed_cmd` (deadband 2 %) > 6 per hour and p-p > 10 % and `tower_supply_temp` p-p > 0.3 K, for 1 h |

New fault → `analytics.faults` ACTIVE (engine raises the alarm, 4.7), then `instructions.py`: RAG search
(query = rule name + asset model + evidence; filters asset_type, alarm code) → Gemini Flash writes a
Markdown maintenance instruction (safety, diagnosis steps, corrective actions, verification) **only from
retrieved sources**, with `[n]` citations → `instruction_md`, `instruction_sources`.

Optimizer: surrogate = LightGBM regressors (`plant_kw`, `chws_temp`, `riser_dp`, `mau_valve_max`,
`mau_sat_dev_max`) on DOE samples (+ live 15-min samples when available); features
`wet_bulb, dry_bulb, load_frac, chws_sp, approach_sp, riser_dp_sp, chillers, twv_min_pos, cw_min_temp`.
Optuna 5 TPE (300 trials, `trial.set_constraint`) for current conditions (15-min means): decision
variables `chws_sp` 5.5–8.0, `approach_sp` 2.0–6.0, `riser_dp_sp` 80–160, `chillers` (feasible
`n × 3517 × 0.95 ≥ load`), `twv_min_pos` 0–30; constraints `chws_temp ≤ 7.5`, `mau_valve_max ≤ 95`,
`riser_dp ≥ 95`, step limits per run (`chws_sp` ± 0.5 K, `approach_sp` ± 1 K, `riser_dp_sp` ± 20 kPa).
Recommend only if predicted saving ≥ max(1 %, 15 kW); then submit a validation scenario (2 h, variants
`baseline` = current set points, `recommended`) → VALIDATED if twin saving > 0 and job success, else
REJECTED. `actions` are the writes to apply (modes to Manual first where needed).

### 6.2 RAG (`afdd_ai/rag`)

* `corpus.py` generates PDFs (fpdf2 + DejaVu Sans, A4) into bucket `afdd-docs` and registers them in
  `rag.documents` (`source='generated'`, `metadata.generator_key` unique):
  * technical manual per model (overview, equipment list, instrument list, set points and ranges, alarm
    limits, alarm codes, diagnostics, SOPs, PM schedule, design data) — `doc_type manual`;
  * FDD reference: LBNL fault types, translated rules, point map (`fdd_reference`);
  * one maintenance report per DONE/CLOSED order (header, people, timeline, problem, evidence values,
    work notes, instructions performed, KPIs) — `maintenance_report`, `doc_date = completed_at`;
  * monthly set-point change logs from `asset.control_requests` + applied recommendations — `setpoint_log`.
  Generated documents are filed at the deepest location covering their equipment (the FDD reference
  site-wide), so everyone who can see that equipment can read them; a placement change moves the
  document and its chunks without re-embedding.
* `ingest.py`: pypdf text per page → heading-aware chunks (~700 tokens, 80 overlap) → `gemini-embedding-001`
  (`RETRIEVAL_DOCUMENT`, title, 768 d, L2-normalised, batches ≤ 50) → `rag.chunks`.
* `search.py`: `search(query, k=6, filters={site_code, asset_type, unit, doc_type, alarm_codes, date_from,
  date_to}, allowed_location_ids=None)` → query embedding (`RETRIEVAL_QUERY`) + HNSW cosine + keyword
  boost (`ts_rank`), scoped like the Go API (document location NULL or inside the user's assigned
  locations and their descendants; equipment / unit checks), returns `[{chunk_id, document_id, title, doc_type, page, heading, content, score,
  unit, asset_type, doc_date}]`.

### 6.3 Agents (`afdd_ai/agent`, service `ai-agent`, `uvicorn afdd_ai.agent.app:app`)

ADK 2.11 (`google-adk[db,mcp]`), Gemini API key. `App(name="afdd_plant_assistant")`.

* `plant_assistant` (root, `AI_MODEL_FAST`, thinking budget 0): greets, answers small talk, routes:
  plant data / alarms / maintenance / documents / charts → `insight_agent`; future scenarios,
  what-if, simulations, recommendations to test → `twin_agent`.
* `insight_agent` (`AI_MODEL_FAST`, thinking 512): tools = backend API tools (below), RAG `search_documents`,
  chart tools, `McpToolset` → `afdd-db`, and `AgentTool(deep_analyst)`.
* `deep_analyst` (`AI_MODEL_PRO`, thinking 1024, `mode single_turn`): long summaries / complex analysis
  over data the insight agent passes in.
* `twin_agent` (`AI_MODEL_FAST`, thinking 512): `McpToolset` → `afdd-twin` (state, catalog, propose,
  status, results), latest recommendation, RAG over past reports, conditions forecast. **Never** claims a
  simulation ran unless `get_simulation_status` says so. Proposes with a rationale citing the optimizer
  and documents.

Backend API tools (`agent/tools_backend.py`, all with the user's token `temp:afdd_session`,
`X-AFDD-Origin: agent`): `get_dashboard_overview`, `list_assets`, `get_asset_summary`,
`get_asset_parameters`, `get_parameter_trend` (+ chart), `get_alarm_events`, `get_maintenance_orders`,
`get_maintenance_order_detail`, `get_early_warnings`, `get_reliability_kpis`, `get_recommendations`,
`get_faults`, `get_anomalies`, plus `call_app_api(script, params)` limited to an allow-list of read-only
scripts.

Chart tools (`agent/charts.py`): `chart_parameter_trend(asset, params[], hours)`, `chart_compare(…)`,
`make_chart(title, chart_type line|bar|scatter|pie, x[], series[{name, data[]}], y_name)` — build a safe
ECharts option (no functions / HTML), cap 2,000 points per series; responses carry `{"chart": {"id",
"title", "option"}}`.

Budget: `before_model_callback` refuses an agent call when today's spend ≥ `AI_DAILY_BUDGET_USD` −
`AI_ANALYTICS_RESERVE_USD` (0.10, kept for the worker) or the user's own spend ≥ `AI_USER_DAILY_BUDGET_USD`
(0.20); `after_model_callback` records usage (cached prompt tokens at the cache rate);
`RunConfig(max_llm_calls=AI_MAX_LLM_CALLS)` (10). A client that disconnects mid-turn: the call in flight
finishes and is recorded, no new call starts. Before every call `context.before_model` replaces chart
options in earlier tool results with a stub (deterministic, so the implicit prompt cache keeps hitting); the
current plant time is sent in the `[UI context]` line of the user message, not in the system instruction.

HTTP API (only reachable through the backend proxy; requires `X-Agent-Secret`):

| route | body / response |
|---|---|
| `POST /chat` | `{session_id?, message, context?: {page, node_id, asset_alias, replay_job_id}}` → SSE stream (6.4) |
| `GET /sessions` | `[{id, title, updated_at}]` for the user |
| `GET /sessions/{id}` | `{id, title, messages:[{id, role, text, agent, charts[], citations[], simulations[], tool_calls[], created_at}]}` |
| `DELETE /sessions/{id}` | `{ok: true}` |
| `GET /healthz` | `{status}` |

### 6.4 SSE event protocol (agent → browser, `event:` name + JSON `data:`)

| event | data |
|---|---|
| `session` | `{session_id, title}` (first event) |
| `agent` | `{name}` (active agent changed) |
| `text` | `{delta, agent}` (streamed text; append) |
| `message` | `{text, agent}` (final text of one model turn; replaces the streamed deltas of that turn) |
| `tool_call` | `{id, name, agent, args}` (args sanitised, no tokens) |
| `tool_result` | `{id, name, agent, ok, summary}` |
| `chart` | `{id, title, option}` |
| `citations` | `{items:[{document_id, title, doc_type, page, snippet, score}]}` |
| `simulation` | `{job_id, title, status, approval_required, rationale, spec_summary}` |
| `usage` | `{input_tokens, output_tokens, cost_usd, today_usd, limit_usd}` |
| `error` | `{message}` |
| `done` | `{}` |

A ping comment `: ping` every 15 s.

### 6.5 MCP servers (`afdd_ai/mcp`, service `ai-mcp`)

MCP SDK 2.x `MCPServer`, streamable HTTP, `host="0.0.0.0"`, `stateless_http=True`, `json_response=True`.
Both require header `Authorization: Bearer <user session token>` (validated with `user/getuserdetails`,
cached 60 s) and `X-Agent-Secret`.

* `afdd-db` :8101 (`afdd_ai_ro`, `statement_timeout` 5 s, ≤ 5,000 rows): `query_parameter_history(asset_alias,
  param_aliases[], start, end, bucket_minutes, agg)`, `get_energy_summary(start, end, bucket)`,
  `list_alarm_events(...)`, `list_anomalies(...)`, `list_faults(...)`, `get_point_map()`,
  `search_documents(query, filters)`, `list_simulation_jobs(...)`. All scope-filtered by the user's locations.
* `afdd-twin` :8102 (backend API with the user's token): `get_plant_state()`, `get_fault_catalog()`,
  `get_forecast_conditions(start, hours)` (climatology from the last 14 days of the same hours),
  `propose_simulation(title, rationale, spec)`, `get_simulation_status(job_id)`,
  `get_simulation_results(job_id)` (summary + top events), `list_simulations(limit)`,
  `get_latest_recommendation()`.

---

## 7. Frontend

* Permission enum (`auth.api.ts`) + `screen-access.hooks.ts`: add `aiassistant`, `aiinsights`, `simulation`
  (both files CRLF: patch with a script, no `biome --write`).
* Layout slot `assistant?: ReactNode` rendered after `<Outlet/>` (layout.tsx is CRLF).
* `modules/utils`: `streamRequest(url, body, onEvent, signal)` (fetch + SSE parser + 401 handling).
* Deps: `react-markdown@9`, `remark-gfm@4`.
* `nlam/src/pages/ai-assistant/`: floating button + resizable panel (z ≈ 1100), session list, streamed
  markdown, tool chips, `ChartBlock` (validated option + theme colours), citation chips → PDF viewer
  (`/api/ai/documents/{id}/file` via fetch → blob URL), `SimulationCard` (Approve / Reject with the
  confirm rules; polls the job; on completion offers "Replay in digital twin" and sends a follow-up
  message so the agent summarises), context store (`useSyncExternalStore`) that pages update (twin:
  selected node / asset / replay job).
* `nlam/src/pages/ai-insights/` (route `/ai-insights`, menu "AI Insights", `aiinsights`): overview strip,
  Recommendations (current vs recommended, savings, twin validation badge, Apply / Dismiss, history),
  Faults (list, evidence, RAG instruction with sources), Anomalies (filters, table, mini trend), Documents
  (library, upload, status, reindex), Rules (thresholds, calibration; admin edit), Fault injection (admin).
* `nlam/src/pages/simulation/` (route `/simulation`, menu "Simulation", `simulation`): jobs list, new
  scenario form (start, duration, weather offsets, load scale, set-point actions, fault injections with
  documented intensities, variants), job detail (summary, variant comparison chart, events), Approve /
  Reject for PROPOSED, "Replay in digital twin".
* Digital twin replay: data-source seam (`TwinSource` context, live default), `ReplayProvider` for
  `?replay=<job>&variant=&t=`, timeline bar (play / pause, 1×/10×/60×, scrub, event markers), REPLAY
  banner, controls disabled, trends via `simulation/gettrend`.

---

## 8. Ownership (parallel implementation)

| owner | files |
|---|---|
| infra | `docker/postgres/Dockerfile`, `.dockerignore`, `docker-compose.yml`, `0006_ts_partitioning.sql`, `internal/scheduler/*`, `internal/config/config.go`, `cmd/server/main.go`, `internal/db/roles.go` |
| simulator | `cmd/simulator/*` (physics, faults, seed, history, content, live, **and** the job runner) |
| backend API | `internal/api/simulation.go`, `ai_insights.go`, `ai_documents.go`, `digital_twin_faults.go`, `agent_proxy.go`, `routes.go`, `server.go` (RequireHuman), `internal/engine/*` (external alarms), `internal/storage` (docs store) |
| analytics | `agentic/afdd_ai/analytics/*`, `agentic/afdd_ai/rag/*`, `agentic/tests/analytics/*` |
| agent | `agentic/afdd_ai/agent/*`, `agentic/afdd_ai/mcp/*`, `agentic/tests/agent/*` |
| frontend core | `modules/**` changes, `nlam/src/app.tsx`, `nlam/src/pages/ai-assistant/**`, `package.json` |
| frontend insights | `nlam/src/pages/ai-insights/**` |
| frontend simulation | `nlam/src/pages/simulation/**`, `nlam/src/pages/digital-twin/**` (replay) |

Shared, written before the fan-out: this document, `0005_agentic.sql`, `agentic/afdd_ai/common/*`,
`agentic/requirements.txt`, `agentic/Dockerfile`.
