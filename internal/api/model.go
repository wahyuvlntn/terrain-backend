package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/script"
)

func (s *Server) registerModel() {
	p := "model_manager/"
	s.handle(p+"get_model_types", AccessUser, s.listTable(`SELECT id, name FROM asset.types ORDER BY order_id, lower(name)`))
	s.handle(p+"get_model_brands", AccessUser, s.listTable(`SELECT id, name FROM asset.brands ORDER BY order_id, lower(name)`))
	s.handle(p+"get_model_series", AccessUser, s.listTable(`SELECT id, name FROM asset.series ORDER BY order_id, lower(name)`))
	s.handle(p+"get_feedback_categories", AccessUser, s.listTable(`SELECT id, name FROM asset.feedback_categories ORDER BY order_id, lower(name)`))
	s.handle(p+"get_feedback_issues", AccessUser, s.mmFeedbackIssues)
	s.handle(p+"get_base_models", AccessSuper, s.mmBaseModels)
	s.handle(p+"get_model_list", AccessUser, s.mmList) // also used by Equipment Overview
	s.handle(p+"get_model_info", AccessSuper, s.mmInfo)
	s.handle(p+"get_model_parameters", AccessSuper, s.mmParameters)
	s.handle(p+"get_model_equipment_connection", AccessSuper, s.mmEquipmentConnection)
	s.handle(p+"get_model_status_definitions", AccessSuper, s.mmStatusDefinitions)
	s.handle(p+"get_model_alarms", AccessSuper, s.mmAlarms)
	s.handle(p+"get_model_dynamic_alarms", AccessSuper, s.mmDynamicAlarms)
	s.handle(p+"get_running_hour_limit", AccessUser, s.mmGetRunningHourLimit)
	s.handle(p+"add_model", AccessSuper, s.mmAdd)
	s.handle(p+"delete_model", AccessSuper, s.mmDelete)
	s.handle(p+"update_model_info", AccessSuper, s.mmUpdateInfo)
	s.handle(p+"add_model_parameter", AccessSuper, s.mmUpsertParameter(true))
	s.handle(p+"update_model_parameter", AccessSuper, s.mmUpsertParameter(false))
	s.handle(p+"delete_model_parameter", AccessSuper, s.mmDeleteParameter)
	s.handle(p+"upsert_model_status_definitions", AccessSuper, s.mmUpsertStatus)
	s.handle(p+"add_model_alarm", AccessSuper, s.mmAddAlarm)
	s.handle(p+"update_model_alarm", AccessSuper, s.mmUpdateAlarm)
	s.handle(p+"delete_model_alarm", AccessSuper, s.mmDeleteAlarm("asset.model_alarms"))
	s.handle(p+"restore_model_alarm", AccessSuper, s.mmRestoreAlarm("asset.model_alarms"))
	s.handle(p+"import_model_alarms", AccessSuper, s.mmImportAlarms)
	s.handle(p+"add_model_dynamic_alarm", AccessSuper, s.mmAddDynamicAlarm)
	s.handle(p+"update_model_dynamic_alarm", AccessSuper, s.mmUpdateDynamicAlarm)
	s.handle(p+"delete_model_dynamic_alarm", AccessSuper, s.mmDeleteAlarm("asset.model_dynamic_alarms"))
	s.handle(p+"restore_model_dynamic_alarm", AccessSuper, s.mmRestoreAlarm("asset.model_dynamic_alarms"))
	s.handle(p+"import_model_dynamic_alarms", AccessSuper, s.mmImportDynamicAlarms)
	s.handle(p+"import_model_parameters", AccessSuper, s.mmImportParameters)
	s.handle(p+"test_status_and_alarms", AccessSuper, s.mmTest)
	s.handle(p+"sync_base_model_parameters", AccessSuper, s.mmSyncBase)
	s.handle(p+"update_running_hour_limit", AccessUser, s.mmUpdateRunningHourLimit)
}

type modelRow struct {
	ID          int64
	Name        string
	Description string
	TypeID      int64
	BrandID     int64
	SeriesID    int64
	BaseID      *int64
}

func (s *Server) loadModelRow(ctx context.Context, q db.Querier, id int64) (*modelRow, error) {
	m := &modelRow{}
	err := q.QueryRow(ctx, `SELECT id, name, description, type_id, brand_id, series_id, base_model_id FROM asset.models WHERE id = $1`, id).
		Scan(&m.ID, &m.Name, &m.Description, &m.TypeID, &m.BrandID, &m.SeriesID, &m.BaseID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_model_not_found")
		}
		return nil, err
	}
	return m, nil
}

