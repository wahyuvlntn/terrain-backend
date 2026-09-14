-- FAB1 production (docs/production/DESIGN.md): products and their module route,
-- lots, lot steps (process runs), metrology results and scrap. The demo
-- simulator writes these tables; the backend reads them for the Production page.
-- "production" permission group (read = view the Production page).

CREATE SCHEMA IF NOT EXISTS prod;

CREATE TABLE prod.products (
    code        TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    technology  TEXT NOT NULL DEFAULT '',
    layers      INT NOT NULL,
    mix         DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE prod.route_steps (
    product_code  TEXT NOT NULL REFERENCES prod.products (code) ON DELETE CASCADE,
    seq           INT NOT NULL,
    step_code     TEXT NOT NULL,
    layer         INT NOT NULL,
    operation     TEXT NOT NULL,
    tool_type     TEXT NOT NULL,          -- ETCH | CVD | MET | WET | LITHO (outside the module)
    recipe        TEXT NOT NULL DEFAULT '',
    ideal_wafer_s DOUBLE PRECISION NOT NULL DEFAULT 0,
    ideal_lot_s   DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_wafers INT NOT NULL DEFAULT 0, -- metrology: wafers measured (0 = all / not metrology)
    sample_rate   DOUBLE PRECISION NOT NULL DEFAULT 1,
    param         TEXT,                   -- metrology parameter measured (thickness | cd | defects)
    PRIMARY KEY (product_code, seq)
);

CREATE TABLE prod.lots (
    lot_code      TEXT PRIMARY KEY,
    product_code  TEXT NOT NULL REFERENCES prod.products (code),
    priority      INT NOT NULL DEFAULT 3,
    wafers_start  INT NOT NULL,
    wafers        INT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('QUEUED', 'PROCESSING', 'AWAY', 'HOLD', 'DONE', 'SCRAPPED')),
    current_seq   INT NOT NULL DEFAULT 1,
    step_code     TEXT,
    equipment_id  BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    hold_reason   TEXT,
    rework_count  INT NOT NULL DEFAULT 0,
    released_at   TIMESTAMPTZ NOT NULL,
    due_at        TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ
);
CREATE INDEX lots_status_idx ON prod.lots (status) WHERE status NOT IN ('DONE', 'SCRAPPED');
CREATE INDEX lots_released_idx ON prod.lots (released_at);
CREATE INDEX lots_completed_idx ON prod.lots (completed_at);

CREATE TABLE prod.lot_steps (
    id             BIGSERIAL PRIMARY KEY,
    lot_code       TEXT NOT NULL REFERENCES prod.lots (lot_code) ON DELETE CASCADE,
    seq            INT NOT NULL,
    step_code      TEXT NOT NULL,
    operation      TEXT NOT NULL,
    equipment_id   BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    chambers       TEXT,
    recipe         TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('QUEUED', 'PROCESSING', 'DONE', 'SKIPPED')),
    queued_at      TIMESTAMPTZ NOT NULL,
    started_at     TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ,
    wafers_in      INT NOT NULL,
    wafers_out     INT,
    scrapped       INT NOT NULL DEFAULT 0,
    reworked       INT NOT NULL DEFAULT 0,
    theoretical_s  DOUBLE PRECISION,
    actual_s       DOUBLE PRECISION,
    outcome        TEXT CHECK (outcome IN ('PASS', 'REWORK', 'SCRAP', 'ABORTED')),
    batch_id       TEXT,
    is_rework      BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX lot_steps_lot_idx ON prod.lot_steps (lot_code, seq);
CREATE INDEX lot_steps_equipment_idx ON prod.lot_steps (equipment_id, ended_at);
CREATE INDEX lot_steps_ended_idx ON prod.lot_steps (ended_at);
CREATE INDEX lot_steps_open_idx ON prod.lot_steps (status) WHERE status IN ('QUEUED', 'PROCESSING');

CREATE TABLE prod.metrology (
    id                      BIGSERIAL PRIMARY KEY,
    lot_code                TEXT NOT NULL REFERENCES prod.lots (lot_code) ON DELETE CASCADE,
    lot_step_id             BIGINT REFERENCES prod.lot_steps (id) ON DELETE CASCADE,
    measured_at             TIMESTAMPTZ NOT NULL,
    param                   TEXT NOT NULL,
    wafer                   INT NOT NULL,
    value                   DOUBLE PRECISION NOT NULL,
    wiw_range               DOUBLE PRECISION,
    target                  DOUBLE PRECISION,
    lsl                     DOUBLE PRECISION,
    usl                     DOUBLE PRECISION,
    metrology_equipment_id  BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    source_equipment_id     BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    source_chamber          TEXT,
    source_step_code        TEXT,
    pass                    BOOLEAN NOT NULL
);
CREATE INDEX metrology_param_idx ON prod.metrology (param, measured_at);
CREATE INDEX metrology_source_idx ON prod.metrology (source_equipment_id, measured_at);

CREATE TABLE prod.scrap (
    id            BIGSERIAL PRIMARY KEY,
    lot_code      TEXT NOT NULL REFERENCES prod.lots (lot_code) ON DELETE CASCADE,
    at            TIMESTAMPTZ NOT NULL,
    wafers        INT NOT NULL,
    reason        TEXT NOT NULL,
    step_code     TEXT,
    equipment_id  BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    chamber       TEXT
);
CREATE INDEX scrap_at_idx ON prod.scrap (at);
CREATE INDEX scrap_equipment_idx ON prod.scrap (equipment_id, at);

-- AI services read production data like the rest of the plant data
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai') THEN
        GRANT USAGE ON SCHEMA prod TO afdd_ai;
        GRANT SELECT ON ALL TABLES IN SCHEMA prod TO afdd_ai;
        ALTER DEFAULT PRIVILEGES IN SCHEMA prod GRANT SELECT ON TABLES TO afdd_ai;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai_ro') THEN
        GRANT USAGE ON SCHEMA prod TO afdd_ai_ro;
        GRANT SELECT ON ALL TABLES IN SCHEMA prod TO afdd_ai_ro;
        ALTER DEFAULT PRIVILEGES IN SCHEMA prod GRANT SELECT ON TABLES TO afdd_ai_ro;
    END IF;
END $$;

INSERT INTO core.permission_groups (alias, name, sort_order) VALUES ('production', 'Production', 24)
ON CONFLICT (alias) DO NOTHING;

INSERT INTO core.permissions (permission_group_id, alias, name)
SELECT g.id, p.alias, initcap(p.alias)
FROM core.permission_groups g CROSS JOIN (VALUES ('read')) AS p (alias)
WHERE g.alias = 'production'
ON CONFLICT (permission_group_id, alias) DO NOTHING;
