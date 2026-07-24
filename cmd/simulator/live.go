package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SimState is persisted in core.settings so the simulation continues where
// it left off (also across the history/live boundary).
type SimState struct {
	SavedAt time.Time             `json:"saved_at"`
	Assets  map[string]AssetState `json:"assets"`
}

type AssetState struct {
	Faults     []Fault   `json:"faults"`
	FilterDays float64   `json:"filter_days"`
	Trip       bool      `json:"trip"`
	TripUntil  time.Time `json:"trip_until"`
	// Controls are the operator-written modes and set points.
	Controls map[string]float64 `json:"controls,omitempty"`
}

const stateKey = "demo_simulator_state"

func captureState(p *Plant) SimState {
	st := SimState{SavedAt: time.Now().UTC(), Assets: map[string]AssetState{}}
	for _, a := range p.Assets {
		as := AssetState{FilterDays: a.FilterDays, Trip: a.Trip, TripUntil: a.TripUntil}
		if len(a.Ctl) > 0 {
			as.Controls = map[string]float64{}
			for k, v := range a.Ctl {
				as.Controls[k] = v
			}
		}
		for _, f := range a.Faults {
			as.Faults = append(as.Faults, *f)
		}
		st.Assets[a.Spec.Alias] = as
	}
	return st
}

func restoreState(p *Plant, st SimState) {
	for alias, as := range st.Assets {
		a := p.ByAlias[alias]
		if a == nil {
			continue
		}
		a.FilterDays = as.FilterDays
		a.Trip, a.TripUntil = as.Trip, as.TripUntil
		if a.Trip && a.TripUntil.IsZero() {
			a.TripUntil = time.Now().Add(20 * time.Minute)
		}
		if a.Trip && len(a.Model.Trips) > 0 {
			ts := a.Model.Trips[0]
			a.TripSpec = &ts
		}
		a.Faults = map[string]*Fault{}
		for _, f := range as.Faults {
			f := f
			a.Faults[f.Kind] = &f
		}
		// controls still writable in the current model definition
		a.Ctl = map[string]float64{}
		for k, v := range as.Controls {
			if a.controlSpec(k) != nil {
				a.Ctl[k] = v
			}
		}
	}
}

// Command is an operator write received on a gateway's command topic
// ("<gateway identity>/cmd/<equipment alias>").
type Command struct {
	Gateway   string  `json:"-"`
	Device    string  `json:"-"`
	RequestID int64   `json:"request_id"`
	Parameter string  `json:"parameter"`
	Value     float64 `json:"value"`
	User      string  `json:"user"`
	// ExpiresAt (ms): the backend stops waiting for the read-back then; a
	// later command is dropped so it can't change the plant after a TIMEOUT.
	ExpiresAt int64 `json:"expires_at"`
}

// LiveOptions configure the live simulation.
type LiveOptions struct {
	MQTTURL     string
	Org, Prj    string
	Interval    time.Duration
	FaultEvery  time.Duration
	TripEvery   time.Duration
	OfflineEvry time.Duration
	MaxFaults   int
	RampMin     float64
	RampMax     float64
}

// Live streams simulated telemetry to the backend broker.
type Live struct {
	ctx     context.Context
	db      *pgxpool.Pool
	plant   *Plant
	opts    LiveOptions
	rng     *rand.Rand
	mu      sync.Mutex
	clients map[string]paho.Client
	nextF   time.Time
	nextT   time.Time
	nextO   time.Time
	lastChk time.Time
	seen    map[string]bool
	trips   chan *Asset   // tripped assets for the operator bot
	cmds    chan Command // operator writes, applied on the simulation loop
}

func newLive(ctx context.Context, db *pgxpool.Pool, plant *Plant, opts LiveOptions, seed uint64) *Live {
	l := &Live{ctx: ctx, db: db, plant: plant, opts: opts, rng: rand.New(rand.NewPCG(seed, seed^42)), clients: map[string]paho.Client{},
		trips: make(chan *Asset, 16), cmds: make(chan Command, 64), seen: map[string]bool{}}
	now := time.Now()
	l.nextF = now.Add(l.expo(opts.FaultEvery) / 2)
	l.nextT = now.Add(l.expo(opts.TripEvery))
	l.nextO = now.Add(l.expo(opts.OfflineEvry))
	l.lastChk = now.Add(-time.Minute)
	return l
}

