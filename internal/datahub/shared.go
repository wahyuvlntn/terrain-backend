package datahub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"afdd/internal/db"
	"afdd/internal/events"
	"afdd/internal/httpx"
)

// Mode is where the hub's values come from.
type Mode int

const (
	// ModeAll runs the collectors in this process and keeps the values in
	// memory (the all-in-one `serve`, the local deployment).
	ModeAll Mode = iota
	// ModeWorker runs the collectors and writes the latest values and
	// gateway states to the database (dh.live_values, dh.gateway_state); REST
	// push values written there by the API replicas are read back.
	ModeWorker
	// ModeReplica runs no collectors: an API replica reads the shared values,
	// persists REST pushes it receives and signals configuration changes to
	// the worker (core.runtime_events).
	ModeReplica
)

// SetMode selects the mode and the shared-state intervals; call before Start.
func (h *Hub) SetMode(m Mode, flush, refresh time.Duration) {
	h.mode = m
	h.flushEvery, h.refreshEvery = flush, refresh
	if h.flushEvery <= 0 {
		h.flushEvery = time.Second
	}
	if h.refreshEvery <= 0 {
		h.refreshEvery = time.Second
	}
}

// Mode returns the hub's mode.
func (h *Hub) Mode() Mode { return h.mode }

// errOnWorker answers on-demand device access in an API replica.
var errOnWorker = &httpx.Error{Status: http.StatusServiceUnavailable,
	Msg: "Reading and browsing devices run on the processing worker, not on this API instance"}

// markDirty records tag values the worker must write.
func (h *Hub) markDirty(rt *runtime, names []string) {
	if h.mode != ModeWorker || rt.gw.Type == RESTServer || len(names) == 0 {
		return
	}
	key := strings.ToLower(rt.gw.Alias)
	h.dirtyMu.Lock()
	set := h.dirty[key]
	if set == nil {
		set = map[string]struct{}{}
		h.dirty[key] = set
	}
	for _, n := range names {
		set[n] = struct{}{}
	}
	h.dirtyMu.Unlock()
}

// flushLoop writes the changed values and the gateway states (worker).
func (h *Hub) flushLoop(ctx context.Context) {
	written := map[string]Status{}
	beat := time.Now()
	t := time.NewTicker(h.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.dirtyMu.Lock()
		dirty := h.dirty
		h.dirty = map[string]map[string]struct{}{}
		h.dirtyMu.Unlock()
		var gws, tags, vals []string
		var ts, recv []int64
		var qs []int16
		var cs []int32
		for key, set := range dirty {
			rt := h.runtime(key)
			if rt == nil {
				continue
			}
			rt.mu.RLock()
			for name := range set {
				v, ok := rt.byName[name]
				if !ok {
					continue
				}
				gws, tags, vals = append(gws, key), append(tags, name), append(vals, v.V)
				ts, recv, qs, cs = append(ts, v.T), append(recv, v.R), append(qs, int16(v.Q)), append(cs, int32(v.C))
			}
			rt.mu.RUnlock()
		}
		if len(tags) > 0 {
			if _, err := h.db.Exec(ctx, `INSERT INTO dh.live_values (gateway, tag, value, ts, quality, changes, recv)
				SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::smallint[], $6::int[], $7::bigint[])
				ON CONFLICT (gateway, tag) DO UPDATE SET value = EXCLUDED.value, ts = EXCLUDED.ts, quality = EXCLUDED.quality,
				changes = EXCLUDED.changes, recv = EXCLUDED.recv`, gws, tags, vals, ts, qs, cs, recv); err != nil {
				slog.Warn("write live values", "err", err)
				h.requeue(dirty)
			}
		}
		// gateway states: on change, and every 30 s as a heartbeat
		heartbeat := time.Since(beat) > 30*time.Second
		if heartbeat {
			beat = time.Now()
		}
		h.mu.RLock()
		rts := make([]*runtime, 0, len(h.gateways))
		for _, rt := range h.gateways {
			rts = append(rts, rt)
		}
		h.mu.RUnlock()
		for _, rt := range rts {
			if rt.gw.Type == RESTServer {
				continue // the API replicas report REST pushes
			}
			key := strings.ToLower(rt.gw.Alias)
			st := h.Status(rt.gw.Alias)
			prev, ok := written[key]
			if ok && !heartbeat && prev.Connected == st.Connected && prev.Timestamp == st.Timestamp && strEq(prev.Error, st.Error) {
				continue
			}
			if err := writeState(ctx, h.db, key, st.Connected, st.Error, st.Timestamp); err != nil {
				slog.Warn("write gateway state", "gateway", key, "err", err)
				continue
			}
			written[key] = st
		}
	}
}

func (h *Hub) requeue(dirty map[string]map[string]struct{}) {
	h.dirtyMu.Lock()
	defer h.dirtyMu.Unlock()
	for key, set := range dirty {
		cur := h.dirty[key]
		if cur == nil {
			cur = map[string]struct{}{}
			h.dirty[key] = cur
		}
		for n := range set {
			cur[n] = struct{}{}
		}
	}
}

func strEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func writeState(ctx context.Context, q db.Querier, key string, connected bool, errMsg *string, ts int64) error {
	_, err := q.Exec(ctx, `INSERT INTO dh.gateway_state (gateway, connected, error, status_ts, updated_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (gateway) DO UPDATE SET connected = EXCLUDED.connected, error = EXCLUDED.error, status_ts = EXCLUDED.status_ts,
		updated_at = now()`, key, connected, errMsg, ts)
	return err
}

// pullShared reads REST push values written by the API replicas (worker).
func (h *Hub) pullShared(ctx context.Context, rt *runtime, since int64) (int64, error) {
	rows, err := h.db.Query(ctx, `SELECT tag, value, ts, quality, changes, recv FROM dh.live_values WHERE gateway = $1 AND recv > $2`,
		strings.ToLower(rt.gw.Alias), since)
	if err != nil {
		return since, err
	}
	defer rows.Close()
	vals := map[string]VTQC{}
	for rows.Next() {
		var name string
		var v VTQC
		var q int16
		var c int32
		if err := rows.Scan(&name, &v.V, &v.T, &q, &c, &v.R); err != nil {
			return since, err
		}
		v.Q, v.C = int(q), int(c)
		vals[name] = v
		if v.R > since {
			since = v.R
		}
	}
	if err := rows.Err(); err != nil {
		return since, err
	}
	if len(vals) > 0 {
		rt.mu.Lock()
		for name, v := range vals {
			rt.byName[name] = v
		}
		rt.mu.Unlock()
		h.notify(rt.gw.Alias)
	}
	return since, nil
}

// startReplica loads the gateway configurations (no collectors) and keeps the
// shared values fresh.
func (h *Hub) startReplica(ctx context.Context) error {
	if err := h.loadConfigs(ctx); err != nil {
		return err
	}
	h.refresh(ctx)
	go func() {
		t := time.NewTicker(h.refreshEvery)
		defer t.Stop()
		configs := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if time.Since(configs) > 30*time.Second {
				configs = time.Now()
				if err := h.loadConfigs(ctx); err != nil {
					slog.Warn("reload gateway configurations", "err", err)
				}
			}
			h.refresh(ctx)
		}
	}()
	return nil
}

// loadConfigs (re)reads every gateway's configuration (replica).
func (h *Hub) loadConfigs(ctx context.Context) error {
	aliases, err := db.Strings(ctx, h.db, `SELECT alias FROM dh.gateways ORDER BY id`)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, a := range aliases {
		keep[strings.ToLower(a)] = true
		if err := h.replicaLoad(ctx, a); err != nil {
			slog.Warn("load gateway configuration", "alias", a, "err", err)
		}
	}
	h.mu.Lock()
	for k := range h.gateways {
		if !keep[k] {
			delete(h.gateways, k)
		}
	}
	h.mu.Unlock()
	return nil
}

