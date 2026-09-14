package api

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/httpx"
)

// Production monitoring of FAB1 (docs/production/DESIGN.md): the 40 nm line
// with its 28 production tools (lithography cells, etch, deposition, CMP,
// diffusion, implant, cleans, strip, metrology, test): output, cycle time,
// work in progress by area and mask layer, quality per characteristic, and
// the SEMI E10 / E79 OEE of the tools. The simulator writes the prod schema;
// the tools' time states come from their equipment status intervals (Running
// = productive, Idle = standby, Planned down = scheduled down, Unplanned down
// = unscheduled down, Offline and missing data = non-scheduled).

func (s *Server) registerProduction() {
	s.handle("production/getoverview", AccessUser, s.prodOverview)
	s.handle("production/gettools", AccessUser, s.prodToolsKPI)
	s.handle("production/gettrend", AccessUser, s.prodTrend)
	s.handle("production/gettimeline", AccessUser, s.prodTimeline)
	s.handle("production/getlots", AccessUser, s.prodLots)
	s.handle("production/getlot", AccessUser, s.prodLot)
	s.handle("production/getqualitykeys", AccessUser, s.prodQualityKeys)
	s.handle("production/getquality", AccessUser, s.prodQuality)
	s.registerProductionPlan()
}

// prodToolTypes maps the equipment types of the production tools to their
// process area (prod.route_steps.area). Litho tracks belong to their
// exposure tool's cell and are not counted on their own.
var prodToolTypes = map[string]string{
	"Lithography Scanner": "LITHO", "Plasma Etch Tool": "ETCH", "CVD Tool": "CVD", "PVD Tool": "PVD", "Electroplating Tool": "ECP",
	"CMP Tool": "CMP", "Diffusion Furnace": "DIFF", "RTP Tool": "DIFF", "Ion Implanter": "IMP", "Wet Clean Station": "WET", "Asher": "STRIP",
	"Metrology Tool": "MET", "Inspection Tool": "MET", "Parametric Test System": "TEST",
}

// prodTypeOrder is the order of the process areas (the flow of a mask layer).
var prodTypeOrder = []string{"LITHO", "ETCH", "CVD", "PVD", "ECP", "CMP", "DIFF", "IMP", "WET", "STRIP", "MET", "TEST"}

// prodAreaNames are the display names of the areas.
var prodAreaNames = map[string]string{"LITHO": "Lithography", "ETCH": "Etch", "CVD": "CVD", "PVD": "PVD", "ECP": "Cu plating", "CMP": "CMP",
	"DIFF": "Diffusion & RTP", "IMP": "Implant", "WET": "Wet clean", "STRIP": "Resist strip", "MET": "Metrology & inspection", "TEST": "Parametric test"}

// prodChambers are the parallel chambers of the multi-chamber tools (by model).
var prodChambers = map[string]int{"ICP Etch Cluster ET-300": 3, "CCP Dielectric Etch DE-300": 3, "PECVD Cluster CV-300": 3,
	"PVD Cluster PV-300": 3, "Cu Plating ECP-300": 4, "Single-Wafer Clean SW-300": 4, "Plasma Asher AS-300": 2}

// prodDetourArea is the area of the rework steps (lots on a detour).
const prodDetourArea = `CASE WHEN l.step_code = 'RW-STRIP' THEN 'STRIP' WHEN l.step_code = 'RW-SCRUB' THEN 'WET'
	WHEN l.step_code LIKE 'RW-%' THEN 'WET' ELSE rs.area END`

func nanNil(f float64) any {
	if math.IsNaN(f) {
		return nil
	}
	return f
}

type prodTool struct {
	ID         int64
	Alias      string
	Name       string
	Model      string
	Type       string
	Chambers   int
	LocationID int64
}

func typeIndex(t string) int {
	for i, x := range prodTypeOrder {
		if x == t {
			return i
		}
	}
	return len(prodTypeOrder)
}

// prodTools returns the production tools the user may see (after checking
// the production permission), by area and alias.
func (s *Server) prodTools(c *Ctx) ([]*prodTool, error) {
	if err := c.Require("production", "read"); err != nil {
		return nil, err
	}
	args := sqlArgs{}
	names := make([]string, 0, len(prodToolTypes))
	for n := range prodToolTypes {
		names = append(names, n)
	}
	q := `SELECT e.id, e.alias, e.name, m.name, t.name, e.location_id FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id
		JOIN asset.types t ON t.id = m.type_id WHERE t.name = ANY(` + args.add(names) + `)`
	sc, err := assetScopeClause(c, &args)
	if err != nil {
		return nil, err
	}
	if sc != "" {
		q += " AND " + sc
	}
	rows, err := s.DB.Query(c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*prodTool
	for rows.Next() {
		t := &prodTool{}
		var typ string
		if err := rows.Scan(&t.ID, &t.Alias, &t.Name, &t.Model, &typ, &t.LocationID); err != nil {
			return nil, err
		}
		t.Type = prodToolTypes[typ]
		t.Chambers = max(prodChambers[t.Model], 1)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := typeIndex(out[i].Type), typeIndex(out[j].Type); a != b {
			return a < b
		}
		return out[i].Alias < out[j].Alias
	})
	return out, nil
}

func toolIDs(tools []*prodTool) []int64 {
	out := make([]int64, len(tools))
	for i, t := range tools {
		out[i] = t.ID
	}
	return out
}

// prodWindow returns [now - hours, now) from the hours parameter.
func prodWindow(c *Ctx, def, maxH int64) (time.Time, time.Time, int64) {
	h := c.F.Int("hours", def)
	if h < 1 {
		h = def
	}
	if h > maxH {
		h = maxH
	}
	to := time.Now().UTC()
	return to.Add(-time.Duration(h) * time.Hour), to, h
}

// oeeAcc holds the time states (seconds) and completed runs of a tool, or of
// several tools summed by add.
type oeeAcc struct {
	T, N, S, U, SB, P  float64
	Theo, Actual       float64
	Runs, In, Scr, Rew int64
	hasRuns            bool
	moves, waferMoves  int64
	tools              int
	PR, PRQ            float64 // aggregates: productive time x rate (x quality)
	aggregated         bool
}

// rq returns a tool's rate and quality efficiency (1 without runs).
func (a *oeeAcc) rq() (float64, float64) {
	r, q := 1.0, 1.0
	if a.Actual > 0 {
		r = math.Min(a.Theo/a.Actual, 1)
	}
	if a.In > 0 {
		q = math.Max(0, float64(a.In-a.Scr-a.Rew)/float64(a.In))
	}
	return r, q
}

// add sums a tool into an aggregate: the time-weighted factors multiply to
// the aggregate OEE (Σ good time / Σ operations time).
func (a *oeeAcc) add(t *oeeAcc) {
	r, q := t.rq()
	a.T, a.N, a.S, a.U, a.SB, a.P = a.T+t.T, a.N+t.N, a.S+t.S, a.U+t.U, a.SB+t.SB, a.P+t.P
	a.PR += t.P * r
	a.PRQ += t.P * r * q
	a.Theo, a.Actual = a.Theo+t.Theo, a.Actual+t.Actual
	a.Runs, a.In, a.Scr, a.Rew = a.Runs+t.Runs, a.In+t.In, a.Scr+t.Scr, a.Rew+t.Rew
	a.hasRuns = a.hasRuns || t.hasRuns
	a.moves, a.waferMoves = a.moves+t.moves, a.waferMoves+t.waferMoves
	a.tools++
	a.aggregated = true
}

func prodPct(num, den float64) any {
	if den <= 0 {
		return nil
	}
	return round1(math.Min(100, math.Max(0, 100*num/den)))
}

