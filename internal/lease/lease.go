// Package lease gives one process at a time the right to do continuous
// processing: a PostgreSQL session advisory lock held on a dedicated
// connection. A replacement instance that starts while the old one still
// runs waits; when the holder dies or loses its connection the lock is
// released and the waiting instance takes over.
package lease

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"afdd/internal/db"
)

// Keys of the leases (pg advisory lock ids).
const (
	ProcessingWorker int64 = 0x4146_4444_0001 // "AFDD" + 1: collectors, evaluation, scheduler
)

// Lease is a held lock.
type Lease struct {
	conn *pgxpool.Conn
	key  int64
	lost chan struct{}
	stop context.CancelFunc
}

// Acquire waits until the lock is held (or ctx ends), checking every retry.
func Acquire(ctx context.Context, d *db.DB, key int64, name string, retry time.Duration) (*Lease, error) {
	waiting := false
	for {
		conn, err := d.Acquire(ctx)
		if err == nil {
			var ok bool
			if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err == nil && ok {
				l := &Lease{conn: conn, key: key, lost: make(chan struct{})}
				mctx, cancel := context.WithCancel(context.Background())
				l.stop = cancel
				go l.monitor(mctx, name)
				slog.Info("lease acquired", "lease", name)
				return l, nil
			}
			conn.Release()
		}
		if err != nil {
			slog.Warn("lease", "lease", name, "err", err)
		} else if !waiting {
			waiting = true
			slog.Info("another instance holds the lease, standing by", "lease", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

// monitor pings the lock's connection; a failure means the lock may be gone.
func (l *Lease) monitor(ctx context.Context, name string) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var held bool
		err := l.conn.QueryRow(pctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
			AND granted AND ((classid::bigint << 32) | objid::bigint) = $1)`, l.key).Scan(&held)
		cancel()
		if err == nil && !held {
			err = errors.New("advisory lock no longer held")
		}
		if err == nil {
			fails = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		fails++
		slog.Warn("lease check failed", "lease", name, "err", err, "fails", fails)
		if fails >= 2 {
			close(l.lost)
			return
		}
	}
}

// Lost is closed when the lease can no longer be confirmed: stop processing.
func (l *Lease) Lost() <-chan struct{} { return l.lost }

// Release gives the lock back.
func (l *Lease) Release() {
	l.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, l.key)
	l.conn.Release()
}
