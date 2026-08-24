package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"afdd/internal/engine"
)

// Plant is the simulated facility.
type Plant struct {
	Assets  []*Asset
	ByAlias map[string]*Asset
	Models  map[string]*ModelSpec
	Env     Env
	// Truth holds the true values of the last step (Step returns the
	// measured ones); simulation jobs keep both.
	Truth map[*Asset]map[string]float64
	// EnvHook adjusts the environment of every step (simulation jobs: weather
	// offsets, load scale, the fixed conditions of a DOE sample).
	EnvHook func(t time.Time, env *Env)
	loc     *time.Location
	weather *Weather
	anchor  time.Time
	rng     *rand.Rand
	defs    map[string]map[string]*engine.ParamDef // model -> alias -> thresholds
	sys     *Asset                                 // the plant controller (CHWS_01)
	groups  map[string][]*Asset
}

func newPlant(seed uint64, loc *time.Location) *Plant {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	p := &Plant{ByAlias: map[string]*Asset{}, Models: map[string]*ModelSpec{}, loc: loc, rng: r,
		weather: newWeather(rand.New(rand.NewPCG(r.Uint64(), r.Uint64()))), defs: map[string]map[string]*engine.ParamDef{},
		anchor: time.Date(2026, 1, 5, 8, 0, 0, 0, loc), groups: map[string][]*Asset{}}
	for _, m := range models() {
		p.Models[m.Name] = m
		defs := map[string]*engine.ParamDef{}
		for _, ps := range m.Params {
			d := &engine.ParamDef{Alias: ps.Alias, Name: ps.Name, Unit: ps.Unit}
			if !ps.Bool {
				sc := ps.Scale
				d.Scale = &sc
			}
			setT := func(t *Threshold, typ *string, lo, hi **float64) {
				if t == nil {
					return
				}
				*typ = t.Type
				v := t.Value
				if strings.Contains(t.Type, ">") {
					*hi = &v
				} else {
					*lo = &v
				}
			}
			setT(ps.Warn, &d.WarnType, &d.WarnLower, &d.WarnUpper)
			setT(ps.Crit, &d.CritType, &d.CritLower, &d.CritUpper)
			defs[ps.Alias] = d
		}
		p.defs[m.Name] = defs
	}
	idx := map[string]int{}
	for _, e := range equipment() {
		m := p.Models[e.Model]
		a := newAsset(e, m, idx[e.Group], rand.New(rand.NewPCG(r.Uint64(), r.Uint64())))
		idx[e.Group]++
		p.addAsset(a)
	}
	return p
}

// addAsset registers an asset (in equipment order).
func (p *Plant) addAsset(a *Asset) {
	p.Assets = append(p.Assets, a)
	p.ByAlias[a.Spec.Alias] = a
	p.groups[a.Spec.Group] = append(p.groups[a.Spec.Group], a)
	if a.Kind == "chws" {
		p.sys = a
	}
}

// pick returns a random asset accepted by ok (nil accepts any).
func (p *Plant) pick(r *rand.Rand, ok func(*Asset) bool) *Asset {
	for {
		a := p.Assets[r.IntN(len(p.Assets))]
		if ok == nil || ok(a) {
			return a
		}
	}
}

// physical reports whether an asset is real equipment (not a BMS system view).
func physical(a *Asset) bool { return a.Spec.Group != "system" }

// group returns the assets of an equipment group in equipment order (shared
// slice: do not modify).
func (p *Plant) group(g string) []*Asset { return p.groups[g] }

// Run modes of equipment with a run_mode control (chillers): Auto leaves the
// unit to the duty controller, On (hand) runs it, Off locks it out.
const (
	runAuto = 0
	runOn   = 1
	runOff  = 2
)

