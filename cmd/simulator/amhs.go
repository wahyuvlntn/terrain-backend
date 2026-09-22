package main

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// AMHS: the overhead hoist transport (OHT) of the FAB1 cleanroom.
//
// The network (fab1_amhs.json, generated from blender/fab_twin_scripts/
// fab_layout.py) is one-way: the interbay loop "IB" and one spur per bay that
// leaves the loop on a diverge switch, passes over the load ports of both
// sides of the bay and merges back, and STK-01's bypass spur. Every load port
// and stocker port is a point (track, s) on a spur.
//
// A lot's FOUP is always somewhere: on a tool's load port, inside a stocker,
// or on a vehicle. Moving it is a transfer: the vehicle that can pick it first
// drives there, picks the FOUP (hoist), carries it along the shortest path,
// drops it and drives back onto the interbay loop, where idle vehicles
// circulate. Vehicles never overlap: one vehicle at a time is on a spur (the
// others drive another lap meanwhile), and a vehicle leaving a spur waits
// before the merge for a gap to every vehicle on the loop (all move at the
// fleet speed there). Transfers happen when a tool is dispatched a lot that is
// not at its port (the run starts when the FOUP is delivered), when a finished
// lot is not dispatched at once (it is stored in its next area's stocker),
// when a lot starts (from the start stocker) and when it is finished (shipped
// to the start stocker). The plan of every vehicle is computed when a
// transfer is assigned, so the app animates exactly the simulated moves.
// ---------------------------------------------------------------------------

//go:embed fab1_amhs.json
var amhsJSON []byte

type amhsTrack struct {
	ID     string       `json:"id"`
	Closed bool         `json:"closed"`
	Len    float64      `json:"len"`
	SIn    float64      `json:"s_in"`
	SOut   float64      `json:"s_out"`
	Points [][2]float64 `json:"points"`
}

