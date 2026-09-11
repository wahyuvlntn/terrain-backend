-- Shared runtime state for stateless API replicas and a separate processing
-- worker (Cloud Run): the latest tag values and gateway states the worker
-- (or an API replica receiving REST pushes) writes and every replica reads,
-- configuration-change events for the worker, and commands waiting for REST
-- push devices to pick them up. The all-in-one `serve` keeps these in memory
-- and does not need the tables.

CREATE TABLE IF NOT EXISTS dh.live_values (
    gateway TEXT NOT NULL,             -- lower(gateway alias)
    tag     TEXT NOT NULL,             -- tag name
    value   TEXT NOT NULL,
    ts      BIGINT NOT NULL,           -- source time (ms)
    quality SMALLINT NOT NULL,         -- 0 good, 1 bad, 2 configuration
    changes INTEGER NOT NULL DEFAULT 0,
    recv    BIGINT NOT NULL,           -- received (ms)
    PRIMARY KEY (gateway, tag)
);
CREATE INDEX IF NOT EXISTS live_values_recv_idx ON dh.live_values (gateway, recv);

CREATE TABLE IF NOT EXISTS dh.gateway_state (
    gateway    TEXT PRIMARY KEY,       -- lower(gateway alias)
    connected  BOOLEAN NOT NULL,
    error      TEXT,
    status_ts  BIGINT NOT NULL,        -- last connection change (ms)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS core.runtime_events (
    id   BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,                -- gateway | engine
    key  TEXT,
    at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dh.commands (
    id           BIGSERIAL PRIMARY KEY,
    gateway      TEXT NOT NULL,        -- lower(gateway alias)
    device       TEXT NOT NULL DEFAULT '',
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS commands_pending_idx ON dh.commands (gateway, id) WHERE delivered_at IS NULL;
