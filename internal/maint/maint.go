// Package maint implements the maintenance order domain: requests, approval,
// recurring schedules, work execution (start / pause / resume / done /
// close), early-warning orders raised by the diagnostics engine and
// running-hour based preventive orders.
package maint

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/notify"
)

// Maintenance statuses.
const (
	StatusRequested  = 1
	StatusRejected   = 2
	StatusOpen       = 3
	StatusInProgress = 4
	StatusDone       = 5
	StatusClosed     = 6
	StatusPaused     = 7
)

// Maintenance types.
const (
	TypeCorrective = 1
	TypePreventive = 2
)

// StatusCode returns the API code (REQUESTED, IN_PROGRESS, ...).
func StatusCode(id int) string {
	switch id {
	case StatusRequested:
		return "REQUESTED"
	case StatusRejected:
		return "REJECTED"
	case StatusOpen:
		return "OPEN"
	case StatusInProgress:
		return "IN_PROGRESS"
	case StatusDone:
		return "DONE"
	case StatusClosed:
		return "CLOSED"
	case StatusPaused:
		return "PAUSED"
	}
	return ""
}

// StatusLabel returns the human label (Requested, In Progress, ...).
func StatusLabel(id int) string {
	switch id {
	case StatusRequested:
		return "Requested"
	case StatusRejected:
		return "Rejected"
	case StatusOpen:
		return "Open"
	case StatusInProgress:
		return "In Progress"
	case StatusDone:
		return "Done"
	case StatusClosed:
		return "Closed"
	case StatusPaused:
		return "Paused"
	}
	return ""
}

// Service implements maintenance logic.
type Service struct {
	db     *db.DB
	notify *notify.Service
	loc    *time.Location
	// OnEquipmentChanged is called when equipment state (forced status)
	// changes so the engine can reload.
	OnEquipmentChanged func()
}

// New creates the service. loc is used for calendar based recurrence.
func New(d *db.DB, n *notify.Service, loc *time.Location) *Service {
	if loc == nil {
		loc = time.UTC
	}
	return &Service{db: d, notify: n, loc: loc}
}

func (s *Service) equipmentChanged() {
	if s.OnEquipmentChanged != nil {
		s.OnEquipmentChanged()
	}
}

// Error is a business rule violation shown to the user.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Order is a loaded maintenance order.
type Order struct {
	ID                int64
	Name              string
	ScheduleID        *int64
	AssetID           int64
	AssetName         string
	AssetAlias        string
	LocationID        int64
	ModelID           int64
	TypeID            int
	PriorityID        *int
	SeverityID        int
	StatusID          int
	Description       string
	PlannedStart      time.Time
	PlannedEnd        *time.Time
	RequestedBy       *int64
	ApprovedBy        *int64
	EditedBy          *int64
	RejectReason      string
	FromSystem        bool
	IssueRelationID   *int64
	IsCompound        bool
	IsDeduction       bool
	DegradationName   *string
	DetectedAt        *time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
	ClosedAt          *time.Time
	CreatedAt         time.Time
	UpdatedAt         *time.Time
	ScheduledNotified bool
}

const orderColumns = `o.id, o.name, o.schedule_id, o.asset_id, e.name, e.alias, e.location_id, e.model_id, o.maintenance_type_id,
	o.priority_id, o.severity_id, o.status_id, o.description, o.planned_start, o.planned_end, o.requested_by, o.approved_by,
	o.edited_by, o.reject_reason, o.from_system, o.issue_relation_id, o.is_compound, o.is_deduction, o.degradation_name,
	o.detected_at, o.started_at, o.completed_at, o.closed_at, o.created_at, o.updated_at, o.scheduled_notified`

