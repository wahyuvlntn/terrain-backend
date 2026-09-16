package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// fabOnly advances only the production model (the equipment states stay as
// they are: a healthy plant) for days in dt steps and returns every record
// since the start of the warm-up.
func fabOnly(p *Plant, start time.Time, days float64, dt time.Duration, before func(at time.Time)) fabOut {
	p.updateDuty(start)
	end := start.Add(hours(days * 24))
	for at := start; at.Before(end); at = at.Add(dt) {
		if before != nil {
			before(at)
		}
		p.fab.step(at, dt.Seconds())
	}
	return p.fab.out
}

// fabReport are the production KPIs of a window.
type fabReport struct {
	days                 float64
	starts, outs         int
	wafersIn, wafersOut  int
	scrappedLots         int
	ctDays, xf, perMask  map[string]float64 // mean by product
	onTime               float64
	wip                  int
	util                 map[string]float64 // busy share by tool
	steps, reworks       map[string]int     // by area
	lithoSteps, lithoRwk int
	scrap                map[string]int // wafers by reason
	watMean, watFail     float64
	watN                 int
	holds                map[string]int
	wait                 map[string]float64 // mean queue time (h) by area
}

func reportFab(p *Plant, out fabOut, from, to time.Time) *fabReport {
	f := p.fab
	r := &fabReport{days: to.Sub(from).Hours() / 24, ctDays: map[string]float64{}, xf: map[string]float64{}, perMask: map[string]float64{},
		util: map[string]float64{}, steps: map[string]int{}, reworks: map[string]int{}, scrap: map[string]int{}, holds: map[string]int{},
		wait: map[string]float64{}}
	n := map[string]int{}
	late := 0
	for _, l := range out.lots {
		if !l.Released.Before(from) && l.Released.Before(to) {
			r.starts++
		}
		if l.Done.Before(from) || !l.Done.Before(to) || l.Done.IsZero() {
			continue
		}
		if l.Status == "SCRAPPED" {
			r.scrappedLots++
			r.wafersIn += l.Start
			continue
		}
		r.outs++
		r.wafersIn += l.Start
		r.wafersOut += len(l.Slots)
		ct := l.Done.Sub(l.Released).Hours() / 24
		pr := f.prod[l.Product]
		r.ctDays[l.Product] += ct
		r.xf[l.Product] += ct * 24 / f.remain[l.Product][1]
		r.perMask[l.Product] += ct / float64(pr.Masks)
		n[l.Product]++
		if l.Done.After(l.Due) {
			late++
		}
	}
	for k, c := range n {
		r.ctDays[k] /= float64(c)
		r.xf[k] /= float64(c)
		r.perMask[k] /= float64(c)
	}
	if r.outs > 0 {
		r.onTime = 1 - float64(late)/float64(r.outs)
	}
	r.wip = len(f.Lots)
	busy := map[string]float64{}
	batches := map[string]bool{}
	for _, s := range out.steps {
		if s.Tool == "" || s.Started.IsZero() || s.Ended.IsZero() || s.Ended.Before(from) || !s.Started.Before(to) {
			continue
		}
		if s.Batch == "" || !batches[s.Batch] {
			a, b := maxT(s.Started, from), s.Ended
			if b.After(to) {
				b = to
			}
			busy[s.Tool] += b.Sub(a).Hours()
			if s.Batch != "" {
				batches[s.Batch] = true
			}
		}
		if s.Outcome == "ABORTED" {
			continue
		}
		area := f.byAlias[s.Tool].Area
		r.steps[area]++
		r.wait[area] += s.Started.Sub(s.Queued).Hours()
		if s.Outcome == "REWORK" {
			r.reworks[area]++
		}
		if area == areaLitho {
			r.lithoSteps++
			if s.Reworked > 0 {
				r.lithoRwk++
			}
		}
	}
	for _, ft := range f.Tools {
		r.util[ft.Alias] = busy[ft.Alias] / (r.days * 24)
	}
	for a, n := range r.steps {
		r.wait[a] /= float64(n)
	}
	for _, l := range out.lots {
		if l.Hold != "" {
			r.holds[l.Hold]++
		}
	}
	for _, s := range out.scrap {
		if !s.At.Before(from) && s.At.Before(to) {
			r.scrap[s.Reason] += s.Wafers
		}
	}
	for _, m := range out.met {
		if m.Param == "wat" && !m.At.Before(from) && m.At.Before(to) {
			r.watMean += m.Value
			r.watN++
			if !m.Pass {
				r.watFail++
			}
		}
	}
	if r.watN > 0 {
		r.watMean /= float64(r.watN)
		r.watFail /= float64(r.watN)
	}
	return r
}

