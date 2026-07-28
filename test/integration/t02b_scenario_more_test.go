package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"afdd/internal/license"
)

// TestScenarioMore continues on the data created by TestScenario.
func TestScenarioMore(t *testing.T) {
	if S.assetID == 0 {
		t.Skip("requires TestScenario data")
	}
	step(t, "maintenance instructions", testInstructions)
	step(t, "personnel", testPersonnel)
	step(t, "licenses", testLicenses)
	step(t, "updates and deletes", testUpdatesAndDeletes)
	step(t, "scheduler jobs", testSchedulerJobs)
}

func testInstructions(t *testing.T) {
	// PDF upload returns the stored file URL.
	pdf := File{Name: "manual.pdf", Type: "application/pdf", Content: []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF")}
	up := ok(t, "maintenance_instruction/upload_pdf", map[string]any{"pdf": pdf})
	if !strings.Contains(toJSON(up), "/public/") && !strings.Contains(toJSON(up), "instructions") {
		t.Fatalf("upload_pdf result: %v", up)
	}
	fails(t, "maintenance_instruction/upload_pdf", map[string]any{"pdf": png("not-a.pdf")})

	ok(t, "maintenance_instruction/create_instruction", map[string]any{"alarm_code_id": S.codes["E03"], "title": "Check compressor cooling",
		"instruction_type_id": 1, "duration": 30, "description": "Diagnose overheating", "content": "<p>Inspect the fan</p>",
		"model_ids": fmt.Sprintf("{%d}", S.modelID)})
	ok(t, "maintenance_instruction/create_instruction", map[string]any{"alarm_code_id": S.codes["E03"], "title": "Replace fan motor",
		"instruction_type_id": 2, "duration": 45, "description": "Repair", "content": "<p>Replace</p>", "model_ids": fmt.Sprintf("{%d}", S.modelID)})
	fails(t, "maintenance_instruction/create_instruction", map[string]any{"title": "", "instruction_type_id": 1, "model_ids": fmt.Sprintf("{%d}", S.modelID)})

	list := ok(t, "maintenance_instruction/get_instructions", map[string]any{"search": "", "page": 1, "limit": 10})
	if !strings.Contains(toJSON(list), "Check compressor cooling") {
		t.Fatalf("instruction list: %s", truncate(toJSON(list), 600))
	}
	iid := E.scalar(t, `SELECT id FROM dm.instructions WHERE title = 'Check compressor cooling'`)
	det := ok(t, "maintenance_instruction/get_instruction_detail", map[string]any{"id": iid})
	if !strings.Contains(toJSON(det), "Inspect the fan") {
		t.Fatalf("instruction detail: %s", truncate(toJSON(det), 600))
	}
	ok(t, "maintenance_instruction/update_instruction", map[string]any{"id": iid, "alarm_code_id": S.codes["E03"], "title": "Check compressor cooling",
		"instruction_type_id": 1, "duration": 40, "description": "Diagnose overheating", "content": "<p>Inspect the fan and coil</p>",
		"model_ids": fmt.Sprintf("{%d}", S.modelID)})

	// The issue options of the order form now include E03 with 85 minutes.
	opts := arr(t, ok(t, "maintenance_order/get_maintenance_instructions", map[string]any{"asset_id": S.assetID}))
	checkAll(t, "issue options", opts, schema{"label": "string", "value": "string", "estimated_time_minute": "number", "is_diagnostic": "boolean", "is_repair": "boolean"})
	var e03 map[string]any
	for _, o := range opts {
		if m := obj(t, o); m["value"] == fmt.Sprint(S.codes["E03"]) {
			e03 = m
		}
	}
	if e03 == nil || num(t, e03["estimated_time_minute"]) != 85 || e03["is_diagnostic"] != true || e03["is_repair"] != true {
		t.Fatalf("E03 option: %v", opts)
	}
	ok(t, "maintenance_instruction/delete_instruction", map[string]any{"id": E.scalar(t, `SELECT id FROM dm.instructions WHERE title = 'Replace fan motor'`)})
}

func testPersonnel(t *testing.T) {
	det := obj(t, ok(t, "personnel_management/get_personnel_details", map[string]any{"id": "tech1"}))
	if !strings.Contains(toJSON(det), "Tech One") {
		t.Fatalf("personnel details: %v", det)
	}
	ok(t, "personnel_management/update_personnel_details", map[string]any{"id": "tech1",
		"assignedBuildingIds": toJSON([]string{fmt.Sprint(S.root)}), "assignedEquipmentTypeIds": toJSON([]string{fmt.Sprint(S.typeID)})})
	det = obj(t, ok(t, "personnel_management/get_personnel_details", map[string]any{"id": "tech1"}))
	if toJSON(det["assignedBuildingIds"]) != toJSON([]string{fmt.Sprint(S.root)}) || toJSON(det["assignedEquipmentTypeIds"]) != toJSON([]string{fmt.Sprint(S.typeID)}) {
		t.Fatalf("assigned building not visible: %s", truncate(toJSON(det), 600))
	}
	techs := ok(t, "maintenance_order/get_technicians", map[string]any{"building_id": S.root, "equipment_id": S.assetID, "maintenance_id": 0})
	if !strings.Contains(toJSON(techs), "tech1") {
		t.Fatalf("technician filtered out for own building/type: %s", toJSON(techs))
	}
	other := E.scalar(t, `INSERT INTO asset.locations (name) VALUES ('Other Site') RETURNING id`)
	techs = ok(t, "maintenance_order/get_technicians", map[string]any{"building_id": other, "equipment_id": S.assetID, "maintenance_id": 0})
	if strings.Contains(toJSON(techs), "tech1") {
		t.Fatalf("technician of another building offered: %s", toJSON(techs))
	}
	E.exec(t, `DELETE FROM asset.locations WHERE id = $1`, other)
}

func testLicenses(t *testing.T) {
	_, privB64, err := license.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := license.DecodePrivateKey(privB64)
	issue := func(c license.Content) File {
		s, err := license.Issue(priv, c)
		if err != nil {
			t.Fatal(err)
		}
		return File{Name: c.Key + ".lic", Type: "application/octet-stream", Content: []byte(s)}
	}
	exp := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	lic := issue(license.Content{Key: "LIC-EQ-1", Type: license.TypeEquipment, AssetKeys: []string{"a1", "a2"}, ExpiredAt: exp, Description: "two units"})
	v := obj(t, ok(t, "license/verify_license", map[string]any{"license_file": lic}))
	if v["key"] != "LIC-EQ-1" {
		t.Fatalf("verify: %v", v)
	}
	ok(t, "license/upload_license", map[string]any{"license_name": "Plant licenses", "license_file": lic})
	fails(t, "license/upload_license", map[string]any{"license_name": "Again", "license_file": lic})
	expired := issue(license.Content{Key: "LIC-OLD", Type: license.TypeEquipment, AssetKeys: []string{"x"}, ExpiredAt: time.Now().Add(-time.Hour).UnixMilli()})
	fails(t, "license/upload_license", map[string]any{"license_name": "Old", "license_file": expired})
	fails(t, "license/upload_license", map[string]any{"license_name": "Garbage", "license_file": File{Name: "x.lic", Type: "text/plain", Content: []byte("garbage")}})

	lics := arr(t, ok(t, "license/get_licenses", nil))
	l := findByName(t, lics, "key", "LIC-EQ-1")
	check(t, "license", l, schema{"key": "string", "name": "string", "type": "string", "total": "number", "used": "number", "is_active": "boolean",
		"status_message": "string", "description": "string|null", "expired_at": "number|null"})
	if num(t, l["total"]) != 2 || num(t, l["used"]) != 0 || l["is_active"] != true {
		t.Fatalf("license row: %v", l)
	}
	licID := E.scalar(t, `SELECT id FROM core.licenses WHERE key = 'LIC-EQ-1'`)
	ok(t, "equipment_manager/update_equipment_temp", map[string]any{"alias": "CH_01", "name": "Chiller 01", "model_id": S.modelID,
		"location_id": S.floor, "license_id": licID})
	info := obj(t, ok(t, "license/get_license_information", map[string]any{"license_key": "LIC-EQ-1"}))
	if len(arr(t, info["used"])) != 1 {
		t.Fatalf("license information: %v", info)
	}
	fails(t, "license/delete_license", map[string]any{"license_key": "LIC-EQ-1"})
	ok(t, "license/update_license", map[string]any{"license_key": "LIC-EQ-1", "license_name": "Plant licenses (renamed)"})
	// Release and delete.
	ok(t, "equipment_manager/update_equipment_temp", map[string]any{"alias": "CH_01", "name": "Chiller 01", "model_id": S.modelID,
		"location_id": S.floor, "license_id": 0})
	ok(t, "license/delete_license", map[string]any{"license_key": "LIC-EQ-1"})
}

func testUpdatesAndDeletes(t *testing.T) {
	// Locations: rename, cannot delete while equipment is attached.
	ok(t, "location/updatelocation", map[string]any{"location_id": S.floor, "name": "Floor 01 East", "parent_id": S.building})
	if n := E.scalar(t, `SELECT count(*) FROM asset.locations WHERE name = 'Floor 01 East'`); n != 1 {
		t.Fatal("location not renamed")
	}
	fails(t, "location/deletelocation", map[string]any{"location_id": S.floor})

	// Models in use cannot be deleted; an unused one can.
	fails(t, "model_manager/delete_model", map[string]any{"model_id": S.modelID})
	tmp := id(t, ok(t, "model_manager/add_model", map[string]any{"name": "Temp Model", "type_id": S.typeID, "brand_id": S.brandID, "series_id": S.seriesID}))
	ok(t, "model_manager/delete_model", map[string]any{"model_id": tmp})

	// Master data in use cannot be deleted.
	fails(t, "master/deleteentry", map[string]any{"schema": "asset", "table": "types", "id": S.typeID})
	fails(t, "master/deleteentry", map[string]any{"schema": "dm", "table": "alarms", "id": S.codes["E01"]})
	ok(t, "master/updatename", map[string]any{"schema": "asset", "table": "series", "id": S.seriesID, "updated_name": "VRV IV", "updated_issue_id": ""})

	// User group with users cannot be deleted; user update and delete work.
	fails(t, "user_management/deleteusergroup", map[string]any{"alias": "techs"})
	ok(t, "user_management/adduser", map[string]any{"username": "temp_user", "name": "Temp", "email": "temp@example.com", "user_id": "EMP999",
		"password": "Temp-Pass1!", "user_group_alias": "techs"})
	ok(t, "user_management/deleteuser", map[string]any{"username": "temp_user"})
	if _, err := E.login("temp_user", "Temp-Pass1!"); err == nil {
		t.Fatal("deleted user can still log in")
	}

	// Deleting a requested order works; a closed one cannot be deleted by a technician.
	sysOrder := E.scalar(t, `SELECT id FROM dm.maintenance_orders WHERE from_system ORDER BY id LIMIT 1`)
	r := E.JSAs(S.techToken, "maintenance_order/delete_maintenance_order", map[string]any{"id": S.orderID, "maintenance_id": S.orderID, "order_id": S.orderID})
	if r.Status == 200 {
		t.Fatalf("technician deleted an order: %s", r)
	}
	ok(t, "maintenance_order/delete_maintenance_order", map[string]any{"id": sysOrder, "maintenance_id": sysOrder, "order_id": sysOrder})
	if n := E.scalar(t, `SELECT count(*) FROM dm.maintenance_orders WHERE id = $1`, sysOrder); n != 0 {
		t.Fatal("order not deleted")
	}
}

func testSchedulerJobs(t *testing.T) {
	for _, j := range E.sched.Jobs() {
		start := time.Now()
		E.sched.RunOnce(E.ctx, j)
		if time.Since(start) > 30*time.Second {
			t.Errorf("job %s took %s", j.Name, time.Since(start))
		}
	}
	// Running hour limits: force 250 running hours since the last completed
	// order and expect the critical notification and an automatic preventive order.
	E.exec(t, `UPDATE dm.maintenance_orders SET completed_at = now() - interval '300 hours' WHERE completed_at IS NOT NULL`)
	E.exec(t, `DELETE FROM ts.running_hour_flags`)
	E.exec(t, `INSERT INTO ts.asset_status (asset_id, status_id, start_ts, end_ts) VALUES ($1, 1, now() - interval '260 hours', now() - interval '10 hours')`, S.assetID)
	for _, j := range E.sched.Jobs() {
		if j.Name == "running-hours" {
			E.sched.RunOnce(E.ctx, j)
		}
	}
	if n := E.scalar(t, `SELECT count(*) FROM core.notifications WHERE category = 'RUNNING_HOUR_CRITICAL'`); n != 1 {
		t.Fatalf("running hour critical notifications = %d", n)
	}
	if n := E.scalar(t, `SELECT count(*) FROM dm.maintenance_orders WHERE from_system AND maintenance_type_id = 2 AND asset_id = $1`, S.assetID); n != 1 {
		t.Fatalf("automatic preventive orders = %d", n)
	}
	// Running the job again does not duplicate.
	for _, j := range E.sched.Jobs() {
		if j.Name == "running-hours" {
			E.sched.RunOnce(E.ctx, j)
		}
	}
	if n := E.scalar(t, `SELECT count(*) FROM core.notifications WHERE category = 'RUNNING_HOUR_CRITICAL'`); n != 1 {
		t.Fatalf("duplicate running hour notification (%d)", n)
	}
	var name string
	if err := E.db.QueryRow(E.ctx, `SELECT name FROM dm.maintenance_orders WHERE from_system AND maintenance_type_id = 2`).Scan(&name); err != nil || !strings.HasPrefix(name, "PM-") {
		t.Fatalf("preventive order name %q (%v)", name, err)
	}
}
