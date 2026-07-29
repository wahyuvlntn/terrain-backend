package integration

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"afdd/internal/notify"
)

type receivedMail struct {
	from, to string
	auth     string
	data     string
}

// fakeSMTP is a minimal SMTP server advertising AUTH LOGIN only (like
// Microsoft 365) that rejects lines longer than the RFC 5321 limit.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []receivedMail
	errs []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(line string) { fmt.Fprintf(c, "%s\r\n", line) }
	w("220 fake ESMTP")
	var cur receivedMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			w("250-fake")
			w("250-AUTH LOGIN")
			w("250 8BITMIME")
		case up == "AUTH LOGIN":
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, _ := r.ReadString('\n')
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, _ := r.ReadString('\n')
			du, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
			dp, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			cur.auth = string(du) + ":" + string(dp)
			w("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur.from = cmd[10:]
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.to = strings.Trim(cmd[8:], "<> ")
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if len(strings.TrimRight(l, "\r\n")) > 998 {
					s.mu.Lock()
					s.errs = append(s.errs, "line too long")
					s.mu.Unlock()
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			cur.data = b.String()
			s.mu.Lock()
			s.mail = append(s.mail, cur)
			s.mu.Unlock()
			cur = receivedMail{auth: cur.auth}
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func decodeBody(t *testing.T, data string) string {
	t.Helper()
	parts := strings.SplitN(data, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("no body in %q", truncate(data, 200))
	}
	if !strings.Contains(parts[0], "Content-Transfer-Encoding: base64") || !strings.Contains(parts[0], "Message-ID: <") {
		t.Fatalf("headers: %s", parts[0])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(parts[1], "\r\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEmailDigest(t *testing.T) {
	if S.assetID == 0 {
		t.Skip("requires TestScenario data")
	}
	smtp := newFakeSMTP(t)
	defer smtp.ln.Close()

	ok(t, "notification/registeremailnotification", map[string]any{"alias": "epm_email_notification", "interval": 1, "is_active": true,
		"is_secure": false, "name": "EPM Email Notification", "sender_email": "noreply@example.com", "sender_name": "EPM",
		"sender_password": "mail-pass", "smtp_host": "127.0.0.1", "smtp_port": smtp.port()})
	// tech1 was unsubscribed by the notification test: subscribe again.
	ok(t, "notification/submitunsubscribedusers", map[string]any{"selected_usernames": `[{"username":"tech1"}]`, "type_id": 1, "unselected_usernames": "[]"})

	// The first run only records the starting point.
	if err := E.notify.RunEmailDigests(E.ctx); err != nil {
		t.Fatal(err)
	}
	loc := S.floor
	other := E.scalar(t, `INSERT INTO asset.locations (name) VALUES ('Unassigned Site') RETURNING id`)
	long := strings.Repeat("Very long maintenance description. ", 80) // > 998 characters
	_ = E.notify.Send(E.ctx, notify.Notification{Category: "MAINTENANCE_ORDER_CREATED", Message: long, LocationID: &loc})
	_ = E.notify.Send(E.ctx, notify.Notification{Category: "MAINTENANCE_ORDER_CREATED", Message: "Order at another site", LocationID: &other})
	_ = E.notify.Send(E.ctx, notify.Notification{Category: "ALARM_CRITICAL", Message: "Not subscribed category", LocationID: &loc})
	E.exec(t, `UPDATE core.email_configs SET last_run_at = now() - interval '2 minutes'`)
	if err := E.notify.RunEmailDigests(E.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		smtp.mu.Lock()
		n := len(smtp.mail)
		smtp.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	smtp.mu.Lock()
	defer smtp.mu.Unlock()
	if len(smtp.errs) > 0 {
		t.Fatalf("smtp errors: %v", smtp.errs)
	}
	if len(smtp.mail) != 1 {
		t.Fatalf("expected exactly one digest (tech1), got %d", len(smtp.mail))
	}
	m := smtp.mail[0]
	if m.to != "tech1@example.com" || m.auth != "noreply@example.com:mail-pass" {
		t.Fatalf("mail envelope: to=%q auth=%q", m.to, m.auth)
	}
	body := decodeBody(t, m.data)
	if !strings.Contains(body, "Very long maintenance description.") {
		t.Fatal("digest does not contain the subscribed notification")
	}
	if strings.Contains(body, "another site") || strings.Contains(body, "Not subscribed") {
		t.Fatal("digest contains notifications outside the user's scope or categories")
	}
	E.exec(t, `UPDATE core.email_configs SET is_active = FALSE`)
	E.exec(t, `DELETE FROM asset.locations WHERE id = $1`, other)
}

func TestEmailDigestServerUnreachable(t *testing.T) {
	if S.assetID == 0 {
		t.Skip("requires TestScenario data")
	}
	ok(t, "notification/registeremailnotification", map[string]any{"alias": "epm_email_notification", "interval": 5, "is_active": true,
		"is_secure": false, "name": "EPM Email Notification", "sender_email": "noreply@example.com", "sender_name": "EPM",
		"sender_password": "", "smtp_host": "127.0.0.1", "smtp_port": 1})
	E.exec(t, `UPDATE core.email_configs SET last_run_at = NULL`)
	if err := E.notify.RunEmailDigests(E.ctx); err != nil { // baseline run
		t.Fatal(err)
	}
	before := E.scalar(t, `SELECT last_notification_id FROM core.email_configs WHERE alias = 'epm_email_notification'`)
	loc := S.floor
	_ = E.notify.Send(E.ctx, notify.Notification{Category: "MAINTENANCE_ORDER_CREATED", Message: "pending while the server is down", LocationID: &loc})
	E.exec(t, `UPDATE core.email_configs SET last_run_at = now() - interval '10 minutes'`)
	start := time.Now()
	if err := E.notify.RunEmailDigests(E.ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("digest took %s with an unreachable server", time.Since(start))
	}
	if after := E.scalar(t, `SELECT last_notification_id FROM core.email_configs WHERE alias = 'epm_email_notification'`); after != before {
		t.Fatalf("pending notifications were dropped (%d -> %d)", before, after)
	}
	if n := E.scalar(t, `SELECT count(*) FROM core.email_configs WHERE last_run_at > now() - interval '1 minute'`); n != 1 {
		t.Fatal("last run not recorded - the job would retry immediately")
	}
	E.exec(t, `UPDATE core.email_configs SET is_active = FALSE`)
}
