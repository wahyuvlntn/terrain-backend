package api

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/maint"
)

func (s *Server) registerPerformance() {
	p := "maintenance_performance/"
	s.handle(p+"get_maintenance_order_overview", AccessUser, s.mpOverview)
	s.handle(p+"get_maintenance_order_trend", AccessUser, s.mpTrend)
	s.handle(p+"get_top_frequent_issues", AccessUser, s.mpTopIssues)
	s.handle(p+"get_paginated_frequent_issues", AccessUser, s.mpPagedIssues)
	s.handle(p+"get_top_personnel_order", AccessUser, s.mpTopPersonnel)
	s.handle(p+"get_paginated_personnel_orders", AccessUser, s.mpPagedPersonnel)
	s.handle(p+"get_personnel_capacity_distribution", AccessUser, s.mpCapacityDistribution)
	s.handle(p+"get_mtbf_chart", AccessUser, s.mpMTBFChart)
	s.handle(p+"get_mttr_chart", AccessUser, s.mpMTTRChart)
	s.handle(p+"get_mtbf_average", AccessUser, s.mpMTBFAverage)
	s.handle(p+"get_mttr_average", AccessUser, s.mpMTTRAverage)
	s.handle(p+"get_mtbf_mttr_lastupdated", AccessUser, s.mpLastUpdated)
	s.handle(p+"get_paginated_mtbf_mttr", AccessUser, s.mpPagedMTBFMTTR)
}

// perfScope resolves equipment and the date range of a performance query.
func (s *Server) perfScope(c *Ctx, short bool) ([]int64, time.Time, time.Time, error) {
	var f assetFilter
	if short {
		f = c.assetFilterKeys("location", "asset", "asset_type", "asset_model")
	} else {
		f = c.assetFilter()
	}
	ids, err := s.resolveAssets(c, f)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	from, to := c.dateRange("start", "end", true, 30)
	return ids, from, to, nil
}

const perfOrderWhere = `o.asset_id = ANY($1) AND o.planned_start >= $2 AND o.planned_start < $3`

func (s *Server) mpOverview(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, false)
	if err != nil {
		return nil, err
	}
	var sql string
	switch strings.ToLower(c.F.StrTrim("group_by")) {
	case "priority":
		sql = `SELECT p.id, initcap(p.name), count(o.id) FROM dm.priorities p
			LEFT JOIN dm.maintenance_orders o ON o.priority_id = p.id AND ` + perfOrderWhere + ` GROUP BY p.id ORDER BY p.id`
	case "severity":
		sql = `SELECT sv.id, initcap(sv.name), count(o.id) FROM dm.severities sv
			LEFT JOIN dm.maintenance_orders o ON o.severity_id = sv.id AND ` + perfOrderWhere + ` GROUP BY sv.id ORDER BY sv.id`
	default:
		sql = `SELECT st.id, st.name, count(o.id) FROM dm.maintenance_statuses st
			LEFT JOIN dm.maintenance_orders o ON o.status_id = st.id AND ` + perfOrderWhere + ` GROUP BY st.id ORDER BY st.id`
	}
	rows, err := s.DB.Query(c, sql, ids, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		id    int64
		name  string
		count int64
	}
	var items []item
	var total int64
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.name, &it.count); err != nil {
			return nil, err
		}
		items = append(items, it)
		total += it.count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if strings.ToLower(c.F.StrTrim("group_by")) == "priority" {
		var unset int64
		if err := s.DB.QueryRow(c, `SELECT count(*) FROM dm.maintenance_orders o WHERE o.priority_id IS NULL AND `+perfOrderWhere, ids, from, to).Scan(&unset); err != nil {
			return nil, err
		}
		if unset > 0 {
			items = append(items, item{id: -1, name: "Not Set", count: unset})
			total += unset
		}
	}
	out := []map[string]any{}
	for _, it := range items {
		var id any = it.id
		if it.id < 0 {
			id = nil
		}
		p := "0"
		if total > 0 {
			p = fmt.Sprintf("%.0f", float64(it.count)/float64(total)*100)
		}
		out = append(out, map[string]any{"value": it.count, "id": id, "name": it.name, "percentage": p})
	}
	return out, nil
}

