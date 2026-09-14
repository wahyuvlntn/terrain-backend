-- FAB1 full 40 nm flow (docs/production/DESIGN.md): products with their mask
-- and metal counts, planned cycle time and die size; route steps with layer
-- names, modules, process areas, the quality characteristics they make or
-- measure (with specification limits), metrology dispositions and queue-time
-- limits; measurements with their quality characteristic; production
-- planning simulations (sim.jobs kind 'production').

ALTER TABLE prod.products
    ADD COLUMN IF NOT EXISTS masks      INT,
    ADD COLUMN IF NOT EXISTS metals     INT,
    ADD COLUMN IF NOT EXISTS cycle_days DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS die_mm2    DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS ideal_h    DOUBLE PRECISION; -- raw process time of the route (X-factor denominator)

ALTER TABLE prod.route_steps ALTER COLUMN layer TYPE TEXT USING layer::text;
ALTER TABLE prod.route_steps
    ADD COLUMN IF NOT EXISTS module    TEXT,             -- FEOL | MOL | BEOL | TEST
    ADD COLUMN IF NOT EXISTS area      TEXT,             -- LITHO | ETCH | CVD | PVD | ECP | CMP | DIFF | IMP | WET | STRIP | MET | TEST
    ADD COLUMN IF NOT EXISTS mask      TEXT,             -- mask exposed (litho steps)
    ADD COLUMN IF NOT EXISTS makes     TEXT[],           -- quality characteristics the step makes on every wafer
    ADD COLUMN IF NOT EXISTS meas_key  TEXT,             -- quality characteristic measured (metrology steps)
    ADD COLUMN IF NOT EXISTS spec_name TEXT,
    ADD COLUMN IF NOT EXISTS unit      TEXT,
    ADD COLUMN IF NOT EXISTS target    DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS lsl       DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS usl       DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS on_fail   TEXT,             -- REWORK_LITHO | REWORK_REDEP | REWORK_CMP | RECLEAN | SCRAP
    ADD COLUMN IF NOT EXISTS qtime_h   DOUBLE PRECISION; -- queue-time limit after the previous step
COMMENT ON COLUMN prod.route_steps.tool_type IS 'step type: the tool qualification (LITHO_ARF, ETCH_D, FURN_OX, ...)';

ALTER TABLE prod.metrology
    ADD COLUMN IF NOT EXISTS char_key TEXT,
    ADD COLUMN IF NOT EXISTS unit     TEXT;
CREATE INDEX IF NOT EXISTS metrology_char_idx ON prod.metrology (char_key, measured_at);
CREATE INDEX IF NOT EXISTS metrology_step_idx ON prod.metrology (lot_step_id);
CREATE INDEX IF NOT EXISTS lot_steps_code_idx ON prod.lot_steps (step_code, ended_at);

DO $$
DECLARE c TEXT;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'sim.jobs'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%kind%'
    LOOP
        EXECUTE format('ALTER TABLE sim.jobs DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE sim.jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('scenario', 'doe', 'production'));
