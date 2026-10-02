# Cloud-like local test stack

Runs the backend the way the GCP deployment does, next to (not instead of) the normal local
`docker compose` stack, to test the Cloud Run readiness behaviour before deploying:

| Service | Like in GCP |
|---|---|
| `db` | PostgreSQL 16 + pgvector with TLS required (`hostnossl` rejected), the four login roles (`roles.sql`, stage 03) |
| `migrate`, `grants` | `afdd migrate` as `postgres`, then `deployment/gcp/db-tools/sql/grants.sql` (stage 05) |
| `api1`, `api2` + `lb` | `afdd api` as `afdd_api` (`sslmode=require`) behind a round-robin balancer (`X-Served-By` header) |
| `worker1`, `worker2` | `afdd worker`: one holds the processing lease, the other stands by |
| `gcs` | fake-gcs-server for both buckets (`STORAGE_PROVIDER=gcs`, `STORAGE_EMULATOR_HOST`) |
| `simulator` | `afdd-sim start` with `SIM_TRANSPORT=http` (REST push gateways, command polling, no broker) |
| `frontend` | the web UI built with `VITE_NOTIFY_MODE=poll`, on http://localhost:8090 |
| profile `ai` | agent, MCP db / twin, 2 analytics workers with TLS database URLs and GCS (no Gemini key) |

## Run

From this directory (Git Bash), with the source to test:

```bash
sh make-cert.sh
docker build -t afdd-backend:cloudtest ../..
docker build -t afdd-ai:cloudtest ../../agentic
docker build --build-arg VITE_NOTIFY_MODE=poll -t digital-twin-afdd-frontend:cloudtest ../../../chiller-digital-twin-frontend
docker compose up -d
docker compose --profile ai up -d agent mcp-db mcp-twin analytics1 analytics2
```

Sign in at http://localhost:8090 as a demo user (password `Demo@2026`, set in the compose file;
`superuser` uses `Cloudtest@2026`). The API is also reachable through the balancer at
http://localhost:8091 and the database at `127.0.0.1:55432` (TLS, user `postgres`).

## What to try

- `docker compose logs worker1 worker2 | grep lease` - one holder; `docker compose stop worker1`
  and the other takes over within seconds.
- Twin control on a cooling tower fan: the command goes through `dh.commands` and the simulator's
  `GET /api/datahub/rest/{alias}/commands`.
- Upload an image (e.g. maintenance instruction media) and fetch its `/public/...` URL with a
  `Range` header through the balancer: both replicas serve it from the emulator.
- `docker compose stop db`, recreate `api1`, start `db` again: the API waits (`DB_CONNECT_TIMEOUT`).

After recreating API containers, restart `lb` (nginx caches their addresses).

## Clean up

```bash
docker compose --profile ai down -v
```
