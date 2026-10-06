package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Bot simulates the facility team working on maintenance orders. It only
// acts on orders nobody touched for a while, so people testing the
// application can always take over an order themselves.
type Bot struct {
	ctx      context.Context
	db       *pgxpool.Pool
	c        *Client
	plant    *Plant
	live     *Live
	rng      *rand.Rand
	approve  time.Duration
	tzOffset int
	pending  []pendingTrip
	snap     *Plant // copy of the live plant for this cycle's work starts (mayStop)
}

type pendingTrip struct {
	asset *Asset
	at    time.Time
}

type botOrder struct {
	ID         int64
	Name       string
	Alias      string
	Status     int
	Type       int
	Severity   int
	Priority   *int
	Desc       string
	PStart     time.Time
	PEnd       *time.Time
	LastEvent  time.Time
	Codes      []string
	CodeIDs    []int64
	Other      []int64
	Techs      []string
	WorkedSec  float64
	Updates    int
	FromSystem bool
	Starter    string // technician who started the work
}

func newBot(ctx context.Context, db *pgxpool.Pool, c *Client, plant *Plant, live *Live, approve time.Duration, loc *time.Location, seed uint64) *Bot {
	_, off := time.Now().In(loc).Zone()
	return &Bot{ctx: ctx, db: db, c: c, plant: plant, live: live, rng: rand.New(rand.NewPCG(seed, 99)), approve: approve, tzOffset: -off / 60}
}

// Run works orders every 30 seconds.
func (b *Bot) Run() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case a := <-b.live.trips:
			b.pending = append(b.pending, pendingTrip{asset: a, at: time.Now().Add(time.Duration(3+b.rng.IntN(5)) * time.Minute)})
		case <-t.C:
			b.tripRequests()
			if err := b.work(); err != nil && b.ctx.Err() == nil {
				slog.Warn("workforce", "err", err)
			}
		}
	}
}

func (b *Bot) iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (b *Bot) tripRequests() {
	var keep []pendingTrip
	for _, p := range b.pending {
		if time.Now().Before(p.at) {
			keep = append(keep, p)
			continue
		}
		a := p.asset
		code := a.TripSpec.Code
		op := operators()[b.rng.IntN(len(operators()))]
		desc := requestTexts[code]
		if desc == "" {
			desc = a.TripSpec.Reason
		}
		start := time.Now().Add(15 * time.Minute)
		_, err := b.c.Call(b.ctx, op, "maintenance_order/submit", map[string]any{"submit_type": "CREATED", "asset_id": b.equipmentID(a.Spec.Alias),
			"maintenance_type_id": 1, "priority_id": 1, "severity_id": 2, "description": desc, "planned_start": b.iso(start),
			"planned_end": b.iso(start.Add(time.Hour)), "instructions": fmt.Sprint(codeID(b.ctx, b.db, code)), "timezone_offset": b.tzOffset})
		if err != nil {
			slog.Warn("operator request failed", "equipment", a.Spec.Alias, "err", err)
		} else {
			slog.Info("operator raised maintenance request", "operator", op, "equipment", a.Spec.Alias, "code", code)
		}
	}
	b.pending = keep
}

func (b *Bot) equipmentID(alias string) int64 {
	return idOf(b.ctx, b.db, `SELECT id FROM asset.equipment WHERE alias = $1`, alias)
}

