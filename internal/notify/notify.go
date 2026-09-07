// Package notify creates in-app notifications, pushes them to connected
// browsers over MQTT and sends periodic e-mail digests.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"afdd/internal/db"
)

// Publisher pushes a payload to a topic (implemented by the MQTT broker).
type Publisher interface {
	Publish(topic string, payload []byte) error
}

// Service creates notifications.
type Service struct {
	db    *db.DB
	pub   Publisher
	topic string
}

// New creates the notification service.
func New(d *db.DB, pub Publisher, topic string) *Service {
	return &Service{db: d, pub: pub, topic: topic}
}

// Notification describes a notification to create.
type Notification struct {
	Category       string
	Message        string
	MessageHTML    *string
	AssetID        *int64
	LocationID     *int64
	AdditionalData map[string]any
	// Recipients restricts the notification to these usernames (nil/empty
	// means every user allowed to see the category and location).
	Recipients []string
	Timestamp  time.Time
}

// Send stores the notification and pushes it to subscribers. Errors are
// logged and returned; callers usually ignore them so notification failures
// never break business operations.
func (s *Service) Send(ctx context.Context, n Notification) error {
	if s == nil {
		return nil
	}
	if n.Timestamp.IsZero() {
		n.Timestamp = time.Now().UTC()
	}
	if n.LocationID == nil && n.AssetID != nil {
		var loc int64
		if err := s.db.QueryRow(ctx, `SELECT location_id FROM asset.equipment WHERE id = $1`, *n.AssetID).Scan(&loc); err == nil {
			n.LocationID = &loc
		}
	}
	var additional []byte
	if n.AdditionalData != nil {
		additional, _ = json.Marshal(n.AdditionalData)
	}
	var recipients []string
	if len(n.Recipients) > 0 {
		recipients = n.Recipients
	}
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO core.notifications
		(category, message, message_html, asset_id, location_id, additional_data, recipients, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		n.Category, n.Message, n.MessageHTML, n.AssetID, n.LocationID, nullJSON(additional), recipients, n.Timestamp).Scan(&id)
	if err != nil {
		slog.Error("store notification", "category", n.Category, "err", err)
		return err
	}
	s.push(ctx, id, n)
	return nil
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// push publishes the notification in the format expected by the web UI.
func (s *Service) push(ctx context.Context, id int64, n Notification) {
	if s.pub == nil || s.topic == "" {
		return
	}
	var parentID *int64
	var locationName *string
	ancestors := []any{}
	if n.LocationID != nil {
		_ = s.db.QueryRow(ctx, `SELECT parent_id, name FROM asset.locations WHERE id = $1`, *n.LocationID).Scan(&parentID, &locationName)
		if ids, err := Ancestors(ctx, s.db, *n.LocationID); err == nil {
			for _, a := range ids {
				ancestors = append(ancestors, a)
			}
		}
	}
	var additional any
	if n.AdditionalData != nil {
		additional = n.AdditionalData
	}
	recipients := n.Recipients
	if recipients == nil {
		recipients = []string{}
	}
	msg := []map[string]any{{
		"id":             id,
		"category":       n.Category,
		"message":        n.Message,
		"messageHTML":    n.MessageHTML,
		"assetId":        n.AssetID,
		"locationId":     n.LocationID,
		"parentId":       parentID,
		"locationName":   locationName,
		"timestamp":      n.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
		"additionalData": additional,
		"isRead":         false,
		"ancestors":      ancestors,
		"recipients":     recipients,
	}}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if err := s.pub.Publish(s.topic, b); err != nil {
		slog.Warn("publish notification", "err", err)
	}
}

// Ancestors returns the location ids from the root down to locationID.
func Ancestors(ctx context.Context, q db.Querier, locationID int64) ([]int64, error) {
	ids, err := db.Int64s(ctx, q, `WITH RECURSIVE a AS (
			SELECT id, parent_id, 0 AS depth FROM asset.locations WHERE id = $1
			UNION ALL
			SELECT l.id, l.parent_id, a.depth + 1 FROM asset.locations l JOIN a ON l.id = a.parent_id
			WHERE a.depth < 100
		) SELECT id FROM a ORDER BY depth DESC`, locationID)
	return ids, err
}

// --- message templates (match the UI translation patterns) -----------------

// AlarmMessage formats an alarm notification.
func AlarmMessage(severity, alarmName, equipment string) string {
	if severity == "CRITICAL" {
		return fmt.Sprintf("Critical Alarm of %s occuring in Equipment %s", alarmName, equipment)
	}
	return fmt.Sprintf("Warning Alarm of %s occuring in Equipment %s", alarmName, equipment)
}

// StatusMessage formats a status change notification.
func StatusMessage(equipment, status string) string {
	return fmt.Sprintf("Equipment %s status change detected to %s", equipment, status)
}

// AbnormalMessage formats a parameter threshold notification.
func AbnormalMessage(equipment, parameter, condition string, value string) string {
	return fmt.Sprintf("%s: Abnormal Parameter is %s %s", equipment, parameter+" "+condition, value)
}

// SPCControlMessage formats an SPC out-of-control notification.
func SPCControlMessage(parameter string, at time.Time) string {
	return fmt.Sprintf("%s parameter is outside of the process control limit at %s", parameter, at.UTC().Format("2006-01-02 15:04"))
}

// SPCSpecMessage formats an SPC out-of-spec notification.
func SPCSpecMessage(parameter string, at time.Time) string {
	return fmt.Sprintf("%s parameter is outside of the specification limit at %s", parameter, at.UTC().Format("2006-01-02 15:04"))
}

// LicenseExpiryMessage formats a license expiry notification.
func LicenseExpiryMessage(product, licenseName string, hours int, base bool) string {
	if base {
		return fmt.Sprintf("Digital Twin AFDD %s Base License '%s' is going to expire in %d hour(s)", product, licenseName, hours)
	}
	return fmt.Sprintf("Digital Twin AFDD %s Equipment License '%s' is going to expire in %d hour(s)", product, licenseName, hours)
}