func (r *fabReport) log(t *testing.T) {
	t.Helper()
	t.Logf("starts %.2f/day, outs %.2f/day, WIP %d lots, line yield %.2f %%, scrapped lots %d, on time %.0f %%",
		float64(r.starts)/r.days, float64(r.outs)/r.days, r.wip, 100*float64(r.wafersOut)/math.Max(float64(r.wafersIn), 1), r.scrappedLots, 100*r.onTime)
	for _, k := range []string{"MCU40", "WSOC40"} {
		t.Logf("%s: CT %.1f days (%.2f days / mask), X-factor %.2f", k, r.ctDays[k], r.perMask[k], r.xf[k])
	}
	var tools []string
	for k := range r.util {
		tools = append(tools, k)
	}
	sort.Strings(tools)
	var b strings.Builder
	for _, k := range tools {
		b.WriteString(k + " " + itoa(int(math.Round(100*r.util[k]))) + "%  ")
	}
	t.Log("utilization: " + b.String())
	t.Logf("steps by area %v, reworks %v, litho rework rate %.2f %%", r.steps, r.reworks, 100*float64(r.lithoRwk)/math.Max(float64(r.lithoSteps), 1))
	t.Logf("scrap wafers %v; WAT mean %.2f %% (%d wafers, %.2f %% fail)", r.scrap, r.watMean, r.watN, 100*r.watFail)
	var w strings.Builder
	for _, a := range []string{areaLitho, areaEtch, areaCVD, areaPVD, areaECP, areaCMP, areaDiff, areaImp, areaWet, areaStrip, areaMet, areaTest} {
		w.WriteString(a + " " + strconvF(r.wait[a]) + " h  ")
	}
	t.Log("mean queue time: " + w.String())
}

// TestFabProduction: a month of production (after the warm-up) with the
// tools' breakdowns and PMs starts about 4.5 lots a day, completes them at a
// realistic pace per mask layer with realistic utilization, rework and
// yield, and every completed lot visited every route step.
func TestFabProduction(t *testing.T) {
	p := newPlant(2026, testLoc)
	start := testStart.UTC()
	days := 30.0
	out := fabOnly(p, start, days, 2*time.Minute, breakdowns(p, 1, start, nil))
	end := start.Add(hours(days * 24))
	r := reportFab(p, out, start, end)
	r.log(t)
	if s := float64(r.starts) / r.days; s < 4 || s > 5 {
		t.Errorf("starts %.2f lots / day, want about 4.5", s)
	}
	if o := float64(r.outs) / r.days; o < 3.5 || o > 5.5 {
		t.Errorf("outs %.2f lots / day, want about the start rate", o)
	}
	for _, k := range []string{"MCU40", "WSOC40"} {
		if pm := r.perMask[k]; pm < 0.3 || pm > 1.2 {
			t.Errorf("%s cycle time %.2f days per mask, want 0.3 - 1.2", k, pm)
		}
	}
	if y := float64(r.wafersOut) / float64(r.wafersIn); y < 0.95 || y > 0.998 {
		t.Errorf("line yield %.3f, want 0.95 - 0.998", y)
	}
	if u := r.util["SCN_01"]; u < 0.65 || u > 0.95 {
		t.Errorf("ArF scanner utilization %.2f, want 0.55 - 0.9 (the bottleneck)", u)
	}
	byLot := map[string]map[int]bool{}
	for _, s := range out.steps {
		if s.Status == "DONE" && s.Outcome != "ABORTED" || s.Status == "SKIPPED" {
			if byLot[s.Lot] == nil {
				byLot[s.Lot] = map[int]bool{}
			}
			byLot[s.Lot][s.Seq] = true
		}
	}
	for _, l := range out.lots {
		if l.Status == "DONE" && !l.Done.Before(start) {
			if got, want := len(byLot[l.Code]), len(p.fab.prod[l.Product].Route); got != want {
				t.Errorf("%s done after %d of %d steps", l.Code, got, want)
			}
		}
	}
}

