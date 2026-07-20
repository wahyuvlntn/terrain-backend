package maint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
)

// Submit actions.
const (
	ActionCreated  = "CREATED"
	ActionApproved = "APPROVED"
	ActionRejected = "REJECTED"
	ActionSaved    = "SAVED"
)

// Actor is the user performing an action.
type Actor struct {
	ID       int64
	Username string
	FullName string
}

// SubmitRequest is the payload of /maintenance_order/submit.
type SubmitRequest struct {
	Action       string
	ID           *int64
	AssetID      *int64
	TypeID       *int64
	PriorityID   *int64
	SeverityID   *int64
	StatusID     *int64
	Description  *string
	PlannedStart *time.Time
	PlannedEnd   *time.Time
	RepeatTypeID *int64
	RepeatEnd    *time.Time
	Issues       []int64 // "instructions": main issue alarm code ids
	IssuesSet    bool
	OtherIssues  []int64
	OtherSet     bool
	Technicians  []string
	TechSet      bool
	RejectReason *string
	IsRecurring  bool
	RequestedBy  *string
}

// SubmitResult is returned to the UI.
type SubmitResult struct {
	Message  string
	OrderIDs []int64
}

const maxOccurrences = 500

func i32(p *int64) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// Submit handles create / approve / reject / save.
func (s *Service) Submit(ctx context.Context, actor Actor, req SubmitRequest) (*SubmitResult, error) {
	switch strings.ToUpper(req.Action) {
	case ActionCreated:
		return s.create(ctx, actor, req)
	case ActionApproved, ActionRejected, ActionSaved:
		if req.ID == nil || *req.ID <= 0 {
			return nil, &Error{Msg: "err_work_order_not_found"}
		}
		return s.update(ctx, actor, req)
	}
	return nil, &Error{Msg: "err_invalid_operation"}
}

