package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"afdd/internal/db"
)

// EmailConfig is the SMTP configuration of an e-mail notification channel.
type EmailConfig struct {
	Alias          string
	Name           string
	SenderEmail    string
	SenderName     string
	SenderPassword string
	Host           string
	Port           int
	Secure         bool
	Interval       time.Duration
	Active         bool
	LastID         int64
	LastRun        *time.Time
}

// LoadEmailConfig reads the configuration of an e-mail channel.
func LoadEmailConfig(ctx context.Context, q db.Querier, alias string) (*EmailConfig, error) {
	c := &EmailConfig{Alias: alias}
	var minutes int
	err := q.QueryRow(ctx, `SELECT notification_name, sender_email, sender_name, sender_password, smtp_host, smtp_port,
		is_secure, interval_minutes, is_active, last_notification_id, last_run_at
		FROM core.email_configs WHERE alias = $1`, alias).Scan(&c.Name, &c.SenderEmail, &c.SenderName, &c.SenderPassword,
		&c.Host, &c.Port, &c.Secure, &minutes, &c.Active, &c.LastID, &c.LastRun)
	if err != nil {
		return nil, err
	}
	if minutes < 1 {
		minutes = 1
	}
	c.Interval = time.Duration(minutes) * time.Minute
	return c, nil
}

type digestItem struct {
	ID       int64
	Category string
	Message  string
	At       time.Time
	Location *int64
	Recips   []string
}

// RunEmailDigests sends pending notifications for every active e-mail
// channel whose interval elapsed. It is called periodically by the scheduler.
func (s *Service) RunEmailDigests(ctx context.Context) error {
	aliases, err := db.Strings(ctx, s.db, `SELECT alias FROM core.email_configs WHERE is_active`)
	if err != nil {
		return err
	}
	for _, alias := range aliases {
		cfg, err := LoadEmailConfig(ctx, s.db, alias)
		if err != nil {
			slog.Error("load email config", "alias", alias, "err", err)
			continue
		}
		if !cfg.Active || cfg.Host == "" || cfg.SenderEmail == "" {
			continue
		}
		if cfg.LastRun != nil && time.Since(*cfg.LastRun) < cfg.Interval {
			continue
		}
		if err := s.sendDigest(ctx, cfg); err != nil {
			slog.Error("send email digest", "alias", alias, "err", err)
		}
	}
	return nil
}

