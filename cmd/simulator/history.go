package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"afdd/internal/engine"
	"afdd/internal/notify"
)

// IDs maps plant definitions to database ids.
type IDs struct {
	Equip      map[string]int64
	Loc        map[string]int64
	EP         map[string]map[string]int64
	Code       map[string]int64
	ModelAlarm map[string]map[string]int64
	Rel        map[string]map[string]int64
	User       map[string]int64
	UserName   map[string]string
	Minutes    map[string]map[string]int // model name -> code -> instruction minutes
}

func loadIDs(ctx context.Context, db *pgxpool.Pool) (*IDs, error) {
	ids := &IDs{Equip: map[string]int64{}, Loc: map[string]int64{}, EP: map[string]map[string]int64{}, Code: map[string]int64{},
		ModelAlarm: map[string]map[string]int64{}, Rel: map[string]map[string]int64{}, User: map[string]int64{}, UserName: map[string]string{},
		Minutes: map[string]map[string]int{}}
	q := func(sql string, fn func(pgx.Rows) error, args ...any) error {
		rows, err := db.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	err := q(`SELECT e.alias, e.id, e.location_id, mp.alias, ep.id FROM asset.equipment e
		JOIN asset.equipment_parameters ep ON ep.equipment_id = e.id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id`,
		func(r pgx.Rows) error {
			var alias, p string
			var id, loc, ep int64
			if err := r.Scan(&alias, &id, &loc, &p, &ep); err != nil {
				return err
			}
			ids.Equip[alias], ids.Loc[alias] = id, loc
			if ids.EP[alias] == nil {
				ids.EP[alias] = map[string]int64{}
			}
			ids.EP[alias][p] = ep
			return nil
		})
	if err != nil {
		return nil, err
	}
	if err := q(`SELECT code, id FROM dm.alarm_codes`, func(r pgx.Rows) error {
		var c string
		var id int64
		err := r.Scan(&c, &id)
		ids.Code[c] = id
		return err
	}); err != nil {
		return nil, err
	}
	if err := q(`SELECT m.name, a.name, a.id FROM asset.model_alarms a JOIN asset.models m ON m.id = a.model_id`, func(r pgx.Rows) error {
		var m, c string
		var id int64
		err := r.Scan(&m, &c, &id)
		if ids.ModelAlarm[m] == nil {
			ids.ModelAlarm[m] = map[string]int64{}
		}
		ids.ModelAlarm[m][c] = id
		return err
	}); err != nil {
		return nil, err
	}
	if err := q(`SELECT m.name, r.name, r.id FROM dm.issue_relations r JOIN asset.models m ON m.id = r.model_id`, func(r pgx.Rows) error {
		var m, n string
		var id int64
		err := r.Scan(&m, &n, &id)
		if ids.Rel[m] == nil {
			ids.Rel[m] = map[string]int64{}
		}
		ids.Rel[m][n] = id
		return err
	}); err != nil {
		return nil, err
	}
	if err := q(`SELECT username, id, COALESCE(NULLIF(full_name, ''), username) FROM core.users WHERE NOT is_deleted`, func(r pgx.Rows) error {
		var u, n string
		var id int64
		err := r.Scan(&u, &id, &n)
		ids.User[u], ids.UserName[u] = id, n
		return err
	}); err != nil {
		return nil, err
	}
	err = q(`SELECT m.name, c.code, sum(i.duration) FROM dm.instructions i JOIN dm.instruction_models im ON im.instruction_id = i.id
		JOIN asset.models m ON m.id = im.model_id JOIN dm.alarm_codes c ON c.id = i.alarm_code_id WHERE NOT i.is_deleted
		GROUP BY m.name, c.code`, func(r pgx.Rows) error {
		var m, c string
		var mins int
		err := r.Scan(&m, &c, &mins)
		if ids.Minutes[m] == nil {
			ids.Minutes[m] = map[string]int{}
		}
		ids.Minutes[m][c] = mins
		return err
	})
	return ids, err
}

// ---------------------------------------------------------------------------

type hEvent struct {
	At      time.Time
	Status  string
	By      string // username, "" = system
	Details map[string]any
}

type hUpdate struct {
	At   time.Time
	By   string
	Note string
}

const (
	stRequested = iota
	stOpen
	stProgress
	stPaused
	stDone
	stClosed
	stRejected
)

var stageStatus = map[int]int{stRequested: 1, stOpen: 3, stProgress: 4, stPaused: 7, stDone: 5, stClosed: 6, stRejected: 2}

// hOrder is a maintenance order of the history.
type hOrder struct {
	Asset       *Asset
	Type        int // 1 corrective, 2 preventive
	FromSystem  bool
	RelName     string
	IsCompound  bool
	IsDeduct    bool
	Desc        string
	Severity    int
	Priority    int // 0 = none
	Issues      []string
	IssueStatus string
	Detected    *time.Time
	Params      []hParam
	RequestedBy string
	ApprovedBy  string
	EditedBy    string
	Techs       []string
	PStart      time.Time
	PEnd        time.Time
	Created     time.Time
	Schedule    int // index into schedules (0 = none)
	Reject      string

	stage                       int
	approveAt, startAt, closeAt time.Time
	rejectAt                    time.Time
	pauseAt, resumeAt           time.Time
	paused                      bool
	workDur, worked             time.Duration
	tripReset                   time.Duration
	forced                      int // forced status while in progress
	events                      []hEvent
	updates                     []hUpdate
	intervals                   [][2]time.Time
	started, completed, closed  *time.Time
	validDiag                   *bool
	updatesDone                 int
	trip                        bool
	name                        string
}

type hParam struct {
	Alias, Cond, Msg string
	At               time.Time
}

type hSchedule struct {
	Asset   *Asset
	Repeat  int
	Anchor  time.Time
	DurSec  int64
	Created time.Time
	Last    time.Time
	id      int64
}

type openEv struct {
	key, typ, name, msg, sev string
	start                    time.Time
	misses                   int
	hits                     int
	modelAlarm, epid, code   *int64
}

type track struct {
	status      int
	statusStart time.Time
	pending     map[string]int
	cond        string
	condStart   time.Time
	events      map[string]*openEv
	forced      int
	forcedN     int
}

// History generates and stores the plant history.
type History struct {
	ctx      context.Context
	db       *pgxpool.Pool
	plant    *Plant
	ids      *IDs
	loc      *time.Location
	rng      *rand.Rand
	start    time.Time
	end      time.Time
	step     time.Duration
	orders   []*hOrder
	scheds   []*hSchedule
	tracks   map[*Asset]*track
	openEW   map[string]*hOrder
	statusR  [][]any
	condR    [][]any
	eventR   [][]any
	notifR   [][]any
	values   [][]any
	nValues  int
	episodes []*episode
}

type episode struct {
	At    time.Time
	Asset *Asset
	Kind  string // fault | trip | offline | minor | reject
	Fault string
	Rate  float64
	Dur   time.Duration
	Code  string
}

func newHistory(ctx context.Context, db *pgxpool.Pool, plant *Plant, ids *IDs, days int, seed uint64) *History {
	end := time.Now().UTC().Truncate(time.Minute)
	return &History{ctx: ctx, db: db, plant: plant, ids: ids, loc: plant.loc, rng: rand.New(rand.NewPCG(seed, seed+7)),
		start: end.Add(-time.Duration(days) * 24 * time.Hour), end: end, step: time.Minute,
		tracks: map[*Asset]*track{}, openEW: map[string]*hOrder{}}
}

func (h *History) uniform(a, b float64) float64 { return a + h.rng.Float64()*(b-a) }

func (h *History) dur(minM, maxM float64) time.Duration {
	return time.Duration(h.uniform(minM, maxM) * float64(time.Minute))
}

// workTime moves t into the next day-shift window (08:00-18:00 local).
func (h *History) workTime(t time.Time) time.Time {
	lt := t.In(h.loc)
	hr := lt.Hour()
	switch {
	case hr < 8:
		return time.Date(lt.Year(), lt.Month(), lt.Day(), 8, 0, 0, 0, h.loc).Add(h.dur(5, 90)).UTC()
	case hr >= 18:
		n := lt.AddDate(0, 0, 1)
		return time.Date(n.Year(), n.Month(), n.Day(), 8, 0, 0, 0, h.loc).Add(h.dur(5, 90)).UTC()
	}
	return t
}

func (h *History) pick(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[h.rng.IntN(len(list))]
}

func (h *History) building(a *Asset) string { return buildingOf(a.Spec.Location) }

func (h *History) techs(a *Asset, n int) []string {
	cands := techniciansFor(h.building(a), a.Model.Type)
	h.rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	if n > len(cands) {
		n = len(cands)
	}
	return append([]string(nil), cands[:n]...)
}

func (h *History) minutes(a *Asset, codes []string, def int) time.Duration {
	total := 0
	for _, c := range codes {
		total += h.ids.Minutes[a.Model.Name][c]
	}
	if total == 0 {
		total = def
	}
	return time.Duration(total) * time.Minute
}

// plan schedules random episodes over the history window.
func (h *History) plan() {
	days := h.end.Sub(h.start).Hours() / 24
	span := h.end.Sub(h.start)
	at := func(min time.Duration) time.Time {
		return h.start.Add(min + time.Duration(h.rng.Float64()*float64(span-min-time.Hour)))
	}
	// Degradations: about one per asset every 45 days.
	for _, a := range h.plant.Assets {
		n := poisson(h.rng, days/45)
		for i := 0; i < n && len(a.Model.Faults) > 0; i++ {
			f := a.Model.Faults[h.rng.IntN(len(a.Model.Faults))]
			ramp := h.uniform(2, 9) * 86400
			h.episodes = append(h.episodes, &episode{At: at(0), Asset: a, Kind: "fault", Fault: f.Kind, Rate: 1.25 / ramp})
		}
	}
	// Make sure a few degradations are developing right now.
	for i := 0; i < 4; i++ {
		a := h.plant.pick(h.rng, func(a *Asset) bool { return len(a.Model.Faults) > 0 })
		f := a.Model.Faults[h.rng.IntN(len(a.Model.Faults))]
		ramp := h.uniform(3, 7) * 86400
		h.episodes = append(h.episodes, &episode{At: h.end.Add(-time.Duration(h.uniform(1, 2.5) * float64(24*time.Hour))), Asset: a,
			Kind: "fault", Fault: f.Kind, Rate: 1.25 / ramp})
	}
	for i := 0; i < int(days/6); i++ {
		a := h.plant.Assets[h.rng.IntN(len(h.plant.Assets))]
		h.episodes = append(h.episodes, &episode{At: at(0), Asset: a, Kind: "trip"})
	}
	for i := 0; i < int(days/10); i++ {
		a := h.plant.Assets[h.rng.IntN(len(h.plant.Assets))]
		h.episodes = append(h.episodes, &episode{At: at(0), Asset: a, Kind: "offline", Dur: h.dur(8, 40)})
	}
	for i := 0; i < int(days/8); i++ {
		a := h.plant.pick(h.rng, physical)
		code := "GEN-001"
		switch a.Kind {
		case "mau", "ct":
			code = "GEN-003"
		case "cda", "vac":
			code = "GEN-002"
		}
		h.episodes = append(h.episodes, &episode{At: at(0), Asset: a, Kind: "minor", Code: code})
	}
	for i := 0; i < int(days/15); i++ {
		a := h.plant.pick(h.rng, physical)
		h.episodes = append(h.episodes, &episode{At: at(0), Asset: a, Kind: "reject", Code: "GEN-002"})
	}
	sort.Slice(h.episodes, func(i, j int) bool { return h.episodes[i].At.Before(h.episodes[j].At) })
}

func poisson(r *rand.Rand, lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// planPM creates the preventive maintenance series.
func (h *History) planPM() {
	created := h.start.Add(-36 * time.Hour)
	horizon := h.end.Add(90 * 24 * time.Hour)
	missed := false
	for i, a := range h.plant.Assets {
		m := a.Model
		if m.PMCode == "" {
			continue
		}
		var anchor time.Time
		first := h.start.In(h.loc)
		if m.PMRepeat == 4 {
			d := h.end.Add(time.Duration(20+i%6*12) * 24 * time.Hour).In(h.loc)
			anchor = time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, h.loc)
		} else {
			anchor = time.Date(first.Year(), first.Month(), 3+(i*3)%24, 9, 0, 0, 0, h.loc)
		}
		dur := time.Duration(m.PMHours * float64(time.Hour))
		sc := &hSchedule{Asset: a, Repeat: m.PMRepeat, Anchor: anchor.UTC(), DurSec: int64(dur.Seconds()), Created: created}
		h.scheds = append(h.scheds, sc)
		sup := supervisorFor(h.building(a))
		techs := h.techs(a, 1+h.rng.IntN(2))
		for k := 0; ; k++ {
			months := k
			if m.PMRepeat == 4 {
				months = 12 * k
			}
			ps := addMonths(anchor, months).UTC()
			if ps.After(horizon) {
				break
			}
			sc.Last = ps
			c := created
			if gen := ps.Add(-90 * 24 * time.Hour); gen.After(c) {
				c = gen
			}
			o := &hOrder{Asset: a, Type: 2, Desc: codeName(m.PMCode) + " - " + a.Spec.Name, Severity: 4, Priority: 2,
				Issues: []string{m.PMCode}, RequestedBy: sup, ApprovedBy: sup, EditedBy: sup, Techs: techs, PStart: ps, PEnd: ps.Add(dur),
				Created: c, Schedule: len(h.scheds), stage: stOpen, forced: 5}
			by := sup
			if c.After(created) {
				by = ""
			}
			o.events = append(o.events, hEvent{At: c, Status: "CREATED", By: by, Details: map[string]any{"By": h.display(by, "System (recurring schedule)")}})
			o.startAt = ps.Add(h.dur(-20, 90))
			if h.rng.Float64() < 0.1 {
				o.startAt = o.startAt.Add(24 * time.Hour)
			}
			if !missed && ps.Before(h.end.Add(-10*24*time.Hour)) && ps.After(h.end.Add(-20*24*time.Hour)) && h.rng.Float64() < 0.3 {
				o.startAt = horizon.Add(time.Hour) // never started: overdue
				missed = true
			}
			o.workDur = time.Duration(float64(dur) * h.uniform(0.75, 1.15))
			h.orders = append(h.orders, o)
		}
	}
}

func addMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m, 1, t.Hour(), t.Minute(), 0, 0, t.Location()).AddDate(0, months, 0)
	last := first.AddDate(0, 1, -1).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), 0, 0, t.Location())
}

