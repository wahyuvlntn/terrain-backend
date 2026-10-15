package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"afdd/internal/auth"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
)

// Digital twin what-if simulations (DESIGN §4.2, §5). The API only manages
// sim.jobs rows and reads the results; the simulator's job runner claims
// QUEUED jobs (LISTEN sim_jobs), runs every variant on a clone of the plant
// and writes sim.series / sim.states / sim.events, then ai-analytics adds the
// post-analysis. Status machine:
//
//	PROPOSED (agent) --approve--> QUEUED --> RUNNING --> ANALYZING --> SUCCEEDED | FAILED
//	PROPOSED --reject--> REJECTED;  PROPOSED | QUEUED --cancel--> CANCELLED
//
// UI submissions start at QUEUED; agent proposals always start at PROPOSED and
// only a person can approve them. A job is visible to a user when every
// equipment its actions reference is in the user's location scope (or the
// user requested it); frames, events and trends only return visible equipment.

const (
	simMaxActive      = 3   // QUEUED + RUNNING jobs per user
	simMaxProposed    = 5   // PROPOSED jobs per user (whatever the agent session)
	simMaxVariants    = 3   // variants per job
	simMaxActions     = 200 // actions per job (common + every variant)
	simMinDuration    = 10  // minutes
	simMaxDuration    = 2880
	simStepS          = 60 // runner step
	simStartSlack     = 5 * time.Minute
	simStartHorizon   = 14 * 24 * time.Hour
	simMaxWeatherOff  = 10.0 // K, dry and wet bulb offsets
	simMinLoadScale   = 0.5
	simMaxLoadScale   = 1.3
	simMaxSeed        = 1<<53 - 1 // exact in JavaScript
	simMaxTitle       = 200
	simMaxRationale   = 4000
	simMaxNote        = 1000
	simFrameMaxEvents = 5000
	// simFrameMaxPoints caps the values of one frames response (series x
	// steps): a 72 h system job with every unit of the plant (31 units, 460
	// series, 4320 steps) is 2.0 M; longer runs, finer steps or a larger plant
	// must select units.
	simFrameMaxPoints = 2_500_000
	simTrendMaxPoints = 360
)

// Job statuses (sim.jobs.status).
const (
	simProposed  = "PROPOSED"
	simQueued    = "QUEUED"
	simRunning   = "RUNNING"
	simAnalyzing = "ANALYZING"
	simSucceeded = "SUCCEEDED"
	simFailed    = "FAILED"
	simRejected  = "REJECTED"
	simCancelled = "CANCELLED"
)

