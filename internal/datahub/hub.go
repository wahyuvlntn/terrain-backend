// Package datahub connects to field devices ("gateways" / data sources) and
// keeps the latest value of every gateway tag in memory.
//
// Supported protocols: OPC UA client, Modbus TCP, Modbus RTU, Siemens S7,
// MQTT client, MQTT server (embedded broker), REST client (polling), REST
// server (push) and MTConnect.
package datahub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"afdd/internal/broker"
	"afdd/internal/db"
	"afdd/internal/httpx"
)

// Protocols.
const (
	OPCUA      = "OPCUA"
	MQTTServer = "MQTT_SERVER"
	MQTTClient = "MQTT_CLIENT"
	RESTServer = "REST_SERVER"
	RESTClient = "REST_CLIENT"
	ModbusTCP  = "MODBUS_TCP"
	ModbusRTU  = "MODBUS_RTU"
	S7         = "S7"
	MTConnect  = "MTCONNECT"
)

// ValidProtocol reports whether p is a supported protocol.
func ValidProtocol(p string) bool {
	switch p {
	case OPCUA, MQTTServer, MQTTClient, RESTServer, RESTClient, ModbusTCP, ModbusRTU, S7, MTConnect:
		return true
	}
	return false
}

// Quality codes (0 = good, as rendered by the web UI).
const (
	QualityGood   = 0
	QualityBad    = 1
	QualityConfig = 2
)

// VTQC is a tag value: value, timestamp (ms), quality and change counter.
type VTQC struct {
	V string `json:"v"`
	T int64  `json:"t"`
	Q int    `json:"q"`
	C int    `json:"c"`
	R int64  `json:"-"` // time the value was received by the hub (ms)
}

// Tag is a configured gateway tag.
type Tag struct {
	Name        string  `json:"name"`
	Address     string  `json:"address"`
	Description *string `json:"description"`
}

// TagNode is an entry of a browsed address space.
type TagNode struct {
	Address  string     `json:"address"`
	Name     string     `json:"name"`
	Children []*TagNode `json:"children,omitempty"`
}

// Gateway is the configuration of a data source.
type Gateway struct {
	ID          int64
	Alias       string
	Name        string
	Type        string
	Description *string
	Params      map[string]string
	Secrets     map[string]string
	Active      bool
	Tags        []Tag
}

// Param returns a parameter with a default.
func (g *Gateway) Param(key, def string) string {
	if v, ok := g.Params[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// ParamInt parses an integer parameter.
func (g *Gateway) ParamInt(key string, def int) int {
	v := g.Param(key, "")
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return int(f)
	}
	return def
}

// ParamFloat parses a float parameter.
func (g *Gateway) ParamFloat(key string, def float64) float64 {
	v := g.Param(key, "")
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}

// ParamBool parses a boolean parameter.
func (g *Gateway) ParamBool(key string, def bool) bool {
	v := g.Param(key, "")
	if v == "" {
		return def
	}
	return httpx.ParseBool(v)
}

// Seconds parses an interval/timeout parameter expressed in seconds.
func (g *Gateway) Seconds(key string, def time.Duration) time.Duration {
	f := g.ParamFloat(key, -1)
	if f <= 0 {
		return def
	}
	return time.Duration(f * float64(time.Second))
}

// driver is implemented by every protocol.
type driver interface {
	// run connects and acquires data until ctx is cancelled.
	run(ctx context.Context, rt *runtime)
	// read reads addresses on demand.
	read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error)
	// browse lists the address space below nodeID (depth 0 = full tree).
	browse(ctx context.Context, rt *runtime, nodeID string, depth int) ([]*TagNode, error)
}

// runtime is the live state of a gateway.
type runtime struct {
	hub    *Hub
	gw     *Gateway
	cancel context.CancelFunc
	done   chan struct{}
	drv    driver

	mu        sync.RWMutex
	connected bool
	lastErr   string
	statusTS  time.Time
	byName    map[string]VTQC // tag name -> value
	byAddress map[string]VTQC // raw address -> value (push protocols)
	keys      []string        // last keys seen (for browse of push protocols)
}

func (rt *runtime) setConnected(ok bool, err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	changed := rt.connected != ok
	rt.connected = ok
	if err != nil {
		rt.lastErr = err.Error()
	} else if ok {
		rt.lastErr = ""
	}
	if changed || err != nil {
		rt.statusTS = time.Now()
	}
}

