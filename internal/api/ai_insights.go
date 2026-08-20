package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

// AI insights (DESIGN §4.3): set-point recommendations of the optimizer
// (validated on the digital twin), LBNL faults detected by the FDD rules with
// their RAG maintenance instructions, sensor anomalies, the rule
// configuration and the LBNL point map. ai-analytics writes the analytics.*
// rows; the API reads them, scoped to the user's locations, and records the
// human decisions (apply / dismiss a recommendation, rule changes).

const (
	recDefaultBudgetUSD = 0.50 // AI_DAILY_BUDGET_USD default of the AI services
	optimizerMinPeriod  = 60   // minutes (OPTIMIZER_INTERVAL_MIN 60..120)
	optimizerMaxPeriod  = 120
	// optimizerStaleAfter is the age of a PENDING (created) or RUNNING
	// (started) optimizer run past which it is taken as interrupted: the
	// analytics worker gives a run at most 15 minutes.
	optimizerStaleAfter = "20 minutes"
)

var (
	recStatuses      = []string{"PROPOSED", "VALIDATING", "VALIDATED", "REJECTED", "APPLIED", "EXPIRED", "DISMISSED", "FAILED"}
	faultStatuses    = []string{"ACTIVE", "CLEARED"}
	anomalyDetectors = []string{"range", "flatline", "zscore", "ewma", "spc"}
	// recApplying holds the recommendations being applied or dismissed (id ->
	// "apply" | "dismiss"): one decision at a time.
	recApplying sync.Map
)

// recBusy is the conflict of a decision on a recommendation another one is
// being made on.
func recBusy(op any) error {
	if op == "dismiss" {
		return httpx.Conflict("This recommendation is being dismissed")
	}
	return httpx.Conflict("This recommendation is being applied, wait for the result")
}

func (s *Server) registerAIInsights() {
	s.handle("ai_insights/getoverview", AccessUser, s.aiOverview)
	s.handle("ai_insights/getrecommendations", AccessUser, s.aiRecommendations)
	s.handle("ai_insights/getrecommendation", AccessUser, s.aiRecommendation)
	s.handle("ai_insights/applyrecommendation", AccessUser, s.aiApplyRecommendation)
	s.handle("ai_insights/dismissrecommendation", AccessUser, s.aiDismissRecommendation)
	s.handle("ai_insights/runoptimizer", AccessUser, s.aiRunOptimizer)
	s.handle("ai_insights/getfaults", AccessUser, s.aiFaults)
	s.handle("ai_insights/getfault", AccessUser, s.aiFault)
	s.handle("ai_insights/getanomalies", AccessUser, s.aiAnomalies)
	s.handle("ai_insights/getrules", AccessUser, s.aiRules)
	s.handle("ai_insights/updaterule", AccessAdmin, s.aiUpdateRule)
	s.handle("ai_insights/getpointmap", AccessUser, s.aiPointMap)
}

// assetScopeClause restricts rows joined to asset.equipment (alias e) to the
// user's locations; "" for unrestricted users.
func assetScopeClause(c *Ctx, args *sqlArgs) (string, error) {
	scope, err := c.Scope()
	if err != nil {
		return "", err
	}
	if scope == nil || scope.Unrestricted {
		return "", nil
	}
	return "e.location_id = ANY(" + args.add(emptyIfNil(scope.LocationIDs())) + ")", nil
}

// ---------------------------------------------------------------------------
// Recommendations
// ---------------------------------------------------------------------------

const recColumns = `r.id, r.run_id, r.site_code, r.status, r.conditions, r.current, r.recommended, r.actions, r.savings_kw, r.savings_pct,
	r.constraints, r.validation_job_id, r.validation, r.model_id, r.rationale, r.created_at, r.valid_until, r.decided_by, r.decided_at,
	r.decision_note, r.apply_results, u.full_name, j.id, j.status`

const recFrom = ` FROM analytics.recommendations r LEFT JOIN core.users u ON u.id = r.decided_by LEFT JOIN sim.jobs j ON j.id = r.validation_job_id`

