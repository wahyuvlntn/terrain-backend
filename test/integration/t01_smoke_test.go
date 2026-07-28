package integration

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// genericForm is a parameter set touching the common inputs of most scripts.
func genericForm() map[string]any {
	now := time.Now()
	day := now.Format("2006-01-02")
	week := now.AddDate(0, 0, -7).Format("2006-01-02")
	return map[string]any{
		"id": 1, "asset_id": 1, "equipment_id": 1, "location_id": 1, "model_id": 1, "order_id": 1, "maintenance_order_id": 1,
		"user_id": 1, "alarm_id": 1, "alarm_code_id": 1, "relation_id": 1, "instruction_id": 1, "gateway_id": 1, "personnel_id": 1,
		"category_id": 1, "type_id": 1, "parameter_id": 1, "schedule_id": 1, "issue_id": 1,
		"page": 1, "limit": 10, "search": "", "timezone_offset": -480,
		"start_date": week, "end_date": day, "date": day, "start": week, "end": day, "from": week, "to": day,
		"start_ts": week + "T00:00:00+08:00", "end_ts": day + "T00:00:00+08:00",
		"schema": "asset", "table": "types", "notification_alias": "epm_email_notification", "alias": "epm_email_notification",
	}
}

var destructive = []string{"delete", "remove", "logout", "password", "reset", "restore", "setactive", "set_active", "import", "upload",
	"update", "add", "create", "save", "submit", "register", "approve", "reject", "start", "pause", "resume", "done", "close", "sync", "duplicate",
	"upsert", "change", "assign", "read", "bulk", "test_", "verify", "edit", "set_", "generate"}

func isDestructive(route string) bool {
	script := route[strings.LastIndex(route, "/")+1:]
	for _, d := range destructive {
		if strings.Contains(script, d) {
			// read-only "get..." scripts are never destructive
			if strings.HasPrefix(script, "get") || strings.HasPrefix(script, "check") {
				return false
			}
			return true
		}
	}
	return false
}

// smoke calls every registered script with generic inputs and reports server
// errors (5xx). Client errors are expected for missing/invalid inputs.
func smoke(t *testing.T, includeDestructive bool) {
	t.Helper()
	routes := E.srv.Routes()
	sort.Strings(routes)
	var failures []string
	hist := map[int]int{}
	defer func() { t.Logf("status histogram: %v", hist) }()
	for _, r := range routes {
		if !includeDestructive && isDestructive(r) {
			continue
		}
		res := E.JS(r, genericForm())
		hist[res.Status]++
		if res.Status >= 500 || res.Status < 0 {
			failures = append(failures, r+": "+res.String())
		}
	}
	for _, q := range E.srv.DBRoutes() {
		status, body := E.DBQ(q, genericForm())
		if status != 200 {
			failures = append(failures, "db:"+q+": status "+itoa(status)+" "+truncate(toJSON(body), 300))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d scripts failed with server errors:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}

func TestSmokeEmptyDatabase(t *testing.T) {
	// On an empty database even destructive scripts must fail cleanly.
	smoke(t, true)
}
