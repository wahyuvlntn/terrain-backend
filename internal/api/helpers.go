package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/license"
	"afdd/internal/maint"
	"afdd/internal/storage"
)

// fail converts domain errors into HTTP errors.
func fail(err error) error {
	if err == nil {
		return nil
	}
	var me *maint.Error
	if errors.As(err, &me) {
		return httpx.BadRequest(me.Msg)
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		return he
	}
	switch {
	case errors.Is(err, license.ErrInvalidFile), errors.Is(err, license.ErrInvalidSig),
		errors.Is(err, license.ErrExpired), errors.Is(err, license.ErrNotApplicable):
		return httpx.BadRequest(err.Error())
	}
	var ute *storage.UnsupportedTypeError
	if errors.As(err, &ute) {
		return httpx.BadRequest("err_supported_upload_image")
	}
	if db.IsUniqueViolation(err) {
		return httpx.BadRequest("Data already exists")
	}
	if db.IsForeignKeyViolation(err) {
		return httpx.BadRequest("Data is still referenced by other records")
	}
	return err
}

func (c *Ctx) actor() maint.Actor {
	return maint.Actor{ID: c.User.ID, Username: c.User.Username, FullName: c.User.FullName}
}

// ---------------------------------------------------------------------------
// Locations
// ---------------------------------------------------------------------------

type locNode struct {
	ID       int64
	ParentID *int64
	Name     string
	Image    *string
	Children []int64
}

type locTree struct {
	nodes map[int64]*locNode
	roots []int64
}

