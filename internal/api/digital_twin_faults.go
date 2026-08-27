package api

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"afdd/internal/broker"
	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/httpx"
)

// LBNL chiller plant faults of the simulated plant (DESIGN §2.4): fixed
// intensity faults that the FDD rules detect. The catalog is static (it
// mirrors the simulator's FaultSpecs) and is shared by the live fault
// injection (admins), the simulation spec validation and
// simulation/getfaultcatalog.

// twinFaultKind is one injectable fault kind with its documented intensities.
type twinFaultKind struct {
	Kind string `json:"kind"`
	// AssetKind is the asset scope of the FDD rule: chiller, tower or plant.
	AssetKind   string    `json:"asset_kind"`
	Assets      []string  `json:"assets"`
	Intensities []float64 `json:"intensities"`
	Unit        string    `json:"unit"`
	Label       string    `json:"label"`
	// Code is the FDD rule / alarm code that detects the fault.
	Code string `json:"code"`
}

var (
	twinChillers = []string{"CH_01", "CH_02", "CH_03"}
	twinTowers   = []string{"CT_01", "CT_02", "CT_03"}
	twinPlant    = []string{"CHWS_01"}
)

// twinFaultCatalog lists the LBNL fault kinds in the order of DESIGN §2.4.
var twinFaultCatalog = []twinFaultKind{
	{Kind: "chwst_bias", AssetKind: "chiller", Assets: twinChillers, Intensities: []float64{-2, -1, 1, 2}, Unit: "K",
		Label: "Chiller CHW leaving temperature sensor bias", Code: "CHL-011"},
	{Kind: "cwst_bias", AssetKind: "tower", Assets: twinTowers, Intensities: []float64{-2, -1, 1, 2}, Unit: "K",
		Label: "Tower leaving water temperature sensor bias", Code: "CT-013"},
	{Kind: "ct_fouling", AssetKind: "tower", Assets: twinTowers, Intensities: []float64{0.95, 0.80, 0.65}, Unit: "capacity factor",
		Label: "Cooling tower heat exchanger fouling", Code: "CT-014"},
	{Kind: "riser_dp_bias", AssetKind: "plant", Assets: twinPlant, Intensities: []float64{-0.20, -0.10, 0.10, 0.20}, Unit: "fraction",
		Label: "Riser differential pressure sensor bias", Code: "CHS-005"},
	{Kind: "twv_leak", AssetKind: "plant", Assets: twinPlant, Intensities: []float64{0.25, 0.50, 0.75}, Unit: "fraction",
		Label: "Condenser 3-way valve leakage", Code: "CHS-006"},
	{Kind: "twv_stuck", AssetKind: "plant", Assets: twinPlant, Intensities: []float64{0.50, 0.75}, Unit: "fraction",
		Label: "Condenser 3-way valve stuck", Code: "CHS-007"},
	{Kind: "cw_pi_tuning", AssetKind: "plant", Assets: twinPlant, Intensities: []float64{1}, Unit: "",
		Label: "CW supply temperature loop hunting (PI tuning)", Code: "CHS-008"},
}

// twinFault returns the catalog entry of a fault kind (case-insensitive).
func twinFault(kind string) *twinFaultKind {
	kind = strings.ToLower(strings.TrimSpace(kind))
	for i := range twinFaultCatalog {
		if twinFaultCatalog[i].Kind == kind {
			return &twinFaultCatalog[i]
		}
	}
	return nil
}

// asset returns the canonical alias when the fault applies to the equipment.
func (f *twinFaultKind) asset(alias string) (string, bool) {
	for _, a := range f.Assets {
		if strings.EqualFold(a, strings.TrimSpace(alias)) {
			return a, true
		}
	}
	return "", false
}

// intensity returns the documented intensity equal to v (the injected value
// is always one of them, so runs are comparable with the LBNL data sets).
func (f *twinFaultKind) intensity(v float64) (float64, bool) {
	for _, x := range f.Intensities {
		if math.Abs(x-v) < 1e-9 {
			return x, true
		}
	}
	return 0, false
}

// intensityList formats the documented intensities for messages.
func (f *twinFaultKind) intensityList() string {
	parts := make([]string, len(f.Intensities))
	for i, x := range f.Intensities {
		parts[i] = fmt.Sprintf("%v", x)
	}
	return strings.Join(parts, ", ")
}

