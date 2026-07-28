// Package integration runs the backend against a real PostgreSQL database
// and an in-memory S3 server, calling the HTTP API exactly like the web UI
// does (axios multipart forms).
//
// Run with:
//
//	AFDD_TEST_DATABASE_URL=postgres://afdd:afdd@localhost:25432/afdd?sslmode=disable go test ./test/integration/
//
// A temporary database is created (and dropped afterwards) on that server.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"afdd/internal/api"
	"afdd/internal/auth"
	"afdd/internal/config"
	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/license"
	"afdd/internal/maint"
	"afdd/internal/notify"
	"afdd/internal/scheduler"
	"afdd/internal/storage"
)

const (
	adminUser = "superuser"
	adminPass = "Sup3r-Secret!"
	scriptURL = "/api/js-service/exposed/execute/nlx/afdd_daikin/"
	queryURL  = "/api/database-connection/exposed/execute/nlx/afdd_daikin/business/"
)

// env is the shared test environment.
type env struct {
	ctx    context.Context
	db     *db.DB
	srv    *api.Server
	http   *httptest.Server
	eng    *engine.Engine
	maint  *maint.Service
	hub    *datahub.Hub
	sched  *scheduler.Scheduler
	token  string
	cfg    *config.Config
	notify *notify.Service
}

var E *env

func TestMain(m *testing.M) {
	base := os.Getenv("AFDD_TEST_DATABASE_URL")
	if base == "" {
		fmt.Println("AFDD_TEST_DATABASE_URL not set - skipping integration tests")
		os.Exit(0)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	code, err := run(m, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M, base string) (int, error) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return 0, err
	}
	// AFDD_TEST_KEEP_DB=<name> keeps the seeded database (e.g. to browse it
	// with the web UI); it is recreated on every run.
	name := os.Getenv("AFDD_TEST_KEEP_DB")
	keep := name != ""
	if !keep {
		name = fmt.Sprintf("afdd_it_%d", time.Now().UnixNano())
	} else {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return 0, err
	}
	defer func() {
		if !keep {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		}
		admin.Close(context.Background())
	}()
	u, _ := url.Parse(base)
	u.Path = "/" + name
	dbURL := u.String()

	// AFDD_TEST_S3_ENDPOINT=host:port uses an external S3 server instead of
	// the in-memory one.
	s3Endpoint := os.Getenv("AFDD_TEST_S3_ENDPOINT")
	if s3Endpoint == "" {
		s3 := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
		defer s3.Close()
		s3Endpoint = strings.TrimPrefix(s3.URL, "http://")
	}

	cfg := &config.Config{OrgAlias: "nlx", ProjectAlias: "afdd_daikin", DatabaseURL: dbURL, SessionTTL: time.Hour,
		EvalInterval: time.Second, LogInterval: time.Second, OfflineTimeout: time.Hour, RetentionDays: 730,
		DefaultTimezone: "Asia/Singapore", ScriptTimeout: 2 * time.Second, EarlyWarningCooldown: time.Hour,
		NotificationTopic: "EPM_NOTIFICATION", DataDir: os.TempDir(), CORSOrigins: []string{"*"}}

	d, err := db.Open(ctx, dbURL)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	// Migrations must be idempotent across restarts.
	if err := d.Migrate(ctx); err != nil {
		return 0, fmt.Errorf("re-migrate: %w", err)
	}
	store, err := storage.New(ctx, storage.Options{Endpoint: s3Endpoint, AccessKey: "test", SecretKey: "testtest",
		Bucket: "afdd", Region: "us-east-1"})
	if err != nil {
		return 0, fmt.Errorf("storage: %w", err)
	}
	authSvc := auth.NewService(d, cfg.SessionTTL)
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, adminUser, adminPass); err != nil {
		return 0, err
	}
	n := notify.New(d, nil, cfg.NotificationTopic)
	hub := datahub.New(d, nil, cfg.ScriptTimeout, cfg.DataDir, "nlx.afdd_daikin")
	if err := hub.Start(ctx); err != nil {
		return 0, err
	}
	defer hub.Stop()
	lic, err := license.NewManager(d, "")
	if err != nil {
		return 0, err
	}
	loc, _ := time.LoadLocation(cfg.DefaultTimezone)
	eng := engine.New(d, hub, n, engine.Options{EvalInterval: cfg.EvalInterval, LogInterval: cfg.LogInterval, OfflineTimeout: cfg.OfflineTimeout,
		ScriptTimeout: cfg.ScriptTimeout, EarlyWarningCool: cfg.EarlyWarningCooldown})
	ms := maint.New(d, n, loc)
	ms.OnEquipmentChanged = eng.Invalidate
	eng.SetHooks(ms)
	srv := api.New(api.Deps{Config: cfg, DB: d, Store: store, Auth: authSvc, Notify: n, Hub: hub, Engine: eng, Maint: ms, License: lic})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	sched := scheduler.New(scheduler.Deps{DB: d, Notify: n, Maint: ms, CleanSessions: authSvc.CleanupExpired, RefreshKPIs: srv.RefreshKPIs,
		RetentionDays: cfg.RetentionDays})

	E = &env{ctx: ctx, db: d, srv: srv, http: ts, eng: eng, maint: ms, hub: hub, sched: sched, cfg: cfg, notify: n}
	engCtx, stopEngine := context.WithCancel(ctx)
	engDone := make(chan struct{})
	go func() { defer close(engDone); eng.Run(engCtx) }()
	defer func() { stopEngine(); <-engDone }()
	tok, err := E.login(adminUser, adminPass)
	if err != nil {
		return 0, fmt.Errorf("login: %w", err)
	}
	E.token = tok
	return m.Run(), nil
}

