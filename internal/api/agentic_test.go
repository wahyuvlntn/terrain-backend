package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"afdd/internal/auth"
	"afdd/internal/config"
	"afdd/internal/httpx"
)

func fp(v float64) *float64 { return &v }
func sp(v string) *string   { return &v }

// ---------------------------------------------------------------------------
// Human-only guard
// ---------------------------------------------------------------------------

func TestRequireHuman(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header map[string]string
		want   bool // refused
	}{
		{"browser", nil, false},
		{"agent", map[string]string{"X-AFDD-Origin": "agent"}, true},
		{"lower-case header", map[string]string{"x-afdd-origin": "agent"}, true},
		{"empty value", map[string]string{"X-AFDD-Origin": ""}, true},
		{"other header", map[string]string{"X-Agent-Secret": "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			for k, v := range tc.header {
				r.Header[http.CanonicalHeaderKey(k)] = []string{v}
			}
			err := (&Ctx{R: r}).RequireHuman()
			if (err != nil) != tc.want {
				t.Fatalf("RequireHuman() = %v, refused want %v", err, tc.want)
			}
			if err != nil {
				var he *httpx.Error
				if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Msg != "This action needs a person: approve it in the application" {
					t.Fatalf("unexpected error %#v", err)
				}
			}
		})
	}
	if (&Ctx{}).RequireHuman() != nil {
		t.Fatal("a context without request must pass")
	}
}

// ---------------------------------------------------------------------------
// Simulation spec validation
// ---------------------------------------------------------------------------

func testCatalog() *simCatalog {
	modeOpts := func(labels ...string) []twinControlOption {
		var out []twinControlOption
		for i, l := range labels {
			out = append(out, twinControlOption{Value: float64(i), Label: l})
		}
		return out
	}
	ctl := func(min, max, step float64, mode string) *simControl {
		c := &simControl{twinControl: twinControl{Min: fp(min), Max: fp(max), Step: fp(step)}, Unit: "°C"}
		if mode != "" {
			c.Mode = sp(mode)
		}
		return c
	}
	chws := &simAssetRef{Alias: "CHWS_01", TypeName: "Chilled Water System", Visible: true, Controls: map[string]*simControl{
		"chws_reset_mode": {twinControl: twinControl{Options: modeOpts("Reset", "Manual")}, Name: "CHW Set Point Mode", Live: fp(0)},
		"chws_sp":         ctl(5.5, 8.0, 0.1, "chws_reset_mode"),
		"approach_sp":     ctl(2.0, 8.0, 0.1, ""),
	}}
	ct := &simAssetRef{Alias: "CT_01", TypeName: "Cooling Tower", Visible: true, Controls: map[string]*simControl{
		"fan_mode":     {twinControl: twinControl{Options: modeOpts("Auto", "Manual")}, Name: "Fan Mode", Live: fp(1)},
		"fan_speed_sp": ctl(30, 100, 1, "fan_mode"),
	}}
	return &simCatalog{Assets: map[string]*simAssetRef{
		"chws_01": chws,
		"ct_01":   ct,
		"ch_01":   {Alias: "CH_01", TypeName: "Centrifugal Chiller", Visible: true, Controls: map[string]*simControl{}},
		"mau_01":  {Alias: "MAU_01", TypeName: "Make-up Air Unit", Visible: false, Controls: map[string]*simControl{}},
	}}
}

func decodeTree(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("bad test JSON %s: %v", s, err)
	}
	return v
}