var (
	simStatuses    = []string{simProposed, simQueued, simRunning, simAnalyzing, simSucceeded, simFailed, simRejected, simCancelled}
	simOrigins     = []string{"ui", "agent", "optimizer", "analytics"}
	simActionKinds = []string{"control", "fault", "clear_fault", "trip", "offline", "maintenance", "release"}
	simEventKinds  = []string{"status", "condition", "alarm", "abnormal", "diagnosis", "anomaly", "fault", "control",
		"fault_injected", "fault_cleared", "trip", "note"}
	simVariantName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.\-]{0,39}$`)
	// Equipment types that are plant controllers, not machines: they cannot
	// trip, go offline or be taken into maintenance in a scenario.
	simVirtualTypes = map[string]bool{"chilled water system": true}
)

// simTransitions are the status changes made through the API; the runner and
// the post-analysis make the others.
var simTransitions = map[string]struct {
	from []string
	to   string
}{
	"approve": {from: []string{simProposed}, to: simQueued},
	"reject":  {from: []string{simProposed}, to: simRejected},
	"cancel":  {from: []string{simProposed, simQueued}, to: simCancelled},
}

// simDone is the past participle of each API action (messages).
var simDone = map[string]string{"approve": "approved", "reject": "rejected", "cancel": "cancelled"}

// simTransition returns the status a job in status `from` moves to on an API
// action, and false when the action is not allowed in that status.
func simTransition(action, from string) (string, bool) {
	t, ok := simTransitions[action]
	if !ok {
		return "", false
	}
	for _, f := range t.from {
		if f == from {
			return t.to, true
		}
	}
	return "", false
}

func (s *Server) registerSimulation() {
	s.handle("simulation/listjobs", AccessUser, s.simListJobs)
	s.handle("simulation/getjob", AccessUser, s.simGetJob)
	s.handle("simulation/submit", AccessUser, s.simSubmit)
	s.handle("simulation/propose", AccessUser, s.simPropose)
	s.handle("simulation/approve", AccessUser, s.simApprove)
	s.handle("simulation/reject", AccessUser, s.simReject)
	s.handle("simulation/cancel", AccessUser, s.simCancel)
	s.handle("simulation/getevents", AccessUser, s.simGetEvents)
	s.handle("simulation/getframes", AccessUser, s.simGetFrames)
	s.handle("simulation/gettrend", AccessUser, s.simGetTrend)
	s.handle("simulation/getfaultcatalog", AccessUser, s.simFaultCatalog)
}

// ---------------------------------------------------------------------------
// Scenario spec (DESIGN §5.1)
// ---------------------------------------------------------------------------

// simSpec is a validated, normalised scenario spec as stored in sim.jobs.spec:
// defaults filled in, aliases canonical, whole minutes, at least one variant.
type simSpec struct {
	Start       *string      `json:"start"` // UTC ISO; null = when the runner starts
	DurationMin int          `json:"duration_min"`
	Initial     string       `json:"initial"` // live | nominal
	Stochastic  bool         `json:"stochastic"`
	Seed        int64        `json:"seed"` // 0 = derived from the job id
	Weather     simWeather   `json:"weather"`
	LoadScale   float64      `json:"load_scale"`
	Actions     []simAction  `json:"actions"` // common to every variant
	Variants    []simVariant `json:"variants"`
}

type simWeather struct {
	DryBulbOffset float64 `json:"dry_bulb_offset"`
	WetBulbOffset float64 `json:"wet_bulb_offset"`
}

type simAction struct {
	AtMin       int      `json:"at_min"`
	Kind        string   `json:"kind"`
	Asset       string   `json:"asset"`
	Param       string   `json:"param,omitempty"`
	Fault       string   `json:"fault,omitempty"`
	Value       *float64 `json:"value,omitempty"`
	DurationMin *int     `json:"duration_min,omitempty"`
}

type simVariant struct {
	Name    string      `json:"name"`
	Actions []simAction `json:"actions"`
}

// simCatalog is what a spec is validated against: the equipment (by
// lower-case alias) with the user's visibility and their writable parameters.
type simCatalog struct {
	Assets map[string]*simAssetRef
}

type simAssetRef struct {
	Alias    string
	TypeName string
	Visible  bool
	Controls map[string]*simControl // writable parameters by alias
}

type simControl struct {
	twinControl
	Name string
	Unit string
	// Live is the current plant value (used for the mode of a gated set point
	// when the scenario starts from the live state); nil when unknown.
	Live *float64
}

func simBad(format string, args ...any) error { return httpx.BadRequestf(format, args...) }

// parseSimSpec validates a scenario spec given as a JSON tree (JSON numbers
// or, from multipart forms, strings) and returns it normalised.
func parseSimSpec(tree any, now time.Time, loc *time.Location, cat *simCatalog) (*simSpec, error) {
	m, ok := tree.(map[string]any)
	if !ok {
		return nil, simBad("The scenario spec must be a JSON object")
	}
	if err := treeKeys(m, "the scenario spec", "kind", "start", "duration_min", "initial", "stochastic", "seed", "weather",
		"load_scale", "actions", "variants"); err != nil {
		return nil, err
	}
	spec := &simSpec{Initial: "live", LoadScale: 1, Actions: []simAction{}, Variants: []simVariant{}}

	if v, ok := treeString(m["kind"]); ok && v != "" && !strings.EqualFold(v, "scenario") {
		return nil, simBad("Only scenario simulations can be submitted (kind %s)", v)
	}
	if v := m["start"]; v != nil {
		str, ok := treeString(v)
		if !ok {
			return nil, simBad("The scenario start must be an ISO time or null")
		}
		if str = strings.TrimSpace(str); str != "" && !strings.EqualFold(str, "null") {
			t, err := httpx.ParseTime(str, loc)
			if err != nil {
				return nil, simBad("Invalid scenario start %q", str)
			}
			if t.Before(now.Add(-simStartSlack)) {
				return nil, simBad("The scenario start is in the past")
			}
			if t.After(now.Add(simStartHorizon)) {
				return nil, simBad("The scenario start must be within 14 days")
			}
			ts := httpx.TS(t)
			spec.Start = &ts
		}
	}
	d, err := treeWhole(m["duration_min"], "duration_min", true)
	if err != nil {
		return nil, err
	}
	if d < simMinDuration || d > simMaxDuration {
		return nil, simBad("The duration must be %d to %d minutes", simMinDuration, simMaxDuration)
	}
	spec.DurationMin = int(d)
	if v := m["initial"]; v != nil {
		str, _ := treeString(v)
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "", "live":
			spec.Initial = "live"
		case "nominal":
			spec.Initial = "nominal"
		default:
			return nil, simBad("The initial state must be live or nominal")
		}
	}
	if v := m["stochastic"]; v != nil {
		b, ok := treeBool(v)
		if !ok {
			return nil, simBad("stochastic must be true or false")
		}
		spec.Stochastic = b
	}
	if m["seed"] != nil {
		seed, err := treeWhole(m["seed"], "seed", false)
		if err != nil {
			return nil, err
		}
		if seed < 0 || seed > simMaxSeed {
			return nil, simBad("The seed must be a whole number from 0 to %d", int64(simMaxSeed))
		}
		spec.Seed = seed
	}
	if v := m["weather"]; v != nil {
		w, ok := v.(map[string]any)
		if !ok {
			return nil, simBad("weather must be an object")
		}
		if err := treeKeys(w, "weather", "dry_bulb_offset", "wet_bulb_offset"); err != nil {
			return nil, err
		}
		for key, dst := range map[string]*float64{"dry_bulb_offset": &spec.Weather.DryBulbOffset, "wet_bulb_offset": &spec.Weather.WetBulbOffset} {
			if w[key] == nil {
				continue
			}
			x, ok := treeNumber(w[key])
			if !ok || math.Abs(x) > simMaxWeatherOff {
				return nil, simBad("weather %s must be a number from -%v to %v K", key, simMaxWeatherOff, simMaxWeatherOff)
			}
			*dst = x
		}
	}
	if v := m["load_scale"]; v != nil {
		x, ok := treeNumber(v)
		if !ok || x < simMinLoadScale-1e-9 || x > simMaxLoadScale+1e-9 {
			return nil, simBad("load_scale must be a number from %v to %v", simMinLoadScale, simMaxLoadScale)
		}
		spec.LoadScale = x
	}

	total := 0
	common, err := treeArray(m["actions"], "actions")
	if err != nil {
		return nil, err
	}
	for i, a := range common {
		act, err := parseSimAction(a, fmt.Sprintf("Action %d", i+1), spec.DurationMin, cat)
		if err != nil {
			return nil, err
		}
		spec.Actions = append(spec.Actions, act)
	}
	total += len(spec.Actions)

	variants, err := treeArray(m["variants"], "variants")
	if err != nil {
		return nil, err
	}
	if len(variants) > simMaxVariants {
		return nil, simBad("At most %d variants are allowed", simMaxVariants)
	}
	names := map[string]bool{}
	for i, raw := range variants {
		vm, ok := raw.(map[string]any)
		if !ok {
			return nil, simBad("Variant %d must be an object", i+1)
		}
		if err := treeKeys(vm, fmt.Sprintf("variant %d", i+1), "name", "actions"); err != nil {
			return nil, err
		}
		name, _ := treeString(vm["name"])
		name = strings.TrimSpace(name)
		if !simVariantName.MatchString(name) {
			return nil, simBad("Variant %d needs a name of 1 to 40 letters, digits, spaces or _ . -", i+1)
		}
		if names[strings.ToLower(name)] {
			return nil, simBad("Variant names must be unique (%s)", name)
		}
		names[strings.ToLower(name)] = true
		list, err := treeArray(vm["actions"], fmt.Sprintf("variant %s actions", name))
		if err != nil {
			return nil, err
		}
		v := simVariant{Name: name, Actions: []simAction{}}
		for j, a := range list {
			act, err := parseSimAction(a, fmt.Sprintf("Action %d of variant %s", j+1, name), spec.DurationMin, cat)
			if err != nil {
				return nil, err
			}
			v.Actions = append(v.Actions, act)
		}
		total += len(v.Actions)
		spec.Variants = append(spec.Variants, v)
	}
	if len(spec.Variants) == 0 {
		spec.Variants = []simVariant{{Name: "baseline", Actions: []simAction{}}}
	}
	if total > simMaxActions {
		return nil, simBad("At most %d actions are allowed (the spec has %d)", simMaxActions, total)
	}
	if err := checkSimModeGating(spec, cat); err != nil {
		return nil, err
	}
	return spec, nil
}

// parseSimAction validates one action against the catalog.
func parseSimAction(raw any, where string, duration int, cat *simCatalog) (simAction, error) {
	var act simAction
	m, ok := raw.(map[string]any)
	if !ok {
		return act, simBad("%s must be an object", where)
	}
	// label / unit / note are descriptive (as in recommendation actions) and dropped.
	if err := treeKeys(m, strings.ToLower(where[:1])+where[1:], "at_min", "kind", "asset", "param", "fault", "value",
		"duration_min", "label", "unit", "note"); err != nil {
		return act, err
	}
	kind, _ := treeString(m["kind"])
	act.Kind = strings.ToLower(strings.TrimSpace(kind))
	if !contains(simActionKinds, act.Kind) {
		return act, simBad("%s has an unknown kind %q (one of %s)", where, kind, strings.Join(simActionKinds, ", "))
	}
	at := int64(0)
	if m["at_min"] != nil {
		var err error
		if at, err = treeWhole(m["at_min"], where+" at_min", false); err != nil {
			return act, err
		}
	}
	if at < 0 || at >= int64(duration) {
		return act, simBad("%s must happen within the run (at_min 0 to %d)", where, duration-1)
	}
	act.AtMin = int(at)
	if m["duration_min"] != nil {
		d, err := treeWhole(m["duration_min"], where+" duration_min", false)
		if err != nil {
			return act, err
		}
		if d < 1 || d > simMaxDuration {
			return act, simBad("%s duration_min must be 1 to %d minutes", where, simMaxDuration)
		}
		di := int(d)
		act.DurationMin = &di
	}

	alias, _ := treeString(m["asset"])
	ref := cat.Assets[strings.ToLower(strings.TrimSpace(alias))]
	if ref == nil || !ref.Visible {
		return act, simBad("%s references an unknown equipment %q", where, alias)
	}
	act.Asset = ref.Alias
	var value *float64
	if m["value"] != nil {
		x, ok := treeNumber(m["value"])
		if !ok {
			return act, simBad("%s value must be a number", where)
		}
		value = &x
	}

	switch act.Kind {
	case "control":
		param, _ := treeString(m["param"])
		param = strings.ToLower(strings.TrimSpace(param))
		ctl := ref.Controls[param]
		if ctl == nil {
			return act, simBad("%s %s is not a writable parameter of %s", where, quoteOrEmpty(param), ref.Alias)
		}
		if value == nil {
			return act, simBad("%s needs a value for %s %s", where, ref.Alias, param)
		}
		if reason := controlValueError(&ctl.twinControl, *value, ctl.Unit); reason != "" {
			return act, simBad("%s (%s %s) %s", where, ref.Alias, param, lowerFirst(reason))
		}
		act.Param, act.Value = param, value
	case "fault", "clear_fault":
		fk, _ := treeString(m["fault"])
		f := twinFault(fk)
		if f == nil {
			return act, simBad("%s has an unknown fault type %q", where, fk)
		}
		if _, ok := f.asset(ref.Alias); !ok {
			return act, simBad("%s %s does not apply to %s", where, f.Label, ref.Alias)
		}
		act.Fault = f.Kind
		if act.Kind == "fault" {
			if value == nil {
				return act, simBad("%s needs an intensity for %s (%s)", where, f.Label, f.intensityList())
			}
			x, ok := f.intensity(*value)
			if !ok {
				return act, simBad("%s %v is not a documented intensity of %s (%s)", where, *value, f.Label, f.intensityList())
			}
			act.Value = &x
		}
	default: // trip, offline, maintenance, release
		if simVirtualTypes[strings.ToLower(ref.TypeName)] {
			return act, simBad("%s %s is a plant controller and cannot %s", where, ref.Alias, simKindVerb(act.Kind))
		}
	}
	return act, nil
}

func simKindVerb(kind string) string {
	switch kind {
	case "trip":
		return "trip"
	case "offline":
		return "go offline"
	case "maintenance":
		return "be taken into maintenance"
	}
	return "be released"
}

// checkSimModeGating refuses set points written while their mode is in
// automatic: the plant (and the runner) would reject them. The mode in effect
// follows the mode's control actions in the common and variant actions as the
// runner applies them (simModeAt), starting from Auto for a nominal start, else
// from the live mode (unknown live modes are not checked).
func checkSimModeGating(spec *simSpec, cat *simCatalog) error {
	type seqAction struct {
		simAction
		where string
	}
	for _, v := range spec.Variants {
		var seq []seqAction
		var acts []simAction
		for i, a := range spec.Actions {
			seq = append(seq, seqAction{a, fmt.Sprintf("Action %d", i+1)})
			acts = append(acts, a)
		}
		for i, a := range v.Actions {
			seq = append(seq, seqAction{a, fmt.Sprintf("Action %d of variant %s", i+1, v.Name)})
			acts = append(acts, a)
		}
		for _, a := range seq {
			if a.Kind != "control" {
				continue
			}
			ref := cat.Assets[strings.ToLower(a.Asset)]
			ctl := ref.Controls[a.Param]
			if ctl == nil || ctl.Mode == nil || *ctl.Mode == "" {
				continue
			}
			mode := *ctl.Mode
			modeCtl := ref.Controls[mode]
			var initial *float64
			switch {
			case spec.Initial == "nominal":
				initial = new(float64)
			case modeCtl != nil && modeCtl.Live != nil:
				initial = modeCtl.Live
			}
			cur := simModeAt(acts, a.Asset, mode, a.AtMin, initial)
			if cur == nil || *cur != 0 {
				continue
			}
			modeName, label := mode, "Manual"
			if modeCtl != nil {
				modeName = modeCtl.Name
				for _, o := range modeCtl.Options {
					if o.Value != 0 {
						label = o.Label
						break
					}
				}
			}
			return simBad("%s sets %s %s while %s is in automatic, add an earlier action that switches it to %s",
				a.where, a.Asset, a.Param, modeName, label)
		}
	}
	return nil
}

// simModeAt returns the value of a control mode in effect for a set point the
// actions write at minute `at` (nil = unknown), in the runner's order of
// simultaneous actions: the reverts of timed mode writes ending at or before
// `at`, then the mode writes at or before `at`, then the set point. initial is
// the mode at the start (nil = unknown); a revert restores the mode in effect
// before its write.
func simModeAt(actions []simAction, asset, mode string, at int, initial *float64) *float64 {
	type change struct {
		at     int
		revert bool
		idx    int // the write (or the write a revert undoes)
	}
	var changes []change
	for i, b := range actions {
		if b.Kind != "control" || b.Asset != asset || b.Param != mode || b.AtMin > at {
			continue
		}
		changes = append(changes, change{at: b.AtMin, idx: i})
		if b.DurationMin != nil && b.AtMin+*b.DurationMin <= at {
			changes = append(changes, change{at: b.AtMin + *b.DurationMin, revert: true, idx: i})
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		x, y := changes[i], changes[j]
		switch {
		case x.at != y.at:
			return x.at < y.at
		case x.revert != y.revert:
			return x.revert
		case x.revert: // reverts in the order of their writes
			return actions[x.idx].AtMin < actions[y.idx].AtMin
		}
		return false
	})
	cur := initial
	before := map[int]*float64{}
	for _, c := range changes {
		if c.revert {
			cur = before[c.idx]
			continue
		}
		before[c.idx] = cur
		cur = actions[c.idx].Value
	}
	return cur
}

// simCatalog loads the equipment and their writable parameters for spec
// validation (with the live value of each, used for mode gating).
func (s *Server) simCatalog(c *Ctx) (*simCatalog, error) {
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return nil, err
	}
	cat := &simCatalog{Assets: map[string]*simAssetRef{}}
	for k, r := range eq {
		cat.Assets[k] = &simAssetRef{Alias: r.Alias, TypeName: r.TypeName, Visible: r.Visible, Controls: map[string]*simControl{}}
	}
	rows, err := s.DB.Query(c, `SELECT lower(e.alias), mp.alias, mp.name, mp.unit, mp.control_min, mp.control_max, mp.control_step,
			mp.control_options, mp.control_mode, mp.control_target, g.alias, ep.tag_name
		FROM asset.equipment_parameters ep JOIN asset.equipment e ON e.id = ep.equipment_id
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id LEFT JOIN dh.gateways g ON g.id = ep.gateway_id
		WHERE mp.is_writable`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, alias string
		var options []byte
		var gateway, tag *string
		ctl := &simControl{}
		if err := rows.Scan(&key, &alias, &ctl.Name, &ctl.Unit, &ctl.Min, &ctl.Max, &ctl.Step, &options, &ctl.Mode, &ctl.Target,
			&gateway, &tag); err != nil {
			return nil, err
		}
		if len(options) > 0 {
			if err := json.Unmarshal(options, &ctl.Options); err != nil {
				return nil, fmt.Errorf("control options of %s: %w", alias, err)
			}
		}
		if s.Hub != nil {
			if v, ok := s.twinNumber(gateway, tag); ok {
				ctl.Live = &v
			}
		}
		if ref := cat.Assets[key]; ref != nil {
			ref.Controls[alias] = ctl
		}
	}
	return cat, rows.Err()
}

// readSimSpec reads and validates the `spec` input.
func (s *Server) readSimSpec(c *Ctx) (*simSpec, error) {
	tree, present, err := formTree(c, "spec")
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, httpx.BadRequest("A scenario spec is required")
	}
	cat, err := s.simCatalog(c)
	if err != nil {
		return nil, err
	}
	return parseSimSpec(tree, time.Now(), c.Location(), cat)
}

// ---------------------------------------------------------------------------
// JSON tree helpers (JSON bodies decode numbers as json.Number, multipart
// forms send every leaf as a string)
// ---------------------------------------------------------------------------

// formTree returns an input as a JSON tree: JSON text is decoded, nested form
// fields are used as sent. present is false when the input is absent or null.
func formTree(c *Ctx, key string) (any, bool, error) {
	v := c.F.Raw(key)
	if v == nil {
		return nil, false, nil
	}
	str, isStr := v.(string)
	if !isStr {
		return v, true, nil
	}
	str = strings.TrimSpace(str)
	if str == "" || str == "null" || str == "undefined" {
		return nil, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(str))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, true, httpx.BadRequestf("Invalid %s (not valid JSON)", key)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, true, httpx.BadRequestf("Invalid %s (not valid JSON)", key)
	}
	if out == nil {
		return nil, false, nil
	}
	return out, true, nil
}

// treeKeys refuses keys outside the allowed set (catches misspelt fields).
func treeKeys(m map[string]any, what string, allowed ...string) error {
	for k := range m {
		if !contains(allowed, k) {
			return simBad("Unknown field %q in %s", k, what)
		}
	}
	return nil
}

func treeNumber(v any) (float64, bool) {
	var f float64
	switch t := v.(type) {
	case json.Number:
		x, err := t.Float64()
		if err != nil {
			return 0, false
		}
		f = x
	case float64:
		f = t
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// treeWhole reads a whole number (minutes, seeds).
func treeWhole(v any, name string, required bool) (int64, error) {
	if v == nil {
		if required {
			return 0, simBad("%s is required", name)
		}
		return 0, nil
	}
	f, ok := treeNumber(v)
	if !ok || f != math.Trunc(f) || math.Abs(f) > simMaxSeed {
		return 0, simBad("%s must be a whole number", name)
	}
	return int64(f), nil
}

func treeBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1":
			return true, true
		case "false", "0":
			return false, true
		}
	case json.Number:
		switch t.String() {
		case "1":
			return true, true
		case "0":
			return false, true
		}
	}
	return false, false
}

func treeString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// treeArray reads an optional array (absent = empty).
func treeArray(v any, name string) ([]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return t, nil
	}
	return nil, simBad("%s must be an array", name)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func quoteOrEmpty(s string) string {
	if s == "" {
		return "(no param)"
	}
	return strconv.Quote(s)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// sqlArgs collects positional query arguments.
type sqlArgs []any

func (a *sqlArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// simJobAliases lists the equipment aliases referenced by the actions of the
// job j (common actions and every variant).
const simJobAliases = `(SELECT a->>'asset' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(j.spec->'actions') = 'array' THEN j.spec->'actions' ELSE '[]'::jsonb END) a
	UNION ALL
	SELECT a->>'asset' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(j.spec->'variants') = 'array' THEN j.spec->'variants' ELSE '[]'::jsonb END) v
	CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(v->'actions') = 'array' THEN v->'actions' ELSE '[]'::jsonb END) a)`

// simVisibleClause restricts jobs (alias j) to those the user may see; "" for
// unrestricted users.
func (s *Server) simVisibleClause(c *Ctx, args *sqlArgs) (string, error) {
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return "", err
	}
	keys, err := s.visibleAliasKeys(c, eq)
	if err != nil || keys == nil {
		return "", err
	}
	return fmt.Sprintf(`(j.requested_by = %s OR NOT EXISTS (SELECT 1 FROM %s AS x(alias)
		WHERE x.alias IS NOT NULL AND NOT (lower(x.alias) = ANY(%s))))`, args.add(c.User.ID), simJobAliases, args.add(keys)), nil
}

const simJobColumns = `j.id, j.kind, j.title, j.origin, j.status, j.progress, j.rationale, j.requested_by, ru.full_name, au.full_name,
	j.approved_at, j.created_at, j.started_at, j.finished_at, j.sim_start, j.sim_end, j.step_s, j.recommendation_id, j.error,
	COALESCE(NULLIF(ARRAY(SELECT v->>'name' FROM jsonb_array_elements(CASE WHEN jsonb_typeof(j.spec->'variants') = 'array'
			THEN j.spec->'variants' ELSE '[]'::jsonb END) WITH ORDINALITY AS t(v, n) WHERE v->>'name' IS NOT NULL ORDER BY n), '{}'),
		ARRAY(SELECT DISTINCT sr.variant FROM sim.series sr WHERE sr.job_id = j.id ORDER BY 1))`

const simJobFrom = ` FROM sim.jobs j LEFT JOIN core.users ru ON ru.id = j.requested_by LEFT JOIN core.users au ON au.id = j.approved_by`

// scanSimJob scans simJobColumns (plus extra destinations) into a JobRow.
func scanSimJob(row pgx.Row, extra ...any) (map[string]any, error) {
	var id int64
	var kind, title, origin, status string
	var progress float64
	var stepS int
	var rationale, requesterName, approverName, errText *string
	var requestedBy, recommendationID *int64
	var approvedAt, startedAt, finishedAt, simStart, simEnd *time.Time
	var createdAt time.Time
	var variants []string
	dest := []any{&id, &kind, &title, &origin, &status, &progress, &rationale, &requestedBy, &requesterName, &approverName,
		&approvedAt, &createdAt, &startedAt, &finishedAt, &simStart, &simEnd, &stepS, &recommendationID, &errText, &variants}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "kind": kind, "title": title, "origin": origin, "status": status, "progress": httpx.Round(progress, 4),
		"rationale": rationale, "requested_by": requestedBy, "requested_by_name": requesterName, "approved_by_name": approverName,
		"approved_at": httpx.TSPtr(approvedAt), "created_at": httpx.TS(createdAt), "started_at": httpx.TSPtr(startedAt),
		"finished_at": httpx.TSPtr(finishedAt), "sim_start": httpx.TSPtr(simStart), "sim_end": httpx.TSPtr(simEnd),
		"step_s": stepS, "recommendation_id": recommendationID, "error": errText, "variants": emptyIfNil(variants),
	}, nil
}

// simJob is the state of a job needed by the handlers.
type simJob struct {
	ID          int64
	Status      string
	RequestedBy *int64
	Spec        json.RawMessage
	StepS       int
	Row         map[string]any
}

// simLoadJob loads a job visible to the user (404 otherwise, so the existence
// of other users' jobs is not revealed).
func (s *Server) simLoadJob(c *Ctx, id int64) (*simJob, error) {
	if id <= 0 {
		return nil, httpx.BadRequest("A job_id is required")
	}
	var args sqlArgs
	where := "j.id = " + args.add(id)
	vis, err := s.simVisibleClause(c, &args)
	if err != nil {
		return nil, err
	}
	if vis != "" {
		where += " AND " + vis
	}
	j := &simJob{}
	j.Row, err = scanSimJob(s.DB.QueryRow(c, `SELECT `+simJobColumns+`, j.spec`+simJobFrom+` WHERE `+where, args...), &j.Spec)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Simulation job not found")
		}
		return nil, err
	}
	j.ID = id
	j.Status, _ = j.Row["status"].(string)
	j.RequestedBy, _ = j.Row["requested_by"].(*int64)
	j.StepS, _ = j.Row["step_s"].(int)
	if j.StepS <= 0 {
		j.StepS = simStepS
	}
	return j, nil
}

func (s *Server) simJobRow(c *Ctx, id int64) (map[string]any, error) {
	return scanSimJob(s.DB.QueryRow(c, `SELECT `+simJobColumns+simJobFrom+` WHERE j.id = $1`, id))
}

// simListJobs lists the visible jobs, newest first. Input: status (CSV),
// origin, limit (1..100, 20), offset.
func (s *Server) simListJobs(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	var args sqlArgs
	conds := []string{"TRUE"}
	if list, ok := c.F.StringList("status"); ok && len(list) > 0 {
		statuses := []string{}
		for _, st := range list {
			st = strings.ToUpper(strings.TrimSpace(st))
			if !contains(simStatuses, st) {
				return nil, httpx.BadRequestf("Unknown job status %q", st)
			}
			statuses = append(statuses, st)
		}
		conds = append(conds, "j.status = ANY("+args.add(statuses)+")")
	}
	if origin := strings.ToLower(c.F.StrTrim("origin")); origin != "" {
		if !contains(simOrigins, origin) {
			return nil, httpx.BadRequestf("Unknown job origin %q", origin)
		}
		conds = append(conds, "j.origin = "+args.add(origin))
	}
	// kind: scenario / doe (chiller plant) or production (fab), comma separated
	if list, ok := c.F.StringList("kind"); ok && len(list) > 0 {
		kinds := []string{}
		for _, k := range list {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "scenario" && k != "doe" && k != "production" {
				return nil, httpx.BadRequestf("Unknown job kind %q", k)
			}
			kinds = append(kinds, k)
		}
		conds = append(conds, "j.kind = ANY("+args.add(kinds)+")")
	}
	vis, err := s.simVisibleClause(c, &args)
	if err != nil {
		return nil, err
	}
	if vis != "" {
		conds = append(conds, vis)
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM sim.jobs j WHERE `+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	limit := c.limit(20, 100)
	offset := c.F.Int("offset", 0)
	if offset < 0 {
		offset = 0
	}
	// (the query text is built before args is passed: the evaluation order of
	// args.add and args... within one call is unspecified)
	q := `SELECT ` + simJobColumns + simJobFrom + ` WHERE ` + where + ` ORDER BY j.created_at DESC, j.id DESC LIMIT ` +
		args.add(limit) + ` OFFSET ` + args.add(offset)
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []map[string]any{}
	for rows.Next() {
		r, err := scanSimJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"total": total, "jobs": jobs}, nil
}