func (l *Live) expo(mean time.Duration) time.Duration {
	return time.Duration(-math.Log(1-l.rng.Float64()) * float64(mean))
}

func (l *Live) identity(alias string) string { return l.opts.Org + "." + l.opts.Prj + "." + alias }

func (l *Live) connect(alias string) (paho.Client, error) {
	var pw string
	if err := l.db.QueryRow(l.ctx, `SELECT COALESCE(secrets->>'mqtt_password', '') FROM dh.gateways WHERE alias = $1`, alias).Scan(&pw); err != nil {
		return nil, fmt.Errorf("gateway %s: %w", alias, err)
	}
	id := l.identity(alias)
	opts := paho.NewClientOptions().AddBroker(l.opts.MQTTURL).SetClientID("sim-" + alias).SetUsername(id).SetPassword(pw).
		SetAutoReconnect(true).SetConnectTimeout(10 * time.Second).SetKeepAlive(30 * time.Second).SetOrderMatters(false).
		// (re)subscribe to the gateway's commands on every connection
		SetOnConnectHandler(func(c paho.Client) {
			topic := id + "/cmd/+"
			tok := c.Subscribe(topic, 1, l.onCommand(alias))
			if tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
				slog.Warn("mqtt subscribe failed", "topic", topic, "err", tok.Error())
			}
		})
	c := paho.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, fmt.Errorf("mqtt connect %s: timeout", alias)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect %s: %w", alias, err)
	}
	slog.Info("mqtt connected", "gateway", alias, "username", id)
	return c, nil
}

// onCommand queues the operator writes of a gateway for the simulation loop.
func (l *Live) onCommand(gateway string) paho.MessageHandler {
	return func(_ paho.Client, m paho.Message) {
		var cmd Command
		if err := json.Unmarshal(m.Payload(), &cmd); err != nil {
			slog.Warn("invalid command", "topic", m.Topic(), "err", err)
			return
		}
		cmd.Gateway = gateway
		if i := strings.LastIndex(m.Topic(), "/cmd/"); i >= 0 {
			cmd.Device = m.Topic()[i+len("/cmd/"):]
		}
		select {
		case l.cmds <- cmd:
		default:
			slog.Warn("command queue full, command dropped", "equipment", cmd.Device, "parameter", cmd.Parameter)
		}
	}
}

// applyCommand applies an operator write to the plant. The device ignores it
// while its communication is lost (the backend then times the request out).
func (l *Live) applyCommand(cmd Command) bool {
	a := l.plant.ByAlias[cmd.Device]
	switch {
	case a == nil || a.Spec.Gateway != cmd.Gateway:
		slog.Warn("command for unknown equipment", "gateway", cmd.Gateway, "equipment", cmd.Device)
		return false
	case a.Offline:
		slog.Info("command ignored: no communication", "equipment", cmd.Device, "parameter", cmd.Parameter)
		return false
	case cmd.ExpiresAt > 0 && time.Now().UnixMilli() > cmd.ExpiresAt:
		slog.Warn("command expired, dropped", "equipment", cmd.Device, "parameter", cmd.Parameter, "request", cmd.RequestID)
		return false
	}
	if err := a.applyControl(cmd.Parameter, cmd.Value); err != nil {
		slog.Warn("command rejected", "equipment", cmd.Device, "parameter", cmd.Parameter, "value", cmd.Value, "err", err,
			"request", cmd.RequestID, "user", cmd.User)
		return false
	}
	slog.Info("command applied", "equipment", cmd.Device, "parameter", cmd.Parameter, "value", cmd.Value,
		"request", cmd.RequestID, "user", cmd.User)
	return true
}