func scanRec(row pgx.Row) (map[string]any, error) {
	var id int64
	var site, status string
	var runID, validationJobID, modelID, decidedBy, jobID *int64
	var conditions, current, recommended, actions, constraints, validation, applyResults json.RawMessage
	var savingsKW, savingsPct *float64
	var rationale, note, decidedByName, jobStatus *string
	var created, validUntil time.Time
	var decidedAt *time.Time
	if err := row.Scan(&id, &runID, &site, &status, &conditions, &current, &recommended, &actions, &savingsKW, &savingsPct,
		&constraints, &validationJobID, &validation, &modelID, &rationale, &created, &validUntil, &decidedBy, &decidedAt,
		&note, &applyResults, &decidedByName, &jobID, &jobStatus); err != nil {
		return nil, err
	}
	var job any
	if jobID != nil {
		job = map[string]any{"id": *jobID, "status": jobStatus}
	}
	return map[string]any{
		"id": id, "run_id": runID, "site_code": site, "status": status, "conditions": conditions, "current": current,
		"recommended": recommended, "actions": actions, "savings_kw": finiteOrNil(savingsKW), "savings_pct": finiteOrNil(savingsPct),
		"constraints": constraints, "validation_job_id": validationJobID, "validation": validation, "model_id": modelID,
		"rationale": rationale, "created_at": httpx.TS(created), "valid_until": httpx.TS(validUntil), "decided_by": decidedBy,
		"decided_by_name": decidedByName, "decided_at": httpx.TSPtr(decidedAt), "decision_note": note, "apply_results": applyResults,
		"validation_job": job,
	}, nil
}

func finiteOrNil(v *float64) any {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return *v
}

// recScopeClause hides recommendations whose actions write equipment outside
// the user's scope; "" for unrestricted users.
func (s *Server) recScopeClause(c *Ctx, args *sqlArgs) (string, error) {
	eq, err := s.equipmentRefs(c)
	if err != nil {
		return "", err
	}
	keys, err := s.visibleAliasKeys(c, eq)
	if err != nil || keys == nil {
		return "", err
	}
	return `NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(r.actions) = 'array' THEN r.actions ELSE '[]'::jsonb END) a
		WHERE a->>'asset' IS NOT NULL AND NOT (lower(a->>'asset') = ANY(` + args.add(keys) + `)))`, nil
}

// recQuery returns recommendations matching the extra conditions, newest first.
func (s *Server) recQuery(c *Ctx, args sqlArgs, conds []string, limit int64) ([]map[string]any, error) {
	vis, err := s.recScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if vis != "" {
		conds = append(conds, vis)
	}
	if len(conds) == 0 {
		conds = []string{"TRUE"}
	}
	// (the query text is built before args is passed: the evaluation order of
	// args.add and args... within one call is unspecified)
	q := `SELECT ` + recColumns + recFrom + ` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY r.created_at DESC, r.id DESC LIMIT ` + args.add(limit)
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		r, err := scanRec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) recLoad(c *Ctx, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, httpx.BadRequest("A recommendation id is required")
	}
	var args sqlArgs
	cond := "r.id = " + args.add(id)
	list, err := s.recQuery(c, args, []string{cond}, 1)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, httpx.NotFound("Recommendation not found")
	}
	return list[0], nil
}

// aiRecommendations lists recommendations. Input: status? (CSV), limit (1..50, 10).
func (s *Server) aiRecommendations(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	var args sqlArgs
	var conds []string
	if list, ok := c.F.StringList("status"); ok && len(list) > 0 {
		up := make([]string, len(list))
		for i, st := range list {
			up[i] = strings.ToUpper(strings.TrimSpace(st))
			if !contains(recStatuses, up[i]) {
				return nil, httpx.BadRequestf("Unknown recommendation status %q", st)
			}
		}
		conds = append(conds, "r.status = ANY("+args.add(up)+")")
	}
	return s.recQuery(c, args, conds, c.limit(10, 50))
}

// aiRecommendation returns one recommendation. Input: id.
func (s *Server) aiRecommendation(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	return s.recLoad(c, c.F.Int("id", 0))
}

// recAction is one write of a recommendation (analytics.recommendations.actions).
type recAction struct {
	Asset string   `json:"asset"`
	Param string   `json:"param"`
	Value *float64 `json:"value"`
	Unit  string   `json:"unit"`
	Label string   `json:"label"`
}

// orderRecActions puts control-mode writes before set points (a set point is
// refused while its mode is in automatic), keeping the order within each group.
func orderRecActions(actions []recAction, isMode func(recAction) bool) []recAction {
	out := append([]recAction(nil), actions...)
	sort.SliceStable(out, func(i, j int) bool { return isMode(out[i]) && !isMode(out[j]) })
	return out
}

