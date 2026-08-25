package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Seeder creates the plant configuration through the backend API. Every step
// checks what already exists, so seeding can be resumed after a failure.
type Seeder struct {
	c        *Client
	db       *pgxpool.Pool
	password string
}

func (s *Seeder) call(ctx context.Context, script string, form map[string]any) (any, error) {
	data, err := s.c.Super(ctx, script, form)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", script, err)
	}
	return data, nil
}

// Run applies the whole configuration.
func (s *Seeder) Run(ctx context.Context) error {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"master data", s.masterData},
		{"locations", s.locations},
		{"models", s.models},
		{"data sources", s.gateways},
		{"equipment", s.equipment},
		{"alarm mapping", s.alarmMapping},
		{"issue mapping", s.issueMapping},
		{"users", s.users},
		{"personnel", s.personnel},
		{"instructions", s.instructions},
	}
	for _, st := range steps {
		slog.Info("seeding", "step", st.name)
		if err := st.fn(ctx); err != nil {
			return fmt.Errorf("seed %s: %w", st.name, err)
		}
	}
	return nil
}

func (s *Seeder) masterData(ctx context.Context) error {
	add := func(table, sqlTable, name string) error {
		if exists(ctx, s.db, `SELECT 1 FROM `+sqlTable+` WHERE lower(name) = lower($1)`, name) {
			return nil
		}
		_, err := s.call(ctx, "master/addtypebrandseries", map[string]any{"table": table, "field_name": name})
		return err
	}
	for _, t := range equipmentTypes {
		if err := add("types", "asset.types", t); err != nil {
			return err
		}
	}
	for _, b := range brands {
		if err := add("brands", "asset.brands", b); err != nil {
			return err
		}
	}
	for _, sr := range series {
		if err := add("series", "asset.series", sr); err != nil {
			return err
		}
	}
	for _, c := range alarmCodes {
		if exists(ctx, s.db, `SELECT 1 FROM dm.alarm_codes WHERE lower(code) = lower($1)`, c[0]) {
			continue
		}
		if _, err := s.call(ctx, "master/addtypebrandseries", map[string]any{"table": "alarms", "field_name": c[0] + " - " + c[1]}); err != nil {
			return err
		}
	}
	for _, cat := range issueCategories {
		if !exists(ctx, s.db, `SELECT 1 FROM asset.feedback_categories WHERE lower(name) = lower($1)`, cat.Name) {
			if _, err := s.call(ctx, "master/addcategory", map[string]any{"schema": "feedback", "table": "feedback_categories", "category_name": cat.Name}); err != nil {
				return err
			}
		}
		catID := idOf(ctx, s.db, `SELECT id FROM asset.feedback_categories WHERE lower(name) = lower($1)`, cat.Name)
		for _, is := range cat.Issues {
			if exists(ctx, s.db, `SELECT 1 FROM asset.feedback_issues WHERE category_id = $1 AND lower(name) = lower($2)`, catID, is[1]) {
				continue
			}
			if _, err := s.call(ctx, "master/addfeedbackissue", map[string]any{"category_id": catID, "name": is[1], "issue_id": is[0]}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Seeder) locationID(ctx context.Context, name string) int64 {
	return idOf(ctx, s.db, `SELECT id FROM asset.locations WHERE name = $1`, name)
}

func (s *Seeder) locations(ctx context.Context) error {
	for _, l := range locations {
		if s.locationID(ctx, l.Name) != 0 {
			continue
		}
		form := map[string]any{"name": l.Name, "availability_th": l.Avail, "performance_th": l.Perf, "quality_th": 99,
			"location_image": File{Name: strings.ReplaceAll(strings.ToLower(l.Name), " ", "_") + ".png", Type: "image/png", Data: locationImage(l.Image)}}
		if l.Parent != "" {
			form["parent_id"] = s.locationID(ctx, l.Parent)
		}
		if _, err := s.call(ctx, "location/insertnewlocation", form); err != nil {
			return err
		}
	}
	return nil
}

func (s *Seeder) modelID(ctx context.Context, name string) int64 {
	return idOf(ctx, s.db, `SELECT id FROM asset.models WHERE name = $1`, name)
}

func issueID(ctx context.Context, db *pgxpool.Pool, code string) int64 {
	return idOf(ctx, db, `SELECT id FROM asset.feedback_issues WHERE issue_code = $1`, code)
}

func (s *Seeder) models(ctx context.Context) error {
	for _, m := range models() {
		id := s.modelID(ctx, m.Name)
		if id == 0 {
			form := map[string]any{"name": m.Name, "description": m.Description,
				"type_id":   idOf(ctx, s.db, `SELECT id FROM asset.types WHERE name = $1`, m.Type),
				"brand_id":  idOf(ctx, s.db, `SELECT id FROM asset.brands WHERE name = $1`, m.Brand),
				"series_id": idOf(ctx, s.db, `SELECT id FROM asset.series WHERE name = $1`, m.Series)}
			if m.Base != "" {
				form["base_model_id"] = s.modelID(ctx, m.Base)
			}
			if _, err := s.call(ctx, "model_manager/add_model", form); err != nil {
				return err
			}
			id = s.modelID(ctx, m.Name)
		}
		// descriptions follow the definition (e.g. the plant controller now
		// covers the CHW and CW loops)
		if _, err := s.db.Exec(ctx, `UPDATE asset.models SET description = $2 WHERE id = $1 AND description IS DISTINCT FROM $2`, id, m.Description); err != nil {
			return fmt.Errorf("description of %s: %w", m.Name, err)
		}
		if m.Base != "" {
			// Extended models inherit variables, alarms and status scripts;
			// only overrides are applied.
			for code, cond := range m.Overrides {
				aid := idOf(ctx, s.db, `SELECT id FROM asset.model_alarms WHERE model_id = $1 AND name = $2`, id, code)
				var al AlarmSpec
				for _, a := range m.Alarms {
					if a.Code == code {
						al = a
					}
				}
				crit := 1
				if al.Critical {
					crit = 2
				}
				if _, err := s.call(ctx, "model_manager/update_model_alarm", map[string]any{"model_id": id, "alarm_id": aid, "name": code,
					"criticality": crit, "issue_id": issueID(ctx, s.db, al.Issue), "message": strings.ReplaceAll(al.Message, "110", fnum(cond.Value)),
					"script": cond.Script()}); err != nil {
					return err
				}
			}
			continue
		}
		for _, p := range m.Params {
			if exists(ctx, s.db, `SELECT 1 FROM asset.model_parameters WHERE model_id = $1 AND alias = $2`, id, p.Alias) {
				continue
			}
			form := map[string]any{"model_id": id, "parameter_alias": p.Alias, "parameter_name": p.Name, "parameter_unit": p.Unit,
				"parameter_data_type": "number"}
			if p.Bool {
				form["parameter_data_type"] = "boolean"
			} else {
				form["parameter_scale"] = p.Scale
			}
			if _, err := s.call(ctx, "model_manager/add_model_parameter", form); err != nil {
				return err
			}
		}
		if err := s.controls(ctx, id, m); err != nil {
			return err
		}
		if _, err := s.call(ctx, "model_manager/upsert_model_status_definitions", map[string]any{"model_id": id,
			"running_script": "running.v == true", "unplanned_down_script": "trip.v == true", "planned_down_script": "maint_mode.v == true"}); err != nil {
			return err
		}
		for _, a := range m.Alarms {
			crit := 1
			if a.Critical {
				crit = 2
			}
			form := map[string]any{"model_id": id, "name": a.Code, "criticality": crit,
				"issue_id": issueID(ctx, s.db, a.Issue), "message": a.Message, "script": a.Cond.Script()}
			aid := idOf(ctx, s.db, `SELECT id FROM asset.model_alarms WHERE model_id = $1 AND name = $2`, id, a.Code)
			if aid == 0 {
				if _, err := s.call(ctx, "model_manager/add_model_alarm", form); err != nil {
					return err
				}
				continue
			}
			// an alarm whose definition changed (e.g. a deviation now taken from
			// a set point parameter) is updated in place
			if exists(ctx, s.db, `SELECT 1 FROM asset.model_alarms WHERE id = $1 AND script IS NOT DISTINCT FROM $2 AND message = $3`,
				aid, a.Cond.Script(), a.Message) {
				continue
			}
			form["alarm_id"] = aid
			if _, err := s.call(ctx, "model_manager/update_model_alarm", form); err != nil {
				return err
			}
		}
		if m.RunHourWarn > 0 {
			if _, err := s.call(ctx, "model_manager/update_running_hour_limit", map[string]any{"model_id": id, "warning_hour": m.RunHourWarn,
				"critical_hour": m.RunHourCrit, "is_notify_warning": true, "is_notify_critical": true, "is_auto_create_maintenance": m.AutoPM}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Seeder) gateways(ctx context.Context) error {
	for _, g := range gateways {
		if !exists(ctx, s.db, `SELECT 1 FROM dh.gateways WHERE lower(alias) = lower($1)`, g.Alias) {
			if _, err := s.call(ctx, "gateway/addgateway", map[string]any{"alias": g.Alias, "name": g.Name, "type": "MQTT_SERVER",
				"description": g.Description, "params": "{}"}); err != nil {
				return err
			}
		}
		var tags []map[string]string
		for _, e := range equipment() {
			if e.Gateway != g.Alias {
				continue
			}
			m := findModel(e.Model)
			for _, p := range m.Params {
				addr := e.Alias + "/" + p.Alias
				if !exists(ctx, s.db, `SELECT 1 FROM dh.gateway_tags t JOIN dh.gateways g ON g.id = t.gateway_id WHERE g.alias = $1 AND t.name = $2`, g.Alias, addr) {
					tags = append(tags, map[string]string{"name": addr, "address": addr, "description": e.Name + " - " + p.Name})
				}
			}
		}
		if len(tags) > 0 {
			if _, err := s.call(ctx, "gateway/addgatewaytags", map[string]any{"gateway_alias": g.Alias, "gateway_tags": toJSON(tags)}); err != nil {
				return err
			}
		}
		if !exists(ctx, s.db, `SELECT 1 FROM dh.gateways WHERE alias = $1 AND is_active`, g.Alias) {
			if _, err := s.call(ctx, "gateway/turnongateway", map[string]any{"gateway_alias": g.Alias}); err != nil {
				return err
			}
		}
	}
	return nil
}

func findModel(name string) *ModelSpec {
	for _, m := range models() {
		if m.Name == name {
			return m
		}
	}
	return nil
}

func (s *Seeder) equipmentID(ctx context.Context, alias string) int64 {
	return idOf(ctx, s.db, `SELECT id FROM asset.equipment WHERE alias = $1`, alias)
}

func (s *Seeder) equipment(ctx context.Context) error {
	for _, e := range equipment() {
		m := findModel(e.Model)
		if s.equipmentID(ctx, e.Alias) == 0 {
			if _, err := s.call(ctx, "equipment_manager/add_equipment", map[string]any{"alias": e.Alias, "name": e.Name,
				"model_id": s.modelID(ctx, e.Model), "location_id": s.locationID(ctx, e.Location), "installation_date": e.Installed,
				"image_file": File{Name: strings.ToLower(e.Alias) + ".png", Type: "image/png", Data: equipmentImage(m.Key)}}); err != nil {
				return err
			}
		}
		id := s.equipmentID(ctx, e.Alias)
		// Map every parameter that has no tag yet: all of them on new equipment,
		// the parameters added to a model on existing equipment.
		unmapped, err := s.unmappedParams(ctx, id)
		if err != nil {
			return err
		}
		mappings := map[string]any{}
		for _, p := range m.Params {
			if unmapped[p.Alias] {
				mappings[p.Alias] = map[string]any{"gateway_alias": e.Gateway, "tag": e.Alias + "/" + p.Alias, "is_logged": true, "is_displayed": true}
			}
		}
		if len(mappings) > 0 {
			if _, err := s.call(ctx, "equipment_manager/update_equipment_parameter_mapping", map[string]any{"alias": e.Alias, "mappings": toJSON(mappings)}); err != nil {
				return err
			}
		}
		for _, p := range m.Params {
			if p.Warn == nil && p.Crit == nil {
				continue
			}
			epid := idOf(ctx, s.db, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
				WHERE ep.equipment_id = $1 AND mp.alias = $2`, id, p.Alias)
			form := map[string]any{"parameter_id": epid, "warning_threshold_type": "", "critical_threshold_type": ""}
			put := func(prefix string, t *Threshold) {
				if t == nil {
					return
				}
				form[prefix+"_threshold_type"] = t.Type
				if strings.Contains(t.Type, ">") {
					form[prefix+"_upper_limit"] = t.Value
				} else {
					form[prefix+"_lower_limit"] = t.Value
				}
			}
			put("warning", p.Warn)
			put("critical", p.Crit)
			if _, err := s.call(ctx, "asset/updatethreshold", form); err != nil {
				return err
			}
		}
		// SPC on the supply air temperature of the north MAUs.
		if m.Key == "mau" && strings.Compare(e.Alias, "MAU_03") <= 0 {
			epid := idOf(ctx, s.db, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
				WHERE ep.equipment_id = $1 AND mp.alias = 'sat'`, id)
			if _, err := s.call(ctx, "asset/updatespcconfig", map[string]any{"parameter_id": epid, "usl": 18.5, "lsl": 15.5,
				"ucl": 17.36, "lcl": 16.64, "x_bar_bar": 17.0, "std_dev": 0.12}); err != nil {
				return err
			}
		}
		aspects := func(list [][2]string) string {
			out := []map[string]string{}
			for _, a := range list {
				out = append(out, map[string]string{"aspect": a[0], "information": a[1]})
			}
			return toJSON(out)
		}
		if _, err := s.call(ctx, "asset/updateassetaspectinfo", map[string]any{"asset_id": id, "critical": aspects(e.Critical),
			"supporting": aspects(e.Supporting)}); err != nil {
			return err
		}
	}
	return nil
}

// unmappedParams returns the aliases of an equipment's parameters without a tag.
func (s *Seeder) unmappedParams(ctx context.Context, equipmentID int64) (map[string]bool, error) {
	rows, err := s.db.Query(ctx, `SELECT mp.alias FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND ep.tag_name IS NULL`, equipmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		out[alias] = true
	}
	return out, rows.Err()
}

// controls stores the control definition of every parameter of a base model:
// writable set points and modes (range, step, options, mode, target); the
// other parameters are not writable.
func (s *Seeder) controls(ctx context.Context, modelID int64, m *ModelSpec) error {
	for _, p := range m.Params {
		var min, max, step, options, mode, target any
		writable := p.Ctl != nil
		if c := p.Ctl; c != nil {
			if len(c.Options) > 0 {
				options = toJSON(c.Options)
			} else {
				min, max, step = c.Min, c.Max, c.Step
			}
			if c.Mode != "" {
				mode = c.Mode
			}
			if c.Target != "" {
				target = c.Target
			}
		}
		if _, err := s.db.Exec(ctx, `UPDATE asset.model_parameters SET is_writable = $3, control_min = $4, control_max = $5, control_step = $6,
			control_options = $7::jsonb, control_mode = $8, control_target = $9 WHERE model_id = $1 AND alias = $2`,
			modelID, p.Alias, writable, min, max, step, options, mode, target); err != nil {
			return fmt.Errorf("control definition of %s.%s: %w", m.Name, p.Alias, err)
		}
	}
	return nil
}

func (s *Seeder) modelParamIDs(ctx context.Context, modelID int64) map[string]int64 {
	out := map[string]int64{}
	rows, err := s.db.Query(ctx, `SELECT alias, id FROM asset.model_parameters WHERE model_id = $1`, modelID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		var id int64
		if rows.Scan(&a, &id) == nil {
			out[a] = id
		}
	}
	return out
}

func codeID(ctx context.Context, db *pgxpool.Pool, code string) int64 {
	return idOf(ctx, db, `SELECT id FROM dm.alarm_codes WHERE code = $1`, code)
}

func (s *Seeder) alarmMapping(ctx context.Context) error {
	for _, m := range models() {
		if m.IsBase {
			continue
		}
		mid := s.modelID(ctx, m.Name)
		params := s.modelParamIDs(ctx, mid)
		for code, aliases := range m.Mapping {
			cid := codeID(ctx, s.db, code)
			var ids []string
			for _, a := range aliases {
				ids = append(ids, fmt.Sprint(params[a]))
			}
			// saved again when the mapped parameters changed (save replaces them)
			if exists(ctx, s.db, `SELECT 1 FROM dm.alarm_mappings WHERE model_id = $1 AND alarm_code_id = $2
				HAVING array_agg(model_parameter_id::text ORDER BY model_parameter_id) = (SELECT array_agg(x ORDER BY x::bigint) FROM unnest($3::text[]) x)`,
				mid, cid, ids) {
				continue
			}
			if _, err := s.call(ctx, "alarm_mapping/save_selected_parameters", map[string]any{"model_id": mid, "alarm_code_id": cid,
				"parameter_ids": strings.Join(ids, ",")}); err != nil {
				return err
			}
		}
	}
	return nil
}

// relationFlow lays out an issue relation for the React Flow editor.
func (s *Seeder) relationFlow(ctx context.Context, m *ModelSpec, rel RelationSpec, params map[string]int64) map[string]any {
	var nodes, edges []map[string]any
	nodeIDs := map[string]string{}
	paramName := map[string]string{}
	for _, p := range m.Params {
		paramName[p.Alias] = p.Name
	}
	codeNode := func(code string) map[string]any {
		return map[string]any{"id": codeID(ctx, s.db, code), "code": code, "name": codeName(code)}
	}
	row := 0
	inputNode := func(in string) string {
		if id, ok := nodeIDs[in]; ok {
			return id
		}
		id := fmt.Sprintf("n%d", len(nodeIDs)+1)
		nodeIDs[in] = id
		var data map[string]any
		typ := "source"
		if strings.HasPrefix(in, "P:") {
			typ = "parameter"
			data = map[string]any{"id": params[in[2:]], "name": paramName[in[2:]]}
		} else {
			data = codeNode(in[2:])
		}
		nodes = append(nodes, map[string]any{"id": id, "type": typ, "position": map[string]any{"x": 0, "y": row * 120}, "data": data})
		row++
		return id
	}
	target := "t1"
	edgeN := 0
	edge := func(src, dst, typ string) {
		edgeN++
		edges = append(edges, map[string]any{"id": fmt.Sprintf("e%d", edgeN), "source": src, "target": dst, "type": typ})
	}
	compounds := 0
	for _, path := range rel.Paths {
		if len(path) == 1 {
			edge(inputNode(path[0]), target, "mapping")
			continue
		}
		compounds++
		cid := fmt.Sprintf("c%d", compounds)
		first := row
		var ins []string
		for _, in := range path {
			ins = append(ins, inputNode(in))
		}
		y := float64(first+row-1) / 2 * 120
		nodes = append(nodes, map[string]any{"id": cid, "type": "compound", "position": map[string]any{"x": 360, "y": y}, "data": map[string]any{}})
		for _, in := range ins {
			edge(in, cid, "compound")
		}
		edge(cid, target, "mapping")
	}
	nodes = append(nodes, map[string]any{"id": target, "type": "target",
		"position": map[string]any{"x": 720, "y": float64(row-1) / 2 * 120}, "data": codeNode(rel.Target)})
	return map[string]any{"nodes": nodes, "edges": edges}
}

func (s *Seeder) issueMapping(ctx context.Context) error {
	for _, m := range models() {
		if m.IsBase {
			continue
		}
		mid := s.modelID(ctx, m.Name)
		params := s.modelParamIDs(ctx, mid)
		for _, rel := range m.Relations {
			rid := idOf(ctx, s.db, `SELECT id FROM dm.issue_relations WHERE model_id = $1 AND name = $2`, mid, rel.Name)
			if rid == 0 {
				if _, err := s.call(ctx, "issue_mapping/add_relation", map[string]any{"model_id": mid, "name": rel.Name}); err != nil {
					return err
				}
				rid = idOf(ctx, s.db, `SELECT id FROM dm.issue_relations WHERE model_id = $1 AND name = $2`, mid, rel.Name)
			} else if exists(ctx, s.db, `SELECT 1 FROM dm.issue_relations WHERE id = $1 AND jsonb_array_length(flow->'nodes') > 0`, rid) {
				continue
			}
			if _, err := s.call(ctx, "issue_mapping/update_issue_relations", map[string]any{"relation_id": rid, "relation_name": rel.Name,
				"flow": toJSON(s.relationFlow(ctx, m, rel, params))}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Seeder) instructions(ctx context.Context) error {
	modelIDs := map[string][]int64{}
	for _, m := range models() {
		modelIDs[m.Key] = append(modelIDs[m.Key], s.modelID(ctx, m.Name))
	}
	for _, in := range instructions {
		cid := codeID(ctx, s.db, in.Code)
		if exists(ctx, s.db, `SELECT 1 FROM dm.instructions WHERE alarm_code_id = $1 AND title = $2 AND NOT is_deleted`, cid, in.Title) {
			continue
		}
		var ids []string
		for _, k := range instructionModels(in.Code) {
			for _, id := range modelIDs[k] {
				ids = append(ids, fmt.Sprint(id))
			}
		}
		// Written by the reliability engineer.
		if _, err := s.c.Call(ctx, "kevin_ong", "maintenance_instruction/create_instruction", map[string]any{"alarm_code_id": cid, "title": in.Title,
			"instruction_type_id": in.Type, "duration": in.Minutes, "description": in.Desc, "content": in.Content,
			"model_ids": "{" + strings.Join(ids, ",") + "}"}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Seeder) users(ctx context.Context) error {
	for _, g := range userGroups {
		if !exists(ctx, s.db, `SELECT 1 FROM core.user_groups WHERE lower(alias) = lower($1)`, g.Alias) {
			var locs []map[string]any
			for _, b := range g.Buildings {
				locs = append(locs, map[string]any{"id": s.locationID(ctx, b)})
			}
			if _, err := s.call(ctx, "user_management/addusergroup", map[string]any{"alias": g.Alias, "name": g.Name,
				"description": g.Description, "locations": toJSON(locs)}); err != nil {
				return err
			}
		}
		var perms []map[string]any
		for grp, aliases := range g.Perms {
			for _, a := range aliases {
				perms = append(perms, map[string]any{"permission_group_alias": grp, "permission_alias": a, "value": true})
			}
		}
		if _, err := s.call(ctx, "user_management/addusergrouppermission", map[string]any{"alias": g.Alias, "permissions": toJSON(perms)}); err != nil {
			return err
		}
	}
	for _, u := range users {
		if !exists(ctx, s.db, `SELECT 1 FROM core.users WHERE lower(username) = lower($1) AND NOT is_deleted`, u.Username) {
			if _, err := s.call(ctx, "user_management/adduser", map[string]any{"username": u.Username, "name": u.Name, "email": u.Email,
				"user_id": u.EmployeeID, "password": s.password, "user_group_alias": u.Group}); err != nil {
				return err
			}
		}
		if u.Role == "admin" {
			if _, err := s.db.Exec(ctx, `UPDATE core.users SET role = 'admin' WHERE lower(username) = lower($1)`, u.Username); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Seeder) personnel(ctx context.Context) error {
	for _, u := range users {
		if u.Role != "technician" {
			continue
		}
		var b, t []string
		for _, name := range u.Buildings {
			b = append(b, fmt.Sprint(s.locationID(ctx, name)))
		}
		for _, name := range u.Types {
			t = append(t, fmt.Sprint(idOf(ctx, s.db, `SELECT id FROM asset.types WHERE name = $1`, name)))
		}
		if _, err := s.call(ctx, "personnel_management/update_personnel_details", map[string]any{"id": u.Username,
			"assignedBuildingIds": toJSON(b), "assignedEquipmentTypeIds": toJSON(t)}); err != nil {
			return err
		}
	}
	return nil
}
