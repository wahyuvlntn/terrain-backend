package api

// Live event feed of the twin's walk-mode HUD (digital_twin/getliveevents).
// The ledger tests need no database. TestTwinLiveEventsAPI runs over HTTP
// against a scratch copy of the demo database, like TestAgenticAPI:
//
//	AFDD_API_TEST_DATABASE_URL=postgres://afdd:afdd@host:5432/afdd_scratch_api?sslmode=disable go test ./internal/api/ -run TestTwinLiveEvents
//
// It deletes the events of the last hours and writes its own.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTwinKnownAt(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		kind    string
		started time.Time
		startup bool
		want    time.Time
	}{
		{"start-up scan: the start", "fault", now.Add(-3 * time.Hour), true, now.Add(-3 * time.Hour)},
		{"start-up scan: stamped after now", "alarm", now.Add(time.Millisecond), true, now},
		{"new alarm", "alarm", now.Add(-2 * time.Second), false, now},
		{"stamped after now", "trip", now.Add(3 * time.Millisecond), false, now},
		{"alarm committed late", "alarm", now.Add(-6 * time.Second), false, now},
		{"back-dated fault", "fault", now.Add(-3 * time.Hour), false, now},
		{"back-dated anomaly", "anomaly", now.Add(-4 * time.Minute), false, now},
		{"alarm history", "alarm", now.Add(-20 * time.Minute), false, now.Add(-20 * time.Minute)},
		{"fault history", "fault", now.Add(-8 * time.Hour), false, now.Add(-8 * time.Hour)},
	}
	for _, tc := range cases {
		if got := twinKnownAt(tc.kind, tc.started, now, tc.startup); !got.Equal(tc.want) {
			t.Errorf("%s: known %s, want %s", tc.name, got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
		}
	}

	// Scan times strictly increase, and a known time is kept across scans.
	l := &twinEventLedger{}
	ev := twinEvent{ID: "fault:1", Kind: "fault", Started: time.Now().Add(-3 * time.Hour)}
	load := func(context.Context, time.Time) ([]twinEvent, error) { return []twinEvent{ev}, nil }
	var last time.Time
	for i := 0; i < 5; i++ {
		now, events, err := l.scan(context.Background(), load)
		if err != nil {
			t.Fatal(err)
		}
		if !now.After(last) || now.Nanosecond()%int(time.Millisecond) != 0 {
			t.Fatalf("scan %d at %s after %s", i, now.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano))
		}
		if !events[0].Known.Equal(ev.Started) {
			t.Fatalf("scan %d: known %s, want the start", i, events[0].Known)
		}
		last = now
	}
}

func allEvents(*twinEvent) bool { return true }

// feedIDs lists the ids of a feed as the answer carries them.
func feedIDs(events []*twinEvent) string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.key()
	}
	return strings.Join(out, ",")
}

// TestTwinEventLedgerIdle: nobody polls for hours, then rows appear whose
// events began after the previous scan but before the floor of since (an FDD
// fault written at 11:55 with its episode start 10:00): they are news for a
// new client and for one back from sleep (since floored), and come once.
func TestTwinEventLedgerIdle(t *testing.T) {
	start := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	l := &twinEventLedger{}
	l.assign(start, nil) // the start-up scan, then no poll until 12:00
	now := start.Add(4 * time.Hour)
	scan := func() []twinEvent {
		return []twinEvent{
			{ID: "fault:1", Kind: "fault", Started: start.Add(2 * time.Hour)},
			{ID: "anomaly:1", Kind: "anomaly", Started: now.Add(-90 * time.Minute)},
			{ID: "alarm:1", Kind: "alarm", Started: now.Add(-30 * time.Minute)}, // history: older than an alarm's lag
			{ID: "alarm:2", Kind: "alarm", Started: now.Add(-time.Second)},
		}
	}
	first := scan()
	l.assign(now, first)
	if got := feedIDs(twinFeed(first, now.Add(-twinEventsWindow), 30, allEvents)); got != "alarm:2,anomaly:1,fault:1,alarm:1" {
		t.Errorf("after hours without a poll: %s", got)
	}
	next := scan()
	l.assign(now.Add(5*time.Second), next)
	if got := feedIDs(twinFeed(next, now, 30, allEvents)); got != "" {
		t.Errorf("the next poll: %s", got)
	}
	if got := feedIDs(twinFeed(next, now.Add(5*time.Second-twinEventsWindow), 30, allEvents)); got != "alarm:2,anomaly:1,fault:1,alarm:1" {
		t.Errorf("a new client: %s", got)
	}
}