func (s *Server) mpTrend(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, false)
	if err != nil {
		return nil, err
	}
	_, off := time.Now().In(c.Location()).Zone()
	rows, err := s.DB.Query(c, `SELECT to_char(date_trunc('day', o.planned_start + make_interval(secs => $4)), 'YYYY-MM-DD'),
			count(*) FILTER (WHERE o.maintenance_type_id = 1), count(*) FILTER (WHERE o.maintenance_type_id = 2)
		FROM dm.maintenance_orders o WHERE `+perfOrderWhere+` GROUP BY 1`, ids, from, to, off)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string][2]int64{}
	for rows.Next() {
		var day string
		var cor, pre int64
		if err := rows.Scan(&day, &cor, &pre); err != nil {
			return nil, err
		}
		counts[day] = [2]int64{cor, pre}
	}
	loc := c.Location()
	out := []map[string]any{}
	cur := from.In(loc)
	cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, loc)
	for i := 0; cur.Before(to) && i < 3700; i++ {
		key := cur.Format("2006-01-02")
		v := counts[key]
		out = append(out, map[string]any{"day": key, "correctiveCount": v[0], "preventiveCount": v[1]})
		cur = cur.AddDate(0, 0, 1)
	}
	return out, nil
}

type issueCount struct {
	name  string
	count int64
}