func scanOrder(row pgx.Row) (*Order, error) {
	o := &Order{}
	err := row.Scan(&o.ID, &o.Name, &o.ScheduleID, &o.AssetID, &o.AssetName, &o.AssetAlias, &o.LocationID, &o.ModelID, &o.TypeID,
		&o.PriorityID, &o.SeverityID, &o.StatusID, &o.Description, &o.PlannedStart, &o.PlannedEnd, &o.RequestedBy, &o.ApprovedBy,
		&o.EditedBy, &o.RejectReason, &o.FromSystem, &o.IssueRelationID, &o.IsCompound, &o.IsDeduction, &o.DegradationName,
		&o.DetectedAt, &o.StartedAt, &o.CompletedAt, &o.ClosedAt, &o.CreatedAt, &o.UpdatedAt, &o.ScheduledNotified)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// Get loads an order.
func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (*Order, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM dm.maintenance_orders o
		JOIN asset.equipment e ON e.id = o.asset_id WHERE o.id = $1`, id))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, &Error{Msg: "err_work_order_not_found"}
		}
		return nil, err
	}
	return o, nil
}

func (s *Service) getForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*Order, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM dm.maintenance_orders WHERE id = $1 FOR UPDATE`, id); err != nil {
		return nil, err
	}
	return s.Get(ctx, tx, id)
}

// addEvent records a progress entry.
func addEvent(ctx context.Context, q db.Querier, orderID int64, status string, details map[string]any, by *int64, at time.Time) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := q.Exec(ctx, `INSERT INTO dm.maintenance_order_events (order_id, status, details, created_by, created_at) VALUES ($1, $2, $3, $4, $5)`,
		orderID, status, details, by, at)
	return err
}

// Technicians returns the usernames assigned to an order.
func Technicians(ctx context.Context, q db.Querier, orderID int64) ([]string, error) {
	return db.Strings(ctx, q, `SELECT u.username FROM dm.maintenance_order_technicians t JOIN core.users u ON u.id = t.user_id
		WHERE t.order_id = $1 ORDER BY u.username`, orderID)
}

func usernameOf(ctx context.Context, q db.Querier, id *int64) string {
	if id == nil {
		return ""
	}
	var u string
	_ = q.QueryRow(ctx, `SELECT username FROM core.users WHERE id = $1`, *id).Scan(&u)
	return u
}

// UserIDByUsername resolves a username (active users first).
func UserIDByUsername(ctx context.Context, q db.Querier, username string) (*int64, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, nil
	}
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM core.users WHERE lower(username) = lower($1) ORDER BY is_deleted, id DESC LIMIT 1`, username).Scan(&id)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &id, nil
}

// setTechnicians replaces the technicians of an order (comma separated usernames).
func setTechnicians(ctx context.Context, tx pgx.Tx, orderID int64, usernames []string) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM dm.maintenance_order_technicians WHERE order_id = $1`, orderID); err != nil {
		return nil, err
	}
	var added []string
	for _, u := range usernames {
		id, err := UserIDByUsername(ctx, tx, u)
		if err != nil {
			return nil, err
		}
		if id == nil {
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO dm.maintenance_order_technicians (order_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, orderID, *id)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() > 0 {
			added = append(added, u)
		}
	}
	return added, nil
}