// updateDuty runs the duty/standby controllers: the plant controller's duty
// chiller count (load staging, normally N+1) with a weekly rotation of the
// units.
func (p *Plant) updateDuty(t time.Time) {
	week := int(t.Sub(p.anchor).Hours() / 168)
	if week < 0 {
		week = -week
	}
	assign := func(g string, need int, available func(*Asset) bool) int {
		members := p.group(g)
		n := len(members)
		ok := func(a *Asset) bool { return !a.Trip && !a.Maint && (available == nil || available(a)) }
		running := 0
		// units switched On by an operator run first (even beyond the duty
		// count); units switched Off never run; Auto units fill the duty in
		// rotation order
		for _, a := range members {
			a.Duty = a.ctl("run_mode", runAuto) == runOn && ok(a)
			if a.Duty {
				running++
			}
		}
		for i := 0; i < n; i++ {
			a := members[(i+week)%n]
			if a.Duty || a.ctl("run_mode", runAuto) != runAuto {
				continue
			}
			if running < need && ok(a) {
				a.Duty = true
				running++
			}
		}
		return running
	}
	// A chiller can only run with its dedicated CHW and CW pumps available
	// (evaporator / condenser flow switch interlocks); otherwise the standby
	// chiller takes over.
	chwp, cwp := p.group("chwp"), p.group("cwp")
	pumpsOK := func(a *Asset) bool {
		for _, g := range [][]*Asset{chwp, cwp} {
			if a.Index < len(g) && (g[a.Index].Trip || g[a.Index].Maint) {
				return false
			}
		}
		return true
	}
	ch := assign("chillers", p.dutyChillers(), pumpsOK)
	assign("towers", ch, nil)
	assign("pcw", 3, nil)
	// Dedicated primary pumps: CHWP-0n / CWP-0n run with chiller CH-0n.
	chillers := p.group("chillers")
	for _, g := range []string{"chwp", "cwp"} {
		for i, a := range p.group(g) {
			a.Duty = i < len(chillers) && chillers[i].Duty && !a.Trip && !a.Maint
		}
	}
	for _, g := range []string{"cda", "mau", "vac", "system"} {
		for _, a := range p.group(g) {
			a.Duty = true
		}
	}
}

// Step advances the plant by dt seconds to t and returns the measured values
// of every asset (sensor faults applied); the true values stay in p.Truth.
// Order: weather and load, duty (staging), the plant controller and its
// loops, the equipment physics, the plant-level points, the sensors.
func (p *Plant) Step(t time.Time, dt float64) map[*Asset]map[string]float64 {
	env := p.weather.at(t, p.loc, dt)
	if p.EnvHook != nil {
		p.EnvHook(t, &env)
	}
	env.FabLoad = env.Load * 2 * chillerCapKW
	// the fab coils see the CHW supply and riser DP of the previous step
	env.CHWS, env.RiserDP = defCHWSP, defRiserDPSP
	if s := p.sys; s != nil && s.last != nil {
		env.CHWS, env.RiserDP = s.last["chws_temp"], s.last["riser_dp"]
	}
	p.Env = env
	p.updateDuty(t)
	nCh := 0
	for _, a := range p.group("chillers") {
		if a.Running() {
			nCh++
		}
	}
	env.ChillerLoad = clamp(env.FabLoad/(float64(max(nCh, 1))*chillerCapKW), 0.2, 1)
	p.supervise(t, &env, dt)
	p.Env = env
	truth := make(map[*Asset]map[string]float64, len(p.Assets))
	for _, a := range p.Assets {
		truth[a] = a.Values(t, env, dt)
	}
	// plant-level (BMS) points are derived from the equipment values
	if s := p.sys; s != nil {
		s.chwSystem(truth[s], p, truth, env, dt)
	}
	meas := make(map[*Asset]map[string]float64, len(p.Assets))
	for _, a := range p.Assets {
		a.meas = a.sense(truth[a])
		meas[a] = a.meas
	}
	p.Truth = truth
	return meas
}

// ---------------------------------------------------------------------------
// Faults, trips and communication losses (shared by the live simulation, the
// history and the simulation jobs)

