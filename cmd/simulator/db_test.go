package main

// Database tests of the simulation job runner and of the history storage.
// They need AFDD_TEST_DATABASE_URL, a role that may create databases on a
// PostgreSQL server with pgvector (e.g. postgres://afdd:afdd@postgres:5432/postgres);
// each test creates its own scratch database (the tables it needs taken from
// the migrations, or all migrations) and drops it afterwards. Without the
// variable they skip.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"afdd/internal/db"
)

var scratchSeq atomic.Int64

// scratchURL creates a scratch database and returns its URL; the database
// is dropped when the test ends (after the pools opened on it are closed).
func scratchURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AFDD_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AFDD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("afdd_scratch_sim_%d_%d", os.Getpid(), scratchSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		admin.Close(context.Background())
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// scratchPool returns a pool on a scratch database prepared with the setup
// SQL.
func scratchPool(t *testing.T, setup ...string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, scratchURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, sql := range setup {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return pool
}

// migratedPool returns a pool on a scratch database with every migration of
// the backend applied.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, scratchURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d.Pool
}

// migrationPart returns the text of a migration from the first occurrence of
// from up to (not including) the first occurrence of to after it.
func migrationPart(t *testing.T, file, from, to string) string {
	t.Helper()
	b, err := os.ReadFile("../../internal/db/migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("%s: %q not found", file, from)
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		t.Fatalf("%s: %q not found after %q", file, to, from)
	}
	return s[i : i+j]
}

// simSchema creates the sim schema of migration 0005 (with the core.users it
// references).
func simSchema(t *testing.T) []string {
	return []string{
		`CREATE SCHEMA core; CREATE TABLE core.users (id BIGSERIAL PRIMARY KEY); CREATE SCHEMA sim;`,
		migrationPart(t, "0005_agentic.sql", "CREATE TABLE sim.jobs", "INSERT INTO core.permission_groups"),
	}
}

func insertJob(t *testing.T, pool *pgxpool.Pool, kind, origin, title, spec string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sim.jobs (kind, title, origin, status, spec) VALUES ($1, $2, $3, 'QUEUED', $4::jsonb) RETURNING id`,
		kind, title, origin, spec).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type jobState struct {
	Status   string
	Progress float32
	Error    *string
	Summary  map[string]any
	SimStart *time.Time
	SimEnd   *time.Time
}

func loadJob(t *testing.T, pool *pgxpool.Pool, id int64) jobState {
	t.Helper()
	var j jobState
	if err := pool.QueryRow(context.Background(), `SELECT status, progress, error, summary, sim_start, sim_end FROM sim.jobs WHERE id = $1`, id).
		Scan(&j.Status, &j.Progress, &j.Error, &j.Summary, &j.SimStart, &j.SimEnd); err != nil {
		t.Fatal(err)
	}
	return j
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestJobRunnerDB: the runner claims queued jobs, runs scenarios (nominal and
// from the live snapshot) and a DOE, stores sim.series / states / events /
// doe_samples, hands the jobs to the post-analysis (ANALYZING), fails a bad
// spec, drops the results of a job cancelled while it ran, fails jobs left
// RUNNING by a restart and wakes up on NOTIFY sim_jobs.
func TestJobRunnerDB(t *testing.T) {
	pool := scratchPool(t, simSchema(t)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := designPlant(17)
	simulate(live, testStart, 30, 60, nil)
	var snapshots atomic.Int32
	snapshot := func(_ context.Context, seed uint64) (*Plant, error) {
		snapshots.Add(1)
		return live.Clone(seed), nil
	}
	r := newJobRunner(ctx, pool, testLoc, snapshot, defaultJobOptions())

	scenario := insertJob(t, pool, "scenario", "ui", "Approach trial", `{"start": null, "duration_min": 60, "initial": "nominal", "seed": 3,
		"actions": [{"at_min": 5, "kind": "fault", "asset": "CH_01", "fault": "chwst_bias", "value": 1, "label": "dropped by the API"}],
		"variants": [{"name": "baseline", "actions": []}, {"name": "recommended", "actions": [
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 4.5}]}]}`)
	fromLive := insertJob(t, pool, "scenario", "optimizer", "Validation", `{"start": null, "duration_min": 30, "initial": "live", "seed": 0,
		"variants": [{"name": "baseline", "actions": []}], "purpose": "validation"}`)
	doe := insertJob(t, pool, "doe", "analytics", "Surrogate DOE", `{"samples": 6, "settle_min": 12, "avg_min": 5, "seed": 0, "purpose": "surrogate"}`)
	bad := insertJob(t, pool, "scenario", "ui", "Bad", `{"duration_min": 60, "initial": "nominal", "actions": [{"at_min": 1, "kind": "trip", "asset": "CH_09"}]}`)
	if err := r.drain(); err != nil {
		t.Fatal(err)
	}

	j := loadJob(t, pool, scenario)
	if j.Status != "ANALYZING" || j.Progress != 1 || j.SimStart == nil || j.SimEnd == nil || j.SimEnd.Sub(*j.SimStart) != time.Hour {
		t.Fatalf("scenario job: %+v", j)
	}
	variants, _ := j.Summary["variants"].(map[string]any)
	if len(variants) != 2 || variants["baseline"] == nil || variants["recommended"] == nil {
		t.Errorf("summary variants %v", j.Summary["variants"])
	}
	params := 0
	for _, a := range live.Assets {
		params += len(a.Model.Params)
	}
	if n := count(t, pool, `SELECT count(*) FROM sim.series WHERE job_id = $1`, scenario); n != 2*params {
		t.Errorf("%d series rows, want %d", n, 2*params)
	}
	if n := count(t, pool, `SELECT count(*) FROM sim.series WHERE job_id = $1 AND cardinality(measured) <> 60`, scenario); n != 0 {
		t.Errorf("%d series without 60 values", n)
	}
	// truth where the sensor lies: CH_01 chwst measured 1 K above
	var meas, truth []float32
	if err := pool.QueryRow(ctx, `SELECT measured, truth FROM sim.series WHERE job_id = $1 AND variant = 'baseline' AND asset_alias = 'CH_01'
		AND param_alias = 'chwst'`, scenario).Scan(&meas, &truth); err != nil {
		t.Fatal(err)
	}
	if len(truth) != 60 || meas[50]-truth[50] < 0.98 || meas[50]-truth[50] > 1.02 {
		t.Errorf("CH_01 chwst measured %v true %v", meas[50], truth[50])
	}
	if n := count(t, pool, `SELECT count(*) FROM sim.series WHERE job_id = $1 AND asset_alias = 'CH_02' AND truth IS NOT NULL`, scenario); n != 0 {
		t.Errorf("CH_02 has %d truth series", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM sim.states WHERE job_id = $1 AND kind = 'status' AND end_ts IS NOT NULL`, scenario); n < 2*len(live.Assets) {
		t.Errorf("%d status intervals", n)
	}
	var ctlDetails map[string]any
	if err := pool.QueryRow(ctx, `SELECT details FROM sim.events WHERE job_id = $1 AND variant = 'recommended' AND kind = 'control'`, scenario).
		Scan(&ctlDetails); err != nil {
		t.Fatal(err)
	}
	if ctlDetails["status"] != "APPLIED" || ctlDetails["param"] != "approach_sp" {
		t.Errorf("control event details %v", ctlDetails)
	}
	if n := count(t, pool, `SELECT count(*) FROM sim.events WHERE job_id = $1 AND kind = 'fault_injected'`, scenario); n != 2 {
		t.Errorf("%d fault_injected events, want one per variant", n)
	}

	if j := loadJob(t, pool, fromLive); j.Status != "ANALYZING" || snapshots.Load() != 1 {
		t.Errorf("live job %+v, %d snapshots", j, snapshots.Load())
	}

	j = loadJob(t, pool, doe)
	if j.Status != "ANALYZING" || j.Summary["samples"] != float64(6) {
		t.Errorf("doe job %+v", j)
	}
	rows, err := pool.Query(ctx, `SELECT inputs, outputs FROM sim.doe_samples WHERE job_id = $1 ORDER BY idx`, doe)
	if err != nil {
		t.Fatal(err)
	}
	samples := 0
	for rows.Next() {
		var in, out map[string]float64
		if err := rows.Scan(&in, &out); err != nil {
			t.Fatal(err)
		}
		samples++
		if len(in) != len(doeInputs) || len(out) != len(doeOutputs) {
			t.Errorf("sample keys %d / %d", len(in), len(out))
		}
	}
	if samples != 6 {
		t.Errorf("%d doe samples", samples)
	}

	if j := loadJob(t, pool, bad); j.Status != "FAILED" || j.Error == nil || !strings.Contains(*j.Error, "CH_09") {
		t.Errorf("bad job %+v", j)
	}

	// a job cancelled while it runs keeps no results
	gone := insertJob(t, pool, "scenario", "ui", "Cancelled", `{"duration_min": 30, "initial": "nominal"}`)
	job, err := r.claim()
	if err != nil || job == nil || job.ID != gone {
		t.Fatalf("claim: %+v %v", job, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sim.jobs SET status = 'CANCELLED' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	r.execute(job)
	if j := loadJob(t, pool, gone); j.Status != "CANCELLED" || count(t, pool, `SELECT count(*) FROM sim.series WHERE job_id = $1`, gone) != 0 {
		t.Errorf("cancelled job %+v has results", j)
	}

	// a job left RUNNING by a restart fails; a notified job runs at once
	stale := insertJob(t, pool, "scenario", "ui", "Interrupted", `{"duration_min": 30, "initial": "nominal"}`)
	if _, err := pool.Exec(ctx, `UPDATE sim.jobs SET status = 'RUNNING' WHERE id = $1`, stale); err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		newJobRunner(rctx, pool, testLoc, snapshot, defaultJobOptions()).Run()
		close(done)
	}()
	defer func() { stop(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for loadJob(t, pool, stale).Status != "FAILED" {
		if time.Now().After(deadline) {
			t.Fatal("interrupted job not failed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the runner is listening now
	notified := insertJob(t, pool, "scenario", "ui", "Notified", `{"duration_min": 10, "initial": "nominal"}`)
	submitted := time.Now()
	if _, err := pool.Exec(ctx, `SELECT pg_notify('sim_jobs', $1::bigint::text)`, notified); err != nil {
		t.Fatal(err)
	}
	for loadJob(t, pool, notified).Status != "ANALYZING" {
		if time.Since(submitted) > 4*time.Second {
			t.Fatal("notified job not run within 4 s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("notified 10 min job done %v after the NOTIFY", time.Since(submitted).Round(time.Millisecond))
	b, _ := json.Marshal(variants["baseline"])
	t.Logf("baseline KPIs: %s", b)
}

// TestEnsurePartitionsDB: the history creates the monthly partitions of its
// window (UTC months, also when the plant clock is SGT near a month boundary)
// and tolerates a database without partitioning (no function yet).
func TestEnsurePartitionsDB(t *testing.T) {
	pool := scratchPool(t, `CREATE SCHEMA ts; CREATE TABLE ts.parameter_values (equipment_parameter_id BIGINT NOT NULL, ts TIMESTAMPTZ NOT NULL,
		value DOUBLE PRECISION, value_text TEXT) PARTITION BY RANGE (ts);`)
	ctx := context.Background()
	// 1 Sep 00:30 SGT is still August in UTC
	from := time.Date(2026, 9, 1, 0, 30, 0, 0, testLoc)
	to := time.Date(2026, 10, 30, 9, 0, 0, 0, testLoc)
	if err := ensurePartitions(ctx, pool, from, to); err != nil {
		t.Fatalf("without the function: %v", err)
	}
	fn := migrationPart(t, "0006_ts_partitioning.sql", "CREATE FUNCTION ts.ensure_parameter_values_partitions", "-- Drops the partitions")
	if _, err := pool.Exec(ctx, fn); err != nil {
		t.Fatal(err)
	}
	if err := ensurePartitions(ctx, pool, from, to); err != nil {
		t.Fatal(err)
	}
	var parts []string
	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'ts.parameter_values'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	parts, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"parameter_values_y2026m08", "parameter_values_y2026m09", "parameter_values_y2026m10"}
	if strings.Join(parts, ",") != strings.Join(want, ",") {
		t.Errorf("partitions %v, want %v", parts, want)
	}
	// the history's COPY lands in them (first and last minute of the window)
	n, err := pool.CopyFrom(ctx, pgx.Identifier{"ts", "parameter_values"}, []string{"equipment_parameter_id", "ts", "value", "value_text"},
		pgx.CopyFromRows([][]any{{int64(1), from.UTC(), 1.5, nil}, {int64(1), to.UTC(), 2.5, nil}}))
	if err != nil || n != 2 {
		t.Fatalf("copy: %d, %v", n, err)
	}
	// idempotent
	if err := ensurePartitions(ctx, pool, from, to); err != nil {
		t.Fatal(err)
	}
}

// seedPlantRows inserts the plant definition the history needs (the rows the
// seeder creates through the API): types, models with their parameters,
// alarms and issue relations, alarm codes, equipment with their parameters
// and the demo users.
func seedPlantRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	id := func(sql string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	brand := id(`INSERT INTO asset.brands (name) VALUES ('Brand') RETURNING id`)
	series := id(`INSERT INTO asset.series (name) VALUES ('Series') RETURNING id`)
	loc := id(`INSERT INTO asset.locations (name) VALUES ('Plant') RETURNING id`)
	for _, c := range alarmCodes {
		exec(`INSERT INTO dm.alarm_codes (code, name) VALUES ($1, $2)`, c[0], c[1])
	}
	modelIDs := map[string]int64{}
	for _, m := range models() {
		typ := id(`INSERT INTO asset.types (name) VALUES ($1) RETURNING id`, m.Type+" / "+m.Name)
		mid := id(`INSERT INTO asset.models (name, type_id, brand_id, series_id) VALUES ($1, $2, $3, $4) RETURNING id`, m.Name, typ, brand, series)
		modelIDs[m.Name] = mid
		for _, p := range m.Params {
			dt := "number"
			if p.Bool {
				dt = "boolean"
			}
			exec(`INSERT INTO asset.model_parameters (model_id, alias, name, unit, data_type) VALUES ($1, $2, $3, $4, $5)`, mid, p.Alias, p.Name, p.Unit, dt)
		}
		for _, a := range effectiveAlarms(m) {
			exec(`INSERT INTO asset.model_alarms (model_id, name, message) VALUES ($1, $2, $3)`, mid, a.Code, a.Message)
		}
		for _, r := range m.Relations {
			exec(`INSERT INTO dm.issue_relations (model_id, name) VALUES ($1, $2)`, mid, r.Name)
		}
	}
	for _, e := range equipment() {
		eid := id(`INSERT INTO asset.equipment (alias, name, model_id, location_id) VALUES ($1, $2, $3, $4) RETURNING id`, e.Alias, e.Name, modelIDs[e.Model], loc)
		exec(`INSERT INTO asset.equipment_parameters (equipment_id, model_parameter_id) SELECT $1, id FROM asset.model_parameters WHERE model_id = $2`,
			eid, modelIDs[e.Model])
	}
	for _, u := range users {
		exec(`INSERT INTO core.users (username, password_hash, full_name) VALUES ($1, 'x', $2)`, u.Username, u.Name)
	}
}

// TestHistoryStoreDB generates and stores 10 days of history in a migrated
// scratch database (ts.parameter_values partitioned by migration 0006): the
// values, status and condition intervals, alarm events with the FDD alarms
// of the LBNL episodes, the orders with their FDD corrective orders, the
// operator set point changes as asset.control_requests and notifications.
func TestHistoryStoreDB(t *testing.T) {
	pool := migratedPool(t)
	seedPlantRows(t, pool)
	ctx := context.Background()
	p := newPlant(2026, testLoc)
	started := time.Now()
	h, err := runHistory(ctx, pool, p, 10, 2026)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("10 days generated and stored in %v", time.Since(started).Round(time.Millisecond))
	var kind string
	if err := pool.QueryRow(ctx, `SELECT relkind::text FROM pg_class WHERE oid = 'ts.parameter_values'::regclass`).Scan(&kind); err != nil || kind != "p" {
		t.Fatalf("ts.parameter_values relkind %q (%v), want partitioned", kind, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM ts.parameter_values`); n != h.nValues || n == 0 {
		t.Errorf("%d parameter values stored, generated %d", n, h.nValues)
	}
	fdd := count(t, pool, `SELECT count(*) FROM ts.alarm_events WHERE source_key LIKE 'ext:fdd:%' AND alarm_code_id IS NOT NULL AND end_ts IS NOT NULL`)
	ctl := count(t, pool, `SELECT count(*) FROM asset.control_requests WHERE status = 'APPLIED' AND user_id IS NOT NULL AND completed_at > requested_at`)
	orders := count(t, pool, `SELECT count(*) FROM dm.maintenance_orders`)
	fddOrders := count(t, pool, `SELECT count(*) FROM dm.maintenance_orders o JOIN dm.issue_relations r ON r.id = o.issue_relation_id
		WHERE r.name LIKE '% (FDD)' AND o.completed_at IS NOT NULL`)
	notes := count(t, pool, `SELECT count(*) FROM dm.work_updates u JOIN dm.maintenance_orders o ON o.id = u.order_id
		JOIN dm.issue_relations r ON r.id = o.issue_relation_id WHERE r.name LIKE '% (FDD)'`)
	t.Logf("stored: %d FDD alarm events, %d control requests, %d orders (%d completed FDD orders, %d FDD work notes), %d status rows, %d notifications",
		fdd, ctl, orders, fddOrders, notes, count(t, pool, `SELECT count(*) FROM ts.asset_status`), count(t, pool, `SELECT count(*) FROM core.notifications`))
	if fdd == 0 || ctl != len(h.ctlR) || ctl == 0 || orders != len(h.orders) || fddOrders == 0 || notes == 0 {
		t.Errorf("history rows: FDD alarms %d, control requests %d of %d, orders %d of %d, FDD orders %d, notes %d",
			fdd, ctl, len(h.ctlR), orders, len(h.orders), fddOrders, notes)
	}
	// production records: lots, process runs on the tools, metrology with
	// its source tool, the lot step id sequence past the explicit ids
	lots := count(t, pool, `SELECT count(*) FROM prod.lots`)
	runs := count(t, pool, `SELECT count(*) FROM prod.lot_steps WHERE status = 'DONE' AND equipment_id IS NOT NULL AND theoretical_s > 0`)
	met := count(t, pool, `SELECT count(*) FROM prod.metrology WHERE source_equipment_id IS NOT NULL AND metrology_equipment_id IS NOT NULL`)
	wip := count(t, pool, `SELECT count(*) FROM prod.lots WHERE status IN ('QUEUED', 'PROCESSING', 'AWAY', 'HOLD')`)
	seq := count(t, pool, `SELECT (last_value = (SELECT max(id) FROM prod.lot_steps))::int FROM prod.lot_steps_id_seq`)
	t.Logf("production: %d lots (%d in progress), %d runs, %d metrology results, %d route steps", lots, wip, runs, met,
		count(t, pool, `SELECT count(*) FROM prod.route_steps`))
	if !h.fabStore || lots < 150 || lots-wip < 100 || runs < 20000 || met < 5000 || wip == 0 || seq != 1 {
		t.Errorf("production rows: stored %v, lots %d, runs %d, metrology %d, WIP %d, sequence ok %d", h.fabStore, lots, runs, met, wip, seq)
	}
	// a second run replaces the history (clearExisting) instead of adding to it
	if _, err := runHistory(ctx, pool, newPlant(7, testLoc), 3, 7); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM asset.control_requests WHERE requested_at < now() - interval '4 days'`); n != 0 {
		t.Errorf("%d control requests of the first run left", n)
	}
	// (the second run's warm-up starts 45 days back, the first run's 52)
	if n := count(t, pool, `SELECT count(*) FROM prod.lots WHERE released_at < now() - interval '49 days'`); n != 0 {
		t.Errorf("%d lots of the first run left", n)
	}
}
