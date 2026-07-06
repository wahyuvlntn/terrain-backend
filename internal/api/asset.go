package api

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/maint"
)

func (s *Server) registerAsset() {
	s.handle("asset/getassetname", AccessUser, s.assetName)
	s.handle("asset/getassetoverview", AccessUser, s.assetOverview)
	s.handle("asset/exportassetoverview", AccessUser, s.assetOverviewExport)
	s.handle("asset/getassetdetails", AccessUser, s.assetDetails)
	s.handle("asset/getparameters", AccessUser, s.assetParameters)
	s.handle("asset/getallparameters", AccessUser, s.assetAllParameters)
	s.handle("asset/getparameterdata", AccessUser, s.assetParameterData)
	s.handle("asset/getmultiparameterdata", AccessUser, s.assetMultiParameterData)
	s.handle("asset/getlatestparamdata", AccessUser, s.assetLatestParam)
	s.handle("asset/getthreshold", AccessUser, s.assetGetThreshold)
	s.handle("asset/updatethreshold", AccessUser, s.assetUpdateThreshold)
	s.handle("asset/get_spc_config", AccessUser, s.assetGetSPCConfig)
	s.handle("asset/updatespcconfig", AccessUser, s.assetUpdateSPCConfig)
	s.handle("asset/get_spc_data_v2", AccessUser, s.assetSPCData)
	s.handle("asset/calculate_spc_metrics", AccessUser, s.assetCalculateSPC)
	s.handle("asset/getassetaspectinfo", AccessUser, s.assetAspect)
	s.handle("asset/updateassetaspectinfo", AccessUser, s.assetUpdateAspect)
	s.handle("asset/get_models", AccessUser, s.listTable(`SELECT id, name FROM asset.models ORDER BY lower(name)`))
}

func (s *Server) assetName(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	var name string
	if err := s.DB.QueryRow(c, `SELECT name FROM asset.equipment WHERE id = $1`, id).Scan(&name); err != nil {
		if db.IsNoRows(err) {
			return map[string]any{"name": ""}, nil
		}
		return nil, err
	}
	return map[string]any{"name": name}, nil
}

type assetInfo struct {
	ID         int64
	Name       string
	Alias      string
	Image      *string
	TypeName   string
	ModelName  string
	ModelID    int64
	LocationID int64
	IsUtility  bool
}

func (s *Server) assetInfos(ctx context.Context, ids []int64) (map[int64]*assetInfo, error) {
	out := map[int64]*assetInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT e.id, e.name, e.alias, e.image_path, t.name, m.name, m.id, e.location_id, e.is_utility
		FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id JOIN asset.types t ON t.id = m.type_id WHERE e.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a := &assetInfo{}
		if err := rows.Scan(&a.ID, &a.Name, &a.Alias, &a.Image, &a.TypeName, &a.ModelName, &a.ModelID, &a.LocationID, &a.IsUtility); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

// overviewRows builds the equipment overview cards.
func (s *Server) overviewRows(c *Ctx, paginate bool) ([]map[string]any, int, error) {
	ids, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, 0, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	infos, err := s.assetInfos(c, ids)
	if err != nil {
		return nil, 0, err
	}
	states, err := s.currentStates(c, ids)
	if err != nil {
		return nil, 0, err
	}
	statusFilter, statusPresent := c.F.StringList("status")
	condFilter, condPresent := c.F.StringList("conditions")
	statusSet := map[string]bool{}
	for _, v := range statusFilter {
		statusSet[strings.ToUpper(strings.TrimSpace(v))] = true
	}
	condSet := map[string]bool{}
	for _, v := range condFilter {
		condSet[strings.ToUpper(strings.TrimSpace(v))] = true
	}
	search := strings.ToLower(c.F.StrTrim("search"))
	var filtered []int64
	for _, id := range ids {
		a := infos[id]
		if a == nil {
			continue
		}
		st := states[id]
		if statusPresent {
			name, _ := statusName(st).(string)
			if name == "" {
				name = "OFFLINE"
			}
			if !statusSet[name] && !(name == "UNPLANNED DOWN" && statusSet["DOWN"]) {
				continue
			}
		}
		if condPresent {
			cond, _ := conditionName(st).(string)
			if cond == "" {
				cond = "OFFLINE"
			}
			if !condSet[cond] {
				continue
			}
		}
		if search != "" && !strings.Contains(strings.ToLower(a.Name), search) && !strings.Contains(strings.ToLower(a.Alias), search) {
			continue
		}
		filtered = append(filtered, id)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return strings.ToLower(infos[filtered[i]].Name) < strings.ToLower(infos[filtered[j]].Name)
	})
	total := len(filtered)
	pageIDs := filtered
	if paginate {
		limit := c.limit(9, 200)
		page := c.page(1)
		start := int((page - 1) * limit)
		if start > total {
			start = total
		}
		end := start + int(limit)
		if end > total {
			end = total
		}
		pageIDs = filtered[start:end]
	}
	durs, err := s.statusDurations(c, pageIDs, from, to)
	if err != nil {
		return nil, 0, err
	}
	counts, err := s.alarmCounts(c, pageIDs, from, to)
	if err != nil {
		return nil, 0, err
	}
	rh, err := s.runningHours(c, pageIDs)
	if err != nil {
		return nil, 0, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, 0, err
	}
	span := elapsed(from, to)
	out := []map[string]any{}
	for _, id := range pageIDs {
		a := infos[id]
		st := states[id]
		up := computeUptime(durs[id], span)
		row := map[string]any{
			"id": id, "name": a.Name, "status": statusName(st), "status_start_time": nil,
			"type_name": a.TypeName, "model_name": a.ModelName, "condition": conditionName(st),
			"warning_counts": counts[id][0], "critical_counts": counts[id][1], "image": imageURL(a.Image),
			"condition_start_time": nil, "uptime": up.UptimePct, "running_duration": up.RunningHours,
			"total_duration": up.TotalHours, "location": tree.path(a.LocationID, " > "), "running_duration_hours": rh[id],
			"is_utility": a.IsUtility,
		}
		if st != nil {
			row["status_start_time"] = httpx.TSPtr(st.StatusSince)
			row["condition_start_time"] = httpx.TSPtr(st.ConditionSince)
		}
		out = append(out, row)
	}
	return out, total, nil
}

