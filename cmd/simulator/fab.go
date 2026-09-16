package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// FAB1 production (docs/production/DESIGN.md): a discrete-event model of the
// whole wafer fab. Lots of 25 wafers are started every few hours and run the
// full 40 nm flow of their product (fabroute.go): lithography cells (track +
// scanner), etch, deposition, CMP, furnaces, implant, cleans and metrology,
// re-entrant once per mask layer, for about a month. Every processing step
// leaves quality characteristics on the wafers (resist CD, overlay, etch CD,
// film thickness, sheet resistance, implant dose, defect adders) that depend
// on the tool, its chambers and its faults; metrology measures sampled
// wafers and out-of-spec results are reworked (litho, film, CMP, cleans) or
// scrapped, and the wafer acceptance test at the end sums up what the wafers
// went through. The model runs inside Plant.Step: tool availability comes from
// the equipment state (trips, maintenance, support unit interlocks) and the
// tools' own stops (assists, engineering holds, chamber cleans, calibrations,
// consumable changes); the tools' telemetry and the dry pumps' gas load come
// from the production state, and every lot, lot step, measurement and scrap
// event is recorded for the prod schema.
// ---------------------------------------------------------------------------

const (
	lotSize      = 25
	assistMTBF   = 30 * time.Hour // short unscheduled stops cleared by the tool owner
	filmPerRFH   = 0.08           // CVD chamber film build-up (µm per RF hour)
	warmUp       = 42 * 24 * time.Hour
	warmUpStep   = 2 * time.Minute
	breakageRate = 1.0 / 200000 // wafers broken per wafer move
	lotHoldRate  = 0.01         // lots held for an engineering reason between two steps
	holdTime     = 3 * time.Hour
	holdMax      = 24 * time.Hour
	wetWait      = 20 * time.Minute // a wet bench lot waits this long for a batch partner
	maxReworks   = 2                // litho reworks of a layer before out-of-spec wafers are scrapped
	defectUSL    = 30               // defect adders per wafer
)

// FabConfig is the production plan: lot starts, product mix, hot lots,
// dispatching and furnace batching. Planning simulations change it.
type FabConfig struct {
	StartsPerDay float64            `json:"starts_per_day"`
	Mix          map[string]float64 `json:"mix,omitempty"` // product -> share (default: the products' mix)
	HotShare     float64            `json:"hot_share"`
	Dispatch     string             `json:"dispatch"` // FIFO | CR (critical ratio)
	WipCap       int                `json:"wip_cap"`
	FurnMin      int                `json:"furnace_min_lots"` // a furnace starts with this many lots of a recipe ...
	FurnWaitH    float64            `json:"furnace_wait_h"`   // ... or when the first waited this long
	Vehicles     int                `json:"vehicles"`         // OHT fleet (0: the network's)
}

func defaultFabConfig() FabConfig {
	return FabConfig{StartsPerDay: 4.5, HotShare: 0.05, Dispatch: "FIFO", WipCap: 240, FurnMin: 3, FurnWaitH: 10}
}

// litho cells: the track linked to each exposure tool.
var lithoCells = map[string]string{"TRK_01": "SCN_01", "TRK_02": "SCN_02", "TRK_03": "STP_01"}

// detourSteps are the rework steps a lot takes before it returns to its route.
var detourSteps = map[string]*RouteStep{}

func init() {
	for _, s := range []RouteStep{
		{Code: "RW-STRIP", Op: "Rework: resist strip", Type: stAsh, Recipe: "ASH-RWK"},
		{Code: "RW-CLEAN", Op: "Rework: post-strip clean", Type: stWet, Recipe: "SPM-SC1"},
		{Code: "RW-FILM", Op: "Rework: film strip", Type: stWet, Recipe: "FILM-STRIP"},
		{Code: "RW-SCRUB", Op: "Rework: particle scrub clean", Type: stSWC, Recipe: "SCRUB-DIW"},
	} {
		tm := stepTimes[s.Type]
		s.Layer, s.Module, s.WaferS, s.LotS, s.Rate = "REWORK", "REWORK", tm[0], tm[1], 1
		detourSteps[s.Code] = &s
	}
}

// LotStep is a prod.lot_steps row: one visit of a lot to a step.
type LotStep struct {
	ID       int64     `json:"id"`
	Lot      string    `json:"lot"`
	Seq      int       `json:"seq"`
	Code     string    `json:"code"`
	Op       string    `json:"op"`
	Tool     string    `json:"tool,omitempty"`
	Chambers string    `json:"chambers,omitempty"`
	Recipe   string    `json:"recipe"`
	Status   string    `json:"status"` // QUEUED | PROCESSING | DONE | SKIPPED
	Queued   time.Time `json:"queued"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	In       int       `json:"in"`
	Out      int       `json:"out"`
	Scrapped int       `json:"scrapped"`
	Reworked int       `json:"reworked"`
	Theo     float64   `json:"theo"`
	Actual   float64   `json:"actual"`
	Outcome  string    `json:"outcome,omitempty"` // PASS | REWORK | SCRAP | ABORTED
	Batch    string    `json:"batch,omitempty"`
	Rework   bool      `json:"rework"`
}

// QVal is a quality characteristic of the wafers of a lot: the value per
// wafer slot, the chamber that made it and the step (row) that produced it
// (the row takes the rework and scrap found later).
type QVal struct {
	V    []float64 `json:"v"`  // by slot - 1
	Ch   []int     `json:"ch"` // chamber index by slot - 1 (-1: none)
	Tool string    `json:"tool"`
	Src  *LotStep  `json:"src"`
}

// Lot is a lot in the fab (prod.lots).
type Lot struct {
	Code     string    `json:"code"`
	Product  string    `json:"product"`
	Priority int       `json:"priority"` // 1 hot lot, 3 normal
	Start    int       `json:"wafers_start"`
	Slots    []int     `json:"slots"` // wafer slots still in the lot
	Status   string    `json:"status"`
	Seq      int       `json:"seq"`              // current route step
	Detour   []string  `json:"detour,omitempty"` // rework steps still to run before Seq
	Rework   int       `json:"rework"`
	ReworkTo int       `json:"rework_to"` // route steps up to this one run as rework (0: none)
	Released time.Time `json:"released"`
	Due      time.Time `json:"due"`
	Done     time.Time `json:"done"`
	Ready    time.Time `json:"ready"`    // available for dispatch (in place / stored, hold ends)
	Loc      string    `json:"loc"`      // where its FOUP is: a load port, a stocker, a batch tool (AMHS)
	Arrive   time.Time `json:"arrive"`   // when it gets there (the last planned transfer)
	PrevEnd  time.Time `json:"prev_end"` // end of the previous processing step (Q-time)
	Hold     string    `json:"hold,omitempty"`
	Tool     string    `json:"tool,omitempty"`
	Row      *LotStep  `json:"row"`
	// quality: the open characteristics, the accumulated deviation of every
	// characteristic made so far (sum of squared normalised deviations, by
	// slot - 1; it drives the wafer acceptance test), and the defect adders
	// since the last inspection with the tool that added most of them
	Q       map[string]*QVal `json:"q,omitempty"`
	Risk    []float64        `json:"risk"`
	Adders  float64          `json:"adders"`
	CumAdd  float64          `json:"cum_adders"`
	AddMax  float64          `json:"add_max"`
	AddTool string           `json:"add_tool,omitempty"`
	AddStep *LotStep         `json:"add_step,omitempty"`
	Fails   map[string]int   `json:"fails,omitempty"` // failed measurements per step code
}

// Chamber is a process chamber, plating cell, tube or stage.
type Chamber struct {
	Name       string    `json:"name"`
	RFH        float64   `json:"rf_h"`     // RF hours since the last clean
	CleanAt    float64   `json:"clean_at"` // RF hours of the next clean (0: no chamber cleans)
	CleanUntil time.Time `json:"clean_until"`
	Off        float64   `json:"off"`    // film thickness offset (fraction)
	CDOff      float64   `json:"cd_off"` // etch CD offset (nm)
}

func (c *Chamber) up(t time.Time) bool { return c.CleanUntil.IsZero() || !t.Before(c.CleanUntil) }

// Run is a tool's current run (a lot, or a batch of lots).
type Run struct {
	Lots     []string  `json:"lots"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Chambers []int     `json:"chambers"`
	Step     string    `json:"step"`
	Recipe   string    `json:"recipe"`
	WaferS   float64   `json:"wafer_s"`
	LotS     float64   `json:"lot_s"`
	Wafers   int       `json:"wafers"`
	Batch    string    `json:"batch,omitempty"`
}

type doneMark struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}

// Window is a planned stop of a tool (planning simulations).
type Window struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

