package main

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"afdd/internal/engine"
)

// runSpec parses a scenario spec and runs it on a nominal plant.
func runSpec(t *testing.T, raw string, seed uint64) (*MemRecorder, map[string]any, time.Duration) {
	t.Helper()
	base := newPlant(seed, testLoc)
	spec, err := parseScenario([]byte(raw), base, maxSystemJobMinutes)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	rec := &MemRecorder{}
	run := &scenarioRun{Spec: spec, Base: base, Seed: seed, Start: testStart.UTC(), StepS: 60, Opts: defaultJobOptions(), Rec: rec}
	started := time.Now()
	sum, err := run.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rec, sum, time.Since(started)
}

func eventsOf(rec *MemRecorder, variant, kind string) []EventRow {
	var out []EventRow
	for _, e := range rec.EventRows {
		if e.Variant == variant && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func seriesRow(rec *MemRecorder, variant, asset, param string) *SeriesRow {
	for i := range rec.SeriesRows {
		s := &rec.SeriesRows[i]
		if s.Variant == variant && s.Asset == asset && s.Param == param {
			return s
		}
	}
	return nil
}

// TestScenario24h runs a 24 h single-variant scenario with a sensor fault, a
// tower fouling, a chiller trip and a set point change.
func TestScenario24h(t *testing.T) {
	rec, sum, took := runSpec(t, `{"duration_min": 1440, "initial": "nominal", "actions": [
		{"at_min": 120, "kind": "fault", "asset": "CH_01", "fault": "chwst_bias", "value": 1},
		{"at_min": 240, "kind": "fault", "asset": "CT_02", "fault": "ct_fouling", "value": 0.65, "duration_min": 360},
		{"at_min": 360, "kind": "trip", "asset": "CH_02", "duration_min": 40},
		{"at_min": 720, "kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 4.5}
	]}`, 101)
	t.Logf("24 h x 1 variant: %v (%d series, %d states, %d events)", took.Round(time.Millisecond), len(rec.SeriesRows), len(rec.StateRows), len(rec.EventRows))
	if took > 10*time.Second {
		t.Errorf("24 h run took %v, want < 10 s", took)
	}
	params := 0
	for _, a := range newPlant(1, testLoc).Assets {
		params += len(a.Model.Params)
	}
	if len(rec.SeriesRows) != params {
		t.Errorf("%d series, want one per asset parameter (%d)", len(rec.SeriesRows), params)
	}
	for _, s := range rec.SeriesRows {
		if len(s.Measured) != 1440 || (s.Truth != nil && len(s.Truth) != 1440) {
			t.Fatalf("%s.%s: %d values", s.Asset, s.Param, len(s.Measured))
		}
	}
	// truth only where the sensor lies
	if s := seriesRow(rec, "baseline", "CH_01", "chwst"); s == nil || s.Truth == nil {
		t.Error("CH_01 chwst has no truth series under its sensor bias")
	} else if d := float64(s.Measured[600] - s.Truth[600]); math.Abs(d-1) > 0.02 {
		t.Errorf("CH_01 chwst measured - true = %.2f, want 1", d)
	}
	if s := seriesRow(rec, "baseline", "CH_02", "chwst"); s == nil || s.Truth != nil {
		t.Error("CH_02 chwst should have no truth series")
	}
	kinds := map[string]int{}
	for _, e := range rec.EventRows {
		kinds[e.Kind]++
	}
	t.Logf("events by kind: %v", kinds)
	for _, k := range []string{"fault_injected", "fault_cleared", "trip", "control", "status"} {
		if kinds[k] == 0 {
			t.Errorf("no %s event", k)
		}
	}
	trip := eventsOf(rec, "baseline", "trip")
	if len(trip) != 1 || trip[0].Asset != "CH_02" || !trip[0].Ts.Equal(testStart.UTC().Add(361*time.Minute)) {
		t.Errorf("trip events %+v", trip)
	}
	// the standby chiller takes over while CH_02 is down, CH_02 comes back
	var ch2 []string
	for _, e := range eventsOf(rec, "baseline", "status") {
		if e.Asset == "CH_02" {
			ch2 = append(ch2, e.Code)
		}
	}
	t.Logf("CH_02 status changes: %v", ch2)
	if !slices.Contains(ch2, "UNPLANNED DOWN") {
		t.Errorf("CH_02 never down: %v", ch2)
	}
	ctl := eventsOf(rec, "baseline", "control")
	if len(ctl) != 1 || ctl[0].Details["status"] != "APPLIED" {
		t.Errorf("control events %+v", ctl)
	}
	k := sum["variants"].(map[string]any)["baseline"].(map[string]any)
	t.Logf("KPIs: %v", k)
	for _, key := range []string{"energy_kwh", "avg_plant_kw", "avg_kw_per_rt", "peak_load_kw", "avg_chws_temp", "max_chws_temp", "min_riser_dp",
		"max_mau_valve", "alarm_counts", "status_changes", "trips"} {
		if _, ok := k[key]; !ok {
			t.Errorf("KPI %s missing", key)
		}
	}
	if e := k["energy_kwh"].(float64); e < 15000 || e > 35000 {
		t.Errorf("energy %.0f kWh implausible for a day", e)
	}
	if r := k["avg_kw_per_rt"].(float64); r < 0.55 || r > 0.9 {
		t.Errorf("kW/RT %.3f implausible", r)
	}
	if k["trips"].(int) != 1 {
		t.Errorf("trips %v", k["trips"])
	}
	// states cover the run without gaps for every asset
	byAsset := map[string][]StateRow{}
	for _, s := range rec.StateRows {
		if s.Kind == "status" {
			byAsset[s.Asset] = append(byAsset[s.Asset], s)
		}
	}
	for alias, list := range byAsset {
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
		if !list[0].Start.Equal(testStart.UTC()) || !list[len(list)-1].End.Equal(testStart.UTC().Add(24*time.Hour)) {
			t.Errorf("%s states do not cover the run", alias)
		}
		for i := 1; i < len(list); i++ {
			if !list[i].Start.Equal(list[i-1].End) {
				t.Errorf("%s states have a gap at %v", alias, list[i].Start)
			}
		}
	}
}

// TestScenarioVariantsSameSeed: two variants with the same seed are identical
// until the first action of one of them.
func TestScenarioVariantsSameSeed(t *testing.T) {
	rec, sum, _ := runSpec(t, `{"duration_min": 240, "initial": "nominal", "seed": 77,
		"actions": [{"at_min": 30, "kind": "control", "asset": "CHWS_01", "param": "riser_dp_sp", "value": 115}],
		"variants": [{"name": "baseline"}, {"name": "trial", "actions": [
			{"at_min": 120, "kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 5.0}]}]}`, 77)
	type key struct{ asset, param string }
	base := map[key]*SeriesRow{}
	for i := range rec.SeriesRows {
		if s := &rec.SeriesRows[i]; s.Variant == "baseline" {
			base[key{s.Asset, s.Param}] = s
		}
	}
	firstDiff := 240
	for i := range rec.SeriesRows {
		s := &rec.SeriesRows[i]
		if s.Variant != "trial" {
			continue
		}
		b := base[key{s.Asset, s.Param}]
		for k := range s.Measured {
			eq := s.Measured[k] == b.Measured[k] || (math.IsNaN(float64(s.Measured[k])) && math.IsNaN(float64(b.Measured[k])))
			if !eq {
				firstDiff = min(firstDiff, k)
				break
			}
		}
	}
	t.Logf("first difference between the variants at step %d", firstDiff)
	if firstDiff != 120 {
		t.Errorf("variants first differ at step %d, want 120 (the trial's action)", firstDiff)
	}
	v := sum["variants"].(map[string]any)
	b, tr := v["baseline"].(map[string]any), v["trial"].(map[string]any)
	t.Logf("baseline %.0f kWh %.4f kW/RT, approach 5.0 trial %.0f kWh %.4f kW/RT", b["energy_kwh"], b["avg_kw_per_rt"], tr["energy_kwh"], tr["avg_kw_per_rt"])
	if b["energy_kwh"] == tr["energy_kwh"] {
		t.Error("the trial's action changed nothing")
	}
}

// TestScenarioActionOrder: at the same minute a control mode is written before
// the set point it gates, whatever the order of the spec (common or variant
// actions); timed reverts restore the set point before its mode returns to
// automatic, and a set point revert whose mode is already automatic is
// skipped. No control is refused.
func TestScenarioActionOrder(t *testing.T) {
	rec, _, _ := runSpec(t, `{"duration_min": 60, "initial": "nominal", "actions": [
		{"at_min": 2, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7.0, "duration_min": 30},
		{"at_min": 2, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1, "duration_min": 30},
		{"at_min": 10, "kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 80, "duration_min": 20}],
		"variants": [{"name": "v", "actions": [
		{"at_min": 10, "kind": "control", "asset": "CT_01", "param": "fan_mode", "value": 1, "duration_min": 20},
		{"at_min": 40, "kind": "control", "asset": "CHWS_01", "param": "chillers_sp", "value": 3, "duration_min": 15},
		{"at_min": 40, "kind": "control", "asset": "CHWS_01", "param": "staging_mode", "value": 1, "duration_min": 5}]}]}`, 3)
	type ctl struct {
		min    int
		code   string
		revert bool
	}
	var got []ctl
	for _, e := range eventsOf(rec, "v", "control") {
		if e.Details["status"] != "APPLIED" {
			t.Errorf("control refused at %v: %s", e.Ts, e.Message)
		}
		got = append(got, ctl{int(e.Ts.Sub(testStart) / time.Minute), e.Code, e.Details["revert"] == true})
	}
	t.Logf("control events %v", got)
	want := []ctl{
		{2, "chws_reset_mode", false}, {2, "chws_sp", false},
		{10, "fan_mode", false}, {10, "fan_speed_sp", false},
		{30, "fan_speed_sp", true}, {30, "fan_mode", true},
		{32, "chws_sp", true}, {32, "chws_reset_mode", true},
		{40, "staging_mode", false}, {40, "chillers_sp", false},
		{45, "staging_mode", true}, // chillers_sp at 55: its mode is automatic again, nothing to restore
	}
	// A tower stopped in automatic reports no fan set point: then there is
	// nothing to restore at 30 either.
	if len(got) == len(want)-1 {
		want = append(want[:4], want[5:]...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("control events\n got %v\nwant %v", got, want)
	}
	if s := seriesRow(rec, "v", "CHWS_01", "chws_reset_mode"); s.Measured[20] != 1 || s.Measured[40] != 0 {
		t.Errorf("chws_reset_mode %v at 20 min, %v at 40 min", s.Measured[20], s.Measured[40])
	}
}

// TestRevertWithoutSetPoint: a set point revert whose recorded value is
// outside the range (the unit was stopped in automatic) is skipped.
func TestRevertWithoutSetPoint(t *testing.T) {
	p := newPlant(4, testLoc)
	ct := p.ByAlias["CT_02"]
	if err := ct.applyControl("fan_mode", 1); err != nil {
		t.Fatal(err)
	}
	if err := ct.applyControl("fan_speed_sp", 80); err != nil {
		t.Fatal(err)
	}
	out := &MemRecorder{}
	r := &scenarioRun{Start: testStart, StepS: 60}
	rec := newVariantRecorder(p, "v", testStart, 60, 1, out)
	value := 80.0
	act := SimAction{Kind: "control", Asset: "CT_02", Param: "fan_speed_sp", Value: &value}
	if next := r.apply(p, pendingAction{at: testStart, act: act, revert: true, old: 0}, testStart, 0, rec, nil); next != nil || len(out.EventRows) != 0 {
		t.Fatalf("revert to 0 %%: next %v, events %+v", next, out.EventRows)
	}
	if ct.Ctl["fan_speed_sp"] != 80 {
		t.Errorf("fan set point %v, want 80 kept", ct.Ctl["fan_speed_sp"])
	}
	// a revert within the range is applied
	r.apply(p, pendingAction{at: testStart, act: act, revert: true, old: 70}, testStart, 0, rec, nil)
	if len(out.EventRows) != 1 || out.EventRows[0].Details["status"] != "APPLIED" || ct.Ctl["fan_speed_sp"] != 70 {
		t.Fatalf("revert to 70 %%: %+v, set point %v", out.EventRows, ct.Ctl["fan_speed_sp"])
	}
}

// TestTrackSeverityChange: an open abnormal event whose severity changes is
// closed and reopened with the new severity at once (as the live engine).
func TestTrackSeverityChange(t *testing.T) {
	p := newPlant(1, testLoc)
	out := &MemRecorder{}
	r := newVariantRecorder(p, "v", testStart, 60, 6, out)
	a := p.ByAlias["CT_01"]
	at := func(k int) time.Time { return testStart.Add(time.Duration(k) * time.Minute) }
	for k, sev := range []string{"WARNING", "WARNING", "CRITICAL", "CRITICAL", "WARNING"} {
		ev := Evaluation{Status: engine.StatusRunning, Cond: sev, Alarms: map[string]hit{},
			Abnormal: map[string]hit{"cwst": {Severity: sev, Message: "cwst " + sev, Value: 31}}}
		r.track(a, r.series[a], ev, at(k))
	}
	r.finish(at(5))
	type span struct {
		sev        string
		start, end int
	}
	var got []span
	for _, e := range out.EventRows {
		if e.Kind != "abnormal" {
			continue
		}
		s := span{e.Severity, int(e.Ts.Sub(testStart) / time.Minute), -1}
		if e.End != nil {
			s.end = int(e.End.Sub(testStart) / time.Minute)
		}
		got = append(got, s)
	}
	want := []span{{"WARNING", 1, 2}, {"CRITICAL", 2, 4}, {"WARNING", 4, -1}}
	if !slices.Equal(got, want) {
		t.Fatalf("abnormal events %v, want %v", got, want)
	}
}

func TestScenarioSpecValidation(t *testing.T) {
	p := newPlant(1, testLoc)
	for _, raw := range []string{
		`{"duration_min": 5}`,
		`{"duration_min": 60, "initial": "yesterday"}`,
		`{"duration_min": 60, "load_scale": 2}`,
		`{"duration_min": 60, "variants": [{"name": "a"}, {"name": "a"}]}`,
		`{"duration_min": 60, "variants": [{"name": "a"}, {"name": "b"}, {"name": "c"}, {"name": "d"}]}`,
		`{"duration_min": 60, "actions": [{"at_min": 1, "kind": "control", "asset": "CH_09", "param": "chwst_sp", "value": 7}]}`,
		`{"duration_min": 60, "actions": [{"at_min": 1, "kind": "control", "asset": "CH_01", "param": "chwst", "value": 7}]}`,
		`{"duration_min": 60, "actions": [{"at_min": 1, "kind": "fault", "asset": "CH_01", "fault": "twv_leak", "value": 0.5}]}`,
		`{"duration_min": 60, "actions": [{"at_min": 90, "kind": "trip", "asset": "CH_01"}]}`,
		`{"duration_min": 60, "actions": [{"at_min": 1, "kind": "explode", "asset": "CH_01"}]}`,
	} {
		if _, err := parseScenario([]byte(raw), p, maxJobMinutes); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	s, err := parseScenario([]byte(`{"duration_min": 60}`), p, maxJobMinutes)
	if err != nil || s.Initial != "live" || s.LoadScale != 1 || len(s.Variants) != 1 || s.Variants[0].Name != "baseline" {
		t.Errorf("defaults: %+v, %v", s, err)
	}
	// the analytics' 72 h calibration run is beyond the limit of people's jobs
	long := []byte(`{"duration_min": 4320, "initial": "nominal", "purpose": "calibration"}`)
	if _, err := parseScenario(long, p, jobMaxMinutes("analytics")); err != nil {
		t.Errorf("72 h analytics job refused: %v", err)
	}
	if _, err := parseScenario(long, p, jobMaxMinutes("ui")); err == nil {
		t.Error("72 h UI job accepted")
	}
}

func TestDOE(t *testing.T) {
	spec, err := parseDOE([]byte(`{"samples": 24, "settle_min": 12, "avg_min": 5, "seed": 5}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := &MemRecorder{}
	run := &doeRun{Spec: spec, Base: newPlant(5, testLoc), Seed: 5, Start: testStart.UTC(), StepS: 60, Rec: rec}
	started := time.Now()
	sum, err := run.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d samples in %v, summary %v", len(rec.Samples), time.Since(started).Round(time.Millisecond), sum)
	if len(rec.Samples) != 24 {
		t.Fatalf("%d samples", len(rec.Samples))
	}
	keys := func(m map[string]float64) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	wantIn, wantOut := slices.Sorted(slices.Values(doeInputs)), slices.Sorted(slices.Values(doeOutputs))
	chillers := map[float64]int{}
	for _, s := range rec.Samples {
		if !slices.Equal(keys(s.Inputs), wantIn) || !slices.Equal(keys(s.Outputs), wantOut) {
			t.Fatalf("sample %d keys %v / %v", s.Idx, keys(s.Inputs), keys(s.Outputs))
		}
		for k, b := range spec.Bounds {
			if v := s.Inputs[k]; v < b[0]-1e-9 || v > b[1]+1e-9 {
				t.Errorf("sample %d %s = %v outside %v", s.Idx, k, v, b)
			}
		}
		chillers[s.Inputs["chillers"]]++
		if v := s.Inputs["chws_sp"]; math.Abs(v*10-math.Round(v*10)) > 1e-9 {
			t.Errorf("chws_sp %v not on its 0.1 step", v)
		}
		if s.Outputs["chillers_running"] != s.Inputs["chillers"] {
			t.Errorf("sample %d runs %v chillers, asked %v", s.Idx, s.Outputs["chillers_running"], s.Inputs["chillers"])
		}
		if c := s.Outputs["capacity_ok"]; c != 0 && c != 1 {
			t.Errorf("capacity_ok %v", c)
		}
	}
	t.Logf("chiller counts sampled: %v", chillers)
	for _, n := range []float64{1, 2, 3} {
		if chillers[n] < 6 {
			t.Errorf("chillers %v sampled %d times of 24 (Latin hypercube strata)", n, chillers[n])
		}
	}
	b, _ := json.Marshal(rec.Samples[0])
	t.Logf("sample 0: %s", b)
	// one chiller cannot carry a high load: capacity is not ok
	for _, s := range rec.Samples {
		if s.Inputs["chillers"] == 1 && s.Inputs["load_frac"] > 0.7 && s.Outputs["capacity_ok"] == 1 {
			t.Errorf("sample %d: one chiller at load %.2f reported capacity ok (chws %.2f)", s.Idx, s.Inputs["load_frac"], s.Outputs["chws_temp"])
		}
	}
}

func TestLatinHypercube(t *testing.T) {
	pts := latinHypercube(newPlant(1, testLoc).rng, 50, 3)
	for j := 0; j < 3; j++ {
		seen := make([]bool, 50)
		for _, p := range pts {
			seen[int(p[j]*50)] = true
		}
		for i, ok := range seen {
			if !ok {
				t.Fatalf("dimension %d stratum %d empty", j, i)
			}
		}
	}
}

// TestCloneDeep: a clone shares no mutable state with its original.
func TestCloneDeep(t *testing.T) {
	p := designPlant(55)
	simulate(p, testStart, 30, 60, nil)
	ch := p.ByAlias["CH_01"]
	if _, err := ch.injectFault("chwst_bias", 1, testStart, 0); err != nil {
		t.Fatal(err)
	}
	ch.TripSpec = &ch.Model.Trips[0]
	simulate(p, testStart.Add(30*time.Minute), 2, 60, nil)
	snap := func(a *Asset) string {
		b, _ := json.Marshal(map[string]any{"faults": a.Faults, "ctl": a.Ctl, "cmd": a.Cmd, "smooth": a.smooth, "last": a.last, "meas": a.meas,
			"trip": a.TripSpec, "noise": len(a.noise)})
		return string(b)
	}
	before := map[string]string{}
	for _, a := range p.Assets {
		before[a.Spec.Alias] = snap(a)
	}
	c := p.Clone(99)
	cc := c.ByAlias["CH_01"]
	if cc == ch || c.sys == p.sys || c.weather == p.weather || len(c.group("chillers")) != 3 || c.group("chillers")[0] != cc {
		t.Fatal("clone shares assets, groups or weather")
	}
	// mutate everything in the clone, then run it
	cc.Faults["chwst_bias"].Value = -2
	cc.Ctl["chwst_sp"] = 7.9
	cc.Cmd["x"] = 1
	cc.smooth["chwst"] = 99
	cc.last["chwst"] = 99
	cc.TripSpec.Reason = "changed"
	for _, n := range cc.noise {
		n.x = 42
	}
	c.sys.Ctl["approach_sp"] = 6
	c.weather.db.x = 5
	if err := c.sys.applyControl("riser_dp_sp", 90); err != nil {
		t.Fatal(err)
	}
	simulate(c, testStart.Add(32*time.Minute), 60, 60, nil)
	for _, a := range p.Assets {
		if got := snap(a); got != before[a.Spec.Alias] {
			t.Errorf("%s changed by its clone", a.Spec.Alias)
		}
	}
	if p.weather.db.x == 5 {
		t.Error("weather shared")
	}
	// same seed, same state: identical clones evolve identically (the
	// environment hook is a job setting, not plant state)
	a, b := p.Clone(7), p.Clone(7)
	if a.EnvHook != nil {
		t.Error("the environment hook was cloned")
	}
	a.EnvHook, b.EnvHook = p.EnvHook, p.EnvHook
	sa, sb := simulate(a, testStart.Add(32*time.Minute), 30, 60, nil), simulate(b, testStart.Add(32*time.Minute), 30, 60, nil)
	for k := range sa {
		if sa[k].meas["CHWS_01"]["plant_kw"] != sb[k].meas["CHWS_01"]["plant_kw"] {
			t.Fatalf("clones with the same seed diverge at step %d", k)
		}
	}
	// the clone starts where the original stands (no restart transient)
	if d := math.Abs(sa[0].meas["CHWS_01"]["fan_speed_cmd"] - p.sys.meas["fan_speed_cmd"]); d > 2 {
		t.Errorf("fan command jumps %.1f %% after cloning", d)
	}
}

// TestJobSeed: a derived seed fits 53 bits and, reported as int64 in the
// summary, reproduces the job when put back into a spec.
func TestJobSeed(t *testing.T) {
	seen := map[uint64]bool{}
	for id := int64(1); id <= 1000; id++ {
		s := jobSeed(0, id)
		if s == 0 || s >= 1<<53 || seen[s] {
			t.Fatalf("job %d: seed %d", id, s)
		}
		seen[s] = true
		if again := jobSeed(int64(s), id+1); again != s {
			t.Fatalf("seed %d reported as %d does not round-trip (%d)", s, int64(s), again)
		}
	}
	if s := jobSeed(-7, 1); int64(s) != -7 {
		t.Errorf("spec seed -7 reported as %d", int64(s))
	}
}
