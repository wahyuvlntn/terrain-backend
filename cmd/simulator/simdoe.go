package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// ---------------------------------------------------------------------------
// Design of experiments (DESIGN 5.1): a Latin hypercube over the supervisory
// set points, weather and load, each sample run on a nominal plant with the
// conditions held fixed until it settles; the averages of the last minutes
// train the optimizer's surrogate model.
// ---------------------------------------------------------------------------

// DOESpec is the spec of a doe job.
type DOESpec struct {
	Samples   int                   `json:"samples"`
	SettleMin float64               `json:"settle_min"`
	AvgMin    float64               `json:"avg_min"`
	Seed      int64                 `json:"seed"`
	Bounds    map[string][2]float64 `json:"bounds"`
}

// doeInputs are the sampled inputs, in sampling order (sim.doe_samples.inputs).
var doeInputs = []string{"wet_bulb", "dry_bulb_minus_wb", "load_frac", "chws_sp", "approach_sp", "riser_dp_sp", "chillers", "twv_min_pos", "cw_min_temp"}

// doeOutputs are the averaged outputs (sim.doe_samples.outputs).
var doeOutputs = []string{"plant_kw", "chiller_kw", "chwp_kw", "cwp_kw", "tower_kw", "cooling_load_kw", "kw_per_rt", "chws_temp",
	"riser_dp", "mau_valve_max", "mau_sat_dev_max", "cw_supply_temp", "fan_speed_cmd", "twv_pos", "chwp_speed_cmd",
	"chillers_running", "capacity_ok", "alarms_critical"}

// doeDefaults are the default bounds; doeLimits the values the plant accepts
// (control ranges, plausible weather and load).
var (
	doeDefaults = map[string][2]float64{"wet_bulb": {22, 28}, "dry_bulb_minus_wb": {1, 7}, "load_frac": {0.5, 0.97}, "chws_sp": {5.5, 8.0},
		"approach_sp": {2.0, 6.0}, "riser_dp_sp": {80, 160}, "chillers": {1, 3}, "twv_min_pos": {0, 30}, "cw_min_temp": {18, 24}}
	doeLimits = map[string][2]float64{"wet_bulb": {10, 32}, "dry_bulb_minus_wb": {0.5, 15}, "load_frac": {0.2, 1.4}, "chws_sp": {5.5, 8.0},
		"approach_sp": {2.0, 8.0}, "riser_dp_sp": {80, 160}, "chillers": {1, 3}, "twv_min_pos": {0, 50}, "cw_min_temp": {15, 28}}
	// doeSteps snaps the set points to their control steps.
	doeSteps = map[string]float64{"chws_sp": 0.1, "approach_sp": 0.1, "riser_dp_sp": 1, "chillers": 1, "twv_min_pos": 1, "cw_min_temp": 0.5}
)

// parseDOE decodes and validates a doe spec (defaults for missing values).
func parseDOE(raw []byte) (*DOESpec, error) {
	var s DOESpec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	if s.Samples == 0 {
		s.Samples = 400
	}
	if s.SettleMin == 0 {
		s.SettleMin = 12
	}
	if s.AvgMin == 0 {
		s.AvgMin = 5
	}
	if s.Samples < 1 || s.Samples > 5000 {
		return nil, fmt.Errorf("samples %d outside 1 .. 5000", s.Samples)
	}
	if s.SettleMin < 1 || s.SettleMin > 240 || s.AvgMin < 1 || s.AvgMin > 120 {
		return nil, fmt.Errorf("settle_min %v / avg_min %v outside 1 .. 240 / 1 .. 120", s.SettleMin, s.AvgMin)
	}
	bounds := map[string][2]float64{}
	for k, b := range doeDefaults {
		bounds[k] = b
	}
	for k, b := range s.Bounds {
		lim, ok := doeLimits[k]
		if !ok {
			return nil, fmt.Errorf("unknown bound %q", k)
		}
		if b[0] > b[1] || b[0] < lim[0] || b[1] > lim[1] {
			return nil, fmt.Errorf("bound %s %v outside %v", k, b, lim)
		}
		bounds[k] = b
	}
	s.Bounds = bounds
	return &s, nil
}

// latinHypercube returns n points in [0, 1)^d with one point in each of the
// n strata of every dimension.
func latinHypercube(r *rand.Rand, n, d int) [][]float64 {
	out := make([][]float64, n)
	for i := range out {
		out[i] = make([]float64, d)
	}
	for j := 0; j < d; j++ {
		perm := r.Perm(n)
		for i := 0; i < n; i++ {
			out[i][j] = (float64(perm[i]) + r.Float64()) / float64(n)
		}
	}
	return out
}

// doeSample maps a unit point to the inputs of a sample (set points on their
// control steps, the chiller count an integer).
func doeSample(u []float64, bounds map[string][2]float64) map[string]float64 {
	in := make(map[string]float64, len(doeInputs))
	for j, k := range doeInputs {
		b := bounds[k]
		x := b[0] + u[j]*(b[1]-b[0])
		if k == "chillers" {
			x = clamp(math.Floor(b[0]+u[j]*(b[1]-b[0]+1)), b[0], b[1])
		}
		if st := doeSteps[k]; st > 0 {
			x = clamp(b[0]+math.Round((x-b[0])/st)*st, b[0], b[1])
			x = round(x, 1)
		} else {
			x = round(x, 3)
		}
		in[k] = x
	}
	return in
}

// doeRun runs a design of experiments on clones of a nominal plant.
type doeRun struct {
	Spec     *DOESpec
	Base     *Plant
	Seed     uint64
	Start    time.Time
	StepS    int
	Rec      Recorder
	Progress func(float64)
}

