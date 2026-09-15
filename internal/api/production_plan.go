package api

import (
	"encoding/json"
	"math"
	"strings"

	"afdd/internal/httpx"
)

// Production planning simulations (docs/production/DESIGN.md section 6):
// sim.jobs of kind 'production' that run the production model of a copy of
// the fab under up to three production plans. The simulator's job runner
// claims and completes them (it analyses them itself); listing, approval,
// rejection and cancellation use the simulation endpoints. People submit
// plans, the AI assistant only proposes them for a person to approve.

const (
	planMaxDays     = 90
	planMaxActions  = 60
	planMaxVariants = 3
	planStepS       = 300
)

// planDefaults is the production plan the live fab runs (the simulator's
// defaultFabConfig).
var planDefaults = map[string]any{"starts_per_day": 4.5, "hot_share": 0.05, "dispatch": "FIFO", "furnace_wait_h": 10.0, "furnace_min_lots": 3,
	"vehicles": 10}

func (s *Server) registerProductionPlan() {
	s.handle("production/getplanning", AccessUser, s.planOptions)
	s.handle("production/submitplan", AccessUser, s.planSubmit)
	s.handle("production/proposeplan", AccessUser, s.planPropose)
}

// planOptions returns what a production plan can use: the current plan, the
// products with their mix and planned cycle time, the tools by area and the
// limits.
func (s *Server) planOptions(c *Ctx) (any, error) {
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	products := []map[string]any{}
	rows, err := s.DB.Query(c, `SELECT code, name, technology, mix, COALESCE(masks, layers), metals, cycle_days, ideal_h FROM prod.products ORDER BY code`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code, name, tech string
		var mix float64
		var masks int64
		var metals *int64
		var cycle, ideal *float64
		if err := rows.Scan(&code, &name, &tech, &mix, &masks, &metals, &cycle, &ideal); err != nil {
			rows.Close()
			return nil, err
		}
		products = append(products, map[string]any{"code": code, "name": name, "technology": tech, "mix": mix, "masks": masks, "metals": metals,
			"cycle_days": cycle, "ideal_h": ideal})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for _, t := range tools {
		list = append(list, map[string]any{"alias": t.Alias, "name": t.Name, "area": t.Type, "area_name": prodAreaNames[t.Type]})
	}
	return map[string]any{"defaults": planDefaults, "products": products, "tools": list,
		"limits": map[string]any{"max_horizon_days": planMaxDays, "max_variants": planMaxVariants, "max_actions": planMaxActions,
			"starts_per_day": []float64{0, 12}, "hot_share": []float64{0, 0.5}, "furnace_wait_h": []float64{0, 48}, "furnace_min_lots": []int{1, 4},
			"vehicles": []int{1, 30}, "dispatch": []string{"FIFO", "CR"}}}, nil
}

// planSpec is a validated production plan spec (the simulator's ProdSpec).
type planSpec struct {
	Kind        string        `json:"kind"`
	HorizonDays float64       `json:"horizon_days"`
	Initial     string        `json:"initial"`
	Seed        int64         `json:"seed"`
	Breakdowns  bool          `json:"breakdowns"`
	Plan        planPlan      `json:"plan"`
	Actions     []planAction  `json:"actions"`
	Variants    []planVariant `json:"variants"`
}

type planPlan struct {
	StartsPerDay   *float64           `json:"starts_per_day,omitempty"`
	Mix            map[string]float64 `json:"mix,omitempty"`
	HotShare       *float64           `json:"hot_share,omitempty"`
	Dispatch       string             `json:"dispatch,omitempty"`
	FurnaceWaitH   *float64           `json:"furnace_wait_h,omitempty"`
	FurnaceMinLots *int               `json:"furnace_min_lots,omitempty"`
	Vehicles       *int               `json:"vehicles,omitempty"` // OHT fleet size
}

type planAction struct {
	Kind      string  `json:"kind"`
	Asset     string  `json:"asset"`
	AtDay     float64 `json:"at_day,omitempty"`
	DurationH float64 `json:"duration_h,omitempty"`
	Count     int     `json:"count,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

type planVariant struct {
	Name    string       `json:"name"`
	Plan    planPlan     `json:"plan"`
	Actions []planAction `json:"actions"`
}

// readPlanSpec validates the spec input against the user's tools and the
// products.
func (s *Server) readPlanSpec(c *Ctx) (*planSpec, error) {
	tree, present, err := formTree(c, "spec")
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, httpx.BadRequest("A production plan spec is required")
	}
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, httpx.Forbidden("You do not see any production tool")
	}
	aliases := map[string]string{}
	for _, t := range tools {
		aliases[strings.ToLower(t.Alias)] = t.Alias
	}
	var products []string
	rows, err := s.DB.Query(c, `SELECT code FROM prod.products`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			rows.Close()
			return nil, err
		}
		products = append(products, code)
	}
	rows.Close()
	return parsePlanSpec(tree, aliases, products)
}

// parsePlanSpec validates a production plan spec given as a JSON tree.
func parsePlanSpec(tree any, aliases map[string]string, products []string) (*planSpec, error) {
	m, ok := tree.(map[string]any)
	if !ok {
		return nil, simBad("The production plan spec must be a JSON object")
	}
	if err := treeKeys(m, "the production plan spec", "kind", "horizon_days", "initial", "seed", "breakdowns", "plan", "actions", "variants"); err != nil {
		return nil, err
	}
	if v, ok := treeString(m["kind"]); ok && v != "" && !strings.EqualFold(v, "production") {
		return nil, simBad("This is a production plan (kind production, not %s)", v)
	}
	spec := &planSpec{Kind: "production", Initial: "live", Breakdowns: true, Actions: []planAction{}, Variants: []planVariant{}}
	h, ok := treeNumber(m["horizon_days"])
	if !ok || h < 1 || h > planMaxDays {
		return nil, simBad("horizon_days must be 1 to %d days", planMaxDays)
	}
	spec.HorizonDays = math.Round(h*100) / 100
	if v := m["initial"]; v != nil {
		str, _ := treeString(v)
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "", "live":
		case "nominal":
			spec.Initial = "nominal"
		default:
			return nil, simBad("The initial state must be live (the fab as it is now) or nominal")
		}
	}
	if m["seed"] != nil {
		seed, err := treeWhole(m["seed"], "seed", false)
		if err != nil {
			return nil, err
		}
		if seed < 0 {
			return nil, simBad("The seed must not be negative")
		}
		spec.Seed = seed
	}
	if v := m["breakdowns"]; v != nil {
		b, ok := treeBool(v)
		if !ok {
			return nil, simBad("breakdowns must be true or false")
		}
		spec.Breakdowns = b
	}
	var err error
	if spec.Plan, err = parsePlan(m["plan"], "the plan", products); err != nil {
		return nil, err
	}
	if spec.Actions, err = parsePlanActions(m["actions"], "the actions", spec.HorizonDays, aliases); err != nil {
		return nil, err
	}
	n := len(spec.Actions)
	vs, err := treeArray(m["variants"], "variants")
	if err != nil {
		return nil, err
	}
	if len(vs) > planMaxVariants {
		return nil, simBad("At most %d variants", planMaxVariants)
	}
	seen := map[string]bool{}
	for i, x := range vs {
		vm, ok := x.(map[string]any)
		if !ok {
			return nil, simBad("Variant %d must be an object", i+1)
		}
		if err := treeKeys(vm, "a variant", "name", "plan", "actions"); err != nil {
			return nil, err
		}
		name, _ := treeString(vm["name"])
		name = strings.TrimSpace(name)
		if !simVariantName.MatchString(name) {
			return nil, simBad("Variant %d needs a name of 1 to 40 letters, digits, spaces, '_', '.' or '-'", i+1)
		}
		if seen[strings.ToLower(name)] {
			return nil, simBad("Variant names must be unique (%s)", name)
		}
		seen[strings.ToLower(name)] = true
		v := planVariant{Name: name, Actions: []planAction{}}
		if v.Plan, err = parsePlan(vm["plan"], "the plan of "+name, products); err != nil {
			return nil, err
		}
		if v.Actions, err = parsePlanActions(vm["actions"], "the actions of "+name, spec.HorizonDays, aliases); err != nil {
			return nil, err
		}
		n += len(v.Actions)
		spec.Variants = append(spec.Variants, v)
	}
	if len(spec.Variants) == 0 {
		spec.Variants = []planVariant{{Name: "Plan", Actions: []planAction{}}}
	}
	if n > planMaxActions {
		return nil, simBad("At most %d actions in a production plan", planMaxActions)
	}
	return spec, nil
}

func parsePlan(v any, what string, products []string) (planPlan, error) {
	var p planPlan
	if v == nil {
		return p, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return p, simBad("%s must be an object", what)
	}
	if err := treeKeys(m, what, "starts_per_day", "mix", "hot_share", "dispatch", "furnace_wait_h", "furnace_min_lots", "vehicles"); err != nil {
		return p, err
	}
	num := func(key string, lo, hi float64) (*float64, error) {
		if m[key] == nil {
			return nil, nil
		}
		f, ok := treeNumber(m[key])
		if !ok || f < lo || f > hi {
			return nil, simBad("%s: %s must be %g to %g", what, key, lo, hi)
		}
		return &f, nil
	}
	var err error
	if p.StartsPerDay, err = num("starts_per_day", 0, 12); err != nil {
		return p, err
	}
	if p.HotShare, err = num("hot_share", 0, 0.5); err != nil {
		return p, err
	}
	if p.FurnaceWaitH, err = num("furnace_wait_h", 0, 48); err != nil {
		return p, err
	}
	fm, err := num("furnace_min_lots", 1, 4)
	if err != nil {
		return p, err
	}
	if fm != nil {
		if *fm != math.Trunc(*fm) {
			return p, simBad("%s: furnace_min_lots must be a whole number", what)
		}
		k := int(*fm)
		p.FurnaceMinLots = &k
	}
	fv, err := num("vehicles", 1, 30)
	if err != nil {
		return p, err
	}
	if fv != nil {
		if *fv != math.Trunc(*fv) {
			return p, simBad("%s: vehicles must be a whole number", what)
		}
		k := int(*fv)
		p.Vehicles = &k
	}
	if d, ok := treeString(m["dispatch"]); ok && d != "" {
		d = strings.ToUpper(strings.TrimSpace(d))
		if d != "FIFO" && d != "CR" {
			return p, simBad("%s: dispatch must be FIFO or CR (critical ratio)", what)
		}
		p.Dispatch = d
	}
	if mv := m["mix"]; mv != nil {
		mm, ok := mv.(map[string]any)
		if !ok {
			return p, simBad("%s: mix must map product codes to shares", what)
		}
		p.Mix = map[string]float64{}
		total := 0.0
		for k, x := range mm {
			if !containsStr(products, k) {
				return p, simBad("%s: unknown product %q in the mix (%s)", what, k, strings.Join(products, ", "))
			}
			f, ok := treeNumber(x)
			if !ok || f < 0 || f > 100 {
				return p, simBad("%s: the share of %s must be 0 to 100", what, k)
			}
			p.Mix[k] = f
			total += f
		}
		if total <= 0 {
			return p, simBad("%s: the product mix needs a share above 0", what)
		}
	}
	return p, nil
}

func parsePlanActions(v any, what string, horizon float64, aliases map[string]string) ([]planAction, error) {
	list, err := treeArray(v, what)
	if err != nil {
		return nil, err
	}
	out := []planAction{}
	for i, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, simBad("%s: action %d must be an object", what, i+1)
		}
		if err := treeKeys(m, "an action", "kind", "asset", "at_day", "duration_h", "count", "reason"); err != nil {
			return nil, err
		}
		kind, _ := treeString(m["kind"])
		asset, _ := treeString(m["asset"])
		a := planAction{Kind: strings.ToLower(strings.TrimSpace(kind)), Asset: aliases[strings.ToLower(strings.TrimSpace(asset))]}
		if a.Asset == "" {
			return nil, simBad("%s: action %d: %q is not a production tool you can see", what, i+1, asset)
		}
		if r, ok := treeString(m["reason"]); ok {
			a.Reason = strings.TrimSpace(r)
			if len([]rune(a.Reason)) > 200 {
				return nil, simBad("%s: action %d: the reason is too long (200 characters)", what, i+1)
			}
		}
		switch a.Kind {
		case "tool_down":
			at, ok1 := treeNumber(m["at_day"])
			d, ok2 := treeNumber(m["duration_h"])
			if m["at_day"] == nil {
				at, ok1 = 0, true
			}
			if !ok1 || !ok2 || at < 0 || at >= horizon || d <= 0 || d > 24*horizon {
				return nil, simBad("%s: tool_down of %s needs at_day within the horizon and duration_h above 0", what, a.Asset)
			}
			a.AtDay, a.DurationH = math.Round(at*1000)/1000, math.Round(d*100)/100
		case "add_tool":
			n, err := treeWhole(m["count"], "count", false)
			if err != nil {
				return nil, err
			}
			if m["count"] == nil {
				n = 1
			}
			if n < 1 || n > 3 {
				return nil, simBad("%s: add_tool of %s: count must be 1 to 3", what, a.Asset)
			}
			a.Count = int(n)
		default:
			return nil, simBad("%s: action %d has an unknown kind %q (tool_down, add_tool)", what, i+1, kind)
		}
		out = append(out, a)
	}
	return out, nil
}

// planSubmit queues a production plan from the UI. Input: title, spec,
// rationale?. The AI agent cannot submit (it proposes).
func (s *Server) planSubmit(c *Ctx) (any, error) {
	if err := c.Require("simulation", "create"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	return s.planCreate(c, "ui", simQueued, false)
}

// planPropose records a production plan proposed by the AI assistant for a
// person to approve. Input: title, spec, rationale, agent_session_id?.
func (s *Server) planPropose(c *Ctx) (any, error) {
	if err := c.Require("simulation", "read"); err != nil {
		return nil, err
	}
	if !c.fromAgent() {
		return nil, httpx.Forbidden("Production plans are proposed by the AI assistant, submit your own plan instead")
	}
	return s.planCreate(c, "agent", simProposed, true)
}

func (s *Server) planCreate(c *Ctx, origin, status string, needRationale bool) (any, error) {
	title, err := readSimText(c, "title", "title", true, simMaxTitle)
	if err != nil {
		return nil, err
	}
	rationale, err := readSimText(c, "rationale", "rationale", needRationale, simMaxRationale)
	if err != nil {
		return nil, err
	}
	var session *string
	if origin == "agent" {
		if session, err = readSimText(c, "agent_session_id", "agent session id", false, 200); err != nil {
			return nil, err
		}
	}
	spec, err := s.readPlanSpec(c)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	id, err := s.simCreateJob(c, simNewJob{Kind: "production", Raw: raw, StepS: planStepS, Title: *title, Rationale: rationale, Origin: origin,
		Status: status, AgentSession: session})
	if err != nil {
		return nil, err
	}
	return s.simJobRow(c, id)
}