// aiApplyRecommendation writes a validated recommendation to the plant through
// the digital twin write path (one control request per action, modes first),
// a person only. Input: id -> {recommendation, results, restores}. The
// recommendation becomes APPLIED when every write is confirmed; otherwise it
// stays VALIDATED, the results show which write failed (the remaining ones are
// not sent) and the control modes the apply had changed are written back
// (restores). A recommendation dismissed or expired during the apply is not
// marked applied (409; the writes are recorded in its apply_results).
func (s *Server) aiApplyRecommendation(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	if err := c.Require("digitaltwin", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	if op, busy := recApplying.LoadOrStore(id, "apply"); busy {
		return nil, recBusy(op)
	}
	defer recApplying.Delete(id)

	rec, err := s.recLoad(c, id)
	if err != nil {
		return nil, err
	}
	if st := rec["status"].(string); st != "VALIDATED" {
		return nil, httpx.Conflict(fmt.Sprintf("Only a validated recommendation can be applied (this one is %s)", st))
	}
	var validUntil time.Time
	var raw json.RawMessage
	if err := s.DB.QueryRow(c, `SELECT valid_until, actions FROM analytics.recommendations WHERE id = $1`, id).Scan(&validUntil, &raw); err != nil {
		return nil, err
	}
	if time.Now().After(validUntil) {
		_, _ = s.DB.Exec(c, `UPDATE analytics.recommendations SET status = 'EXPIRED' WHERE id = $1 AND status = 'VALIDATED'`, id)
		return nil, httpx.Conflict("The recommendation has expired, wait for the next optimizer run")
	}
	var actions []recAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, fmt.Errorf("recommendation %d actions: %w", id, err)
	}
	if len(actions) == 0 {
		return nil, httpx.Conflict("The recommendation has no set points to apply")
	}

	// The operator's decision is carried out even if the browser goes away.
	cc := *c
	cc.Context = context.WithoutCancel(c)
	ac := &cc

	modes := map[string]bool{}
	for _, a := range actions {
		var assetID int64
		if err := s.DB.QueryRow(ac, `SELECT id FROM asset.equipment WHERE lower(alias) = lower($1)`, a.Asset).Scan(&assetID); err != nil {
			if db.IsNoRows(err) {
				continue // reported as a failed write below
			}
			return nil, err
		}
		controls, err := s.twinControls(ac, assetID)
		if err != nil {
			return nil, err
		}
		for alias, ctl := range controls {
			if len(ctl.Options) > 0 {
				modes[strings.ToLower(a.Asset)+"/"+alias] = true
			}
		}
	}
	isMode := func(a recAction) bool { return modes[strings.ToLower(a.Asset)+"/"+a.Param] }
	ordered := orderRecActions(actions, isMode)

	results, restores, allApplied := applyRecWrites(ordered, isMode, func(a recAction) map[string]any {
		row, reason := s.applyRecAction(ac, a)
		if row == nil {
			row = recFailure(ac, a, reason)
		}
		row["asset_alias"], row["label"] = a.Asset, a.Label
		return row
	})
	outcome := "APPLIED"
	if !allApplied {
		outcome = "FAILED"
		if len(results) > 1 {
			outcome = "PARTIAL"
		}
	}
	record, _ := json.Marshal(map[string]any{"status": outcome, "by": c.User.Username, "by_name": c.User.FullName,
		"at": httpx.TS(time.Now()), "results": results, "planned": len(ordered), "restores": restores})
	// Only a recommendation still VALIDATED is decided here: one dismissed or
	// expired meanwhile keeps its status, and the writes made are recorded.
	tag, err := s.DB.Exec(ac, `UPDATE analytics.recommendations SET apply_results = $2,
			status = CASE WHEN $3 THEN 'APPLIED' ELSE status END,
			decided_by = CASE WHEN $3 THEN $4 ELSE decided_by END,
			decided_at = CASE WHEN $3 THEN now() ELSE decided_at END
		WHERE id = $1 AND status = 'VALIDATED'`, id, record, allApplied, c.User.ID)
	if err != nil {
		return nil, err
	}
	slog.Info("recommendation applied", "id", id, "outcome", outcome, "writes", len(results), "restores", len(restores),
		"user", c.User.Username, "conflict", tag.RowsAffected() == 0)
	if tag.RowsAffected() == 0 {
		var status string
		if err := s.DB.QueryRow(ac, `UPDATE analytics.recommendations SET apply_results = $2 WHERE id = $1 RETURNING status`,
			id, record).Scan(&status); err != nil {
			return nil, err
		}
		return nil, httpx.Conflict(fmt.Sprintf("The recommendation became %s while it was being applied, so it is not marked applied. "+
			"%d of %d writes were confirmed by the plant, see its apply results", status, recConfirmed(results), len(ordered)))
	}
	rec, err = s.recLoad(ac, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"recommendation": rec, "results": results, "restores": restores}, nil
}

