package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/maint"
)

func (s *Server) registerEarlyWarning() {
	p := "early_warning_diagnostics/"
	s.handle(p+"get_early_warning_list", AccessUser, s.ewList)
	s.handle(p+"get_maintenance_order_overview", AccessUser, s.ewOrderOverview)
	s.handle(p+"get_asset_overview", AccessUser, s.ewAssetOverview)
	s.handle(p+"get_asset_parameters", AccessUser, s.ewAssetParameters)
	s.handle(p+"get_instruction_list", AccessUser, s.ewInstructionList)
	s.handle(p+"change_alarm_validity", AccessUser, s.ewChangeValidity)
	s.handle(p+"export_early_warning_diagnostics", AccessUser, s.ewExport)
	s.handle(p+"get_diagnostics_overview", AccessUser, s.ewDiagnosticsOverview)
	s.handle(p+"get_parameter_message", AccessUser, s.ewParameterMessage)
	s.handle(p+"get_maintenance_alarm_timestamps", AccessUser, s.ewAlarmTimestamps)

	d := "early_warning_dashboard/"
	s.handle(d+"get_early_warning_types", AccessUser, s.ewdTypes)
	s.handle(d+"get_early_warning_statuses", AccessUser, s.ewdStatuses)
	s.handle(d+"get_parameter_warning_occurences", AccessUser, s.ewdParameterOccurrences)
	s.handle(d+"get_error_code_occurences", AccessUser, s.ewdErrorCodeOccurrences)
	s.handle(d+"get_early_warning_and_diagnostic_trend", AccessUser, s.ewdTrend)
	s.handle(d+"get_issue_pattern_list", AccessUser, s.ewdIssuePatternList)
	s.handle(d+"get_issue_pattern_chart_v2", AccessUser, s.ewdIssuePatternChart)
	s.handle(d+"get_issue_pattern_detail_v2", AccessUser, s.ewdIssuePatternDetail)
}

// Early warnings are maintenance orders raised by the diagnostics engine.
const ewWhere = `o.from_system AND o.issue_relation_id IS NOT NULL`

type ewRow struct {
	ID         int64
	Name       string
	AssetID    int64
	AssetName  string
	ModelName  string
	LocationID int64
	Detected   time.Time
	Severity   string
	StatusID   int
	Compound   bool
	Deduction  bool
	Start      time.Time
	End        *time.Time
}

func (s *Server) ewRows(ctx context.Context, assets []int64, from, to time.Time, severities []int64, search string) ([]*ewRow, error) {
	var sev any
	if severities != nil {
		sev = severities
	}
	rows, err := s.DB.Query(ctx, `SELECT o.id, o.name, e.id, e.name, m.name, e.location_id, COALESCE(o.detected_at, o.created_at), sv.name,
			o.status_id, o.is_compound, o.is_deduction, o.planned_start, o.planned_end
		FROM dm.maintenance_orders o JOIN asset.equipment e ON e.id = o.asset_id JOIN asset.models m ON m.id = e.model_id
		JOIN dm.severities sv ON sv.id = o.severity_id
		WHERE `+ewWhere+` AND o.asset_id = ANY($1) AND COALESCE(o.detected_at, o.created_at) >= $2 AND COALESCE(o.detected_at, o.created_at) < $3
		AND ($4::bigint[] IS NULL OR o.severity_id = ANY($4))
		AND ($5 = '' OR o.name ILIKE $6 OR e.name ILIKE $6 OR m.name ILIKE $6 OR EXISTS (
			SELECT 1 FROM dm.maintenance_order_issues i JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id
			WHERE i.order_id = o.id AND (ac.code ILIKE $6 OR ac.name ILIKE $6)))
		ORDER BY COALESCE(o.detected_at, o.created_at) DESC, o.id DESC`, assets, from, to, sev, search, ilike(search))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ewRow
	for rows.Next() {
		r := &ewRow{}
		if err := rows.Scan(&r.ID, &r.Name, &r.AssetID, &r.AssetName, &r.ModelName, &r.LocationID, &r.Detected, &r.Severity, &r.StatusID,
			&r.Compound, &r.Deduction, &r.Start, &r.End); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) ewFiltered(c *Ctx) ([]*ewRow, error) {
	jf, af, err := c.jsonFilter("filter")
	if err != nil {
		return nil, err
	}
	assets, err := s.resolveAssets(c, af)
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start_time", "end_time", false, 7)
	sev, _ := anyInts(jf.SeverityIDs)
	if jf.SeverityIDs != nil && sev == nil {
		sev = []int64{}
	}
	return s.ewRows(c, assets, from, to, sev, c.F.StrTrim("search"))
}

func (s *Server) issueCodesFor(ctx context.Context, ids []int64) (map[int64][]map[string]any, error) {
	out := map[int64][]map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT i.order_id, ac.code, ac.name FROM dm.maintenance_order_issues i JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id
		WHERE i.order_id = ANY($1) AND NOT i.is_other ORDER BY i.position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var code, name string
		if err := rows.Scan(&id, &code, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], map[string]any{"code": code, "name": name})
	}
	return out, rows.Err()
}