func (h *History) display(username, def string) string {
	if username == "" {
		return def
	}
	if n, ok := h.ids.UserName[username]; ok {
		return n
	}
	return username
}

// newCorrective creates a corrective order and plans its workflow.
func (h *History) newCorrective(a *Asset, t time.Time, o *hOrder, urgent bool) {
	o.Asset, o.Type, o.Created, o.stage = a, 1, t, stRequested
	if len(o.events) == 0 {
		o.events = append(o.events, hEvent{At: t, Status: "CREATED", By: o.RequestedBy, Details: map[string]any{"By": h.display(o.RequestedBy, "")}})
	}
	sup := supervisorFor(h.building(a))
	if urgent {
		o.approveAt = t.Add(h.dur(10, 45))
	} else {
		o.approveAt = h.workTime(t.Add(h.dur(20, 150)))
	}
	o.ApprovedBy, o.EditedBy = sup, sup
	o.Techs = h.techs(a, 1+h.rng.IntN(2))
	if len(o.Techs) == 0 {
		o.Techs = []string{h.pick(techniciansFor("CUB Central Utility Building", "Centrifugal Chiller"))}
	}
	if urgent {
		o.startAt = o.approveAt.Add(h.dur(20, 90))
	} else if o.Severity == 1 {
		o.startAt = h.workTime(o.approveAt.Add(h.dur(30, 120)))
	} else {
		o.startAt = h.workTime(o.approveAt.Add(h.dur(120, 1440)))
	}
	o.workDur = time.Duration(float64(h.minutes(a, o.Issues, 60)) * h.uniform(0.8, 1.4))
	if o.workDur < 30*time.Minute {
		o.workDur = 30 * time.Minute
	}
	if h.rng.Float64() < 0.15 && o.workDur > time.Hour {
		o.pauseAt = time.Time{} // decided when work starts
		o.paused = false
		o.resumeAt = time.Time{}
		o.tripReset = -1 // marker: pause planned
	}
	v := h.rng.Float64() < 0.92
	o.validDiag = &v
	h.orders = append(h.orders, o)
	if t.After(h.end.Add(-48 * time.Hour)) {
		h.notify(t, a, "MAINTENANCE_ORDER_CREATED", "", o)
	}
}

