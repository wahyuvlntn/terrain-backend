// Package engine evaluates equipment state from live gateway values: status
// (running / idle / down / offline), model alarms, dynamic alarms, parameter
// thresholds (abnormal conditions), SPC rules, condition, running hours and
// the early-warning issue relations that raise maintenance orders.
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"afdd/internal/db"
)

// Status ids (shared with the web UI).
const (
	StatusRunning       = 1
	StatusIdle          = 2
	StatusUnplannedDown = 3
	StatusOffline       = 4
	StatusPlannedDown   = 5
)

// StatusName maps a status id to the label used by the UI.
func StatusName(id int) string {
	switch id {
	case StatusRunning:
		return "RUNNING"
	case StatusIdle:
		return "IDLE"
	case StatusUnplannedDown:
		return "UNPLANNED DOWN"
	case StatusPlannedDown:
		return "PLANNED DOWN"
	case StatusOffline:
		return "OFFLINE"
	}
	return ""
}

// StatusIDFromName maps a UI status label to an id (0 when unknown).
func StatusIDFromName(name string) int {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "RUNNING":
		return StatusRunning
	case "IDLE":
		return StatusIdle
	case "UNPLANNED DOWN", "UNPLANNED_DOWN", "DOWN":
		return StatusUnplannedDown
	case "PLANNED DOWN", "PLANNED_DOWN":
		return StatusPlannedDown
	case "OFFLINE":
		return StatusOffline
	}
	return 0
}

// Conditions.
const (
	CondNormal   = "NORMAL"
	CondWarning  = "WARNING"
	CondCritical = "CRITICAL"
	CondOffline  = "OFFLINE"
)

// ParamDef is an equipment parameter with its mapping and limits.
type ParamDef struct {
	EPID         int64
	MPID         int64
	Alias        string
	Name         string
	Unit         string
	DataType     string
	Scale        *int
	GatewayAlias string
	Tag          string
	IsLogged     bool
	IsDisplayed  bool

	WarnLower, WarnUpper *float64
	WarnType             string
	CritLower, CritUpper *float64
	CritType             string

	USL, LSL, UCL, LCL, CenterLine, Sigma *float64
}

// Mapped reports whether the parameter is bound to a gateway tag.
func (p *ParamDef) Mapped() bool { return p.GatewayAlias != "" && p.Tag != "" }

// AlarmDef is an effective model alarm.
type AlarmDef struct {
	ID          int64
	Name        string
	Criticality int
	Message     string
	Script      string
}

// DynAlarmDef is an effective dynamic alarm.
type DynAlarmDef struct {
	ID     int64
	Name   string
	Script string
}

// Relation is an issue relation graph of a model.
type Relation struct {
	ID   int64
	Name string
	Flow Flow
}

// Flow is the React Flow graph stored for an issue relation.
type Flow struct {
	Nodes []FlowNode `json:"nodes"`
	Edges []FlowEdge `json:"edges"`
}

// FlowNode is a graph node.
type FlowNode struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Position map[string]any `json:"position,omitempty"`
	Data     map[string]any `json:"data"`
}

// FlowEdge is a graph edge.
type FlowEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// ModelDef is the effective (inheritance resolved) definition of a model.
type ModelDef struct {
	ID               int64
	Name             string
	TypeID           int64
	UnplannedScript  string
	PlannedScript    string
	RunningScript    string
	Alarms           []AlarmDef
	DynAlarms        []DynAlarmDef
	Relations        []Relation
	RunHourWarning   int
	RunHourCritical  int
	RunHourNotifyW   bool
	RunHourNotifyC   bool
	RunHourAutoMaint bool
}

// Equipment is an equipment with its parameters.
type Equipment struct {
	ID          int64
	Alias       string
	Name        string
	ModelID     int64
	LocationID  int64
	LicenseOK   bool
	ForcedState *int
	InstalledAt time.Time
	Params      []*ParamDef
}

// Snapshot is a consistent view of all definitions.
type Snapshot struct {
	Equipment  map[int64]*Equipment
	Models     map[int64]*ModelDef
	ParamByEP  map[int64]*ParamDef
	ParamOwner map[int64]int64 // equipment parameter id -> equipment id
	AlarmCodes *AlarmCodeIndex
	LoadedAt   time.Time
}