func (s *Server) ewList(c *Ctx) (any, error) {
	rows, err := s.ewFiltered(c)
	if err != nil {
		return nil, err
	}
	compound, degradation := 0, 0
	inclC, inclD := c.F.Bool("include_compound", false), c.F.Bool("include_degradation", false)
	var shown []*ewRow
	for _, r := range rows {
		if r.Compound {
			compound++
			if inclC {
				shown = append(shown, r)
			}
		} else {
			degradation++
			if inclD {
				shown = append(shown, r)
			}
		}
	}
	page, limit := c.page(1), c.limit(10, 500)
	pageRows := paginate(shown, page, limit)
	ids := make([]int64, 0, len(pageRows))
	for _, r := range pageRows {
		ids = append(ids, r.ID)
	}
	codes, err := s.issueCodesFor(c, ids)
	if err != nil {
		return nil, err
	}
	data := []map[string]any{}
	for _, r := range pageRows {
		var current any
		if v := codes[r.ID]; len(v) > 0 {
			current = v
		}
		data = append(data, map[string]any{"id": r.ID, "model": r.ModelName, "current_issues": current, "timestamp": httpx.TS(r.Detected),
			"severity": r.Severity, "asset_name": r.AssetName})
	}
	return map[string]any{"data": data, "compound_count": compound, "degradation_count": degradation}, nil
}