func TestParseSimSpec(t *testing.T) {
	now := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	cat := testCatalog()
	cases := []struct {
		name string
		spec string
		err  string // substring of the expected error, "" = valid
	}{
		{"minimal", `{"duration_min": 60}`, ""},
		{"multipart strings", `{"duration_min": "120", "stochastic": "true", "seed": "42", "load_scale": "1.1",
			"weather": {"dry_bulb_offset": "1.5"}, "actions": [{"at_min": "0", "kind": "control", "asset": "CHWS_01",
			"param": "approach_sp", "value": "3.0"}]}`, ""},
		{"full", `{"start": "2026-10-11T06:00:00Z", "duration_min": 240, "initial": "nominal", "seed": 0,
			"weather": {"dry_bulb_offset": 0, "wet_bulb_offset": 0.5}, "load_scale": 1.0,
			"actions": [{"at_min": 0, "kind": "fault", "asset": "CT_01", "fault": "ct_fouling", "value": 0.8}],
			"variants": [{"name": "baseline", "actions": []},
				{"name": "recommended", "actions": [{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1},
					{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7.2}]}]}`, ""},
		{"start just now", `{"start": "2026-10-10T05:57:00Z", "duration_min": 60}`, ""},
		{"start in past", `{"start": "2026-10-10T05:50:00Z", "duration_min": 60}`, "in the past"},
		{"start too far", `{"start": "2026-10-25T06:00:00Z", "duration_min": 60}`, "within 14 days"},
		{"bad start", `{"start": "tomorrow", "duration_min": 60}`, "Invalid scenario start"},
		{"no duration", `{}`, "duration_min is required"},
		{"short", `{"duration_min": 5}`, "10 to 2880"},
		{"long", `{"duration_min": 3000}`, "10 to 2880"},
		{"fraction", `{"duration_min": 30.5}`, "whole number"},
		{"unknown field", `{"duration_min": 60, "duration": 60}`, `Unknown field "duration"`},
		{"doe", `{"kind": "doe", "duration_min": 60}`, "Only scenario"},
		{"initial", `{"duration_min": 60, "initial": "warm"}`, "live or nominal"},
		{"seed negative", `{"duration_min": 60, "seed": -1}`, "seed"},
		{"seed fraction", `{"duration_min": 60, "seed": 1.5}`, "whole number"},
		{"load scale", `{"duration_min": 60, "load_scale": 1.5}`, "load_scale"},
		{"weather", `{"duration_min": 60, "weather": {"wet_bulb_offset": 12}}`, "wet_bulb_offset"},
		{"weather field", `{"duration_min": 60, "weather": {"rh": 1}}`, `Unknown field "rh"`},
		{"too many variants", `{"duration_min": 60, "variants": [{"name": "a"}, {"name": "b"}, {"name": "c"}, {"name": "d"}]}`, "At most 3 variants"},
		{"duplicate variant", `{"duration_min": 60, "variants": [{"name": "a"}, {"name": "A"}]}`, "unique"},
		{"bad variant name", `{"duration_min": 60, "variants": [{"name": "a/b"}]}`, "needs a name"},
		{"unknown asset", `{"duration_min": 60, "actions": [{"kind": "trip", "asset": "CH_09"}]}`, "unknown equipment"},
		{"invisible asset", `{"duration_min": 60, "actions": [{"kind": "trip", "asset": "MAU_01"}]}`, "unknown equipment"},
		{"unknown kind", `{"duration_min": 60, "actions": [{"kind": "explode", "asset": "CH_01"}]}`, "unknown kind"},
		{"action field", `{"duration_min": 60, "actions": [{"kind": "trip", "asset": "CH_01", "when": 3}]}`, `Unknown field "when"`},
		{"at end", `{"duration_min": 60, "actions": [{"at_min": 60, "kind": "trip", "asset": "CH_01"}]}`, "within the run"},
		{"action duration", `{"duration_min": 60, "actions": [{"kind": "offline", "asset": "CH_01", "duration_min": 0}]}`, "duration_min must be"},
		{"not writable", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "chws_temp", "value": 7}]}`, "not a writable parameter"},
		{"no value", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "approach_sp"}]}`, "needs a value"},
		{"out of range", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 9}]}`, "outside the allowed range"},
		{"off step", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "approach_sp", "value": 3.05}]}`, "steps of"},
		{"bad option", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CT_01", "param": "fan_mode", "value": 2}]}`, "not a valid option"},
		{"gated nominal", `{"duration_min": 60, "initial": "nominal", "actions": [{"kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 80}]}`, "in automatic"},
		{"gated live auto", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7}]}`, "CHW Set Point Mode is in automatic"},
		{"gated live manual", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 80}]}`, ""},
		{"gated mode later", `{"duration_min": 60, "actions": [{"at_min": 10, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1},
			{"at_min": 5, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7}]}`, "in automatic"},
		{"gated mode back to auto", `{"duration_min": 60, "actions": [{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1},
			{"at_min": 5, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 0},
			{"at_min": 9, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7}]}`, "in automatic"},
		{"gated mode in common", `{"duration_min": 60, "actions": [{"kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1}],
			"variants": [{"name": "v", "actions": [{"at_min": 1, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 6.0}]}]}`, ""},
		{"gated same minute set point first", `{"duration_min": 60, "initial": "nominal", "actions": [
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7},
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1}]}`, ""},
		{"gated within the mode's duration", `{"duration_min": 60, "actions": [
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1, "duration_min": 10},
			{"at_min": 9, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7, "duration_min": 30}]}`, ""},
		{"gated after the mode's duration", `{"duration_min": 60, "actions": [
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1, "duration_min": 10},
			{"at_min": 10, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7}]}`, "in automatic"},
		{"gated mode switched again at its end", `{"duration_min": 60, "actions": [
			{"at_min": 0, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1, "duration_min": 10},
			{"at_min": 10, "kind": "control", "asset": "CHWS_01", "param": "chws_reset_mode", "value": 1}],
			"variants": [{"name": "v", "actions": [{"at_min": 10, "kind": "control", "asset": "CHWS_01", "param": "chws_sp", "value": 7}]}]}`, ""},
		{"gated live mode restored", `{"duration_min": 60, "actions": [
			{"at_min": 0, "kind": "control", "asset": "CT_01", "param": "fan_mode", "value": 0, "duration_min": 5},
			{"at_min": 5, "kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 80}]}`, ""},
		{"gated live mode in auto for a while", `{"duration_min": 60, "actions": [
			{"at_min": 0, "kind": "control", "asset": "CT_01", "param": "fan_mode", "value": 0, "duration_min": 5},
			{"at_min": 4, "kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 80}]}`, "Fan Mode is in automatic"},
		{"unknown fault", `{"duration_min": 60, "actions": [{"kind": "fault", "asset": "CH_01", "fault": "melt", "value": 1}]}`, "unknown fault type"},
		{"fault wrong asset", `{"duration_min": 60, "actions": [{"kind": "fault", "asset": "CH_01", "fault": "ct_fouling", "value": 0.8}]}`, "does not apply to CH_01"},
		{"fault intensity", `{"duration_min": 60, "actions": [{"kind": "fault", "asset": "CT_01", "fault": "ct_fouling", "value": 0.5}]}`, "not a documented intensity"},
		{"fault no value", `{"duration_min": 60, "actions": [{"kind": "fault", "asset": "CHWS_01", "fault": "twv_leak"}]}`, "needs an intensity"},
		{"clear fault", `{"duration_min": 60, "actions": [{"kind": "clear_fault", "asset": "CHWS_01", "fault": "twv_leak"}]}`, ""},
		{"trip controller", `{"duration_min": 60, "actions": [{"kind": "trip", "asset": "CHWS_01"}]}`, "plant controller"},
		{"actions not array", `{"duration_min": 60, "actions": {"kind": "trip"}}`, "must be an array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseSimSpec(decodeTree(t, tc.spec), now, time.UTC, cat)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if spec == nil || len(spec.Variants) == 0 {
					t.Fatal("a valid spec has at least one variant")
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.err)
			}
			var he *httpx.Error
			if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
				t.Fatalf("expected a 400 error, got %#v", err)
			}
			if !strings.Contains(he.Msg, tc.err) {
				t.Fatalf("error %q does not contain %q", he.Msg, tc.err)
			}
			if strings.Contains(he.Msg, ":") {
				t.Fatalf("user-facing message %q must not contain ':'", he.Msg)
			}
		})
	}

	// Too many actions.
	var acts []string
	for i := 0; i < simMaxActions+1; i++ {
		acts = append(acts, `{"kind": "release", "asset": "CH_01"}`)
	}
	if _, err := parseSimSpec(decodeTree(t, `{"duration_min": 60, "actions": [`+strings.Join(acts, ",")+`]}`), now, time.UTC, cat); err == nil ||
		!strings.Contains(err.Error(), "At most 200 actions") {
		t.Fatalf("201 actions: %v", err)
	}
}

