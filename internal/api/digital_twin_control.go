package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/httpx"
)

// Two-way control from the digital twin: an operator writes a set point or a
// control mode of an equipment, the command goes to the plant over the
// gateway's MQTT command topic, and the request is confirmed when the plant
// reports the new value back (BMS write / read-back). Every request is kept in
// asset.control_requests: PENDING, then APPLIED, REJECTED (validation) or
// TIMEOUT (no read-back).

const (
	// twinControlWait is how long a write waits for the plant's read-back.
	twinControlWait = 10 * time.Second
	// twinControlPoll is the read-back polling period (an in-memory hub lookup).
	twinControlPoll = 50 * time.Millisecond
	// twinControlStale rejects a write when the equipment's last value is older
	// (3 missed 5 s reports: the device stopped reporting, so it would not take
	// the command either).
	twinControlStale = 15 * time.Second
	// twinControlExpire turns PENDING requests left by a restart into TIMEOUT.
	twinControlExpire = "30 seconds"
)

type twinControlOption struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

// twinControl is the control definition of a writable model parameter.
type twinControl struct {
	Min     *float64            `json:"min"`
	Max     *float64            `json:"max"`
	Step    *float64            `json:"step"`
	Options []twinControlOption `json:"options"`
	// Mode is the alias of the mode parameter that must be out of automatic
	// (value <> 0) before this set point can be written.
	Mode *string `json:"mode"`
	// Target is the alias of the measured parameter the control drives.
	Target *string `json:"target"`
}

func (s *Server) registerDigitalTwinControl() {
	s.handle("digital_twin/writeparameter", AccessUser, s.twinWriteParameter)
	s.handle("digital_twin/getcontrolrequests", AccessUser, s.twinControlRequests)
}

