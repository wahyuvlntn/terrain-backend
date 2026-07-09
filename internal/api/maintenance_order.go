package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/maint"
)

func (s *Server) registerMaintenanceOrder() {
	s.handle("maintenance_order/get_maintenance_order_list", AccessUser, s.moList)
	s.handle("maintenance_order/get_maintenance_status", AccessUser, s.moStatus)
	s.handle("maintenance_order/get_maintenance_name_and_type", AccessUser, s.moNameAndType)
	s.handle("maintenance_order/get_maintenance_order_types", AccessUser, s.options(`SELECT name, id::text FROM dm.maintenance_types ORDER BY id`))
	s.handle("maintenance_order/get_repeat_by", AccessUser, s.options(`SELECT name, id::text FROM dm.repeat_types ORDER BY id`))
	s.handle("maintenance_order/get_priorities", AccessUser, s.options(`SELECT name, id::text FROM dm.priorities ORDER BY id`))
	s.handle("maintenance_order/get_severities", AccessUser, s.options(`SELECT name, id::text FROM dm.severities ORDER BY id`))
	s.handle("maintenance_order/get_buildings", AccessUser, s.moBuildings)
	s.handle("maintenance_order/get_floors", AccessUser, s.moChildren("building_id"))
	s.handle("maintenance_order/get_rooms", AccessUser, s.moChildren("floor_id"))
	s.handle("maintenance_order/get_equipments", AccessUser, s.moEquipments)
	s.handle("maintenance_order/get_maintenance_instructions", AccessUser, s.moInstructionOptions)
	s.handle("maintenance_order/get_technicians", AccessUser, s.moTechnicians)
	s.handle("maintenance_order/get_maintenance_order_detail", AccessUser, s.moDetail)
	s.handle("maintenance_order/submit", AccessUser, s.moSubmit)
	s.handle("maintenance_order/delete_maintenance_order", AccessUser, s.moDelete)
	s.handle("maintenance_order/get_progress", AccessUser, s.moProgress)
	s.handle("maintenance_order/get_maintenance_supervisor_and_technicians", AccessUser, s.moPersonnel)
	s.handle("maintenance_order/get_maintenance_export_data", AccessUser, s.moExport)
	s.handle("maintenance_order/get_other_possible_issues", AccessUser, s.moOtherIssues)
	s.handle("maintenance_order/get_calendar_monthly", AccessUser, s.moCalendarMonthly)
	s.handle("maintenance_order/get_calendar_weekly", AccessUser, s.moCalendarWeekly)
}

// options returns [{label, value}] rows.
func (s *Server) options(sql string, args ...any) HandlerFunc {
	return func(c *Ctx) (any, error) {
		return s.optionRows(c, sql, args...)
	}
}

func (s *Server) optionRows(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var label, value string
		if err := rows.Scan(&label, &value); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"label": label, "value": value})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

type moRow struct {
	ID          int64
	Name        string
	AssetID     int64
	AssetAlias  string
	AssetName   string
	LocationID  int64
	Start       time.Time
	End         *time.Time
	StatusID    int
	Severity    string
	Priority    *string
	TypeID      int
	FromSystem  bool
	RequestedBy *string
	Description string
	IsCompound  bool
	IsDeduction bool
	CreatedAt   time.Time
}

type moQuery struct {
	assets     []int64
	from, to   *time.Time
	statuses   []int64
	severities []int64
	priorities []int64
	types      []int64
	techs      []string
	search     string
	system     *bool
}