func (e *env) login(user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]any{"username": user, "password": pass})
	resp, err := http.Post(e.http.URL+"/api/user-management/exposed/get-session-add-expiry", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"nodelinx_exposed_session_id"`
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != 200 || out.Token == "" {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	return out.Token, nil
}

// File is an uploaded file in a form.
type File struct {
	Name    string
	Type    string
	Content []byte
}

// appendForm mimics axios toFormData: nulls are dropped, arrays of scalars
// become key[], objects become key[sub], everything else is stringified.
func appendForm(w *multipart.Writer, key string, v any) {
	switch t := v.(type) {
	case nil:
		return
	case File:
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, t.Name)}
		h["Content-Type"] = []string{t.Type}
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(t.Content)
	case string:
		_ = w.WriteField(key, t)
	case bool:
		_ = w.WriteField(key, fmt.Sprint(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			appendForm(w, key+"["+k+"]", t[k])
		}
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice {
			flat := true
			for i := 0; i < rv.Len(); i++ {
				k := reflect.Indirect(rv.Index(i)).Kind()
				if k == reflect.Map || k == reflect.Slice || k == reflect.Struct {
					flat = false
				}
			}
			for i := 0; i < rv.Len(); i++ {
				if flat {
					appendForm(w, key+"[]", rv.Index(i).Interface())
				} else {
					appendForm(w, fmt.Sprintf("%s[%d]", key, i), rv.Index(i).Interface())
				}
			}
			return
		}
		_ = w.WriteField(key, fmt.Sprint(v))
	}
}

type result struct {
	Status int
	Data   any
	Error  string
	Raw    []byte
}

func (r result) String() string {
	return fmt.Sprintf("status=%d error=%q body=%s", r.Status, r.Error, truncate(string(r.Raw), 600))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (e *env) post(path, token string, form map[string]any) result {
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
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set(auth.SessionHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{Status: -1, Error: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res := result{Status: resp.StatusCode, Raw: raw}
	var env struct {
		Data    any     `json:"data"`
		Error   *string `json:"error"`
		Message string  `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		res.Data = env.Data
		if env.Error != nil {
			res.Error = *env.Error
		}
	}
	return res
}

// JS calls a js-service script as the superuser.
func (e *env) JS(script string, form map[string]any) result {
	return e.post(scriptURL+script, e.token, form)
}

