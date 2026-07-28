package integration

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSmokeFullDatabase calls every read-only script with ids of the data
// created by TestScenario (and several input variants) and fails on any
// server error.
func TestSmokeFullDatabase(t *testing.T) {
	if S.assetID == 0 {
		t.Skip("requires TestScenario data")
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	variants := []map[string]any{
		{},
		{"location_ids": fmt.Sprintf("{%d}", S.root), "asset_ids": fmt.Sprintf("{%d}", S.assetID)},
		{"location_ids": "{}", "asset_ids": "{}"},
		{"filter": toJSON(map[string]any{"location_ids": []any{S.root}, "asset_ids": []any{S.assetID}}), "period": "monthly"},
		{"start_ts": now.Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano), "end_ts": now.UTC().Format(time.RFC3339Nano),
			"start_time": now.Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano), "end_time": now.UTC().Format(time.RFC3339Nano)},
		{"search": "chiller", "search_filter": "chiller", "sort_state": `{"name":true}`},
	}
	base := genericForm()
	for k, v := range map[string]any{"id": S.orderID, "asset_id": S.assetID, "equipment_id": S.assetID, "location_id": S.floor,
		"model_id": S.modelID, "order_id": S.orderID, "maintenance_id": S.orderID, "relation_id": S.relationID,
		"alarm_code_id": S.codes["E01"], "alias": "CH_01", "gateway_alias": "gw_rest", "username": "tech1", "ug_alias": "techs",
		"date": day, "building_id": S.root, "floor_id": S.building, "room_id": S.floor} {
		base[k] = v
	}
	routes := E.srv.Routes()
	sort.Strings(routes)
	var failures []string
	calls := 0
	for _, r := range routes {
		if isDestructive(r) {
			continue
		}
		for _, v := range variants {
			form := map[string]any{}
			for k, x := range base {
				form[k] = x
			}
			for k, x := range v {
				form[k] = x
			}
			res := E.JS(r, form)
			calls++
			if res.Status >= 500 || res.Status < 0 {
				failures = append(failures, r+": "+res.String())
				break
			}
		}
	}
	t.Logf("%d calls", calls)
	if len(failures) > 0 {
		t.Fatalf("%d scripts failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}
