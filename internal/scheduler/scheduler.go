// Package scheduler runs the periodic background jobs: email digests,
// maintenance schedule extension and reminders, KPI refresh, running hour
// monitoring, license expiry notifications, session cleanup, partition
// maintenance and data retention. Every job takes a PostgreSQL advisory lock
// so that several backend instances never run the same job concurrently.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"afdd/internal/db"
	"afdd/internal/license"
	"afdd/internal/maint"
	"afdd/internal/notify"
)

// Deps are the services used by the jobs.
type Deps struct {
	DB            *db.DB
	Notify        *notify.Service
	Maint         *maint.Service
	CleanSessions func(ctx context.Context) error
	RefreshKPIs   func(ctx context.Context) error
	RetentionDays int
	ProductName   string
	// NoPartitionJobs leaves partition creation and retention (DDL) to a
	// privileged maintenance job (the cloud deployment's runtime identity
	// cannot create or drop partitions).
	NoPartitionJobs bool
}

// Job is a named periodic task.
type Job struct {
	Name     string
	Interval time.Duration
	Delay    time.Duration // first run delay
	Run      func(ctx context.Context) error
}

// Scheduler runs jobs until its context is cancelled.
type Scheduler struct {
	d    Deps
	jobs []Job
	wg   sync.WaitGroup
}

// New builds the scheduler with the standard job set.
func New(d Deps) *Scheduler {
	if d.ProductName == "" {
		d.ProductName = "AFDD"
	}
	s := &Scheduler{d: d}
	s.jobs = []Job{
		{Name: "email-digest", Interval: 30 * time.Second, Delay: 20 * time.Second, Run: d.Notify.RunEmailDigests},
		{Name: "maintenance-reminders", Interval: time.Minute, Delay: 15 * time.Second, Run: d.Maint.NotifyScheduled},
		{Name: "maintenance-schedules", Interval: 6 * time.Hour, Delay: 30 * time.Second, Run: d.Maint.ExtendSchedules},
		{Name: "running-hours", Interval: 5 * time.Minute, Delay: 45 * time.Second, Run: s.checkRunningHours},
		{Name: "license-expiry", Interval: time.Hour, Delay: time.Minute, Run: s.checkLicenses},
		{Name: "session-cleanup", Interval: time.Hour, Delay: 2 * time.Minute, Run: d.CleanSessions},
		{Name: "kpi-refresh", Interval: 15 * time.Minute, Delay: 90 * time.Second, Run: d.RefreshKPIs},
		// Also run once at start-up (MaintainPartitions) before the engine logs
		// values; the first scheduled run retries soon in case that one failed.
	}
	if !d.NoPartitionJobs {
		s.jobs = append(s.jobs,
			Job{Name: PartitionMaintenanceJob, Interval: 24 * time.Hour, Delay: 10 * time.Minute, Run: s.MaintainPartitions},
			Job{Name: "retention", Interval: 24 * time.Hour, Delay: 5 * time.Minute, Run: s.retention})
	}
	return s
}

// PartitionMaintenanceJob is the name of the job that creates the monthly
// partitions of ts.parameter_values.
const PartitionMaintenanceJob = "partition-maintenance"

const (
	// partitionMonthsAhead is how many months after the current one get their
	// ts.parameter_values partition in advance.
	partitionMonthsAhead = 3
	// maxPartitionMonthsBack caps how far back empty partitions are created
	// for a long retention period (730 days, the default, fits). Older data
	// can still be written after an explicit
	// ts.ensure_parameter_values_partitions call (the history generator).
	maxPartitionMonthsBack = 24
)

