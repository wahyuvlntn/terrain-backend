package api

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

func (s *Server) registerPersonnel() {
	p := "personnel_management/"
	s.handle(p+"get_all_buildings", AccessUser, s.pmBuildings)
	s.handle(p+"get_all_equipment_types", AccessUser, s.pmEquipmentTypes)
	s.handle(p+"get_personnel_list", AccessUser, s.pmList)
	s.handle(p+"get_personnel_details", AccessUser, s.pmDetails)
	s.handle(p+"update_personnel_details", AccessUser, s.pmUpdate)
}

func (s *Server) pmBuildings(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, id := range buildingIDs(tree, s.cfg.BuildingLevel) {
		out = append(out, map[string]any{"id": strconv.FormatInt(id, 10), "name": tree.name(id)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i]["name"].(string)) < strings.ToLower(out[j]["name"].(string))
	})
	return out, nil
}

func (s *Server) pmEquipmentTypes(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT id::text, name FROM asset.types ORDER BY order_id, lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name})
	}
	return out, rows.Err()
}

type personnel struct {
	ID        int64
	Username  string
	Name      string
	Photo     *string
	Deleted   bool
	CreatedAt time.Time
	UpdatedAt *time.Time
	Buildings []int64
	Types     []int64
	Current   *string
	Capacity  float64
}

func (s *Server) loadPersonnel(ctx context.Context, from, to time.Time) ([]*personnel, error) {
	rows, err := s.DB.Query(ctx, `SELECT u.id, u.username, COALESCE(NULLIF(u.full_name, ''), u.username), u.photo, u.is_deleted,
			COALESCE(p.created_at, u.created_at), COALESCE(p.updated_at, u.updated_at)
		FROM core.users u LEFT JOIN dm.personnel p ON p.user_id = u.id
		WHERE u.id IN (`+personnelSQL+`) ORDER BY u.is_deleted, lower(u.full_name)`)
	if err != nil {
		return nil, err
	}
	var list []*personnel
	byID := map[int64]*personnel{}
	for rows.Next() {
		p := &personnel{}
		if err := rows.Scan(&p.ID, &p.Username, &p.Name, &p.Photo, &p.Deleted, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, p)
		byID[p.ID] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	rows, err = s.DB.Query(ctx, `SELECT user_id, location_id FROM dm.personnel_buildings WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u, l int64
		if err := rows.Scan(&u, &l); err != nil {
			rows.Close()
			return nil, err
		}
		byID[u].Buildings = append(byID[u].Buildings, l)
	}
	rows.Close()
	rows, err = s.DB.Query(ctx, `SELECT user_id, type_id FROM dm.personnel_equipment_types WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u, t int64
		if err := rows.Scan(&u, &t); err != nil {
			rows.Close()
			return nil, err
		}
		byID[u].Types = append(byID[u].Types, t)
	}
	rows.Close()
	// Current order: an in-progress/paused order assigned to the technician.
	rows, err = s.DB.Query(ctx, `SELECT DISTINCT ON (t.user_id) t.user_id, o.name FROM dm.maintenance_order_technicians t
		JOIN dm.maintenance_orders o ON o.id = t.order_id WHERE t.user_id = ANY($1) AND o.status_id IN (4, 7)
		ORDER BY t.user_id, o.started_at DESC NULLS LAST`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u int64
		var name string
		if err := rows.Scan(&u, &name); err != nil {
			rows.Close()
			return nil, err
		}
		byID[u].Current = &name
	}
	rows.Close()
	// Capacity: planned workload vs 8 working hours a day in the period.
	rows, err = s.DB.Query(ctx, `SELECT t.user_id, sum(COALESCE(NULLIF((SELECT sum(i.duration) * 60 FROM dm.maintenance_order_issues moi
				JOIN dm.instructions i ON i.alarm_code_id = moi.alarm_code_id AND NOT i.is_deleted
				JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = e.model_id
				WHERE moi.order_id = o.id AND NOT moi.is_other), 0),
			EXTRACT(EPOCH FROM (o.planned_end - o.planned_start)), 0))
		FROM dm.maintenance_order_technicians t JOIN dm.maintenance_orders o ON o.id = t.order_id JOIN asset.equipment e ON e.id = o.asset_id
		WHERE t.user_id = ANY($1) AND o.status_id <> 2 AND o.planned_start >= $2 AND o.planned_start < $3 GROUP BY t.user_id`, ids, from, to)
	if err != nil {
		return nil, err
	}
	days := math.Ceil(to.Sub(from).Hours() / 24)
	if days < 1 {
		days = 1
	}
	for rows.Next() {
		var u int64
		var secs *float64
		if err := rows.Scan(&u, &secs); err != nil {
			rows.Close()
			return nil, err
		}
		if secs != nil {
			byID[u].Capacity = math.Min(100, math.Round(*secs/(days*8*3600)*1000)/10)
		}
	}
	rows.Close()
	return list, nil
}

func capacityBucket(v float64) string {
	switch {
	case v >= 80:
		return "80_100"
	case v >= 60:
		return "60_80"
	case v >= 40:
		return "40_60"
	case v >= 20:
		return "20_40"
	default:
		return "0_20"
	}
}

func jsonStringList(c *Ctx, key string) ([]string, bool) {
	var list []any
	ok, err := c.F.JSON(key, &list)
	if err != nil || !ok {
		return nil, false
	}
	out := []string{}
	for _, v := range list {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case float64:
			out = append(out, strconv.FormatInt(int64(t), 10))
		}
	}
	return out, true
}

func (s *Server) pmList(c *Ctx) (any, error) {
	if err := c.Require("personnelmanagement", "read"); err != nil {
		return nil, err
	}
	loc := c.Location()
	now := time.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -7)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	if t := c.F.TimePtr("start", loc); t != nil {
		from = *t
	}
	if t := c.F.TimePtr("end", loc); t != nil {
		to = t.Add(24 * time.Hour)
	}
	list, err := s.loadPersonnel(c, from, to)
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	typeNames := map[int64]string{}
	trows, err := s.DB.Query(c, `SELECT id, name FROM asset.types`)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var id int64
		var name string
		if err := trows.Scan(&id, &name); err != nil {
			trows.Close()
			return nil, err
		}
		typeNames[id] = name
	}
	trows.Close()

	buildings, bPresent := jsonStringList(c, "buildingIds")
	types, tPresent := jsonStringList(c, "equipmentTypeIds")
	caps, cPresent := jsonStringList(c, "capacityIds")
	bset, tset, cset := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range buildings {
		bset[v] = true
	}
	for _, v := range types {
		tset[v] = true
	}
	for _, v := range caps {
		cset[v] = true
	}
	search := strings.ToLower(c.F.StrTrim("search"))
	var filtered []*personnel
	for _, p := range list {
		if bPresent {
			match := false
			for _, b := range p.Buildings {
				if bset[strconv.FormatInt(b, 10)] {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		if tPresent {
			match := false
			for _, t := range p.Types {
				if tset[strconv.FormatInt(t, 10)] {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		if cPresent && !cset[capacityBucket(p.Capacity)] {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(p.Name), search) && !strings.Contains(strings.ToLower(p.Username), search) {
			continue
		}
		filtered = append(filtered, p)
	}
	page, limit := c.page(1), c.limit(10, 500)
	entries := []map[string]any{}
	for _, p := range paginate(filtered, page, limit) {
		var bnames, tnames []string
		for _, b := range p.Buildings {
			bnames = append(bnames, tree.name(b))
		}
		for _, t := range p.Types {
			tnames = append(tnames, typeNames[t])
		}
		sort.Strings(bnames)
		sort.Strings(tnames)
		entries = append(entries, map[string]any{
			"id": p.Username, "name": p.Name, "image": imageURL(p.Photo), "assignedBuildings": nilIfEmpty(strings.Join(bnames, ", ")),
			"assignedEquipmentTypes": nilIfEmpty(strings.Join(tnames, ", ")), "currentMaintenanceOrder": p.Current,
			"capacityPercentage": p.Capacity, "isDeleted": p.Deleted,
		})
	}
	return map[string]any{"total": len(filtered), "entries": entries}, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) personnelByUsername(c *Ctx, username string) (*personnel, error) {
	list, err := s.loadPersonnel(c, time.Now().AddDate(0, 0, -7), time.Now())
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if strings.EqualFold(p.Username, username) && !p.Deleted {
			return p, nil
		}
	}
	for _, p := range list {
		if strings.EqualFold(p.Username, username) {
			return p, nil
		}
	}
	return nil, httpx.NotFound("Personnel not found")
}

func (s *Server) pmDetails(c *Ctx) (any, error) {
	if err := c.Require("personnelmanagement", "read"); err != nil {
		return nil, err
	}
	p, err := s.personnelByUsername(c, c.F.StrTrim("id"))
	if err != nil {
		return nil, err
	}
	bids, tids := []string{}, []string{}
	for _, b := range p.Buildings {
		bids = append(bids, strconv.FormatInt(b, 10))
	}
	for _, t := range p.Types {
		tids = append(tids, strconv.FormatInt(t, 10))
	}
	var updated any
	if p.UpdatedAt != nil {
		updated = p.UpdatedAt.UnixMilli()
	}
	return map[string]any{
		"id": p.Username, "name": p.Name, "assignedBuildingIds": bids, "assignedEquipmentTypeIds": tids,
		"currentMaintenanceOrder": p.Current, "createdAt": p.CreatedAt.UnixMilli(), "updatedAt": updated, "isDeleted": p.Deleted,
	}, nil
}

func (s *Server) pmUpdate(c *Ctx) (any, error) {
	if err := c.Require("personnelmanagement", "update"); err != nil {
		return nil, err
	}
	p, err := s.personnelByUsername(c, c.F.StrTrim("id"))
	if err != nil {
		return nil, err
	}
	if p.Deleted {
		return nil, httpx.BadRequest("Deleted personnel cannot be updated")
	}
	bids, _ := jsonStringList(c, "assignedBuildingIds")
	tids, _ := jsonStringList(c, "assignedEquipmentTypeIds")
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `INSERT INTO dm.personnel (user_id) VALUES ($1) ON CONFLICT (user_id) DO UPDATE SET updated_at = now()`, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `DELETE FROM dm.personnel_buildings WHERE user_id = $1`, p.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `DELETE FROM dm.personnel_equipment_types WHERE user_id = $1`, p.ID); err != nil {
			return err
		}
		for _, b := range bids {
			id, err := strconv.ParseInt(b, 10, 64)
			if err != nil {
				continue
			}
			if _, err := tx.Exec(c, `INSERT INTO dm.personnel_buildings (user_id, location_id) SELECT $1, $2
				WHERE EXISTS (SELECT 1 FROM asset.locations WHERE id = $2) ON CONFLICT DO NOTHING`, p.ID, id); err != nil {
				return err
			}
		}
		for _, t := range tids {
			id, err := strconv.ParseInt(t, 10, 64)
			if err != nil {
				continue
			}
			if _, err := tx.Exec(c, `INSERT INTO dm.personnel_equipment_types (user_id, type_id) SELECT $1, $2
				WHERE EXISTS (SELECT 1 FROM asset.types WHERE id = $2) ON CONFLICT DO NOTHING`, p.ID, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return httpx.Key("personnel_details_successfully_updated", p.Name), nil
}

var _ = db.IsNoRows