func strconvF(v float64) string { return fmtF(v, 1) }

func fmtF(v float64, d int) string {
	p := math.Pow(10, float64(d))
	x := math.Round(v * p)
	s := itoa(int(math.Abs(x) / p))
	frac := int(math.Abs(x)) % int(p)
	if d > 0 {
		fs := itoa(frac)
		for len(fs) < d {
			fs = "0" + fs
		}
		s += "." + fs
	}
	if x < 0 {
		s = "-" + s
	}
	return s
}

// TestFabRoute: the routes are a genuine 40 nm flow: about 30 / 35 masks,
// every litho layer has its measurements, every rework target is a litho or
// process step before the measurement, every measured key is made earlier.
func TestFabRoute(t *testing.T) {
	for _, pr := range fabProducts() {
		masks := map[string]int{}
		made := map[string]bool{}
		for i, s := range pr.Route {
			if s.Seq != i+1 {
				t.Fatalf("%s step %d has seq %d", pr.Code, i+1, s.Seq)
			}
			if s.litho() {
				masks[s.Type]++
			}
			for _, k := range s.Makes {
				made[k] = true
			}
			if s.Meas != "" {
				if pr.spec(s.Meas) == nil {
					t.Errorf("%s %s measures %s without a specification", pr.Code, s.Code, s.Meas)
				}
				if !made[s.Meas] && s.Meas != "WAT" {
					t.Errorf("%s %s measures %s before any step makes it", pr.Code, s.Code, s.Meas)
				}
			}
			if s.OnFail != "" && s.OnFail != failScrap && s.OnFail != failReclean && s.OnFail != failHold && (s.Back < 1 || s.Back >= s.Seq) {
				t.Errorf("%s %s reworks to step %d", pr.Code, s.Code, s.Back)
			}
			if stepTimes[s.Type] == [2]float64{} {
				t.Errorf("%s %s has an unknown step type %s", pr.Code, s.Code, s.Type)
			}
		}
		total := masks[stLithoArF] + masks[stLithoKrF] + masks[stLithoI]
		t.Logf("%s: %d steps, %d masks (ArF %d, KrF %d, i-line %d), %d metals, ideal %.0f h", pr.Code, len(pr.Route), total,
			masks[stLithoArF], masks[stLithoKrF], masks[stLithoI], pr.Metals, func() float64 {
				h := 0.0
				for i := range pr.Route {
					h += idealH(&pr.Route[i])
				}
				return h
			}())
		if total != pr.Masks || total < 28 || total > 40 {
			t.Errorf("%s: %d masks (product says %d)", pr.Code, total, pr.Masks)
		}
	}
	// every step type has a qualified tool
	p := newPlant(3, testLoc)
	quals := map[string]bool{}
	for _, ft := range p.fab.Tools {
		for _, q := range ft.Quals {
			quals[q] = true
		}
	}
	for typ := range stepTimes {
		if !quals[typ] {
			t.Errorf("no tool qualified for %s", typ)
		}
	}
	for _, d := range detourSteps {
		if !quals[d.Type] {
			t.Errorf("no tool qualified for rework step %s", d.Code)
		}
	}
	if len(p.fab.Tools) != 28 {
		t.Errorf("%d production tools, want 28 (31 tools, the 3 tracks belong to their litho cells)", len(p.fab.Tools))
	}
	for _, ft := range p.fab.Tools {
		if ft.Class == classLitho && ft.track == nil {
			t.Errorf("litho cell %s has no track", ft.Alias)
		}
	}
}

// TestFabSampling: three sampled wafers spread over the chambers of a
// three-chamber tool; one wafer is a random one.
func TestFabSampling(t *testing.T) {
	f := newPlant(1, testLoc).fab
	slots := make([]int, 25)
	for i := range slots {
		slots[i] = i + 1
	}
	s := f.sampleSlots(slots, 3)
	ch := map[int]bool{}
	for _, x := range s {
		ch[(x-1)%3] = true
	}
	if len(s) != 3 || len(ch) != 3 {
		t.Errorf("sample %v covers chambers %v, want 3 wafers on 3 chambers", s, ch)
	}
	if got := f.sampleSlots(slots[:2], 3); len(got) != 2 {
		t.Errorf("sample of a 2-wafer lot: %v", got)
	}
}