// twinControls returns the control definition of every writable parameter of
// an equipment, keyed by model-parameter alias.
func (s *Server) twinControls(ctx context.Context, assetID int64) (map[string]*twinControl, error) {
	rows, err := s.DB.Query(ctx, `SELECT mp.alias, mp.control_min, mp.control_max, mp.control_step, mp.control_options, mp.control_mode, mp.control_target
		FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND mp.is_writable`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*twinControl{}
	for rows.Next() {
		var alias string
		var options []byte
		ctl := &twinControl{}
		if err := rows.Scan(&alias, &ctl.Min, &ctl.Max, &ctl.Step, &options, &ctl.Mode, &ctl.Target); err != nil {
			return nil, err
		}
		if len(options) > 0 {
			if err := json.Unmarshal(options, &ctl.Options); err != nil {
				return nil, fmt.Errorf("control options of %s: %w", alias, err)
			}
		}
		out[alias] = ctl
	}
	return out, rows.Err()
}

// twinWriteParameter writes a set point or control mode to the plant.
// Input: parameter_id (equipment parameter), value. Validation failures are
// recorded and returned as a REJECTED request, not as an HTTP error. Plant
// writes need a person: the AI agent cannot call this (RequireHuman).
func (s *Server) twinWriteParameter(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	epid := c.F.Int("parameter_id", 0)
	vp := c.F.FloatPtr("value")
	if epid <= 0 || vp == nil || math.IsNaN(*vp) || math.IsInf(*vp, 0) {
		return nil, httpx.BadRequest("A parameter_id and a numeric value are required")
	}
	return s.twinWrite(c, epid, *vp)
}

// twinWrite is the write path shared by digital_twin/writeparameter and
// ai_insights/applyrecommendation: scope check, validation (recorded as a
// REJECTED request), MQTT command, read-back wait and the final request row.
// The caller checks the permissions and the human-only guard.
func (s *Server) twinWrite(c *Ctx, epid int64, value float64) (map[string]any, error) {
	p, err := s.loadEP(c, epid) // also checks the user's location scope
	if err != nil {
		return nil, err
	}
	controls, err := s.twinControls(c, p.AssetID)
	if err != nil {
		return nil, err
	}
	ctl := controls[p.Alias]
	if ctl == nil {
		return nil, httpx.BadRequestf("%s is not writable", p.Name)
	}
	// Requests are completed even if the client goes away mid-wait.
	bg := context.WithoutCancel(c)
	s.twinExpireRequests(bg)

	var gwType string
	if p.Gateway != nil {
		if err := s.DB.QueryRow(c, `SELECT type FROM dh.gateways WHERE alias = $1`, *p.Gateway).Scan(&gwType); err != nil && !db.IsNoRows(err) {
			return nil, err
		}
	}
	var old *float64
	if v, ok := s.twinNumber(p.Gateway, p.Tag); ok {
		old = &v
	}

	reason := s.twinValidateWrite(c, p, ctl, controls, value, gwType)
	var id int64
	if reason != "" {
		err = s.DB.QueryRow(bg, `INSERT INTO asset.control_requests (equipment_id, equipment_parameter_id, user_id, old_value, value, status, reason, completed_at)
			VALUES ($1, $2, $3, $4, $5, 'REJECTED', $6, now()) RETURNING id`, p.AssetID, p.EPID, c.User.ID, old, value, reason).Scan(&id)
		if err != nil {
			return nil, err
		}
		return s.twinControlRequest(bg, id)
	}
	if err := s.DB.QueryRow(bg, `INSERT INTO asset.control_requests (equipment_id, equipment_parameter_id, user_id, old_value, value)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, p.AssetID, p.EPID, c.User.ID, old, value).Scan(&id); err != nil {
		return nil, err
	}

	// The tag addresses the device and its key ("<device>/<key>", as published
	// by the device); the command goes to the device's command topic.
	device, key := "", *p.Tag
	if i := strings.LastIndex(key, "/"); i >= 0 {
		device, key = key[:i], key[i+1:]
	}
	// expires_at (ms): the device drops a command it receives after the backend
	// stopped waiting, so a request reported TIMEOUT never changes the plant later
	sent := time.Now()
	payload, _ := json.Marshal(map[string]any{"request_id": id, "parameter": key, "value": value, "user": c.User.Username,
		"expires_at": sent.Add(twinControlWait).UnixMilli()})
	if err := s.sendCommand(bg, *p.Gateway, gwType, device, payload, sent.Add(twinControlWait)); err != nil {
		s.twinCompleteRequest(bg, id, "REJECTED", "Could not send the command: "+err.Error())
		return s.twinControlRequest(bg, id)
	}

	// Read-back: the plant confirms when it reports the requested value.
	tol := 1e-6
	if p.Scale != nil {
		tol = 0.5 * math.Pow(10, -float64(*p.Scale))
	}
	if ctl.Step != nil && *ctl.Step > 0 {
		tol = math.Min(tol, *ctl.Step/2)
	}
	deadline := sent.Add(twinControlWait)
	for time.Now().Before(deadline) {
		time.Sleep(twinControlPoll)
		v, ok := s.Hub.Value(*p.Gateway, *p.Tag)
		if !ok || v.Q != datahub.QualityGood || v.R < sent.UnixMilli() {
			continue
		}
		if x, ok := parseHubNumber(v.V); ok && math.Abs(x-value) <= tol+1e-9 {
			s.twinCompleteRequest(bg, id, "APPLIED", "")
			return s.twinControlRequest(bg, id)
		}
	}
	s.twinCompleteRequest(bg, id, "TIMEOUT", fmt.Sprintf("The plant did not confirm the new value within %d s", int(twinControlWait.Seconds())))
	return s.twinControlRequest(bg, id)
}

// twinValidateWrite returns why a write must be refused ("" = valid).
func (s *Server) twinValidateWrite(c *Ctx, p *epInfo, ctl *twinControl, controls map[string]*twinControl, value float64, gwType string) string {
	switch {
	case p.Gateway == nil || p.Tag == nil:
		return "The parameter is not mapped to a data source tag"
	case !s.acceptsCommands(gwType):
		return "The data source of this parameter does not accept commands"
	}
	if st := s.Hub.Status(*p.Gateway); !st.Active || !st.Connected {
		return fmt.Sprintf("Data source %s is not connected", *p.Gateway)
	}
	v, found := s.Hub.Value(*p.Gateway, *p.Tag)
	if !found || v.Q != datahub.QualityGood {
		return "No live data from the equipment"
	}
	if age := time.Since(time.UnixMilli(v.R)); v.R <= 0 || age > twinControlStale {
		return fmt.Sprintf("No live data from the equipment for %s", age.Round(time.Second))
	}
	if len(ctl.Options) > 0 {
		// A valid option needs no further checks (modes are never gated).
		return controlValueError(ctl, value, p.Unit)
	}
	if reason := controlValueError(ctl, value, p.Unit); reason != "" {
		return reason
	}
	if ctl.Mode != nil && *ctl.Mode != "" {
		var mode *epInfo
		var modeID int64
		err := s.DB.QueryRow(c, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			WHERE ep.equipment_id = $1 AND mp.alias = $2`, p.AssetID, *ctl.Mode).Scan(&modeID)
		if err == nil {
			mode, err = s.loadEP(c, modeID)
		}
		if err != nil {
			return "The control mode of this set point is not configured"
		}
		m, ok := s.twinNumber(mode.Gateway, mode.Tag)
		if !ok {
			return fmt.Sprintf("%s is unknown", mode.Name)
		}
		if m == 0 {
			label := "manual"
			if mc := controls[mode.Alias]; mc != nil {
				for _, o := range mc.Options {
					if o.Value != 0 {
						label = o.Label
						break
					}
				}
			}
			return fmt.Sprintf("%s is in automatic: switch it to %s first", mode.Name, label)
		}
	}
	return ""
}

