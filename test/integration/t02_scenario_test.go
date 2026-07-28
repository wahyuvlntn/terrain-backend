package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A 1x1 PNG.
var pngBytes, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

func png(name string) File { return File{Name: name, Type: "image/png", Content: pngBytes} }

// S holds the ids created by the scenario.
var S struct {
	typeID, brandID, seriesID int64
	codes                     map[string]int64
	root, building, floor     int64
	modelID                   int64
	assetID                   int64
	apiKey                    string
	relationID                int64
	techToken                 string
	orderID                   int64
}

func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func findByName(t *testing.T, list []any, key, name string) map[string]any {
	t.Helper()
	for _, it := range list {
		m := obj(t, it)
		if m[key] == name {
			return m
		}
	}
	t.Fatalf("%q not found in %s", name, truncate(toJSON(list), 500))
	return nil
}

func TestScenario(t *testing.T) {
	step(t, "master data", testMasterData)
	step(t, "locations", testLocations)
	step(t, "model", testModel)
	step(t, "gateway and equipment", testGatewayEquipment)
	step(t, "live data and engine", testLiveData)
	step(t, "issue mapping and early warning", testEarlyWarning)
	step(t, "users and permissions", testUsers)
	step(t, "maintenance order lifecycle", testMaintenanceLifecycle)
	step(t, "notifications", testNotifications)
	step(t, "theme", testTheme)
}

func testMasterData(t *testing.T) {
	ok(t, "master/addtypebrandseries", map[string]any{"table": "types", "field_name": "Chiller"})
	ok(t, "master/addtypebrandseries", map[string]any{"table": "types", "field_name": "Air Handling Unit"})
	ok(t, "master/addtypebrandseries", map[string]any{"table": "brands", "field_name": "Daikin"})
	ok(t, "master/addtypebrandseries", map[string]any{"table": "series", "field_name": "VRV"})
	fails(t, "master/addtypebrandseries", map[string]any{"table": "types", "field_name": "chiller"}) // duplicate
	for _, c := range []string{"E01 - High Pressure", "E02 - Low Refrigerant", "E03 - Compressor Overheat"} {
		ok(t, "master/addtypebrandseries", map[string]any{"table": "alarms", "field_name": c})
	}
	cats := arr(t, ok(t, "master/getdatacategories", map[string]any{"schema": "asset", "table": "", "search_filter": ""}))
	if len(cats) != 4 {
		t.Fatalf("expected 4 equipment categories, got %v", cats)
	}
	types := arr(t, ok(t, "master/getdatafields", map[string]any{"schema": "asset", "table": "types", "category_id": 1, "search_filter": ""}))
	S.typeID = id(t, findByName(t, types, "name", "Chiller")["id"])
	hasKeys(t, obj(t, types[0]), "id", "name", "order_id")
	S.brandID = id(t, findByName(t, arr(t, ok(t, "master/getdatafields", map[string]any{"schema": "asset", "table": "brands", "category_id": 2, "search_filter": ""})), "name", "Daikin")["id"])
	S.seriesID = id(t, findByName(t, arr(t, ok(t, "master/getdatafields", map[string]any{"schema": "asset", "table": "series", "category_id": 3, "search_filter": ""})), "name", "VRV")["id"])
	alarms := arr(t, ok(t, "master/getdatafields", map[string]any{"schema": "dm", "table": "alarms", "category_id": 4, "search_filter": ""}))
	S.codes = map[string]int64{}
	for _, a := range alarms {
		m := obj(t, a)
		S.codes[strings.SplitN(str(t, m["name"]), " ", 2)[0]] = id(t, m["id"])
	}
	if len(S.codes) != 3 || S.codes["E01"] == 0 {
		t.Fatalf("alarm codes not parsed: %v", alarms)
	}
	// Reorder: move the last type to the first position.
	ok(t, "master/updateorder", map[string]any{"schema": "asset", "table": "types", "source_id": id(t, obj(t, types[1])["id"]),
		"target_id": id(t, obj(t, types[0])["id"]), "source_order_id": 2, "target_order_id": 1})
	types2 := arr(t, ok(t, "master/getdatafields", map[string]any{"schema": "asset", "table": "types", "category_id": 1, "search_filter": ""}))
	if obj(t, types2[0])["name"] != obj(t, types[1])["name"] {
		t.Fatalf("reorder did not move entry: %v", types2)
	}
	counts := arr(t, ok(t, "master/getdatasetcount", nil))
	if num(t, findByName(t, counts, "group", "equipment")["count"]) != 7 {
		t.Fatalf("unexpected dataset count %v", counts)
	}
	// Filter (db query) endpoints used by the filter bar.
	status, body := E.DBQ("filter/getalltypes", nil)
	if status != 200 || len(body.([]any)) != 2 {
		t.Fatalf("filter/getalltypes: %d %v", status, body)
	}
}

