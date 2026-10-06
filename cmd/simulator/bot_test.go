package main

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"
)

// TestLiveKeepsDutyChillers: orders due together on every chiller (an early
// warning in hot weather) start one at a time. The bot stops a chiller, its
// dedicated pumps or a tower only while the other units cover the duty
// chillers, judged on one copy of the live plant per cycle (taken on the
// simulation loop); random trips follow the same rule.
func TestLiveKeepsDutyChillers(t *testing.T) {
	p := newPlant(3, testLoc)
	p.sys.Cmd["stage_n"] = 2 // two duty chillers, one standby
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &Live{snaps: make(chan snapshotReq)}
	snaps := 0
	go func() { // the simulation loop serving Snapshot
		for {
			select {
			case req := <-l.snaps:
				snaps++
				req.reply <- p.Clone(req.seed)
			case <-ctx.Done():
				return
			}
		}
	}()
	b := &Bot{ctx: ctx, live: l}
	for _, c := range []struct {
		alias string
		want  bool
	}{
		{"CH_01", true},
		{"CH_02", false}, // CH_01 started: the other two are the duty chillers
		{"CWP_03", false},
		{"CHWP_01", true}, // its chiller is already stopped
		{"CT_01", true},
		{"CT_02", false},
		{"MAU_01", true}, // no standby rule
	} {
		got, err := b.mayStop(c.alias)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s may stop: %v, want %v", c.alias, got, c.want)
		}
		if got {
			b.snap.ByAlias[c.alias].Maint = true // work started, as in work()
		}
	}
	if snaps != 1 || p.ByAlias["CH_01"].Maint {
		t.Errorf("%d snapshots per cycle (want 1), live CH_01 in maintenance: %v", snaps, p.ByAlias["CH_01"].Maint)
	}

	// CH_01 in maintenance in the live plant (syncForced): random trips spare
	// the duty chillers and their pumps, a tower still trips
	p.ByAlias["CH_01"].Maint = true
	p.updateDuty(testStart)
	r := rand.New(rand.NewPCG(3, 3))
	tripped := map[string]bool{}
	for i := 0; i < 2000; i++ {
		if a := p.injectRandomTrip(r, testStart); a != nil {
			tripped[a.Spec.Alias] = true
			a.SpikeUntil = time.Time{}
		}
	}
	for _, alias := range []string{"CH_02", "CH_03", "CHWP_02", "CHWP_03", "CWP_02", "CWP_03"} {
		if tripped[alias] {
			t.Errorf("random trip on %s without a standby chiller", alias)
		}
	}
	if !tripped["CT_01"] && !tripped["CT_02"] && !tripped["CT_03"] {
		t.Error("no random trip on a cooling tower (one standby)")
	}
}
