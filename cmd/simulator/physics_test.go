package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

var testLoc = time.FixedZone("SGT", 8*3600)

// testStart is a Tuesday morning (the weekly rotation keeps CH-01 / CH-02 on duty).
var testStart = time.Date(2026, 10, 6, 10, 0, 0, 0, testLoc)

// fixedEnv holds the weather and the fab load (fraction of two chillers).
func fixedEnv(wb, db, load float64) func(time.Time, *Env) {
	return func(_ time.Time, env *Env) {
		env.WetBulb, env.DryBulb, env.Load = wb, db, load
		env.RH = rhOf(db, wb)
	}
}

// designPlant is a nominal plant at the design conditions (wet bulb 25.5 °C,
// dry bulb 28 °C, load 0.74).
func designPlant(seed uint64) *Plant {
	p := newPlant(seed, testLoc)
	p.EnvHook = fixedEnv(25.5, 28, 0.74)
	return p
}

// sample is one step of a run: measured and true values by alias.
type sample struct {
	t     time.Time
	meas  map[string]map[string]float64
	truth map[string]map[string]float64
}

// simulate runs a plant for minutes at a step of stepS seconds and returns
// every step; before (optional) runs before each step.
func simulate(p *Plant, start time.Time, minutes int, stepS float64, before func(k int, t time.Time)) []sample {
	n := int(float64(minutes) * 60 / stepS)
	out := make([]sample, 0, n)
	for k := 0; k < n; k++ {
		t := start.Add(time.Duration(float64(k) * stepS * float64(time.Second)))
		if before != nil {
			before(k, t)
		}
		meas := p.Step(t, stepS)
		s := sample{t: t, meas: map[string]map[string]float64{}, truth: map[string]map[string]float64{}}
		for a, v := range meas {
			s.meas[a.Spec.Alias] = v
			s.truth[a.Spec.Alias] = p.Truth[a]
		}
		out = append(out, s)
	}
	return out
}

// avg is the mean of a measured (truth = false) or true value over samples.
func avg(ss []sample, alias, param string, truth bool) float64 {
	sum := 0.0
	for _, s := range ss {
		if truth {
			sum += s.truth[alias][param]
		} else {
			sum += s.meas[alias][param]
		}
	}
	return sum / float64(len(ss))
}

// seriesOf returns a measured value over samples.
func seriesOf(ss []sample, alias, param string) []float64 {
	out := make([]float64, len(ss))
	for i, s := range ss {
		out[i] = s.meas[alias][param]
	}
	return out
}

// reversals counts the direction changes of a signal, ignoring moves smaller
// than deadband (turning points with hysteresis).
func reversals(xs []float64, deadband float64) int {
	if len(xs) == 0 {
		return 0
	}
	n, dir, ext := 0, 0, xs[0]
	for _, x := range xs[1:] {
		switch {
		case dir >= 0 && x > ext:
			ext, dir = x, 1
		case dir <= 0 && x < ext:
			ext, dir = x, -1
		case dir == 1 && x < ext-deadband:
			n++
			ext, dir = x, -1
		case dir == -1 && x > ext+deadband:
			n++
			ext, dir = x, 1
		case dir == 0 && math.Abs(x-ext) > deadband:
			dir = 1
			if x < ext {
				dir = -1
			}
			ext = x
		}
	}
	return n
}

