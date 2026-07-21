package maint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
)

// IssueValidity is the technician verdict for an alarm code / instruction type.
type IssueValidity struct {
	AlarmCodeID       int64 `json:"alarm_code_id"`
	InstructionTypeID int64 `json:"instruction_type_id"`
	IsValid           *bool `json:"is_valid"`
}

// IsAssigned reports whether a user is a technician of the order.
func IsAssigned(ctx context.Context, q db.Querier, orderID, userID int64) bool {
	var ok bool
	_ = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_order_technicians WHERE order_id = $1 AND user_id = $2)`, orderID, userID).Scan(&ok)
	return ok
}

func (s *Service) transition(ctx context.Context, orderID int64, fn func(tx pgx.Tx, o *Order, now time.Time) error) (*Order, error) {
	var out *Order
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getForUpdate(ctx, tx, orderID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := fn(tx, o, now); err != nil {
			return err
		}
		out, err = s.Get(ctx, tx, orderID)
		return err
	})
	if err == nil {
		s.equipmentChanged()
	}
	return out, err
}

func (s *Service) workRecipients(ctx context.Context, o *Order) []string {
	return joinNonEmpty(usernameOf(ctx, s.db, o.RequestedBy), usernameOf(ctx, s.db, o.ApprovedBy))
}

// Start moves an open order to in progress. forceStatus stops the
// equipment (planned down for preventive, unplanned down for corrective).
func (s *Service) Start(ctx context.Context, actor Actor, orderID int64, forceStatus bool) error {
	forced := false
	o, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		switch o.StatusID {
		case StatusOpen:
		case StatusInProgress:
			return errf("Maintenance order is already in progress")
		case StatusPaused:
			return errf("Maintenance order is paused, resume it instead")
		default:
			return errf("Only open maintenance orders can be started")
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, started_at = COALESCE(started_at, $3), updated_at = $3 WHERE id = $1`,
			o.ID, StatusInProgress, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dm.work_intervals (order_id, started_at) VALUES ($1, $2)`, o.ID, now); err != nil {
			return err
		}
		details := map[string]any{"By": displayName(actor)}
		if forceStatus {
			status := 5 // planned down
			label := "Planned Down"
			if o.TypeID == TypeCorrective {
				status, label = 3, "Unplanned Down"
			}
			if _, err := tx.Exec(ctx, `UPDATE asset.equipment SET forced_status_id = $2, forced_by_order_id = $3 WHERE id = $1`, o.AssetID, status, o.ID); err != nil {
				return err
			}
			details["Equipment Status"] = label
			forced = true
		}
		return addEvent(ctx, tx, o.ID, "STARTED", details, &actor.ID, now)
	})
	if err != nil {
		return err
	}
	_ = forced
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_STARTED", fmt.Sprintf("Maintenance order %s for equipment %s has been started by %s", o.Name, o.AssetName, displayName(actor)), s.workRecipients(ctx, o))
	return nil
}

// Pause puts an in-progress order on hold.
func (s *Service) Pause(ctx context.Context, actor Actor, orderID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errf("Reason is required")
	}
	o, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if o.StatusID != StatusInProgress {
			return errf("Only in-progress maintenance orders can be paused")
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, updated_at = $3 WHERE id = $1`, o.ID, StatusPaused, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.work_intervals SET ended_at = $2 WHERE order_id = $1 AND ended_at IS NULL`, o.ID, now); err != nil {
			return err
		}
		return addEvent(ctx, tx, o.ID, "PAUSED", map[string]any{"By": displayName(actor), "Reason": reason}, &actor.ID, now)
	})
	if err != nil {
		return err
	}
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_PAUSED", fmt.Sprintf("Maintenance order %s for equipment %s has been paused: %s", o.Name, o.AssetName, reason), s.workRecipients(ctx, o))
	return nil
}

// Resume continues a paused order.
func (s *Service) Resume(ctx context.Context, actor Actor, orderID int64) error {
	o, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if o.StatusID != StatusPaused {
			return errf("Only paused maintenance orders can be resumed")
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, updated_at = $3 WHERE id = $1`, o.ID, StatusInProgress, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dm.work_intervals (order_id, started_at) VALUES ($1, $2)`, o.ID, now); err != nil {
			return err
		}
		return addEvent(ctx, tx, o.ID, "RESUMED", map[string]any{"By": displayName(actor)}, &actor.ID, now)
	})
	if err != nil {
		return err
	}
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_RESUMED", fmt.Sprintf("Maintenance order %s for equipment %s has been resumed", o.Name, o.AssetName), s.workRecipients(ctx, o))
	return nil
}

// AddWorkUpdate records a technician note with images and related issues.
func (s *Service) AddWorkUpdate(ctx context.Context, actor Actor, orderID int64, note string, images []string, issueIDs []int64) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return errf("Note is required")
	}
	if len([]rune(note)) > 1000 {
		return errf("Note is too long")
	}
	_, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if o.StatusID != StatusInProgress && o.StatusID != StatusPaused {
			return errf("Work updates can only be added to in-progress maintenance orders")
		}
		if images == nil {
			images = []string{}
		}
		if issueIDs == nil {
			issueIDs = []int64{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dm.work_updates (order_id, note, image_urls, issue_ids, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, o.ID, note, images, issueIDs, actor.ID, now); err != nil {
			return err
		}
		issues := []map[string]any{}
		if len(issueIDs) > 0 {
			rows, err := tx.Query(ctx, `SELECT c.code, c.name, i.is_other FROM dm.alarm_codes c
				LEFT JOIN dm.maintenance_order_issues i ON i.alarm_code_id = c.id AND i.order_id = $1
				WHERE c.id = ANY($2) ORDER BY i.position NULLS LAST, c.code`, o.ID, issueIDs)
			if err != nil {
				return err
			}
			for rows.Next() {
				var code, name string
				var isOther *bool
				if err := rows.Scan(&code, &name, &isOther); err != nil {
					rows.Close()
					return err
				}
				item := map[string]any{"label": code + " - " + name}
				if isOther != nil {
					item["is_other"] = *isOther
				}
				issues = append(issues, item)
			}
			rows.Close()
		}
		details := map[string]any{"By": displayName(actor), "Note": note, "attachment_urls": images}
		if len(issues) > 0 {
			details["Issue"] = issues
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET updated_at = $2 WHERE id = $1`, o.ID, now); err != nil {
			return err
		}
		return addEvent(ctx, tx, o.ID, "UPDATED", details, &actor.ID, now)
	})
	return err
}

