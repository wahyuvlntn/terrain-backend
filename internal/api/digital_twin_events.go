package api

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"afdd/internal/engine"
	"afdd/internal/httpx"
)

// Live event feed of the digital twin's walk-mode HUD: alarms, FDD faults,
// sensor anomalies and trips (UNPLANNED DOWN status intervals) of the
// equipment the user can see, newest first. The HUD polls every few seconds
// with since = the `now` of its previous answer.
//
// Not every event row appears when its event starts. FDD faults are written
// with the start of the detected episode, up to hours back (the rules need up
// to 2 h of persistence in a 6 h window); anomalies with their first sample,
// minutes back; engine alarms and status changes with the start of the
// evaluation cycle, shortly before they commit. A filter on the start time
// alone would never deliver such a row to a polling client, so `since` is
// compared with the time the event became known (twinEventLedger); the
// answer's started_at stays the start of the event.
//
// FDD faults and anomalies come from the AI service: like AI Insights, they
// need aiinsights.read. An FDD fault is also raised as an engine alarm (source
// key ext:fdd:<code>, in every user's alarm list); the feed carries the fault
// to users who may read AI insights and the alarm to the others, never both.
// An anomaly or fault row whose severity rises to CRITICAL in place is
// announced once more, as a new event (twinEvent.key).
//
// While equipment is OFFLINE (a backend restart: the engine's first cycles
// run before the gateways report again, or a communication loss) the engine
// closes its alarms and its trip interval, and opens new rows when the data
// returns. Such a row continues the earlier event (twinEvent.Prev): it is not
// news, the feed lists the event once, as its latest row with the start of
// the first one.

const (
	// twinEventsWindow is the default and the floor of since.
	twinEventsWindow = 60 * time.Minute
	// twinEventsScanCap bounds the rows read per source and scan.
	twinEventsScanCap = 2000
	// twinEventsStartupScan bounds the scan at process start.
	twinEventsStartupScan = 15 * time.Second
)

// twinEventLag is, per kind, the longest delay between the start of an event
// and the moment its row appears. Each scan reads the rows that started
// within twinEventsWindow + lag; a row that is older than its lag when first
// seen is history (a regenerated demo history), not news.
var twinEventLag = map[string]time.Duration{
	"alarm":   10 * time.Minute,
	"trip":    10 * time.Minute,
	"anomaly": 2 * time.Hour,
	"fault":   7 * time.Hour,
}

// twinEvent is one event of the feed.
type twinEvent struct {
	ID         string // "<kind>:<row id>"
	Kind       string
	Severity   string
	AssetID    int64
	Alias      string
	Name       string
	LocationID int64
	Title      string
	Detail     *string
	Started    time.Time
	Active     bool
	// Prev is the row this one continues after an OFFLINE stretch ("" for a
	// new event), PrevStart its start.
	Prev      string
	PrevStart time.Time
	// FDD marks the engine alarm of an FDD fault (source key ext:fdd:<code>).
	FDD bool
	// Known is when the server first knew the event (twinEventLedger), or its
	// escalation when Escalated is set.
	Known     time.Time
	Escalated bool
}

// key is the event's id in the feed: the row's, or for a row that turned
// CRITICAL in place the row's with ":critical", so that the HUD, which keeps
// the events it has by id, takes the escalation as a new event (an engine
// alarm escalates as a new row).
func (e *twinEvent) key() string {
	if e.Escalated {
		return e.ID + ":critical"
	}
	return e.ID
}

func (e *twinEvent) json() map[string]any {
	return map[string]any{
		"id": e.key(), "kind": e.Kind, "severity": e.Severity,
		"asset": map[string]any{"id": e.AssetID, "alias": e.Alias, "name": e.Name},
		"title": e.Title, "detail": e.Detail, "started_at": httpx.TS(e.Started), "active": e.Active,
	}
}

// twinCritical reports a CRITICAL severity (anything else shows as a warning).
func twinCritical(severity string) bool {
	return strings.EqualFold(strings.TrimSpace(severity), "CRITICAL")
}

// twinSeen is what the ledger keeps of an event row: when it became known (or
// escalated), the start of its event (the first row's, for a continuation),
// the highest severity announced and whether that was an escalation.
type twinSeen struct {
	known, started time.Time
	severity       string
	escalated      bool
}

