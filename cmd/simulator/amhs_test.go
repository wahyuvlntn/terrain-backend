package main

import (
	"encoding/json"
	"math"
	"sort"
	"testing"
	"time"
)

// TestAmhsNetwork: every load port and stocker port lies on the one-way
// network, every tool of the fab has load ports, and the shortest path
// between any two ports is a connected run that ends at the target.
func TestAmhsNetwork(t *testing.T) {
	n := amhsNetwork
	ib := n.byID["IB"]
	for id, p := range n.Ports {
		tr := n.byID[p.Track]
		if tr == nil || p.S < 0 || p.S > tr.Len+1e-6 {
			t.Errorf("port %s at %s s=%.2f is off the network", id, p.Track, p.S)
		}
	}
	for unit, ps := range n.Units {
		for _, id := range ps {
			if _, ok := n.Ports[id]; !ok {
				t.Errorf("%s: port %s missing", unit, id)
			}
		}
	}
	for _, s := range append([]string{n.Release}, n.Buffers["default"], n.buffer(areaLitho), n.buffer(areaDiff)) {
		if !n.isStocker(s) || len(n.Units[s]) == 0 {
			t.Errorf("%s is not a stocker with ports", s)
		}
	}
	p := newPlant(1, testLoc)
	for _, ft := range p.fab.Tools {
		if len(toolPorts(ft)) == 0 {
			t.Errorf("%s has no load ports on the network", ft.Alias)
		}
	}
	maxSpur := 0.0
	for _, tr := range n.Tracks {
		if tr.ID != "IB" {
			maxSpur = math.Max(maxSpur, tr.Len)
			if tr.SIn < 0 || tr.SIn > ib.Len || tr.SOut < 0 || tr.SOut > ib.Len {
				t.Errorf("spur %s joins the loop at %.2f / %.2f", tr.ID, tr.SIn, tr.SOut)
			}
		}
	}
	ids := make([]string, 0, len(n.Ports))
	for id := range n.Ports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, a := range ids {
		for _, b := range ids {
			pa, pb := n.Ports[a], n.Ports[b]
			segs, d := n.path(amhsPos{pa.Track, pa.S}, amhsPos{pb.Track, pb.S})
			if d < 0 || d > ib.Len+2*maxSpur {
				t.Fatalf("path %s -> %s: %.1f m", a, b, d)
			}
			sum := 0.0
			for i, s := range segs {
				sum += s.S1 - s.S0
				if i > 0 && segs[i-1].Track == s.Track && s.Track != "IB" {
					t.Fatalf("path %s -> %s runs %s twice", a, b, s.Track)
				}
			}
			if math.Abs(sum-d) > 1e-6 {
				t.Fatalf("path %s -> %s: segments %.3f m, length %.3f m", a, b, sum, d)
			}
			if len(segs) > 0 {
				last := segs[len(segs)-1]
				end := last.S1
				if last.Track == "IB" {
					end = math.Mod(end, ib.Len)
				}
				if last.Track != pb.Track || math.Abs(end-pb.S) > 1e-6 && math.Abs(end-pb.S-ib.Len) > 1e-6 && math.Abs(end+ib.Len-pb.S) > 1e-6 {
					t.Fatalf("path %s -> %s ends at %s %.2f, want %s %.2f", a, b, last.Track, end, pb.Track, pb.S)
				}
			}
		}
	}
}