func (s *Server) assetOverview(c *Ctx) (any, error) {
	rows, total, err := s.overviewRows(c, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"counts": total, "assets": rows}, nil
}

func (s *Server) assetOverviewExport(c *Ctx) (any, error) {
	rows, _, err := s.overviewRows(c, false)
	return rows, err
}

type interval struct {
	Value string `json:"value"`
	Start string `json:"start"`
	End   string `json:"end"`
}

func (s *Server) assetDetails(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	from, to := c.dayRange("date")
	now := time.Now().UTC()
	clipEnd := to
	if clipEnd.After(now) {
		clipEnd = now
	}
	infos, err := s.assetInfos(c, []int64{id})
	if err != nil {
		return nil, err
	}
	a := infos[id]
	if a == nil {
		return nil, httpx.NotFound("err_asset_not_found")
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, []int64{id})
	if err != nil {
		return nil, err
	}
	st := states[id]

	statuses := []interval{}
	counts := map[int]int{}
	if from.Before(clipEnd) {
		rows, err := s.DB.Query(c, `SELECT status_id, GREATEST(start_ts, $2), LEAST(COALESCE(end_ts, $3), $3)
			FROM ts.asset_status WHERE asset_id = $1 AND start_ts < $3 AND COALESCE(end_ts, $3) > $2 ORDER BY start_ts`, id, from, clipEnd)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sid int
			var a, b time.Time
			if err := rows.Scan(&sid, &a, &b); err != nil {
				rows.Close()
				return nil, err
			}
			if !b.After(a) {
				continue
			}
			statuses = append(statuses, interval{Value: engine.StatusName(sid), Start: httpx.TS(a), End: httpx.TS(b)})
			counts[sid]++
		}
		rows.Close()
	}
	conditions := []interval{}
	if from.Before(clipEnd) {
		rows, err := s.DB.Query(c, `SELECT condition, GREATEST(start_ts, $2), LEAST(COALESCE(end_ts, $3), $3)
			FROM ts.asset_condition WHERE asset_id = $1 AND start_ts < $3 AND COALESCE(end_ts, $3) > $2 ORDER BY start_ts`, id, from, clipEnd)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cond string
			var a, b time.Time
			if err := rows.Scan(&cond, &a, &b); err != nil {
				rows.Close()
				return nil, err
			}
			if !b.After(a) {
				continue
			}
			conditions = append(conditions, interval{Value: cond, Start: httpx.TS(a), End: httpx.TS(b)})
		}
		rows.Close()
	}
	durs, err := s.statusDurations(c, []int64{id}, from, to)
	if err != nil {
		return nil, err
	}
	up := computeUptime(durs[id], elapsed(from, to))
	alarmCounts, err := s.alarmCounts(c, []int64{id}, from, to)
	if err != nil {
		return nil, err
	}
	rh, err := s.runningHours(c, []int64{id})
	if err != nil {
		return nil, err
	}

	alarms := []map[string]any{}
	arows, err := s.DB.Query(c, `SELECT id, alarm_type, model_alarm_id, name, severity, message, start_ts, end_ts
		FROM ts.alarm_events WHERE asset_id = $1 AND start_ts >= $2 AND start_ts < $3 ORDER BY start_ts DESC LIMIT 500`, id, from, to)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var eid int64
		var typ, name, sev, msg string
		var maID *int64
		var start time.Time
		var end *time.Time
		if err := arows.Scan(&eid, &typ, &maID, &name, &sev, &msg, &start, &end); err != nil {
			arows.Close()
			return nil, err
		}
		message := name
		if msg != "" && msg != name {
			message = name + " - " + msg
		}
		typeID, statusID := 1, 1
		if typ == "ABNORMAL" {
			typeID = 2
		}
		if end != nil {
			statusID = 2
		}
		alarms = append(alarms, map[string]any{
			"severity": sev, "message": message, "timestamp": httpx.TS(start), "alarm_id": maID, "alarm_name": name,
			"type_id": typeID, "status_id": statusID,
		})
	}
	arows.Close()

	maintenances := []map[string]any{}
	mrows, err := s.DB.Query(c, `SELECT o.id, o.name, o.maintenance_type_id, o.status_id, COALESCE(o.updated_at, o.created_at),
			(SELECT c.code || ' - ' || c.name FROM dm.maintenance_order_issues i JOIN dm.alarm_codes c ON c.id = i.alarm_code_id
			 WHERE i.order_id = o.id AND NOT i.is_other ORDER BY i.position LIMIT 1), o.description
		FROM dm.maintenance_orders o WHERE o.asset_id = $1 AND o.planned_start < $3
			AND (o.status_id NOT IN (5, 6, 2) OR COALESCE(o.closed_at, o.completed_at, o.updated_at, o.created_at) >= $2)
		ORDER BY o.planned_start DESC LIMIT 200`, id, from, to)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var mid int64
		var name, desc string
		var typ, status int
		var updated time.Time
		var issue *string
		if err := mrows.Scan(&mid, &name, &typ, &status, &updated, &issue, &desc); err != nil {
			mrows.Close()
			return nil, err
		}
		if issue == nil && desc != "" {
			d := desc
			issue = &d
		}
		typeName := "Preventive"
		if typ == maint.TypeCorrective {
			typeName = "Corrective"
		}
		maintenances = append(maintenances, map[string]any{
			"id": mid, "name": name, "type_name": typeName, "issue": issue, "status": maint.StatusLabel(status), "updated_at": httpx.TS(updated),
		})
	}
	mrows.Close()

	res := map[string]any{
		"name": a.Name, "status": statusName(st), "status_start_time": nil, "statuses": statuses,
		"running_counts": counts[engine.StatusRunning], "unplanned_down_counts": counts[engine.StatusUnplannedDown],
		"planned_down": counts[engine.StatusPlannedDown], "offline_counts": counts[engine.StatusOffline],
		"type_name": a.TypeName, "model_name": a.ModelName, "location_name": tree.path(a.LocationID, " > "),
		"condition": conditionName(st), "condition_start_time": nil, "conditions": conditions,
		"warning_counts": alarmCounts[id][0], "critical_counts": alarmCounts[id][1], "image": imageURL(a.Image),
		"uptime": nz(derefFloat(up.UptimePct)), "alarms": alarms, "maintenances": maintenances, "is_utility": a.IsUtility,
		"uptime_duration": up.RunningHours, "total_duration": up.TotalHours, "running_duration_hours": rh[id],
	}
	if st != nil {
		res["status_start_time"] = httpx.TSPtr(st.StatusSince)
		res["condition_start_time"] = httpx.TSPtr(st.ConditionSince)
	}
	return res, nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