// applyRecWrites sends the writes of a recommendation in order and stops at
// the first one the plant does not confirm. Then it writes back, newest
// first, the original value of every control mode the apply changed, so a
// partial apply does not leave equipment in manual. results are the writes
// sent (planned order), restores the write-backs (flag "restore"; the mode's
// result gets "restored" = whether its write-back was confirmed); ok is true
// when every write was confirmed. write returns a control request row.
func applyRecWrites(ordered []recAction, isMode func(recAction) bool, write func(recAction) map[string]any) (results, restores []map[string]any, ok bool) {
	results, restores = []map[string]any{}, []map[string]any{}
	type change struct {
		a   recAction
		row map[string]any
		old float64
	}
	var changed []change
	for _, a := range ordered {
		row := write(a)
		results = append(results, row)
		if row["status"] != "APPLIED" {
			for i := len(changed) - 1; i >= 0; i-- {
				m := changed[i]
				name, _ := m.row["name"].(string)
				if name == "" {
					name = m.a.Param
				}
				r := write(recAction{Asset: m.a.Asset, Param: m.a.Param, Value: &m.old, Unit: m.a.Unit, Label: "Restore " + name})
				r["restore"] = true
				m.row["restored"] = r["status"] == "APPLIED"
				restores = append(restores, r)
			}
			return results, restores, false
		}
		if !isMode(a) {
			continue
		}
		old, _ := row["old_value"].(*float64)
		value, _ := row["value"].(float64)
		switch {
		case old == nil:
			slog.Warn("recommendation apply: mode written without a known previous value", "asset", a.Asset, "param", a.Param)
		case *old != value:
			changed = append(changed, change{a: a, row: row, old: *old})
		}
	}
	return results, restores, true
}

// recConfirmed counts the writes the plant confirmed.
func recConfirmed(results []map[string]any) int {
	n := 0
	for _, r := range results {
		if r["status"] == "APPLIED" {
			n++
		}
	}
	return n
}

// applyRecAction writes one action; a nil row comes with the reason it could
// not be requested (the write path records validation failures itself).
func (s *Server) applyRecAction(c *Ctx, a recAction) (map[string]any, string) {
	if a.Value == nil || math.IsNaN(*a.Value) || math.IsInf(*a.Value, 0) {
		return nil, "The recommendation has no value for this set point"
	}
	p, err := s.epByAlias(c, a.Asset, a.Param)
	if err != nil {
		return nil, errorReason(err)
	}
	row, err := s.twinWrite(c, p.EPID, *a.Value)
	if err != nil {
		return nil, errorReason(err)
	}
	return row, ""
}

func errorReason(err error) string {
	var he *httpx.Error
	if errors.As(err, &he) && he.Status < 500 {
		return he.Msg
	}
	slog.Error("apply recommendation write", "err", err)
	return "The write could not be sent to the plant"
}

// recFailure is a control-request-shaped result for a write that could not
// be requested.
func recFailure(c *Ctx, a recAction, reason string) map[string]any {
	now := httpx.TS(time.Now())
	name := a.Label
	if name == "" {
		name = a.Param
	}
	return map[string]any{"id": nil, "asset_id": nil, "parameter_id": nil, "alias": a.Param, "name": name, "unit": a.Unit, "scale": nil,
		"old_value": nil, "value": a.Value, "status": "REJECTED", "reason": reason, "requested_at": now, "completed_at": now,
		"username": c.User.Username, "user_name": c.User.FullName}
}

// aiDismissRecommendation dismisses a validated recommendation (a person
// only). Input: id, note?.
func (s *Server) aiDismissRecommendation(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	if op, busy := recApplying.LoadOrStore(id, "dismiss"); busy {
		return nil, recBusy(op)
	}
	defer recApplying.Delete(id)
	rec, err := s.recLoad(c, id)
	if err != nil {
		return nil, err
	}
	note, err := readSimText(c, "note", "note", false, simMaxNote)
	if err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(c, `UPDATE analytics.recommendations SET status = 'DISMISSED', decided_by = $2, decided_at = now(), decision_note = $3
		WHERE id = $1 AND status = 'VALIDATED'`, id, c.User.ID, note)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.Conflict(fmt.Sprintf("Only a validated recommendation can be dismissed (this one is %s)", rec["status"]))
	}
	return s.recLoad(c, id)
}