// advance moves an order through its workflow at time t.
func (h *History) advance(o *hOrder, t time.Time) {
	a := o.Asset
	tr := h.tracks[a]
	switch o.stage {
	case stRequested:
		if !o.rejectAt.IsZero() {
			if !t.Before(o.rejectAt) {
				o.stage = stRejected
				o.events = append(o.events, hEvent{At: t, Status: "REJECTED", By: o.ApprovedBy, Details: map[string]any{"By": h.display(o.ApprovedBy, ""), "Reason": o.Reject}})
			}
			return
		}
		if t.Before(o.approveAt) {
			return
		}
		o.stage = stOpen
		if o.Priority == 0 {
			o.Priority = 2
			if o.Severity <= 2 {
				o.Priority = 1
			}
		}
		o.PStart = o.startAt.Truncate(15 * time.Minute)
		o.PEnd = o.PStart.Add(o.workDur).Truncate(15 * time.Minute).Add(15 * time.Minute)
		o.events = append(o.events, hEvent{At: t, Status: "APPROVED", By: o.ApprovedBy, Details: map[string]any{"By": h.display(o.ApprovedBy, "")}})
		h.notifyOrder(t, o, "MAINTENANCE_ORDER_APPROVED", "Maintenance order %s for equipment %s has been approved")
	case stOpen:
		if t.Before(o.startAt) {
			return
		}
		o.stage = stProgress
		ts := t
		o.started = &ts
		o.intervals = append(o.intervals, [2]time.Time{t, {}})
		d := map[string]any{"By": h.display(o.Techs[0], "")}
		if o.forced > 0 {
			label := "Planned Down"
			if o.forced == 3 {
				label = "Unplanned Down"
			}
			d["Equipment Status"] = label
			tr.forcedN++
			tr.forced = o.forced
			a.Maint = true
		}
		o.events = append(o.events, hEvent{At: t, Status: "STARTED", By: o.Techs[0], Details: d})
		if o.tripReset == -1 {
			o.pauseAt = t.Add(time.Duration(float64(o.workDur) * h.uniform(0.3, 0.6)))
			o.tripReset = 0
		}
		h.notifyOrder(t, o, "MAINTENANCE_ORDER_STARTED", "Maintenance order %s for equipment %s has been started")
	case stProgress:
		o.worked += h.step
		if o.trip && a.Trip && o.worked >= 20*time.Minute {
			a.Trip = false
		}
		frac := float64(o.worked) / float64(o.workDur)
		if (o.updatesDone == 0 && frac >= 0.35) || (o.updatesDone == 1 && frac >= 0.8 && len(workNotes[o.Issues[0]]) > 1) {
			note := noteFor(o.Issues[0], o.updatesDone)
			o.updates = append(o.updates, hUpdate{At: t, By: o.Techs[0], Note: note})
			issues := []map[string]any{{"label": o.Issues[0] + " - " + codeName(o.Issues[0]), "is_other": false}}
			o.events = append(o.events, hEvent{At: t, Status: "UPDATED", By: o.Techs[0], Details: map[string]any{"By": h.display(o.Techs[0], ""),
				"Note": note, "attachment_urls": []string{}, "Issue": issues}})
			o.updatesDone++
		}
		if !o.pauseAt.IsZero() && !o.paused && !t.Before(o.pauseAt) {
			o.paused = true
			o.stage = stPaused
			o.resumeAt = t.Add(h.dur(240, 1800))
			o.intervals[len(o.intervals)-1][1] = t
			reason := "Waiting for spare parts from vendor"
			o.events = append(o.events, hEvent{At: t, Status: "PAUSED", By: o.Techs[0], Details: map[string]any{"By": h.display(o.Techs[0], ""), "Reason": reason}})
			return
		}
		if o.worked < o.workDur {
			return
		}
		o.stage = stDone
		tc := t
		o.completed = &tc
		o.intervals[len(o.intervals)-1][1] = t
		if o.forced > 0 {
			tr.forcedN--
			if tr.forcedN <= 0 {
				tr.forcedN, tr.forced = 0, 0
				a.Maint = false
			}
		}
		if o.trip {
			a.Trip = false
		}
		if o.validDiag == nil || *o.validDiag {
			repair(a, o.Issues, o.Type == 2)
		}
		by := o.Techs[0]
		o.events = append(o.events, hEvent{At: t, Status: "COMPLETED", By: by, Details: map[string]any{"By": h.display(by, "")}})
		if o.Type == 1 {
			valid := true
			if o.validDiag != nil {
				valid = *o.validDiag
			}
			var rep any = valid
			if !valid {
				rep = nil
			}
			o.events = append(o.events, hEvent{At: t.Add(time.Millisecond), Status: "COMPLETED", By: by, Details: map[string]any{
				"Issue":    []map[string]any{{"label": o.Issues[0] + " - " + codeName(o.Issues[0]), "is_other": false}},
				"is_other": false, "diagnosis": valid, "repair": rep}})
		}
		o.closeAt = h.workTime(t.Add(h.dur(45, 900)))
		h.notifyOrder(t, o, "MAINTENANCE_ORDER_COMPLETED", "Maintenance order %s for equipment %s has been completed")
	case stPaused:
		if t.Before(o.resumeAt) {
			return
		}
		o.stage = stProgress
		o.intervals = append(o.intervals, [2]time.Time{t, {}})
		o.events = append(o.events, hEvent{At: t, Status: "RESUMED", By: o.Techs[0], Details: map[string]any{"By": h.display(o.Techs[0], "")}})
	case stDone:
		if t.Before(o.closeAt) {
			return
		}
		o.stage = stClosed
		tc := t
		o.closed = &tc
		o.events = append(o.events, hEvent{At: t, Status: "CLOSED", By: o.ApprovedBy, Details: map[string]any{"By": h.display(o.ApprovedBy, "")}})
		h.notifyOrder(t, o, "MAINTENANCE_ORDER_CLOSED_TECHNICIAN", "Maintenance order %s for equipment %s has been closed")
	}
}