// TestAmhsTransport: three days of production move every lot by the
// simulated vehicles: transfers take realistic times, a vehicle does one job
// at a time, a run starts only when its FOUP is on one of the tool's ports
// (and the FOUP stays there until it ends), idle vehicles keep their distance
// and the twin snapshot is consistent and small.
func TestAmhsTransport(t *testing.T) {
	p := newPlant(5, testLoc)
	start := testStart.UTC()
	days := 3.0
	out := fabOnly(p, start, days, 2*time.Minute, nil)
	f := p.fab
	n := amhsNetwork
	end := start.Add(hours(days * 24))
	from := start.Add(12 * time.Hour)
	if len(out.transfers) == 0 {
		t.Fatal("no transfers")
	}
	kinds := map[string]int{}
	byLot := map[string][]Transfer{}
	byVeh := map[string][]Transfer{}
	minutes, cnt, carry, maxMin := 0.0, 0, 0.0, 0.0
	for _, tr := range out.transfers {
		byLot[tr.Lot] = append(byLot[tr.Lot], tr)
		byVeh[tr.Vehicle] = append(byVeh[tr.Vehicle], tr)
		if tr.Picked.Before(tr.Requested.Add(durS(n.HoistS))) || tr.Delivered.Sub(tr.Picked).Seconds() < n.HoistS || tr.Dist <= 0 {
			t.Fatalf("transfer %+v: impossible times", tr)
		}
		if tr.Requested.Before(from) {
			continue
		}
		kinds[tr.Kind]++
		m := tr.Delivered.Sub(tr.Requested).Minutes()
		minutes += m
		maxMin = math.Max(maxMin, m)
		carry += tr.Dist
		cnt++
	}
	mean := minutes / float64(cnt)
	busy := 0.0
	for _, v := range f.AMHS.Vehicles {
		busy += v.BusyS
	}
	t.Logf("%d transfers / day %v, request to delivery %.1f min (max %.1f), carry %.0f m, fleet %d busy %.0f %%",
		int(float64(cnt)/(days-0.5)), kinds, mean, maxMin, carry/float64(cnt), len(f.AMHS.Vehicles),
		100*busy/(float64(len(f.AMHS.Vehicles))*end.Sub(start.Add(-warmUp)).Seconds()))
	if mean < 0.7 || mean > 8 {
		t.Errorf("mean transfer %.1f min, want 0.7 - 8", mean)
	}
	if kinds["deliver"] == 0 || kinds["store"] == 0 || kinds["ship"] == 0 {
		t.Errorf("transfer kinds %v", kinds)
	}
	for id, ts := range byVeh {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Picked.Before(ts[j].Picked) })
		for i := 1; i < len(ts); i++ {
			if ts[i].Picked.Add(-durS(n.HoistS)).Before(ts[i-1].Delivered) {
				t.Fatalf("%s picks %s at %v before dropping %s at %v", id, ts[i].Lot, ts[i].Picked, ts[i-1].Lot, ts[i-1].Delivered)
			}
		}
	}
	checked := 0
	for _, s := range out.steps {
		ft := f.byAlias[s.Tool]
		if ft == nil || s.Started.Before(from) || s.Ended.IsZero() || s.Outcome == "ABORTED" || len(toolPorts(ft)) == 0 {
			continue
		}
		var at *Transfer
		for i, tr := range byLot[s.Lot] {
			if !tr.Delivered.After(s.Started) {
				at = &byLot[s.Lot][i]
			} else if tr.Picked.Before(s.Ended) {
				t.Fatalf("%s %s on %s %v - %v: FOUP picked at %v", s.Lot, s.Code, s.Tool, s.Started, s.Ended, tr.Picked)
			}
		}
		if at == nil {
			continue
		}
		if u := unitOfPort(at.To); u != ft.Alias && u != ft.Track {
			t.Fatalf("%s %s on %s started %v with its FOUP at %s", s.Lot, s.Code, s.Tool, s.Started, at.To)
		}
		checked++
	}
	if checked < 100 {
		t.Errorf("only %d runs checked against their deliveries", checked)
	}
	perPort := map[string]int{}
	for _, l := range f.Lots {
		if l.Loc == "" {
			t.Fatalf("lot %s (%s) has no location", l.Code, l.Status)
		}
		if _, ok := n.Ports[l.Loc]; ok {
			perPort[l.Loc]++
		}
	}
	for port, k := range perPort {
		if k > 2 {
			t.Errorf("%d FOUPs on %s", k, port)
		}
	}
	L := n.byID["IB"].Len
	for i, a := range f.AMHS.Vehicles {
		for _, b := range f.AMHS.Vehicles[i+1:] {
			x := math.Abs(n.phase(a.Free, a.FreeS) - n.phase(b.Free, b.FreeS))
			if d := math.Min(x, L-x); d < amhsGap-1e-6 {
				t.Errorf("idle %s and %s only %.2f m apart", a.ID, b.ID, d)
			}
		}
	}
	snap := f.snapshot(end)
	b, _ := json.Marshal(snap)
	t.Logf("snapshot %d bytes: %d vehicles (%d moving), %d port FOUPs, stockers %v", len(b), len(snap.Vehicles), snap.Moving, len(snap.Ports), snap.Stockers)
	if len(snap.Vehicles) != n.Fleet || len(b) > 64<<10 {
		t.Errorf("snapshot: %d vehicles, %d bytes", len(snap.Vehicles), len(b))
	}
	for _, v := range snap.Vehicles {
		for _, s := range v.Segs {
			if n.byID[s.Track] == nil || s.T1.Before(s.T0) || s.S1 < s.S0 {
				t.Fatalf("%s segment %+v", v.ID, s)
			}
		}
	}
	for _, pl := range snap.Ports {
		if _, ok := n.Ports[pl.Port]; !ok {
			t.Fatalf("snapshot FOUP %s on unknown port %s", pl.Lot, pl.Port)
		}
	}
}