// FabTool is the production state of a process tool (a litho cell is its
// exposure tool with the linked track).
type FabTool struct {
	Alias       string             `json:"alias"`
	Kind        string             `json:"kind"`
	Area        string             `json:"area"`
	Class       string             `json:"class"`
	Quals       []string           `json:"quals"`
	Batch       int                `json:"batch,omitempty"`
	Track       string             `json:"track,omitempty"`
	Chambers    []*Chamber         `json:"chambers"`
	Off         float64            `json:"off"` // tool offset (litho CD / overlay, in sigma)
	Run         *Run               `json:"run,omitempty"`
	FreeAt      time.Time          `json:"free_at"` // idle and available since
	Up          bool               `json:"up"`
	StopFrom    time.Time          `json:"stop_from"` // assist or engineering hold (unscheduled)
	StopUntil   time.Time          `json:"stop_until"`
	StopReason  string             `json:"stop_reason,omitempty"`
	HoldSince   time.Time          `json:"hold_since"`
	SchedUntil  time.Time          `json:"sched_until"` // calibration / consumable change (scheduled down)
	SchedReason string             `json:"sched_reason,omitempty"`
	NextAssist  time.Time          `json:"next_assist"`
	NextCal     time.Time          `json:"next_cal"`
	Life        map[string]float64 `json:"life,omitempty"`    // consumable counters (busy hours / wafers)
	LifeAt      map[string]float64 `json:"life_at,omitempty"` // their change points
	MaintSince  time.Time          `json:"maint_since"`
	Downs       []Window           `json:"downs,omitempty"`
	Done        []doneMark         `json:"done"`
	Day         string             `json:"day"`
	Today       int                `json:"today"`
	Queue       int                `json:"queue"` // lots waiting for this tool's step types

	asset *Asset
	track *Asset
	pump  *Asset
	def   *ToolDef
	rules []maintRule
	loc   *time.Location
}

func after(t, until time.Time) bool { return until.IsZero() || !t.Before(until) }

// sev is the severity of a fault of the tool (a planned tool has none).
func (ft *FabTool) sev(kind string) float64 {
	if ft.asset == nil {
		return 0
	}
	return ft.asset.sev(kind)
}

// planned reports a planned stop at t.
func (ft *FabTool) planned(t time.Time) *Window {
	for i := range ft.Downs {
		if w := &ft.Downs[i]; !t.Before(w.From) && t.Before(w.To) {
			return w
		}
	}
	return nil
}

// available reports whether the tool can start a run at t.
func (ft *FabTool) available(t time.Time) bool {
	return ft.Up && after(t, ft.StopUntil) && after(t, ft.SchedUntil) && ft.planned(t) == nil && len(ft.upChambers(t)) > 0
}

// stopped reports a fab-internal stop at t: an assist or engineering hold
// (unscheduled) or a calibration / consumable change (scheduled). A hold set
// while the tool processes starts when its run ends.
func (ft *FabTool) stopped(t time.Time) (unscheduled, scheduled bool) {
	return !after(t, ft.StopUntil) && !t.Before(ft.StopFrom), !after(t, ft.SchedUntil) || ft.planned(t) != nil
}

func (ft *FabTool) upChambers(t time.Time) []int {
	var out []int
	for i, c := range ft.Chambers {
		if c.up(t) {
			out = append(out, i)
		}
	}
	return out
}

// busy reports whether the tool processes at t.
func (ft *FabTool) busy(t time.Time) bool { return ft.Run != nil && !ft.Run.Start.After(t) }