func peakToPeak(xs []float64) float64 {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, x := range xs {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return hi - lo
}

// runningUnit returns the first running unit of a group.
func runningUnit(t *testing.T, p *Plant, group string) *Asset {
	t.Helper()
	for _, a := range p.group(group) {
		if a.Running() {
			return a
		}
	}
	t.Fatalf("no running unit in %s", group)
	return nil
}

func TestDesignSteadyState(t *testing.T) {
	p := designPlant(11)
	ss := simulate(p, testStart, 180, 60, nil)
	last := ss[120:]
	s := p.sys.Spec.Alias
	fan, hz := avg(last, s, "fan_speed_cmd", false), avg(last, s, "chwp_speed_cmd", false)
	dp, dpSP := avg(last, s, "riser_dp", false), avg(last, s, "riser_dp_sp", false)
	chws, chwsSP := avg(last, s, "chws_temp", false), avg(last, s, "chws_sp", false)
	twv, minPos := avg(last, s, "twv_pos", false), avg(last, s, "twv_min_pos", false)
	n := avg(last, s, "chillers_running", false)
	t.Logf("design: fan_speed_cmd %.1f %%, chwp_speed_cmd %.2f Hz, riser_dp %.1f / %.0f kPa, chws %.2f / %.1f °C, twv %.1f %%, chillers %.1f",
		fan, hz, dp, dpSP, chws, chwsSP, twv, n)
	t.Logf("design: load %.0f kW, plant %.0f kW, %.3f kW/RT, cwst_sp %.2f, tower supply %.2f, CW supply %.2f / return %.2f °C, CW flow %.0f m³/h, CHW plant flow %.0f, bypass %.0f m³/h",
		avg(last, s, "cooling_load", false), avg(last, s, "plant_kw", false), avg(last, s, "plant_eff", false), avg(last, s, "cwst_sp", false),
		avg(last, s, "tower_supply_temp", false), avg(last, s, "cw_supply_temp", false), avg(last, s, "cw_return_temp", false),
		avg(last, s, "cw_flow", false), avg(last, s, "plant_flow", false), avg(last, s, "bypass_flow", false))
	ch, ct, pm, cw := runningUnit(t, p, "chillers").Spec.Alias, runningUnit(t, p, "towers").Spec.Alias, runningUnit(t, p, "chwp").Spec.Alias, runningUnit(t, p, "cwp").Spec.Alias
	t.Logf("design: %s load %.1f %%, %.0f kW, COP-eq %.3f kW/RT, CHW flow %.0f, cond flow %.0f, cwet %.2f, cond press %.0f kPa; %s fan %.1f %% %.1f kW %.1f A approach %.2f K; %s %.1f kW %.1f A %.0f m³/h %.2f bar; %s %.1f kW",
		ch, avg(last, ch, "load_pct", false), avg(last, ch, "power_kw", false), avg(last, ch, "efficiency", false), avg(last, ch, "chw_flow", false),
		avg(last, ch, "cond_flow", false), avg(last, ch, "cwet", false), avg(last, ch, "cond_press", false),
		ct, avg(last, ct, "fan_speed", false), avg(last, ct, "fan_power", false), avg(last, ct, "fan_current", false), avg(last, ct, "approach", false),
		pm, avg(last, pm, "power_kw", false), avg(last, pm, "motor_current", false), avg(last, pm, "flow", false), avg(last, pm, "discharge_press", false),
		cw, avg(last, cw, "power_kw", false))
	if fan < 55 || fan > 75 {
		t.Errorf("fan_speed_cmd %.1f %% outside 55 .. 75", fan)
	}
	if hz < 40 || hz > 48 {
		t.Errorf("chwp_speed_cmd %.2f Hz outside 40 .. 48", hz)
	}
	if math.Abs(dp-dpSP) > 3 {
		t.Errorf("riser_dp %.1f not within 3 kPa of %.0f", dp, dpSP)
	}
	if math.Abs(chws-chwsSP) > 0.3 {
		t.Errorf("chws_temp %.2f not within 0.3 K of chws_sp %.1f", chws, chwsSP)
	}
	if math.Abs(twv-minPos) > 1 {
		t.Errorf("twv_pos %.1f not at its minimum %.0f", twv, minPos)
	}
	if n != 2 {
		t.Errorf("chillers running %.1f, want 2 at the design load", n)
	}
	// no alarm and no abnormal parameter on the healthy chiller plant at design
	for _, g := range []string{"chillers", "towers", "chwp", "cwp", "system", "mau"} {
		for _, a := range p.group(g) {
			ev := p.evaluate(a, a.meas, false, 0)
			if len(ev.Alarms) > 0 || len(ev.Abnormal) > 0 {
				t.Errorf("%s at design: alarms %v, abnormal %v", a.Spec.Alias, ev.Alarms, ev.Abnormal)
			}
		}
	}
}

// TestLoopStepIndependence: the plant loops run in internal 5 s steps, so a
// 60 s step (history, jobs) and a 5 s step (live) settle at the same point.
func TestLoopStepIndependence(t *testing.T) {
	a := simulate(designPlant(3), testStart, 120, 60, nil)[60:]
	b := simulate(designPlant(3), testStart, 120, 5, nil)[720:]
	s := "CHWS_01"
	for _, k := range []struct {
		param string
		tol   float64
	}{{"fan_speed_cmd", 3}, {"chwp_speed_cmd", 0.5}, {"riser_dp", 2}, {"cw_supply_temp", 0.2}, {"plant_kw", 25}} {
		x, y := avg(a, s, k.param, false), avg(b, s, k.param, false)
		t.Logf("%s: dt 60 s %.2f, dt 5 s %.2f", k.param, x, y)
		if math.Abs(x-y) > k.tol {
			t.Errorf("%s differs between steps: %.2f vs %.2f", k.param, x, y)
		}
	}
}

// faultRun runs a design plant for 3 h with a fault injected after 1 h on the
// unit pick returns, and the same plant healthy; it returns the last hour of
// both.
func faultRun(t *testing.T, seed uint64, kind string, value float64, pick func(p *Plant) *Asset) (faulty, healthy []sample, unit string) {
	t.Helper()
	ph := designPlant(seed)
	healthy = simulate(ph, testStart, 180, 60, nil)[120:]
	pf := designPlant(seed)
	faulty = simulate(pf, testStart, 180, 60, func(k int, at time.Time) {
		if k == 60 {
			a := pick(pf)
			unit = a.Spec.Alias
			if _, err := a.injectFault(kind, value, at, 0); err != nil {
				t.Fatalf("inject %s on %s: %v", kind, a.Spec.Alias, err)
			}
		}
	})[120:]
	return faulty, healthy, unit
}

func TestLBNLFaultSignatures(t *testing.T) {
	s := "CHWS_01"
	running := func(group string) func(p *Plant) *Asset {
		return func(p *Plant) *Asset {
			for _, a := range p.group(group) {
				if a.Running() {
					return a
				}
			}
			return nil
		}
	}
	sys := func(p *Plant) *Asset { return p.sys }

	t.Run("chwst_bias", func(t *testing.T) {
		f, h, ch := faultRun(t, 21, "chwst_bias", 2, running("chillers"))
		meas, truth := avg(f, ch, "chwst", false), avg(f, ch, "chwst", true)
		asp := avg(f, ch, "chwst_asp", false)
		hdr, hdrH := avg(f, s, "chws_temp", false), avg(h, s, "chws_temp", false)
		t.Logf("%s chwst measured %.2f, true %.2f, asp %.2f; header %.2f (healthy %.2f)", ch, meas, truth, asp, hdr, hdrH)
		if math.Abs(meas-truth-2) > 0.05 || math.Abs(meas-asp) > 0.2 {
			t.Errorf("biased sensor: measured %.2f true %.2f asp %.2f", meas, truth, asp)
		}
		if d := hdrH - hdr; d < 0.7 || d > 1.3 { // one of two chillers 2 K colder
			t.Errorf("header moved %.2f K, want about 1 K colder", d)
		}
	})
	t.Run("cwst_bias", func(t *testing.T) {
		f, h, ct := faultRun(t, 22, "cwst_bias", 2, running("towers"))
		meas, truth := avg(f, ct, "cwst", false), avg(f, ct, "cwst", true)
		sp, tower := avg(f, s, "cwst_sp", false), avg(f, s, "tower_supply_temp", false)
		fan, fanH := avg(f, s, "fan_speed_cmd", false), avg(h, s, "fan_speed_cmd", false)
		t.Logf("%s cwst measured %.2f true %.2f; cwst_sp %.2f, tower supply %.2f; fan %.1f %% (healthy %.1f)", ct, meas, truth, sp, tower, fan, fanH)
		if math.Abs(meas-truth-2) > 0.05 {
			t.Errorf("bias not on the measured cwst: %.2f vs %.2f", meas, truth)
		}
		if d := sp - tower; d < 0.7 || d > 1.3 { // the fan PI holds the biased average
			t.Errorf("tower supply %.2f K below the set point, want about 1 K", d)
		}
		if fan-fanH < 5 {
			t.Errorf("fan speed %.1f not above healthy %.1f", fan, fanH)
		}
	})
	t.Run("ct_fouling", func(t *testing.T) {
		f, h, ct := faultRun(t, 23, "ct_fouling", 0.65, running("towers"))
		fan, fanH := avg(f, s, "fan_speed_cmd", false), avg(h, s, "fan_speed_cmd", false)
		app := avg(f, ct, "approach", false)
		fanCell := avg(f, ct, "fan_speed", false)
		expected := towerApproach(1, fanCell, 1) // fault-free approach at this fan speed (design load)
		t.Logf("%s fan %.1f %% (healthy %.1f), approach %.2f K vs fault-free %.2f K at that speed", ct, fan, fanH, app, expected)
		if fan-fanH < 15 {
			t.Errorf("fan speed %.1f not well above healthy %.1f", fan, fanH)
		}
		if app-expected < 0.8 {
			t.Errorf("approach %.2f not above the fault-free %.2f at the same fan speed", app, expected)
		}
	})
	t.Run("riser_dp_bias", func(t *testing.T) {
		for _, v := range []float64{0.2, -0.2} {
			f, h, _ := faultRun(t, 24, "riser_dp_bias", v, sys)
			meas, truth, sp := avg(f, s, "riser_dp", false), avg(f, s, "riser_dp", true), avg(f, s, "riser_dp_sp", false)
			hz, hzH := avg(f, s, "chwp_speed_cmd", false), avg(h, s, "chwp_speed_cmd", false)
			t.Logf("bias %+.2f: riser dp measured %.1f true %.1f (sp %.0f), pumps %.2f Hz (healthy %.2f)", v, meas, truth, sp, hz, hzH)
			if math.Abs(meas/truth-(1+v)) > 0.01 {
				t.Errorf("measured %.1f is not true %.1f x %.2f", meas, truth, 1+v)
			}
			if v > 0 && (math.Abs(meas-sp) > 3 || math.Abs(truth-sp/(1+v)) > 4 || hz >= hzH-0.5) {
				t.Errorf("positive bias: measured %.1f, true %.1f, %.2f Hz vs healthy %.2f", meas, truth, hz, hzH)
			}
			if v < 0 && (truth < sp+15 || hz <= hzH+0.5) {
				t.Errorf("negative bias: true %.1f should be well above %.0f, %.2f Hz vs healthy %.2f", truth, sp, hz, hzH)
			}
		}
	})
	t.Run("twv_leak", func(t *testing.T) {
		f, h, _ := faultRun(t, 25, "twv_leak", 0.5, sys)
		pos, cmd := avg(f, s, "twv_pos", false), avg(f, s, "twv_cmd", false)
		sup, tower, ret := avg(f, s, "cw_supply_temp", false), avg(f, s, "tower_supply_temp", false), avg(f, s, "cw_return_temp", false)
		mix := (sup - tower) / (ret - tower)
		t.Logf("leak 0.5: twv pos %.1f cmd %.1f; CW supply %.2f (healthy %.2f), tower %.2f, return %.2f; mixing fraction %.3f",
			pos, cmd, sup, avg(h, s, "cw_supply_temp", false), tower, ret, mix)
		if pos > 2 || math.Abs(mix-0.5) > 0.05 {
			t.Errorf("leak signature: position %.1f, mixing fraction %.3f", pos, mix)
		}
	})
	t.Run("twv_stuck", func(t *testing.T) {
		f, _, _ := faultRun(t, 26, "twv_stuck", 0.75, sys)
		pos, cmd := avg(f, s, "twv_pos", false), avg(f, s, "twv_cmd", false)
		t.Logf("stuck 0.75: twv pos %.1f cmd %.1f, CW supply %.2f", pos, cmd, avg(f, s, "cw_supply_temp", false))
		if math.Abs(pos-75) > 0.5 || math.Abs(pos-cmd) <= 10 {
			t.Errorf("stuck signature: position %.1f, command %.1f", pos, cmd)
		}
	})
	t.Run("cw_pi_tuning", func(t *testing.T) {
		f, h, _ := faultRun(t, 27, "cw_pi_tuning", 1, sys)
		rf, rh := reversals(seriesOf(f, s, "fan_speed_cmd"), 2), reversals(seriesOf(h, s, "fan_speed_cmd"), 2)
		ppFan, ppT := peakToPeak(seriesOf(f, s, "fan_speed_cmd")), peakToPeak(seriesOf(f, s, "tower_supply_temp"))
		t.Logf("PI detuned: %d fan command reversals/h (healthy %d), fan p-p %.1f %%, tower supply p-p %.2f K (healthy %.2f K)",
			rf, rh, ppFan, ppT, peakToPeak(seriesOf(h, s, "tower_supply_temp")))
		if rf <= 6 || rh > 2 {
			t.Errorf("reversals/h faulty %d (want > 6), healthy %d (want <= 2)", rf, rh)
		}
		if ppFan <= 10 || ppT <= 0.5 {
			t.Errorf("hunting amplitude: fan p-p %.1f %%, tower supply p-p %.2f K", ppFan, ppT)
		}
	})
}

// TestMAUValveSaturation: warm CHW and a low riser DP on a hot afternoon
// saturate the MAU coil valves and the supply air warms (AHU-003).
func TestMAUValveSaturation(t *testing.T) {
	p := newPlant(31, testLoc)
	p.EnvHook = fixedEnv(26.3, 31.5, 0.80)
	ok := simulate(p, testStart, 60, 60, nil)[40:]
	p2 := newPlant(31, testLoc)
	p2.EnvHook = fixedEnv(26.3, 31.5, 0.80)
	s := p2.sys
	for _, w := range [][2]any{{"chws_reset_mode", 1.0}, {"chws_sp", 8.0}, {"riser_dp_sp", 80.0}} {
		if err := s.applyControl(w[0].(string), w[1].(float64)); err != nil {
			t.Fatal(err)
		}
	}
	sat := simulate(p2, testStart, 60, 60, nil)[40:]
	vOK, vSat := avg(ok, "MAU_01", "chw_valve", false), avg(sat, "MAU_01", "chw_valve", false)
	tOK, tSat := avg(ok, "MAU_01", "sat", false), avg(sat, "MAU_01", "sat", false)
	t.Logf("MAU_01 valve %.1f %% -> %.1f %%, SAT %.2f -> %.2f °C (CHWS %.2f -> %.2f °C, riser DP %.0f -> %.0f kPa)", vOK, vSat, tOK, tSat,
		avg(ok, "CHWS_01", "chws_temp", false), avg(sat, "CHWS_01", "chws_temp", false), avg(ok, "CHWS_01", "riser_dp", false), avg(sat, "CHWS_01", "riser_dp", false))
	if vOK > 95 || math.Abs(tOK-17) > 0.3 {
		t.Errorf("default set points: valve %.1f, SAT %.2f", vOK, tOK)
	}
	if vSat < 99 || tSat < 17.8 {
		t.Errorf("saturated: valve %.1f, SAT %.2f (want the valve at 100 and SAT up)", vSat, tSat)
	}
	ev := p2.evaluate(p2.ByAlias["MAU_01"], p2.ByAlias["MAU_01"].meas, false, 0)
	t.Logf("MAU_01 alarms: %v", ev.Alarms)
	// a hotter afternoon leaves a third of the demand unmet: AHU-003
	p3 := newPlant(31, testLoc)
	p3.EnvHook = fixedEnv(27, 33.5, 0.85)
	for _, w := range [][2]any{{"chws_reset_mode", 1.0}, {"chws_sp", 8.0}, {"riser_dp_sp", 80.0}} {
		if err := p3.sys.applyControl(w[0].(string), w[1].(float64)); err != nil {
			t.Fatal(err)
		}
	}
	hot := simulate(p3, testStart, 60, 60, nil)[40:]
	ev = p3.evaluate(p3.ByAlias["MAU_01"], p3.ByAlias["MAU_01"].meas, false, 0)
	t.Logf("hot afternoon: MAU_01 SAT %.2f °C, alarms %v", avg(hot, "MAU_01", "sat", false), ev.Alarms)
	if _, ok := ev.Alarms["AHU-003"]; !ok {
		t.Errorf("no AHU-003 with SAT %.2f °C", avg(hot, "MAU_01", "sat", false))
	}
}

// TestStagingWaits: load staging adds a chiller after 10 min above 95 % of
// the running capacity and sheds one after 30 min below 80 % of the
// remaining capacity.
func TestStagingWaits(t *testing.T) {
	p := newPlant(41, testLoc)
	load := 0.74
	p.EnvHook = func(at time.Time, env *Env) { fixedEnv(25.5, 28, load)(at, env) }
	ss := simulate(p, testStart, 30, 60, nil)
	if n := ss[len(ss)-1].meas["CHWS_01"]["chillers_running"]; n != 2 {
		t.Fatalf("chillers at 0.74: %v", n)
	}
	load = 0.97 // 6.8 MW > 2 x 3517 x 0.95
	up := -1
	start := testStart.Add(30 * time.Minute)
	ss = simulate(p, start, 30, 60, nil)
	for k, s := range ss {
		if s.meas["CHWS_01"]["chillers_running"] == 3 {
			up = k
			break
		}
	}
	t.Logf("stage up %d min after the load step", up)
	if up < 10 || up > 13 {
		t.Errorf("stage up after %d min, want 10 .. 13", up)
	}
	load = 0.60 // 4.2 MW < 2 x 3517 x 0.80
	down := -1
	ss = simulate(p, start.Add(30*time.Minute), 45, 60, nil)
	for k, s := range ss {
		if s.meas["CHWS_01"]["chillers_running"] == 2 {
			down = k
			break
		}
	}
	t.Logf("stage down %d min after the load drop", down)
	if down < 30 || down > 34 {
		t.Errorf("stage down after %d min, want 30 .. 34", down)
	}
	// Manual staging holds the operator's count
	if err := p.sys.applyControl("staging_mode", 1); err != nil {
		t.Fatal(err)
	}
	if err := p.sys.applyControl("chillers_sp", 1); err != nil {
		t.Fatal(err)
	}
	ss = simulate(p, start.Add(75*time.Minute), 20, 60, nil)
	if n := ss[len(ss)-1].meas["CHWS_01"]["chillers_running"]; n != 1 {
		t.Errorf("manual staging 1: %v chillers running", n)
	}
}

// TestCHWSystemParams: the plant controller publishes every one of its
// points, with chillers running and with the whole plant stopped.
func TestCHWSystemParams(t *testing.T) {
	p := designPlant(5)
	check := func(label string) {
		v := p.sys.meas
		for _, ps := range p.sys.Model.Params {
			if _, ok := v[ps.Alias]; !ok {
				t.Errorf("%s: %s missing", label, ps.Alias)
			}
		}
	}
	simulate(p, testStart, 10, 60, nil)
	check("running")
	for _, c := range p.group("chillers") {
		if err := c.applyControl("run_mode", runOff); err != nil {
			t.Fatal(err)
		}
	}
	ss := simulate(p, testStart.Add(10*time.Minute), 10, 60, nil)
	check("stopped")
	if v := ss[len(ss)-1].meas["CHWS_01"]; v["chillers_running"] != 0 || v["chwp_speed_cmd"] != 0 || v["fan_speed_cmd"] != 0 {
		t.Errorf("stopped plant: %v chillers, pumps %v Hz, fans %v %%", v["chillers_running"], v["chwp_speed_cmd"], v["fan_speed_cmd"])
	}
}

// TestCHWSResetAndPush: Reset mode recomputes the CHW supply set point once
// per hour from the outdoor air and pushes it to the chillers; Manual takes
// the operator's value.
func TestCHWSResetAndPush(t *testing.T) {
	p := newPlant(7, testLoc)
	db := 31.0
	p.EnvHook = func(at time.Time, env *Env) { fixedEnv(26, db, 0.74)(at, env) }
	simulate(p, testStart, 5, 60, nil)
	sp := p.sys.Cmd["chws_sp"]
	if math.Abs(sp-6.0) > 0.11 {
		t.Errorf("reset at 31 °C: %.1f, want about 6.0", sp)
	}
	db = 26
	ss := simulate(p, testStart.Add(5*time.Minute), 30, 60, nil) // same hour: held
	if got := ss[len(ss)-1].meas["CHWS_01"]["chws_sp"]; got != sp {
		t.Errorf("set point changed within the hour: %.1f -> %.1f", sp, got)
	}
	ss = simulate(p, testStart.Add(65*time.Minute), 10, 60, nil)
	if got := ss[len(ss)-1].meas["CHWS_01"]["chws_sp"]; math.Abs(got-7.0) > 0.11 {
		t.Errorf("reset next hour at 26 °C: %.1f, want about 7.0", got)
	}
	for _, c := range p.group("chillers") {
		if c.Ctl["chwst_sp"] != p.sys.Cmd["chws_sp"] {
			t.Errorf("%s set point %.1f not pushed (%.1f)", c.Spec.Alias, c.Ctl["chwst_sp"], p.sys.Cmd["chws_sp"])
		}
	}
	// a local write lasts until the next push
	if err := p.ByAlias["CH_01"].applyControl("chwst_sp", 7.5); err != nil {
		t.Fatal(err)
	}
	simulate(p, testStart.Add(80*time.Minute), 5, 60, nil)
	if got := p.ByAlias["CH_01"].Ctl["chwst_sp"]; got != 7.5 {
		t.Errorf("local write overwritten without a plant change: %.1f", got)
	}
	if err := p.sys.applyControl("chws_reset_mode", 1); err != nil {
		t.Fatal(err)
	}
	if err := p.sys.applyControl("chws_sp", 6.8); err != nil {
		t.Fatal(err)
	}
	ss = simulate(p, testStart.Add(85*time.Minute), 30, 60, nil)
	for _, c := range p.group("chillers") {
		if c.Ctl["chwst_sp"] != 6.8 {
			t.Errorf("%s set point %.1f after the manual plant set point 6.8", c.Spec.Alias, c.Ctl["chwst_sp"])
		}
	}
	if got := avg(ss[20:], "CHWS_01", "chws_temp", false); math.Abs(got-6.86) > 0.2 {
		t.Errorf("header %.2f after the manual set point 6.8", got)
	}
}

// TestFaultCommand: the digital twin's fault injection on the command topic.
func TestFaultCommand(t *testing.T) {
	p := designPlant(9)
	simulate(p, testStart, 2, 60, nil)
	l := newLive(t.Context(), nil, p, LiveOptions{RampMin: 20, RampMax: 60}, 9)
	var cmd Command
	if err := json.Unmarshal([]byte(`{"request_id": 12, "fault": "twv_leak", "value": 0.5, "action": "inject", "user": "sarah_lim"}`), &cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Gateway, cmd.Device = "cub_bms", "CHWS_01"
	if !l.applyCommand(cmd) {
		t.Fatal("injection not applied")
	}
	if v, ok := p.sys.fval("twv_leak"); !ok || v != 0.5 {
		t.Fatalf("twv_leak = %v, %v", v, ok)
	}
	for _, bad := range []Command{
		{Gateway: "cub_bms", Device: "CHWS_01", Fault: "twv_leak", Value: 0.9, Action: "inject"},   // beyond the intensities
		{Gateway: "cub_bms", Device: "CHWS_01", Fault: "chwst_bias", Value: 1, Action: "inject"},   // not a CHWS fault
		{Gateway: "cub_bms", Device: "CHWS_01", Fault: "twv_leak", Value: 0.5, Action: "explode"},  // unknown action
		{Gateway: "fab1_hvac", Device: "CHWS_01", Fault: "twv_leak", Value: 0.5, Action: "inject"}, // wrong gateway
	} {
		if l.applyCommand(bad) {
			t.Errorf("accepted %+v", bad)
		}
	}
	cmd.Action = "clear"
	if !l.applyCommand(cmd) || p.sys.Faults["twv_leak"] != nil {
		t.Error("clear failed")
	}
	// a developing fault injected at a fixed severity
	if !l.applyCommand(Command{Gateway: "cub_bms", Device: "CH_01", Fault: "cond_fouling", Value: 0.8, Action: "inject"}) {
		t.Fatal("cond_fouling not injected")
	}
	if f := p.ByAlias["CH_01"].Faults["cond_fouling"]; f == nil || f.Sev != 0.8 || f.Rate != 0 {
		t.Errorf("cond_fouling %+v", f)
	}
}

// TestManualFaultsNotRandom: random injection never picks an LBNL fault.
func TestManualFaultsNotRandom(t *testing.T) {
	p := designPlant(13)
	simulate(p, testStart, 2, 60, nil)
	r := p.rng
	for i := 0; i < 2000; i++ {
		a, fs, _ := p.injectRandomFault(r, testStart, 20, 60)
		if a != nil {
			if fs.Manual {
				t.Fatalf("random injection picked %s on %s", fs.Kind, a.Spec.Alias)
			}
			delete(a.Faults, fs.Kind)
		}
	}
}

// TestFDDContent: every FDD code has its alarm code, Diagnostic and Repair
// SOPs (MDX-safe), work notes, an FDD issue relation on its model and an
// LBNL fault repaired by it.
func TestFDDContent(t *testing.T) {
	for _, in := range instructions {
		for _, s := range []string{in.Title, in.Desc, in.Content} {
			if strings.ContainsAny(s, "<>{}") {
				t.Errorf("%s %q: content contains < > { or }", in.Code, in.Title)
			}
		}
	}
	for _, code := range fddCodes {
		if codeName(code) == code {
			t.Errorf("%s: no alarm code", code)
		}
		types := map[int]bool{}
		for _, in := range instructions {
			if in.Code == code {
				types[in.Type] = true
			}
		}
		if !types[1] || !types[2] {
			t.Errorf("%s: SOP types %v, want Diagnostic and Repair", code, types)
		}
		if len(workNotes[code]) == 0 {
			t.Errorf("%s: no work notes", code)
		}
		keys := instructionModels(code)
		found := false
		for _, m := range models() {
			if !contains(keys, m.Key) {
				continue
			}
			for _, r := range m.Relations {
				if r.Target == code && r.Name == codeName(code)+" (FDD)" && len(r.Paths) == 1 && r.Paths[0][0] == "A:"+code {
					found = true
				}
			}
			for _, f := range m.Faults {
				if f.Manual && contains(f.Codes, code) {
					found = found && true
				}
			}
		}
		if !found {
			t.Errorf("%s: no FDD relation on a model of %v", code, keys)
		}
	}
	for _, m := range models() {
		for _, f := range m.Faults {
			if f.Manual && (len(f.Codes) == 0 || !isFDDCode(f.Codes[0]) || len(f.Intensities) == 0) {
				t.Errorf("%s %s: LBNL fault without FDD code or intensities", m.Name, f.Kind)
			}
		}
	}
}