func (h *History) notifyOrder(t time.Time, o *hOrder, cat, format string) {
	if t.Before(h.end.Add(-48 * time.Hour)) {
		return
	}
	h.notify(t, o.Asset, cat, fmt.Sprintf(format, "%ORDER%", o.Asset.Spec.Name), o)
}

func (h *History) notify(t time.Time, a *Asset, cat, msg string, o *hOrder) {
	var add any
	if o != nil {
		if msg == "" {
			msg = fmt.Sprintf("Early warning: maintenance order %s has been raised for equipment %s", "%ORDER%", a.Spec.Name)
			if !o.FromSystem {
				msg = fmt.Sprintf("Maintenance order %s has been requested for equipment %s", "%ORDER%", a.Spec.Name)
			}
		}
		add = o
	}
	h.notifR = append(h.notifR, []any{cat, msg, h.ids.Equip[a.Spec.Alias], h.ids.Loc[a.Spec.Alias], add, t})
}

// Generate runs the simulation over the history window.
func (h *History) Generate() error {
	h.plan()
	h.planPM()
	for _, a := range h.plant.Assets {
		h.tracks[a] = &track{events: map[string]*openEv{}, pending: map[string]int{}}
	}
	ep := 0
	dt := h.step.Seconds()
	recent := h.end.Add(-7 * 24 * time.Hour)
	notifyFrom := h.end.Add(-48 * time.Hour)
	lastLog := time.Time{}
	for t := h.start; t.Before(h.end); t = t.Add(h.step) {
		for ep < len(h.episodes) && !h.episodes[ep].At.After(t) {
			h.startEpisode(h.episodes[ep], t)
			ep++
		}
		for _, a := range h.plant.Assets {
			for _, f := range a.Faults {
				f.Sev = math.Min(maxSeverity, f.Sev+f.Rate*dt)
			}
			if a.Kind == "mau" {
				a.FilterDays += dt / 86400
			}
			if a.Offline && !t.Before(a.OffUntil) {
				a.Offline = false
			}
			if !a.SpikeUntil.IsZero() && !t.Before(a.SpikeUntil) {
				a.SpikeUntil = time.Time{}
				a.Trip = true
			}
		}
		for _, o := range h.orders {
			if o.stage == stClosed || o.stage == stRejected || o.Created.After(t) {
				continue
			}
			h.advance(o, t)
		}
		values := h.plant.Step(t, dt)
		logNow := lastLog.IsZero() || (t.Before(recent) && t.Sub(lastLog) >= 15*time.Minute) || (!t.Before(recent) && t.Sub(lastLog) >= 5*time.Minute)
		if logNow {
			lastLog = t
		}
		for _, a := range h.plant.Assets {
			v := values[a]
			tr := h.tracks[a]
			ev := h.plant.evaluate(a, v, a.Offline, tr.forced)
			h.trackStatus(a, tr, ev, t, !t.Before(notifyFrom))
			h.trackEvents(a, tr, ev, t, !t.Before(notifyFrom))
			if !a.Offline {
				if logNow {
					h.logValues(a, v, t)
				}
				if !a.SpikeUntil.IsZero() {
					continue
				}
				for _, d := range h.plant.diagnose(a, ev) {
					h.earlyWarning(a, d, ev, t)
				}
			}
		}
		if len(h.values) >= 100000 {
			if err := h.flushValues(); err != nil {
				return err
			}
		}
	}
	return h.flushValues()
}

