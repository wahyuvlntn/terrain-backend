-- AFDD / Asset Monitoring & Digital Maintenance backend schema.
-- Schemas:
--   core  : users, permissions, sessions, themes, settings, notifications, licenses
--   asset : locations, master data, models, equipment
--   dh    : data hub (gateways and tags)
--   dm    : digital maintenance (alarm codes, issue relations, instructions, orders)
--   ts    : time-series and event history

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS asset;
CREATE SCHEMA IF NOT EXISTS dh;
CREATE SCHEMA IF NOT EXISTS dm;
CREATE SCHEMA IF NOT EXISTS ts;

-- ---------------------------------------------------------------------------
-- core
-- ---------------------------------------------------------------------------

CREATE TABLE core.settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.files (
    id            BIGSERIAL PRIMARY KEY,
    object_key    TEXT NOT NULL UNIQUE,
    original_name TEXT NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size          BIGINT NOT NULL DEFAULT 0,
    created_by    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.user_groups (
    id          BIGSERIAL PRIMARY KEY,
    alias       TEXT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX user_groups_alias_uk ON core.user_groups (lower(alias));

CREATE TABLE core.users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    full_name     TEXT NOT NULL DEFAULT '',
    email         TEXT NOT NULL DEFAULT '',
    employee_id   TEXT,
    photo         TEXT,
    role          TEXT NOT NULL DEFAULT 'basic' CHECK (role IN ('superuser', 'admin', 'basic')),
    user_group_id BIGINT REFERENCES core.user_groups (id) ON DELETE SET NULL,
    is_deleted    BOOLEAN NOT NULL DEFAULT FALSE,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX users_username_active_uk ON core.users (lower(username)) WHERE NOT is_deleted;
CREATE INDEX users_group_idx ON core.users (user_group_id);

CREATE TABLE core.sessions (
    token        TEXT PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_user_idx ON core.sessions (user_id);
CREATE INDEX sessions_expiry_idx ON core.sessions (expires_at);

CREATE TABLE core.permission_groups (
    id         SERIAL PRIMARY KEY,
    alias      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    sort_order INT NOT NULL DEFAULT 0
);

CREATE TABLE core.permissions (
    id                  SERIAL PRIMARY KEY,
    permission_group_id INT NOT NULL REFERENCES core.permission_groups (id) ON DELETE CASCADE,
    alias               TEXT NOT NULL,
    name                TEXT NOT NULL,
    UNIQUE (permission_group_id, alias)
);

CREATE TABLE core.user_group_permissions (
    user_group_id BIGINT NOT NULL REFERENCES core.user_groups (id) ON DELETE CASCADE,
    permission_id INT NOT NULL REFERENCES core.permissions (id) ON DELETE CASCADE,
    value         BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (user_group_id, permission_id)
);

CREATE TABLE core.themes (
    id                        BIGSERIAL PRIMARY KEY,
    name                      TEXT NOT NULL,
    is_system                 BOOLEAN NOT NULL DEFAULT FALSE,
    is_surface_secondary_used BOOLEAN NOT NULL DEFAULT FALSE,
    is_surface_tertiary_used  BOOLEAN NOT NULL DEFAULT FALSE,
    created_by                BIGINT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ
);

CREATE TABLE core.theme_colors (
    theme_id BIGINT NOT NULL REFERENCES core.themes (id) ON DELETE CASCADE,
    name     TEXT NOT NULL,
    hex_code TEXT NOT NULL,
    PRIMARY KEY (theme_id, name)
);

CREATE TABLE core.licenses (
    id          BIGSERIAL PRIMARY KEY,
    key         TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    description TEXT,
    asset_keys  JSONB NOT NULL DEFAULT '[]',
    total       INT NOT NULL DEFAULT 0,
    system_id   TEXT,
    expired_at  TIMESTAMPTZ,
    raw         TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX licenses_name_uk ON core.licenses (lower(name));

-- Notification categories (ids are fixed because the web UI references them).
CREATE TABLE core.notification_categories (
    id               INT PRIMARY KEY,
    alias            TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    permission_group TEXT,
    is_selectable    BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE core.notifications (
    id              BIGSERIAL PRIMARY KEY,
    category        TEXT NOT NULL,
    message         TEXT NOT NULL,
    message_html    TEXT,
    asset_id        BIGINT,
    location_id     BIGINT,
    additional_data JSONB,
    recipients      TEXT[],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX notifications_created_idx ON core.notifications (created_at DESC);
CREATE INDEX notifications_category_idx ON core.notifications (category);

CREATE TABLE core.notification_reads (
    notification_id BIGINT NOT NULL REFERENCES core.notifications (id) ON DELETE CASCADE,
    user_id         BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    read_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (notification_id, user_id)
);
CREATE INDEX notification_reads_user_idx ON core.notification_reads (user_id);

CREATE TABLE core.email_configs (
    alias                 TEXT PRIMARY KEY,
    notification_id       SERIAL,
    notification_name     TEXT NOT NULL DEFAULT 'EPM Email Notification',
    sender_email          TEXT NOT NULL DEFAULT '',
    sender_name           TEXT NOT NULL DEFAULT 'EPM Notification',
    sender_password       TEXT NOT NULL DEFAULT '',
    smtp_host             TEXT NOT NULL DEFAULT '',
    smtp_port             INT NOT NULL DEFAULT 587,
    is_secure             BOOLEAN NOT NULL DEFAULT FALSE,
    interval_minutes      INT NOT NULL DEFAULT 5,
    is_active             BOOLEAN NOT NULL DEFAULT FALSE,
    last_notification_id  BIGINT NOT NULL DEFAULT 0,
    last_run_at           TIMESTAMPTZ,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE core.email_group_categories (
    notification_alias TEXT NOT NULL REFERENCES core.email_configs (alias) ON DELETE CASCADE,
    user_group_id      BIGINT NOT NULL REFERENCES core.user_groups (id) ON DELETE CASCADE,
    category_id        INT NOT NULL REFERENCES core.notification_categories (id) ON DELETE CASCADE,
    PRIMARY KEY (notification_alias, user_group_id, category_id)
);

CREATE TABLE core.email_unsubscribed_users (
    user_id BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    type_id INT NOT NULL DEFAULT 1,
    PRIMARY KEY (user_id, type_id)
);

-- ---------------------------------------------------------------------------
-- asset
-- ---------------------------------------------------------------------------

CREATE TABLE asset.locations (
    id              BIGSERIAL PRIMARY KEY,
    parent_id       BIGINT REFERENCES asset.locations (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    image_path      TEXT,
    availability_th DOUBLE PRECISION NOT NULL DEFAULT 0,
    performance_th  DOUBLE PRECISION NOT NULL DEFAULT 0,
    quality_th      DOUBLE PRECISION NOT NULL DEFAULT 0,
    energy_th       DOUBLE PRECISION,
    carbon_th       DOUBLE PRECISION,
    cost_th         DOUBLE PRECISION,
    currency_id     INT,
    timezone        TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ
);
CREATE INDEX locations_parent_idx ON asset.locations (parent_id);

CREATE TABLE core.user_group_locations (
    user_group_id BIGINT NOT NULL REFERENCES core.user_groups (id) ON DELETE CASCADE,
    location_id   BIGINT NOT NULL REFERENCES asset.locations (id) ON DELETE CASCADE,
    PRIMARY KEY (user_group_id, location_id)
);

CREATE TABLE asset.types (
    id       BIGSERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX types_name_uk ON asset.types (lower(name));

CREATE TABLE asset.brands (
    id       BIGSERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX brands_name_uk ON asset.brands (lower(name));

CREATE TABLE asset.series (
    id       BIGSERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX series_name_uk ON asset.series (lower(name));

-- Issue categories / issues used by model alarms ("feedback" master data).
CREATE TABLE asset.feedback_categories (
    id       BIGSERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX feedback_categories_name_uk ON asset.feedback_categories (lower(name));

CREATE TABLE asset.feedback_issues (
    id          BIGSERIAL PRIMARY KEY,
    category_id BIGINT NOT NULL REFERENCES asset.feedback_categories (id) ON DELETE RESTRICT,
    issue_code  TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,
    order_id    INT NOT NULL DEFAULT 0
);
CREATE INDEX feedback_issues_category_idx ON asset.feedback_issues (category_id);

-- Energy-monitoring master data (kept for API completeness).
CREATE TABLE asset.parameter_gains (
    id   SERIAL PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE asset.power_meter_types (
    id       SERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE TABLE asset.source_types (
    id          SERIAL PRIMARY KEY,
    category_id INT NOT NULL,
    name        TEXT NOT NULL,
    order_id    INT NOT NULL DEFAULT 0
);
CREATE TABLE asset.cost_types (
    id       SERIAL PRIMARY KEY,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);

CREATE TABLE asset.models (
    id                         BIGSERIAL PRIMARY KEY,
    name                       TEXT NOT NULL,
    description                TEXT NOT NULL DEFAULT '',
    type_id                    BIGINT NOT NULL REFERENCES asset.types (id) ON DELETE RESTRICT,
    brand_id                   BIGINT NOT NULL REFERENCES asset.brands (id) ON DELETE RESTRICT,
    series_id                  BIGINT NOT NULL REFERENCES asset.series (id) ON DELETE RESTRICT,
    base_model_id              BIGINT REFERENCES asset.models (id) ON DELETE RESTRICT,
    unplanned_down_script      TEXT,
    planned_down_script        TEXT,
    running_script             TEXT,
    running_hour_warning       INT NOT NULL DEFAULT 0,
    running_hour_critical      INT NOT NULL DEFAULT 0,
    running_hour_notify_warn   BOOLEAN NOT NULL DEFAULT FALSE,
    running_hour_notify_crit   BOOLEAN NOT NULL DEFAULT FALSE,
    running_hour_auto_maint    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ
);
CREATE UNIQUE INDEX models_name_uk ON asset.models (lower(name));

CREATE TABLE asset.model_parameters (
    id                BIGSERIAL PRIMARY KEY,
    model_id          BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    alias             TEXT NOT NULL,
    name              TEXT NOT NULL,
    unit              TEXT NOT NULL DEFAULT '',
    data_type         TEXT NOT NULL DEFAULT 'number' CHECK (data_type IN ('string', 'number', 'boolean')),
    scale             INT,
    gain_id           INT,
    parameter_type_id INT,
    base_parameter_id BIGINT REFERENCES asset.model_parameters (id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ,
    UNIQUE (model_id, alias)
);

CREATE TABLE asset.model_alarms (
    id                BIGSERIAL PRIMARY KEY,
    model_id          BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    base_alarm_id     BIGINT REFERENCES asset.model_alarms (id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    criticality       INT NOT NULL DEFAULT 1 CHECK (criticality IN (1, 2)),
    issue_category_id BIGINT REFERENCES asset.feedback_categories (id) ON DELETE SET NULL,
    issue_id          BIGINT REFERENCES asset.feedback_issues (id) ON DELETE SET NULL,
    message           TEXT NOT NULL DEFAULT '',
    script            TEXT,
    is_disabled       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ
);
CREATE INDEX model_alarms_model_idx ON asset.model_alarms (model_id);
CREATE UNIQUE INDEX model_alarms_name_uk ON asset.model_alarms (model_id, lower(name));

CREATE TABLE asset.model_dynamic_alarms (
    id            BIGSERIAL PRIMARY KEY,
    model_id      BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    base_alarm_id BIGINT REFERENCES asset.model_dynamic_alarms (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    script        TEXT,
    is_disabled   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ
);
CREATE INDEX model_dynamic_alarms_model_idx ON asset.model_dynamic_alarms (model_id);
CREATE UNIQUE INDEX model_dynamic_alarms_name_uk ON asset.model_dynamic_alarms (model_id, lower(name));

-- ---------------------------------------------------------------------------
-- dh (data hub)
-- ---------------------------------------------------------------------------

CREATE TABLE dh.gateways (
    id          BIGSERIAL PRIMARY KEY,
    alias       TEXT NOT NULL,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    description TEXT,
    params      JSONB NOT NULL DEFAULT '{}',
    secrets     JSONB NOT NULL DEFAULT '{}',
    is_active   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX gateways_alias_uk ON dh.gateways (lower(alias));

CREATE TABLE dh.gateway_tags (
    id          BIGSERIAL PRIMARY KEY,
    gateway_id  BIGINT NOT NULL REFERENCES dh.gateways (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    address     TEXT NOT NULL DEFAULT '',
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (gateway_id, name)
);

-- ---------------------------------------------------------------------------
-- equipment (asset)
-- ---------------------------------------------------------------------------

CREATE TABLE asset.equipment (
    id                 BIGSERIAL PRIMARY KEY,
    alias              TEXT NOT NULL,
    name               TEXT NOT NULL,
    image_path         TEXT,
    model_id           BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE RESTRICT,
    location_id        BIGINT NOT NULL REFERENCES asset.locations (id) ON DELETE RESTRICT,
    license_id         BIGINT REFERENCES core.licenses (id) ON DELETE SET NULL,
    is_utility         BOOLEAN NOT NULL DEFAULT FALSE,
    installed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    aspect_critical    JSONB NOT NULL DEFAULT '[]',
    aspect_supporting  JSONB NOT NULL DEFAULT '[]',
    -- Status forced by maintenance work ("stop equipment" when starting work).
    forced_status_id   INT,
    forced_by_order_id BIGINT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ
);
CREATE UNIQUE INDEX equipment_alias_uk ON asset.equipment (lower(alias));
CREATE UNIQUE INDEX equipment_name_uk ON asset.equipment (lower(name));
CREATE INDEX equipment_location_idx ON asset.equipment (location_id);
CREATE INDEX equipment_model_idx ON asset.equipment (model_id);

CREATE TABLE asset.equipment_parameters (
    id                     BIGSERIAL PRIMARY KEY,
    equipment_id           BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    model_parameter_id     BIGINT NOT NULL REFERENCES asset.model_parameters (id) ON DELETE CASCADE,
    gateway_id             BIGINT REFERENCES dh.gateways (id) ON DELETE SET NULL,
    tag_name               TEXT,
    is_logged              BOOLEAN NOT NULL DEFAULT TRUE,
    is_displayed           BOOLEAN NOT NULL DEFAULT TRUE,
    gain_id                INT,
    warning_lower_limit    DOUBLE PRECISION,
    warning_upper_limit    DOUBLE PRECISION,
    warning_threshold_type TEXT NOT NULL DEFAULT '',
    critical_lower_limit   DOUBLE PRECISION,
    critical_upper_limit   DOUBLE PRECISION,
    critical_threshold_type TEXT NOT NULL DEFAULT '',
    spc_usl                DOUBLE PRECISION,
    spc_lsl                DOUBLE PRECISION,
    spc_ucl                DOUBLE PRECISION,
    spc_lcl                DOUBLE PRECISION,
    spc_center_line        DOUBLE PRECISION,
    spc_sigma              DOUBLE PRECISION,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ,
    UNIQUE (equipment_id, model_parameter_id)
);
CREATE INDEX equipment_parameters_gateway_idx ON asset.equipment_parameters (gateway_id, tag_name);

-- ---------------------------------------------------------------------------
-- dm (digital maintenance)
-- ---------------------------------------------------------------------------

CREATE TABLE dm.alarm_codes (
    id       BIGSERIAL PRIMARY KEY,
    code     TEXT NOT NULL,
    name     TEXT NOT NULL,
    order_id INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX alarm_codes_code_uk ON dm.alarm_codes (lower(code));

CREATE TABLE dm.alarm_mappings (
    model_id           BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    alarm_code_id      BIGINT NOT NULL REFERENCES dm.alarm_codes (id) ON DELETE CASCADE,
    model_parameter_id BIGINT NOT NULL REFERENCES asset.model_parameters (id) ON DELETE CASCADE,
    PRIMARY KEY (model_id, alarm_code_id, model_parameter_id)
);

CREATE TABLE dm.issue_relations (
    id         BIGSERIAL PRIMARY KEY,
    model_id   BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    flow       JSONB NOT NULL DEFAULT '{"nodes":[],"edges":[]}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX issue_relations_name_uk ON dm.issue_relations (model_id, lower(name));

CREATE TABLE dm.instruction_types (
    id   INT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE dm.instructions (
    id                  BIGSERIAL PRIMARY KEY,
    alarm_code_id       BIGINT REFERENCES dm.alarm_codes (id) ON DELETE SET NULL,
    title               TEXT NOT NULL,
    instruction_type_id INT NOT NULL REFERENCES dm.instruction_types (id),
    duration            INT NOT NULL DEFAULT 0,
    description         TEXT NOT NULL DEFAULT '',
    content             TEXT,
    pdf_url             TEXT,
    pdf_name            TEXT,
    created_by          BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ,
    is_deleted          BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX instructions_alarm_code_idx ON dm.instructions (alarm_code_id);

CREATE TABLE dm.instruction_models (
    instruction_id BIGINT NOT NULL REFERENCES dm.instructions (id) ON DELETE CASCADE,
    model_id       BIGINT NOT NULL REFERENCES asset.models (id) ON DELETE CASCADE,
    PRIMARY KEY (instruction_id, model_id)
);

CREATE TABLE dm.maintenance_types (
    id   INT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE dm.priorities (
    id   INT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE dm.severities (
    id   INT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE dm.repeat_types (
    id   INT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE dm.maintenance_statuses (
    id   INT PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL
);

CREATE TABLE dm.maintenance_schedules (
    id             BIGSERIAL PRIMARY KEY,
    repeat_type_id INT NOT NULL REFERENCES dm.repeat_types (id),
    repeat_end     TIMESTAMPTZ,
    anchor_start   TIMESTAMPTZ NOT NULL,
    duration_sec   BIGINT NOT NULL DEFAULT 0,
    generated_until TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dm.maintenance_orders (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL DEFAULT '',
    schedule_id         BIGINT REFERENCES dm.maintenance_schedules (id) ON DELETE SET NULL,
    asset_id            BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    maintenance_type_id INT NOT NULL REFERENCES dm.maintenance_types (id),
    priority_id         INT REFERENCES dm.priorities (id),
    severity_id         INT NOT NULL REFERENCES dm.severities (id),
    status_id           INT NOT NULL REFERENCES dm.maintenance_statuses (id),
    description         TEXT NOT NULL DEFAULT '',
    planned_start       TIMESTAMPTZ NOT NULL,
    planned_end         TIMESTAMPTZ,
    requested_by        BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    approved_by         BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    edited_by           BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    reject_reason       TEXT NOT NULL DEFAULT '',
    from_system         BOOLEAN NOT NULL DEFAULT FALSE,
    issue_relation_id   BIGINT REFERENCES dm.issue_relations (id) ON DELETE SET NULL,
    is_compound         BOOLEAN NOT NULL DEFAULT FALSE,
    is_deduction        BOOLEAN NOT NULL DEFAULT FALSE,
    degradation_name    TEXT,
    detected_at         TIMESTAMPTZ,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    closed_at           TIMESTAMPTZ,
    scheduled_notified  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ
);
CREATE INDEX maintenance_orders_asset_idx ON dm.maintenance_orders (asset_id);
CREATE INDEX maintenance_orders_planned_idx ON dm.maintenance_orders (planned_start);
CREATE INDEX maintenance_orders_status_idx ON dm.maintenance_orders (status_id);
CREATE INDEX maintenance_orders_schedule_idx ON dm.maintenance_orders (schedule_id);

CREATE TABLE dm.maintenance_order_technicians (
    order_id BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    PRIMARY KEY (order_id, user_id)
);

-- Issues (alarm codes) attached to an order, with technician validation.
-- The instructions of an order are the model instructions of these codes.
CREATE TABLE dm.maintenance_order_issues (
    order_id            BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    alarm_code_id       BIGINT NOT NULL REFERENCES dm.alarm_codes (id) ON DELETE CASCADE,
    is_other            BOOLEAN NOT NULL DEFAULT FALSE,
    position            INT NOT NULL DEFAULT 0,
    is_valid_diagnostic BOOLEAN,
    is_valid_repair     BOOLEAN,
    alarm_status        TEXT NOT NULL DEFAULT 'WARNING',
    detected_at         TIMESTAMPTZ,
    PRIMARY KEY (order_id, alarm_code_id)
);

-- Parameters that contributed to an early-warning (degradation) order.
CREATE TABLE dm.maintenance_order_parameters (
    order_id               BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    equipment_parameter_id BIGINT NOT NULL REFERENCES asset.equipment_parameters (id) ON DELETE CASCADE,
    condition              TEXT NOT NULL DEFAULT 'WARNING',
    message                TEXT,
    detected_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, equipment_parameter_id)
);

CREATE TABLE dm.maintenance_order_events (
    id         BIGSERIAL PRIMARY KEY,
    order_id   BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    status     TEXT NOT NULL,
    details    JSONB NOT NULL DEFAULT '{}',
    created_by BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX maintenance_order_events_order_idx ON dm.maintenance_order_events (order_id, created_at);

CREATE TABLE dm.work_updates (
    id         BIGSERIAL PRIMARY KEY,
    order_id   BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    note       TEXT NOT NULL,
    image_urls TEXT[] NOT NULL DEFAULT '{}',
    issue_ids  BIGINT[] NOT NULL DEFAULT '{}',
    created_by BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX work_updates_order_idx ON dm.work_updates (order_id);

-- Active work intervals (start/resume -> pause/done) used for actual time.
CREATE TABLE dm.work_intervals (
    id         BIGSERIAL PRIMARY KEY,
    order_id   BIGINT NOT NULL REFERENCES dm.maintenance_orders (id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ
);
CREATE INDEX work_intervals_order_idx ON dm.work_intervals (order_id);

CREATE TABLE dm.personnel (
    user_id    BIGINT PRIMARY KEY REFERENCES core.users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ
);
CREATE TABLE dm.personnel_buildings (
    user_id     BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    location_id BIGINT NOT NULL REFERENCES asset.locations (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, location_id)
);
CREATE TABLE dm.personnel_equipment_types (
    user_id BIGINT NOT NULL REFERENCES core.users (id) ON DELETE CASCADE,
    type_id BIGINT NOT NULL REFERENCES asset.types (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, type_id)
);

-- Pre-computed reliability KPIs (refreshed by the scheduler).
CREATE TABLE dm.asset_kpis (
    asset_id     BIGINT PRIMARY KEY REFERENCES asset.equipment (id) ON DELETE CASCADE,
    mtbf_hours   DOUBLE PRECISION,
    mttr_hours   DOUBLE PRECISION,
    failures     INT NOT NULL DEFAULT 0,
    repairs      INT NOT NULL DEFAULT 0,
    computed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- ts (time series & event history)
-- ---------------------------------------------------------------------------

CREATE TABLE ts.parameter_values (
    equipment_parameter_id BIGINT NOT NULL,
    ts                     TIMESTAMPTZ NOT NULL,
    value                  DOUBLE PRECISION,
    value_text             TEXT,
    PRIMARY KEY (equipment_parameter_id, ts)
);
CREATE INDEX parameter_values_ts_brin ON ts.parameter_values USING BRIN (ts);

CREATE TABLE ts.asset_status (
    id        BIGSERIAL PRIMARY KEY,
    asset_id  BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    status_id INT NOT NULL,
    start_ts  TIMESTAMPTZ NOT NULL,
    end_ts    TIMESTAMPTZ
);
CREATE INDEX asset_status_asset_idx ON ts.asset_status (asset_id, start_ts);
CREATE UNIQUE INDEX asset_status_open_uk ON ts.asset_status (asset_id) WHERE end_ts IS NULL;

CREATE TABLE ts.asset_condition (
    id        BIGSERIAL PRIMARY KEY,
    asset_id  BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    condition TEXT NOT NULL,
    start_ts  TIMESTAMPTZ NOT NULL,
    end_ts    TIMESTAMPTZ
);
CREATE INDEX asset_condition_asset_idx ON ts.asset_condition (asset_id, start_ts);
CREATE UNIQUE INDEX asset_condition_open_uk ON ts.asset_condition (asset_id) WHERE end_ts IS NULL;

-- Alarm events: ALARM (model / dynamic alarms) and ABNORMAL (threshold breaches).
CREATE TABLE ts.alarm_events (
    id                     BIGSERIAL PRIMARY KEY,
    asset_id               BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    alarm_type             TEXT NOT NULL CHECK (alarm_type IN ('ALARM', 'ABNORMAL')),
    source_key             TEXT NOT NULL,
    model_alarm_id         BIGINT,
    equipment_parameter_id BIGINT,
    alarm_code_id          BIGINT,
    name                   TEXT NOT NULL,
    severity               TEXT NOT NULL CHECK (severity IN ('WARNING', 'CRITICAL')),
    message                TEXT NOT NULL DEFAULT '',
    start_ts               TIMESTAMPTZ NOT NULL,
    end_ts                 TIMESTAMPTZ
);
CREATE INDEX alarm_events_asset_idx ON ts.alarm_events (asset_id, start_ts);
CREATE INDEX alarm_events_start_idx ON ts.alarm_events (start_ts);
CREATE UNIQUE INDEX alarm_events_open_uk ON ts.alarm_events (asset_id, source_key) WHERE end_ts IS NULL;

-- Running-hour notification bookkeeping.
CREATE TABLE ts.running_hour_flags (
    asset_id         BIGINT PRIMARY KEY REFERENCES asset.equipment (id) ON DELETE CASCADE,
    baseline_hours   DOUBLE PRECISION NOT NULL DEFAULT 0,
    warning_sent     BOOLEAN NOT NULL DEFAULT FALSE,
    critical_sent    BOOLEAN NOT NULL DEFAULT FALSE,
    maintenance_made BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Seed data
-- ---------------------------------------------------------------------------

INSERT INTO core.permission_groups (alias, name, sort_order) VALUES
    ('locationoverview', 'Location Overview', 1),
    ('equipmentoverview', 'Equipment Overview', 2),
    ('alarmsreport', 'Alarms Report', 3),
    ('maintenanceorder', 'Maintenance Order', 4),
    ('maintenanceperformance', 'Maintenance Performance', 5),
    ('maintenanceinstruction', 'Maintenance Instruction', 6),
    ('personnelmanagement', 'Personnel Management', 7),
    ('earlywarningdiagnostics', 'Early Warning & Diagnostics', 8),
    ('earlywarningdashboard', 'Early Warning Dashboard', 9),
    ('themeeditor', 'Theme Editor', 10),
    ('notificationmanager', 'Notification Manager', 11),
    ('locationmanager', 'Location Manager', 12),
    ('datasourcemanager', 'Data Source Manager', 13),
    ('modelmanager', 'Model Manager', 14),
    ('equipmentmanager', 'Equipment Manager', 15),
    ('masterdata', 'Master Data', 16),
    ('licensemanager', 'License Manager', 17),
    ('alarmmapping', 'Alarm Mapping', 18),
    ('issuemapping', 'Issue Mapping', 19);

INSERT INTO core.permissions (permission_group_id, alias, name)
SELECT g.id, p.alias, initcap(p.alias)
FROM core.permission_groups g
JOIN (VALUES
    ('locationoverview', 'read'), ('locationoverview', 'update'),
    ('equipmentoverview', 'read'), ('equipmentoverview', 'update'), ('equipmentoverview', 'create'), ('equipmentoverview', 'delete'),
    ('alarmsreport', 'read'), ('alarmsreport', 'update'),
    ('maintenanceorder', 'read'), ('maintenanceorder', 'update'), ('maintenanceorder', 'create'), ('maintenanceorder', 'delete'),
    ('maintenanceorder', 'approve'), ('maintenanceorder', 'report'),
    ('maintenanceperformance', 'read'),
    ('maintenanceinstruction', 'read'), ('maintenanceinstruction', 'update'), ('maintenanceinstruction', 'create'), ('maintenanceinstruction', 'delete'),
    ('personnelmanagement', 'read'), ('personnelmanagement', 'update'),
    ('earlywarningdiagnostics', 'read'), ('earlywarningdiagnostics', 'update'),
    ('earlywarningdashboard', 'read'),
    ('themeeditor', 'read'), ('themeeditor', 'update'), ('themeeditor', 'create'), ('themeeditor', 'delete'),
    ('notificationmanager', 'read'), ('notificationmanager', 'update'),
    ('locationmanager', 'read'), ('locationmanager', 'update'),
    ('datasourcemanager', 'read'), ('datasourcemanager', 'update'), ('datasourcemanager', 'create'), ('datasourcemanager', 'delete'),
    ('modelmanager', 'read'), ('modelmanager', 'update'), ('modelmanager', 'create'), ('modelmanager', 'delete'),
    ('equipmentmanager', 'read'), ('equipmentmanager', 'update'), ('equipmentmanager', 'create'), ('equipmentmanager', 'delete'),
    ('masterdata', 'read'), ('masterdata', 'update'), ('masterdata', 'create'), ('masterdata', 'delete'),
    ('licensemanager', 'read'), ('licensemanager', 'update'), ('licensemanager', 'create'), ('licensemanager', 'delete'),
    ('alarmmapping', 'read'), ('alarmmapping', 'update'),
    ('issuemapping', 'read'), ('issuemapping', 'update'), ('issuemapping', 'create'), ('issuemapping', 'delete')
) AS p (grp, alias) ON p.grp = g.alias;

INSERT INTO core.notification_categories (id, alias, name, permission_group, is_selectable) VALUES
    (1, 'FEEDBACK_RUNNING', 'Feedback Running', 'feedbackssubmission', FALSE),
    (2, 'FEEDBACK_IDLE', 'Feedback Idle', 'feedbackssubmission', FALSE),
    (3, 'FEEDBACK_DOWN', 'Feedback Down', 'feedbackssubmission', FALSE),
    (4, 'FEEDBACK_OFFLINE', 'Feedback Offline', 'feedbackssubmission', FALSE),
    (5, 'ALARM_WARNING', 'Alarm Warning', 'alarmsreport', TRUE),
    (6, 'ALARM_CRITICAL', 'Alarm Critical', 'alarmsreport', TRUE),
    (7, 'PARAMETER_WARNING', 'Parameter Warning', 'equipmentoverview', TRUE),
    (8, 'PARAMETER_CRITICAL', 'Parameter Critical', 'equipmentoverview', TRUE),
    (9, 'STATUS_RUNNING', 'Status Running', 'equipmentoverview', TRUE),
    (10, 'STATUS_IDLE', 'Status Idle', 'equipmentoverview', TRUE),
    (11, 'STATUS_DOWN', 'Status Down', 'equipmentoverview', TRUE),
    (12, 'STATUS_OFFLINE', 'Status Offline', 'equipmentoverview', TRUE),
    (13, 'OEE_WARNING', 'OEE Warning', 'locationoverview', FALSE),
    (14, 'OEE_CRITICAL', 'OEE Critical', 'locationoverview', FALSE),
    (15, 'SPC_OUT_OF_CONTROL', 'SPC Out of Control', 'equipmentoverview', TRUE),
    (16, 'SPC_OUT_OF_SPEC', 'SPC Out of Spec', 'equipmentoverview', TRUE),
    (17, 'LICENSE_EXPIRY', 'License Expiry', 'licensemanager', TRUE),
    (18, 'PRODUCTION_START', 'Production Start', 'productiontracker', FALSE),
    (19, 'PRODUCTION_PAUSED', 'Production Paused', 'productiontracker', FALSE),
    (20, 'PRODUCTION_FINISHED', 'Production Finished', 'productiontracker', FALSE),
    (21, 'PRODUCTION_RESUME', 'Production Resume', 'productiontracker', FALSE),
    (22, 'MAINTENANCE_ORDER_CREATED', 'Maintenance Order Created', 'maintenanceorder', TRUE),
    (23, 'MAINTENANCE_ORDER_APPROVED', 'Maintenance Order Approved', 'maintenanceorder', TRUE),
    (24, 'MAINTENANCE_ORDER_REJECTED', 'Maintenance Order Rejected', 'maintenanceorder', TRUE),
    (25, 'MAINTENANCE_ORDER_ASSIGNED', 'Maintenance Order Assigned', 'maintenanceorder', TRUE),
    (26, 'MAINTENANCE_ORDER_STARTED', 'Maintenance Order Started', 'maintenanceorder', TRUE),
    (27, 'MAINTENANCE_ORDER_PAUSED', 'Maintenance Order Paused', 'maintenanceorder', TRUE),
    (28, 'MAINTENANCE_ORDER_COMPLETED', 'Maintenance Order Completed', 'maintenanceorder', TRUE),
    (29, 'MAINTENANCE_ORDER_RESUMED', 'Maintenance Order Resumed', 'maintenanceorder', TRUE),
    (30, 'MAINTENANCE_ORDER_CLOSED_REQUESTOR', 'Maintenance Order Closed (Requestor)', 'maintenanceorder', TRUE),
    (31, 'MAINTENANCE_ORDER_CLOSED_TECHNICIAN', 'Maintenance Order Closed (Technician)', 'maintenanceorder', TRUE),
    (32, 'MAINTENANCE_ORDER_SCHEDULED', 'Maintenance Order Scheduled', 'maintenanceorder', TRUE),
    (33, 'STATUS_PLANNED_DOWN', 'Status Planned Down', 'equipmentoverview', TRUE),
    (34, 'RUNNING_HOUR_WARNING', 'Running Hour Warning', 'equipmentoverview', TRUE),
    (35, 'RUNNING_HOUR_CRITICAL', 'Running Hour Critical', 'equipmentoverview', TRUE),
    (36, 'SPC_8_POINTS_ZONE_A', 'SPC 8 Points Beyond Zone C', 'equipmentoverview', TRUE),
    (37, 'SPC_7_POINTS_MONOTONIC', 'SPC 7 Points Trending', 'equipmentoverview', TRUE),
    (38, 'SPC_14_POINTS_ALTERNATING', 'SPC 14 Points Alternating', 'equipmentoverview', TRUE),
    (39, 'SPC_2_OUT_OF_3_POINTS_ZONE_A_OR_BEYOND', 'SPC 2 of 3 Points in Zone A', 'equipmentoverview', TRUE),
    (40, 'SPC_4_OUT_OF_5_POINTS_ZONE_B_OR_BEYOND', 'SPC 4 of 5 Points in Zone B', 'equipmentoverview', TRUE),
    (41, 'SPC_15_POINTS_ZONE_C', 'SPC 15 Points in Zone C', 'equipmentoverview', TRUE);

INSERT INTO core.email_configs (alias) VALUES ('epm_email_notification');

INSERT INTO asset.feedback_categories (id, name, order_id) VALUES (1, 'General', 1);
SELECT setval(pg_get_serial_sequence('asset.feedback_categories', 'id'), 1);
INSERT INTO asset.feedback_issues (id, category_id, issue_code, name, order_id) VALUES (1, 1, 'GEN-001', 'Others', 1);
SELECT setval(pg_get_serial_sequence('asset.feedback_issues', 'id'), 1);

INSERT INTO asset.parameter_gains (name) VALUES ('1'), ('10'), ('100'), ('1000');
INSERT INTO asset.power_meter_types (name, order_id) VALUES ('Main Grid', 1), ('Sub Meter', 2);
INSERT INTO asset.source_types (category_id, name, order_id) VALUES (1, 'Grid', 1), (1, 'Solar', 2);
INSERT INTO asset.cost_types (name, order_id) VALUES ('Peak', 1), ('Off Peak', 2);

INSERT INTO dm.instruction_types (id, name) VALUES (1, 'Diagnostic'), (2, 'Repair'), (3, 'Severity');
INSERT INTO dm.maintenance_types (id, name) VALUES (1, 'CORRECTIVE'), (2, 'PREVENTIVE');
INSERT INTO dm.priorities (id, name) VALUES (1, 'HIGH'), (2, 'MEDIUM'), (3, 'LOW');
INSERT INTO dm.severities (id, name) VALUES (1, 'CRITICAL'), (2, 'HIGH'), (3, 'MEDIUM'), (4, 'LOW');
INSERT INTO dm.repeat_types (id, name) VALUES (1, 'DAILY'), (2, 'WEEKLY'), (3, 'MONTHLY'), (4, 'YEARLY');
INSERT INTO dm.maintenance_statuses (id, code, name) VALUES
    (1, 'REQUESTED', 'Requested'),
    (2, 'REJECTED', 'Rejected'),
    (3, 'OPEN', 'Open'),
    (4, 'IN_PROGRESS', 'In Progress'),
    (5, 'DONE', 'Done'),
    (6, 'CLOSED', 'Closed'),
    (7, 'PAUSED', 'Paused');
