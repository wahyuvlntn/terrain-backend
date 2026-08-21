package scheduler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
)

func TestPartitionWindow(t *testing.T) {
	month := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	sgt := time.FixedZone("SGT", 8*3600)
	cases := []struct {
		name      string
		now       time.Time
		retention int
		from, to  time.Time
	}{
		{"default retention", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), 730, month(2024, 10), month(2027, 1)},
		{"retention off", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), 0, month(2026, 10), month(2027, 1)},
		{"negative retention", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), -5, month(2026, 10), month(2027, 1)},
		{"short retention", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), 30, month(2026, 9), month(2027, 1)},
		{"long retention is capped", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), 3650, month(2024, 10), month(2027, 1)},
		{"year boundary", time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), 1, month(2026, 12), month(2027, 3)},
		// 2026-11-01 05:00 SGT is still October in UTC: months are UTC.
		{"months are UTC", time.Date(2026, 11, 1, 5, 0, 0, 0, sgt), 0, month(2026, 10), month(2027, 1)},
	}
	for _, c := range cases {
		from, to := partitionWindow(c.now, c.retention)
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%s: window = %s..%s, want %s..%s", c.name, from.Format(time.DateOnly), to.Format(time.DateOnly),
				c.from.Format(time.DateOnly), c.to.Format(time.DateOnly))
		}
		if from.Location() != time.UTC || to.Location() != time.UTC {
			t.Errorf("%s: window not in UTC", c.name)
		}
	}
}

func TestJobs(t *testing.T) {
	s := New(Deps{RetentionDays: 730})
	want := map[string]time.Duration{PartitionMaintenanceJob: 24 * time.Hour, "retention": 24 * time.Hour}
	for _, j := range s.Jobs() {
		if iv, ok := want[j.Name]; ok {
			if j.Interval != iv || j.Run == nil {
				t.Errorf("job %s: interval %s (want %s), run set %v", j.Name, j.Interval, iv, j.Run != nil)
			}
			delete(want, j.Name)
		}
	}
	for name := range want {
		t.Errorf("job %s missing", name)
	}
}

// scratchDB creates a migrated scratch database on the server of
// AFDD_TEST_DATABASE_URL (skips the test when it is not set) and drops it at
// the end. The server needs the pgvector extension (migration 0005).
func scratchDB(t *testing.T) *db.DB {
	t.Helper()
	base := os.Getenv("AFDD_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("AFDD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("afdd_sched_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close(context.Background())
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	d, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func partitionNames(t *testing.T, d *db.DB) map[string]bool {
	t.Helper()
	names, err := db.Strings(context.Background(), d, `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'ts.parameter_values'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func partitionName(m time.Time) string {
	return fmt.Sprintf("parameter_values_y%04dm%02d", m.Year(), int(m.Month()))
}

func TestPartitionsOnDatabase(t *testing.T) {
	d := scratchDB(t)
	ctx := context.Background()
	s := &Scheduler{d: Deps{DB: d, RetentionDays: 730}}
	now := time.Now().UTC()

	// The migration covers 2026-01 (empty table) up to now + 3 months.
	from, to := partitionWindow(now, s.d.RetentionDays)
	before := partitionNames(t, d)
	if !before[partitionName(to)] || !before[partitionName(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))] {
		t.Fatalf("migration partitions %v", before)
	}

	if err := s.MaintainPartitions(ctx); err != nil {
		t.Fatal(err)
	}
	after := partitionNames(t, d)
	for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
		if !after[partitionName(m)] {
			t.Errorf("partition %s missing after maintenance", partitionName(m))
		}
	}
	// Idempotent.
	if err := s.MaintainPartitions(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(partitionNames(t, d)); n != len(after) {
		t.Errorf("second run changed the partitions: %d -> %d", len(after), n)
	}

	// An attached month has the primary key, the BRIN index and the grants.
	cur := partitionName(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	var indexes int
	var grantRW, grantRO bool
	if err := d.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_indexes WHERE schemaname = 'ts' AND tablename = $1),
			has_table_privilege('afdd_ai', 'ts.'||$1, 'SELECT'), has_table_privilege('afdd_ai_ro', 'ts.'||$1, 'SELECT')`, cur).
		Scan(&indexes, &grantRW, &grantRO); err != nil {
		t.Fatal(err)
	}
	if indexes != 2 || !grantRW || !grantRO {
		t.Errorf("partition %s: %d indexes, afdd_ai select %v, afdd_ai_ro select %v", cur, indexes, grantRW, grantRO)
	}

	// The engine's logging statement works, duplicates are ignored.
	insert := func(ts time.Time) error {
		_, err := d.Exec(ctx, `INSERT INTO ts.parameter_values (equipment_parameter_id, ts, value, value_text)
			SELECT * FROM unnest($1::bigint[], $2::timestamptz[], $3::float8[], $4::text[])
			ON CONFLICT DO NOTHING`, []int64{1, 1}, []time.Time{ts, ts}, []*float64{ptr(1.5), ptr(2.5)}, []*string{nil, nil})
		return err
	}
	if err := insert(now.Truncate(time.Second)); err != nil {
		t.Fatalf("engine insert: %v", err)
	}
	// A month outside every partition is rejected (there is no default partition).
	if err := insert(now.AddDate(5, 0, 0)); err == nil {
		t.Error("insert without a partition succeeded")
	}

	// Retention drops whole months that ended before the cutoff, keeps the rest.
	old := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -30, 0)
	if _, err := d.Exec(ctx, `SELECT ts.ensure_parameter_values_partitions($1::date, $1::date)`, old.Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	if err := insert(old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.retention(ctx); err != nil {
		t.Fatalf("retention: %v", err)
	}
	left := partitionNames(t, d)
	if left[partitionName(old)] {
		t.Errorf("partition %s older than the retention survived", partitionName(old))
	}
	if !left[partitionName(from)] || !left[cur] {
		t.Errorf("retention dropped a partition inside the window (%s or %s)", partitionName(from), cur)
	}
	var rows int
	if err := d.QueryRow(ctx, `SELECT count(*) FROM ts.parameter_values`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("rows after retention = %d, want 1", rows)
	}

	// A cutoff in the future and absurd ranges are refused.
	for _, q := range []string{
		`SELECT ts.drop_parameter_values_partitions_before(now() + interval '1 day')`,
		`SELECT ts.ensure_parameter_values_partitions('-infinity'::date, '2026-01-01'::date)`,
		`SELECT ts.ensure_parameter_values_partitions('0001-01-01'::date, '2026-01-01'::date)`,
	} {
		if _, err := d.Exec(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func ptr[T any](v T) *T { return &v }