// markAllBad flags every value as bad quality (connection lost).
func (rt *runtime) markAllBad() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for k, v := range rt.byName {
		if v.Q == QualityGood {
			v.Q = QualityBad
			rt.byName[k] = v
		}
	}
}

// update stores values keyed by tag name.
func (rt *runtime) update(values map[string]VTQC) {
	if len(values) == 0 {
		return
	}
	recv := time.Now().UnixMilli()
	rt.mu.Lock()
	for name, v := range values {
		v.R = recv
		prev, ok := rt.byName[name]
		if ok && prev.V == v.V && prev.Q == v.Q {
			v.C = prev.C
		} else if ok {
			v.C = prev.C + 1
		}
		rt.byName[name] = v
	}
	rt.mu.Unlock()
	rt.hub.notify(rt.gw.Alias)
}

// updateByAddress stores push values keyed by address and maps them to tags.
func (rt *runtime) updateByAddress(values map[string]VTQC) {
	if len(values) == 0 {
		return
	}
	byName := map[string]VTQC{}
	rt.mu.Lock()
	keys := make([]string, 0, len(values))
	for addr, v := range values {
		rt.byAddress[addr] = v
		keys = append(keys, addr)
	}
	sort.Strings(keys)
	rt.keys = mergeKeys(rt.keys, keys)
	tags := rt.gw.Tags
	rt.mu.Unlock()
	for _, t := range tags {
		if v, ok := values[t.Address]; ok {
			byName[t.Name] = v
		}
	}
	rt.update(byName)
}

func mergeKeys(a, b []string) []string {
	set := map[string]bool{}
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 5000 {
		out = out[:5000]
	}
	return out
}

// Hub manages all gateways.
type Hub struct {
	db            *db.DB
	broker        *broker.Broker
	scriptTimeout time.Duration
	dataDir       string
	mqttPrefix    string // "<org>.<project>" used for MQTT_SERVER usernames/topics

	mu       sync.RWMutex
	gateways map[string]*runtime // lower(alias) -> runtime

	subMu       sync.RWMutex
	subscribers []func(alias string)
}

// New creates the hub.
func New(d *db.DB, b *broker.Broker, scriptTimeout time.Duration, dataDir, mqttPrefix string) *Hub {
	return &Hub{db: d, broker: b, scriptTimeout: scriptTimeout, dataDir: dataDir, mqttPrefix: mqttPrefix, gateways: map[string]*runtime{}}
}

// MQTTIdentity returns the username/topic devices use for an MQTT_SERVER gateway.
func (h *Hub) MQTTIdentity(alias string) string {
	if h.mqttPrefix == "" {
		return alias
	}
	return h.mqttPrefix + "." + alias
}

// OnUpdate registers a callback invoked (asynchronously) when gateway values change.
func (h *Hub) OnUpdate(fn func(alias string)) {
	h.subMu.Lock()
	h.subscribers = append(h.subscribers, fn)
	h.subMu.Unlock()
}

func (h *Hub) notify(alias string) {
	h.subMu.RLock()
	subs := h.subscribers
	h.subMu.RUnlock()
	for _, fn := range subs {
		fn(alias)
	}
}

// Start loads every gateway and connects the active ones.
func (h *Hub) Start(ctx context.Context) error {
	aliases, err := db.Strings(ctx, h.db, `SELECT alias FROM dh.gateways ORDER BY id`)
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if err := h.Reload(ctx, a); err != nil {
			slog.Error("start gateway", "alias", a, "err", err)
		}
	}
	return nil
}

// Stop disconnects every gateway.
func (h *Hub) Stop() {
	h.mu.Lock()
	rts := make([]*runtime, 0, len(h.gateways))
	for _, rt := range h.gateways {
		rts = append(rts, rt)
	}
	h.gateways = map[string]*runtime{}
	h.mu.Unlock()
	for _, rt := range rts {
		h.stopRuntime(rt)
	}
}

func (h *Hub) stopRuntime(rt *runtime) {
	if rt.cancel != nil {
		rt.cancel()
		select {
		case <-rt.done:
		case <-time.After(10 * time.Second):
			slog.Warn("gateway did not stop in time", "alias", rt.gw.Alias)
		}
	}
	if rt.gw.Type == MQTTServer && h.broker != nil {
		h.broker.RemoveGateway(h.MQTTIdentity(rt.gw.Alias))
	}
}

