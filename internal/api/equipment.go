package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

func (s *Server) registerEquipment() {
	p := "equipment_manager/"
	s.handle(p+"get_parameter_gains", AccessSuper, s.listTable(`SELECT id, name FROM asset.parameter_gains ORDER BY id`))
	s.handle(p+"get_power_meter_types", AccessSuper, s.listTable(`SELECT id, name FROM asset.power_meter_types ORDER BY order_id, id`))
	s.handle(p+"get_source_types", AccessSuper, s.emSourceTypes)
	s.handle(p+"get_used_cost_types_per_site", AccessSuper, s.listTable(`SELECT id, name FROM asset.cost_types ORDER BY order_id, id`))
	s.handle(p+"get_equipment_models", AccessSuper, s.emModels)
	s.handle(p+"get_equipment_licenses", AccessSuper, s.emLicenses)
	s.handle(p+"get_gateways", AccessSuper, s.emGateways)
	s.handle(p+"get_equipment_list", AccessSuper, s.emList)
	s.handle(p+"get_location_tree", AccessSuper, s.emLocationTree)
	s.handle(p+"get_equipment_info", AccessSuper, s.emInfo)
	s.handle(p+"get_equipment_parameters", AccessSuper, s.emParameters)
	s.handle(p+"get_gateway_tags", AccessSuper, s.emGatewayTags)
	s.handle(p+"get_equipment_parameters_vtqcs", AccessSuper, s.emVTQCs)
	s.handle(p+"add_equipment", AccessSuper, s.emAdd)
	s.handle(p+"update_equipment_temp", AccessSuper, s.emUpdate)
	s.handle(p+"delete_equipment", AccessSuper, s.emDelete)
	s.handle(p+"update_equipment_parameter_mapping", AccessSuper, s.emUpdateMapping)
}

func (s *Server) emSourceTypes(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT id, name, category_id FROM asset.source_types ORDER BY order_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, cat int32
		var name string
		if err := rows.Scan(&id, &name, &cat); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "category_id": cat})
	}
	return out, rows.Err()
}

func (s *Server) emModels(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT m.id, m.name, t.id, t.name, b.id, b.name, sr.id, sr.name FROM asset.models m
		JOIN asset.types t ON t.id = m.type_id JOIN asset.brands b ON b.id = m.brand_id JOIN asset.series sr ON sr.id = m.series_id
		ORDER BY lower(m.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, tid, bid, sid int64
		var name, tn, bn, sn string
		if err := rows.Scan(&id, &name, &tid, &tn, &bid, &bn, &sid, &sn); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "type_id": tid, "type": tn, "brand_id": bid, "brand": bn, "series_id": sid, "series": sn})
	}
	return out, rows.Err()
}

func (s *Server) emLicenses(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT l.id, l.name, l.total, l.expired_at,
			(SELECT count(*) FROM asset.equipment e WHERE e.license_id = l.id)
		FROM core.licenses l WHERE l.type <> 'BASE' ORDER BY lower(l.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var total int
		var exp *time.Time
		var used int64
		if err := rows.Scan(&id, &name, &total, &exp, &used); err != nil {
			return nil, err
		}
		active, status := licenseStatus(exp)
		avail := int64(total) - used
		if avail < 0 {
			avail = 0
		}
		out = append(out, map[string]any{"id": id, "name": name, "used": used, "available": avail, "l_active": active, "l_status": status})
	}
	return out, rows.Err()
}

func (s *Server) emGateways(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT alias, name, type, is_active FROM dh.gateways ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var alias, name, typ string
		var active bool
		if err := rows.Scan(&alias, &name, &typ, &active); err != nil {
			return nil, err
		}
		var status any
		if active {
			status = s.Hub.Status(alias).Connected
		}
		out = append(out, map[string]any{"alias": alias, "name": name, "protocol_alias": typ, "protocol_name": protocolName(typ), "status": status})
	}
	return out, rows.Err()
}

func (s *Server) emList(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT id, alias, name, image_path, model_id, location_id, license_id FROM asset.equipment ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, model, loc int64
		var alias, name string
		var image *string
		var lic *int64
		if err := rows.Scan(&id, &alias, &name, &image, &model, &loc, &lic); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "alias": alias, "name": name, "image_path": imageURL(image), "model_id": model,
			"location_id": loc, "license_id": lic})
	}
	return out, rows.Err()
}

func (s *Server) emLocationTree(c *Ctx) (any, error) {
	return s.buildLocationTree(c, c.F.Str("search"), nil, nil)
}

type equipmentRow struct {
	ID         int64
	Alias      string
	Name       string
	Image      *string
	ModelID    int64
	LocationID int64
	LicenseID  *int64
	IsUtility  bool
	Installed  time.Time
}

