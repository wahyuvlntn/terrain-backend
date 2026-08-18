-- Agentic AI (docs/agentic/DESIGN.md): pgvector RAG, analytics (anomalies,
-- LBNL fault detection, set-point optimisation), digital twin simulation jobs,
-- agent session memory, permission groups and least-privilege service roles.
--
-- Requires the postgres image with pgvector (docker/postgres/Dockerfile).

CREATE EXTENSION IF NOT EXISTS vector;

CREATE SCHEMA IF NOT EXISTS rag;
CREATE SCHEMA IF NOT EXISTS analytics;
CREATE SCHEMA IF NOT EXISTS sim;
CREATE SCHEMA IF NOT EXISTS agent;

-- ---------------------------------------------------------------------------
-- Sites (multi-site ready). A location belongs to a site through its root
-- building; NULL site_id means the default site.
-- ---------------------------------------------------------------------------
CREATE TABLE core.sites (
    id         SERIAL PRIMARY KEY,
    code       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    timezone   TEXT,
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sites_default_uk ON core.sites (is_default) WHERE is_default;
INSERT INTO core.sites (code, name, timezone, is_default) VALUES ('SGF1', 'Singapore Fab 1 campus', 'Asia/Singapore', TRUE);

ALTER TABLE asset.locations ADD COLUMN site_id INT REFERENCES core.sites (id) ON DELETE SET NULL;

-- Equipment with its type, model, location path and site (used by the Python services).
CREATE VIEW analytics.v_assets AS
WITH RECURSIVE tree AS (
    SELECT l.id, l.parent_id, l.name::text AS path, l.id AS root_id, l.site_id
    FROM asset.locations l WHERE l.parent_id IS NULL
    UNION ALL
    SELECT c.id, c.parent_id, t.path || ' > ' || c.name, t.root_id, COALESCE(c.site_id, t.site_id)
    FROM asset.locations c JOIN tree t ON c.parent_id = t.id
)
SELECT e.id, e.alias, e.name, ty.name AS type_name, m.id AS model_id, m.name AS model_name,
       e.location_id, tree.path AS location_path, tree.root_id AS building_id,
       COALESCE(s.code, (SELECT code FROM core.sites WHERE is_default)) AS site_code
FROM asset.equipment e
JOIN asset.models m ON m.id = e.model_id
JOIN asset.types ty ON ty.id = m.type_id
LEFT JOIN tree ON tree.id = e.location_id
LEFT JOIN core.sites s ON s.id = tree.site_id;

-- ---------------------------------------------------------------------------
-- RAG: documents (PDF originals in the private MinIO bucket afdd-docs) and
-- their embedded chunks (gemini-embedding-001, 768 dimensions, L2-normalised).
-- ---------------------------------------------------------------------------
CREATE TABLE rag.documents (
    id           BIGSERIAL PRIMARY KEY,
    title        TEXT NOT NULL,
    doc_type     TEXT NOT NULL CHECK (doc_type IN ('manual', 'sop', 'maintenance_report', 'setpoint_log', 'fdd_reference', 'other')),
    source       TEXT NOT NULL DEFAULT 'upload' CHECK (source IN ('upload', 'generated')),
    object_key   TEXT NOT NULL UNIQUE,
    file_name    TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/pdf',
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    sha256       TEXT,
    site_code    TEXT NOT NULL DEFAULT 'SGF1',
    location_id  BIGINT REFERENCES asset.locations (id) ON DELETE SET NULL,
    asset_id     BIGINT REFERENCES asset.equipment (id) ON DELETE SET NULL,
    unit         TEXT,          -- equipment alias (CH_01); NULL = all units of the type
    asset_type   TEXT,          -- asset.types.name; NULL = general
    model_id     BIGINT REFERENCES asset.models (id) ON DELETE SET NULL,
    alarm_codes  TEXT[] NOT NULL DEFAULT '{}',
    doc_date     DATE,
    status       TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'INDEXING', 'READY', 'FAILED')),
    error        TEXT,
    pages        INT,
    chunks       INT,
    metadata     JSONB NOT NULL DEFAULT '{}',   -- generated docs: generator_key (unique), source ids
    uploaded_by  BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    indexed_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX documents_generator_uk ON rag.documents ((metadata ->> 'generator_key')) WHERE source = 'generated';
CREATE INDEX documents_status_idx ON rag.documents (status);
CREATE INDEX documents_filter_idx ON rag.documents (doc_type, asset_type, unit);