// partitionWindow returns the first and the last month (first day, UTC) whose
// ts.parameter_values partitions must exist at now: from the month that
// contains the start of the retention window (the current month when
// retention is off, at most maxPartitionMonthsBack months back) to
// partitionMonthsAhead months after the current one.
func partitionWindow(now time.Time, retentionDays int) (from, to time.Time) {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from = month
	if retentionDays > 0 {
		start := now.AddDate(0, 0, -retentionDays)
		from = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
		if limit := month.AddDate(0, -maxPartitionMonthsBack, 0); from.Before(limit) {
			from = limit
		}
	}
	return from, month.AddDate(0, partitionMonthsAhead, 0)
}

// MaintainPartitions creates the missing monthly partitions of
// ts.parameter_values (there is no default partition: a logged value without
// a partition is rejected). Idempotent and safe to run concurrently; the
// server also calls it at start-up, before the engine starts.
func (s *Scheduler) MaintainPartitions(ctx context.Context) error {
	from, to := partitionWindow(time.Now(), s.d.RetentionDays)
	var created int
	if err := s.d.DB.QueryRow(ctx, `SELECT ts.ensure_parameter_values_partitions($1::date, $2::date)`,
		from.Format(time.DateOnly), to.Format(time.DateOnly)).Scan(&created); err != nil {
		return fmt.Errorf("ensure parameter value partitions %s..%s: %w", from.Format("2006-01"), to.Format("2006-01"), err)
	}
	if created > 0 {
		slog.Info("created parameter value partitions", "count", created, "from", from.Format("2006-01"), "to", to.Format("2006-01"))
	}
	return nil
}

// Jobs returns the configured jobs (used by tests).
func (s *Scheduler) Jobs() []Job { return s.jobs }

// Start launches every job in its own goroutine.
func (s *Scheduler) Start(ctx context.Context) {
	for _, j := range s.jobs {
		if j.Run == nil {
			continue
		}
		s.wg.Add(1)
		go func(j Job) {
			defer s.wg.Done()
			s.loop(ctx, j)
		}(j)
	}
}

// Wait blocks until all job loops have stopped.
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) loop(ctx context.Context, j Job) {
	timer := time.NewTimer(j.Delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.RunOnce(ctx, j)
		timer.Reset(j.Interval)
	}
}

func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("afdd-scheduler:" + name))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// RunOnce executes a job if no other instance holds its lock. Panics are
// recovered so a faulty job never stops the scheduler.
func (s *Scheduler) RunOnce(ctx context.Context, j Job) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler job panicked", "job", j.Name, "panic", r)
		}
	}()
	conn, err := s.d.DB.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("scheduler: acquire connection", "job", j.Name, "err", err)
		}
		return
	}
	defer conn.Release()
	key := lockKey(j.Name)
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil || !ok {
		return
	}
	defer func() {
		// Use a fresh context: the job context may already be cancelled.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key)
	}()
	jctx, cancel := context.WithTimeout(ctx, maxDuration(j.Interval*4, 2*time.Minute))
	defer cancel()
	start := time.Now()
	if err := j.Run(jctx); err != nil && ctx.Err() == nil {
		slog.Error("scheduler job failed", "job", j.Name, "err", err)
		return
	}
	slog.Debug("scheduler job done", "job", j.Name, "took", time.Since(start))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// runningHoursAt sums the RUNNING status time of an equipment up to t.