type epInfo struct {
	EPID       int64
	AssetID    int64
	AssetName  string
	Alias      string
	Name       string
	Unit       string
	DataType   string
	Scale      *int
	Gateway    *string
	Tag        *string
	WarnLower  *float64
	WarnUpper  *float64
	WarnType   string
	CritLower  *float64
	CritUpper  *float64
	CritType   string
	USL, LSL   *float64
	UCL, LCL   *float64
	CenterLine *float64
	Sigma      *float64
}

func (s *Server) loadEP(c *Ctx, epid int64) (*epInfo, error) {
	p := &epInfo{}
	err := s.DB.QueryRow(c, `SELECT ep.id, e.id, e.name, mp.alias, mp.name, mp.unit, mp.data_type, mp.scale, g.alias, ep.tag_name,
		ep.warning_lower_limit, ep.warning_upper_limit, ep.warning_threshold_type, ep.critical_lower_limit, ep.critical_upper_limit,
		ep.critical_threshold_type, ep.spc_usl, ep.spc_lsl, ep.spc_ucl, ep.spc_lcl, ep.spc_center_line, ep.spc_sigma
		FROM asset.equipment_parameters ep JOIN asset.equipment e ON e.id = ep.equipment_id
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id LEFT JOIN dh.gateways g ON g.id = ep.gateway_id
		WHERE ep.id = $1`, epid).Scan(&p.EPID, &p.AssetID, &p.AssetName, &p.Alias, &p.Name, &p.Unit, &p.DataType, &p.Scale, &p.Gateway, &p.Tag,
		&p.WarnLower, &p.WarnUpper, &p.WarnType, &p.CritLower, &p.CritUpper, &p.CritType, &p.USL, &p.LSL, &p.UCL, &p.LCL, &p.CenterLine, &p.Sigma)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_parameter_is_not_found")
		}
		return nil, err
	}
	if err := s.visibleAsset(c, p.AssetID); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *epInfo) paramDef() *engine.ParamDef {
	return &engine.ParamDef{EPID: p.EPID, Alias: p.Alias, Name: p.Name, Unit: p.Unit, DataType: p.DataType, Scale: p.Scale,
		WarnLower: p.WarnLower, WarnUpper: p.WarnUpper, WarnType: p.WarnType, CritLower: p.CritLower, CritUpper: p.CritUpper, CritType: p.CritType,
		USL: p.USL, LSL: p.LSL, UCL: p.UCL, LCL: p.LCL, CenterLine: p.CenterLine, Sigma: p.Sigma}
}

