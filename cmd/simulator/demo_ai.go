package main

// Demo records of the AI services (AI Insights, Simulation): the analytics
// faults of the FDD episodes in the plant history, simulation jobs of both
// workspaces in every state and origin, and optimizer recommendations through
// their lifecycle. Jobs that succeeded really ran on the twin (their windows
// lie in the past); recommendations are validated by the analytics service
// like live ones and then decided. Seeded once per demoAIVersion and again
// after every history regeneration, in the background once the analytics
// service has created its FDD rules; `afdd-sim demo-ai` re-seeds relative to
// now. Only the records of the previous seed (ids in the marker, faults with
// evidence.source = history) are replaced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const demoAIKey = "demo_ai"

// demoAIVersion is bumped when the demo AI records change (2: jobs and
// validations run on the plant without its current faults).
const demoAIVersion = 2

type demoAIMarker struct {
	Version   int       `json:"version"`
	HistoryAt time.Time `json:"history_at"`
	SeededAt  time.Time `json:"seeded_at"`
	Jobs      []int64   `json:"jobs"`
	Recs      []int64   `json:"recommendations"`
	Runs      []int64   `json:"optimizer_runs"`
}

type demoAI struct {
	ctx    context.Context
	db     *pgxpool.Pool
	loc    *time.Location
	now    time.Time
	runner *JobRunner
	m      demoAIMarker
}

// startDemoAI seeds the demo AI records in the background.
func startDemoAI(ctx context.Context, db *pgxpool.Pool, loc *time.Location) {
	go func() {
		if err := seedDemoAI(ctx, db, loc, false); err != nil && ctx.Err() == nil {
			slog.Warn("demo AI records", "err", err)
		}
	}()
}