type amhsPort struct {
	Track string  `json:"track"`
	S     float64 `json:"s"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type amhsNet struct {
	Speed   float64             `json:"speed"`
	HoistS  float64             `json:"hoist_s"`
	Fleet   int                 `json:"fleet"`
	Tracks  []amhsTrack         `json:"tracks"`
	Ports   map[string]amhsPort `json:"ports"`
	Units   map[string][]string `json:"units"` // tool alias / stocker name -> its ports
	Buffers map[string]string   `json:"buffers"`
	Release string              `json:"release"`
	byID    map[string]*amhsTrack
}

var amhsNetwork = mustLoadAMHS(amhsJSON)

func mustLoadAMHS(b []byte) *amhsNet {
	var n amhsNet
	if err := json.Unmarshal(b, &n); err != nil {
		panic(fmt.Sprintf("fab1_amhs.json: %v", err))
	}
	n.byID = map[string]*amhsTrack{}
	for i := range n.Tracks {
		n.byID[n.Tracks[i].ID] = &n.Tracks[i]
	}
	if n.byID["IB"] == nil || n.Speed <= 0 || n.Fleet <= 0 {
		panic("fab1_amhs.json: no interbay loop / speed / fleet")
	}
	return &n
}

// isStocker reports whether a location is a stocker (else a load port).
func (n *amhsNet) isStocker(loc string) bool {
	_, ok := n.Units[loc]
	return ok && strings.HasPrefix(loc, "STK-")
}

// buffer is the stocker that stores lots waiting for a step of the area.
func (n *amhsNet) buffer(area string) string {
	if s, ok := n.Buffers[area]; ok {
		return s
	}
	return n.Buffers["default"]
}

// amhsPos is a point of the network.
type amhsPos struct {
	Track string
	S     float64
}

type pathSeg struct {
	Track  string
	S0, S1 float64
}

// path is the shortest route from a to b on the one-way network and its length.
func (n *amhsNet) path(a, b amhsPos) ([]pathSeg, float64) {
	ib := n.byID["IB"]
	var segs []pathSeg
	total := 0.0
	add := func(track string, s0, s1 float64) {
		if s1-s0 > 1e-9 {
			segs = append(segs, pathSeg{track, s0, s1})
			total += s1 - s0
		}
	}
	cur := a
	if cur.Track != "IB" {
		t := n.byID[cur.Track]
		if b.Track == cur.Track && b.S >= cur.S-1e-9 {
			add(cur.Track, cur.S, b.S)
			return segs, total
		}
		add(cur.Track, cur.S, t.Len)
		cur = amhsPos{"IB", t.SOut}
	}
	target := b.S
	if b.Track != "IB" {
		target = n.byID[b.Track].SIn
	}
	d := math.Mod(target-cur.S+ib.Len, ib.Len)
	if d > ib.Len-1e-9 {
		d = 0
	}
	add("IB", cur.S, cur.S+d) // s past the loop length is taken modulo
	if b.Track != "IB" {
		add(b.Track, 0, b.S)
	}
	return segs, total
}

// OhtSeg is a piece of a vehicle's plan: a run along a track at constant speed
// (S0 to S1, T0 to T1), a hoist cycle at a port (pick / drop, S0 = S1) or a
// wait for a gap before merging (S0 = S1, no hoist).
type OhtSeg struct {
	T0    time.Time `json:"t0"`
	T1    time.Time `json:"t1"`
	Track string    `json:"track"`
	S0    float64   `json:"s0"`
	S1    float64   `json:"s1"`
	Hoist string    `json:"hoist,omitempty"` // pick | drop
	Port  string    `json:"port,omitempty"`
	Lot   string    `json:"lot,omitempty"` // the FOUP carried, picked or dropped
}

// Vehicle is an OHT vehicle: its plan, and from Free on it circulates idle on
// the interbay loop from FreeS.
type Vehicle struct {
	ID    string    `json:"id"`
	Segs  []OhtSeg  `json:"segs"`
	Free  time.Time `json:"free"`
	FreeS float64   `json:"free_s"`
	BusyS float64   `json:"busy_s"` // seconds on transfers (utilization)
	Jobs  int       `json:"jobs"`

	// its stretches on the interbay loop (loopIntervals), for the plan they were made of
	ivs    []loopIv
	ivsKey [3]int64
}

// Transfer is a prod.transfers row.
type Transfer struct {
	ID        int64
	Lot       string
	Product   string
	Kind      string // deliver | store | ship
	From, To  string
	Vehicle   string
	Requested time.Time
	Picked    time.Time
	Delivered time.Time
	Dist      float64
	Busy      float64 // the vehicle's seconds on the job
}

// AMHS is the transport state of the fab (saved with it).
type AMHS struct {
	Vehicles []*Vehicle        `json:"vehicles"`
	NextID   int64             `json:"next_id"`
	RR       int               `json:"rr"`            // stocker port rotation
	Res      map[string][]Resv `json:"res,omitempty"` // planned stays on each spur
}

// Resv is a vehicle's planned stay on a spur.
type Resv struct {
	Vehicle string    `json:"v"`
	T0      time.Time `json:"t0"`
	T1      time.Time `json:"t1"`
}

func newAMHS(n *amhsNet, at time.Time) *AMHS {
	a := &AMHS{NextID: 1}
	ib := n.byID["IB"]
	for i := 0; i < n.Fleet; i++ {
		a.Vehicles = append(a.Vehicles, &Vehicle{ID: fmt.Sprintf("V%02d", i+1), Free: at, FreeS: ib.Len * float64(i) / float64(n.Fleet)})
	}
	return a
}

// fleet sizes the fleet to the plan (vehicles join idle on the loop; idle
// ones leave).
func (f *Fab) fleet(at time.Time) {
	n := amhsNetwork
	want := n.Fleet
	if f.Cfg.Vehicles > 0 {
		want = f.Cfg.Vehicles
	}
	if f.AMHS == nil {
		f.AMHS = newAMHS(n, at)
	}
	ib := n.byID["IB"]
	for len(f.AMHS.Vehicles) < want {
		// a new vehicle joins in the widest gap between the idle ones
		best, gap := 0.0, -1.0
		for s := 0.0; s < ib.Len; s += 0.5 {
			if d := f.clearance(nil, n.phase(at, s)); d > gap {
				best, gap = s, d
			}
		}
		k := len(f.AMHS.Vehicles)
		f.AMHS.Vehicles = append(f.AMHS.Vehicles, &Vehicle{ID: fmt.Sprintf("V%02d", k+1), Free: at, FreeS: best})
	}
	for len(f.AMHS.Vehicles) > want && !f.AMHS.Vehicles[len(f.AMHS.Vehicles)-1].Free.After(at) {
		f.AMHS.Vehicles = f.AMHS.Vehicles[:len(f.AMHS.Vehicles)-1]
	}
}

// amhsGap is the least distance between two vehicles on the interbay loop
// (a vehicle is 0.8 m long).
const amhsGap = 1.4

// resMargin separates two vehicles' stays on one spur.
const resMargin = time.Second

// maxLaps bounds the extra loop laps a vehicle drives while a spur is busy.
const maxLaps = 12

var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// phase is where on the interbay loop a vehicle that is at s at t was at the
// epoch: every vehicle on the loop moves at the fleet speed, so two keep
// their distance while both stay on it.
func (n *amhsNet) phase(t time.Time, s float64) float64 {
	return ringMod(s-n.Speed*float64(t.UnixNano())/1e9, n.byID["IB"].Len)
}

func ringMod(x, m float64) float64 {
	r := x - m*math.Floor(x/m)
	if r >= m || r < 0 {
		return 0
	}
	return r
}

// clearance is the distance of an idle phase to the nearest other vehicle's
// idle phase (after its plan).
func (f *Fab) clearance(v *Vehicle, ph float64) float64 {
	n := amhsNetwork
	L := n.byID["IB"].Len
	d := L
	for _, o := range f.AMHS.Vehicles {
		if o == v {
			continue
		}
		x := math.Abs(ph - n.phase(o.Free, o.FreeS))
		d = math.Min(d, math.Min(x, L-x))
	}
	return d
}

// loopIv is a stretch of time (unix ns; t0 math.MinInt64: since before its
// plan) a vehicle is on the interbay loop, with its phase.
type loopIv struct {
	t0, t1 int64
	ph     float64
}

// vehLoops are a vehicle's stretches on the loop.
type vehLoops struct {
	id  string
	ivs []loopIv
}

// loopIntervals are the stretches every vehicle is on the interbay loop per
// its plan: its runs on the loop, idling between jobs and after its plan.
func (f *Fab) loopIntervals() []vehLoops {
	out := make([]vehLoops, 0, len(f.AMHS.Vehicles))
	for _, o := range f.AMHS.Vehicles {
		out = append(out, vehLoops{o.ID, o.loops()})
	}
	return out
}

func (o *Vehicle) loops() []loopIv {
	n := amhsNetwork
	key := [3]int64{int64(len(o.Segs)), o.Free.UnixNano(), 0}
	if len(o.Segs) > 0 {
		key[2] = o.Segs[0].T0.UnixNano()
	}
	if o.ivs != nil && key == o.ivsKey {
		return o.ivs
	}
	ivs := make([]loopIv, 0, len(o.Segs)+1)
	for i, s := range o.Segs {
		t0, t1 := s.T0.UnixNano(), s.T1.UnixNano()
		if s.Track == "IB" && s.Hoist == "" && s.S1 > s.S0 {
			if i == 0 {
				ivs = append(ivs, loopIv{math.MinInt64, t0, n.phase(s.T0, s.S0)})
			}
			ivs = append(ivs, loopIv{t0, t1, n.phase(s.T0, s.S0)})
		}
		next := int64(math.MaxInt64)
		if i+1 < len(o.Segs) {
			next = o.Segs[i+1].T0.UnixNano()
		}
		if next <= t1 || s.Hoist != "" {
			continue
		}
		// idle on the loop until its next segment: after a run on the loop, or out of a spur
		if s.Track == "IB" && s.S1 > s.S0 {
			ivs = append(ivs, loopIv{t1, next, n.phase(s.T1, s.S1)})
		} else if tr := n.byID[s.Track]; tr != nil && s.Track != "IB" && s.S1 >= tr.Len-1e-6 {
			ivs = append(ivs, loopIv{t1, next, n.phase(s.T1, tr.SOut)})
		}
	}
	if len(o.Segs) == 0 {
		ivs = append(ivs, loopIv{math.MinInt64, math.MaxInt64, n.phase(o.Free, o.FreeS)})
	}
	o.ivs, o.ivsKey = ivs, key
	return ivs
}

type gapSpan struct{ a, b float64 }

// mergeWait is how long (s) a vehicle that would join the interbay loop at s
// at tm waits before the merge, so that it keeps amhsGap from every vehicle on
// the loop while it is there (stay seconds, < 0: for good).
func (f *Fab) mergeWait(v *Vehicle, tm time.Time, s, stay float64, loops []vehLoops) float64 {
	n := amhsNetwork
	L := n.byID["IB"].Len
	horizon := L / n.Speed
	from, end := tm.UnixNano(), int64(math.MaxInt64)
	if stay >= 0 {
		end = tm.Add(durS(stay + horizon)).UnixNano()
	}
	// waiting x / speed seconds joins with the phase ph0 - x: the x within amhsGap of another
	// vehicle's phase are taken
	ph0 := n.phase(tm, s)
	spans := f.spans[:0]
	for _, vl := range loops {
		if vl.id == v.ID {
			continue
		}
		for _, iv := range vl.ivs {
			if iv.t1 > from && iv.t0 < end {
				d := ringMod(ph0-iv.ph, L)
				spans = append(spans, gapSpan{d - amhsGap, d + amhsGap})
				if d-amhsGap < 0 {
					spans = append(spans, gapSpan{d - amhsGap + L, d + amhsGap + L})
				}
				if d+amhsGap > L {
					spans = append(spans, gapSpan{d - amhsGap - L, d + amhsGap - L})
				}
			}
		}
	}
	f.spans = spans
	slices.SortFunc(spans, func(a, b gapSpan) int { return cmp.Compare(a.a, b.a) })
	x := 0.0
	for _, sp := range spans {
		if sp.b <= x {
			continue
		}
		if sp.a > x {
			break
		}
		x = sp.b
	}
	if x > L {
		return 0 // no gap in a full loop (an oversized fleet)
	}
	return x / n.Speed
}

// idlePos is a vehicle's position at t >= Free.
func (n *amhsNet) idlePos(v *Vehicle, t time.Time) amhsPos {
	ib := n.byID["IB"]
	s := v.FreeS + n.Speed*math.Max(0, t.Sub(v.Free).Seconds())
	return amhsPos{"IB", math.Mod(s, ib.Len)}
}

func durS(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// portOf is the pickup / drop port of a location: the port itself, or a port
// of the stocker.
func (f *Fab) portOf(loc string) string {
	n := amhsNetwork
	if ps, ok := n.Units[loc]; ok && len(ps) > 0 { // a stocker or a batch tool's inside
		f.AMHS.RR++
		return ps[f.AMHS.RR%len(ps)]
	}
	return loc
}

// toolPorts are the load ports of a tool (a litho cell loads at its track).
func toolPorts(ft *FabTool) []string {
	unit := ft.Alias
	if ft.Track != "" {
		unit = ft.Track
	}
	return amhsNetwork.Units[unit]
}

// freePort is the tool's load port with the fewest FOUPs on it or coming to it.
func (f *Fab) freePort(ft *FabTool) string {
	ports := toolPorts(ft)
	if len(ports) == 0 {
		return ""
	}
	count := map[string]int{}
	for _, l := range f.Lots {
		count[l.Loc]++
	}
	best := ports[0]
	for _, p := range ports[1:] {
		if count[p] < count[best] {
			best = p
		}
	}
	return best
}

// visit is a vehicle's stay on a spur (entered during leg 0: the drive to the
// pickup, or 1: the carry).
type visit struct {
	track  string
	t0, t1 time.Time
	leg    int
}

// amhsJob is a planned transfer of one vehicle.
type amhsJob struct {
	v                        *Vehicle
	start, picked, delivered time.Time
	end                      time.Time
	free                     amhsPos
	segs                     []OhtSeg
	visits                   []visit
	dist                     float64
}

// buildJob plans a vehicle's transfer from A to B requested at rq, driving
// laps[0] / laps[1] extra loop laps before entering the pickup's / the drop's
// spur. Leaving a spur it waits before the merge for a gap on the loop.
func (f *Fab) buildJob(v *Vehicle, rq time.Time, A, B amhsPos, fromPort, toPort, lot string, laps [2]int,
	loops []vehLoops) *amhsJob {
	n := amhsNetwork
	ib := n.byID["IB"]
	j := &amhsJob{v: v, start: maxT(rq, v.Free), segs: make([]OhtSeg, 0, 12), visits: make([]visit, 0, 3)}
	t := j.start
	leg := 0
	add := func(s OhtSeg) {
		j.segs = append(j.segs, s)
		if s.Track != "IB" {
			if k := len(j.visits) - 1; k >= 0 && j.visits[k].track == s.Track && !j.visits[k].t1.Before(s.T0) {
				j.visits[k].t1 = s.T1
			} else {
				j.visits = append(j.visits, visit{s.Track, s.T0, s.T1, leg})
			}
		}
		t = s.T1
	}
	run := func(track string, s0, s1 float64, carry string) {
		if s1-s0 < 1e-9 {
			return
		}
		add(OhtSeg{T0: t, T1: t.Add(durS((s1 - s0) / n.Speed)), Track: track, S0: s0, S1: s1, Lot: carry})
		if carry != "" {
			j.dist += s1 - s0
		}
	}
	wait := func(track string, s, w float64) {
		if w > 0 {
			add(OhtSeg{T0: t, T1: t.Add(durS(w)), Track: track, S0: s, S1: s})
		}
	}
	hoist := func(kind, port string, p amhsPos) time.Time {
		add(OhtSeg{T0: t, T1: t.Add(durS(n.HoistS)), Track: p.Track, S0: p.S, S1: p.S, Hoist: kind, Port: port, Lot: lot})
		return t
	}
	ahead := func(s, target float64, extra int) float64 { // loop distance from s to target
		d := ringMod(target-s, ib.Len)
		if d > ib.Len-1e-9 {
			d = 0
		}
		return d + float64(extra)*ib.Len
	}
	entry := func(p amhsPos) float64 { // where the loop is left for p
		if p.Track == "IB" {
			return p.S
		}
		return n.byID[p.Track].SIn
	}
	// leave a spur onto the loop: hold a vehicle length before the merge until there is a gap
	leave := func(tr *amhsTrack, s float64, carry string, stay float64) {
		hold := math.Max(s, tr.Len-1)
		run(tr.ID, s, hold, carry)
		tm := t.Add(durS((tr.Len - hold) / n.Speed))
		wait(tr.ID, hold, f.mergeWait(v, tm, tr.SOut, stay, loops))
		run(tr.ID, hold, tr.Len, carry)
	}
	// to the pickup
	cur := n.idlePos(v, j.start)
	run("IB", cur.S, cur.S+ahead(cur.S, entry(A), laps[0]), "")
	if A.Track != "IB" {
		run(A.Track, 0, A.S, "")
	}
	j.picked = hoist("pick", fromPort, A)
	leg = 1
	// carry
	if A.Track != "IB" && B.Track == A.Track && B.S >= A.S-1e-9 && laps[1] == 0 {
		run(A.Track, A.S, B.S, lot)
	} else {
		s := A.S
		if A.Track != "IB" {
			tr := n.byID[A.Track]
			leave(tr, A.S, lot, ahead(tr.SOut, entry(B), laps[1])/n.Speed)
			s = tr.SOut
		}
		run("IB", s, s+ahead(s, entry(B), laps[1]), lot)
		if B.Track != "IB" {
			run(B.Track, 0, B.S, lot)
		}
	}
	j.delivered = hoist("drop", toPort, B)
	leg = 2
	// back onto the loop
	if B.Track != "IB" {
		tr := n.byID[B.Track]
		leave(tr, B.S, "", -1)
		j.free = amhsPos{"IB", tr.SOut}
	} else {
		wait("IB", B.S, f.mergeWait(v, t, B.S, -1, loops))
		j.free = B
	}
	j.end = t
	return j
}

// conflict is the first spur stay of a job that overlaps another vehicle's
// (-1: none): one vehicle at a time on a spur, so none passes another that
// stands at a port.
func (f *Fab) conflict(j *amhsJob) int {
	for k, vi := range j.visits {
		for _, r := range f.AMHS.Res[vi.track] {
			if r.Vehicle != j.v.ID && vi.t0.Before(r.T1.Add(resMargin)) && r.T0.Before(vi.t1.Add(resMargin)) {
				return k
			}
		}
	}
	return -1
}

// planJob plans a vehicle's transfer: while a spur it needs is taken, it
// drives another lap of the loop (with the FOUP when it already carries it).
func (f *Fab) planJob(v *Vehicle, rq time.Time, A, B amhsPos, fromPort, toPort, lot string, loops []vehLoops) *amhsJob {
	var laps [2]int
	var j *amhsJob
	for range maxLaps {
		j = f.buildJob(v, rq, A, B, fromPort, toPort, lot, laps, loops)
		k := f.conflict(j)
		if k < 0 {
			break
		}
		laps[j.visits[k].leg]++
	}
	return j
}

// move plans the transfer of a lot's FOUP to `to` (a load port or a stocker)
// requested at `at`, and returns when it is delivered. A FOUP already there
// (or a location the network does not know) does not move.
func (f *Fab) move(l *Lot, to string, at time.Time, kind string) time.Time {
	n := amhsNetwork
	f.fleet(at)
	from := l.Loc
	rq := maxT(at, l.Arrive)
	if from == to || to == "" {
		return rq
	}
	fromPort, toPort := f.portOf(from), f.portOf(to)
	pa, okA := n.Ports[fromPort]
	pb, okB := n.Ports[toPort]
	if !okA || !okB {
		l.Loc, l.Arrive = to, rq
		return rq
	}
	A := amhsPos{pa.Track, pa.S}
	B := amhsPos{pb.Track, pb.S}
	f.pruneRes(rq.Add(-10 * time.Minute))
	// the three vehicles that could reach the pickup first are planned; the one that picks first
	// takes the job
	type cand struct {
		v     *Vehicle
		reach time.Time
	}
	cands := make([]cand, 0, len(f.AMHS.Vehicles))
	for _, v := range f.AMHS.Vehicles {
		start := maxT(rq, v.Free)
		_, d := n.path(n.idlePos(v, start), A)
		cands = append(cands, cand{v, start.Add(durS(d / n.Speed))})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].reach.Before(cands[j].reach) })
	loops := f.loopIntervals()
	var best *amhsJob
	for _, c := range cands[:min(3, len(cands))] {
		if best != nil && !c.reach.Add(durS(n.HoistS)).Before(best.picked) {
			break // it cannot pick earlier
		}
		j := f.planJob(c.v, rq, A, B, fromPort, toPort, l.Code, loops)
		if best == nil || j.picked.Before(best.picked) || j.picked.Equal(best.picked) && j.delivered.Before(best.delivered) {
			best = j
		}
	}
	v := best.v
	busy := best.end.Sub(best.start).Seconds()
	v.Segs = append(v.Segs, best.segs...)
	v.BusyS += busy
	v.Jobs++
	v.Free, v.FreeS = best.end, best.free.S
	// keep the plan short: what is over for ten minutes is dropped
	keep := v.Segs[:0]
	for _, s := range v.Segs {
		if s.T1.After(rq.Add(-10 * time.Minute)) {
			keep = append(keep, s)
		}
	}
	v.Segs = keep
	if f.AMHS.Res == nil {
		f.AMHS.Res = map[string][]Resv{}
	}
	for _, vi := range best.visits {
		f.AMHS.Res[vi.track] = append(f.AMHS.Res[vi.track], Resv{Vehicle: v.ID, T0: vi.t0, T1: vi.t1})
	}
	f.out.transfers = append(f.out.transfers, Transfer{ID: f.AMHS.NextID, Lot: l.Code, Product: l.Product, Kind: kind, From: from, To: to,
		Vehicle: v.ID, Requested: rq, Picked: best.picked, Delivered: best.delivered, Dist: best.dist, Busy: busy})
	f.AMHS.NextID++
	l.Loc, l.Arrive = to, best.delivered
	return best.delivered
}

// pruneRes drops the spur stays that ended before t.
func (f *Fab) pruneRes(t time.Time) {
	for k, rs := range f.AMHS.Res {
		keep := rs[:0]
		for _, r := range rs {
			if r.T1.After(t) {
				keep = append(keep, r)
			}
		}
		f.AMHS.Res[k] = keep
	}
}

// deliver brings a lot to a free load port of the tool (dispatch) and returns
// when it is there.
func (f *Fab) deliver(l *Lot, ft *FabTool, at time.Time) time.Time {
	if l.Loc != "" {
		for _, p := range toolPorts(ft) {
			if p == l.Loc {
				return maxT(at, l.Arrive) // already on one of its ports
			}
		}
	}
	if ft.Class == classBatch {
		// a batch tool takes the FOUP in from its port
		unit := ft.Alias
		if l.Loc == unit {
			return maxT(at, l.Arrive)
		}
		d := f.move(l, f.freePort(ft), at, "deliver")
		l.Loc = unit
		return d
	}
	return f.move(l, f.freePort(ft), at, "deliver")
}

// store moves the lots left on a load port (finished, not dispatched yet, or
// on hold) into the stocker of their next area.
func (f *Fab) store() {
	n := amhsNetwork
	var lots []*Lot
	for _, l := range f.Lots {
		if (l.Status == "QUEUED" || l.Status == "HOLD") && l.Loc != "" && !n.isStocker(l.Loc) {
			lots = append(lots, l)
		}
	}
	sort.Slice(lots, func(i, j int) bool {
		ti, tj := lots[i].Row.Queued, lots[j].Row.Queued
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return lots[i].Code < lots[j].Code
	})
	for _, l := range lots {
		area := areaMet
		if rs := f.stepOf(l); rs != nil {
			area = stepArea(rs.Type)
		}
		at := l.Row.Queued
		if l.Status == "HOLD" {
			at = maxT(l.PrevEnd, l.Row.Queued)
		}
		d := f.move(l, n.buffer(area), at, "store")
		if l.Status == "QUEUED" {
			l.Ready = maxT(l.Ready, d)
		}
	}
}

// ship takes a finished (or scrapped) lot's FOUP to the start stocker.
func (f *Fab) ship(l *Lot, at time.Time) {
	if l.Loc != "" && !amhsNetwork.isStocker(l.Loc) {
		f.move(l, amhsNetwork.Release, at, "ship")
	}
}

// shiftAMHS moves the transport plan by d (the simulator was stopped).
func (a *AMHS) shift(d time.Duration) {
	if a == nil {
		return
	}
	for _, v := range a.Vehicles {
		v.Free = v.Free.Add(d)
		for i := range v.Segs {
			v.Segs[i].T0 = v.Segs[i].T0.Add(d)
			v.Segs[i].T1 = v.Segs[i].T1.Add(d)
		}
	}
	for _, rs := range a.Res {
		for i := range rs {
			rs[i].T0, rs[i].T1 = rs[i].T0.Add(d), rs[i].T1.Add(d)
		}
	}
}

// utilization of the fleet over the vehicles' lifetimes so far (0..1).
func (a *AMHS) utilization(from, to time.Time) float64 {
	if a == nil || !to.After(from) {
		return 0
	}
	busy := 0.0
	for _, v := range a.Vehicles {
		busy += v.BusyS
	}
	return busy / (float64(len(a.Vehicles)) * to.Sub(from).Seconds())
}

// ---------------------------------------------------------------------------
// Live snapshot for the app (core.settings demo_amhs_state)

const amhsStateKey = "demo_amhs_state"

// AmhsPortLot is a FOUP on a load port from From until Until (zero: open).
type AmhsPortLot struct {
	Port    string     `json:"port"`
	Lot     string     `json:"lot"`
	Product string     `json:"product,omitempty"`
	Hot     bool       `json:"hot,omitempty"`
	From    *time.Time `json:"from,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
}