// aiRunOptimizer requests an optimizer run now (ai-analytics picks PENDING
// runs on NOTIFY optimizer_runs). A run already pending or running is
// returned instead of a second one, unless it is older than
// optimizerStaleAfter: a run a crashed or cancelled worker left behind is
// marked FAILED and no longer blocks a new one.
func (s *Server) aiRunOptimizer(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "update"); err != nil {
		return nil, err
	}
	trigger := "manual"
	if c.fromAgent() {
		trigger = "agent"
	}
	var id int64
	var status string
	existing := false
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `SELECT pg_advisory_xact_lock(hashtextextended('afdd:optimizer_runs', 0))`); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `UPDATE analytics.optimizer_runs SET status = 'FAILED', error = 'interrupted (stale)', finished_at = now()
			WHERE status IN ('PENDING', 'RUNNING') AND COALESCE(started_at, created_at) <= now() - interval '`+optimizerStaleAfter+`'`); err != nil {
			return err
		}
		err := tx.QueryRow(c, `SELECT id, status FROM analytics.optimizer_runs WHERE status IN ('PENDING', 'RUNNING')
			ORDER BY created_at DESC LIMIT 1`).Scan(&id, &status)
		if err == nil {
			existing = true
			return nil
		}
		if !db.IsNoRows(err) {
			return err
		}
		status = "PENDING"
		if err := tx.QueryRow(c, `INSERT INTO analytics.optimizer_runs (trigger, requested_by, status) VALUES ($1, $2, 'PENDING') RETURNING id`,
			trigger, c.User.ID).Scan(&id); err != nil {
			return err
		}
		_, err = tx.Exec(c, `SELECT pg_notify('optimizer_runs', $1::text)`, strconv.FormatInt(id, 10))
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"run_id": id, "status": status, "existing": existing}, nil
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

// dailyBudgetUSD is the AI services' daily Gemini budget (AI_DAILY_BUDGET_USD).
func dailyBudgetUSD() float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("AI_DAILY_BUDGET_USD")), 64); err == nil && v >= 0 {
		return v
	}
	return recDefaultBudgetUSD
}

// optimizerPeriod infers the scheduled optimizer period from the last two
// scheduled runs (60..120 minutes, default 60).
func optimizerPeriod(runs []time.Time) time.Duration {
	if len(runs) >= 2 {
		gap := runs[0].Sub(runs[1]).Round(time.Minute)
		if gap >= optimizerMinPeriod*time.Minute && gap <= optimizerMaxPeriod*time.Minute {
			return gap
		}
	}
	return optimizerMinPeriod * time.Minute
}

// aiOverview is the AI insights strip: the current recommendation, fault and
// anomaly counts, the document library state, the optimizer schedule and
// today's AI spend.
func (s *Server) aiOverview(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	var rec any
	var args sqlArgs
	list, err := s.recQuery(c, args, []string{"r.status IN ('PROPOSED', 'VALIDATING', 'VALIDATED')", "r.valid_until > now()"}, 1)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		rec = list[0]
	}

	counts := func(table string) (map[string]any, error) {
		var a sqlArgs
		where := "TRUE"
		scope, err := assetScopeClause(c, &a)
		if err != nil {
			return nil, err
		}
		if scope != "" {
			where = scope
		}
		var active, recent int
		if err := s.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE x.status = 'ACTIVE'),
				count(*) FILTER (WHERE x.started_at >= now() - interval '24 hours')
			FROM `+table+` x JOIN asset.equipment e ON e.id = x.asset_id WHERE `+where, a...).Scan(&active, &recent); err != nil {
			return nil, err
		}
		return map[string]any{"active": active, "last24h": recent}, nil
	}
	faults, err := counts("analytics.faults")
	if err != nil {
		return nil, err
	}
	anomalies, err := counts("analytics.anomalies")
	if err != nil {
		return nil, err
	}

	var dargs sqlArgs
	dwhere := "TRUE"
	dscope, err := s.docScopeClause(c, &dargs)
	if err != nil {
		return nil, err
	}
	if dscope != "" {
		dwhere = dscope
	}
	var ready, pending, failed int
	if err := s.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE d.status = 'READY'), count(*) FILTER (WHERE d.status IN ('PENDING', 'INDEXING')),
		count(*) FILTER (WHERE d.status = 'FAILED') FROM rag.documents d WHERE `+dwhere, dargs...).Scan(&ready, &pending, &failed); err != nil {
		return nil, err
	}

	optimizer := map[string]any{"last_run_at": nil, "status": nil, "next_run_at": nil}
	var lastStatus string
	var lastAt time.Time
	switch err := s.DB.QueryRow(c, `SELECT status, COALESCE(started_at, created_at) FROM analytics.optimizer_runs
		ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&lastStatus, &lastAt); {
	case err == nil:
		optimizer["last_run_at"], optimizer["status"] = httpx.TS(lastAt), lastStatus
	case !db.IsNoRows(err):
		return nil, err
	}
	var scheduled []time.Time
	rows, err := s.DB.Query(c, `SELECT created_at FROM analytics.optimizer_runs WHERE trigger = 'schedule' ORDER BY created_at DESC LIMIT 2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		scheduled = append(scheduled, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(scheduled) > 0 {
		optimizer["next_run_at"] = httpx.TS(scheduled[0].Add(optimizerPeriod(scheduled)))
	}

	var spent float64
	if err := s.DB.QueryRow(c, `SELECT COALESCE(sum(cost_usd), 0)::float8 FROM analytics.llm_usage
		WHERE at >= (date_trunc('day', now() AT TIME ZONE $1) AT TIME ZONE $1)`, s.cfg.DefaultTimezone).Scan(&spent); err != nil {
		return nil, err
	}
	return map[string]any{
		"recommendation": rec, "faults": faults, "anomalies": anomalies,
		"documents": map[string]any{"ready": ready, "pending": pending, "failed": failed},
		"optimizer": optimizer,
		"budget":    map[string]any{"today_usd": httpx.Round(spent, 4), "limit_usd": dailyBudgetUSD()},
	}, nil
}

// ---------------------------------------------------------------------------
// Faults and anomalies
// ---------------------------------------------------------------------------

const faultColumns = `f.id, f.site_code, f.rule_code, f.asset_id, f.severity, f.message, f.intensity_estimate, f.started_at, f.last_seen_at,
	f.ended_at, f.status, f.instruction_status, f.instruction_model, f.instruction_at, e.alias, e.name, fr.name, fr.lbnl_fault`

func scanFault(row pgx.Row, extra ...any) (map[string]any, error) {
	var id, assetID int64
	var site, code, severity, message, status, instrStatus, alias, name string
	var intensity *float64
	var started, lastSeen time.Time
	var ended, instrAt *time.Time
	var instrModel, ruleName, lbnl *string
	dest := []any{&id, &site, &code, &assetID, &severity, &message, &intensity, &started, &lastSeen, &ended, &status, &instrStatus,
		&instrModel, &instrAt, &alias, &name, &ruleName, &lbnl}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "site_code": site, "rule_code": code, "asset_id": assetID, "severity": severity, "message": message,
		"intensity_estimate": finiteOrNil(intensity), "started_at": httpx.TS(started), "last_seen_at": httpx.TS(lastSeen),
		"ended_at": httpx.TSPtr(ended), "status": status, "instruction_status": instrStatus, "instruction_model": instrModel,
		"instruction_at": httpx.TSPtr(instrAt), "asset_alias": alias, "asset_name": name, "rule_name": ruleName, "lbnl_fault": lbnl,
	}, nil
}

