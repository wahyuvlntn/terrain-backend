package engine

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"afdd/internal/db"
)

// TestExternalAlarmsFromDB loads analytics.faults from a scratch database
// (AFDD_API_TEST_DATABASE_URL, name containing "scratch" or "test").
func TestExternalAlarmsFromDB(t *testing.T) {
	dsn := os.Getenv("AFDD_API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AFDD_API_TEST_DATABASE_URL not set")
	}
	if u, err := url.Parse(dsn); err != nil || (!strings.Contains(u.Path, "scratch") && !strings.Contains(u.Path, "test")) {
		t.Fatalf("refusing to run against %q (not a scratch database)", dsn)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(ctx, `INSERT INTO analytics.fault_rules (code, name, lbnl_fault, description, asset_scope, signals, params, defaults)
		VALUES ('CHS-005', 'Riser Differential Pressure Sensor Bias', 'Secondary loop DP sensor bias', 'test', 'plant', '[]', '{}', '{}')
		ON CONFLICT (code) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var assetID, faultID int64
	if err := d.QueryRow(ctx, `INSERT INTO analytics.faults (rule_code, asset_id, severity, message, started_at, last_seen_at)
		SELECT 'CHS-005', id, 'CRITICAL', 'Riser DP reads 18 % high', now(), now() FROM asset.equipment WHERE alias = 'CHWS_01'
		RETURNING asset_id, id`).Scan(&assetID, &faultID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.Exec(ctx, `DELETE FROM analytics.faults WHERE id = $1`, faultID) }()

	e := New(d, nil, nil, Options{})
	now := time.Now()
	e.refreshExternal(ctx, now)
	got := e.externalAlarms(assetID)
	found := false
	for _, a := range got {
		if a.Key == "ext:fdd:CHS-005" && a.Severity == "CRITICAL" && a.Message == "Riser DP reads 18 % high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("external alarms of CHWS_01: %+v", got)
	}

	// Cleared faults disappear on the next refresh (not before 30 s).
	if _, err := d.Exec(ctx, `UPDATE analytics.faults SET status = 'CLEARED', ended_at = now() WHERE id = $1`, faultID); err != nil {
		t.Fatal(err)
	}
	e.refreshExternal(ctx, now.Add(10*time.Second))
	if len(e.externalAlarms(assetID)) == 0 {
		t.Fatal("the cache must hold for 30 s")
	}
	e.refreshExternal(ctx, now.Add(externalRefresh))
	for _, a := range e.externalAlarms(assetID) {
		if a.Key == "ext:fdd:CHS-005" {
			t.Fatal("a cleared fault must not raise an alarm")
		}
	}

	// A missing table is "no faults", not an error.
	rows, err := d.Query(ctx, `SELECT 1 FROM analytics.no_such_table`)
	if err == nil {
		for rows.Next() {
		}
		rows.Close()
		err = rows.Err()
	}
	if !undefinedRelation(err) {
		t.Fatalf("missing table error not recognised: %v", err)
	}
}