// LoadGateway reads a gateway configuration with its tags.
func LoadGateway(ctx context.Context, q db.Querier, alias string) (*Gateway, error) {
	g := &Gateway{}
	var params, secrets []byte
	err := q.QueryRow(ctx, `SELECT id, alias, name, type, description, params, secrets, is_active FROM dh.gateways WHERE lower(alias) = lower($1)`, alias).
		Scan(&g.ID, &g.Alias, &g.Name, &g.Type, &g.Description, &params, &secrets, &g.Active)
	if err != nil {
		return nil, err
	}
	g.Secrets = map[string]string{}
	if len(secrets) > 0 {
		_ = json.Unmarshal(secrets, &g.Secrets)
	}
	g.Params = map[string]string{}
	if len(params) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(params, &raw); err == nil {
			for k, v := range raw {
				switch t := v.(type) {
				case string:
					g.Params[k] = t
				case nil:
				default:
					b, _ := json.Marshal(t)
					g.Params[k] = strings.Trim(string(b), `"`)
				}
			}
		}
	}
	rows, err := q.Query(ctx, `SELECT name, address, description FROM dh.gateway_tags WHERE gateway_id = $1 ORDER BY name`, g.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Address, &t.Description); err != nil {
			return nil, err
		}
		g.Tags = append(g.Tags, t)
	}
	return g, rows.Err()
}

// Reload (re)starts a gateway from its stored configuration. Values of a
// running gateway are kept when it is restarted.
func (h *Hub) Reload(ctx context.Context, alias string) error {
	key := strings.ToLower(alias)
	h.mu.Lock()
	old := h.gateways[key]
	delete(h.gateways, key)
	h.mu.Unlock()
	if old != nil {
		h.stopRuntime(old)
	}

	g, err := LoadGateway(ctx, h.db, alias)
	if err != nil {
		if db.IsNoRows(err) {
			return nil // deleted
		}
		return err
	}
	rt := &runtime{hub: h, gw: g, byName: map[string]VTQC{}, byAddress: map[string]VTQC{}, done: make(chan struct{}), statusTS: time.Now()}
	if old != nil {
		old.mu.RLock()
		for _, t := range g.Tags {
			if v, ok := old.byName[t.Name]; ok {
				rt.byName[t.Name] = v
			}
		}
		for k, v := range old.byAddress {
			rt.byAddress[k] = v
		}
		rt.keys = old.keys
		old.mu.RUnlock()
	}
	rt.drv = newDriver(g.Type)
	h.mu.Lock()
	h.gateways[key] = rt
	h.mu.Unlock()

	if !g.Active || rt.drv == nil {
		close(rt.done)
		rt.markAllBad()
		return nil
	}
	rctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	go func() {
		defer close(rt.done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("gateway driver panic", "alias", g.Alias, "panic", r)
				rt.setConnected(false, fmt.Errorf("driver crashed: %v", r))
			}
		}()
		rt.drv.run(rctx, rt)
		rt.setConnected(false, nil)
		rt.markAllBad()
	}()
	return nil
}

// Remove stops and forgets a gateway.
func (h *Hub) Remove(alias string) {
	key := strings.ToLower(alias)
	h.mu.Lock()
	rt := h.gateways[key]
	delete(h.gateways, key)
	h.mu.Unlock()
	if rt != nil {
		h.stopRuntime(rt)
	}
}

func (h *Hub) runtime(alias string) *runtime {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.gateways[strings.ToLower(alias)]
}

// Status describes the connection state of a gateway.
type Status struct {
	Active    bool    `json:"is_active"`
	Connected bool    `json:"is_connected"`
	Error     *string `json:"error"`
	Timestamp int64   `json:"timestamp"`
}

