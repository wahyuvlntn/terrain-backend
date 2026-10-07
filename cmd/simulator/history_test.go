package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeIDs gives every plant definition a database id, as loadIDs would.
func fakeIDs(p *Plant) *IDs {
	ids := &IDs{Equip: map[string]int64{}, Loc: map[string]int64{}, EP: map[string]map[string]int64{}, Code: map[string]int64{},
		ModelAlarm: map[string]map[string]int64{}, Rel: map[string]map[string]int64{}, User: map[string]int64{}, UserName: map[string]string{},
		Minutes: map[string]map[string]int{}}
	var n int64
	next := func() int64 { n++; return n }
	for _, a := range p.Assets {
		ids.Equip[a.Spec.Alias], ids.Loc[a.Spec.Alias] = next(), 1
		ids.EP[a.Spec.Alias] = map[string]int64{}
		for _, ps := range a.Model.Params {
			ids.EP[a.Spec.Alias][ps.Alias] = next()
		}
	}
	for _, c := range alarmCodes {
		ids.Code[c[0]] = next()
	}
	for _, m := range models() {
		ids.ModelAlarm[m.Name], ids.Rel[m.Name] = map[string]int64{}, map[string]int64{}
		for _, al := range m.Alarms {
			ids.ModelAlarm[m.Name][al.Code] = next()
		}
		for _, r := range m.Relations {
			ids.Rel[m.Name][r.Name] = next()
		}
	}
	for _, u := range users {
		ids.User[u.Username], ids.UserName[u.Username] = next(), u.Name
	}
	return ids
}

// TestHistoryShort generates 10 days of history in memory: LBNL episodes
// with their FDD alarms and corrective orders, operator set point changes
// stored as control requests, values for every parameter.
func TestHistoryShort(t *testing.T) {
	p := newPlant(2026, testLoc)
	h := newHistory(context.Background(), nil, p, fakeIDs(p), 10, 2026)
	values := 0
	h.copyValues = func(rows [][]any) (int64, error) {
		values += len(rows)
		return int64(len(rows)), nil
	}
	started := time.Now()
	if err := h.Generate(); err != nil {
		t.Fatal(err)
	}
	t.Logf("10 days in %v: %d values, %d orders, %d alarm events, %d status rows, %d LBNL episodes, %d control requests",
		time.Since(started).Round(time.Millisecond), values, len(h.orders), len(h.eventR), len(h.statusR), len(h.fdd), len(h.ctlR))
	if values == 0 {
		t.Fatal("no parameter values")
	}

	// every LBNL fault kind is planned, detected and repaired by its FDD order
	planned := map[string]int{}
	for _, e := range h.episodes {
		if e.Kind == "lbnl" {
			planned[e.Fault]++
		}
	}
	t.Logf("LBNL episodes planned: %v", planned)
	for _, m := range models() {
		for _, f := range m.Faults {
			if f.Manual && planned[f.Kind] == 0 {
				t.Errorf("no episode of %s", f.Kind)
			}
		}
	}
	for _, f := range h.fdd {
		// the episode's order: the first FDD order of its code raised after detection
		var order *hOrder
		for _, o := range h.orders {
			if o.Asset == f.Asset && o.FromSystem && len(o.Issues) == 1 && o.Issues[0] == f.Code && !f.start.IsZero() &&
				!o.Created.Before(f.start) && (order == nil || o.Created.Before(order.Created)) {
				order = o
			}
		}
		state := "open"
		if order != nil && order.completed != nil {
			state = "repaired " + order.completed.Sub(f.Onset).Round(time.Hour).String() + " after onset"
		}
		t.Logf("%s %s on %s at %s, detected %v: %s", f.Code, f.Kind, f.Asset.Spec.Alias, f.Onset.Format("01-02 15:04"), !f.start.IsZero(), state)
		if f.start.IsZero() {
			t.Errorf("%s on %s never detected", f.Kind, f.Asset.Spec.Alias)
			continue
		}
		if d := f.start.Sub(f.Onset); d < time.Hour || d > 4*time.Hour+time.Minute {
			t.Errorf("%s detected %v after onset, want 1 .. 4 h", f.Kind, d)
		}
		if order == nil {
			t.Errorf("%s on %s: no FDD corrective order", f.Kind, f.Asset.Spec.Alias)
			continue
		}
		if order.RelName != codeName(f.Code)+" (FDD)" || order.Type != 1 || !strings.Contains(order.Desc, f.Code) {
			t.Errorf("FDD order %q relation %q", order.Desc, order.RelName)
		}
		if order.completed != nil {
			// a later episode of the same fault may still run at the end
			later := false
			for _, g := range h.fdd {
				later = later || (g.Asset == f.Asset && g.Kind == f.Kind && g.Onset.After(f.Onset) && !g.done)
			}
			if !f.done || (f.Asset.Faults[f.Kind] != nil && !later) {
				t.Errorf("%s on %s still active after its order completed", f.Kind, f.Asset.Spec.Alias)
			}
			if len(order.updates) == 0 {
				t.Errorf("FDD order of %s has no work notes", f.Code)
			}
		}
	}
	ext := 0
	for _, r := range h.eventR {
		if key, _ := r[2].(string); strings.HasPrefix(key, "ext:fdd:") {
			ext++
		}
	}
	if ext == 0 {
		t.Error("no FDD alarm events")
	}

	// operator set point changes: applied, recorded and set back
	if len(h.ctlR) == 0 {
		t.Fatal("no control requests")
	}
	for _, r := range h.ctlR {
		if r[5] != "APPLIED" || r[0] != h.ids.Equip["CHWS_01"] {
			t.Errorf("control request %v", r)
		}
	}
	s := p.sys
	if s.manual("chws_reset_mode") || s.manual("staging_mode") || s.ctl("approach_sp", defApproachSP) != defApproachSP ||
		s.ctl("riser_dp_sp", defRiserDPSP) != defRiserDPSP {
		t.Errorf("controls not set back at the end: %v", s.Ctl)
	}
}

// TestHistoryChillersAvailable generates a month of history (the demo
// deployment's window) and checks that overlapping maintenance and trips
// never stop every chiller: the fab always has load, so every stored sample
// of the plant controller shows a running chiller. The LBNL episodes are
// still repaired by their orders before the end of the window.
func TestHistoryChillersAvailable(t *testing.T) {
	p := newPlant(3, testLoc)
	h := newHistory(context.Background(), nil, p, fakeIDs(p), 30, 3)
	ep := h.ids.EP["CHWS_01"]["chillers_running"]
	var off []time.Time
	h.copyValues = func(rows [][]any) (int64, error) {
		for _, r := range rows {
			if r[0] == ep && r[2].(float64) < 1 {
				off = append(off, r[1].(time.Time))
			}
		}
		return int64(len(rows)), nil
	}
	if err := h.Generate(); err != nil {
		t.Fatal(err)
	}
	if len(off) > 0 {
		t.Errorf("%d samples without a running chiller, from %s to %s", len(off), off[0].Format(time.RFC3339), off[len(off)-1].Format(time.RFC3339))
	}
	for _, f := range h.fdd {
		if f.ClearBy.Before(h.end) && !f.done {
			t.Errorf("%s on %s (planned end %s) not repaired by the end", f.Kind, f.Asset.Spec.Alias, f.ClearBy.Format(time.RFC3339))
		}
	}
}
