package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"afdd/internal/engine"
)

// ---------------------------------------------------------------------------
// Digital twin simulation jobs (docs/agentic/DESIGN.md 5): what-if scenarios
// (variants of actions on clones of the live or a nominal plant) and designs
// of experiments for the optimizer's surrogate. The runner claims QUEUED jobs
// from sim.jobs, runs them in simulated time and writes sim.* only: never
// ts.*, dm.*, core.notifications or MQTT.
// ---------------------------------------------------------------------------

// ScenarioSpec is the spec of a scenario job (DESIGN 5.1).
type ScenarioSpec struct {
	Start       *time.Time `json:"start"`        // null = now
	DurationMin float64    `json:"duration_min"` // 10 .. 2880
	Initial     string     `json:"initial"`      // live | nominal
	Stochastic  bool       `json:"stochastic"`   // random faults, trips and comm losses
	Seed        int64      `json:"seed"`         // 0 = derived from the job id
	Weather     struct {
		DryBulbOffset float64 `json:"dry_bulb_offset"`
		WetBulbOffset float64 `json:"wet_bulb_offset"`
	} `json:"weather"`
	LoadScale float64       `json:"load_scale"` // 0.5 .. 1.3 (0 = 1)
	Actions   []SimAction   `json:"actions"`    // common to every variant
	Variants  []VariantSpec `json:"variants"`
}

// VariantSpec is one variant of a scenario.
type VariantSpec struct {
	Name    string      `json:"name"`
	Actions []SimAction `json:"actions"`
}

// SimAction is a scheduled change of a scenario.
type SimAction struct {
	AtMin       float64  `json:"at_min"`
	Kind        string   `json:"kind"` // control | fault | clear_fault | trip | offline | maintenance | release
	Asset       string   `json:"asset"`
	Param       string   `json:"param"`
	Fault       string   `json:"fault"`
	Value       *float64 `json:"value"`
	DurationMin float64  `json:"duration_min"`
}

// Spec limits (DESIGN 4.2).
const (
	maxVariants   = 3
	maxActions    = 200
	minJobMinutes = 10
	maxJobMinutes = 2880 // jobs of people and the agent (the API's limit)
	// maxSystemJobMinutes is the limit of the analytics service's and the
	// optimizer's own jobs (the 72 h fault-free calibration run, DESIGN 6.1).
	maxSystemJobMinutes = 4320
)

// jobMaxMinutes is the longest scenario a job of an origin may run.
func jobMaxMinutes(origin string) float64 {
	if origin == "analytics" || origin == "optimizer" {
		return maxSystemJobMinutes
	}
	return maxJobMinutes
}

var actionKinds = []string{"control", "fault", "clear_fault", "trip", "offline", "maintenance", "release"}