func testLocations(t *testing.T) {
	ok(t, "location/insertnewlocation", map[string]any{"name": "Singapore Plant", "availability_th": 80, "performance_th": 80, "quality_th": 90,
		"location_image": png("plant.png")})
	S.root = E.scalar(t, `SELECT id FROM asset.locations WHERE name = 'Singapore Plant'`)
	ok(t, "location/insertnewlocation", map[string]any{"name": "Building A1", "parent_id": S.root, "location_image": png("b.png")})
	S.building = E.scalar(t, `SELECT id FROM asset.locations WHERE name = 'Building A1'`)
	ok(t, "location/insertnewlocation", map[string]any{"name": "Floor 01", "parent_id": S.building, "location_image": png("f.png")})
	S.floor = E.scalar(t, `SELECT id FROM asset.locations WHERE name = 'Floor 01'`)
	fails(t, "location/insertnewlocation", map[string]any{"name": "Floor 01", "parent_id": S.building, "location_image": png("f.png")})
	fails(t, "location/insertnewlocation", map[string]any{"name": "No Image Location"})

	tree := arr(t, ok(t, "location/getlocationtreebyfilter", map[string]any{"search_filter": ""}))
	if len(tree) != 1 {
		t.Fatalf("expected one root, got %s", toJSON(tree))
	}
	det := obj(t, ok(t, "location/getlocationdetailsbyid", map[string]any{"location_id": S.root}))
	img, _ := det["image_path"].(string)
	if img == "" {
		img, _ = det["image"].(string)
	}
	if img != "" {
		// Uploaded images are served from /public/.
		path := img
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		resp, err := http.Get(E.http.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("image %s not served: %v %v", path, err, resp)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(b, pngBytes) {
			t.Fatalf("served image differs")
		}
	}
	anc := arr(t, ok(t, "location_overview/getlocationancestors", map[string]any{"location_id": S.floor}))
	if len(anc) != 3 {
		t.Fatalf("ancestors of floor: %v", anc)
	}
}

func testModel(t *testing.T) {
	S.modelID = id(t, ok(t, "model_manager/add_model", map[string]any{"name": "VRV-X", "description": "test model", "type_id": S.typeID,
		"brand_id": S.brandID, "series_id": S.seriesID}))
	fails(t, "model_manager/add_model", map[string]any{"name": "vrv-x", "type_id": S.typeID, "brand_id": S.brandID, "series_id": S.seriesID})
	for _, p := range []map[string]any{
		{"parameter_alias": "temp", "parameter_name": "Temperature", "parameter_unit": "°C", "parameter_data_type": "number", "parameter_scale": 1},
		{"parameter_alias": "pressure", "parameter_name": "Pressure", "parameter_unit": "bar", "parameter_data_type": "number", "parameter_scale": 2},
		{"parameter_alias": "running", "parameter_name": "Running", "parameter_unit": "", "parameter_data_type": "boolean"},
	} {
		p["model_id"] = S.modelID
		ok(t, "model_manager/add_model_parameter", p)
	}
	fails(t, "model_manager/add_model_parameter", map[string]any{"model_id": S.modelID, "parameter_alias": "1bad", "parameter_name": "Bad", "parameter_data_type": "number"})
	params := arr(t, ok(t, "model_manager/get_model_parameters", map[string]any{"model_id": S.modelID}))
	if len(params) != 3 {
		t.Fatalf("parameters: %v", params)
	}
	fails(t, "model_manager/upsert_model_status_definitions", map[string]any{"model_id": S.modelID, "running_script": "running.v ==="})
	ok(t, "model_manager/upsert_model_status_definitions", map[string]any{"model_id": S.modelID, "running_script": "running.v == true"})
	ok(t, "model_manager/add_model_alarm", map[string]any{"model_id": S.modelID, "name": "E01", "criticality": 2, "issue_id": 1,
		"message": "Discharge pressure too high", "script": "pressure.v > 30"})
	ok(t, "model_manager/add_model_alarm", map[string]any{"model_id": S.modelID, "name": "E03", "criticality": 1, "issue_id": 1,
		"message": "Compressor overheating", "script": "temp.v > 80"})
	now := time.Now().UnixMilli()
	values := map[string]any{"running": map[string]any{"v": true, "t": now, "q": true}, "pressure": map[string]any{"v": 35, "t": now, "q": true},
		"temp": map[string]any{"v": 25, "t": now, "q": true}}
	res := obj(t, ok(t, "model_manager/test_status_and_alarms", map[string]any{"model_id": S.modelID, "values": toJSON(values)}))
	if num(t, res["status"]) != 1 {
		t.Fatalf("expected RUNNING in test result: %v", res)
	}
	al := arr(t, res["alarms"])
	if len(al) != 1 || obj(t, al[0])["name"] != "E01" {
		t.Fatalf("expected E01 alarm in test result: %v", res)
	}
	ok(t, "model_manager/update_running_hour_limit", map[string]any{"model_id": S.modelID, "warning_hour": 100, "critical_hour": 200,
		"is_notify_warning": true, "is_notify_critical": true, "is_auto_create_maintenance": true})
	rh := obj(t, ok(t, "model_manager/get_running_hour_limit", map[string]any{"model_id": S.modelID}))
	if num(t, rh["critical_hour"]) != 200 || rh["is_auto_create_maintenance"] != true {
		t.Fatalf("running hour limit: %v", rh)
	}
	// Alarm code ↔ parameter mapping.
	pid := E.scalar(t, `SELECT id FROM asset.model_parameters WHERE model_id = $1 AND alias = 'pressure'`, S.modelID)
	ok(t, "alarm_mapping/save_selected_parameters", map[string]any{"model_id": S.modelID, "alarm_code_id": S.codes["E01"], "parameter_ids": fmt.Sprint(pid)})
	sel := arr(t, ok(t, "alarm_mapping/get_selected_parameters", map[string]any{"model_id": S.modelID, "alarm_code_id": S.codes["E01"]}))
	if len(sel) != 1 || obj(t, sel[0])["id"] != fmt.Sprint(pid) {
		t.Fatalf("selected parameters: %v", sel)
	}
	cm := obj(t, ok(t, "alarm_mapping/get_alarm_codes_mapping", map[string]any{"model_id": S.modelID, "page": 1, "limit": 10, "search": ""}))
	if num(t, cm["mapped_count"]) != 1 || num(t, cm["unmapped_count"]) != 2 {
		t.Fatalf("mapping counts: %v", cm)
	}
}

func testGatewayEquipment(t *testing.T) {
	ok(t, "gateway/addgateway", map[string]any{"alias": "gw_rest", "name": "REST Gateway", "type": "REST_SERVER", "params": "{}"})
	tags := []map[string]any{{"name": "temp", "address": "temp"}, {"name": "pressure", "address": "pressure"}, {"name": "running", "address": "running"}}
	ok(t, "gateway/addgatewaytags", map[string]any{"gateway_alias": "gw_rest", "gateway_tags": toJSON(tags)})
	ok(t, "gateway/turnongateway", map[string]any{"gateway_alias": "gw_rest"})
	var key string
	if err := E.db.QueryRow(E.ctx, `SELECT secrets->>'api_key' FROM dh.gateways WHERE alias = 'gw_rest'`).Scan(&key); err != nil || key == "" {
		t.Fatalf("api key not generated: %v", err)
	}
	S.apiKey = key

	ok(t, "equipment_manager/add_equipment", map[string]any{"alias": "CH_01", "name": "Chiller 01", "model_id": S.modelID, "location_id": S.floor,
		"installation_date": "2024-01-15", "image_file": png("ch.png")})
	fails(t, "equipment_manager/add_equipment", map[string]any{"alias": "CH_01", "name": "Chiller dup", "model_id": S.modelID, "location_id": S.floor})
	S.assetID = E.scalar(t, `SELECT id FROM asset.equipment WHERE alias = 'CH_01'`)
	mappings := map[string]any{}
	for _, a := range []string{"temp", "pressure", "running"} {
		mappings[a] = map[string]any{"gateway_alias": "gw_rest", "tag": a, "is_logged": true, "is_displayed": true}
	}
	ok(t, "equipment_manager/update_equipment_parameter_mapping", map[string]any{"alias": "CH_01", "mappings": toJSON(mappings)})
	eps := ok(t, "equipment_manager/get_equipment_parameters", map[string]any{"alias": "CH_01"})
	if !strings.Contains(toJSON(eps), "gw_rest") {
		t.Fatalf("mapping not visible: %s", truncate(toJSON(eps), 500))
	}
}

func pushValues(t *testing.T, values map[string]any) {
	t.Helper()
	body, _ := json.Marshal(values)
	req, _ := http.NewRequest(http.MethodPost, E.http.URL+"/api/datahub/rest/gw_rest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", S.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("rest ingest: %d %s", resp.StatusCode, raw)
	}
}

func testLiveData(t *testing.T) {
	// Wrong key is rejected.
	req, _ := http.NewRequest(http.MethodPost, E.http.URL+"/api/datahub/rest/gw_rest", strings.NewReader(`{"temp":1}`))
	req.Header.Set("X-Api-Key", "wrong")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("expected 401 for wrong api key, got %v %v", resp, err)
	}
	pushValues(t, map[string]any{"temp": 25.46, "pressure": 35.123, "running": true})
	waitFor(t, "RUNNING status", 15*time.Second, func() bool {
		var n int64
		_ = E.db.QueryRow(E.ctx, `SELECT count(*) FROM ts.asset_status WHERE asset_id = $1 AND status_id = 1 AND end_ts IS NULL`, S.assetID).Scan(&n)
		return n == 1
	})
	waitFor(t, "E01 alarm event", 15*time.Second, func() bool {
		var n int64
		_ = E.db.QueryRow(E.ctx, `SELECT count(*) FROM ts.alarm_events WHERE asset_id = $1 AND end_ts IS NULL AND severity = 'CRITICAL'`, S.assetID).Scan(&n)
		return n == 1
	})
	waitFor(t, "logged parameter values", 15*time.Second, func() bool {
		var n int64
		_ = E.db.QueryRow(E.ctx, `SELECT count(*) FROM ts.parameter_values`).Scan(&n)
		return n >= 3
	})
	vt := ok(t, "equipment_manager/get_equipment_parameters_vtqcs", map[string]any{"alias": "CH_01"})
	if !strings.Contains(toJSON(vt), "35.12") {
		t.Fatalf("vtqc does not contain scaled pressure: %s", truncate(toJSON(vt), 600))
	}
	day := time.Now().In(time.FixedZone("c", 8*3600)).Format("2006-01-02")
	ev := ok(t, "alarm/getalarmevents", map[string]any{"start_ts": day, "end_ts": day, "asset_ids": fmt.Sprintf("{%d}", S.assetID),
		"alarm_type_id": 0, "search_filter": "", "timezone_offset": -480, "page": 1, "limit": 3})
	if !strings.Contains(toJSON(ev), "E01") {
		t.Fatalf("alarm events do not contain E01: %s", truncate(toJSON(ev), 600))
	}
	det := ok(t, "asset/getassetdetails", map[string]any{"asset_id": S.assetID, "timezone_offset": -480})
	if !strings.Contains(strings.ToUpper(toJSON(det)), "CRITICAL") {
		t.Fatalf("asset details should report CRITICAL condition: %s", truncate(toJSON(det), 800))
	}
	// Alarm clears when pressure normalises.
	pushValues(t, map[string]any{"temp": 25.0, "pressure": 20.0, "running": true})
	waitFor(t, "E01 alarm cleared", 15*time.Second, func() bool {
		var n int64
		_ = E.db.QueryRow(E.ctx, `SELECT count(*) FROM ts.alarm_events WHERE asset_id = $1 AND end_ts IS NULL`, S.assetID).Scan(&n)
		return n == 0
	})
}