// controlValueError checks a value against a control definition alone (valid
// option, range, step); "" = valid. Live state (data source, mode gating) is
// checked by twinValidateWrite.
func controlValueError(ctl *twinControl, value float64, unit string) string {
	if len(ctl.Options) > 0 {
		for _, o := range ctl.Options {
			if o.Value == value {
				return ""
			}
		}
		return fmt.Sprintf("%v is not a valid option", value)
	}
	if ctl.Min != nil && value < *ctl.Min-1e-9 || ctl.Max != nil && value > *ctl.Max+1e-9 {
		return fmt.Sprintf("Value outside the allowed range %s – %s %s", fmtLimit(ctl.Min), fmtLimit(ctl.Max), unit)
	}
	if ctl.Step != nil && *ctl.Step > 0 {
		base := 0.0
		if ctl.Min != nil {
			base = *ctl.Min
		}
		n := (value - base) / *ctl.Step
		if math.Abs(n-math.Round(n)) > 1e-6 {
			return fmt.Sprintf("Value must be in steps of %v %s", *ctl.Step, unit)
		}
	}
	return ""
}

// twinNumber returns the live numeric value of a tag (good quality only).
func (s *Server) twinNumber(gateway, tag *string) (float64, bool) {
	if gateway == nil || tag == nil {
		return 0, false
	}
	v, found := s.Hub.Value(*gateway, *tag)
	if !found || v.Q != datahub.QualityGood {
		return 0, false
	}
	return parseHubNumber(v.V)
}

// parseHubNumber parses a hub value; booleans count as 1 / 0.
func parseHubNumber(s string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return 1, true
	case "false":
		return 0, true
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func fmtLimit(v *float64) string {
	if v == nil {
		return "…"
	}
	return fmt.Sprintf("%v", *v)
}

func (s *Server) twinCompleteRequest(ctx context.Context, id int64, status, reason string) {
	var r *string
	if reason != "" {
		r = &reason
	}
	_, _ = s.DB.Exec(ctx, `UPDATE asset.control_requests SET status = $2, reason = $3, completed_at = now() WHERE id = $1 AND status = 'PENDING'`,
		id, status, r)
}

// twinExpireRequests closes requests left PENDING (e.g. by a restart mid-wait).
func (s *Server) twinExpireRequests(ctx context.Context) {
	_, _ = s.DB.Exec(ctx, `UPDATE asset.control_requests SET status = 'TIMEOUT', reason = 'The plant did not confirm the new value',
		completed_at = now() WHERE status = 'PENDING' AND requested_at < now() - interval '`+twinControlExpire+`'`)
}

const twinControlSelect = `SELECT r.id, r.equipment_id, r.equipment_parameter_id, mp.alias, mp.name, mp.unit, mp.scale, r.old_value, r.value,
	r.status, r.reason, r.requested_at, r.completed_at, u.username, u.full_name
	FROM asset.control_requests r JOIN asset.equipment_parameters ep ON ep.id = r.equipment_parameter_id
	JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id LEFT JOIN core.users u ON u.id = r.user_id`

func scanControlRequest(row interface{ Scan(...any) error }) (map[string]any, error) {
	var id, assetID, epid int64
	var alias, name, unit, status string
	var scale *int
	var old *float64
	var value float64
	var reason, username, fullName *string
	var requested time.Time
	var completed *time.Time
	if err := row.Scan(&id, &assetID, &epid, &alias, &name, &unit, &scale, &old, &value, &status, &reason, &requested, &completed,
		&username, &fullName); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "asset_id": assetID, "parameter_id": epid, "alias": alias, "name": name, "unit": unit, "scale": scale,
		"old_value": old, "value": value, "status": status, "reason": reason,
		"requested_at": httpx.TS(requested), "completed_at": httpx.TSPtr(completed),
		"username": username, "user_name": fullName,
	}, nil
}

func (s *Server) twinControlRequest(ctx context.Context, id int64) (map[string]any, error) {
	return scanControlRequest(s.DB.QueryRow(ctx, twinControlSelect+` WHERE r.id = $1`, id))
}

// twinControlRequests lists the latest write requests of an equipment (newest
// first). Input: asset_id, limit (1..50, default 10).
func (s *Server) twinControlRequests(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	limit := c.F.Int("limit", 10)
	if limit < 1 || limit > 50 {
		limit = 10
	}
	s.twinExpireRequests(c)
	rows, err := s.DB.Query(c, twinControlSelect+` WHERE r.equipment_id = $1 ORDER BY r.requested_at DESC, r.id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		r, err := scanControlRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