func (s *Server) equipmentByAlias(ctx context.Context, q db.Querier, alias string) (*equipmentRow, error) {
	e := &equipmentRow{}
	err := q.QueryRow(ctx, `SELECT id, alias, name, image_path, model_id, location_id, license_id, is_utility, installed_at
		FROM asset.equipment WHERE lower(alias) = lower($1)`, strings.TrimSpace(alias)).
		Scan(&e.ID, &e.Alias, &e.Name, &e.Image, &e.ModelID, &e.LocationID, &e.LicenseID, &e.IsUtility, &e.Installed)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_equipment_not_found")
		}
		return nil, err
	}
	return e, nil
}

func (s *Server) emInfo(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"alias": e.Alias, "name": e.Name, "image_path": imageURL(e.Image), "model_id": e.ModelID,
		"created_at": e.Installed.UnixMilli(), "location_id": e.LocationID, "license_id": e.LicenseID, "is_utility": e.IsUtility,
		// energy-monitoring fields (unused by this product, kept for completeness)
		"installation_date": e.Installed.Format("2006-01-02"), "power_meter_type_id": 1, "source_type_id": 1,
		"usage_limit_use": false, "usage_limit_rate_kwh": 0, "usage_limit_interval": "monthly", "limit_notification_use": false,
		"limit_notification_warning_percentage": 0, "limit_notification_critical_percentage": 0, "rate_limit_use": false,
		"rate_limit_holiday_percentage": 0, "rate_limit_percentages": []any{}, "not_calculate_efficiency": false,
	}, nil
}

func (s *Server) emParameters(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT mp.alias, mp.name, mp.unit, g.alias, ep.tag_name, ep.is_logged, ep.is_displayed, mp.data_type,
			mp.scale, COALESCE(ep.gain_id, mp.gain_id), mp.parameter_type_id
		FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		LEFT JOIN dh.gateways g ON g.id = ep.gateway_id WHERE ep.equipment_id = $1 ORDER BY lower(mp.name)`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var alias, name, unit, dt string
		var gw, tag *string
		var logged, displayed bool
		var scale, gain, ptype *int32
		if err := rows.Scan(&alias, &name, &unit, &gw, &tag, &logged, &displayed, &dt, &scale, &gain, &ptype); err != nil {
			return nil, err
		}
		if gw == nil || tag == nil {
			gw, tag = nil, nil
		}
		row := map[string]any{"alias": alias, "name": name, "unit": unit, "gateway_alias": gw, "tag": tag, "is_logged": logged,
			"is_displayed": displayed, "data_type": dt, "scale": scale}
		if gain != nil {
			row["gain_id"] = *gain
		}
		if ptype != nil {
			row["model_parameter_type_id"] = *ptype
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Server) emGatewayTags(c *Ctx) (any, error) {
	id, _, err := s.gatewayID(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT name, address, description FROM dh.gateway_tags WHERE gateway_id = $1 ORDER BY lower(name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var addr, desc *string
		if err := rows.Scan(&name, &addr, &desc); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "address": addr, "description": desc})
	}
	return out, rows.Err()
}

func (s *Server) emVTQCs(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT mp.alias, g.alias, ep.tag_name FROM asset.equipment_parameters ep
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id JOIN dh.gateways g ON g.id = ep.gateway_id
		WHERE ep.equipment_id = $1 AND ep.tag_name IS NOT NULL`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]datahub.VTQC{}
	for rows.Next() {
		var alias, gw, tag string
		if err := rows.Scan(&alias, &gw, &tag); err != nil {
			return nil, err
		}
		if v, ok := s.Hub.Value(gw, tag); ok {
			if v.T < 0 {
				v.T = 0
			}
			out[alias] = v
		}
	}
	return out, rows.Err()
}

type equipmentInput struct {
	alias, name string
	modelID     int64
	locationID  int64
	licenseID   *int64
	installed   *time.Time
	isUtility   *bool
}

func (s *Server) equipmentInput(c *Ctx) (*equipmentInput, error) {
	in := &equipmentInput{
		alias:      c.F.StrTrim("alias"),
		name:       c.F.StrTrim("name"),
		modelID:    c.F.Int("model_id", 0),
		locationID: c.F.Int("location_id", 0),
		licenseID:  c.F.IntPtr("license_id"),
		isUtility:  c.F.BoolPtr("is_utility"),
	}
	if ms := c.F.IntPtr("created_at"); ms != nil && *ms > 0 {
		t := time.UnixMilli(*ms).UTC()
		in.installed = &t
	} else if t := c.F.TimePtr("installation_date", c.Location()); t != nil {
		in.installed = t
	}
	if in.licenseID != nil && *in.licenseID <= 0 {
		in.licenseID = nil
	}
	if in.name == "" {
		return nil, httpx.BadRequest("err_invalid_equipment_name")
	}
	var ok bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.models WHERE id = $1)`, in.modelID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.BadRequest("err_invalid_model")
	}
	if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.locations WHERE id = $1)`, in.locationID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.BadRequest("err_invalid_location")
	}
	return in, nil
}