func (s *Server) assetParameters(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	search := c.F.StrTrim("search")
	skip := c.F.Int("skip", 0)
	if skip < 0 {
		skip = 0
	}
	limit := c.limit(2, 100)
	var total int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND ep.is_displayed AND mp.data_type <> 'string' AND ($2 = '' OR mp.name ILIKE $3)`, id, search, ilike(search)).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ep.id, mp.name, mp.unit FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND ep.is_displayed AND mp.data_type <> 'string' AND ($2 = '' OR mp.name ILIKE $3)
		ORDER BY lower(mp.name), ep.id OFFSET $4 LIMIT $5`, id, search, ilike(search), skip, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	params := []map[string]any{}
	for rows.Next() {
		var pid int64
		var name, unit string
		if err := rows.Scan(&pid, &name, &unit); err != nil {
			return nil, err
		}
		params = append(params, map[string]any{"id": pid, "name": name, "unit": unit})
	}
	return map[string]any{"parameters": params, "counts": total}, rows.Err()
}

func (s *Server) assetAllParameters(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ep.id, mp.name FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND ep.is_displayed AND mp.data_type <> 'string' ORDER BY lower(mp.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var pid int64
		var name string
		if err := rows.Scan(&pid, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": pid, "name": name})
	}
	return out, rows.Err()
}

type point struct {
	ts  time.Time
	val *float64
}

// series loads logged numeric values in [from, to), down-sampled to at most
// maxPoints buckets (bucket average).
func (s *Server) series(ctx context.Context, epid int64, from, to time.Time, maxPoints int) ([]point, error) {
	pts, _, err := s.seriesBuckets(ctx, epid, from, to, maxPoints)
	return pts, err
}

// seriesBuckets is series that also reports the bucket width: 0 when the raw
// rows are returned, else each point's ts is the start of its bucket.
func (s *Server) seriesBuckets(ctx context.Context, epid int64, from, to time.Time, maxPoints int) ([]point, time.Duration, error) {
	rows, err := s.DB.Query(ctx, `SELECT ts, value FROM ts.parameter_values WHERE equipment_parameter_id = $1 AND ts >= $2 AND ts < $3 ORDER BY ts`, epid, from, to)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var pts []point
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.ts, &p.val); err != nil {
			return nil, 0, err
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if maxPoints <= 0 || len(pts) <= maxPoints {
		return pts, 0, nil
	}
	bucket := to.Sub(from) / time.Duration(maxPoints)
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
		b := p.ts.Truncate(bucket)
		if !b.Equal(cur) {
			flush()
			cur = b
		}
		if p.val != nil {
			sum += *p.val
			n++
		}
	}
	flush()
	return out, bucket, nil
}

