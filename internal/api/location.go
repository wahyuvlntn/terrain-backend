package api

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/auth"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

func (s *Server) registerLocation() {
	s.handle("location/getlocationtreebyfilter", AccessUser, s.locTreeByFilter)
	s.handle("location/getlocationtreebyfilterdel", AccessUser, s.locTreeByFilter)
	s.handle("location/getlocationdetailsbyid", AccessUser, s.locDetails)
	s.handle("location/deletelocation", AccessSuper, s.locDelete)
	s.handle("location/insertnewlocation", AccessSuper, s.locInsert)
	s.handle("location/updatelocation", AccessSuper, s.locUpdate)
	s.handle("location/updatelocationparentid", AccessSuper, s.locUpdateParent)
	s.handle("location/getgeneratedqrinformation", AccessUser, s.locQR)
	s.handle("location/getallassetsbyparentlocation", AccessUser, s.locAssets)
	s.handle("location/getmodelsbyselectedlocation", AccessUser, s.locModels)
	s.handle("location/getbrandsbyselectedmodels", AccessUser, s.locBrands)
	s.handle("location/getlicensesbyassetids", AccessUser, s.locLicenses)

	s.handle("location_overview/getalllocations", AccessUser, s.loAllLocations)
	s.handle("location_overview/getdirectchildbylocationid", AccessUser, s.loDirectChildren)
	s.handle("location_overview/getlocationapq", AccessUser, s.loAPQ)
	s.handle("location_overview/getlocationancestors", AccessUser, s.loAncestors)
	s.handle("location_overview/getlocationtreeexport", AccessUser, s.loExport)
	s.handle("location_overview/getplantoverview", AccessUser, s.loPlantOverview)
}

func (s *Server) locTreeByFilter(c *Ctx) (any, error) {
	trees, err := s.buildLocationTree(c, c.F.Str("search_filter"), c.F.IntPtr("excluded_id"), nil)
	if err != nil {
		return nil, err
	}
	return treeRows(trees), nil
}

func (s *Server) locDetails(c *Ctx) (any, error) {
	id := c.F.IntPtr("location_id")
	if id == nil {
		return nil, nil
	}
	row, err := db.Row(c, s.DB, `SELECT l.id, l.name, l.parent_id, p.name AS parent_name, l.image_path, l.availability_th, l.performance_th,
		l.quality_th, l.currency_id, l.timezone, l.energy_th, l.carbon_th, l.cost_th
		FROM asset.locations l LEFT JOIN asset.locations p ON p.id = l.parent_id WHERE l.id = $1`, *id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.NotFound("err_location_not_found")
	}
	out := map[string]any{
		"id": row["id"], "locationName": row["name"], "parentId": row["parent_id"], "parentLocation": row["parent_name"],
		"locationImagePath": imageURL(strPtr(row["image_path"])),
		"availabilityTh":    row["availability_th"], "performanceTh": row["performance_th"], "qualityTh": row["quality_th"],
	}
	if v, ok := row["currency_id"].(int32); ok {
		out["currencyId"] = fmt.Sprint(v)
	}
	if v, ok := row["timezone"].(string); ok {
		out["timezone"] = v
	}
	for _, k := range []string{"energy_th", "carbon_th", "cost_th"} {
		if v, ok := row[k].(float64); ok {
			out[map[string]string{"energy_th": "energyTh", "carbon_th": "carbonTh", "cost_th": "costTh"}[k]] = v
		}
	}
	return out, nil
}

func strPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func (s *Server) locDelete(c *Ctx) (any, error) {
	id := c.F.Int("location_id", 0)
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	if tree.nodes[id] == nil {
		return nil, httpx.NotFound("err_location_not_found")
	}
	ids := []int64{}
	for k := range tree.descendants([]int64{id}) {
		ids = append(ids, k)
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM asset.equipment WHERE location_id = ANY($1)`, ids).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_location_has_assets")
	}
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM core.user_group_locations WHERE location_id = ANY($1)`, ids).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("err_location_has_user_groups")
	}
	images, err := db.Strings(c, s.DB, `SELECT image_path FROM asset.locations WHERE id = ANY($1) AND image_path IS NOT NULL`, ids)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `DELETE FROM asset.locations WHERE id = $1`, id); err != nil {
		return nil, fail(err)
	}
	for _, img := range images {
		s.Store.DeleteURL(c, img)
	}
	s.Engine.Invalidate()
	return "deleted_selected_location", nil
}

type locationInput struct {
	name                               string
	parentID                           *int64
	availability, performance, quality float64
	energy, carbon, cost               *float64
	currency                           *int64
	timezone                           *string
}

func (c *Ctx) locationInput() (*locationInput, error) {
	in := &locationInput{
		name:         c.F.StrTrim("name"),
		parentID:     c.F.IntPtr("parent_id"),
		availability: c.F.Float("availability_th", 0),
		performance:  c.F.Float("performance_th", 0),
		quality:      c.F.Float("quality_th", 0),
		energy:       c.F.FloatPtr("energy_th"),
		carbon:       c.F.FloatPtr("carbon_th"),
		cost:         c.F.FloatPtr("cost_th"),
		currency:     c.F.IntPtr("currency_id"),
		timezone:     c.F.NonEmptyPtr("timezone"),
	}
	if in.parentID != nil && *in.parentID <= 0 {
		in.parentID = nil
	}
	if in.name == "" {
		return nil, httpx.BadRequest("location_name_is_required")
	}
	if len([]rune(in.name)) < 4 {
		return nil, httpx.BadRequest("Location name must be at least 4 characters")
	}
	for _, v := range []float64{in.availability, in.performance, in.quality} {
		if v < 0 || v > 100 {
			return nil, httpx.BadRequest("err_input_valid_number_between_0_100")
		}
	}
	if in.timezone != nil {
		if _, err := time.LoadLocation(*in.timezone); err != nil {
			in.timezone = nil
		}
	}
	return in, nil
}