func (ft *FabTool) maxRFH(t time.Time) float64 {
	m := 0.0
	for i, c := range ft.Chambers {
		h := c.RFH
		if ft.busy(t) && containsInt(ft.Run.Chambers, i) {
			h += math.Max(0, t.Sub(ft.Run.Start).Seconds()-ft.Run.LotS) / 3600
		}
		m = math.Max(m, h)
	}
	return m
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// lifePct is a consumable counter in % of its change interval.
func (ft *FabTool) lifePct(reason string) float64 {
	if ft.LifeAt[reason] <= 0 {
		return 0
	}
	return 100 * ft.Life[reason] / ft.LifeAt[reason]
}

// count records completed wafers (throughput and wafers today).
func (ft *FabTool) count(at time.Time, n int, loc *time.Location) {
	day := at.In(loc).Format(time.DateOnly)
	if day != ft.Day {
		ft.Day, ft.Today = day, 0
	}
	ft.Today += n
	keep := ft.Done[:0]
	for _, d := range ft.Done {
		if at.Sub(d.At) <= time.Hour {
			keep = append(keep, d)
		}
	}
	ft.Done = append(keep, doneMark{At: at, N: n})
}

// throughput is the wafers completed in the hour before t.
func (ft *FabTool) throughput(t time.Time) float64 {
	n, keep := 0, ft.Done[:0]
	for _, d := range ft.Done {
		if t.Sub(d.At) <= time.Hour {
			keep = append(keep, d)
			if !d.At.After(t) {
				n += d.N
			}
		}
	}
	ft.Done = keep
	return float64(n)
}

func (ft *FabTool) today(t time.Time, loc *time.Location) float64 {
	if ft.Day != t.In(loc).Format(time.DateOnly) {
		return 0
	}
	return float64(ft.Today)
}

func (ft *FabTool) progress(t time.Time) float64 {
	if !ft.busy(t) {
		return 0
	}
	d := ft.Run.End.Sub(ft.Run.Start).Seconds()
	return clamp(100*t.Sub(ft.Run.Start).Seconds()/math.Max(d, 1), 0, 100)
}

// MetRow is a prod.metrology row.
type MetRow struct {
	Lot      string
	StepID   int64
	At       time.Time
	Param    string // thickness | cd | overlay | rs | defects | wat
	Key      string // quality characteristic (e.g. POLY.CDE)
	Unit     string
	Target   float64
	LSL, USL float64 // NaN: none
	Wafer    int
	Value    float64
	Range    float64 // within-wafer range
	HasRange bool
	MetTool  string
	SrcTool  string
	SrcCh    string
	SrcStep  string
	Pass     bool
}

// ScrapRow is a prod.scrap row.
type ScrapRow struct {
	Lot     string
	At      time.Time
	Wafers  int
	Reason  string
	Step    string
	Tool    string
	Chamber string
}

// fabOut collects the rows to store: lots and lot steps by key (latest
// state), measurements and scrap in order.
type fabOut struct {
	lots      map[string]*Lot
	steps     map[int64]*LotStep
	met       []MetRow
	scrap     []ScrapRow
	transfers []Transfer
}

func (o *fabOut) lot(l *Lot) {
	if o.lots == nil {
		o.lots = map[string]*Lot{}
	}
	o.lots[l.Code] = l
}

func (o *fabOut) step(r *LotStep) {
	if o.steps == nil {
		o.steps = map[int64]*LotStep{}
	}
	o.steps[r.ID] = r
}

func (o *fabOut) empty() bool {
	return len(o.lots) == 0 && len(o.steps) == 0 && len(o.met) == 0 && len(o.scrap) == 0 && len(o.transfers) == 0
}

// Fab is the production model of the fab.
type Fab struct {
	Tools       []*FabTool      `json:"tools"`
	Lots        map[string]*Lot `json:"lots"` // work in progress
	NextRelease time.Time       `json:"next_release"`
	SeqDay      string          `json:"seq_day"`
	Seq         int             `json:"seq"`
	NextStep    int64           `json:"next_step"` // next prod.lot_steps id
	Batches     int             `json:"batches"`
	Last        time.Time       `json:"last"`
	AMHS        *AMHS           `json:"amhs,omitempty"`

	Cfg FabConfig `json:"-"`

	warming  bool // the warm-up runs the tools as in service (the equipment state is that of one instant)
	loc      *time.Location
	rng      *rand.Rand
	rel      *rand.Rand // lot starts: their own stream, so flow changes (transport, dispatch) keep the same starts
	products []*Product
	prod     map[string]*Product
	remain   map[string][]float64 // ideal hours from a route step to the end
	byAlias  map[string]*FabTool
	out      fabOut
	spans    []gapSpan // scratch of the transport's merge-gap search
}

// chambersOf are the parallel chambers of a step type's tools (ideal times).
var chambersOf = map[string]int{stEtchC: 3, stEtchD: 3, stCVD: 3, stPVD: 3, stECP: 4, stSWC: 4, stAsh: 2}

// idealH is the ideal time of a full lot at a route step (hours).
func idealH(rs *RouteStep) float64 {
	switch {
	case rs.litho():
		return (lotSize*rs.WaferS + rs.LotS) / 3600
	case rs.WaferS == 0:
		return rs.LotS / 3600
	case rs.Param != "":
		return (float64(sampleN(rs, lotSize))*rs.WaferS + rs.LotS) / 3600
	}
	return (lotSize*rs.WaferS/float64(max(chambersOf[rs.Type], 1)) + rs.LotS) / 3600
}

func sampleN(rs *RouteStep, in int) int {
	if rs.Sample == 0 || rs.Sample > in {
		return in
	}
	return rs.Sample
}

func newFab(p *Plant, r *rand.Rand) *Fab {
	f := &Fab{Lots: map[string]*Lot{}, NextStep: 1, Cfg: defaultFabConfig(), loc: p.loc, rng: r, rel: rand.New(rand.NewPCG(r.Uint64(), r.Uint64())), products: fabProducts(),
		prod: map[string]*Product{}, remain: map[string][]float64{}, byAlias: map[string]*FabTool{}}
	for _, pr := range f.products {
		f.prod[pr.Code] = pr
		rem := make([]float64, len(pr.Route)+2)
		for i := len(pr.Route); i >= 1; i-- {
			rem[i] = rem[i+1] + idealH(&pr.Route[i-1]) + 0.1
		}
		f.remain[pr.Code] = rem
	}
	for _, a := range p.Assets {
		def := toolDef(a.Kind)
		if def == nil {
			continue
		}
		ft := &FabTool{Alias: a.Spec.Alias, Kind: a.Kind, Off: r.NormFloat64()}
		for _, name := range def.Chambers {
			c := &Chamber{Name: name, Off: 0.004 * r.NormFloat64(), CDOff: 0.35 * r.NormFloat64()}
			if def.ChamberClean {
				// the chambers start at different points of their clean cycle
				c.CleanAt = 110 + 30*r.Float64()
				c.RFH = r.Float64() * 0.9 * c.CleanAt
			}
			ft.Chambers = append(ft.Chambers, c)
		}
		f.attach(ft, a)
		f.Tools = append(f.Tools, ft)
	}
	for _, a := range p.Assets {
		if ft := f.byAlias[lithoCells[a.Spec.Alias]]; ft != nil && a.Kind == "track" {
			ft.Track, ft.track = a.Spec.Alias, a
			a.cell = ft
		}
	}
	return f
}

// attach links a tool state to its equipment, its definition and its dry pump.
func (f *Fab) attach(ft *FabTool, a *Asset) {
	def := toolDef(ft.Kind)
	ft.def, ft.loc = def, f.loc
	ft.Area, ft.Class, ft.Batch, ft.Quals = def.Area, def.Class, def.Batch, def.Quals
	if q, ok := toolQuals[ft.Alias]; ok {
		ft.Quals = q
	}
	ft.rules = append([]maintRule(nil), def.Maint...)
	if contains(ft.Quals, stFurnLP) {
		ft.rules = append(ft.rules, maintRule{Reason: "Tube clean", Unit: "busyh", Min: 300, Max: 360, DurMin: 14 * 60, DurMax: 18 * 60})
	}
	if ft.Class == classCluster && !ft.hasDaily() {
		// daily monitor wafers (particles, rate, uniformity) at any hour
		ft.rules = append(ft.rules, maintRule{Reason: "Daily monitor wafers", Unit: "daily", Min: 0, Max: 24, DurMin: 20, DurMax: 40})
	}
	if a != nil {
		ft.asset, ft.pump = a, a.vacPump
		a.tool = ft
		if a.vacPump != nil {
			a.vacPump.Coupled = true
		}
	}
	f.byAlias[ft.Alias] = ft
}

func (ft *FabTool) hasDaily() bool {
	for _, r := range ft.rules {
		if r.Unit == "daily" {
			return true
		}
	}
	return false
}

// restore takes the saved production state (tools matched by alias and kind).
func (f *Fab) restore(st *Fab) {
	if st == nil {
		return
	}
	for _, s := range st.Tools {
		ft := f.byAlias[s.Alias]
		if ft == nil || s.Kind != ft.Kind || len(s.Chambers) != len(ft.Chambers) {
			continue
		}
		keep := *ft
		*ft = *s
		ft.asset, ft.track, ft.pump, ft.def, ft.rules, ft.loc = keep.asset, keep.track, keep.pump, keep.def, keep.rules, keep.loc
		ft.Area, ft.Class, ft.Batch, ft.Quals, ft.Track = keep.Area, keep.Class, keep.Batch, keep.Quals, keep.Track
	}
	f.Lots = map[string]*Lot{}
	for code, l := range st.Lots {
		pr := f.prod[l.Product]
		if pr == nil || l.Row == nil || l.Seq < 1 || l.Seq > len(pr.Route) || len(l.Risk) != lotSize {
			continue
		}
		ok := true
		for _, d := range l.Detour {
			ok = ok && detourSteps[d] != nil
		}
		if ok {
			f.Lots[code] = l
		}
	}
	// runs of lots that did not survive (definition changes) are dropped
	for _, ft := range f.Tools {
		if ft.Run == nil {
			continue
		}
		for _, code := range ft.Run.Lots {
			if f.Lots[code] == nil {
				ft.Run = nil
				break
			}
		}
	}
	f.NextRelease, f.SeqDay, f.Seq, f.Batches, f.Last = st.NextRelease, st.SeqDay, st.Seq, st.Batches, st.Last
	f.NextStep = max(f.NextStep, st.NextStep)
	if st.AMHS != nil && len(st.AMHS.Vehicles) > 0 {
		f.AMHS = st.AMHS
	}
}

// shift moves the production schedule by d (the simulator was stopped): the
// fab resumes where it stood instead of replaying the gap.
func (f *Fab) shift(d time.Duration) {
	mv := func(t *time.Time) {
		if !t.IsZero() {
			*t = t.Add(d)
		}
	}
	for _, ft := range f.Tools {
		if ft.Run != nil {
			mv(&ft.Run.Start)
			mv(&ft.Run.End)
		}
		for _, p := range []*time.Time{&ft.FreeAt, &ft.StopFrom, &ft.StopUntil, &ft.HoldSince, &ft.SchedUntil, &ft.NextAssist, &ft.NextCal, &ft.MaintSince} {
			mv(p)
		}
		for _, c := range ft.Chambers {
			mv(&c.CleanUntil)
		}
	}
	for _, l := range f.Lots {
		mv(&l.Ready)
		mv(&l.Due)
		mv(&l.PrevEnd)
		mv(&l.Arrive)
	}
	mv(&f.NextRelease)
	mv(&f.Last)
	f.AMHS.shift(d)
}

func (f *Fab) expo(mean time.Duration) time.Duration {
	return time.Duration(-math.Log(1-f.rng.Float64()) * float64(mean))
}

func (f *Fab) minutes(lo, hi float64) time.Duration {
	return time.Duration((lo + f.rng.Float64()*(hi-lo)) * float64(time.Minute))
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func hours(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }

func maxT(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

// step advances production to t (dt seconds after the previous step). An
// empty fab is first filled with warmUp of production.
func (f *Fab) step(t time.Time, dt float64) {
	if f.Last.IsZero() {
		f.Last = t.Add(-warmUp)
		f.NextRelease = f.Last
		f.warming = true
		for w := f.Last.Add(warmUpStep); w.Before(t); w = w.Add(warmUpStep) {
			f.advance(w, warmUpStep.Seconds())
		}
		f.warming = false
	}
	f.advance(t, dt)
}

func (f *Fab) advance(t time.Time, dt float64) {
	f.complete(t)
	f.tools(t)
	for i := 0; i < 100; i++ {
		if f.arrive(t)+f.release(t)+f.dispatch(t, dt)+f.complete(t) == 0 {
			break
		}
	}
	f.store()
	f.couple(t)
	f.Last = t
}

// stepOf returns the step a lot is at (a rework step during a detour).
func (f *Fab) stepOf(l *Lot) *RouteStep {
	if len(l.Detour) > 0 {
		return detourSteps[l.Detour[0]]
	}
	pr := f.prod[l.Product]
	if l.Seq < 1 || l.Seq > len(pr.Route) {
		return nil
	}
	return &pr.Route[l.Seq-1]
}

// release starts the lots whose start time came (while WIP < the cap).
func (f *Fab) release(t time.Time) int {
	n := 0
	every := 24 / math.Max(f.Cfg.StartsPerDay, 0.1)
	for !f.NextRelease.After(t) {
		at := f.NextRelease
		f.NextRelease = at.Add(hours(every * clamp(-math.Log(1-f.rel.Float64()), 0.15, 3.5)))
		if f.Cfg.StartsPerDay <= 0 || len(f.Lots) >= f.Cfg.WipCap {
			continue
		}
		f.newLot(at)
		n++
	}
	return n
}

// pickProduct draws the product of a new lot from the mix.
func (f *Fab) pickProduct() *Product {
	total := 0.0
	share := func(p *Product) float64 {
		if f.Cfg.Mix != nil {
			return f.Cfg.Mix[p.Code]
		}
		return p.Mix
	}
	for _, p := range f.products {
		total += share(p)
	}
	x := f.rel.Float64() * total
	for _, p := range f.products {
		if x -= share(p); x < 0 {
			return p
		}
	}
	return f.products[0]
}

func (f *Fab) newLot(at time.Time) *Lot {
	pr := f.pickProduct()
	day := at.In(f.loc).Format("060102")
	if day != f.SeqDay {
		f.SeqDay, f.Seq = day, 0
	}
	f.Seq++
	prio, due := 3, pr.CycleDays
	if f.rel.Float64() < f.Cfg.HotShare {
		prio, due = 1, pr.CycleDays*0.6
	}
	l := &Lot{Code: fmt.Sprintf("%s-%s-%03d", pr.Code, day, f.Seq), Product: pr.Code, Priority: prio, Start: lotSize, Released: at,
		Due: at.Add(hours(due * 24)), Seq: 1, Risk: make([]float64, lotSize), Q: map[string]*QVal{}}
	for s := 1; s <= lotSize; s++ {
		l.Slots = append(l.Slots, s)
	}
	l.Loc, l.Arrive = amhsNetwork.Release, at
	f.Lots[l.Code] = l
	f.enter(l, at)
	return l
}

// advancePos moves a lot's position past its current step.
func (l *Lot) advancePos() {
	if len(l.Detour) > 0 {
		l.Detour = l.Detour[1:]
	} else {
		l.Seq++
	}
}

// next moves a lot to its next step.
func (f *Fab) next(l *Lot, at time.Time) {
	l.advancePos()
	f.enter(l, at)
}

// enter queues a lot at its current position where its FOUP is (it moves when
// a tool is dispatched it, or into a stocker when it is not dispatched at
// once). An unsampled measurement is skipped; past the last step the lot is
// complete and its FOUP is shipped.
func (f *Fab) enter(l *Lot, at time.Time) {
	pr := f.prod[l.Product]
	for {
		if len(l.Detour) == 0 && l.Seq > len(pr.Route) {
			l.Status, l.Done, l.Tool, l.Hold, l.Q = "DONE", at, "", "", nil
			l.Seq = len(pr.Route)
			delete(f.Lots, l.Code)
			f.out.lot(l)
			f.ship(l, at)
			return
		}
		if l.ReworkTo > 0 && len(l.Detour) == 0 && l.Seq > l.ReworkTo {
			l.ReworkTo = 0
		}
		rs := f.stepOf(l)
		r := &LotStep{ID: f.NextStep, Lot: l.Code, Seq: l.Seq, Code: rs.Code, Op: rs.Op, Recipe: rs.Recipe, Status: "QUEUED", Queued: at,
			In: len(l.Slots), Rework: len(l.Detour) > 0 || l.ReworkTo >= l.Seq}
		f.NextStep++
		l.Row, l.Tool, l.Hold = r, "", ""
		f.out.step(r)
		f.out.lot(l)
		if rs.Rate < 1 && len(l.Detour) == 0 && l.Fails[rs.Code] == 0 && f.rng.Float64() >= rs.Rate {
			r.Status, r.Started, r.Ended, r.Out = "SKIPPED", at, at, len(l.Slots)
			if rs.Meas != "" {
				f.fold(l, rs.Meas, pr) // not measured: the characteristic is closed unseen
			}
			l.Seq++
			continue
		}
		l.Status, l.Ready = "QUEUED", maxT(at, l.Arrive)
		return
	}
}

// holdLot puts a lot on hold for an engineering review of about d; it then
// enters its (already set) position. Reviews happen in engineering hours
// (08:00 - 20:00 plant time): a hold that would end at night waits for the
// morning.
func (f *Fab) holdLot(l *Lot, at time.Time, reason string, d time.Duration) {
	l.Status, l.Hold, l.Ready, l.Tool = "HOLD", reason, f.engHours(at.Add(d)), ""
	f.out.lot(l)
}

func (f *Fab) engHours(t time.Time) time.Time {
	lt := t.In(f.loc)
	day := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, f.loc)
	switch h := lt.Hour(); {
	case h < 8:
		return day.Add(8*time.Hour + f.minutes(0, 60)).UTC()
	case h >= 20:
		return day.AddDate(0, 0, 1).Add(8*time.Hour + f.minutes(0, 60)).UTC()
	}
	return t
}

// rework sends a lot back to route step back after the detour steps, after
// an engineering disposition hold.
func (f *Fab) rework(l *Lot, back int, detour []string, at time.Time, reason string, d time.Duration) {
	l.Rework++
	l.ReworkTo = max(l.ReworkTo, l.Seq)
	l.Seq, l.Detour = back, append([]string(nil), detour...)
	f.holdLot(l, at, reason, d)
}

// arrive releases the lots whose hold ended.
func (f *Fab) arrive(t time.Time) int {
	var due []*Lot
	for _, l := range f.Lots {
		if l.Status == "HOLD" && !l.Ready.After(t) {
			due = append(due, l)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].Ready.Equal(due[j].Ready) {
			return due[i].Ready.Before(due[j].Ready)
		}
		return due[i].Code < due[j].Code
	})
	for _, l := range due {
		f.enter(l, l.Ready)
	}
	return len(due)
}

// prio is the dispatch priority of a lot at t: hot lots, then lots whose
// queue-time limit is half spent, then the rest.
func (f *Fab) prio(l *Lot, t time.Time) int {
	if l.Priority == 1 {
		return 1
	}
	if rs := f.stepOf(l); rs != nil && rs.QTime > 0 && len(l.Detour) == 0 && !l.PrevEnd.IsZero() && t.Sub(l.PrevEnd) > hours(rs.QTime/2) {
		return 2
	}
	return l.Priority
}

// cr is the critical ratio of a lot: time to its due date over its
// remaining ideal time.
func (f *Fab) cr(l *Lot, t time.Time) float64 {
	return l.Due.Sub(t).Hours() / math.Max(f.remain[l.Product][min(l.Seq, len(f.remain[l.Product])-1)], 0.5)
}

func (f *Fab) better(a, b *Lot, t time.Time) bool {
	if pa, pb := f.prio(a, t), f.prio(b, t); pa != pb {
		return pa < pb
	}
	if f.Cfg.Dispatch == "CR" {
		if ca, cb := f.cr(a, t), f.cr(b, t); ca != cb {
			return ca < cb
		}
	}
	if !a.Ready.Equal(b.Ready) {
		return a.Ready.Before(b.Ready)
	}
	return a.Code < b.Code
}

// queues returns the lots waiting at their tools by step type, best first. A
// lot whose queue-time limit expired goes back to the clean before it.
func (f *Fab) queues(t time.Time) map[string][]*Lot {
	q := map[string][]*Lot{}
	var expired []*Lot
	for _, l := range f.Lots {
		if l.Status != "QUEUED" || l.Ready.After(t) {
			continue
		}
		rs := f.stepOf(l)
		if rs.QTime > 0 && len(l.Detour) == 0 && !l.PrevEnd.IsZero() && t.Sub(l.PrevEnd) > hours(rs.QTime) {
			expired = append(expired, l)
			continue
		}
		q[rs.Type] = append(q[rs.Type], l)
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].Code < expired[j].Code })
	for _, l := range expired {
		at := l.PrevEnd.Add(hours(f.stepOf(l).QTime))
		r := l.Row
		r.Status, r.Started, r.Ended, r.Out, r.Outcome = "SKIPPED", at, at, len(l.Slots), "REWORK"
		f.out.step(r)
		l.Rework++
		l.ReworkTo = max(l.ReworkTo, l.Seq)
		l.Seq--
		l.PrevEnd = time.Time{}
		f.enter(l, maxT(at, r.Queued))
	}
	for _, lots := range q {
		sort.Slice(lots, func(i, j int) bool { return f.better(lots[i], lots[j], t) })
	}
	return q
}