func (s *Server) mmFeedbackIssues(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT id, name FROM asset.feedback_issues WHERE category_id = $1 ORDER BY order_id, lower(name)`, c.F.Int("category_id", 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name})
	}
	return out, rows.Err()
}

func (s *Server) mmBaseModels(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT id, name, type_id, brand_id, series_id FROM asset.models WHERE base_model_id IS NULL ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, t, b, sr int64
		var name string
		if err := rows.Scan(&id, &name, &t, &b, &sr); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "type_id": t, "brand_id": b, "series_id": sr})
	}
	return out, rows.Err()
}

func (s *Server) mmList(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT m.id, m.name, b.name, sr.name, bm.name, (SELECT count(*) FROM asset.equipment e WHERE e.model_id = m.id)
		FROM asset.models m JOIN asset.brands b ON b.id = m.brand_id JOIN asset.series sr ON sr.id = m.series_id
		LEFT JOIN asset.models bm ON bm.id = m.base_model_id ORDER BY lower(m.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []map[string]any{}
	for rows.Next() {
		var id, count int64
		var name, brand, series string
		var base *string
		if err := rows.Scan(&id, &name, &brand, &series, &base, &count); err != nil {
			return nil, err
		}
		entries = append(entries, map[string]any{"id": id, "name": name, "brand": brand, "series": series, "base_model": base, "equipment_counts": count})
	}
	return map[string]any{"total": len(entries), "entries": entries}, rows.Err()
}

func (s *Server) mmInfo(c *Ctx) (any, error) {
	m, err := s.loadModelRow(c, s.DB, c.F.Int("model_id", 0))
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": m.ID, "name": m.Name, "description": m.Description, "type_id": m.TypeID, "brand_id": m.BrandID,
		"series_id": m.SeriesID, "base_model_id": m.BaseID}, nil
}

func (s *Server) mmParameters(c *Ctx) (any, error) {
	id := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT alias, name, unit, data_type, scale, gain_id, parameter_type_id FROM asset.model_parameters
		WHERE model_id = $1 ORDER BY lower(name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var alias, name, unit, dt string
		var scale, gain, ptype *int32
		if err := rows.Scan(&alias, &name, &unit, &dt, &scale, &gain, &ptype); err != nil {
			return nil, err
		}
		row := map[string]any{"alias": alias, "name": name, "unit": unit, "data_type": dt, "scale": scale}
		if gain != nil && *gain > 0 {
			row["gain_id"] = *gain
		}
		if ptype != nil && *ptype > 0 {
			row["model_parameter_type_id"] = *ptype
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Server) mmEquipmentConnection(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT e.alias, e.name, l.type, l.expired_at, l.id IS NOT NULL,
			(SELECT count(*) FROM asset.equipment_parameters ep WHERE ep.equipment_id = e.id AND ep.tag_name IS NOT NULL)
		FROM asset.equipment e LEFT JOIN core.licenses l ON l.id = e.license_id WHERE e.model_id = $1 ORDER BY lower(e.name)`, c.F.Int("model_id", 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var alias, name string
		var ltype *string
		var exp *time.Time
		var hasLic bool
		var mapped int64
		if err := rows.Scan(&alias, &name, &ltype, &exp, &hasLic, &mapped); err != nil {
			return nil, err
		}
		var active, status any
		if hasLic {
			a, st := licenseStatus(exp)
			active, status = a, st
		}
		out = append(out, map[string]any{"alias": alias, "name": name, "license_type": ltype, "license_is_active": active,
			"license_status": status, "mapped_params": mapped})
	}
	return out, rows.Err()
}

func (s *Server) mmStatusDefinitions(c *Ctx) (any, error) {
	id := c.F.Int("model_id", 0)
	m, err := s.loadModelRow(c, s.DB, id)
	if err != nil {
		return nil, err
	}
	var u, p, r *string
	if err := s.DB.QueryRow(c, `SELECT unplanned_down_script, planned_down_script, running_script FROM asset.models WHERE id = $1`, id).Scan(&u, &p, &r); err != nil {
		return nil, err
	}
	out := map[string]any{"is_extended": m.BaseID != nil, "unplanned_down_script": u, "planned_down_script": p, "running_script": r,
		"base_unplanned_down_script": nil, "base_planned_down_script": nil, "base_running_script": nil}
	if m.BaseID != nil {
		base, err := engine.LoadModel(c, s.DB, *m.BaseID)
		if err == nil {
			out["base_unplanned_down_script"] = nilIfEmpty(base.UnplannedScript)
			out["base_planned_down_script"] = nilIfEmpty(base.PlannedScript)
			out["base_running_script"] = nilIfEmpty(base.RunningScript)
		}
	}
	return out, nil
}

func (s *Server) mmAlarms(c *Ctx) (any, error) {
	id := c.F.Int("model_id", 0)
	rows, err := s.DB.Query(c, `SELECT a.id, a.name, a.criticality, COALESCE(a.issue_category_id, 1), COALESCE(a.issue_id, 1), a.message, a.script,
			a.is_disabled, b.script
		FROM asset.model_alarms a LEFT JOIN asset.model_alarms b ON b.id = a.base_alarm_id WHERE a.model_id = $1 ORDER BY lower(a.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var aid, cat, issue int64
		var crit int
		var name, msg string
		var sc, base *string
		var disabled bool
		if err := rows.Scan(&aid, &name, &crit, &cat, &issue, &msg, &sc, &disabled, &base); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"model_id": id, "alarm_id": aid, "name": name, "criticality": crit, "issue_category_id": cat,
			"issue_id": issue, "message": msg, "script": sc, "is_disabled": disabled, "base_script": base})
	}
	return out, rows.Err()
}

func (s *Server) mmDynamicAlarms(c *Ctx) (any, error) {
	id := c.F.Int("model_id", 0)
	rows, err := s.DB.Query(c, `SELECT a.id, a.name, a.script, a.is_disabled, b.script
		FROM asset.model_dynamic_alarms a LEFT JOIN asset.model_dynamic_alarms b ON b.id = a.base_alarm_id WHERE a.model_id = $1 ORDER BY lower(a.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var aid int64
		var name string
		var sc, base *string
		var disabled bool
		if err := rows.Scan(&aid, &name, &sc, &disabled, &base); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"model_id": id, "alarm_id": aid, "name": name, "script": sc, "is_disabled": disabled, "base_script": base})
	}
	return out, rows.Err()
}