func (s *Server) orderAlarmCodes(ctx context.Context, orderID int64) ([]map[string]any, []map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT ac.code, ac.code || ' - ' || ac.name, i.is_other, i.alarm_status, i.detected_at
		FROM dm.maintenance_order_issues i JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id WHERE i.order_id = $1 ORDER BY i.position`, orderID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	main, other := []map[string]any{}, []map[string]any{}
	for rows.Next() {
		var code, label, status string
		var isOther bool
		var detected *time.Time
		if err := rows.Scan(&code, &label, &isOther, &status, &detected); err != nil {
			return nil, nil, err
		}
		item := map[string]any{"code": code, "label": label, "alarm_status": status}
		if detected != nil {
			item["timestamp"] = httpx.TS(*detected)
		}
		if isOther {
			item["alarm_status"] = "NORMAL"
			other = append(other, item)
		} else {
			main = append(main, item)
		}
	}
	return main, other, rows.Err()
}

func (s *Server) ewOrderOverview(c *Ctx) (any, error) {
	o, err := s.Maint.Get(c, s.DB, c.F.Int("order_id", 0))
	if err != nil {
		return []any{}, nil
	}
	if err := s.visibleAsset(c, o.AssetID); err != nil {
		return nil, err
	}
	main, other, err := s.orderAlarmCodes(c, o.ID)
	if err != nil {
		return nil, err
	}
	techs, err := s.technicianNames(c, []int64{o.ID})
	if err != nil {
		return nil, err
	}
	var sevName, prName string
	_ = s.DB.QueryRow(c, `SELECT name FROM dm.severities WHERE id = $1`, o.SeverityID).Scan(&sevName)
	if o.PriorityID != nil {
		_ = s.DB.QueryRow(c, `SELECT name FROM dm.priorities WHERE id = $1`, *o.PriorityID).Scan(&prName)
	}
	typ := "PREVENTIVE"
	if o.TypeID == maint.TypeCorrective {
		typ = "CORRECTIVE"
	}
	end := o.PlannedStart
	if o.PlannedEnd != nil {
		end = *o.PlannedEnd
	}
	var actual any
	if o.StartedAt != nil {
		actual = maint.ActualSeconds(c, s.DB, o.ID)
	}
	est := maint.EstimatedSeconds(c, s.DB, o)
	var estimated any
	if est > 0 {
		estimated = est
	}
	return []map[string]any{{
		"id": o.ID, "name": o.Name, "status": maint.StatusCode(o.StatusID), "severity": sevName, "priority": prName,
		"planned_start": httpx.TS(o.PlannedStart), "planned_end": httpx.TS(end), "estimated_time": estimated, "actual_time": actual,
		"assignee": strings.Join(techs[o.ID], ", "), "maintenance_type": typ, "alarm_codes": main, "other_alarm_codes": other,
		"asset_id": o.AssetID, "is_deduction": o.IsDeduction, "approved_by": usernameByID(c, s.DB, o.ApprovedBy),
	}}, nil
}

func (s *Server) ewAssetOverview(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	infos, err := s.assetInfos(c, []int64{id})
	if err != nil {
		return nil, err
	}
	a := infos[id]
	if a == nil {
		return []any{}, nil
	}
	states, err := s.currentStates(c, []int64{id})
	if err != nil {
		return nil, err
	}
	st := states[id]
	durs, err := s.statusDurations(c, []int64{id}, from, to)
	if err != nil {
		return nil, err
	}
	up := computeUptime(durs[id], elapsed(from, to))
	counts, err := s.alarmCounts(c, []int64{id}, from, to)
	if err != nil {
		return nil, err
	}
	rh, err := s.runningHours(c, []int64{id})
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	row := map[string]any{
		"id": id, "name": a.Name, "model_name": a.ModelName, "type_name": a.TypeName, "location": tree.path(a.LocationID, " > "),
		"image": derefStr(imageURL(a.Image)), "status": statusName(st), "status_start_time": nil, "status_duration": nil,
		"condition": conditionName(st), "condition_start_time": nil, "condition_duration": nil, "uptime": up.UptimePct,
		"warning_counts": counts[id][0], "critical_counts": counts[id][1], "running_duration": up.RunningHours,
		"running_duration_hours": rh[id], "total_duration": up.TotalHours, "is_utility": a.IsUtility,
	}
	if st != nil {
		if st.StatusSince != nil {
			row["status_start_time"] = httpx.TS(*st.StatusSince)
			row["status_duration"] = int64(time.Since(*st.StatusSince).Seconds())
		}
		if st.ConditionSince != nil {
			row["condition_start_time"] = httpx.TS(*st.ConditionSince)
			row["condition_duration"] = int64(time.Since(*st.ConditionSince).Seconds())
		}
	}
	return []map[string]any{row}, nil
}

func (s *Server) ewAssetParameters(c *Ctx) (any, error) {
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ep.id, mp.name, mp.unit FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND mp.data_type = 'number' ORDER BY lower(mp.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var pid int64
		var name, unit string
		if err := rows.Scan(&pid, &name, &unit); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": pid, "name": name, "unit": unit})
	}
	return out, rows.Err()
}

func formatHM(seconds int64) string {
	if seconds <= 0 {
		return "0 minutes"
	}
	h, m := seconds/3600, (seconds%3600)/60
	if h > 0 {
		return fmt.Sprintf("%d hours %d minutes", h, m)
	}
	return fmt.Sprintf("%d minutes", m)
}

func typeLabel(typeID int, name string) string {
	if typeID == 1 {
		return "Diagnosis"
	}
	return name
}

func (s *Server) ewInstructionList(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	issues, err := s.orderIssues(c, o.ID)
	if err != nil {
		return nil, err
	}
	items, err := s.orderInstructions(c, o, issues)
	if err != nil {
		return nil, err
	}
	group := func(isOther bool) []map[string]any {
		out := []map[string]any{}
		for _, is := range issues {
			if is.IsOther != isOther {
				continue
			}
			byType := map[int][]map[string]any{}
			names := map[int]string{}
			for _, it := range items {
				if it.CodeID != is.ID {
					continue
				}
				names[it.TypeID] = typeLabel(it.TypeID, it.TypeName)
				byType[it.TypeID] = append(byType[it.TypeID], map[string]any{
					"id": it.ID, "name": it.Name, "content": it.Content, "isUsingPdf": it.IsUsingPdf, "pdfUrl": it.PdfURL,
					"pdfName": it.PdfName, "description": it.Description, "duration": it.Duration, "createdBy": it.CreatedBy,
					"createdAt": it.CreatedAt, "updatedAt": it.UpdatedAt, "modelNames": it.ModelNames, "isOther": it.IsOther,
					"isValid": it.IsValid, "typeId": it.TypeID, "codeId": it.CodeID,
				})
			}
			types := []map[string]any{}
			for _, t := range []int{1, 2, 3} {
				var valid *bool
				switch t {
				case 1:
					valid = is.ValidDiag
				case 2:
					valid = is.ValidRep
				}
				data := byType[t]
				if data == nil {
					if t == 3 {
						continue
					}
					data = []map[string]any{}
					names[t] = typeLabel(t, map[int]string{1: "Diagnostic", 2: "Repair"}[t])
				}
				types = append(types, map[string]any{"typeName": names[t], "typeId": t, "isValid": valid, "data": data})
			}
			out = append(out, map[string]any{"codeName": is.Code + " - " + is.Name, "codeId": is.ID, "data": types})
		}
		return out
	}
	var minutes int64
	hasOther := false
	for _, it := range items {
		if !it.IsOther {
			minutes += int64(it.Duration)
		} else {
			hasOther = true
		}
	}
	for _, is := range issues {
		if is.IsOther {
			hasOther = true
		}
	}
	var est any
	if minutes > 0 {
		est = formatHM(minutes * 60)
	}
	return map[string]any{"instructions": group(false), "otherInstructions": group(true), "estimatedTime": est, "hasOtherInstructions": hasOther}, nil
}

func (s *Server) ewChangeValidity(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	isSPV := o.ApprovedBy != nil && *o.ApprovedBy == c.User.ID
	isTech := (o.StatusID == maint.StatusInProgress || o.StatusID == maint.StatusPaused) && maint.IsAssigned(c, s.DB, o.ID, c.User.ID)
	if !isSPV && !isTech && !c.User.IsAdmin() && !c.User.Can("maintenanceorder", "approve") {
		return nil, httpx.Forbidden("")
	}
	if o.StatusID == maint.StatusRequested || o.StatusID == maint.StatusOpen {
		return nil, httpx.BadRequest("Validity can only be changed after the work has started")
	}
	var codes []struct {
		ID        int64 `json:"id"`
		ValidDiag *bool `json:"is_valid_diagnostic"`
		ValidRep  *bool `json:"is_valid_repair"`
	}
	if _, err := c.F.JSON("alarm_codes", &codes); err != nil {
		return nil, httpx.BadRequest("Invalid alarm codes")
	}
	var changes []struct {
		Event      string         `json:"event"`
		Attributes map[string]any `json:"attributes"`
	}
	_, _ = c.F.JSON("changed_validities", &changes)
	var vals []maint.IssueValidity
	for _, cd := range codes {
		vals = append(vals, maint.IssueValidity{AlarmCodeID: cd.ID, InstructionTypeID: 1, IsValid: cd.ValidDiag})
		if cd.ValidDiag != nil && *cd.ValidDiag {
			vals = append(vals, maint.IssueValidity{AlarmCodeID: cd.ID, InstructionTypeID: 2, IsValid: cd.ValidRep})
		}
	}
	if err := s.Maint.ChangeAlarmValidity(c, c.actor(), o.ID, nil, vals); err != nil {
		return nil, fail(err)
	}
	now := time.Now().UTC()
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		for i, ch := range changes {
			event := strings.TrimSpace(ch.Event)
			if event == "" {
				continue
			}
			details := ch.Attributes
			if details == nil {
				details = map[string]any{}
			}
			details["By"] = c.User.FullName
			if _, err := tx.Exec(c, `INSERT INTO dm.maintenance_order_events (order_id, status, details, created_by, created_at) VALUES ($1, $2, $3, $4, $5)`,
				o.ID, event, details, c.User.ID, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return "Validity successfully updated", nil
}

func validityText(v *bool, yes, no string) string {
	if v == nil {
		return "-"
	}
	if *v {
		return yes
	}
	return no
}

func (s *Server) ewExport(c *Ctx) (any, error) {
	rows, err := s.ewFiltered(c)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	techs, err := s.technicianNames(c, ids)
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range rows {
		issues, err := s.orderIssues(c, r.ID)
		if err != nil {
			return nil, err
		}
		var detected, other, diag, rep []string
		for _, is := range issues {
			label := is.Code + " - " + is.Name
			if is.IsOther {
				other = append(other, label)
				continue
			}
			detected = append(detected, label)
			diag = append(diag, validityText(is.ValidDiag, "Valid", "Invalid"))
			rep = append(rep, validityText(is.ValidRep, "Resolved", "Unresolved"))
		}
		params, err := db.Strings(c, s.DB, `SELECT mp.name FROM dm.maintenance_order_parameters p
			JOIN asset.equipment_parameters ep ON ep.id = p.equipment_parameter_id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			WHERE p.order_id = $1 ORDER BY mp.name`, r.ID)
		if err != nil {
			return nil, err
		}
		b, f, rm := s.locationLevelNames(tree, r.LocationID)
		end := "-"
		if r.End != nil {
			end = httpx.TS(*r.End)
		}
		dash := func(v []string) string {
			if len(v) == 0 {
				return "-"
			}
			return strings.Join(v, ", ")
		}
		out = append(out, map[string]any{
			"building": b, "floor": f, "room": rm, "equipment": r.AssetName, "order_id": r.Name, "detected_issue": dash(detected),
			"other_possible_issue": dash(other), "diagnostic_result": dash(diag), "repair_result": dash(rep), "related_parameter": dash(params),
			"planned_start": httpx.TS(r.Start), "planned_finish": end, "severity": r.Severity, "status": maint.StatusLabel(r.StatusID),
			"technician": dash(techs[r.ID]),
		})
	}
	return out, nil
}

func (s *Server) ewDiagnosticsOverview(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	return map[string]any{"is_compound": o.IsCompound, "is_deduction": o.IsDeduction}, nil
}

func (s *Server) ewParameterMessage(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT p.equipment_parameter_id, mp.id, mp.name, p.message, p.condition, p.detected_at
		FROM dm.maintenance_order_parameters p JOIN asset.equipment_parameters ep ON ep.id = p.equipment_parameter_id
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id WHERE p.order_id = $1 ORDER BY p.detected_at, mp.name`, o.ID)
	if err != nil {
		return nil, err
	}
	type pm struct {
		mpid     int64
		name     string
		message  *string
		cond     string
		detected time.Time
	}
	var params []pm
	for rows.Next() {
		var epid int64
		var p pm
		if err := rows.Scan(&epid, &p.mpid, &p.name, &p.message, &p.cond, &p.detected); err != nil {
			rows.Close()
			return nil, err
		}
		params = append(params, p)
	}
	rows.Close()
	out := []map[string]any{}
	for _, p := range params {
		alarms := []map[string]any{}
		arows, err := s.DB.Query(c, `SELECT ac.code, ac.name, COALESCE(i.alarm_status, 'NORMAL') FROM dm.alarm_mappings am
			JOIN dm.alarm_codes ac ON ac.id = am.alarm_code_id
			LEFT JOIN dm.maintenance_order_issues i ON i.order_id = $3 AND i.alarm_code_id = ac.id
			WHERE am.model_id = $1 AND am.model_parameter_id = $2 ORDER BY ac.code`, o.ModelID, p.mpid, o.ID)
		if err != nil {
			return nil, err
		}
		for arows.Next() {
			var code, name, status string
			if err := arows.Scan(&code, &name, &status); err != nil {
				arows.Close()
				return nil, err
			}
			alarms = append(alarms, map[string]any{"alarm_code": code, "alarm_name": name, "alarm_status": status})
		}
		arows.Close()
		out = append(out, map[string]any{"name": p.name, "message": p.message, "alarms": alarms, "detected_at": httpx.TS(p.detected)})
	}
	return out, nil
}