// dispatch starts runs on the idle tools, the longest idle first.
func (f *Fab) dispatch(t time.Time, dt float64) int {
	var idle []*FabTool
	for _, ft := range f.Tools {
		if ft.Run == nil && ft.available(t) {
			idle = append(idle, ft)
		}
	}
	if len(idle) == 0 {
		return 0
	}
	sort.SliceStable(idle, func(i, j int) bool { return idle[i].FreeAt.Before(idle[j].FreeAt) })
	q := f.queues(t)
	floor := t.Add(-secs(dt))
	n := 0
	for _, ft := range idle {
		if ft.Class == classBatch {
			if f.batch(ft, q, t, floor) {
				n++
			}
			continue
		}
		var best *Lot
		for _, typ := range ft.Quals {
			for _, l := range q[typ] {
				if l.Status == "QUEUED" {
					if best == nil || f.better(l, best, t) {
						best = l
					}
					break
				}
			}
		}
		if best != nil {
			at := maxT(ft.FreeAt, best.Ready, floor)
			f.start(ft, []*Lot{best}, f.deliver(best, ft, at))
			n++
		}
	}
	return n
}

// batch starts a batch of lots of one recipe: a wet bench takes two lots and
// waits wetWait for a partner, a furnace takes up to four and starts with
// FurnMin lots or when the first waited FurnWaitH. Hot lots and lots near
// their queue-time limit do not wait.
func (f *Fab) batch(ft *FabTool, q map[string][]*Lot, t, floor time.Time) bool {
	groups := map[string][]*Lot{}
	var recipes []string
	for _, typ := range ft.Quals {
		for _, l := range q[typ] {
			if l.Status != "QUEUED" {
				continue
			}
			rc := f.stepOf(l).Recipe
			if groups[rc] == nil {
				recipes = append(recipes, rc)
			}
			groups[rc] = append(groups[rc], l)
		}
	}
	minLots, wait := 2, wetWait
	if ft.Area == areaDiff {
		minLots, wait = f.Cfg.FurnMin, hours(f.Cfg.FurnWaitH)
	}
	minLots = max(1, min(minLots, ft.Batch))
	var best []*Lot
	var bestAt time.Time
	for _, rc := range recipes {
		g := groups[rc]
		byReady := append([]*Lot(nil), g...)
		sort.SliceStable(byReady, func(i, j int) bool { return byReady[i].Ready.Before(byReady[j].Ready) })
		var at time.Time
		urgent := false
		for _, l := range g {
			urgent = urgent || f.prio(l, t) <= 2
		}
		switch {
		case len(g) >= minLots:
			at = byReady[minLots-1].Ready
		case urgent:
			at = byReady[0].Ready
		default:
			at = byReady[0].Ready.Add(wait)
		}
		at = maxT(at, ft.FreeAt, floor)
		if at.After(t) {
			continue
		}
		var lots []*Lot
		for _, l := range g {
			if !l.Ready.After(at) && len(lots) < ft.Batch {
				lots = append(lots, l)
			}
		}
		if len(lots) > 0 && (best == nil || f.better(lots[0], best[0], t)) {
			best, bestAt = lots, at
		}
	}
	if best == nil {
		return false
	}
	start := bestAt
	for _, l := range best {
		start = maxT(start, f.deliver(l, ft, bestAt))
	}
	f.start(ft, best, start)
	return true
}

