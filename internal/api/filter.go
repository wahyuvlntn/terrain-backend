package api

import (
	"strconv"
	"strings"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

func (s *Server) registerFilter() {
	// database-connection named queries
	s.handleDB("filter/getalltypes", AccessUser, s.listTable(`SELECT id, name FROM asset.types ORDER BY order_id, lower(name)`))
	s.handleDB("filter/getallmodels", AccessUser, s.listTable(`SELECT id, name FROM asset.models ORDER BY lower(name)`))
	s.handleDB("filter/getallbrands", AccessUser, s.listTable(`SELECT id, name FROM asset.brands ORDER BY order_id, lower(name)`))
	s.handleDB("filter/getallseries", AccessUser, s.listTable(`SELECT id, name FROM asset.series ORDER BY order_id, lower(name)`))
	s.handleDB("filter/getallseverities", AccessUser, s.listTable(`SELECT id, initcap(name) AS name FROM dm.severities ORDER BY id`))
	s.handleDB("filter/getallpriorities", AccessUser, s.listTable(`SELECT id, initcap(name) AS name FROM dm.priorities ORDER BY id`))
	s.handleDB("filter/getallmaintenancetypes", AccessUser, s.listTable(`SELECT id, initcap(name) AS name FROM dm.maintenance_types ORDER BY id`))
	s.handleDB("misc/getlastparamts", AccessUser, s.miscLastParamTS)

	// js-service
	s.handle("filter/getalltypes", AccessUser, s.listTable(`SELECT id, name FROM asset.types ORDER BY order_id, lower(name)`))
	s.handle("filter/getallmodels", AccessUser, s.listTable(`SELECT id, name FROM asset.models ORDER BY lower(name)`))
	s.handle("filter/getalllocationsandassets", AccessUser, s.filterLocationsAndAssets)
	s.handle("filter/getlocationtreeinput", AccessUser, s.filterLocationTreeInput)
	s.handle("filter/getlocationassetnames", AccessUser, s.filterLocationAssetNames)
	s.handle("filter/getfilternames", AccessUser, s.filterNames)
	s.handle("filter/gettypenames", AccessUser, s.namesByIDs(`SELECT name FROM asset.types WHERE id = ANY($1) ORDER BY order_id, lower(name)`))
	s.handle("filter/getmodelnames", AccessUser, s.namesByIDs(`SELECT name FROM asset.models WHERE id = ANY($1) ORDER BY lower(name)`))
	s.handle("filter/getbrandnames", AccessUser, s.namesByIDs(`SELECT name FROM asset.brands WHERE id = ANY($1) ORDER BY order_id, lower(name)`))
	s.handle("filter/getseriesnames", AccessUser, s.namesByIDs(`SELECT name FROM asset.series WHERE id = ANY($1) ORDER BY order_id, lower(name)`))
	s.handle("filter/getseveritynames", AccessUser, s.namesByIDs(`SELECT initcap(name) FROM dm.severities WHERE id = ANY($1) ORDER BY id`))
	s.handle("filter/getprioritynames", AccessUser, s.namesByIDs(`SELECT initcap(name) FROM dm.priorities WHERE id = ANY($1) ORDER BY id`))
	s.handle("filter/getmaintenancetypenames", AccessUser, s.namesByIDs(`SELECT initcap(name) FROM dm.maintenance_types WHERE id = ANY($1) ORDER BY id`))
	s.handle("filter/getalltechnicians", AccessUser, s.filterAllTechnicians)
	s.handle("filter/gettechniciannames", AccessUser, s.filterTechnicianNames)
}

// listTable returns rows as [{id, name}].
func (s *Server) listTable(sql string) HandlerFunc {
	return func(c *Ctx) (any, error) {
		rows, err := s.DB.Query(c, sql)
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
}

func (s *Server) namesByIDs(sql string) HandlerFunc {
	return func(c *Ctx) (any, error) {
		ids, _ := c.F.IDs("ids")
		if len(ids) == 0 {
			return []string{}, nil
		}
		return db.Strings(c, s.DB, sql, ids)
	}
}

func (s *Server) miscLastParamTS(c *Ctx) (any, error) {
	var ts *string
	err := s.DB.QueryRow(c, `SELECT to_char(max(ts) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') FROM ts.parameter_values
		WHERE ts > now() - interval '30 days'`).Scan(&ts)
	if err != nil {
		return nil, err
	}
	if ts == nil {
		// Fall back to the last status evaluation.
		_ = s.DB.QueryRow(c, `SELECT to_char(max(start_ts) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') FROM ts.asset_status`).Scan(&ts)
	}
	return []map[string]any{{"last_ts": ts}}, nil
}

func (s *Server) filterLocationsAndAssets(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	assets := map[int64][]map[string]any{}
	rows, err := s.DB.Query(c, `SELECT id, name, location_id FROM asset.equipment ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, loc int64
		var name string
		if err := rows.Scan(&id, &name, &loc); err != nil {
			rows.Close()
			return nil, err
		}
		assets[loc] = append(assets[loc], map[string]any{"id": id, "name": name})
	}
	rows.Close()
	out := []map[string]any{}
	var walk func(id int64)
	walk = func(id int64) {
		n := tree.nodes[id]
		if scope.AllowsLocation(id) {
			var parent any
			if n.ParentID != nil && scope.AllowsLocation(*n.ParentID) {
				parent = *n.ParentID
			}
			out = append(out, map[string]any{"id": id, "parent_id": parent, "name": n.Name, "assets": emptyIfNil(assets[id])})
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

// treeJSON is the {id, name, childs} structure used by tree inputs.
type treeJSON struct {
	ID     int64       `json:"id"`
	Name   string      `json:"name"`
	Childs []*treeJSON `json:"childs,omitempty"`
}

// buildLocationTree builds visible subtrees, optionally filtered by search,
// excluding excludedID (and its descendants) and rooted at rootID.
func (s *Server) buildLocationTree(c *Ctx, search string, excludedID *int64, rootID *int64) ([]*treeJSON, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	search = strings.ToLower(strings.TrimSpace(search))
	var build func(id int64) *treeJSON
	build = func(id int64) *treeJSON {
		if excludedID != nil && id == *excludedID {
			return nil
		}
		n := tree.nodes[id]
		node := &treeJSON{ID: id, Name: n.Name}
		for _, ch := range n.Children {
			if cn := build(ch); cn != nil {
				node.Childs = append(node.Childs, cn)
			}
		}
		visible := scope.AllowsLocation(id)
		matches := search == "" || strings.Contains(strings.ToLower(n.Name), search)
		if !visible {
			if len(node.Childs) == 0 {
				return nil
			}
			return nil // children are lifted by the caller
		}
		if !matches && len(node.Childs) == 0 {
			return nil
		}
		return node
	}
	// Collect top-most visible nodes.
	var roots []int64
	if rootID != nil {
		if tree.nodes[*rootID] != nil {
			roots = []int64{*rootID}
		}
	} else {
		var find func(id int64)
		find = func(id int64) {
			if scope.AllowsLocation(id) {
				roots = append(roots, id)
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
	out := []*treeJSON{}
	for _, r := range roots {
		if n := build(r); n != nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// treeRows wraps trees as [{build_location_tree: "<json>"}] like the
// original stored procedure output.
func treeRows(trees []*treeJSON) []map[string]any {
	out := []map[string]any{}
	for _, t := range trees {
		out = append(out, map[string]any{"build_location_tree": toJSON(t)})
	}
	return out
}

func (s *Server) filterLocationTreeInput(c *Ctx) (any, error) {
	trees, err := s.buildLocationTree(c, c.F.Str("search_filter"), c.F.IntPtr("excluded_id"), nil)
	if err != nil {
		return nil, err
	}
	return treeRows(trees), nil
}

func (s *Server) filterLocationAssetNames(c *Ctx) (any, error) {
	locIDs, locPresent := c.F.IDs("location_ids")
	assetIDs, _ := c.F.IDs("asset_ids")
	out := []map[string]any{}
	if locPresent {
		locs := []map[string]any{}
		if len(locIDs) > 0 {
			rows, err := s.DB.Query(c, `SELECT id, name FROM asset.locations WHERE id = ANY($1) ORDER BY lower(name)`, locIDs)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err != nil {
					rows.Close()
					return nil, err
				}
				locs = append(locs, map[string]any{"id": id, "name": name})
			}
			rows.Close()
		}
		out = append(out, map[string]any{"is_location": true, "data": locs})
	}
	assets := []map[string]any{}
	if len(assetIDs) > 0 {
		rows, err := s.DB.Query(c, `SELECT id, name FROM asset.equipment WHERE id = ANY($1) ORDER BY lower(name)`, assetIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			assets = append(assets, map[string]any{"id": id, "name": name})
		}
		rows.Close()
	}
	out = append(out, map[string]any{"is_location": false, "data": assets})
	return out, nil
}

func (s *Server) filterNames(c *Ctx) (any, error) {
	type group struct {
		key, typ, sql string
	}
	groups := []group{
		{"location_ids", "location", `SELECT id, name FROM asset.locations WHERE id = ANY($1) ORDER BY lower(name)`},
		{"asset_ids", "asset", `SELECT id, name FROM asset.equipment WHERE id = ANY($1) ORDER BY lower(name)`},
		{"type_ids", "type", `SELECT id, name FROM asset.types WHERE id = ANY($1) ORDER BY lower(name)`},
		{"model_ids", "model", `SELECT id, name FROM asset.models WHERE id = ANY($1) ORDER BY lower(name)`},
		{"brand_ids", "brand", `SELECT id, name FROM asset.brands WHERE id = ANY($1) ORDER BY lower(name)`},
		{"series_ids", "series", `SELECT id, name FROM asset.series WHERE id = ANY($1) ORDER BY lower(name)`},
	}
	out := []map[string]any{}
	for _, g := range groups {
		ids, present := c.F.IDs(g.key)
		if !present {
			continue
		}
		rows, err := db.Rows(c, s.DB, g.sql, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": g.typ, "data": rows})
	}
	return out, nil
}

func (s *Server) filterAllTechnicians(c *Ctx) (any, error) {
	rows, err := s.DB.Query(c, `SELECT username, CASE WHEN is_deleted THEN full_name || ' (deleted)' ELSE full_name END
		FROM core.users WHERE id IN (`+personnelSQL+`) ORDER BY is_deleted, lower(full_name)`)
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
		if strings.TrimSpace(name) == "" {
			name = id
		}
		out = append(out, map[string]any{"id": id, "name": name})
	}
	return out, rows.Err()
}

func (s *Server) filterTechnicianNames(c *Ctx) (any, error) {
	list, _ := c.F.StringList("usernames")
	if len(list) == 0 {
		return []string{}, nil
	}
	return db.Strings(c, s.DB, `SELECT CASE WHEN full_name = '' THEN username ELSE full_name END FROM core.users
		WHERE lower(username) = ANY(SELECT lower(x) FROM unnest($1::text[]) x) ORDER BY lower(full_name)`, list)
}

// personnelSQL selects the users acting as maintenance personnel: users of
// groups granted the maintenance order "report" permission, plus anybody
// already assigned to an order (to keep history readable).
const personnelSQL = `SELECT u.id FROM core.users u
	JOIN core.user_group_permissions ugp ON ugp.user_group_id = u.user_group_id AND ugp.value
	JOIN core.permissions p ON p.id = ugp.permission_id AND p.alias = 'report'
	JOIN core.permission_groups g ON g.id = p.permission_group_id AND g.alias = 'maintenanceorder'
	WHERE u.role = 'basic'
	UNION SELECT user_id FROM dm.maintenance_order_technicians`

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

var _ = httpx.TS