func (s *Service) validateRefs(ctx context.Context, q db.Querier, req *SubmitRequest) error {
	if req.AssetID == nil {
		return &Error{Msg: "err_equipment_not_found"}
	}
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM asset.equipment WHERE id = $1)`, *req.AssetID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return &Error{Msg: "err_equipment_not_found"}
	}
	if req.TypeID == nil || (*req.TypeID != TypeCorrective && *req.TypeID != TypePreventive) {
		return &Error{Msg: "err_invalid_type"}
	}
	if req.SeverityID == nil || *req.SeverityID < 1 || *req.SeverityID > 4 {
		return errf("Severity is required")
	}
	if req.PriorityID != nil && (*req.PriorityID < 1 || *req.PriorityID > 3) {
		req.PriorityID = nil
	}
	if req.PlannedStart == nil {
		return errf("Planned start is required")
	}
	if req.PlannedEnd != nil && req.PlannedEnd.Before(*req.PlannedStart) {
		return &Error{Msg: "err_start_must_before_end"}
	}
	if req.RepeatTypeID != nil && (*req.RepeatTypeID < 1 || *req.RepeatTypeID > 4) {
		req.RepeatTypeID = nil
	}
	if req.RepeatTypeID != nil && req.RepeatEnd != nil && req.RepeatEnd.Before(*req.PlannedStart) {
		return errf("Repeat end cannot be before planned start")
	}
	return nil
}

// occurrenceStart returns the k-th occurrence start of a series.
func (s *Service) occurrenceStart(anchor time.Time, repeat int, k int) time.Time {
	local := anchor.In(s.loc)
	var t time.Time
	switch repeat {
	case 1:
		t = local.AddDate(0, 0, k)
	case 2:
		t = local.AddDate(0, 0, 7*k)
	case 3:
		t = addMonthsClamped(local, k)
	case 4:
		t = addMonthsClamped(local, 12*k)
	default:
		t = local
	}
	return t.UTC()
}

// addMonthsClamped adds months keeping the day within the target month
// (Jan 31 + 1 month = Feb 28/29 instead of Mar 3).
func addMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location()).AddDate(0, months, 0)
	last := first.AddDate(0, 1, -1).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// horizon is how far ahead open-ended series are generated.
const horizon = 90 * 24 * time.Hour

func (s *Service) create(ctx context.Context, actor Actor, req SubmitRequest) (*SubmitResult, error) {
	if err := s.validateRefs(ctx, s.db, &req); err != nil {
		return nil, err
	}
	status := StatusRequested
	if req.StatusID != nil && (*req.StatusID == StatusRequested || *req.StatusID == StatusOpen) {
		status = int(*req.StatusID)
	}
	requester := &actor.ID
	if req.RequestedBy != nil {
		if id, err := UserIDByUsername(ctx, s.db, *req.RequestedBy); err == nil && id != nil {
			requester = id
		}
	}
	var created []int64
	now := time.Now().UTC()
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var scheduleID *int64
		var duration time.Duration
		if req.PlannedEnd != nil {
			duration = req.PlannedEnd.Sub(*req.PlannedStart)
		}
		if req.RepeatTypeID != nil {
			var sid int64
			if err := tx.QueryRow(ctx, `INSERT INTO dm.maintenance_schedules (repeat_type_id, repeat_end, anchor_start, duration_sec)
				VALUES ($1, $2, $3, $4) RETURNING id`, *req.RepeatTypeID, req.RepeatEnd, *req.PlannedStart, int64(duration.Seconds())).Scan(&sid); err != nil {
				return err
			}
			scheduleID = &sid
		}
		starts := []time.Time{*req.PlannedStart}
		if scheduleID != nil {
			starts = s.seriesStarts(*req.PlannedStart, int(*req.RepeatTypeID), req.RepeatEnd, nil, now)
		}
		var lastStart time.Time
		for _, start := range starts {
			var end *time.Time
			if req.PlannedEnd != nil {
				e := start.Add(duration)
				end = &e
			}
			id, err := s.insertOrder(ctx, tx, orderInsert{
				ScheduleID: scheduleID, AssetID: *req.AssetID, TypeID: int(*req.TypeID), PriorityID: i32(req.PriorityID),
				SeverityID: int(*req.SeverityID), StatusID: status, Description: strOr(req.Description, ""),
				PlannedStart: start, PlannedEnd: end, RequestedBy: requester,
			})
			if err != nil {
				return err
			}
			if req.IssuesSet || req.OtherSet {
				if err := setIssues(ctx, tx, id, req.Issues, req.OtherIssues); err != nil {
					return err
				}
			}
			if req.TechSet {
				if _, err := setTechnicians(ctx, tx, id, req.Technicians); err != nil {
					return err
				}
			}
			if err := addEvent(ctx, tx, id, "CREATED", map[string]any{"By": displayName(actor)}, &actor.ID, now); err != nil {
				return err
			}
			created = append(created, id)
			lastStart = start
		}
		if scheduleID != nil {
			if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_schedules SET generated_until = $2 WHERE id = $1`, *scheduleID, lastStart); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(created) > 0 {
		if o, err := s.Get(ctx, s.db, created[0]); err == nil {
			msg := fmt.Sprintf("Maintenance order %s has been requested for equipment %s", o.Name, o.AssetName)
			if len(created) > 1 {
				msg = fmt.Sprintf("Recurring maintenance order %s (%d occurrences) has been requested for equipment %s", o.Name, len(created), o.AssetName)
			}
			s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_CREATED", msg, nil)
		}
	}
	return &SubmitResult{Message: "Maintenance order successfully created", OrderIDs: created}, nil
}

// seriesStarts lists occurrence starts strictly after `after` (or from the
// anchor when after is nil), bounded by repeatEnd (or the rolling horizon)
// and maxOccurrences.
func (s *Service) seriesStarts(anchor time.Time, repeat int, repeatEnd *time.Time, after *time.Time, now time.Time) []time.Time {
	limit := now.Add(horizon)
	if repeatEnd != nil {
		limit = *repeatEnd
	}
	k0 := 0
	if after != nil {
		k0 = s.estimateIndex(anchor, repeat, *after) - 1
		if k0 < 0 {
			k0 = 0
		}
	}
	var out []time.Time
	for k := k0; k < k0+maxOccurrences+2; k++ {
		t := s.occurrenceStart(anchor, repeat, k)
		if after != nil && !t.After(*after) {
			continue
		}
		if t.After(limit) || len(out) >= maxOccurrences {
			break
		}
		out = append(out, t)
	}
	if len(out) == 0 && after == nil {
		out = append(out, anchor)
	}
	return out
}

// estimateIndex approximates the occurrence index at time t.
func (s *Service) estimateIndex(anchor time.Time, repeat int, t time.Time) int {
	if !t.After(anchor) {
		return 0
	}
	d := t.Sub(anchor)
	switch repeat {
	case 1:
		return int(d.Hours() / 24)
	case 2:
		return int(d.Hours() / (24 * 7))
	case 3:
		a, b := anchor.In(s.loc), t.In(s.loc)
		return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	case 4:
		return t.In(s.loc).Year() - anchor.In(s.loc).Year()
	}
	return 0
}