// applyValidities stores technician verdicts.
func applyValidities(ctx context.Context, tx pgx.Tx, orderID int64, issues []IssueValidity) error {
	for _, iv := range issues {
		var col string
		switch iv.InstructionTypeID {
		case 1:
			col = "is_valid_diagnostic"
		case 2:
			col = "is_valid_repair"
		default:
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_order_issues SET `+col+` = $3 WHERE order_id = $1 AND alarm_code_id = $2`,
			orderID, iv.AlarmCodeID, iv.IsValid); err != nil {
			return err
		}
		// An invalid (or unset) diagnosis makes the repair verdict meaningless.
		if iv.InstructionTypeID == 1 && (iv.IsValid == nil || !*iv.IsValid) {
			if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_order_issues SET is_valid_repair = NULL WHERE order_id = $1 AND alarm_code_id = $2`,
				orderID, iv.AlarmCodeID); err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateIssueValidity sets a single verdict.
func (s *Service) UpdateIssueValidity(ctx context.Context, actor Actor, orderID int64, iv IssueValidity) error {
	_, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if o.StatusID == StatusClosed || o.StatusID == StatusRejected {
			return errf("Maintenance order is already closed")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_order_issues WHERE order_id = $1 AND alarm_code_id = $2)`,
			o.ID, iv.AlarmCodeID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return &Error{Msg: "err_alarm_not_found"}
		}
		if err := applyValidities(ctx, tx, o.ID, []IssueValidity{iv}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET updated_at = $2 WHERE id = $1`, o.ID, now)
		return err
	})
	return err
}