func pointsJSON(pts []point) []map[string]any {
	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		var v any
		if p.val != nil {
			v = *p.val
		}
		out = append(out, map[string]any{"value": v, "timestamp": httpx.TS(p.ts)})
	}
	return out
}

func thresholdAreas(p *epInfo) []map[string]any {
	hasUpper := func(t string) bool { return strings.Contains(t, ">") }
	hasLower := func(t string) bool { return strings.Contains(t, "<") }
	var wU, wL, cU, cL *float64
	if hasUpper(p.WarnType) {
		wU = p.WarnUpper
	}
	if hasLower(p.WarnType) {
		wL = p.WarnLower
	}
	if hasUpper(p.CritType) {
		cU = p.CritUpper
	}
	if hasLower(p.CritType) {
		cL = p.CritLower
	}
	if wU == nil && wL == nil && cU == nil && cL == nil {
		return []map[string]any{}
	}
	bound := func(v *float64, def string) any {
		if v == nil {
			return def
		}
		return *v
	}
	out := []map[string]any{}
	if cU != nil {
		out = append(out, map[string]any{"from": *cU, "to": "max", "type": "CRITICAL"})
	}
	if wU != nil {
		out = append(out, map[string]any{"from": *wU, "to": bound(cU, "max"), "type": "WARNING"})
	}
	if cL != nil {
		out = append(out, map[string]any{"from": "min", "to": *cL, "type": "CRITICAL"})
	}
	if wL != nil {
		out = append(out, map[string]any{"from": bound(cL, "min"), "to": *wL, "type": "WARNING"})
	}
	low := wL
	if low == nil {
		low = cL
	}
	high := wU
	if high == nil {
		high = cU
	}
	out = append(out, map[string]any{"from": bound(low, "min"), "to": bound(high, "max"), "type": "NORMAL"})
	return out
}

// yBounds computes padded axis bounds over data and extra reference values.
func yBounds(pts []point, extra ...*float64) (any, any) {
	min, max := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		if p.val != nil {
			min = math.Min(min, *p.val)
			max = math.Max(max, *p.val)
		}
	}
	for _, e := range extra {
		if e != nil {
			min = math.Min(min, *e)
			max = math.Max(max, *e)
		}
	}
	if math.IsInf(min, 1) {
		return nil, nil
	}
	pad := (max - min) * 0.1
	if pad == 0 {
		pad = math.Max(math.Abs(max)*0.1, 1)
	}
	return round2(min - pad), round2(max + pad)
}

func (s *Server) assetParameterData(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	from, to := c.dayRange("date")
	pts, err := s.series(c, p.EPID, from, to, 2000)
	if err != nil {
		return nil, err
	}
	areas := thresholdAreas(p)
	ymin, ymax := yBounds(pts, p.WarnLower, p.WarnUpper, p.CritLower, p.CritUpper)
	if len(areas) == 0 {
		ymin, ymax = yBounds(pts)
	}
	return map[string]any{"data": pointsJSON(pts), "mark_area": areas, "y_min": ymin, "y_max": ymax}, nil
}

type spcStats struct {
	mean, sd float64
	ok       bool
	ucl, lcl *float64
	usl, lsl *float64
}