func seedDemoAI(ctx context.Context, db *pgxpool.Pool, loc *time.Location, force bool) error {
	for waited := false; ; waited = true {
		if exists(ctx, db, `SELECT 1 WHERE to_regclass('sim.jobs') IS NOT NULL AND to_regclass('analytics.recommendations') IS NOT NULL`) &&
			exists(ctx, db, `SELECT 1 FROM analytics.fault_rules`) && exists(ctx, db, `SELECT 1 FROM core.settings WHERE key = $1`, stateKey) {
			break
		}
		if !waited {
			slog.Info("demo AI records: waiting for the analytics service (FDD rules) and the plant state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
	var sm seedMarker
	getSetting(ctx, db, seedKey, &sm)
	d := &demoAI{ctx: ctx, db: db, loc: loc, now: time.Now().UTC().Truncate(time.Minute)}
	getSetting(ctx, db, demoAIKey, &d.m)
	if !force && d.m.Version >= demoAIVersion && d.m.HistoryAt.Equal(sm.History) {
		return nil
	}
	slog.Info("seeding demo AI records")
	d.runner = newJobRunner(ctx, db, loc, d.snapshot, defaultJobOptions())
	if err := d.clear(); err != nil {
		return err
	}
	d.m = demoAIMarker{HistoryAt: sm.History}
	faults, err := d.faults()
	if err != nil {
		return fmt.Errorf("faults: %w", err)
	}
	if err := d.jobs(); err != nil {
		return fmt.Errorf("simulation jobs: %w", err)
	}
	recs, err := d.recommendations()
	if err != nil {
		return fmt.Errorf("recommendations: %w", err)
	}
	d.m.Version, d.m.SeededAt = demoAIVersion, time.Now().UTC()
	if err := putSetting(ctx, db, demoAIKey, d.m); err != nil {
		return err
	}
	slog.Info("demo AI records ready", "faults", faults, "jobs", len(d.m.Jobs), "recommendations", recs)
	return nil
}

// snapshot is the plant as the simulator last saved it (the jobs' initial
// state "live") without its current faults: the demo records lie in the past,
// so a two-week-old recommendation is not judged on a valve that started
// leaking today or a sensor bias of the live FDD demo.
func (d *demoAI) snapshot(ctx context.Context, seed uint64) (*Plant, error) {
	var st SimState
	if !getSetting(ctx, d.db, stateKey, &st) {
		return nil, errors.New("no saved plant state")
	}
	p := newPlant(seed, d.loc)
	restoreState(p, st)
	for _, a := range p.Assets {
		clear(a.Faults)
	}
	return p, nil
}

func (d *demoAI) save() error { return putSetting(d.ctx, d.db, demoAIKey, d.m) }

func (d *demoAI) clear() error {
	for _, q := range []struct {
		sql string
		ids []int64
	}{
		{`DELETE FROM analytics.recommendations WHERE id = ANY($1)`, d.m.Recs},
		{`DELETE FROM sim.jobs WHERE id = ANY($1)`, d.m.Jobs},
		{`DELETE FROM analytics.optimizer_runs WHERE id = ANY($1)`, d.m.Runs},
	} {
		if len(q.ids) == 0 {
			continue
		}
		if _, err := d.db.Exec(d.ctx, q.sql, q.ids); err != nil {
			return err
		}
	}
	_, err := d.db.Exec(d.ctx, `DELETE FROM analytics.faults WHERE evidence->>'source' = 'history'`)
	return err
}

func (d *demoAI) userID(username string) any {
	if username == "" {
		return nil
	}
	if id := idOf(d.ctx, d.db, `SELECT id FROM core.users WHERE lower(username) = lower($1)`, username); id != 0 {
		return id
	}
	return nil
}

// faults stores an analytics fault per FDD alarm of the history (the engine
// raises an active analytics fault as alarm ext:fdd:<rule>): cleared when the
// FDD order repaired it, active when it still runs at the end of the history
// (the analytics service then confirms or clears it on live data). The
// instructions are written by the analytics service.
func (d *demoAI) faults() (int64, error) {
	tag, err := d.db.Exec(d.ctx, `INSERT INTO analytics.faults (site_code, rule_code, asset_id, severity, message, evidence,
			started_at, last_seen_at, ended_at, status)
		SELECT COALESCE(v.site_code, 'SGF1'), r.code, e.asset_id, r.severity,
			r.name || ' on ' || v.alias || ': ' || lower(left(r.lbnl_fault, 1)) || substr(r.lbnl_fault, 2),
			jsonb_build_object('source', 'history', 'window', jsonb_build_object('start', e.start_ts, 'end', e.end_ts),
				'rule_version', r.version, 'detected_by', 'FDD rule ' || r.code),
			e.start_ts, CASE WHEN ongoing THEN now() ELSE e.end_ts END, CASE WHEN ongoing THEN NULL ELSE e.end_ts END,
			CASE WHEN ongoing THEN 'ACTIVE' ELSE 'CLEARED' END
		FROM (SELECT e.*, e.end_ts >= $1::timestamptz - interval '10 minutes' AS ongoing FROM ts.alarm_events e
			WHERE e.source_key LIKE 'ext:fdd:%' AND e.end_ts IS NOT NULL) e
		JOIN analytics.fault_rules r ON r.code = substr(e.source_key, 9)
		JOIN analytics.v_assets v ON v.id = e.asset_id
		ORDER BY e.start_ts
		ON CONFLICT DO NOTHING`, d.historyEnd())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// historyEnd is the end of the generated history (the history closes every
// status interval there).
func (d *demoAI) historyEnd() time.Time {
	var sm seedMarker
	getSetting(d.ctx, d.db, seedKey, &sm)
	var t time.Time
	if err := d.db.QueryRow(d.ctx, `SELECT COALESCE(max(end_ts), now()) FROM ts.asset_status WHERE end_ts <= $1`, sm.History).Scan(&t); err != nil {
		return d.now
	}
	return t
}

// ---------------------------------------------------------------------------
// Simulation jobs

type demoJob struct {
	Kind, Title, Origin, Status string
	By, Decider                 string        // requested by, approved / rejected by
	Ago                         time.Duration // created this long ago
	Spec                        map[string]any
	Rationale, Note, Error      string
	Progress                    float64
}

func control(at float64, asset, param string, v float64) map[string]any {
	return map[string]any{"at_min": at, "kind": "control", "asset": asset, "param": param, "value": v}
}

// scenarioSpec is a scenario spec starting at start (zero: when it runs).
func scenarioSpec(start time.Time, minutes float64, variants ...map[string]any) map[string]any {
	var st any
	if !start.IsZero() {
		st = start.UTC().Format(time.RFC3339)
	}
	vs := []any{map[string]any{"name": "baseline", "actions": []any{}}}
	for _, v := range variants {
		vs = append(vs, v)
	}
	return map[string]any{"start": st, "duration_min": minutes, "initial": "live", "stochastic": false, "seed": 0,
		"weather": map[string]any{"dry_bulb_offset": 0, "wet_bulb_offset": 0}, "load_scale": 1, "actions": []any{}, "variants": vs}
}

func variant(name string, actions ...map[string]any) map[string]any {
	list := []any{}
	for _, a := range actions {
		list = append(list, a)
	}
	return map[string]any{"name": name, "actions": list}
}

func prodSpec(days float64, variants ...map[string]any) map[string]any {
	vs := []any{}
	for _, v := range variants {
		vs = append(vs, v)
	}
	return map[string]any{"kind": "production", "horizon_days": days, "initial": "live", "seed": 0, "breakdowns": true,
		"plan": map[string]any{}, "actions": []any{}, "variants": vs}
}

func prodVariant(name string, plan map[string]any, actions ...map[string]any) map[string]any {
	list := []any{}
	for _, a := range actions {
		list = append(list, a)
	}
	return map[string]any{"name": name, "plan": plan, "actions": list}
}

// localAt is the plant-local time of day hh:mm on the day the given time
// falls on, plus days.
func (d *demoAI) localAt(t time.Time, days, hh, mm int) time.Time {
	l := t.In(d.loc)
	return time.Date(l.Year(), l.Month(), l.Day()+days, hh, mm, 0, 0, d.loc).UTC()
}

func (d *demoAI) demoJobs() []demoJob {
	h := time.Hour
	day := 24 * h
	c := func(ago time.Duration) time.Time { return d.now.Add(-ago) }
	interrupted := "interrupted: the simulator restarted while the job was running"
	return []demoJob{
		// --- chiller plant -----------------------------------------------------
		{Kind: "scenario", Title: "Afternoon peak: 2 vs 3 chillers on line (6 h)", Origin: "ui", Status: "SUCCEEDED", By: "sarah_lim", Ago: 9*day + 3*h,
			Spec: scenarioSpec(d.localAt(c(9*day), 1, 12, 0), 360, variant("3 chillers (manual staging)",
				control(0, "CHWS_01", "staging_mode", 1), control(0, "CHWS_01", "chillers_sp", 3))),
			Rationale: "Check whether running the third chiller through the afternoon peak lowers kW/RT enough to pay for its auxiliaries."},
		{Kind: "scenario", Title: "Raise CHW supply set point to 7.2 °C overnight (8 h)", Origin: "agent", Status: "SUCCEEDED", By: "kevin_ong", Decider: "kevin_ong", Ago: 7*day + 5*h,
			Spec: scenarioSpec(d.localAt(c(7*day), 0, 22, 0), 480, variant("CHWS 7.2 °C",
				control(0, "CHWS_01", "chws_reset_mode", 1), control(0, "CHWS_01", "chws_sp", 7.2))),
			Rationale: "Night load is about 70 % of the day peak; a warmer supply lowers the chiller lift while the MAU valves still have margin."},
		{Kind: "doe", Title: "Surrogate refresh DOE (120 samples)", Origin: "analytics", Status: "SUCCEEDED", Ago: 6*day + 2*h,
			Spec: map[string]any{"samples": 120, "settle_min": 12, "avg_min": 5, "seed": 0, "purpose": "surrogate",
				"bounds": map[string]any{"chws_sp": []float64{5.5, 8.0}, "chillers": []float64{1, 3}, "wet_bulb": []float64{22, 28}, "load_frac": []float64{0.5, 0.97},
					"approach_sp": []float64{2.0, 6.0}, "cw_min_temp": []float64{18, 24}, "riser_dp_sp": []float64{80, 160}, "twv_min_pos": []float64{0, 30},
					"dry_bulb_minus_wb": []float64{1, 7}}}},
		{Kind: "scenario", Title: "Tower approach set point 3.5 K vs 2.5 K (4 h)", Origin: "ui", Status: "SUCCEEDED", By: "kevin_ong", Ago: 5*day + 6*h,
			Spec: scenarioSpec(d.localAt(c(5*day), 0, 14, 0), 240, variant("Approach 2.5 K", control(0, "CHWS_01", "approach_sp", 2.5)),
				variant("Approach 4.5 K", control(0, "CHWS_01", "approach_sp", 4.5))),
			Rationale: "Find the tower approach where fan energy and chiller lift balance at this wet bulb."},
		{Kind: "scenario", Title: "Simulate CH-02 tripping at peak load", Origin: "agent", Status: "REJECTED", By: "sarah_lim", Decider: "sarah_lim", Ago: 5*day + 1*h,
			Spec:      scenarioSpec(time.Time{}, 180, variant("CH_02 trip", map[string]any{"at_min": 60, "kind": "trip", "asset": "CH_02"})),
			Rationale: "Show how the standby chiller and the riser DP respond if the duty chiller trips at peak.",
			Note:      "Not during the Monday duty rotation - propose it again for the weekend."},
		{Kind: "scenario", Title: "Condenser 3-way valve leakage: energy impact (6 h)", Origin: "analytics", Status: "SUCCEEDED", Ago: 4*day + 4*h,
			Spec: scenarioSpec(d.localAt(c(4*day), 0, 10, 0), 360, variant("Valve leaking 50 %",
				map[string]any{"at_min": 0, "kind": "fault", "asset": "CHWS_01", "fault": "twv_leak", "value": 0.5})),
			Rationale: "FDD rule CHS-006 calibration check: plant kW penalty of a 50 % bypass leak."},
		{Kind: "scenario", Title: "Riser DP 120 vs 105 kPa (pump energy, 4 h)", Origin: "ui", Status: "FAILED", By: "kevin_ong", Ago: 3*day + 7*h,
			Spec:  scenarioSpec(time.Time{}, 240, variant("Riser DP 105 kPa", control(0, "CHWS_01", "riser_dp_sp", 105))),
			Error: interrupted, Progress: 0.42},
		{Kind: "scenario", Title: "Standby chiller response to a CH-02 trip (3 h)", Origin: "ui", Status: "SUCCEEDED", By: "sarah_lim", Ago: 2*day + 20*h,
			Spec:      scenarioSpec(d.localAt(c(2*day), 1, 6, 0), 180, variant("CH_02 trip at 60 min", map[string]any{"at_min": 60, "kind": "trip", "asset": "CH_02"})),
			Rationale: "Weekend re-run of the trip what-if."},
		{Kind: "scenario", Title: "Pre-cool before the afternoon peak (CHWS 6.0 °C, 3 h)", Origin: "ui", Status: "CANCELLED", By: "sarah_lim", Ago: 2*day + 2*h,
			Spec: scenarioSpec(time.Time{}, 180, variant("CHWS 6.0 °C", control(0, "CHWS_01", "chws_reset_mode", 1), control(0, "CHWS_01", "chws_sp", 6.0))),
			Note: "Wrong window - rerunning from 11:00.", Progress: 0.18},
		{Kind: "scenario", Title: "Riser DP set point 120 → 105 kPa (4 h)", Origin: "agent", Status: "SUCCEEDED", By: "kevin_ong", Decider: "kevin_ong", Ago: 30 * h,
			Spec:      scenarioSpec(d.localAt(c(30*h), 0, 9, 0), 240, variant("Riser DP 105 kPa", control(0, "CHWS_01", "riser_dp_sp", 105))),
			Rationale: "The MAU valves top out near 70 %; a lower riser DP should save pump energy without starving the far coils."},
		{Kind: "scenario", Title: "Tower fouling (capacity factor 0.8): FDD threshold check", Origin: "agent", Status: "PROPOSED", By: "kevin_ong", Ago: 3 * h,
			Spec:      scenarioSpec(time.Time{}, 360, variant("CT_01 fouled", map[string]any{"at_min": 0, "kind": "fault", "asset": "CT_01", "fault": "ct_fouling", "value": 0.8})),
			Rationale: "Re-check rule CT-014 after its last calibration: approach rise of a 20 % capacity loss at today's wet bulb."},
		{Kind: "scenario", Title: "Stage the third chiller at 85 % load instead of 95 %", Origin: "agent", Status: "PROPOSED", By: "rajesh_kumar", Ago: 1 * h,
			Spec:      scenarioSpec(time.Time{}, 240, variant("Manual 3 chillers", control(0, "CHWS_01", "staging_mode", 1), control(0, "CHWS_01", "chillers_sp", 3))),
			Rationale: "Earlier staging keeps each chiller nearer its best efficiency point during the 13:00-17:00 peak."},
		// --- fab production -----------------------------------------------------
		{Kind: "production", Title: "Current plan, 7 days", Origin: "ui", Status: "SUCCEEDED", By: "nurul_aisyah", Ago: 29 * h,
			Spec: prodSpec(7, prodVariant("Current plan", map[string]any{}))},
		{Kind: "production", Title: "Starts 4.5 vs 5.5 lots/day: WIP and cycle time (14 days)", Origin: "agent", Status: "SUCCEEDED", By: "nurul_aisyah", Decider: "nurul_aisyah", Ago: 22 * h,
			Spec:      prodSpec(14, prodVariant("Current plan", map[string]any{}), prodVariant("5.5 starts/day", map[string]any{"starts_per_day": 5.5})),
			Rationale: "Customer pull-in of MCU40: does the line absorb one more lot per day without the furnaces and wet benches running away?"},
		{Kind: "production", Title: "Product mix 60/40 vs 75/25 MCU40/WSOC40 (10 days)", Origin: "ui", Status: "SUCCEEDED", By: "nurul_aisyah", Ago: 15 * h,
			Spec: prodSpec(10, prodVariant("Current mix", map[string]any{}), prodVariant("MCU40 75 %", map[string]any{"mix": map[string]any{"MCU40": 0.75, "WSOC40": 0.25}}))},
		{Kind: "production", Title: "ETCH_02 down 24 h for a chamber wet clean", Origin: "analytics", Status: "SUCCEEDED", Ago: 9 * h,
			Spec: prodSpec(7, prodVariant("No downtime", map[string]any{}), prodVariant("ETCH_02 down day 1",
				map[string]any{}, map[string]any{"kind": "tool_down", "asset": "ETCH_02", "at_day": 1, "duration_h": 24, "reason": "Chamber wet clean"})),
			Rationale: "Early warning ETD-013 on ETCH_02 points to a chamber clean; check the impact of taking it on day 1."},
		{Kind: "production", Title: "Dispatch FIFO vs critical ratio (14 days)", Origin: "agent", Status: "SUCCEEDED", By: "nurul_aisyah", Decider: "nurul_aisyah", Ago: 5 * h,
			Spec:      prodSpec(14, prodVariant("FIFO", map[string]any{"dispatch": "FIFO"}), prodVariant("Critical ratio", map[string]any{"dispatch": "CR"})),
			Rationale: "On-time delivery slipped on WSOC40; critical-ratio dispatch should protect the late lots."},
		{Kind: "production", Title: "Furnace batching: wait 10 h vs 4 h for a full boat", Origin: "ui", Status: "FAILED", By: "nurul_aisyah", Ago: 3*day + 2*h,
			Spec:  prodSpec(14, prodVariant("Wait 10 h", map[string]any{"furnace_wait_h": 10}), prodVariant("Wait 4 h", map[string]any{"furnace_wait_h": 4})),
			Error: interrupted, Progress: 0.63},
		{Kind: "production", Title: "Add a second plasma asher", Origin: "agent", Status: "REJECTED", By: "nurul_aisyah", Decider: "sarah_lim", Ago: 2*day + 4*h,
			Spec: prodSpec(30, prodVariant("Current", map[string]any{}), prodVariant("Second asher", map[string]any{},
				map[string]any{"kind": "add_tool", "asset": "ASH_01", "count": 1})),
			Rationale: "ASH_01 runs above 75 % utilization; a second asher would remove the strip bottleneck.",
			Note:      "No capex this quarter - look at asher PM timing first."},
		{Kind: "production", Title: "30-day horizon at 6 starts/day", Origin: "ui", Status: "CANCELLED", By: "nurul_aisyah", Ago: 26 * h,
			Spec: prodSpec(30, prodVariant("6 starts/day", map[string]any{"starts_per_day": 6})), Note: "Started by mistake.", Progress: 0.27},
		{Kind: "production", Title: "Hot lot share 5 % → 10 % (14 days)", Origin: "agent", Status: "PROPOSED", By: "nurul_aisyah", Ago: 2 * h,
			Spec:      prodSpec(14, prodVariant("Hot 5 %", map[string]any{"hot_share": 0.05}), prodVariant("Hot 10 %", map[string]any{"hot_share": 0.10})),
			Rationale: "Two customers asked for expedited lots; check what doubling the hot lots costs the normal lots in cycle time."},
		{Kind: "production", Title: "OHT fleet 12 vs 9 vehicles during the AMHS upgrade", Origin: "agent", Status: "PROPOSED", By: "nurul_aisyah", Ago: 40 * time.Minute,
			Spec:      prodSpec(7, prodVariant("Current fleet", map[string]any{}), prodVariant("9 vehicles", map[string]any{"vehicles": 9})),
			Rationale: "Three vehicles go out for the rail upgrade next week; check transport waits at the litho bay."},
	}
}

// jobs stores the demo jobs; the succeeded ones run on the twin first.
func (d *demoAI) jobs() error {
	for _, j := range d.demoJobs() {
		if _, err := d.job(j, nil); err != nil {
			return fmt.Errorf("%s: %w", j.Title, err)
		}
	}
	return nil
}

// job inserts one job (and runs it when it succeeded); recID links an
// optimizer validation.
func (d *demoAI) job(j demoJob, recID *int64) (int64, error) {
	spec, _ := json.Marshal(j.Spec)
	created := d.now.Add(-j.Ago)
	var approvedAt, started, finished any
	if j.Decider != "" {
		approvedAt = created.Add(3 * time.Minute)
	}
	status := j.Status
	run := status == "SUCCEEDED"
	switch status {
	case "SUCCEEDED", "FAILED", "CANCELLED":
		s := created.Add(time.Minute)
		if j.Decider != "" {
			s = created.Add(4 * time.Minute)
		}
		started, finished = s, s.Add(time.Duration(1+len(j.Title)%3)*time.Minute)
		if run {
			status = "RUNNING"
		}
	case "REJECTED":
		finished = nil
	}
	var note, errText, rationale any
	if j.Note != "" {
		note = j.Note
	}
	if j.Error != "" {
		errText = j.Error
	}
	if j.Rationale != "" {
		rationale = j.Rationale
	}
	var id int64
	if err := d.db.QueryRow(d.ctx, `INSERT INTO sim.jobs (kind, title, origin, status, spec, rationale, requested_by, approved_by, approved_at,
			decision_note, recommendation_id, progress, error, created_at, started_at, finished_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::timestamptz,$15::timestamptz,$16::timestamptz,
			COALESCE($16::timestamptz, $15::timestamptz, $14::timestamptz)) RETURNING id`,
		j.Kind, j.Title, j.Origin, status, spec, rationale, d.userID(j.By), d.userID(j.Decider), approvedAt, note, recID, j.Progress, errText,
		created, started, finished).Scan(&id); err != nil {
		return 0, err
	}
	d.m.Jobs = append(d.m.Jobs, id)
	if err := d.save(); err != nil {
		return id, err
	}
	if !run {
		return id, nil
	}
	d.runner.execute(&simJob{ID: id, Kind: j.Kind, Title: j.Title, Origin: j.Origin, Spec: spec, StepS: 60})
	var after string
	var cause *string
	if err := d.db.QueryRow(d.ctx, `SELECT status, error FROM sim.jobs WHERE id = $1`, id).Scan(&after, &cause); err != nil {
		return id, err
	}
	if after == "FAILED" {
		slog.Warn("demo simulation job failed", "job", id, "title", j.Title, "err", cause)
		_, err := d.db.Exec(d.ctx, `UPDATE sim.jobs SET finished_at = $2, updated_at = $2 WHERE id = $1`, id, finished)
		return id, err
	}
	switch j.Kind {
	case "production":
		// a production plan starts from the plant as it is: move its window
		// and dates back to when the job ran
		return id, d.backdateProduction(id, started.(time.Time), finished.(time.Time))
	case "doe":
		_, err := d.db.Exec(d.ctx, `UPDATE sim.jobs SET sim_end = $2::timestamptz + (sim_end - sim_start), sim_start = $2 WHERE id = $1`, id, started)
		return id, err
	}
	return id, nil
}

// backdateProduction moves a production job's simulated window, daily series
// and events to its (past) start.
func (d *demoAI) backdateProduction(id int64, started, finished time.Time) error {
	var simStart time.Time
	var raw []byte
	var status string
	if err := d.db.QueryRow(d.ctx, `SELECT status, COALESCE(sim_start, now()), summary FROM sim.jobs WHERE id = $1`, id).Scan(&status, &simStart, &raw); err != nil {
		return err
	}
	if status != "SUCCEEDED" {
		return fmt.Errorf("job %d ended %s", id, status)
	}
	shift := started.Sub(simStart).Truncate(time.Minute)
	ds, de := simStart.In(d.loc), started.In(d.loc)
	days := int(math.Round(time.Date(de.Year(), de.Month(), de.Day(), 0, 0, 0, 0, time.UTC).Sub(
		time.Date(ds.Year(), ds.Month(), ds.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24))
	var summary any
	if err := json.Unmarshal(raw, &summary); err != nil {
		return err
	}
	_, err := d.db.Exec(d.ctx, `UPDATE sim.jobs SET summary = $2, sim_start = sim_start + $3::interval, sim_end = sim_end + $3::interval,
		finished_at = $4, updated_at = $4 WHERE id = $1`, id, shiftTimes(summary, shift, days), fmt.Sprintf("%d seconds", int64(shift.Seconds())), finished)
	return err
}

// shiftTimes moves RFC 3339 timestamps by d and plain dates by days.
func shiftTimes(v any, d time.Duration, days int) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = shiftTimes(e, d, days)
		}
	case []any:
		for i, e := range x {
			x[i] = shiftTimes(e, d, days)
		}
	case string:
		if len(x) == 10 {
			if t, err := time.Parse("2006-01-02", x); err == nil {
				return t.AddDate(0, 0, days).Format("2006-01-02")
			}
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.Add(d).Format(time.RFC3339)
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// Optimizer recommendations

type recAct struct {
	Param string
	Value float64
}

type demoRec struct {
	Ago       time.Duration
	Trigger   string // schedule | manual | agent
	By        string // requester of a manual / agent run
	Acts      []recAct
	Bad       bool // kept as it is (the twin usually rejects it); else the first set that validates
	Interrupt bool // the validation job was interrupted (FAILED)
}

// recPool are further set point changes tried in turn (from a different one
// for every recommendation) until the twin validates one.
var recPool = [][]recAct{
	{{"chws_sp", 7.3}}, {{"approach_sp", 3.0}}, {{"approach_sp", 4.0}}, {{"chws_sp", 7.2}, {"riser_dp_sp", 125}}, {{"riser_dp_sp", 108}},
	{{"approach_sp", 4.5}}, {{"chws_sp", 7.4}, {"riser_dp_sp", 130}}, {{"approach_sp", 3.0}, {"chws_sp", 7.2}}, {{"twv_min_pos", 10}},
	{{"chws_sp", 7.5}}, {{"riser_dp_sp", 104}}, {{"approach_sp", 2.5}},
}

// validationSpec is the optimizer's validation (optimizer.validation_spec):
// baseline vs the recommended set points from t = 0, 2 h from start.
func validationSpec(start time.Time, acts []recAct, seed int64) map[string]any {
	vacts := []map[string]any{control(0, "CHWS_01", "chws_reset_mode", 1)}
	for _, a := range acts {
		vacts = append(vacts, control(0, "CHWS_01", a.Param, a.Value))
	}
	spec := scenarioSpec(start, 120, variant("recommended", vacts...))
	spec["seed"], spec["purpose"] = seed, "validation"
	return spec
}

// validates runs a validation on the twin in memory and applies the analytics
// service's verdict (simpost constraints, optimizer.judge): the recommended
// variant saves energy and causes no critical violation the baseline does not
// have (CHW supply above 8.5 °C or riser DP below 80 kPa for more than 10 min,
// a MAU CHW valve at 100 % for more than 15 min).
func (d *demoAI) validates(spec map[string]any, seed int64) bool {
	raw, _ := json.Marshal(spec)
	base, err := d.snapshot(d.ctx, uint64(seed))
	if err != nil {
		return false
	}
	base.dropFab()
	sp, err := parseScenario(raw, base, maxSystemJobMinutes)
	if err != nil || sp.Start == nil {
		return false
	}
	rec := &MemRecorder{}
	sum, err := (&scenarioRun{Spec: sp, Base: base, Seed: uint64(seed), Start: sp.Start.UTC().Truncate(time.Second), StepS: 60,
		Opts: defaultJobOptions(), Rec: rec}).run(d.ctx)
	if err != nil {
		return false
	}
	kw := func(v string) float64 {
		k, _ := sum["variants"].(map[string]any)[v].(map[string]any)
		f, _ := k["avg_plant_kw"].(float64)
		return f
	}
	// critical violations per variant: constraint -> violated minutes
	violations := func(v string) map[string]float64 {
		masks := map[string][]bool{}
		var valves [][]float32
		step := 60
		for _, r := range rec.SeriesRows {
			if r.Variant != v {
				continue
			}
			step = r.StepS
			switch {
			case r.Asset == "CHWS_01" && r.Param == "chws_temp":
				masks["chws_temp_high|10"] = maskOf(r.Measured, func(x float32) bool { return x > 8.5 })
			case r.Asset == "CHWS_01" && r.Param == "riser_dp":
				masks["riser_dp_low|10"] = maskOf(r.Measured, func(x float32) bool { return x < 80 })
			case strings.HasPrefix(r.Asset, "MAU") && r.Param == "chw_valve":
				valves = append(valves, r.Measured)
			}
		}
		if len(valves) > 0 {
			vm := make([]bool, len(valves[0]))
			for _, vs := range valves {
				for i := range vm {
					vm[i] = vm[i] || (i < len(vs) && vs[i] >= 99.5)
				}
			}
			masks["mau_valve_saturated|15"] = vm
		}
		out := map[string]float64{}
		for key, m := range masks {
			name, limit, _ := strings.Cut(key, "|")
			longest, run, total := 0, 0, 0
			for _, b := range m {
				if b {
					run++
					total++
					longest = max(longest, run)
				} else {
					run = 0
				}
			}
			if lim := map[string]float64{"10": 10, "15": 15}[limit]; float64(longest*step)/60 > lim {
				out[name] = float64(total*step) / 60
			}
		}
		return out
	}
	bv := violations("baseline")
	for c, minutes := range violations("recommended") {
		if b, ok := bv[c]; !ok || minutes > b+5 {
			return false
		}
	}
	return kw("baseline")-kw("recommended") > 0
}

func maskOf(xs []float32, f func(float32) bool) []bool {
	out := make([]bool, len(xs))
	for i, x := range xs {
		out[i] = !math.IsNaN(float64(x)) && f(x)
	}
	return out
}

var recParam = map[string][2]string{
	"chws_sp": {"CHW Supply Set Point", "°C"}, "approach_sp": {"Tower Approach Set Point", "K"}, "riser_dp_sp": {"Riser DP Set Point", "kPa"},
	"twv_min_pos": {"3-Way Valve Min Position", "%"}, "chillers_sp": {"Duty Chillers Set Point", ""},
}

// recDecisions are the decisions of the validated recommendations, oldest
// first; the newest validated one stays open (VALIDATED).
var recDecisions = []string{"APPLIED", "EXPIRED", "APPLIED", "DISMISSED", "APPLIED", "EXPIRED", "APPLIED", "DISMISSED", "APPLIED", "EXPIRED"}

var dismissNotes = []string{"Cleanroom humidity was already at 50 % RH - not raising the CHW supply today.",
	"Chiller rotation scheduled this afternoon; will review after the changeover."}

func (d *demoAI) demoRecs() []demoRec {
	h, day := time.Hour, 24*time.Hour
	return []demoRec{
		{Ago: 13*day + 5*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.2}, {"riser_dp_sp", 125}}},
		{Ago: 12*day + 2*h, Trigger: "schedule", Acts: []recAct{{"riser_dp_sp", 105}}, Bad: true},
		{Ago: 11*day + 7*h, Trigger: "schedule", Acts: []recAct{{"approach_sp", 3.0}, {"chws_sp", 7.0}}},
		{Ago: 10*day + 3*h, Trigger: "manual", By: "kevin_ong", Acts: []recAct{{"approach_sp", 4.5}}},
		{Ago: 9*day + 6*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.2}, {"riser_dp_sp", 110}}},
		{Ago: 8*day + 1*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 6.4}}, Interrupt: true},
		{Ago: 7*day + 4*h, Trigger: "agent", By: "sarah_lim", Acts: []recAct{{"chws_sp", 7.3}}},
		{Ago: 6*day + 8*h, Trigger: "schedule", Acts: []recAct{{"approach_sp", 3.0}}},
		{Ago: 5*day + 2*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.5}, {"twv_min_pos", 10}}, Bad: true},
		{Ago: 4*day + 5*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.1}, {"riser_dp_sp", 120}}},
		{Ago: 3*day + 9*h, Trigger: "manual", By: "sarah_lim", Acts: []recAct{{"approach_sp", 2.5}, {"chws_sp", 7.0}}},
		{Ago: 2*day + 3*h, Trigger: "schedule", Acts: []recAct{{"riser_dp_sp", 100}}, Bad: true},
		{Ago: 1*day + 6*h, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.2}, {"riser_dp_sp", 115}}},
		{Ago: 20 * h, Trigger: "agent", By: "kevin_ong", Acts: []recAct{{"riser_dp_sp", 110}, {"chws_sp", 7.2}}},
		{Ago: 4 * h, Trigger: "schedule", Acts: []recAct{{"approach_sp", 4.0}}},
		{Ago: 9 * h, Trigger: "schedule", Acts: []recAct{{"approach_sp", 2.5}, {"chws_sp", 7.5}}, Bad: true},
		{Ago: 50 * time.Minute, Trigger: "schedule", Acts: []recAct{{"chws_sp", 7.2}, {"riser_dp_sp", 110}}},
	}
}

