-- Monthly range partitioning of ts.parameter_values (docs/agentic/DESIGN.md 3, 4.8).
--
-- Partitions are named ts.parameter_values_yYYYYmMM and cover one UTC calendar
-- month [first day 00:00 UTC, first day of the next month). There is no DEFAULT
-- partition: rows need a partition ahead of time. The scheduler job
-- "partition-maintenance" (also run at start-up, before the engine logs values)
-- keeps the months from the start of the retention window (at most 24 months
-- back) to now + 3 months, the demo history generator calls
-- ts.ensure_parameter_values_partitions for its window before COPY, and
-- retention drops whole months with ts.drop_parameter_values_partitions_before
-- (the old ctid batch delete would match ctids of every partition and delete
-- recent rows).
--
-- The conversion runs inside the migration transaction: rename the table, create
-- the partitioned parent and its months, copy every row, drop the old table, then
-- build the primary key and the BRIN index (one bulk build per partition is much
-- faster than maintaining the indexes row by row during the copy). About 10 s
-- for 3.1 M rows; readers and writers of the table wait for it.

SET LOCAL maintenance_work_mem = '256MB';
-- Fail (and let the restarted backend retry) rather than queue behind a long
-- reader while every other session queues behind this migration.
SET LOCAL lock_timeout = '30s';

-- Creates the missing monthly partitions for every month from from_month to
-- to_month, both inclusive (the dates are truncated to their month; months are
-- UTC). Idempotent; returns the number of partitions created. A new month is
-- created as a plain table and attached, which needs only a SHARE UPDATE
-- EXCLUSIVE lock on the parent (CREATE TABLE ... PARTITION OF would take ACCESS
-- EXCLUSIVE and stall the engine inserts and every reader while it waits).
CREATE FUNCTION ts.ensure_parameter_values_partitions(from_month date, to_month date)
RETURNS int
LANGUAGE plpgsql
SET timezone = 'UTC'
SET lock_timeout = '5s'
AS $$
DECLARE
    m       date;
    last_m  date;
    part    text;
    created int := 0;
BEGIN
    IF from_month IS NULL OR to_month IS NULL OR NOT isfinite(from_month) OR NOT isfinite(to_month) THEN
        RAISE EXCEPTION 'ensure_parameter_values_partitions: finite from_month and to_month are required (got % .. %)', from_month, to_month;
    END IF;
    m := date_trunc('month', from_month::timestamp)::date;
    last_m := date_trunc('month', to_month::timestamp)::date;
    -- Only stops absurd input (a year-1 date would create 24,000 tables).
    IF last_m - m > 36525 THEN
        RAISE EXCEPTION 'ensure_parameter_values_partitions: % .. % spans more than 100 years', m, last_m;
    END IF;
    -- Serialises concurrent callers (backend instances, the history generator).
    PERFORM pg_advisory_xact_lock(7272720006);
    WHILE m <= last_m LOOP
        part := format('parameter_values_y%sm%s', to_char(m, 'YYYY'), to_char(m, 'MM'));
        IF NOT EXISTS (
            SELECT 1
            FROM pg_catalog.pg_inherits i
            JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
            WHERE i.inhparent = 'ts.parameter_values'::regclass
              AND c.relnamespace = 'ts'::regnamespace
              AND c.relname = part
        ) THEN
            EXECUTE format('CREATE TABLE ts.%I (LIKE ts.parameter_values)', part);
            EXECUTE format('ALTER TABLE ts.parameter_values ATTACH PARTITION ts.%I FOR VALUES FROM (%L) TO (%L)',
                           part,
                           to_char(m, 'YYYY-MM-DD') || ' 00:00:00+00',
                           to_char(m + interval '1 month', 'YYYY-MM-DD') || ' 00:00:00+00');
            created := created + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN created;
END
$$;