// visibleFaultCatalog returns the catalog restricted to the equipment the
// user can see (kinds without a visible equipment are left out).
func visibleFaultCatalog(visible func(alias string) bool) []twinFaultKind {
	out := []twinFaultKind{}
	for _, f := range twinFaultCatalog {
		g := f
		g.Assets = []string{}
		for _, a := range f.Assets {
			if visible(a) {
				g.Assets = append(g.Assets, a)
			}
		}
		if len(g.Assets) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func (s *Server) registerDigitalTwinFaults() {
	s.handle("digital_twin/injectfault", AccessAdmin, s.twinInjectFault)
	s.handle("digital_twin/getactivefaults", AccessUser, s.twinActiveFaults)
}

// twinInjectFault injects or clears an LBNL fault on the live plant (admins
// only, never the AI agent). Input: asset_id, fault (kind), value (one of the
// documented intensities; ignored for clear), action inject | clear. The
// command goes to the equipment's MQTT command topic; the simulator applies it
// on its next step and the following report shows the effect.
func (s *Server) twinInjectFault(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	assetID := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, assetID); err != nil {
		return nil, err
	}
	f := twinFault(c.F.StrTrim("fault"))
	if f == nil {
		return nil, httpx.BadRequest("Unknown fault type")
	}
	action := strings.ToLower(c.F.StrTrim("action"))
	if action == "" {
		action = "inject"
	}
	if action != "inject" && action != "clear" {
		return nil, httpx.BadRequest("The action must be inject or clear")
	}
	var alias string
	if err := s.DB.QueryRow(c, `SELECT alias FROM asset.equipment WHERE id = $1`, assetID).Scan(&alias); err != nil {
		return nil, err
	}
	canonical, ok := f.asset(alias)
	if !ok {
		return nil, httpx.BadRequestf("%s does not apply to %s", f.Label, alias)
	}
	value := 0.0
	if action == "inject" {
		vp := c.F.FloatPtr("value")
		if vp == nil {
			return nil, httpx.BadRequestf("Choose an intensity for %s (%s)", f.Label, f.intensityList())
		}
		v, ok := f.intensity(*vp)
		if !ok {
			return nil, httpx.BadRequestf("%v is not a documented intensity of %s (%s)", *vp, f.Label, f.intensityList())
		}
		value = v
	}

	gateway, device, err := s.equipmentCommandTarget(c, assetID)
	if err != nil {
		return nil, err
	}
	requestID := commandRequestID()
	payload, _ := json.Marshal(map[string]any{"request_id": requestID, "fault": f.Kind, "value": value, "action": action,
		"user": c.User.Username, "expires_at": time.Now().Add(twinControlWait).UnixMilli()})
	topic := broker.CommandTopic(s.Hub.MQTTIdentity(gateway), device)
	if err := s.Broker.Publish(topic, payload); err != nil {
		return nil, &httpx.Error{Status: 502, Msg: "Could not send the command to the plant", Err: err}
	}
	slog.Info("fault command sent", "equipment", canonical, "fault", f.Kind, "value", value, "action", action,
		"request", requestID, "user", c.User.Username)
	return map[string]any{"status": "SENT", "request_id": requestID, "alias": canonical, "fault": f.Kind, "value": value,
		"action": action}, nil
}

// equipmentCommandTarget returns the MQTT_SERVER gateway and the device name
// of an equipment's command topic, taken from its mapped tags
// ("<device>/<key>"), after checking that the equipment is reporting.
func (s *Server) equipmentCommandTarget(ctx context.Context, assetID int64) (gateway, device string, err error) {
	rows, err := s.DB.Query(ctx, `SELECT g.alias, g.type, ep.tag_name FROM asset.equipment_parameters ep
		JOIN dh.gateways g ON g.id = ep.gateway_id
		WHERE ep.equipment_id = $1 AND ep.tag_name IS NOT NULL AND ep.tag_name LIKE '%/%' ORDER BY ep.id`, assetID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	type tag struct{ gateway, typ, name string }
	var tags []tag
	for rows.Next() {
		var t tag
		if err := rows.Scan(&t.gateway, &t.typ, &t.name); err != nil {
			return "", "", err
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if len(tags) == 0 {
		return "", "", httpx.BadRequest("The equipment is not mapped to a data source")
	}
	t := tags[0]
	if t.typ != datahub.MQTTServer {
		return "", "", httpx.BadRequest("The data source of this equipment does not accept commands")
	}
	if st := s.Hub.Status(t.gateway); !st.Active || !st.Connected {
		return "", "", httpx.BadRequestf("Data source %s is not connected", t.gateway)
	}
	// The equipment must be reporting, otherwise it would not take the command.
	fresh := false
	for _, x := range tags {
		if x.gateway != t.gateway {
			continue
		}
		if v, ok := s.Hub.Value(x.gateway, x.name); ok && v.Q == datahub.QualityGood && v.R > 0 &&
			time.Since(time.UnixMilli(v.R)) <= twinControlStale {
			fresh = true
			break
		}
	}
	if !fresh {
		return "", "", httpx.BadRequest("No live data from the equipment")
	}
	return t.gateway, t.name[:strings.LastIndex(t.name, "/")], nil
}

// commandRequestID returns a positive random id that tags a command in the
// simulator log (fault commands have no control request row).
func commandRequestID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 11) // < 2^53: exact in JavaScript
}

// simulatorFault is a fault in the persisted simulator state
// (core.settings demo_simulator_state, cmd/simulator SimState).
type simulatorFault struct {
	Kind  string    `json:"kind"`
	Sev   float64   `json:"sev"`
	Value *float64  `json:"value"`
	Start time.Time `json:"start"`
}

type simulatorState struct {
	SavedAt time.Time `json:"saved_at"`
	Assets  map[string]struct {
		Faults []simulatorFault `json:"faults"`
	} `json:"assets"`
}

// twinActiveFaults lists the faults present in the live plant (degradations
// and injected LBNL faults) from the simulator's persisted state, for the
// equipment the user can see: [{alias, kind, value, sev, start, label, code}].
func (s *Server) twinActiveFaults(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	var raw []byte
	err := s.DB.QueryRow(c, `SELECT value FROM core.settings WHERE key = 'demo_simulator_state'`).Scan(&raw)
	if err != nil {
		if db.IsNoRows(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	return s.activeFaultsFrom(c, raw)
}

func (s *Server) activeFaultsFrom(c *Ctx, raw []byte) ([]map[string]any, error) {
	var st simulatorState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("simulator state: %w", err)
	}
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for alias, as := range st.Assets {
		ref := eq[strings.ToLower(alias)]
		if ref == nil || !ref.Visible {
			continue
		}
		for _, f := range as.Faults {
			row := map[string]any{"alias": ref.Alias, "asset_id": ref.ID, "kind": f.Kind, "value": f.Value,
				"sev": httpx.Round(f.Sev, 4), "start": httpx.TSPtr(&f.Start), "label": nil, "code": nil}
			if k := twinFault(f.Kind); k != nil {
				row["label"], row["code"] = k.Label, k.Code
			}
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i]["alias"] != out[j]["alias"] {
			return out[i]["alias"].(string) < out[j]["alias"].(string)
		}
		return out[i]["kind"].(string) < out[j]["kind"].(string)
	})
	return out, nil
}

// equipmentRef is an equipment with its type, model and visibility for the
// current user (keyed by lower-case alias in equipmentRefs).
type equipmentRef struct {
	ID         int64
	Alias      string
	Name       string
	TypeName   string
	ModelName  string
	LocationID int64
	Visible    bool
}

// equipmentRefs loads every equipment (the plant has a few dozen) with the
// user's visibility, keyed by lower-case alias.
func (s *Server) equipmentRefs(c *Ctx) (map[string]*equipmentRef, error) {
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT e.id, e.alias, e.name, t.name, m.name, e.location_id
		FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id JOIN asset.types t ON t.id = m.type_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*equipmentRef{}
	for rows.Next() {
		r := &equipmentRef{}
		if err := rows.Scan(&r.ID, &r.Alias, &r.Name, &r.TypeName, &r.ModelName, &r.LocationID); err != nil {
			return nil, err
		}
		r.Visible = scope.AllowsLocation(r.LocationID)
		out[strings.ToLower(r.Alias)] = r
	}
	return out, rows.Err()
}

// visibleAliasKeys returns the lower-case aliases of the visible equipment
// (nil when the user is unrestricted).
func (s *Server) visibleAliasKeys(c *Ctx, eq map[string]*equipmentRef) ([]string, error) {
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	if scope == nil || scope.Unrestricted {
		return nil, nil
	}
	keys := []string{}
	for k, r := range eq {
		if r.Visible {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
