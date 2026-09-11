// Package events carries configuration changes from API replicas to the
// processing worker through the database (core.runtime_events): a gateway
// edited in the UI must restart its collector, a model edit must reload the
// evaluation engine, wherever those run.
package events

import (
	"context"
	"log/slog"
	"time"

	"afdd/internal/db"
)

// Kinds of runtime events.
const (
	Gateway = "gateway" // key: gateway alias (reload, or forget when deleted)
	Engine  = "engine"  // the evaluation definitions changed
)

// Emit records an event (best effort: the worker also reloads periodically).
func Emit(ctx context.Context, d *db.DB, kind, key string) {
	if _, err := d.Exec(ctx, `INSERT INTO core.runtime_events (kind, key) VALUES ($1, $2)`, kind, key); err != nil {
		slog.Warn("record runtime event", "kind", kind, "key", key, "err", err)
	}
}

// Watch calls fn for every event recorded after it started, in order, polling
// every interval until ctx is cancelled. Events older than a day are pruned.
func Watch(ctx context.Context, d *db.DB, interval time.Duration, fn func(kind, key string)) {
	var last int64
	for {
		if err := d.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM core.runtime_events`).Scan(&last); err == nil {
			break
		} else if ctx.Err() != nil {
			return
		} else {
			slog.Warn("runtime events", "err", err)
		}
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
	pruned := time.Now()
	for sleep(ctx, interval) {
		rows, err := d.Query(ctx, `SELECT id, kind, COALESCE(key, '') FROM core.runtime_events WHERE id > $1 ORDER BY id LIMIT 500`, last)
		if err != nil {
			slog.Warn("runtime events", "err", err)
			continue
		}
		type ev struct {
			id        int64
			kind, key string
		}
		var evs []ev
		for rows.Next() {
			var e ev
			if rows.Scan(&e.id, &e.kind, &e.key) == nil {
				evs = append(evs, e)
			}
		}
		rows.Close()
		seen := map[[2]string]bool{}
		for _, e := range evs {
			last = e.id
			if k := [2]string{e.kind, e.key}; !seen[k] {
				seen[k] = true
				fn(e.kind, e.key)
			}
		}
		if time.Since(pruned) > 10*time.Minute {
			pruned = time.Now()
			_, _ = d.Exec(ctx, `DELETE FROM core.runtime_events WHERE at < now() - interval '1 day'`)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
