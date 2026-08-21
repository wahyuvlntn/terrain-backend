package engine

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"afdd/internal/db"
)

// External FDD alarms (DESIGN §4.7): the LBNL fault detection rules of the
// ai-analytics service write analytics.faults; every ACTIVE fault is raised as
// an engine alarm of its equipment, named after the rule code (an alarm code),
// so the usual chain follows: alarm event with alarm_code_id, notification,
// equipment condition and, through the code's issue relation, an early-warning
// maintenance order with the code's SOPs. The fault is cleared by the service;
// the alarm then closes on the next cycle.

const (
	// ExternalAlarmPrefix is the source key prefix of external FDD alarms.
	ExternalAlarmPrefix = "ext:fdd:"
	// externalRefresh is how often the active faults are reloaded.
	externalRefresh = 30 * time.Second
)

// externalState caches the active external alarms per equipment.
type externalState struct {
	alarms   map[int64][]ActiveAlarm
	loadedAt time.Time
	// loaded is set by the first successful load: until then the external
	// alarms restored open at start-up are kept as they are.
	loaded bool
	// failing is set after a load error was logged, so a persistent failure
	// logs once until a load succeeds again.
	failing bool
}

// refreshExternal reloads the active faults at most every externalRefresh. A
// missing analytics schema (database without the AI migration) means no
// external alarms; any other error keeps the previous alarms so a transient
// database error does not close and reopen them (before the first successful
// load: the alarms restored open, see externalAlarms).
func (e *Engine) refreshExternal(ctx context.Context, now time.Time) {
	e.extMu.Lock()
	due := now.Sub(e.ext.loadedAt) >= externalRefresh
	e.extMu.Unlock()
	if !due {
		return
	}
	alarms, err := loadExternalAlarms(ctx, e.db)
	e.extMu.Lock()
	defer e.extMu.Unlock()
	e.ext.loadedAt = now
	if err != nil {
		if undefinedRelation(err) {
			e.ext.alarms, e.ext.loaded, e.ext.failing = nil, true, false
			return
		}
		if !e.ext.failing {
			slog.Warn("load external FDD alarms", "err", err)
		}
		e.ext.failing = true
		return
	}
	e.ext.alarms, e.ext.loaded, e.ext.failing = alarms, true, false
}

// externalAlarms returns the active external alarms of an equipment. Before
// the first successful load (the database failed right after a start) they
// are the external alarms open for it (restored at start-up), so those are
// neither closed nor reopened.
func (e *Engine) externalAlarms(id int64) []ActiveAlarm {
	e.extMu.Lock()
	loaded, alarms := e.ext.loaded, e.ext.alarms[id]
	e.extMu.Unlock()
	if loaded {
		return alarms
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	st := e.states[id]
	if st == nil {
		return nil
	}
	var open []ActiveAlarm
	for key, ev := range st.events {
		if code, ok := strings.CutPrefix(key, ExternalAlarmPrefix); ok {
			open = append(open, ActiveAlarm{Key: key, Type: "ALARM", Name: code, Severity: ev.severity, Message: code})
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Key < open[j].Key })
	return open
}

// loadExternalAlarms reads the ACTIVE faults as alarms keyed by equipment.
func loadExternalAlarms(ctx context.Context, q db.Querier) (map[int64][]ActiveAlarm, error) {
	rows, err := q.Query(ctx, `SELECT rule_code, asset_id, severity, message FROM analytics.faults WHERE status = 'ACTIVE'
		ORDER BY asset_id, rule_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ActiveAlarm{}
	for rows.Next() {
		var code, severity, message string
		var assetID int64
		if err := rows.Scan(&code, &assetID, &severity, &message); err != nil {
			return nil, err
		}
		out[assetID] = append(out[assetID], ExternalAlarm(code, severity, message))
	}
	return out, rows.Err()
}

// ExternalAlarm builds the engine alarm of an external FDD fault.
func ExternalAlarm(code, severity, message string) ActiveAlarm {
	code = strings.TrimSpace(code)
	severity = strings.ToUpper(strings.TrimSpace(severity))
	if severity != "CRITICAL" {
		severity = "WARNING"
	}
	if strings.TrimSpace(message) == "" {
		message = code
	}
	return ActiveAlarm{Key: ExternalAlarmPrefix + code, Type: "ALARM", Name: code, Severity: severity, Message: message}
}

// appendExternal adds the external alarms of an equipment to its evaluation
// result, except while the equipment is offline (no data, nothing to detect).
func appendExternal(res *Result, ext []ActiveAlarm) {
	if res.Status == StatusOffline || len(ext) == 0 {
		return
	}
	res.Alarms = append(res.Alarms, ext...)
}

// undefinedRelation reports a missing table or schema.
func undefinedRelation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000")
}