func computeStats(vals []float64) (float64, float64, bool) {
	if len(vals) == 0 {
		return 0, 0, false
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	if len(vals) < 2 {
		return mean, 0, true
	}
	var ss float64
	for _, v := range vals {
		ss += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(ss / float64(len(vals)-1)), true
}

func (p *epInfo) spcFor(pts []point) spcStats {
	var vals []float64
	for _, pt := range pts {
		if pt.val != nil {
			vals = append(vals, *pt.val)
		}
	}
	st := spcStats{usl: p.USL, lsl: p.LSL}
	mean, sd, ok := computeStats(vals)
	if p.CenterLine != nil {
		mean, ok = *p.CenterLine, true
	}
	if p.Sigma != nil && *p.Sigma > 0 {
		sd = *p.Sigma
	}
	st.mean, st.sd, st.ok = mean, sd, ok
	if p.UCL != nil {
		st.ucl = p.UCL
	} else if ok && sd > 0 {
		v := mean + 3*sd
		st.ucl = &v
	}
	if p.LCL != nil {
		st.lcl = p.LCL
	} else if ok && sd > 0 {
		v := mean - 3*sd
		st.lcl = &v
	}
	return st
}

func dashOr(v *float64) any {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return "-"
	}
	return round2(*v)
}

func (st spcStats) cp() *float64 {
	if st.usl == nil || st.lsl == nil || st.sd <= 0 {
		return nil
	}
	v := (*st.usl - *st.lsl) / (6 * st.sd)
	return &v
}

func (st spcStats) cpk() *float64 {
	if st.sd <= 0 || !st.ok || (st.usl == nil && st.lsl == nil) {
		return nil
	}
	v := math.Inf(1)
	if st.usl != nil {
		v = math.Min(v, (*st.usl-st.mean)/(3*st.sd))
	}
	if st.lsl != nil {
		v = math.Min(v, (st.mean-*st.lsl)/(3*st.sd))
	}
	return &v
}

func (st spcStats) metrics(withSpec bool) map[string]any {
	var mean, sd *float64
	if st.ok {
		m, s := st.mean, st.sd
		mean, sd = &m, &s
	}
	out := map[string]any{"grand_mean": dashOr(mean), "std_dev": dashOr(sd), "cp": dashOr(st.cp()), "cpk": dashOr(st.cpk()),
		"ucl": dashOr(st.ucl), "lcl": dashOr(st.lcl)}
	if withSpec {
		out["usl"] = dashOr(st.usl)
		out["lsl"] = dashOr(st.lsl)
	}
	return out
}

func (s *Server) assetMultiParameterData(c *Ctx) (any, error) {
	ids, _ := c.F.IDs("parameter_ids")
	from, to := c.dayRange("date")
	realTime := []map[string]any{}
	spc := []map[string]any{}
	for _, id := range ids {
		p, err := s.loadEP(c, id)
		if err != nil {
			return nil, err
		}
		pts, err := s.series(c, p.EPID, from, to, 0)
		if err != nil {
			return nil, err
		}
		realTime = append(realTime, map[string]any{"name": p.Name, "data": pointsJSON(pts)})
		st := p.spcFor(pts)
		m := st.metrics(false)
		data := make([]map[string]any, 0, len(pts))
		for _, pt := range pts {
			var v any
			if pt.val != nil {
				v = *pt.val
			}
			row := map[string]any{"value": v, "timestamp": httpx.TS(pt.ts)}
			for k, mv := range m {
				row[k] = mv
			}
			data = append(data, row)
		}
		spc = append(spc, map[string]any{"name": p.Name, "data": data})
	}
	return map[string]any{"real_time": realTime, "spc": spc}, nil
}

func (s *Server) assetLatestParam(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	var value *string
	condition := engine.CondOffline
	if p.Gateway != nil && p.Tag != nil {
		if v, ok := s.Hub.Value(*p.Gateway, *p.Tag); ok && v.Q == datahub.QualityGood && v.T > 0 {
			tv := engine.ToValue(v.V, v.T, true, p.DataType, p.Scale)
			str := v.V
			if num, ok := tv.Num(); ok && p.DataType == "number" {
				str = formatNum(num, p.Scale)
				condition = engine.ParamCondition(p.paramDef(), num)
			} else {
				condition = engine.CondNormal
			}
			value = &str
		}
	}
	if value == nil {
		// Fall back to the last logged value.
		var num *float64
		var text *string
		var ts time.Time
		err := s.DB.QueryRow(c, `SELECT value, value_text, ts FROM ts.parameter_values WHERE equipment_parameter_id = $1 ORDER BY ts DESC LIMIT 1`, p.EPID).Scan(&num, &text, &ts)
		if err == nil {
			if num != nil {
				v := formatNum(*num, p.Scale)
				value = &v
			} else {
				value = text
			}
		}
	}
	limits := func(t string, lower, upper *float64) map[string]any {
		out := map[string]any{"upper_limit": nil, "lower_limit": nil}
		if strings.Contains(t, ">") {
			out["upper_limit"] = upper
		}
		if strings.Contains(t, "<") {
			out["lower_limit"] = lower
		}
		return out
	}
	return map[string]any{
		"value": value, "condition": condition, "unit": p.Unit,
		"warning_threshold":  limits(p.WarnType, p.WarnLower, p.WarnUpper),
		"critical_threshold": limits(p.CritType, p.CritLower, p.CritUpper),
	}, nil
}

func formatNum(v float64, scale *int) string {
	if scale != nil && *scale >= 0 && *scale <= 10 {
		return strconvFormat(v, *scale)
	}
	return strconvFormat(v, -1)
}

func (s *Server) assetGetThreshold(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	z := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	return map[string]any{
		"warning_lower_limit": z(p.WarnLower), "warning_upper_limit": z(p.WarnUpper), "warning_threshold_type": p.WarnType,
		"critical_lower_limit": z(p.CritLower), "critical_upper_limit": z(p.CritUpper), "critical_threshold_type": p.CritType,
	}, nil
}

func (s *Server) assetUpdateThreshold(c *Ctx) (any, error) {
	if err := c.Require("equipmentoverview", "update"); err != nil {
		return nil, err
	}
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	wt := c.F.Str("warning_threshold_type")
	ct := c.F.Str("critical_threshold_type")
	if !engine.ValidThresholdType(wt) || !engine.ValidThresholdType(ct) {
		return nil, httpx.BadRequest("err_invalid_threshold_type")
	}
	wu, wl := c.F.FloatPtr("warning_upper_limit"), c.F.FloatPtr("warning_lower_limit")
	cu, cl := c.F.FloatPtr("critical_upper_limit"), c.F.FloatPtr("critical_lower_limit")
	need := func(t string, lower, upper *float64) error {
		if strings.Contains(t, ">") && upper == nil {
			return httpx.BadRequest("err_input_valid_number")
		}
		if strings.Contains(t, "<") && lower == nil {
			return httpx.BadRequest("err_input_valid_number")
		}
		if strings.Contains(t, ">") && strings.Contains(t, "<") && *upper < *lower {
			return httpx.BadRequest("err_upper_limit_must_be_greater_than_lower_limit")
		}
		return nil
	}
	if err := need(wt, wl, wu); err != nil {
		return nil, err
	}
	if err := need(ct, cl, cu); err != nil {
		return nil, err
	}
	if strings.Contains(wt, ">") && strings.Contains(ct, ">") && *cu < *wu {
		return nil, httpx.BadRequest("err_critical_upper_limit_must_be_greater_than_warning_upper_limit")
	}
	if strings.Contains(wt, "<") && strings.Contains(ct, "<") && *cl > *wl {
		return nil, httpx.BadRequest("err_critical_lower_limit_must_be_less_than_warning_lower_limit")
	}
	_, err = s.DB.Exec(c, `UPDATE asset.equipment_parameters SET warning_upper_limit = $2, warning_lower_limit = $3, warning_threshold_type = $4,
		critical_upper_limit = $5, critical_lower_limit = $6, critical_threshold_type = $7, updated_at = now() WHERE id = $1`,
		p.EPID, wu, wl, wt, cu, cl, ct)
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "Threshold successfully updated", nil
}

func (s *Server) assetGetSPCConfig(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	return map[string]any{"usl": p.USL, "lsl": p.LSL, "ucl": p.UCL, "lcl": p.LCL, "center_line": p.CenterLine, "sigma": p.Sigma}, nil
}

func (s *Server) assetUpdateSPCConfig(c *Ctx) (any, error) {
	if err := c.Require("equipmentoverview", "update"); err != nil {
		return nil, err
	}
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	usl, lsl := c.F.FloatPtr("usl"), c.F.FloatPtr("lsl")
	ucl, lcl := c.F.FloatPtr("ucl"), c.F.FloatPtr("lcl")
	center := c.F.FloatPtr("x_bar_bar")
	if center == nil {
		center = c.F.FloatPtr("centerLine")
	}
	sigma := c.F.FloatPtr("std_dev")
	if sigma == nil {
		sigma = c.F.FloatPtr("sigma")
	}
	if usl != nil && lsl != nil && *lsl >= *usl {
		return nil, httpx.BadRequest("err_usl_must_be_greater_than_lsl")
	}
	if ucl != nil && lcl != nil && *lcl >= *ucl {
		return nil, httpx.BadRequest("UCL must be greater than LCL")
	}
	if sigma != nil && *sigma < 0 {
		return nil, httpx.BadRequest("err_input_valid_number_greater_0")
	}
	_, err = s.DB.Exec(c, `UPDATE asset.equipment_parameters SET spc_usl = $2, spc_lsl = $3, spc_ucl = $4, spc_lcl = $5,
		spc_center_line = $6, spc_sigma = $7, updated_at = now() WHERE id = $1`, p.EPID, usl, lsl, ucl, lcl, center, sigma)
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "SPC configuration successfully updated", nil
}

func (s *Server) assetSPCData(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	from, to := c.dayRange("date")
	pts, err := s.series(c, p.EPID, from, to, 2000)
	if err != nil {
		return nil, err
	}
	st := p.spcFor(pts)
	var metrics any
	if st.ok {
		metrics = st.metrics(true)
	}
	areas := []map[string]any{}
	bound := func(v *float64, def string) any {
		if v == nil {
			return def
		}
		return *v
	}
	if st.ucl != nil || st.lcl != nil {
		areas = append(areas, map[string]any{"from": bound(st.lcl, "min"), "to": bound(st.ucl, "max"), "type": "NORMAL"})
	}
	if st.ucl != nil {
		areas = append(areas, map[string]any{"from": *st.ucl, "to": bound(st.usl, "max"), "type": "WARNING"})
	}
	if st.usl != nil {
		areas = append(areas, map[string]any{"from": *st.usl, "to": "max", "type": "CRITICAL"})
	}
	if st.lcl != nil {
		areas = append(areas, map[string]any{"from": bound(st.lsl, "min"), "to": *st.lcl, "type": "WARNING"})
	}
	if st.lsl != nil {
		areas = append(areas, map[string]any{"from": "min", "to": *st.lsl, "type": "CRITICAL"})
	}
	ymin, ymax := yBounds(pts, st.ucl, st.lcl, st.usl, st.lsl)
	offline := []map[string]any{}
	rows, err := s.DB.Query(c, `SELECT GREATEST(start_ts, $2), LEAST(COALESCE(end_ts, now()), $3) FROM ts.asset_status
		WHERE asset_id = $1 AND status_id = 4 AND start_ts < $3 AND COALESCE(end_ts, now()) > $2 ORDER BY start_ts`, p.AssetID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, b time.Time
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return nil, err
		}
		if b.After(a) {
			offline = append(offline, map[string]any{"start": a.UnixMilli(), "end": b.UnixMilli()})
		}
	}
	rows.Close()
	return map[string]any{"metrics": metrics, "data": pointsJSON(pts), "y_min": ymin, "y_max": ymax, "mark_area": areas, "offline_ranges": offline}, nil
}