// replicaLoad (re)reads one gateway's configuration, keeping its values.
func (h *Hub) replicaLoad(ctx context.Context, alias string) error {
	g, err := LoadGateway(ctx, h.db, alias)
	key := strings.ToLower(alias)
	if err != nil {
		if db.IsNoRows(err) {
			h.mu.Lock()
			delete(h.gateways, key)
			h.mu.Unlock()
			return nil
		}
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if rt := h.gateways[key]; rt != nil {
		rt.mu.Lock()
		rt.gw = g
		rt.mu.Unlock()
		return nil
	}
	rt := &runtime{hub: h, gw: g, byName: map[string]VTQC{}, byAddress: map[string]VTQC{}, done: make(chan struct{}), statusTS: time.Now()}
	close(rt.done)
	rt.connected = g.Type == RESTServer
	h.gateways[key] = rt
	return nil
}

// refresh reads the shared values and gateway states (replica).
func (h *Hub) refresh(ctx context.Context) {
	rows, err := h.db.Query(ctx, `SELECT gateway, tag, value, ts, quality, changes, recv FROM dh.live_values`)
	if err != nil {
		slog.Warn("read live values", "err", err)
		return
	}
	byGW := map[string]map[string]VTQC{}
	for rows.Next() {
		var gw, name string
		var v VTQC
		var q int16
		var c int32
		if err := rows.Scan(&gw, &name, &v.V, &v.T, &q, &c, &v.R); err != nil {
			rows.Close()
			slog.Warn("read live values", "err", err)
			return
		}
		v.Q, v.C = int(q), int(c)
		m := byGW[gw]
		if m == nil {
			m = map[string]VTQC{}
			byGW[gw] = m
		}
		m[name] = v
	}
	rows.Close()
	type state struct {
		connected bool
		err       *string
		ts        int64
	}
	states := map[string]state{}
	srows, err := h.db.Query(ctx, `SELECT gateway, connected, error, status_ts FROM dh.gateway_state`)
	if err == nil {
		for srows.Next() {
			var k string
			var s state
			if srows.Scan(&k, &s.connected, &s.err, &s.ts) == nil {
				states[k] = s
			}
		}
		srows.Close()
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for key, rt := range h.gateways {
		vals := byGW[key]
		st, hasState := states[key]
		rt.mu.Lock()
		for name, v := range vals {
			if cur, ok := rt.byName[name]; !ok || v.R >= cur.R {
				rt.byName[name] = v
			}
		}
		if rt.gw.Type == RESTServer {
			rt.connected = true
			if hasState {
				rt.lastErr = deref(st.err)
				rt.statusTS = time.UnixMilli(st.ts)
			}
		} else if hasState {
			rt.connected, rt.lastErr, rt.statusTS = st.connected, deref(st.err), time.UnixMilli(st.ts)
		} else {
			rt.connected = false
		}
		rt.mu.Unlock()
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// persistPush writes the values of a REST push received by an API replica, so
// the worker and the other replicas see them.
func (h *Hub) persistPush(ctx context.Context, rt *runtime, names []string) error {
	key := strings.ToLower(rt.gw.Alias)
	var tags, vals []string
	var ts, recv []int64
	var qs []int16
	var cs []int32
	rt.mu.RLock()
	for _, n := range names {
		if v, ok := rt.byName[n]; ok {
			tags, vals = append(tags, n), append(vals, v.V)
			ts, recv, qs, cs = append(ts, v.T), append(recv, v.R), append(qs, int16(v.Q)), append(cs, int32(v.C))
		}
	}
	errMsg := rt.lastErr
	rt.mu.RUnlock()
	if len(tags) > 0 {
		// the change counter goes on from the stored one (several replicas receive pushes)
		if _, err := h.db.Exec(ctx, `INSERT INTO dh.live_values AS lv (gateway, tag, value, ts, quality, changes, recv)
			SELECT $1, * FROM unnest($2::text[], $3::text[], $4::bigint[], $5::smallint[], $6::int[], $7::bigint[])
			ON CONFLICT (gateway, tag) DO UPDATE SET value = EXCLUDED.value, ts = EXCLUDED.ts, quality = EXCLUDED.quality,
			changes = lv.changes + CASE WHEN lv.value = EXCLUDED.value AND lv.quality = EXCLUDED.quality THEN 0 ELSE 1 END,
			recv = GREATEST(lv.recv, EXCLUDED.recv)`, key, tags, vals, ts, qs, cs, recv); err != nil {
			return err
		}
	}
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	return writeState(ctx, h.db, key, true, e, time.Now().UnixMilli())
}

// QueueCommand stores a command for a REST push device (it picks it up with
// TakeCommands) until it expires.
func (h *Hub) QueueCommand(ctx context.Context, alias, device string, payload []byte, expires time.Time) error {
	_, err := h.db.Exec(ctx, `INSERT INTO dh.commands (gateway, device, payload, expires_at) VALUES ($1, $2, $3, $4)`,
		strings.ToLower(alias), device, payload, expires)
	return err
}

// Command is a queued command delivered to a REST push device.
type Command struct {
	ID      int64           `json:"id"`
	Device  string          `json:"device"`
	Payload json.RawMessage `json:"payload"`
}

// TakeCommands returns (and marks delivered) the pending commands of a
// REST_SERVER gateway, authenticated with its API key.
func (h *Hub) TakeCommands(ctx context.Context, alias, apiKey string) ([]Command, error) {
	rt := h.runtime(alias)
	if rt == nil || rt.gw.Type != RESTServer {
		return nil, httpx.NotFound("err_invalid_gateway_alias")
	}
	if expected := rt.gw.Secrets["api_key"]; expected != "" && apiKey != expected {
		return nil, &httpx.Error{Status: http.StatusUnauthorized, Msg: "invalid api key"}
	}
	rows, err := h.db.Query(ctx, `UPDATE dh.commands SET delivered_at = now() WHERE id IN (
			SELECT id FROM dh.commands WHERE gateway = $1 AND delivered_at IS NULL AND expires_at > now() ORDER BY id LIMIT 100
			FOR UPDATE SKIP LOCKED)
		RETURNING id, device, payload`, strings.ToLower(alias))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Command{}
	for rows.Next() {
		var c Command
		var raw []byte
		if err := rows.Scan(&c.ID, &c.Device, &raw); err != nil {
			return nil, err
		}
		c.Payload = raw
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID }) // oldest first, as queued
	_, _ = h.db.Exec(ctx, `DELETE FROM dh.commands WHERE created_at < now() - interval '1 day'`)
	return out, nil
}

// emit signals a configuration change to the worker (replica).
func (h *Hub) emit(ctx context.Context, alias string) {
	if h.mode == ModeReplica {
		events.Emit(ctx, h.db, events.Gateway, alias)
	}
}