func testEarlyWarning(t *testing.T) {
	ok(t, "issue_mapping/add_relation", map[string]any{"model_id": S.modelID, "name": "High pressure chain"})
	rel := obj(t, ok(t, "issue_mapping/get_relation_list", map[string]any{"model_id": S.modelID, "search": "", "page": 1, "limit": 10}))
	S.relationID = id(t, obj(t, arr(t, rel["data"])[0])["id"])
	flow := map[string]any{
		"nodes": []any{
			map[string]any{"id": "s1", "type": "source", "position": map[string]any{"x": 0, "y": 0}, "data": map[string]any{"id": S.codes["E01"], "code": "E01", "name": "High Pressure"}, "selected": true},
			map[string]any{"id": "t1", "type": "target", "position": map[string]any{"x": 300, "y": 0}, "data": map[string]any{"id": S.codes["E02"], "code": "E02", "name": "Low Refrigerant"}},
		},
		"edges": []any{map[string]any{"id": "e1", "source": "s1", "target": "t1", "type": "mapping"}},
	}
	fails(t, "issue_mapping/update_issue_relations", map[string]any{"relation_id": S.relationID, "relation_name": "x",
		"flow": `{"nodes":[{"id":"a","type":"bogus"}],"edges":[]}`})
	ok(t, "issue_mapping/update_issue_relations", map[string]any{"relation_id": S.relationID, "relation_name": "High pressure chain", "flow": toJSON(flow)})
	got := obj(t, ok(t, "issue_mapping/get_issue_relations", map[string]any{"relation_id": S.relationID}))
	nodes := arr(t, got["nodes"])
	if len(nodes) != 2 || obj(t, nodes[0])["draggable"] != true {
		t.Fatalf("relation nodes: %v", got)
	}
	// Trigger E01 again: the relation diagnoses E02 and raises a system order.
	pushValues(t, map[string]any{"temp": 25.0, "pressure": 40.0, "running": true})
	waitFor(t, "early warning maintenance order", 20*time.Second, func() bool {
		var n int64
		_ = E.db.QueryRow(E.ctx, `SELECT count(*) FROM dm.maintenance_orders WHERE asset_id = $1 AND from_system`, S.assetID).Scan(&n)
		return n == 1
	})
	day := time.Now().In(time.FixedZone("c", 8*3600)).Format("2006-01-02")
	_ = day
	list := ok(t, "early_warning_diagnostics/get_early_warning_list", map[string]any{"start_time": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano),
		"end_time": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "filter": "{}", "include_compound": true, "include_degradation": true,
		"timezone_offset": -480, "page": 1, "limit": 10, "search": ""})
	item := obj(t, arr(t, obj(t, list)["data"])[0])
	hasKeys(t, item, "id", "model", "current_issues", "timestamp", "severity", "asset_name")
	isoTS(t, item["timestamp"])
	if !strings.Contains(toJSON(list), "E02") && !strings.Contains(toJSON(list), "Low Refrigerant") {
		t.Fatalf("early warning list does not show E02: %s", truncate(toJSON(list), 800))
	}
	pushValues(t, map[string]any{"temp": 25.0, "pressure": 20.0, "running": true})
}

