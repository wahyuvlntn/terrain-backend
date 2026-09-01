# AFDD Backend

Backend for the Daikin **Advance Fault Detection & Diagnostic** (asset monitoring and digital
maintenance) web application. It implements the complete API used by the frontend repository
(`modules/`, `nlam/`, `mobile/`) — all 294 scripts and queries the UI calls — plus the data
acquisition, evaluation engine, early-warning diagnosis, maintenance workflow and background jobs.

- **Go 1.26**, single static binary (`cmd/server`)
- **PostgreSQL 14+** (schema migrations are embedded and applied on start)
- **MinIO / S3** for uploaded files (images, PDFs, logos), served under `/public/`
- **Embedded MQTT broker** for device data and live UI notifications

## Quick start

```bash
cp .env.example .env          # adjust DATABASE_URL / MINIO_* if needed
docker compose up -d --build  # postgres + minio + backend
docker compose logs backend   # the first start prints the generated superuser password
```

Or run locally against existing services:

```bash
go run ./cmd/server           # reads .env from the working directory
```

The API listens on `https://<host>:4500` (self-signed certificate unless `TLS_CERT_FILE`/`TLS_KEY_FILE`
are set). Point the frontend at it exactly like the original deployment: the frontend `nginx.conf`
proxies `/api/` and `/public/` to port 4500. Alternatively set `STATIC_DIR` to the built `dist/nlam`
folder (and `STATIC_MOBILE_DIR` to the mobile build) and the backend serves the UI itself. When the
UI is built in dev mode (`VITE_DEV=true`) it calls the API host hard coded in
`modules/utils/src/lib/request.ts`; set `CORS_ORIGINS` accordingly.

The web UI connects to `wss://<host>:9002/mqtt` with `VITE_MQTT_USERNAME`/`VITE_MQTT_PASSWORD`
(defaults `indie_nodelinx` / `indie123`, configurable with `MQTT_CLIENT_USERNAME`/`MQTT_CLIENT_PASSWORD`).
Browsers only accept that connection with a trusted certificate.

### CLI

```text
afdd serve                                  run the backend (default)
afdd migrate                                apply migrations and exit
afdd create-user -username U -password P [-role superuser|admin|basic] [-name N] [-email E]
afdd reset-password -username U -password P
afdd license keygen                         create the license signing key pair
afdd license system-id                      print this installation's system id
afdd license issue -key PRIV -type BASE|EQUIPMENT [-assets a,b | -count N] [-expires YYYY-MM-DD] [-system-id ID]
afdd healthcheck                            used by the Docker HEALTHCHECK
```

All configuration options are documented in [`.env.example`](.env.example).

## Architecture

```
cmd/server            wiring, CLI, graceful shutdown
internal/api          HTTP API: one file per frontend module (≈300 handlers)
internal/httpx        request parsing (axios multipart / bracket notation / JSON) and response envelopes
internal/auth         sessions, roles, permission groups, location scopes, password policy
internal/db           pgx pool, transactions, embedded migrations (internal/db/migrations)
internal/storage      MinIO client, uploads, /public/ file server (range requests)
internal/datahub      data sources: OPC UA, Modbus TCP/RTU, Siemens S7, MQTT client, MQTT server,
                      REST client, REST server (push), MTConnect; JS payload mapping
internal/broker       embedded MQTT broker (TCP, TLS, secure WebSocket) with per-gateway ACLs
internal/engine       evaluation cycle: status scripts, alarms, dynamic alarms, thresholds,
                      SPC (Western Electric rules), condition, value logging, issue-relation diagnosis
internal/maint        maintenance orders: requests, approval, recurrence, work reports, forced status,
                      early-warning and running-hour orders
internal/notify       notifications (DB + MQTT push) and e-mail digests (SMTP / STARTTLS / TLS)
internal/license      ed25519 signed license files
internal/scheduler    background jobs (advisory-locked, safe with several instances)
internal/script       sandboxed JavaScript (goja) with timeouts
```

### API contract