// evolution lists what changed while faults, trips and communication evolved.
type evolution struct {
	Tripped, TripReset, Restored []*Asset
}

// evolve advances fault severities, the MAU filter loading, communication
// losses and trips by dt seconds to now; tripFor draws how long a new trip
// lasts until the operator resets it.
func (p *Plant) evolve(now time.Time, dt float64, tripFor func(a *Asset) time.Duration) evolution {
	var ev evolution
	for _, a := range p.Assets {
		for _, f := range a.Faults {
			f.Sev = math.Min(maxSeverity, f.Sev+f.Rate*dt)
		}
		if a.Kind == "mau" {
			a.FilterDays += dt / 86400
		}
		if a.Offline && now.After(a.OffUntil) {
			a.Offline = false
			ev.Restored = append(ev.Restored, a)
		}
		if !a.SpikeUntil.IsZero() && now.After(a.SpikeUntil) {
			a.SpikeUntil = time.Time{}
			a.Trip = true
			a.TripUntil = now.Add(tripFor(a))
			ev.Tripped = append(ev.Tripped, a)
		}
		if a.Trip && !a.TripUntil.IsZero() && now.After(a.TripUntil) {
			a.Trip = false
			a.TripUntil = time.Time{}
			ev.TripReset = append(ev.TripReset, a)
		}
	}
	return ev
}

// injectRandomFault starts a random developing fault (never a Manual one) on
// a random running asset that does not have it yet; it develops fully in
// rampMin .. rampMax minutes.
func (p *Plant) injectRandomFault(r *rand.Rand, now time.Time, rampMin, rampMax float64) (*Asset, *FaultSpec, float64) {
	for tries := 0; tries < 20; tries++ {
		a := p.Assets[r.IntN(len(p.Assets))]
		cands := a.Model.randomFaults()
		if !a.Running() || len(cands) == 0 {
			continue
		}
		f := cands[r.IntN(len(cands))]
		if a.Faults[f.Kind] != nil {
			continue
		}
		ramp := (rampMin + r.Float64()*(rampMax-rampMin)) * 60
		a.Faults[f.Kind] = &Fault{Kind: f.Kind, Rate: 1.25 / ramp, Start: now}
		return a, a.Model.faultSpec(f.Kind), ramp
	}
	return nil, nil, 0
}

// injectRandomTrip makes a random running asset spike and trip 45 s later.
func (p *Plant) injectRandomTrip(r *rand.Rand, now time.Time) *Asset {
	for tries := 0; tries < 20; tries++ {
		a := p.Assets[r.IntN(len(p.Assets))]
		if !a.Running() || a.Trip || len(a.Model.Trips) == 0 {
			continue
		}
		a.startTrip(now)
		return a
	}
	return nil
}

// startTrip starts the overload spike that trips the asset 45 s later.
func (a *Asset) startTrip(now time.Time) bool {
	if len(a.Model.Trips) == 0 {
		return false
	}
	ts := a.Model.Trips[0]
	a.TripSpec = &ts
	a.SpikeUntil = now.Add(45 * time.Second)
	return true
}