func (s *Server) checkSiblingName(c *Ctx, q db.Querier, name string, parentID *int64, exclude int64) error {
	var exists bool
	err := q.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.locations WHERE lower(name) = lower($1)
		AND parent_id IS NOT DISTINCT FROM $2 AND id <> $3)`, name, parentID, exclude).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return httpx.BadRequest("err_unique_location")
	}
	return nil
}

func (s *Server) locInsert(c *Ctx) (any, error) {
	in, err := c.locationInput()
	if err != nil {
		return nil, err
	}
	if in.parentID != nil {
		var ok bool
		if err := s.DB.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM asset.locations WHERE id = $1)`, *in.parentID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, httpx.BadRequest("err_location_not_found")
		}
	}
	if err := s.checkSiblingName(c, s.DB, in.name, in.parentID, 0); err != nil {
		return nil, err
	}
	var image *string
	if fh := c.F.File("location_image"); fh != nil {
		if fh.Size > 4<<20 {
			return nil, httpx.BadRequest("File size is too large")
		}
		saved, err := s.Store.SaveMultipart(c, "locations", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		image = &saved.URL
	} else {
		return nil, httpx.BadRequest("err_image_required")
	}
	var id int64
	err = s.DB.QueryRow(c, `INSERT INTO asset.locations (parent_id, name, image_path, availability_th, performance_th, quality_th,
		energy_th, carbon_th, cost_th, currency_id, timezone) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		in.parentID, in.name, image, in.availability, in.performance, in.quality, in.energy, in.carbon, in.cost, in.currency, in.timezone).Scan(&id)
	if err != nil {
		s.Store.DeleteURL(c, *image)
		return nil, err
	}
	return "added_new_location", nil
}

func (s *Server) locUpdate(c *Ctx) (any, error) {
	id := c.F.Int("location_id", 0)
	in, err := c.locationInput()
	if err != nil {
		return nil, err
	}
	var oldImage *string
	var oldParent *int64
	if err := s.DB.QueryRow(c, `SELECT image_path, parent_id FROM asset.locations WHERE id = $1`, id).Scan(&oldImage, &oldParent); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_location_not_found")
		}
		return nil, err
	}
	if err := s.validateParent(c, id, in.parentID); err != nil {
		return nil, err
	}
	if err := s.checkSiblingName(c, s.DB, in.name, in.parentID, id); err != nil {
		return nil, err
	}
	image := oldImage
	if fh := c.F.File("location_image"); fh != nil {
		if fh.Size > 4<<20 {
			return nil, httpx.BadRequest("File size is too large")
		}
		saved, err := s.Store.SaveMultipart(c, "locations", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		image = &saved.URL
	}
	_, err = s.DB.Exec(c, `UPDATE asset.locations SET parent_id = $2, name = $3, image_path = $4, availability_th = $5, performance_th = $6,
		quality_th = $7, energy_th = COALESCE($8, energy_th), carbon_th = COALESCE($9, carbon_th), cost_th = COALESCE($10, cost_th),
		currency_id = COALESCE($11, currency_id), timezone = COALESCE($12, timezone), updated_at = now() WHERE id = $1`,
		id, in.parentID, in.name, image, in.availability, in.performance, in.quality, in.energy, in.carbon, in.cost, in.currency, in.timezone)
	if err != nil {
		return nil, err
	}
	if image != oldImage && oldImage != nil {
		s.Store.DeleteURL(c, *oldImage)
	}
	return "successfully_edited_location", nil
}

func (s *Server) validateParent(c *Ctx, id int64, parent *int64) error {
	if parent == nil {
		return nil
	}
	if *parent == id {
		return httpx.BadRequest("err_cannot_be_own_parent")
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return err
	}
	if tree.nodes[*parent] == nil {
		return httpx.BadRequest("err_location_not_found")
	}
	if tree.descendants([]int64{id})[*parent] {
		return httpx.BadRequest("err_a_location_must_not_be_placed_under_its_own_descendants")
	}
	return nil
}

func (s *Server) locUpdateParent(c *Ctx) (any, error) {
	id := c.F.Int("location_id", 0)
	parent := c.F.IntPtr("parent_id")
	if parent != nil && *parent <= 0 {
		parent = nil
	}
	var name string
	if err := s.DB.QueryRow(c, `SELECT name FROM asset.locations WHERE id = $1`, id).Scan(&name); err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_location_not_found")
		}
		return nil, err
	}
	if err := s.validateParent(c, id, parent); err != nil {
		return nil, err
	}
	if err := s.checkSiblingName(c, s.DB, name, parent, id); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `UPDATE asset.locations SET parent_id = $2, updated_at = now() WHERE id = $1`, id, parent); err != nil {
		return nil, err
	}
	return "successfully_edited_location", nil
}

func qrField(name, value string) map[string]any {
	return map[string]any{"name": name, "value": value, "excluded": false}
}

func (s *Server) locQR(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	locIDs := httpx.ParseIDList(c.F.Str("location_ids"))
	assetIDs := httpx.ParseIDList(c.F.Str("asset_ids"))
	locations := []map[string]any{}
	for _, id := range locIDs {
		n := tree.nodes[id]
		if n == nil || !scope.AllowsLocation(id) {
			continue
		}
		parent := "-"
		if n.ParentID != nil {
			parent = tree.name(*n.ParentID)
		}
		anc := tree.ancestors(id)
		locations = append(locations, map[string]any{
			"value":       toJSON(map[string]any{"type": "location", "id": id, "path": fmt.Sprintf("/location-overview?location_id=%d&locations=%s", id, toJSON(int64Strings(anc)))}),
			"firstField":  qrField("Location", n.Name),
			"secondField": qrField("Parent", parent),
			"thirdField":  qrField("Path", tree.path(id, " > ")),
			"fourthField": qrField("ID", fmt.Sprint(id)),
			"fifthField":  map[string]any{"name": "", "value": "", "excluded": true},
		})
	}
	equipment := []map[string]any{}
	if len(assetIDs) > 0 {
		rows, err := s.DB.Query(c, `SELECT e.id, e.alias, e.name, e.location_id, m.name, t.name FROM asset.equipment e
			JOIN asset.models m ON m.id = e.model_id JOIN asset.types t ON t.id = m.type_id WHERE e.id = ANY($1) ORDER BY lower(e.name)`, assetIDs)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, loc int64
			var alias, name, model, typ string
			if err := rows.Scan(&id, &alias, &name, &loc, &model, &typ); err != nil {
				return nil, err
			}
			if !scope.AllowsLocation(loc) {
				continue
			}
			equipment = append(equipment, map[string]any{
				"value":       toJSON(map[string]any{"type": "equipment", "id": id, "alias": alias, "path": fmt.Sprintf("/asset-overview/%d", id)}),
				"firstField":  qrField("Equipment", name),
				"secondField": qrField("ID", alias),
				"thirdField":  qrField("Model", model),
				"fourthField": qrField("Type", typ),
				"fifthField":  qrField("Location", tree.path(loc, " > ")),
			})
		}
	}
	out := [][]map[string]any{}
	if len(locations) > 0 {
		out = append(out, locations)
	}
	if len(equipment) > 0 {
		out = append(out, equipment)
	}
	return out, nil
}

func int64Strings(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprint(id)
	}
	return out
}

// pgTextArray parses a Postgres array literal of strings ("{a,b}").
func pgTextArray(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "null") || strings.EqualFold(s, "undefined") {
		return nil, false
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, "}"), "{")
	out := []string{}
	if strings.TrimSpace(s) == "" {
		return out, true
	}
	for _, p := range splitCSVQuoted(s) {
		out = append(out, p)
	}
	return out, true
}

// splitCSVQuoted splits a comma separated list honouring double quotes.
func splitCSVQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQ = !inQ
		case ch == '\\' && inQ && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case ch == ',' && !inQ:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, strings.TrimSpace(cur.String()))
	return out
}

func (s *Server) locSubtreeAssets(c *Ctx, locationID int64) ([]int64, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	locs := []int64{}
	for id := range tree.descendants([]int64{locationID}) {
		if scope.AllowsLocation(id) {
			locs = append(locs, id)
		}
	}
	return db.Int64s(c, s.DB, `SELECT id FROM asset.equipment WHERE location_id = ANY($1)`, locs)
}

func (s *Server) locAssets(c *Ctx) (any, error) {
	ids, err := s.locSubtreeAssets(c, c.F.Int("location_id", 0))
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	type row struct {
		AssetID        int64  `json:"assetId"`
		LocationName   string `json:"locationName"`
		AssetName      string `json:"assetName"`
		ModelName      string `json:"modelName"`
		ModelID        int64  `json:"modelId"`
		BrandName      string `json:"brandName"`
		SeriesName     string `json:"seriesName"`
		Gateway        string `json:"industrialGateway"`
		License        string `json:"license"`
		IsLicenseActve *bool  `json:"isLicenseActive"`
		Count          int    `json:"count"`
		locID          int64
	}
	rows, err := s.DB.Query(c, `SELECT e.id, e.location_id, e.name, m.name, m.id, b.name, sr.name,
			COALESCE((SELECT string_agg(DISTINCT g.name, ', ') FROM asset.equipment_parameters ep JOIN dh.gateways g ON g.id = ep.gateway_id
				WHERE ep.equipment_id = e.id), '-'),
			COALESCE(l.name, '-'), CASE WHEN l.id IS NULL THEN NULL ELSE (l.expired_at IS NULL OR l.expired_at > now()) END
		FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id JOIN asset.brands b ON b.id = m.brand_id
		JOIN asset.series sr ON sr.id = m.series_id LEFT JOIN core.licenses l ON l.id = e.license_id
		WHERE e.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var all []*row
	for rows.Next() {
		r := &row{}
		if err := rows.Scan(&r.AssetID, &r.locID, &r.AssetName, &r.ModelName, &r.ModelID, &r.BrandName, &r.SeriesName, &r.Gateway, &r.License, &r.IsLicenseActve); err != nil {
			rows.Close()
			return nil, err
		}
		r.LocationName = tree.name(r.locID)
		all = append(all, r)
	}
	rows.Close()

	// Filters ("{...}" literals; an empty literal matches nothing).
	if ids, present := pgTextArray(c.F.Str("model_ids")); present {
		set := map[string]bool{}
		for _, v := range ids {
			set[v] = true
		}
		all = filterRows(all, func(r *row) bool { return set[fmt.Sprint(r.ModelID)] })
	}
	if brands, present := pgTextArray(c.F.Str("brands")); present {
		set := map[string]bool{}
		for _, v := range brands {
			set[strings.ToLower(v)] = true
		}
		all = filterRows(all, func(r *row) bool { return set[strings.ToLower(r.BrandName)] })
	}
	if aids, present := pgTextArray(c.F.Str("asset_ids")); present {
		set := map[string]bool{}
		for _, v := range aids {
			set[v] = true
		}
		all = filterRows(all, func(r *row) bool { return set[fmt.Sprint(r.AssetID)] })
	}
	if lics, present := pgTextArray(c.F.Str("licenses")); present {
		set := map[string]bool{}
		for _, v := range lics {
			set[strings.ToLower(v)] = true
		}
		all = filterRows(all, func(r *row) bool { return set[strings.ToLower(r.License)] })
	}

	type order struct {
		key string
		get func(r *row) string
	}
	orders := []order{
		{"order_name", func(r *row) string { return strings.ToLower(r.AssetName) }},
		{"order_brand", func(r *row) string { return strings.ToLower(r.BrandName) }},
		{"order_series", func(r *row) string { return strings.ToLower(r.SeriesName) }},
		{"order_model", func(r *row) string { return strings.ToLower(r.ModelName) }},
		{"order_license", func(r *row) string { return strings.ToLower(r.License) }},
	}
	sorted := false
	for _, o := range orders {
		if asc := c.F.BoolPtr(o.key); asc != nil {
			get := o.get
			a := *asc
			sort.SliceStable(all, func(i, j int) bool {
				if a {
					return get(all[i]) < get(all[j])
				}
				return get(all[i]) > get(all[j])
			})
			sorted = true
			break
		}
	}
	if !sorted {
		desc := false
		if asc := c.F.BoolPtr("order_id"); asc != nil {
			desc = !*asc
		}
		sort.SliceStable(all, func(i, j int) bool {
			if desc {
				return all[i].AssetID > all[j].AssetID
			}
			return all[i].AssetID < all[j].AssetID
		})
	}
	total := len(all)
	if lim := c.F.IntPtr("limit"); lim != nil && *lim > 0 {
		page := c.F.Int("page", 1)
		if page < 1 {
			page = 1
		}
		start := int((page - 1) * *lim)
		if start > len(all) {
			start = len(all)
		}
		end := start + int(*lim)
		if end > len(all) {
			end = len(all)
		}
		all = all[start:end]
	}
	for _, r := range all {
		r.Count = total
	}
	return emptyIfNil(all), nil
}

func filterRows[T any](rows []T, keep func(T) bool) []T {
	out := rows[:0]
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) locModels(c *Ctx) (any, error) {
	ids, err := s.locSubtreeAssets(c, c.F.Int("location_id", 0))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT DISTINCT m.id, m.name FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id
		WHERE e.id = ANY($1) ORDER BY m.name`, ids)
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
		out = append(out, map[string]any{"modelId": id, "name": name})
	}
	return out, rows.Err()
}

func (s *Server) locBrands(c *Ctx) (any, error) {
	ids := httpx.ParseIDList(c.F.Str("model_ids"))
	names, err := db.Strings(c, s.DB, `SELECT DISTINCT b.name FROM asset.models m JOIN asset.brands b ON b.id = m.brand_id
		WHERE m.id = ANY($1) ORDER BY b.name`, ids)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, n := range names {
		out = append(out, map[string]any{"brand": n})
	}
	return out, nil
}

func (s *Server) locLicenses(c *Ctx) (any, error) {
	ids := httpx.ParseIDList(c.F.Str("asset_ids"))
	names, err := db.Strings(c, s.DB, `SELECT DISTINCT COALESCE(l.name, '-') FROM asset.equipment e LEFT JOIN core.licenses l ON l.id = e.license_id
		WHERE e.id = ANY($1) ORDER BY 1`, ids)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, n := range names {
		out = append(out, map[string]any{"license": n})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Location overview
// ---------------------------------------------------------------------------

func (s *Server) loAllLocations(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id, availability_th, performance_th, quality_th FROM asset.locations`)
	if err != nil {
		return nil, err
	}
	th := map[int64][3]float64{}
	for rows.Next() {
		var id int64
		var a, p, q float64
		if err := rows.Scan(&id, &a, &p, &q); err != nil {
			rows.Close()
			return nil, err
		}
		th[id] = [3]float64{a, p, q}
	}
	rows.Close()
	out := []map[string]any{}
	var walk func(id int64)
	walk = func(id int64) {
		n := tree.nodes[id]
		if scope.AllowsLocation(id) {
			out = append(out, map[string]any{
				"locationId": id, "locationName": n.Name, "parentId": n.ParentID,
				"availabilityTh": th[id][0], "performanceTh": th[id][1], "qualityTh": th[id][2],
				"locationImagePath": derefStr(imageURL(n.Image)), "hierarchy": tree.depth(id),
			})
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	for _, r := range tree.roots {
		walk(r)
	}
	return out, nil
}

type locStats struct {
	total, running, idle, planned, unplanned, offline, normal, warning, critical int
}

// subtreeStats counts current statuses/conditions of all visible equipment
// under each location.
func (s *Server) subtreeStats(c *Ctx, tree *locTree, locIDs []int64) (map[int64]*locStats, error) {
	byLoc, all, err := s.visibleEquipment(c)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, all)
	if err != nil {
		return nil, err
	}
	out := map[int64]*locStats{}
	for _, l := range locIDs {
		out[l] = countStates(assetsUnder(tree, byLoc, l), states)
	}
	return out, nil
}

// visibleEquipment returns the equipment the user may see, by location.
func (s *Server) visibleEquipment(c *Ctx) (byLoc map[int64][]int64, all []int64, err error) {
	scope, err := c.Scope()
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.DB.Query(c, `SELECT e.id, e.location_id FROM asset.equipment e`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byLoc = map[int64][]int64{}
	for rows.Next() {
		var id, loc int64
		if err := rows.Scan(&id, &loc); err != nil {
			return nil, nil, err
		}
		if scope.AllowsLocation(loc) {
			byLoc[loc] = append(byLoc[loc], id)
			all = append(all, id)
		}
	}
	return byLoc, all, rows.Err()
}

// assetsUnder lists the equipment in the subtrees of the given locations.
func assetsUnder(tree *locTree, byLoc map[int64][]int64, locIDs ...int64) []int64 {
	var out []int64
	for d := range tree.descendants(locIDs) {
		out = append(out, byLoc[d]...)
	}
	return out
}

// countStates counts the current statuses and conditions of equipment.
func countStates(assets []int64, states map[int64]*currentState) *locStats {
	st := &locStats{}
	for _, a := range assets {
		st.total++
		cs := states[a]
		switch {
		case cs == nil || cs.Status == 0 || cs.Status == engine.StatusOffline:
			st.offline++
		case cs.Status == engine.StatusRunning:
			st.running++
		case cs.Status == engine.StatusIdle:
			st.idle++
		case cs.Status == engine.StatusPlannedDown:
			st.planned++
		case cs.Status == engine.StatusUnplannedDown:
			st.unplanned++
		}
		if cs != nil {
			switch cs.Condition {
			case engine.CondNormal:
				st.normal++
			case engine.CondWarning:
				st.warning++
			case engine.CondCritical:
				st.critical++
			}
		}
	}
	return st
}

// childLocations returns the visible children of a location (without one:
// the top-most visible locations) whose name contains search, by name.
func childLocations(tree *locTree, scope *auth.Scope, id *int64, search string) []int64 {
	var children []int64
	if id != nil && tree.nodes[*id] != nil {
		children = tree.nodes[*id].Children
	} else {
		// Top-most visible locations.
		var find func(id int64)
		find = func(id int64) {
			if scope.AllowsLocation(id) {
				children = append(children, id)
				return
			}
			for _, ch := range tree.nodes[id].Children {
				find(ch)
			}
		}
		for _, r := range tree.roots {
			find(r)
		}
	}
	search = strings.ToLower(search)
	var visible []int64
	for _, id := range children {
		if !scope.AllowsLocation(id) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(tree.name(id)), search) {
			continue
		}
		visible = append(visible, id)
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return strings.ToLower(tree.name(visible[i])) < strings.ToLower(tree.name(visible[j]))
	})
	return visible
}