func (b *Bot) load() ([]*botOrder, error) {
	var aliases []string
	for _, a := range b.plant.Assets {
		aliases = append(aliases, a.Spec.Alias)
	}
	rows, err := b.db.Query(b.ctx, `SELECT o.id, o.name, e.alias, o.status_id, o.maintenance_type_id, o.severity_id, o.priority_id, o.description,
			o.planned_start, o.planned_end, o.from_system,
			COALESCE((SELECT max(ev.created_at) FROM dm.maintenance_order_events ev WHERE ev.order_id = o.id), o.created_at),
			COALESCE((SELECT array_agg(c.code ORDER BY i.position) FROM dm.maintenance_order_issues i JOIN dm.alarm_codes c ON c.id = i.alarm_code_id
				WHERE i.order_id = o.id AND NOT i.is_other), '{}'),
			COALESCE((SELECT array_agg(i.alarm_code_id ORDER BY i.position) FROM dm.maintenance_order_issues i WHERE i.order_id = o.id AND NOT i.is_other), '{}'),
			COALESCE((SELECT array_agg(i.alarm_code_id ORDER BY i.position) FROM dm.maintenance_order_issues i WHERE i.order_id = o.id AND i.is_other), '{}'),
			COALESCE((SELECT array_agg(u.username ORDER BY u.username) FROM dm.maintenance_order_technicians t JOIN core.users u ON u.id = t.user_id
				WHERE t.order_id = o.id AND NOT u.is_deleted), '{}'),
			COALESCE((SELECT sum(EXTRACT(EPOCH FROM (COALESCE(w.ended_at, now()) - w.started_at))) FROM dm.work_intervals w WHERE w.order_id = o.id), 0)::float8,
			(SELECT count(*) FROM dm.work_updates wu WHERE wu.order_id = o.id),
			COALESCE((SELECT u.username FROM dm.maintenance_order_events ev JOIN core.users u ON u.id = ev.created_by
				WHERE ev.order_id = o.id AND ev.status = 'STARTED' ORDER BY ev.created_at DESC LIMIT 1), '')
		FROM dm.maintenance_orders o JOIN asset.equipment e ON e.id = o.asset_id
		WHERE e.alias = ANY($1) AND o.status_id IN (1, 3, 4, 5, 7) AND o.planned_start < now() + interval '1 day'
		ORDER BY o.planned_start LIMIT 200`, aliases)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*botOrder
	for rows.Next() {
		o := &botOrder{}
		if err := rows.Scan(&o.ID, &o.Name, &o.Alias, &o.Status, &o.Type, &o.Severity, &o.Priority, &o.Desc, &o.PStart, &o.PEnd, &o.FromSystem,
			&o.LastEvent, &o.Codes, &o.CodeIDs, &o.Other, &o.Techs, &o.WorkedSec, &o.Updates, &o.Starter); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (b *Bot) work() error {
	orders, err := b.load()
	if err != nil {
		return err
	}
	b.snap = nil
	now := time.Now()
	idle := func(o *botOrder, d time.Duration) bool { return now.Sub(o.LastEvent) >= d }
	// A technician works on one job at a time.
	busy := map[string]bool{}
	for _, o := range orders {
		if o.Status == 4 && o.Starter != "" {
			busy[o.Starter] = true
		}
	}
	worker := func(o *botOrder) string {
		if o.Starter != "" && contains(o.Techs, o.Starter) {
			return o.Starter
		}
		if len(o.Techs) > 0 {
			return o.Techs[0]
		}
		return ""
	}
	for _, o := range orders {
		a := b.plant.ByAlias[o.Alias]
		if a == nil {
			continue
		}
		var err error
		switch o.Status {
		case 1: // requested -> supervisor approves and assigns technicians
			if idle(o, b.approve) {
				err = b.approveOrder(o, a)
			}
		case 3: // open -> a free technician starts when due
			if !now.After(o.PStart) || !idle(o, 5*time.Minute) {
				continue
			}
			stop, serr := b.mayStop(o.Alias)
			if serr != nil {
				return serr
			}
			if !stop {
				continue // stays open until a standby unit is back
			}
			for _, t := range o.Techs {
				if busy[t] {
					continue
				}
				busy[t] = true
				err = b.as(t, "maintenance_order_work_report/start_work_report", map[string]any{"maintenance_id": o.ID, "is_status_change": true})
				b.logAct(err, "work started", o, t)
				if err == nil {
					b.snap.ByAlias[o.Alias].Maint = true // the live plant stops it at its next syncForced
				}
				break
			}
		case 7: // paused -> resume when the technician is free
			if w := worker(o); w != "" && idle(o, 20*time.Minute) && !busy[w] {
				busy[w] = true
				err = b.as(w, "maintenance_order_work_report/resume_work_report", map[string]any{"maintenance_id": o.ID})
				b.logAct(err, "work resumed", o, w)
			}
		case 4: // in progress -> update, then done
			w := worker(o)
			if w == "" || !idle(o, 3*time.Minute) {
				continue
			}
			target := b.workMinutes(a, o.Codes)
			code := ""
			if len(o.Codes) > 0 {
				code = o.Codes[0]
			}
			if o.Updates == 0 && o.WorkedSec > target*60*0.4 {
				form := map[string]any{"maintenance_id": o.ID, "note": noteFor(code, 0)}
				if len(o.CodeIDs) > 0 {
					form["issue_ids"] = fmt.Sprint(o.CodeIDs[0])
				}
				err = b.as(w, "maintenance_order_work_report/add_work_update", form)
				b.logAct(err, "work update", o, w)
			} else if o.WorkedSec >= target*60 {
				err = b.as(w, "maintenance_order_work_report/set_work_report_status_to_done", map[string]any{"maintenance_id": o.ID,
					"issues": toJSON(b.validities(o))})
				b.logAct(err, "work completed", o, w)
			}
		case 5: // done -> supervisor closes
			if idle(o, 10*time.Minute) {
				sup := supervisorFor(buildingOf(a.Spec.Location))
				err = b.as(sup, "maintenance_order_work_report/close_work_report", map[string]any{"maintenance_id": o.ID, "issues": "[]"})
				b.logAct(err, "order closed", o, sup)
			}
		}
		if err != nil {
			slog.Warn("workforce action failed", "order", o.Name, "err", err)
		}
	}
	return nil
}

// mayStop reports whether the equipment may stop for maintenance now: a
// chiller, its dedicated pumps or a tower only while the other units still
// cover the duty chillers (Plant.canStop). It reads a copy of the live plant
// taken once per cycle (the plant is only safe to read on the simulation loop).
func (b *Bot) mayStop(alias string) (bool, error) {
	if b.snap == nil {
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		defer cancel()
		p, err := b.live.Snapshot(ctx, 0)
		if err != nil {
			return false, fmt.Errorf("plant snapshot: %w", err)
		}
		b.snap = p
	}
	return b.snap.canStop(b.snap.ByAlias[alias]), nil
}

func (b *Bot) as(user, script string, form map[string]any) error {
	_, err := b.c.Call(b.ctx, user, script, form)
	return err
}

func (b *Bot) logAct(err error, what string, o *botOrder, user string) {
	if err == nil {
		slog.Info("workforce: "+what, "order", o.Name, "equipment", o.Alias, "by", user)
	}
}

// workMinutes is the simulated work duration (instructions compressed 6x).
func (b *Bot) workMinutes(a *Asset, codes []string) float64 {
	total := 0
	for _, in := range instructions {
		if contains(codes, in.Code) && contains(instructionModels(in.Code), a.Kind) {
			total += in.Minutes
		}
	}
	m := float64(total) / 6
	if m < 8 {
		m = 8
	}
	if m > 40 {
		m = 40
	}
	return m
}

func (b *Bot) validities(o *botOrder) []map[string]any {
	var out []map[string]any
	for i, id := range o.CodeIDs {
		code := o.Codes[i]
		for _, typ := range []int{1, 2} {
			has := false
			for _, in := range instructions {
				if in.Code == code && in.Type == typ {
					has = true
				}
			}
			if has {
				out = append(out, map[string]any{"alarm_code_id": id, "instruction_type_id": typ, "is_valid": true})
			}
		}
	}
	return out
}

func (b *Bot) approveOrder(o *botOrder, a *Asset) error {
	building := buildingOf(a.Spec.Location)
	sup := supervisorFor(building)
	techs := o.Techs
	if len(techs) == 0 {
		cands := techniciansFor(building, a.Model.Type)
		loads := map[string]int{}
		rows, err := b.db.Query(b.ctx, `SELECT u.username, count(*) FROM dm.maintenance_order_technicians t JOIN core.users u ON u.id = t.user_id
			JOIN dm.maintenance_orders o ON o.id = t.order_id WHERE o.status_id IN (3, 4, 7) GROUP BY u.username`)
		if err == nil {
			for rows.Next() {
				var u string
				var n int
				if rows.Scan(&u, &n) == nil {
					loads[u] = n
				}
			}
			rows.Close()
		}
		sort.SliceStable(cands, func(i, j int) bool { return loads[cands[i]] < loads[cands[j]] })
		if len(cands) > 0 {
			techs = cands[:1]
			if o.Severity <= 2 && len(cands) > 1 {
				techs = cands[:2]
			}
		}
	}
	if len(techs) == 0 {
		return fmt.Errorf("no technician for %s", o.Alias)
	}
	prio := 2
	if o.Priority != nil {
		prio = *o.Priority
	} else if o.Severity <= 2 {
		prio = 1
	}
	start := time.Now().Add(time.Duration(5+b.rng.IntN(10)) * time.Minute)
	if o.PStart.After(start) {
		start = o.PStart
	}
	dur := time.Duration(b.workMinutes(a, o.Codes)*1.5) * time.Minute
	// Automatic (running hour) preventive orders get the model checklist.
	if len(o.CodeIDs) == 0 && o.Type == 2 && a.Model.PMCode != "" {
		if id := codeID(b.ctx, b.db, a.Model.PMCode); id != 0 {
			o.CodeIDs, o.Codes = []int64{id}, []string{a.Model.PMCode}
		}
	}
	codes := make([]string, len(o.CodeIDs))
	for i, id := range o.CodeIDs {
		codes[i] = fmt.Sprint(id)
	}
	others := make([]string, len(o.Other))
	for i, id := range o.Other {
		others[i] = fmt.Sprint(id)
	}
	form := map[string]any{"submit_type": "APPROVED", "id": o.ID, "asset_id": b.equipmentID(o.Alias), "maintenance_type_id": o.Type,
		"priority_id": prio, "severity_id": o.Severity, "description": o.Desc, "planned_start": b.iso(start), "planned_end": b.iso(start.Add(dur)),
		"instructions": strings.Join(codes, ","), "other_alarm_codes": strings.Join(others, ","), "technicians": strings.Join(techs, ","),
		"timezone_offset": b.tzOffset}
	err := b.as(sup, "maintenance_order/submit", form)
	if err == nil {
		slog.Info("workforce: order approved", "order", o.Name, "equipment", o.Alias, "by", sup, "technicians", techs)
	}
	return err
}