// checkLicense verifies that an equipment license has room for one more
// equipment (excluding the equipment itself).
func (s *Server) checkLicense(ctx context.Context, q db.Querier, licenseID int64, excludeEquipment int64) error {
	var total int
	var typ string
	var exp *time.Time
	if err := q.QueryRow(ctx, `SELECT total, type, expired_at FROM core.licenses WHERE id = $1`, licenseID).Scan(&total, &typ, &exp); err != nil {
		if db.IsNoRows(err) {
			return httpx.BadRequest("err_invalid_license")
		}
		return err
	}
	if typ == "BASE" {
		return httpx.BadRequest("err_invalid_license")
	}
	if exp != nil && exp.Before(time.Now()) {
		return httpx.BadRequest("err_license_expired")
	}
	var used int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM asset.equipment WHERE license_id = $1 AND id <> $2`, licenseID, excludeEquipment).Scan(&used); err != nil {
		return err
	}
	if used >= total {
		return httpx.BadRequest("err_license_already_reached_maximum_usage")
	}
	return nil
}

// syncEquipmentParameters creates the parameter rows of an equipment from its
// model, keeping mappings/limits of parameters whose alias still exists.
func syncEquipmentParameters(ctx context.Context, tx pgx.Tx, equipmentID, modelID int64) error {
	// Re-point rows of the previous model to the new model's parameters with the same alias.
	if _, err := tx.Exec(ctx, `UPDATE asset.equipment_parameters ep SET model_parameter_id = np.id, updated_at = now()
		FROM asset.model_parameters mp, asset.model_parameters np
		WHERE ep.equipment_id = $1 AND mp.id = ep.model_parameter_id AND mp.model_id <> $2
		AND np.model_id = $2 AND np.alias = mp.alias
		AND NOT EXISTS (SELECT 1 FROM asset.equipment_parameters x WHERE x.equipment_id = $1 AND x.model_parameter_id = np.id)`, equipmentID, modelID); err != nil {
		return err
	}
	// Remove rows that do not belong to the model anymore.
	if _, err := tx.Exec(ctx, `DELETE FROM asset.equipment_parameters ep USING asset.model_parameters mp
		WHERE ep.equipment_id = $1 AND mp.id = ep.model_parameter_id AND mp.model_id <> $2`, equipmentID, modelID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO asset.equipment_parameters (equipment_id, model_parameter_id, is_logged, is_displayed)
		SELECT $1, mp.id, TRUE, TRUE FROM asset.model_parameters mp WHERE mp.model_id = $2
		ON CONFLICT (equipment_id, model_parameter_id) DO NOTHING`, equipmentID, modelID)
	return err
}

func (s *Server) emAdd(c *Ctx) (any, error) {
	in, err := s.equipmentInput(c)
	if err != nil {
		return nil, err
	}
	if !aliasRe.MatchString(in.alias) {
		return nil, httpx.BadRequest("err_invalid_equipment_id")
	}
	var image *string
	if fh := c.F.File("image_file"); fh != nil {
		saved, err := s.Store.SaveMultipart(c, "equipment", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		image = &saved.URL
	}
	var id int64
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.equipment WHERE lower(alias) = lower($1))`, in.alias).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return httpx.BadRequest("err_equipment_with_the_same_id_already_exists")
		}
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.equipment WHERE lower(name) = lower($1))`, in.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return httpx.BadRequest("err_equipment_with_the_same_name_already_exists")
		}
		if in.licenseID != nil {
			if _, err := tx.Exec(c, `SELECT 1 FROM core.licenses WHERE id = $1 FOR UPDATE`, *in.licenseID); err != nil {
				return err
			}
			if err := s.checkLicense(c, tx, *in.licenseID, 0); err != nil {
				return err
			}
		}
		installed := time.Now().UTC()
		if in.installed != nil {
			installed = *in.installed
		}
		utility := in.isUtility != nil && *in.isUtility
		if err := tx.QueryRow(c, `INSERT INTO asset.equipment (alias, name, image_path, model_id, location_id, license_id, is_utility, installed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, in.alias, in.name, image, in.modelID, in.locationID, in.licenseID, utility, installed).Scan(&id); err != nil {
			return err
		}
		return syncEquipmentParameters(c, tx, id, in.modelID)
	})
	if err != nil {
		if image != nil {
			s.Store.DeleteURL(c, *image)
		}
		return nil, err
	}
	s.Engine.Invalidate()
	return id, nil
}

func (s *Server) emUpdate(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	in, err := s.equipmentInput(c)
	if err != nil {
		return nil, err
	}
	image := e.Image
	var newImage *string
	if fh := c.F.File("image_file"); fh != nil {
		saved, err := s.Store.SaveMultipart(c, "equipment", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		newImage = &saved.URL
		image = newImage
	} else if c.F.Bool("delete_image", false) {
		image = nil
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.equipment WHERE lower(name) = lower($1) AND id <> $2)`, in.name, e.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return httpx.BadRequest("err_equipment_with_the_same_name_already_exists")
		}
		if in.licenseID != nil && (e.LicenseID == nil || *e.LicenseID != *in.licenseID) {
			if _, err := tx.Exec(c, `SELECT 1 FROM core.licenses WHERE id = $1 FOR UPDATE`, *in.licenseID); err != nil {
				return err
			}
			if err := s.checkLicense(c, tx, *in.licenseID, e.ID); err != nil {
				return err
			}
		}
		installed := e.Installed
		if in.installed != nil {
			installed = *in.installed
		}
		utility := e.IsUtility
		if in.isUtility != nil {
			utility = *in.isUtility
		}
		if _, err := tx.Exec(c, `UPDATE asset.equipment SET name = $2, image_path = $3, model_id = $4, location_id = $5, license_id = $6,
			is_utility = $7, installed_at = $8, updated_at = now() WHERE id = $1`,
			e.ID, in.name, image, in.modelID, in.locationID, in.licenseID, utility, installed); err != nil {
			return err
		}
		if in.modelID != e.ModelID {
			return syncEquipmentParameters(c, tx, e.ID, in.modelID)
		}
		return nil
	})
	if err != nil {
		if newImage != nil {
			s.Store.DeleteURL(c, *newImage)
		}
		return nil, err
	}
	if e.Image != nil && (image == nil || *image != *e.Image) {
		s.Store.DeleteURL(c, *e.Image)
	}
	s.Engine.Invalidate()
	return httpx.Key("equipment_successfully_updated", in.name), nil
}