CREATE TABLE rag.chunks (
    id          BIGSERIAL PRIMARY KEY,
    document_id BIGINT NOT NULL REFERENCES rag.documents (id) ON DELETE CASCADE,
    chunk_index INT NOT NULL,
    page        INT,
    heading     TEXT,
    content     TEXT NOT NULL,
    token_count INT,
    embedding   vector(768) NOT NULL,
    -- denormalised metadata filters (site, unit, asset type, date)
    site_code   TEXT NOT NULL,
    location_id BIGINT,
    asset_id    BIGINT,
    unit        TEXT,
    asset_type  TEXT,
    doc_type    TEXT NOT NULL,
    doc_date    DATE,
    alarm_codes TEXT[] NOT NULL DEFAULT '{}',
    tsv         tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(heading, '') || ' ' || content)) STORED,
    UNIQUE (document_id, chunk_index)
);
CREATE INDEX chunks_embedding_hnsw ON rag.chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX chunks_filter_idx ON rag.chunks (site_code, asset_type, doc_type, doc_date);
CREATE INDEX chunks_unit_idx ON rag.chunks (unit);
CREATE INDEX chunks_tsv_idx ON rag.chunks USING gin (tsv);
CREATE INDEX chunks_codes_idx ON rag.chunks USING gin (alarm_codes);

-- ---------------------------------------------------------------------------
-- Analytics
-- ---------------------------------------------------------------------------

-- Statistical sensor anomalies on production data (one ACTIVE row per parameter and detector).
CREATE TABLE analytics.anomalies (
    id                     BIGSERIAL PRIMARY KEY,
    site_code              TEXT NOT NULL DEFAULT 'SGF1',
    asset_id               BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    equipment_parameter_id BIGINT NOT NULL REFERENCES asset.equipment_parameters (id) ON DELETE CASCADE,
    param_alias            TEXT NOT NULL,
    detector               TEXT NOT NULL CHECK (detector IN ('range', 'flatline', 'zscore', 'ewma', 'spc')),
    severity               TEXT NOT NULL CHECK (severity IN ('WARNING', 'CRITICAL')),
    score                  DOUBLE PRECISION,
    value                  DOUBLE PRECISION,
    expected               DOUBLE PRECISION,
    message                TEXT NOT NULL DEFAULT '',
    details                JSONB NOT NULL DEFAULT '{}',
    started_at             TIMESTAMPTZ NOT NULL,
    last_seen_at           TIMESTAMPTZ NOT NULL,
    ended_at               TIMESTAMPTZ,
    status                 TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CLEARED'))
);
CREATE UNIQUE INDEX anomalies_open_uk ON analytics.anomalies (equipment_parameter_id, detector) WHERE status = 'ACTIVE';
CREATE INDEX anomalies_asset_idx ON analytics.anomalies (asset_id, started_at DESC);
CREATE INDEX anomalies_started_idx ON analytics.anomalies (started_at DESC);