func testUsers(t *testing.T) {
	ok(t, "user_management/addusergroup", map[string]any{"alias": "techs", "name": "Technicians", "description": "Field technicians",
		"locations": toJSON([]any{map[string]any{"id": S.root}})})
	perms := []map[string]any{}
	for _, p := range []string{"read", "update", "report"} {
		perms = append(perms, map[string]any{"permission_group_alias": "maintenanceorder", "permission_alias": p, "value": true})
	}
	perms = append(perms, map[string]any{"permission_group_alias": "equipmentoverview", "permission_alias": "read", "value": true})
	ok(t, "user_management/addusergrouppermission", map[string]any{"alias": "techs", "permissions": toJSON(perms)})
	ok(t, "user_management/adduser", map[string]any{"username": "tech1", "name": "Tech One", "email": "tech1@example.com", "user_id": "EMP001",
		"password": "Tech-Pass1!", "user_group_alias": "techs"})
	tok, err := E.login("tech1", "Tech-Pass1!")
	if err != nil {
		t.Fatal(err)
	}
	S.techToken = tok
	if _, err := E.login("tech1", "wrong"); err == nil {
		t.Fatal("login with a wrong password must fail")
	}
	r := E.JSAs(tok, "user/getuserdetails", nil)
	if r.Status != 200 {
		t.Fatalf("user details for technician: %s", r)
	}
	u := obj(t, r.Data)
	if u["role"] != "basic" {
		t.Fatalf("technician role: %v", u)
	}
	// Technicians cannot reach superuser scripts.
	if r := E.JSAs(tok, "model_manager/get_model_info", map[string]any{"model_id": S.modelID}); r.Status != 403 {
		t.Fatalf("expected 403 for basic user, got %s", r)
	}
	// No session -> 401.
	if r := E.post(scriptURL+"user/getuserdetails", "", nil); r.Status != 401 {
		t.Fatalf("expected 401 without session, got %s", r)
	}
	techs := ok(t, "maintenance_order/get_technicians", map[string]any{"asset_id": S.assetID})
	if !strings.Contains(toJSON(techs), "tech1") {
		t.Fatalf("tech1 should be a technician candidate: %s", truncate(toJSON(techs), 500))
	}
}