// JSAs calls a js-service script with another session.
func (e *env) JSAs(token, script string, form map[string]any) result {
	return e.post(scriptURL+script, token, form)
}

// DBQ calls a database-connection query; the response is a raw JSON array.
func (e *env) DBQ(query string, form map[string]any) (int, any) {
	r := e.post(queryURL+query, e.token, form)
	var v any
	_ = json.Unmarshal(r.Raw, &v)
	return r.Status, v
}

// ok calls a script and fails the test unless it succeeds.
func ok(t *testing.T, script string, form map[string]any) any {
	t.Helper()
	r := E.JS(script, form)
	if r.Status != 200 || r.Error != "" {
		t.Fatalf("%s failed: %s", script, r)
	}
	return r.Data
}

// fails calls a script and expects a 4xx error.
func fails(t *testing.T, script string, form map[string]any) result {
	t.Helper()
	r := E.JS(script, form)
	if r.Status < 400 || r.Status >= 500 {
		t.Fatalf("%s: expected client error, got %s", script, r)
	}
	return r
}

// --- small JSON helpers -----------------------------------------------------

func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", v, truncate(fmt.Sprint(v), 300))
	}
	return m
}

func arr(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got %T: %v", v, truncate(fmt.Sprint(v), 300))
	}
	return a
}

func num(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected number, got %T: %v", v, v)
	}
	return f
}

func id(t *testing.T, v any) int64 { t.Helper(); return int64(num(t, v)) }

func str(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected string, got %T: %v", v, v)
	}
	return s
}

// hasKeys asserts that an object contains every key (values may be null).
func hasKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, truncate(fmt.Sprint(m), 400))
		}
	}
}

// isoTS asserts a zod .datetime() compatible timestamp.
func isoTS(t *testing.T, v any) {
	t.Helper()
	s := str(t, v)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", s); err != nil {
		t.Fatalf("timestamp %q is not YYYY-MM-DDTHH:MM:SS.sssZ", s)
	}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("exec %s: %v", truncate(sql, 120), err)
	}
}

func (e *env) scalar(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %s: %v", truncate(sql, 120), err)
	}
	return v
}

func itoa(i int) string { return strconv.Itoa(i) }

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// schema mirrors a zod object: field -> "string" | "number" | "boolean" |
// "array" | "object" | "any", optionally suffixed with "|null" (nullable)
// and/or "?" (optional), or "enum:A,B,C".
type schema map[string]string

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// check validates an object against a schema (like zod's parse).
func check(t *testing.T, where string, v any, sc schema) {
	t.Helper()
	m, okm := v.(map[string]any)
	if !okm {
		t.Fatalf("%s: expected object, got %s", where, typeOf(v))
	}
	for field, spec := range sc {
		optional := strings.HasSuffix(spec, "?")
		spec = strings.TrimSuffix(spec, "?")
		nullable := strings.HasSuffix(spec, "|null")
		spec = strings.TrimSuffix(spec, "|null")
		val, present := m[field]
		if !present {
			if optional {
				continue
			}
			t.Fatalf("%s: missing field %q in %s", where, field, truncate(toJSON(m), 400))
		}
		if val == nil {
			if nullable || spec == "any" {
				continue
			}
			t.Fatalf("%s: field %q is null but not nullable", where, field)
		}
		if strings.HasPrefix(spec, "enum:") {
			s, _ := val.(string)
			allowed := strings.Split(strings.TrimPrefix(spec, "enum:"), ",")
			found := false
			for _, a := range allowed {
				if a == s {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: field %q = %v not in enum %v", where, field, val, allowed)
			}
			continue
		}
		if spec != "any" && typeOf(val) != spec {
			t.Fatalf("%s: field %q has type %s, want %s (value %v)", where, field, typeOf(val), spec, truncate(fmt.Sprint(val), 80))
		}
	}
}

// checkAll validates every element of an array.
func checkAll(t *testing.T, where string, v any, sc schema) {
	t.Helper()
	for i, it := range arr(t, v) {
		check(t, fmt.Sprintf("%s[%d]", where, i), it, sc)
	}
}