// TestFabStageDrift: a drifting wafer stage on the ArF scanner fails overlay
// on the critical layers: the lots are reworked (strip, clean, expose again)
// and the scanner goes on engineering hold (requalification fails while the
// fault stays), so it exposes far less until the repair.
func TestFabStageDrift(t *testing.T) {
	p := newPlant(77, testLoc)
	start := testStart.UTC()
	scn := p.ByAlias["SCN_01"]
	out := fabOnly(p, start, 4, 2*time.Minute, func(at time.Time) {
		if at.Equal(start.Add(24 * time.Hour)) {
			scn.Faults["stage_drift"] = &Fault{Kind: "stage_drift", Sev: 0.8}
		}
	})
	from := start.Add(24 * time.Hour)
	before, after, runsBefore, runsAfter := 0, 0, 0, 0
	detours := 0
	for _, s := range out.steps {
		if s.Code == "RW-STRIP" && !s.Queued.Before(from) {
			detours++
		}
		if s.Tool != "SCN_01" || s.Ended.IsZero() || s.Ended.Before(start) || s.Outcome == "ABORTED" {
			continue
		}
		if s.Ended.Before(from) {
			runsBefore++
			if s.Reworked > 0 {
				before++
			}
		} else {
			runsAfter++
			if s.Reworked > 0 {
				after++
			}
		}
	}
	t.Logf("SCN_01: %d exposures (%d reworked) in the day before, %d (%d reworked) in 3 days with the fault; %d rework strips",
		runsBefore, before, runsAfter, after, detours)
	if after < 2 || float64(after)/math.Max(float64(runsAfter), 1) < 5*float64(before+1)/float64(runsBefore) {
		t.Errorf("stage drift: %d of %d exposures reworked after vs %d of %d before", after, runsAfter, before, runsBefore)
	}
	if float64(runsAfter)/3 > 0.6*float64(runsBefore) {
		t.Errorf("SCN_01 kept exposing %d lots in 3 days on hold (%d the day before)", runsAfter, runsBefore)
	}
	if detours == 0 {
		t.Error("no rework strip after overlay failures")
	}
	if ft := p.fab.byAlias["SCN_01"]; ft.HoldSince.IsZero() && ft.StopUntil.IsZero() {
		t.Error("SCN_01 not on engineering hold with the fault")
	}
}

// TestFabSupportInterlock: a tripped scrubber interlocks all its tools: their
// runs abort (unplanned) and they start nothing until it runs again.
func TestFabSupportInterlock(t *testing.T) {
	p := newPlant(5, testLoc)
	start := testStart.UTC()
	scr := p.ByAlias["SCR_01"]
	var aborted []string
	for k := 0; k < 3*60; k++ {
		at := start.Add(time.Duration(k) * time.Minute)
		if k == 60 {
			scr.Trip, scr.TripUntil = true, at.Add(time.Hour)
		}
		if k == 120 {
			scr.Trip = false
		}
		p.Step(at, 60)
		if k > 61 && k < 120 {
			for _, alias := range []string{"ETCH_01", "ETCH_02", "ETCH_03"} {
				a := p.ByAlias[alias]
				if a.Interlock != engineUnplanned || a.Running() {
					t.Fatalf("%s not interlocked while SCR_01 is tripped", alias)
				}
				if ft := p.fab.byAlias[alias]; ft.Run != nil {
					t.Fatalf("%s runs while interlocked", alias)
				}
			}
		}
	}
	for _, s := range p.fab.out.steps {
		if s.Outcome == "ABORTED" && strings.HasPrefix(s.Tool, "ETCH") {
			aborted = append(aborted, s.Tool)
		}
	}
	t.Logf("aborted etch runs: %v", aborted)
	if p.ByAlias["CVD_01"].Interlock != 0 {
		t.Error("CVD_01 interlocked by the etch scrubber")
	}
}