func (s *Server) assetCalculateSPC(c *Ctx) (any, error) {
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	loc := c.Location()
	parse := func(key string, endOfDay bool) *time.Time {
		raw := strings.TrimSpace(c.F.Str(key))
		if raw == "" {
			return nil
		}
		t, err := httpx.ParseTime(raw, loc)
		if err != nil {
			return nil
		}
		if endOfDay && len(raw) == 10 {
			t = t.Add(24 * time.Hour)
		}
		return &t
	}
	from, to := parse("start_date", false), parse("end_date", true)
	if from == nil || to == nil || !to.After(*from) {
		return nil, httpx.BadRequest("err_invalid_date_range")
	}
	pts, err := s.series(c, p.EPID, *from, *to, 0)
	if err != nil {
		return nil, err
	}
	var vals []float64
	for _, pt := range pts {
		if pt.val != nil {
			vals = append(vals, *pt.val)
		}
	}
	mean, sd, ok := computeStats(vals)
	if !ok {
		return map[string]any{"ucl": nil, "lcl": nil, "center_line": nil, "sigma": nil}, nil
	}
	return map[string]any{"ucl": round4(mean + 3*sd), "lcl": round4(mean - 3*sd), "center_line": round4(mean), "sigma": round4(sd)}, nil
}

func round4(f float64) float64 { return httpx.Round(f, 4) }