func (s *Server) moQueryFromFilter(c *Ctx) (*moQuery, error) {
	jf, af, err := c.jsonFilter("filter")
	if err != nil {
		return nil, err
	}
	assets, err := s.resolveAssets(c, af)
	if err != nil {
		return nil, err
	}
	q := &moQuery{assets: assets, search: c.F.StrTrim("search")}
	if v, ok := anyInts(jf.StatusIDs); ok {
		q.statuses = v
		if q.statuses == nil {
			q.statuses = []int64{}
		}
		// "In Progress" includes paused work.
		for _, st := range v {
			if st == maint.StatusInProgress {
				q.statuses = append(q.statuses, maint.StatusPaused)
			}
		}
	}
	if v, ok := anyInts(jf.SeverityIDs); ok {
		q.severities = v
	}
	if v, ok := anyInts(jf.PriorityIDs); ok {
		q.priorities = v
	}
	if v, ok := anyInts(jf.TypeMaintIDs); ok {
		q.types = v
	}
	if v, ok := anyStrings(jf.Technicians); ok {
		q.techs = v
	}
	return q, nil
}

func (s *Server) moRows(ctx context.Context, q *moQuery, order string) ([]*moRow, error) {
	if order == "" {
		order = "o.planned_start DESC, o.id DESC"
	}
	nullIfNil := func(v []int64) any {
		if v == nil {
			return nil
		}
		return v
	}
	var techs any
	if q.techs != nil {
		techs = q.techs
	}
	rows, err := s.DB.Query(ctx, `SELECT o.id, o.name, e.id, e.alias, e.name, e.location_id, o.planned_start, o.planned_end, o.status_id,
			sv.name, pr.name, o.maintenance_type_id, o.from_system, COALESCE(NULLIF(ru.full_name, ''), ru.username), o.description,
			o.is_compound, o.is_deduction, o.created_at
		FROM dm.maintenance_orders o JOIN asset.equipment e ON e.id = o.asset_id
		JOIN dm.severities sv ON sv.id = o.severity_id LEFT JOIN dm.priorities pr ON pr.id = o.priority_id
		LEFT JOIN core.users ru ON ru.id = o.requested_by
		WHERE o.asset_id = ANY($1)
		AND ($2::timestamptz IS NULL OR COALESCE(o.planned_end, o.planned_start) >= $2)
		AND ($3::timestamptz IS NULL OR o.planned_start < $3)
		AND ($4::bigint[] IS NULL OR o.status_id = ANY($4))
		AND ($5::bigint[] IS NULL OR o.severity_id = ANY($5))
		AND ($6::bigint[] IS NULL OR o.priority_id = ANY($6))
		AND ($7::bigint[] IS NULL OR o.maintenance_type_id = ANY($7))
		AND ($8::text[] IS NULL OR EXISTS (SELECT 1 FROM dm.maintenance_order_technicians t JOIN core.users tu ON tu.id = t.user_id
			WHERE t.order_id = o.id AND lower(tu.username) = ANY(SELECT lower(x) FROM unnest($8::text[]) x)))
		AND ($9 = '' OR o.name ILIKE $10 OR e.name ILIKE $10 OR e.alias ILIKE $10 OR o.description ILIKE $10)
		AND ($11::boolean IS NULL OR o.from_system = $11)
		ORDER BY `+order, q.assets, q.from, q.to, nullIfNil(q.statuses), nullIfNil(q.severities), nullIfNil(q.priorities),
		nullIfNil(q.types), techs, q.search, ilike(q.search), q.system)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*moRow
	for rows.Next() {
		r := &moRow{}
		if err := rows.Scan(&r.ID, &r.Name, &r.AssetID, &r.AssetAlias, &r.AssetName, &r.LocationID, &r.Start, &r.End, &r.StatusID,
			&r.Severity, &r.Priority, &r.TypeID, &r.FromSystem, &r.RequestedBy, &r.Description, &r.IsCompound, &r.IsDeduction, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// technicianNames returns assignee display names per order.
func (s *Server) technicianNames(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT t.order_id, COALESCE(NULLIF(u.full_name, ''), u.username) FROM dm.maintenance_order_technicians t
		JOIN core.users u ON u.id = t.user_id WHERE t.order_id = ANY($1) ORDER BY lower(u.full_name)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

func moItem(r *moRow, techs []string) map[string]any {
	pr := ""
	if r.Priority != nil {
		pr = *r.Priority
	}
	return map[string]any{
		"id": r.ID, "name": r.Name, "equipment_id": r.AssetAlias, "planned_start": httpx.TS(r.Start),
		"status": maint.StatusCode(r.StatusID), "severity": r.Severity, "priority": pr, "assignee": strings.Join(techs, ", "),
		"is_corrective": r.TypeID == maint.TypeCorrective, "from_system": r.FromSystem, "requested_by": r.RequestedBy,
	}
}

func (s *Server) moList(c *Ctx) (any, error) {
	q, err := s.moQueryFromFilter(c)
	if err != nil {
		return nil, err
	}
	q.from = c.timeParam("start_time")
	q.to = c.timeParam("end_time")
	rows, err := s.moRows(c, q, "")
	if err != nil {
		return nil, err
	}
	const pageSize = 5
	page := int(c.page(1))
	all := make([]int64, 0, len(rows))
	for _, r := range rows {
		all = append(all, r.ID)
	}
	start := (page - 1) * pageSize
	if start > len(rows) {
		start = len(rows)
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	pageRows := rows[start:end]
	ids := make([]int64, 0, len(pageRows))
	for _, r := range pageRows {
		ids = append(ids, r.ID)
	}
	techs, err := s.technicianNames(c, ids)
	if err != nil {
		return nil, err
	}
	data := []map[string]any{}
	for _, r := range pageRows {
		data = append(data, moItem(r, techs[r.ID]))
	}
	return map[string]any{"data": data, "count": len(rows), "all_data_ids": all}, nil
}

// visibleOrder loads an order and checks location access.
func (s *Server) visibleOrder(c *Ctx, id int64) (*maint.Order, error) {
	o, err := s.Maint.Get(c, s.DB, id)
	if err != nil {
		return nil, fail(err)
	}
	if err := s.visibleAsset(c, o.AssetID); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Server) moStatus(c *Ctx) (any, error) {
	id := c.F.Int("order_id", 0)
	if id <= 0 {
		// The order page queries before an order is selected.
		return "", nil
	}
	o, err := s.visibleOrder(c, id)
	if err != nil {
		return nil, err
	}
	return maint.StatusLabel(o.StatusID), nil
}

func (s *Server) moNameAndType(c *Ctx) (any, error) {
	id := c.F.Int("maintenance_id", 0)
	if id <= 0 {
		return map[string]any{"name": "", "is_corrective": false}, nil
	}
	o, err := s.visibleOrder(c, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": o.Name, "is_corrective": o.TypeID == maint.TypeCorrective}, nil
}

// ---------------------------------------------------------------------------
// Location dropdowns (building / floor / room / equipment)
// ---------------------------------------------------------------------------

func (s *Server) moBuildings(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, id := range buildingIDs(tree, s.cfg.BuildingLevel) {
		if scope.AllowsLocation(id) {
			out = append(out, map[string]any{"label": tree.name(id), "value": strconv.FormatInt(id, 10)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i]["label"].(string)) < strings.ToLower(out[j]["label"].(string))
	})
	return out, nil
}

// buildingIDs lists locations at the configured building depth.
func buildingIDs(tree *locTree, level int) []int64 {
	var out []int64
	var walk func(id int64, depth int)
	walk = func(id int64, depth int) {
		if depth == level {
			out = append(out, id)
			return
		}
		for _, ch := range tree.nodes[id].Children {
			walk(ch, depth+1)
		}
	}
	for _, r := range tree.roots {
		walk(r, 0)
	}
	return out
}

func (s *Server) moChildren(key string) HandlerFunc {
	return func(c *Ctx) (any, error) {
		parent := c.F.Int(key, 0)
		tree, err := s.loadLocations(c)
		if err != nil {
			return nil, err
		}
		scope, err := c.Scope()
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		n := tree.nodes[parent]
		if n == nil {
			return out, nil
		}
		for _, ch := range n.Children {
			if scope.AllowsLocation(ch) {
				out = append(out, map[string]any{"label": tree.name(ch), "value": strconv.FormatInt(ch, 10)})
			}
		}
		return out, nil
	}
}

func (s *Server) moEquipments(c *Ctx) (any, error) {
	room := c.F.Int("room_id", 0)
	if room <= 0 {
		return []map[string]any{}, nil
	}
	ids, err := s.locSubtreeAssets(c, room)
	if err != nil {
		return nil, err
	}
	return s.optionRows(c, `SELECT name, id::text FROM asset.equipment WHERE id = ANY($1) ORDER BY lower(name)`, ids)
}

// locationLevels returns the building/floor/room ids of an equipment.
func (s *Server) locationLevels(tree *locTree, locID int64) (string, string, string) {
	chain := tree.ancestors(locID)
	bl := s.cfg.BuildingLevel
	at := func(i int) string {
		if i >= 0 && i < len(chain) {
			return strconv.FormatInt(chain[i], 10)
		}
		return ""
	}
	if len(chain) <= bl {
		return at(len(chain) - 1), "", ""
	}
	return at(bl), at(bl + 1), at(bl + 2)
}

func (s *Server) locationLevelNames(tree *locTree, locID int64) (string, string, string) {
	b, f, r := s.locationLevels(tree, locID)
	name := func(v string) string {
		if v == "" {
			return "-"
		}
		id, _ := strconv.ParseInt(v, 10, 64)
		return tree.name(id)
	}
	return name(b), name(f), name(r)
}

// ---------------------------------------------------------------------------
// Issues (alarm codes) and technicians
// ---------------------------------------------------------------------------

func (s *Server) moInstructionOptions(c *Ctx) (any, error) {
	assetID := c.F.Int("asset_id", 0)
	if assetID <= 0 {
		return []map[string]any{}, nil
	}
	var modelID int64
	if err := s.DB.QueryRow(c, `SELECT model_id FROM asset.equipment WHERE id = $1`, assetID).Scan(&modelID); err != nil {
		if db.IsNoRows(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	// Selectable issues are the alarm codes with instructions for the model;
	// codes already attached to orders of the equipment (e.g. raised by an
	// early warning) are included so existing orders always resolve.
	rows, err := s.DB.Query(c, `WITH model_ins AS (
			SELECT i.alarm_code_id, i.duration, i.instruction_type_id FROM dm.instructions i
			JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = $1
			WHERE NOT i.is_deleted AND i.alarm_code_id IS NOT NULL
		), codes AS (
			SELECT alarm_code_id AS id FROM model_ins
			UNION SELECT moi.alarm_code_id FROM dm.maintenance_order_issues moi JOIN dm.maintenance_orders mo ON mo.id = moi.order_id
			WHERE mo.asset_id = $2
		)
		SELECT ac.id, ac.code, ac.name, COALESCE(sum(mi.duration), 0),
			COALESCE(bool_or(mi.instruction_type_id = 1), FALSE), COALESCE(bool_or(mi.instruction_type_id = 2), FALSE)
		FROM codes JOIN dm.alarm_codes ac ON ac.id = codes.id LEFT JOIN model_ins mi ON mi.alarm_code_id = ac.id
		GROUP BY ac.id, ac.code, ac.name, ac.order_id ORDER BY ac.order_id, ac.code`, modelID, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, minutes int64
		var code, name string
		var diag, rep bool
		if err := rows.Scan(&id, &code, &name, &minutes, &diag, &rep); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"label": code + " - " + name, "value": strconv.FormatInt(id, 10),
			"estimated_time_minute": minutes, "is_diagnostic": diag, "is_repair": rep})
	}
	return out, rows.Err()
}

func (s *Server) moTechnicians(c *Ctx) (any, error) {
	building := c.F.Int("building_id", 0)
	equipment := c.F.Int("equipment_id", 0)
	maintenanceID := c.F.Int("maintenance_id", 0)
	var typeID int64
	if equipment > 0 {
		_ = s.DB.QueryRow(c, `SELECT m.type_id FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id WHERE e.id = $1`, equipment).Scan(&typeID)
	}
	rows, err := s.DB.Query(c, `SELECT u.username, COALESCE(NULLIF(u.full_name, ''), u.username), u.is_deleted,
			EXISTS (SELECT 1 FROM dm.maintenance_order_technicians t WHERE t.order_id = $3 AND t.user_id = u.id) AS assigned
		FROM core.users u
		WHERE u.id IN (`+personnelSQL+`)
		AND ($1 <= 0 OR NOT EXISTS (SELECT 1 FROM dm.personnel_buildings pb WHERE pb.user_id = u.id)
			OR EXISTS (SELECT 1 FROM dm.personnel_buildings pb WHERE pb.user_id = u.id AND pb.location_id = $1))
		AND ($2 <= 0 OR NOT EXISTS (SELECT 1 FROM dm.personnel_equipment_types pt WHERE pt.user_id = u.id)
			OR EXISTS (SELECT 1 FROM dm.personnel_equipment_types pt WHERE pt.user_id = u.id AND pt.type_id = $2))
		ORDER BY u.is_deleted, lower(u.full_name)`, building, typeID, maintenanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var username, name string
		var deleted, assigned bool
		if err := rows.Scan(&username, &name, &deleted, &assigned); err != nil {
			return nil, err
		}
		if deleted {
			if !assigned {
				continue
			}
			name += " (deleted)"
		}
		out = append(out, map[string]any{"label": name, "value": username})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Detail / submit / delete
// ---------------------------------------------------------------------------

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func userDisplay(ctx context.Context, q db.Querier, id *int64) string {
	if id == nil {
		return ""
	}
	var name string
	_ = q.QueryRow(ctx, `SELECT COALESCE(NULLIF(full_name, ''), username) FROM core.users WHERE id = $1`, *id).Scan(&name)
	return name
}

func usernameByID(ctx context.Context, q db.Querier, id *int64) string {
	if id == nil {
		return ""
	}
	var name string
	_ = q.QueryRow(ctx, `SELECT username FROM core.users WHERE id = $1`, *id).Scan(&name)
	return name
}

func (s *Server) moDetail(c *Ctx) (any, error) {
	id := c.F.Int("order_id", 0)
	o, err := s.Maint.Get(c, s.DB, id)
	if err != nil {
		return []any{}, nil
	}
	if err := s.visibleAsset(c, o.AssetID); err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	b, f, r := s.locationLevels(tree, o.LocationID)
	main, other, err := maint.Issues(c, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	techs, err := maint.Technicians(c, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	var repeatType *string
	var repeatEnd any
	if o.ScheduleID != nil {
		var rt int
		var re *time.Time
		if err := s.DB.QueryRow(c, `SELECT repeat_type_id, repeat_end FROM dm.maintenance_schedules WHERE id = $1`, *o.ScheduleID).Scan(&rt, &re); err == nil {
			v := strconv.Itoa(rt)
			repeatType = &v
			repeatEnd = httpx.TSPtr(re)
		}
	}
	var priority *string
	if o.PriorityID != nil {
		v := strconv.Itoa(*o.PriorityID)
		priority = &v
	}
	var updated any
	if o.UpdatedAt != nil {
		updated = httpx.TS(*o.UpdatedAt)
	}
	var planEnd any
	if o.PlannedEnd != nil {
		planEnd = httpx.TS(*o.PlannedEnd)
	}
	approved := userDisplay(c, s.DB, o.ApprovedBy)
	if o.StatusID == maint.StatusRequested {
		approved = ""
	}
	return []map[string]any{{
		"id": o.ID, "actual_time": maint.ActualSeconds(c, s.DB, o.ID), "estimated_time": maint.EstimatedSeconds(c, s.DB, o),
		"building": b, "floor": f, "room": r, "equipment": strconv.FormatInt(o.AssetID, 10),
		"created_at": httpx.TS(o.CreatedAt), "description": o.Description, "maintenance_id": o.ScheduleID,
		"maintenance_type_id": strconv.Itoa(o.TypeID), "planned_start": httpx.TS(o.PlannedStart), "planned_end": planEnd,
		"priority": priority, "repeat_end": repeatEnd, "repeat_type_id": repeatType,
		"requested_by": func() string {
			if o.RequestedBy == nil {
				if o.FromSystem {
					return "System"
				}
				return "-"
			}
			return userDisplay(c, s.DB, o.RequestedBy)
		}(),
		"severity": strconv.Itoa(o.SeverityID), "status": maint.StatusCode(o.StatusID), "updated_at": updated,
		"instructions": joinIDs(main), "technicians": strings.Join(techs, ","), "approved_by": approved,
		"edited_by": userDisplay(c, s.DB, o.EditedBy), "reject_reason": o.RejectReason, "is_started": o.StartedAt != nil,
		"issue_relation_id": o.IssueRelationID, "other_alarm_codes": joinIDs(other),
	}}, nil
}

func (s *Server) moSubmit(c *Ctx) (any, error) {
	action := strings.ToUpper(c.F.StrTrim("submit_type"))
	switch action {
	case maint.ActionCreated:
		if err := c.Require("maintenanceorder", "create"); err != nil {
			return nil, err
		}
	case maint.ActionApproved, maint.ActionRejected:
		if err := c.Require("maintenanceorder", "approve"); err != nil {
			return nil, err
		}
	case maint.ActionSaved:
		if !c.User.Can("maintenanceorder", "update") && !c.User.Can("maintenanceorder", "approve") {
			return nil, httpx.Forbidden("")
		}
	default:
		return nil, httpx.BadRequest("err_invalid_operation")
	}
	loc := c.Location()
	req := maint.SubmitRequest{
		Action:       action,
		ID:           c.F.IntPtr("id"),
		AssetID:      c.F.IntPtr("asset_id"),
		TypeID:       c.F.IntPtr("maintenance_type_id"),
		PriorityID:   c.F.IntPtr("priority_id"),
		SeverityID:   c.F.IntPtr("severity_id"),
		StatusID:     c.F.IntPtr("status_id"),
		Description:  c.F.StrPtr("description"),
		PlannedStart: c.F.TimePtr("planned_start", loc),
		PlannedEnd:   c.F.TimePtr("planned_end", loc),
		RepeatTypeID: c.F.IntPtr("repeat_type_id"),
		RepeatEnd:    c.F.TimePtr("repeat_end", loc),
		RejectReason: c.F.StrPtr("reject_reason"),
		IsRecurring:  c.F.Bool("is_recurring", false),
		RequestedBy:  c.F.NonEmptyPtr("requested_by"),
	}
	if c.F.Has("instructions") {
		req.Issues, req.IssuesSet = httpx.ParseIDList(c.F.Str("instructions")), true
	}
	if c.F.Has("other_alarm_codes") {
		req.OtherIssues, req.OtherSet = httpx.ParseIDList(c.F.Str("other_alarm_codes")), true
	}
	if c.F.Has("technicians") {
		list, _ := c.F.StringList("technicians")
		req.Technicians, req.TechSet = list, true
	}
	if req.AssetID != nil {
		if err := s.visibleAsset(c, *req.AssetID); err != nil {
			return nil, err
		}
	}
	if req.ID != nil {
		if _, err := s.visibleOrder(c, *req.ID); err != nil {
			return nil, err
		}
	}
	res, err := s.Maint.Submit(c, c.actor(), req)
	if err != nil {
		return nil, fail(err)
	}
	result := []map[string]any{}
	for _, id := range res.OrderIDs {
		result = append(result, map[string]any{"maintenance_id": id})
	}
	return map[string]any{"message": res.Message, "result": result}, nil
}

func (s *Server) moDelete(c *Ctx) (any, error) {
	if err := c.Require("maintenanceorder", "delete"); err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	if _, err := s.visibleOrder(c, id); err != nil {
		return nil, err
	}
	n, err := s.Maint.Delete(c, id, c.F.Bool("is_recurring", false))
	if err != nil {
		return nil, fail(err)
	}
	if n > 1 {
		return fmt.Sprintf("%d maintenance orders successfully deleted", n), nil
	}
	return "Maintenance order successfully deleted", nil
}

func (s *Server) moProgress(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("order_id", 0))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT created_at, status, details FROM dm.maintenance_order_events WHERE order_id = $1 ORDER BY created_at DESC, id DESC`, o.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var at time.Time
		var status string
		var details map[string]any
		if err := rows.Scan(&at, &status, &details); err != nil {
			return nil, err
		}
		if details == nil {
			details = map[string]any{}
		}
		out = append(out, map[string]any{"datetime": httpx.TS(at), "status": status, "details": details})
	}
	return out, rows.Err()
}

func (s *Server) moPersonnel(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	sup := userDisplay(c, s.DB, o.ApprovedBy)
	if sup == "" {
		sup = "-"
	}
	techs, err := s.technicianNames(c, []int64{o.ID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"supervisor": sup, "technicians": emptyIfNil(techs[o.ID])}, nil
}

// issueLabels returns "code - name" labels of an order's main issues.
func (s *Server) issueLabels(ctx context.Context, ids []int64) (map[int64][]string, map[int64][]string, error) {
	main := map[int64][]string{}
	other := map[int64][]string{}
	if len(ids) == 0 {
		return main, other, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT i.order_id, c.code || ' - ' || c.name, i.is_other FROM dm.maintenance_order_issues i
		JOIN dm.alarm_codes c ON c.id = i.alarm_code_id WHERE i.order_id = ANY($1) ORDER BY i.position`, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var label string
		var isOther bool
		if err := rows.Scan(&id, &label, &isOther); err != nil {
			return nil, nil, err
		}
		if isOther {
			other[id] = append(other[id], label)
		} else {
			main[id] = append(main[id], label)
		}
	}
	return main, other, rows.Err()
}

func (s *Server) moExport(c *Ctx) (any, error) {
	q, err := s.moQueryFromFilter(c)
	if err != nil {
		return nil, err
	}
	q.from = c.timeParam("start_time")
	q.to = c.timeParam("end_time")
	rows, err := s.moRows(c, q, "o.planned_start, o.id")
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
	issues, _, err := s.issueLabels(c, ids)
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i, r := range rows {
		b, f, rm := s.locationLevelNames(tree, r.LocationID)
		var issue any
		if len(issues[r.ID]) > 0 {
			issue = strings.Join(issues[r.ID], ", ")
		}
		pr := "-"
		if r.Priority != nil {
			pr = *r.Priority
		}
		typ := "PREVENTIVE"
		if r.TypeID == maint.TypeCorrective {
			typ = "CORRECTIVE"
		}
		var end any
		if r.End != nil {
			end = httpx.TS(*r.End)
		}
		out = append(out, map[string]any{
			"no": i + 1, "order_id": r.Name, "planned_start": httpx.TS(r.Start), "planned_end": end, "maintenance_type": typ,
			"priority": pr, "issue": issue, "severity": r.Severity, "building": b, "floor": f, "room": rm, "equipment": r.AssetName,
			"status": maint.StatusLabel(r.StatusID), "requested_by": derefStr(r.RequestedBy), "technician": strings.Join(techs[r.ID], ", "),
		})
	}
	return out, nil
}

// relatedCodes returns alarm codes connected (as targets) in the issue
// relations relevant to an order.
func (s *Server) relatedCodes(ctx context.Context, o *maint.Order) ([]int64, error) {
	var flows [][]byte
	var err error
	if o.IssueRelationID != nil {
		var f []byte
		if err := s.DB.QueryRow(ctx, `SELECT flow FROM dm.issue_relations WHERE id = $1`, *o.IssueRelationID).Scan(&f); err == nil {
			flows = append(flows, f)
		}
	} else {
		rows, err := s.DB.Query(ctx, `SELECT flow FROM dm.issue_relations WHERE model_id = $1`, o.ModelID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f []byte
			if err := rows.Scan(&f); err != nil {
				rows.Close()
				return nil, err
			}
			flows = append(flows, f)
		}
		rows.Close()
	}
	main, _, err := maint.Issues(ctx, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	mainSet := int64Set(main)
	codes, err := s.alarmCodeIndex(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []int64
	for _, f := range flows {
		targets := flowTargetCodes(f, codes)
		related := o.IssueRelationID != nil
		for _, t := range targets {
			if mainSet[t] {
				related = true
			}
		}
		if !related {
			continue
		}
		for _, t := range targets {
			if !mainSet[t] && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (s *Server) moOtherIssues(c *Ctx) (any, error) {
	o, err := s.visibleOrder(c, c.F.Int("maintenance_id", 0))
	if err != nil {
		return nil, err
	}
	ids, err := s.relatedCodes(c, o)
	if err != nil {
		return nil, err
	}
	_, other, err := maint.Issues(c, s.DB, o.ID)
	if err != nil {
		return nil, err
	}
	ids = uniqueInt64(append(ids, other...))
	out := []map[string]any{}
	for _, id := range ids {
		out = append(out, map[string]any{"id": id})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Calendar
// ---------------------------------------------------------------------------

func (s *Server) moCalendarMonthly(c *Ctx) (any, error) {
	q, err := s.moQueryFromFilter(c)
	if err != nil {
		return nil, err
	}
	loc := c.Location()
	month := int(c.F.Int("month", int64(time.Now().In(loc).Month())-1))
	year := int(c.F.Int("year", int64(time.Now().In(loc).Year())))
	if month < 0 || month > 11 {
		return nil, httpx.BadRequest("err_invalid_date")
	}
	start := time.Date(year, time.Month(month+1), 1, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)
	su, eu := start.UTC(), end.UTC()
	q.to = &eu
	rows, err := s.moRows(c, q, "o.planned_start, o.id")
	if err != nil {
		return nil, err
	}
	var inMonth []*moRow
	for _, r := range rows {
		if !r.Start.Before(su) {
			inMonth = append(inMonth, r)
		}
	}
	ids := make([]int64, 0, len(inMonth))
	for _, r := range inMonth {
		ids = append(ids, r.ID)
	}
	techs, err := s.technicianNames(c, ids)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range inMonth {
		out = append(out, moItem(r, techs[r.ID]))
	}
	return out, nil
}

func (s *Server) moCalendarWeekly(c *Ctx) (any, error) {
	q, err := s.moQueryFromFilter(c)
	if err != nil {
		return nil, err
	}
	from := c.timeParam("start_time")
	to := c.timeParam("end_time")
	if from == nil || to == nil || !to.After(*from) {
		return nil, httpx.BadRequest("err_invalid_date_range")
	}
	q.from, q.to = from, to
	rows, err := s.moRows(c, q, "o.planned_start, o.id")
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
	loc := c.Location()
	out := []map[string]any{}
	for _, r := range rows {
		end := r.Start.Add(time.Hour)
		if r.End != nil && r.End.After(r.Start) {
			end = *r.End
		}
		// Split into per-day segments (client time zone) within the range.
		segStart := r.Start
		for i := 0; segStart.Before(end) && i < 62; i++ {
			ls := segStart.In(loc)
			dayEnd := time.Date(ls.Year(), ls.Month(), ls.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			segEnd := end
			if segEnd.After(dayEnd) {
				segEnd = dayEnd.Add(-time.Minute)
			}
			if segEnd.After(*from) && segStart.Before(*to) {
				item := moItem(r, techs[r.ID])
				item["planned_start"] = httpx.TS(segStart)
				item["planned_end"] = httpx.TS(segEnd)
				item["planned_start_sc"] = httpx.TS(r.Start)
				item["planned_end_sc"] = httpx.TS(end)
				out = append(out, item)
			}
			segStart = dayEnd
		}
	}
	return out, nil
}