func (l *Live) ensureClients() {
	for _, g := range gateways {
		l.mu.Lock()
		c := l.clients[g.Alias]
		l.mu.Unlock()
		if c != nil && c.IsConnectionOpen() {
			continue
		}
		if c != nil {
			c.Disconnect(100)
		}
		nc, err := l.connect(g.Alias)
		if err != nil {
			slog.Warn("mqtt connect failed (retrying)", "err", err)
			continue
		}
		l.mu.Lock()
		l.clients[g.Alias] = nc
		l.mu.Unlock()
	}
}

// Run publishes until the context is cancelled.
func (l *Live) Run() error {
	l.ensureClients()
	tick := time.NewTicker(l.opts.Interval)
	defer tick.Stop()
	slowTick := time.NewTicker(15 * time.Second)
	defer slowTick.Stop()
	saveTick := time.NewTicker(time.Minute)
	defer saveTick.Stop()
	last := time.Now()
	l.syncForced()
	for {
		select {
		case <-l.ctx.Done():
			_ = putSetting(context.Background(), l.db, stateKey, captureState(l.plant))
			return nil
		case <-slowTick.C:
			l.ensureClients()
			l.syncForced()
			l.checkRepairs()
		case <-saveTick.C:
			if err := putSetting(l.ctx, l.db, stateKey, captureState(l.plant)); err != nil {
				slog.Warn("save state", "err", err)
			}
		case now := <-tick.C:
			dt := now.Sub(last).Seconds()
			last = now
			l.scenario(now, dt)
			values := l.plant.Step(now.UTC(), dt)
			l.publish(values)
		case cmd := <-l.cmds:
			if !l.applyCommand(cmd) {
				continue
			}
			// step and publish right away, so the change shows (and the
			// backend reads the new value back) without waiting for the tick
			now := time.Now()
			dt := now.Sub(last).Seconds()
			last = now
			l.scenario(now, dt)
			l.publish(l.plant.Step(now.UTC(), dt))
			if err := putSetting(l.ctx, l.db, stateKey, captureState(l.plant)); err != nil {
				slog.Warn("save state", "err", err)
			}
		}
	}
}

func (l *Live) publish(values map[*Asset]map[string]float64) {
	for _, a := range l.plant.Assets {
		if a.Offline {
			continue
		}
		l.mu.Lock()
		c := l.clients[a.Spec.Gateway]
		l.mu.Unlock()
		if c == nil || !c.IsConnectionOpen() {
			continue
		}
		v := values[a]
		payload := map[string]any{}
		for _, p := range a.Model.Params {
			if p.Bool {
				payload[p.Alias] = v[p.Alias] == 1
			} else {
				payload[p.Alias] = round(v[p.Alias], p.Scale)
			}
		}
		b, _ := json.Marshal(payload)
		c.Publish(l.identity(a.Spec.Gateway)+"/"+a.Spec.Alias, 0, false, b)
	}
}

func (l *Live) activeFaults() int {
	n := 0
	for _, a := range l.plant.Assets {
		n += len(a.Faults)
	}
	return n
}

// scenario evolves faults, trips and communication losses.
func (l *Live) scenario(now time.Time, dt float64) {
	for _, a := range l.plant.Assets {
		for _, f := range a.Faults {
			f.Sev = math.Min(maxSeverity, f.Sev+f.Rate*dt)
		}
		if a.Kind == "mau" {
			a.FilterDays += dt / 86400
		}
		if a.Offline && now.After(a.OffUntil) {
			a.Offline = false
			slog.Info("communication restored", "equipment", a.Spec.Alias)
		}
		if !a.SpikeUntil.IsZero() && now.After(a.SpikeUntil) {
			a.SpikeUntil = time.Time{}
			a.Trip = true
			a.TripUntil = now.Add(time.Duration(l.uniform(25, 45) * float64(time.Minute)))
			slog.Info("equipment tripped", "equipment", a.Spec.Alias, "reason", a.TripSpec.Reason)
			select {
			case l.trips <- a:
			default:
			}
		}
		if a.Trip && !a.TripUntil.IsZero() && now.After(a.TripUntil) {
			a.Trip = false
			a.TripUntil = time.Time{}
			slog.Info("trip reset by operator", "equipment", a.Spec.Alias)
		}
	}
	if now.After(l.nextF) {
		l.nextF = now.Add(l.expo(l.opts.FaultEvery))
		if l.activeFaults() < l.opts.MaxFaults {
			l.injectFault(now)
		}
	}
	if now.After(l.nextT) {
		l.nextT = now.Add(l.expo(l.opts.TripEvery))
		l.injectTrip(now)
	}
	if now.After(l.nextO) {
		l.nextO = now.Add(l.expo(l.opts.OfflineEvry))
		a := l.plant.Assets[l.rng.IntN(len(l.plant.Assets))]
		a.Offline, a.OffUntil = true, now.Add(time.Duration(l.uniform(7, 15)*float64(time.Minute)))
		slog.Info("communication lost", "equipment", a.Spec.Alias, "until", a.OffUntil.Format(time.Kitchen))
	}
}