// parseScenario decodes and validates a scenario spec against the plant; a
// run may last up to maxMin minutes.
func parseScenario(raw []byte, p *Plant, maxMin float64) (*ScenarioSpec, error) {
	var s ScenarioSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	if s.DurationMin < minJobMinutes || s.DurationMin > maxMin {
		return nil, fmt.Errorf("duration_min %v outside %d .. %v", s.DurationMin, minJobMinutes, maxMin)
	}
	switch s.Initial {
	case "":
		s.Initial = "live"
	case "live", "nominal":
	default:
		return nil, fmt.Errorf("initial %q is not live or nominal", s.Initial)
	}
	if s.LoadScale == 0 {
		s.LoadScale = 1
	}
	if s.LoadScale < 0.5 || s.LoadScale > 1.3 {
		return nil, fmt.Errorf("load_scale %v outside 0.5 .. 1.3", s.LoadScale)
	}
	if math.Abs(s.Weather.DryBulbOffset) > 10 || math.Abs(s.Weather.WetBulbOffset) > 10 {
		return nil, errors.New("weather offsets beyond ±10 K")
	}
	if len(s.Variants) == 0 {
		s.Variants = []VariantSpec{{Name: "baseline"}}
	}
	if len(s.Variants) > maxVariants {
		return nil, fmt.Errorf("%d variants (at most %d)", len(s.Variants), maxVariants)
	}
	n := len(s.Actions)
	names := map[string]bool{}
	for i := range s.Variants {
		v := &s.Variants[i]
		v.Name = strings.TrimSpace(v.Name)
		if v.Name == "" || names[v.Name] {
			return nil, fmt.Errorf("variant %d: missing or duplicate name %q", i+1, v.Name)
		}
		names[v.Name] = true
		n += len(v.Actions)
	}
	if n > maxActions {
		return nil, fmt.Errorf("%d actions (at most %d)", n, maxActions)
	}
	check := func(where string, list []SimAction) error {
		for i, a := range list {
			if err := a.validate(p, s.DurationMin); err != nil {
				return fmt.Errorf("%s action %d: %w", where, i+1, err)
			}
		}
		return nil
	}
	if err := check("common", s.Actions); err != nil {
		return nil, err
	}
	for _, v := range s.Variants {
		if err := check("variant "+v.Name, v.Actions); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// validate checks an action against the plant's equipment, writable
// parameters and fault catalog.
func (a SimAction) validate(p *Plant, duration float64) error {
	if !contains(actionKinds, a.Kind) {
		return fmt.Errorf("unknown kind %q", a.Kind)
	}
	if a.AtMin < 0 || a.AtMin > duration || a.DurationMin < 0 {
		return fmt.Errorf("at_min %v / duration_min %v outside the run", a.AtMin, a.DurationMin)
	}
	as := p.ByAlias[a.Asset]
	if as == nil {
		return fmt.Errorf("unknown asset %q", a.Asset)
	}
	switch a.Kind {
	case "control":
		if as.controlSpec(a.Param) == nil {
			return fmt.Errorf("%s.%s is not writable", a.Asset, a.Param)
		}
		if a.Value == nil {
			return errors.New("control needs a value")
		}
	case "fault", "clear_fault":
		if as.Model.faultSpec(a.Fault) == nil {
			return fmt.Errorf("%s has no fault %q", a.Asset, a.Fault)
		}
	case "trip":
		if len(as.Model.Trips) == 0 {
			return fmt.Errorf("%s cannot trip", a.Asset)
		}
	}
	return nil
}

// JobOptions are the random-event rates of stochastic scenarios (those of
// the live simulation).
type JobOptions struct {
	FaultEvery, TripEvery, OfflineEvery time.Duration
	MaxFaults                           int
	RampMin, RampMax                    float64
}

func jobOptions(o LiveOptions) JobOptions {
	return JobOptions{FaultEvery: o.FaultEvery, TripEvery: o.TripEvery, OfflineEvery: o.OfflineEvry, MaxFaults: o.MaxFaults,
		RampMin: o.RampMin, RampMax: o.RampMax}
}

func defaultJobOptions() JobOptions {
	return JobOptions{FaultEvery: 45 * time.Minute, TripEvery: 4 * time.Hour, OfflineEvery: 8 * time.Hour, MaxFaults: 4, RampMin: 20, RampMax: 60}
}

// ---------------------------------------------------------------------------
// Output

// SeriesRow is one measured series of a variant (NaN = no value, e.g. no
// communication); Truth is set only where the true values differ.
type SeriesRow struct {
	Variant, Asset, Param string
	T0                    time.Time
	StepS                 int
	Measured, Truth       []float32
}

// StateRow is a status or condition interval.
type StateRow struct {
	Variant, Asset, Kind, Value string
	Start, End                  time.Time
}

// EventRow is a simulation event.
type EventRow struct {
	Variant  string
	Ts       time.Time
	End      *time.Time
	Asset    string
	Kind     string // status | condition | alarm | abnormal | diagnosis | control | fault_injected | fault_cleared | trip | note
	Code     string
	Severity string
	Message  string
	Value    *float64
	Details  map[string]any
}

// DOESampleRow is one design-of-experiments sample.
type DOESampleRow struct {
	Idx             int
	Inputs, Outputs map[string]float64
}

// Recorder receives the output of a simulation job.
type Recorder interface {
	Series(SeriesRow)
	State(StateRow)
	Event(EventRow)
	Sample(DOESampleRow)
}

// MemRecorder keeps a job's output in memory (written to sim.* at the end of
// the job in one transaction; tests read it directly).
type MemRecorder struct {
	SeriesRows []SeriesRow
	StateRows  []StateRow
	EventRows  []EventRow
	Samples    []DOESampleRow
}

func (m *MemRecorder) Series(r SeriesRow)    { m.SeriesRows = append(m.SeriesRows, r) }
func (m *MemRecorder) State(r StateRow)      { m.StateRows = append(m.StateRows, r) }
func (m *MemRecorder) Event(r EventRow)      { m.EventRows = append(m.EventRows, r) }
func (m *MemRecorder) Sample(r DOESampleRow) { m.Samples = append(m.Samples, r) }

// ---------------------------------------------------------------------------
// Scenario execution (no database)

// scenarioRun is everything a scenario run needs.
type scenarioRun struct {
	Spec     *ScenarioSpec
	Base     *Plant    // initial state (cloned for every variant)
	Seed     uint64    // random sources of the clones and the scenario
	Start    time.Time // first simulated instant
	StepS    int
	Opts     JobOptions
	Rec      Recorder
	Progress func(done float64) // fraction of the job done (called often)
}

// steps is the number of recorded steps of a variant.
func (r *scenarioRun) steps() int { return int(math.Round(r.Spec.DurationMin * 60 / float64(r.StepS))) }

// run runs every variant on a fresh clone of the base plant and returns the
// job summary {variants: {name: KPIs}, ...}.
func (r *scenarioRun) run(ctx context.Context) (map[string]any, error) {
	variants := map[string]any{}
	for i, v := range r.Spec.Variants {
		p := r.Base.Clone(r.Seed)
		k, err := r.variant(ctx, p, v, func(f float64) {
			if r.Progress != nil {
				r.Progress((float64(i) + f) / float64(len(r.Spec.Variants)))
			}
		})
		if err != nil {
			return nil, err
		}
		variants[v.Name] = k
	}
	return map[string]any{"variants": variants, "seed": int64(r.Seed), "initial": r.Spec.Initial, "step_s": r.StepS,
		"steps": r.steps()}, nil
}

// pendingAction is an action of a variant at its simulated time.
type pendingAction struct {
	at  time.Time
	act SimAction
	// revert of an action with a duration: clear the fault, release the
	// maintenance, restore the previous control value
	revert bool
	old    float64
}

// actionRank orders the actions due at the same simulated instant: reverts
// first (a timed action ends before one that starts at its end), set point
// reverts before the other reverts (a set point is restored while its
// control mode is still manual), then new control modes before every other
// action (a set point is refused while its mode is in automatic). Within a
// rank the spec's order holds (common actions, then the variant's).
func actionRank(p *Plant, pa pendingAction) int {
	var ctl *ControlSpec
	if a := p.ByAlias[pa.act.Asset]; a != nil && pa.act.Kind == "control" {
		if ps := a.controlSpec(pa.act.Param); ps != nil {
			ctl = ps.Ctl
		}
	}
	switch {
	case pa.revert && ctl != nil && ctl.Mode != "":
		return 0
	case pa.revert:
		return 1
	case ctl != nil && len(ctl.Options) > 0:
		return 2
	}
	return 3
}

// actionBefore reports whether the pending action x runs before y.
func actionBefore(p *Plant, x, y pendingAction) bool {
	if !x.at.Equal(y.at) {
		return x.at.Before(y.at)
	}
	return actionRank(p, x) < actionRank(p, y)
}

// variant runs one variant and returns its physical KPIs.
func (r *scenarioRun) variant(ctx context.Context, p *Plant, v VariantSpec, progress func(float64)) (map[string]any, error) {
	steps, step := r.steps(), time.Duration(r.StepS)*time.Second
	dt := float64(r.StepS)
	end := r.Start.Add(time.Duration(steps) * step)
	rng := rand.New(rand.NewPCG(r.Seed, r.Seed^0x51ab5eed))
	uniform := func(a, b float64) float64 { return a + rng.Float64()*(b-a) }
	expo := func(mean time.Duration) time.Duration {
		return time.Duration(-math.Log(1-rng.Float64()) * float64(mean))
	}
	spec := r.Spec
	p.EnvHook = func(_ time.Time, env *Env) {
		env.DryBulb += spec.Weather.DryBulbOffset
		env.WetBulb += spec.Weather.WetBulbOffset
		if env.WetBulb > env.DryBulb-0.5 {
			env.WetBulb = env.DryBulb - 0.5
		}
		env.RH = rhOf(env.DryBulb, env.WetBulb)
		env.Load = clamp(env.Load*spec.LoadScale, 0.2, 1.4)
	}
	var queue []pendingAction
	for _, list := range [][]SimAction{spec.Actions, v.Actions} {
		for _, a := range list {
			queue = append(queue, pendingAction{at: r.Start.Add(time.Duration(a.AtMin * float64(time.Minute))), act: a})
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return actionBefore(p, queue[i], queue[j]) })

	rec := newVariantRecorder(p, v.Name, r.Start, r.StepS, steps, r.Rec)
	tripDur := map[*Asset]time.Duration{}
	tripFor := func(a *Asset) time.Duration {
		if d, ok := tripDur[a]; ok {
			delete(tripDur, a)
			return d
		}
		return time.Duration(uniform(25, 45) * float64(time.Minute))
	}
	nextF, nextT, nextO := r.Start.Add(expo(r.Opts.FaultEvery)/2), r.Start.Add(expo(r.Opts.TripEvery)), r.Start.Add(expo(r.Opts.OfflineEvery))
	rate := 1.25 / (math.Max(r.Opts.RampMax, 1) * 60)

	for k := 0; k < steps; k++ {
		if k%120 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			progress(float64(k) / float64(steps))
		}
		t := r.Start.Add(time.Duration(k) * step)
		// actions due at this step (reverts are queued in order)
		for len(queue) > 0 && !queue[0].at.After(t) {
			pa := queue[0]
			queue = queue[1:]
			if next := r.apply(p, pa, t, rate, rec, tripDur); next != nil {
				i := sort.Search(len(queue), func(i int) bool { return actionBefore(p, *next, queue[i]) })
				queue = append(queue[:i], append([]pendingAction{*next}, queue[i:]...)...)
			}
		}
		// faults, trips and communication evolve in simulated time
		ev := p.evolve(t, dt, tripFor)
		for _, a := range ev.Tripped {
			rec.event(EventRow{Ts: t, Asset: a.Spec.Alias, Kind: "trip", Code: a.TripSpec.Code, Severity: "CRITICAL", Message: a.TripSpec.Reason,
				Details: map[string]any{"until": a.TripUntil.UTC().Format(time.RFC3339)}})
			rec.trips++
		}
		if spec.Stochastic {
			r.randomEvents(p, rng, t, &nextF, &nextT, &nextO, expo, uniform, rec)
		}
		meas := p.Step(t, dt)
		rec.record(k, t, meas, p.Truth)
	}
	progress(1)
	return rec.finish(end), nil
}

// randomEvents injects the random faults, trips and communication losses of
// a stochastic scenario (the live simulation's rates).
func (r *scenarioRun) randomEvents(p *Plant, rng *rand.Rand, t time.Time, nextF, nextT, nextO *time.Time,
	expo func(time.Duration) time.Duration, uniform func(a, b float64) float64, rec *variantRecorder) {
	if t.After(*nextF) {
		*nextF = t.Add(expo(r.Opts.FaultEvery))
		n := 0
		for _, a := range p.Assets {
			n += len(a.Faults)
		}
		if n < r.Opts.MaxFaults {
			if a, f, ramp := p.injectRandomFault(rng, t, r.Opts.RampMin, r.Opts.RampMax); a != nil {
				rec.event(EventRow{Ts: t, Asset: a.Spec.Alias, Kind: "fault_injected", Code: f.Kind, Message: f.Label + " started (random)",
					Details: map[string]any{"label": f.Label, "codes": f.Codes, "ramp_minutes": math.Round(ramp / 60), "random": true}})
			}
		}
	}
	if t.After(*nextT) {
		*nextT = t.Add(expo(r.Opts.TripEvery))
		p.injectRandomTrip(rng, t)
	}
	if t.After(*nextO) {
		*nextO = t.Add(expo(r.Opts.OfflineEvery))
		a := p.Assets[rng.IntN(len(p.Assets))]
		a.Offline, a.OffUntil = true, t.Add(time.Duration(uniform(7, 15)*float64(time.Minute)))
	}
}

// apply applies an action (or the revert of one) at simulated time t and
// returns the revert to schedule for an action with a duration.
func (r *scenarioRun) apply(p *Plant, pa pendingAction, t time.Time, rate float64, rec *variantRecorder, tripDur map[*Asset]time.Duration) *pendingAction {
	act := pa.act
	a := p.ByAlias[act.Asset]
	if a == nil {
		return nil
	}
	value := 0.0
	if act.Value != nil {
		value = *act.Value
	}
	var revert *pendingAction
	later := func(old float64) {
		if act.DurationMin > 0 && !pa.revert {
			revert = &pendingAction{at: t.Add(time.Duration(act.DurationMin * float64(time.Minute))), act: act, revert: true, old: old}
		}
	}
	note := func(msg string, details map[string]any) {
		rec.event(EventRow{Ts: t, Asset: act.Asset, Kind: "note", Message: msg, Details: details})
	}
	switch act.Kind {
	case "control":
		ps := a.controlSpec(act.Param)
		if ps == nil {
			return nil
		}
		if pa.revert && len(ps.Ctl.Options) == 0 {
			// Nothing to restore when the set point's mode went back to
			// automatic first (the controller sets it), or when no set point
			// was in effect as the action ran (a unit stopped in automatic
			// reports 0): restoring would only be refused.
			if (ps.Ctl.Mode != "" && !a.manual(ps.Ctl.Mode)) || pa.old < ps.Ctl.Min-1e-9 || pa.old > ps.Ctl.Max+1e-9 {
				return nil
			}
		}
		old := a.ctl(act.Param, 0)
		if a.last != nil {
			if v, ok := a.last[act.Param]; ok {
				old = v
			}
		}
		if pa.revert {
			value = pa.old
		}
		details := map[string]any{"param": act.Param, "name": ps.Name, "unit": ps.Unit, "old": old, "value": value, "revert": pa.revert}
		if err := a.applyControl(act.Param, value); err != nil {
			details["status"], details["reason"] = "REJECTED", err.Error()
			rec.event(EventRow{Ts: t, Asset: act.Asset, Kind: "control", Code: act.Param, Severity: "WARNING", Value: &value,
				Message: fmt.Sprintf("%s of %s: %s → %s rejected (%v)", ps.Name, act.Asset, fnum(old), fnum(value), err), Details: details})
			return nil
		}
		details["status"] = "APPLIED"
		rec.event(EventRow{Ts: t, Asset: act.Asset, Kind: "control", Code: act.Param, Value: &value,
			Message: fmt.Sprintf("%s of %s: %s → %s %s", ps.Name, act.Asset, fnum(old), fnum(value), ps.Unit), Details: details})
		later(old)
	case "fault", "clear_fault":
		fs := a.Model.faultSpec(act.Fault)
		if fs == nil {
			return nil
		}
		if act.Kind == "clear_fault" || pa.revert {
			if a.Faults[act.Fault] == nil {
				return nil
			}
			delete(a.Faults, act.Fault)
			rec.event(EventRow{Ts: t, Asset: act.Asset, Kind: "fault_cleared", Code: act.Fault, Message: fs.Label + " cleared",
				Details: map[string]any{"label": fs.Label}})
			return nil
		}
		f, err := a.injectFault(act.Fault, value, t, rate)
		if err != nil {
			note(fmt.Sprintf("Fault %s on %s not injected: %v", act.Fault, act.Asset, err), map[string]any{"fault": act.Fault, "error": err.Error()})
			return nil
		}
		x := f.Value
		if len(fs.Intensities) == 0 {
			x = f.Sev
		}
		rec.event(EventRow{Ts: t, Asset: act.Asset, Kind: "fault_injected", Code: act.Fault, Value: &x, Message: fs.Label + " injected",
			Details: map[string]any{"label": fs.Label, "codes": fs.Codes, "unit": fs.Unit, "value": value, "manual": fs.Manual}})
		later(0)
	case "trip":
		if a.Trip || !a.SpikeUntil.IsZero() || !a.startTrip(t) {
			return nil
		}
		if act.DurationMin > 0 {
			tripDur[a] = time.Duration(act.DurationMin * float64(time.Minute))
		}
	case "offline":
		d := act.DurationMin
		if d <= 0 {
			d = 10
		}
		a.Offline, a.OffUntil = true, t.Add(time.Duration(d*float64(time.Minute)))
		note(fmt.Sprintf("Communication with %s lost for %s min", act.Asset, fnum(d)), map[string]any{"until": a.OffUntil.UTC().Format(time.RFC3339)})
	case "maintenance", "release":
		if act.Kind == "release" || pa.revert {
			if !a.Maint {
				return nil
			}
			a.Maint, a.Forced = false, 0
			note(act.Asset+" released after maintenance", nil)
			return nil
		}
		a.Maint, a.Forced = true, engine.StatusPlannedDown
		note(act.Asset+" stopped for maintenance", nil)
		later(0)
	}
	return revert
}

// ---------------------------------------------------------------------------
// Variant recording: series, status / condition intervals, debounced alarm
// and abnormal events, diagnoses and the physical KPIs.

type assetSeries struct {
	params        []ParamSpec
	meas, truth   [][]float32
	differs       []bool
	status, cond  string
	statusStart   time.Time
	condStart     time.Time
	pending       map[string]int
	open          map[string]*openSimEvent
	diag          map[string]*openSimEvent
	diagMissCount map[string]int
}

type openSimEvent struct {
	row    EventRow
	misses int
}

type variantRecorder struct {
	p      *Plant
	name   string
	t0     time.Time
	stepS  int
	steps  int
	out    Recorder
	assets []*Asset
	series map[*Asset]*assetSeries

	// KPIs (true values of the plant controller)
	n                             int
	energy, sumKW, sumRT          float64
	sumCHWS, maxCHWS, peakLoad    float64
	minDP, maxValve               float64
	alarmWarn, alarmCrit, changes int
	trips                         int
}

func newVariantRecorder(p *Plant, name string, t0 time.Time, stepS, steps int, out Recorder) *variantRecorder {
	r := &variantRecorder{p: p, name: name, t0: t0, stepS: stepS, steps: steps, out: out, assets: p.Assets,
		series: make(map[*Asset]*assetSeries, len(p.Assets)), minDP: math.Inf(1), maxCHWS: math.Inf(-1)}
	for _, a := range p.Assets {
		s := &assetSeries{params: a.Model.Params, pending: map[string]int{}, open: map[string]*openSimEvent{},
			diag: map[string]*openSimEvent{}, diagMissCount: map[string]int{}}
		s.meas = make([][]float32, len(s.params))
		s.truth = make([][]float32, len(s.params))
		s.differs = make([]bool, len(s.params))
		for i := range s.params {
			s.meas[i] = make([]float32, steps)
			s.truth[i] = make([]float32, steps)
		}
		r.series[a] = s
	}
	return r
}

func (r *variantRecorder) event(e EventRow) {
	e.Variant = r.name
	r.out.Event(e)
}

// record stores step k (time t): series values, status / condition, events
// and KPIs.
func (r *variantRecorder) record(k int, t time.Time, meas, truth map[*Asset]map[string]float64) {
	nan := float32(math.NaN())
	for _, a := range r.assets {
		s := r.series[a]
		mv, tv := meas[a], truth[a]
		for i, ps := range s.params {
			x := float32(round(tv[ps.Alias], ps.Scale))
			if ps.Bool {
				x = float32(tv[ps.Alias])
			}
			s.truth[i][k] = x
			m := nan
			if !a.Offline {
				m = float32(round(mv[ps.Alias], ps.Scale))
				if ps.Bool {
					m = float32(mv[ps.Alias])
				}
			}
			s.meas[i][k] = m
			if m != x { // NaN never equals: offline samples keep their true values
				s.differs[i] = true
			}
		}
		forced := a.Forced
		if a.Maint && forced == 0 {
			forced = engine.StatusPlannedDown
		}
		ev := r.p.evaluate(a, mv, a.Offline, forced)
		r.track(a, s, ev, t)
	}
	// KPIs from the plant controller's true values
	if sys := r.p.sys; sys != nil {
		v := truth[sys]
		r.n++
		r.sumKW += v["plant_kw"]
		r.energy += v["plant_kw"] * float64(r.stepS) / 3600
		r.sumRT += v["cooling_load"] / 3.517
		r.sumCHWS += v["chws_temp"]
		r.maxCHWS = math.Max(r.maxCHWS, v["chws_temp"])
		r.peakLoad = math.Max(r.peakLoad, v["cooling_load"])
		if v["running"] == 1 {
			r.minDP = math.Min(r.minDP, v["riser_dp"])
		}
	}
	for _, m := range r.p.group("mau") {
		if m.Running() {
			r.maxValve = math.Max(r.maxValve, truth[m]["chw_valve"])
		}
	}
}

// track follows status / condition intervals, debounced alarm and abnormal
// events (open after 2 hits, close after 4 misses, as the history; a change
// of severity closes and reopens at once) and issue relation diagnoses.
func (r *variantRecorder) track(a *Asset, s *assetSeries, ev Evaluation, t time.Time) {
	alias := a.Spec.Alias
	status := engine.StatusName(ev.Status)
	switch {
	case s.statusStart.IsZero():
		s.status, s.statusStart = status, t
	case status != s.status:
		r.out.State(StateRow{Variant: r.name, Asset: alias, Kind: "status", Value: s.status, Start: s.statusStart, End: t})
		val := float64(ev.Status)
		r.event(EventRow{Ts: t, Asset: alias, Kind: "status", Code: status, Value: &val,
			Message: fmt.Sprintf("%s: %s → %s", a.Spec.Name, s.status, status), Details: map[string]any{"from": s.status, "to": status}})
		r.changes++
		s.status, s.statusStart = status, t
	}
	switch {
	case s.condStart.IsZero():
		s.cond, s.condStart = ev.Cond, t
	case ev.Cond != s.cond:
		r.out.State(StateRow{Variant: r.name, Asset: alias, Kind: "condition", Value: s.cond, Start: s.condStart, End: t})
		sev := ""
		if ev.Cond == engine.CondWarning || ev.Cond == engine.CondCritical {
			sev = ev.Cond
		}
		r.event(EventRow{Ts: t, Asset: alias, Kind: "condition", Code: ev.Cond, Severity: sev,
			Message: fmt.Sprintf("%s condition: %s → %s", a.Spec.Name, s.cond, ev.Cond), Details: map[string]any{"from": s.cond, "to": ev.Cond}})
		s.cond, s.condStart = ev.Cond, t
	}

	active := map[string]bool{}
	open := func(key string, row EventRow) {
		active[key] = true
		if e := s.open[key]; e != nil {
			if e.row.Severity == row.Severity {
				e.misses = 0
				return
			}
			// A severity change (WARNING <-> CRITICAL) closes the event and
			// opens one with the new severity at once, as the live engine does.
			end := t
			e.row.End = &end
			r.out.Event(e.row)
			delete(s.open, key)
		} else if s.pending[key]++; s.pending[key] < 2 {
			return
		}
		row.Variant, row.Asset, row.Ts = r.name, alias, t
		s.open[key] = &openSimEvent{row: row}
		if row.Kind == "alarm" {
			if row.Severity == "CRITICAL" {
				r.alarmCrit++
			} else {
				r.alarmWarn++
			}
		}
	}
	for code, h := range ev.Alarms {
		open("alarm:"+code, EventRow{Kind: "alarm", Code: code, Severity: h.Severity, Message: h.Message,
			Details: map[string]any{"name": codeName(code)}})
	}
	for alias, h := range ev.Abnormal {
		v := h.Value
		open("abn:"+alias, EventRow{Kind: "abnormal", Code: alias, Severity: h.Severity, Message: h.Message, Value: &v,
			Details: map[string]any{"param": alias}})
	}
	for key := range s.pending {
		if !active[key] {
			delete(s.pending, key)
		}
	}
	for key, e := range s.open {
		if active[key] {
			continue
		}
		if e.misses++; e.misses >= 4 || ev.Cond == engine.CondOffline {
			end := t
			e.row.End = &end
			r.out.Event(e.row)
			delete(s.open, key)
		}
	}

	// issue relation diagnoses (early warnings), not while offline or spiking
	seen := map[string]bool{}
	if !a.Offline && a.SpikeUntil.IsZero() {
		for _, d := range r.p.diagnose(a, ev) {
			name := d.Rel.Name
			seen[name] = true
			if e := s.diag[name]; e != nil {
				e.misses = 0
				continue
			}
			kind := "Early warning"
			switch {
			case d.IsCompound && d.IsDeduct:
				kind = "Compound degradation"
			case d.IsCompound:
				kind = "Compound early warning"
			case d.IsDeduct:
				kind = "Degradation"
			}
			s.diag[name] = &openSimEvent{row: EventRow{Variant: r.name, Ts: t, Asset: alias, Kind: "diagnosis", Code: d.Rel.Target, Severity: d.Severity,
				Message: fmt.Sprintf("%s detected by issue relation '%s': %s - %s", kind, name, d.Rel.Target, codeName(d.Rel.Target)),
				Details: map[string]any{"relation": name, "compound": d.IsCompound, "deduction": d.IsDeduct, "params": d.Params, "codes": d.Codes}}}
		}
	}
	for name, e := range s.diag {
		if seen[name] {
			continue
		}
		if e.misses++; e.misses >= 4 {
			end := t
			e.row.End = &end
			r.out.Event(e.row)
			delete(s.diag, name)
		}
	}
}

// finish closes the intervals at the end of the run, emits the events still
// open (no end), the series, and returns the variant KPIs.
func (r *variantRecorder) finish(end time.Time) map[string]any {
	for _, a := range r.assets {
		s := r.series[a]
		alias := a.Spec.Alias
		if !s.statusStart.IsZero() {
			r.out.State(StateRow{Variant: r.name, Asset: alias, Kind: "status", Value: s.status, Start: s.statusStart, End: end})
			r.out.State(StateRow{Variant: r.name, Asset: alias, Kind: "condition", Value: s.cond, Start: s.condStart, End: end})
		}
		for _, m := range []map[string]*openSimEvent{s.open, s.diag} {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				r.out.Event(m[k].row)
			}
		}
		for i, ps := range s.params {
			row := SeriesRow{Variant: r.name, Asset: alias, Param: ps.Alias, T0: r.t0, StepS: r.stepS, Measured: s.meas[i]}
			if s.differs[i] {
				row.Truth = s.truth[i]
			}
			r.out.Series(row)
		}
	}
	kpi := map[string]any{
		"energy_kwh": round(r.energy, 1), "avg_plant_kw": 0.0, "avg_kw_per_rt": 0.0, "peak_load_kw": round(r.peakLoad, 0),
		"avg_chws_temp": 0.0, "max_chws_temp": 0.0, "min_riser_dp": 0.0, "max_mau_valve": round(r.maxValve, 1),
		"alarm_counts": map[string]int{"warning": r.alarmWarn, "critical": r.alarmCrit}, "status_changes": r.changes, "trips": r.trips,
	}
	if r.n > 0 {
		kpi["avg_plant_kw"] = round(r.sumKW/float64(r.n), 1)
		kpi["avg_chws_temp"] = round(r.sumCHWS/float64(r.n), 3)
		kpi["max_chws_temp"] = round(r.maxCHWS, 3)
	}
	if r.sumRT > 0 {
		kpi["avg_kw_per_rt"] = round(r.sumKW/r.sumRT, 4)
	}
	if !math.IsInf(r.minDP, 1) {
		kpi["min_riser_dp"] = round(r.minDP, 1)
	}
	return kpi
}