func (s *Server) loDirectChildren(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	visible := childLocations(tree, scope, c.F.IntPtr("location_id"), c.F.StrTrim("search_filter"))
	total := len(visible)
	const pageSize = 6
	page := c.page(1)
	start := int((page - 1) * pageSize)
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageIDs := visible[start:end]
	stats, err := s.subtreeStats(c, tree, pageIDs)
	if err != nil {
		return nil, err
	}
	th := map[int64][3]float64{}
	if len(pageIDs) > 0 {
		rows, err := s.DB.Query(c, `SELECT id, availability_th, performance_th, quality_th FROM asset.locations WHERE id = ANY($1)`, pageIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var a, p, q float64
			if err := rows.Scan(&id, &a, &p, &q); err != nil {
				rows.Close()
				return nil, err
			}
			th[id] = [3]float64{a, p, q}
		}
		rows.Close()
	}
	data := []map[string]any{}
	for _, id := range pageIDs {
		st := stats[id]
		n := tree.nodes[id]
		data = append(data, map[string]any{
			"availabilityTh": th[id][0], "performanceTh": th[id][1], "qualityTh": th[id][2],
			"locationId": id, "locationImagePath": derefStr(imageURL(n.Image)), "locationName": n.Name,
			"totalAsset": st.total, "totalCriticalAsset": st.critical, "totalIdleAsset": st.idle, "totalNormalAsset": st.normal,
			"totalPlannedDownAsset": st.planned, "totalRunningAsset": st.running, "totalUnplannedDownAsset": st.unplanned,
			"totalOfflineAsset": st.offline, "totalWarningAsset": st.warning, "isLastChild": len(n.Children) == 0,
		})
	}
	return map[string]any{"count": total, "data": data}, nil
}