func (l *Live) uniform(a, b float64) float64 { return a + l.rng.Float64()*(b-a) }

func (l *Live) injectFault(now time.Time) {
	for tries := 0; tries < 20; tries++ {
		a := l.plant.Assets[l.rng.IntN(len(l.plant.Assets))]
		if !a.Running() || len(a.Model.Faults) == 0 {
			continue
		}
		f := a.Model.Faults[l.rng.IntN(len(a.Model.Faults))]
		if a.Faults[f.Kind] != nil {
			continue
		}
		ramp := l.uniform(l.opts.RampMin, l.opts.RampMax) * 60
		a.Faults[f.Kind] = &Fault{Kind: f.Kind, Rate: 1.25 / ramp, Start: now}
		slog.Info("degradation started", "equipment", a.Spec.Alias, "fault", f.Label, "ramp_minutes", int(ramp/60))
		return
	}
}

func (l *Live) injectTrip(now time.Time) {
	for tries := 0; tries < 20; tries++ {
		a := l.plant.Assets[l.rng.IntN(len(l.plant.Assets))]
		if !a.Running() || a.Trip || len(a.Model.Trips) == 0 {
			continue
		}
		ts := a.Model.Trips[0]
		a.TripSpec = &ts
		a.SpikeUntil = now.Add(45 * time.Second)
		return
	}
}

// syncForced stops equipment that maintenance put in a forced status.
func (l *Live) syncForced() {
	rows, err := l.db.Query(l.ctx, `SELECT alias, forced_status_id IS NOT NULL FROM asset.equipment`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var alias string
		var forced bool
		if rows.Scan(&alias, &forced) != nil {
			continue
		}
		if a := l.plant.ByAlias[alias]; a != nil && a.Maint != forced {
			a.Maint = forced
			if forced {
				slog.Info("equipment stopped for maintenance", "equipment", alias)
			} else {
				slog.Info("equipment released after maintenance", "equipment", alias)
			}
		}
	}
}

// checkRepairs clears faults fixed by completed maintenance orders.
func (l *Live) checkRepairs() {
	since := l.lastChk
	l.lastChk = time.Now()
	rows, err := l.db.Query(l.ctx, `SELECT e.alias, o.name, o.maintenance_type_id, COALESCE(array_agg(c.code) FILTER (WHERE c.code IS NOT NULL), '{}')
		FROM dm.maintenance_orders o JOIN asset.equipment e ON e.id = o.asset_id
		LEFT JOIN dm.maintenance_order_issues i ON i.order_id = o.id LEFT JOIN dm.alarm_codes c ON c.id = i.alarm_code_id
		WHERE o.completed_at > $1 AND o.completed_at <= now() GROUP BY e.alias, o.name, o.maintenance_type_id`, since.Add(-30*time.Second))
	if err != nil {
		slog.Warn("check repairs", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var alias, name string
		var typ int
		var codes []string
		if rows.Scan(&alias, &name, &typ, &codes) != nil {
			continue
		}
		a := l.plant.ByAlias[alias]
		if a == nil || l.seen[name] {
			continue
		}
		l.seen[name] = true
		fixed := repair(a, codes, typ == 2)
		if a.Trip {
			a.Trip, a.TripUntil = false, time.Time{}
		}
		slog.Info("maintenance completed", "order", name, "equipment", alias, "repaired", fixed)
	}
}