// twinEventLedger remembers when the server first saw each event row. Scans
// are serialised and their times (`now`) strictly increase, and a row first
// seen (or escalated) by a scan gets a known time after the previous scan and
// not after its own, history aside: a client whose last answer came from an
// earlier scan receives the row on its next poll, and a client that received
// it does not get it again.
//
// The ledger lives in memory and scans only when asked (polls, start-up). The
// server scans once at start-up (registerDigitalTwinEvents), so what exists
// then is known since its start and never announced again, while a row that
// appears later, back-dated or not, is announced once, however long nobody
// polled before (twinKnownAt). A back-dated detection written while the
// backend was down is not announced. A row that turns CRITICAL in place (the
// anomaly worker escalates its row; a fault takes its rule's severity) is
// announced once more, known from the scan that saw the escalation. The
// ledger holds the rows of the latest scan only: a row leaves it when it
// leaves the scan window (60 min + its kind's lag), by then its known time
// lies before the floor of since (an escalation less than an hour before
// then leaves new clients' lists early).
type twinEventLedger struct {
	mu   sync.Mutex
	last time.Time           // now of the previous scan (zero before the first)
	seen map[string]twinSeen // event id -> known time, event start, severity
}

// scan reads the events at a new `now` and assigns their known times.
func (l *twinEventLedger) scan(ctx context.Context, load func(context.Context, time.Time) ([]twinEvent, error)) (time.Time, []twinEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Millisecond) // the answer carries milliseconds
	if !now.After(l.last) {
		now = l.last.Add(time.Millisecond)
	}
	events, err := load(ctx, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	l.assign(now, events)
	return now, events, nil
}

// assign sets the known time and the event start of every row of the scan at
// now. Rows are taken oldest first, so a continued row is settled before its
// continuation. A row that turned CRITICAL since it was announced becomes an
// escalation, known from now. Only the rows of this scan are kept: the scan
// window slides forward, so a row that left it does not come back.
func (l *twinEventLedger) assign(now time.Time, events []twinEvent) {
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return events[order[i]].Started.Before(events[order[j]].Started) })
	seen := make(map[string]twinSeen, len(events))
	for _, i := range order {
		ev := &events[i]
		s, ok := l.seen[ev.ID]
		if !ok {
			s = l.firstSeen(ev, seen, now)
		}
		if twinCritical(ev.Severity) && !twinCritical(s.severity) {
			s.known, s.severity, s.escalated = now, ev.Severity, true
		}
		ev.Known, ev.Started, ev.Escalated = s.known, s.started, s.escalated
		seen[ev.ID] = s
	}
	l.seen = seen
	l.last = now
}

// firstSeen settles a row first seen by the scan at now (cur: the rows of this
// scan settled so far). A continuation takes over its event: the known time,
// start and severity of the row it continues, from this scan or the previous
// one, or, for a row older than the scan window, as if that row were seen now.
func (l *twinEventLedger) firstSeen(ev *twinEvent, cur map[string]twinSeen, now time.Time) twinSeen {
	startup := l.last.IsZero()
	if ev.Prev == "" {
		return twinSeen{known: twinKnownAt(ev.Kind, ev.Started, now, startup), started: ev.Started, severity: ev.Severity}
	}
	if s, ok := cur[ev.Prev]; ok {
		return s
	}
	if s, ok := l.seen[ev.Prev]; ok {
		return s
	}
	return twinSeen{known: twinKnownAt(ev.Kind, ev.PrevStart, now, startup), started: ev.PrevStart, severity: ev.Severity}
}

// twinKnownAt is the known time of an event first seen by the scan at now. At
// the start-up scan (the ledger's first) what exists is known since its start
// (at most now: a row stamped just after now can commit before the query).
// Later, a row that is history (older than its kind's lag, so it was never
// news) is known since its start as well, and any other row from now on. So
// the known time does not depend on when the previous scan ran: the ledger
// scans only when polled, and a fault back-dated past the floor of since and
// first seen after an hour without polls is still news.
func twinKnownAt(kind string, started, now time.Time, startup bool) time.Time {
	if startup || now.Sub(started) > twinEventLag[kind] {
		if started.After(now) {
			return now
		}
		return started
	}
	return now
}

func (s *Server) registerDigitalTwinEvents() {
	ledger := &twinEventLedger{}
	s.handle("digital_twin/getliveevents", AccessUser, func(c *Ctx) (any, error) { return s.twinLiveEvents(c, ledger) })
	if s.DB == nil {
		return
	}
	// The start-up scan: the events that exist now are known since their
	// start, so a restart announces none of them again. If it fails, the first
	// request's scan takes its place.
	ctx, cancel := context.WithTimeout(context.Background(), twinEventsStartupScan)
	defer cancel()
	if _, _, err := ledger.scan(ctx, s.loadTwinEvents); err != nil {
		slog.Warn("digital twin live events: start-up scan", "err", err)
	}
}

