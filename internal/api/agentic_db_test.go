package api

// End-to-end checks of the agentic endpoints over HTTP against a scratch
// copy of the demo database (never the live one):
//
//	AFDD_API_TEST_DATABASE_URL=postgres://afdd:afdd@host:5432/afdd_scratch_api?sslmode=disable go test ./internal/api/ -run TestAgenticAPI
//
// The database must already hold the demo plant and migration 0005; the test
// writes freely into it (grants, sessions, jobs, analytics rows), so the name
// must contain "scratch" or "test".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"afdd/internal/auth"
	"afdd/internal/config"
	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/storage"
)

type apiEnv struct {
	t      *testing.T
	ctx    context.Context
	db     *db.DB
	srv    *Server
	http   *httptest.Server
	tokens map[string]string
}

type apiResp struct {
	Status int
	Data   json.RawMessage
	Error  string
	Header http.Header
	Raw    []byte
}

// formFile is a file field of a multipart call.
type formFile struct {
	Name, Type string
	Content    []byte
}

// writeForm mimics axios toFormData (nested objects as key[sub], arrays as
// key[i], nulls dropped, scalars stringified).
func writeForm(w *multipart.Writer, key string, v any) {
	switch t := v.(type) {
	case nil:
	case formFile:
		h := map[string][]string{"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, t.Name)},
			"Content-Type": {t.Type}}
		p, _ := w.CreatePart(h)
		_, _ = p.Write(t.Content)
	case map[string]any:
		for k, sub := range t {
			writeForm(w, key+"["+k+"]", sub)
		}
	case []any:
		for i, sub := range t {
			writeForm(w, fmt.Sprintf("%s[%d]", key, i), sub)
		}
	default:
		_ = w.WriteField(key, fmt.Sprint(t))
	}
}

