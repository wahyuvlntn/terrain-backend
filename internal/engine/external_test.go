package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"afdd/internal/db"
)

func TestExternalAlarm(t *testing.T) {
	a := ExternalAlarm(" CHL-011 ", "critical", "CHW leaving temperature sensor reads 1.6 K high")
	if a.Key != "ext:fdd:CHL-011" || a.Type != "ALARM" || a.Name != "CHL-011" || a.Severity != "CRITICAL" || a.DefID != 0 {
		t.Fatalf("alarm %+v", a)
	}
	if b := ExternalAlarm("CT-014", "info", ""); b.Severity != "WARNING" || b.Message != "CT-014" {
		t.Fatalf("defaults %+v", b)
	}
}

func TestAppendExternal(t *testing.T) {
	ext := []ActiveAlarm{ExternalAlarm("CHS-005", "WARNING", "riser DP sensor bias")}

	res := Result{Status: StatusRunning}
	appendExternal(&res, ext)
	if len(res.Alarms) != 1 || res.Condition() != CondWarning {
		t.Fatalf("running unit: alarms %v condition %s", res.Alarms, res.Condition())
	}

	// An offline (or unlicensed) unit raises nothing: it would be detected on stale data.
	off := Result{Status: StatusOffline}
	appendExternal(&off, ext)
	if len(off.Alarms) != 0 || off.Condition() != CondOffline {
		t.Fatalf("offline unit: %+v", off)
	}

	// External alarms join the model alarms; a critical one sets the condition.
	both := Result{Status: StatusIdle, Alarms: []ActiveAlarm{{Key: "alarm:1", Type: "ALARM", Name: "CHL-001", Severity: "WARNING"}}}
	appendExternal(&both, []ActiveAlarm{ExternalAlarm("CHL-011", "CRITICAL", "")})
	if len(both.Alarms) != 2 || both.Condition() != CondCritical {
		t.Fatalf("combined: %+v", both)
	}
}

func TestExternalCodeResolves(t *testing.T) {
	c := AlarmCode{ID: 42, Code: "CHS-008", Name: "CW Supply Temperature Loop Hunting (PI Tuning)"}
	ix := &AlarmCodeIndex{byKey: map[string]int64{lower(c.Code): c.ID, lower(c.Code + "-" + c.Name): c.ID, lower(c.Name): c.ID},
		Info: map[int64]AlarmCode{c.ID: c}}
	if id, ok := ix.Resolve(ExternalAlarm("CHS-008", "WARNING", "").Name); !ok || id != 42 {
		t.Fatalf("rule code must resolve to its alarm code: %d %v", id, ok)
	}
}

// TestExternalAlarmsBeforeFirstLoad: while no load has succeeded since the
// start (the database fails), the external alarms restored open are kept, so
// they are neither closed nor reopened; once a load succeeds, the loaded
// faults rule.
func TestExternalAlarmsBeforeFirstLoad(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://afdd:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // every query fails
	e := New(&db.DB{Pool: pool}, nil, nil, Options{})
	e.stateMu.Lock()
	st := e.stateFor(7)
	st.events["ext:fdd:CT-014"] = &openEvent{id: 1, severity: "CRITICAL"}
	st.events["alarm:3"] = &openEvent{id: 2, severity: "WARNING"}
	e.stateMu.Unlock()

	now := time.Now()
	for i := 0; i < 2; i++ { // the first load and a retry 30 s later fail
		e.refreshExternal(ctx, now.Add(time.Duration(i)*externalRefresh))
		got := e.externalAlarms(7)
		if len(got) != 1 || got[0].Key != "ext:fdd:CT-014" || got[0].Name != "CT-014" || got[0].Severity != "CRITICAL" {
			t.Fatalf("restored external alarms after %d failed loads: %+v", i+1, got)
		}
		res := Result{Status: StatusRunning}
		appendExternal(&res, got)
		if res.Condition() != CondCritical {
			t.Fatalf("condition %s, want the restored alarm to hold CRITICAL", res.Condition())
		}
	}
	if got := e.externalAlarms(8); len(got) != 0 {
		t.Fatalf("equipment without restored alarms: %+v", got)
	}

	// After a successful load (no active fault) the alarm closes.
	e.extMu.Lock()
	e.ext.alarms, e.ext.loaded, e.ext.failing = map[int64][]ActiveAlarm{}, true, false
	e.extMu.Unlock()
	if got := e.externalAlarms(7); len(got) != 0 {
		t.Fatalf("after a load: %+v", got)
	}
	// A later failure keeps the last loaded alarms (none), not the restored ones.
	e.refreshExternal(ctx, now.Add(5*externalRefresh))
	if got := e.externalAlarms(7); len(got) != 0 || !e.ext.loaded {
		t.Fatalf("after a later failure: %+v", got)
	}
}

func TestUndefinedRelation(t *testing.T) {
	missing := fmt.Errorf("query: %w", &pgconn.PgError{Code: "42P01", Message: `relation "analytics.faults" does not exist`})
	if !undefinedRelation(missing) || !undefinedRelation(&pgconn.PgError{Code: "3F000"}) {
		t.Fatal("missing table / schema must be recognised")
	}
	if undefinedRelation(&pgconn.PgError{Code: "57014"}) || undefinedRelation(errors.New("x")) {
		t.Fatal("other errors are real failures")
	}
}