type orderInsert struct {
	ScheduleID      *int64
	AssetID         int64
	TypeID          int
	PriorityID      *int
	SeverityID      int
	StatusID        int
	Description     string
	PlannedStart    time.Time
	PlannedEnd      *time.Time
	RequestedBy     *int64
	ApprovedBy      *int64
	FromSystem      bool
	IssueRelationID *int64
	IsCompound      bool
	IsDeduction     bool
	DegradationName *string
	DetectedAt      *time.Time
}

func (s *Service) insertOrder(ctx context.Context, tx pgx.Tx, in orderInsert) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO dm.maintenance_orders (schedule_id, asset_id, maintenance_type_id, priority_id, severity_id,
		status_id, description, planned_start, planned_end, requested_by, approved_by, from_system, issue_relation_id, is_compound,
		is_deduction, degradation_name, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		in.ScheduleID, in.AssetID, in.TypeID, in.PriorityID, in.SeverityID, in.StatusID, in.Description, in.PlannedStart, in.PlannedEnd,
		in.RequestedBy, in.ApprovedBy, in.FromSystem, in.IssueRelationID, in.IsCompound, in.IsDeduction, in.DegradationName, in.DetectedAt).Scan(&id)
	if err != nil {
		return 0, err
	}
	name, err := s.nextOrderName(ctx, tx, in.TypeID, time.Now())
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE dm.maintenance_orders SET name = $2 WHERE id = $1`, id, name)
	return id, err
}

// nextOrderName returns "<CM|PM>-<MM>-<YY>-<seq>" with a per type and
// month sequence (e.g. CM-12-25-015).
func (s *Service) nextOrderName(ctx context.Context, tx pgx.Tx, typeID int, at time.Time) (string, error) {
	prefix := "PM"
	if typeID == TypeCorrective {
		prefix = "CM"
	}
	lt := at.In(s.loc)
	base := fmt.Sprintf("%s-%02d-%02d-", prefix, int(lt.Month()), lt.Year()%100)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, base); err != nil {
		return "", err
	}
	var next int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(substring(name from '([0-9]+)$')::int), 0) + 1
		FROM dm.maintenance_orders WHERE name LIKE $1`, base+"%").Scan(&next); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%03d", base, next), nil
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func displayName(a Actor) string {
	if strings.TrimSpace(a.FullName) != "" {
		return a.FullName
	}
	return a.Username
}

// targets returns the orders affected by an update: the order itself plus,
// when isRecurring, every later occurrence of the series not started yet.
func (s *Service) targets(ctx context.Context, q db.Querier, o *Order, isRecurring bool) ([]int64, error) {
	ids := []int64{o.ID}
	if !isRecurring || o.ScheduleID == nil {
		return ids, nil
	}
	more, err := db.Int64s(ctx, q, `SELECT id FROM dm.maintenance_orders WHERE schedule_id = $1 AND id <> $2
		AND planned_start > $3 AND status_id IN (1, 3) ORDER BY planned_start`, *o.ScheduleID, o.ID, o.PlannedStart)
	if err != nil {
		return nil, err
	}
	return append(ids, more...), nil
}