func testMaintenanceLifecycle(t *testing.T) {
	start := time.Now().Add(time.Hour).UTC().Truncate(time.Minute)
	ok(t, "maintenance_order/submit", map[string]any{"submit_type": "CREATED", "asset_id": S.assetID, "maintenance_type_id": 1, "priority_id": 1,
		"severity_id": 1, "description": "Check discharge pressure", "planned_start": start.Format(time.RFC3339Nano),
		"planned_end": start.Add(2 * time.Hour).Format(time.RFC3339Nano), "instructions": fmt.Sprint(S.codes["E01"]), "technicians": "tech1",
		"timezone_offset": -480})
	S.orderID = E.scalar(t, `SELECT id FROM dm.maintenance_orders WHERE asset_id = $1 AND NOT from_system ORDER BY id DESC LIMIT 1`, S.assetID)
	var name string
	_ = E.db.QueryRow(E.ctx, `SELECT name FROM dm.maintenance_orders WHERE id = $1`, S.orderID).Scan(&name)
	if !strings.HasPrefix(name, "CM-") {
		t.Fatalf("order name %q should start with CM-", name)
	}
	detail := arr(t, ok(t, "maintenance_order/get_maintenance_order_detail", map[string]any{"order_id": S.orderID, "timezone_offset": -480}))
	if len(detail) != 1 {
		t.Fatalf("order detail: %v", detail)
	}
	orderDetailSchema := schema{"actual_time": "number", "building": "string", "created_at": "string", "description": "string", "equipment": "string",
		"estimated_time": "number", "floor": "string", "id": "number", "maintenance_id": "number|null", "maintenance_type_id": "string",
		"planned_end": "string|null", "planned_start": "string", "priority": "string|null", "repeat_end": "string|null", "repeat_type_id": "string|null",
		"requested_by": "string", "room": "string", "severity": "string", "status": "enum:REQUESTED,REJECTED,IN_PROGRESS,DONE,CLOSED,OPEN,PAUSED",
		"updated_at": "string|null", "instructions": "string", "technicians": "string", "approved_by": "string", "edited_by": "string",
		"reject_reason": "string", "is_started": "boolean", "issue_relation_id": "number|null", "other_alarm_codes": "string"}
	check(t, "order detail", detail[0], orderDetailSchema)
	if d := obj(t, detail[0]); d["status"] != "REQUESTED" || d["technicians"] != "tech1" || d["building"] != fmt.Sprint(S.root) || d["equipment"] != fmt.Sprint(S.assetID) {
		t.Fatalf("order detail values: %v", d)
	}
	ok(t, "maintenance_order/submit", map[string]any{"submit_type": "APPROVED", "id": S.orderID, "asset_id": S.assetID, "maintenance_type_id": 1,
		"priority_id": 1, "severity_id": 1, "description": "Check discharge pressure", "planned_start": start.Format(time.RFC3339Nano),
		"planned_end": start.Add(2 * time.Hour).Format(time.RFC3339Nano), "instructions": fmt.Sprint(S.codes["E01"]), "technicians": "tech1",
		"timezone_offset": -480})
	if st := E.scalar(t, `SELECT status_id FROM dm.maintenance_orders WHERE id = $1`, S.orderID); st != 3 {
		t.Fatalf("approved order status = %d, want 3 (open)", st)
	}
	tech := func(script string, form map[string]any) any {
		t.Helper()
		if form == nil {
			form = map[string]any{}
		}
		form["maintenance_id"] = S.orderID
		r := E.JSAs(S.techToken, "maintenance_order_work_report/"+script, form)
		if r.Status != 200 || r.Error != "" {
			t.Fatalf("%s as technician: %s", script, r)
		}
		return r.Data
	}
	wr := obj(t, tech("get_work_report", nil))
	_ = wr
	tech("start_work_report", map[string]any{"is_status_change": true})
	if st := E.scalar(t, `SELECT status_id FROM dm.maintenance_orders WHERE id = $1`, S.orderID); st != 4 {
		t.Fatalf("started order status = %d, want 4 (in progress)", st)
	}
	tech("pause_work_report", map[string]any{"reason": "Waiting for spare part"})
	tech("resume_work_report", nil)
	tech("add_work_update", map[string]any{"note": "Replaced pressure switch", "issue_ids": fmt.Sprint(S.codes["E01"])})
	tech("set_work_report_status_to_done", nil)
	if st := E.scalar(t, `SELECT status_id FROM dm.maintenance_orders WHERE id = $1`, S.orderID); st != 5 {
		t.Fatalf("done order status = %d, want 5", st)
	}
	r := E.JS("maintenance_order_work_report/close_work_report", map[string]any{"maintenance_id": S.orderID})
	if r.Status != 200 {
		t.Fatalf("close as superuser: %s", r)
	}
	if st := E.scalar(t, `SELECT status_id FROM dm.maintenance_orders WHERE id = $1`, S.orderID); st != 6 {
		t.Fatalf("closed order status = %d, want 6", st)
	}
	day := time.Now().In(time.FixedZone("c", 8*3600)).Format("2006-01-02")
	list := ok(t, "maintenance_order/get_maintenance_order_list", map[string]any{"start_date": day, "end_date": day, "timezone_offset": -480, "page": 1, "limit": 20})
	if !strings.Contains(toJSON(list), name) {
		t.Fatalf("order %s missing from list: %s", name, truncate(toJSON(list), 600))
	}
	ok(t, "maintenance_order/get_calendar_monthly", map[string]any{"date": day, "month": time.Now().Month(), "year": time.Now().Year(), "timezone_offset": -480})
	// KPI refresh and performance endpoints.
	if err := E.srv.RefreshKPIs(E.ctx); err != nil {
		t.Fatal(err)
	}
	ok(t, "maintenance_performance/get_mtbf_mttr_lastupdated", nil)
}