// slow is the run time stretch of the tool's faults.
func (ft *FabTool) slow() float64 {
	s := 1.0
	for k, v := range ft.def.Slow {
		s += v * ft.sev(k)
	}
	return s
}

// start begins a run of lots on a tool at at.
func (f *Fab) start(ft *FabTool, lots []*Lot, at time.Time) {
	rs := f.stepOf(lots[0])
	up := ft.upChambers(at)
	if len(up) == 0 {
		up = []int{0}
	}
	wafers := 0
	for _, l := range lots {
		wafers += len(l.Slots)
	}
	var dur float64
	switch ft.Class {
	case classBatch:
		dur = rs.LotS * (1 + 0.04*f.rng.Float64())
	case classMetro:
		dur = float64(sampleN(rs, wafers))*rs.WaferS*ft.slow()*(1+0.03*f.rng.NormFloat64()) + rs.LotS*(0.95+0.15*f.rng.Float64())
	case classLitho:
		dur = float64(wafers)*rs.WaferS*ft.slow()*(1+0.01*f.rng.NormFloat64()) + rs.LotS*(0.95+0.15*f.rng.Float64())
	default:
		dur = float64(wafers)*rs.WaferS*ft.slow()*(1+0.015*f.rng.NormFloat64())/float64(len(up)) + rs.LotS*(0.95+0.15*f.rng.Float64())
	}
	run := &Run{Start: at, End: at.Add(secs(dur)), Chambers: up, Step: rs.Code, Recipe: rs.Recipe, WaferS: rs.WaferS, LotS: rs.LotS, Wafers: wafers}
	if ft.Class == classBatch {
		f.Batches++
		run.Batch = fmt.Sprintf("%s-B%05d", ft.Alias, f.Batches)
	}
	var names []string
	if len(ft.Chambers) > 1 {
		for _, i := range up {
			names = append(names, ft.Chambers[i].Name)
		}
	}
	for _, l := range lots {
		run.Lots = append(run.Lots, l.Code)
		r := l.Row
		r.Status, r.Started, r.Tool, r.Chambers, r.Batch, r.In = "PROCESSING", at, ft.Alias, strings.Join(names, ","), run.Batch, len(l.Slots)
		l.Status, l.Tool = "PROCESSING", ft.Alias
		f.out.step(r)
		f.out.lot(l)
	}
	ft.Run = run
}

// theoretical is the ideal time of a lot at a step on a tool: all chambers
// of a cluster tool, a full batch (shared by its lots), the sampled wafers.
func (ft *FabTool) theoretical(rs *RouteStep, in int) float64 {
	switch ft.Class {
	case classBatch:
		return rs.LotS / float64(max(ft.Batch, 1))
	case classMetro:
		return float64(sampleN(rs, in))*rs.WaferS + rs.LotS
	case classLitho:
		return float64(in)*rs.WaferS + rs.LotS
	}
	return float64(in)*rs.WaferS/float64(max(len(ft.Chambers), 1)) + rs.LotS
}

// complete finishes the runs that ended by t, in time order.
func (f *Fab) complete(t time.Time) int {
	var due []*FabTool
	for _, ft := range f.Tools {
		if ft.Run != nil && !ft.Run.End.After(t) {
			due = append(due, ft)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].Run.End.Before(due[j].Run.End) })
	for _, ft := range due {
		f.finish(ft)
	}
	return len(due)
}

func (f *Fab) finish(ft *FabTool) {
	run := ft.Run
	ft.Run, ft.FreeAt = nil, run.End
	end := run.End
	actual := end.Sub(run.Start).Seconds()
	if ft.def.ChamberClean {
		for _, i := range run.Chambers {
			ft.Chambers[i].RFH += math.Max(0, actual-run.LotS) / 3600
		}
	}
	f.wear(ft, actual/3600, run.Wafers)
	ft.count(end, run.Wafers, f.loc)
	for li, code := range run.Lots {
		l := f.Lots[code]
		if l == nil {
			continue
		}
		rs := f.stepOf(l)
		r := l.Row
		r.Status, r.Ended, r.Outcome = "DONE", end, "PASS"
		r.Theo = ft.theoretical(rs, r.In)
		r.Actual = actual / float64(len(run.Lots))
		l.PrevEnd = end
		routed := false
		if ft.Class == classMetro {
			routed = f.measure(ft, l, rs, r, end)
		} else {
			f.process(ft, l, run, rs, r, li)
		}
		f.breakage(ft, l, r, run, end)
		r.Out = len(l.Slots)
		f.out.step(r)
		if len(l.Slots) == 0 {
			f.scrapLot(l, end)
			continue
		}
		if !routed {
			if len(l.Detour) == 0 && f.rng.Float64() < lotHoldRate {
				l.advancePos()
				f.holdLot(l, end, lotHoldReasons[f.rng.IntN(len(lotHoldReasons))], f.minutes(120, 720))
				continue
			}
			f.next(l, end)
		}
	}
}

var lotHoldReasons = []string{"Process engineer review", "Sampling plan verification", "Recipe parameter check", "MES data mismatch - lot history review",
	"Customer engineering request"}

// wear advances the consumable counters of a tool by a run.
func (f *Fab) wear(ft *FabTool, h float64, wafers int) {
	for _, r := range ft.rules {
		switch r.Unit {
		case "busyh":
			ft.Life[r.Reason] += h
		case "wafers":
			ft.Life[r.Reason] += float64(wafers)
		}
	}
}

func (f *Fab) scrapLot(l *Lot, at time.Time) {
	l.Status, l.Done, l.Tool, l.Hold, l.Q = "SCRAPPED", at, "", "", nil
	delete(f.Lots, l.Code)
	f.out.lot(l)
	f.ship(l, at)
}

func chamberOf(run *Run, i int) int { return run.Chambers[i%len(run.Chambers)] }

// ---------------------------------------------------------------------------
// Quality

// effects sums the quality effects of the tool's faults on a characteristic
// (by key suffix): absolute and relative.
func (ft *FabTool) effects(suffix string) (abs, rel float64) {
	for k, list := range ft.def.Effects {
		s := ft.sev(k)
		if s == 0 {
			continue
		}
		for _, e := range list {
			if e.Suffix == suffix {
				abs += e.Abs * s
				rel += e.Rel * s
			}
		}
	}
	if ft.track != nil && suffix == "CDR" {
		abs += 3 * ft.track.sev("hotplate_drift")
	}
	return
}

// process applies a processing step to a lot: the characteristics it makes,
// the end of the litho characteristics of a layer whose pattern it
// transfers, and the defect adders it leaves (a clean removes them).
func (f *Fab) process(ft *FabTool, l *Lot, run *Run, rs *RouteStep, r *LotStep, li int) {
	pr := f.prod[l.Product]
	if l.Q == nil {
		l.Q = map[string]*QVal{}
	}
	for _, key := range rs.Makes {
		f.makeQ(ft, l, run, r, key, li, pr)
	}
	if !rs.litho() && rs.Layer != "REWORK" {
		if l.Q[rs.Layer+".OVL"] != nil {
			f.fold(l, rs.Layer+".OVL", pr)
		}
		if l.Q[rs.Layer+".CDR"] != nil {
			if contains(rs.Makes, rs.Layer+".CDE") {
				delete(l.Q, rs.Layer+".CDR") // carried on by the etched CD
			} else {
				f.fold(l, rs.Layer+".CDR", pr)
			}
		}
	}
	add := ft.def.Adders
	for k, v := range ft.def.FaultAdders {
		add += v * ft.sev(k)
	}
	if ft.track != nil {
		add += 8 * ft.track.sev("nozzle_contamination")
	}
	if add < 0 {
		l.Adders *= 0.4
		return
	}
	add *= 0.6 + 0.8*f.rng.Float64()
	l.Adders += add
	l.CumAdd += add
	if add > l.AddMax {
		l.AddMax, l.AddTool, l.AddStep = add, ft.Alias, r
	}
}

