package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Client calls the backend API the way the web UI does. Sessions are created
// directly in the database for any user, so no passwords are needed.
type Client struct {
	DB     *pgxpool.Pool
	Base   string
	Org    string
	Prj    string
	http   *http.Client
	mu     sync.Mutex
	tokens map[string]string
	super  string
}

func newClient(db *pgxpool.Pool, base, org, prj string) *Client {
	return &Client{DB: db, Base: strings.TrimRight(base, "/"), Org: org, Prj: prj, tokens: map[string]string{},
		http: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // talks to the local backend (self-signed)
		}}}
}

// File is an uploaded file.
type File struct {
	Name, Type string
	Data       []byte
}

// APIError is an error returned by a script.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Msg) }

func (c *Client) superuser(ctx context.Context) (string, error) {
	if c.super != "" {
		return c.super, nil
	}
	err := c.DB.QueryRow(ctx, `SELECT username FROM core.users WHERE role = 'superuser' AND NOT is_deleted ORDER BY id LIMIT 1`).Scan(&c.super)
	return c.super, err
}

func (c *Client) token(ctx context.Context, username string) (string, error) {
	c.mu.Lock()
	tok := c.tokens[username]
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	var uid int64
	if err := c.DB.QueryRow(ctx, `SELECT id FROM core.users WHERE lower(username) = lower($1) AND NOT is_deleted`, username).Scan(&uid); err != nil {
		return "", fmt.Errorf("user %s: %w", username, err)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok = "sim-" + hex.EncodeToString(b)
	if _, err := c.DB.Exec(ctx, `INSERT INTO core.sessions (token, user_id, expires_at) VALUES ($1, $2, now() + interval '7 days')`, tok, uid); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[username] = tok
	c.mu.Unlock()
	return tok, nil
}

// Call runs a js-service script as a user ("" = superuser).
func (c *Client) Call(ctx context.Context, username, script string, form map[string]any) (any, error) {
	if username == "" {
		u, err := c.superuser(ctx)
		if err != nil {
			return nil, err
		}
		username = u
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token(ctx, username)
		if err != nil {
			return nil, err
		}
		data, err := c.post(ctx, tok, script, form)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			c.mu.Lock()
			delete(c.tokens, username)
			c.mu.Unlock()
			continue
		}
		return data, err
	}
	return nil, &APIError{Status: 401, Msg: "unauthorized"}
}

// MustCall is Call for the superuser.
func (c *Client) Super(ctx context.Context, script string, form map[string]any) (any, error) {
	return c.Call(ctx, "", script, form)
}

func (c *Client) post(ctx context.Context, token, script string, form map[string]any) (any, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		appendForm(w, k, form[k])
	}
	_ = w.Close()
	url := fmt.Sprintf("%s/api/js-service/exposed/execute/%s/%s/%s", c.Base, c.Org, c.Prj, script)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Nodelinx-Exposed-Session-Id", token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Data  any     `json:"data"`
		Error *string `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode != http.StatusOK || (env.Error != nil && *env.Error != "") {
		msg := string(raw)
		if env.Error != nil {
			msg = *env.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Msg: msg}
	}
	return env.Data, nil
}

// appendForm encodes values like axios toFormData.
func appendForm(w *multipart.Writer, key string, v any) {
	switch t := v.(type) {
	case nil:
		return
	case File:
		h := map[string][]string{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, t.Name)},
			"Content-Type":        {t.Type},
		}
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(t.Data)
	case string:
		_ = w.WriteField(key, t)
	case bool:
		_ = w.WriteField(key, fmt.Sprint(t))
	case float64:
		_ = w.WriteField(key, fnum(t))
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice {
			for i := 0; i < rv.Len(); i++ {
				appendForm(w, key+"[]", rv.Index(i).Interface())
			}
			return
		}
		_ = w.WriteField(key, fmt.Sprint(v))
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// Database helpers

func scalarInt(ctx context.Context, db *pgxpool.Pool, sql string, args ...any) (int64, error) {
	var v int64
	err := db.QueryRow(ctx, sql, args...).Scan(&v)
	return v, err
}

// idOf returns the id of the first row or 0 when none matches.
func idOf(ctx context.Context, db *pgxpool.Pool, sql string, args ...any) int64 {
	var v int64
	if err := db.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return 0
	}
	return v
}

func exists(ctx context.Context, db *pgxpool.Pool, sql string, args ...any) bool {
	var ok bool
	_ = db.QueryRow(ctx, `SELECT EXISTS (`+sql+`)`, args...).Scan(&ok)
	return ok
}

// getSetting reads a JSON value from core.settings.
func getSetting(ctx context.Context, db *pgxpool.Pool, key string, dst any) bool {
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM core.settings WHERE key = $1`, key).Scan(&raw); err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

func putSetting(ctx context.Context, db *pgxpool.Pool, key string, v any) error {
	raw, _ := json.Marshal(v)
	_, err := db.Exec(ctx, `INSERT INTO core.settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return err
}
