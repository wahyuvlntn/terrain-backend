-- Two-way control from the digital twin: writable model parameters (set points
-- and control modes), an audit trail of every write request, and the
-- "digitaltwin" permission group ("update" = operate the plant from the twin).

-- A writable parameter is a set point (numeric range) or a control mode
-- (options). control_mode names the mode parameter that must be out of
-- automatic (value <> 0) for a set point to be written; control_target names
-- the measured parameter the control drives (shown with its instrument).
ALTER TABLE asset.model_parameters
    ADD COLUMN is_writable     BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN control_min     DOUBLE PRECISION,
    ADD COLUMN control_max     DOUBLE PRECISION,
    ADD COLUMN control_step    DOUBLE PRECISION,
    ADD COLUMN control_options JSONB,
    ADD COLUMN control_mode    TEXT,
    ADD COLUMN control_target  TEXT;

-- Write requests: PENDING until the plant reports the new value back
-- (APPLIED), refused by validation (REJECTED) or never confirmed (TIMEOUT).
CREATE TABLE asset.control_requests (
    id                     BIGSERIAL PRIMARY KEY,
    equipment_id           BIGINT NOT NULL REFERENCES asset.equipment (id) ON DELETE CASCADE,
    equipment_parameter_id BIGINT NOT NULL REFERENCES asset.equipment_parameters (id) ON DELETE CASCADE,
    user_id                BIGINT REFERENCES core.users (id) ON DELETE SET NULL,
    old_value              DOUBLE PRECISION,
    value                  DOUBLE PRECISION NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'APPLIED', 'REJECTED', 'TIMEOUT')),
    reason                 TEXT,
    requested_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at           TIMESTAMPTZ
);
CREATE INDEX control_requests_equipment_idx ON asset.control_requests (equipment_id, requested_at DESC);
CREATE INDEX control_requests_pending_idx ON asset.control_requests (requested_at) WHERE status = 'PENDING';

INSERT INTO core.permission_groups (alias, name, sort_order) VALUES ('digitaltwin', 'Digital Twin', 20)
ON CONFLICT (alias) DO NOTHING;

INSERT INTO core.permissions (permission_group_id, alias, name)
SELECT g.id, p.alias, initcap(p.alias)
FROM core.permission_groups g CROSS JOIN (VALUES ('read'), ('update')) AS p (alias)
WHERE g.alias = 'digitaltwin'
ON CONFLICT (permission_group_id, alias) DO NOTHING;