// locationAPQ computes availability / uptime of the equipment below a
// location over a period. Performance and quality are the rate and quality
// efficiency of the process tools below the location (production records);
// null where there are none.
func (s *Server) locationAPQ(c *Ctx, tree *locTree, locID *int64, from, to time.Time) (map[string]any, error) {
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	var locs []int64
	if locID != nil && tree.nodes[*locID] != nil {
		for id := range tree.descendants([]int64{*locID}) {
			if scope.AllowsLocation(id) {
				locs = append(locs, id)
			}
		}
	} else {
		for id := range tree.nodes {
			if scope.AllowsLocation(id) {
				locs = append(locs, id)
			}
		}
	}
	assets, err := db.Int64s(c, s.DB, `SELECT id FROM asset.equipment WHERE location_id = ANY($1)`, locs)
	if err != nil {
		return nil, err
	}
	durs, err := s.statusDurations(c, assets, from, to)
	if err != nil {
		return nil, err
	}
	var running, planned, total float64
	span := elapsed(from, to)
	for _, a := range assets {
		d := durs[a]
		running += d[engine.StatusRunning]
		planned += d[engine.StatusPlannedDown]
		total += span
	}
	var availability, uptime *float64
	if total > 0 {
		u := round2(running / total * 100)
		uptime = &u
		if total-planned > 0 {
			a := round2(running / (total - planned) * 100)
			if a > 100 {
				a = 100
			}
			availability = &a
		}
	}
	var availTh, perfTh, qualTh float64
	if locID != nil {
		_ = s.DB.QueryRow(c, `SELECT availability_th, performance_th, quality_th FROM asset.locations WHERE id = $1`, *locID).Scan(&availTh, &perfTh, &qualTh)
	}
	perf, quality := s.toolPerfQuality(c, assets, from, to)
	return map[string]any{
		"totalAvailability": availability, "totalUptime": uptime, "totalPerformance": perf, "totalQuality": quality,
		"availabilityGood": availability == nil || *availability >= availTh,
		"performanceGood":  perf == nil || *perf >= perfTh, "qualityGood": quality == nil || *quality >= qualTh,
	}, nil
}