func (s *Server) mmGetRunningHourLimit(c *Ctx) (any, error) {
	var w, cr int
	var nw, nc, auto bool
	err := s.DB.QueryRow(c, `SELECT running_hour_warning, running_hour_critical, running_hour_notify_warn, running_hour_notify_crit, running_hour_auto_maint
		FROM asset.models WHERE id = $1`, c.F.Int("model_id", 0)).Scan(&w, &cr, &nw, &nc, &auto)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_model_not_found")
		}
		return nil, err
	}
	return map[string]any{"warning_hour": w, "critical_hour": cr, "is_notify_warning": nw, "is_notify_critical": nc, "is_auto_create_maintenance": auto}, nil
}

func (s *Server) validateMasterRefs(c *Ctx, q db.Querier, typeID, brandID, seriesID int64) error {
	for _, chk := range []struct {
		table string
		id    int64
		key   string
	}{{"asset.types", typeID, "err_invalid_type"}, {"asset.brands", brandID, "err_invalid_brand"}, {"asset.series", seriesID, "err_invalid_series"}} {
		var ok bool
		if err := q.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM `+chk.table+` WHERE id = $1)`, chk.id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return httpx.BadRequest(chk.key)
		}
	}
	return nil
}

func (s *Server) mmAdd(c *Ctx) (any, error) {
	name := c.F.StrTrim("name")
	if name == "" {
		return nil, httpx.BadRequest("err_invalid_model_name")
	}
	typeID, brandID, seriesID := c.F.Int("type_id", 0), c.F.Int("brand_id", 0), c.F.Int("series_id", 0)
	base := c.F.IntPtr("base_model_id")
	if base != nil && *base <= 0 {
		base = nil
	}
	var id int64
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := s.validateMasterRefs(c, tx, typeID, brandID, seriesID); err != nil {
			return err
		}
		if base != nil {
			b, err := s.loadModelRow(c, tx, *base)
			if err != nil {
				return httpx.BadRequest("err_invalid_base_model")
			}
			if b.BaseID != nil {
				return httpx.BadRequest("err_invalid_base_model")
			}
		}
		if err := tx.QueryRow(c, `INSERT INTO asset.models (name, description, type_id, brand_id, series_id, base_model_id)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, name, c.F.Str("description"), typeID, brandID, seriesID, base).Scan(&id); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_model_with_the_same_name_already_exists")
			}
			return err
		}
		if base != nil {
			if _, err := tx.Exec(c, `INSERT INTO asset.model_parameters (model_id, alias, name, unit, data_type, scale, gain_id, parameter_type_id, base_parameter_id)
				SELECT $1, alias, name, unit, data_type, scale, gain_id, parameter_type_id, id FROM asset.model_parameters WHERE model_id = $2`, id, *base); err != nil {
				return err
			}
			if _, err := tx.Exec(c, `INSERT INTO asset.model_alarms (model_id, base_alarm_id, name, criticality, issue_category_id, issue_id, message, script)
				SELECT $1, id, name, criticality, issue_category_id, issue_id, message, NULL FROM asset.model_alarms WHERE model_id = $2`, id, *base); err != nil {
				return err
			}
			if _, err := tx.Exec(c, `INSERT INTO asset.model_dynamic_alarms (model_id, base_alarm_id, name, script)
				SELECT $1, id, name, NULL FROM asset.model_dynamic_alarms WHERE model_id = $2`, id, *base); err != nil {
				return err
			}
			if _, err := tx.Exec(c, `UPDATE asset.models m SET running_hour_warning = b.running_hour_warning, running_hour_critical = b.running_hour_critical,
				running_hour_notify_warn = b.running_hour_notify_warn, running_hour_notify_crit = b.running_hour_notify_crit,
				running_hour_auto_maint = b.running_hour_auto_maint FROM asset.models b WHERE m.id = $1 AND b.id = $2`, id, *base); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (s *Server) mmDelete(c *Ctx) (any, error) {
	id := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, id); err != nil {
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment WHERE model_id = $1`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_an_equipment_is_still_connected_to_this_model")
	}
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.models WHERE base_model_id = $1`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_another_model_is_still_using_this_model_as_its_base")
	}
	if _, err := s.DB.Exec(c, `DELETE FROM asset.models WHERE id = $1`, id); err != nil {
		return nil, fail(err)
	}
	return id, nil
}