func (h *History) startEpisode(e *episode, t time.Time) {
	a := e.Asset
	switch e.Kind {
	case "fault":
		if a.Faults[e.Fault] == nil {
			a.Faults[e.Fault] = &Fault{Kind: e.Fault, Rate: e.Rate, Start: t}
		}
	case "offline":
		a.Offline, a.OffUntil = true, t.Add(e.Dur)
	case "trip":
		if a.Trip || a.Maint || !a.Running() || len(a.Model.Trips) == 0 {
			return
		}
		ts := a.Model.Trips[0]
		a.TripSpec = &ts
		a.SpikeUntil = t.Add(2 * time.Minute)
		op := h.pick(operators())
		o := &hOrder{Desc: requestTexts[ts.Code], Severity: 2, Priority: 1, Issues: []string{ts.Code}, RequestedBy: op, forced: 3, trip: true}
		if o.Desc == "" {
			o.Desc = ts.Reason
		}
		h.newCorrective(a, t.Add(h.dur(5, 20)), o, true)
	case "minor":
		op := h.pick(append(operators(), h.techs(a, 1)...))
		o := &hOrder{Desc: requestTexts[e.Code], Severity: 3, Issues: []string{e.Code}, RequestedBy: op}
		h.newCorrective(a, t, o, false)
	case "reject":
		op := h.pick(operators())
		o := &hOrder{Desc: requestTexts[e.Code], Severity: 3, Issues: []string{e.Code}, RequestedBy: op,
			Reject: "Inspected on site - noise is within the normal operating range, no fault found."}
		h.newCorrective(a, t, o, false)
		o.rejectAt = h.workTime(t.Add(h.dur(60, 240)))
	}
}