// twinLiveEvents returns the events of the visible equipment that became known
// after since: {now, events[{id, kind, severity, asset{id, alias, name}, title,
// detail, started_at, active}]}, newest first. Input: since (ISO-8601, default
// and floor now - 60 min), limit (1..100, 30).
func (s *Server) twinLiveEvents(c *Ctx, ledger *twinEventLedger) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	insights := c.User.Can("aiinsights", "read")
	limit := int(c.limit(30, 100))
	now, events, err := ledger.scan(c, s.loadTwinEvents)
	if err != nil {
		return nil, err
	}
	since := now.Add(-twinEventsWindow)
	if t := c.timeParam("since"); t != nil && t.After(since) {
		since = *t
	}
	allow := func(ev *twinEvent) bool { return twinEventVisible(ev, insights) && scope.AllowsLocation(ev.LocationID) }
	out := []map[string]any{}
	for _, ev := range twinFeed(events, since, limit, allow) {
		out = append(out, ev.json())
	}
	return map[string]any{"now": httpx.TS(now), "events": out}, nil
}

// twinEventVisible reports whether a user with or without aiinsights.read
// gets an event (location scope aside): FDD faults and anomalies need it, as
// in AI Insights; the engine alarm of an FDD fault goes to the users without
// it, in place of the fault.
func twinEventVisible(ev *twinEvent, insights bool) bool {
	switch {
	case ev.Kind == "fault" || ev.Kind == "anomaly":
		return insights
	case ev.FDD:
		return !insights
	}
	return true
}

// twinFeed picks the events of a scan for one answer: those known after since
// that the user may see (allow), without the rows another row of the scan
// continues, newest known first, at most limit. It sorts events in place.
func twinFeed(events []twinEvent, since time.Time, limit int, allow func(*twinEvent) bool) []*twinEvent {
	continued := map[string]bool{}
	for i := range events {
		if events[i].Prev != "" {
			continued[events[i].Prev] = true
		}
	}
	sort.Slice(events, func(i, j int) bool {
		a, b := &events[i], &events[j]
		if !a.Known.Equal(b.Known) {
			return a.Known.After(b.Known)
		}
		if !a.Started.Equal(b.Started) {
			return a.Started.After(b.Started)
		}
		return a.ID > b.ID
	})
	out := []*twinEvent{}
	for i := range events {
		ev := &events[i]
		if continued[ev.ID] || !ev.Known.After(since) || !allow(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) == limit {
			break
		}
	}
	return out
}

