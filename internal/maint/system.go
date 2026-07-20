package maint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/engine"
)

// openStatuses are statuses of orders that are still being handled.
var openStatuses = []int{StatusRequested, StatusOpen, StatusInProgress, StatusPaused}

// CreateEarlyWarning implements engine.Hooks: it raises a corrective order
// for an issue detected through an issue relation, unless an unfinished
// order for the same equipment, relation and issue already exists.
func (s *Service) CreateEarlyWarning(ctx context.Context, ew engine.EarlyWarning) error {
	var created *Order
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		// Serialise detection per equipment.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4242, $1::int)`, int32(ew.AssetID%2147483647)); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_orders o
			JOIN dm.maintenance_order_issues i ON i.order_id = o.id AND NOT i.is_other
			WHERE o.asset_id = $1 AND o.from_system AND o.issue_relation_id = $2 AND i.alarm_code_id = $3
			AND o.status_id = ANY($4))`, ew.AssetID, ew.RelationID, ew.TargetCodeID, openStatuses).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		var code, codeName string
		if err := tx.QueryRow(ctx, `SELECT code, name FROM dm.alarm_codes WHERE id = $1`, ew.TargetCodeID).Scan(&code, &codeName); err != nil {
			if db.IsNoRows(err) {
				return nil
			}
			return err
		}
		severity := 3 // MEDIUM
		if ew.Severity == "CRITICAL" {
			severity = 1
		}
		var modelID int64
		if err := tx.QueryRow(ctx, `SELECT model_id FROM asset.equipment WHERE id = $1`, ew.AssetID).Scan(&modelID); err != nil {
			return err
		}
		var minutes int64
		_ = tx.QueryRow(ctx, `SELECT COALESCE(sum(i.duration), 0) FROM dm.instructions i
			JOIN dm.instruction_models im ON im.instruction_id = i.id AND im.model_id = $2
			WHERE i.alarm_code_id = $1 AND NOT i.is_deleted`, ew.TargetCodeID, modelID).Scan(&minutes)
		if minutes <= 0 {
			minutes = 60
		}
		start := ew.DetectedAt
		end := start.Add(time.Duration(minutes) * time.Minute)
		kind := "Early warning"
		switch {
		case ew.IsCompound && ew.IsDeduction:
			kind = "Compound degradation"
		case ew.IsCompound:
			kind = "Compound early warning"
		case ew.IsDeduction:
			kind = "Degradation"
		}
		desc := fmt.Sprintf("%s detected by issue relation '%s': %s - %s", kind, ew.RelationName, code, codeName)
		relID := ew.RelationID
		degName := ew.RelationName
		detected := ew.DetectedAt
		id, err := s.insertOrder(ctx, tx, orderInsert{
			AssetID: ew.AssetID, TypeID: TypeCorrective, SeverityID: severity, StatusID: StatusRequested, Description: desc,
			PlannedStart: start, PlannedEnd: &end, FromSystem: true, IssueRelationID: &relID, IsCompound: ew.IsCompound,
			IsDeduction: ew.IsDeduction, DegradationName: &degName, DetectedAt: &detected,
		})
		if err != nil {
			return err
		}
		if err := setIssues(ctx, tx, id, []int64{ew.TargetCodeID}, ew.OtherCodeIDs); err != nil {
			return err
		}
		status := strings.ToUpper(ew.Severity)
		if status == "" {
			status = "WARNING"
		}
		if _, err := tx.Exec(ctx, `UPDATE dm.maintenance_order_issues SET alarm_status = $3, detected_at = $4 WHERE order_id = $1 AND alarm_code_id = $2`,
			id, ew.TargetCodeID, status, ew.DetectedAt); err != nil {
			return err
		}
		for _, p := range ew.Parameters {
			if _, err := tx.Exec(ctx, `INSERT INTO dm.maintenance_order_parameters (order_id, equipment_parameter_id, condition, message, detected_at)
				VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, id, p.EPID, p.Condition, p.Message, ew.DetectedAt); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, id, "CREATED", map[string]any{"By": "System (early warning)", "Detected": desc}, nil, ew.DetectedAt); err != nil {
			return err
		}
		created, err = s.Get(ctx, tx, id)
		return err
	})
	if err != nil || created == nil {
		return err
	}
	s.notifyOrder(ctx, created, "MAINTENANCE_ORDER_CREATED",
		fmt.Sprintf("Early warning: maintenance order %s has been raised for equipment %s", created.Name, created.AssetName), nil)
	return nil
}

// CreateRunningHourOrder raises a preventive order when an equipment reaches
// its critical running hours.
func (s *Service) CreateRunningHourOrder(ctx context.Context, assetID int64, hours float64) (int64, error) {
	var id int64
	var created *Order
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dm.maintenance_orders WHERE asset_id = $1 AND from_system
			AND issue_relation_id IS NULL AND maintenance_type_id = $2 AND status_id = ANY($3))`, assetID, TypePreventive, openStatuses).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		now := time.Now().UTC()
		end := now.Add(2 * time.Hour)
		var err error
		id, err = s.insertOrder(ctx, tx, orderInsert{
			AssetID: assetID, TypeID: TypePreventive, SeverityID: 2, StatusID: StatusRequested,
			Description:  fmt.Sprintf("Running hours reached %.0f hours. Preventive maintenance required.", hours),
			PlannedStart: now, PlannedEnd: &end, FromSystem: true,
		})
		if err != nil {
			return err
		}
		if err := addEvent(ctx, tx, id, "CREATED", map[string]any{"By": "System (running hours)"}, nil, now); err != nil {
			return err
		}
		created, err = s.Get(ctx, tx, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	if created != nil {
		s.notifyOrder(ctx, created, "MAINTENANCE_ORDER_CREATED",
			fmt.Sprintf("Maintenance order %s has been raised for equipment %s (running hours limit)", created.Name, created.AssetName), nil)
	}
	return id, nil
}

// NotifyScheduled notifies technicians of open orders whose planned start
// has been reached.
func (s *Service) NotifyScheduled(ctx context.Context) error {
	ids, err := db.Int64s(ctx, s.db, `SELECT id FROM dm.maintenance_orders WHERE status_id = $1 AND NOT scheduled_notified
		AND planned_start <= now() AND planned_start > now() - interval '1 day' ORDER BY planned_start LIMIT 500`, StatusOpen)
	if err != nil {
		return err
	}
	for _, id := range ids {
		tag, err := s.db.Exec(ctx, `UPDATE dm.maintenance_orders SET scheduled_notified = TRUE WHERE id = $1 AND NOT scheduled_notified`, id)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		o, err := s.Get(ctx, s.db, id)
		if err != nil {
			continue
		}
		techs, _ := Technicians(ctx, s.db, id)
		s.notifyOrder(ctx, o, "MAINTENANCE_ORDER_SCHEDULED",
			fmt.Sprintf("Maintenance order %s for equipment %s is scheduled to start now", o.Name, o.AssetName), techs)
	}
	return nil
}