// plantAt reads CHWS_01 values at t from the history.
func (d *demoAI) plantAt(t time.Time) map[string]float64 {
	out := map[string]float64{}
	rows, err := d.db.Query(d.ctx, `SELECT mp.alias, v.value FROM asset.equipment e
		JOIN asset.equipment_parameters ep ON ep.equipment_id = e.id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		CROSS JOIN LATERAL (SELECT value FROM ts.parameter_values pv WHERE pv.equipment_parameter_id = ep.id AND pv.ts <= $1 AND pv.ts > $1 - interval '1 hour'
			ORDER BY pv.ts DESC LIMIT 1) v
		WHERE e.alias = 'CHWS_01' AND v.value IS NOT NULL`, t)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		var v float64
		if rows.Scan(&a, &v) == nil {
			out[a] = v
		}
	}
	var valve float64
	if d.db.QueryRow(d.ctx, `SELECT max(v.value) FROM asset.equipment e
		JOIN asset.equipment_parameters ep ON ep.equipment_id = e.id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id AND mp.alias = 'chw_valve'
		CROSS JOIN LATERAL (SELECT value FROM ts.parameter_values pv WHERE pv.equipment_parameter_id = ep.id AND pv.ts <= $1 AND pv.ts > $1 - interval '1 hour'
			ORDER BY pv.ts DESC LIMIT 1) v WHERE e.alias LIKE 'MAU%'`, t).Scan(&valve) == nil {
		out["mau_valve_max"] = valve
	}
	return out
}