// twinEventSources reads each kind of event for one scan. Every query returns
// row id, equipment id, alias, name, location, title, severity, detail, start,
// active, the row it continues (id and start, or NULL) and whether it is the
// engine alarm of an FDD fault, and reads an index on the start time ($1 =
// earliest start, $2 = row cap; trips: per equipment through
// asset_status_asset_idx). args are $3...
//
// Continuations: a trip row continues the trip before it when the equipment
// was OFFLINE in between (the latest earlier status that is not OFFLINE is
// UNPLANNED DOWN). An alarm row continues an earlier row of the same alarm
// (equipment, source key, severity) when the equipment came back from OFFLINE
// at most 2 minutes before it began (one of its two previous status rows) and
// the earlier row lasted until the equipment dropped out (no other status in
// between: after a long outage the engine closes everything at its last
// heartbeat).
var twinEventSources = []struct {
	kind  string
	query string
	args  []any
}{
	{"alarm", `SELECT a.id, e.id, e.alias, e.name, e.location_id, a.name, a.severity, NULLIF(a.message, ''), a.start_ts, a.end_ts IS NULL,
			c.id, c.start_ts, starts_with(a.source_key, $3)
		FROM ts.alarm_events a JOIN asset.equipment e ON e.id = a.asset_id
		LEFT JOIN LATERAL (SELECT p.id, p.start_ts
			FROM (SELECT o.start_ts FROM (SELECT s.status_id, s.start_ts, s.end_ts FROM ts.asset_status s
					WHERE s.asset_id = a.asset_id AND s.start_ts < a.start_ts ORDER BY s.start_ts DESC LIMIT 2) o
				WHERE o.status_id = $4 AND o.end_ts <= a.start_ts AND o.end_ts > a.start_ts - interval '2 minutes'
				ORDER BY o.start_ts DESC LIMIT 1) o
			CROSS JOIN LATERAL (SELECT p.id, p.start_ts, p.end_ts, p.severity FROM ts.alarm_events p
				WHERE p.asset_id = a.asset_id AND p.source_key = a.source_key AND p.start_ts < o.start_ts
				ORDER BY p.start_ts DESC LIMIT 1) p
			WHERE p.severity = a.severity AND p.end_ts <= o.start_ts AND NOT EXISTS (SELECT 1 FROM ts.asset_status s
				WHERE s.asset_id = a.asset_id AND s.status_id <> $4 AND s.start_ts < o.start_ts AND (s.end_ts IS NULL OR s.end_ts > p.end_ts))) c ON TRUE
		WHERE a.start_ts > $1 ORDER BY a.start_ts DESC LIMIT $2`,
		[]any{engine.ExternalAlarmPrefix, engine.StatusOffline}},
	{"fault", `SELECT f.id, e.id, e.alias, e.name, e.location_id, COALESCE(fr.name || ' (' || f.rule_code || ')', f.rule_code), f.severity,
			NULLIF(f.message, ''), f.started_at, f.status = 'ACTIVE', NULL::bigint, NULL::timestamptz, FALSE
		FROM analytics.faults f JOIN asset.equipment e ON e.id = f.asset_id LEFT JOIN analytics.fault_rules fr ON fr.code = f.rule_code
		WHERE f.started_at > $1 ORDER BY f.started_at DESC LIMIT $2`, nil},
	{"anomaly", `SELECT x.id, e.id, e.alias, e.name, e.location_id, COALESCE(mp.name, x.param_alias) || ' anomaly', x.severity,
			NULLIF(x.message, ''), x.started_at, x.status = 'ACTIVE', NULL::bigint, NULL::timestamptz, FALSE
		FROM analytics.anomalies x JOIN asset.equipment e ON e.id = x.asset_id
		LEFT JOIN asset.equipment_parameters ep ON ep.id = x.equipment_parameter_id LEFT JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE x.started_at > $1 ORDER BY x.started_at DESC LIMIT $2`, nil},
	{"trip", `SELECT st.id, e.id, e.alias, e.name, e.location_id, 'Trip / breakdown', 'CRITICAL', NULL::text, st.start_ts, st.end_ts IS NULL,
			c.id, c.start_ts, FALSE
		FROM asset.equipment e CROSS JOIN LATERAL (SELECT s.id, s.start_ts, s.end_ts FROM ts.asset_status s
			WHERE s.asset_id = e.id AND s.start_ts > $1 AND s.status_id = $3) st
		LEFT JOIN LATERAL (SELECT p.id, p.start_ts FROM (SELECT s.id, s.status_id, s.start_ts FROM ts.asset_status s
				WHERE s.asset_id = e.id AND s.start_ts < st.start_ts AND s.status_id <> $4 ORDER BY s.start_ts DESC LIMIT 1) p
			WHERE p.status_id = $3) c ON TRUE
		ORDER BY st.start_ts DESC LIMIT $2`, []any{engine.StatusUnplannedDown, engine.StatusOffline}},
}

// loadTwinEvents reads the events of every equipment (the ledger is shared by
// all users; the handler applies the user's scope) for the scan at now.
func (s *Server) loadTwinEvents(ctx context.Context, now time.Time) ([]twinEvent, error) {
	out := []twinEvent{}
	for _, src := range twinEventSources {
		args := append([]any{now.Add(-twinEventsWindow - twinEventLag[src.kind]), twinEventsScanCap}, src.args...)
		rows, err := s.DB.Query(ctx, src.query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var prevID *int64
			var prevStart *time.Time
			ev := twinEvent{Kind: src.kind}
			if err := rows.Scan(&id, &ev.AssetID, &ev.Alias, &ev.Name, &ev.LocationID, &ev.Title, &ev.Severity, &ev.Detail,
				&ev.Started, &ev.Active, &prevID, &prevStart, &ev.FDD); err != nil {
				rows.Close()
				return nil, err
			}
			ev.ID = src.kind + ":" + itoa64(id)
			if prevID != nil && prevStart != nil {
				ev.Prev, ev.PrevStart = src.kind+":"+itoa64(*prevID), *prevStart
			}
			out = append(out, ev)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
