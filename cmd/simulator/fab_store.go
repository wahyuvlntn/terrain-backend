package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Storage of the production model (schema prod, migration 0008): the
// history copies every row at the end, the live simulation upserts what
// changed since its last flush.

// upsertProducts stores the products and their routes.
func upsertProducts(ctx context.Context, db execer) error {
	for _, p := range fabProducts() {
		ideal := 0.0
		for i := range p.Route {
			ideal += idealH(&p.Route[i]) + 0.1
		}
		if _, err := db.Exec(ctx, `INSERT INTO prod.products (code, name, technology, layers, mix, masks, metals, cycle_days, die_mm2, ideal_h)
			VALUES ($1, $2, $3, $4, $5, $4, $6, $7, $8, $9)
			ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name, technology = EXCLUDED.technology, layers = EXCLUDED.layers, mix = EXCLUDED.mix,
			masks = EXCLUDED.masks, metals = EXCLUDED.metals, cycle_days = EXCLUDED.cycle_days, die_mm2 = EXCLUDED.die_mm2, ideal_h = EXCLUDED.ideal_h`,
			p.Code, p.Name, p.Tech, p.Masks, p.Mix, p.Metals, p.CycleDays, p.DieMM2, math.Round(ideal*100)/100); err != nil {
			return fmt.Errorf("product %s: %w", p.Code, err)
		}
		if _, err := db.Exec(ctx, `DELETE FROM prod.route_steps WHERE product_code = $1 AND seq > $2`, p.Code, len(p.Route)); err != nil {
			return err
		}
		for _, s := range p.Route {
			var specName, unit, target, lsl, usl any
			if q := p.spec(s.Meas); q != nil {
				specName, unit, target, lsl, usl = q.Name, q.Unit, q.Target, limOrNil(q.LSL), limOrNil(q.USL)
			} else if s.Param == "defects" {
				specName, unit, target, usl = "Defect adders", "adders", 0.0, float64(defectUSL)
			}
			makes := s.Makes
			if makes == nil {
				makes = []string{}
			}
			if _, err := db.Exec(ctx, `INSERT INTO prod.route_steps (product_code, seq, step_code, layer, operation, tool_type, recipe,
				ideal_wafer_s, ideal_lot_s, sample_wafers, sample_rate, param, module, area, mask, makes, meas_key, spec_name, unit, target, lsl, usl,
				on_fail, qtime_h) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
				ON CONFLICT (product_code, seq) DO UPDATE SET step_code = EXCLUDED.step_code, layer = EXCLUDED.layer, operation = EXCLUDED.operation,
				tool_type = EXCLUDED.tool_type, recipe = EXCLUDED.recipe, ideal_wafer_s = EXCLUDED.ideal_wafer_s, ideal_lot_s = EXCLUDED.ideal_lot_s,
				sample_wafers = EXCLUDED.sample_wafers, sample_rate = EXCLUDED.sample_rate, param = EXCLUDED.param, module = EXCLUDED.module,
				area = EXCLUDED.area, mask = EXCLUDED.mask, makes = EXCLUDED.makes, meas_key = EXCLUDED.meas_key, spec_name = EXCLUDED.spec_name,
				unit = EXCLUDED.unit, target = EXCLUDED.target, lsl = EXCLUDED.lsl, usl = EXCLUDED.usl, on_fail = EXCLUDED.on_fail, qtime_h = EXCLUDED.qtime_h`,
				p.Code, s.Seq, s.Code, s.Layer, s.Op, s.Type, s.Recipe, s.WaferS, s.LotS, s.Sample, s.Rate, strOrNil(s.Param), s.Module, stepArea(s.Type),
				strOrNil(s.Mask), makes, strOrNil(s.Meas), specName, unit, target, lsl, usl, strOrNil(s.OnFail), numOrNil(s.QTime)); err != nil {
				return fmt.Errorf("route step %s %s: %w", p.Code, s.Code, err)
			}
		}
	}
	// retire products of an earlier plant configuration that no lot refers to any more
	codes := []string{}
	for _, p := range fabProducts() {
		codes = append(codes, p.Code)
	}
	if _, err := db.Exec(ctx, `DELETE FROM prod.route_steps r WHERE r.product_code <> ALL($1)
		AND NOT EXISTS (SELECT 1 FROM prod.lots l WHERE l.product_code = r.product_code)`, codes); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM prod.products p WHERE p.code <> ALL($1)
		AND NOT EXISTS (SELECT 1 FROM prod.lots l WHERE l.product_code = p.code)`, codes); err != nil {
		return err
	}
	return nil
}

func tsOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// limOrNil is a specification limit (NaN: none).
func limOrNil(f float64) any {
	if math.IsNaN(f) {
		return nil
	}
	return f
}

func numOrNil(f float64) any {
	if f == 0 || math.IsNaN(f) {
		return nil
	}
	return math.Round(f*1000) / 1000
}

var lotCols = []string{"lot_code", "product_code", "priority", "wafers_start", "wafers", "status", "current_seq", "step_code", "equipment_id",
	"hold_reason", "rework_count", "released_at", "due_at", "completed_at", "location"}

var stepCols = []string{"id", "lot_code", "seq", "step_code", "operation", "equipment_id", "chambers", "recipe", "status", "queued_at", "started_at",
	"ended_at", "wafers_in", "wafers_out", "scrapped", "reworked", "theoretical_s", "actual_s", "outcome", "batch_id", "is_rework"}

var metCols = []string{"lot_code", "lot_step_id", "measured_at", "param", "wafer", "value", "wiw_range", "target", "lsl", "usl",
	"metrology_equipment_id", "source_equipment_id", "source_chamber", "source_step_code", "pass", "char_key", "unit"}

var scrapCols = []string{"lot_code", "at", "wafers", "reason", "step_code", "equipment_id", "chamber"}

var transferCols = []string{"id", "lot_code", "product_code", "kind", "from_loc", "to_loc", "vehicle", "requested_at", "picked_at",
	"delivered_at", "distance_m", "busy_s"}

func transferRows(out fabOut) [][]any {
	rows := make([][]any, 0, len(out.transfers))
	for _, t := range out.transfers {
		rows = append(rows, []any{t.ID, t.Lot, t.Product, t.Kind, t.From, t.To, t.Vehicle, t.Requested, t.Picked, t.Delivered,
			math.Round(t.Dist*100) / 100, math.Round(t.Busy*10) / 10})
	}
	return rows
}

// fabRows builds the rows of collected production records; eq maps an
// equipment alias to its id (nil when unknown).
func (f *Fab) fabRows(out fabOut, eq func(string) any) (lots, steps, met, scrap [][]any) {
	for _, l := range out.lots {
		var tool, hold any
		if l.Status == "PROCESSING" {
			tool = eq(l.Tool)
		}
		if l.Status == "HOLD" {
			hold = l.Hold
		}
		step := ""
		if l.Status != "DONE" && l.Status != "SCRAPPED" {
			if rs := f.stepOf(l); rs != nil {
				step = rs.Code
			}
		} else if pr := f.prod[l.Product]; pr != nil && l.Seq >= 1 && l.Seq <= len(pr.Route) {
			step = pr.Route[l.Seq-1].Code
		}
		lots = append(lots, []any{l.Code, l.Product, l.Priority, l.Start, len(l.Slots), l.Status, l.Seq, strOrNil(step), tool, hold, l.Rework,
			l.Released, tsOrNil(l.Due), tsOrNil(l.Done), strOrNil(l.Loc)})
	}
	for _, r := range out.steps {
		var outW any
		if !r.Ended.IsZero() {
			outW = r.Out
		}
		steps = append(steps, []any{r.ID, r.Lot, r.Seq, r.Code, r.Op, eq(r.Tool), strOrNil(r.Chambers), r.Recipe, r.Status, r.Queued,
			tsOrNil(r.Started), tsOrNil(r.Ended), r.In, outW, r.Scrapped, r.Reworked, numOrNil(r.Theo), numOrNil(r.Actual), strOrNil(r.Outcome),
			strOrNil(r.Batch), r.Rework})
	}
	for _, m := range out.met {
		var rng any
		if m.HasRange {
			rng = math.Round(m.Range*10000) / 10000
		}
		met = append(met, []any{m.Lot, m.StepID, m.At, m.Param, m.Wafer, math.Round(m.Value*10000) / 10000, rng, m.Target, limOrNil(m.LSL),
			limOrNil(m.USL), eq(m.MetTool), eq(m.SrcTool), strOrNil(m.SrcCh), strOrNil(m.SrcStep), m.Pass, strOrNil(m.Key), strOrNil(m.Unit)})
	}
	for _, s := range out.scrap {
		scrap = append(scrap, []any{s.Lot, s.At, s.Wafers, s.Reason, strOrNil(s.Step), eq(s.Tool), strOrNil(s.Chamber)})
	}
	return
}

// copyFab stores the production records of the history (in a transaction)
// and moves the lot step id sequence past the explicit ids.
func (f *Fab) copyFab(ctx context.Context, tx pgx.Tx, out fabOut, eq func(string) any) error {
	if err := upsertProducts(ctx, tx); err != nil {
		return err
	}
	lots, steps, met, scrap := f.fabRows(out, eq)
	for _, c := range []struct {
		table string
		cols  []string
		rows  [][]any
	}{{"lots", lotCols, lots}, {"lot_steps", stepCols, steps}, {"metrology", metCols, met}, {"scrap", scrapCols, scrap},
		{"transfers", transferCols, transferRows(out)}} {
		if len(c.rows) == 0 {
			continue
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"prod", c.table}, c.cols, pgx.CopyFromRows(c.rows)); err != nil {
			return fmt.Errorf("copy prod.%s: %w", c.table, err)
		}
	}
	_, err := tx.Exec(ctx, `SELECT setval('prod.lot_steps_id_seq', GREATEST((SELECT COALESCE(max(id), 0) FROM prod.lot_steps), 1))`)
	return err
}

// upsertFab stores the production records changed since the last flush.
func (f *Fab) upsertFab(ctx context.Context, db *pgxpool.Pool, out fabOut, eq func(string) any) error {
	lots, steps, met, scrap := f.fabRows(out, eq)
	b := &pgx.Batch{}
	for _, r := range lots {
		b.Queue(`INSERT INTO prod.lots (lot_code, product_code, priority, wafers_start, wafers, status, current_seq, step_code, equipment_id,
			hold_reason, rework_count, released_at, due_at, completed_at, location) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (lot_code) DO UPDATE SET priority = EXCLUDED.priority, wafers = EXCLUDED.wafers, status = EXCLUDED.status,
			current_seq = EXCLUDED.current_seq, step_code = EXCLUDED.step_code, equipment_id = EXCLUDED.equipment_id,
			hold_reason = EXCLUDED.hold_reason, rework_count = EXCLUDED.rework_count, due_at = EXCLUDED.due_at, completed_at = EXCLUDED.completed_at,
			location = EXCLUDED.location`, r...)
	}
	for _, r := range steps {
		b.Queue(`INSERT INTO prod.lot_steps (id, lot_code, seq, step_code, operation, equipment_id, chambers, recipe, status, queued_at, started_at,
			ended_at, wafers_in, wafers_out, scrapped, reworked, theoretical_s, actual_s, outcome, batch_id, is_rework)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
			ON CONFLICT (id) DO UPDATE SET equipment_id = EXCLUDED.equipment_id, chambers = EXCLUDED.chambers, status = EXCLUDED.status,
			started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at, wafers_in = EXCLUDED.wafers_in, wafers_out = EXCLUDED.wafers_out,
			scrapped = EXCLUDED.scrapped, reworked = EXCLUDED.reworked, theoretical_s = EXCLUDED.theoretical_s, actual_s = EXCLUDED.actual_s,
			outcome = EXCLUDED.outcome, batch_id = EXCLUDED.batch_id, is_rework = EXCLUDED.is_rework`, r...)
	}
	for _, r := range met {
		b.Queue(`INSERT INTO prod.metrology (lot_code, lot_step_id, measured_at, param, wafer, value, wiw_range, target, lsl, usl,
			metrology_equipment_id, source_equipment_id, source_chamber, source_step_code, pass, char_key, unit)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`, r...)
	}
	for _, r := range scrap {
		b.Queue(`INSERT INTO prod.scrap (lot_code, at, wafers, reason, step_code, equipment_id, chamber) VALUES ($1, $2, $3, $4, $5, $6, $7)`, r...)
	}
	for _, r := range transferRows(out) {
		b.Queue(`INSERT INTO prod.transfers (id, lot_code, product_code, kind, from_loc, to_loc, vehicle, requested_at, picked_at, delivered_at, distance_m,
			busy_s) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO NOTHING`, r...)
	}
	if b.Len() == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
}

// prodReady reports whether the prod schema has the full-flow columns and the
// transport records (migrations 0008 - 0010).
func prodReady(ctx context.Context, db *pgxpool.Pool) bool {
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'prod' AND table_name = 'lots' AND column_name = 'location')`).Scan(&ok)
	return err == nil && ok
}

