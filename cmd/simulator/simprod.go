package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Production planning simulations (sim.jobs kind 'production',
// docs/production/DESIGN.md section 6): the production model of a copy of the
// live fab (its lots and tools as they are now) or of a nominal fab is run
// ahead for up to 90 days under up to three production plans: lot starts per
// day, product mix, hot lot share, dispatch rule, furnace batching, planned
// tool downs and additional tools. The tools break down and get their PMs at
// their usual rates (seeded, the same for every variant). The result per
// variant: outs, cycle time per product (days, days per mask, X-factor), WIP,
// on-time delivery, line yield, tool and area utilization, queue times, the
// bottleneck and a daily series; the comparison is against the first variant.
// ---------------------------------------------------------------------------

const (
	prodMaxDays     = 90
	prodStepS       = 300
	prodMaxVariants = 3
	prodMaxActions  = 60
)

// ProdPlan is a production plan (nil / empty fields keep the current value).
type ProdPlan struct {
	StartsPerDay   *float64           `json:"starts_per_day,omitempty"`
	Mix            map[string]float64 `json:"mix,omitempty"`
	HotShare       *float64           `json:"hot_share,omitempty"`
	Dispatch       string             `json:"dispatch,omitempty"`
	FurnaceWaitH   *float64           `json:"furnace_wait_h,omitempty"`
	FurnaceMinLots *int               `json:"furnace_min_lots,omitempty"`
	Vehicles       *int               `json:"vehicles,omitempty"` // OHT fleet size
}

// ProdAction is a planned change: a tool down (from at_day for duration_h)
// or additional tools like an existing one (count).
type ProdAction struct {
	Kind      string  `json:"kind"` // tool_down | add_tool
	Asset     string  `json:"asset"`
	AtDay     float64 `json:"at_day,omitempty"`
	DurationH float64 `json:"duration_h,omitempty"`
	Count     int     `json:"count,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// ProdVariant is one plan of a production simulation.
type ProdVariant struct {
	Name    string       `json:"name"`
	Plan    ProdPlan     `json:"plan"`
	Actions []ProdAction `json:"actions"`
}

// ProdSpec is the spec of a production planning job.
type ProdSpec struct {
	Kind        string        `json:"kind"`
	HorizonDays float64       `json:"horizon_days"`
	Initial     string        `json:"initial"` // live | nominal
	Seed        int64         `json:"seed"`
	Breakdowns  *bool         `json:"breakdowns,omitempty"` // tool breakdowns and PMs (default true)
	Plan        ProdPlan      `json:"plan"`
	Actions     []ProdAction  `json:"actions"`
	Variants    []ProdVariant `json:"variants"`
}

// parseProdSpec decodes and validates a production spec against the plant.
func parseProdSpec(raw []byte, p *Plant) (*ProdSpec, error) {
	var s ProdSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	if s.HorizonDays < 1 || s.HorizonDays > prodMaxDays {
		return nil, fmt.Errorf("horizon_days must be 1 to %d", prodMaxDays)
	}
	if s.Initial == "" {
		s.Initial = "live"
	}
	if s.Initial != "live" && s.Initial != "nominal" {
		return nil, errors.New("initial must be live or nominal")
	}
	if len(s.Variants) == 0 {
		s.Variants = []ProdVariant{{Name: "Plan"}}
	}
	if len(s.Variants) > prodMaxVariants {
		return nil, fmt.Errorf("at most %d variants", prodMaxVariants)
	}
	n := len(s.Actions)
	seen := map[string]bool{}
	for _, v := range s.Variants {
		if v.Name == "" || seen[v.Name] {
			return nil, fmt.Errorf("variant names must be unique and not empty (%q)", v.Name)
		}
		seen[v.Name] = true
		n += len(v.Actions)
	}
	if n > prodMaxActions {
		return nil, fmt.Errorf("at most %d actions", prodMaxActions)
	}
	check := func(pl ProdPlan, a []ProdAction) error {
		if err := pl.validate(); err != nil {
			return err
		}
		for _, x := range a {
			if err := x.validate(p, s.HorizonDays); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(s.Plan, s.Actions); err != nil {
		return nil, err
	}
	for _, v := range s.Variants {
		if err := check(v.Plan, v.Actions); err != nil {
			return nil, fmt.Errorf("variant %s: %w", v.Name, err)
		}
	}
	return &s, nil
}

func (pl ProdPlan) validate() error {
	if v := pl.StartsPerDay; v != nil && (*v < 0 || *v > 12) {
		return errors.New("starts_per_day must be 0 to 12")
	}
	if v := pl.HotShare; v != nil && (*v < 0 || *v > 0.5) {
		return errors.New("hot_share must be 0 to 0.5")
	}
	if pl.Dispatch != "" && pl.Dispatch != "FIFO" && pl.Dispatch != "CR" {
		return errors.New("dispatch must be FIFO or CR")
	}
	if v := pl.FurnaceWaitH; v != nil && (*v < 0 || *v > 48) {
		return errors.New("furnace_wait_h must be 0 to 48")
	}
	if v := pl.FurnaceMinLots; v != nil && (*v < 1 || *v > 4) {
		return errors.New("furnace_min_lots must be 1 to 4")
	}
	if v := pl.Vehicles; v != nil && (*v < 1 || *v > 30) {
		return errors.New("vehicles must be 1 to 30")
	}
	if pl.Mix != nil {
		total := 0.0
		products := map[string]bool{}
		for _, pr := range fabProducts() {
			products[pr.Code] = true
		}
		for k, v := range pl.Mix {
			if !products[k] {
				return fmt.Errorf("unknown product %q in the mix", k)
			}
			if v < 0 || math.IsNaN(v) {
				return errors.New("product shares must not be negative")
			}
			total += v
		}
		if total <= 0 {
			return errors.New("the product mix must have a share above 0")
		}
	}
	return nil
}

func (a ProdAction) validate(p *Plant, horizon float64) error {
	if p.fab == nil || p.fab.byAlias[a.Asset] == nil {
		return fmt.Errorf("%s is not a production tool", a.Asset)
	}
	switch a.Kind {
	case "tool_down":
		if a.AtDay < 0 || a.AtDay >= horizon || a.DurationH <= 0 || a.DurationH > 24*horizon {
			return fmt.Errorf("tool_down of %s: at_day must lie in the horizon and duration_h above 0", a.Asset)
		}
	case "add_tool":
		if a.Count < 1 || a.Count > 3 {
			return fmt.Errorf("add_tool of %s: count must be 1 to 3", a.Asset)
		}
	default:
		return fmt.Errorf("unknown action kind %q (tool_down, add_tool)", a.Kind)
	}
	return nil
}

// apply sets a plan on the fab configuration.
func (pl ProdPlan) apply(c *FabConfig) {
	if pl.StartsPerDay != nil {
		c.StartsPerDay = *pl.StartsPerDay
	}
	if pl.Mix != nil {
		c.Mix = map[string]float64{}
		for k, v := range pl.Mix {
			c.Mix[k] = v
		}
	}
	if pl.HotShare != nil {
		c.HotShare = *pl.HotShare
	}
	if pl.Dispatch != "" {
		c.Dispatch = pl.Dispatch
	}
	if pl.FurnaceWaitH != nil {
		c.FurnWaitH = *pl.FurnaceWaitH
	}
	if pl.FurnaceMinLots != nil {
		c.FurnMin = *pl.FurnaceMinLots
	}
	if pl.Vehicles != nil {
		c.Vehicles = *pl.Vehicles
	}
}

// ---------------------------------------------------------------------------
// Fab copies and tools

// cloneTo deep-copies the production state onto a cloned plant.
func (f *Fab) cloneTo(c *Plant, seed uint64) *Fab {
	b, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	var st Fab
	if err := json.Unmarshal(b, &st); err != nil {
		return nil
	}
	nf := newFab(c, rand.New(rand.NewPCG(seed, seed^0x5bd1e995)))
	nf.restore(&st)
	nf.Cfg = f.Cfg
	if f.Cfg.Mix != nil {
		nf.Cfg.Mix = map[string]float64{}
		for k, v := range f.Cfg.Mix {
			nf.Cfg.Mix[k] = v
		}
	}
	for _, ft := range f.Tools {
		if ft.asset == nil { // a planned tool of a planning run
			nt := *ft
			nt.Chambers = nil
			for _, ch := range ft.Chambers {
				cc := *ch
				nt.Chambers = append(nt.Chambers, &cc)
			}
			nt.Run, nt.Life, nt.LifeAt = nil, map[string]float64{}, map[string]float64{}
			nf.attach(&nt, nil)
			nf.Tools = append(nf.Tools, &nt)
		}
	}
	return nf
}

// dropFab removes the production model (facility scenarios: the process
// tools stay idle, the dry pumps run their own process cycle).
func (p *Plant) dropFab() {
	if p.fab == nil {
		return
	}
	p.fab = nil
	for _, a := range p.Assets {
		a.tool, a.cell, a.Coupled, a.ProcLoad = nil, nil, false, 0
	}
}

// addTool adds a planned tool like an existing one (always available, no
// faults, no support units).
func (f *Fab) addTool(like string, n int) *FabTool {
	src := f.byAlias[like]
	ft := &FabTool{Alias: fmt.Sprintf("%s+%d", like, n), Kind: src.Kind, Off: f.rng.NormFloat64(), FreeAt: f.Last, Up: true}
	for _, c := range src.Chambers {
		nc := &Chamber{Name: c.Name, Off: 0.004 * f.rng.NormFloat64(), CDOff: 0.35 * f.rng.NormFloat64()}
		if c.CleanAt > 0 {
			nc.CleanAt = 110 + 30*f.rng.Float64()
		}
		ft.Chambers = append(ft.Chambers, nc)
	}
	f.attach(ft, nil)
	ft.Quals = src.Quals
	f.Tools = append(f.Tools, ft)
	return ft
}

// healthy returns the process tools and their support units to service (a
// plan assumes the repairs in progress are done).
func (p *Plant) healthy() {
	for _, a := range p.Assets {
		if a.tool == nil && a.cell == nil && len(a.served) == 0 && !a.Coupled {
			continue
		}
		a.Faults = map[string]*Fault{}
		a.Trip, a.TripUntil, a.SpikeUntil, a.Maint, a.Forced, a.Offline = false, time.Time{}, time.Time{}, false, 0, false
	}
}

// breakdowns returns a hook that breaks the process tools down about every
// two weeks per tool (2 - 8 h until the repair) and takes them into a
// monthly PM of 6 - 10 h, then refreshes the duty and interlocks; down
// collects the downtime events.
func breakdowns(p *Plant, seed uint64, start time.Time, down func(alias, kind string, from, to time.Time)) func(at time.Time) {
	r := rand.New(rand.NewPCG(seed, 7))
	type state struct {
		until time.Time
		next  [2]time.Time
	}
	st := map[*Asset]*state{}
	tools := append([]*Asset(nil), p.group("tool")...)
	for _, a := range tools {
		st[a] = &state{next: [2]time.Time{start.Add(hours(-math.Log(1-r.Float64()) * 14 * 24)), start.Add(hours(r.Float64() * 30 * 24))}}
	}
	return func(at time.Time) {
		for _, a := range tools {
			s := st[a]
			if !s.until.IsZero() && !at.Before(s.until) {
				a.Trip, a.Maint, a.Forced, s.until = false, false, 0, time.Time{}
			}
			if s.until.IsZero() && !at.Before(s.next[0]) {
				s.until = at.Add(hours(2 + 6*r.Float64()))
				a.Trip = true
				s.next[0] = at.Add(hours(-math.Log(1-r.Float64()) * 14 * 24))
				if down != nil {
					down(a.Spec.Alias, "breakdown", at, s.until)
				}
			}
			if s.until.IsZero() && !at.Before(s.next[1]) {
				s.until = at.Add(hours(6 + 4*r.Float64()))
				a.Maint, a.Forced = true, engineScheduled
				s.next[1] = s.next[1].Add(30 * 24 * time.Hour)
				if down != nil {
					down(a.Spec.Alias, "pm", at, s.until)
				}
			}
		}
		p.updateDuty(at)
	}
}

// ---------------------------------------------------------------------------
// KPIs

// ProdKPI are the production KPIs of a window.
type ProdKPI struct {
	Days         float64            `json:"days"`
	Starts       int                `json:"starts"`
	Outs         int                `json:"outs"`
	WafersIn     int                `json:"wafers_in"`
	WafersOut    int                `json:"wafers_out"`
	ScrappedLots int                `json:"scrapped_lots"`
	LineYield    float64            `json:"line_yield"` // %
	OnTime       float64            `json:"on_time"`    // %
	CTDays       map[string]float64 `json:"ct_days"`
	CTPerMask    map[string]float64 `json:"ct_per_mask"`
	XFactor      map[string]float64 `json:"x_factor"`
	OutsBy       map[string]int     `json:"outs_by_product"`
	WIPEnd       int                `json:"wip_end"`
	Util         map[string]float64 `json:"util"`          // busy share by tool
	AreaUtil     map[string]float64 `json:"area_util"`     // mean busy share of the area's tools
	QueueH       map[string]float64 `json:"queue_h"`       // mean queue time by area
	Moves        map[string]int     `json:"moves_by_area"` // lot steps run
	Reworks      map[string]int     `json:"reworks_by_area"`
	LithoRework  float64            `json:"litho_rework"` // % of exposures reworked
	Scrap        map[string]int     `json:"scrap_wafers"` // by reason
	WATMean      float64            `json:"wat_mean"`
	WATFail      float64            `json:"wat_fail"` // %
	Bottleneck   string             `json:"bottleneck"`
	// overhead transport: transfers, mean minutes from request to delivery, fleet busy share
	Transfers    int     `json:"transfers"`
	TransportMin float64 `json:"transport_min"`
	OHTUtil      float64 `json:"oht_util"` // %
	Vehicles     int     `json:"vehicles"`
}

// prodKPIs computes the KPIs of the records of a fab in [from, to).
func prodKPIs(f *Fab, out fabOut, from, to time.Time) *ProdKPI {
	k := &ProdKPI{Days: to.Sub(from).Hours() / 24, CTDays: map[string]float64{}, CTPerMask: map[string]float64{}, XFactor: map[string]float64{},
		OutsBy: map[string]int{}, Util: map[string]float64{}, AreaUtil: map[string]float64{}, QueueH: map[string]float64{}, Moves: map[string]int{},
		Reworks: map[string]int{}, Scrap: map[string]int{}}
	late := 0
	for _, l := range out.lots {
		if !l.Released.Before(from) && l.Released.Before(to) {
			k.Starts++
		}
		if l.Done.IsZero() || l.Done.Before(from) || !l.Done.Before(to) {
			continue
		}
		k.WafersIn += l.Start
		if l.Status == "SCRAPPED" {
			k.ScrappedLots++
			continue
		}
		k.Outs++
		k.WafersOut += len(l.Slots)
		pr := f.prod[l.Product]
		ct := l.Done.Sub(l.Released).Hours() / 24
		k.OutsBy[l.Product]++
		k.CTDays[l.Product] += ct
		k.CTPerMask[l.Product] += ct / float64(max(pr.Masks, 1))
		k.XFactor[l.Product] += ct * 24 / f.remain[l.Product][1]
		if l.Done.After(l.Due) {
			late++
		}
	}
	for p, n := range k.OutsBy {
		k.CTDays[p] = round2(k.CTDays[p] / float64(n))
		k.CTPerMask[p] = round2(k.CTPerMask[p] / float64(n))
		k.XFactor[p] = round2(k.XFactor[p] / float64(n))
	}
	if k.WafersIn > 0 {
		k.LineYield = round2(100 * float64(k.WafersOut) / float64(k.WafersIn))
	}
	if k.Outs > 0 {
		k.OnTime = round2(100 * (1 - float64(late)/float64(k.Outs)))
	}
	k.WIPEnd = len(f.Lots)
	busy := map[string]float64{}
	batches := map[string]bool{}
	exposures, reworked := 0, 0
	for _, s := range out.steps {
		if s.Tool == "" || s.Started.IsZero() || s.Ended.IsZero() || s.Ended.Before(from) || !s.Started.Before(to) {
			continue
		}
		if s.Batch == "" || !batches[s.Batch] {
			b := s.Ended
			if b.After(to) {
				b = to
			}
			busy[s.Tool] += b.Sub(maxT(s.Started, from)).Hours()
			if s.Batch != "" {
				batches[s.Batch] = true
			}
		}
		ft := f.byAlias[s.Tool]
		if ft == nil || s.Outcome == "ABORTED" {
			continue
		}
		k.Moves[ft.Area]++
		k.QueueH[ft.Area] += s.Started.Sub(s.Queued).Hours()
		if s.Outcome == "REWORK" {
			k.Reworks[ft.Area]++
		}
		if ft.Area == areaLitho {
			exposures++
			if s.Reworked > 0 {
				reworked++
			}
		}
	}
	if exposures > 0 {
		k.LithoRework = round2(100 * float64(reworked) / float64(exposures))
	}
	for a, n := range k.Moves {
		k.QueueH[a] = round2(k.QueueH[a] / float64(n))
	}
	areaN := map[string]int{}
	best := -1.0
	for _, ft := range f.Tools {
		u := busy[ft.Alias] / math.Max(k.Days*24, 1e-9)
		k.Util[ft.Alias] = round2(100 * u)
		k.AreaUtil[ft.Area] += u
		areaN[ft.Area]++
		if u > best {
			best, k.Bottleneck = u, ft.Alias
		}
	}
	for a, n := range areaN {
		k.AreaUtil[a] = round2(100 * k.AreaUtil[a] / float64(n))
	}
	for _, s := range out.scrap {
		if !s.At.Before(from) && s.At.Before(to) {
			k.Scrap[s.Reason] += s.Wafers
		}
	}
	n := 0
	for _, m := range out.met {
		if m.Param == "wat" && !m.At.Before(from) && m.At.Before(to) {
			k.WATMean += m.Value
			n++
			if !m.Pass {
				k.WATFail++
			}
		}
	}
	if n > 0 {
		k.WATMean = round2(k.WATMean / float64(n))
		k.WATFail = round2(100 * k.WATFail / float64(n))
	}
	busyS := 0.0
	for _, t := range out.transfers {
		if t.Requested.Before(from) || !t.Requested.Before(to) {
			continue
		}
		k.Transfers++
		k.TransportMin += t.Delivered.Sub(t.Requested).Minutes()
		busyS += t.Busy
	}
	if k.Transfers > 0 {
		k.TransportMin = round2(k.TransportMin / float64(k.Transfers))
	}
	if f.AMHS != nil {
		k.Vehicles = len(f.AMHS.Vehicles)
		k.OHTUtil = round2(100 * busyS / math.Max(float64(k.Vehicles)*to.Sub(from).Seconds(), 1))
	}
	return k
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---------------------------------------------------------------------------
// Runner

type prodDay struct {
	Day       string  `json:"day"`
	Starts    int     `json:"starts"`
	Outs      int     `json:"outs"`
	WafersOut int     `json:"wafers_out"`
	WIP       int     `json:"wip"`
	CTDays    float64 `json:"ct_days"` // mean of the lots completed that day (0 none)
	Hold      int     `json:"hold"`    // lots on hold at the end of the day
}

type prodEvent struct {
	Tool string `json:"tool"`
	Kind string `json:"kind"` // breakdown | pm | planned
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// production runs a production planning job.
func (r *JobRunner) production(ctx context.Context, job *simJob, progress func(float64)) (map[string]any, error) {
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
	if base.fab == nil {
		return nil, errors.New("the production model is not available")
	}
	spec, err := parseProdSpec(job.Spec, base)
	if err != nil {
		return nil, err
	}
	start := time.Now().UTC().Truncate(time.Minute)
	if base.fab.Last.After(start) {
		start = base.fab.Last
	}
	end := start.Add(hours(spec.HorizonDays * 24))
	if _, err := r.db.Exec(ctx, `UPDATE sim.jobs SET sim_start = $2, sim_end = $3, step_s = $4, updated_at = now() WHERE id = $1`,
		job.ID, start, end, prodStepS); err != nil {
		return nil, err
	}
	return runProduction(ctx, base, spec, seed, start, progress)
}

// runProduction runs the variants of a production spec from start.
func runProduction(ctx context.Context, base *Plant, spec *ProdSpec, seed uint64, start time.Time, progress func(float64)) (map[string]any, error) {
	base.healthy()
	base.updateDuty(start)
	base.fab.step(start, prodStepS) // a nominal fab is warmed up once for every variant
	end := start.Add(hours(spec.HorizonDays * 24))
	variants := map[string]any{}
	var order []string
	kpis := map[string]*ProdKPI{}
	for i, v := range spec.Variants {
		p := base.Clone(seed)
		if p.fab == nil {
			return nil, errors.New("the production model could not be copied")
		}
		f := p.fab
		f.Cfg = base.fab.Cfg
		spec.Plan.apply(&f.Cfg)
		v.Plan.apply(&f.Cfg)
		var events []prodEvent
		for _, a := range append(append([]ProdAction(nil), spec.Actions...), v.Actions...) {
			switch a.Kind {
			case "tool_down":
				from := start.Add(hours(a.AtDay * 24))
				to := from.Add(hours(a.DurationH))
				reason := a.Reason
				if reason == "" {
					reason = "Planned tool down"
				}
				f.byAlias[a.Asset].Downs = append(f.byAlias[a.Asset].Downs, Window{From: from, To: to, Reason: reason})
				events = append(events, prodEvent{Tool: a.Asset, Kind: "planned", From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Note: reason})
			case "add_tool":
				for n := 1; n <= a.Count; n++ {
					ft := f.addTool(a.Asset, n)
					events = append(events, prodEvent{Tool: ft.Alias, Kind: "added", From: start.Format(time.RFC3339), Note: "Additional tool like " + a.Asset})
				}
			}
		}
		hook := func(at time.Time) {}
		if spec.Breakdowns == nil || *spec.Breakdowns {
			hook = breakdowns(p, seed, start, func(alias, kind string, from, to time.Time) {
				events = append(events, prodEvent{Tool: alias, Kind: kind, From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)})
			})
		}
		f.out = fabOut{}
		wip0 := len(f.Lots)
		var daily []prodDay
		dayStart := start
		steps := int(spec.HorizonDays * 86400 / prodStepS)
		for k := 1; k <= steps; k++ {
			if k%2000 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if progress != nil {
					progress((float64(i) + float64(k)/float64(steps)) / float64(len(spec.Variants)))
				}
			}
			at := start.Add(time.Duration(k*prodStepS) * time.Second)
			hook(at)
			f.step(at, prodStepS)
			if at.Sub(dayStart) >= 24*time.Hour || k == steps {
				d := prodDay{Day: dayStart.In(f.loc).Format(time.DateOnly), WIP: len(f.Lots)}
				ct, n := 0.0, 0
				for _, l := range f.out.lots {
					if !l.Released.Before(dayStart) && l.Released.Before(at) {
						d.Starts++
					}
					if l.Status == "DONE" && !l.Done.Before(dayStart) && l.Done.Before(at) {
						d.Outs++
						d.WafersOut += len(l.Slots)
						ct += l.Done.Sub(l.Released).Hours() / 24
						n++
					}
				}
				for _, l := range f.Lots {
					if l.Status == "HOLD" {
						d.Hold++
					}
				}
				if n > 0 {
					d.CTDays = round2(ct / float64(n))
				}
				daily = append(daily, d)
				dayStart = at
			}
		}
		k := prodKPIs(f, f.out, start, end)
		kpis[v.Name] = k
		order = append(order, v.Name)
		sort.Slice(events, func(a, b int) bool { return events[a].From < events[b].From })
		cfg := f.Cfg
		variants[v.Name] = map[string]any{"plan": cfg, "kpi": k, "wip_start": wip0, "daily": daily, "events": events,
			"tools": toolRows(f, k), "label": v.Name}
	}
	summary := map[string]any{"kind": "production", "variants": variants, "order": order, "horizon_days": spec.HorizonDays,
		"initial": spec.Initial, "seed": int64(seed), "step_s": prodStepS, "comparison": compareProd(order, kpis)}
	summary["success"] = true
	summary["reason"] = prodReason(order, kpis)
	return summary, nil
}

// toolRows lists the tools of a variant with their area and utilization.
func toolRows(f *Fab, k *ProdKPI) []map[string]any {
	var rows []map[string]any
	for _, ft := range f.Tools {
		rows = append(rows, map[string]any{"alias": ft.Alias, "area": ft.Area, "util": k.Util[ft.Alias], "planned": ft.asset == nil})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a]["util"].(float64) > rows[b]["util"].(float64) })
	return rows
}

// compareProd compares every variant with the first.
func compareProd(order []string, kpis map[string]*ProdKPI) []map[string]any {
	if len(order) == 0 {
		return nil
	}
	b := kpis[order[0]]
	var out []map[string]any
	for _, name := range order {
		k := kpis[name]
		row := map[string]any{"variant": name, "outs": k.Outs, "wafers_out": k.WafersOut, "outs_delta": k.Outs - b.Outs,
			"wafers_out_delta": k.WafersOut - b.WafersOut, "on_time": k.OnTime, "line_yield": k.LineYield, "wip_end": k.WIPEnd,
			"bottleneck": k.Bottleneck, "ct_days": k.CTDays}
		ct := map[string]float64{}
		for p, v := range k.CTDays {
			ct[p] = round2(v - b.CTDays[p])
		}
		row["ct_delta"] = ct
		out = append(out, row)
	}
	return out
}

// prodReason is the one-line result of a production simulation.
func prodReason(order []string, kpis map[string]*ProdKPI) string {
	var parts []string
	for _, name := range order {
		k := kpis[name]
		var ct []string
		for _, p := range []string{"MCU40", "WSOC40"} {
			if v, ok := k.CTDays[p]; ok {
				ct = append(ct, fmt.Sprintf("%s %.1f d", p, v))
			}
		}
		parts = append(parts, fmt.Sprintf("%s: %d lots out (%d wafers), cycle time %s, on time %.0f %%, bottleneck %s", name, k.Outs, k.WafersOut,
			strings.Join(ct, " / "), k.OnTime, k.Bottleneck))
	}
	return strings.Join(parts, "; ")
}