func (e *apiEnv) do(user, script string, body map[string]any, multipartForm bool, headers map[string]string) apiResp {
	e.t.Helper()
	var buf bytes.Buffer
	ct := "application/json"
	if multipartForm {
		mw := multipart.NewWriter(&buf)
		for k, v := range body {
			writeForm(mw, k, v)
		}
		_ = mw.Close()
		ct = mw.FormDataContentType()
	} else {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/api/js-service/exposed/execute/nlx/afdd_daikin/"+script, &buf)
	req.Header.Set("Content-Type", ct)
	if user != "" {
		req.Header.Set(auth.SessionHeader, e.tokens[user])
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error any             `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	out := apiResp{Status: resp.StatusCode, Data: env.Data, Header: resp.Header, Raw: raw}
	if env.Error != nil {
		out.Error = fmt.Sprint(env.Error)
	}
	return out
}

func (e *apiEnv) ok(user, script string, body map[string]any) json.RawMessage {
	e.t.Helper()
	r := e.do(user, script, body, false, nil)
	if r.Status != 200 {
		e.t.Fatalf("%s as %s: %d %s", script, user, r.Status, r.Raw)
	}
	return r.Data
}

func (e *apiEnv) expect(r apiResp, status int, msg string) {
	e.t.Helper()
	if r.Status != status || !strings.Contains(r.Error, msg) {
		e.t.Fatalf("want %d %q, got %d %s", status, msg, r.Status, r.Raw)
	}
}

func (e *apiEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *apiEnv) scalar(sql string, args ...any) any {
	e.t.Helper()
	var v any
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func setupAPIEnv(t *testing.T) *apiEnv {
	dsn := os.Getenv("AFDD_API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AFDD_API_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if name := strings.Trim(u.Path, "/"); !strings.Contains(name, "scratch") && !strings.Contains(name, "test") {
		t.Fatalf("refusing to run against database %q (not a scratch database)", name)
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)

	s3 := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(s3.Close)
	docs, err := storage.New(ctx, storage.Options{Endpoint: strings.TrimPrefix(s3.URL, "http://"), AccessKey: "k", SecretKey: "s",
		Bucket: "afdd-docs", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{OrgAlias: "nlx", ProjectAlias: "afdd_daikin", DefaultTimezone: "Asia/Singapore", ScriptTimeout: time.Second}
	authSvc := auth.NewService(d, time.Hour)
	// The hub is not started: gateways read as disconnected, so plant writes are
	// validated and refused without touching any plant.
	hub := datahub.New(d, nil, time.Second, os.TempDir(), "nlx.afdd_daikin")
	srv := New(Deps{Config: cfg, DB: d, Auth: authSvc, Hub: hub, DocsStore: docs})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &apiEnv{t: t, ctx: ctx, db: d, srv: srv, http: ts, tokens: map[string]string{}}
	// Start from empty agentic tables so the test can be rerun (scratch only).
	// Faults: only this test's rule (the engine package test runs in parallel).
	e.exec(`TRUNCATE sim.jobs, analytics.recommendations, analytics.optimizer_runs, analytics.anomalies, analytics.llm_usage,
		rag.documents CASCADE`)
	e.exec(`DELETE FROM analytics.faults WHERE rule_code = 'CT-014'`)

	// Grants of DESIGN §3 for the groups used here (the simulator seeds them).
	grant := func(group int, perms ...string) {
		for _, p := range perms {
			gp := strings.SplitN(p, ".", 2)
			e.exec(`INSERT INTO core.user_group_permissions (user_group_id, permission_id, value)
				SELECT $1, p.id, TRUE FROM core.permissions p JOIN core.permission_groups g ON g.id = p.permission_group_id
				WHERE g.alias = $2 AND p.alias = $3 ON CONFLICT DO NOTHING`, group, gp[0], gp[1])
		}
	}
	grant(4, "aiassistant.read", "aiinsights.read", "simulation.read", "simulation.create") // control room
	grant(3, "aiassistant.read", "aiinsights.read", "simulation.read")                      // FAB1 technicians
	grant(2, "aiassistant.read")                                                            // CUB technicians
	// The CUB technicians stand for a group with the assistant only (the demo
	// seed also gives them aiinsights.read and simulation.read).
	e.exec(`DELETE FROM core.user_group_permissions gp USING core.permissions p, core.permission_groups g
		WHERE gp.user_group_id = 2 AND gp.permission_id = p.id AND p.permission_group_id = g.id
		AND g.alias IN ('aiinsights', 'simulation')`)
	for _, user := range []string{"superuser", "sarah_lim", "jason_teo", "hafiz_rahman", "ahmad_faizal", "siti_rahmah"} {
		tok := fmt.Sprintf("apitest-%s-%d", user, time.Now().UnixNano())
		e.exec(`INSERT INTO core.sessions (token, user_id, expires_at) SELECT $1, id, now() + interval '1 hour' FROM core.users WHERE username = $2`,
			tok, user)
		e.tokens[user] = tok
	}
	return e
}

func TestAgenticAPI(t *testing.T) {
	e := setupAPIEnv(t)
	agent := map[string]string{"X-AFDD-Origin": "agent"}

	// Notifications to the runner and the analytics worker.
	listener, err := pgx.Connect(e.ctx, os.Getenv("AFDD_API_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(e.ctx)
	for _, ch := range []string{"sim_jobs", "rag_documents", "optimizer_runs"} {
		if _, err := listener.Exec(e.ctx, "LISTEN "+ch); err != nil {
			t.Fatal(err)
		}
	}
	drain := func() map[string][]string {
		got := map[string][]string{}
		for {
			ctx, cancel := context.WithTimeout(e.ctx, 300*time.Millisecond)
			n, err := listener.WaitForNotification(ctx)
			cancel()
			if err != nil {
				return got
			}
			got[n.Channel] = append(got[n.Channel], n.Payload)
		}
	}

	type jobRow struct {
		ID             int64    `json:"id"`
		Status         string   `json:"status"`
		Origin         string   `json:"origin"`
		Variants       []string `json:"variants"`
		SimStart       *string  `json:"sim_start"`
		ApprovedByName *string  `json:"approved_by_name"`
		RequestedBy    *int64   `json:"requested_by"`
	}
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	spec := map[string]any{
		"start": start, "duration_min": 120, "initial": "live",
		"actions": []any{map[string]any{"at_min": 0, "kind": "fault", "asset": "CT_01", "fault": "ct_fouling", "value": 0.8}},
		"variants": []any{
			map[string]any{"name": "baseline", "actions": []any{}},
			map[string]any{"name": "recommended", "actions": []any{map[string]any{"at_min": 0, "kind": "control", "asset": "ct_01",
				"param": "fan_mode", "value": 1}, map[string]any{"at_min": 0, "kind": "control", "asset": "CT_01", "param": "fan_speed_sp", "value": 85}}},
		},
	}

	t.Run("fault catalog", func(t *testing.T) {
		e.t = t
		all := decode[[]twinFaultKind](t, e.ok("jason_teo", "simulation/getfaultcatalog", nil))
		if len(all) != 7 || all[0].Kind != "chwst_bias" || len(all[0].Assets) != 3 {
			t.Fatalf("catalog %+v", all)
		}
		if fab := decode[[]twinFaultKind](t, e.ok("hafiz_rahman", "simulation/getfaultcatalog", nil)); len(fab) != 0 {
			t.Fatalf("FAB1-only user sees CUB faults %+v", fab)
		}
		e.expect(e.do("ahmad_faizal", "simulation/getfaultcatalog", nil, false, nil), 403, "permission")
	})

	var queued []int64
	t.Run("submit", func(t *testing.T) {
		e.t = t
		e.expect(e.do("jason_teo", "simulation/submit", map[string]any{"title": "x", "spec": spec}, false, agent), 403,
			"This action needs a person")
		e.expect(e.do("hafiz_rahman", "simulation/submit", map[string]any{"title": "x", "spec": spec}, false, nil), 403, "permission")
		e.expect(e.do("jason_teo", "simulation/submit", map[string]any{"title": "x"}, false, nil), 400, "spec is required")
		bad := map[string]any{"duration_min": 60, "actions": []any{map[string]any{"kind": "control", "asset": "CT_01", "param": "fan_speed_sp",
			"value": 85}}, "initial": "nominal"}
		e.expect(e.do("jason_teo", "simulation/submit", map[string]any{"title": "x", "spec": bad}, false, nil), 400, "in automatic")

		for i := 0; i < simMaxActive; i++ {
			r := decode[jobRow](t, e.ok("jason_teo", "simulation/submit", map[string]any{"title": fmt.Sprintf("Tower fouling %d", i),
				"spec": spec, "rationale": "test"}))
			if r.Status != "QUEUED" || r.Origin != "ui" || strings.Join(r.Variants, ",") != "baseline,recommended" || r.SimStart == nil {
				t.Fatalf("job row %+v", r)
			}
			queued = append(queued, r.ID)
		}
		e.expect(e.do("jason_teo", "simulation/submit", map[string]any{"title": "4th", "spec": spec}, false, nil), 409, "At most 3")

		// axios multipart encoding (every leaf a string), spec as nested fields.
		r := e.do("superuser", "simulation/submit", map[string]any{"title": "Multipart", "spec": map[string]any{"duration_min": 30,
			"stochastic": true, "actions": []any{map[string]any{"at_min": 5, "kind": "trip", "asset": "CH_02"}}}}, true, nil)
		if r.Status != 200 {
			t.Fatalf("multipart submit %s", r.Raw)
		}
		// spec sent as a JSON string field.
		r = e.do("superuser", "simulation/submit", map[string]any{"title": "String spec", "spec": `{"duration_min": 15}`}, true, nil)
		if r.Status != 200 {
			t.Fatalf("string spec submit %s", r.Raw)
		}
		stored := e.scalar(`SELECT spec::text FROM sim.jobs WHERE id = $1`, decode[jobRow](t, r.Data).ID).(string)
		if !strings.Contains(stored, `"variants": [{"name": "baseline", "actions": []}]`) || !strings.Contains(stored, `"initial": "live"`) {
			t.Fatalf("stored spec not normalised: %s", stored)
		}
		if n := drain()["sim_jobs"]; len(n) != 5 {
			t.Fatalf("sim_jobs notifications %v", n)
		}
	})

	var proposed []int64
	t.Run("propose approve reject cancel", func(t *testing.T) {
		e.t = t
		// People submit: proposals come from the agent path only.
		e.expect(e.do("jason_teo", "simulation/propose", map[string]any{"title": "Mine", "spec": spec, "rationale": "r"}, false, nil), 403,
			"proposed by the AI assistant")
		for i := 0; i < simMaxProposed; i++ {
			r := e.do("jason_teo", "simulation/propose", map[string]any{"title": fmt.Sprintf("Proposal %d", i), "spec": spec,
				"rationale": "Optimizer suggests a faster fan", "agent_session_id": "sess-1"}, false, agent)
			if r.Status != 200 {
				t.Fatalf("proposal %d %s", r.Status, r.Raw)
			}
			job := decode[jobRow](t, r.Data)
			if job.Status != "PROPOSED" || job.Origin != "agent" {
				t.Fatalf("proposal %+v", job)
			}
			proposed = append(proposed, job.ID)
		}
		e.expect(e.do("jason_teo", "simulation/propose", map[string]any{"title": "6th", "spec": spec, "rationale": "r",
			"agent_session_id": "sess-1"}, false, agent), 409, "already have 5")
		e.expect(e.do("jason_teo", "simulation/propose", map[string]any{"title": "no rationale", "spec": spec}, false, agent), 400, "rationale")
		// The quota is per person: another (caller-chosen) session id does not reset it.
		e.expect(e.do("jason_teo", "simulation/propose", map[string]any{"title": "Other chat", "spec": spec, "rationale": "r",
			"agent_session_id": "sess-2"}, false, agent), 409, "already have 5")

		e.expect(e.do("jason_teo", "simulation/approve", map[string]any{"job_id": proposed[0]}, false, agent), 403, "This action needs a person")
		e.expect(e.do("jason_teo", "simulation/approve", map[string]any{"job_id": proposed[0]}, false, nil), 409, "At most 3")
		cancelled := decode[jobRow](t, e.ok("jason_teo", "simulation/cancel", map[string]any{"job_id": queued[2], "reason": "not needed"}))
		if cancelled.Status != "CANCELLED" {
			t.Fatalf("cancel %+v", cancelled)
		}
		approved := decode[jobRow](t, e.ok("jason_teo", "simulation/approve", map[string]any{"job_id": proposed[0]}))
		if approved.Status != "QUEUED" || approved.ApprovedByName == nil || *approved.ApprovedByName == "" {
			t.Fatalf("approve %+v", approved)
		}
		e.expect(e.do("jason_teo", "simulation/approve", map[string]any{"job_id": proposed[0]}, false, nil), 409, "Only a proposed")

		e.expect(e.do("jason_teo", "simulation/reject", map[string]any{"job_id": proposed[1]}, false, agent), 403, "needs a person")
		rej := decode[map[string]any](t, e.ok("jason_teo", "simulation/reject", map[string]any{"job_id": proposed[1], "reason": "Too risky now"}))
		if rej["status"] != "REJECTED" {
			t.Fatalf("reject %v", rej)
		}
		got := decode[map[string]any](t, e.ok("jason_teo", "simulation/getjob", map[string]any{"job_id": proposed[1]}))
		if got["decision_note"] != "Too risky now" || got["agent_session_id"] != "sess-1" || got["spec"] == nil || got["summary"] == nil {
			t.Fatalf("getjob %v", got)
		}
		// A running job can no longer be cancelled.
		e.exec(`UPDATE sim.jobs SET status = 'RUNNING' WHERE id = $1`, queued[0])
		e.expect(e.do("jason_teo", "simulation/cancel", map[string]any{"job_id": queued[0]}, false, nil), 409, "proposed or queued")
		// Cancelling needs a person: the requester or an administrator.
		e.expect(e.do("jason_teo", "simulation/cancel", map[string]any{"job_id": proposed[3]}, false, agent), 403, "needs a person")
		e.expect(e.do("siti_rahmah", "simulation/cancel", map[string]any{"job_id": proposed[3]}, false, nil), 403, "Only the person who requested")
		if r := decode[jobRow](t, e.ok("sarah_lim", "simulation/cancel", map[string]any{"job_id": proposed[3]})); r.Status != "CANCELLED" {
			t.Fatalf("admin cancel %+v", r)
		}
		// The AI services' own jobs: administrators only.
		sys := e.scalar(`INSERT INTO sim.jobs (kind, title, origin, status, spec, step_s) VALUES ('scenario', 'Calibration', 'analytics', 'QUEUED',
			'{"duration_min": 60, "actions": [], "variants": [{"name": "baseline", "actions": []}]}', 60) RETURNING id`).(int64)
		e.expect(e.do("jason_teo", "simulation/cancel", map[string]any{"job_id": sys}, false, nil), 403, "administrator")
		if r := decode[jobRow](t, e.ok("superuser", "simulation/cancel", map[string]any{"job_id": sys})); r.Status != "CANCELLED" {
			t.Fatalf("superuser cancel %+v", r)
		}
		// A proposal whose start has passed cannot be approved.
		e.exec(`UPDATE sim.jobs SET spec = jsonb_set(spec, '{start}', to_jsonb($2::text)) WHERE id = $1`, proposed[2],
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
		e.exec(`UPDATE sim.jobs SET status = 'SUCCEEDED' WHERE id = $1`, queued[1]) // frees a slot
		e.expect(e.do("jason_teo", "simulation/approve", map[string]any{"job_id": proposed[2]}, false, nil), 409, "start has passed")
		// 5 proposals + cancel + approve + reject + 2 cancels (refused calls notify nothing)
		if n := drain()["sim_jobs"]; len(n) != 10 {
			t.Fatalf("sim_jobs notifications %v", n)
		}
	})

	t.Run("list and scope", func(t *testing.T) {
		e.t = t
		type list struct {
			Total int      `json:"total"`
			Jobs  []jobRow `json:"jobs"`
		}
		all := decode[list](t, e.ok("jason_teo", "simulation/listjobs", map[string]any{"limit": 100}))
		if all.Total < 10 || len(all.Jobs) != all.Total {
			t.Fatalf("list %d/%d", len(all.Jobs), all.Total)
		}
		pr := decode[list](t, e.ok("jason_teo", "simulation/listjobs", map[string]any{"status": "PROPOSED,QUEUED", "origin": "agent"}))
		for _, j := range pr.Jobs {
			if j.Origin != "agent" || (j.Status != "PROPOSED" && j.Status != "QUEUED") {
				t.Fatalf("filtered list row %+v", j)
			}
		}
		page := decode[list](t, e.ok("jason_teo", "simulation/listjobs", map[string]any{"limit": 2, "offset": 1}))
		if len(page.Jobs) != 2 || page.Total != all.Total || page.Jobs[0].ID != all.Jobs[1].ID {
			t.Fatalf("paging %+v", page)
		}
		e.expect(e.do("jason_teo", "simulation/listjobs", map[string]any{"status": "DONE"}, false, nil), 400, "Unknown job status")
		// FAB1-only: jobs touching CUB equipment are hidden, plant-wide ones without actions are not.
		fab := decode[list](t, e.ok("hafiz_rahman", "simulation/listjobs", map[string]any{"limit": 100}))
		for _, j := range fab.Jobs {
			if j.ID == queued[0] || j.ID == proposed[0] {
				t.Fatalf("FAB1 user sees CUB job %d", j.ID)
			}
		}
		if fab.Total == 0 {
			t.Fatal("the job without actions should be visible")
		}
		e.expect(e.do("hafiz_rahman", "simulation/getjob", map[string]any{"job_id": queued[0]}, false, nil), 404, "not found")
	})

	t.Run("frames events trend", func(t *testing.T) {
		e.t = t
		job := queued[0]
		t0 := time.Now().UTC().Truncate(time.Minute)
		var cwst, temp []float32
		for i := 0; i < 180; i++ {
			cwst = append(cwst, 29+0.01*float32(i%7))
			temp = append(temp, 6.5)
		}
		e.exec(`INSERT INTO sim.series (job_id, variant, asset_alias, param_alias, t0, step_s, measured) VALUES
			($1, 'recommended', 'CT_01', 'cwst', $2, 60, $3), ($1, 'recommended', 'CHWS_01', 'chws_temp', $4, 60, $5),
			($1, 'baseline', 'CT_01', 'cwst', $2, 60, $3)`, job, t0, cwst, t0.Add(-2*time.Minute), temp)
		e.exec(`INSERT INTO sim.states (job_id, variant, asset_alias, kind, value, start_ts, end_ts) VALUES
			($1, 'recommended', 'CT_01', 'status', 'RUNNING', $2, $3), ($1, 'recommended', 'CT_01', 'status', 'IDLE', $3, NULL),
			($1, 'recommended', 'CT_01', 'condition', 'NORMAL', $2, NULL)`, job, t0, t0.Add(150*time.Minute))
		e.exec(`INSERT INTO sim.events (job_id, variant, ts, asset_alias, kind, code, severity, message, value) VALUES
			($1, 'recommended', $2, 'CT_01', 'fault_injected', 'CT-014', NULL, 'ct_fouling 0.8', 0.8),
			($1, 'recommended', $3, 'CT_01', 'alarm', 'CT-002', 'WARNING', 'High CWST', 29.9),
			($1, 'recommended', $3, NULL, 'note', NULL, NULL, 'plant note', NULL),
			($1, 'baseline', $2, 'CT_01', 'note', NULL, NULL, 'baseline note', NULL)`, job, t0, t0.Add(30*time.Minute))

		type frames struct {
			JobID   int64                            `json:"job_id"`
			Variant string                           `json:"variant"`
			T0      string                           `json:"t0"`
			StepS   int                              `json:"step_s"`
			Steps   int                              `json:"steps"`
			Assets  []map[string]any                 `json:"assets"`
			Series  map[string]map[string][]*float64 `json:"series"`
			States  []map[string]any                 `json:"states"`
			Events  []map[string]any                 `json:"events"`
		}
		f := decode[frames](t, e.ok("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "RECOMMENDED"}))
		if f.Variant != "recommended" || f.StepS != 60 || f.Steps != 182 || len(f.Assets) != 2 || len(f.States) != 3 || len(f.Events) != 3 {
			t.Fatalf("frames variant %s step %d steps %d assets %d states %d events %d", f.Variant, f.StepS, f.Steps, len(f.Assets), len(f.States), len(f.Events))
		}
		if f.T0 != t0.Add(-2*time.Minute).Format("2006-01-02T15:04:05.000Z") || f.Series["CT_01"]["cwst"][0] != nil ||
			*f.Series["CT_01"]["cwst"][2] != 29 || *f.Series["CHWS_01"]["chws_temp"][0] != 6.5 {
			t.Fatalf("alignment t0 %s cwst %v", f.T0, f.Series["CT_01"]["cwst"][:4])
		}
		if f.Assets[0]["alias"] != "CHWS_01" || f.Assets[1]["type_name"] != "Cooling Tower" {
			t.Fatalf("assets %v", f.Assets)
		}
		one := decode[frames](t, e.ok("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "recommended", "aliases": "ct_01"}))
		if len(one.Series) != 1 || one.Series["CT_01"] == nil || len(one.Events) != 3 {
			t.Fatalf("aliases filter series %v events %d", one.Series, len(one.Events))
		}
		def := decode[frames](t, e.ok("jason_teo", "simulation/getframes", map[string]any{"job_id": job}))
		if def.Variant != "baseline" {
			t.Fatalf("default variant %s", def.Variant)
		}
		e.expect(e.do("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "x"}, false, nil), 404, "Unknown variant")

		type events struct {
			Total  int              `json:"total"`
			Events []map[string]any `json:"events"`
		}
		ev := decode[events](t, e.ok("jason_teo", "simulation/getevents", map[string]any{"job_id": job, "kinds": "alarm,fault_injected",
			"asset": "CT_01", "variant": "recommended"}))
		if ev.Total != 2 || ev.Events[0]["kind"] != "fault_injected" || ev.Events[1]["severity"] != "WARNING" {
			t.Fatalf("events %+v", ev)
		}
		all := decode[events](t, e.ok("jason_teo", "simulation/getevents", map[string]any{"job_id": job, "limit": 1}))
		if all.Total != 4 || len(all.Events) != 1 {
			t.Fatalf("events paging %+v", all)
		}
		e.expect(e.do("jason_teo", "simulation/getevents", map[string]any{"job_id": job, "kinds": "boom"}, false, nil), 400, "Unknown event kind")

		type trend struct {
			ID      int64            `json:"id"`
			Minutes int              `json:"minutes"`
			Data    []map[string]any `json:"data"`
			SPC     map[string]any   `json:"spc"`
			Stats   map[string]any   `json:"stats"`
			At      string           `json:"at"`
		}
		tr := decode[trend](t, e.ok("jason_teo", "simulation/gettrend", map[string]any{"job_id": job, "variant": "recommended",
			"asset_alias": "CT_01", "param_alias": "cwst", "minutes": 60}))
		// Window: the last 60 minutes of the run (61 points); the unit is IDLE
		// after 150 min, so 30 samples are excluded from SPC.
		if len(tr.Data) != 61 || tr.Minutes != 60 || tr.Stats["excluded"].(float64) != 30 || tr.SPC["computed"] != true {
			t.Fatalf("trend points %d stats %v spc %v", len(tr.Data), tr.Stats, tr.SPC)
		}
		if tr.Data[0]["running"] != true || tr.Data[60]["running"] != false {
			t.Fatalf("running flags %v %v", tr.Data[0], tr.Data[60])
		}
		long := decode[trend](t, e.ok("jason_teo", "simulation/gettrend", map[string]any{"job_id": job, "variant": "recommended",
			"asset_alias": "CT_01", "param_alias": "cwst", "minutes": 1440, "at": t0.Add(100 * time.Minute).Format(time.RFC3339)}))
		if len(long.Data) == 0 || len(long.Data) > 361 || long.At != t0.Add(100*time.Minute).Format("2006-01-02T15:04:05.000Z") {
			t.Fatalf("long trend %d points at %s", len(long.Data), long.At)
		}
		e.expect(e.do("jason_teo", "simulation/gettrend", map[string]any{"job_id": job, "variant": "recommended", "asset_alias": "CT_01",
			"param_alias": "fan_speed"}, false, nil), 404, "no series")
		e.expect(e.do("jason_teo", "simulation/gettrend", map[string]any{"job_id": job, "variant": "recommended", "asset_alias": "CT_01",
			"param_alias": "nope"}, false, nil), 404, "err_parameter_is_not_found")

		// Frames are capped (series x steps of the run, estimated before reading).
		e.exec(`UPDATE sim.jobs SET sim_end = sim_start + interval '2000 days' WHERE id = $1`, job)
		e.expect(e.do("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "recommended"}, false, nil), 400,
			"select the units to replay")
		e.expect(e.do("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "recommended", "aliases": "CT_01"}, false, nil),
			400, "select fewer units")
		e.exec(`UPDATE sim.jobs SET sim_end = sim_start + interval '120 minutes' WHERE id = $1`, job)
		if f := decode[frames](t, e.ok("jason_teo", "simulation/getframes", map[string]any{"job_id": job, "variant": "recommended"})); f.Steps != 182 {
			t.Fatalf("frames after the cap check: %d steps", f.Steps)
		}
	})

	t.Run("insights", func(t *testing.T) {
		e.t = t
		e.exec(`INSERT INTO analytics.fault_rules (code, name, lbnl_fault, description, asset_scope, signals, params, defaults) VALUES
			('CT-014', 'Cooling Tower Heat Exchanger Fouling', 'Cooling tower fouling', 'test rule', 'tower', '[]',
			 '{"approach_excess_k": 1.0, "window_min": 120}', '{"approach_excess_k": 1.0, "window_min": 120, "fan_min_pct": 90}')
			ON CONFLICT (code) DO UPDATE SET params = EXCLUDED.params, defaults = EXCLUDED.defaults, enabled = TRUE, updated_by = NULL`)
		e.exec(`INSERT INTO analytics.faults (rule_code, asset_id, severity, message, started_at, last_seen_at, evidence, instruction_md)
			SELECT 'CT-014', id, 'WARNING', 'Approach 1.4 K above expected', now() - interval '2 hours', now(), '{"approach_k": 4.9}', '# Clean the fill'
			FROM asset.equipment WHERE alias = 'CT_01'`)
		e.exec(`INSERT INTO analytics.faults (rule_code, asset_id, severity, message, started_at, last_seen_at, ended_at, status)
			SELECT 'CT-014', id, 'WARNING', 'old', now() - interval '3 days', now() - interval '2 days', now() - interval '2 days', 'CLEARED'
			FROM asset.equipment WHERE alias = 'MAU_01'`)
		e.exec(`INSERT INTO analytics.anomalies (asset_id, equipment_parameter_id, param_alias, detector, severity, value, message, started_at, last_seen_at)
			SELECT e.id, ep.id, mp.alias, 'zscore', 'WARNING', 31.2, 'cwst jump', now() - interval '10 minutes', now()
			FROM asset.equipment e JOIN asset.equipment_parameters ep ON ep.equipment_id = e.id JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			WHERE e.alias = 'CT_01' AND mp.alias = 'cwst'`)
		e.exec(`INSERT INTO analytics.optimizer_runs (trigger, status, created_at, started_at, finished_at) VALUES
			('schedule', 'SUCCEEDED', now() - interval '150 minutes', now() - interval '150 minutes', now() - interval '149 minutes'),
			('schedule', 'SUCCEEDED', now() - interval '60 minutes', now() - interval '60 minutes', now() - interval '59 minutes')`)
		e.exec(`INSERT INTO analytics.llm_usage (component, model, input_tokens, output_tokens, cost_usd) VALUES ('agent', 'gemini-2.5-flash', 1000, 200, 0.0123)`)
		var recID int64
		if err := e.db.QueryRow(e.ctx, `INSERT INTO analytics.recommendations (status, conditions, current, recommended, actions, savings_kw,
				savings_pct, valid_until, rationale) VALUES ('VALIDATED', '{"wet_bulb": 26.1}', '{"setpoints": {"fan_speed_sp": 70}}',
				'{"setpoints": {"fan_speed_sp": 85}}', '[{"asset": "CT_01", "param": "fan_speed_sp", "value": 85, "unit": "%", "label": "Fan speed"},
				{"asset": "CT_01", "param": "fan_mode", "value": 1, "unit": "", "label": "Fan mode"}]', 42.5, 3.1, now() + interval '1 hour', 'test')
			RETURNING id`).Scan(&recID); err != nil {
			t.Fatal(err)
		}

		ov := decode[map[string]any](t, e.ok("jason_teo", "ai_insights/getoverview", nil))
		faults := ov["faults"].(map[string]any)
		if faults["active"].(float64) < 1 || ov["recommendation"] == nil || ov["optimizer"].(map[string]any)["next_run_at"] == nil ||
			ov["budget"].(map[string]any)["today_usd"].(float64) < 0.0123 || ov["budget"].(map[string]any)["limit_usd"].(float64) <= 0 {
			t.Fatalf("overview %v", ov)
		}
		if per := ov["optimizer"].(map[string]any); per["status"] != "SUCCEEDED" {
			t.Fatalf("optimizer %v", per)
		}
		fab := decode[map[string]any](t, e.ok("hafiz_rahman", "ai_insights/getoverview", nil))
		if fab["recommendation"] != nil || fab["faults"].(map[string]any)["active"].(float64) != 0 {
			t.Fatalf("FAB1 overview leaks CUB data %v", fab)
		}

		fl := decode[[]map[string]any](t, e.ok("jason_teo", "ai_insights/getfaults", map[string]any{"status": "ACTIVE", "rule_codes": "ct-014"}))
		if len(fl) != 1 || fl[0]["asset_alias"] != "CT_01" || fl[0]["rule_name"] == nil || fl[0]["instruction_md"] != nil {
			t.Fatalf("faults %v", fl)
		}
		ff := decode[[]map[string]any](t, e.ok("hafiz_rahman", "ai_insights/getfaults", nil))
		if len(ff) != 1 || ff[0]["asset_alias"] != "MAU_01" {
			t.Fatalf("FAB1 faults %v", ff)
		}
		recent := decode[[]map[string]any](t, e.ok("jason_teo", "ai_insights/getfaults", map[string]any{"from": time.Now().Add(-24 * time.Hour).Format(time.RFC3339)}))
		for _, f := range recent {
			if f["asset_alias"] == "MAU_01" {
				t.Fatal("from filter must drop faults that ended before it")
			}
		}
		one := decode[map[string]any](t, e.ok("jason_teo", "ai_insights/getfault", map[string]any{"id": fl[0]["id"]}))
		if one["instruction_md"] != "# Clean the fill" || one["evidence"].(map[string]any)["approach_k"].(float64) != 4.9 {
			t.Fatalf("fault detail %v", one)
		}
		e.expect(e.do("hafiz_rahman", "ai_insights/getfault", map[string]any{"id": fl[0]["id"]}, false, nil), 404, "not found")

		ct1 := e.scalar(`SELECT id FROM asset.equipment WHERE alias = 'CT_01'`).(int64)
		an := decode[[]map[string]any](t, e.ok("jason_teo", "ai_insights/getanomalies", map[string]any{"detectors": "zscore", "asset_ids": []any{ct1}}))
		if len(an) == 0 || an[0]["param_name"] == nil || an[0]["asset_alias"] != "CT_01" {
			t.Fatalf("anomalies %v", an)
		}
		e.expect(e.do("jason_teo", "ai_insights/getanomalies", map[string]any{"detectors": "magic"}, false, nil), 400, "Unknown detector")

		rules := decode[[]map[string]any](t, e.ok("jason_teo", "ai_insights/getrules", nil))
		if len(rules) == 0 {
			t.Fatal("no rules")
		}
		e.expect(e.do("jason_teo", "ai_insights/updaterule", map[string]any{"code": "CT-014", "enabled": false}, false, nil), 403, "permission")
		e.expect(e.do("superuser", "ai_insights/updaterule", map[string]any{"code": "CT-014", "enabled": false}, false, agent), 403, "needs a person")
		e.expect(e.do("superuser", "ai_insights/updaterule", map[string]any{"code": "CT-014", "params": map[string]any{"bogus": 1}}, false, nil),
			400, "Unknown rule parameter")
		before := e.scalar(`SELECT version FROM analytics.fault_rules WHERE code = 'CT-014'`).(int32)
		upd := e.do("superuser", "ai_insights/updaterule", map[string]any{"code": "ct-014", "enabled": "false",
			"params": map[string]any{"approach_excess_k": "1.2"}}, true, nil)
		if upd.Status != 200 {
			t.Fatalf("update rule %s", upd.Raw)
		}
		rule := decode[map[string]any](t, upd.Data)
		params := rule["params"].(map[string]any)
		if rule["enabled"] != false || int32(rule["version"].(float64)) != before+1 || params["approach_excess_k"].(float64) != 1.2 ||
			params["window_min"].(float64) != 120 || rule["updated_by_name"] == nil {
			t.Fatalf("updated rule %v", rule)
		}
		pm := decode[[]map[string]any](t, e.ok("hafiz_rahman", "ai_insights/getpointmap", nil))
		if len(pm) < 30 {
			t.Fatalf("point map %d rows", len(pm))
		}

		// Recommendations.
		recs := decode[[]map[string]any](t, e.ok("jason_teo", "ai_insights/getrecommendations", map[string]any{"status": "VALIDATED"}))
		if len(recs) == 0 || recs[0]["id"].(float64) != float64(recID) {
			t.Fatalf("recommendations %v", recs)
		}
		if fr := decode[[]map[string]any](t, e.ok("hafiz_rahman", "ai_insights/getrecommendations", nil)); len(fr) != 0 {
			t.Fatalf("FAB1 user sees a CUB recommendation %v", fr)
		}
		e.expect(e.do("jason_teo", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, nil), 403, "permission") // no aiinsights.update
		e.expect(e.do("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, agent), 403, "needs a person")
		applied := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}))
		results := applied["results"].([]any)
		first := results[0].(map[string]any)
		// Mode first; the plant is not reachable here, so the write is refused
		// (recorded as a REJECTED control request) and nothing else is sent.
		if len(results) != 1 || first["alias"] != "fan_mode" || first["status"] != "REJECTED" || first["id"] == nil ||
			!strings.Contains(first["reason"].(string), "not connected") || first["asset_alias"] != "CT_01" {
			t.Fatalf("apply results %v", results)
		}
		rec := applied["recommendation"].(map[string]any)
		if rec["status"] != "VALIDATED" || rec["apply_results"].(map[string]any)["status"] != "FAILED" {
			t.Fatalf("recommendation after failed apply %v", rec)
		}
		if rs, ok := applied["restores"].([]any); !ok || len(rs) != 0 {
			t.Fatalf("nothing was changed, nothing to restore %v", applied["restores"])
		}
		// One decision at a time: no dismiss during an apply, no apply during a dismiss.
		recApplying.Store(recID, "apply")
		e.expect(e.do("sarah_lim", "ai_insights/dismissrecommendation", map[string]any{"id": recID}, false, nil), 409, "being applied")
		recApplying.Store(recID, "dismiss")
		e.expect(e.do("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, nil), 409, "being dismissed")
		recApplying.Delete(recID)
		if n := e.scalar(`SELECT count(*) FROM asset.control_requests WHERE id = $1 AND status = 'REJECTED'`, int64(first["id"].(float64))).(int64); n != 1 {
			t.Fatal("the write path must record the control request")
		}
		e.expect(e.do("sarah_lim", "ai_insights/dismissrecommendation", map[string]any{"id": recID}, false, agent), 403, "needs a person")
		dis := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/dismissrecommendation", map[string]any{"id": recID, "note": "Wet bulb rising"}))
		if dis["status"] != "DISMISSED" || dis["decision_note"] != "Wet bulb rising" || dis["decided_by_name"] == nil {
			t.Fatalf("dismissed %v", dis)
		}
		e.expect(e.do("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, nil), 409, "Only a validated")
		e.exec(`UPDATE analytics.recommendations SET status = 'VALIDATED', valid_until = now() - interval '1 minute' WHERE id = $1`, recID)
		e.expect(e.do("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, nil), 409, "expired")
		if st := e.scalar(`SELECT status FROM analytics.recommendations WHERE id = $1`, recID); st != "EXPIRED" {
			t.Fatalf("expired recommendation status %v", st)
		}
		// Dismissed while the writes run (here: by a trigger on the first
		// control request): not marked applied, the writes are recorded.
		e.exec(`UPDATE analytics.recommendations SET status = 'VALIDATED', valid_until = now() + interval '1 hour', rationale = 'race test',
			apply_results = NULL WHERE id = $1`, recID)
		e.exec(`CREATE FUNCTION public.apitest_dismiss_rec() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			UPDATE analytics.recommendations SET status = 'DISMISSED' WHERE rationale = 'race test' AND status = 'VALIDATED'; RETURN NULL; END $$`)
		defer e.exec(`DROP FUNCTION IF EXISTS public.apitest_dismiss_rec() CASCADE`)
		e.exec(`CREATE TRIGGER apitest_dismiss_rec AFTER INSERT ON asset.control_requests FOR EACH ROW EXECUTE FUNCTION public.apitest_dismiss_rec()`)
		e.expect(e.do("sarah_lim", "ai_insights/applyrecommendation", map[string]any{"id": recID}, false, nil), 409,
			"became DISMISSED while it was being applied")
		e.exec(`DROP FUNCTION public.apitest_dismiss_rec() CASCADE`)
		var raced map[string]any
		if err := e.db.QueryRow(e.ctx, `SELECT apply_results FROM analytics.recommendations WHERE id = $1 AND status = 'DISMISSED'`, recID).
			Scan(&raced); err != nil || raced["status"] != "FAILED" || len(raced["results"].([]any)) != 1 {
			t.Fatalf("apply record of the raced recommendation %v %v", raced, err)
		}

		drain()
		run1 := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/runoptimizer", nil))
		run2 := decode[map[string]any](t, e.do("sarah_lim", "ai_insights/runoptimizer", nil, false, agent).Data)
		if run1["existing"] != false || run2["existing"] != true || run1["run_id"] != run2["run_id"] {
			t.Fatalf("optimizer runs %v %v", run1, run2)
		}
		if n := drain()["optimizer_runs"]; len(n) != 1 {
			t.Fatalf("optimizer_runs notifications %v", n)
		}
		e.exec(`UPDATE analytics.optimizer_runs SET status = 'SUCCEEDED' WHERE status = 'PENDING'`)
		run3 := decode[map[string]any](t, e.do("sarah_lim", "ai_insights/runoptimizer", nil, false, agent).Data)
		if tr := e.scalar(`SELECT trigger FROM analytics.optimizer_runs WHERE id = $1`, int64(run3["run_id"].(float64))); tr != "agent" {
			t.Fatalf("agent-requested run trigger %v", tr)
		}
		// A run left RUNNING (or PENDING) for over 20 minutes no longer blocks a new one.
		runOf := func(r map[string]any) int64 { return int64(r["run_id"].(float64)) }
		e.exec(`UPDATE analytics.optimizer_runs SET status = 'RUNNING', started_at = now() - interval '5 minutes' WHERE id = $1`, runOf(run3))
		if r := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/runoptimizer", nil)); r["existing"] != true || runOf(r) != runOf(run3) {
			t.Fatalf("a live running run must be returned %v", r)
		}
		e.exec(`UPDATE analytics.optimizer_runs SET started_at = now() - interval '25 minutes' WHERE id = $1`, runOf(run3))
		run4 := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/runoptimizer", nil))
		if run4["existing"] != false || runOf(run4) == runOf(run3) {
			t.Fatalf("a stale running run blocks a new one %v", run4)
		}
		if st := e.scalar(`SELECT status || ' ' || error FROM analytics.optimizer_runs WHERE id = $1`, runOf(run3)); st != "FAILED interrupted (stale)" {
			t.Fatalf("stale run %v", st)
		}
		e.exec(`UPDATE analytics.optimizer_runs SET created_at = now() - interval '21 minutes' WHERE id = $1`, runOf(run4))
		if run5 := decode[map[string]any](t, e.ok("sarah_lim", "ai_insights/runoptimizer", nil)); run5["existing"] != false || runOf(run5) == runOf(run4) {
			t.Fatalf("a stale pending run blocks a new one %v", run5)
		}
	})

	t.Run("documents", func(t *testing.T) {
		e.t = t
		pdf := []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")
		upload := func(user string, body map[string]any, headers map[string]string) apiResp {
			return e.do(user, "ai_documents/upload", body, true, headers)
		}
		e.expect(upload("jason_teo", map[string]any{"file": formFile{"m.pdf", "application/pdf", pdf}, "title": "x", "doc_type": "manual"}, nil),
			403, "permission")
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"m.pdf", "application/pdf", pdf}, "doc_type": "manual"}, agent), 403, "needs a person")
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"m.pdf", "application/pdf", []byte("hello")}, "doc_type": "manual"}, nil),
			400, "not a PDF")
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"m.pdf", "application/pdf", pdf}, "doc_type": "novel"}, nil), 400, "Unknown document type")
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"m.pdf", "application/pdf", pdf}, "unit": "CH_99"}, nil), 400, "Unknown equipment")
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"m.pdf", "application/pdf", pdf}, "unit": "CH_01", "asset_type": "Cooling Tower"}, nil),
			400, "not a Cooling Tower")
		drain()
		r := upload("sarah_lim", map[string]any{"file": formFile{`C:\docs\CH-01 overhaul.pdf`, "application/pdf", pdf}, "title": "CH-01 overhaul report",
			"doc_type": "maintenance_report", "unit": "ch_01", "doc_date": "2026-09-30", "alarm_codes": "CHL-005"}, nil)
		if r.Status != 200 {
			t.Fatalf("upload %s", r.Raw)
		}
		doc := decode[map[string]any](t, r.Data)
		key := doc["object_key"].(string)
		if doc["status"] != "PENDING" || doc["unit"] != "CH_01" || doc["asset_type"] != "Centrifugal Chiller" || doc["site_code"] != "SGF1" ||
			doc["doc_date"] != "2026-09-30" || doc["file_name"] != "CH-01 overhaul.pdf" || !strings.HasPrefix(key, fmt.Sprintf("rag/%d/", time.Now().UTC().Year())) ||
			!strings.HasSuffix(key, ".pdf") || len(doc["sha256"].(string)) != 64 || doc["asset_alias"] != "CH_01" {
			t.Fatalf("document %v", doc)
		}
		id := int64(doc["id"].(float64))
		if n := drain()["rag_documents"]; len(n) != 1 || n[0] != fmt.Sprint(id) {
			t.Fatalf("rag_documents notifications %v", n)
		}
		e.expect(upload("sarah_lim", map[string]any{"file": formFile{"again.pdf", "application/pdf", pdf}, "doc_type": "other"}, nil), 409, "already in the library")

		type docs struct {
			Total     int              `json:"total"`
			Documents []map[string]any `json:"documents"`
		}
		l := decode[docs](t, e.ok("jason_teo", "ai_documents/list", map[string]any{"doc_type": "maintenance_report", "search": "overhaul"}))
		if l.Total != 1 || l.Documents[0]["file_url"] != fmt.Sprintf("/api/ai/documents/%d/file", id) {
			t.Fatalf("list %+v", l)
		}
		if fl := decode[docs](t, e.ok("hafiz_rahman", "ai_documents/list", map[string]any{"search": "overhaul"})); fl.Total != 0 {
			t.Fatalf("FAB1 user sees a CUB document %+v", fl)
		}
		e.expect(e.do("hafiz_rahman", "ai_documents/get", map[string]any{"id": id}, false, nil), 404, "not found")

		get := func(user string, hdr map[string]string) *http.Response {
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/ai/documents/%d/file", e.http.URL, id), nil)
			if user != "" {
				req.Header.Set(auth.SessionHeader, e.tokens[user])
			}
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		resp := get("jason_teo", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/pdf" || !bytes.Equal(body, pdf) ||
			resp.Header.Get("Content-Disposition") != `inline; filename="CH-01 overhaul.pdf"` || resp.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("file %d %v", resp.StatusCode, resp.Header)
		}
		resp = get("ahmad_faizal", map[string]string{"Range": "bytes=0-4"}) // aiassistant.read only, CUB scope
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent || string(body) != "%PDF-" {
			t.Fatalf("range %d %q", resp.StatusCode, body)
		}
		for user, want := range map[string]int{"": 401, "hafiz_rahman": 404} {
			resp := get(user, nil)
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("file as %q: %d, want %d", user, resp.StatusCode, want)
			}
		}
		e.exec(`DELETE FROM core.user_group_permissions WHERE user_group_id = 2`) // CUB technicians lose aiassistant.read
		e.srv.Auth.InvalidateAll()
		if resp := get("ahmad_faizal", nil); resp.StatusCode != 403 {
			t.Fatalf("file without permission: %d", resp.StatusCode)
		}

		re := decode[map[string]any](t, e.ok("sarah_lim", "ai_documents/reindex", map[string]any{"id": id}))
		if re["status"] != "PENDING" {
			t.Fatalf("reindex %v", re)
		}
		e.exec(`UPDATE rag.documents SET status = 'INDEXING' WHERE id = $1`, id)
		e.expect(e.do("sarah_lim", "ai_documents/reindex", map[string]any{"id": id}, false, nil), 409, "being indexed")

		e.expect(e.do("sarah_lim", "ai_documents/delete", map[string]any{"id": id}, false, agent), 403, "needs a person")
		del := decode[map[string]any](t, e.ok("sarah_lim", "ai_documents/delete", map[string]any{"id": id}))
		if del["deleted"] != true {
			t.Fatalf("delete %v", del)
		}
		if _, err := e.srv.DocsStore.Open(e.ctx, key); err == nil {
			t.Fatal("the PDF must be removed from the bucket")
		}
	})

	t.Run("live faults and writes", func(t *testing.T) {
		e.t = t
		var ct1 int64
		if err := e.db.QueryRow(e.ctx, `SELECT id FROM asset.equipment WHERE alias = 'CT_01'`).Scan(&ct1); err != nil {
			t.Fatal(err)
		}
		e.expect(e.do("jason_teo", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "ct_fouling", "value": 0.8}, false, nil),
			403, "permission")
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "ct_fouling", "value": 0.8}, false, agent),
			403, "needs a person")
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "melt"}, false, nil), 400, "Unknown fault")
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "chwst_bias", "value": 1}, false, nil),
			400, "does not apply to CT_01")
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "ct_fouling", "value": 0.5}, false, nil),
			400, "not a documented intensity")
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "ct_fouling", "value": 0.8, "action": "stop"}, false, nil),
			400, "inject or clear")
		// Valid request: refused here because the plant's data source is not connected.
		e.expect(e.do("superuser", "digital_twin/injectfault", map[string]any{"asset_id": ct1, "fault": "ct_fouling", "value": 0.8}, false, nil),
			400, "not connected")

		e.exec(`UPDATE core.settings SET value = '{"saved_at": "2026-10-10T06:00:00Z", "assets": {"CT_01": {"faults": [{"kind": "ct_fouling",
			"sev": 1, "rate": 0, "value": 0.8, "start": "2026-10-10T05:00:00Z"}]}, "MAU_01": {"faults": [{"kind": "coil_fouling", "sev": 0.42,
			"rate": 0.001, "start": "2026-10-09T05:00:00Z"}]}}}' WHERE key = 'demo_simulator_state'`)
		active := decode[[]map[string]any](t, e.ok("jason_teo", "digital_twin/getactivefaults", nil))
		if len(active) != 2 || active[0]["alias"] != "CT_01" || active[0]["value"].(float64) != 0.8 || active[0]["code"] != "CT-014" ||
			active[1]["value"] != nil || active[1]["code"] != nil {
			t.Fatalf("active faults %v", active)
		}
		if fab := decode[[]map[string]any](t, e.ok("hafiz_rahman", "digital_twin/getactivefaults", nil)); len(fab) != 1 || fab[0]["alias"] != "MAU_01" {
			t.Fatalf("FAB1 active faults %v", fab)
		}

		var epid int64
		if err := e.db.QueryRow(e.ctx, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			WHERE ep.equipment_id = $1 AND mp.alias = 'fan_mode'`, ct1).Scan(&epid); err != nil {
			t.Fatal(err)
		}
		e.expect(e.do("jason_teo", "digital_twin/writeparameter", map[string]any{"parameter_id": epid, "value": 1}, false, agent), 403, "needs a person")
		w := decode[map[string]any](t, e.ok("jason_teo", "digital_twin/writeparameter", map[string]any{"parameter_id": epid, "value": 1}))
		if w["status"] != "REJECTED" || !strings.Contains(w["reason"].(string), "not connected") {
			t.Fatalf("write %v", w)
		}
	})

	t.Run("routes", func(t *testing.T) {
		e.t = t
		want := []string{"simulation/listjobs", "simulation/getjob", "simulation/submit", "simulation/propose", "simulation/approve",
			"simulation/reject", "simulation/cancel", "simulation/getevents", "simulation/getframes", "simulation/gettrend",
			"simulation/getfaultcatalog", "ai_insights/getoverview", "ai_insights/getrecommendations", "ai_insights/getrecommendation",
			"ai_insights/applyrecommendation", "ai_insights/dismissrecommendation", "ai_insights/runoptimizer", "ai_insights/getfaults",
			"ai_insights/getfault", "ai_insights/getanomalies", "ai_insights/getrules", "ai_insights/updaterule", "ai_insights/getpointmap",
			"ai_documents/list", "ai_documents/get", "ai_documents/upload", "ai_documents/delete", "ai_documents/reindex",
			"digital_twin/injectfault", "digital_twin/getactivefaults"}
		have := e.srv.Routes()
		sort.Strings(have)
		for _, r := range want {
			i := sort.SearchStrings(have, r)
			if i >= len(have) || have[i] != r {
				t.Errorf("route %s is not registered", r)
			}
		}
		// The agent proxy answers 503 (no AGENT_URL here) after authenticating.
		req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/api/agent/chat", strings.NewReader("{}"))
		req.Header.Set(auth.SessionHeader, e.tokens["jason_teo"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatalf("agent proxy without AGENT_URL: %d", resp.StatusCode)
		}
		resp, _ = http.Post(e.http.URL+"/api/agent/chat", "application/json", strings.NewReader("{}"))
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("agent proxy without session: %d", resp.StatusCode)
		}
	})
}