func runningHoursAt(ctx context.Context, q db.Querier, assetID int64, t time.Time) (float64, error) {
	var h float64
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(end_ts, now()), $2) - start_ts))), 0) / 3600.0
		FROM ts.asset_status WHERE asset_id = $1 AND status_id = 1 AND start_ts < $2`, assetID, t).Scan(&h)
	return h, err
}

// checkRunningHours notifies (and optionally raises a preventive maintenance
// request) when the running hours accumulated since the last completed
// maintenance order exceed the model limits.
func (s *Scheduler) checkRunningHours(ctx context.Context) error {
	rows, err := s.d.DB.Query(ctx, `SELECT e.id, e.name, m.running_hour_warning, m.running_hour_critical,
			m.running_hour_notify_warn, m.running_hour_notify_crit, m.running_hour_auto_maint
		FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id
		WHERE (m.running_hour_notify_warn AND m.running_hour_warning > 0)
		   OR ((m.running_hour_notify_crit OR m.running_hour_auto_maint) AND m.running_hour_critical > 0)`)
	if err != nil {
		return err
	}
	type eq struct {
		id               int64
		name             string
		warn, crit       int
		nWarn, nCrit, am bool
	}
	var list []eq
	for rows.Next() {
		var e eq
		if err := rows.Scan(&e.id, &e.name, &e.warn, &e.crit, &e.nWarn, &e.nCrit, &e.am); err != nil {
			rows.Close()
			return err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, e := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.checkEquipmentHours(ctx, e.id, e.name, e.warn, e.crit, e.nWarn, e.nCrit, e.am, now); err != nil {
			slog.Error("running hour check", "asset", e.id, "err", err)
		}
	}
	return nil
}

func (s *Scheduler) checkEquipmentHours(ctx context.Context, id int64, name string, warn, crit int, nWarn, nCrit, autoMaint bool, now time.Time) error {
	var baseline float64
	var warnSent, critSent, made bool
	var resetAt *time.Time
	err := s.d.DB.QueryRow(ctx, `SELECT baseline_hours, warning_sent, critical_sent, maintenance_made, reset_at
		FROM ts.running_hour_flags WHERE asset_id = $1`, id).Scan(&baseline, &warnSent, &critSent, &made, &resetAt)
	if db.IsNoRows(err) {
		if _, err := s.d.DB.Exec(ctx, `INSERT INTO ts.running_hour_flags (asset_id) VALUES ($1) ON CONFLICT DO NOTHING`, id); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// A completed maintenance order restarts the count.
	var lastDone *time.Time
	if err := s.d.DB.QueryRow(ctx, `SELECT max(completed_at) FROM dm.maintenance_orders WHERE asset_id = $1 AND completed_at IS NOT NULL`, id).
		Scan(&lastDone); err != nil {
		return err
	}
	if lastDone != nil && (resetAt == nil || lastDone.After(*resetAt)) {
		b, err := runningHoursAt(ctx, s.d.DB, id, *lastDone)
		if err != nil {
			return err
		}
		baseline, warnSent, critSent, made, resetAt = b, false, false, false, lastDone
		if _, err := s.d.DB.Exec(ctx, `UPDATE ts.running_hour_flags SET baseline_hours = $2, warning_sent = FALSE, critical_sent = FALSE,
			maintenance_made = FALSE, reset_at = $3, updated_at = now() WHERE asset_id = $1`, id, b, *lastDone); err != nil {
			return err
		}
	}
	total, err := runningHoursAt(ctx, s.d.DB, id, now)
	if err != nil {
		return err
	}
	hours := total - baseline
	if hours < 0 {
		hours = 0
	}
	if crit > 0 && hours >= float64(crit) {
		if nCrit && !critSent {
			aid := id
			_ = s.d.Notify.Send(ctx, notify.Notification{Category: "RUNNING_HOUR_CRITICAL", AssetID: &aid,
				Message:        fmt.Sprintf("Equipment %s has reached the critical running hour limit (%d hours)", name, crit),
				AdditionalData: map[string]any{"running_hours": hours, "limit": crit}})
			critSent = true
		}
		if autoMaint && !made {
			if _, err := s.d.Maint.CreateRunningHourOrder(ctx, id, hours); err != nil {
				return err
			}
			made = true
		}
		warnSent = true // the warning stage is implied
	} else if warn > 0 && hours >= float64(warn) && nWarn && !warnSent {
		aid := id
		_ = s.d.Notify.Send(ctx, notify.Notification{Category: "RUNNING_HOUR_WARNING", AssetID: &aid,
			Message:        fmt.Sprintf("Equipment %s has reached the warning running hour limit (%d hours)", name, warn),
			AdditionalData: map[string]any{"running_hours": hours, "limit": warn}})
		warnSent = true
	}
	_, err = s.d.DB.Exec(ctx, `UPDATE ts.running_hour_flags SET warning_sent = $2, critical_sent = $3, maintenance_made = $4, updated_at = now()
		WHERE asset_id = $1`, id, warnSent, critSent, made)
	return err
}

// License expiry reminders are sent once per threshold (hours before expiry).
var expiryThresholds = []int{720, 168, 72, 24}

func (s *Scheduler) checkLicenses(ctx context.Context) error {
	rows, err := s.d.DB.Query(ctx, `SELECT id, name, type, expired_at, expiry_notified_hours FROM core.licenses
		WHERE expired_at IS NOT NULL AND expired_at > now() AND expired_at <= now() + interval '31 days'`)
	if err != nil {
		return err
	}
	type lic struct {
		id       int64
		name     string
		typ      string
		exp      time.Time
		notified *int
	}
	var list []lic
	for rows.Next() {
		var l lic
		if err := rows.Scan(&l.id, &l.name, &l.typ, &l.exp, &l.notified); err != nil {
			rows.Close()
			return err
		}
		list = append(list, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range list {
		left := time.Until(l.exp).Hours()
		threshold := 0
		for _, t := range expiryThresholds {
			if left <= float64(t) {
				threshold = t
			}
		}
		if threshold == 0 || (l.notified != nil && *l.notified <= threshold) {
			continue
		}
		msg := notify.LicenseExpiryMessage(s.d.ProductName, l.name, int(left+0.5), l.typ == license.TypeBase)
		if err := s.d.Notify.Send(ctx, notify.Notification{Category: "LICENSE_EXPIRY", Message: msg,
			AdditionalData: map[string]any{"license_name": l.name, "expired_at": l.exp.UTC().Format(time.RFC3339)}}); err != nil {
			return err
		}
		if _, err := s.d.DB.Exec(ctx, `UPDATE core.licenses SET expiry_notified_hours = $2 WHERE id = $1`, l.id, threshold); err != nil {
			return err
		}
	}
	// Renewed licenses can be reminded again.
	_, err = s.d.DB.Exec(ctx, `UPDATE core.licenses SET expiry_notified_hours = NULL
		WHERE expiry_notified_hours IS NOT NULL AND (expired_at IS NULL OR expired_at > now() + interval '31 days')`)
	return err
}

// retention removes raw measurements and notifications older than the
// retention period. Measurements are partitioned by month, so whole months
// that ended before the cutoff are dropped (never a ctid delete on the
// partitioned table: ctids repeat across partitions); the other tables are
// deleted from in small batches to keep locks short.
func (s *Scheduler) retention(ctx context.Context) error {
	if s.d.RetentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -s.d.RetentionDays)
	var errs []error
	var dropped int
	if err := s.d.DB.QueryRow(ctx, `SELECT ts.drop_parameter_values_partitions_before($1)`, cutoff).Scan(&dropped); err != nil {
		errs = append(errs, fmt.Errorf("drop parameter value partitions: %w", err))
	} else if dropped > 0 {
		slog.Info("retention dropped parameter value partitions", "count", dropped, "cutoff", cutoff.Format(time.RFC3339))
	}
	for _, q := range []string{
		`DELETE FROM core.notifications WHERE id = ANY(ARRAY(SELECT id FROM core.notifications WHERE created_at < $1 LIMIT 5000))`,
		`DELETE FROM ts.asset_condition WHERE ctid = ANY(ARRAY(SELECT ctid FROM ts.asset_condition WHERE end_ts IS NOT NULL AND end_ts < $1 LIMIT 20000))`,
	} {
		for {
			if ctx.Err() != nil {
				return errors.Join(append(errs, ctx.Err())...)
			}
			tag, err := s.d.DB.Exec(ctx, q, cutoff)
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			if tag.RowsAffected() == 0 {
				break
			}
		}
	}
	return errors.Join(errs...)
}
