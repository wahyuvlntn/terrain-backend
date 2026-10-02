package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"afdd/internal/config"
	"afdd/internal/engine"
)

func TestPlantOverviewCounts(t *testing.T) {
	// 1 -> 2 -> 3, and 4 on its own
	tree := &locTree{nodes: map[int64]*locNode{
		1: {ID: 1, Children: []int64{2}},
		2: {ID: 2, Children: []int64{3}},
		3: {ID: 3},
		4: {ID: 4},
	}, roots: []int64{1, 4}}
	byLoc := map[int64][]int64{1: {10}, 3: {11, 12}, 4: {13}}

	if got := len(assetsUnder(tree, byLoc, 2)); got != 2 {
		t.Fatalf("assets under 2 = %d, want 2", got)
	}
	if got := len(assetsUnder(tree, byLoc, 1, 4)); got != 4 {
		t.Fatalf("assets under 1 and 4 = %d, want 4", got)
	}

	states := map[int64]*currentState{
		10: {Status: engine.StatusRunning, Condition: engine.CondNormal},
		11: {Status: engine.StatusUnplannedDown, Condition: engine.CondCritical},
		12: {Status: engine.StatusIdle, Condition: engine.CondWarning},
		// 13 has no state: offline without a condition
	}
	st := countStates(assetsUnder(tree, byLoc, 1, 4), states)
	if st.total != 4 || st.running != 1 || st.unplanned != 1 || st.idle != 1 || st.offline != 1 {
		t.Fatalf("statuses = %+v", st)
	}
	if st.normal != 1 || st.warning != 1 || st.critical != 1 {
		t.Fatalf("conditions = %+v", st)
	}

	o := &plantOrders{}
	o.addStatus("IN_PROGRESS", 2)
	o.addStatus("CLOSED", 3)
	o.add(&plantOrders{Requested: 1, Total: 1})
	if o.InProgress != 2 || o.Closed != 3 || o.Requested != 1 || o.Total != 6 {
		t.Fatalf("orders = %+v", o)
	}
}

func TestDemoSessionNeedsDemoUser(t *testing.T) {
	// demo mode off: the endpoint does not exist
	off := &Server{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	off.serveDemoSession(rec, httptest.NewRequest(http.MethodPost, "/api/user-management/exposed/demo-session", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("demo mode off: status %d, want 404", rec.Code)
	}
	// demo mode on: only POST opens a session
	on := &Server{cfg: &config.Config{DemoUser: "superuser"}}
	rec = httptest.NewRecorder()
	on.serveDemoSession(rec, httptest.NewRequest(http.MethodGet, "/api/user-management/exposed/demo-session", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d, want 405", rec.Code)
	}
}