const faultFrom = ` FROM analytics.faults f JOIN asset.equipment e ON e.id = f.asset_id LEFT JOIN analytics.fault_rules fr ON fr.code = f.rule_code`

// timeWindowConds adds the from / to filters: rows overlapping [from, to).
func (c *Ctx) timeWindowConds(args *sqlArgs, startCol, endCol string) []string {
	var conds []string
	if from := c.timeParam("from"); from != nil {
		conds = append(conds, "("+endCol+" IS NULL OR "+endCol+" >= "+args.add(*from)+")")
	}
	if to := c.timeParam("to"); to != nil {
		conds = append(conds, startCol+" < "+args.add(*to))
	}
	return conds
}

// aiFaults lists detected faults. Input: status? (CSV), asset_ids?,
// rule_codes?, from?, to?, limit (1..200, 50).
func (s *Server) aiFaults(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	var args sqlArgs
	conds := []string{"TRUE"}
	if list, ok := c.F.StringList("status"); ok && len(list) > 0 {
		up := make([]string, len(list))
		for i, st := range list {
			up[i] = strings.ToUpper(strings.TrimSpace(st))
			if !contains(faultStatuses, up[i]) {
				return nil, httpx.BadRequestf("Unknown fault status %q", st)
			}
		}
		conds = append(conds, "f.status = ANY("+args.add(up)+")")
	}
	if ids, ok := c.F.IDs("asset_ids"); ok && len(ids) > 0 {
		conds = append(conds, "f.asset_id = ANY("+args.add(ids)+")")
	}
	if list, ok := c.F.StringList("rule_codes"); ok && len(list) > 0 {
		up := make([]string, len(list))
		for i, code := range list {
			up[i] = strings.ToUpper(strings.TrimSpace(code))
		}
		conds = append(conds, "f.rule_code = ANY("+args.add(up)+")")
	}
	conds = append(conds, c.timeWindowConds(&args, "f.started_at", "f.ended_at")...)
	scope, err := assetScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		conds = append(conds, scope)
	}
	q := `SELECT ` + faultColumns + faultFrom + ` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY f.started_at DESC, f.id DESC LIMIT ` + args.add(c.limit(50, 200))
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		f, err := scanFault(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// aiFault returns a fault with its evidence and maintenance instruction.
// Input: id.
func (s *Server) aiFault(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("id", 0)
	if id <= 0 {
		return nil, httpx.BadRequest("A fault id is required")
	}
	var args sqlArgs
	where := "f.id = " + args.add(id)
	scope, err := assetScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		where += " AND " + scope
	}
	var md *string
	var sources, evidence json.RawMessage
	f, err := scanFault(s.DB.QueryRow(c, `SELECT `+faultColumns+`, f.instruction_md, f.instruction_sources, f.evidence`+faultFrom+
		` WHERE `+where, args...), &md, &sources, &evidence)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Fault not found")
		}
		return nil, err
	}
	f["instruction_md"], f["instruction_sources"], f["evidence"] = md, sources, evidence
	return f, nil
}