// simGetJob returns a job with its spec and summary. Input: job_id.
func (s *Server) simGetJob(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	var summary json.RawMessage
	var note, session *string
	if err := s.DB.QueryRow(c, `SELECT summary, decision_note, agent_session_id FROM sim.jobs WHERE id = $1`, j.ID).
		Scan(&summary, &note, &session); err != nil {
		return nil, err
	}
	row := j.Row
	row["spec"], row["summary"], row["decision_note"], row["agent_session_id"] = j.Spec, summary, note, session
	return row, nil
}

// simNewJob is a job to insert: a scenario (Spec) or another kind with its
// validated spec as JSON (Raw).
type simNewJob struct {
	Kind         string
	Raw          json.RawMessage
	StepS        int
	Title        string
	Spec         *simSpec
	Rationale    *string
	Origin       string
	Status       string
	AgentSession *string
}

// simCreateJob inserts a job after the per-user limits (serialised per user
// with a transaction-scoped advisory lock) and notifies sim_jobs.
func (s *Server) simCreateJob(c *Ctx, nj simNewJob) (int64, error) {
	kind, stepS := "scenario", simStepS
	if nj.Kind != "" {
		kind = nj.Kind
	}
	if nj.StepS > 0 {
		stepS = nj.StepS
	}
	spec := []byte(nj.Raw)
	var err error
	if nj.Raw == nil {
		if spec, err = json.Marshal(nj.Spec); err != nil {
			return 0, err
		}
	}
	var simStart, simEnd *time.Time
	if nj.Spec != nil && nj.Spec.Start != nil {
		if t, err := time.Parse(time.RFC3339Nano, *nj.Spec.Start); err == nil {
			end := t.Add(time.Duration(nj.Spec.DurationMin) * time.Minute)
			simStart, simEnd = &t, &end
		}
	}
	var id int64
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := simLockUser(c, tx, &c.User.ID); err != nil {
			return err
		}
		switch nj.Status {
		case simQueued:
			if err := simCheckActive(c, tx, c.User.ID); err != nil {
				return err
			}
		case simProposed:
			// per person, whatever the (caller-chosen) agent session
			var n int
			if err := tx.QueryRow(c, `SELECT count(*) FROM sim.jobs WHERE requested_by = $1 AND status = 'PROPOSED'`,
				c.User.ID).Scan(&n); err != nil {
				return err
			}
			if n >= simMaxProposed {
				return httpx.Conflict(fmt.Sprintf("You already have %d proposed simulations waiting for approval, "+
					"approve or reject them first", simMaxProposed))
			}
		}
		if err := tx.QueryRow(c, `INSERT INTO sim.jobs (kind, title, origin, status, spec, rationale, requested_by, agent_session_id,
				sim_start, sim_end, step_s) VALUES ($11, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			nj.Title, nj.Origin, nj.Status, spec, nj.Rationale, c.User.ID, nj.AgentSession, simStart, simEnd, stepS, kind).Scan(&id); err != nil {
			return err
		}
		return simNotify(c, tx, id)
	})
	return id, err
}

// simLockUser serialises the limit checks and inserts of one user.
func simLockUser(c *Ctx, tx pgx.Tx, userID *int64) error {
	var key int64
	if userID != nil {
		key = *userID
	}
	_, err := tx.Exec(c, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "afdd:sim_jobs:"+strconv.FormatInt(key, 10))
	return err
}

// simCheckActive enforces the QUEUED + RUNNING limit of a user.
func simCheckActive(c *Ctx, tx pgx.Tx, userID int64) error {
	var n int
	if err := tx.QueryRow(c, `SELECT count(*) FROM sim.jobs WHERE requested_by = $1 AND status IN ('QUEUED', 'RUNNING')`, userID).Scan(&n); err != nil {
		return err
	}
	if n >= simMaxActive {
		return httpx.Conflict(fmt.Sprintf("At most %d simulations can be queued or running per person, wait for one to finish", simMaxActive))
	}
	return nil
}

func simNotify(c *Ctx, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(c, `SELECT pg_notify('sim_jobs', $1::text)`, strconv.FormatInt(id, 10))
	return err
}

// readSimText reads a trimmed text input with a length limit.
func readSimText(c *Ctx, key, label string, required bool, max int) (*string, error) {
	v := strings.TrimSpace(c.F.Str(key))
	if v == "" {
		if required {
			return nil, httpx.BadRequestf("A %s is required", label)
		}
		return nil, nil
	}
	if len([]rune(v)) > max {
		return nil, httpx.BadRequestf("The %s is too long (at most %d characters)", label, max)
	}
	return &v, nil
}

// simSubmit queues a scenario from the UI. Input: title, spec, rationale?.
// The AI agent cannot submit (it proposes, and a person approves).
func (s *Server) simSubmit(c *Ctx) (any, error) {
	if err := c.Require("simulation", "create"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	if k := strings.ToLower(c.F.StrTrim("kind")); k != "" && k != "scenario" {
		return nil, httpx.BadRequest("Only scenario simulations can be submitted")
	}
	title, err := readSimText(c, "title", "title", true, simMaxTitle)
	if err != nil {
		return nil, err
	}
	rationale, err := readSimText(c, "rationale", "rationale", false, simMaxRationale)
	if err != nil {
		return nil, err
	}
	spec, err := s.readSimSpec(c)
	if err != nil {
		return nil, err
	}
	id, err := s.simCreateJob(c, simNewJob{Title: *title, Spec: spec, Rationale: rationale, Origin: "ui", Status: simQueued})
	if err != nil {
		return nil, err
	}
	return s.simJobRow(c, id)
}

// simPropose records a scenario proposed by the AI agent for approval (the
// agent path only: people submit). Input: title, spec, rationale,
// agent_session_id?.
func (s *Server) simPropose(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	if !c.fromAgent() {
		return nil, httpx.Forbidden("Simulations are proposed by the AI assistant, submit your own simulation instead")
	}
	title, err := readSimText(c, "title", "title", true, simMaxTitle)
	if err != nil {
		return nil, err
	}
	rationale, err := readSimText(c, "rationale", "rationale", true, simMaxRationale)
	if err != nil {
		return nil, err
	}
	session, err := readSimText(c, "agent_session_id", "agent session id", false, 200)
	if err != nil {
		return nil, err
	}
	spec, err := s.readSimSpec(c)
	if err != nil {
		return nil, err
	}
	id, err := s.simCreateJob(c, simNewJob{Title: *title, Spec: spec, Rationale: rationale, Origin: "agent", Status: simProposed,
		AgentSession: session})
	if err != nil {
		return nil, err
	}
	return s.simJobRow(c, id)
}

// simApprove queues a proposed job (a person only). Input: job_id.
func (s *Server) simApprove(c *Ctx) (any, error) {
	if err := c.Require("simulation", "create"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	if _, ok := simTransition("approve", j.Status); !ok {
		return nil, httpx.Conflict(fmt.Sprintf("Only a proposed simulation can be approved (this one is %s)", j.Status))
	}
	// A proposal with a fixed start can't run once the start has passed: the
	// runner starts from the live state.
	var sp struct {
		Start *string `json:"start"`
	}
	if err := json.Unmarshal(j.Spec, &sp); err == nil && sp.Start != nil {
		if t, err := time.Parse(time.RFC3339Nano, *sp.Start); err == nil && t.Before(time.Now().Add(-simStartSlack)) {
			return nil, httpx.Conflict("The scenario start has passed, propose it again with a new start")
		}
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := simLockUser(c, tx, j.RequestedBy); err != nil {
			return err
		}
		if j.RequestedBy != nil {
			if err := simCheckActive(c, tx, *j.RequestedBy); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(c, `UPDATE sim.jobs SET status = 'QUEUED', approved_by = $2, approved_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'PROPOSED'`, j.ID, c.User.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.Conflict("The simulation was changed in the meantime, reload it")
		}
		return simNotify(c, tx, j.ID)
	})
	if err != nil {
		return nil, err
	}
	return s.simJobRow(c, j.ID)
}

// simReject rejects a proposed job (a person only). Input: job_id, reason?.
// approved_by / approved_at record the person who decided.
func (s *Server) simReject(c *Ctx) (any, error) {
	if err := c.Require("simulation", "create"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	return s.simDecide(c, "reject")
}

// simCancel cancels a proposed or queued job (a person only: the requester or
// an administrator). Input: job_id, reason?.
func (s *Server) simCancel(c *Ctx) (any, error) {
	if err := c.Require("simulation", "create"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	return s.simDecide(c, "cancel")
}

// simCancelAllowed checks who may cancel a job: its requester or an
// administrator; the optimizer's and the analytics service's own jobs only an
// administrator.
func simCancelAllowed(u *auth.User, origin string, requestedBy *int64) error {
	if u.IsAdmin() {
		return nil
	}
	if origin == "optimizer" || origin == "analytics" {
		return httpx.Forbidden("Only an administrator can cancel the simulations of the AI services")
	}
	if requestedBy == nil || *requestedBy != u.ID {
		return httpx.Forbidden("Only the person who requested this simulation or an administrator can cancel it")
	}
	return nil
}

func (s *Server) simDecide(c *Ctx, action string) (any, error) {
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	if action == "cancel" {
		origin, _ := j.Row["origin"].(string)
		if err := simCancelAllowed(c.User, origin, j.RequestedBy); err != nil {
			return nil, err
		}
	}
	note, err := readSimText(c, "reason", "reason", false, simMaxNote)
	if err != nil {
		return nil, err
	}
	to, ok := simTransition(action, j.Status)
	if !ok {
		allowed := strings.ToLower(strings.Join(simTransitions[action].from, " or "))
		return nil, httpx.Conflict(fmt.Sprintf("Only a %s simulation can be %s (this one is %s)", allowed,
			simDone[action], j.Status))
	}
	from := simTransitions[action].from
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		if action == "reject" {
			tag, err = tx.Exec(c, `UPDATE sim.jobs SET status = $2, approved_by = $4, approved_at = now(), decision_note = $5, updated_at = now()
				WHERE id = $1 AND status = ANY($3)`, j.ID, to, from, c.User.ID, note)
		} else {
			tag, err = tx.Exec(c, `UPDATE sim.jobs SET status = $2, decision_note = COALESCE($4, decision_note), finished_at = now(),
				updated_at = now() WHERE id = $1 AND status = ANY($3)`, j.ID, to, from, note)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.Conflict("The simulation was changed in the meantime, reload it")
		}
		return simNotify(c, tx, j.ID)
	})
	if err != nil {
		return nil, err
	}
	return s.simJobRow(c, j.ID)
}

// simFaultCatalog lists the LBNL faults a scenario can inject, restricted to
// the visible equipment.
func (s *Server) simFaultCatalog(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return nil, err
	}
	return visibleFaultCatalog(func(alias string) bool {
		r := eq[strings.ToLower(alias)]
		return r != nil && r.Visible
	}), nil
}

// ---------------------------------------------------------------------------
// Results: events, frames (replay), trends
// ---------------------------------------------------------------------------

// simVariantOf returns the requested variant (case-insensitive) or the first
// one of the job.
func simVariantOf(j *simJob, requested string) (string, error) {
	variants, _ := j.Row["variants"].([]string)
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(variants) == 0 {
			return "", httpx.NotFound("The simulation has no results yet")
		}
		return variants[0], nil
	}
	for _, v := range variants {
		if strings.EqualFold(v, requested) {
			return v, nil
		}
	}
	return "", httpx.NotFound(fmt.Sprintf("Unknown variant %q", requested))
}

// simAliasFilter returns the lower-case aliases the result queries are limited
// to: the requested ones (CSV / array) intersected with the visible ones. nil
// means no restriction.
func (s *Server) simAliasFilter(c *Ctx, key string) ([]string, map[string]*equipmentRef, error) {
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return nil, nil, err
	}
	visible, err := s.visibleAliasKeys(c, eq)
	if err != nil {
		return nil, nil, err
	}
	list, present := c.F.StringList(key)
	if !present || len(list) == 0 {
		return visible, eq, nil
	}
	out := []string{}
	for _, a := range list {
		k := strings.ToLower(strings.TrimSpace(a))
		if k == "" || contains(out, k) {
			continue
		}
		if visible != nil && !contains(visible, k) {
			continue
		}
		out = append(out, k)
	}
	return out, eq, nil
}

type simEventFilter struct {
	JobID   int64
	Variant string
	Kinds   []string
	Aliases []string // lower-case; nil = all
	Limit   int64
	Offset  int64
}

// simEvents returns the matching events (in time order) and their total.
// Plant-level events (no equipment) are always included.
func (s *Server) simEvents(c *Ctx, f simEventFilter) (int, []map[string]any, error) {
	var args sqlArgs
	conds := []string{"e.job_id = " + args.add(f.JobID)}
	if f.Variant != "" {
		conds = append(conds, "e.variant = "+args.add(f.Variant))
	}
	if len(f.Kinds) > 0 {
		conds = append(conds, "e.kind = ANY("+args.add(f.Kinds)+")")
	}
	if f.Aliases != nil {
		conds = append(conds, "(e.asset_alias IS NULL OR lower(e.asset_alias) = ANY("+args.add(f.Aliases)+"))")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM sim.events e WHERE `+where, args...).Scan(&total); err != nil {
		return 0, nil, err
	}
	q := `SELECT e.id, e.variant, e.ts, e.end_ts, e.asset_alias, e.kind, e.code, e.severity, e.message, e.value, e.details
		FROM sim.events e WHERE ` + where + ` ORDER BY e.ts, e.id LIMIT ` + args.add(f.Limit) + ` OFFSET ` + args.add(f.Offset)
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var variant, kind, message string
		var ts time.Time
		var end *time.Time
		var alias, code, severity *string
		var value *float64
		var details json.RawMessage
		if err := rows.Scan(&id, &variant, &ts, &end, &alias, &kind, &code, &severity, &message, &value, &details); err != nil {
			return 0, nil, err
		}
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			value = nil
		}
		out = append(out, map[string]any{"id": id, "variant": variant, "ts": httpx.TS(ts), "end_ts": httpx.TSPtr(end),
			"asset_alias": alias, "kind": kind, "code": code, "severity": severity, "message": message, "value": value, "details": details})
	}
	return total, out, rows.Err()
}