// plantOff reports whether CHWS_01 values of plantAt show no running chiller,
// no load or no plant power: nothing to optimise there, and the conditions
// would divide by zero (NaN cannot be stored as JSON). A missing value (no
// sample in the hour) is not taken as off.
func plantOff(p map[string]float64) bool {
	for _, k := range []string{"chillers_running", "cooling_load", "plant_kw"} {
		if v, ok := p[k]; ok && v < 1 {
			return true
		}
	}
	return false
}

// recommendations stores the optimizer runs and recommendations, runs their
// validations on the twin, waits for the analytics service's verdicts and
// decides the validated ones.
func (d *demoAI) recommendations() (int, error) {
	var modelID any
	modelVersion := "lgbm-doe"
	var mid int64
	var ver string
	if d.db.QueryRow(d.ctx, `SELECT id, version FROM analytics.surrogate_models ORDER BY id DESC LIMIT 1`).Scan(&mid, &ver) == nil {
		modelID, modelVersion = mid, ver
	}
	type made struct {
		id      int64
		created time.Time
		acts    []map[string]any
		by      string
	}
	var recs []made
	for i, r := range d.demoRecs() {
		// The history takes every chiller down at once now and then (overlapping
		// maintenance): step back to the latest half hour the plant ran.
		created := d.now.Add(-r.Ago)
		p := d.plantAt(created)
		for back := 0; plantOff(p) && back < 48; back++ {
			created = created.Add(-30 * time.Minute)
			p = d.plantAt(created)
		}
		if plantOff(p) {
			continue // no running plant within a day
		}
		r.Ago = d.now.Sub(created)
		seed := int64(1000 + i)
		if !r.Bad && !r.Interrupt {
			cands := [][]recAct{r.Acts}
			for k := range recPool {
				cands = append(cands, recPool[(i*5+k)%len(recPool)])
			}
			for _, c := range cands {
				if d.validates(validationSpec(created, c, seed), seed) {
					r.Acts = c
					break
				}
			}
		}
		get := func(k string, def float64) float64 {
			if v, ok := p[k]; ok {
				return v
			}
			return def
		}
		wb, oat, load := get("wet_bulb", 25.5), get("oa_temp", 28.5), get("cooling_load", 5000)
		kw, chws, riser, valve := get("plant_kw", 950), get("chws_temp", 6.85), get("riser_dp", 120), get("mau_valve_max", 60)
		chillers := math.Round(get("chillers_running", 2))
		cur := map[string]any{"chws_sp": round(get("chws_sp", 6.8), 1), "approach_sp": round(get("approach_sp", 3.5), 1),
			"riser_dp_sp": math.Round(get("riser_dp_sp", 120)), "twv_min_pos": math.Round(get("twv_min_pos", 0)), "chillers": chillers}
		rec := map[string]any{}
		for k, v := range cur {
			rec[k] = v
		}
		acts := []map[string]any{{"asset": "CHWS_01", "param": "chws_reset_mode", "value": 1.0, "unit": "", "label": "CHW Set Point Mode: Manual"}}
		var labels []string
		for _, a := range r.Acts {
			info := recParam[a.Param]
			from, _ := cur[a.Param].(float64)
			label := fmt.Sprintf("%s: %s → %s %s", info[0], fnum(from), fnum(a.Value), info[1])
			acts = append(acts, map[string]any{"asset": "CHWS_01", "param": a.Param, "value": a.Value, "unit": info[1], "label": strings.TrimSpace(label)})
			labels = append(labels, strings.TrimSpace(label))
			rec[a.Param] = a.Value
		}
		// surrogate prediction (the twin validation below is the real check)
		saving := 15 + float64((i*7)%13)
		pct := saving / kw * 100
		predCur := map[string]any{"plant_kw": round(kw*1.004, 3), "riser_dp": round(riser, 3), "chws_temp": round(chws, 3),
			"kw_per_rt": round(kw/(load/3.517), 4), "mau_valve_max": round(valve, 3), "mau_sat_dev_max": 0.18}
		recSP, _ := rec["chws_sp"].(float64)
		recDP, _ := rec["riser_dp_sp"].(float64)
		predRec := map[string]any{"plant_kw": round(kw*1.004-saving, 3), "riser_dp": round(recDP+1.5, 3), "chws_temp": round(recSP+0.03, 3),
			"kw_per_rt": round((kw*1.004-saving)/(load/3.517), 4), "mau_valve_max": round(math.Min(94, valve+(recSP-chws)*18+(120-recDP)*0.25), 3), "mau_sat_dev_max": 0.19}
		cond := map[string]any{"modes": map[string]any{"staging_mode": 0.0, "chws_reset_mode": 0.0},
			"window":   map[string]any{"start": created.Add(-15 * time.Minute).Format(time.RFC3339), "end": created.Format(time.RFC3339)},
			"dry_bulb": round(oat, 2), "wet_bulb": round(wb, 2), "load_frac": round(load/(chillers*3517), 4), "cw_min_temp": round(get("cw_min_temp", 20), 1),
			"cooling_load_kw": round(load, 1), "chillers_running": chillers,
			"measured": map[string]any{"plant_kw": round(kw, 3), "riser_dp": round(riser, 3), "chws_temp": round(chws, 3), "mau_valve_max": round(valve, 3), "mau_sat_dev_max": 0.09}}
		constraints := []map[string]any{
			{"ok": true, "op": "<=", "name": "chws_temp", "unit": "°C", "label": "CHW supply header temperature", "limit": 7.5, "margin": 0.0725,
				"current": predCur["chws_temp"], "predicted": predRec["chws_temp"], "recommended": predRec["chws_temp"]},
			{"ok": true, "op": "<=", "name": "mau_valve_max", "unit": "%", "label": "Highest MAU CHW valve position", "limit": 95.0, "margin": 4.75,
				"current": predCur["mau_valve_max"], "predicted": predRec["mau_valve_max"], "recommended": predRec["mau_valve_max"]},
			{"ok": true, "op": ">=", "name": "riser_dp", "unit": "kPa", "label": "Riser differential pressure", "limit": 95.0, "margin": 4.75,
				"current": predCur["riser_dp"], "predicted": predRec["riser_dp"], "recommended": predRec["riser_dp"]},
		}
		rationale := fmt.Sprintf("At wet bulb %.1f °C and %.0f kW cooling load the surrogate (%s) predicts %.0f kW plant power with: %s, against "+
			"%.0f kW at the current set points (saving %.0f kW, %.1f %%), with CHW supply %.2f °C, riser DP %.0f kPa and MAU valves up to %.0f %%.",
			wb, load, modelVersion, predRec["plant_kw"], strings.Join(labels, "; "), predCur["plant_kw"], saving, pct,
			predRec["chws_temp"], predRec["riser_dp"], predRec["mau_valve_max"])
		metrics := map[string]any{"recommended": true, "saving_kw": round(saving, 2), "saving_pct": round(pct, 3), "n_feasible": 300 - i%7,
			"model_version": modelVersion, "conditions": cond, "best": rec, "predicted_best": predRec, "predicted_current": predCur,
			"seconds": round(1.1+float64(i%5)*0.3, 3)}
		var runID int64
		if err := d.db.QueryRow(d.ctx, `INSERT INTO analytics.optimizer_runs (trigger, requested_by, status, model_id, n_trials, metrics, created_at, started_at, finished_at)
			VALUES ($1, $2, 'SUCCEEDED', $3, 300, $4, $5::timestamptz, $5::timestamptz, $5::timestamptz + interval '3 seconds') RETURNING id`,
			r.Trigger, d.userID(r.By), modelID, metrics, created.Add(-time.Minute)).Scan(&runID); err != nil {
			return 0, err
		}
		d.m.Runs = append(d.m.Runs, runID)
		var recID int64
		if err := d.db.QueryRow(d.ctx, `INSERT INTO analytics.recommendations (run_id, status, conditions, current, recommended, actions, savings_kw,
				savings_pct, constraints, model_id, rationale, created_at, valid_until)
			VALUES ($1, 'VALIDATING', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now() + interval '1 hour') RETURNING id`,
			runID, cond, map[string]any{"setpoints": cur, "predicted": predCur}, map[string]any{"setpoints": rec, "predicted": predRec},
			acts, round(saving, 2), round(pct, 3), constraints, modelID, rationale, created).Scan(&recID); err != nil {
			return 0, err
		}
		d.m.Recs = append(d.m.Recs, recID)
		if _, err := d.db.Exec(d.ctx, `UPDATE analytics.optimizer_runs SET metrics = metrics || jsonb_build_object('recommendation_id', $2::bigint) WHERE id = $1`,
			runID, recID); err != nil {
			return 0, err
		}
		// the validation: baseline vs recommended for 2 h from the recommendation
		spec := validationSpec(created, r.Acts, seed)
		job := demoJob{Kind: "scenario", Title: fmt.Sprintf("Validate recommendation #%d (baseline vs recommended, 2 h)", recID), Origin: "optimizer",
			Status: "SUCCEEDED", Ago: r.Ago - time.Minute, Spec: spec, Rationale: rationale}
		if r.Interrupt {
			job.Status, job.Error, job.Progress = "FAILED", "interrupted: the simulator restarted while the job was running", 0.55
		}
		jobID, err := d.job(job, &recID)
		if err != nil {
			return 0, err
		}
		if _, err := d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET validation_job_id = $2 WHERE id = $1`, recID, jobID); err != nil {
			return 0, err
		}
		if err := d.save(); err != nil {
			return 0, err
		}
		recs = append(recs, made{recID, created, acts, r.By})
	}
	// verdicts of the analytics service (validation jobs finished)
	deadline := time.Now().Add(20 * time.Minute)
	for {
		var open int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM analytics.recommendations WHERE id = ANY($1) AND status = 'VALIDATING'`, d.m.Recs).Scan(&open); err != nil {
			return 0, err
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%d recommendations still validating after 20 min (is the analytics service running?)", open)
		}
		select {
		case <-d.ctx.Done():
			return 0, d.ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	// decisions: the newest validated recommendation stays open
	var validated []made
	for _, r := range recs {
		if exists(d.ctx, d.db, `SELECT 1 FROM analytics.recommendations WHERE id = $1 AND status = 'VALIDATED'`, r.id) {
			validated = append(validated, r)
		}
		if _, err := d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET valid_until = created_at + interval '2 hours' WHERE id = $1`, r.id); err != nil {
			return 0, err
		}
	}
	for i, r := range validated {
		decided := r.created.Add(time.Duration(20+i*7%40) * time.Minute)
		var err error
		switch decision := recDecisions[i%len(recDecisions)]; {
		case i == len(validated)-1:
			_, err = d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET valid_until = now() + interval '12 hours' WHERE id = $1`, r.id)
		case decision == "APPLIED":
			by := map[bool]string{true: r.by, false: "sarah_lim"}[r.by != ""]
			u := userByName(by)
			var results []map[string]any
			for _, a := range r.acts {
				name := strings.SplitN(a["label"].(string), ":", 2)[0]
				results = append(results, map[string]any{"id": nil, "asset_id": nil, "parameter_id": nil, "alias": a["param"], "name": name,
					"unit": a["unit"], "scale": nil, "old_value": nil, "value": a["value"], "status": "APPLIED", "reason": nil,
					"requested_at": decided.Format(time.RFC3339), "completed_at": decided.Add(1300 * time.Millisecond).Format(time.RFC3339),
					"username": by, "user_name": u.Name, "asset_alias": "CHWS_01", "label": a["label"]})
			}
			record := map[string]any{"status": "APPLIED", "by": by, "by_name": u.Name, "at": decided.Format(time.RFC3339), "results": results,
				"planned": len(r.acts), "restores": []any{}}
			_, err = d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET status = 'APPLIED', apply_results = $2, decided_by = $3, decided_at = $4
				WHERE id = $1`, r.id, record, d.userID(by), decided)
		case decision == "DISMISSED":
			_, err = d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET status = 'DISMISSED', decided_by = $2, decided_at = $3, decision_note = $4
				WHERE id = $1`, r.id, d.userID("sarah_lim"), decided, dismissNotes[i%len(dismissNotes)])
		default:
			_, err = d.db.Exec(d.ctx, `UPDATE analytics.recommendations SET status = 'EXPIRED',
				validation = COALESCE(validation, '{}'::jsonb) || jsonb_build_object('expired_reason', 'valid_until passed') WHERE id = $1`, r.id)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(recs), nil
}