// makeQ sets a characteristic of every wafer of a lot.
func (f *Fab) makeQ(ft *FabTool, l *Lot, run *Run, r *LotStep, key string, li int, pr *Product) {
	dot := strings.LastIndexByte(key, '.')
	layer, suffix := key[:dot], key[dot+1:]
	spec := pr.spec(key)
	if spec == nil && suffix != "DOSE" && suffix != "RSADJ" {
		return
	}
	abs, rel := ft.effects(suffix)
	old := l.Q[key]
	q := &QVal{V: make([]float64, lotSize), Ch: make([]int, lotSize), Tool: ft.Alias, Src: r}
	for i := range q.Ch {
		q.Ch[i] = -1
	}
	lot := 0.9 * f.rng.NormFloat64() // lot-to-lot (alignment, dose) error of a litho run, in wafer sigma
	for i, s := range l.Slots {
		ci := chamberOf(run, i)
		c := ft.Chambers[ci]
		n := f.rng.NormFloat64()
		var v float64
		switch suffix {
		case "CDR":
			v = spec.Target + 0.4*spec.Sigma*ft.Off + abs + lot*spec.Sigma + spec.Sigma*n
		case "OVL":
			v = abs + 0.3*spec.Sigma*ft.Off + lot*spec.Sigma + spec.Sigma*n
		case "CDE":
			v = spec.Target + abs + c.CDOff + 0.006*c.RFH + spec.Sigma*n
			if cdr, cs := l.Q[layer+".CDR"], pr.spec(layer+".CDR"); cdr != nil && cs != nil {
				v += 0.5 * (cdr.V[s-1] - cs.Target)
			}
		case "THK":
			k := 1 + rel + c.Off
			if ft.Kind == "cvd" {
				k -= 0.0012 * c.RFH * filmPerRFH // the recipe targets the middle of the clean cycle
			}
			if ft.Class == classBatch {
				// boat position: the wafers at the ends of the load see the end zones
				pos := float64(li*lotSize+i) / math.Max(float64(run.Wafers-1), 1)
				k += 0.008 * (pos - 0.5)
			}
			v = spec.Target*k + abs + spec.Sigma*n
		case "CMP":
			v = spec.Target + abs + 0.05*ft.lifePct("Pad change") + spec.Sigma*n
		case "RS":
			adj := 1.0
			if layer == "SD" {
				adj -= 0.5 * (f.qv(l, "NSD.DOSE", s) + f.qv(l, "PSD.DOSE", s)) / 2
			} else if a := l.Q[layer+".RSADJ"]; a != nil {
				adj = a.V[s-1]
			}
			v = spec.Target*adj*(1+rel) + abs + spec.Sigma*n
		case "DOSE":
			v = rel + 0.01*n
		case "RSADJ":
			base := 1.0
			if old != nil {
				base = old.V[s-1]
			}
			v = base * (1 + rel + 0.006*n)
		}
		q.V[s-1], q.Ch[s-1] = v, ci
	}
	l.Q[key] = q
	switch suffix {
	case "RS":
		if layer == "SD" {
			delete(l.Q, "NSD.DOSE")
			delete(l.Q, "PSD.DOSE")
		} else {
			delete(l.Q, layer+".RSADJ")
		}
	case "DOSE":
		if layer != "NSD" && layer != "PSD" {
			// well, threshold and LDD doses only show at the WAT (±5 % window)
			for _, s := range l.Slots {
				z := q.V[s-1] / 0.05
				l.Risk[s-1] += z * z
			}
			delete(l.Q, key)
		}
	}
}

func (f *Fab) qv(l *Lot, key string, s int) float64 {
	if q := l.Q[key]; q != nil {
		return q.V[s-1]
	}
	return 0
}

// fold closes a characteristic: its normalised deviation goes into the
// wafers' accumulated deviation.
func (f *Fab) fold(l *Lot, key string, pr *Product) {
	q := l.Q[key]
	delete(l.Q, key)
	spec := pr.spec(key)
	if q == nil || spec == nil {
		return
	}
	h := spec.half()
	if h <= 0 {
		return
	}
	for _, s := range l.Slots {
		z := (q.V[s-1] - spec.Target) / h
		l.Risk[s-1] += z * z
	}
}

// sampleSlots returns the wafers measured. Three wafers: the first, one near
// the middle and one near the end of the lot, at positions that went to
// different chambers of the three-chamber tools (wafers are loaded round
// robin); one wafer: a random one.
func (f *Fab) sampleSlots(slots []int, n int) []int {
	if n >= len(slots) {
		return append([]int(nil), slots...)
	}
	if n == 1 {
		return []int{slots[f.rng.IntN(len(slots))]}
	}
	if n == 2 {
		return []int{slots[0], slots[len(slots)-1]}
	}
	last := len(slots) - 1
	mid := len(slots) / 2
	mid += (1 - mid%3 + 3) % 3
	end := last - (last%3-2+3)%3
	if mid >= last || end <= mid {
		return []int{slots[0], slots[len(slots)/2], slots[last]}
	}
	return []int{slots[0], slots[mid], slots[end]}
}

func (f *Fab) chamberName(alias string, i int) string {
	if ft := f.byAlias[alias]; ft != nil && i >= 0 && i < len(ft.Chambers) && len(ft.Chambers) > 1 {
		return ft.Chambers[i].Name
	}
	return ""
}

// metroError is the repeatability and the bias of a measurement tool for a
// characteristic (its faults degrade both).
func (ft *FabTool) metroError(spec *QSpec) (noise, bias float64) {
	noise = 0.15
	for k, v := range ft.def.MeasNoise {
		noise += v * ft.sev(k)
	}
	for k, v := range ft.def.MeasBias {
		bias += v * ft.sev(k)
	}
	return noise * spec.Sigma, bias * spec.Sigma
}

// watScores computes the wafer acceptance test result of every wafer: the
// share of parametric test sites in specification, lowered by every
// deviation the wafer accumulated and by its defect load.
func (f *Fab) watScores(l *Lot, pr *Product) *QVal {
	for key := range l.Q {
		f.fold(l, key, pr)
	}
	q := &QVal{V: make([]float64, lotSize), Ch: make([]int, lotSize)}
	for _, s := range l.Slots {
		q.V[s-1] = math.Min(100, 99.4-0.35*l.Risk[s-1]-0.003*l.CumAdd+0.4*f.rng.NormFloat64())
		q.Ch[s-1] = -1
	}
	return q
}