func (s *Server) issueCounts(c *Ctx) ([]issueCount, error) {
	ids, from, to, err := s.perfScope(c, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ac.code || ' - ' || ac.name, count(DISTINCT o.id) FROM dm.maintenance_orders o
		JOIN dm.maintenance_order_issues i ON i.order_id = o.id AND NOT i.is_other
		JOIN dm.alarm_codes ac ON ac.id = i.alarm_code_id
		WHERE `+perfOrderWhere+` AND o.status_id <> 2 GROUP BY ac.id, ac.code, ac.name`, ids, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []issueCount
	for rows.Next() {
		var it issueCount
		if err := rows.Scan(&it.name, &it.count); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out, rows.Err()
}

func (s *Server) mpTopIssues(c *Ctx) (any, error) {
	items, err := s.issueCounts(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i, it := range items {
		if i >= 3 {
			break
		}
		out = append(out, map[string]any{"id": i + 1, "instructionName": it.name, "occurence": it.count})
	}
	return out, nil
}

func paginate[T any](items []T, page, limit int64) []T {
	if limit <= 0 {
		return items
	}
	start := int((page - 1) * limit)
	if start > len(items) {
		start = len(items)
	}
	end := start + int(limit)
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func (s *Server) mpPagedIssues(c *Ctx) (any, error) {
	items, err := s.issueCounts(c)
	if err != nil {
		return nil, err
	}
	search := strings.ToLower(c.F.StrTrim("search"))
	if search != "" {
		items = filterRows(items, func(it issueCount) bool { return strings.Contains(strings.ToLower(it.name), search) })
	}
	key, asc := c.sortSpec("occurence", false)
	sort.SliceStable(items, func(i, j int) bool {
		var less, equal bool
		if key == "instructionName" {
			a, b := strings.ToLower(items[i].name), strings.ToLower(items[j].name)
			less, equal = a < b, a == b
		} else {
			less, equal = items[i].count < items[j].count, items[i].count == items[j].count
		}
		if equal {
			return false
		}
		if asc {
			return less
		}
		return !less
	})
	page, limit := c.page(1), c.limit(13, 500)
	data := []map[string]any{}
	for i, it := range paginate(items, page, limit) {
		data = append(data, map[string]any{"id": int((page-1)*limit) + i + 1, "instructionName": it.name, "occurence": it.count})
	}
	return map[string]any{"count": len(items), "data": data}, nil
}

type personnelStat struct {
	username  string
	name      string
	orders    int64
	estimated float64 // seconds
	actual    float64 // seconds of completed orders
	planned   float64 // estimated seconds of completed orders
	capacity  float64
}

// personnelStats aggregates maintenance workload per technician.
func (s *Server) personnelStats(c *Ctx) ([]*personnelStat, error) {
	ids, from, to, err := s.perfScope(c, false)
	if err != nil {
		return nil, err
	}
	stats := map[int64]*personnelStat{}
	rows, err := s.DB.Query(c, `SELECT u.id, u.username, COALESCE(NULLIF(u.full_name, ''), u.username) FROM core.users u
		WHERE u.id IN (`+personnelSQL+`) AND NOT u.is_deleted`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		p := &personnelStat{}
		if err := rows.Scan(&id, &p.username, &p.name); err != nil {
			rows.Close()
			return nil, err
		}
		stats[id] = p
	}
	rows.Close()

	rows, err = s.DB.Query(c, `SELECT t.user_id, o.id, o.status_id, o.planned_start, o.planned_end, e.model_id,
			COALESCE((SELECT sum(EXTRACT(EPOCH FROM (COALESCE(w.ended_at, now()) - w.started_at))) FROM dm.work_intervals w WHERE w.order_id = o.id), 0),
			COALESCE((SELECT sum(i.duration) FROM dm.maintenance_order_issues moi
				JOIN dm.instructions i ON i.alarm_code_id = moi.alarm_code_id AND NOT i.is_deleted
				JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = e.model_id
				WHERE moi.order_id = o.id AND NOT moi.is_other), 0)
		FROM dm.maintenance_order_technicians t JOIN dm.maintenance_orders o ON o.id = t.order_id
		JOIN asset.equipment e ON e.id = o.asset_id
		WHERE `+perfOrderWhere+` AND o.status_id <> 2`, ids, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var uid, oid, modelID int64
		var status int
		var start time.Time
		var end *time.Time
		var actual float64
		var minutes int64
		if err := rows.Scan(&uid, &oid, &status, &start, &end, &modelID, &actual, &minutes); err != nil {
			rows.Close()
			return nil, err
		}
		p := stats[uid]
		if p == nil {
			continue
		}
		est := float64(minutes) * 60
		if est == 0 && end != nil && end.After(start) {
			est = end.Sub(start).Seconds()
		}
		p.orders++
		p.estimated += est
		if status == maint.StatusDone || status == maint.StatusClosed {
			p.actual += actual
			p.planned += est
		}
	}
	rows.Close()
	days := math.Ceil(to.Sub(from).Hours() / 24)
	if days < 1 {
		days = 1
	}
	available := days * 8 * 3600 // 8 working hours a day
	out := make([]*personnelStat, 0, len(stats))
	for _, p := range stats {
		p.capacity = math.Min(100, math.Round(p.estimated/available*1000)/10)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].orders != out[j].orders {
			return out[i].orders > out[j].orders
		}
		return strings.ToLower(out[i].name) < strings.ToLower(out[j].name)
	})
	return out, nil
}

func formatTimeLoss(seconds float64) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	mins := int64(math.Round(seconds / 60))
	if mins == 0 {
		return "0m"
	}
	h, m := mins/60, mins%60
	if h > 0 {
		return fmt.Sprintf("%s%dh %dm", sign, h, m)
	}
	return fmt.Sprintf("%s%dm", sign, m)
}

func personnelRow(i int, p *personnelStat) map[string]any {
	return map[string]any{"id": i, "personnelName": p.name, "orderCount": p.orders, "capacity": p.capacity,
		"timeLoss": formatTimeLoss(p.actual - p.planned)}
}

func (s *Server) mpTopPersonnel(c *Ctx) (any, error) {
	stats, err := s.personnelStats(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i, p := range stats {
		if i >= 3 || p.orders == 0 {
			break
		}
		out = append(out, personnelRow(i+1, p))
	}
	return out, nil
}

func (s *Server) mpPagedPersonnel(c *Ctx) (any, error) {
	stats, err := s.personnelStats(c)
	if err != nil {
		return nil, err
	}
	search := strings.ToLower(c.F.StrTrim("search"))
	if search != "" {
		stats = filterRows(stats, func(p *personnelStat) bool {
			return strings.Contains(strings.ToLower(p.name), search) || strings.Contains(strings.ToLower(p.username), search)
		})
	}
	key, asc := c.sortSpec("orderCount", false)
	sort.SliceStable(stats, func(i, j int) bool {
		a, b := stats[i], stats[j]
		var less, equal bool
		switch key {
		case "personnelName":
			less, equal = strings.ToLower(a.name) < strings.ToLower(b.name), strings.EqualFold(a.name, b.name)
		case "capacity":
			less, equal = a.capacity < b.capacity, a.capacity == b.capacity
		case "timeLoss":
			la, lb := a.actual-a.planned, b.actual-b.planned
			less, equal = la < lb, la == lb
		default:
			less, equal = a.orders < b.orders, a.orders == b.orders
		}
		if equal {
			return false
		}
		if asc {
			return less
		}
		return !less
	})
	page, limit := c.page(1), c.limit(13, 500)
	data := []map[string]any{}
	for i, p := range paginate(stats, page, limit) {
		data = append(data, personnelRow(int((page-1)*limit)+i+1, p))
	}
	return map[string]any{"count": len(stats), "data": data}, nil
}

func (s *Server) mpCapacityDistribution(c *Ctx) (any, error) {
	stats, err := s.personnelStats(c)
	if err != nil {
		return nil, err
	}
	ranges := []string{"0-20%", "21-40%", "41-60%", "61-80%", "81-100%"}
	counts := make([]int, 5)
	for _, p := range stats {
		idx := 0
		switch {
		case p.capacity > 80:
			idx = 4
		case p.capacity > 60:
			idx = 3
		case p.capacity > 40:
			idx = 2
		case p.capacity > 20:
			idx = 1
		}
		counts[idx]++
	}
	out := []map[string]any{}
	for i, r := range ranges {
		out = append(out, map[string]any{"range": r, "personnelCount": counts[i]})
	}
	return out, nil
}

// reliability computes MTBF / MTTR (hours) per equipment over a period.
// A failure is a non-rejected corrective maintenance order; the repair time
// is the actual work time of completed corrective orders.
type reliability struct {
	assetID  int64
	name     string
	mtbf     *float64
	mttr     *float64
	failures int64
	repairs  int64
}

func (s *Server) reliabilityStats(ctx context.Context, ids []int64, from, to time.Time) ([]*reliability, error) {
	infos, err := s.assetInfos(ctx, ids)
	if err != nil {
		return nil, err
	}
	durs, err := s.statusDurations(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	type agg struct {
		failures, repairs int64
		repairSecs        float64
	}
	aggs := map[int64]*agg{}
	rows, err := s.DB.Query(ctx, `SELECT o.asset_id, count(*),
			count(*) FILTER (WHERE o.status_id IN (5, 6)),
			COALESCE(sum((SELECT sum(EXTRACT(EPOCH FROM (COALESCE(w.ended_at, now()) - w.started_at))) FROM dm.work_intervals w WHERE w.order_id = o.id))
				FILTER (WHERE o.status_id IN (5, 6)), 0)
		FROM dm.maintenance_orders o WHERE o.asset_id = ANY($1) AND o.maintenance_type_id = 1 AND o.status_id <> 2
		AND COALESCE(o.detected_at, o.planned_start) >= $2 AND COALESCE(o.detected_at, o.planned_start) < $3 GROUP BY o.asset_id`, ids, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		a := &agg{}
		if err := rows.Scan(&id, &a.failures, &a.repairs, &a.repairSecs); err != nil {
			rows.Close()
			return nil, err
		}
		aggs[id] = a
	}
	rows.Close()
	out := []*reliability{}
	for _, id := range ids {
		info := infos[id]
		if info == nil {
			continue
		}
		r := &reliability{assetID: id, name: info.Name}
		running := durs[id][engine.StatusRunning] / 3600
		a := aggs[id]
		if a != nil {
			r.failures, r.repairs = a.failures, a.repairs
			if a.repairs > 0 {
				v := round2(a.repairSecs / float64(a.repairs) / 3600)
				r.mttr = &v
			}
		}
		if running > 0 || r.failures > 0 {
			f := r.failures
			if f < 1 {
				f = 1
			}
			v := round2(running / float64(f))
			r.mtbf = &v
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Server) mpMTBFChart(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, true)
	if err != nil {
		return nil, err
	}
	stats, err := s.reliabilityStats(c, ids, from, to)
	if err != nil {
		return nil, err
	}
	stats = filterRows(stats, func(r *reliability) bool { return r.mtbf != nil })
	sort.SliceStable(stats, func(i, j int) bool { return *stats[i].mtbf > *stats[j].mtbf })
	cats, vals := []string{}, []float64{}
	for i, r := range stats {
		if i >= 5 {
			break
		}
		cats = append(cats, r.name)
		vals = append(vals, *r.mtbf)
	}
	return map[string]any{"category": cats, "unit": "hours", "values": vals}, nil
}

func (s *Server) mpMTTRChart(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, true)
	if err != nil {
		return nil, err
	}
	stats, err := s.reliabilityStats(c, ids, from, to)
	if err != nil {
		return nil, err
	}
	stats = filterRows(stats, func(r *reliability) bool { return r.mttr != nil })
	sort.SliceStable(stats, func(i, j int) bool { return *stats[i].mttr < *stats[j].mttr })
	cats, vals := []string{}, []float64{}
	for i, r := range stats {
		if i >= 5 {
			break
		}
		cats = append(cats, r.name)
		vals = append(vals, *r.mttr)
	}
	return map[string]any{"category": cats, "unit": "hours", "values": vals}, nil
}

func averageOf(stats []*reliability, pick func(*reliability) *float64) string {
	var sum float64
	var n int
	for _, r := range stats {
		if v := pick(r); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", sum/float64(n))
}

func (s *Server) mpMTBFAverage(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, true)
	if err != nil {
		return nil, err
	}
	stats, err := s.reliabilityStats(c, ids, from, to)
	if err != nil {
		return nil, err
	}
	return []map[string]any{{"mtbf": averageOf(stats, func(r *reliability) *float64 { return r.mtbf }), "unit": "hours"}}, nil
}

func (s *Server) mpMTTRAverage(c *Ctx) (any, error) {
	ids, from, to, err := s.perfScope(c, true)
	if err != nil {
		return nil, err
	}
	stats, err := s.reliabilityStats(c, ids, from, to)
	if err != nil {
		return nil, err
	}
	return []map[string]any{{"mttr": averageOf(stats, func(r *reliability) *float64 { return r.mttr }), "unit": "hours"}}, nil
}

func (s *Server) mpLastUpdated(c *Ctx) (any, error) {
	var ts *time.Time
	if err := s.DB.QueryRow(c, `SELECT max(computed_at) FROM dm.asset_kpis`).Scan(&ts); err != nil {
		return nil, err
	}
	if ts == nil {
		now := time.Now()
		ts = &now
	}
	return map[string]any{"last_updated": httpx.TS(*ts)}, nil
}

// RefreshKPIs recomputes the trailing-year MTBF/MTTR of every equipment.
func (s *Server) RefreshKPIs(ctx context.Context) error {
	ids, err := dbInt64sCtx(ctx, s, `SELECT id FROM asset.equipment`)
	if err != nil {
		return err
	}
	to := time.Now().UTC()
	from := to.AddDate(-1, 0, 0)
	stats, err := s.reliabilityStats(ctx, ids, from, to)
	if err != nil {
		return err
	}
	for _, r := range stats {
		if _, err := s.DB.Exec(ctx, `INSERT INTO dm.asset_kpis (asset_id, mtbf_hours, mttr_hours, failures, repairs, computed_at)
			VALUES ($1, $2, $3, $4, $5, now()) ON CONFLICT (asset_id) DO UPDATE SET mtbf_hours = EXCLUDED.mtbf_hours,
			mttr_hours = EXCLUDED.mttr_hours, failures = EXCLUDED.failures, repairs = EXCLUDED.repairs, computed_at = now()`,
			r.assetID, r.mtbf, r.mttr, r.failures, r.repairs); err != nil {
			return err
		}
	}
	return nil
}

func dbInt64sCtx(ctx context.Context, s *Server, sql string, args ...any) ([]int64, error) {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Server) mpPagedMTBFMTTR(c *Ctx) (any, error) {
	ids, err := s.resolveAssets(c, c.assetFilterKeys("location", "asset", "asset_type", "asset_model"))
	if err != nil {
		return nil, err
	}
	type row struct {
		name       string
		mtbf, mttr *float64
	}
	rows, err := s.DB.Query(c, `SELECT e.name, k.mtbf_hours, k.mttr_hours FROM asset.equipment e
		LEFT JOIN dm.asset_kpis k ON k.asset_id = e.id WHERE e.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var items []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.mtbf, &r.mttr); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, r)
	}
	rows.Close()
	search := strings.ToLower(c.F.StrTrim("search"))
	if search != "" {
		items = filterRows(items, func(r row) bool { return strings.Contains(strings.ToLower(r.name), search) })
	}
	key, asc := c.sortSpec("mtbf", false)
	val := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	sort.SliceStable(items, func(i, j int) bool {
		var less, equal bool
		switch key {
		case "assetName":
			less, equal = strings.ToLower(items[i].name) < strings.ToLower(items[j].name), strings.EqualFold(items[i].name, items[j].name)
		case "mttr":
			less, equal = val(items[i].mttr) < val(items[j].mttr), val(items[i].mttr) == val(items[j].mttr)
		default:
			less, equal = val(items[i].mtbf) < val(items[j].mtbf), val(items[i].mtbf) == val(items[j].mtbf)
		}
		if equal {
			return false
		}
		if asc {
			return less
		}
		return !less
	})
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%.1f", *p)
	}
	page, limit := c.page(1), c.limit(13, 500)
	data := []map[string]any{}
	for i, r := range paginate(items, page, limit) {
		data = append(data, map[string]any{"id": int((page-1)*limit) + i + 1, "assetName": r.name, "mtbf": f(r.mtbf), "mttr": f(r.mttr)})
	}
	return map[string]any{"count": len(items), "data": data}, nil
}