func (s *Server) loadLocations(ctx context.Context) (*locTree, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, parent_id, name, image_path FROM asset.locations ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &locTree{nodes: map[int64]*locNode{}}
	var order []int64
	for rows.Next() {
		n := &locNode{}
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Image); err != nil {
			return nil, err
		}
		t.nodes[n.ID] = n
		order = append(order, n.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range order {
		n := t.nodes[id]
		if n.ParentID != nil {
			if p := t.nodes[*n.ParentID]; p != nil {
				p.Children = append(p.Children, id)
				continue
			}
		}
		t.roots = append(t.roots, id)
	}
	return t, nil
}

// descendants returns ids and all of their descendants.
func (t *locTree) descendants(ids []int64) map[int64]bool {
	out := map[int64]bool{}
	stack := append([]int64(nil), ids...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[id] {
			continue
		}
		n := t.nodes[id]
		if n == nil {
			continue
		}
		out[id] = true
		stack = append(stack, n.Children...)
	}
	return out
}

// ancestors returns the chain from the root down to id (inclusive).
func (t *locTree) ancestors(id int64) []int64 {
	var chain []int64
	seen := map[int64]bool{}
	for cur := t.nodes[id]; cur != nil && !seen[cur.ID]; {
		seen[cur.ID] = true
		chain = append([]int64{cur.ID}, chain...)
		if cur.ParentID == nil {
			break
		}
		cur = t.nodes[*cur.ParentID]
	}
	return chain
}

func (t *locTree) depth(id int64) int { return len(t.ancestors(id)) - 1 }

// path returns "Root > Child > ... > Name".
func (t *locTree) path(id int64, sep string) string {
	var names []string
	for _, a := range t.ancestors(id) {
		names = append(names, t.nodes[a].Name)
	}
	return strings.Join(names, sep)
}

func (t *locTree) name(id int64) string {
	if n := t.nodes[id]; n != nil {
		return n.Name
	}
	return ""
}

// ---------------------------------------------------------------------------
// Asset filters
// ---------------------------------------------------------------------------

type assetFilter struct {
	locations    []int64
	locPresent   bool
	assets       []int64
	types        []int64
	typePresent  bool
	models       []int64
	modelPresent bool
}

func (c *Ctx) assetFilterKeys(locKey, assetKey, typeKey, modelKey string) assetFilter {
	var f assetFilter
	f.locations, f.locPresent = c.F.IDs(locKey)
	f.assets, _ = c.F.IDs(assetKey)
	if typeKey != "" {
		f.types, f.typePresent = c.F.IDs(typeKey)
	}
	if modelKey != "" {
		f.models, f.modelPresent = c.F.IDs(modelKey)
	}
	return f
}

// assetFilter reads the standard location_ids/asset_ids/type_ids/model_ids.
func (c *Ctx) assetFilter() assetFilter {
	return c.assetFilterKeys("location_ids", "asset_ids", "type_ids", "model_ids")
}

// filterFromJSON reads the JSON "filter" parameter used by maintenance pages.
type jsonFilter struct {
	LocationIDs  *[]int64 `json:"locationIds"`
	AssetIDs     []int64  `json:"assetIds"`
	TypeIDs      *[]int64 `json:"typeIds"`
	ModelIDs     *[]int64 `json:"modelIds"`
	StatusIDs    *[]any   `json:"maintenanceStatusIds"`
	SeverityIDs  *[]any   `json:"severityIds"`
	PriorityIDs  *[]any   `json:"priorityIds"`
	TypeMaintIDs *[]any   `json:"maintenanceTypeIds"`
	Technicians  *[]any   `json:"technicianIds"`
}

func anyInts(p *[]any) ([]int64, bool) {
	if p == nil {
		return nil, false
	}
	out := []int64{}
	for _, v := range *p {
		switch t := v.(type) {
		case float64:
			out = append(out, int64(t))
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, true
}

func anyStrings(p *[]any) ([]string, bool) {
	if p == nil {
		return nil, false
	}
	out := []string{}
	for _, v := range *p {
		out = append(out, fmt.Sprint(v))
	}
	return out, true
}

func (c *Ctx) jsonFilter(key string) (*jsonFilter, assetFilter, error) {
	jf := &jsonFilter{}
	if _, err := c.F.JSON(key, jf); err != nil {
		return nil, assetFilter{}, httpx.BadRequest("Invalid filter")
	}
	af := assetFilter{assets: jf.AssetIDs}
	if jf.LocationIDs != nil {
		af.locations, af.locPresent = *jf.LocationIDs, true
	}
	if jf.TypeIDs != nil {
		af.types, af.typePresent = *jf.TypeIDs, true
	}
	if jf.ModelIDs != nil {
		af.models, af.modelPresent = *jf.ModelIDs, true
	}
	return jf, af, nil
}

// resolveAssets returns the ids of the equipment matching the filter and
// visible to the user, sorted ascending.
func (s *Server) resolveAssets(c *Ctx, f assetFilter) ([]int64, error) {
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	all := !f.locPresent
	var locSet map[int64]bool
	if !all {
		locSet = tree.descendants(f.locations)
	}
	assetSet := map[int64]bool{}
	for _, a := range f.assets {
		assetSet[a] = true
	}
	typeSet := map[int64]bool{}
	for _, t := range f.types {
		typeSet[t] = true
	}
	modelSet := map[int64]bool{}
	for _, m := range f.models {
		modelSet[m] = true
	}
	rows, err := s.DB.Query(c, `SELECT e.id, e.location_id, e.model_id, m.type_id FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id, loc, model, typ int64
		if err := rows.Scan(&id, &loc, &model, &typ); err != nil {
			return nil, err
		}
		if !scope.AllowsLocation(loc) {
			continue
		}
		if !all && !locSet[loc] && !assetSet[id] {
			continue
		}
		if f.typePresent && !typeSet[typ] {
			continue
		}
		if f.modelPresent && !modelSet[model] {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, rows.Err()
}

// visibleAsset verifies that the user may access an equipment.
func (s *Server) visibleAsset(c *Ctx, assetID int64) error {
	var loc int64
	if err := s.DB.QueryRow(c, `SELECT location_id FROM asset.equipment WHERE id = $1`, assetID).Scan(&loc); err != nil {
		if db.IsNoRows(err) {
			return httpx.NotFound("err_asset_not_found")
		}
		return err
	}
	scope, err := c.Scope()
	if err != nil {
		return err
	}
	if !scope.AllowsLocation(loc) {
		return httpx.Forbidden("")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------

// timeParam parses a timestamp/date parameter in the client's time zone.
func (c *Ctx) timeParam(keys ...string) *time.Time {
	loc := c.Location()
	for _, k := range keys {
		if t := c.F.TimePtr(k, loc); t != nil {
			return t
		}
	}
	return nil
}

// dayRange returns [start of date, +24h) in the client time zone.
func (c *Ctx) dayRange(key string) (time.Time, time.Time) {
	loc := c.Location()
	s := strings.TrimSpace(c.F.Str(key))
	var day time.Time
	if len(s) >= 10 {
		if d, err := time.ParseInLocation("2006-01-02", s[:10], loc); err == nil {
			day = d
		}
	}
	if day.IsZero() {
		now := time.Now().In(loc)
		day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	}
	return day.UTC(), day.AddDate(0, 0, 1).UTC()
}

// dateRange reads a start/end pair. Date-only values are local midnights;
// when inclusiveEnd is true the end date is extended by one day.
func (c *Ctx) dateRange(startKey, endKey string, inclusiveEnd bool, defaultDays int) (time.Time, time.Time) {
	loc := c.Location()
	now := time.Now()
	end := c.timeParam(endKey)
	start := c.timeParam(startKey)
	var e time.Time
	if end == nil {
		n := now.In(loc)
		e = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1).UTC()
	} else {
		e = *end
		if inclusiveEnd {
			e = e.Add(24 * time.Hour)
		}
	}
	var st time.Time
	if start == nil {
		st = e.AddDate(0, 0, -defaultDays)
	} else {
		st = *start
	}
	if !e.After(st) {
		e = st.Add(24 * time.Hour)
	}
	return st, e
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

func (c *Ctx) page(def int64) int64 {
	p := c.F.Int("page", def)
	if p < 1 {
		p = 1
	}
	return p
}

func (c *Ctx) limit(def, max int64) int64 {
	l := c.F.Int("limit", def)
	if l < 1 {
		l = def
	}
	if max > 0 && l > max {
		l = max
	}
	return l
}

func ilike(search string) string {
	search = strings.TrimSpace(search)
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(search) + "%"
}

func hours(d time.Duration) float64 { return httpx.Round(d.Hours(), 2) }

func round1(f float64) float64 { return httpx.Round(f, 1) }

func round2(f float64) float64 { return httpx.Round(f, 2) }

func pct(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return httpx.Round(part/total*100, 2)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func nz(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// photoURL formats a stored user photo the way the layout expects
// ("public/..." without a leading slash).
func photoURL(p *string) string {
	if p == nil || *p == "" {
		return ""
	}
	return strings.TrimPrefix(*p, "/")
}

// imageURL formats stored images as "/public/..." paths.
func imageURL(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	v := *p
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	return &v
}

func int64Set(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func uniqueInt64(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// emptyIfNil returns an empty slice for nil slices so JSON shows [].
func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// sortSpec reads the "sort_state" JSON (table column definitions) and
// returns the key of the first column with a defined sort state.
func (c *Ctx) sortSpec(def string, defAsc bool) (string, bool) {
	var cols []struct {
		Key       string `json:"key"`
		SortState *bool  `json:"sortState"`
	}
	if _, err := c.F.JSON("sort_state", &cols); err == nil {
		for _, col := range cols {
			if col.SortState != nil {
				return col.Key, *col.SortState
			}
		}
	}
	return def, defAsc
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func strconvFormat(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

func asUnsupported(err error, t **storage.UnsupportedTypeError) bool { return errors.As(err, t) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }
