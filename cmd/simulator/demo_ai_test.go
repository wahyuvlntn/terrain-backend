package main

import (
	"testing"
	"time"
)

func TestShiftTimes(t *testing.T) {
	v := map[string]any{"daily": []any{map[string]any{"day": "2026-10-15", "outs": 3.0}},
		"events": []any{map[string]any{"from": "2026-10-15T02:00:00Z", "note": "PM"}}, "reason": "2026 plan"}
	got := shiftTimes(v, -26*time.Hour, -1).(map[string]any)
	if d := got["daily"].([]any)[0].(map[string]any)["day"]; d != "2026-10-14" {
		t.Errorf("day %v", d)
	}
	if f := got["events"].([]any)[0].(map[string]any)["from"]; f != "2026-10-14T00:00:00Z" {
		t.Errorf("from %v", f)
	}
	if got["reason"] != "2026 plan" {
		t.Errorf("reason %v", got["reason"])
	}
}

func TestPlantOff(t *testing.T) {
	for _, c := range []struct {
		p   map[string]float64
		off bool
	}{
		{map[string]float64{"chillers_running": 2, "cooling_load": 4800, "plant_kw": 910}, false},
		{map[string]float64{}, false}, // no samples: plantAt's defaults apply
		{map[string]float64{"chillers_running": 0, "cooling_load": 0, "plant_kw": 0}, true}, // every chiller in maintenance
		{map[string]float64{"chillers_running": 1, "cooling_load": 0}, true},
		{map[string]float64{"chillers_running": 2, "plant_kw": 0.4}, true},
	} {
		if got := plantOff(c.p); got != c.off {
			t.Errorf("plantOff(%v) = %v, want %v", c.p, got, c.off)
		}
	}
}

func TestDiagnosesExtendModels(t *testing.T) {
	for _, d := range afddDiags {
		found := false
		for _, m := range models() {
			if m.IsBase || !contains(d.Keys, m.Key) {
				continue
			}
			found = true
			repaired := false
			for _, f := range m.Faults {
				repaired = repaired || (f.Kind == d.Fault && contains(f.Codes, d.Code))
			}
			if !repaired {
				t.Errorf("%s on %s: fault %s does not list it as a repair code", d.Code, m.Name, d.Fault)
			}
			params := map[string]bool{}
			for _, p := range m.Params {
				params[p.Alias] = true
			}
			for _, path := range d.Paths {
				for _, in := range path {
					if in[:2] == "P:" && !params[in[2:]] {
						t.Errorf("%s on %s: unknown parameter %s", d.Code, m.Name, in)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no model with key %v", d.Code, d.Keys)
		}
		if codeName(d.Code) == d.Code {
			t.Errorf("%s has no name", d.Code)
		}
	}
}