// AmhsSnapshot is what the twin animates: the vehicles' plans and the FOUPs on
// the load ports, with the stocker contents.
type AmhsSnapshot struct {
	Now      time.Time      `json:"now"`
	Speed    float64        `json:"speed"`
	HoistS   float64        `json:"hoist_s"`
	Vehicles []AmhsVehicle  `json:"vehicles"`
	Ports    []AmhsPortLot  `json:"ports"`
	Stockers map[string]int `json:"stockers"`
	Moving   int            `json:"moving"`
}

type AmhsVehicle struct {
	ID string `json:"id"`
	// Before: where it circulated idle from before its first segment; After: from the end of its
	// plan on (between two segments it circulates from where the earlier one left the loop)
	Before AmhsAnchor `json:"before"`
	After  AmhsAnchor `json:"after"`
	Segs   []OhtSeg   `json:"segs"`
}

// AmhsAnchor is an idle vehicle's interbay position s at time t (it moves on at the fleet speed).
type AmhsAnchor struct {
	T time.Time `json:"t"`
	S float64   `json:"s"`
}

// inside reports whether a unit takes FOUPs in (stockers, batch tools): a FOUP dropped on its
// port disappears into it, one picked from it comes out shortly before.
func (f *Fab) inside(unit string) bool {
	if amhsNetwork.isStocker(unit) {
		return true
	}
	ft := f.byAlias[unit]
	return ft != nil && ft.Class == classBatch
}

