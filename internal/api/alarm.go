package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"afdd/internal/httpx"
)

func (s *Server) registerAlarm() {
	s.handle("alarm/getallassetbyfilter", AccessUser, s.alarmAssetsByFilter)
	s.handle("alarm/getalarmbyassetid", AccessUser, s.alarmLogs)
	s.handle("alarm/getalarmcountperday", AccessUser, s.alarmCountPerDay)
	s.handle("alarm/getmostoccuralarmperasset", AccessUser, s.alarmTopAssets)
	s.handle("alarm/getmostoccurabnormalperasset", AccessUser, s.alarmTopAbnormal)
	s.handle("alarm/getalarmcount", AccessUser, s.alarmMetrics)
	s.handle("alarm/getalarmevents", AccessUser, s.alarmEvents)
	s.handle("alarm/getalarmeventsexport", AccessUser, s.alarmEventsExport)
}

func (s *Server) alarmAssetsByFilter(c *Ctx) (any, error) {
	ids, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id})
	}
	return out, nil
}

// alarmQuery holds the common alarm report parameters.
type alarmQuery struct {
	assets   []int64
	from, to time.Time
	types    []string // ALARM / ABNORMAL
	search   string
	offsetM  int // minutes east of UTC (client time zone)
	period   string
}

func (s *Server) alarmParams(c *Ctx) (*alarmQuery, error) {
	ids, _ := c.F.IDs("asset_ids")
	// Restrict to visible equipment.
	visible, err := s.resolveAssets(c, assetFilter{})
	if err != nil {
		return nil, err
	}
	vis := int64Set(visible)
	q := &alarmQuery{}
	for _, id := range ids {
		if vis[id] {
			q.assets = append(q.assets, id)
		}
	}
	q.from, q.to = c.dateRange("start_ts", "end_ts", true, 7)
	switch c.F.Int("alarm_type_id", 0) {
	case 1:
		q.types = []string{"ALARM"}
	case 2:
		q.types = []string{"ABNORMAL"}
	case -1:
		q.types = []string{}
	default:
		q.types = []string{"ALARM", "ABNORMAL"}
	}
	q.search = c.F.StrTrim("search_filter")
	_, off := time.Now().In(c.Location()).Zone()
	q.offsetM = off / 60
	q.period = strings.ToLower(c.F.StrTrim("period"))
	if q.period != "monthly" {
		q.period = "daily"
	}
	return q, nil
}

const alarmWhere = `ev.asset_id = ANY($1) AND ev.start_ts >= $2 AND ev.start_ts < $3 AND ev.alarm_type = ANY($4)
	AND ($5 = '' OR e.name ILIKE $6 OR ev.name ILIKE $6 OR ev.message ILIKE $6 OR ev.severity ILIKE $6 OR ev.alarm_type ILIKE $6)`

func (q *alarmQuery) args() []any {
	return []any{q.assets, q.from, q.to, q.types, q.search, ilike(q.search)}
}

func (s *Server) alarmLogs(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	limit := c.limit(16, 1000)
	page := c.page(1)
	order := "ev.start_ts DESC"
	for _, o := range []struct{ key, col string }{
		{"order_timestamp", "ev.start_ts"}, {"order_equipment", "lower(e.name)"}, {"order_alarm_type", "ev.alarm_type"},
		{"order_alarm_status", "ev.severity"}, {"order_alarm_message", "lower(ev.message)"},
	} {
		if asc := c.F.BoolPtr(o.key); asc != nil {
			dir := "DESC"
			if *asc {
				dir = "ASC"
			}
			order = o.col + " " + dir + ", ev.start_ts DESC"
			break
		}
	}
	var total int
	args := q.args()
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere, args...).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT e.name, ev.start_ts, ev.alarm_type, ev.severity, ev.name, ev.message
		FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere+`
		ORDER BY `+order+` OFFSET $7 LIMIT $8`, append(args, (page-1)*limit, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var asset, typ, sev, name, msg string
		var ts time.Time
		if err := rows.Scan(&asset, &ts, &typ, &sev, &name, &msg); err != nil {
			return nil, err
		}
		message := msg
		if message == "" {
			message = name
		} else if typ == "ALARM" && !strings.Contains(msg, name) {
			message = name + " - " + msg
		}
		data = append(data, map[string]any{"assetName": asset, "timestamp": httpx.TS(ts), "alarmType": typ, "alarmStatus": sev, "alarmMessage": message})
	}
	return map[string]any{"total": total, "data": data}, rows.Err()
}

type bucketCount struct {
	warning, critical, abnormal int
}

// buckets enumerates the day or month buckets of the range (client tz).
func (q *alarmQuery) buckets() []time.Time {
	loc := time.FixedZone("client", q.offsetM*60)
	start := q.from.In(loc)
	end := q.to.In(loc)
	var out []time.Time
	if q.period == "monthly" {
		cur := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, loc)
		for cur.Before(end) && len(out) < 240 {
			out = append(out, cur)
			cur = cur.AddDate(0, 1, 0)
		}
		return out
	}
	cur := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	for cur.Before(end) && len(out) < 3700 {
		out = append(out, cur)
		cur = cur.AddDate(0, 0, 1)
	}
	return out
}