// simGetEvents lists the events of a job. Input: job_id, variant?, kinds?
// (CSV), asset?, limit (1..1000, 200), offset.
func (s *Server) simGetEvents(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	f := simEventFilter{JobID: j.ID, Limit: c.limit(200, 1000), Offset: c.F.Int("offset", 0)}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if v := c.F.StrTrim("variant"); v != "" {
		if f.Variant, err = simVariantOf(j, v); err != nil {
			return nil, err
		}
	}
	if list, ok := c.F.StringList("kinds"); ok {
		for _, k := range list {
			k = strings.ToLower(strings.TrimSpace(k))
			if !contains(simEventKinds, k) {
				return nil, httpx.BadRequestf("Unknown event kind %q", k)
			}
			f.Kinds = append(f.Kinds, k)
		}
	}
	if f.Aliases, _, err = s.simAliasFilter(c, "asset"); err != nil {
		return nil, err
	}
	total, events, err := s.simEvents(c, f)
	if err != nil {
		return nil, err
	}
	return map[string]any{"total": total, "events": events}, nil
}

// simSeriesRow is one sim.series row (measured values only).
type simSeriesRow struct {
	Asset    string
	Param    string
	T0       time.Time
	StepS    int
	Measured []*float32
}

// assembleFrames aligns series on one time grid: t0 = the earliest series
// start, the step of the first series (or defStep), every value at index
// round((t − t0) / step). Missing samples and non-finite values are null.
func assembleFrames(rows []simSeriesRow, defStep int) (time.Time, int, int, map[string]map[string][]*float32) {
	series := map[string]map[string][]*float32{}
	if len(rows) == 0 {
		return time.Time{}, defStep, 0, series
	}
	step := rows[0].StepS
	if step <= 0 {
		step = defStep
	}
	t0 := rows[0].T0
	for _, r := range rows[1:] {
		if r.T0.Before(t0) {
			t0 = r.T0
		}
	}
	stepD := time.Duration(step) * time.Second
	index := func(r simSeriesRow, i int) int {
		rs := r.StepS
		if rs <= 0 {
			rs = step
		}
		t := r.T0.Add(time.Duration(i) * time.Duration(rs) * time.Second)
		return int(math.Round(float64(t.Sub(t0)) / float64(stepD)))
	}
	steps := 0
	for _, r := range rows {
		if n := len(r.Measured); n > 0 {
			if last := index(r, n-1) + 1; last > steps {
				steps = last
			}
		}
	}
	for _, r := range rows {
		vals := make([]*float32, steps)
		for i, v := range r.Measured {
			if v == nil || math.IsNaN(float64(*v)) || math.IsInf(float64(*v), 0) {
				continue
			}
			if k := index(r, i); k >= 0 && k < steps {
				x := *v
				vals[k] = &x
			}
		}
		if series[r.Asset] == nil {
			series[r.Asset] = map[string][]*float32{}
		}
		series[r.Asset][r.Param] = vals
	}
	return t0, step, steps, series
}