func TestParseSimSpecNormalises(t *testing.T) {
	now := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	spec, err := parseSimSpec(decodeTree(t, `{"start": "2026-10-10T14:30:00+08:00", "duration_min": "90", "actions": [
		{"at_min": 3, "kind": "CONTROL", "asset": "chws_01", "param": "APPROACH_SP", "value": 4.0, "label": "Approach"},
		{"kind": "fault", "asset": "ct_01", "fault": "CWST_BIAS", "value": "-1"},
		{"kind": "clear_fault", "asset": "CT_01", "fault": "cwst_bias", "value": 5}]}`), now, time.UTC, testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Start == nil || *spec.Start != "2026-10-10T06:30:00.000Z" {
		t.Errorf("start = %v", spec.Start)
	}
	if spec.DurationMin != 90 || spec.Initial != "live" || spec.LoadScale != 1 || spec.Stochastic {
		t.Errorf("defaults not applied: %+v", spec)
	}
	if len(spec.Variants) != 1 || spec.Variants[0].Name != "baseline" || spec.Variants[0].Actions == nil {
		t.Errorf("default variant: %+v", spec.Variants)
	}
	a := spec.Actions
	if a[0].Kind != "control" || a[0].Asset != "CHWS_01" || a[0].Param != "approach_sp" || a[0].AtMin != 3 || *a[0].Value != 4 {
		t.Errorf("control action not normalised: %+v", a[0])
	}
	if a[1].Asset != "CT_01" || a[1].Fault != "cwst_bias" || *a[1].Value != -1 {
		t.Errorf("fault action not normalised: %+v", a[1])
	}
	if a[2].Value != nil {
		t.Errorf("clear_fault keeps no value: %+v", a[2])
	}
	b, _ := json.Marshal(spec)
	for _, want := range []string{`"start":"2026-10-10T06:30:00.000Z"`, `"duration_min":90`, `"variants":[{"name":"baseline","actions":[]}]`,
		`"weather":{"dry_bulb_offset":0,"wet_bulb_offset":0}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("stored spec %s lacks %s", b, want)
		}
	}
	if strings.Contains(string(b), "label") {
		t.Errorf("descriptive fields must be dropped: %s", b)
	}
}

// ---------------------------------------------------------------------------
// Job state machine
// ---------------------------------------------------------------------------

func TestSimTransition(t *testing.T) {
	for _, tc := range []struct {
		action, from, to string
		ok               bool
	}{
		{"approve", simProposed, simQueued, true},
		{"approve", simQueued, "", false},
		{"approve", simRejected, "", false},
		{"reject", simProposed, simRejected, true},
		{"reject", simQueued, "", false},
		{"cancel", simProposed, simCancelled, true},
		{"cancel", simQueued, simCancelled, true},
		{"cancel", simRunning, "", false},
		{"cancel", simAnalyzing, "", false},
		{"cancel", simSucceeded, "", false},
		{"cancel", simCancelled, "", false},
		{"run", simQueued, "", false},
	} {
		to, ok := simTransition(tc.action, tc.from)
		if ok != tc.ok || to != tc.to {
			t.Errorf("%s from %s = (%q, %v), want (%q, %v)", tc.action, tc.from, to, ok, tc.to, tc.ok)
		}
	}
	// Every target is a known status and terminal for the API.
	for action, tr := range simTransitions {
		if !contains(simStatuses, tr.to) {
			t.Errorf("%s leads to unknown status %s", action, tr.to)
		}
		if _, ok := simDone[action]; !ok {
			t.Errorf("%s has no message verb", action)
		}
	}
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

func f32(v float32) *float32 { return &v }

func TestAssembleFrames(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	nan := float32(math.NaN())
	rows := []simSeriesRow{
		{Asset: "CHWS_01", Param: "chws_temp", T0: t0, StepS: 60, Measured: []*float32{f32(6.5), f32(6.6), nil, f32(6.7)}},
		{Asset: "CHWS_01", Param: "riser_dp", T0: t0, StepS: 60, Measured: []*float32{f32(120), f32(nan)}},
		{Asset: "CT_01", Param: "cwst", T0: t0.Add(2 * time.Minute), StepS: 60, Measured: []*float32{f32(29.1), f32(29.2), f32(29.3)}},
		{Asset: "CH_01", Param: "chwst", T0: t0, StepS: 120, Measured: []*float32{f32(6.1), f32(6.2)}},
	}
	start, step, steps, series := assembleFrames(rows, 60)
	if !start.Equal(t0) || step != 60 || steps != 5 {
		t.Fatalf("t0 %v step %d steps %d", start, step, steps)
	}
	vals := func(a, p string) []any {
		var out []any
		for _, v := range series[a][p] {
			if v == nil {
				out = append(out, nil)
			} else {
				out = append(out, float64(*v))
			}
		}
		return out
	}
	check := func(a, p string, want ...any) {
		t.Helper()
		got := vals(a, p)
		if len(got) != len(want) {
			t.Fatalf("%s.%s = %v, want %v", a, p, got, want)
		}
		for i := range want {
			if want[i] == nil && got[i] != nil || want[i] != nil && (got[i] == nil || math.Abs(got[i].(float64)-want[i].(float64)) > 1e-5) {
				t.Fatalf("%s.%s = %v, want %v", a, p, got, want)
			}
		}
	}
	check("CHWS_01", "chws_temp", 6.5, 6.6, nil, 6.7, nil)
	check("CHWS_01", "riser_dp", 120.0, nil, nil, nil, nil)
	check("CT_01", "cwst", nil, nil, 29.1, 29.2, 29.3)
	check("CH_01", "chwst", 6.1, nil, 6.2, nil, nil)

	// JSON: float32 values keep their short form, missing ones are null.
	b, err := json.Marshal(series["CHWS_01"]["chws_temp"])
	if err != nil || string(b) != `[6.5,6.6,null,6.7,null]` {
		t.Fatalf("json %s %v", b, err)
	}

	if _, step, steps, series := assembleFrames(nil, 60); step != 60 || steps != 0 || len(series) != 0 {
		t.Fatal("no rows gives an empty frame set")
	}
}

func TestRunningFlagsFromSimStates(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	var pts []point
	for i := 0; i < 6; i++ {
		v := float64(i)
		pts = append(pts, point{ts: t0.Add(time.Duration(i) * time.Minute), val: &v})
	}
	end := t0.Add(3 * time.Minute)
	spans := simStatusSpans([]string{"RUNNING", "3"}, []time.Time{t0, end}, []*time.Time{&end, nil})
	got := runningFlags(pts, 0, spans)
	want := []bool{true, true, true, false, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flags %v, want %v", got, want)
		}
	}
	// No status interval: running throughout.
	for _, f := range runningFlags(pts, 0, nil) {
		if !f {
			t.Fatal("no status intervals means running")
		}
	}
}

func TestBucketPoints(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var pts []point
	for i := 0; i < 1440; i++ {
		v := float64(i % 10)
		pts = append(pts, point{ts: t0.Add(time.Duration(i) * time.Minute), val: &v})
	}
	out, bucket := bucketPoints(pts, t0, t0.Add(24*time.Hour), 360)
	if bucket != 4*time.Minute || len(out) != 360 {
		t.Fatalf("bucket %v points %d", bucket, len(out))
	}
	if *out[0].val != 1.5 { // mean of 0,1,2,3
		t.Fatalf("first bucket mean %v", *out[0].val)
	}
	if few, b := bucketPoints(pts[:100], t0, t0.Add(100*time.Minute), 360); b != 0 || len(few) != 100 {
		t.Fatal("short series are returned as they are")
	}
}

// ---------------------------------------------------------------------------
// Fault catalog, recommendations, rules
// ---------------------------------------------------------------------------

func TestFaultCatalog(t *testing.T) {
	codes := map[string]string{"chwst_bias": "CHL-011", "cwst_bias": "CT-013", "ct_fouling": "CT-014", "riser_dp_bias": "CHS-005",
		"twv_leak": "CHS-006", "twv_stuck": "CHS-007", "cw_pi_tuning": "CHS-008"}
	if len(twinFaultCatalog) != len(codes) {
		t.Fatalf("catalog has %d kinds", len(twinFaultCatalog))
	}
	for kind, code := range codes {
		f := twinFault(strings.ToUpper(kind))
		if f == nil || f.Code != code || len(f.Assets) == 0 || len(f.Intensities) == 0 {
			t.Fatalf("%s: %+v", kind, f)
		}
	}
	f := twinFault("riser_dp_bias")
	if v, ok := f.intensity(-0.1); !ok || v != -0.10 {
		t.Error("documented intensity not accepted")
	}
	if _, ok := f.intensity(0.15); ok {
		t.Error("undocumented intensity accepted")
	}
	if a, ok := twinFault("ct_fouling").asset("ct_02"); !ok || a != "CT_02" {
		t.Error("asset match must be case-insensitive and canonical")
	}
	vis := visibleFaultCatalog(func(a string) bool { return strings.HasPrefix(a, "CT_0") && a != "CT_03" })
	if len(vis) != 2 || len(vis[0].Assets) != 2 || len(twinFault("cwst_bias").Assets) != 3 {
		t.Fatalf("visible catalog %+v (the static catalog must not change)", vis)
	}
}

func TestOrderRecActions(t *testing.T) {
	acts := []recAction{
		{Asset: "CHWS_01", Param: "chws_sp"}, {Asset: "CHWS_01", Param: "approach_sp"},
		{Asset: "CHWS_01", Param: "chws_reset_mode"}, {Asset: "CHWS_01", Param: "riser_dp_sp"}, {Asset: "CHWS_01", Param: "staging_mode"},
	}
	modes := map[string]bool{"chws_reset_mode": true, "staging_mode": true}
	got := orderRecActions(acts, func(a recAction) bool { return modes[a.Param] })
	var names []string
	for _, a := range got {
		names = append(names, a.Param)
	}
	want := "chws_reset_mode,staging_mode,chws_sp,approach_sp,riser_dp_sp"
	if strings.Join(names, ",") != want {
		t.Fatalf("order %v, want %s", names, want)
	}
	if acts[0].Param != "chws_sp" {
		t.Fatal("the input must not be reordered in place")
	}
}

// TestApplyRecWrites: a write the plant does not confirm stops the apply and
// the modes it changed are written back newest first; modes already in the
// requested state are left alone.
func TestApplyRecWrites(t *testing.T) {
	plant := map[string]float64{"chws_reset_mode": 0, "staging_mode": 1, "chws_sp": 6.5, "chillers_sp": 2}
	refuse := map[string]bool{}
	var sent []string
	write := func(a recAction) map[string]any {
		old := plant[a.Param]
		sent = append(sent, fmt.Sprintf("%s=%v", a.Param, *a.Value))
		row := map[string]any{"alias": a.Param, "name": "Name of " + a.Param, "old_value": &old, "value": *a.Value, "status": "APPLIED"}
		if refuse[a.Param] {
			row["status"] = "TIMEOUT"
		} else {
			plant[a.Param] = *a.Value
		}
		return row
	}
	isMode := func(a recAction) bool { return strings.HasSuffix(a.Param, "_mode") }
	ordered := []recAction{{Asset: "CHWS_01", Param: "chws_reset_mode", Value: fp(1)}, {Asset: "CHWS_01", Param: "staging_mode", Value: fp(1)},
		{Asset: "CHWS_01", Param: "chws_sp", Value: fp(7)}, {Asset: "CHWS_01", Param: "chillers_sp", Value: fp(3)}}

	results, restores, ok := applyRecWrites(ordered, isMode, write)
	if !ok || len(results) != 4 || len(restores) != 0 || plant["chws_reset_mode"] != 1 {
		t.Fatalf("full apply ok %v results %d restores %v plant %v", ok, len(results), restores, plant)
	}

	plant = map[string]float64{"chws_reset_mode": 0, "staging_mode": 1, "chws_sp": 6.5, "chillers_sp": 2}
	refuse["chillers_sp"], sent = true, nil
	results, restores, ok = applyRecWrites(ordered, isMode, write)
	// staging_mode was already manual: only the reset mode goes back to automatic
	if ok || len(results) != 4 || len(restores) != 1 || strings.Join(sent, ",") != "chws_reset_mode=1,staging_mode=1,chws_sp=7,chillers_sp=3,chws_reset_mode=0" {
		t.Fatalf("partial apply ok %v results %d restores %d writes %v", ok, len(results), len(restores), sent)
	}
	if r := restores[0]; r["restore"] != true || r["status"] != "APPLIED" || r["alias"] != "chws_reset_mode" || plant["chws_reset_mode"] != 0 {
		t.Fatalf("restore %v plant %v", r, plant)
	}
	if results[0]["restored"] != true || results[1]["restored"] != nil {
		t.Fatalf("restored flags %v / %v", results[0]["restored"], results[1]["restored"])
	}

	// two changed modes are restored newest first; a refused restore is reported
	plant = map[string]float64{"chws_reset_mode": 0, "staging_mode": 0, "chws_sp": 6.5, "chillers_sp": 2}
	refuse = map[string]bool{"chws_sp": true}
	sent = nil
	results, restores, ok = applyRecWrites(ordered, isMode, write)
	if ok || len(results) != 3 || strings.Join(sent, ",") != "chws_reset_mode=1,staging_mode=1,chws_sp=7,staging_mode=0,chws_reset_mode=0" {
		t.Fatalf("writes %v", sent)
	}
	if len(restores) != 2 || restores[0]["alias"] != "staging_mode" || restores[1]["alias"] != "chws_reset_mode" {
		t.Fatalf("restores %v", restores)
	}

	// the first write fails: nothing to restore
	refuse = map[string]bool{"chws_reset_mode": true}
	results, restores, ok = applyRecWrites(ordered, isMode, write)
	if ok || len(results) != 1 || len(restores) != 0 {
		t.Fatalf("first write refused: results %d restores %d", len(results), len(restores))
	}
}

func TestSimCancelAllowed(t *testing.T) {
	owner, other := int64(14), int64(15)
	basic := &auth.User{ID: owner, Role: "basic"}
	admin := &auth.User{ID: 2, Role: auth.RoleAdmin}
	for _, tc := range []struct {
		name   string
		user   *auth.User
		origin string
		by     *int64
		ok     bool
	}{
		{"own job", basic, "ui", &owner, true},
		{"own proposal", basic, "agent", &owner, true},
		{"someone else's", basic, "ui", &other, false},
		{"no requester", basic, "ui", nil, false},
		{"optimizer job", basic, "optimizer", &owner, false},
		{"analytics job", basic, "analytics", nil, false},
		{"admin, someone else's", admin, "ui", &other, true},
		{"admin, analytics job", admin, "analytics", nil, true},
		{"superuser", &auth.User{ID: 1, Role: auth.RoleSuperuser}, "optimizer", nil, true},
	} {
		err := simCancelAllowed(tc.user, tc.origin, tc.by)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		var he *httpx.Error
		if err != nil && (!errors.As(err, &he) || he.Status != http.StatusForbidden) {
			t.Errorf("%s: want 403, got %#v", tc.name, err)
		}
	}
}

func TestRuleParamsUpdate(t *testing.T) {
	defaults := decodeTree(t, `{"threshold_k": 0.5, "window_min": 30, "use_header": true, "mode": "ewma", "weights": [1, 2]}`).(map[string]any)
	got, err := ruleParamsUpdate(decodeTree(t, `{"threshold_k": "0.7", "use_header": "false", "mode": "raw", "weights": [3]}`), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got["threshold_k"] != 0.7 || got["use_header"] != false || got["mode"] != "raw" {
		t.Fatalf("converted %v", got)
	}
	for _, bad := range []string{`{"unknown": 1}`, `{"window_min": "abc"}`, `{"use_header": "maybe"}`, `{"weights": 3}`, `[1]`} {
		if _, err := ruleParamsUpdate(decodeTree(t, bad), defaults); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestOptimizerPeriod(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	if p := optimizerPeriod([]time.Time{t0, t0.Add(-90 * time.Minute)}); p != 90*time.Minute {
		t.Errorf("period %v", p)
	}
	if p := optimizerPeriod([]time.Time{t0, t0.Add(-5 * time.Hour)}); p != time.Hour {
		t.Errorf("gap outside 60..120 must fall back to 60 min, got %v", p)
	}
	if p := optimizerPeriod(nil); p != time.Hour {
		t.Errorf("default %v", p)
	}
}

func TestDocHelpers(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me\Chiller Manual.PDF`: "Chiller Manual.PDF",
		"../../etc/passwd":               "passwd.pdf",
		"report\x00\"x\".pdf":            "reportx.pdf",
		"":                               "document.pdf",
	} {
		if got := docFileName(in); got != want {
			t.Errorf("docFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if n := len([]rune(docFileName(strings.Repeat("a", 500) + ".pdf"))); n != docMaxName {
		t.Errorf("long names are cut to %d, got %d", docMaxName, n)
	}
	u := newUUID()
	if len(u) != 36 || u[14] != '4' || !strings.ContainsRune("89ab", rune(u[19])) || u == newUUID() {
		t.Errorf("uuid %s", u)
	}
}

// ---------------------------------------------------------------------------
// Agent proxy
// ---------------------------------------------------------------------------

func TestAgentProxyHeadersAndStreaming(t *testing.T) {
	type seen struct {
		path, query string
		header      http.Header
		body        string
	}
	got := make(chan seen, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: session\ndata: {\"session_id\":\"s1\"}\n\n")
		w.(http.Flusher).Flush()
		<-release // the first event must reach the client before the stream ends
		_, _ = io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer upstream.Close()

	s := &Server{cfg: &config.Config{AgentURL: upstream.URL, AgentSecret: "s3cret"}}
	admin := &auth.User{ID: 7, Username: "sarah_lim", FullName: "Sarah Lim", Role: auth.RoleAdmin}
	id := agentIdentity{UserID: 7, Username: "sarah_lim", FullName: "Sarah Lim\r\n", Token: "real-token", Permissions: []string{"admin"}}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.proxyAgent(w, r, admin, id) }))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/api/agent/chat?x=1", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.SessionHeader, "real-token")
	req.Header.Set("X-AFDD-Origin", "agent")
	req.Header.Set("X-Agent-Secret", "forged")
	req.Header.Set("X-AFDD-User-Id", "1")
	req.Header.Set("X-Afdd-Permissions", "admin")
	req.Header.Set("Authorization", "Bearer forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "event: session\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first SSE event was not flushed through the proxy")
	}
	close(release)

	up := <-got
	if up.path != "/chat" || up.query != "x=1" || up.body != `{"message":"hi"}` {
		t.Fatalf("upstream request %s?%s %q", up.path, up.query, up.body)
	}
	h := up.header
	for k, want := range map[string]string{
		"X-Agent-Secret": "s3cret", "X-Afdd-User-Id": "7", "X-Afdd-Username": "sarah_lim", "X-Afdd-User-Name": "Sarah Lim",
		"X-Afdd-Session": "real-token", "X-Afdd-Permissions": "admin",
	} {
		if v := h.Values(k); len(v) != 1 || v[0] != want {
			t.Errorf("%s = %v, want %q", k, v, want)
		}
	}
	for _, k := range []string{"X-Afdd-Origin", auth.SessionHeader, "Authorization"} {
		if h.Get(k) != "" {
			t.Errorf("client header %s reached the agent: %q", k, h.Get(k))
		}
	}
}

func TestAgentProxyRefusals(t *testing.T) {
	basic := &auth.User{ID: 9, Username: "ahmad", Role: auth.RoleBasic}
	admin := &auth.User{ID: 7, Username: "sarah_lim", Role: auth.RoleAdmin}
	call := func(s *Server, u *auth.User) (int, map[string]any) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/agent/sessions", nil)
		s.proxyAgent(w, r, u, agentIdentity{UserID: u.ID, Username: u.Username, Token: "t"})
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	// Without aiassistant.read.
	s := &Server{cfg: &config.Config{AgentURL: "http://127.0.0.1:1", AgentSecret: "x"}}
	if code, _ := call(s, basic); code != http.StatusForbidden {
		t.Errorf("user without aiassistant.read: %d", code)
	}
	// Agent unreachable: 503 JSON envelope.
	code, body := call(s, admin)
	if code != http.StatusServiceUnavailable || body["error"] != "The AI assistant is not available right now" {
		t.Errorf("unreachable agent: %d %v", code, body)
	}
	// The agent rejecting the proxy's credentials becomes 502, never a 401
	// that the browser would take for an expired session.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
	}))
	defer upstream.Close()
	code, body = call(&Server{cfg: &config.Config{AgentURL: upstream.URL, AgentSecret: "wrong"}}, admin)
	if code != http.StatusBadGateway || body["error"] != "The AI assistant is not configured correctly" {
		t.Errorf("agent 401: %d %v", code, body)
	}
	// Not configured (no URL / no secret).
	for _, cfg := range []*config.Config{{AgentURL: "", AgentSecret: "x"}, {AgentURL: "http://ai-agent:8000", AgentSecret: ""}} {
		if code, _ := call(&Server{cfg: cfg}, admin); code != http.StatusServiceUnavailable {
			t.Errorf("unconfigured proxy %+v: %d", cfg, code)
		}
	}
}

func TestSetAgentHeadersSortsPermissions(t *testing.T) {
	h := http.Header{}
	h.Set("X-Agent-Anything", "x")
	h.Set("Cookie", "a=b")
	setAgentHeaders(h, "s", agentIdentity{UserID: 3, Username: "u", Token: "t", Permissions: []string{"simulation.read", "aiassistant.read"}})
	if h.Get("X-AFDD-Permissions") != "aiassistant.read,simulation.read" || h.Get("X-Agent-Anything") != "" || h.Get("Cookie") != "" {
		t.Fatalf("headers %v", h)
	}
}
