package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestHealthyPlantQuiet: three days of the natural weather and load (no
// fault, no trip) raise no alarm on the chiller plant, keep two chillers on
// duty and hold the supervisory points in their normal ranges.
func TestHealthyPlantQuiet(t *testing.T) {
	base := newPlant(404, testLoc)
	spec, err := parseScenario([]byte(`{"duration_min": 2880, "initial": "nominal"}`), base, maxJobMinutes)
	if err != nil {
		t.Fatal(err)
	}
	rec := &MemRecorder{}
	run := &scenarioRun{Spec: spec, Base: base, Seed: 404, Start: testStart.UTC(), StepS: 60, Opts: defaultJobOptions(), Rec: rec}
	sum, err := run.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("KPIs: %v", sum["variants"].(map[string]any)["baseline"])
	plant := map[string]bool{}
	for _, g := range []string{"chillers", "towers", "chwp", "cwp", "system", "mau"} {
		for _, a := range base.group(g) {
			plant[a.Spec.Alias] = true
		}
	}
	for _, e := range rec.EventRows {
		if plant[e.Asset] && (e.Kind == "alarm" || e.Kind == "abnormal" || e.Kind == "diagnosis") {
			t.Errorf("%s %s %s %s: %s", e.Ts.Format("01-02 15:04"), e.Asset, e.Kind, e.Code, e.Message)
		}
	}
	ranges := map[string][2]float64{"fan_speed_cmd": {40, 90}, "chwp_speed_cmd": {38, 48}, "riser_dp": {110, 130}, "chillers_running": {2, 2},
		"twv_pos": {0, 1}, "plant_eff": {0.55, 0.8}, "chws_temp": {5.8, 7.4}, "cw_supply_temp": {26, 32}}
	for param, r := range ranges {
		s := seriesRow(rec, "baseline", "CHWS_01", param)
		lo, hi := float32(1e9), float32(-1e9)
		for _, x := range s.Measured[30:] {
			lo, hi = min(lo, x), max(hi, x)
		}
		t.Logf("CHWS_01 %s: %.2f .. %.2f", param, lo, hi)
		if float64(lo) < r[0] || float64(hi) > r[1] {
			t.Errorf("CHWS_01 %s %.2f .. %.2f outside %v", param, lo, hi, r)
		}
	}
}

// TestDevelopingFaultsAlarm: each developing (non-LBNL) fault of the chiller
// plant models still raises an alarm or abnormal parameter at full severity
// under closed-loop control.
func TestDevelopingFaultsAlarm(t *testing.T) {
	for _, g := range []string{"chillers", "towers", "chwp", "cwp", "mau"} {
		probe := designPlant(1)
		for _, fs := range probe.group(g)[0].Model.randomFaults() {
			p := designPlant(61)
			var unit *Asset
			ss := simulate(p, testStart, 120, 60, func(k int, at time.Time) {
				if k == 30 {
					unit = runningUnit(t, p, g)
					if _, err := unit.injectFault(fs.Kind, 1.2, at, 0); err != nil {
						t.Fatal(err)
					}
				}
			})
			_ = ss
			hits := map[string]bool{}
			for _, a := range p.Assets {
				if a != unit && a != p.sys {
					continue
				}
				ev := p.evaluate(a, a.meas, false, 0)
				for c := range ev.Alarms {
					hits[a.Spec.Alias+":A:"+c] = true
				}
				for c := range ev.Abnormal {
					hits[a.Spec.Alias+":P:"+c] = true
				}
				for _, d := range p.diagnose(a, ev) {
					hits[a.Spec.Alias+":D:"+d.Rel.Target] = true
				}
			}
			var list []string
			for k := range hits {
				list = append(list, k)
			}
			sort.Strings(list)
			msg := fmt.Sprintf("%s %s on %s: %s", g, fs.Kind, unit.Spec.Alias, strings.Join(list, " "))
			t.Log(msg)
			own := false
			for k := range hits {
				own = own || strings.HasPrefix(k, unit.Spec.Alias+":")
			}
			if !own {
				t.Errorf("no alarm or abnormal parameter: %s", msg)
			}
		}
	}
}