func (s *Server) mmUpdateInfo(c *Ctx) (any, error) {
	id := c.F.Int("id", 0)
	if _, err := s.loadModelRow(c, s.DB, id); err != nil {
		return nil, err
	}
	name := c.F.StrTrim("name")
	if name == "" {
		return nil, httpx.BadRequest("err_invalid_model_name")
	}
	typeID, brandID, seriesID := c.F.Int("type_id", 0), c.F.Int("brand_id", 0), c.F.Int("series_id", 0)
	if err := s.validateMasterRefs(c, s.DB, typeID, brandID, seriesID); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `UPDATE asset.models SET name = $2, description = $3, type_id = $4, brand_id = $5, series_id = $6, updated_at = now() WHERE id = $1`,
		id, name, c.F.Str("description"), typeID, brandID, seriesID); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("err_model_with_the_same_name_already_exists")
		}
		return nil, err
	}
	s.Engine.Invalidate()
	return "Model successfully updated", nil
}

type paramInput struct {
	alias, name, unit, dataType string
	scale, gain, ptype          *int64
}

func validateParam(p *paramInput) error {
	if !script.ValidIdentifier(p.alias) {
		return httpx.BadRequest("err_variable_name_is_empty")
	}
	if strings.TrimSpace(p.name) == "" {
		return httpx.BadRequest(httpx.Key("err_variable_display_name_is_empty", p.alias))
	}
	switch p.dataType {
	case "string", "number", "boolean":
	default:
		return httpx.BadRequest("err_invalid_data_type")
	}
	if p.dataType != "number" {
		p.scale = nil
	}
	if p.scale != nil && (*p.scale < 0 || *p.scale > 10) {
		return httpx.BadRequest("err_invalid_scale")
	}
	return nil
}

// upsertParameter inserts or updates a parameter of a model and creates the
// matching equipment parameter rows.
func upsertParameter(ctx context.Context, tx pgx.Tx, modelID int64, p *paramInput, create bool) error {
	var dupName bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM asset.model_parameters WHERE model_id = $1 AND lower(name) = lower($2) AND alias <> $3)`,
		modelID, p.name, p.alias).Scan(&dupName); err != nil {
		return err
	}
	if dupName {
		return httpx.BadRequest("err_variable_with_the_same_display_name_already_exists")
	}
	if create {
		var pid int64
		err := tx.QueryRow(ctx, `INSERT INTO asset.model_parameters (model_id, alias, name, unit, data_type, scale, gain_id, parameter_type_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, modelID, p.alias, p.name, p.unit, p.dataType, p.scale, p.gain, p.ptype).Scan(&pid)
		if err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_variable_with_the_same_name_already_exists")
			}
			return err
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE asset.model_parameters SET name = $3, unit = $4, data_type = $5, scale = $6, gain_id = $7,
			parameter_type_id = $8, updated_at = now() WHERE model_id = $1 AND alias = $2`, modelID, p.alias, p.name, p.unit, p.dataType, p.scale, p.gain, p.ptype)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.BadRequest("err_variable_not_found")
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO asset.equipment_parameters (equipment_id, model_parameter_id)
		SELECT e.id, mp.id FROM asset.equipment e JOIN asset.model_parameters mp ON mp.model_id = e.model_id
		WHERE e.model_id = $1 AND mp.alias = $2 ON CONFLICT DO NOTHING`, modelID, p.alias)
	return err
}

func (s *Server) mmUpsertParameter(create bool) HandlerFunc {
	return func(c *Ctx) (any, error) {
		modelID := c.F.Int("model_id", 0)
		if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
			return nil, err
		}
		p := &paramInput{alias: c.F.StrTrim("parameter_alias"), name: c.F.StrTrim("parameter_name"), unit: c.F.Str("parameter_unit"),
			dataType: strings.ToLower(c.F.StrTrim("parameter_data_type")), scale: c.F.IntPtr("parameter_scale"),
			gain: c.F.IntPtr("parameter_gain_id"), ptype: c.F.IntPtr("parameter_type_id")}
		if err := validateParam(p); err != nil {
			return nil, err
		}
		if err := s.DB.Tx(c, func(tx pgx.Tx) error { return upsertParameter(c, tx, modelID, p, create) }); err != nil {
			return nil, err
		}
		s.Engine.Invalidate()
		if create {
			return httpx.Key("variable_successfully_added", p.name), nil
		}
		return httpx.Key("variable_successfully_updated", p.name), nil
	}
}

