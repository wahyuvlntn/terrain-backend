package api

import (
	"context"
	"time"

	"afdd/internal/httpx"
)

// Plant overview: for a location (none = the top-most visible ones) and each
// visible child, the current equipment statuses and conditions, the alarm
// events of a period by type and severity (plus the open ones) and the
// maintenance orders planned in the period by status.

type plantAlarms struct {
	AlarmCritical    int `json:"alarmCritical"`
	AlarmWarning     int `json:"alarmWarning"`
	AbnormalCritical int `json:"abnormalCritical"`
	AbnormalWarning  int `json:"abnormalWarning"`
	ActiveAlarm      int `json:"activeAlarm"`
	ActiveAbnormal   int `json:"activeAbnormal"`
}

func (a *plantAlarms) add(b *plantAlarms) {
	a.AlarmCritical += b.AlarmCritical
	a.AlarmWarning += b.AlarmWarning
	a.AbnormalCritical += b.AbnormalCritical
	a.AbnormalWarning += b.AbnormalWarning
	a.ActiveAlarm += b.ActiveAlarm
	a.ActiveAbnormal += b.ActiveAbnormal
}

type plantOrders struct {
	Requested  int `json:"requested"`
	Open       int `json:"open"`
	InProgress int `json:"inProgress"`
	Paused     int `json:"paused"`
	Done       int `json:"done"`
	Closed     int `json:"closed"`
	Rejected   int `json:"rejected"`
	Total      int `json:"total"`
}

func (o *plantOrders) addStatus(code string, n int) {
	switch code {
	case "REQUESTED":
		o.Requested += n
	case "OPEN":
		o.Open += n
	case "IN_PROGRESS":
		o.InProgress += n
	case "PAUSED":
		o.Paused += n
	case "DONE":
		o.Done += n
	case "CLOSED":
		o.Closed += n
	case "REJECTED":
		o.Rejected += n
	}
	o.Total += n
}

func (o *plantOrders) add(b *plantOrders) {
	o.Requested += b.Requested
	o.Open += b.Open
	o.InProgress += b.InProgress
	o.Paused += b.Paused
	o.Done += b.Done
	o.Closed += b.Closed
	o.Rejected += b.Rejected
	o.Total += b.Total
}

func (s *Server) loPlantOverview(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}
	id := c.F.IntPtr("location_id")
	if id != nil && (tree.nodes[*id] == nil || !scope.AllowsLocation(*id)) {
		return nil, httpx.NotFound("err_location_not_found")
	}
	from, to := c.dateRange("start", "end", true, 7)

	byLoc, all, err := s.visibleEquipment(c)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, all)
	if err != nil {
		return nil, err
	}
	alarms, err := s.assetAlarmCounts(c, all, from, to)
	if err != nil {
		return nil, err
	}
	orders, err := s.assetOrderCounts(c, all, from, to)
	if err != nil {
		return nil, err
	}

	counts := func(locIDs ...int64) map[string]any {
		assets := assetsUnder(tree, byLoc, locIDs...)
		st := countStates(assets, states)
		al, or := &plantAlarms{}, &plantOrders{}
		for _, a := range assets {
			if v := alarms[a]; v != nil {
				al.add(v)
			}
			if v := orders[a]; v != nil {
				or.add(v)
			}
		}
		return map[string]any{
			"equipment": map[string]int{
				"total": st.total, "running": st.running, "idle": st.idle, "plannedDown": st.planned,
				"unplannedDown": st.unplanned, "offline": st.offline,
				"normal": st.normal, "warning": st.warning, "critical": st.critical,
			},
			"alarms":      al,
			"maintenance": or,
		}
	}

	var summary map[string]any
	path := []map[string]any{}
	if id != nil {
		summary = counts(*id)
		for _, a := range tree.ancestors(*id) {
			if scope.AllowsLocation(a) {
				path = append(path, map[string]any{"locationId": a, "locationName": tree.name(a)})
			}
		}
	} else {
		summary = counts(childLocations(tree, scope, nil, "")...)
	}

	data := []map[string]any{}
	for _, ch := range childLocations(tree, scope, id, c.F.StrTrim("search_filter")) {
		n := tree.nodes[ch]
		row := counts(ch)
		row["locationId"] = ch
		row["locationName"] = n.Name
		row["locationImagePath"] = derefStr(imageURL(n.Image))
		row["childCount"] = len(n.Children)
		data = append(data, row)
	}
	return map[string]any{"path": path, "summary": summary, "data": data}, nil
}

// assetAlarmCounts counts the alarm events that started in [from, to) and the
// open ones, per equipment.
func (s *Server) assetAlarmCounts(ctx context.Context, assets []int64, from, to time.Time) (map[int64]*plantAlarms, error) {
	out := map[int64]*plantAlarms{}
	if len(assets) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT asset_id,
			count(*) FILTER (WHERE alarm_type = 'ALARM' AND severity = 'CRITICAL' AND start_ts >= $2),
			count(*) FILTER (WHERE alarm_type = 'ALARM' AND severity = 'WARNING' AND start_ts >= $2),
			count(*) FILTER (WHERE alarm_type = 'ABNORMAL' AND severity = 'CRITICAL' AND start_ts >= $2),
			count(*) FILTER (WHERE alarm_type = 'ABNORMAL' AND severity = 'WARNING' AND start_ts >= $2),
			count(*) FILTER (WHERE alarm_type = 'ALARM' AND end_ts IS NULL),
			count(*) FILTER (WHERE alarm_type = 'ABNORMAL' AND end_ts IS NULL)
		FROM ts.alarm_events
		WHERE asset_id = ANY($1) AND start_ts < $3 AND (start_ts >= $2 OR end_ts IS NULL)
		GROUP BY asset_id`, assets, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		a := &plantAlarms{}
		if err := rows.Scan(&id, &a.AlarmCritical, &a.AlarmWarning, &a.AbnormalCritical, &a.AbnormalWarning,
			&a.ActiveAlarm, &a.ActiveAbnormal); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// assetOrderCounts counts the maintenance orders planned in [from, to) by
// status, per equipment.
func (s *Server) assetOrderCounts(ctx context.Context, assets []int64, from, to time.Time) (map[int64]*plantOrders, error) {
	out := map[int64]*plantOrders{}
	if len(assets) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT o.asset_id, st.code, count(*)
		FROM dm.maintenance_orders o JOIN dm.maintenance_statuses st ON st.id = o.status_id
		WHERE o.asset_id = ANY($1) AND o.planned_start >= $2 AND o.planned_start < $3
		GROUP BY o.asset_id, st.code`, assets, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var code string
		var n int
		if err := rows.Scan(&id, &code, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = &plantOrders{}
		}
		out[id].addStatus(code, n)
	}
	return out, rows.Err()
}