func (s *Service) update(ctx context.Context, actor Actor, req SubmitRequest) (*SubmitResult, error) {
	action := strings.ToUpper(req.Action)
	var affected []*Order
	var newTechs = map[int64][]string{}
	now := time.Now().UTC()
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getForUpdate(ctx, tx, *req.ID)
		if err != nil {
			return err
		}
		switch action {
		case ActionApproved, ActionRejected:
			if o.StatusID != StatusRequested && !(action == ActionApproved && o.StatusID == StatusOpen) {
				return errf("Only requested maintenance orders can be %s", strings.ToLower(action))
			}
		case ActionSaved:
			if o.StatusID != StatusRequested && o.StatusID != StatusOpen {
				return errf("Only requested or open maintenance orders can be edited")
			}
		}
		if action != ActionRejected {
			// Fill missing values from the stored order.
			if req.AssetID == nil {
				req.AssetID = &o.AssetID
			}
			if req.TypeID == nil {
				t := int64(o.TypeID)
				req.TypeID = &t
			}
			if req.SeverityID == nil {
				sv := int64(o.SeverityID)
				req.SeverityID = &sv
			}
			if req.PlannedStart == nil {
				req.PlannedStart = &o.PlannedStart
			}
			if err := s.validateRefs(ctx, tx, &req); err != nil {
				return err
			}
			if action == ActionApproved {
				techs := req.Technicians
				if !req.TechSet {
					if techs, err = Technicians(ctx, tx, o.ID); err != nil {
						return err
					}
				}
				if len(techs) == 0 {
					return errf("Technicians are required")
				}
			}
		}

		ids, err := s.targets(ctx, tx, o, req.IsRecurring)
		if err != nil {
			return err
		}
		var duration *time.Duration
		if req.PlannedEnd != nil && req.PlannedStart != nil {
			d := req.PlannedEnd.Sub(*req.PlannedStart)
			duration = &d
		}
		for i, id := range ids {
			cur := o
			if i > 0 {
				if cur, err = s.Get(ctx, tx, id); err != nil {
					return err
				}
			}
			switch action {
			case ActionRejected:
				if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET status_id = $2, reject_reason = $3, approved_by = $4,
					edited_by = $4, updated_at = now() WHERE id = $1`, id, StatusRejected, strOr(req.RejectReason, ""), actor.ID); err != nil {
					return err
				}
				details := map[string]any{"By": displayName(actor)}
				if r := strOr(req.RejectReason, ""); r != "" {
					details["Reason"] = r
				}
				if err := addEvent(ctx, tx, id, "REJECTED", details, &actor.ID, now); err != nil {
					return err
				}
			default:
				start := cur.PlannedStart
				if i == 0 {
					start = *req.PlannedStart
				} else if req.PlannedStart != nil {
					// Keep each occurrence's date, apply the new time of day.
					ns := req.PlannedStart.In(s.loc)
					cs := cur.PlannedStart.In(s.loc)
					start = time.Date(cs.Year(), cs.Month(), cs.Day(), ns.Hour(), ns.Minute(), ns.Second(), 0, s.loc).UTC()
				}
				var end *time.Time
				if duration != nil {
					e := start.Add(*duration)
					end = &e
				} else if cur.PlannedEnd != nil && i > 0 {
					end = cur.PlannedEnd
				}
				status := cur.StatusID
				if action == ActionApproved {
					status = StatusOpen
				}
				var approvedBy *int64 = cur.ApprovedBy
				if action == ActionApproved {
					approvedBy = &actor.ID
				}
				if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET asset_id = $2, maintenance_type_id = $3, priority_id = $4,
					severity_id = $5, status_id = $6, description = $7, planned_start = $8, planned_end = $9, approved_by = $10,
					edited_by = $11, updated_at = now(), scheduled_notified = CASE WHEN planned_start <> $8 THEN FALSE ELSE scheduled_notified END
					WHERE id = $1`, id, *req.AssetID, *req.TypeID, i32(req.PriorityID), *req.SeverityID, status,
					strOr(req.Description, cur.Description), start, end, approvedBy, actor.ID); err != nil {
					return err
				}
				if req.IssuesSet || req.OtherSet {
					main, other, err := Issues(ctx, tx, id)
					if err != nil {
						return err
					}
					if req.IssuesSet {
						main = req.Issues
					}
					if req.OtherSet {
						other = req.OtherIssues
					}
					if err := setIssues(ctx, tx, id, main, other); err != nil {
						return err
					}
				}
				if req.TechSet {
					before, _ := Technicians(ctx, tx, id)
					if _, err := setTechnicians(ctx, tx, id, req.Technicians); err != nil {
						return err
					}
					after, _ := Technicians(ctx, tx, id)
					newTechs[id] = diff(after, before)
					if action == ActionApproved {
						newTechs[id] = after
					}
				}
				evStatus := "UPDATED"
				if action == ActionApproved {
					evStatus = "APPROVED"
				}
				if err := addEvent(ctx, tx, id, evStatus, map[string]any{"By": displayName(actor)}, &actor.ID, now); err != nil {
					return err
				}
			}
			upd, err := s.Get(ctx, tx, id)
			if err != nil {
				return err
			}
			affected = append(affected, upd)
		}
		// Repeat settings changes on a draft save regenerate the series.
		if action == ActionSaved && len(affected) > 0 {
			if err := s.applyRepeatChange(ctx, tx, affected[0], req, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	msg := "Maintenance order successfully updated"
	switch action {
	case ActionApproved:
		msg = "Maintenance order successfully approved"
	case ActionRejected:
		msg = "Maintenance order successfully rejected"
	}
	ids := make([]int64, 0, len(affected))
	for _, o := range affected {
		ids = append(ids, o.ID)
	}
	if len(affected) > 0 {
		first := affected[0]
		requester := usernameOf(ctx, s.db, first.RequestedBy)
		suffix := ""
		if len(affected) > 1 {
			suffix = fmt.Sprintf(" (and %d following orders)", len(affected)-1)
		}
		switch action {
		case ActionApproved:
			s.notifyOrder(ctx, first, "MAINTENANCE_ORDER_APPROVED", fmt.Sprintf("Maintenance order %s%s for equipment %s has been approved", first.Name, suffix, first.AssetName), joinNonEmpty(requester))
		case ActionRejected:
			s.notifyOrder(ctx, first, "MAINTENANCE_ORDER_REJECTED", fmt.Sprintf("Maintenance order %s%s for equipment %s has been rejected", first.Name, suffix, first.AssetName), joinNonEmpty(requester))
		}
		if action != ActionRejected {
			for _, o := range affected {
				techs := newTechs[o.ID]
				if len(techs) == 0 || (o.StatusID != StatusOpen && action != ActionApproved) {
					continue
				}
				s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_ASSIGNED", fmt.Sprintf("Maintenance order %s for equipment %s has been assigned to you", o.Name, o.AssetName), techs)
				break
			}
		}
	}
	return &SubmitResult{Message: msg, OrderIDs: ids}, nil
}

func diff(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[strings.ToLower(x)] = true
	}
	var out []string
	for _, x := range a {
		if !set[strings.ToLower(x)] {
			out = append(out, x)
		}
	}
	return out
}