// AlarmCodeIndex resolves alarm names to master data alarm codes.
type AlarmCodeIndex struct {
	byKey map[string]int64
	Info  map[int64]AlarmCode
}

// AlarmCode is a master data alarm code.
type AlarmCode struct {
	ID   int64
	Code string
	Name string
}

// Resolve finds the alarm code referenced by an alarm name. Accepted forms:
// "<code>", "<code>-<name>", "<code> <text>", "<code>: <text>" or "<name>".
func (ix *AlarmCodeIndex) Resolve(name string) (int64, bool) {
	if ix == nil {
		return 0, false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return 0, false
	}
	if id, ok := ix.byKey[n]; ok {
		return id, true
	}
	for _, sep := range []string{" - ", "-", ":", " "} {
		if i := strings.Index(n, sep); i > 0 {
			if id, ok := ix.byKey[strings.TrimSpace(n[:i])]; ok {
				return id, true
			}
		}
	}
	return 0, false
}

// LoadAlarmCodes loads the alarm code index.
func LoadAlarmCodes(ctx context.Context, q db.Querier) (*AlarmCodeIndex, error) {
	ix := &AlarmCodeIndex{byKey: map[string]int64{}, Info: map[int64]AlarmCode{}}
	rows, err := q.Query(ctx, `SELECT id, code, name FROM dm.alarm_codes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c AlarmCode
		if err := rows.Scan(&c.ID, &c.Code, &c.Name); err != nil {
			return nil, err
		}
		ix.Info[c.ID] = c
		ix.byKey[strings.ToLower(strings.TrimSpace(c.Code))] = c.ID
		ix.byKey[strings.ToLower(strings.TrimSpace(c.Code+"-"+c.Name))] = c.ID
		if _, exists := ix.byKey[strings.ToLower(strings.TrimSpace(c.Name))]; !exists {
			ix.byKey[strings.ToLower(strings.TrimSpace(c.Name))] = c.ID
		}
	}
	return ix, rows.Err()
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// LoadModel resolves the effective definition of one model.
func LoadModel(ctx context.Context, q db.Querier, modelID int64) (*ModelDef, error) {
	m := &ModelDef{ID: modelID}
	var baseID *int64
	var unp, pla, run *string
	err := q.QueryRow(ctx, `SELECT name, type_id, base_model_id, unplanned_down_script, planned_down_script, running_script,
		running_hour_warning, running_hour_critical, running_hour_notify_warn, running_hour_notify_crit, running_hour_auto_maint
		FROM asset.models WHERE id = $1`, modelID).Scan(&m.Name, &m.TypeID, &baseID, &unp, &pla, &run,
		&m.RunHourWarning, &m.RunHourCritical, &m.RunHourNotifyW, &m.RunHourNotifyC, &m.RunHourAutoMaint)
	if err != nil {
		return nil, err
	}
	m.UnplannedScript, m.PlannedScript, m.RunningScript = strOr(unp), strOr(pla), strOr(run)
	// Inherit missing status scripts along the base chain.
	seen := map[int64]bool{modelID: true}
	for baseID != nil && !seen[*baseID] {
		seen[*baseID] = true
		var next *int64
		var bu, bp, br *string
		if err := q.QueryRow(ctx, `SELECT base_model_id, unplanned_down_script, planned_down_script, running_script FROM asset.models WHERE id = $1`, *baseID).
			Scan(&next, &bu, &bp, &br); err != nil {
			break
		}
		if m.UnplannedScript == "" {
			m.UnplannedScript = strOr(bu)
		}
		if m.PlannedScript == "" {
			m.PlannedScript = strOr(bp)
		}
		if m.RunningScript == "" {
			m.RunningScript = strOr(br)
		}
		baseID = next
	}

	rows, err := q.Query(ctx, `SELECT a.id, a.name, a.criticality, a.message, COALESCE(a.script, b.script, '')
		FROM asset.model_alarms a LEFT JOIN asset.model_alarms b ON b.id = a.base_alarm_id
		WHERE a.model_id = $1 AND NOT a.is_disabled ORDER BY a.name`, modelID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a AlarmDef
		if err := rows.Scan(&a.ID, &a.Name, &a.Criticality, &a.Message, &a.Script); err != nil {
			rows.Close()
			return nil, err
		}
		m.Alarms = append(m.Alarms, a)
	}
	rows.Close()

	rows, err = q.Query(ctx, `SELECT a.id, a.name, COALESCE(a.script, b.script, '')
		FROM asset.model_dynamic_alarms a LEFT JOIN asset.model_dynamic_alarms b ON b.id = a.base_alarm_id
		WHERE a.model_id = $1 AND NOT a.is_disabled ORDER BY a.name`, modelID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a DynAlarmDef
		if err := rows.Scan(&a.ID, &a.Name, &a.Script); err != nil {
			rows.Close()
			return nil, err
		}
		m.DynAlarms = append(m.DynAlarms, a)
	}
	rows.Close()

	rows, err = q.Query(ctx, `SELECT id, name, flow FROM dm.issue_relations WHERE model_id = $1 ORDER BY id`, modelID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r Relation
		var flow []byte
		if err := rows.Scan(&r.ID, &r.Name, &flow); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(flow, &r.Flow)
		m.Relations = append(m.Relations, r)
	}
	rows.Close()
	return m, rows.Err()
}

// LoadSnapshot loads every equipment, model and parameter definition.
func LoadSnapshot(ctx context.Context, q db.Querier, licenseEnforced bool) (*Snapshot, error) {
	s := &Snapshot{
		Equipment:  map[int64]*Equipment{},
		Models:     map[int64]*ModelDef{},
		ParamByEP:  map[int64]*ParamDef{},
		ParamOwner: map[int64]int64{},
		LoadedAt:   time.Now(),
	}
	codes, err := LoadAlarmCodes(ctx, q)
	if err != nil {
		return nil, err
	}
	s.AlarmCodes = codes

	rows, err := q.Query(ctx, `SELECT e.id, e.alias, e.name, e.model_id, e.location_id, e.forced_status_id, e.installed_at,
		(l.id IS NOT NULL AND (l.expired_at IS NULL OR l.expired_at > now())) AS license_ok
		FROM asset.equipment e LEFT JOIN core.licenses l ON l.id = e.license_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		e := &Equipment{}
		var lic bool
		if err := rows.Scan(&e.ID, &e.Alias, &e.Name, &e.ModelID, &e.LocationID, &e.ForcedState, &e.InstalledAt, &lic); err != nil {
			rows.Close()
			return nil, err
		}
		e.LicenseOK = lic || !licenseEnforced
		s.Equipment[e.ID] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT ep.id, ep.equipment_id, mp.id, mp.alias, mp.name, mp.unit, mp.data_type, mp.scale,
		g.alias, ep.tag_name, ep.is_logged, ep.is_displayed,
		ep.warning_lower_limit, ep.warning_upper_limit, ep.warning_threshold_type,
		ep.critical_lower_limit, ep.critical_upper_limit, ep.critical_threshold_type,
		ep.spc_usl, ep.spc_lsl, ep.spc_ucl, ep.spc_lcl, ep.spc_center_line, ep.spc_sigma
		FROM asset.equipment_parameters ep
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		LEFT JOIN dh.gateways g ON g.id = ep.gateway_id
		ORDER BY ep.equipment_id, mp.name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p := &ParamDef{}
		var eqID int64
		var gw, tag *string
		if err := rows.Scan(&p.EPID, &eqID, &p.MPID, &p.Alias, &p.Name, &p.Unit, &p.DataType, &p.Scale,
			&gw, &tag, &p.IsLogged, &p.IsDisplayed,
			&p.WarnLower, &p.WarnUpper, &p.WarnType, &p.CritLower, &p.CritUpper, &p.CritType,
			&p.USL, &p.LSL, &p.UCL, &p.LCL, &p.CenterLine, &p.Sigma); err != nil {
			rows.Close()
			return nil, err
		}
		p.GatewayAlias, p.Tag = strOr(gw), strOr(tag)
		if e := s.Equipment[eqID]; e != nil {
			e.Params = append(e.Params, p)
			s.ParamByEP[p.EPID] = p
			s.ParamOwner[p.EPID] = eqID
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, e := range s.Equipment {
		if _, ok := s.Models[e.ModelID]; ok {
			continue
		}
		m, err := LoadModel(ctx, q, e.ModelID)
		if err != nil {
			return nil, err
		}
		s.Models[e.ModelID] = m
	}
	return s, nil
}