// aiAnomalies lists sensor anomalies. Input: status? (CSV), asset_ids?,
// detectors? (CSV), from?, to?, limit (1..500, 100).
func (s *Server) aiAnomalies(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	var args sqlArgs
	conds := []string{"TRUE"}
	if list, ok := c.F.StringList("status"); ok && len(list) > 0 {
		up := make([]string, len(list))
		for i, st := range list {
			up[i] = strings.ToUpper(strings.TrimSpace(st))
			if !contains(faultStatuses, up[i]) {
				return nil, httpx.BadRequestf("Unknown anomaly status %q", st)
			}
		}
		conds = append(conds, "x.status = ANY("+args.add(up)+")")
	}
	if ids, ok := c.F.IDs("asset_ids"); ok && len(ids) > 0 {
		conds = append(conds, "x.asset_id = ANY("+args.add(ids)+")")
	}
	if list, ok := c.F.StringList("detectors"); ok && len(list) > 0 {
		low := lowerAll(list)
		for _, d := range low {
			if !contains(anomalyDetectors, d) {
				return nil, httpx.BadRequestf("Unknown detector %q", d)
			}
		}
		conds = append(conds, "x.detector = ANY("+args.add(low)+")")
	}
	conds = append(conds, c.timeWindowConds(&args, "x.started_at", "x.ended_at")...)
	scope, err := assetScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		conds = append(conds, scope)
	}
	q := `SELECT x.id, x.site_code, x.asset_id, x.equipment_parameter_id, x.param_alias, x.detector, x.severity,
			x.score, x.value, x.expected, x.message, x.details, x.started_at, x.last_seen_at, x.ended_at, x.status, e.alias, e.name,
			mp.name, mp.unit
		FROM analytics.anomalies x JOIN asset.equipment e ON e.id = x.asset_id
		LEFT JOIN asset.equipment_parameters ep ON ep.id = x.equipment_parameter_id LEFT JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ` + strings.Join(conds, " AND ") + ` ORDER BY x.started_at DESC, x.id DESC LIMIT ` + args.add(c.limit(100, 500))
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, assetID, epid int64
		var site, param, detector, severity, message, status, alias, name string
		var score, value, expected *float64
		var details json.RawMessage
		var started, lastSeen time.Time
		var ended *time.Time
		var paramName, unit *string
		if err := rows.Scan(&id, &site, &assetID, &epid, &param, &detector, &severity, &score, &value, &expected, &message, &details,
			&started, &lastSeen, &ended, &status, &alias, &name, &paramName, &unit); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "site_code": site, "asset_id": assetID, "equipment_parameter_id": epid, "param_alias": param, "detector": detector,
			"severity": severity, "score": finiteOrNil(score), "value": finiteOrNil(value), "expected": finiteOrNil(expected),
			"message": message, "details": details, "started_at": httpx.TS(started), "last_seen_at": httpx.TS(lastSeen),
			"ended_at": httpx.TSPtr(ended), "status": status, "asset_alias": alias, "asset_name": name, "param_name": paramName, "unit": unit,
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Rules and point map
// ---------------------------------------------------------------------------

const ruleColumns = `r.code, r.name, r.lbnl_fault, r.description, r.asset_scope, r.signals, r.params, r.defaults, r.calibration,
	r.calibrated_at, r.enabled, r.severity, r.version, r.updated_by, r.updated_at, u.full_name`

const ruleFrom = ` FROM analytics.fault_rules r LEFT JOIN core.users u ON u.id = r.updated_by`

func scanRule(row pgx.Row) (map[string]any, error) {
	var code, name, lbnl, desc, scope, severity string
	var signals, params, defaults, calibration json.RawMessage
	var calibratedAt *time.Time
	var enabled bool
	var version int
	var updatedBy *int64
	var updatedAt time.Time
	var updatedByName *string
	if err := row.Scan(&code, &name, &lbnl, &desc, &scope, &signals, &params, &defaults, &calibration, &calibratedAt, &enabled,
		&severity, &version, &updatedBy, &updatedAt, &updatedByName); err != nil {
		return nil, err
	}
	return map[string]any{
		"code": code, "name": name, "lbnl_fault": lbnl, "description": desc, "asset_scope": scope, "signals": signals,
		"params": params, "defaults": defaults, "calibration": calibration, "calibrated_at": httpx.TSPtr(calibratedAt),
		"enabled": enabled, "severity": severity, "version": version, "updated_by": updatedBy, "updated_by_name": updatedByName,
		"updated_at": httpx.TS(updatedAt),
	}, nil
}

