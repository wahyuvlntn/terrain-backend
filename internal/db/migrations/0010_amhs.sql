-- FAB1 overhead hoist transport (AMHS): where each lot's FOUP is, and every transfer of it.

ALTER TABLE prod.lots ADD COLUMN IF NOT EXISTS location TEXT;

CREATE TABLE IF NOT EXISTS prod.transfers (
    id           BIGINT PRIMARY KEY,
    lot_code     TEXT NOT NULL,
    product_code TEXT,
    kind         TEXT NOT NULL CHECK (kind IN ('deliver', 'store', 'ship')),
    from_loc     TEXT NOT NULL,
    to_loc       TEXT NOT NULL,
    vehicle      TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    picked_at    TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ NOT NULL,
    distance_m   DOUBLE PRECISION,
    busy_s       DOUBLE PRECISION -- the vehicle's time on the job (drive to the pickup, carry, back to the loop)
);
CREATE INDEX IF NOT EXISTS transfers_requested_idx ON prod.transfers (requested_at);
CREATE INDEX IF NOT EXISTS transfers_lot_idx ON prod.transfers (lot_code);