func (s *Server) ewAlarmTimestamps(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	main, other, err := maint.Issues(c, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	codes := append(append([]int64{}, main...), other...)
	ref := o.CreatedAt
	if o.DetectedAt != nil {
		ref = *o.DetectedAt
	}
	rows, err := s.DB.Query(c, `SELECT DISTINCT start_ts FROM ts.alarm_events WHERE asset_id = $1
		AND (alarm_code_id = ANY($2) OR equipment_parameter_id IN (SELECT equipment_parameter_id FROM dm.maintenance_order_parameters WHERE order_id = $5))
		AND start_ts >= $3 AND start_ts <= $4 ORDER BY start_ts`, o.AssetID, codes, ref.Add(-7*24*time.Hour), ref.Add(24*time.Hour), o.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ts := []string{}
	max := ref
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		ts = append(ts, httpx.TS(t))
		if t.After(max) {
			max = t
		}
	}
	return map[string]any{"timestamps": ts, "max_timestamp": httpx.TS(max)}, rows.Err()
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

func (s *Server) ewdBase(c *Ctx) ([]*ewRow, time.Time, time.Time, []int64, error) {
	assets, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, time.Time{}, time.Time{}, nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	rows, err := s.ewRows(c, assets, from, to, nil, "")
	return rows, from, to, assets, err
}

func withPercentages(items []map[string]any, total int) []map[string]any {
	for _, it := range items {
		n := it["count"].(int)
		p := 0.0
		if total > 0 {
			p = round2(float64(n) / float64(total) * 100)
		}
		it["percentage"] = p
	}
	return items
}

func (s *Server) ewdTypes(c *Ctx) (any, error) {
	rows, _, _, _, err := s.ewdBase(c)
	if err != nil {
		return nil, err
	}
	deg, comp := 0, 0
	for _, r := range rows {
		if r.Compound {
			comp++
		} else {
			deg++
		}
	}
	return withPercentages([]map[string]any{
		{"type_name": "Degradation", "count": deg},
		{"type_name": "Compound Degradation", "count": comp},
	}, deg+comp), nil
}

func (s *Server) ewdStatuses(c *Ctx) (any, error) {
	rows, _, _, _, err := s.ewdBase(c)
	if err != nil {
		return nil, err
	}
	counts := map[int]int{}
	for _, r := range rows {
		st := r.StatusID
		if st == maint.StatusPaused {
			st = maint.StatusInProgress
		}
		counts[st]++
	}
	items := []map[string]any{}
	for _, st := range []int{1, 2, 3, 4, 5, 6} {
		items = append(items, map[string]any{"status_name": maint.StatusLabel(st), "count": counts[st]})
	}
	return withPercentages(items, len(rows)), nil
}

func (s *Server) ewdParameterOccurrences(c *Ctx) (any, error) {
	assets, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	rows, err := s.DB.Query(c, `SELECT ev.name, count(*) FROM ts.alarm_events ev WHERE ev.alarm_type = 'ABNORMAL'
		AND ev.asset_id = ANY($1) AND ev.start_ts >= $2 AND ev.start_ts < $3 GROUP BY ev.name ORDER BY count(*) DESC, ev.name LIMIT 10`, assets, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"parameter_name": name, "count": n})
	}
	return out, rows.Err()
}