// ---------------------------------------------------------------------------
// Runner (database)

// errJobGone means the job left RUNNING while it ran (cancelled).
var errJobGone = errors.New("job is no longer running")

// simJob is a claimed job.
type simJob struct {
	ID     int64
	Kind   string
	Title  string
	Origin string
	Spec   []byte
	StepS  int
}

// JobRunner claims and runs the simulation jobs.
type JobRunner struct {
	ctx      context.Context
	db       *pgxpool.Pool
	loc      *time.Location
	snapshot func(ctx context.Context, seed uint64) (*Plant, error)
	opts     JobOptions
}

func newJobRunner(ctx context.Context, db *pgxpool.Pool, loc *time.Location, snapshot func(context.Context, uint64) (*Plant, error), opts JobOptions) *JobRunner {
	return &JobRunner{ctx: ctx, db: db, loc: loc, snapshot: snapshot, opts: opts}
}

// Run listens on sim_jobs (and polls every 5 s) until the context ends.
func (r *JobRunner) Run() {
	r.recoverInterrupted()
	for r.ctx.Err() == nil {
		err := r.listen()
		if r.ctx.Err() != nil {
			return
		}
		wait := 5 * time.Second
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" { // sim.jobs missing: migration 0005 not applied
			wait = time.Minute
		}
		slog.Warn("simulation jobs: listener stopped (retrying)", "err", err, "retry_in", wait)
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// recoverInterrupted fails the jobs a previous simulator process left RUNNING.
func (r *JobRunner) recoverInterrupted() {
	rows, err := r.db.Query(r.ctx, `UPDATE sim.jobs SET status = 'FAILED', error = 'interrupted: the simulator restarted while the job was running',
		finished_at = now(), updated_at = now() WHERE status = 'RUNNING' RETURNING id`)
	if err != nil {
		slog.Warn("simulation jobs: recover interrupted jobs", "err", err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		slog.Warn("simulation jobs: recover interrupted jobs", "err", err)
		return
	}
	for _, id := range ids {
		r.notify(r.ctx, r.db, id)
		slog.Warn("simulation job interrupted by a restart, marked failed", "job", id)
	}
}

// listen holds a dedicated connection with LISTEN sim_jobs and runs every
// claimable job on each notification or every 5 s.
func (r *JobRunner) listen() error {
	pc, err := r.db.Acquire(r.ctx)
	if err != nil {
		return err
	}
	conn := pc.Hijack() // a LISTEN connection must not go back to the pool
	defer conn.Close(context.Background())
	if _, err := conn.Exec(r.ctx, `LISTEN sim_jobs`); err != nil {
		return err
	}
	for {
		if err := r.drain(); err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		_, err := conn.WaitForNotification(wctx)
		cancel()
		if r.ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !pgconn.Timeout(err) {
			return err
		}
	}
}

// drain runs the queued jobs one after the other.
func (r *JobRunner) drain() error {
	for r.ctx.Err() == nil {
		job, err := r.claim()
		if err != nil || job == nil {
			return err
		}
		r.execute(job)
	}
	return nil
}

// claim takes the oldest QUEUED job (RUNNING), or returns nil.
func (r *JobRunner) claim() (*simJob, error) {
	var j simJob
	err := r.db.QueryRow(r.ctx, `UPDATE sim.jobs j SET status = 'RUNNING', started_at = now(), updated_at = now(), progress = 0, error = NULL
		WHERE j.id = (SELECT id FROM sim.jobs WHERE status = 'QUEUED' ORDER BY created_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING j.id, j.kind, j.title, j.origin, j.spec, j.step_s`).Scan(&j.ID, &j.Kind, &j.Title, &j.Origin, &j.Spec, &j.StepS)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.notify(r.ctx, r.db, j.ID)
	return &j, nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// notify sends NOTIFY sim_jobs with the job id (a bigint parameter: pgx does
// not encode an integer into a text parameter).
func (r *JobRunner) notify(ctx context.Context, db execer, id int64) {
	if _, err := db.Exec(ctx, `SELECT pg_notify('sim_jobs', $1::bigint::text)`, id); err != nil {
		slog.Warn("simulation jobs: notify", "job", id, "err", err)
	}
}

// jobSeed is the seed of a job: the spec's, else derived from the id
// (splitmix64, 53 bits). The summary reports it as int64(seed), which a new
// spec takes back unchanged (and a browser reads exactly).
func jobSeed(spec int64, id int64) uint64 {
	if spec != 0 {
		return uint64(spec)
	}
	z := uint64(id) + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	if z &= 1<<53 - 1; z == 0 {
		z = 1
	}
	return z
}

// execute runs a claimed job and records its outcome.
func (r *JobRunner) execute(job *simJob) {
	started := time.Now()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	stepS := job.StepS
	if stepS < 10 || stepS > 600 {
		stepS = 60
	}
	rec := &MemRecorder{}
	lastProgress := time.Now()
	progress := func(f float64) {
		if time.Since(lastProgress) < 2*time.Second {
			return
		}
		lastProgress = time.Now()
		tag, err := r.db.Exec(ctx, `UPDATE sim.jobs SET progress = $2, updated_at = now() WHERE id = $1 AND status = 'RUNNING'`, job.ID, clamp(f, 0, 0.999))
		if err == nil && tag.RowsAffected() == 0 {
			cancel() // cancelled (or otherwise taken off RUNNING) meanwhile
		}
	}
	slog.Info("simulation job started", "job", job.ID, "kind", job.Kind, "title", job.Title)
	var summary map[string]any
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
				slog.Error("simulation job panicked", "job", job.ID, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		switch job.Kind {
		case "scenario":
			summary, err = r.scenario(ctx, job, stepS, rec, progress)
		case "doe":
			summary, err = r.doe(ctx, job, stepS, rec, progress)
		default:
			err = fmt.Errorf("unknown job kind %q", job.Kind)
		}
		return err
	}()
	if err != nil && ctx.Err() != nil && r.ctx.Err() == nil {
		err = errJobGone
	}
	switch {
	case errors.Is(err, errJobGone):
		slog.Info("simulation job no longer running, results dropped", "job", job.ID)
		return
	case r.ctx.Err() != nil:
		return // shutting down: marked failed on the next start
	case err != nil:
		r.fail(job.ID, err)
		return
	}
	summary["runtime_ms"] = time.Since(started).Milliseconds()
	if err := r.complete(job.ID, rec, summary); err != nil {
		if errors.Is(err, errJobGone) {
			slog.Info("simulation job no longer running, results dropped", "job", job.ID)
			return
		}
		r.fail(job.ID, fmt.Errorf("store results: %w", err))
		return
	}
	slog.Info("simulation job done, waiting for its analysis", "job", job.ID, "took", time.Since(started).Round(time.Millisecond),
		"series", len(rec.SeriesRows), "events", len(rec.EventRows), "samples", len(rec.Samples))
}

// scenario runs a scenario job.
func (r *JobRunner) scenario(ctx context.Context, job *simJob, stepS int, rec Recorder, progress func(float64)) (map[string]any, error) {
	var head struct {
		Seed    int64  `json:"seed"`
		Initial string `json:"initial"`
	}
	if err := json.Unmarshal(job.Spec, &head); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	seed := jobSeed(head.Seed, job.ID)
	var base *Plant
	if head.Initial == "nominal" {
		base = newPlant(seed, r.loc)
	} else {
		if r.snapshot == nil {
			return nil, errors.New("the live plant is not available for initial: live")
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		p, err := r.snapshot(sctx, seed)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("snapshot of the live plant: %w", err)
		}
		base = p
	}
	spec, err := parseScenario(job.Spec, base, jobMaxMinutes(job.Origin))
	if err != nil {
		return nil, err
	}
	start := time.Now().UTC().Truncate(time.Minute)
	if spec.Start != nil {
		start = spec.Start.UTC().Truncate(time.Second)
	}
	run := &scenarioRun{Spec: spec, Base: base, Seed: seed, Start: start, StepS: stepS, Opts: r.opts, Rec: rec, Progress: progress}
	end := start.Add(time.Duration(run.steps()*stepS) * time.Second)
	if _, err := r.db.Exec(ctx, `UPDATE sim.jobs SET sim_start = $2, sim_end = $3, step_s = $4, updated_at = now() WHERE id = $1`,
		job.ID, start, end, stepS); err != nil {
		return nil, err
	}
	return run.run(ctx)
}

// fail marks a job FAILED with its error.
func (r *JobRunner) fail(id int64, cause error) {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	slog.Warn("simulation job failed", "job", id, "err", msg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.db.Exec(ctx, `UPDATE sim.jobs SET status = 'FAILED', error = $2, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'RUNNING'`, id, msg); err != nil {
		slog.Warn("simulation jobs: mark failed", "job", id, "err", err)
		return
	}
	r.notify(ctx, r.db, id)
}

// complete stores the results and hands the job to the post-analysis of the
// analytics service (ANALYZING: anomaly and FDD events and the variant
// comparison of a scenario, the sample count of a DOE; it then sets
// SUCCEEDED) in one transaction, then notifies.
func (r *JobRunner) complete(id int64, rec *MemRecorder, summary map[string]any) error {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Minute)
	defer cancel()
	return pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		if err := writeResults(ctx, tx, id, rec); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE sim.jobs SET status = 'ANALYZING', progress = 1, summary = $2, updated_at = now()
			WHERE id = $1 AND status = 'RUNNING'`, id, summary)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errJobGone
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify('sim_jobs', $1::bigint::text)`, id)
		return err
	})
}