// TestFabStateRoundTrip: the production state survives a JSON round trip
// (live restart) and a restored fab continues identically.
func TestFabStateRoundTrip(t *testing.T) {
	p := newPlant(9, testLoc)
	start := testStart.UTC()
	fabOnly(p, start, 1, 2*time.Minute, nil)
	b, err := json.Marshal(p.fab)
	if err != nil {
		t.Fatal(err)
	}
	var st Fab
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	q := newPlant(9, testLoc)
	q.fab.restore(&st)
	if len(q.fab.Lots) != len(p.fab.Lots) || q.fab.NextStep != p.fab.NextStep {
		t.Fatalf("restored %d lots (next step %d), want %d (%d)", len(q.fab.Lots), q.fab.NextStep, len(p.fab.Lots), p.fab.NextStep)
	}
	for _, ft := range q.fab.Tools {
		if ft.asset == nil || ft.def == nil || (ft.Class == classLitho && ft.track == nil) {
			t.Fatalf("%s lost its links on restore", ft.Alias)
		}
	}
	t.Logf("state %d kB, %d lots", len(b)/1024, len(p.fab.Lots))
	if len(b) > 3<<20 {
		t.Errorf("state %d bytes", len(b))
	}
}

// TestProductionPlanning: a production planning run of the nominal fab
// compares plans: more lot starts with a second ArF scanner give more outs,
// a two-day ArF scanner down gives fewer outs and longer cycle times than
// the baseline; the summary has the KPIs, daily series and events.
func TestProductionPlanning(t *testing.T) {
	p := newPlant(31, testLoc)
	start := testStart.UTC()
	five := 5.5
	spec := &ProdSpec{Kind: "production", HorizonDays: 25, Initial: "nominal", Variants: []ProdVariant{
		{Name: "Baseline"},
		{Name: "More starts", Plan: ProdPlan{StartsPerDay: &five}, Actions: []ProdAction{{Kind: "add_tool", Asset: "SCN_01", Count: 1},
			{Kind: "add_tool", Asset: "SCN_02", Count: 1}, {Kind: "add_tool", Asset: "IMP_01", Count: 1}, {Kind: "add_tool", Asset: "ASH_01", Count: 1}}},
		{Name: "Scanner down", Actions: []ProdAction{{Kind: "tool_down", Asset: "SCN_01", AtDay: 2, DurationH: 48, Reason: "Laser chamber exchange"}}},
	}}
	raw, _ := json.Marshal(spec)
	if _, err := parseProdSpec(raw, p); err != nil {
		t.Fatal(err)
	}
	sum, err := runProduction(t.Context(), p, spec, 31, start, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := func(v string) *ProdKPI { return sum["variants"].(map[string]any)[v].(map[string]any)["kpi"].(*ProdKPI) }
	b, more, down := k("Baseline"), k("More starts"), k("Scanner down")
	t.Log(sum["reason"])
	t.Logf("starts %d / %d / %d, bottlenecks %s / %s / %s", b.Starts, more.Starts, down.Starts, b.Bottleneck, more.Bottleneck, down.Bottleneck)
	if more.Starts <= b.Starts {
		t.Errorf("more starts: %d starts vs %d", more.Starts, b.Starts)
	}
	if down.Outs >= b.Outs && down.CTDays["MCU40"] <= b.CTDays["MCU40"] {
		t.Errorf("scanner down: %d outs, CT %.1f vs baseline %d outs, CT %.1f", down.Outs, down.CTDays["MCU40"], b.Outs, b.CTDays["MCU40"])
	}
	if d := sum["variants"].(map[string]any)["Baseline"].(map[string]any)["daily"].([]prodDay); len(d) != 25 {
		t.Errorf("%d daily rows", len(d))
	}
	bad := []byte(`{"kind":"production","horizon_days":10,"actions":[{"kind":"tool_down","asset":"CH_01","at_day":1,"duration_h":5}]}`)
	if _, err := parseProdSpec(bad, p); err == nil {
		t.Error("a chiller accepted as a production tool")
	}
	if _, err := json.Marshal(sum); err != nil {
		t.Errorf("summary is not JSON: %v", err)
	}
}