func (s *Server) ewdErrorCodeOccurrences(c *Ctx) (any, error) {
	assets, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	rows, err := s.DB.Query(c, `SELECT ac.code, count(*) FROM ts.alarm_events ev JOIN dm.alarm_codes ac ON ac.id = ev.alarm_code_id
		WHERE ev.alarm_type = 'ALARM' AND ev.asset_id = ANY($1) AND ev.start_ts >= $2 AND ev.start_ts < $3
		GROUP BY ac.code ORDER BY count(*) DESC, ac.code LIMIT 10`, assets, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var code string
		var n int64
		if err := rows.Scan(&code, &n); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"error_code": code, "count": n})
	}
	return out, rows.Err()
}

func (s *Server) ewdTrend(c *Ctx) (any, error) {
	rows, from, to, _, err := s.ewdBase(c)
	if err != nil {
		return nil, err
	}
	loc := c.Location()
	deg, comp := map[string]int{}, map[string]int{}
	for _, r := range rows {
		key := r.Detected.In(loc).Format("2006-01-02")
		if r.Compound {
			comp[key]++
		} else {
			deg[key]++
		}
	}
	out := []map[string]any{}
	cur := from.In(loc)
	cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc)
	for i := 0; cur.Before(to) && i < 3700; i++ {
		key := cur.Format("2006-01-02")
		out = append(out, map[string]any{"day": key, "degradation": deg[key], "compound_degradation": comp[key]})
		cur = cur.AddDate(0, 0, 1)
	}
	return out, nil
}