// writeResults replaces a job's sim.* rows with the recorded output.
func writeResults(ctx context.Context, tx pgx.Tx, id int64, rec *MemRecorder) error {
	for _, table := range []string{"series", "states", "events", "doe_samples"} {
		if _, err := tx.Exec(ctx, `DELETE FROM sim.`+table+` WHERE job_id = $1`, id); err != nil {
			return err
		}
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	copyRows := func(table string, cols []string, rows [][]any) error {
		if len(rows) == 0 {
			return nil
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"sim", table}, cols, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("copy sim.%s: %w", table, err)
		}
		return nil
	}
	rows := make([][]any, 0, len(rec.SeriesRows))
	for _, s := range rec.SeriesRows {
		var truth any
		if s.Truth != nil {
			truth = float4Array(s.Truth)
		}
		rows = append(rows, []any{id, s.Variant, s.Asset, s.Param, s.T0, s.StepS, float4Array(s.Measured), truth})
	}
	if err := copyRows("series", []string{"job_id", "variant", "asset_alias", "param_alias", "t0", "step_s", "measured", "truth"}, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, s := range rec.StateRows {
		var end any
		if !s.End.IsZero() {
			end = s.End
		}
		rows = append(rows, []any{id, s.Variant, s.Asset, s.Kind, s.Value, s.Start, end})
	}
	if err := copyRows("states", []string{"job_id", "variant", "asset_alias", "kind", "value", "start_ts", "end_ts"}, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, e := range rec.EventRows {
		details := e.Details
		if details == nil {
			details = map[string]any{}
		}
		var end, value any
		if e.End != nil {
			end = *e.End
		}
		if e.Value != nil && !math.IsNaN(*e.Value) && !math.IsInf(*e.Value, 0) {
			value = *e.Value
		}
		rows = append(rows, []any{id, e.Variant, e.Ts, end, nullable(e.Asset), e.Kind, nullable(e.Code), nullable(e.Severity), e.Message, value, details})
	}
	if err := copyRows("events", []string{"job_id", "variant", "ts", "end_ts", "asset_alias", "kind", "code", "severity", "message", "value", "details"}, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, s := range rec.Samples {
		rows = append(rows, []any{id, s.Idx, s.Inputs, s.Outputs})
	}
	return copyRows("doe_samples", []string{"job_id", "idx", "inputs", "outputs"}, rows)
}

// float4Array converts a series to REAL[] with NULL for NaN (no value).
func float4Array(xs []float32) []pgtype.Float4 {
	out := make([]pgtype.Float4, len(xs))
	for i, x := range xs {
		if !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0) {
			out[i] = pgtype.Float4{Float32: x, Valid: true}
		}
	}
	return out
}