// measure runs a metrology step and routes the lot when the result needs
// it (returns true): litho rework (strip, clean, expose again; twice at
// most), film rework (strip and deposit again), polish again, re-clean, or
// scrap of the wafers out of spec; an in-spec result outside the control
// limits holds the lot for an engineering review.
func (f *Fab) measure(ft *FabTool, l *Lot, rs *RouteStep, r *LotStep, at time.Time) bool {
	if rs.Param == "defects" {
		return f.inspect(ft, l, rs, r, at)
	}
	pr := f.prod[l.Product]
	spec := pr.spec(rs.Meas)
	if spec == nil {
		return false
	}
	q := l.Q[rs.Meas]
	if rs.Param == "wat" {
		q = f.watScores(l, pr)
	}
	noise, bias := ft.metroError(spec)
	val := func(s int) (float64, int) {
		if q == nil {
			return spec.Target, -1
		}
		return q.V[s-1], q.Ch[s-1]
	}
	srcTool, src := "", &LotStep{}
	if q != nil && q.Src != nil {
		srcTool, src = q.Tool, q.Src
	}
	if rs.Param == "wat" {
		srcTool, src = ft.Alias, r
	}
	readings := map[int]float64{}
	read := func(s int) bool {
		v, ch := val(s)
		m := v + bias + noise*f.rng.NormFloat64()
		ok := spec.pass(m)
		readings[s] = m
		rng, has := 0.0, false
		if rs.Param == "thickness" || rs.Param == "rs" {
			rng, has = math.Abs(m)*(0.01+0.004*math.Abs(f.rng.NormFloat64())), true
		}
		f.out.met = append(f.out.met, MetRow{Lot: l.Code, StepID: r.ID, At: at, Param: rs.Param, Key: rs.Meas, Unit: spec.Unit, Target: spec.Target,
			LSL: spec.LSL, USL: spec.USL, Wafer: s, Value: m, Range: rng, HasRange: has, MetTool: ft.Alias, SrcTool: srcTool,
			SrcCh: f.chamberName(srcTool, ch), SrcStep: src.Code, Pass: ok})
		return ok
	}
	fails, ooc := 0, false
	for _, s := range f.sampleSlots(l.Slots, sampleN(rs, len(l.Slots))) {
		if !read(s) {
			fails++
		} else if rs.Param != "wat" && math.Abs(readings[s]-spec.Target) > 3.5*spec.Sigma {
			ooc = true
		}
	}
	if fails == 0 {
		if !strings.HasSuffix(rs.Meas, ".CDR") {
			f.fold(l, rs.Meas, pr)
		}
		if ooc {
			l.advancePos()
			f.holdLot(l, at, "SPC out of control: "+spec.Name+" - engineering review", f.minutes(60, 240))
			return true
		}
		return false
	}
	if l.Fails == nil {
		l.Fails = map[string]int{}
	}
	l.Fails[rs.Code]++
	nf := l.Fails[rs.Code]
	all := func() []int {
		var bad []int
		for _, s := range append([]int(nil), l.Slots...) {
			if _, ok := readings[s]; !ok {
				read(s)
			}
			if !spec.pass(readings[s]) {
				bad = append(bad, s)
			}
		}
		return bad
	}
	chOf := func(s int) int {
		_, ch := val(s)
		return ch
	}
	reason := strings.ToUpper(rs.Param) + "_OOS"
	switch rs.OnFail {
	case failLitho:
		if nf <= maxReworks {
			f.markRework(src, len(l.Slots))
			r.Outcome = "REWORK"
			delete(l.Q, rs.Layer+".CDR")
			delete(l.Q, rs.Layer+".OVL")
			f.holdIfCause(srcTool, at, "Engineering hold: "+spec.Name+" out of spec")
			f.rework(l, rs.Back, []string{"RW-STRIP", "RW-CLEAN"}, at, "Litho rework: "+spec.Name+" out of spec", f.minutes(20, 90))
			return true
		}
	case failRedep:
		if nf == 1 {
			f.markRework(src, len(l.Slots))
			r.Outcome = "REWORK"
			delete(l.Q, rs.Meas)
			f.hold(srcTool, at, "Engineering hold: "+spec.Name+" out of spec")
			f.rework(l, rs.Back, []string{"RW-FILM"}, at, "Film rework: "+spec.Name+" out of spec - strip and deposit again", f.minutes(30, 120))
			return true
		}
	case failHold:
		r.Outcome = "PASS"
		f.hold(srcTool, at, "Engineering hold: "+spec.Name+" out of spec")
		f.fold(l, rs.Meas, pr)
		l.advancePos()
		f.holdLot(l, at, "Engineering disposition: "+spec.Name+" out of spec", f.minutes(60, 360))
		return true
	case failRepolish:
		bad := all()
		var over, under []int
		for _, s := range bad {
			if readings[s] > spec.Target {
				under = append(under, s) // too much film left: polish again
			} else {
				over = append(over, s)
			}
		}
		f.scrapWafers(l, over, at, reason, src, srcTool, chOf)
		f.hold(srcTool, at, "Engineering hold: "+spec.Name+" out of spec")
		if len(under) > 0 && nf == 1 && len(l.Slots) > 0 {
			f.markRework(src, len(l.Slots))
			r.Outcome = "REWORK"
			delete(l.Q, rs.Meas)
			f.rework(l, rs.Back, nil, at, "CMP rework: "+spec.Name+" above spec - polish again", f.minutes(15, 60))
			return true
		}
		f.scrapWafers(l, under, at, reason, src, srcTool, chOf)
		if len(bad) > 0 {
			r.Outcome = "SCRAP"
		}
		f.fold(l, rs.Meas, pr)
		return false
	}
	bad := all()
	if len(bad) > 0 {
		f.scrapWafers(l, bad, at, reason, src, srcTool, chOf)
		r.Outcome = "SCRAP"
		if rs.Param == "wat" {
			f.holdIfCause(srcTool, at, "Engineering hold: WAT fails")
		} else {
			f.hold(srcTool, at, "Engineering hold: "+spec.Name+" out of spec")
		}
	}
	if !strings.HasSuffix(rs.Meas, ".CDR") {
		f.fold(l, rs.Meas, pr)
	}
	return false
}

// inspect runs a defect inspection: the adders since the last inspection on
// the sampled wafers. Too many: hold the tool that added most of them,
// scrub and inspect again once; then scrap the two worst wafers.
func (f *Fab) inspect(ft *FabTool, l *Lot, rs *RouteStep, r *LotStep, at time.Time) bool {
	capture := 1.0
	for k, b := range ft.def.MeasBias {
		capture += b * ft.sev(k)
	}
	mean := (2 + l.Adders) * math.Max(capture, 0.2)
	src := l.AddStep
	if src == nil {
		src = &LotStep{}
	}
	fails := 0
	for _, s := range f.sampleSlots(l.Slots, sampleN(rs, len(l.Slots))) {
		v := float64(poisson(f.rng, mean*(0.7+0.6*f.rng.Float64())))
		ok := v <= defectUSL
		if !ok {
			fails++
		}
		f.out.met = append(f.out.met, MetRow{Lot: l.Code, StepID: r.ID, At: at, Param: "defects", Unit: "adders", Target: 0, LSL: math.NaN(),
			USL: defectUSL, Wafer: s, Value: v, MetTool: ft.Alias, SrcTool: l.AddTool, SrcStep: src.Code, Pass: ok})
	}
	reset := func() { l.Adders, l.AddMax, l.AddTool, l.AddStep = 0, 0, "", nil }
	if fails == 0 {
		reset()
		return false
	}
	if l.Fails == nil {
		l.Fails = map[string]int{}
	}
	l.Fails[rs.Code]++
	f.hold(l.AddTool, at, "Engineering hold: defect excursion")
	if l.Fails[rs.Code] == 1 {
		f.markRework(src, len(l.Slots))
		r.Outcome = "REWORK"
		f.rework(l, l.Seq, []string{"RW-SCRUB"}, at, "Defect excursion: scrub and inspect again", f.minutes(20, 60))
		return true
	}
	worst := append([]int(nil), l.Slots...)
	f.rng.Shuffle(len(worst), func(i, j int) { worst[i], worst[j] = worst[j], worst[i] })
	f.scrapWafers(l, worst[:min(2, len(worst))], at, "DEFECTS", src, l.AddTool, func(int) int { return -1 })
	r.Outcome = "SCRAP"
	reset()
	return false
}

// markRework records a rework on the step that caused it.
func (f *Fab) markRework(src *LotStep, n int) {
	src.Reworked += n
	src.Outcome = "REWORK"
	if src.ID != 0 {
		f.out.step(src)
	}
}

