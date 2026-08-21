package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/notify"
)

// Hooks lets the engine raise maintenance orders without importing the
// maintenance package.
type Hooks interface {
	CreateEarlyWarning(ctx context.Context, ew EarlyWarning) error
}

// Options configure the engine.
type Options struct {
	EvalInterval     time.Duration
	LogInterval      time.Duration
	OfflineTimeout   time.Duration
	ScriptTimeout    time.Duration
	LicenseEnforced  bool
	EarlyWarningCool time.Duration
}

// Engine evaluates all equipment periodically.
type Engine struct {
	db     *db.DB
	hub    *datahub.Hub
	notify *notify.Service
	opts   Options
	eval   Evaluator

	hooksMu sync.RWMutex
	hooks   Hooks

	snap    atomic.Pointer[Snapshot]
	dirty   atomic.Bool
	reload  chan struct{}
	stateMu sync.Mutex
	states  map[int64]*eqState
	spc     *spcTracker
	lastLog time.Time

	ewMu      sync.Mutex
	ewRecent  map[string]time.Time
	lastError map[int64]string

	// External FDD alarms (analytics.faults), see external.go.
	extMu sync.Mutex
	ext   externalState
}

type openEvent struct {
	id       int64
	severity string
	start    time.Time
}

type eqState struct {
	status      int
	statusSince time.Time
	condition   string
	condSince   time.Time
	events      map[string]*openEvent
	lastResult  *Result
}

// New creates the engine.
func New(d *db.DB, hub *datahub.Hub, n *notify.Service, o Options) *Engine {
	if o.EvalInterval <= 0 {
		o.EvalInterval = 10 * time.Second
	}
	if o.LogInterval <= 0 {
		o.LogInterval = time.Minute
	}
	if o.OfflineTimeout <= 0 {
		o.OfflineTimeout = 5 * time.Minute
	}
	e := &Engine{
		db: d, hub: hub, notify: n, opts: o,
		eval:      Evaluator{Timeout: o.ScriptTimeout},
		reload:    make(chan struct{}, 1),
		states:    map[int64]*eqState{},
		spc:       newSPCTracker(),
		ewRecent:  map[string]time.Time{},
		lastError: map[int64]string{},
	}
	e.dirty.Store(true)
	return e
}

// SetHooks installs the maintenance hooks.
func (e *Engine) SetHooks(h Hooks) {
	e.hooksMu.Lock()
	e.hooks = h
	e.hooksMu.Unlock()
}

// Invalidate requests a reload of the definitions on the next cycle.
func (e *Engine) Invalidate() {
	e.dirty.Store(true)
	select {
	case e.reload <- struct{}{}:
	default:
	}
}

// Snapshot returns the current definitions (loading them if needed).
func (e *Engine) Snapshot(ctx context.Context) (*Snapshot, error) {
	if s := e.snap.Load(); s != nil && !e.dirty.Load() {
		return s, nil
	}
	return e.loadSnapshot(ctx)
}

func (e *Engine) loadSnapshot(ctx context.Context) (*Snapshot, error) {
	e.dirty.Store(false)
	s, err := LoadSnapshot(ctx, e.db, e.opts.LicenseEnforced)
	if err != nil {
		e.dirty.Store(true)
		return nil, err
	}
	e.snap.Store(s)
	return s, nil
}

// Run evaluates equipment until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	if err := e.recover(ctx); err != nil {
		slog.Error("engine recovery", "err", err)
	}
	t := time.NewTicker(e.opts.EvalInterval)
	defer t.Stop()
	periodic := time.NewTicker(2 * time.Minute)
	defer periodic.Stop()
	for {
		e.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.reload:
		case <-periodic.C:
			e.dirty.Store(true)
		}
	}
}