// TestTwinEventLedgerEscalation: a row that turns CRITICAL in place (the
// anomaly worker escalates its row) is announced once more, under its own id
// with the event's start; a severity that falls and rises again, or a row
// first seen CRITICAL, is not.
func TestTwinEventLedgerEscalation(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	rows := func(anomaly, fault string) []twinEvent {
		return []twinEvent{
			{ID: "anomaly:1", Kind: "anomaly", Severity: anomaly, Started: base.Add(-time.Minute)},
			{ID: "fault:1", Kind: "fault", Severity: fault, Started: base.Add(-2 * time.Hour)},
			{ID: "alarm:1", Kind: "alarm", Severity: "CRITICAL", Started: base.Add(time.Second)},
		}
	}
	l := &twinEventLedger{}
	l.assign(base, nil)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }

	s1 := rows("WARNING", "CRITICAL")
	l.assign(at(5), s1)
	if got := feedIDs(twinFeed(s1, base, 30, allEvents)); got != "alarm:1,anomaly:1,fault:1" {
		t.Errorf("first seen: %s", got)
	}
	s2 := rows("CRITICAL", "WARNING")
	l.assign(at(10), s2)
	esc := twinFeed(s2, at(5), 30, allEvents)
	if got := feedIDs(esc); got != "anomaly:1:critical" {
		t.Fatalf("escalated: %s", got)
	}
	if ev := esc[0]; ev.Severity != "CRITICAL" || !ev.Known.Equal(at(10)) || !ev.Started.Equal(base.Add(-time.Minute)) {
		t.Errorf("escalation %+v", *ev)
	}
	s3 := rows("CRITICAL", "CRITICAL")
	l.assign(at(15), s3)
	if got := feedIDs(twinFeed(s3, at(10), 30, allEvents)); got != "" {
		t.Errorf("nothing twice: %s", got)
	}
	if got := feedIDs(twinFeed(s3, at(15).Add(-twinEventsWindow), 30, allEvents)); got != "anomaly:1:critical,alarm:1,fault:1" {
		t.Errorf("a new client: %s", got)
	}
	// A new ledger (a restart) takes what exists as it is.
	l = &twinEventLedger{}
	s4 := rows("CRITICAL", "CRITICAL")
	l.assign(at(20), s4)
	if got := feedIDs(twinFeed(s4, base.Add(-3*time.Hour), 30, allEvents)); got != "alarm:1,anomaly:1,fault:1" {
		t.Errorf("after a restart: %s", got)
	}
}

func TestTwinEventVisible(t *testing.T) {
	cases := []struct {
		ev               twinEvent
		insights, others bool
	}{
		{twinEvent{Kind: "alarm"}, true, true},
		{twinEvent{Kind: "trip"}, true, true},
		{twinEvent{Kind: "fault"}, true, false},
		{twinEvent{Kind: "anomaly"}, true, false},
		{twinEvent{Kind: "alarm", FDD: true}, false, true}, // the fault carries it
	}
	for _, tc := range cases {
		if got := twinEventVisible(&tc.ev, true); got != tc.insights {
			t.Errorf("%+v with aiinsights.read: %v", tc.ev, got)
		}
		if got := twinEventVisible(&tc.ev, false); got != tc.others {
			t.Errorf("%+v without aiinsights.read: %v", tc.ev, got)
		}
	}
}

// TestTwinEventLedgerContinuation: a row that continues an event after an
// OFFLINE stretch (backend restart, communication loss) takes over the known
// time and the start of the event, and the feed lists the event once.
func TestTwinEventLedgerContinuation(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	ev := func(id string, started time.Duration, prev string, prevStarted time.Duration) twinEvent {
		e := twinEvent{ID: id, Kind: strings.SplitN(id, ":", 2)[0], Started: base.Add(started)}
		if prev != "" {
			e.Prev, e.PrevStart = prev, base.Add(prevStarted)
		}
		return e
	}
	check := func(name string, e twinEvent, known, started time.Duration) {
		t.Helper()
		if !e.Known.Equal(base.Add(known)) || !e.Started.Equal(base.Add(started)) {
			t.Errorf("%s: known %s started %s, want %s / %s", name, e.Known.Sub(base), e.Started.Sub(base), known, started)
		}
	}

	// Start-up scan: an ongoing trip and an ongoing alarm, known since their start.
	l := &twinEventLedger{}
	l.assign(base, []twinEvent{ev("trip:1", -20*time.Minute, "", 0), ev("alarm:1", -8*time.Minute, "", 0)})
	// The engine's first cycles after the restart: the equipment is OFFLINE,
	// then both go on as new rows, beside a new alarm.
	scan := []twinEvent{ev("trip:1", -20*time.Minute, "", 0), ev("alarm:1", -8*time.Minute, "", 0),
		ev("trip:2", 30*time.Second, "trip:1", -20*time.Minute), ev("alarm:2", 30*time.Second, "alarm:1", -8*time.Minute),
		ev("alarm:3", 30*time.Second, "", 0)}
	l.assign(base.Add(40*time.Second), scan)
	check("continued trip", scan[2], -20*time.Minute, -20*time.Minute)
	check("continued alarm", scan[3], -8*time.Minute, -8*time.Minute)
	check("new alarm", scan[4], 40*time.Second, 30*time.Second)
	// A client that polled before the restart gets the new alarm only; a new
	// client gets each event once, as its latest row.
	if got := feedIDs(twinFeed(scan, base.Add(-time.Second), 30, allEvents)); got != "alarm:3" {
		t.Errorf("since before the restart: %s", got)
	}
	if got := feedIDs(twinFeed(scan, base.Add(-time.Hour), 30, allEvents)); got != "alarm:3,alarm:2,trip:2" {
		t.Errorf("a new client: %s", got)
	}
	// The next scan keeps it all, without the continued rows' ids (they left
	// the window) and with the continuations' ids only.
	next := []twinEvent{ev("trip:2", 30*time.Second, "trip:1", -20*time.Minute), ev("alarm:2", 30*time.Second, "alarm:1", -8*time.Minute)}
	l.assign(base.Add(45*time.Second), next)
	check("trip kept", next[0], -20*time.Minute, -20*time.Minute)
	if len(l.seen) != 2 {
		t.Errorf("the ledger holds %d rows, want the 2 of the latest scan", len(l.seen))
	}

	// A chain of three seen in one first scan (out of order): the first start.
	l = &twinEventLedger{}
	chain := []twinEvent{ev("trip:12", -5*time.Minute, "trip:11", -30*time.Minute), ev("trip:11", -30*time.Minute, "trip:10", -50*time.Minute),
		ev("trip:10", -50*time.Minute, "", 0)}
	l.assign(base, chain)
	check("chain of three", chain[0], -50*time.Minute, -50*time.Minute)
	if got := feedIDs(twinFeed(chain, base.Add(-time.Hour), 30, allEvents)); got != "trip:12" {
		t.Errorf("chain feed: %s", got)
	}

	// The continued row is not in the scan: as if it were seen now.
	l = &twinEventLedger{}
	l.assign(base, nil)
	lone := []twinEvent{ev("alarm:20", 2*time.Second, "alarm:19", -3*time.Hour), ev("trip:30", 4*time.Second, "trip:29", 2*time.Second),
		ev("trip:40", -time.Second, "trip:39", -7*time.Minute)}
	l.assign(base.Add(5*time.Second), lone)
	check("continues history", lone[0], -3*time.Hour, -3*time.Hour)
	check("continues a row never seen", lone[1], 5*time.Second, 2*time.Second)
	check("continues a row older than the last scan", lone[2], 5*time.Second, -7*time.Minute)
}