-- Drops the partitions whose upper bound is at or before cutoff (whole months
-- only: data of the month that contains the cutoff is kept until the month has
-- passed completely). Returns the number of partitions dropped. Refuses a cutoff
-- in the future so a misconfiguration can never drop the current month.
CREATE FUNCTION ts.drop_parameter_values_partitions_before(cutoff timestamptz)
RETURNS int
LANGUAGE plpgsql
-- pg_get_expr renders the bounds with the session TimeZone and DateStyle; fix
-- both so the bound text always parses back to the same instant.
SET timezone = 'UTC'
SET datestyle = 'ISO, YMD'
SET lock_timeout = '5s'
AS $$
DECLARE
    r       record;
    dropped int := 0;
BEGIN
    IF cutoff IS NULL THEN
        RAISE EXCEPTION 'drop_parameter_values_partitions_before: cutoff is required';
    END IF;
    IF cutoff > now() THEN
        RAISE EXCEPTION 'drop_parameter_values_partitions_before: cutoff % is in the future', cutoff;
    END IF;
    PERFORM pg_advisory_xact_lock(7272720006);
    FOR r IN
        SELECT p.part, p.upper_bound
        FROM (
            SELECT c.oid::regclass AS part,
                   (regexp_match(pg_catalog.pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz AS upper_bound
            FROM pg_catalog.pg_inherits i
            JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
            WHERE i.inhparent = 'ts.parameter_values'::regclass
        ) p
        WHERE p.upper_bound <= cutoff
        ORDER BY p.upper_bound
    LOOP
        EXECUTE format('DROP TABLE %s', r.part);
        dropped := dropped + 1;
    END LOOP;
    RETURN dropped;
END
$$;

REVOKE ALL ON FUNCTION ts.ensure_parameter_values_partitions(date, date) FROM PUBLIC;
REVOKE ALL ON FUNCTION ts.drop_parameter_values_partitions_before(timestamptz) FROM PUBLIC;
COMMENT ON FUNCTION ts.ensure_parameter_values_partitions(date, date) IS
    'Creates the missing monthly (UTC) partitions of ts.parameter_values for from_month..to_month inclusive; returns the number created.';
COMMENT ON FUNCTION ts.drop_parameter_values_partitions_before(timestamptz) IS
    'Drops the ts.parameter_values partitions whose upper bound is <= cutoff (retention); returns the number dropped.';

-- No writer may slip in between the copy and the drop.
LOCK TABLE ts.parameter_values IN ACCESS EXCLUSIVE MODE;
ALTER TABLE ts.parameter_values RENAME TO parameter_values_unpartitioned;

CREATE TABLE ts.parameter_values (
    equipment_parameter_id BIGINT NOT NULL,
    ts                     TIMESTAMPTZ NOT NULL,
    value                  DOUBLE PRECISION,
    value_text             TEXT
) PARTITION BY RANGE (ts);

-- Months from the oldest row (2026-01 when the table is empty) to the newest row
-- or now + 3 months, whichever is later.
SELECT ts.ensure_parameter_values_partitions(
           COALESCE((b.lo AT TIME ZONE 'UTC')::date, DATE '2026-01-01'),
           (GREATEST(b.hi, now() + interval '3 months') AT TIME ZONE 'UTC')::date)
FROM (SELECT min(ts) AS lo, max(ts) AS hi FROM ts.parameter_values_unpartitioned) b;

INSERT INTO ts.parameter_values (equipment_parameter_id, ts, value, value_text)
SELECT equipment_parameter_id, ts, value, value_text FROM ts.parameter_values_unpartitioned;

DROP TABLE ts.parameter_values_unpartitioned;

ALTER TABLE ts.parameter_values ADD CONSTRAINT parameter_values_pkey PRIMARY KEY (equipment_parameter_id, ts);
CREATE INDEX parameter_values_ts_brin ON ts.parameter_values USING BRIN (ts);

GRANT SELECT ON ts.parameter_values TO afdd_ai, afdd_ai_ro;

-- Fresh statistics for the planner (also analyses every partition).
ANALYZE ts.parameter_values;