func (e *Engine) cycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("engine cycle panic", "panic", r)
		}
	}()
	snap, err := e.Snapshot(ctx)
	if err != nil {
		slog.Error("engine load definitions", "err", err)
		return
	}
	now := time.Now().UTC()
	e.refreshExternal(ctx, now)
	ids := make([]int64, 0, len(snap.Equipment))
	for id := range snap.Equipment {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		eq := snap.Equipment[id]
		if err := e.evaluateEquipment(ctx, snap, eq, now); err != nil {
			slog.Error("evaluate equipment", "equipment", eq.Alias, "err", err)
		}
	}
	e.dropRemovedStates(snap)
	if now.Sub(e.lastLog) >= e.opts.LogInterval {
		e.lastLog = now
		if err := e.logValues(ctx, snap, now.Truncate(time.Second)); err != nil {
			slog.Error("log parameter values", "err", err)
		}
	}
	_, _ = e.db.Exec(ctx, `INSERT INTO core.settings (key, value, updated_at) VALUES ('engine_heartbeat', to_jsonb($1::text), now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, now.Format(time.RFC3339Nano))
}

func (e *Engine) dropRemovedStates(snap *Snapshot) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	for id := range e.states {
		if _, ok := snap.Equipment[id]; !ok {
			delete(e.states, id)
		}
	}
}

// recover loads open intervals and closes those left dangling by an outage.
func (e *Engine) recover(ctx context.Context) error {
	var hb *string
	_ = e.db.QueryRow(ctx, `SELECT value #>> '{}' FROM core.settings WHERE key = 'engine_heartbeat'`).Scan(&hb)
	if hb != nil {
		last, err := time.Parse(time.RFC3339Nano, *hb)
		if err == nil && time.Since(last) > 3*e.opts.EvalInterval+time.Minute {
			slog.Warn("engine was down, closing open intervals", "last_heartbeat", last)
			for _, q := range []string{
				`UPDATE ts.asset_status SET end_ts = GREATEST($1, start_ts) WHERE end_ts IS NULL`,
				`UPDATE ts.asset_condition SET end_ts = GREATEST($1, start_ts) WHERE end_ts IS NULL`,
				`UPDATE ts.alarm_events SET end_ts = GREATEST($1, start_ts) WHERE end_ts IS NULL`,
			} {
				if _, err := e.db.Exec(ctx, q, last); err != nil {
					return err
				}
			}
		}
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	rows, err := e.db.Query(ctx, `SELECT asset_id, status_id, start_ts FROM ts.asset_status WHERE end_ts IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var st int
		var since time.Time
		if err := rows.Scan(&id, &st, &since); err != nil {
			rows.Close()
			return err
		}
		s := e.stateFor(id)
		s.status, s.statusSince = st, since
	}
	rows.Close()
	rows, err = e.db.Query(ctx, `SELECT asset_id, condition, start_ts FROM ts.asset_condition WHERE end_ts IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var c string
		var since time.Time
		if err := rows.Scan(&id, &c, &since); err != nil {
			rows.Close()
			return err
		}
		s := e.stateFor(id)
		s.condition, s.condSince = c, since
	}
	rows.Close()
	rows, err = e.db.Query(ctx, `SELECT id, asset_id, source_key, severity, start_ts FROM ts.alarm_events WHERE end_ts IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ev openEvent
		var asset int64
		var key string
		if err := rows.Scan(&ev.id, &asset, &key, &ev.severity, &ev.start); err != nil {
			return err
		}
		e.stateFor(asset).events[key] = &ev
	}
	return rows.Err()
}

// stateFor returns the state of an equipment (caller holds stateMu).
func (e *Engine) stateFor(id int64) *eqState {
	s := e.states[id]
	if s == nil {
		s = &eqState{events: map[string]*openEvent{}}
		e.states[id] = s
	}
	return s
}

// CurrentValues returns the typed values of an equipment keyed by alias.
func (e *Engine) CurrentValues(eq *Equipment) (map[string]Value, map[string]datahub.VTQC) {
	values := map[string]Value{}
	raw := map[string]datahub.VTQC{}
	for _, p := range eq.Params {
		if !p.Mapped() {
			values[p.Alias] = Value{V: nil, Q: false}
			continue
		}
		v, ok := e.hub.Value(p.GatewayAlias, p.Tag)
		if !ok {
			values[p.Alias] = Value{V: nil, Q: false}
			continue
		}
		raw[p.Alias] = v
		values[p.Alias] = ToValue(v.V, v.T, v.Q == datahub.QualityGood && v.T > 0, p.DataType, p.Scale)
	}
	return values, raw
}

// isOffline reports whether no mapped parameter delivered good data recently.
func (e *Engine) isOffline(eq *Equipment, now time.Time) bool {
	freshest := int64(0)
	mapped := false
	for _, p := range eq.Params {
		if !p.Mapped() {
			continue
		}
		mapped = true
		v, ok := e.hub.Value(p.GatewayAlias, p.Tag)
		if !ok || v.Q != datahub.QualityGood {
			continue
		}
		if v.R > freshest {
			freshest = v.R
		}
	}
	if !mapped || freshest == 0 {
		return true
	}
	return now.Sub(time.UnixMilli(freshest)) > e.opts.OfflineTimeout
}

// LatestResult returns the last evaluation result of an equipment.
func (e *Engine) LatestResult(id int64) *Result {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if s := e.states[id]; s != nil {
		return s.lastResult
	}
	return nil
}

func (e *Engine) evaluateEquipment(ctx context.Context, snap *Snapshot, eq *Equipment, now time.Time) error {
	var res Result
	if !eq.LicenseOK {
		res = Result{Status: StatusOffline}
	} else {
		values, _ := e.CurrentValues(eq)
		offline := e.isOffline(eq, now)
		res = e.eval.Evaluate(snap.Models[eq.ModelID], eq.Params, values, eq.ForcedState, offline)
	}
	appendExternal(&res, e.externalAlarms(eq.ID))
	if len(res.Errors) > 0 {
		msg := strings.Join(res.Errors, "; ")
		e.ewMu.Lock()
		if e.lastError[eq.ID] != msg {
			e.lastError[eq.ID] = msg
			slog.Warn("script errors", "equipment", eq.Alias, "errors", msg)
		}
		e.ewMu.Unlock()
	}

	e.stateMu.Lock()
	st := e.stateFor(eq.ID)
	prevStatus := st.status
	st.lastResult = &res
	e.stateMu.Unlock()

	var notifications []notify.Notification
	assetID := eq.ID
	locID := eq.LocationID

	// Status transitions.
	if res.Status != prevStatus {
		err := e.db.Tx(ctx, func(tx dbTx) error {
			if _, err := tx.Exec(ctx, `UPDATE ts.asset_status SET end_ts = $2 WHERE asset_id = $1 AND end_ts IS NULL`, eq.ID, now); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ts.asset_status (asset_id, status_id, start_ts) VALUES ($1, $2, $3)`, eq.ID, res.Status, now)
			return err
		})
		if err != nil {
			return err
		}
		e.stateMu.Lock()
		st.status, st.statusSince = res.Status, now
		e.stateMu.Unlock()
		if prevStatus != 0 {
			notifications = append(notifications, notify.Notification{
				Category: statusCategory(res.Status), Message: notify.StatusMessage(eq.Name, StatusName(res.Status)),
				AssetID: &assetID, LocationID: &locID, Timestamp: now,
				AdditionalData: map[string]any{"status": StatusName(res.Status), "previous_status": StatusName(prevStatus)},
			})
		}
	}

	// Condition transitions.
	cond := res.Condition()
	e.stateMu.Lock()
	prevCond := st.condition
	e.stateMu.Unlock()
	if cond != prevCond {
		err := e.db.Tx(ctx, func(tx dbTx) error {
			if _, err := tx.Exec(ctx, `UPDATE ts.asset_condition SET end_ts = $2 WHERE asset_id = $1 AND end_ts IS NULL`, eq.ID, now); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ts.asset_condition (asset_id, condition, start_ts) VALUES ($1, $2, $3)`, eq.ID, cond, now)
			return err
		})
		if err != nil {
			return err
		}
		e.stateMu.Lock()
		st.condition, st.condSince = cond, now
		e.stateMu.Unlock()
	}

	// Alarm events.
	active := map[string]ActiveAlarm{}
	for _, a := range res.Alarms {
		active[a.Key] = a
	}
	for _, a := range res.Abnormal {
		active[a.Key] = a
	}
	e.stateMu.Lock()
	open := make(map[string]*openEvent, len(st.events))
	for k, v := range st.events {
		open[k] = v
	}
	e.stateMu.Unlock()

	for key, ev := range open {
		a, still := active[key]
		if still && a.Severity == ev.severity {
			continue
		}
		if _, err := e.db.Exec(ctx, `UPDATE ts.alarm_events SET end_ts = $2 WHERE id = $1`, ev.id, now); err != nil {
			return err
		}
		e.stateMu.Lock()
		delete(st.events, key)
		e.stateMu.Unlock()
	}
	keys := make([]string, 0, len(active))
	for k := range active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		a := active[key]
		e.stateMu.Lock()
		_, exists := st.events[key]
		e.stateMu.Unlock()
		if exists {
			continue
		}
		var codeID *int64
		var modelAlarmID, epID *int64
		if a.Type == "ALARM" {
			if id, ok := snap.AlarmCodes.Resolve(a.Name); ok {
				codeID = &id
			}
			if strings.HasPrefix(a.Key, "alarm:") {
				v := a.DefID
				modelAlarmID = &v
			}
		} else {
			v := a.EPID
			epID = &v
		}
		var id int64
		err := e.db.QueryRow(ctx, `INSERT INTO ts.alarm_events (asset_id, alarm_type, source_key, model_alarm_id, equipment_parameter_id,
			alarm_code_id, name, severity, message, start_ts) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (asset_id, source_key) WHERE end_ts IS NULL DO UPDATE SET severity = EXCLUDED.severity
			RETURNING id`, eq.ID, a.Type, key, modelAlarmID, epID, codeID, a.Name, a.Severity, a.Message, now).Scan(&id)
		if err != nil {
			return err
		}
		e.stateMu.Lock()
		st.events[key] = &openEvent{id: id, severity: a.Severity, start: now}
		e.stateMu.Unlock()
		if a.Type == "ALARM" {
			cat := "ALARM_WARNING"
			if a.Severity == "CRITICAL" {
				cat = "ALARM_CRITICAL"
			}
			notifications = append(notifications, notify.Notification{
				Category: cat, Message: notify.AlarmMessage(a.Severity, a.Name, eq.Name), AssetID: &assetID, LocationID: &locID, Timestamp: now,
				AdditionalData: map[string]any{"alarm_name": a.Name, "alarm_message": a.Message, "event_id": id},
			})
		} else {
			cat := "PARAMETER_WARNING"
			if a.Severity == "CRITICAL" {
				cat = "PARAMETER_CRITICAL"
			}
			notifications = append(notifications, notify.Notification{
				Category: cat, Message: fmt.Sprintf("%s: Abnormal Parameter is %s %s", eq.Name, a.Name, strings.TrimPrefix(a.Message, a.Name+" is ")),
				AssetID: &assetID, LocationID: &locID, Timestamp: now,
				AdditionalData: map[string]any{"parameter_id": a.EPID, "condition": a.Severity},
			})
		}
	}

	for _, n := range notifications {
		_ = e.notify.Send(ctx, n)
	}

	if m := snap.Models[eq.ModelID]; m != nil && len(m.Relations) > 0 && res.Status != StatusOffline {
		e.diagnose(ctx, snap, eq, m, &res, now)
	}
	return nil
}

func statusCategory(status int) string {
	switch status {
	case StatusRunning:
		return "STATUS_RUNNING"
	case StatusIdle:
		return "STATUS_IDLE"
	case StatusUnplannedDown:
		return "STATUS_DOWN"
	case StatusPlannedDown:
		return "STATUS_PLANNED_DOWN"
	default:
		return "STATUS_OFFLINE"
	}
}

// logValues stores the current value of every logged parameter.
func (e *Engine) logValues(ctx context.Context, snap *Snapshot, ts time.Time) error {
	var ids []int64
	var tss []time.Time
	var nums []*float64
	var texts []*string
	type spcPoint struct {
		eq  *Equipment
		p   *ParamDef
		val float64
	}
	var spcPoints []spcPoint
	for _, eq := range snap.Equipment {
		if !eq.LicenseOK {
			continue
		}
		for _, p := range eq.Params {
			if !p.IsLogged || !p.Mapped() {
				continue
			}
			raw, ok := e.hub.Value(p.GatewayAlias, p.Tag)
			if !ok || raw.Q != datahub.QualityGood || raw.T <= 0 {
				continue
			}
			if ts.Sub(time.UnixMilli(raw.R)) > e.opts.OfflineTimeout {
				continue
			}
			v := ToValue(raw.V, raw.T, true, p.DataType, p.Scale)
			if !v.Q {
				continue
			}
			ids = append(ids, p.EPID)
			tss = append(tss, ts)
			if n, ok := v.Num(); ok && p.DataType != "string" {
				nv := n
				nums = append(nums, &nv)
				texts = append(texts, nil)
				if p.DataType == "number" {
					spcPoints = append(spcPoints, spcPoint{eq: eq, p: p, val: n})
				}
			} else {
				s := raw.V
				nums = append(nums, nil)
				texts = append(texts, &s)
			}
		}
	}
	if len(ids) > 0 {
		const chunk = 5000
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			_, err := e.db.Exec(ctx, `INSERT INTO ts.parameter_values (equipment_parameter_id, ts, value, value_text)
				SELECT * FROM unnest($1::bigint[], $2::timestamptz[], $3::float8[], $4::text[])
				ON CONFLICT DO NOTHING`, ids[i:end], tss[i:end], nums[i:end], texts[i:end])
			if err != nil {
				return err
			}
		}
	}
	for _, sp := range spcPoints {
		e.checkSPC(ctx, sp.eq, sp.p, sp.val, ts)
	}
	return nil
}

// TestModel evaluates a model against supplied values (model manager "Test").
func (e *Engine) TestModel(ctx context.Context, modelID int64, values map[string]Value) (*Result, *ModelDef, error) {
	m, err := LoadModel(ctx, e.db, modelID)
	if err != nil {
		return nil, nil, err
	}
	params, err := modelParams(ctx, e.db, modelID)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range params {
		if _, ok := values[p.Alias]; !ok {
			values[p.Alias] = Value{V: nil, Q: false}
		}
	}
	res := e.eval.Evaluate(m, nil, values, nil, false)
	return &res, m, nil
}

func modelParams(ctx context.Context, q db.Querier, modelID int64) ([]*ParamDef, error) {
	rows, err := q.Query(ctx, `SELECT id, alias, name, unit, data_type, scale FROM asset.model_parameters WHERE model_id = $1`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ParamDef
	for rows.Next() {
		p := &ParamDef{}
		if err := rows.Scan(&p.MPID, &p.Alias, &p.Name, &p.Unit, &p.DataType, &p.Scale); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Diagnostics returns a JSON-friendly summary used by the health endpoint.
func (e *Engine) Diagnostics() map[string]any {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return map[string]any{"equipment": len(e.states)}
}