func (s *Server) emDelete(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	var open int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.maintenance_orders WHERE asset_id = $1 AND status_id IN (4, 7)`, e.ID).Scan(&open); err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, httpx.BadRequest("Equipment has maintenance work in progress")
	}
	if _, err := s.DB.Exec(c, `DELETE FROM asset.equipment WHERE id = $1`, e.ID); err != nil {
		return nil, fail(err)
	}
	if e.Image != nil {
		s.Store.DeleteURL(c, *e.Image)
	}
	s.Engine.Invalidate()
	return httpx.Key("equipment_successfully_deleted", e.Name), nil
}

type mappingInput struct {
	GatewayAlias *string `json:"gateway_alias"`
	Tag          *string `json:"tag"`
	IsLogged     *bool   `json:"is_logged"`
	IsDisplayed  *bool   `json:"is_displayed"`
	GainID       *int64  `json:"gain_id"`
}

func (s *Server) emUpdateMapping(c *Ctx) (any, error) {
	e, err := s.equipmentByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	mappings := map[string]mappingInput{}
	if _, err := c.F.JSON("mappings", &mappings); err != nil {
		return nil, httpx.BadRequest("err_invalid_mappings")
	}
	n := 0
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		for alias, m := range mappings {
			var epid int64
			err := tx.QueryRow(c, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
				WHERE ep.equipment_id = $1 AND mp.alias = $2`, e.ID, alias).Scan(&epid)
			if err != nil {
				if db.IsNoRows(err) {
					return httpx.BadRequest(httpx.Key("err_parameter_not_found", alias))
				}
				return err
			}
			var gwID *int64
			var tag *string
			if m.GatewayAlias != nil && *m.GatewayAlias != "" && m.Tag != nil && strings.TrimSpace(*m.Tag) != "" {
				id, _, err := s.gatewayID(c, tx, *m.GatewayAlias)
				if err != nil {
					return err
				}
				t := strings.TrimSpace(*m.Tag)
				var ok bool
				if err := tx.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM dh.gateway_tags WHERE gateway_id = $1 AND name = $2)`, id, t).Scan(&ok); err != nil {
					return err
				}
				if !ok {
					return httpx.BadRequest("err_invalid_tag_name")
				}
				gwID, tag = &id, &t
			}
			logged, displayed := true, true
			if m.IsLogged != nil {
				logged = *m.IsLogged
			}
			if m.IsDisplayed != nil {
				displayed = *m.IsDisplayed
			}
			if _, err := tx.Exec(c, `UPDATE asset.equipment_parameters SET gateway_id = $2, tag_name = $3, is_logged = $4, is_displayed = $5,
				gain_id = COALESCE($6, gain_id), updated_at = now() WHERE id = $1`, epid, gwID, tag, logged, displayed, m.GainID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Engine.Invalidate()
	return httpx.Key("parameters_successfully_mapped", strconv.Itoa(n)), nil
}