func (s *Server) loAPQ(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start", "end", true, 7)
	return s.locationAPQ(c, tree, c.F.IntPtr("location_id"), from, to)
}

func (s *Server) loAncestors(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	id := c.F.IntPtr("location_id")
	if id == nil {
		return []int64{}, nil
	}
	return emptyIfNil(tree.ancestors(*id)), nil
}

func (s *Server) loExport(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	from, to := c.dateRange("start", "end", true, 7)
	var roots []int64
	if id := c.F.IntPtr("location_id"); id != nil && tree.nodes[*id] != nil {
		roots = []int64{*id}
	} else {
		roots = tree.roots
	}
	var ids []int64
	var walk func(id int64)
	walk = func(id int64) {
		if scope.AllowsLocation(id) {
			ids = append(ids, id)
		}
		for _, ch := range tree.nodes[id].Children {
			walk(ch)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	stats, err := s.subtreeStats(c, tree, ids)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, id := range ids {
		lid := id
		apq, err := s.locationAPQ(c, tree, &lid, from, to)
		if err != nil {
			return nil, err
		}
		var a, p, q float64
		_ = s.DB.QueryRow(c, `SELECT availability_th, performance_th, quality_th FROM asset.locations WHERE id = $1`, id).Scan(&a, &p, &q)
		st := stats[id]
		n := tree.nodes[id]
		out = append(out, map[string]any{
			"availabilityGood": apq["availabilityGood"], "availabilityTh": a, "hierarchyLevel": tree.depth(id), "id": id,
			"name": n.Name, "parentId": n.ParentID, "performanceGood": apq["performanceGood"], "performanceTh": p, "qualityGood": apq["qualityGood"], "qualityTh": q,
			"totalAssets": st.total, "totalAvailability": apq["totalAvailability"], "totalCritical": st.critical, "totalIdle": st.idle,
			"totalNormal": st.normal, "totalOffline": st.offline, "totalPerformance": apq["totalPerformance"], "totalPlannedDown": st.planned,
			"totalQuality": apq["totalQuality"], "totalRunning": st.running, "totalUnplannedDown": st.unplanned, "totalUptime": apq["totalUptime"],
			"totalWarning": st.warning,
		})
	}
	return out, nil
}

var _ = pgx.ErrNoRows