// factors returns availability, operational, rate and quality efficiency,
// OEE, uptime and utilization (percent, nil when undefined).
func (a *oeeAcc) factors() map[string]any {
	sched, up := a.T-a.N, a.P+a.SB
	var pr, prq float64
	if a.aggregated {
		pr, prq = a.PR, a.PRQ
	} else {
		r, q := a.rq()
		pr, prq = a.P*r, a.P*r*q
	}
	out := map[string]any{
		"availability": prodPct(up, sched), "operational": prodPct(a.P, up), "rate": nil, "quality": nil,
		"oee": prodPct(prq, sched), "uptime": prodPct(up, a.T), "utilization": prodPct(a.P, a.T),
	}
	if a.hasRuns {
		out["rate"], out["quality"] = prodPct(pr, a.P), prodPct(prq, pr)
		if a.P <= 0 {
			r, q := a.rq()
			out["rate"], out["quality"] = round1(100*r), round1(100*q)
		}
	}
	return out
}

// toolAccs computes the time states and runs of tools over [from, to).
// Batch runs count once per batch (their lots share the run).
func (s *Server) toolAccs(ctx context.Context, ids []int64, from, to time.Time) (map[int64]*oeeAcc, error) {
	out := map[int64]*oeeAcc{}
	span := elapsed(from, to)
	for _, id := range ids {
		out[id] = &oeeAcc{T: span}
	}
	if len(ids) == 0 {
		return out, nil
	}
	durs, err := s.statusDurations(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		a, d := out[id], durs[id]
		a.P, a.SB, a.U, a.S = d[engine.StatusRunning], d[engine.StatusIdle], d[engine.StatusUnplannedDown], d[engine.StatusPlannedDown]
		a.N = math.Max(0, a.T-a.P-a.SB-a.U-a.S) // offline and no data
	}
	rows, err := s.DB.Query(ctx, `SELECT equipment_id, count(*) FILTER (WHERE theoretical_s IS NOT NULL),
			COALESCE(sum(theoretical_s), 0), COALESCE(sum(actual_s) FILTER (WHERE theoretical_s IS NOT NULL), 0),
			COALESCE(sum(wafers_in) FILTER (WHERE theoretical_s IS NOT NULL), 0), COALESCE(sum(scrapped), 0), COALESCE(sum(reworked), 0)
		FROM prod.lot_steps WHERE equipment_id = ANY($1) AND status = 'DONE' AND ended_at >= $2 AND ended_at < $3
		GROUP BY equipment_id`, ids, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, runs, in, scr, rew int64
		var theo, actual float64
		if err := rows.Scan(&id, &runs, &theo, &actual, &in, &scr, &rew); err != nil {
			return nil, err
		}
		if a := out[id]; a != nil {
			a.Runs, a.Theo, a.Actual, a.In, a.Scr, a.Rew = runs, theo, actual, in, scr, rew
			a.hasRuns = runs > 0
			a.moves, a.waferMoves = runs, in
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Overview

func (s *Server) prodOverview(c *Ctx) (any, error) {
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	from, to, h := prodWindow(c, 24, 720)
	accs, err := s.toolAccs(c, toolIDs(tools), from, to)
	if err != nil {
		return nil, err
	}
	total := &oeeAcc{}
	byType := map[string]*oeeAcc{}
	for _, t := range tools {
		total.add(accs[t.ID])
		if byType[t.Type] == nil {
			byType[t.Type] = &oeeAcc{}
		}
		byType[t.Type].add(accs[t.ID])
	}
	f := total.factors()
	kpi := map[string]any{"oee": f["oee"], "availability": f["availability"], "operational": f["operational"], "rate": f["rate"],
		"quality": f["quality"], "uptime": f["uptime"], "utilization": f["utilization"], "wafer_starts": 0, "lot_starts": 0, "wafer_outs": 0,
		"lot_outs": 0, "moves": total.moves, "wafer_moves": total.waferMoves, "wip_lots": 0, "wip_wafers": 0, "cycle_time_h": nil,
		"cycle_time_days": nil, "x_factor": nil, "on_time": nil, "line_yield": nil, "scrap_wafers": 0, "rework_wafers": 0, "hold_lots": 0,
		"tools": len(tools), "transfers": 0, "transport_min": nil, "oht_util": nil, "vehicles": 0}
	r, q := 1.0, 1.0
	if total.P > 0 {
		r = total.PR / total.P
	}
	if total.PR > 0 {
		q = total.PRQ / total.PR
	}
	toH := func(sec float64) float64 { return round1(sec / 3600) }
	losses := map[string]any{"total_h": toH(total.T), "non_scheduled_h": toH(total.N), "scheduled_down_h": toH(total.S),
		"unscheduled_down_h": toH(total.U), "standby_h": toH(total.SB), "rate_loss_h": toH(total.P * (1 - r)),
		"quality_loss_h": toH(total.P * r * (1 - q)), "good_h": toH(total.PRQ)}
	types := []map[string]any{}
	for _, typ := range prodTypeOrder {
		a := byType[typ]
		if a == nil {
			continue
		}
		tf := a.factors()
		types = append(types, map[string]any{"tool_type": typ, "name": prodAreaNames[typ], "oee": tf["oee"], "availability": tf["availability"],
			"operational": tf["operational"], "rate": tf["rate"], "quality": tf["quality"], "utilization": tf["utilization"], "tools": a.tools,
			"wafers": a.In, "moves": a.moves})
	}
	res := map[string]any{"window": map[string]any{"from": httpx.TS(from), "to": httpx.TS(to), "hours": h}, "kpi": kpi, "losses": losses,
		"by_type": types, "wip_by_area": []any{}, "wip_by_layer": []any{}, "products": []any{}}
	if len(tools) > 0 {
		products, err := s.prodModuleKPI(c, kpi, from, to)
		if err != nil {
			return nil, err
		}
		res["products"] = products
		if res["wip_by_area"], err = s.prodWipByArea(c); err != nil {
			return nil, err
		}
		if res["wip_by_layer"], err = s.prodWipByLayer(c); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// prodModuleKPI fills the fab KPIs (starts, outs, cycle time, X-factor,
// on-time delivery, line yield, WIP, holds, scrap and rework) and returns
// them per product.
func (s *Server) prodModuleKPI(c *Ctx, kpi map[string]any, from, to time.Time) ([]map[string]any, error) {
	var starts, lotStarts int64
	if err := s.DB.QueryRow(c, `SELECT COALESCE(sum(wafers_start), 0), count(*) FROM prod.lots WHERE released_at >= $1 AND released_at < $2`,
		from, to).Scan(&starts, &lotStarts); err != nil {
		return nil, err
	}
	kpi["wafer_starts"], kpi["lot_starts"] = starts, lotStarts
	rows, err := s.DB.Query(c, `SELECT p.code, p.name, COALESCE(p.masks, p.layers), p.cycle_days, p.ideal_h,
			count(l.*) FILTER (WHERE l.status = 'DONE'), COALESCE(sum(l.wafers) FILTER (WHERE l.status = 'DONE'), 0),
			COALESCE(sum(l.wafers_start), 0),
			avg(EXTRACT(EPOCH FROM l.completed_at - l.released_at) / 3600) FILTER (WHERE l.status = 'DONE'),
			count(*) FILTER (WHERE l.status = 'DONE' AND l.completed_at <= l.due_at),
			(SELECT count(*) FROM prod.lots w WHERE w.product_code = p.code AND w.status IN ('QUEUED', 'PROCESSING', 'HOLD'))
		FROM prod.products p LEFT JOIN prod.lots l ON l.product_code = p.code AND l.completed_at >= $1 AND l.completed_at < $2
			AND l.status IN ('DONE', 'SCRAPPED')
		GROUP BY p.code ORDER BY p.code`, from, to)
	if err != nil {
		return nil, err
	}
	products := []map[string]any{}
	var lotOuts, waferOuts, finishedStart, onTime int64
	var ctSum float64
	var xSum, xN float64
	for rows.Next() {
		var code, name string
		var masks, outs, wout, wstart, ontime, wip int64
		var cycleDays, idealH, ct *float64
		if err := rows.Scan(&code, &name, &masks, &cycleDays, &idealH, &outs, &wout, &wstart, &ct, &ontime, &wip); err != nil {
			rows.Close()
			return nil, err
		}
		p := map[string]any{"product_code": code, "name": name, "masks": masks, "planned_cycle_days": cycleDays, "lot_outs": outs,
			"wafer_outs": wout, "wip_lots": wip, "cycle_time_days": nil, "days_per_mask": nil, "x_factor": nil, "on_time": nil, "line_yield": nil}
		if ct != nil {
			p["cycle_time_days"] = round2(*ct / 24)
			if masks > 0 {
				p["days_per_mask"] = round2(*ct / 24 / float64(masks))
			}
			if idealH != nil && *idealH > 0 {
				p["x_factor"] = round2(*ct / *idealH)
				xSum += *ct / *idealH * float64(outs)
				xN += float64(outs)
			}
			ctSum += *ct * float64(outs)
		}
		if outs > 0 {
			p["on_time"] = round1(100 * float64(ontime) / float64(outs))
		}
		if wstart > 0 {
			p["line_yield"] = round2(100 * float64(wout) / float64(wstart))
		}
		lotOuts, waferOuts, finishedStart, onTime = lotOuts+outs, waferOuts+wout, finishedStart+wstart, onTime+ontime
		products = append(products, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kpi["lot_outs"], kpi["wafer_outs"] = lotOuts, waferOuts
	if lotOuts > 0 {
		kpi["cycle_time_h"] = round2(ctSum / float64(lotOuts))
		kpi["cycle_time_days"] = round2(ctSum / float64(lotOuts) / 24)
		kpi["on_time"] = round1(100 * float64(onTime) / float64(lotOuts))
	}
	if xN > 0 {
		kpi["x_factor"] = round2(xSum / xN)
	}
	if finishedStart > 0 {
		kpi["line_yield"] = httpx.Round(100*float64(waferOuts)/float64(finishedStart), 2)
	}
	srows, err := s.DB.Query(c, `SELECT status, count(*), COALESCE(sum(wafers), 0) FROM prod.lots
		WHERE status IN ('QUEUED', 'PROCESSING', 'AWAY', 'HOLD') GROUP BY status`)
	if err != nil {
		return nil, err
	}
	var wipLots, wipWafers int64
	for srows.Next() {
		var st string
		var n, w int64
		if err := srows.Scan(&st, &n, &w); err != nil {
			srows.Close()
			return nil, err
		}
		wipLots, wipWafers = wipLots+n, wipWafers+w
		if st == "HOLD" {
			kpi["hold_lots"] = n
		}
	}
	srows.Close()
	kpi["wip_lots"], kpi["wip_wafers"] = wipLots, wipWafers
	var scrap, rework int64
	if err := s.DB.QueryRow(c, `SELECT COALESCE((SELECT sum(wafers) FROM prod.scrap WHERE at >= $1 AND at < $2), 0),
		COALESCE((SELECT sum(reworked) FROM prod.lot_steps WHERE ended_at >= $1 AND ended_at < $2), 0)`, from, to).Scan(&scrap, &rework); err != nil {
		return nil, err
	}
	kpi["scrap_wafers"], kpi["rework_wafers"] = scrap, rework
	// overhead transport: transfers, mean minutes from request to delivery,
	// the fleet's busy share (the vehicles that moved lots in the window)
	var moves, vehicles int64
	var mins, busy *float64
	if err := s.DB.QueryRow(c, `SELECT count(*), avg(extract(epoch FROM delivered_at - requested_at)) / 60, sum(busy_s), count(DISTINCT vehicle)
		FROM prod.transfers WHERE requested_at >= $1 AND requested_at < $2`, from, to).Scan(&moves, &mins, &busy, &vehicles); err != nil {
		return nil, err
	}
	kpi["transfers"], kpi["transport_min"], kpi["oht_util"], kpi["vehicles"] = moves, roundPtr(mins, 2), nil, vehicles
	if busy != nil && vehicles > 0 {
		kpi["oht_util"] = round1(100 * *busy / (float64(vehicles) * to.Sub(from).Seconds()))
	}
	return products, nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// prodWipByArea counts the lots in the fab by the area of their current step
// and their state.
func (s *Server) prodWipByArea(c *Ctx) ([]map[string]any, error) {
	rows, err := s.DB.Query(c, `SELECT `+prodDetourArea+`, l.status, count(*), COALESCE(sum(l.wafers), 0) FROM prod.lots l
		LEFT JOIN prod.route_steps rs ON rs.product_code = l.product_code AND rs.seq = l.current_seq
		WHERE l.status IN ('QUEUED', 'PROCESSING', 'HOLD') GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]map[string]int64{}
	for rows.Next() {
		var area *string
		var st string
		var n, w int64
		if err := rows.Scan(&area, &st, &n, &w); err != nil {
			return nil, err
		}
		a := derefStr(area)
		if counts[a] == nil {
			counts[a] = map[string]int64{}
		}
		counts[a][strings.ToLower(st)] += n
		counts[a]["wafers"] += w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, a := range prodTypeOrder {
		m := counts[a]
		out = append(out, map[string]any{"area": a, "name": prodAreaNames[a], "queued": m["queued"], "processing": m["processing"], "hold": m["hold"],
			"wafers": m["wafers"]})
	}
	return out, nil
}

// prodWipByLayer counts the lots by the mask layer / module of their current
// step, in flow order (the line profile of the fab).
func (s *Server) prodWipByLayer(c *Ctx) ([]map[string]any, error) {
	rows, err := s.DB.Query(c, `WITH layers AS (
			SELECT layer, min(module) AS module, min(seq) AS first FROM prod.route_steps GROUP BY layer)
		SELECT y.layer, y.module, y.first, count(l.*) FILTER (WHERE l.status = 'QUEUED'), count(l.*) FILTER (WHERE l.status = 'PROCESSING'),
			count(l.*) FILTER (WHERE l.status = 'HOLD')
		FROM layers y LEFT JOIN prod.route_steps rs ON rs.layer = y.layer
		LEFT JOIN prod.lots l ON l.product_code = rs.product_code AND l.current_seq = rs.seq AND l.status IN ('QUEUED', 'PROCESSING', 'HOLD')
		GROUP BY y.layer, y.module, y.first ORDER BY y.first`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var layer string
		var module *string
		var first, q, p, hold int64
		if err := rows.Scan(&layer, &module, &first, &q, &p, &hold); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"layer": layer, "module": module, "queued": q, "processing": p, "hold": hold})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Tools

// prodLive returns the live values (data hub, else the last logged value of
// the last hour) of tool parameters.
func (s *Server) prodLive(c *Ctx, ids []int64, aliases []string) (map[int64]map[string]float64, error) {
	out := map[int64]map[string]float64{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(c, `SELECT ep.id, ep.equipment_id, mp.alias, g.alias, ep.tag_name FROM asset.equipment_parameters ep
		JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id LEFT JOIN dh.gateways g ON g.id = ep.gateway_id
		WHERE ep.equipment_id = ANY($1) AND mp.alias = ANY($2)`, ids, aliases)
	if err != nil {
		return nil, err
	}
	type ep struct {
		asset int64
		alias string
	}
	var stale []int64
	eps := map[int64]ep{}
	for rows.Next() {
		var id, asset int64
		var alias string
		var gw, tag *string
		if err := rows.Scan(&id, &asset, &alias, &gw, &tag); err != nil {
			rows.Close()
			return nil, err
		}
		if out[asset] == nil {
			out[asset] = map[string]float64{}
		}
		if s.Hub != nil && gw != nil && tag != nil {
			if v, ok := s.Hub.Value(*gw, *tag); ok && v.Q == datahub.QualityGood && v.T > 0 {
				if num, isNum := engine.ToValue(v.V, v.T, true, "number", nil).Num(); isNum {
					out[asset][alias] = num
					continue
				}
			}
		}
		eps[id] = ep{asset, alias}
		stale = append(stale, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stale) == 0 {
		return out, nil
	}
	rows, err = s.DB.Query(c, `SELECT DISTINCT ON (equipment_parameter_id) equipment_parameter_id, value FROM ts.parameter_values
		WHERE equipment_parameter_id = ANY($1) AND ts > now() - interval '1 hour' AND value IS NOT NULL
		ORDER BY equipment_parameter_id, ts DESC`, stale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		e := eps[id]
		out[e.asset][e.alias] = v
	}
	return out, rows.Err()
}

func liveVal(m map[string]float64, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

func (s *Server) prodToolsKPI(c *Ctx) (any, error) {
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	from, to, _ := prodWindow(c, 24, 720)
	ids := toolIDs(tools)
	accs, err := s.toolAccs(c, ids, from, to)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, ids)
	if err != nil {
		return nil, err
	}
	live, err := s.prodLive(c, ids, []string{"lot_progress", "chambers_up", "queue_lots", "throughput_wph"})
	if err != nil {
		return nil, err
	}
	lots := map[int64][2]string{}
	alarms := map[int64]int64{}
	if len(ids) > 0 {
		rows, err := s.DB.Query(c, `SELECT equipment_id, string_agg(lot_code, ', ' ORDER BY lot_code), min(step_code) FROM prod.lots
			WHERE status = 'PROCESSING' AND equipment_id = ANY($1) GROUP BY equipment_id`, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var codes string
			var step *string
			if err := rows.Scan(&id, &codes, &step); err != nil {
				rows.Close()
				return nil, err
			}
			lots[id] = [2]string{codes, derefStr(step)}
		}
		rows.Close()
		rows, err = s.DB.Query(c, `SELECT asset_id, count(*) FROM ts.alarm_events WHERE end_ts IS NULL AND asset_id = ANY($1) GROUP BY asset_id`, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			alarms[id] = n
		}
		rows.Close()
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		a, st, lv := accs[t.ID], states[t.ID], live[t.ID]
		f := a.factors()
		var avgWPH any
		if a.P > 0 {
			avgWPH = round1(float64(a.In) / (a.P / 3600))
		}
		row := map[string]any{"id": t.ID, "alias": t.Alias, "name": t.Name, "model": t.Model, "tool_type": t.Type, "area_name": prodAreaNames[t.Type],
			"status": statusName(st), "status_start_time": nil, "condition": conditionName(st), "current_lot": nil, "current_step": nil,
			"lot_progress": liveVal(lv, "lot_progress"), "chambers_up": liveVal(lv, "chambers_up"), "chambers_total": t.Chambers,
			"queue_lots": liveVal(lv, "queue_lots"), "oee": f["oee"], "availability": f["availability"], "operational": f["operational"],
			"rate": f["rate"], "quality": f["quality"], "uptime": f["uptime"], "utilization": f["utilization"], "wafers": a.In, "moves": a.moves,
			"throughput_wph": liveVal(lv, "throughput_wph"), "avg_wph": avgWPH, "scrapped": a.Scr, "reworked": a.Rew, "alarms": alarms[t.ID],
			"location_id": t.LocationID}
		if t.Chambers <= 1 {
			row["chambers_up"] = nil
		}
		if st != nil {
			row["status_start_time"] = httpx.TSPtr(st.StatusSince)
		}
		if l, ok := lots[t.ID]; ok {
			row["current_lot"], row["current_step"] = l[0], l[1]
		}
		out = append(out, row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Trend

// prodFilterTools narrows tools to equipment_id / tool_type (area).
func prodFilterTools(c *Ctx, tools []*prodTool) ([]*prodTool, error) {
	if id := c.F.IntPtr("equipment_id"); id != nil {
		for _, t := range tools {
			if t.ID == *id {
				return []*prodTool{t}, nil
			}
		}
		return nil, httpx.NotFound("err_asset_not_found")
	}
	if typ := strings.ToUpper(c.F.StrTrim("tool_type")); typ != "" {
		var out []*prodTool
		for _, t := range tools {
			if t.Type == typ {
				out = append(out, t)
			}
		}
		return out, nil
	}
	return tools, nil
}

func (s *Server) prodTrend(c *Ctx) (any, error) {
	all, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	tools, err := prodFilterTools(c, all)
	if err != nil {
		return nil, err
	}
	days := c.F.Int("days", 14)
	if days < 1 || days > 60 {
		days = 14
	}
	bucketName := "day"
	step := 24 * time.Hour
	loc := c.Location()
	now := time.Now().In(loc)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Add(24 * time.Hour)
	if strings.EqualFold(c.F.StrTrim("bucket"), "shift") {
		// day shift 08:00 - 20:00, night shift 20:00 - 08:00 plant time
		bucketName, step = "shift", 12*time.Hour
		end = time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, loc)
		for !end.After(now) {
			end = end.Add(step)
		}
	}
	from := end.Add(-time.Duration(days) * 24 * time.Hour)
	n := int(end.Sub(from) / step)
	ids := toolIDs(tools)
	accs := make([]map[int64]*oeeAcc, n)
	for i := range accs {
		accs[i] = map[int64]*oeeAcc{}
		b0 := from.Add(time.Duration(i) * step)
		span := elapsed(b0, b0.Add(step))
		for _, id := range ids {
			accs[i][id] = &oeeAcc{T: span}
		}
	}
	idx := func(t time.Time) int { return int(t.Sub(from) / step) }
	if len(ids) > 0 {
		rows, err := s.DB.Query(c, `SELECT b.b0, s.asset_id, s.status_id,
				sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(s.end_ts, now()), b.b0 + $3::interval, now()) - GREATEST(s.start_ts, b.b0))))
			FROM generate_series($1::timestamptz, $2::timestamptz - $3::interval, $3::interval) b(b0)
			JOIN ts.asset_status s ON s.start_ts < b.b0 + $3::interval AND COALESCE(s.end_ts, now()) > b.b0
			WHERE s.asset_id = ANY($4) GROUP BY 1, 2, 3`, from, end, step, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b0 time.Time
			var id int64
			var st int
			var secs float64
			if err := rows.Scan(&b0, &id, &st, &secs); err != nil {
				rows.Close()
				return nil, err
			}
			i := idx(b0)
			if i < 0 || i >= n || accs[i][id] == nil || secs < 0 {
				continue
			}
			a := accs[i][id]
			switch st {
			case engine.StatusRunning:
				a.P += secs
			case engine.StatusIdle:
				a.SB += secs
			case engine.StatusUnplannedDown:
				a.U += secs
			case engine.StatusPlannedDown:
				a.S += secs
			}
		}
		rows.Close()
		rows, err = s.DB.Query(c, `SELECT floor(EXTRACT(EPOCH FROM ended_at - $1) / $3)::int, equipment_id,
				count(*) FILTER (WHERE theoretical_s IS NOT NULL), COALESCE(sum(theoretical_s), 0),
				COALESCE(sum(actual_s) FILTER (WHERE theoretical_s IS NOT NULL), 0),
				COALESCE(sum(wafers_in) FILTER (WHERE theoretical_s IS NOT NULL), 0), COALESCE(sum(scrapped), 0), COALESCE(sum(reworked), 0)
			FROM prod.lot_steps WHERE equipment_id = ANY($4) AND status = 'DONE' AND ended_at >= $1 AND ended_at < $2
			GROUP BY 1, 2`, from, end, step.Seconds(), ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var i int
			var id, runs, in, scr, rew int64
			var theo, actual float64
			if err := rows.Scan(&i, &id, &runs, &theo, &actual, &in, &scr, &rew); err != nil {
				rows.Close()
				return nil, err
			}
			if i < 0 || i >= n || accs[i][id] == nil {
				continue
			}
			a := accs[i][id]
			a.Runs, a.Theo, a.Actual, a.In, a.Scr, a.Rew = runs, theo, actual, in, scr, rew
			a.hasRuns, a.moves, a.waferMoves = runs > 0, runs, in
		}
		rows.Close()
	}
	outs := make([]int64, n)
	starts := make([]int64, n)
	cts := make([]any, n)
	wip := make([]any, n)
	if len(all) > 0 {
		rows, err := s.DB.Query(c, `SELECT floor(EXTRACT(EPOCH FROM completed_at - $1) / $3)::int, COALESCE(sum(wafers), 0),
				avg(EXTRACT(EPOCH FROM completed_at - released_at) / 86400)
			FROM prod.lots WHERE status = 'DONE' AND completed_at >= $1 AND completed_at < $2 GROUP BY 1`, from, end, step.Seconds())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var i int
			var w int64
			var ct *float64
			if err := rows.Scan(&i, &w, &ct); err != nil {
				rows.Close()
				return nil, err
			}
			if i >= 0 && i < n {
				outs[i] = w
				if ct != nil {
					cts[i] = round2(*ct)
				}
			}
		}
		rows.Close()
		rows, err = s.DB.Query(c, `SELECT floor(EXTRACT(EPOCH FROM released_at - $1) / $3)::int, COALESCE(sum(wafers_start), 0)
			FROM prod.lots WHERE released_at >= $1 AND released_at < $2 GROUP BY 1`, from, end, step.Seconds())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var i int
			var w int64
			if err := rows.Scan(&i, &w); err != nil {
				rows.Close()
				return nil, err
			}
			if i >= 0 && i < n {
				starts[i] = w
			}
		}
		rows.Close()
		// lots in the fab at the end of each bucket
		rows, err = s.DB.Query(c, `SELECT b.i, count(l.*) FROM generate_series(0, $3::int - 1) b(i)
			LEFT JOIN prod.lots l ON l.released_at < $1::timestamptz + (b.i + 1) * $2::interval
				AND (l.completed_at IS NULL OR l.completed_at >= $1::timestamptz + (b.i + 1) * $2::interval)
			WHERE $1::timestamptz + (b.i + 1) * $2::interval <= now() + $2::interval GROUP BY b.i`, from, step, n)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var i int
			var w int64
			if err := rows.Scan(&i, &w); err != nil {
				rows.Close()
				return nil, err
			}
			if i >= 0 && i < n {
				wip[i] = w
			}
		}
		rows.Close()
	}
	points := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		b0 := from.Add(time.Duration(i) * step)
		if b0.After(time.Now()) {
			break
		}
		tot := &oeeAcc{}
		for _, id := range ids {
			a := accs[i][id]
			a.N = math.Max(0, a.T-a.P-a.SB-a.U-a.S)
			tot.add(a)
		}
		f := tot.factors()
		points = append(points, map[string]any{"ts": httpx.TS(b0), "oee": f["oee"], "availability": f["availability"], "operational": f["operational"],
			"rate": f["rate"], "quality": f["quality"], "wafer_outs": outs[i], "wafer_starts": starts[i], "wafer_moves": tot.waferMoves,
			"cycle_time_days": cts[i], "wip_lots": wip[i]})
	}
	return map[string]any{"bucket": bucketName, "points": points}, nil
}

// ---------------------------------------------------------------------------
// Timeline

func (s *Server) prodTimeline(c *Ctx) (any, error) {
	all, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	tools := all
	if want, ok := c.F.IDs("equipment_ids"); ok && len(want) > 0 {
		tools = nil
		for _, t := range all {
			for _, id := range want {
				if t.ID == id {
					tools = append(tools, t)
				}
			}
		}
	}
	from, to, _ := prodWindow(c, 24, 168)
	ids := toolIDs(tools)
	type interval struct {
		Status string `json:"status"`
		Start  string `json:"start"`
		End    string `json:"end"`
	}
	type run struct {
		LotCode  string `json:"lot_code"`
		StepCode string `json:"step_code"`
		Start    string `json:"start"`
		End      any    `json:"end"`
		Wafers   int64  `json:"wafers"`
		Outcome  any    `json:"outcome"`
		batch    string
	}
	intervals := map[int64][]interval{}
	runs := map[int64][]*run{}
	if len(ids) > 0 {
		rows, err := s.DB.Query(c, `SELECT asset_id, status_id, GREATEST(start_ts, $1), LEAST(COALESCE(end_ts, now()), $2) FROM ts.asset_status
			WHERE asset_id = ANY($3) AND start_ts < $2 AND COALESCE(end_ts, now()) > $1 ORDER BY asset_id, start_ts`, from, to, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var st int
			var a, b time.Time
			if err := rows.Scan(&id, &st, &a, &b); err != nil {
				rows.Close()
				return nil, err
			}
			if b.After(a) {
				intervals[id] = append(intervals[id], interval{Status: engine.StatusName(st), Start: httpx.TS(a), End: httpx.TS(b)})
			}
		}
		rows.Close()
		rows, err = s.DB.Query(c, `SELECT equipment_id, lot_code, step_code, started_at, ended_at, wafers_in, COALESCE(batch_id, ''), outcome
			FROM prod.lot_steps WHERE equipment_id = ANY($1) AND started_at IS NOT NULL AND started_at < $3 AND COALESCE(ended_at, now()) > $2
			ORDER BY equipment_id, started_at, lot_code`, ids, from, to)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, wafers int64
			var lot, step, batch string
			var start time.Time
			var end *time.Time
			var outcome *string
			if err := rows.Scan(&id, &lot, &step, &start, &end, &wafers, &batch, &outcome); err != nil {
				rows.Close()
				return nil, err
			}
			list := runs[id]
			if batch != "" && len(list) > 0 && list[len(list)-1].batch == batch {
				last := list[len(list)-1]
				last.LotCode += " + " + lot
				last.Wafers += wafers
				continue
			}
			r := &run{LotCode: lot, StepCode: step, Start: httpx.TS(start), End: httpx.TSPtr(end), Wafers: wafers, batch: batch}
			if outcome != nil {
				r.Outcome = *outcome
			}
			runs[id] = append(list, r)
		}
		rows.Close()
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		iv := intervals[t.ID]
		if iv == nil {
			iv = []interval{}
		}
		rs := runs[t.ID]
		if rs == nil {
			rs = []*run{}
		}
		out = append(out, map[string]any{"id": t.ID, "alias": t.Alias, "name": t.Name, "tool_type": t.Type, "intervals": iv, "runs": rs})
	}
	return map[string]any{"from": httpx.TS(from), "to": httpx.TS(to), "tools": out}, nil
}

// ---------------------------------------------------------------------------
// Lots

const lotSelect = `SELECT l.lot_code, l.product_code, l.priority, l.wafers_start, l.wafers, l.status, l.step_code,
	CASE WHEN l.step_code LIKE 'RW-%' THEN 'Rework (' || l.step_code || ')' ELSE rs.operation END, e.alias, rs.layer, ` + prodDetourArea + `,
	l.current_seq, COALESCE(p.n, 0), l.released_at, l.completed_at, l.hold_reason, l.rework_count, l.due_at, pr.cycle_days, l.location
	FROM prod.lots l LEFT JOIN prod.route_steps rs ON rs.product_code = l.product_code AND rs.seq = l.current_seq
	LEFT JOIN asset.equipment e ON e.id = l.equipment_id
	LEFT JOIN prod.products pr ON pr.code = l.product_code
	LEFT JOIN (SELECT product_code, count(*) AS n FROM prod.route_steps GROUP BY product_code) p ON p.product_code = l.product_code`

type rowScanner interface{ Scan(dest ...any) error }

func scanLot(r rowScanner) (map[string]any, error) {
	var code, product, status string
	var prio, start, wafers, seq, n, rework int64
	var step, op, alias, hold, layer, area, location *string
	var released time.Time
	var completed, due *time.Time
	var cycleDays *float64
	if err := r.Scan(&code, &product, &prio, &start, &wafers, &status, &step, &op, &alias, &layer, &area, &seq, &n, &released, &completed, &hold,
		&rework, &due, &cycleDays, &location); err != nil {
		return nil, err
	}
	progress := 0.0
	if status == "DONE" {
		progress = 100
	} else if n > 0 {
		progress = 100 * float64(seq-1) / float64(n)
	}
	endT := time.Now()
	if completed != nil {
		endT = *completed
	}
	var late any
	if due != nil {
		late = endT.After(*due)
	}
	row := map[string]any{"lot_code": code, "product_code": product, "priority": prio, "wafers_start": start, "wafers": wafers, "status": status,
		"step_code": step, "step_name": op, "equipment_alias": alias, "layer": layer, "area": area, "seq": seq, "steps": n,
		"progress_pct": round1(progress), "released_at": httpx.TS(released), "completed_at": httpx.TSPtr(completed), "due_at": httpx.TSPtr(due),
		"cycle_time_h": round2(endT.Sub(released).Hours()), "cycle_time_days": round2(endT.Sub(released).Hours() / 24), "hold_reason": hold,
		"rework_count": rework, "late": late, "location": location}
	return row, nil
}

// prodVisible reports whether the user sees the fab (at least one tool).
func (s *Server) prodVisible(c *Ctx) (bool, error) {
	tools, err := s.prodTools(c)
	return len(tools) > 0, err
}

func (s *Server) prodLots(c *Ctx) (any, error) {
	ok, err := s.prodVisible(c)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"count": 0, "data": []any{}}, nil
	}
	args := sqlArgs{}
	var where []string
	order := "l.priority, l.released_at"
	switch strings.ToLower(c.F.StrTrim("status")) {
	case "done":
		where = append(where, "l.status IN ('DONE', 'SCRAPPED')")
		order = "l.completed_at DESC"
	case "hold":
		where = append(where, "l.status = 'HOLD'")
	case "all":
		order = "l.released_at DESC"
	default:
		where = append(where, "l.status IN ('QUEUED', 'PROCESSING', 'AWAY', 'HOLD')")
	}
	if q := c.F.StrTrim("search"); q != "" {
		where = append(where, "l.lot_code ILIKE "+args.add(ilike(q)))
	}
	if p := c.F.StrTrim("product_code"); p != "" {
		where = append(where, "l.product_code = "+args.add(p))
	}
	if a := strings.ToUpper(c.F.StrTrim("area")); a != "" {
		where = append(where, prodDetourArea+" = "+args.add(a))
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	var count int64
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM prod.lots l LEFT JOIN prod.route_steps rs ON rs.product_code = l.product_code
		AND rs.seq = l.current_seq`+cond, args...).Scan(&count); err != nil {
		return nil, err
	}
	limit := c.limit(50, 200)
	offset := (c.page(1) - 1) * limit
	rows, err := s.DB.Query(c, lotSelect+cond+" ORDER BY "+order+", l.lot_code LIMIT "+args.add(limit)+" OFFSET "+args.add(offset), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		row, err := scanLot(rows)
		if err != nil {
			return nil, err
		}
		data = append(data, row)
	}
	return map[string]any{"count": count, "data": data}, rows.Err()
}

func (s *Server) prodLot(c *Ctx) (any, error) {
	ok, err := s.prodVisible(c)
	if err != nil {
		return nil, err
	}
	code := c.F.StrTrim("lot_code")
	if !ok || code == "" {
		return nil, httpx.NotFound("err_lot_not_found")
	}
	lot, err := scanLot(s.DB.QueryRow(c, lotSelect+" WHERE l.lot_code = $1", code))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("err_lot_not_found")
		}
		return nil, err
	}
	steps := []map[string]any{}
	rows, err := s.DB.Query(c, `SELECT ls.id, ls.seq, ls.step_code, ls.operation, e.alias, ls.chambers, ls.recipe, ls.status, ls.queued_at, ls.started_at,
			ls.ended_at, ls.wafers_in, ls.wafers_out, ls.scrapped, ls.reworked, ls.theoretical_s, ls.actual_s, ls.outcome, ls.batch_id, ls.is_rework
		FROM prod.lot_steps ls LEFT JOIN asset.equipment e ON e.id = ls.equipment_id WHERE ls.lot_code = $1 ORDER BY ls.id`, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, seq, in, scr, rew int64
		var step, op, recipe, status string
		var alias, chambers, outcome, batch *string
		var queued time.Time
		var started, ended *time.Time
		var out *int64
		var theo, actual *float64
		var isRework bool
		if err := rows.Scan(&id, &seq, &step, &op, &alias, &chambers, &recipe, &status, &queued, &started, &ended, &in, &out, &scr, &rew,
			&theo, &actual, &outcome, &batch, &isRework); err != nil {
			rows.Close()
			return nil, err
		}
		var queueMin any
		if started != nil {
			queueMin = round1(started.Sub(queued).Minutes())
		}
		steps = append(steps, map[string]any{"id": id, "seq": seq, "step_code": step, "operation": op, "equipment_alias": alias,
			"chambers": chambers, "recipe": recipe, "status": status, "queued_at": httpx.TS(queued), "started_at": httpx.TSPtr(started),
			"ended_at": httpx.TSPtr(ended), "wafers_in": in, "wafers_out": out, "scrapped": scr, "reworked": rew, "theoretical_s": theo,
			"actual_s": actual, "outcome": outcome, "batch_id": batch, "is_rework": isRework, "queue_min": queueMin})
	}
	rows.Close()
	metrology := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT m.measured_at, m.param, m.char_key, m.unit, m.wafer, m.value, m.wiw_range, m.target, m.lsl, m.usl, m.pass,
			me.alias, se.alias, m.source_chamber, m.source_step_code
		FROM prod.metrology m LEFT JOIN asset.equipment me ON me.id = m.metrology_equipment_id
		LEFT JOIN asset.equipment se ON se.id = m.source_equipment_id WHERE m.lot_code = $1 ORDER BY m.measured_at, m.param, m.wafer`, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at time.Time
		var param string
		var key, unit *string
		var wafer int64
		var value float64
		var rng, target, lsl, usl *float64
		var pass bool
		var met, src, ch, srcStep *string
		if err := rows.Scan(&at, &param, &key, &unit, &wafer, &value, &rng, &target, &lsl, &usl, &pass, &met, &src, &ch, &srcStep); err != nil {
			rows.Close()
			return nil, err
		}
		metrology = append(metrology, map[string]any{"measured_at": httpx.TS(at), "param": param, "char_key": key, "unit": unit, "wafer": wafer,
			"value": value, "wiw_range": rng, "target": target, "lsl": lsl, "usl": usl, "pass": pass, "metrology_alias": met, "source_alias": src,
			"source_chamber": ch, "source_step_code": srcStep})
	}
	rows.Close()
	scrap := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT sc.at, sc.wafers, sc.reason, sc.step_code, e.alias, sc.chamber FROM prod.scrap sc
		LEFT JOIN asset.equipment e ON e.id = sc.equipment_id WHERE sc.lot_code = $1 ORDER BY sc.at`, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at time.Time
		var wafers int64
		var reason string
		var step, alias, ch *string
		if err := rows.Scan(&at, &wafers, &reason, &step, &alias, &ch); err != nil {
			rows.Close()
			return nil, err
		}
		scrap = append(scrap, map[string]any{"at": httpx.TS(at), "wafers": wafers, "reason": reason, "step_code": step, "equipment_alias": alias,
			"chamber": ch})
	}
	rows.Close()
	route := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT seq, step_code, operation, tool_type, layer, area, module, mask, meas_key, qtime_h
		FROM prod.route_steps WHERE product_code = $1 ORDER BY seq`, lot["product_code"])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var step, op, typ, layer string
		var area, module, mask, meas *string
		var qtime *float64
		if err := rows.Scan(&seq, &step, &op, &typ, &layer, &area, &module, &mask, &meas, &qtime); err != nil {
			return nil, err
		}
		route = append(route, map[string]any{"seq": seq, "step_code": step, "operation": op, "tool_type": typ, "layer": layer, "area": area,
			"module": module, "mask": mask, "meas_key": meas, "qtime_h": qtime})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// the FOUP's overhead transfers, newest first
	transfers := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT kind, from_loc, to_loc, vehicle, requested_at, picked_at, delivered_at, distance_m
		FROM prod.transfers WHERE lot_code = $1 ORDER BY requested_at DESC, id DESC LIMIT 50`, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, from, to, vehicle string
		var rq, picked, delivered time.Time
		var dist *float64
		if err := rows.Scan(&kind, &from, &to, &vehicle, &rq, &picked, &delivered, &dist); err != nil {
			return nil, err
		}
		transfers = append(transfers, map[string]any{"kind": kind, "from": from, "to": to, "vehicle": vehicle, "requested_at": httpx.TS(rq),
			"picked_at": httpx.TS(picked), "delivered_at": httpx.TS(delivered), "minutes": round2(delivered.Sub(rq).Minutes()), "distance_m": dist})
	}
	return map[string]any{"lot": lot, "steps": steps, "metrology": metrology, "scrap": scrap, "route": route, "transfers": transfers}, rows.Err()
}