func (h *History) earlyWarning(a *Asset, d Diagnosis, ev Evaluation, t time.Time) {
	key := a.Spec.Alias + "|" + d.Rel.Name
	if o := h.openEW[key]; o != nil {
		if o.stage != stDone && o.stage != stClosed && o.stage != stRejected {
			return
		}
		if o.completed != nil && t.Sub(*o.completed) < time.Hour {
			return // engine cooldown
		}
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
	code := d.Rel.Target
	sev := 3
	if d.Severity == "CRITICAL" {
		sev = 1
	}
	det := t
	o := &hOrder{FromSystem: true, RelName: d.Rel.Name, IsCompound: d.IsCompound, IsDeduct: d.IsDeduct, Severity: sev,
		Desc:   fmt.Sprintf("%s detected by issue relation '%s': %s - %s", kind, d.Rel.Name, code, codeName(code)),
		Issues: []string{code}, IssueStatus: d.Severity, Detected: &det, forced: 3, PStart: t}
	o.PEnd = t.Add(h.minutes(a, o.Issues, 60))
	for _, p := range d.Params {
		hp := ev.Abnormal[p]
		o.Params = append(o.Params, hParam{Alias: p, Cond: hp.Severity, Msg: hp.Message, At: t})
	}
	o.events = append(o.events, hEvent{At: t, Status: "CREATED", Details: map[string]any{"By": "System (early warning)", "Detected": o.Desc}})
	h.newCorrective(a, t, o, false)
	h.openEW[key] = o
}

func (h *History) trackStatus(a *Asset, tr *track, ev Evaluation, t time.Time, notifyOn bool) {
	id := h.ids.Equip[a.Spec.Alias]
	if tr.statusStart.IsZero() {
		tr.status, tr.statusStart = ev.Status, t
	} else if ev.Status != tr.status {
		h.statusR = append(h.statusR, []any{id, tr.status, tr.statusStart, t})
		tr.status, tr.statusStart = ev.Status, t
		if notifyOn {
			h.notify(t, a, statusCategory(ev.Status), notify.StatusMessage(a.Spec.Name, engine.StatusName(ev.Status)), nil)
		}
	}
	if tr.condStart.IsZero() {
		tr.cond, tr.condStart = ev.Cond, t
	} else if ev.Cond != tr.cond {
		h.condR = append(h.condR, []any{id, tr.cond, tr.condStart, t})
		tr.cond, tr.condStart = ev.Cond, t
	}
}

func statusCategory(s int) string {
	switch s {
	case engine.StatusRunning:
		return "STATUS_RUNNING"
	case engine.StatusIdle:
		return "STATUS_IDLE"
	case engine.StatusUnplannedDown:
		return "STATUS_DOWN"
	case engine.StatusPlannedDown:
		return "STATUS_PLANNED_DOWN"
	}
	return "STATUS_OFFLINE"
}

func (h *History) trackEvents(a *Asset, tr *track, ev Evaluation, t time.Time, notifyOn bool) {
	id := h.ids.Equip[a.Spec.Alias]
	active := map[string]bool{}
	for code, hh := range ev.Alarms {
		key := "alarm:" + code
		active[key] = true
		if e := tr.events[key]; e != nil {
			e.misses = 0
			continue
		}
		if tr.pending[key]++; tr.pending[key] < 2 {
			continue
		}
		maID := h.ids.ModelAlarm[a.Model.Name][code]
		cid := h.ids.Code[code]
		tr.events[key] = &openEv{key: fmt.Sprintf("alarm:%d", maID), typ: "ALARM", name: code, msg: hh.Message, sev: hh.Severity, start: t,
			modelAlarm: &maID, code: &cid}
		if notifyOn {
			cat := "ALARM_WARNING"
			if hh.Severity == "CRITICAL" {
				cat = "ALARM_CRITICAL"
			}
			h.notify(t, a, cat, notify.AlarmMessage(hh.Severity, code, a.Spec.Name), nil)
		}
	}
	for alias, hh := range ev.Abnormal {
		key := "abn:" + alias
		active[key] = true
		if e := tr.events[key]; e != nil {
			e.misses = 0
			continue
		}
		if tr.pending[key]++; tr.pending[key] < 2 {
			continue
		}
		ep := h.ids.EP[a.Spec.Alias][alias]
		name := ""
		for _, p := range a.Model.Params {
			if p.Alias == alias {
				name = p.Name
			}
		}
		tr.events[key] = &openEv{key: fmt.Sprintf("abn:%d", ep), typ: "ABNORMAL", name: name, msg: hh.Message, sev: hh.Severity, start: t, epid: &ep}
		if notifyOn {
			cat := "PARAMETER_WARNING"
			if hh.Severity == "CRITICAL" {
				cat = "PARAMETER_CRITICAL"
			}
			h.notify(t, a, cat, fmt.Sprintf("%s: Abnormal Parameter is %s %s", a.Spec.Name, name, strings.TrimPrefix(hh.Message, name+" is ")), nil)
		}
	}
	for key := range tr.pending {
		if !active[key] {
			delete(tr.pending, key)
		}
	}
	for key, e := range tr.events {
		if active[key] {
			continue
		}
		e.misses++
		if e.misses >= 4 || ev.Cond == engine.CondOffline {
			h.eventR = append(h.eventR, []any{id, e.typ, e.key, e.modelAlarm, e.epid, e.code, e.name, e.sev, e.msg, e.start, t})
			delete(tr.events, key)
		}
	}
}

func (h *History) logValues(a *Asset, v map[string]float64, t time.Time) {
	for _, p := range a.Model.Params {
		ep := h.ids.EP[a.Spec.Alias][p.Alias]
		if ep == 0 {
			continue
		}
		x := v[p.Alias]
		if !p.Bool {
			x = round(x, p.Scale)
		}
		h.values = append(h.values, []any{ep, t, x, nil})
	}
}

func (h *History) flushValues() error {
	if len(h.values) == 0 {
		return nil
	}
	n, err := h.db.CopyFrom(h.ctx, pgx.Identifier{"ts", "parameter_values"}, []string{"equipment_parameter_id", "ts", "value", "value_text"},
		pgx.CopyFromRows(h.values))
	if err != nil {
		return fmt.Errorf("copy parameter values: %w", err)
	}
	h.nValues += int(n)
	h.values = h.values[:0]
	return nil
}

// Store writes intervals, events, orders and notifications.
func (h *History) Store() error {
	for _, a := range h.plant.Assets {
		tr := h.tracks[a]
		id := h.ids.Equip[a.Spec.Alias]
		h.statusR = append(h.statusR, []any{id, tr.status, tr.statusStart, h.end})
		h.condR = append(h.condR, []any{id, tr.cond, tr.condStart, h.end})
		for _, e := range tr.events {
			h.eventR = append(h.eventR, []any{id, e.typ, e.key, e.modelAlarm, e.epid, e.code, e.name, e.sev, e.msg, e.start, h.end})
		}
	}
	return pgx.BeginFunc(h.ctx, h.db, func(tx pgx.Tx) error {
		copyRows := func(table string, cols []string, rows [][]any) error {
			_, err := tx.CopyFrom(h.ctx, pgx.Identifier{"ts", table}, cols, pgx.CopyFromRows(rows))
			if err != nil {
				return fmt.Errorf("copy %s: %w", table, err)
			}
			return nil
		}
		if err := copyRows("asset_status", []string{"asset_id", "status_id", "start_ts", "end_ts"}, h.statusR); err != nil {
			return err
		}
		if err := copyRows("asset_condition", []string{"asset_id", "condition", "start_ts", "end_ts"}, h.condR); err != nil {
			return err
		}
		if err := copyRows("alarm_events", []string{"asset_id", "alarm_type", "source_key", "model_alarm_id", "equipment_parameter_id",
			"alarm_code_id", "name", "severity", "message", "start_ts", "end_ts"}, h.eventR); err != nil {
			return err
		}
		if err := h.storeOrders(tx); err != nil {
			return err
		}
		return h.storeNotifications(tx)
	})
}

func (h *History) uid(username string) any {
	if id, ok := h.ids.User[username]; ok {
		return id
	}
	return nil
}

func (h *History) storeOrders(tx pgx.Tx) error {
	// Names: CM|PM-MM-YY-NNN by creation month, chronological.
	sorted := append([]*hOrder(nil), h.orders...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.Before(sorted[j].Created) })
	seq := map[string]int{}
	for _, o := range sorted {
		prefix := "PM"
		if o.Type == 1 {
			prefix = "CM"
		}
		lt := o.Created.In(h.loc)
		base := fmt.Sprintf("%s-%02d-%02d-", prefix, int(lt.Month()), lt.Year()%100)
		seq[base]++
		o.name = fmt.Sprintf("%s%03d", base, seq[base])
	}
	for _, sc := range h.scheds {
		if err := tx.QueryRow(h.ctx, `INSERT INTO dm.maintenance_schedules (repeat_type_id, anchor_start, duration_sec, generated_until, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, sc.Repeat, sc.Anchor, sc.DurSec, sc.Last, sc.Created).Scan(&sc.id); err != nil {
			return err
		}
	}
	var forcedRows [][3]int64
	for _, o := range sorted {
		a := o.Asset
		status := stageStatus[o.stage]
		var sched, relID, prio any
		if o.Schedule > 0 {
			sched = h.scheds[o.Schedule-1].id
		}
		if o.RelName != "" {
			relID = h.ids.Rel[a.Model.Name][o.RelName]
		}
		if o.Priority > 0 {
			prio = o.Priority
		}
		approved := any(nil)
		if o.stage != stRequested {
			approved = h.uid(o.ApprovedBy)
		}
		var degName any
		if o.RelName != "" {
			degName = o.RelName
		}
		updated := o.Created
		if n := len(o.events); n > 0 {
			updated = o.events[n-1].At
		}
		var id int64
		err := tx.QueryRow(h.ctx, `INSERT INTO dm.maintenance_orders (name, schedule_id, asset_id, maintenance_type_id, priority_id, severity_id,
			status_id, description, planned_start, planned_end, requested_by, approved_by, edited_by, reject_reason, from_system, issue_relation_id,
			is_compound, is_deduction, degradation_name, detected_at, started_at, completed_at, closed_at, scheduled_notified, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26) RETURNING id`,
			o.name, sched, h.ids.Equip[a.Spec.Alias], o.Type, prio, o.Severity, status, o.Desc, o.PStart, o.PEnd,
			h.uid(o.RequestedBy), approved, approved, o.Reject, o.FromSystem, relID, o.IsCompound, o.IsDeduct, degName, o.Detected,
			o.started, o.completed, o.closed, o.PStart.Before(h.end), o.Created, updated).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		for i, c := range o.Issues {
			var vd, vr any
			if o.Type == 1 && o.completed != nil && o.validDiag != nil {
				vd = *o.validDiag
				if *o.validDiag {
					vr = true
				}
			}
			st := o.IssueStatus
			if st == "" {
				st = "WARNING"
			}
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.maintenance_order_issues (order_id, alarm_code_id, is_other, position, is_valid_diagnostic,
				is_valid_repair, alarm_status, detected_at) VALUES ($1,$2,FALSE,$3,$4,$5,$6,$7)`, id, h.ids.Code[c], i+1, vd, vr, st, o.Detected); err != nil {
				return err
			}
		}
		for _, t := range o.Techs {
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.maintenance_order_technicians (order_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, h.uid(t)); err != nil {
				return err
			}
		}
		for _, p := range o.Params {
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.maintenance_order_parameters (order_id, equipment_parameter_id, condition, message, detected_at)
				VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id, h.ids.EP[a.Spec.Alias][p.Alias], p.Cond, p.Msg, p.At); err != nil {
				return err
			}
		}
		for _, e := range o.events {
			if e.At.After(h.end) {
				continue
			}
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.maintenance_order_events (order_id, status, details, created_by, created_at) VALUES ($1,$2,$3,$4,$5)`,
				id, e.Status, e.Details, h.uid(e.By), e.At); err != nil {
				return err
			}
		}
		for _, iv := range o.intervals {
			var endT any
			if !iv[1].IsZero() {
				endT = iv[1]
			}
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.work_intervals (order_id, started_at, ended_at) VALUES ($1,$2,$3)`, id, iv[0], endT); err != nil {
				return err
			}
		}
		for _, u := range o.updates {
			if _, err := tx.Exec(h.ctx, `INSERT INTO dm.work_updates (order_id, note, issue_ids, created_by, created_at) VALUES ($1,$2,$3,$4,$5)`,
				id, u.Note, []int64{h.ids.Code[o.Issues[0]]}, h.uid(u.By), u.At); err != nil {
				return err
			}
		}
		if (o.stage == stProgress || o.stage == stPaused) && o.forced > 0 {
			forcedRows = append(forcedRows, [3]int64{h.ids.Equip[a.Spec.Alias], int64(o.forced), id})
		}
		for i := range h.notifR {
			if h.notifR[i][4] == o {
				h.notifR[i][4] = map[string]any{"maintenance_id": id, "order_name": o.name, "selected": id}
				h.notifR[i][1] = strings.Replace(h.notifR[i][1].(string), "%ORDER%", o.name, 1)
			}
		}
	}
	for _, f := range forcedRows {
		if _, err := tx.Exec(h.ctx, `UPDATE asset.equipment SET forced_status_id = $2, forced_by_order_id = $3 WHERE id = $1`, f[0], f[1], f[2]); err != nil {
			return err
		}
	}
	return nil
}