func TestTwinFeed(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	ev := func(id string, known, started time.Duration, loc int64, prev string) twinEvent {
		return twinEvent{ID: id, Kind: strings.SplitN(id, ":", 2)[0], Known: base.Add(known), Started: base.Add(started), LocationID: loc, Prev: prev}
	}
	events := func() []twinEvent {
		fdd := ev("alarm:4", -5*time.Minute, -5*time.Minute, 2, "") // the engine alarm of fault:1
		fdd.FDD = true
		return []twinEvent{
			ev("alarm:1", -10*time.Minute, -10*time.Minute, 1, ""),
			ev("fault:1", 0, -3*time.Hour, 2, ""),
			ev("alarm:2", 0, -time.Minute, 1, ""),
			ev("alarm:3", 0, -time.Minute, 2, ""),
			ev("trip:1", -20*time.Minute, -20*time.Minute, 1, ""),
			ev("trip:2", -20*time.Minute, -20*time.Minute, 1, "trip:1"),
			ev("anomaly:1", -2*time.Hour, -2*time.Hour, 1, ""),
			ev("anomaly:2", -30*time.Minute, -40*time.Minute, 1, ""),
			fdd,
		}
	}
	insights := func(ev *twinEvent) bool { return twinEventVisible(ev, true) }
	cases := []struct {
		name  string
		since time.Duration
		limit int
		allow func(*twinEvent) bool
		want  string
	}{
		{"newest known first, then the latest start, then the id", -time.Hour, 30, insights,
			"alarm:3,alarm:2,fault:1,alarm:1,trip:2,anomaly:2"},
		{"since", -10 * time.Minute, 30, insights, "alarm:3,alarm:2,fault:1"},
		{"limit", -time.Hour, 2, insights, "alarm:3,alarm:2"},
		{"location scope", -time.Hour, 30, func(ev *twinEvent) bool { return insights(ev) && ev.LocationID == 1 },
			"alarm:2,alarm:1,trip:2,anomaly:2"},
		{"without aiinsights.read: the FDD alarm, no fault or anomaly", -time.Hour, 30,
			func(ev *twinEvent) bool { return twinEventVisible(ev, false) }, "alarm:3,alarm:2,alarm:4,alarm:1,trip:2"},
	}
	for _, tc := range cases {
		if got := feedIDs(twinFeed(events(), base.Add(tc.since), tc.limit, tc.allow)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestTwinEventLedgerConcurrent scans from many requests at once while rows
// appear: scan times are unique and increase in scan order, and a client that
// polls every scan in turn receives every row exactly once.
func TestTwinEventLedgerConcurrent(t *testing.T) {
	l := &twinEventLedger{}
	var mu sync.Mutex
	var rows []twinEvent
	load := func(_ context.Context, now time.Time) ([]twinEvent, error) {
		mu.Lock()
		defer mu.Unlock()
		if n := len(rows); n%2 == 0 {
			rows = append(rows, twinEvent{ID: fmt.Sprintf("fault:%d", n), Kind: "fault", Started: now.Add(-time.Hour)}) // back-dated
		} else {
			rows = append(rows, twinEvent{ID: fmt.Sprintf("alarm:%d", n), Kind: "alarm", Started: now})
		}
		return append([]twinEvent(nil), rows...), nil
	}
	type answer struct {
		now    time.Time
		events []twinEvent
	}
	var amu sync.Mutex
	var answers []answer
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				now, events, err := l.scan(context.Background(), load)
				if err != nil {
					t.Error(err)
					return
				}
				amu.Lock()
				answers = append(answers, answer{now, events})
				amu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Slice(answers, func(i, j int) bool { return answers[i].now.Before(answers[j].now) })
	got := map[string]int{}
	var since time.Time
	for i, a := range answers {
		if i > 0 && !a.now.After(answers[i-1].now) {
			t.Fatalf("scan %d at %s, not after %s", i, a.now.Format(time.RFC3339Nano), answers[i-1].now.Format(time.RFC3339Nano))
		}
		if len(a.events) != i+1 {
			t.Fatalf("scan %d saw %d rows: scans overlapped", i, len(a.events))
		}
		for _, ev := range twinFeed(a.events, since, 1<<30, allEvents) {
			got[ev.ID]++
		}
		since = a.now
	}
	for _, r := range rows {
		if got[r.ID] != 1 {
			t.Errorf("%s received %d times", r.ID, got[r.ID])
		}
	}
	if len(l.seen) != len(rows) {
		t.Errorf("the ledger holds %d rows, the last scan %d", len(l.seen), len(rows))
	}
}

// TestTwinEventLedgerDelivery simulates clients that poll with since = the now
// of their previous answer while rows appear late (back-dated starts, commits
// after the scan time), events go on as continuation rows, anomalies and
// faults turn CRITICAL in place, and the backend restarts: every event and
// every escalation that is news reaches every client exactly once, and
// history never does. Across the restart a client may miss only a back-dated
// row, or an escalation, that appeared after its last poll and before the
// restart.
func TestTwinEventLedgerDelivery(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	base := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	const snapshotDelay = 2 * time.Millisecond // a scan's queries see rows committed shortly after its now
	type row struct {
		ev       twinEvent
		appear   time.Time
		history  bool
		escalate time.Time // when it turns CRITICAL in place (zero: never)
	}
	kinds := []string{"alarm", "trip", "anomaly", "fault"}
	var rows []row
	for i := 0; i < 600; i++ {
		kind := kinds[rng.IntN(len(kinds))]
		appear := base.Add(time.Duration(rng.Int64N(int64(80 * time.Minute))))
		var delay time.Duration
		switch kind {
		case "alarm", "trip":
			delay = time.Duration(rng.Int64N(int64(3 * time.Second))) // stamped at the cycle start
		default:
			delay = time.Duration(rng.Int64N(int64(twinEventLag[kind] - time.Minute)))
		}
		r := row{appear: appear}
		if rng.IntN(20) == 0 { // a regenerated history row
			delay, r.history = twinEventLag[kind]+time.Hour, true
		}
		r.ev = twinEvent{ID: fmt.Sprintf("%s:%d", kind, i), Kind: kind, Started: appear.Add(-delay)}
		rows = append(rows, r)
	}
	// An alarm or trip still going when its equipment drops out goes on as a
	// new row that continues it.
	root := map[string]string{}
	for i, n := 0, len(rows); i < n; i++ {
		r := rows[i]
		if r.history || (r.ev.Kind != "alarm" && r.ev.Kind != "trip") || rng.IntN(4) != 0 {
			continue
		}
		appear := r.appear.Add(10*time.Second + time.Duration(rng.Int64N(int64(30*time.Minute))))
		id := fmt.Sprintf("%s:c%d", r.ev.Kind, i)
		root[id] = r.ev.ID
		rows = append(rows, row{appear: appear, ev: twinEvent{ID: id, Kind: r.ev.Kind,
			Started: appear.Add(-time.Duration(rng.Int64N(int64(3 * time.Second)))), Prev: r.ev.ID, PrevStart: r.ev.Started}})
	}
	// An anomaly or fault turns CRITICAL a while after it appeared.
	for i := range rows {
		r := &rows[i]
		if r.history || (r.ev.Kind != "anomaly" && r.ev.Kind != "fault") || rng.IntN(3) != 0 {
			continue
		}
		r.ev.Severity = "WARNING"
		r.escalate = r.appear.Add(30*time.Second + time.Duration(rng.Int64N(int64(20*time.Minute))))
	}
	chainOf := func(id string) string {
		if r, ok := root[id]; ok {
			return r
		}
		return id
	}
	visibleAt := func(now time.Time) []twinEvent {
		var out []twinEvent
		for _, r := range rows {
			if !r.appear.After(now.Add(snapshotDelay)) {
				ev := r.ev
				if !r.escalate.IsZero() && !r.escalate.After(now.Add(snapshotDelay)) {
					ev.Severity = "CRITICAL"
				}
				out = append(out, ev)
			}
		}
		return out
	}

	type client struct {
		every      int
		first      time.Time // scan time of its first poll
		last       time.Time // scan time of its latest poll
		since      time.Time
		atRestart  time.Time // its since when the backend restarted
		got        map[string]int
		historyGot []string
	}
	clients := []*client{{every: 1}, {every: 2}, {every: 7}}
	for _, c := range clients {
		c.got = map[string]int{}
	}
	l := &twinEventLedger{}
	restartAt := base.Add(45 * time.Minute)
	var restarted time.Time // the start-up scan
	now := base.Add(10 * time.Minute)
	for j := 0; now.Before(base.Add(90 * time.Minute)); j++ {
		if restarted.IsZero() && now.After(restartAt) {
			// A backend restart: a new ledger and its start-up scan, which no
			// client sees; the clients poll on with their since.
			restarted = now
			for _, c := range clients {
				c.atRestart = c.since
			}
			l = &twinEventLedger{}
			l.assign(now, visibleAt(now))
			now = now.Add(time.Duration(1000+rng.IntN(20000)) * time.Millisecond)
			continue
		}
		visible := visibleAt(now)
		l.assign(now, visible)
		for _, c := range clients {
			if j%c.every != 0 {
				continue
			}
			since := c.since
			if c.first.IsZero() {
				c.first, since = now, now.Add(-twinEventsWindow)
			}
			for _, ev := range twinFeed(visible, since, 1<<30, allEvents) {
				c.got[chainOf(ev.key())]++
			}
			c.since, c.last = now, now
		}
		now = now.Add(time.Duration(800+rng.IntN(5000)) * time.Millisecond)
	}
	if restarted.IsZero() {
		t.Fatal("no restart")
	}

	escalations := 0 // escalations received
	for ci, c := range clients {
		for _, r := range rows {
			if r.ev.Prev != "" {
				continue // counted with the event it continues
			}
			n := c.got[r.ev.ID]
			lost := r.appear.After(c.atRestart) && !r.appear.After(restarted.Add(snapshotDelay)) && !r.ev.Started.After(c.atRestart)
			news := !r.history && !lost && r.appear.After(c.first.Add(snapshotDelay)) && !r.appear.After(c.last)
			switch {
			case n > 1:
				t.Errorf("client %d received %s %d times", ci, r.ev.ID, n)
			case r.history && r.appear.After(c.first) && n > 0:
				t.Errorf("client %d received the history row %s", ci, r.ev.ID)
			case news && r.escalate.IsZero() && n != 1:
				t.Errorf("client %d missed %s (appeared %s, started %s)", ci, r.ev.ID, r.appear.Format(time.RFC3339Nano),
					r.ev.Started.Format(time.RFC3339Nano))
			}
			if r.escalate.IsZero() {
				continue
			}
			// A client that polls only after the escalation gets the event
			// once, as its escalation.
			key := r.ev.ID + ":critical"
			ne := c.got[key]
			escalations += ne
			lostEsc := r.escalate.After(c.atRestart) && !r.escalate.After(restarted.Add(snapshotDelay))
			switch {
			case ne > 1:
				t.Errorf("client %d received %s %d times", ci, key, ne)
			case !lostEsc && r.escalate.After(c.first.Add(snapshotDelay)) && !r.escalate.After(c.last) && ne != 1:
				t.Errorf("client %d missed %s (appeared %s, escalated %s)", ci, key, r.appear.Format(time.RFC3339Nano),
					r.escalate.Format(time.RFC3339Nano))
			case news && n+ne == 0:
				t.Errorf("client %d missed %s and its escalation", ci, r.ev.ID)
			}
		}
	}
	if escalations == 0 {
		t.Error("no escalation reached a client")
	}
}

func TestTwinLiveEventsAPI(t *testing.T) {
	e := setupAPIEnv(t)

	aliases := []string{"CT_01", "CT_02", "CH_02", "MAU_01"}
	ids := map[string]int64{}
	for _, a := range aliases {
		ids[a] = e.scalar(`SELECT id FROM asset.equipment WHERE alias = $1`, a).(int64)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	// A clean slate: no events of the last hours, and the test's equipment
	// RUNNING for the last 9 hours.
	e.exec(`DELETE FROM ts.alarm_events WHERE start_ts > now() - interval '9 hours'`)
	e.exec(`DELETE FROM ts.asset_status WHERE status_id = 3 AND start_ts > now() - interval '9 hours'`)
	e.exec(`DELETE FROM analytics.faults WHERE status = 'ACTIVE' OR started_at > now() - interval '9 hours'`)
	e.exec(`DELETE FROM analytics.anomalies`)
	for _, a := range aliases {
		e.exec(`DELETE FROM ts.asset_status WHERE asset_id = $1 AND start_ts >= $2`, ids[a], at(-9*time.Hour))
		e.exec(`UPDATE ts.asset_status SET end_ts = $2 WHERE asset_id = $1 AND end_ts IS NULL`, ids[a], at(-9*time.Hour))
		e.exec(`INSERT INTO ts.asset_status (asset_id, status_id, start_ts) VALUES ($1, 1, $2)`, ids[a], at(-9*time.Hour))
	}
	// The CUB technicians first stand for a group without digitaltwin.read,
	// then (setupAPIEnv took aiinsights.read from them) for one without
	// aiinsights.read.
	grantTwinRead := `INSERT INTO core.user_group_permissions (user_group_id, permission_id, value)
		SELECT 2, p.id, TRUE FROM core.permissions p JOIN core.permission_groups g ON g.id = p.permission_group_id
		WHERE g.alias = 'digitaltwin' AND p.alias = 'read' ON CONFLICT DO NOTHING`
	e.exec(`DELETE FROM core.user_group_permissions gp USING core.permissions p, core.permission_groups g
		WHERE gp.user_group_id = 2 AND gp.permission_id = p.id AND p.permission_group_id = g.id AND g.alias = 'digitaltwin' AND p.alias = 'read'`)
	t.Cleanup(func() { _, _ = e.db.Exec(context.Background(), grantTwinRead) })

	// addAlarm writes an alarm row; a zero end leaves it open.
	addAlarm := func(asset, key, name, severity, msg string, start, end time.Time) string {
		var endTS any
		if !end.IsZero() {
			endTS = end
		}
		id := e.scalar(`INSERT INTO ts.alarm_events (asset_id, alarm_type, source_key, name, severity, message, start_ts, end_ts)
			VALUES ($1, 'ALARM', $2, $3, $4, $5, $6, $7) RETURNING id`, ids[asset], key, name, severity, msg, start, endTS).(int64)
		return fmt.Sprintf("alarm:%d", id)
	}
	endAlarm := func(id string, end time.Time) {
		var row int64
		if _, err := fmt.Sscanf(id, "alarm:%d", &row); err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE ts.alarm_events SET end_ts = $2 WHERE id = $1`, row, end)
	}
	// setStatus ends the equipment's open status interval and opens a new one,
	// like the engine.
	setStatus := func(asset string, status int, from time.Time) string {
		e.exec(`UPDATE ts.asset_status SET end_ts = $2 WHERE asset_id = $1 AND end_ts IS NULL`, ids[asset], from)
		return fmt.Sprintf("trip:%d", e.scalar(`INSERT INTO ts.asset_status (asset_id, status_id, start_ts) VALUES ($1, $2, $3) RETURNING id`,
			ids[asset], status, from).(int64))
	}
	addFault := func(asset, code string, start time.Time) string {
		id := e.scalar(`INSERT INTO analytics.faults (rule_code, asset_id, severity, message, started_at, last_seen_at)
			VALUES ($1, $2, 'WARNING', $3, $4, now()) RETURNING id`, code, ids[asset], "Test fault "+code, start).(int64)
		return fmt.Sprintf("fault:%d", id)
	}
	paramName := map[string]string{}
	anomalyRow := map[string]int64{}
	addAnomaly := func(asset, param, severity string, start time.Time) string {
		var epid int64
		var name string
		if err := e.db.QueryRow(e.ctx, `SELECT ep.id, mp.name FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			WHERE ep.equipment_id = $1 AND mp.alias = $2`, ids[asset], param).Scan(&epid, &name); err != nil {
			t.Fatal(err)
		}
		id := e.scalar(`INSERT INTO analytics.anomalies (asset_id, equipment_parameter_id, param_alias, detector, severity, message, started_at, last_seen_at)
			VALUES ($1, $2, $3, 'zscore', $4, 'Test anomaly', $5, now()) RETURNING id`, ids[asset], epid, param, severity, start).(int64)
		key := fmt.Sprintf("anomaly:%d", id)
		paramName[key], anomalyRow[key] = name, id
		return key
	}

	type event struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
		Asset    struct {
			ID    int64  `json:"id"`
			Alias string `json:"alias"`
			Name  string `json:"name"`
		} `json:"asset"`
		Title     string  `json:"title"`
		Detail    *string `json:"detail"`
		StartedAt string  `json:"started_at"`
		Active    bool    `json:"active"`
	}
	type feed struct {
		Now    string  `json:"now"`
		Events []event `json:"events"`
	}
	poll := func(user string, body map[string]any) (feed, time.Time) {
		t.Helper()
		f := decode[feed](t, e.ok(user, "digital_twin/getliveevents", body))
		now, err := time.Parse(time.RFC3339Nano, f.Now)
		if err != nil || !strings.HasSuffix(f.Now, "Z") || len(f.Now) != len("2006-01-02T15:04:05.000Z") {
			t.Fatalf("now %q: %v", f.Now, err)
		}
		return f, now
	}
	// order lists the events of the test's equipment (other tests may write
	// events of other equipment into the same database).
	order := func(f feed) string {
		var out []string
		for _, ev := range f.Events {
			if _, ok := ids[ev.Asset.Alias]; ok {
				out = append(out, ev.ID)
			}
		}
		return strings.Join(out, ",")
	}
	list := func(ids ...string) string { return strings.Join(ids, ",") }
	byID := func(f feed, id string) event {
		for _, ev := range f.Events {
			if ev.ID == id {
				return ev
			}
		}
		t.Fatalf("%s missing from %s", id, order(f))
		return event{}
	}
	ts := func(tm time.Time) string { return tm.UTC().Format("2006-01-02T15:04:05.000Z") }
	// restart stands for a backend restart: a new server, with a new ledger
	// and its start-up scan, on the same database.
	restart := func() {
		srv := New(e.srv.Deps)
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		e.srv, e.http = srv, hs
	}

	e.expect(e.do("ahmad_faizal", "digital_twin/getliveevents", nil, false, nil), 403, "permission")
	e.exec(grantTwinRead)
	e.srv.Auth.InvalidateAll()

	// Before the restart: events of the last minutes, an ongoing MAU_01 trip
	// with two alarms, an FDD fault and its engine alarm.
	e1 := addAlarm("CT_01", "test:events:ct-004", "CT-004", "WARNING", "Fan motor current high", at(-5*time.Minute), time.Time{})
	e2 := addAlarm("MAU_01", "test:events:ahu-003", "AHU-003", "WARNING", "", at(-3*time.Minute), at(-2*time.Minute))
	addAlarm("CT_01", "test:events:ct-001", "CT-001", "WARNING", "Older than the window", at(-90*time.Minute), time.Time{})
	e4 := setStatus("CH_02", 3, at(-2*time.Minute))
	m1 := setStatus("MAU_01", 3, at(-20*time.Minute))
	ma1 := addAlarm("MAU_01", "test:events:mau-a", "AHU-001", "WARNING", "Supply air temperature high", at(-8*time.Minute), time.Time{})
	mb1 := addAlarm("MAU_01", "test:events:mau-b", "AHU-002", "WARNING", "Filter dP high", at(-8*time.Minute), time.Time{})
	f0 := addFault("CT_01", "CT-014", at(-3*time.Hour))
	ext := addAlarm("CT_01", "ext:fdd:CT-014", "CT-014", "WARNING", "CT-014", at(-time.Minute), time.Time{})

	// The server of the test started before these rows were written, so it
	// sees all of them late (the old alarm and the trip as history); an FDD
	// fault never comes twice (its ext:fdd alarm is left out).
	p0, now0 := poll("sarah_lim", nil)
	got := strings.Split(order(p0), ",")
	sort.Strings(got)
	want := []string{e1, e2, e4, m1, ma1, mb1, f0}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("before the restart %s, want %s (no %s)", order(p0), list(want...), ext)
	}

	// After a restart nothing that existed before is announced again ...
	restart()
	if f, _ := poll("sarah_lim", map[string]any{"since": p0.Now}); order(f) != "" {
		t.Fatalf("since the last poll before the restart: %s", order(f))
	}
	// ... and a new client gets the last hour, newest first.
	f1, now1 := poll("sarah_lim", nil)
	if !now1.After(now0) {
		t.Fatalf("now %s not after %s", f1.Now, p0.Now)
	}
	if got, want := order(f1), list(e4, e2, e1, mb1, ma1, m1); got != want {
		t.Fatalf("first poll after the restart %s, want %s", got, want)
	}
	if d := time.Since(now1); d < 0 || d > time.Minute {
		t.Fatalf("now %s is not the server time", f1.Now)
	}
	a1 := byID(f1, e1)
	if a1.Kind != "alarm" || a1.Severity != "WARNING" || a1.Asset.ID != ids["CT_01"] || a1.Asset.Alias != "CT_01" || a1.Asset.Name == "" ||
		a1.Title != "CT-004" || a1.Detail == nil || *a1.Detail != "Fan motor current high" || !a1.Active || a1.StartedAt != ts(at(-5*time.Minute)) {
		t.Fatalf("alarm %+v", a1)
	}
	if a2 := byID(f1, e2); a2.Detail != nil || a2.Active || a2.Asset.Alias != "MAU_01" {
		t.Fatalf("ended alarm %+v", a2)
	}
	if tr := byID(f1, e4); tr.Kind != "trip" || tr.Severity != "CRITICAL" || tr.Title != "Trip / breakdown" || tr.Detail != nil || !tr.Active ||
		tr.Asset.Alias != "CH_02" {
		t.Fatalf("trip %+v", tr)
	}

	// The engine after the restart: MAU_01 is OFFLINE until its data returns,
	// which closes its trip and alarms; then the trip and one alarm go on, the
	// other alarm comes back CRITICAL and a third one is raised. CT_01's alarm
	// clears and comes back while CT_01 runs: news.
	r1 := time.Now().UTC().Truncate(time.Millisecond)
	r2 := r1.Add(20 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	setStatus("MAU_01", 4, r1)
	endAlarm(ma1, r1)
	endAlarm(mb1, r1)
	endAlarm(e1, r1)
	m2 := setStatus("MAU_01", 3, r2)
	ma2 := addAlarm("MAU_01", "test:events:mau-a", "AHU-001", "WARNING", "Supply air temperature high", r2, time.Time{})
	mb2 := addAlarm("MAU_01", "test:events:mau-b", "AHU-002", "CRITICAL", "Filter dP very high", r2, time.Time{})
	mc := addAlarm("MAU_01", "test:events:mau-c", "AHU-004", "WARNING", "Fan current high", r2, time.Time{})
	e1b := addAlarm("CT_01", "test:events:ct-004", "CT-004", "WARNING", "Fan motor current high", r2, time.Time{})

	f2, now2 := poll("sarah_lim", map[string]any{"since": f1.Now})
	if got, want := order(f2), list(e1b, mc, mb2); got != want {
		t.Fatalf("after the split %s, want %s (not %s, %s)", got, want, m2, ma2)
	}
	if fresh, _ := poll("sarah_lim", nil); order(fresh) != list(e1b, mc, mb2, e4, e2, e1, ma2, mb1, m2) {
		t.Fatalf("a new client after the split: %s", order(fresh))
	} else if tr := byID(fresh, m2); !tr.Active || tr.StartedAt != ts(at(-20*time.Minute)) {
		t.Fatalf("continued trip %+v", tr)
	} else if a := byID(fresh, ma2); !a.Active || a.StartedAt != ts(at(-8*time.Minute)) {
		t.Fatalf("continued alarm %+v", a)
	}

	// Rows that appear later, most of them back-dated: an FDD fault (episode
	// start hours back), anomalies (first sample minutes back), an alarm older
	// than an alarm's lag (history) and one committed after the scan.
	e6 := addFault("CT_02", "CT-013", at(-3*time.Hour))
	e7 := addAnomaly("CT_01", "fan_current", "CRITICAL", at(-4*time.Minute))
	e8 := addAlarm("CT_01", "test:events:ct-002", "CT-002", "WARNING", "History", at(-21*time.Minute), time.Time{})
	e9 := addAlarm("CT_02", "test:events:ct-003", "CT-003", "WARNING", "Committed after the scan", now2.Add(-2*time.Second), time.Time{})
	e10 := addAnomaly("MAU_01", "sat", "WARNING", at(-2*time.Minute))

	f3, now3 := poll("sarah_lim", map[string]any{"since": f2.Now})
	if !now3.After(now2) {
		t.Fatalf("now %s not after %s", f3.Now, f2.Now)
	}
	if got, want := order(f3), list(e9, e10, e7, e6); got != want {
		t.Fatalf("back-dated rows %s, want %s", got, want)
	}
	if f := byID(f3, e6); f.Kind != "fault" || f.Title != "Tower Leaving Water Temperature Sensor Bias (CT-013)" || f.Detail == nil ||
		*f.Detail != "Test fault CT-013" || !f.Active || f.StartedAt != ts(at(-3*time.Hour)) {
		t.Fatalf("fault %+v", f)
	}
	if a := byID(f3, e7); a.Kind != "anomaly" || a.Severity != "CRITICAL" || a.Title != paramName[e7]+" anomaly" || !a.Active {
		t.Fatalf("anomaly %+v", a)
	}

	// Nothing twice.
	if f4, _ := poll("sarah_lim", map[string]any{"since": f3.Now}); order(f4) != "" {
		t.Fatalf("fourth poll %s", order(f4))
	}
	e.exec(`UPDATE analytics.faults SET status = 'CLEARED', ended_at = now() WHERE rule_code = 'CT-013' AND asset_id = $1`, ids["CT_02"])
	f5, _ := poll("sarah_lim", map[string]any{"since": f3.Now})
	if order(f5) != "" {
		t.Fatalf("a cleared fault is not a new event: %s", order(f5))
	}

	// The anomaly worker escalates its row in place: the escalation is news,
	// once, as a new event with the start of the anomaly.
	e.exec(`UPDATE analytics.anomalies SET severity = 'CRITICAL' WHERE id = $1`, anomalyRow[e10])
	e10c := e10 + ":critical"
	f6, _ := poll("sarah_lim", map[string]any{"since": f5.Now})
	if order(f6) != e10c {
		t.Fatalf("escalated anomaly %s, want %s", order(f6), e10c)
	}
	if a := byID(f6, e10c); a.Kind != "anomaly" || a.Severity != "CRITICAL" || a.StartedAt != ts(at(-2*time.Minute)) || !a.Active {
		t.Fatalf("escalation %+v", a)
	}
	if f, _ := poll("sarah_lim", map[string]any{"since": f6.Now}); order(f) != "" {
		t.Fatalf("after the escalation: %s", order(f))
	}

	// Location scope: the FAB1 technician sees the MAU events only.
	if fab, _ := poll("hafiz_rahman", nil); order(fab) != list(e10c, mc, mb2, e2, ma2, mb1, m2) {
		t.Fatalf("FAB1 feed %s", order(fab))
	}
	// Without aiinsights.read: no FDD faults or anomalies, the FDD fault's
	// engine alarm instead (CUB scope).
	cub, _ := poll("ahmad_faizal", nil)
	if order(cub) != list(e9, e1b, ext, e4, e1, e8) {
		t.Fatalf("feed without aiinsights.read %s, want %s", order(cub), list(e9, e1b, ext, e4, e1, e8))
	}
	for _, ev := range cub.Events {
		if ev.Kind == "fault" || ev.Kind == "anomaly" {
			t.Fatalf("feed without aiinsights.read has %s", ev.ID)
		}
	}
	if a := byID(cub, ext); a.Kind != "alarm" || a.Title != "CT-014" || !a.Active || a.Asset.Alias != "CT_01" {
		t.Fatalf("FDD alarm %+v", a)
	}
	// limit; since before the floor; an invalid since means the default.
	if top, _ := poll("sarah_lim", map[string]any{"limit": 2}); order(top) != list(e10c, e9) {
		t.Fatalf("limit 2: %s", order(top))
	}
	all := list(e10c, e9, e7, e6, e1b, mc, mb2, e4, e2, e1, ma2, mb1, m2, e8)
	for _, since := range []any{time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), "not a time"} {
		if f, _ := poll("sarah_lim", map[string]any{"since": since, "limit": 500}); order(f) != all {
			t.Fatalf("since %v: %s, want %s", since, order(f), all)
		}
	}
}