// setIssues replaces the main and other issues (alarm code ids) of an order,
// keeping validation results of codes that remain.
func setIssues(ctx context.Context, tx pgx.Tx, orderID int64, main, other []int64) error {
	keep := map[int64]bool{}
	for _, c := range main {
		keep[c] = true
	}
	for _, c := range other {
		keep[c] = true
	}
	ids := make([]int64, 0, len(keep))
	for id := range keep {
		ids = append(ids, id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dm.maintenance_order_issues WHERE order_id = $1 AND NOT (alarm_code_id = ANY($2))`, orderID, ids); err != nil {
		return err
	}
	pos := 0
	upsert := func(code int64, isOther bool) error {
		pos++
		_, err := tx.Exec(ctx, `INSERT INTO dm.maintenance_order_issues (order_id, alarm_code_id, is_other, position)
			SELECT $1, $2, $3, $4 WHERE EXISTS (SELECT 1 FROM dm.alarm_codes WHERE id = $2)
			ON CONFLICT (order_id, alarm_code_id) DO UPDATE SET is_other = EXCLUDED.is_other, position = EXCLUDED.position`,
			orderID, code, isOther, pos)
		return err
	}
	seen := map[int64]bool{}
	for _, c := range main {
		if seen[c] {
			continue
		}
		seen[c] = true
		if err := upsert(c, false); err != nil {
			return err
		}
	}
	for _, c := range other {
		if seen[c] {
			continue
		}
		seen[c] = true
		if err := upsert(c, true); err != nil {
			return err
		}
	}
	return nil
}

// Issues returns the issue codes of an order.
func Issues(ctx context.Context, q db.Querier, orderID int64) (main, other []int64, err error) {
	rows, err := q.Query(ctx, `SELECT alarm_code_id, is_other FROM dm.maintenance_order_issues WHERE order_id = $1 ORDER BY position, alarm_code_id`, orderID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	main, other = []int64{}, []int64{}
	for rows.Next() {
		var id int64
		var isOther bool
		if err := rows.Scan(&id, &isOther); err != nil {
			return nil, nil, err
		}
		if isOther {
			other = append(other, id)
		} else {
			main = append(main, id)
		}
	}
	return main, other, rows.Err()
}

// EstimatedSeconds sums the instruction durations of the main issues of an
// order (instructions of the equipment's model). Falls back to the planned
// window when no instruction applies.
func EstimatedSeconds(ctx context.Context, q db.Querier, o *Order) int64 {
	var minutes int64
	_ = q.QueryRow(ctx, `SELECT COALESCE(sum(i.duration), 0) FROM dm.maintenance_order_issues moi
		JOIN dm.instructions i ON i.alarm_code_id = moi.alarm_code_id AND NOT i.is_deleted
		JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = $2
		WHERE moi.order_id = $1 AND NOT moi.is_other`, o.ID, o.ModelID).Scan(&minutes)
	if minutes > 0 {
		return minutes * 60
	}
	if o.PlannedEnd != nil && o.PlannedEnd.After(o.PlannedStart) {
		return int64(o.PlannedEnd.Sub(o.PlannedStart).Seconds())
	}
	return 0
}

// ActualSeconds sums the active work intervals of an order.
func ActualSeconds(ctx context.Context, q db.Querier, orderID int64) int64 {
	var secs float64
	_ = q.QueryRow(ctx, `SELECT COALESCE(sum(EXTRACT(EPOCH FROM (COALESCE(ended_at, now()) - started_at))), 0)
		FROM dm.work_intervals WHERE order_id = $1`, orderID).Scan(&secs)
	if secs < 0 {
		return 0
	}
	return int64(secs)
}

// notifyOrder sends a maintenance notification.
func (s *Service) notifyOrder(ctx context.Context, o *Order, category, message string, recipients []string) {
	assetID := o.AssetID
	loc := o.LocationID
	clean := []string{}
	seen := map[string]bool{}
	for _, r := range recipients {
		r = strings.TrimSpace(r)
		if r != "" && !seen[strings.ToLower(r)] {
			seen[strings.ToLower(r)] = true
			clean = append(clean, r)
		}
	}
	if err := s.notify.Send(ctx, notify.Notification{
		Category: category, Message: message, AssetID: &assetID, LocationID: &loc, Recipients: clean,
		AdditionalData: map[string]any{"maintenance_id": o.ID, "order_name": o.Name, "selected": o.ID,
			"planned_start": o.PlannedStart.UTC().Format(time.RFC3339)},
	}); err != nil {
		slog.Warn("maintenance notification", "err", err)
	}
}

func joinNonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// releaseForcedStatus clears an equipment status forced by an order.
func (s *Service) releaseForcedStatus(ctx context.Context, q db.Querier, o *Order) error {
	// Callers notify the engine after their transaction commits.
	_, err := q.Exec(ctx, `UPDATE asset.equipment SET forced_status_id = NULL, forced_by_order_id = NULL
		WHERE id = $1 AND forced_by_order_id = $2`, o.AssetID, o.ID)
	return err
}