// applyRepeatChange handles repeat settings edited on a draft save.
func (s *Service) applyRepeatChange(ctx context.Context, tx pgx.Tx, o *Order, req SubmitRequest, now time.Time) error {
	var curType *int64
	var curEnd *time.Time
	if o.ScheduleID != nil {
		if err := tx.QueryRow(ctx, `SELECT repeat_type_id, repeat_end FROM dm.maintenance_schedules WHERE id = $1`, *o.ScheduleID).Scan(&curType, &curEnd); err != nil {
			return err
		}
	}
	sameType := (curType == nil && req.RepeatTypeID == nil) || (curType != nil && req.RepeatTypeID != nil && *curType == *req.RepeatTypeID)
	sameEnd := (curEnd == nil && req.RepeatEnd == nil) || (curEnd != nil && req.RepeatEnd != nil && curEnd.Equal(*req.RepeatEnd))
	if sameType && sameEnd {
		return nil
	}
	if o.ScheduleID != nil {
		// Only "this and following" edits may change the series.
		if !req.IsRecurring {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dm.maintenance_orders WHERE schedule_id = $1 AND planned_start > $2 AND status_id IN (1, 3)`,
			*o.ScheduleID, o.PlannedStart); err != nil {
			return err
		}
	}
	if req.RepeatTypeID == nil {
		_, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET schedule_id = NULL WHERE id = $1`, o.ID)
		return err
	}
	var duration time.Duration
	if o.PlannedEnd != nil {
		duration = o.PlannedEnd.Sub(o.PlannedStart)
	}
	var sid int64
	if err := tx.QueryRow(ctx, `INSERT INTO dm.maintenance_schedules (repeat_type_id, repeat_end, anchor_start, duration_sec)
		VALUES ($1, $2, $3, $4) RETURNING id`, *req.RepeatTypeID, req.RepeatEnd, o.PlannedStart, int64(duration.Seconds())).Scan(&sid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_orders SET schedule_id = $2 WHERE id = $1`, o.ID, sid); err != nil {
		return err
	}
	last, err := s.generateOccurrences(ctx, tx, o, sid, int(*req.RepeatTypeID), req.RepeatEnd, now)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE dm.maintenance_schedules SET generated_until = $2 WHERE id = $1`, sid, last)
	return err
}