// snapshot is the transport state around now: the segments that end after
// half a minute ago and every planned one, and the FOUPs on ports.
func (f *Fab) snapshot(now time.Time) *AmhsSnapshot {
	n := amhsNetwork
	snap := &AmhsSnapshot{Now: now, Speed: n.Speed, HoistS: n.HoistS, Stockers: map[string]int{}}
	if f.AMHS == nil {
		return snap
	}
	ib := n.byID["IB"]
	back := now.Add(-30 * time.Second) // the app shows the fab a few seconds behind (plans are known before they show)
	half := durS(n.HoistS / 2)
	lead := 15 * time.Second
	planned := map[string]bool{}
	for _, v := range f.AMHS.Vehicles {
		av := AmhsVehicle{ID: v.ID, After: AmhsAnchor{v.Free, v.FreeS}, Before: AmhsAnchor{v.Free, v.FreeS}}
		for _, s := range v.Segs {
			if s.T1.After(back) {
				av.Segs = append(av.Segs, s)
			}
		}
		if len(av.Segs) > 0 {
			first := av.Segs[0]
			if first.Track == "IB" {
				av.Before = AmhsAnchor{first.T0, math.Mod(first.S0, ib.Len)}
			} else {
				av.Before = AmhsAnchor{first.T0, n.byID[first.Track].SIn}
			}
		}
		for _, s := range av.Segs {
			if !s.T0.After(now) && s.T1.After(now) {
				snap.Moving++
			}
			if s.Lot != "" {
				planned[s.Lot] = true
			}
			unit := unitOfPort(s.Port)
			switch s.Hoist {
			case "pick":
				u := s.T0.Add(half)
				e := AmhsPortLot{Port: s.Port, Lot: s.Lot, Until: &u}
				if f.inside(unit) {
					fr := s.T0.Add(-lead)
					e.From = &fr
				}
				snap.Ports = append(snap.Ports, e)
			case "drop":
				fr := s.T1.Add(-half)
				e := AmhsPortLot{Port: s.Port, Lot: s.Lot, From: &fr}
				if f.inside(unit) {
					u := s.T1.Add(lead)
					e.Until = &u
				}
				snap.Ports = append(snap.Ports, e)
			}
		}
		snap.Vehicles = append(snap.Vehicles, av)
	}
	// a FOUP dropped on a load port stays until its next pick there
	for i := range snap.Ports {
		p := &snap.Ports[i]
		if p.From == nil || p.Until != nil {
			continue
		}
		for _, q := range snap.Ports {
			if q.Lot == p.Lot && q.Port == p.Port && q.From == nil && q.Until != nil && q.Until.After(*p.From) {
				u := *q.Until
				p.Until = &u
			}
		}
	}
	keep := snap.Ports[:0]
	for _, p := range snap.Ports {
		if p.From == nil && p.Until != nil {
			covered := false
			for _, q := range snap.Ports {
				if q.From != nil && q.Until != nil && q.Lot == p.Lot && q.Port == p.Port && q.Until.Equal(*p.Until) {
					covered = true
				}
			}
			if covered {
				continue
			}
		}
		keep = append(keep, p)
	}
	snap.Ports = keep
	codes := make([]string, 0, len(f.Lots))
	for c := range f.Lots {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		l := f.Lots[c]
		switch {
		case n.isStocker(l.Loc):
			if !l.Arrive.After(now) {
				snap.Stockers[l.Loc]++
			}
		case planned[l.Code] || l.Arrive.After(now):
		case n.Ports[l.Loc].Track != "":
			snap.Ports = append(snap.Ports, AmhsPortLot{Port: l.Loc, Lot: l.Code})
		}
	}
	for i := range snap.Ports {
		if l := f.Lots[snap.Ports[i].Lot]; l != nil {
			snap.Ports[i].Product, snap.Ports[i].Hot = l.Product, l.Priority == 1
		}
	}
	return snap
}

func unitOfPort(port string) string {
	if i := strings.LastIndexByte(port, ':'); i > 0 {
		return port[:i]
	}
	return port
}