// run samples, simulates and records every sample; it returns the summary.
func (r *doeRun) run(ctx context.Context) (map[string]any, error) {
	units := latinHypercube(rand.New(rand.NewPCG(r.Seed, r.Seed^0xd0e)), r.Spec.Samples, len(doeInputs))
	settle := int(math.Round(r.Spec.SettleMin * 60 / float64(r.StepS)))
	avg := max(1, int(math.Round(r.Spec.AvgMin*60/float64(r.StepS))))
	ok := 0
	for i, u := range units {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.Progress != nil {
			r.Progress(float64(i) / float64(len(units)))
		}
		in := doeSample(u, r.Spec.Bounds)
		out, err := r.sample(r.Base.Clone(r.Seed+uint64(i)+1), in, settle, avg)
		if err != nil {
			return nil, fmt.Errorf("sample %d: %w", i, err)
		}
		if out["capacity_ok"] == 1 {
			ok++
		}
		r.Rec.Sample(DOESampleRow{Idx: i, Inputs: in, Outputs: out})
	}
	return map[string]any{"samples": len(units), "seed": int64(r.Seed), "settle_min": r.Spec.SettleMin, "avg_min": r.Spec.AvgMin, "step_s": r.StepS,
		"capacity_ok_frac": round(float64(ok)/float64(len(units)), 3), "inputs": doeInputs, "outputs": doeOutputs}, nil
}

// sample sets the plant controller to a sample's set points, holds its
// weather and load, steps settle + avg steps and averages the outputs over
// the last avg steps.
func (r *doeRun) sample(p *Plant, in map[string]float64, settle, avg int) (map[string]float64, error) {
	s := p.sys
	if s == nil {
		return nil, fmt.Errorf("no plant controller")
	}
	for _, w := range []struct {
		param string
		value float64
	}{
		{"chws_reset_mode", 1}, {"chws_sp", in["chws_sp"]}, {"staging_mode", 1}, {"chillers_sp", in["chillers"]},
		{"approach_sp", in["approach_sp"]}, {"riser_dp_sp", in["riser_dp_sp"]}, {"twv_min_pos", in["twv_min_pos"]}, {"cw_min_temp", in["cw_min_temp"]},
	} {
		if err := s.applyControl(w.param, w.value); err != nil {
			return nil, err
		}
	}
	wb, db, load := in["wet_bulb"], in["wet_bulb"]+in["dry_bulb_minus_wb"], in["load_frac"]
	p.EnvHook = func(_ time.Time, env *Env) {
		env.WetBulb, env.DryBulb, env.Load = wb, db, load
		env.RH = rhOf(db, wb)
	}
	sum := map[string]float64{}
	crit := map[string]bool{}
	dt := float64(r.StepS)
	t := r.Start
	for k := 0; k < settle+avg; k++ {
		meas := p.Step(t, dt)
		t = t.Add(time.Duration(r.StepS) * time.Second)
		if k < settle {
			continue
		}
		v := meas[s]
		for _, key := range []string{"plant_kw", "chws_temp", "riser_dp", "cw_supply_temp", "fan_speed_cmd", "twv_pos", "chwp_speed_cmd", "chillers_running"} {
			sum[key] += v[key]
		}
		sum["cooling_load_kw"] += v["cooling_load"]
		var mauValve, satDev float64
		for _, a := range p.Assets {
			m := meas[a]
			if !a.Running() {
				continue
			}
			switch a.Kind {
			case "chiller":
				sum["chiller_kw"] += m["power_kw"]
			case "chwp":
				sum["chwp_kw"] += m["power_kw"]
			case "cwp":
				sum["cwp_kw"] += m["power_kw"]
			case "ct":
				sum["tower_kw"] += m["fan_power"]
			case "mau":
				mauValve = math.Max(mauValve, m["chw_valve"])
				satDev = math.Max(satDev, math.Abs(m["sat"]-17))
			}
			for code, h := range p.evaluate(a, m, a.Offline, a.Forced).Alarms {
				if h.Severity == "CRITICAL" {
					crit[a.Spec.Alias+"|"+code] = true
				}
			}
		}
		sum["mau_valve_max"] += mauValve
		sum["mau_sat_dev_max"] += satDev
	}
	out := make(map[string]float64, len(doeOutputs))
	for _, k := range doeOutputs {
		out[k] = sum[k] / float64(avg)
	}
	out["kw_per_rt"] = 0
	if out["cooling_load_kw"] > 0 {
		out["kw_per_rt"] = out["plant_kw"] / (out["cooling_load_kw"] / 3.517)
	}
	out["capacity_ok"] = b2f(out["chws_temp"] <= s.Cmd["chws_sp"]+0.5 && out["mau_valve_max"] < 98)
	out["alarms_critical"] = float64(len(crit))
	for k, v := range out {
		out[k] = round(v, 4)
	}
	return out, nil
}

// doe runs a doe job.
func (r *JobRunner) doe(ctx context.Context, job *simJob, stepS int, rec Recorder, progress func(float64)) (map[string]any, error) {
	spec, err := parseDOE(job.Spec)
	if err != nil {
		return nil, err
	}
	seed := jobSeed(spec.Seed, job.ID)
	start := time.Now().UTC().Truncate(time.Minute)
	end := start.Add(time.Duration(spec.SettleMin+spec.AvgMin) * time.Minute)
	if _, err := r.db.Exec(ctx, `UPDATE sim.jobs SET sim_start = $2, sim_end = $3, step_s = $4, updated_at = now() WHERE id = $1`,
		job.ID, start, end, stepS); err != nil {
		return nil, err
	}
	base := newPlant(seed, r.loc)
	base.dropFab()
	run := &doeRun{Spec: spec, Base: base, Seed: seed, Start: start, StepS: stepS, Rec: rec, Progress: progress}
	return run.run(ctx)
}