func (q *alarmQuery) bucketKey(t time.Time) string {
	loc := time.FixedZone("client", q.offsetM*60)
	lt := t.In(loc)
	if q.period == "monthly" {
		return lt.Format("2006-01")
	}
	return lt.Format("2006-01-02")
}

// countsByBucket groups event counts per asset and bucket.
func (s *Server) countsByBucket(ctx context.Context, q *alarmQuery) (map[int64]map[string]*bucketCount, error) {
	trunc := "day"
	if q.period == "monthly" {
		trunc = "month"
	}
	rows, err := s.DB.Query(ctx, `SELECT ev.asset_id, to_char(date_trunc('`+trunc+`', ev.start_ts + make_interval(mins => $7)), 'YYYY-MM-DD'),
			count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'WARNING'),
			count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'CRITICAL'),
			count(*) FILTER (WHERE ev.alarm_type = 'ABNORMAL')
		FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere+` GROUP BY 1, 2`, append(q.args(), q.offsetM)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]*bucketCount{}
	for rows.Next() {
		var asset int64
		var day string
		var b bucketCount
		if err := rows.Scan(&asset, &day, &b.warning, &b.critical, &b.abnormal); err != nil {
			return nil, err
		}
		key := day
		if q.period == "monthly" {
			key = day[:7]
		}
		if out[asset] == nil {
			out[asset] = map[string]*bucketCount{}
		}
		bc := b
		out[asset][key] = &bc
	}
	return out, rows.Err()
}

func bucketLabels(q *alarmQuery, t time.Time) (label, alt, axis string) {
	if q.period == "monthly" {
		return t.Format("Jan 2006"), t.Format("2006-01-02"), t.Format("Jan 2006")
	}
	return t.Format("2006-01-02"), t.Format("2006-01-02"), t.Format("02/01")
}

func (s *Server) alarmCountPerDay(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	per, err := s.countsByBucket(c, q)
	if err != nil {
		return nil, err
	}
	totals := map[string]*bucketCount{}
	for _, m := range per {
		for k, b := range m {
			t := totals[k]
			if t == nil {
				t = &bucketCount{}
				totals[k] = t
			}
			t.warning += b.warning
			t.critical += b.critical
			t.abnormal += b.abnormal
		}
	}
	out := []map[string]any{}
	for _, b := range q.buckets() {
		key := b.Format("2006-01-02")
		if q.period == "monthly" {
			key = b.Format("2006-01")
		}
		t := totals[key]
		if t == nil {
			t = &bucketCount{}
		}
		label, alt, _ := bucketLabels(q, b)
		out = append(out, map[string]any{"date": label, "dateAlt": alt, "warningCount": t.warning, "criticalCount": t.critical, "abnormalCount": t.abnormal})
	}
	return out, nil
}

func (s *Server) assetCountRows(c *Ctx, q *alarmQuery) ([]struct {
	name                        string
	warning, critical, abnormal int
}, error) {
	rows, err := s.DB.Query(c, `SELECT e.name,
			count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'WARNING'),
			count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'CRITICAL'),
			count(*) FILTER (WHERE ev.alarm_type = 'ABNORMAL')
		FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere+` GROUP BY e.id, e.name`, q.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		name                        string
		warning, critical, abnormal int
	}
	for rows.Next() {
		var r struct {
			name                        string
			warning, critical, abnormal int
		}
		if err := rows.Scan(&r.name, &r.warning, &r.critical, &r.abnormal); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) alarmTopAssets(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	rows, err := s.assetCountRows(c, q)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].warning+rows[i].critical, rows[j].warning+rows[j].critical
		if a != b {
			return a > b
		}
		return rows[i].name < rows[j].name
	})
	out := []map[string]any{}
	for _, r := range rows {
		if r.warning+r.critical == 0 {
			continue
		}
		out = append(out, map[string]any{"assetName": r.name, "warningCount": r.warning, "criticalCount": r.critical})
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

func (s *Server) alarmTopAbnormal(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	rows, err := s.assetCountRows(c, q)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].abnormal != rows[j].abnormal {
			return rows[i].abnormal > rows[j].abnormal
		}
		return rows[i].name < rows[j].name
	})
	out := []map[string]any{}
	for _, r := range rows {
		if r.abnormal == 0 {
			continue
		}
		out = append(out, map[string]any{"assetName": r.name, "abnormalCount": r.abnormal})
		if len(out) >= 10 {
			break
		}
	}
	return out, nil
}

func (s *Server) alarmMetrics(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	var w, cr, ab int
	if err := s.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'WARNING'),
			count(*) FILTER (WHERE ev.alarm_type = 'ALARM' AND ev.severity = 'CRITICAL'),
			count(*) FILTER (WHERE ev.alarm_type = 'ABNORMAL')
		FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere, q.args()...).Scan(&w, &cr, &ab); err != nil {
		return nil, err
	}
	return map[string]any{"totalWarning": w, "totalCritical": cr, "totalAbnormal": ab, "totalAlarm": w + cr + ab}, nil
}