func (s *Server) mmDeleteParameter(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	alias := c.F.StrTrim("parameter_alias")
	var pid int64
	var name string
	if err := s.DB.QueryRow(c, `SELECT id, name FROM asset.model_parameters WHERE model_id = $1 AND alias = $2`, modelID, alias).Scan(&pid, &name); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.BadRequest("err_variable_not_found")
		}
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment_parameters WHERE model_parameter_id = $1 AND tag_name IS NOT NULL`, pid).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_there_is_an_equipment_parameter_still_connected_to_this_model_variable")
	}
	if _, err := s.DB.Exec(c, `DELETE FROM asset.model_parameters WHERE id = $1`, pid); err != nil {
		return nil, fail(err)
	}
	s.Engine.Invalidate()
	return httpx.Key("variable_successfully_deleted", name), nil
}

func checkScript(label string, src *string) error {
	if src == nil {
		return nil
	}
	if err := script.Check(*src); err != nil {
		return httpx.BadRequest(fmt.Sprintf("%s script has a syntax error - %s", label, strings.ReplaceAll(err.Error(), ":", " -")))
	}
	return nil
}

func (s *Server) mmUpsertStatus(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	u, p, r := c.F.NonEmptyPtr("unplanned_down_script"), c.F.NonEmptyPtr("planned_down_script"), c.F.NonEmptyPtr("running_script")
	for label, sc := range map[string]*string{"Unplanned down": u, "Planned down": p, "Running": r} {
		if err := checkScript(label, sc); err != nil {
			return nil, err
		}
	}
	if _, err := s.DB.Exec(c, `UPDATE asset.models SET unplanned_down_script = $2, planned_down_script = $3, running_script = $4, updated_at = now() WHERE id = $1`,
		modelID, u, p, r); err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return "model_status_successfully_updated", nil
}

func (s *Server) issueCategory(c *Ctx, q db.Querier, issueID int64) (int64, error) {
	var cat int64
	if err := q.QueryRow(c, `SELECT category_id FROM asset.feedback_issues WHERE id = $1`, issueID).Scan(&cat); err != nil {
		if db.IsNoRows(err) {
			return 0, httpx.BadRequest("err_invalid_issue")
		}
		return 0, err
	}
	return cat, nil
}

func (s *Server) mmAddAlarm(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	name := c.F.StrTrim("name")
	crit := c.F.Int("criticality", 1)
	issue := c.F.Int("issue_id", 1)
	src := c.F.NonEmptyPtr("script")
	if name == "" {
		return nil, httpx.BadRequest("err_alarm_name_is_empty")
	}
	if crit != 1 && crit != 2 {
		return nil, httpx.BadRequest("err_invalid_criticality")
	}
	if src == nil {
		return nil, httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", name))
	}
	if err := checkScript("Alarm", src); err != nil {
		return nil, err
	}
	var id int64
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		cat, err := s.issueCategory(c, tx, issue)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(c, `INSERT INTO asset.model_alarms (model_id, name, criticality, issue_category_id, issue_id, message, script)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, modelID, name, crit, cat, issue, c.F.Str("message"), *src).Scan(&id); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
			return err
		}
		// Extended models inherit the new alarm.
		_, err = tx.Exec(c, `INSERT INTO asset.model_alarms (model_id, base_alarm_id, name, criticality, issue_category_id, issue_id, message, script)
			SELECT m.id, $2, $3, $4, $5, $6, $7, NULL FROM asset.models m WHERE m.base_model_id = $1
			ON CONFLICT DO NOTHING`, modelID, id, name, crit, cat, issue, c.F.Str("message"))
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return id, nil
}