func validityDetails(ctx context.Context, tx pgx.Tx, orderID int64) ([]map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT c.code, c.name, i.is_other, i.is_valid_diagnostic, i.is_valid_repair
		FROM dm.maintenance_order_issues i JOIN dm.alarm_codes c ON c.id = i.alarm_code_id
		WHERE i.order_id = $1 ORDER BY i.position`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var code, name string
		var isOther bool
		var diag, rep *bool
		if err := rows.Scan(&code, &name, &isOther, &diag, &rep); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"Issue": []map[string]any{{"label": code + " - " + name, "is_other": isOther}},
			"is_other": isOther, "diagnosis": diag, "repair": rep})
	}
	return out, rows.Err()
}

// Done marks the work as completed by the technician.
func (s *Service) Done(ctx context.Context, actor Actor, orderID int64, issues []IssueValidity) error {
	o, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if o.StatusID != StatusInProgress && o.StatusID != StatusPaused {
			return errf("Only in-progress maintenance orders can be marked as done")
		}
		if err := applyValidities(ctx, tx, o.ID, issues); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.work_intervals SET ended_at = $2 WHERE order_id = $1 AND ended_at IS NULL`, o.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, completed_at = $3, updated_at = $3 WHERE id = $1`,
			o.ID, StatusDone, now); err != nil {
			return err
		}
		if err := s.releaseForcedStatus(ctx, tx, o); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, o.ID, "COMPLETED", map[string]any{"By": displayName(actor)}, &actor.ID, now); err != nil {
			return err
		}
		return s.validityEvents(ctx, tx, o.ID, actor, now)
	})
	if err != nil {
		return err
	}
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_COMPLETED", fmt.Sprintf("Maintenance order %s for equipment %s has been completed", o.Name, o.AssetName), s.workRecipients(ctx, o))
	return nil
}

func (s *Service) validityEvents(ctx context.Context, tx pgx.Tx, orderID int64, actor Actor, now time.Time) error {
	details, err := validityDetails(ctx, tx, orderID)
	if err != nil {
		return err
	}
	for i, d := range details {
		// Keep a stable order in the timeline (same second).
		if err := addEvent(ctx, tx, orderID, "COMPLETED", d, &actor.ID, now.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
			return err
		}
	}
	return nil
}

// Close closes a completed order (supervisor).
func (s *Service) Close(ctx context.Context, actor Actor, orderID int64, issues []IssueValidity) error {
	o, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		switch o.StatusID {
		case StatusDone, StatusInProgress, StatusPaused:
		default:
			return errf("Only completed maintenance orders can be closed")
		}
		if err := applyValidities(ctx, tx, o.ID, issues); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.work_intervals SET ended_at = $2 WHERE order_id = $1 AND ended_at IS NULL`, o.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, closed_at = $3, completed_at = COALESCE(completed_at, $3),
			updated_at = $3 WHERE id = $1`, o.ID, StatusClosed, now); err != nil {
			return err
		}
		if err := s.releaseForcedStatus(ctx, tx, o); err != nil {
			return err
		}
		return addEvent(ctx, tx, o.ID, "CLOSED", map[string]any{"By": displayName(actor)}, &actor.ID, now)
	})
	if err != nil {
		return err
	}
	techs, _ := Technicians(ctx, s.db, o.ID)
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_CLOSED_TECHNICIAN", fmt.Sprintf("Maintenance order %s for equipment %s has been closed", o.Name, o.AssetName), techs)
	s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_CLOSED_REQUESTOR", fmt.Sprintf("Your maintenance request %s for equipment %s has been closed", o.Name, o.AssetName),
		joinNonEmpty(usernameOf(ctx, s.db, o.RequestedBy)))
	return nil
}

// ChangeAlarmValidity updates verdicts from the early warning page.
func (s *Service) ChangeAlarmValidity(ctx context.Context, actor Actor, orderID int64, codes []int64, validities []IssueValidity) error {
	_, err := s.transition(ctx, orderID, func(tx pgx.Tx, o *Order, now time.Time) error {
		if err := applyValidities(ctx, tx, o.ID, validities); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET updated_at = $2, edited_by = $3 WHERE id = $1`, o.ID, now, actor.ID)
		return err
	})
	return err
}