| Frontend call | Endpoint | Response |
|---|---|---|
| `jsRequest({url:'/module/script'})` | `POST /api/js-service/exposed/execute/{org}/{project}/module/script` (multipart) | `{"data": ..., "error": null}` or `{"data": null, "error": "<message>"}` with 4xx/5xx |
| `dbRequest({url:'/filter/x'})` | `POST /api/database-connection/exposed/execute/{org}/{project}/{db}/filter/x` | raw JSON array |
| login | `POST /api/user-management/exposed/get-session-add-expiry` `{username,password}` | `{"nodelinx_exposed_session_id": "..."}` |

Requests are authenticated with the `Nodelinx-Exposed-Session-Id` header. Error messages are i18n keys
of the frontend (`err_...`) or `MSG:<key>:<param>...`, so the UI shows translated toasts. Timestamps are
ISO-8601 UTC (`YYYY-MM-DDTHH:MM:SS.sssZ`); the client time zone comes from `timezone_offset`.

## Domain behaviour and design decisions

- **Roles.** `superuser` (everything, configuration screens), `admin` (all permissions, all locations),
  `basic` (permissions of the user group, limited to the group's locations and their descendants).
- **Buildings / floors / rooms** in the maintenance screens are levels of the location tree starting at
  `BUILDING_LEVEL` (default 0: root locations are buildings).
- **Technicians / personnel** are basic users whose group grants `maintenanceorder.report`, plus anyone already
  assigned to an order. Personnel management restricts them to buildings and equipment types.
- **Alarm codes** are master data (Master Data → Equipment → Alarms, entered as `CODE - Name`).
  Model alarms are linked to codes by name (`E01`, `E01 - ...`, `E01: ...` or the code name).
- **Maintenance order issues** (`instructions` / `other_alarm_codes`) are alarm-code ids; the work
  report shows the model's instructions for those codes. Order names follow `CM|PM-MM-YY-NNN`.
- **Early warnings.** Issue relations (Issue Mapping) are graphs of *source* alarm codes, *parameter*
  abnormalities and *compound* (AND) nodes pointing at *target* codes. When a target becomes active the
  engine raises a system corrective order (degradation, or compound when an AND node was involved),
  throttled by `EARLY_WARNING_COOLDOWN`.
- **Running hours** count RUNNING time since the last completed order of the equipment; model limits
  send `RUNNING_HOUR_WARNING/CRITICAL` notifications and can raise a preventive order automatically.
- **Statuses**: 1 Running, 2 Idle, 3 Unplanned down, 4 Offline (no good data for `OFFLINE_TIMEOUT`),
  5 Planned down. Starting work on an order can force the equipment status until the work is done.
- **Recurring orders** are generated up to their end date (max 500) or for a rolling 90 day horizon.
- **E-mail** digests use implicit TLS (port 465 with *Secure*), STARTTLS when offered, and AUTH PLAIN or
  LOGIN; credentials are never sent over an unencrypted connection to a remote server.
- **Licenses** are signed with an ed25519 key (`afdd license keygen`); uploads are verified with
  `LICENSE_PUBLIC_KEY`. With `LICENSE_ENFORCED=true` unlicensed equipment is not evaluated.
- **Data sources**
  - *MQTT server*: devices connect to the embedded broker (1883 / 8883) with username
    `{ORG_ALIAS}.{PROJECT_ALIAS}.{gateway alias}` and the generated password, and publish JSON to the
    topic of the same name (or sub topics, addressed as `<sub topic>/<key>`).
  - *REST server*: devices `POST /api/datahub/rest/{gateway alias}` with header `X-Api-Key`.
  - JSON payloads are flattened to tag addresses (`a.b.c`); an optional JavaScript expression
    (`output = ... response ...`) can reshape payloads. Values carry quality, so stale or bad data turns
    equipment offline instead of producing false alarms.
  - Modbus addresses: `<0|1|3|4><register>[.<bit|length>][:<TYPE>][{unit-id: 1, byte-order: "BIG_ENDIAN"}]`.

## Demo plant and simulator

`cmd/simulator` (`afdd-sim` in the image) turns an empty installation into a living plant: the facility
systems of a 300 mm semiconductor wafer fab in Singapore.

```bash
docker compose --profile demo up -d --build   # seeds once, then streams live data
docker compose logs -f simulator              # follow faults, trips and the maintenance team
```

**What is created** (through the public API, so everything is validated like user input):

| Area | Content |
|---|---|
| Locations | FAB1 Wafer Fab (sub-fab bays, MAU plant rooms) and CUB Central Utility Building (chiller plant, PCW pump room, CDA room, cooling tower yard) |
| Models | Daikin WSC-126 centrifugal chiller, BAC cooling tower, Grundfos PCW pump, Grundfos NKG CHW and CW pumps, CUB chilled water plant controller (CHWS), Atlas Copco ZR 315 VSD+ compressor, Daikin Vision make-up air unit, Edwards iXH dry pumps (a base model with Etch and CVD extended models) with variables, status scripts, alarms, thresholds, running hour limits and writable control points |
| Equipment | 31 units (3 chillers N+1 with dedicated CHW and CW pumps, 3 cooling towers, the CHW plant controller CHWS_01, 4 PCW pumps 3+1, 2 CDA compressors, 6 MAUs, 6 dry pumps) on 3 MQTT data sources (`cub_bms`, `fab1_hvac`, `fab1_subfab`) |
| Diagnostics | 71 alarm codes (including the LBNL fault codes CHL-011, CT-013, CT-014, CHS-005..008 raised by the AI fault detection), alarm mapping, 34 issue relations (direct, degradation and compound paths) |
| Instructions | Diagnostic, repair and severity SOPs plus preventive maintenance checklists |
| People | 14 users in 4 groups with permissions, personnel buildings and equipment types |
| History | 60 days (`HISTORY_DAYS`): parameter logs, statuses, alarms, early warnings, corrective/preventive orders with work reports, recurring PM schedules, KPIs |

All demo users share the password `Demo@2026` (`DEMO_PASSWORD`): facility manager `sarah_lim` (admin),
supervisors `rajesh_kumar` (CUB) and `nurul_aisyah` (FAB1), reliability engineer `kevin_ong`, technicians
`ahmad_faizal`, `lim_kokwai`, `kumar_selvam`, `chen_jiahui` (CUB) and `hafiz_rahman`, `tan_meiling`,
`daniel_wong`, `priya_nair` (FAB1), control room operators `jason_teo` and `siti_rahmah`.

**Live behaviour.** Every 5 seconds the simulator publishes physically consistent values (weather and load
driven, N+1 duty/standby rotation) to the embedded broker. The chiller plant runs under a supervisory
controller (CHWS_01): CHW supply reset and chiller staging, tower fan PI on the condenser water supply,
the condenser 3-way bypass valve, variable-primary CHW pumps holding the riser DP, and MAU coil coupling.
Operators can write set points and modes from the digital twin (two-way control), and administrators can
inject the seven LBNL chiller-plant faults (sensor biases, tower fouling, valve leak / stuck, PI hunting)
from AI Insights. It injects degradations (condenser fouling,
refrigerant leak, bearing wear, filter clogging, coil fouling, belt slip, cavitation, impeller wear,
intercooler fouling, dryer failure, air leaks, by-product deposition, rotor wear, purge blockage, …) that
develop over 20-60 minutes until the engine raises alarms and early-warning maintenance orders; random trips
(unplanned down) and communication losses. Starting work with "stop equipment" stops the simulated machine,
and completing an order repairs the faults matching its issues. The **maintenance team simulation**
(`SIM_WORKFORCE=true`) raises operator requests for trips, approves orders and assigns technicians, starts
work, posts work updates, completes and closes orders as the demo users - but only on orders nobody touched
for a while (`SIM_APPROVE_DELAY`, 15 minutes), so you can always handle an order yourself. Set
`SIM_WORKFORCE=false` to do all maintenance by hand.

Useful commands:

```bash
docker compose stop simulator                              # pause the plant (equipment goes offline)
docker compose --profile demo run --rm simulator history   # regenerate history and orders (simulator stopped)
docker compose --profile demo up -d simulator              # resume
```

The demo users have `@sgfab.example` e-mail addresses and are not subscribed to e-mail digests; subscribe
groups in the Notification Manager only if you change their addresses.

## Background jobs

| Job | Interval |
|---|---|
| E-mail digests (per configured interval) | 30 s |
| Maintenance reminders (`MAINTENANCE_ORDER_SCHEDULED`) | 1 min |
| Extend open-ended recurring orders | 6 h |
| Running hour limits | 5 min |
| License expiry reminders (30 / 7 / 3 / 1 days) | 1 h |
| Session cleanup | 1 h |
| MTBF / MTTR refresh | 15 min |
| Retention of raw values and notifications (`RETENTION_DAYS`) | daily |
| Parameter value partitions (monthly, created 3 months ahead; retention drops whole months) | daily, and at start-up |

## AI services (agentic feature)

Three services share the `afdd-ai` image built from `agentic/` (Python 3.12, Google ADK, Gemini). The design
is in `docs/agentic/DESIGN.md`.

| Service | Role |
|---|---|
| `ai-agent` | The plant assistant: a root agent routing to an insight agent and a digital-twin agent (plus a Pro "deep analyst"), streamed to the browser as SSE through the backend proxy `/api/agent/*`. Conversations are stored in schema `agent`. |
| `ai-mcp` | MCP servers `afdd-db` (read-only SQL tools, port 8101) and `afdd-twin` (simulation tools, port 8102) used by the agents. |
| `ai-analytics` | Anomaly detection, LBNL rule-based fault detection with RAG-grounded maintenance instructions, set-point optimisation (LightGBM surrogate + Optuna, validated on the twin), the document corpus and its embeddings (pgvector), and the post-analysis of simulation jobs. |

```bash
docker compose build ai-agent                              # builds afdd-ai:latest for all three
docker compose up -d ai-agent ai-mcp ai-analytics
docker compose logs -f ai-analytics
```

Settings (in `.env`, read by compose): `GOOGLE_API_KEY` (required for any AI call), `AGENT_SHARED_SECRET`
(backend proxy to agent; set a random value), `AI_DAILY_BUDGET_USD` (default 0.50, all Gemini spend per
day), `AI_USER_DAILY_BUDGET_USD` (0.20 per user), `AI_ANALYTICS_RESERVE_USD` (0.10 kept for background
analytics), `AI_MODEL_FAST` / `AI_MODEL_PRO` (default `gemini-3.5-flash-lite` / `gemini-3.1-pro-preview`).
Every Gemini call is priced and recorded in `analytics.llm_usage`.

Simulation jobs (what-if scenarios on a copy of the plant) run in the simulator (`SIM_JOBS`, default
true). The assistant can only propose them; a person approves them in the application.

## Tests

```bash
go test ./...                                   # unit tests
# integration tests need a PostgreSQL server (a temporary database is created and dropped)
AFDD_TEST_DATABASE_URL='postgres://user:pass@localhost:5432/postgres?sslmode=disable' go test ./test/integration/
```

The integration suite starts the full backend in-process with an in-memory S3 server and calls the
API like the web UI (axios-style multipart). It covers every registered script on an empty and on a
populated database, an end-to-end scenario (master data → locations → models → data source →
equipment → live data → engine → alarms → early warning → maintenance lifecycle → notifications →
themes → licenses → scheduler jobs), e-mail digests against an SMTP server, and protocol tests against
real MQTT, Modbus TCP, OPC UA and HTTP servers. Set `AFDD_TEST_KEEP_DB=<name>` to keep the populated database for manual exploration.

More database-backed suites: `cmd/simulator` (`AFDD_TEST_DATABASE_URL`, creates its own scratch databases),
`internal/api` and `internal/engine` agentic tests (`AFDD_API_TEST_DATABASE_URL`, a scratch copy of a seeded
database whose name contains `scratch` or `test`). The Python services have their own suite:

```bash
cd agentic && python -m venv .venv && .venv/Scripts/pip install --isolated -r requirements.txt
.venv/Scripts/python -m pytest tests -q      # needs Postgres on 127.0.0.1:5432 and MinIO on 127.0.0.1:9000
```

No test calls Gemini (models are scripted); `AFDD_TEST_REAL_EMBEDDINGS=1` opts in to one tiny embedding
test. Tests that write the live work queues skip themselves while the `ai-analytics` container runs.