func (s *Service) sendDigest(ctx context.Context, cfg *EmailConfig) error {
	// On first activation only notifications created from now on are sent.
	if cfg.LastID == 0 {
		var maxID int64
		_ = s.db.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM core.notifications`).Scan(&maxID)
		_, err := s.db.Exec(ctx, `UPDATE core.email_configs SET last_notification_id = $2, last_run_at = now() WHERE alias = $1`, cfg.Alias, maxID)
		return err
	}

	rows, err := s.db.Query(ctx, `SELECT id, category, message, created_at, location_id, recipients
		FROM core.notifications WHERE id > $1 ORDER BY id LIMIT 5000`, cfg.LastID)
	if err != nil {
		return err
	}
	var items []digestItem
	var maxID = cfg.LastID
	for rows.Next() {
		var it digestItem
		if err := rows.Scan(&it.ID, &it.Category, &it.Message, &it.At, &it.Location, &it.Recips); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
		if it.ID > maxID {
			maxID = it.ID
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		_, err := s.db.Exec(ctx, `UPDATE core.email_configs SET last_run_at = now() WHERE alias = $1`, cfg.Alias)
		return err
	}

	// Subscriptions: user group -> categories.
	subs := map[int64]map[string]bool{}
	srows, err := s.db.Query(ctx, `SELECT egc.user_group_id, nc.alias FROM core.email_group_categories egc
		JOIN core.notification_categories nc ON nc.id = egc.category_id WHERE egc.notification_alias = $1`, cfg.Alias)
	if err != nil {
		return err
	}
	for srows.Next() {
		var g int64
		var cat string
		if err := srows.Scan(&g, &cat); err != nil {
			srows.Close()
			return err
		}
		if subs[g] == nil {
			subs[g] = map[string]bool{}
		}
		subs[g][cat] = true
	}
	srows.Close()

	type recipient struct {
		username, name, email string
		groupID               int64
	}
	var recipients []recipient
	if len(subs) > 0 {
		groupIDs := make([]int64, 0, len(subs))
		for g := range subs {
			groupIDs = append(groupIDs, g)
		}
		urows, err := s.db.Query(ctx, `SELECT u.username, u.full_name, u.email, u.user_group_id FROM core.users u
			WHERE NOT u.is_deleted AND u.user_group_id = ANY($1) AND u.email <> ''
			AND NOT EXISTS (SELECT 1 FROM core.email_unsubscribed_users x WHERE x.user_id = u.id AND x.type_id = 1)`, groupIDs)
		if err != nil {
			return err
		}
		for urows.Next() {
			var r recipient
			if err := urows.Scan(&r.username, &r.name, &r.email, &r.groupID); err != nil {
				urows.Close()
				return err
			}
			recipients = append(recipients, r)
		}
		urows.Close()
	}

	groupScopes := map[int64]map[int64]bool{}
	var sendErr error
	for _, r := range recipients {
		scope, ok := groupScopes[r.groupID]
		if !ok {
			ids, err := db.Int64s(ctx, s.db, `WITH RECURSIVE t AS (
				SELECT location_id AS id FROM core.user_group_locations WHERE user_group_id = $1
				UNION SELECT c.id FROM asset.locations c JOIN t ON c.parent_id = t.id) SELECT id FROM t`, r.groupID)
			if err != nil {
				return err
			}
			scope = map[int64]bool{}
			for _, id := range ids {
				scope[id] = true
			}
			groupScopes[r.groupID] = scope
		}
		var mine []digestItem
		for _, it := range items {
			if !subs[r.groupID][it.Category] {
				continue
			}
			if it.Location != nil && !scope[*it.Location] {
				continue
			}
			if len(it.Recips) > 0 && !containsFold(it.Recips, r.username) {
				continue
			}
			mine = append(mine, it)
		}
		if len(mine) == 0 {
			continue
		}
		if err := sendMail(cfg, r.email, r.name, mine); err != nil {
			var se *ServerError
			if errors.As(err, &se) {
				// The mail server is unreachable or rejects the account:
				// keep the notifications for the next run (after the
				// configured interval) instead of trying every recipient.
				_, uerr := s.db.Exec(ctx, `UPDATE core.email_configs SET last_run_at = now() WHERE alias = $1`, cfg.Alias)
				if uerr != nil {
					return uerr
				}
				return err
			}
			slog.Warn("send digest email", "to", r.email, "err", err)
			sendErr = err
		}
	}
	_, err = s.db.Exec(ctx, `UPDATE core.email_configs SET last_notification_id = $2, last_run_at = now() WHERE alias = $1`, cfg.Alias, maxID)
	if err != nil {
		return err
	}
	return sendErr
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func sendMail(cfg *EmailConfig, to, toName string, items []digestItem) error {
	sort.Slice(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	subject := fmt.Sprintf("%s - %d new notification(s)", cfg.Name, len(items))
	var b strings.Builder
	b.WriteString("<html><body style=\"font-family:Arial,sans-serif\">")
	b.WriteString("<p>Hello " + html.EscapeString(toName) + ",</p><p>The following notifications were raised:</p>")
	b.WriteString("<table cellpadding=\"6\" style=\"border-collapse:collapse;border:1px solid #ccc\">")
	b.WriteString("<tr style=\"background:#022545;color:#fff\"><th align=\"left\">Time (UTC)</th><th align=\"left\">Category</th><th align=\"left\">Message</th></tr>")
	for _, it := range items {
		b.WriteString("<tr><td style=\"border:1px solid #ccc\">" + it.At.UTC().Format("2006-01-02 15:04:05") + "</td>")
		b.WriteString("<td style=\"border:1px solid #ccc\">" + html.EscapeString(strings.ReplaceAll(it.Category, "_", " ")) + "</td>")
		b.WriteString("<td style=\"border:1px solid #ccc\">" + html.EscapeString(it.Message) + "</td></tr>")
	}
	b.WriteString("</table><p style=\"color:#888\">This message was generated automatically.</p></body></html>")
	return SendHTML(cfg, to, subject, b.String())
}

// ServerError is a failure to connect, secure or authenticate with the mail
// server (as opposed to a rejected recipient).
type ServerError struct{ Err error }

func (e *ServerError) Error() string { return "smtp server: " + e.Err.Error() }

func (e *ServerError) Unwrap() error { return e.Err }

// SendHTML sends a single HTML e-mail using the channel's SMTP settings.
func SendHTML(cfg *EmailConfig, to, subject, body string) error {
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("invalid recipient %q", to)
	}
	msg := buildMessage(cfg, to, subject, body)
	c, err := dialSMTP(cfg)
	if err != nil {
		return &ServerError{Err: err}
	}
	defer c.Close()
	if err := c.Mail(cfg.SenderEmail); err != nil {
		return &ServerError{Err: err}
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// dialSMTP connects, secures and authenticates an SMTP session.
func dialSMTP(cfg *EmailConfig) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}

	var c *smtp.Client
	encrypted := false
	if cfg.Secure && cfg.Port != 587 && cfg.Port != 25 {
		// Implicit TLS (SMTPS, usually port 465).
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, err
		}
		c, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return nil, err
		}
		encrypted = true
	} else {
		conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
		c, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				c.Close()
				return nil, err
			}
			encrypted = true
		} else if cfg.Secure {
			c.Close()
			return nil, errors.New("smtp server does not support STARTTLS")
		}
	}
	if cfg.SenderPassword != "" {
		if ok, mechs := c.Extension("AUTH"); ok {
			if !encrypted && !isLoopback(cfg.Host) {
				c.Close()
				return nil, errors.New("refusing to send SMTP credentials over an unencrypted connection; enable TLS on the mail server")
			}
			var a smtp.Auth
			switch upper := strings.ToUpper(mechs); {
			case strings.Contains(upper, "PLAIN"):
				a = &plainAuth{user: cfg.SenderEmail, pass: cfg.SenderPassword}
			case strings.Contains(upper, "LOGIN"):
				a = &loginAuth{user: cfg.SenderEmail, pass: cfg.SenderPassword}
			default:
				c.Close()
				return nil, fmt.Errorf("unsupported SMTP authentication mechanisms %q", mechs)
			}
			if err := c.Auth(a); err != nil {
				c.Close()
				return nil, err
			}
		}
	}
	return c, nil
}

// buildMessage renders the MIME message. The HTML body is base64 encoded in
// 76 character lines so the SMTP line length limit is never exceeded.
func buildMessage(cfg *EmailConfig, to, subject, body string) []byte {
	from := mail.Address{Name: cfg.SenderName, Address: cfg.SenderEmail}
	domain := "localhost"
	if i := strings.LastIndexByte(cfg.SenderEmail, '@'); i >= 0 && i < len(cfg.SenderEmail)-1 {
		domain = cfg.SenderEmail[i+1:]
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	headers := []string{
		"From: " + from.String(),
		"To: " + to,
		"Subject: " + mimeHeader(subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id[:]) + "@" + domain + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=\"UTF-8\"",
		"Content-Transfer-Encoding: base64",
	}
	var b strings.Builder
	b.WriteString(strings.Join(headers, crlf))
	b.WriteString(crlf + crlf)
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString(crlf)
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString(crlf)
	return []byte(b.String())
}

const crlf = string(rune(13)) + string(rune(10))

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// plainAuth implements AUTH PLAIN (RFC 4616). Encryption is checked by the
// caller.
type plainAuth struct{ user, pass string }

func (a *plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", plainResponse(a.user, a.pass), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// loginAuth implements AUTH LOGIN (used e.g. by Microsoft 365).
type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.Contains(prompt, "username"):
		return []byte(a.user), nil
	case strings.Contains(prompt, "password"):
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected server challenge %q", fromServer)
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + b64(s) + "?="
		}
	}
	return s
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// plainResponse builds the RFC 4616 message: NUL user NUL password.
func plainResponse(user, pass string) []byte {
	resp := make([]byte, 0, len(user)+len(pass)+2)
	resp = append(resp, 0)
	resp = append(resp, user...)
	resp = append(resp, 0)
	return append(resp, pass...)
}