func testNotifications(t *testing.T) {
	n := obj(t, ok(t, "notification/getnotifications", map[string]any{"page": 1, "limit": 50, "search": "", "notif_type": "all"}))
	list := arr(t, n["notifications"])
	if len(list) == 0 {
		t.Fatal("expected notifications after alarms and maintenance events")
	}
	first := obj(t, list[0])
	hasKeys(t, first, "id", "category", "message", "message_html", "asset_id", "location_id", "parent_id", "location_name", "timestamp",
		"additional_data", "is_read", "ancestors")
	isoTS(t, first["timestamp"])
	unread := num(t, ok(t, "notification/getunreadnotificationcounts", nil))
	if unread == 0 {
		t.Fatal("expected unread notifications")
	}
	ok(t, "notification/updatenotifreadstatus", map[string]any{"notification_id": first["id"], "is_read": true})
	if num(t, ok(t, "notification/getunreadnotificationcounts", nil)) != unread-1 {
		t.Fatal("unread count did not decrease")
	}
	ok(t, "notification/readallnotif", map[string]any{"username": adminUser})
	if num(t, ok(t, "notification/getunreadnotificationcounts", nil)) != 0 {
		t.Fatal("read all did not clear unread count")
	}
	// The technician only sees notifications of permitted categories.
	r := E.JSAs(S.techToken, "notification/getnotifications", map[string]any{"page": 1, "limit": 50})
	if r.Status != 200 {
		t.Fatalf("technician notifications: %s", r)
	}
	for _, it := range arr(t, obj(t, r.Data)["notifications"]) {
		cat := str(t, obj(t, it)["category"])
		if strings.HasPrefix(cat, "ALARM_") || strings.HasPrefix(cat, "LICENSE") {
			t.Fatalf("technician must not see %s notifications", cat)
		}
	}
	// Notification manager.
	ok(t, "notification/registeremailnotification", map[string]any{"alias": "epm_email_notification", "interval": 5, "is_active": false,
		"is_secure": false, "name": "EPM Email Notification", "sender_email": "noreply@example.com", "sender_name": "EPM", "sender_password": "x",
		"smtp_host": "smtp.example.com", "smtp_port": 587})
	d := obj(t, ok(t, "notification/getemailserverdetails", map[string]any{"alias": "epm_email_notification"}))
	if d["smtp_host"] != "smtp.example.com" || d["recurring_type"] != "minute" {
		t.Fatalf("email details: %v", d)
	}
	ok(t, "notification/registerusergrouptoemailnotification", map[string]any{"input": toJSON([]any{
		map[string]any{"category_id": 22, "notif_alias": "epm_email_notification", "type_id": 1, "ug_alias": "techs"}})})
	asg := arr(t, ok(t, "notification/getassignedcategoriesperusergroup", map[string]any{"notification_alias": "epm_email_notification", "type_id": 1, "ug_alias": "techs"}))
	if len(asg) != 1 {
		t.Fatalf("assigned categories: %v", asg)
	}
	groups := arr(t, ok(t, "notification/getusergroups", map[string]any{"notification_alias": "epm_email_notification", "search": ""}))
	g := findByName(t, groups, "alias", "techs")
	if num(t, g["category_count"]) != 1 || num(t, g["user_count"]) != 1 {
		t.Fatalf("group summary: %v", g)
	}
	ok(t, "notification/submitunsubscribedusers", map[string]any{"selected_usernames": "[]", "type_id": 1, "unselected_usernames": `[{"username":"tech1"}]`})
	if len(arr(t, ok(t, "notification/getunsubscribedusers", map[string]any{"type_id": 1, "ug_alias": "techs"}))) != 1 {
		t.Fatal("tech1 should be unsubscribed")
	}
}