// ---------------------------------------------------------------------------
// Quality

// prodQualityKey identifies a characteristic: a key (POLY.CDE), DEFECTS
// (defect adders) or WAT.
func prodQualityCond(key string, args *sqlArgs) string {
	if strings.EqualFold(key, "DEFECTS") {
		return "m.param = 'defects'"
	}
	return "m.char_key = " + args.add(key)
}

// prodQualityKeys lists the characteristics measured in the window with their
// specification, sample count, fails and Cpk (most failing first). Input:
// hours (168, at most 720).
func (s *Server) prodQualityKeys(c *Ctx) (any, error) {
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if len(tools) == 0 {
		return out, nil
	}
	from, to, _ := prodWindow(c, 168, 720)
	rows, err := s.DB.Query(c, `SELECT COALESCE(m.char_key, 'DEFECTS'), min(m.param), min(m.unit), avg(m.target), avg(m.lsl), avg(m.usl), count(*),
			count(*) FILTER (WHERE NOT m.pass), avg(m.value), stddev_samp(m.value),
			(SELECT min(rs.spec_name) FROM prod.route_steps rs WHERE rs.meas_key = m.char_key OR (m.char_key IS NULL AND rs.param = 'defects')),
			(SELECT string_agg(DISTINCT rs.step_code, ', ') FROM prod.route_steps rs WHERE rs.meas_key = m.char_key),
			(SELECT min(rs.seq) FROM prod.route_steps rs WHERE rs.meas_key = m.char_key OR (m.char_key IS NULL AND rs.param = 'defects'))
		FROM prod.metrology m WHERE m.measured_at >= $1 AND m.measured_at < $2 GROUP BY 1 ORDER BY 13, 1`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, param string
		var unit, name, steps *string
		var target, lsl, usl, mean, sd *float64
		var n, fails int64
		var first *int64
		if err := rows.Scan(&key, &param, &unit, &target, &lsl, &usl, &n, &fails, &mean, &sd, &name, &steps, &first); err != nil {
			return nil, err
		}
		if key == "DEFECTS" {
			name = strPtr("Defect adders")
		}
		out = append(out, map[string]any{"char_key": key, "param": param, "name": name, "unit": unit, "target": target, "lsl": lsl, "usl": usl,
			"n": n, "fails": fails, "mean": roundPtr(mean, 3), "cpk": cpkOf(mean, sd, lsl, usl), "steps": steps})
	}
	return out, rows.Err()
}