// clearProduction removes the production records (history regeneration).
func clearProduction(ctx context.Context, db *pgxpool.Pool) error {
	if !prodReady(ctx, db) {
		return errors.New("schema prod missing or old (migrations 0008 - 0010 not applied)")
	}
	_, err := db.Exec(ctx, `TRUNCATE prod.transfers, prod.scrap, prod.metrology, prod.lot_steps, prod.lots RESTART IDENTITY`)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42501" { // no TRUNCATE privilege (the cloud's runtime role): delete instead
		for _, t := range []string{"prod.transfers", "prod.scrap", "prod.metrology", "prod.lot_steps", "prod.lots"} {
			if _, err := db.Exec(ctx, `DELETE FROM `+t); err != nil {
				return err
			}
		}
		return nil
	}
	return err
}

// equipmentIDs maps equipment aliases to ids.
func equipmentIDs(ctx context.Context, db *pgxpool.Pool) (map[string]int64, error) {
	rows, err := db.Query(ctx, `SELECT alias, id FROM asset.equipment`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a string
		var id int64
		if err := rows.Scan(&a, &id); err != nil {
			return nil, err
		}
		out[a] = id
	}
	return out, rows.Err()
}

func idLookup(ids map[string]int64) func(string) any {
	return func(alias string) any {
		if id, ok := ids[alias]; ok && alias != "" {
			return id
		}
		return nil
	}
}