// simLoadSeries loads the measured series of a job variant (aliases nil = all).
func (s *Server) simLoadSeries(c *Ctx, jobID int64, variant string, aliases []string, param string) ([]simSeriesRow, error) {
	var args sqlArgs
	q := `SELECT asset_alias, param_alias, t0, step_s, measured FROM sim.series WHERE job_id = ` + args.add(jobID) +
		` AND variant = ` + args.add(variant)
	if aliases != nil {
		q += ` AND lower(asset_alias) = ANY(` + args.add(aliases) + `)`
	}
	if param != "" {
		q += ` AND param_alias = ` + args.add(param)
	}
	rows, err := s.DB.Query(c, q+` ORDER BY asset_alias, param_alias`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []simSeriesRow
	for rows.Next() {
		var r simSeriesRow
		if err := rows.Scan(&r.Asset, &r.Param, &r.T0, &r.StepS, &r.Measured); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// simCheckFrameSize refuses a frames request of more than simFrameMaxPoints
// values (series x steps of the run), estimated before the series are read.
// selected tells whether the caller chose the units.
func (s *Server) simCheckFrameSize(c *Ctx, jobID int64, variant string, aliases []string, selected bool) error {
	var args sqlArgs
	q := `SELECT count(*), COALESCE(sum(EXTRACT(EPOCH FROM j.sim_end - j.sim_start) / GREATEST(sr.step_s, 1)), 0)::float8
		FROM sim.series sr JOIN sim.jobs j ON j.id = sr.job_id WHERE sr.job_id = ` + args.add(jobID) + ` AND sr.variant = ` + args.add(variant)
	if aliases != nil {
		q += ` AND lower(sr.asset_alias) = ANY(` + args.add(aliases) + `)`
	}
	var series int64
	var points float64
	if err := s.DB.QueryRow(c, q, args...).Scan(&series, &points); err != nil {
		return err
	}
	if points <= simFrameMaxPoints {
		return nil
	}
	if selected {
		return httpx.BadRequestf("The selected units have %d series (about %.1f million values), select fewer units", series, points/1e6)
	}
	return httpx.BadRequestf("The run has %d series (about %.1f million values), select the units to replay with aliases", series, points/1e6)
}

// simGetFrames returns the replay frames of a job variant: every measured
// series on one time grid, the status / condition intervals and the events.
// Input: job_id, variant? (default the first), aliases? (CSV).
func (s *Server) simGetFrames(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	variant, err := simVariantOf(j, c.F.StrTrim("variant"))
	if err != nil {
		return nil, err
	}
	aliases, eq, err := s.simAliasFilter(c, "aliases")
	if err != nil {
		return nil, err
	}
	list, _ := c.F.StringList("aliases")
	if err := s.simCheckFrameSize(c, j.ID, variant, aliases, len(list) > 0); err != nil {
		return nil, err
	}
	rows, err := s.simLoadSeries(c, j.ID, variant, aliases, "")
	if err != nil {
		return nil, err
	}
	t0, step, steps, series := assembleFrames(rows, j.StepS)

	assets := []map[string]any{}
	names := make([]string, 0, len(series))
	for alias := range series {
		names = append(names, alias)
	}
	sort.Strings(names)
	for _, alias := range names {
		a := map[string]any{"alias": alias, "id": nil, "name": alias, "type_name": nil, "model_name": nil}
		if r := eq[strings.ToLower(alias)]; r != nil {
			a["id"], a["name"], a["type_name"], a["model_name"] = r.ID, r.Name, r.TypeName, r.ModelName
		}
		assets = append(assets, a)
	}

	var args sqlArgs
	q := `SELECT asset_alias, kind, value, start_ts, end_ts FROM sim.states WHERE job_id = ` + args.add(j.ID) + ` AND variant = ` + args.add(variant)
	if aliases != nil {
		q += ` AND lower(asset_alias) = ANY(` + args.add(aliases) + `)`
	}
	srows, err := s.DB.Query(c, q+` ORDER BY start_ts, id`, args...)
	if err != nil {
		return nil, err
	}
	states := []map[string]any{}
	for srows.Next() {
		var alias, kind, value string
		var start time.Time
		var end *time.Time
		if err := srows.Scan(&alias, &kind, &value, &start, &end); err != nil {
			srows.Close()
			return nil, err
		}
		states = append(states, map[string]any{"alias": alias, "kind": kind, "value": value, "start": httpx.TS(start), "end": httpx.TSPtr(end)})
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	_, events, err := s.simEvents(c, simEventFilter{JobID: j.ID, Variant: variant, Aliases: aliases, Limit: simFrameMaxEvents})
	if err != nil {
		return nil, err
	}
	var start any
	if steps > 0 {
		start = httpx.TS(t0)
	}
	return map[string]any{"job_id": j.ID, "variant": variant, "t0": start, "step_s": step, "steps": steps, "assets": assets,
		"series": series, "states": states, "events": events}, nil
}

// bucketPoints averages points into absolute-aligned buckets when there are
// more than maxPoints (as seriesBuckets does for logged values).
func bucketPoints(pts []point, from, to time.Time, maxPoints int) ([]point, time.Duration) {
	if maxPoints <= 0 || len(pts) <= maxPoints {
		return pts, 0
	}
	bucket := to.Sub(from) / time.Duration(maxPoints)
	if bucket <= 0 {
		return pts, 0
	}
	var out []point
	var sum float64
	var n int
	cur := pts[0].ts.Truncate(bucket)
	flush := func() {
		if n > 0 {
			v := sum / float64(n)
			out = append(out, point{ts: cur, val: &v})
		}
		sum, n = 0, 0
	}
	for _, p := range pts {
		if b := p.ts.Truncate(bucket); !b.Equal(cur) {
			flush()
			cur = b
		}
		if p.val != nil {
			sum += *p.val
			n++
		}
	}
	flush()
	return out, bucket
}

// simStatusSpans converts sim.states status values (status names such as
// RUNNING, or numeric status ids) into status intervals.
func simStatusSpans(values []string, starts []time.Time, ends []*time.Time) []statusSpan {
	out := make([]statusSpan, 0, len(values))
	for i, v := range values {
		st := 0
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			st = n
		} else if strings.EqualFold(strings.TrimSpace(v), engine.StatusName(engine.StatusRunning)) {
			st = engine.StatusRunning
		}
		out = append(out, statusSpan{status: st, start: starts[i], end: ends[i]})
	}
	return out
}

// epByAlias loads an equipment parameter by equipment and parameter alias
// (scope checked).
func (s *Server) epByAlias(c *Ctx, assetAlias, paramAlias string) (*epInfo, error) {
	var epid int64
	err := s.DB.QueryRow(c, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.equipment e ON e.id = ep.equipment_id
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id WHERE lower(e.alias) = lower($1) AND mp.alias = $2`,
		strings.TrimSpace(assetAlias), strings.TrimSpace(paramAlias)).Scan(&epid)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_parameter_is_not_found")
		}
		return nil, err
	}
	return s.loadEP(c, epid)
}

// simGetTrend is digital_twin/getparametertrend on a simulated series: the
// window of `minutes` ending at `at` (default the end of the run), bucket
// averaged to at most 360 points, with the same running flags (from the
// variant's simulated status intervals), SPC limits and rule checks.
// Input: job_id, variant, asset_alias, param_alias, at?, minutes (5..1440, 60).
func (s *Server) simGetTrend(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	j, err := s.simLoadJob(c, c.F.Int("job_id", 0))
	if err != nil {
		return nil, err
	}
	variant, err := simVariantOf(j, c.F.StrTrim("variant"))
	if err != nil {
		return nil, err
	}
	assetAlias, paramAlias := c.F.StrTrim("asset_alias"), c.F.StrTrim("param_alias")
	if assetAlias == "" || paramAlias == "" {
		return nil, httpx.BadRequest("asset_alias and param_alias are required")
	}
	p, err := s.epByAlias(c, assetAlias, paramAlias) // also checks the scope
	if err != nil {
		return nil, err
	}
	minutes := c.F.Int("minutes", 60)
	if minutes < 5 {
		minutes = 5
	}
	if minutes > 1440 {
		minutes = 1440
	}
	rows, err := s.simLoadSeries(c, j.ID, variant, []string{strings.ToLower(assetAlias)}, p.Alias)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, httpx.NotFound("The simulation has no series for this parameter")
	}
	r := rows[0]
	step := time.Duration(r.StepS) * time.Second
	if step <= 0 {
		step = time.Duration(j.StepS) * time.Second
	}
	end := r.T0
	if n := len(r.Measured); n > 0 {
		end = r.T0.Add(time.Duration(n-1) * step)
	}
	to := end
	if at := c.timeParam("at"); at != nil {
		to = *at
		if to.Before(r.T0) {
			to = r.T0
		}
		if to.After(end) {
			to = end
		}
	}
	from := to.Add(-time.Duration(minutes) * time.Minute)
	var pts []point
	for i, v := range r.Measured {
		t := r.T0.Add(time.Duration(i) * step)
		if t.Before(from) || t.After(to) {
			continue
		}
		var val *float64
		if v != nil && !math.IsNaN(float64(*v)) && !math.IsInf(float64(*v), 0) {
			x := float64(*v)
			val = &x
		}
		pts = append(pts, point{ts: t, val: val})
	}
	pts, bucket := bucketPoints(pts, from, to, simTrendMaxPoints)

	srows, err := s.DB.Query(c, `SELECT value, start_ts, end_ts FROM sim.states WHERE job_id = $1 AND variant = $2 AND kind = 'status'
		AND lower(asset_alias) = lower($3) AND start_ts <= $5 AND (end_ts IS NULL OR end_ts > $4) ORDER BY start_ts, id`,
		j.ID, variant, assetAlias, from, to)
	if err != nil {
		return nil, err
	}
	var values []string
	var starts []time.Time
	var ends []*time.Time
	for srows.Next() {
		var v string
		var st time.Time
		var en *time.Time
		if err := srows.Scan(&v, &st, &en); err != nil {
			srows.Close()
			return nil, err
		}
		values, starts, ends = append(values, v), append(starts, st), append(ends, en)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}
	out := twinTrend(p, minutes, pts, runningFlags(pts, bucket, simStatusSpans(values, starts, ends)))
	out["job_id"], out["variant"], out["asset_alias"], out["param_alias"], out["at"] = j.ID, variant, assetAlias, p.Alias, httpx.TS(to)
	return out, nil
}