type aspectItem struct {
	Aspect      string `json:"aspect"`
	Information string `json:"information"`
}

func (s *Server) assetAspect(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	var name string
	var image *string
	var crit, sup []byte
	if err := s.DB.QueryRow(c, `SELECT name, image_path, aspect_critical, aspect_supporting FROM asset.equipment WHERE id = $1`, id).
		Scan(&name, &image, &crit, &sup); err != nil {
		return nil, err
	}
	var critical, supporting []aspectItem
	_ = jsonUnmarshal(string(crit), &critical)
	_ = jsonUnmarshal(string(sup), &supporting)
	return map[string]any{"name": name, "image": imageURL(image), "descriptions": map[string]any{
		"critical": emptyIfNil(critical), "supporting": emptyIfNil(supporting)}}, nil
}

func (s *Server) assetUpdateAspect(c *Ctx) (any, error) {
	if err := c.Require("equipmentoverview", "update"); err != nil {
		return nil, err
	}
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	var critical, supporting []aspectItem
	if _, err := c.F.JSON("critical", &critical); err != nil {
		return nil, httpx.BadRequest("Invalid critical aspects")
	}
	if _, err := c.F.JSON("supporting", &supporting); err != nil {
		return nil, httpx.BadRequest("Invalid supporting aspects")
	}
	if _, err := s.DB.Exec(c, `UPDATE asset.equipment SET aspect_critical = $2, aspect_supporting = $3, updated_at = now() WHERE id = $1`,
		id, toJSON(emptyIfNil(critical)), toJSON(emptyIfNil(supporting))); err != nil {
		return nil, err
	}
	return "Equipment information successfully updated", nil
}