func testTheme(t *testing.T) {
	// Public (no session) – used by the sign-in page.
	r := E.post(scriptURL+"theme/getactivetheme", "", nil)
	if r.Status != 200 {
		t.Fatalf("getactivetheme without session: %s", r)
	}
	at := obj(t, r.Data)
	if num(t, at["theme_id"]) != 1 || len(arr(t, at["colors"])) < 100 {
		t.Fatalf("default theme: %v", truncate(toJSON(at), 300))
	}
	colors := []any{}
	for _, c := range arr(t, at["colors"]) {
		m := obj(t, c)
		colors = append(colors, map[string]any{"name": m["name"], "hex_code": m["hex_code"]})
	}
	ok(t, "theme/addtheme", map[string]any{"name": "Ocean", "is_surface_secondary_used": false, "is_surface_tertiary_used": false, "colors": toJSON(colors)})
	themes := obj(t, ok(t, "theme/getthemes", map[string]any{"page": 1, "search": "", "limit": 12}))
	ocean := findByName(t, arr(t, themes["data"]), "name", "Ocean")
	if len(arr(t, ocean["colors"])) != 4 || ocean["is_editable"] != true {
		t.Fatalf("theme card: %v", ocean)
	}
	oid := id(t, ocean["id"])
	ok(t, "theme/setactivetheme", map[string]any{"theme_id": oid, "logo": png("logo.png"), "delete_logo": false})
	at2 := obj(t, E.post(scriptURL+"theme/getactivetheme", "", nil).Data)
	logo, _ := at2["logo"].(string)
	if id(t, at2["theme_id"]) != oid || !strings.HasPrefix(logo, "/public/") {
		t.Fatalf("active theme after update: %v", truncate(toJSON(at2), 300))
	}
	resp, err := http.Get(E.http.URL + logo)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("logo not served: %v %v", err, resp)
	}
	resp.Body.Close()
	fails(t, "theme/deletetheme", map[string]any{"theme_id": 1})
	ok(t, "theme/deletetheme", map[string]any{"theme_id": oid})
	at3 := obj(t, E.post(scriptURL+"theme/getactivetheme", "", nil).Data)
	if num(t, at3["theme_id"]) != 1 {
		t.Fatalf("deleting the active theme should fall back to the default: %v", at3["theme_id"])
	}
}