// Status returns the connection state of a gateway.
func (h *Hub) Status(alias string) Status {
	rt := h.runtime(alias)
	if rt == nil {
		return Status{Timestamp: time.Now().UnixMilli()}
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	st := Status{Active: rt.gw.Active, Connected: rt.gw.Active && rt.connected, Timestamp: rt.statusTS.UnixMilli()}
	if rt.lastErr != "" {
		e := rt.lastErr
		st.Error = &e
	}
	return st
}

// Value returns the latest value of a tag.
func (h *Hub) Value(alias, tag string) (VTQC, bool) {
	rt := h.runtime(alias)
	if rt == nil {
		return VTQC{}, false
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	v, ok := rt.byName[tag]
	if ok && !(rt.gw.Active && rt.connected) && v.Q == QualityGood {
		v.Q = QualityBad
	}
	return v, ok
}

// Read reads addresses on demand (falls back to cached push values).
func (h *Hub) Read(ctx context.Context, alias string, addresses []string) (map[string]string, error) {
	rt := h.runtime(alias)
	if rt == nil {
		return nil, httpx.BadRequest("err_invalid_gateway_alias")
	}
	if rt.drv == nil {
		return nil, httpx.BadRequest("err_invalid_data_source_type")
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := rt.drv.read(rctx, rt, addresses)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range addresses {
		out[a] = res[a]
	}
	return out, nil
}

// Browse lists the address space of a gateway.
func (h *Hub) Browse(ctx context.Context, alias, nodeID string, depth int) ([]*TagNode, error) {
	rt := h.runtime(alias)
	if rt == nil {
		return nil, httpx.BadRequest("err_invalid_gateway_alias")
	}
	if rt.drv == nil {
		return nil, httpx.BadRequest("err_data_source_no_browse")
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	nodes, err := rt.drv.browse(rctx, rt, nodeID, depth)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []*TagNode{}
	}
	return nodes, nil
}

// pushBrowse lists keys received by push protocols.
func pushBrowse(rt *runtime) []*TagNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make([]*TagNode, 0, len(rt.keys))
	for _, k := range rt.keys {
		out = append(out, &TagNode{Address: k, Name: k})
	}
	return out
}

func pushRead(rt *runtime, addresses []string) map[string]string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := map[string]string{}
	for _, a := range addresses {
		if v, ok := rt.byAddress[a]; ok {
			out[a] = v.V
		}
	}
	return out
}

// GeneratePassword creates a random credential.
func GeneratePassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// IngestREST accepts data pushed to a REST_SERVER gateway.
func (h *Hub) IngestREST(ctx context.Context, alias, apiKey string, body io.Reader) (int, error) {
	rt := h.runtime(alias)
	if rt == nil || rt.gw.Type != RESTServer {
		return 0, httpx.NotFound("err_invalid_gateway_alias")
	}
	expected := rt.gw.Secrets["api_key"]
	if expected != "" && apiKey != expected {
		return 0, &httpx.Error{Status: 401, Msg: "invalid api key"}
	}
	if !rt.gw.Active {
		return 0, httpx.BadRequest("data source is turned off")
	}
	b, err := io.ReadAll(io.LimitReader(body, 10<<20))
	if err != nil {
		return 0, err
	}
	values, err := h.mapPayload(rt.gw, b)
	if err != nil {
		rt.setConnected(true, err)
		return 0, httpx.BadRequest(err.Error())
	}
	rt.setConnected(true, nil)
	rt.updateByAddress(values)
	return len(values), nil
}

// errUnsupported is returned by drivers for unsupported operations.
var errUnsupported = errors.New("err_data_source_no_browse")

func newDriver(protocol string) driver {
	switch protocol {
	case OPCUA:
		return &opcuaDriver{}
	case ModbusTCP, ModbusRTU:
		return &modbusDriver{}
	case S7:
		return &s7Driver{}
	case MQTTClient:
		return &mqttClientDriver{}
	case MQTTServer:
		return &mqttServerDriver{}
	case RESTClient:
		return &restClientDriver{}
	case RESTServer:
		return &restServerDriver{}
	case MTConnect:
		return &mtconnectDriver{}
	}
	return nil
}

// sleepCtx waits for d or until ctx is done (returns false when cancelled).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pollLoop runs fn every interval until ctx is cancelled, applying a backoff
// after failures.
func pollLoop(ctx context.Context, interval time.Duration, fn func(ctx context.Context) error, onErr func(error)) {
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	backoff := time.Second
	for {
		start := time.Now()
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := interval - time.Since(start)
		if err != nil {
			onErr(err)
			wait = backoff
			if backoff < 30*time.Second {
				backoff *= 2
			}
		} else {
			backoff = time.Second
		}
		if wait < 0 {
			wait = 0
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}
