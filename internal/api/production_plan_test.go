package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func planTree(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParsePlanSpec(t *testing.T) {
	aliases := map[string]string{"scn_01": "SCN_01", "etch_02": "ETCH_02"}
	products := []string{"MCU40", "WSOC40"}
	spec, err := parsePlanSpec(planTree(t, `{"horizon_days": 30, "plan": {"starts_per_day": 5, "mix": {"MCU40": 0.7, "WSOC40": 0.3}},
		"variants": [{"name": "Baseline"}, {"name": "Second scanner", "actions": [{"kind": "add_tool", "asset": "scn_01"}]},
		{"name": "PM", "plan": {"dispatch": "cr", "vehicles": 6}, "actions": [{"kind": "tool_down", "asset": "ETCH_02", "at_day": 3, "duration_h": 12}]}]}`),
		aliases, products)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "production" || spec.Initial != "live" || !spec.Breakdowns || len(spec.Variants) != 3 {
		t.Fatalf("normalised spec %+v", spec)
	}
	if a := spec.Variants[1].Actions[0]; a.Asset != "SCN_01" || a.Count != 1 {
		t.Errorf("add_tool %+v", a)
	}
	if spec.Variants[2].Plan.Dispatch != "CR" || *spec.Variants[2].Plan.Vehicles != 6 || spec.Variants[2].Actions[0].DurationH != 12 {
		t.Errorf("variant plan %+v", spec.Variants[2])
	}
	if *spec.Plan.StartsPerDay != 5 || spec.Plan.Mix["MCU40"] != 0.7 {
		t.Errorf("plan %+v", spec.Plan)
	}
	// a plan without variants runs one
	spec, err = parsePlanSpec(planTree(t, `{"horizon_days": "7"}`), aliases, products)
	if err != nil || len(spec.Variants) != 1 || spec.Variants[0].Name != "Plan" {
		t.Fatalf("default variant: %v %+v", err, spec)
	}
	for _, bad := range []string{
		`{"horizon_days": 0}`,
		`{"horizon_days": 120}`,
		`{"horizon_days": 10, "kind": "scenario"}`,
		`{"horizon_days": 10, "plan": {"starts_per_day": 20}}`,
		`{"horizon_days": 10, "plan": {"mix": {"N28": 1}}}`,
		`{"horizon_days": 10, "plan": {"mix": {"MCU40": 0}}}`,
		`{"horizon_days": 10, "plan": {"dispatch": "LIFO"}}`,
		`{"horizon_days": 10, "plan": {"furnace_min_lots": 2.5}}`,
		`{"horizon_days": 10, "plan": {"vehicles": 0}}`,
		`{"horizon_days": 10, "plan": {"vehicles": 4.5}}`,
		`{"horizon_days": 10, "actions": [{"kind": "tool_down", "asset": "CH_01", "at_day": 1, "duration_h": 4}]}`,
		`{"horizon_days": 10, "actions": [{"kind": "tool_down", "asset": "SCN_01", "at_day": 11, "duration_h": 4}]}`,
		`{"horizon_days": 10, "actions": [{"kind": "add_tool", "asset": "SCN_01", "count": 4}]}`,
		`{"horizon_days": 10, "actions": [{"kind": "explode", "asset": "SCN_01"}]}`,
		`{"horizon_days": 10, "variants": [{"name": "A"}, {"name": "a"}]}`,
		`{"horizon_days": 10, "variants": [{"name": "A"}, {"name": "B"}, {"name": "C"}, {"name": "D"}]}`,
		`{"horizon_days": 10, "start": "2026-10-14T00:00:00Z"}`,
	} {
		if _, err := parsePlanSpec(planTree(t, bad), aliases, products); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