func roundPtr(p *float64, d int) any {
	if p == nil {
		return nil
	}
	return httpx.Round(*p, d)
}

// cpkOf is the process capability index of a sample against its limits.
func cpkOf(mean, sd, lsl, usl *float64) any {
	if mean == nil || sd == nil || *sd <= 0 {
		return nil
	}
	k := math.Inf(1)
	if usl != nil {
		k = math.Min(k, (*usl-*mean)/(3**sd))
	}
	if lsl != nil {
		k = math.Min(k, (*mean-*lsl)/(3**sd))
	}
	if math.IsInf(k, 0) {
		return nil
	}
	return httpx.Round(k, 2)
}

// prodQuality returns the measurements of one characteristic: points, SPC
// limits (an individuals chart; a c chart for defect counts), the result by
// source tool and chamber with Cpk, the scrap Pareto, rework and the first
// pass yield of its measurement steps. Input: char_key (default the first
// measured), hours, equipment_id? (source tool).
func (s *Server) prodQuality(c *Ctx) (any, error) {
	tools, err := s.prodTools(c)
	if err != nil {
		return nil, err
	}
	from, to, _ := prodWindow(c, 168, 720)
	key := c.F.StrTrim("char_key")
	if key == "" {
		key = c.F.StrTrim("param") // older clients: a parameter name
	}
	var eq *int64
	if id := c.F.IntPtr("equipment_id"); id != nil {
		found := false
		for _, t := range tools {
			found = found || t.ID == *id
		}
		if !found {
			return nil, httpx.NotFound("err_asset_not_found")
		}
		eq = id
	}
	res := map[string]any{"char_key": key, "name": nil, "param": nil, "unit": nil, "target": nil, "lsl": nil, "usl": nil, "points": []any{},
		"spc": nil, "by_chamber": []any{}, "scrap_pareto": []any{}, "rework_wafers": 0, "first_pass_yield": nil}
	if len(tools) == 0 {
		return res, nil
	}
	if key == "" {
		if err := s.DB.QueryRow(c, `SELECT COALESCE(m.char_key, 'DEFECTS') FROM prod.metrology m LEFT JOIN prod.route_steps rs ON rs.meas_key = m.char_key
			WHERE m.measured_at >= $1 AND m.measured_at < $2 ORDER BY rs.seq NULLS LAST LIMIT 1`, from, to).Scan(&key); err != nil && !db.IsNoRows(err) {
			return nil, err
		}
		res["char_key"] = key
	}
	if key == "" {
		return res, nil
	}
	args := sqlArgs{from, to}
	cond := ` WHERE m.measured_at >= $1 AND m.measured_at < $2 AND ` + prodQualityCond(key, &args)
	if eq != nil {
		cond += " AND m.source_equipment_id = " + args.add(*eq)
	}
	var param, unit *string
	var target, lsl, usl *float64
	if err := s.DB.QueryRow(c, `SELECT m.param, m.unit, m.target, m.lsl, m.usl FROM prod.metrology m`+cond+` ORDER BY m.measured_at DESC LIMIT 1`,
		args...).Scan(&param, &unit, &target, &lsl, &usl); err != nil && !db.IsNoRows(err) {
		return nil, err
	}
	res["param"], res["unit"], res["target"], res["lsl"], res["usl"] = param, unit, target, lsl, usl
	var name *string
	if strings.EqualFold(key, "DEFECTS") {
		name = strPtr("Defect adders")
	} else if err := s.DB.QueryRow(c, `SELECT min(spec_name) FROM prod.route_steps WHERE meas_key = $1`, key).Scan(&name); err != nil {
		return nil, err
	}
	res["name"] = name
	rows, err := s.DB.Query(c, `SELECT m.measured_at, m.lot_code, m.wafer, m.value, se.alias, m.source_chamber, m.pass, me.alias
		FROM prod.metrology m LEFT JOIN asset.equipment se ON se.id = m.source_equipment_id
		LEFT JOIN asset.equipment me ON me.id = m.metrology_equipment_id`+cond+` ORDER BY m.measured_at DESC, m.id DESC LIMIT 4000`, args...)
	if err != nil {
		return nil, err
	}
	points := []map[string]any{}
	for rows.Next() {
		var at time.Time
		var lot string
		var wafer int64
		var value float64
		var src, ch, met *string
		var pass bool
		if err := rows.Scan(&at, &lot, &wafer, &value, &src, &ch, &pass, &met); err != nil {
			rows.Close()
			return nil, err
		}
		points = append(points, map[string]any{"ts": httpx.TS(at), "lot_code": lot, "wafer": wafer, "value": value, "source_alias": src,
			"source_chamber": ch, "pass": pass, "metrology_alias": met})
	}
	rows.Close()
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
	res["points"] = points
	var n int64
	var mean, sd *float64
	if err := s.DB.QueryRow(c, `SELECT count(*), avg(m.value), stddev_samp(m.value) FROM prod.metrology m`+cond, args...).Scan(&n, &mean, &sd); err != nil {
		return nil, err
	}
	if n > 1 && mean != nil && sd != nil {
		cl, sigma := *mean, *sd
		if param != nil && *param == "defects" {
			sigma = math.Sqrt(math.Max(cl, 0)) // c chart (Poisson counts)
		}
		res["spc"] = map[string]any{"cl": httpx.Round(cl, 4), "ucl": httpx.Round(cl+3*sigma, 4), "lcl": httpx.Round(cl-3*sigma, 4),
			"sigma": httpx.Round(sigma, 5), "n": n, "cpk": cpkOf(mean, sd, lsl, usl)}
		if param != nil && (*param == "defects" || *param == "wat") {
			res["spc"].(map[string]any)["lcl"] = httpx.Round(math.Max(0, cl-3*sigma), 4)
		}
	}
	byCh := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT se.alias, COALESCE(m.source_chamber, ''), count(*), avg(m.value), stddev_samp(m.value), count(*) FILTER (WHERE NOT m.pass)
		FROM prod.metrology m LEFT JOIN asset.equipment se ON se.id = m.source_equipment_id`+cond+` GROUP BY 1, 2 ORDER BY 1, 2`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var alias *string
		var ch string
		var cnt, fails int64
		var mu float64
		var sdv *float64
		if err := rows.Scan(&alias, &ch, &cnt, &mu, &sdv, &fails); err != nil {
			rows.Close()
			return nil, err
		}
		var sdOut any
		if sdv != nil && *sdv > 0 && cnt > 1 {
			sdOut = httpx.Round(*sdv, 4)
		}
		byCh = append(byCh, map[string]any{"source_alias": alias, "chamber": strOrNilStr(ch), "n": cnt, "mean": httpx.Round(mu, 4), "sd": sdOut,
			"cpk": cpkOf(&mu, sdv, lsl, usl), "fails": fails})
	}
	rows.Close()
	res["by_chamber"] = byCh
	scrapArgs := sqlArgs{from, to}
	scrapCond := ` WHERE at >= $1 AND at < $2`
	if eq != nil {
		scrapCond += " AND equipment_id = " + scrapArgs.add(*eq)
	}
	pareto := []map[string]any{}
	rows, err = s.DB.Query(c, `SELECT reason, sum(wafers) FROM prod.scrap`+scrapCond+` GROUP BY reason ORDER BY 2 DESC, 1`, scrapArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var reason string
		var w int64
		if err := rows.Scan(&reason, &w); err != nil {
			rows.Close()
			return nil, err
		}
		pareto = append(pareto, map[string]any{"reason": reason, "wafers": w})
	}
	rows.Close()
	res["scrap_pareto"] = pareto
	reworkArgs := sqlArgs{from, to}
	reworkCond := ` WHERE ended_at >= $1 AND ended_at < $2`
	if eq != nil {
		reworkCond += " AND equipment_id = " + reworkArgs.add(*eq)
	}
	var rework int64
	if err := s.DB.QueryRow(c, `SELECT COALESCE(sum(reworked), 0) FROM prod.lot_steps`+reworkCond, reworkArgs...).Scan(&rework); err != nil {
		return nil, err
	}
	res["rework_wafers"] = rework
	// first pass yield of the characteristic's measurement steps
	fpyArgs := sqlArgs{from, to}
	stepCond := `step_code IN (SELECT step_code FROM prod.route_steps WHERE meas_key = ` + fpyArgs.add(key) + `)`
	if strings.EqualFold(key, "DEFECTS") {
		stepCond = `step_code IN (SELECT step_code FROM prod.route_steps WHERE param = 'defects')`
	}
	fpyCond := ` WHERE ` + stepCond + ` AND status = 'DONE' AND outcome IS DISTINCT FROM 'ABORTED' AND NOT is_rework AND ended_at >= $1 AND ended_at < $2`
	if eq != nil {
		fpyCond += " AND id IN (SELECT lot_step_id FROM prod.metrology WHERE source_equipment_id = " + fpyArgs.add(*eq) + ")"
	}
	var passN, totalN int64
	if err := s.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE outcome = 'PASS'), count(*) FROM prod.lot_steps`+fpyCond, fpyArgs...).
		Scan(&passN, &totalN); err != nil {
		return nil, err
	}
	if totalN > 0 {
		res["first_pass_yield"] = httpx.Round(100*float64(passN)/float64(totalN), 2)
	}
	return res, nil
}

func strOrNilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// Location Overview

// toolPerfQuality returns the rate and quality efficiency (percent) of the
// production tools among assets over [from, to) (nil without production runs).
func (s *Server) toolPerfQuality(ctx context.Context, assets []int64, from, to time.Time) (perf, quality *float64) {
	if len(assets) == 0 {
		return nil, nil
	}
	var runs, in, scr, rew int64
	var theo, actual float64
	err := s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE theoretical_s IS NOT NULL), COALESCE(sum(theoretical_s), 0),
			COALESCE(sum(actual_s) FILTER (WHERE theoretical_s IS NOT NULL), 0), COALESCE(sum(wafers_in) FILTER (WHERE theoretical_s IS NOT NULL), 0),
			COALESCE(sum(scrapped), 0), COALESCE(sum(reworked), 0)
		FROM prod.lot_steps WHERE equipment_id = ANY($1) AND status = 'DONE' AND ended_at >= $2 AND ended_at < $3`, assets, from, to).
		Scan(&runs, &theo, &actual, &in, &scr, &rew)
	if err != nil || runs == 0 {
		return nil, nil
	}
	if actual > 0 {
		p := round2(math.Min(100, 100*theo/actual))
		perf = &p
	}
	if in > 0 {
		q := round2(math.Max(0, 100*float64(in-scr-rew)/float64(in)))
		quality = &q
	}
	return perf, quality
}