// injectFault starts a fault of the asset's model on demand. A fixed-
// intensity (LBNL) fault takes value as its intensity, which must lie within
// the documented intensities; a developing fault takes value as a fixed
// severity (0 .. maxSeverity), or develops from 0 at rate when value is 0.
func (a *Asset) injectFault(kind string, value float64, now time.Time, rate float64) (*Fault, error) {
	fs := a.Model.faultSpec(kind)
	if fs == nil {
		return nil, fmt.Errorf("%s has no fault %q", a.Spec.Alias, kind)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("invalid %s value", kind)
	}
	f := &Fault{Kind: kind, Start: now}
	if len(fs.Intensities) > 0 {
		lo, hi := fs.Intensities[0], fs.Intensities[0]
		for _, x := range fs.Intensities {
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
		if value == 0 || value < lo-1e-9 || value > hi+1e-9 {
			return nil, fmt.Errorf("%s intensity %v outside %v .. %v", kind, value, fnum(lo), fnum(hi))
		}
		f.Value, f.Sev = value, 1
	} else {
		if value < 0 || value > maxSeverity {
			return nil, fmt.Errorf("%s severity %v outside 0 .. %v", kind, value, maxSeverity)
		}
		f.Sev = value
		if value == 0 {
			f.Rate = rate
		}
	}
	a.Faults[kind] = f
	return f, nil
}

// controlSpec returns the parameter definition of a writable parameter.
func (a *Asset) controlSpec(alias string) *ParamSpec {
	for i := range a.Model.Params {
		if p := &a.Model.Params[i]; p.Alias == alias && p.Ctl != nil {
			return p
		}
	}
	return nil
}

// applyControl applies an operator write (set point or control mode) after
// checking it against the model's control definition.
func (a *Asset) applyControl(alias string, value float64) error {
	ps := a.controlSpec(alias)
	if ps == nil {
		return fmt.Errorf("%s is not writable", alias)
	}
	c := ps.Ctl
	if len(c.Options) > 0 {
		valid := false
		for _, o := range c.Options {
			valid = valid || o.Value == value
		}
		if !valid {
			return fmt.Errorf("%v is not an option of %s", value, alias)
		}
		// bumpless transfer: a set point taken out of automatic starts from
		// the value in effect (the controller output while running)
		if value != 0 && !a.manual(alias) {
			for i := range a.Model.Params {
				sp := &a.Model.Params[i]
				if sp.Ctl == nil || sp.Ctl.Mode != alias {
					continue
				}
				cur := a.last[sp.Alias]
				if cur < sp.Ctl.Min || cur > sp.Ctl.Max {
					// stopped (no set point in effect): the previous manual
					// value, else the default (design speed …), else mid-range
					def := sp.Ctl.Default
					if def == 0 {
						def = (sp.Ctl.Min + sp.Ctl.Max) / 2
					}
					cur = a.ctl(sp.Alias, def)
				}
				if sp.Ctl.Step > 0 {
					cur = sp.Ctl.Min + math.Round((cur-sp.Ctl.Min)/sp.Ctl.Step)*sp.Ctl.Step
				}
				a.Ctl[sp.Alias] = round(clamp(cur, sp.Ctl.Min, sp.Ctl.Max), sp.Scale)
			}
		}
	} else {
		if value < c.Min-1e-9 || value > c.Max+1e-9 {
			return fmt.Errorf("%s %v outside %v..%v", alias, value, c.Min, c.Max)
		}
		if c.Mode != "" && !a.manual(c.Mode) {
			return fmt.Errorf("%s is in automatic", c.Mode)
		}
		value = round(value, ps.Scale)
	}
	a.Ctl[alias] = value
	return nil
}

// alarms returns the effective alarms of a model (base alarms plus overrides).
func effectiveAlarms(m *ModelSpec) []AlarmSpec {
	out := make([]AlarmSpec, 0, len(m.Alarms))
	for _, a := range m.Alarms {
		if c, ok := m.Overrides[a.Code]; ok {
			a.Cond = c
		}
		out = append(out, a)
	}
	return out
}

type hit struct {
	Severity string
	Message  string
	Value    float64
}

// Evaluation mirrors the backend engine for one sample.
type Evaluation struct {
	Status   int
	Alarms   map[string]hit // code -> severity
	Abnormal map[string]hit // alias -> severity
	Cond     string
}

func (p *Plant) evaluate(a *Asset, v map[string]float64, offline bool, forced int) Evaluation {
	ev := Evaluation{Alarms: map[string]hit{}, Abnormal: map[string]hit{}}
	switch {
	case offline:
		ev.Status = engine.StatusOffline
	case v["trip"] == 1:
		ev.Status = engine.StatusUnplannedDown
	case v["maint_mode"] == 1:
		ev.Status = engine.StatusPlannedDown
	case v["running"] == 1:
		ev.Status = engine.StatusRunning
	default:
		ev.Status = engine.StatusIdle
	}
	if offline {
		ev.Cond = engine.CondOffline
		return ev
	}
	if forced > 0 {
		ev.Status = forced
	}
	for _, al := range effectiveAlarms(a.Model) {
		if al.Cond.Eval(v) {
			sev := "WARNING"
			if al.Critical {
				sev = "CRITICAL"
			}
			ev.Alarms[al.Code] = hit{Severity: sev, Message: al.Message}
		}
	}
	defs := p.defs[a.Model.Name]
	for _, ps := range a.Model.Params {
		if ps.Bool || (ps.Warn == nil && ps.Crit == nil) {
			continue
		}
		x := round(v[ps.Alias], ps.Scale)
		cond := engine.ParamCondition(defs[ps.Alias], x)
		if cond == engine.CondNormal {
			continue
		}
		val := fmt.Sprintf("%.*f", ps.Scale, x)
		if ps.Unit != "" {
			val += " " + ps.Unit
		}
		ev.Abnormal[ps.Alias] = hit{Severity: cond, Value: x,
			Message: fmt.Sprintf("%s is %s %s", ps.Name, strings.ToLower(cond), val)}
	}
	ev.Cond = engine.CondNormal
	for _, list := range []map[string]hit{ev.Alarms, ev.Abnormal} {
		for _, h := range list {
			if h.Severity == "CRITICAL" {
				ev.Cond = engine.CondCritical
			} else if ev.Cond == engine.CondNormal {
				ev.Cond = engine.CondWarning
			}
		}
	}
	return ev
}

// Diagnosis is an issue relation firing (mirrors engine.EarlyWarning).
type Diagnosis struct {
	Rel        *RelationSpec
	IsCompound bool
	IsDeduct   bool
	Severity   string
	Params     []string
	Codes      []string
}

func (p *Plant) diagnose(a *Asset, ev Evaluation) []Diagnosis {
	var out []Diagnosis
	for i := range a.Model.Relations {
		rel := &a.Model.Relations[i]
		d := Diagnosis{Rel: rel, Severity: "WARNING"}
		active := false
		for _, path := range rel.Paths {
			ok := true
			for _, in := range path {
				if !inputActive(in, ev) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			active = true
			if len(path) > 1 {
				d.IsCompound = true
			}
			for _, in := range path {
				kind, name := in[:1], in[2:]
				var h hit
				if kind == "P" {
					d.IsDeduct = true
					d.Params = appendUnique(d.Params, name)
					h = ev.Abnormal[name]
				} else {
					d.Codes = appendUnique(d.Codes, name)
					h = ev.Alarms[name]
				}
				if h.Severity == "CRITICAL" {
					d.Severity = "CRITICAL"
				}
			}
		}
		if active {
			sort.Strings(d.Params)
			out = append(out, d)
		}
	}
	return out
}

func inputActive(in string, ev Evaluation) bool {
	if strings.HasPrefix(in, "P:") {
		_, ok := ev.Abnormal[in[2:]]
		return ok
	}
	_, ok := ev.Alarms[in[2:]]
	return ok
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// faultsRepairedBy returns the faults of an asset cleared by maintenance on codes.
func faultsRepairedBy(a *Asset, codes []string) []string {
	var out []string
	for _, f := range a.Model.Faults {
		if a.Faults[f.Kind] == nil {
			continue
		}
		for _, c := range f.Codes {
			for _, oc := range codes {
				if c == oc {
					out = appendUnique(out, f.Kind)
				}
			}
		}
	}
	return out
}

// repair clears faults and wear after completed maintenance.
func repair(a *Asset, codes []string, preventive bool) []string {
	kinds := faultsRepairedBy(a, codes)
	for _, k := range kinds {
		delete(a.Faults, k)
	}
	if preventive || contains(codes, "AHU-010") || contains(codes, "PM-MAU") {
		a.FilterDays = 0
	}
	return kinds
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