// generateOccurrences clones template into the series from index k0.
func (s *Service) generateOccurrences(ctx context.Context, tx pgx.Tx, template *Order, scheduleID int64, repeat int, repeatEnd *time.Time, now time.Time) (time.Time, error) {
	var anchor time.Time
	if err := tx.QueryRow(ctx, `SELECT anchor_start FROM dm.maintenance_schedules WHERE id = $1`, scheduleID).Scan(&anchor); err != nil {
		return time.Time{}, err
	}
	main, other, err := Issues(ctx, tx, template.ID)
	if err != nil {
		return time.Time{}, err
	}
	techs, err := Technicians(ctx, tx, template.ID)
	if err != nil {
		return time.Time{}, err
	}
	var duration *time.Duration
	if template.PlannedEnd != nil {
		d := template.PlannedEnd.Sub(template.PlannedStart)
		duration = &d
	}
	last := template.PlannedStart
	after := template.PlannedStart
	starts := s.seriesStarts(anchor, repeat, repeatEnd, &after, now)
	for _, start := range starts {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_orders WHERE schedule_id = $1 AND planned_start = $2)`, scheduleID, start).Scan(&exists); err != nil {
			return last, err
		}
		if exists {
			last = start
			continue
		}
		var end *time.Time
		if duration != nil {
			e := start.Add(*duration)
			end = &e
		}
		status := template.StatusID
		if status != StatusRequested && status != StatusOpen {
			status = StatusOpen
		}
		sid := scheduleID
		id, err := s.insertOrder(ctx, tx, orderInsert{
			ScheduleID: &sid, AssetID: template.AssetID, TypeID: template.TypeID, PriorityID: template.PriorityID,
			SeverityID: template.SeverityID, StatusID: status, Description: template.Description, PlannedStart: start, PlannedEnd: end,
			RequestedBy: template.RequestedBy, ApprovedBy: template.ApprovedBy,
		})
		if err != nil {
			return last, err
		}
		if err := setIssues(ctx, tx, id, main, other); err != nil {
			return last, err
		}
		if _, err := setTechnicians(ctx, tx, id, techs); err != nil {
			return last, err
		}
		if err := addEvent(ctx, tx, id, "CREATED", map[string]any{"By": "System (recurring schedule)"}, nil, now); err != nil {
			return last, err
		}
		last = start
	}
	return last, nil
}

// ExtendSchedules generates upcoming occurrences of open-ended series.
func (s *Service) ExtendSchedules(ctx context.Context) error {
	// Bounded series are fully generated when created; only open-ended
	// series need to be extended.
	rows, err := db.Rows(ctx, s.db, `SELECT id, repeat_type_id, repeat_end, generated_until FROM dm.maintenance_schedules
		WHERE repeat_end IS NULL`)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, r := range rows {
		sid := r["id"].(int64)
		repeat := int(r["repeat_type_id"].(int32))
		var repeatEnd *time.Time
		if t, ok := r["repeat_end"].(time.Time); ok {
			repeatEnd = &t
		}
		if gu, ok := r["generated_until"].(time.Time); ok && repeatEnd == nil && gu.After(now.Add(horizon-24*time.Hour)) {
			continue
		}
		err := s.db.Tx(ctx, func(tx pgx.Tx) error {
			var templateID int64
			err := tx.QueryRow(ctx, `SELECT id FROM dm.maintenance_orders WHERE schedule_id = $1 ORDER BY planned_start DESC LIMIT 1`, sid).Scan(&templateID)
			if err != nil {
				if db.IsNoRows(err) {
					return nil
				}
				return err
			}
			tmpl, err := s.Get(ctx, tx, templateID)
			if err != nil {
				return err
			}
			if tmpl.StatusID == StatusRejected {
				return nil
			}
			last, err := s.generateOccurrences(ctx, tx, tmpl, sid, repeat, repeatEnd, now)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE dm.maintenance_schedules SET generated_until = $2 WHERE id = $1`, sid, last)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Delete removes an order (and following occurrences when isRecurring).
func (s *Service) Delete(ctx context.Context, id int64, isRecurring bool) (int, error) {
	n := 0
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if o.StatusID == StatusInProgress || o.StatusID == StatusPaused {
			return errf("Maintenance orders in progress cannot be deleted")
		}
		ids, err := s.targets(ctx, tx, o, isRecurring)
		if err != nil {
			return err
		}
		if err := s.releaseForcedStatus(ctx, tx, o); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM dm.maintenance_orders WHERE id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if o.ScheduleID != nil {
			_, err = tx.Exec(ctx, `DELETE FROM dm.maintenance_schedules s WHERE id = $1
				AND NOT EXISTS (SELECT 1 FROM dm.maintenance_orders WHERE schedule_id = s.id)`, *o.ScheduleID)
			if err != nil {
				return err
			}
			if isRecurring {
				// Stop generating further occurrences.
				_, err = tx.Exec(ctx, `UPDATE dm.maintenance_schedules SET repeat_end = $2 WHERE id = $1`, *o.ScheduleID, o.PlannedStart)
			}
		}
		return err
	})
	if err == nil {
		s.equipmentChanged()
	}
	return n, err
}