// scrapWafers removes wafers from a lot, attributed to the tool (and per
// wafer chamber) whose step produced the excursion; that step's row takes
// the scrapped count.
func (f *Fab) scrapWafers(l *Lot, slots []int, at time.Time, reason string, src *LotStep, tool string, chamber func(int) int) {
	if len(slots) == 0 {
		return
	}
	byCh := map[string]int{}
	var order []string
	for _, s := range slots {
		idx := -1
		for i, x := range l.Slots {
			if x == s {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		l.Slots = append(l.Slots[:idx], l.Slots[idx+1:]...)
		name := f.chamberName(tool, chamber(s))
		if _, ok := byCh[name]; !ok {
			order = append(order, name)
		}
		byCh[name]++
	}
	total := 0
	for _, name := range order {
		f.out.scrap = append(f.out.scrap, ScrapRow{Lot: l.Code, At: at, Wafers: byCh[name], Reason: reason, Step: src.Code, Tool: tool, Chamber: name})
		total += byCh[name]
	}
	if total > 0 && src != nil {
		src.Scrapped += total
		if src.Outcome != "REWORK" {
			src.Outcome = "SCRAP"
		}
		if src.ID != 0 {
			f.out.step(src)
		}
	}
	f.out.lot(l)
}

// breakage scraps a wafer broken in handling (breakageRate per wafer move).
func (f *Fab) breakage(ft *FabTool, l *Lot, r *LotStep, run *Run, at time.Time) {
	if len(l.Slots) == 0 || f.rng.Float64() >= float64(len(l.Slots))*breakageRate {
		return
	}
	i := f.rng.IntN(len(l.Slots))
	f.scrapWafers(l, []int{l.Slots[i]}, at, "BREAKAGE", r, ft.Alias, func(int) int { return chamberOf(run, i) })
}

// abort ends a run when the tool goes down: the lots return to the queue of
// the same step; a trip (unplanned) of a single-wafer tool scraps the wafers
// in the chambers now and then.
func (f *Fab) abort(ft *FabTool, t time.Time, unplanned bool) {
	run := ft.Run
	ft.Run, ft.FreeAt = nil, t
	for i, code := range run.Lots {
		l := f.Lots[code]
		if l == nil {
			continue
		}
		r := l.Row
		r.Status, r.Ended, r.Outcome, r.Theo, r.Actual = "DONE", t, "ABORTED", 0, 0
		if unplanned && i == 0 && !run.Start.After(t) && ft.Class != classBatch && ft.Class != classMetro && len(l.Slots) > 0 && f.rng.Float64() < 0.25 {
			k := 1
			if f.rng.Float64() < 0.2 {
				k = 2
			}
			k = min(k, len(l.Slots))
			f.scrapWafers(l, append([]int(nil), l.Slots[:k]...), t, "ROBOT_FAULT", r, ft.Alias, func(int) int { return run.Chambers[0] })
		}
		r.Out, r.Outcome = len(l.Slots), "ABORTED"
		f.out.step(r)
		if len(l.Slots) == 0 {
			f.scrapLot(l, t)
			continue
		}
		f.enter(l, t)
	}
}

// hold puts a process tool on engineering hold after an out-of-spec result
// it produced: no new runs (a run in progress completes) until the
// requalification passes, which needs the fault that caused it repaired.
func (f *Fab) hold(alias string, at time.Time, reason string) {
	ft := f.byAlias[alias]
	if ft == nil || len(ft.def.Hold) == 0 || !ft.HoldSince.IsZero() {
		return
	}
	from := at
	if ft.Run != nil && ft.Run.End.After(at) {
		from = ft.Run.End
	}
	if ft.StopUntil.After(at) {
		ft.StopUntil = maxT(ft.StopUntil, from).Add(holdTime) // after the assist in progress
	} else {
		ft.StopFrom, ft.StopUntil = from, from.Add(holdTime)
	}
	ft.HoldSince, ft.StopReason = from, reason
}

// holdIfCause holds a tool only when one of its faults explains the result
// (a single out-of-spec reading on a healthy tool does not inhibit it).
func (f *Fab) holdIfCause(alias string, at time.Time, reason string) {
	if ft := f.byAlias[alias]; ft != nil && ft.holdCause() {
		f.hold(alias, at, reason)
	}
}

// holdCause reports whether a fault that keeps the tool's results out of
// spec is still there (the requalification fails).
func (ft *FabTool) holdCause() bool {
	for kind, sev := range ft.def.Hold {
		if ft.sev(kind) > sev {
			return true
		}
	}
	return false
}

var assistReasons = []string{"Wafer handling robot assist", "Load port FOUP ID read error", "Recipe download timeout", "End effector vacuum fault"}

func (f *Fab) draw(r maintRule) float64 { return r.Min + f.rng.Float64()*(r.Max-r.Min) }

func (ft *FabTool) rule(reason string) *maintRule {
	for i := range ft.rules {
		if ft.rules[i].Reason == reason {
			return &ft.rules[i]
		}
	}
	return nil
}

// toolUp reports whether a tool's equipment (and its track) runs.
func (ft *FabTool) toolUp() bool {
	return (ft.asset == nil || ft.asset.Running()) && (ft.track == nil || ft.track.Running())
}

// unplannedDown reports whether an equipment stop is unscheduled.
func unplannedDown(a *Asset) bool {
	return a != nil && !a.Running() && (a.Trip || a.Interlock == engineUnplanned || (a.Maint && a.Forced != engineScheduled))
}

// tools updates the tools' availability: equipment state (trips,
// maintenance, support unit interlocks), assists, chamber cleans, daily
// calibrations, consumable changes and the end of preventive maintenance.
func (f *Fab) tools(t time.Time) {
	for _, ft := range f.Tools {
		for _, c := range ft.Chambers {
			if !c.CleanUntil.IsZero() && !t.Before(c.CleanUntil) {
				c.CleanUntil, c.RFH, c.CleanAt = time.Time{}, 0, 110+30*f.rng.Float64()
			}
		}
		if ft.Life == nil {
			ft.Life, ft.LifeAt = map[string]float64{}, map[string]float64{}
		}
		for _, r := range ft.rules {
			if r.Unit != "daily" && ft.LifeAt[r.Reason] == 0 {
				ft.LifeAt[r.Reason] = f.draw(r)
				ft.Life[r.Reason] = 0.9 * f.rng.Float64() * ft.LifeAt[r.Reason]
			}
			if r.Unit == "daily" && ft.NextCal.IsZero() {
				ft.NextCal = f.nextCal(t, r)
			}
		}
		if !ft.SchedUntil.IsZero() && !t.Before(ft.SchedUntil) {
			if r := ft.rule(ft.SchedReason); r != nil && r.Unit != "daily" {
				ft.Life[r.Reason], ft.LifeAt[r.Reason] = 0, f.draw(*r)
			}
			ft.FreeAt = maxT(ft.FreeAt, ft.SchedUntil)
			ft.SchedUntil, ft.SchedReason = time.Time{}, ""
		}
		if !ft.StopUntil.IsZero() && !t.Before(ft.StopUntil) {
			if !ft.HoldSince.IsZero() && ft.holdCause() && t.Sub(ft.HoldSince) < holdMax {
				ft.StopUntil = t.Add(holdTime) // requalification failed
			} else {
				ft.FreeAt = maxT(ft.FreeAt, ft.StopUntil)
				ft.StopFrom, ft.StopUntil, ft.StopReason, ft.HoldSince = time.Time{}, time.Time{}, "", time.Time{}
			}
		}
		if w := ft.planned(t); w != nil {
			ft.FreeAt = maxT(ft.FreeAt, w.To)
		}
		// preventive maintenance (a long maintenance stop) services the
		// chambers and renews a wet bench's baths
		if a := ft.asset; a != nil && a.Maint {
			if ft.MaintSince.IsZero() {
				ft.MaintSince = t
			}
		} else if !ft.MaintSince.IsZero() {
			if t.Sub(ft.MaintSince) >= 3*time.Hour {
				for _, c := range ft.Chambers {
					c.RFH, c.CleanUntil = 0, time.Time{}
					if c.CleanAt > 0 {
						c.CleanAt = 110 + 30*f.rng.Float64()
					}
				}
				if r := ft.rule("Bath change"); r != nil {
					ft.Life[r.Reason] = 0
				}
			}
			ft.MaintSince = time.Time{}
		}
		up := ft.toolUp() || f.warming
		if ft.Run != nil && !up {
			f.abort(ft, t, unplannedDown(ft.asset) || unplannedDown(ft.track))
		}
		if up && !ft.Up {
			ft.FreeAt = maxT(ft.FreeAt, t)
		}
		ft.Up = up
		if ft.NextAssist.IsZero() {
			ft.NextAssist = t.Add(f.expo(assistMTBF))
		}
		if !t.Before(ft.NextAssist) {
			ft.NextAssist = t.Add(f.expo(assistMTBF))
			if up && ft.StopUntil.IsZero() && ft.SchedUntil.IsZero() {
				d := f.minutes(10, 40)
				ft.StopFrom, ft.StopUntil, ft.StopReason = t, t.Add(d), assistReasons[f.rng.IntN(len(assistReasons))]
				if ft.Run != nil {
					ft.Run.End = ft.Run.End.Add(d) // the run waits for the assist
				}
			}
		}
		if ft.Run != nil || !ft.available(t) {
			continue
		}
		if ft.def.ChamberClean {
			// one chamber clean at a time, between runs; the tool keeps
			// running on its other chambers
			cleaning := false
			for _, c := range ft.Chambers {
				cleaning = cleaning || !c.up(t)
			}
			for _, c := range ft.Chambers {
				if !cleaning && c.RFH >= c.CleanAt {
					c.CleanUntil = t.Add(f.minutes(240, 360))
					cleaning = true
				}
			}
		}
		for _, r := range ft.rules {
			if !ft.SchedUntil.IsZero() {
				break
			}
			due := false
			if r.Unit == "daily" {
				if due = !t.Before(ft.NextCal); due {
					ft.NextCal = f.nextCal(t.Add(time.Hour), r)
				}
			} else {
				due = ft.Life[r.Reason] >= ft.LifeAt[r.Reason]
			}
			if due {
				ft.SchedUntil, ft.SchedReason = t.Add(f.minutes(r.DurMin, r.DurMax)), r.Reason
			}
		}
	}
}

// nextCal is the next daily calibration of a tool (in the rule's hour window,
// plant time).
func (f *Fab) nextCal(t time.Time, r maintRule) time.Time {
	lt := t.In(f.loc)
	c := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, f.loc).Add(hours(r.Min + f.rng.Float64()*(r.Max-r.Min)))
	if !c.After(t) {
		c = c.AddDate(0, 0, 1)
	}
	return c.UTC()
}

// couple sets the tools' queue lengths and the dry pumps' process gas load.
func (f *Fab) couple(t time.Time) {
	queue := map[string]int{}
	for _, l := range f.Lots {
		if l.Status == "QUEUED" {
			queue[f.stepOf(l).Type]++
		}
	}
	for _, ft := range f.Tools {
		n := 0
		for _, q := range ft.Quals {
			n += queue[q]
		}
		ft.Queue = n
		if ft.pump != nil {
			ft.pump.ProcLoad = 0.06
			if ft.busy(t) {
				ft.pump.ProcLoad = float64(len(ft.Run.Chambers)) / float64(len(ft.Chambers))
			}
		}
	}
}

// drain returns the rows collected since the last drain.
func (f *Fab) drain() fabOut {
	out := f.out
	f.out = fabOut{}
	return out
}