func (s *Server) alarmEventRows(c *Ctx, q *alarmQuery, assetIDs []int64) ([]map[string]any, error) {
	if len(assetIDs) == 0 {
		return []map[string]any{}, nil
	}
	sub := *q
	sub.assets = assetIDs
	per, err := s.countsByBucket(c, &sub)
	if err != nil {
		return nil, err
	}
	infos, err := s.assetInfos(c, assetIDs)
	if err != nil {
		return nil, err
	}
	type last struct {
		msg, name, status, typ string
		ts                     time.Time
	}
	lasts := map[int64]*last{}
	rows, err := s.DB.Query(c, `SELECT DISTINCT ON (ev.asset_id) ev.asset_id, ev.message, ev.name, ev.severity, ev.alarm_type, ev.start_ts
		FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere+`
		ORDER BY ev.asset_id, ev.start_ts DESC`, sub.args()...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		l := &last{}
		if err := rows.Scan(&id, &l.msg, &l.name, &l.status, &l.typ, &l.ts); err != nil {
			rows.Close()
			return nil, err
		}
		lasts[id] = l
	}
	rows.Close()
	buckets := q.buckets()
	out := []map[string]any{}
	for _, id := range assetIDs {
		a := infos[id]
		if a == nil {
			continue
		}
		data := []map[string]any{}
		for _, b := range buckets {
			key := b.Format("2006-01-02")
			if q.period == "monthly" {
				key = b.Format("2006-01")
			}
			bc := per[id][key]
			if bc == nil {
				bc = &bucketCount{}
			}
			_, alt, axis := bucketLabels(q, b)
			data = append(data, map[string]any{"timestamp": axis, "dateAlt": alt, "warningCount": bc.warning, "criticalCount": bc.critical, "abnormalCount": bc.abnormal})
		}
		row := map[string]any{"assetId": id, "assetName": a.Name, "data": data, "assetImagePath": imageURL(a.Image), "modelName": a.ModelName,
			"lastAlarmMessage": nil, "lastAlarmTimestamp": nil, "lastAlarmName": nil, "lastAlarmStatus": nil, "lastAlarmType": nil}
		if l := lasts[id]; l != nil {
			msg := l.msg
			if msg == "" {
				msg = l.name
			}
			row["lastAlarmMessage"] = msg
			row["lastAlarmTimestamp"] = httpx.TS(l.ts)
			row["lastAlarmName"] = l.name
			row["lastAlarmStatus"] = l.status
			row["lastAlarmType"] = l.typ
		}
		out = append(out, row)
	}
	return out, nil
}

// alarmAssetsWithEvents returns assets (sorted by name) that match the
// search; assets without events are included only when no search is given.
func (s *Server) alarmAssetsOrdered(c *Ctx, q *alarmQuery) ([]int64, error) {
	infos, err := s.assetInfos(c, q.assets)
	if err != nil {
		return nil, err
	}
	withEvents := map[int64]bool{}
	ids, err := dbInt64s(c, s, `SELECT DISTINCT ev.asset_id FROM ts.alarm_events ev JOIN asset.equipment e ON e.id = ev.asset_id WHERE `+alarmWhere, q.args()...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		withEvents[id] = true
	}
	var out []int64
	search := strings.ToLower(q.search)
	for _, id := range q.assets {
		a := infos[id]
		if a == nil {
			continue
		}
		if search != "" && !withEvents[id] && !strings.Contains(strings.ToLower(a.Name), search) {
			continue
		}
		out = append(out, id)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Assets with events first, then by name.
		if withEvents[out[i]] != withEvents[out[j]] {
			return withEvents[out[i]]
		}
		return strings.ToLower(infos[out[i]].Name) < strings.ToLower(infos[out[j]].Name)
	})
	return out, nil
}

func (s *Server) alarmEvents(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	ordered, err := s.alarmAssetsOrdered(c, q)
	if err != nil {
		return nil, err
	}
	limit := int(c.limit(3, 100))
	page := int(c.page(1))
	start := (page - 1) * limit
	if start > len(ordered) {
		start = len(ordered)
	}
	end := start + limit
	if end > len(ordered) {
		end = len(ordered)
	}
	data, err := s.alarmEventRows(c, q, ordered[start:end])
	if err != nil {
		return nil, err
	}
	return map[string]any{"count": len(ordered), "data": data}, nil
}

func (s *Server) alarmEventsExport(c *Ctx) (any, error) {
	q, err := s.alarmParams(c)
	if err != nil {
		return nil, err
	}
	ordered, err := s.alarmAssetsOrdered(c, q)
	if err != nil {
		return nil, err
	}
	return s.alarmEventRows(c, q, ordered)
}

func dbInt64s(c *Ctx, s *Server, sql string, args ...any) ([]int64, error) {
	rows, err := s.DB.Query(c, sql, args...)
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

var _ = fmt.Sprint