-- LBNL fault detection rules (rule code = alarm code). Thresholds are configuration,
-- calibrated on fault-free data (calibration holds the statistics used).
CREATE TABLE analytics.fault_rules (
    code          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    lbnl_fault    TEXT NOT NULL,
    description   TEXT NOT NULL,
    asset_scope   TEXT NOT NULL CHECK (asset_scope IN ('chiller', 'tower', 'plant')),
    signals       JSONB NOT NULL,   -- [{lbnl, asset, param}]
    params        JSONB NOT NULL,   -- thresholds and windows in use
    defaults      JSONB NOT NULL,   -- engineering defaults
    calibration   JSONB,
    calibrated_at TIMESTAMPTZ,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    severity      TEXT NOT NULL DEFAULT 'WARNING' CHECK (severity IN ('WARNING', 'CRITICAL')),
    version       INT NOT NULL DEFAULT 1,
    updated_by    BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Detected faults. ACTIVE rows are raised as engine alarms (code = rule_code).
CREATE TABLE analytics.faults (
    id                  BIGSERIAL PRIMARY KEY,
    site_code           TEXT NOT NULL DEFAULT 'SGF1',
    rule_code           TEXT NOT NULL REFERENCES analytics.fault_rules (code) ON DELETE CASCADE,
    asset_id            BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    severity            TEXT NOT NULL CHECK (severity IN ('WARNING', 'CRITICAL')),
    message             TEXT NOT NULL,
    intensity_estimate  DOUBLE PRECISION,
    evidence            JSONB NOT NULL DEFAULT '{}',
    started_at          TIMESTAMPTZ NOT NULL,
    last_seen_at        TIMESTAMPTZ NOT NULL,
    ended_at            TIMESTAMPTZ,
    status              TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CLEARED')),
    instruction_status  TEXT NOT NULL DEFAULT 'PENDING' CHECK (instruction_status IN ('PENDING', 'READY', 'FAILED', 'SKIPPED')),
    instruction_md      TEXT,
    instruction_sources JSONB NOT NULL DEFAULT '[]',
    instruction_model   TEXT,
    instruction_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX faults_open_uk ON analytics.faults (rule_code, asset_id) WHERE status = 'ACTIVE';
CREATE INDEX faults_asset_idx ON analytics.faults (asset_id, started_at DESC);
CREATE INDEX faults_started_idx ON analytics.faults (started_at DESC);

-- Surrogate models (LightGBM boosters serialised as text in artifact).
CREATE TABLE analytics.surrogate_models (
    id          BIGSERIAL PRIMARY KEY,
    version     TEXT NOT NULL UNIQUE,
    algorithm   TEXT NOT NULL,
    features    JSONB NOT NULL,
    targets     JSONB NOT NULL,
    n_samples   INT NOT NULL,
    metrics     JSONB NOT NULL DEFAULT '{}',
    doe_job_id  BIGINT,
    artifact    BYTEA NOT NULL,
    is_active   BOOLEAN NOT NULL DEFAULT FALSE,
    trained_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX surrogate_models_active_uk ON analytics.surrogate_models (is_active) WHERE is_active;

CREATE TABLE analytics.optimizer_runs (
    id           BIGSERIAL PRIMARY KEY,
    trigger      TEXT NOT NULL DEFAULT 'schedule' CHECK (trigger IN ('schedule', 'manual', 'agent')),
    requested_by BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    status       TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'SKIPPED')),
    model_id     BIGINT REFERENCES analytics.surrogate_models (id) ON DELETE SET NULL,
    n_trials     INT,
    metrics      JSONB NOT NULL DEFAULT '{}',
    error        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ
);
CREATE INDEX optimizer_runs_created_idx ON analytics.optimizer_runs (created_at DESC);

-- Recommended supervisory set points (validated on the digital twin before use).
CREATE TABLE analytics.recommendations (
    id                BIGSERIAL PRIMARY KEY,
    run_id            BIGINT REFERENCES analytics.optimizer_runs (id) ON DELETE SET NULL,
    site_code         TEXT NOT NULL DEFAULT 'SGF1',
    status            TEXT NOT NULL DEFAULT 'PROPOSED' CHECK (status IN ('PROPOSED', 'VALIDATING', 'VALIDATED', 'REJECTED', 'APPLIED', 'EXPIRED', 'DISMISSED', 'FAILED')),
    conditions        JSONB NOT NULL,   -- {wet_bulb, dry_bulb, load_frac, cooling_load_kw, chillers_running, ...}
    current           JSONB NOT NULL,   -- {setpoints:{key:value}, predicted:{plant_kw, kw_per_rt, ...}}
    recommended       JSONB NOT NULL,   -- {setpoints:{key:value}, predicted:{...}}
    actions           JSONB NOT NULL,   -- [{asset, param, value, unit, label}] writes, in order
    savings_kw        DOUBLE PRECISION,
    savings_pct       DOUBLE PRECISION,
    constraints       JSONB NOT NULL DEFAULT '[]',
    validation_job_id BIGINT,
    validation        JSONB,
    model_id          BIGINT REFERENCES analytics.surrogate_models (id) ON DELETE SET NULL,
    rationale         TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until       TIMESTAMPTZ NOT NULL,
    decided_by        BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    decided_at        TIMESTAMPTZ,
    decision_note     TEXT,
    apply_results     JSONB
);
CREATE INDEX recommendations_created_idx ON analytics.recommendations (created_at DESC);
CREATE INDEX recommendations_status_idx ON analytics.recommendations (status);

-- LBNL FDD point names -> our tags (asset_pattern uses {n} for the unit index).
CREATE TABLE analytics.point_map (
    lbnl_name     TEXT PRIMARY KEY,
    lbnl_unit     TEXT NOT NULL,
    asset_pattern TEXT NOT NULL,
    param_alias   TEXT NOT NULL,
    si_unit       TEXT NOT NULL,
    description   TEXT NOT NULL
);
INSERT INTO analytics.point_map (lbnl_name, lbnl_unit, asset_pattern, param_alias, si_unit, description) VALUES
    ('CWL_SEC_LOAD', 'W', 'CHWS_01', 'cooling_load', 'kW', 'Cooling load (fab demand)'),
    ('CWL_SEC_RW_TEMP', '°F', 'CHWS_01', 'chwr_temp', '°C', 'CHW return header temperature (variable primary: one loop)'),
    ('CWL_SEC_SW_TEMP', '°F', 'CHWS_01', 'chws_temp', '°C', 'CHW supply header temperature (variable primary: one loop)'),
    ('CWL_SEC_CW_FLOW', 'GPM', 'CHWS_01', 'fab_flow', 'm³/h', 'Chilled water flow to the fab risers'),
    ('CWL_PRI_RW_TEMP', '°F', 'CHWS_01', 'chwr_temp', '°C', 'CHW return header temperature'),
    ('CWL_PRI_SW_TEMP', '°F', 'CHWS_01', 'chws_temp', '°C', 'CHW supply header temperature'),
    ('CWL_PRI_CW_FLOW', 'GPM', 'CHWS_01', 'plant_flow', 'm³/h', 'Plant (chiller) chilled water flow'),
    ('CWL_PRI_SW_TEMPSPT', '°F', 'CHWS_01', 'chws_sp', '°C', 'Plant CHW supply set point (reset)'),
    ('TWV_CTRL', '0-1', 'CHWS_01', 'twv_pos', '%', 'Condenser 3-way tower bypass valve position'),
    ('CT_SW_TEMPSPT', '°F', 'CHWS_01', 'cwst_sp', '°C', 'Tower leaving water set point = max(wet bulb + approach, minimum)'),
    ('CDWL_SW_TEMP', '°F', 'CHWS_01', 'cw_supply_temp', '°C', 'Condenser water supply to the chillers (after the 3-way valve)'),
    ('CDWL_RW_TEMP', '°F', 'CHWS_01', 'cw_return_temp', '°C', 'Condenser water return from the chillers'),
    ('CDWL_CW_FLOW', 'GPM', 'CHWS_01', 'cw_flow', 'm³/h', 'Condenser water loop flow'),
    ('OA_TEMP', '°F', 'CHWS_01', 'oa_temp', '°C', 'Outdoor air dry bulb'),
    ('OA_TEMP_WB', '°F', 'CHWS_01', 'wet_bulb', '°C', 'Outdoor air wet bulb'),
    ('CWL_SEC_DPSPTS', 'inH2O', 'CHWS_01', 'riser_dp_sp', 'kPa', 'Riser differential pressure set point (secondary DP analog)'),
    ('CWL_SEC_DP', 'inH2O', 'CHWS_01', 'riser_dp', 'kPa', 'Riser differential pressure (measured)'),
    ('CT_STA{n}', '0/1', 'CT_0{n}', 'running', '0/1', 'Cooling tower on / off'),
    ('CT_RW_TEMP{n}', '°F', 'CT_0{n}', 'cwrt', '°C', 'Water entering the cooling tower'),
    ('CT_SW_TEMP{n}', '°F', 'CT_0{n}', 'cwst', '°C', 'Water leaving the cooling tower (sensor)'),
    ('CT_FAN_SPD{n}', '0-1', 'CT_0{n}', 'fan_speed', '%', 'Cooling tower fan speed'),
    ('CT_FAN_SPD_CTRL{n}', '0-1', 'CHWS_01', 'fan_speed_cmd', '%', 'Common tower fan speed command'),
    ('CT_POW{n}', 'kW', 'CT_0{n}', 'fan_power', 'kW', 'Cooling tower fan power'),
    ('CHL_STA{n}', '0/1', 'CH_0{n}', 'running', '0/1', 'Chiller on / off'),
    ('CHL_COMP_SPD_CTRL{n}', '0-1', 'CH_0{n}', 'load_pct', '%', 'Compressor capacity'),
    ('CHL_CW_FLOW{n}', 'GPM', 'CH_0{n}', 'chw_flow', 'm³/h', 'Chiller chilled water flow'),
    ('CHL_CD_FLOW{n}', 'GPM', 'CH_0{n}', 'cond_flow', 'm³/h', 'Chiller condenser water flow'),
    ('CHL_RW_TEMP{n}', '°F', 'CH_0{n}', 'chwrt', '°C', 'Chilled water entering the chiller'),
    ('CHL_SW_TEMP{n}', '°F', 'CH_0{n}', 'chwst', '°C', 'Chilled water leaving the chiller (sensor)'),
    ('CHL_SWCD_TEMP{n}', '°F', 'CH_0{n}', 'cwlt', '°C', 'Condenser water leaving the chiller'),
    ('CHL_RWCD_TEMP{n}', '°F', 'CH_0{n}', 'cwet', '°C', 'Condenser water entering the chiller'),
    ('CHL_POW{n}', 'kW', 'CH_0{n}', 'power_kw', 'kW', 'Chiller power'),
    ('CDWL_PM_POW{n}', 'kW', 'CWP_0{n}', 'power_kw', 'kW', 'Condenser water pump power'),
    ('CWL_PRI_PM_POW{n}', 'kW', 'CHWP_0{n}', 'power_kw', 'kW', 'Primary chilled water pump power'),
    ('CWL_SEC_PM_SPD{n}', '0-1', 'CHWP_0{n}', 'speed', 'Hz', 'CHW pump speed (variable primary pumps do the secondary duty)'),
    ('CWL_SEC_PM_STA{n}', '0/1', 'CHWP_0{n}', 'running', '0/1', 'CHW pump on / off'),
    ('CWL_SEC_PM_POW{n}', 'kW', 'CHWP_0{n}', 'power_kw', 'kW', 'CHW pump power (variable primary)');

-- Gemini usage accounting (daily budget enforcement).
CREATE TABLE analytics.llm_usage (
    id              BIGSERIAL PRIMARY KEY,
    at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    component       TEXT NOT NULL,          -- agent | instructions | embeddings | ...
    model           TEXT NOT NULL,
    input_tokens    INT NOT NULL DEFAULT 0,
    output_tokens   INT NOT NULL DEFAULT 0,
    thinking_tokens INT NOT NULL DEFAULT 0,
    cost_usd        NUMERIC(12, 6) NOT NULL DEFAULT 0,
    user_id         BIGINT,
    session_id      TEXT
);
CREATE INDEX llm_usage_at_idx ON analytics.llm_usage (at);

-- ---------------------------------------------------------------------------
-- Digital twin simulation jobs (never touch production tables).
-- ---------------------------------------------------------------------------
CREATE TABLE sim.jobs (
    id                BIGSERIAL PRIMARY KEY,
    kind              TEXT NOT NULL DEFAULT 'scenario' CHECK (kind IN ('scenario', 'doe')),
    title             TEXT NOT NULL,
    origin            TEXT NOT NULL CHECK (origin IN ('ui', 'agent', 'optimizer', 'analytics')),
    status            TEXT NOT NULL CHECK (status IN ('PROPOSED', 'QUEUED', 'RUNNING', 'ANALYZING', 'SUCCEEDED', 'FAILED', 'REJECTED', 'CANCELLED')),
    spec              JSONB NOT NULL,
    rationale         TEXT,
    requested_by      BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    approved_by       BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    approved_at       TIMESTAMPTZ,
    decision_note     TEXT,
    agent_session_id  TEXT,
    recommendation_id BIGINT,
    progress          REAL NOT NULL DEFAULT 0,
    summary           JSONB NOT NULL DEFAULT '{}',
    error             TEXT,
    sim_start         TIMESTAMPTZ,
    sim_end           TIMESTAMPTZ,
    step_s            INT NOT NULL DEFAULT 60,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_status_idx ON sim.jobs (status, created_at);
CREATE INDEX jobs_created_idx ON sim.jobs (created_at DESC);

-- One measured series per job / variant / asset / parameter (truth only where it differs).
CREATE TABLE sim.series (
    job_id      BIGINT NOT NULL REFERENCES sim.jobs (id) ON DELETE CASCADE,
    variant     TEXT NOT NULL,
    asset_alias TEXT NOT NULL,
    param_alias TEXT NOT NULL,
    t0          TIMESTAMPTZ NOT NULL,
    step_s      INT NOT NULL,
    measured    REAL[] NOT NULL,
    truth       REAL[],
    PRIMARY KEY (job_id, variant, asset_alias, param_alias)
);

CREATE TABLE sim.states (
    id          BIGSERIAL PRIMARY KEY,
    job_id      BIGINT NOT NULL REFERENCES sim.jobs (id) ON DELETE CASCADE,
    variant     TEXT NOT NULL,
    asset_alias TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('status', 'condition')),
    value       TEXT NOT NULL,
    start_ts    TIMESTAMPTZ NOT NULL,
    end_ts      TIMESTAMPTZ
);
CREATE INDEX states_job_idx ON sim.states (job_id, variant, asset_alias, start_ts);

CREATE TABLE sim.events (
    id          BIGSERIAL PRIMARY KEY,
    job_id      BIGINT NOT NULL REFERENCES sim.jobs (id) ON DELETE CASCADE,
    variant     TEXT NOT NULL,
    ts          TIMESTAMPTZ NOT NULL,
    end_ts      TIMESTAMPTZ,
    asset_alias TEXT,
    kind        TEXT NOT NULL CHECK (kind IN ('status', 'condition', 'alarm', 'abnormal', 'diagnosis', 'anomaly', 'fault',
                                              'control', 'fault_injected', 'fault_cleared', 'trip', 'note')),
    code        TEXT,
    severity    TEXT,
    message     TEXT NOT NULL DEFAULT '',
    value       DOUBLE PRECISION,
    details     JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX events_job_idx ON sim.events (job_id, variant, ts);

-- Design-of-experiments samples (surrogate model training).
CREATE TABLE sim.doe_samples (
    job_id  BIGINT NOT NULL REFERENCES sim.jobs (id) ON DELETE CASCADE,
    idx     INT NOT NULL,
    inputs  JSONB NOT NULL,
    outputs JSONB NOT NULL,
    PRIMARY KEY (job_id, idx)
);

-- ---------------------------------------------------------------------------
-- Permission groups (the frontend PermissionGroupAliasEnum must list them).
-- ---------------------------------------------------------------------------
INSERT INTO core.permission_groups (alias, name, sort_order) VALUES
    ('aiassistant', 'AI Assistant', 21),
    ('aiinsights', 'AI Insights', 22),
    ('simulation', 'Simulation', 23)
ON CONFLICT (alias) DO NOTHING;

INSERT INTO core.permissions (permission_group_id, alias, name)
SELECT g.id, p.alias, initcap(p.alias)
FROM core.permission_groups g
JOIN (VALUES ('aiassistant', 'read'), ('aiinsights', 'read'), ('aiinsights', 'update'),
             ('simulation', 'read'), ('simulation', 'create')) AS p (grp, alias) ON p.grp = g.alias
ON CONFLICT (permission_group_id, alias) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Least-privilege service roles. NOLOGIN here; the backend sets LOGIN + the
-- password from AI_DB_PASSWORD / AI_DB_RO_PASSWORD at start-up.
--   afdd_ai    : Python services (read plant data, read-write rag/analytics/sim/agent)
--   afdd_ai_ro : MCP database server (read-only)
-- dh (gateway secrets), core.sessions and password hashes are never granted.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai') THEN
        CREATE ROLE afdd_ai NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'afdd_ai_ro') THEN
        CREATE ROLE afdd_ai_ro NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA asset, ts, dm, core TO afdd_ai, afdd_ai_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA asset, ts, dm TO afdd_ai, afdd_ai_ro;
GRANT SELECT ON core.sites, core.user_groups, core.user_group_locations TO afdd_ai, afdd_ai_ro;
GRANT SELECT (id, username, full_name, employee_id, role, user_group_id, is_deleted) ON core.users TO afdd_ai, afdd_ai_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA asset, ts, dm GRANT SELECT ON TABLES TO afdd_ai, afdd_ai_ro;

GRANT USAGE, CREATE ON SCHEMA rag, analytics, sim, agent TO afdd_ai;
GRANT ALL ON ALL TABLES IN SCHEMA rag, analytics, sim, agent TO afdd_ai;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA rag, analytics, sim, agent TO afdd_ai;
ALTER DEFAULT PRIVILEGES IN SCHEMA rag, analytics, sim, agent GRANT ALL ON TABLES TO afdd_ai;
ALTER DEFAULT PRIVILEGES IN SCHEMA rag, analytics, sim, agent GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO afdd_ai;

GRANT USAGE ON SCHEMA rag, analytics, sim TO afdd_ai_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA rag, analytics, sim TO afdd_ai_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA rag, analytics, sim GRANT SELECT ON TABLES TO afdd_ai_ro;