// aiRules lists the FDD rules with their thresholds and calibration.
func (s *Server) aiRules(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT `+ruleColumns+ruleFrom+` ORDER BY r.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ruleParamsUpdate validates new rule parameters against the rule's
// engineering defaults: every key must be a default parameter and keep its
// JSON type (numbers must be finite; multipart strings are converted).
func ruleParamsUpdate(tree any, defaults map[string]any) (map[string]any, error) {
	m, ok := tree.(map[string]any)
	if !ok {
		return nil, simBad("params must be a JSON object")
	}
	out := map[string]any{}
	for k, v := range m {
		def, known := defaults[k]
		if len(defaults) > 0 && !known {
			return nil, simBad("Unknown rule parameter %q", k)
		}
		switch def.(type) {
		case json.Number, float64:
			x, ok := treeNumber(v)
			if !ok {
				return nil, simBad("Rule parameter %s must be a number", k)
			}
			out[k] = x
		case bool:
			b, ok := treeBool(v)
			if !ok {
				return nil, simBad("Rule parameter %s must be true or false", k)
			}
			out[k] = b
		case string:
			str, ok := treeString(v)
			if !ok {
				return nil, simBad("Rule parameter %s must be text", k)
			}
			out[k] = str
		case []any:
			if _, ok := v.([]any); !ok {
				return nil, simBad("Rule parameter %s must be a list", k)
			}
			out[k] = v
		case map[string]any:
			if _, ok := v.(map[string]any); !ok {
				return nil, simBad("Rule parameter %s must be an object", k)
			}
			out[k] = v
		default: // no default to compare with
			if x, ok := treeNumber(v); ok {
				out[k] = x
			} else {
				out[k] = v
			}
		}
	}
	return out, nil
}

// aiUpdateRule changes a rule's enabled flag and / or parameters (admins, a
// person only). Input: code, enabled?, params? (JSON object, merged into the
// current parameters) -> Rule (version + 1).
func (s *Server) aiUpdateRule(c *Ctx) (any, error) {
	if err := c.RequireHuman(); err != nil {
		return nil, err
	}
	code := strings.ToUpper(c.F.StrTrim("code"))
	if code == "" {
		return nil, httpx.BadRequest("A rule code is required")
	}
	enabled := c.F.BoolPtr("enabled")
	tree, hasParams, err := formTree(c, "params")
	if err != nil {
		return nil, err
	}
	if enabled == nil && !hasParams {
		return nil, httpx.BadRequest("Nothing to change (enabled or params)")
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(c, `SELECT defaults FROM analytics.fault_rules WHERE code = $1 FOR UPDATE`, code).Scan(&raw); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("Rule not found")
			}
			return err
		}
		patch := []byte("{}")
		if hasParams {
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			var defaults map[string]any
			if err := dec.Decode(&defaults); err != nil {
				defaults = map[string]any{}
			}
			params, err := ruleParamsUpdate(tree, defaults)
			if err != nil {
				return err
			}
			if patch, err = json.Marshal(params); err != nil {
				return err
			}
		}
		_, err := tx.Exec(c, `UPDATE analytics.fault_rules SET enabled = COALESCE($2, enabled), params = params || $3::jsonb,
			version = version + 1, updated_by = $4, updated_at = now() WHERE code = $1`, code, enabled, string(patch), c.User.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.Info("FDD rule updated", "code", code, "user", c.User.Username)
	return scanRule(s.DB.QueryRow(c, `SELECT `+ruleColumns+ruleFrom+` WHERE r.code = $1`, code))
}

// aiPointMap lists the LBNL point names mapped to the plant's tags.
func (s *Server) aiPointMap(c *Ctx) (any, error) {
	if err := c.Require("aiinsights", "read"); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT lbnl_name, lbnl_unit, asset_pattern, param_alias, si_unit, description FROM analytics.point_map
		ORDER BY asset_pattern, lbnl_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, lunit, pattern, param, unit, desc string
		if err := rows.Scan(&name, &lunit, &pattern, &param, &unit, &desc); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"lbnl_name": name, "lbnl_unit": lunit, "asset_pattern": pattern, "param_alias": param,
			"si_unit": unit, "description": desc})
	}
	return out, rows.Err()
}
