package api

import (
	"context"
	"time"

	"afdd/internal/engine"
)

// currentState is the open status/condition of an equipment.
type currentState struct {
	Status         int
	StatusSince    *time.Time
	Condition      string
	ConditionSince *time.Time
}

// currentStates returns the open status and condition intervals.
func (s *Server) currentStates(ctx context.Context, assetIDs []int64) (map[int64]*currentState, error) {
	out := map[int64]*currentState{}
	get := func(id int64) *currentState {
		if out[id] == nil {
			out[id] = &currentState{}
		}
		return out[id]
	}
	rows, err := s.DB.Query(ctx, `SELECT asset_id, status_id, start_ts FROM ts.asset_status WHERE end_ts IS NULL AND asset_id = ANY($1)`, assetIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var st int
		var since time.Time
		if err := rows.Scan(&id, &st, &since); err != nil {
			rows.Close()
			return nil, err
		}
		cs := get(id)
		cs.Status, cs.StatusSince = st, &since
	}
	rows.Close()
	rows, err = s.DB.Query(ctx, `SELECT asset_id, condition, start_ts FROM ts.asset_condition WHERE end_ts IS NULL AND asset_id = ANY($1)`, assetIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c string
		var since time.Time
		if err := rows.Scan(&id, &c, &since); err != nil {
			return nil, err
		}
		cs := get(id)
		cs.Condition, cs.ConditionSince = c, &since
	}
	return out, rows.Err()
}

// statusName returns the UI label or nil when unknown.
func statusName(st *currentState) any {
	if st == nil || st.Status == 0 {
		return nil
	}
	return engine.StatusName(st.Status)
}

func conditionName(st *currentState) any {
	if st == nil || st.Condition == "" {
		return nil
	}
	return st.Condition
}

// statusDurations returns, per equipment, the seconds spent in each status
// within [from, to) (clipped to now).
func (s *Server) statusDurations(ctx context.Context, assetIDs []int64, from, to time.Time) (map[int64]map[int]float64, error) {
	out := map[int64]map[int]float64{}
	if len(assetIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT asset_id, status_id,
			sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(end_ts, now()), $2, now()) - GREATEST(start_ts, $1))))
		FROM ts.asset_status
		WHERE asset_id = ANY($3) AND start_ts < $2 AND COALESCE(end_ts, now()) > $1
		GROUP BY asset_id, status_id`, from, to, assetIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st int
		var secs float64
		if err := rows.Scan(&id, &st, &secs); err != nil {
			return nil, err
		}
		if secs < 0 {
			secs = 0
		}
		if out[id] == nil {
			out[id] = map[int]float64{}
		}
		out[id][st] += secs
	}
	return out, rows.Err()
}

// elapsed returns the observable seconds of [from, to) (clipped to now).
func elapsed(from, to time.Time) float64 {
	now := time.Now()
	if to.After(now) {
		to = now
	}
	if !to.After(from) {
		return 0
	}
	return to.Sub(from).Seconds()
}

// runningHours returns lifetime running hours per equipment.
func (s *Server) runningHours(ctx context.Context, assetIDs []int64) (map[int64]float64, error) {
	out := map[int64]float64{}
	if len(assetIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT asset_id, sum(EXTRACT(EPOCH FROM (COALESCE(end_ts, now()) - start_ts))) / 3600.0
		FROM ts.asset_status WHERE asset_id = ANY($1) AND status_id = 1 GROUP BY asset_id`, assetIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var h float64
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = round1(h)
	}
	return out, rows.Err()
}

// alarmCounts returns warning/critical counts of events started in range.
func (s *Server) alarmCounts(ctx context.Context, assetIDs []int64, from, to time.Time) (map[int64][2]int, error) {
	out := map[int64][2]int{}
	if len(assetIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT asset_id, count(*) FILTER (WHERE severity = 'WARNING'), count(*) FILTER (WHERE severity = 'CRITICAL')
		FROM ts.alarm_events WHERE asset_id = ANY($1) AND start_ts >= $2 AND start_ts < $3 GROUP BY asset_id`, assetIDs, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var w, c int
		if err := rows.Scan(&id, &w, &c); err != nil {
			return nil, err
		}
		out[id] = [2]int{w, c}
	}
	return out, rows.Err()
}

// uptimeStats computes uptime percentage and durations (hours).
type uptimeStat struct {
	UptimePct    *float64
	RunningHours float64
	TotalHours   float64
	PlannedHours float64
	UnplannedHrs float64
	OfflineHours float64
	IdleHours    float64
}

func computeUptime(durs map[int]float64, total float64) uptimeStat {
	st := uptimeStat{TotalHours: round2(total / 3600)}
	if durs != nil {
		st.RunningHours = round2(durs[engine.StatusRunning] / 3600)
		st.PlannedHours = round2(durs[engine.StatusPlannedDown] / 3600)
		st.UnplannedHrs = round2(durs[engine.StatusUnplannedDown] / 3600)
		st.OfflineHours = round2(durs[engine.StatusOffline] / 3600)
		st.IdleHours = round2(durs[engine.StatusIdle] / 3600)
	}
	if total > 0 {
		u := round1(durs[engine.StatusRunning] / total * 100)
		if u > 100 {
			u = 100
		}
		st.UptimePct = &u
	}
	return st
}