// TestAmhsFleet: a plan's fleet size sets the vehicles; two vehicles carry the
// same production with longer waits than the full fleet.
func TestAmhsFleet(t *testing.T) {
	mean := func(vehicles int) (float64, int) {
		p := newPlant(5, testLoc)
		p.fab.Cfg.Vehicles = vehicles
		start := testStart.UTC()
		out := fabOnly(p, start, 1, 2*time.Minute, nil)
		m, k := 0.0, 0
		for _, tr := range out.transfers {
			if !tr.Requested.Before(start) {
				m += tr.Delivered.Sub(tr.Requested).Minutes()
				k++
			}
		}
		return m / math.Max(float64(k), 1), len(p.fab.AMHS.Vehicles)
	}
	full, nf := mean(0)
	two, n2 := mean(2)
	t.Logf("mean transfer: %d vehicles %.1f min, %d vehicles %.1f min", nf, full, n2, two)
	if nf != amhsNetwork.Fleet || n2 != 2 {
		t.Errorf("fleets %d / %d", nf, n2)
	}
	if two < 1.2*full {
		t.Errorf("two vehicles %.1f min, full fleet %.1f min: want slower", two, full)
	}
}

// TestAmhsNoOverlap: vehicles never run into each other: one vehicle at a
// time is on a spur, and on the interbay loop every two vehicles keep
// amhsGap, checked on the plans of a day of production sampled every 0.5 s.
func TestAmhsNoOverlap(t *testing.T) {
	p := newPlant(11, testLoc)
	start := testStart.UTC()
	end := start.Add(24 * time.Hour)
	p.updateDuty(start)
	n := amhsNetwork
	segs := map[string]map[string]OhtSeg{}
	for at := start; at.Before(end); at = at.Add(2 * time.Minute) {
		p.fab.step(at, 120)
		for _, v := range p.fab.AMHS.Vehicles {
			if segs[v.ID] == nil {
				segs[v.ID] = map[string]OhtSeg{}
			}
			for _, s := range v.Segs {
				segs[v.ID][s.T0.Format(time.RFC3339Nano)+s.Track] = s
			}
		}
	}
	plans := map[string][]OhtSeg{}
	for id, m := range segs {
		for _, s := range m {
			plans[id] = append(plans[id], s)
		}
		sort.Slice(plans[id], func(i, j int) bool { return plans[id][i].T0.Before(plans[id][j].T0) })
	}
	// spur stays
	type stay struct {
		v      string
		t0, t1 time.Time
	}
	stays := map[string][]stay{}
	for id, ss := range plans {
		for i := 0; i < len(ss); i++ {
			if ss[i].Track == "IB" {
				continue
			}
			k := i
			for k+1 < len(ss) && ss[k+1].Track == ss[i].Track && !ss[k+1].T0.After(ss[k].T1) {
				k++
			}
			stays[ss[i].Track] = append(stays[ss[i].Track], stay{id, ss[i].T0, ss[k].T1})
			i = k
		}
	}
	visits := 0
	for track, ss := range stays {
		visits += len(ss)
		sort.Slice(ss, func(i, j int) bool { return ss[i].t0.Before(ss[j].t0) })
		for i := 1; i < len(ss); i++ {
			if ss[i].v != ss[i-1].v && ss[i].t0.Before(ss[i-1].t1) {
				t.Fatalf("%s: %s enters at %v while %s is there until %v", track, ss[i].v, ss[i].t0, ss[i-1].v, ss[i-1].t1)
			}
		}
	}
	// positions on the loop
	ib := n.byID["IB"]
	pos := func(ss []OhtSeg, at time.Time) (string, float64, bool) {
		i := sort.Search(len(ss), func(i int) bool { return ss[i].T1.After(at) })
		if i < len(ss) && !ss[i].T0.After(at) {
			s := ss[i]
			k := 0.0
			if s.T1.After(s.T0) {
				k = at.Sub(s.T0).Seconds() / s.T1.Sub(s.T0).Seconds()
			}
			return s.Track, s.S0 + (s.S1-s.S0)*k, true
		}
		if i == 0 {
			return "", 0, false
		}
		prev := ss[i-1] // idle since its end
		s0 := prev.S1
		if prev.Track != "IB" {
			s0 = n.byID[prev.Track].SOut
		}
		return "IB", s0 + n.Speed*at.Sub(prev.T1).Seconds(), true
	}
	close, samples := 0, 0
	minD := ib.Len
	for at := start.Add(2 * time.Hour); at.Before(end.Add(-time.Hour)); at = at.Add(500 * time.Millisecond) {
		var on []float64
		for _, ss := range plans {
			if tr, s, ok := pos(ss, at); ok && tr == "IB" {
				on = append(on, ringMod(s, ib.Len))
			}
		}
		for i := range on {
			for j := i + 1; j < len(on); j++ {
				x := math.Abs(on[i] - on[j])
				d := math.Min(x, ib.Len-x)
				minD = math.Min(minD, d)
				if d < amhsGap-0.05 {
					close++
				}
			}
		}
		samples++
	}
	t.Logf("%d spur stays; loop: %d samples, closest two vehicles %.2f m, %d pairs closer than %.1f m", visits, samples, minD, close, amhsGap)
	if close > 0 {
		t.Errorf("%d vehicle pairs closer than %.1f m on the loop (closest %.2f m)", close, amhsGap, minD)
	}
}
