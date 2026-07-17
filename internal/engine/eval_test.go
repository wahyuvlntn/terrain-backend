package engine

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestParamCondition(t *testing.T) {
	p := &ParamDef{WarnType: "X > _ or X < _", WarnUpper: f(80), WarnLower: f(10), CritType: "X >= _", CritUpper: f(100)}
	cases := []struct {
		v    float64
		want string
	}{{50, CondNormal}, {80, CondNormal}, {80.1, CondWarning}, {9.9, CondWarning}, {100, CondCritical}, {150, CondCritical}}
	for _, c := range cases {
		if got := ParamCondition(p, c.v); got != c.want {
			t.Errorf("ParamCondition(%v) = %s, want %s", c.v, got, c.want)
		}
	}
	// Missing limits never trigger.
	if got := ParamCondition(&ParamDef{WarnType: "X > _"}, 1e9); got != CondNormal {
		t.Errorf("threshold without limit = %s", got)
	}
}

func TestValidThresholdType(t *testing.T) {
	for _, ok := range []string{"", "X < _", "X >= _ or X <= _"} {
		if !ValidThresholdType(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	if ValidThresholdType("X == _") {
		t.Error("X == _ must be rejected")
	}
}

func TestSPCViolations(t *testing.T) {
	p := &ParamDef{CenterLine: f(50), Sigma: f(1), USL: f(60), LSL: f(40), UCL: f(53), LCL: f(47)}
	has := func(points []float64, rule string) bool { return slices.Contains(SPCViolations(points, p), rule) }

	if !has([]float64{50, 61}, "SPC_OUT_OF_SPEC") {
		t.Error("value above USL must be out of spec")
	}
	if !has([]float64{50, 54}, "SPC_OUT_OF_CONTROL") {
		t.Error("value above UCL must be out of control")
	}
	if !has([]float64{52.5, 50, 52.5}, "SPC_2_OUT_OF_3_POINTS_ZONE_A_OR_BEYOND") {
		t.Error("2 of 3 beyond 2 sigma")
	}
	if !has([]float64{51.5, 51.5, 50, 51.5, 51.5}, "SPC_4_OUT_OF_5_POINTS_ZONE_B_OR_BEYOND") {
		t.Error("4 of 5 beyond 1 sigma")
	}
	if !has([]float64{48, 48.1, 48.2, 48.3, 48.4, 48.5, 48.6}, "SPC_7_POINTS_MONOTONIC") {
		t.Error("7 increasing points")
	}
	alt := []float64{}
	for i := 0; i < 14; i++ {
		if i%2 == 0 {
			alt = append(alt, 49.5)
		} else {
			alt = append(alt, 50.5)
		}
	}
	if !has(alt, "SPC_14_POINTS_ALTERNATING") {
		t.Error("14 alternating points")
	}
	if v := SPCViolations([]float64{50, 50.2, 49.9, 50.1}, p); len(v) != 0 {
		t.Errorf("stable process reported %v", v)
	}
}

func TestEvaluateStatusAndAlarms(t *testing.T) {
	ev := &Evaluator{Timeout: time.Second}
	m := &ModelDef{
		RunningScript:   "running.v == true",
		UnplannedScript: "fault.v > 0",
		Alarms: []AlarmDef{
			{ID: 1, Name: "E01", Criticality: 2, Script: "pressure.v > 30"},
			{ID: 2, Name: "E02", Criticality: 1, Script: "if (temp.v > 80) { return true } return false"},
		},
	}
	vals := func(running bool, fault, pressure, temp float64) map[string]Value {
		return map[string]Value{"running": {V: running, Q: true}, "fault": {V: fault, Q: true},
			"pressure": {V: pressure, Q: true}, "temp": {V: temp, Q: true}}
	}
	r := ev.Evaluate(m, nil, vals(true, 0, 10, 20), nil, false)
	if r.Status != StatusRunning || len(r.Alarms) != 0 || len(r.Errors) != 0 {
		t.Fatalf("running: %+v", r)
	}
	r = ev.Evaluate(m, nil, vals(true, 1, 35, 90), nil, false)
	if r.Status != StatusUnplannedDown {
		t.Fatalf("unplanned down has priority, got %d", r.Status)
	}
	if len(r.Alarms) != 2 || r.Condition() != CondCritical {
		t.Fatalf("alarms: %+v condition %s", r.Alarms, r.Condition())
	}
	r = ev.Evaluate(m, nil, vals(false, 0, 10, 20), nil, false)
	if r.Status != StatusIdle {
		t.Fatalf("idle expected, got %d", r.Status)
	}
	r = ev.Evaluate(m, nil, vals(true, 0, 10, 20), nil, true)
	if r.Status != StatusOffline || r.Condition() != CondOffline {
		t.Fatalf("offline expected, got %+v", r)
	}
	// Script errors are reported, not fatal.
	bad := &ModelDef{RunningScript: "unknown_variable.v > 1"}
	r = ev.Evaluate(bad, nil, map[string]Value{}, nil, false)
	if len(r.Errors) == 0 {
		t.Fatal("expected a script error")
	}
}

func TestAlarmCodeResolve(t *testing.T) {
	ix := &AlarmCodeIndex{byKey: map[string]int64{}, Info: map[int64]AlarmCode{}}
	for _, c := range []AlarmCode{{1, "E01", "High Pressure"}, {2, "E02", "Low Refrigerant"}} {
		ix.Info[c.ID] = c
		ix.byKey[lower(c.Code)] = c.ID
		ix.byKey[lower(c.Code+"-"+c.Name)] = c.ID
		ix.byKey[lower(c.Name)] = c.ID
	}
	for in, want := range map[string]int64{"E01": 1, "e01": 1, "E01 - anything": 1, "E02: refrigerant": 2, "Low Refrigerant": 2} {
		if got, ok := ix.Resolve(in); !ok || got != want {
			t.Errorf("Resolve(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := ix.Resolve("E99"); ok {
		t.Error("unknown code resolved")
	}
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