func (s *Server) mmUpdateAlarm(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	alarmID := c.F.Int("alarm_id", 0)
	name := c.F.StrTrim("name")
	crit := c.F.Int("criticality", 1)
	issue := c.F.Int("issue_id", 1)
	src := c.F.NonEmptyPtr("script")
	if name == "" {
		return nil, httpx.BadRequest("err_alarm_name_is_empty")
	}
	if crit != 1 && crit != 2 {
		return nil, httpx.BadRequest("err_invalid_criticality")
	}
	if err := checkScript("Alarm", src); err != nil {
		return nil, err
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		var base *int64
		if err := tx.QueryRow(c, `SELECT base_alarm_id FROM asset.model_alarms WHERE id = $1 AND model_id = $2`, alarmID, modelID).Scan(&base); err != nil {
			if db.IsNoRows(err) {
				return httpx.BadRequest("err_alarm_doesnt_belong_to_this_model")
			}
			return err
		}
		if base == nil && src == nil {
			return httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", name))
		}
		cat, err := s.issueCategory(c, tx, issue)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(c, `UPDATE asset.model_alarms SET name = $2, criticality = $3, issue_category_id = $4, issue_id = $5, message = $6,
			script = $7, updated_at = now() WHERE id = $1`, alarmID, name, crit, cat, issue, c.F.Str("message"), src); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
			return err
		}
		if base == nil {
			_, err = tx.Exec(c, `UPDATE asset.model_alarms SET name = $2, criticality = $3, issue_category_id = $4, issue_id = $5, message = $6,
				updated_at = now() WHERE base_alarm_id = $1`, alarmID, name, crit, cat, issue, c.F.Str("message"))
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("alarm_successfully_updated", name), nil
}

func (s *Server) mmDeleteAlarm(table string) HandlerFunc {
	return func(c *Ctx) (any, error) {
		modelID := c.F.Int("model_id", 0)
		alarmID := c.F.Int("alarm_id", 0)
		var base *int64
		var name string
		if err := s.DB.QueryRow(c, `SELECT base_alarm_id, name FROM `+table+` WHERE id = $1 AND model_id = $2`, alarmID, modelID).Scan(&base, &name); err != nil {
			if db.IsNoRows(err) {
				return nil, httpx.BadRequest("err_alarm_not_found")
			}
			return nil, err
		}
		var err error
		if base != nil {
			_, err = s.DB.Exec(c, `UPDATE `+table+` SET is_disabled = TRUE, updated_at = now() WHERE id = $1`, alarmID)
		} else {
			_, err = s.DB.Exec(c, `DELETE FROM `+table+` WHERE id = $1`, alarmID)
		}
		if err != nil {
			return nil, err
		}
		s.Engine.Invalidate()
		if table == "asset.model_dynamic_alarms" {
			return httpx.Key("dynamic_alarm_successfully_deleted", name), nil
		}
		return httpx.Key("alarm_successfully_deleted", name), nil
	}
}

func (s *Server) mmRestoreAlarm(table string) HandlerFunc {
	return func(c *Ctx) (any, error) {
		modelID := c.F.Int("model_id", 0)
		alarmID := c.F.Int("alarm_id", 0)
		var base *int64
		var name string
		if err := s.DB.QueryRow(c, `SELECT base_alarm_id, name FROM `+table+` WHERE id = $1 AND model_id = $2`, alarmID, modelID).Scan(&base, &name); err != nil {
			if db.IsNoRows(err) {
				return nil, httpx.BadRequest("err_alarm_not_found")
			}
			return nil, err
		}
		if base == nil {
			if table == "asset.model_dynamic_alarms" {
				return nil, httpx.BadRequest("err_only_extended_dynamic_alarm_restored")
			}
			return nil, httpx.BadRequest("err_only_extended_alarm_restored")
		}
		if _, err := s.DB.Exec(c, `UPDATE `+table+` SET is_disabled = FALSE, updated_at = now() WHERE id = $1`, alarmID); err != nil {
			return nil, err
		}
		s.Engine.Invalidate()
		if table == "asset.model_dynamic_alarms" {
			return httpx.Key("dynamic_alarm_enabled_successfully", name), nil
		}
		return httpx.Key("alarm_enabled_successfully", name), nil
	}
}

func (s *Server) mmImportAlarms(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	var alarms []struct {
		Name        string `json:"name"`
		Criticality string `json:"criticality"`
		Message     string `json:"message"`
		Script      string `json:"script"`
	}
	if _, err := c.F.JSON("alarms", &alarms); err != nil || len(alarms) == 0 {
		return nil, httpx.BadRequest("err_alarms_must_be_an_array")
	}
	for _, a := range alarms {
		if strings.TrimSpace(a.Name) == "" {
			return nil, httpx.BadRequest("err_alarm_name_is_empty")
		}
		if strings.TrimSpace(a.Script) == "" {
			return nil, httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", a.Name))
		}
		c := strings.ToLower(strings.TrimSpace(a.Criticality))
		if c != "warning" && c != "critical" {
			return nil, httpx.BadRequest(httpx.Key("err_alarm_criticality_must_be_either_warning_or_critical", a.Name))
		}
		sc := a.Script
		if err := checkScript("Alarm "+a.Name, &sc); err != nil {
			return nil, err
		}
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		for _, a := range alarms {
			crit := 1
			if strings.EqualFold(strings.TrimSpace(a.Criticality), "critical") {
				crit = 2
			}
			name := strings.TrimSpace(a.Name)
			var id int64
			err := tx.QueryRow(c, `SELECT id FROM asset.model_alarms WHERE model_id = $1 AND lower(name) = lower($2)`, modelID, name).Scan(&id)
			switch {
			case err == nil:
				if _, err := tx.Exec(c, `UPDATE asset.model_alarms SET criticality = $2, message = $3, script = $4, updated_at = now() WHERE id = $1`,
					id, crit, a.Message, a.Script); err != nil {
					return err
				}
			case db.IsNoRows(err):
				if err := tx.QueryRow(c, `INSERT INTO asset.model_alarms (model_id, name, criticality, issue_category_id, issue_id, message, script)
					VALUES ($1,$2,$3,1,1,$4,$5) RETURNING id`, modelID, name, crit, a.Message, a.Script).Scan(&id); err != nil {
					return err
				}
				if _, err := tx.Exec(c, `INSERT INTO asset.model_alarms (model_id, base_alarm_id, name, criticality, issue_category_id, issue_id, message, script)
					SELECT m.id, $2, $3, $4, 1, 1, $5, NULL FROM asset.models m WHERE m.base_model_id = $1 ON CONFLICT DO NOTHING`,
					modelID, id, name, crit, a.Message); err != nil {
					return err
				}
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("alarms_successfully_imported", len(alarms)), nil
}

func (s *Server) mmAddDynamicAlarm(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	name := c.F.StrTrim("name")
	src := c.F.NonEmptyPtr("script")
	if name == "" {
		return nil, httpx.BadRequest("err_alarm_name_is_empty")
	}
	if src == nil {
		return nil, httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", name))
	}
	if err := checkScript("Dynamic alarm", src); err != nil {
		return nil, err
	}
	var id int64
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		if err := tx.QueryRow(c, `INSERT INTO asset.model_dynamic_alarms (model_id, name, script) VALUES ($1,$2,$3) RETURNING id`, modelID, name, *src).Scan(&id); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
			return err
		}
		_, err := tx.Exec(c, `INSERT INTO asset.model_dynamic_alarms (model_id, base_alarm_id, name, script)
			SELECT m.id, $2, $3, NULL FROM asset.models m WHERE m.base_model_id = $1 ON CONFLICT DO NOTHING`, modelID, id, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return id, nil
}

func (s *Server) mmUpdateDynamicAlarm(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	alarmID := c.F.Int("alarm_id", 0)
	name := c.F.StrTrim("name")
	src := c.F.NonEmptyPtr("script")
	if name == "" {
		return nil, httpx.BadRequest("err_alarm_name_is_empty")
	}
	if err := checkScript("Dynamic alarm", src); err != nil {
		return nil, err
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		var base *int64
		if err := tx.QueryRow(c, `SELECT base_alarm_id FROM asset.model_dynamic_alarms WHERE id = $1 AND model_id = $2`, alarmID, modelID).Scan(&base); err != nil {
			if db.IsNoRows(err) {
				return httpx.BadRequest("err_dynamic_alarm_doesnt_belong_to_this_model")
			}
			return err
		}
		if base == nil && src == nil {
			return httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", name))
		}
		if _, err := tx.Exec(c, `UPDATE asset.model_dynamic_alarms SET name = $2, script = $3, updated_at = now() WHERE id = $1`, alarmID, name, src); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
			return err
		}
		if base == nil {
			_, err := tx.Exec(c, `UPDATE asset.model_dynamic_alarms SET name = $2, updated_at = now() WHERE base_alarm_id = $1`, alarmID, name)
			if db.IsUniqueViolation(err) {
				return httpx.BadRequest("err_alarm_with_the_same_name_already_exists")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("dynamic_alarm_successfully_updated", name), nil
}

func (s *Server) mmImportDynamicAlarms(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	var alarms []struct {
		Name   string `json:"name"`
		Script string `json:"script"`
	}
	if _, err := c.F.JSON("alarms", &alarms); err != nil || len(alarms) == 0 {
		return nil, httpx.BadRequest("err_alarms_must_be_an_array")
	}
	for _, a := range alarms {
		if strings.TrimSpace(a.Name) == "" {
			return nil, httpx.BadRequest("err_alarm_name_is_empty")
		}
		sc := a.Script
		if strings.TrimSpace(sc) == "" {
			return nil, httpx.BadRequest(httpx.Key("err_alarm_script_is_empty", a.Name))
		}
		if err := checkScript("Dynamic alarm "+a.Name, &sc); err != nil {
			return nil, err
		}
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		for _, a := range alarms {
			name := strings.TrimSpace(a.Name)
			var id int64
			err := tx.QueryRow(c, `SELECT id FROM asset.model_dynamic_alarms WHERE model_id = $1 AND lower(name) = lower($2)`, modelID, name).Scan(&id)
			switch {
			case err == nil:
				if _, err := tx.Exec(c, `UPDATE asset.model_dynamic_alarms SET script = $2, updated_at = now() WHERE id = $1`, id, a.Script); err != nil {
					return err
				}
			case db.IsNoRows(err):
				if err := tx.QueryRow(c, `INSERT INTO asset.model_dynamic_alarms (model_id, name, script) VALUES ($1,$2,$3) RETURNING id`, modelID, name, a.Script).Scan(&id); err != nil {
					return err
				}
				if _, err := tx.Exec(c, `INSERT INTO asset.model_dynamic_alarms (model_id, base_alarm_id, name, script)
					SELECT m.id, $2, $3, NULL FROM asset.models m WHERE m.base_model_id = $1 ON CONFLICT DO NOTHING`, modelID, id, name); err != nil {
					return err
				}
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("dynamic_alarms_successfully_imported", len(alarms)), nil
}

func (s *Server) mmImportParameters(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	var params []struct {
		VariableName string `json:"variable_name"`
		DisplayName  string `json:"display_name"`
		Unit         string `json:"unit"`
		DataType     string `json:"data_type"`
		Scale        *int64 `json:"scale"`
	}
	if _, err := c.F.JSON("parameters", &params); err != nil || len(params) == 0 {
		return nil, httpx.BadRequest("err_variables_must_be_an_array")
	}
	inputs := make([]*paramInput, 0, len(params))
	seen := map[string]bool{}
	for _, p := range params {
		in := &paramInput{alias: strings.TrimSpace(p.VariableName), name: strings.TrimSpace(p.DisplayName), unit: p.Unit,
			dataType: strings.ToLower(strings.TrimSpace(p.DataType)), scale: p.Scale}
		if err := validateParam(in); err != nil {
			return nil, err
		}
		if seen[in.alias] {
			return nil, httpx.BadRequest("err_variable_with_the_same_name_already_exists")
		}
		seen[in.alias] = true
		inputs = append(inputs, in)
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		for _, in := range inputs {
			var exists bool
			if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.model_parameters WHERE model_id = $1 AND alias = $2)`, modelID, in.alias).Scan(&exists); err != nil {
				return err
			}
			if err := upsertParameter(c, tx, modelID, in, !exists); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("variables_successfully_imported", len(inputs)), nil
}

func (s *Server) mmTest(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	if _, err := s.loadModelRow(c, s.DB, modelID); err != nil {
		return nil, err
	}
	raw := map[string]struct {
		V any   `json:"v"`
		T int64 `json:"t"`
		Q bool  `json:"q"`
	}{}
	if _, err := c.F.JSON("values", &raw); err != nil {
		return nil, httpx.BadRequest("Invalid values")
	}
	params, err := s.DB.Query(c, `SELECT alias, data_type FROM asset.model_parameters WHERE model_id = $1`, modelID)
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	for params.Next() {
		var a, t string
		if err := params.Scan(&a, &t); err != nil {
			params.Close()
			return nil, err
		}
		types[a] = t
	}
	params.Close()
	values := map[string]engine.Value{}
	for alias, v := range raw {
		str := fmt.Sprint(v.V)
		if v.V == nil {
			str = ""
		}
		val := engine.ToValue(str, v.T, v.Q, types[alias], nil)
		val.Q = v.Q
		values[alias] = val
	}
	res, m, err := s.Engine.TestModel(c, modelID, values)
	if err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		return nil, httpx.BadRequest(strings.ReplaceAll(strings.Join(res.Errors, "; "), ":", " -"))
	}
	alarms := []map[string]any{}
	dynamic := []map[string]any{}
	crit := func(s string) int {
		if s == "CRITICAL" {
			return 2
		}
		return 1
	}
	for _, a := range res.Alarms {
		if a.DynName != "" {
			dynamic = append(dynamic, map[string]any{"name": a.DynName, "alarm_name": a.Name, "criticality": crit(a.Severity), "message": a.Message})
			continue
		}
		alarms = append(alarms, map[string]any{"name": a.Name, "criticality": crit(a.Severity), "message": a.Message})
	}
	_ = m
	return map[string]any{"status": res.Status, "alarms": alarms, "dynamic_alarms": dynamic}, nil
}

func (s *Server) mmSyncBase(c *Ctx) (any, error) {
	modelID := c.F.Int("model_id", 0)
	m, err := s.loadModelRow(c, s.DB, modelID)
	if err != nil {
		return nil, err
	}
	if m.BaseID != nil {
		return nil, httpx.BadRequest("err_cannot_sync_an_extended_model")
	}
	affected, err := db.Int64s(c, s.DB, `SELECT id FROM asset.models WHERE base_model_id = $1 ORDER BY id`, modelID)
	if err != nil {
		return nil, err
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		for _, ext := range affected {
			if _, err := tx.Exec(c, `INSERT INTO asset.model_parameters (model_id, alias, name, unit, data_type, scale, gain_id, parameter_type_id, base_parameter_id)
				SELECT $1, b.alias, b.name, b.unit, b.data_type, b.scale, b.gain_id, b.parameter_type_id, b.id FROM asset.model_parameters b
				WHERE b.model_id = $2 ON CONFLICT (model_id, alias) DO UPDATE SET name = EXCLUDED.name, unit = EXCLUDED.unit,
				data_type = EXCLUDED.data_type, scale = EXCLUDED.scale, gain_id = EXCLUDED.gain_id, parameter_type_id = EXCLUDED.parameter_type_id,
				base_parameter_id = EXCLUDED.base_parameter_id, updated_at = now()`, ext, modelID); err != nil {
				if db.IsUniqueViolation(err) {
					return httpx.BadRequest("err_model_variable_with_the_same_display_name_already_exists_in_the_extended_models")
				}
				return err
			}
			if _, err := tx.Exec(c, `INSERT INTO asset.equipment_parameters (equipment_id, model_parameter_id)
				SELECT e.id, mp.id FROM asset.equipment e JOIN asset.model_parameters mp ON mp.model_id = e.model_id
				WHERE e.model_id = $1 ON CONFLICT DO NOTHING`, ext); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return emptyIfNil(affected), nil
}

func (s *Server) mmUpdateRunningHourLimit(c *Ctx) (any, error) {
	// "Set Limit Hours" is available on Equipment Overview.
	if !c.User.IsSuperuser() {
		if err := c.Require("equipmentoverview", "update"); err != nil {
			return nil, err
		}
	}
	modelID := c.F.Int("model_id", 0)
	w, cr := c.F.Int("warning_hour", 0), c.F.Int("critical_hour", 0)
	nw, nc, auto := c.F.Bool("is_notify_warning", false), c.F.Bool("is_notify_critical", false), c.F.Bool("is_auto_create_maintenance", false)
	if w < 0 || cr < 0 {
		return nil, httpx.BadRequest("err_input_valid_number")
	}
	if nw && w == 0 {
		return nil, httpx.BadRequest("Warning hour must be greater than 0 when notifications are enabled")
	}
	if (nc || auto) && cr == 0 {
		return nil, httpx.BadRequest("Critical hour must be greater than 0 when notifications are enabled")
	}
	tag, err := s.DB.Exec(c, `UPDATE asset.models SET running_hour_warning = $2, running_hour_critical = $3, running_hour_notify_warn = $4,
		running_hour_notify_crit = $5, running_hour_auto_maint = $6, updated_at = now() WHERE id = $1`, modelID, w, cr, nw, nc, auto)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("err_model_not_found")
	}
	s.Engine.Invalidate()
	return "Running hour limit successfully updated", nil
}