type issuePattern struct {
	codeID   int64
	code     string
	name     string // parameter name or alarm code name
	isParam  bool
	orderIDs []int64
	last     time.Time
}

// issuePatterns groups early warnings by detected alarm code and the
// parameter that revealed it (alarm code name when no parameter applies).
func (s *Server) issuePatterns(ctx context.Context, rows []*ewRow) ([]*issuePattern, error) {
	ids := make([]int64, 0, len(rows))
	detected := map[int64]time.Time{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		detected[r.ID] = r.Detected
	}
	if len(ids) == 0 {
		return nil, nil
	}
	params := map[int64][]string{}
	prow, err := s.DB.Query(ctx, `SELECT p.order_id, mp.name FROM dm.maintenance_order_parameters p
		JOIN asset.equipment_parameters ep ON ep.id = p.equipment_parameter_id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE p.order_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for prow.Next() {
		var id int64
		var name string
		if err := prow.Scan(&id, &name); err != nil {
			prow.Close()
			return nil, err
		}
		params[id] = append(params[id], name)
	}
	prow.Close()
	crow, err := s.DB.Query(ctx, `SELECT i.order_id, ac.id, ac.code, ac.name FROM dm.maintenance_order_issues i JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id
		WHERE i.order_id = ANY($1) AND NOT i.is_other`, ids)
	if err != nil {
		return nil, err
	}
	patterns := map[string]*issuePattern{}
	for crow.Next() {
		var oid, cid int64
		var code, cname string
		if err := crow.Scan(&oid, &cid, &code, &cname); err != nil {
			crow.Close()
			return nil, err
		}
		names := params[oid]
		isParam := len(names) > 0
		if !isParam {
			names = []string{cname}
		}
		for _, n := range names {
			key := fmt.Sprintf("%d|%s", cid, strings.ToLower(n))
			p := patterns[key]
			if p == nil {
				p = &issuePattern{codeID: cid, code: code, name: n, isParam: isParam}
				patterns[key] = p
			}
			p.orderIDs = append(p.orderIDs, oid)
			if detected[oid].After(p.last) {
				p.last = detected[oid]
			}
		}
	}
	crow.Close()
	out := make([]*issuePattern, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].orderIDs) != len(out[j].orderIDs) {
			return len(out[i].orderIDs) > len(out[j].orderIDs)
		}
		if out[i].code != out[j].code {
			return out[i].code < out[j].code
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

func (s *Server) ewdIssuePatternList(c *Ctx) (any, error) {
	rows, _, _, _, err := s.ewdBase(c)
	if err != nil {
		return nil, err
	}
	patterns, err := s.issuePatterns(c, rows)
	if err != nil {
		return nil, err
	}
	data := []map[string]any{}
	for _, p := range paginate(patterns, c.page(1), 6) {
		data = append(data, map[string]any{"alarm_code_id": p.codeID, "issue_code": p.code, "issue_name": p.name})
	}
	return map[string]any{"count": len(patterns), "data": data}, nil
}

// patternOrders returns the early warnings of a pattern within the period.
func (s *Server) patternOrders(c *Ctx) ([]*ewRow, *issuePattern, error) {
	assets, err := s.resolveAssets(c, assetFilter{})
	if err != nil {
		return nil, nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	rows, err := s.ewRows(c, assets, from, to, nil, "")
	if err != nil {
		return nil, nil, err
	}
	patterns, err := s.issuePatterns(c, rows)
	if err != nil {
		return nil, nil, err
	}
	codeID := c.F.Int("alarm_code_id", 0)
	name := strings.ToLower(c.F.StrTrim("model_parameter_name"))
	var match *issuePattern
	for _, p := range patterns {
		if p.codeID == codeID && strings.ToLower(p.name) == name {
			match = p
			break
		}
	}
	if match == nil {
		return nil, &issuePattern{codeID: codeID, name: c.F.StrTrim("model_parameter_name")}, nil
	}
	set := int64Set(match.orderIDs)
	return filterRows(rows, func(r *ewRow) bool { return set[r.ID] }), match, nil
}

func (s *Server) ewdIssuePatternChart(c *Ctx) (any, error) {
	rows, _, err := s.patternOrders(c)
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start_date", "end_date", false, 7)
	loc := c.Location()
	monthly := strings.EqualFold(c.F.StrTrim("type"), "monthly")
	counts := map[string]int{}
	for _, r := range rows {
		lt := r.Detected.In(loc)
		key := lt.Format("2006-01-02")
		if monthly {
			key = lt.Format("2006-01")
		}
		counts[key]++
	}
	out := []map[string]any{}
	cur := from.In(loc)
	if monthly {
		cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, loc)
	} else {
		cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc)
	}
	for i := 0; cur.Before(to) && i < 3700; i++ {
		if monthly {
			out = append(out, map[string]any{"timestamp": cur.Format("Jan 2006"), "count": counts[cur.Format("2006-01")]})
			cur = cur.AddDate(0, 1, 0)
		} else {
			out = append(out, map[string]any{"timestamp": cur.Format("02/01"), "count": counts[cur.Format("2006-01-02")]})
			cur = cur.AddDate(0, 0, 1)
		}
	}
	return out, nil
}

func (s *Server) ewdIssuePatternDetail(c *Ctx) (any, error) {
	rows, p, err := s.patternOrders(c)
	if err != nil {
		return nil, err
	}
	var code string
	_ = s.DB.QueryRow(c, `SELECT code FROM dm.alarm_codes WHERE id = $1`, p.codeID).Scan(&code)
	assets := map[int64]bool{}
	var repairSecs int64
	var repairs int64
	for _, r := range rows {
		assets[r.AssetID] = true
		if r.StatusID == maint.StatusDone || r.StatusID == maint.StatusClosed {
			repairSecs += maint.ActualSeconds(c, s.DB, r.ID)
			repairs++
		}
	}
	mttr := "-"
	if repairs > 0 {
		mttr = formatHM(repairSecs / repairs)
	}
	return map[string]any{"alarm_code_id": p.codeID, "issue_code": code, "issue_name": p.name, "total_count": len(rows),
		"mttr": mttr, "effected_unit": len(assets)}, nil
}
