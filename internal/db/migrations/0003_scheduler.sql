-- Scheduler bookkeeping.

-- Smallest "hours before expiry" threshold already notified for a license.
ALTER TABLE core.licenses ADD COLUMN expiry_notified_hours INT;

-- Running hours are counted from the last completed maintenance order.
ALTER TABLE ts.running_hour_flags ADD COLUMN reset_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS maintenance_orders_asset_completed_idx ON dm.maintenance_orders (asset_id, completed_at);
CREATE INDEX IF NOT EXISTS notifications_location_idx ON core.notifications (location_id);