func (h *History) storeNotifications(tx pgx.Tx) error {
	var rows [][]any
	for _, n := range h.notifR {
		var add any
		if m, ok := n[4].(map[string]any); ok {
			b, _ := json.Marshal(m)
			add = string(b)
		}
		rows = append(rows, []any{n[0], n[1], n[2], n[3], add, n[5]})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(h.ctx, pgx.Identifier{"core", "notifications"}, []string{"category", "message", "asset_id", "location_id", "additional_data", "created_at"},
		pgx.CopyFromRows(rows))
	return err
}

// clearExisting removes earlier (partial) history of the plant equipment.
func clearExisting(ctx context.Context, db *pgxpool.Pool, ids *IDs) error {
	var eq []int64
	for _, id := range ids.Equip {
		eq = append(eq, id)
	}
	stmts := []string{
		`DELETE FROM ts.parameter_values WHERE equipment_parameter_id IN (SELECT id FROM asset.equipment_parameters WHERE equipment_id = ANY($1))`,
		`DELETE FROM ts.asset_status WHERE asset_id = ANY($1)`,
		`DELETE FROM ts.asset_condition WHERE asset_id = ANY($1)`,
		`DELETE FROM ts.alarm_events WHERE asset_id = ANY($1)`,
		`DELETE FROM ts.running_hour_flags WHERE asset_id = ANY($1)`,
		`DELETE FROM core.notifications WHERE asset_id = ANY($1)`,
		`DELETE FROM dm.maintenance_schedules WHERE id IN (SELECT schedule_id FROM dm.maintenance_orders WHERE asset_id = ANY($1))`,
		`DELETE FROM dm.maintenance_orders WHERE asset_id = ANY($1)`,
		`UPDATE asset.equipment SET forced_status_id = NULL, forced_by_order_id = NULL WHERE id = ANY($1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s, eq); err != nil {
			return fmt.Errorf("%s: %w", s[:40], err)
		}
	}
	return nil
}

// runHistory generates and stores the history, returning the plant state at
// the end of the window for the live simulator.
func runHistory(ctx context.Context, db *pgxpool.Pool, plant *Plant, days int, seed uint64) (*History, error) {
	ids, err := loadIDs(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := clearExisting(ctx, db, ids); err != nil {
		return nil, err
	}
	h := newHistory(ctx, db, plant, ids, days, seed)
	slog.Info("generating history", "from", h.start.Format(time.RFC3339), "to", h.end.Format(time.RFC3339))
	started := time.Now()
	if err := h.Generate(); err != nil {
		return nil, err
	}
	if err := h.Store(); err != nil {
		return nil, err
	}
	stats := map[string]int{}
	for _, o := range h.orders {
		stats[fmt.Sprintf("status_%d", stageStatus[o.stage])]++
	}
	slog.Info("history stored", "took", time.Since(started).Round(time.Second), "values", h.nValues, "status_intervals", len(h.statusR),
		"alarm_events", len(h.eventR), "orders", len(h.orders), "notifications", len(h.notifR), "order_states", stats)
	return h, nil
}
