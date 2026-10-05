package api

import (
	"strings"
	"time"

	"afdd/internal/httpx"
)

// toolActivityHours is how far back the finished runs of a tool reach.
const toolActivityHours = 24

// twinToolActivity is what a FAB1 process tool is doing, for its 3D dashboard:
// the runs in process (lot, step, recipe, chambers, wafers, start and the
// expected run time: the median of the tool's last runs of that step, else the
// route's ideal times) and the runs that ended in the last 24 hours, newest
// first. It needs the digitaltwin "read" permission and the tool's location;
// lot codes and products are left out without the production "read"
// permission. Equipment that runs no lots answers tool false.
func (s *Server) twinToolActivity(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	var typ string
	if err := s.DB.QueryRow(c, `SELECT t.name FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id
		JOIN asset.types t ON t.id = m.type_id WHERE e.id = $1`, id).Scan(&typ); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	lots := c.User.Can("production", "read")
	current, recent := []map[string]any{}, []map[string]any{}
	res := map[string]any{"now": httpx.TS(now), "tool": prodToolTypes[typ] != "", "lots_visible": lots,
		"hours": toolActivityHours, "current": current, "recent": recent}
	if prodToolTypes[typ] == "" {
		return res, nil
	}
	lot := func(code, product string) (any, any) {
		if !lots {
			return nil, nil
		}
		return code, product
	}

	rows, err := s.DB.Query(c, `SELECT ls.lot_code, l.product_code, ls.seq, ls.step_code, ls.operation, ls.recipe, ls.chambers, ls.batch_id,
			ls.wafers_in, ls.queued_at, ls.started_at, ls.is_rework, rs.area, rs.sample_wafers, rs.ideal_wafer_s, rs.ideal_lot_s,
			(SELECT count(*) FROM prod.route_steps r2 WHERE r2.product_code = l.product_code),
			(SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY d.s) FROM (
				SELECT extract(epoch FROM p.ended_at - p.started_at) AS s FROM prod.lot_steps p
				WHERE p.equipment_id = ls.equipment_id AND p.step_code = ls.step_code AND p.status = 'DONE' AND p.started_at IS NOT NULL
				ORDER BY p.ended_at DESC LIMIT 10) d)
		FROM prod.lot_steps ls JOIN prod.lots l ON l.lot_code = ls.lot_code
		LEFT JOIN prod.route_steps rs ON rs.product_code = l.product_code AND rs.seq = ls.seq
		WHERE ls.equipment_id = $1 AND ls.status = 'PROCESSING' ORDER BY ls.started_at, ls.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code, product, step, op, recipe string
		var seq, in, steps int64
		var chambers, batch, area *string
		var queued time.Time
		var started *time.Time
		var rework bool
		var sample *int64
		var waferS, lotS, typical *float64
		if err := rows.Scan(&code, &product, &seq, &step, &op, &recipe, &chambers, &batch, &in, &queued, &started, &rework,
			&area, &sample, &waferS, &lotS, &steps, &typical); err != nil {
			return nil, err
		}
		lc, pc := lot(code, product)
		measured := in
		if area != nil && *area == "MET" && sample != nil && *sample > 0 {
			measured = min(in, *sample)
		}
		current = append(current, map[string]any{"lot_code": lc, "product_code": pc, "seq": seq, "route_steps": steps,
			"step_code": step, "operation": op, "recipe": recipe, "chambers": chambers, "batch_id": batch, "area": area,
			"wafers": in, "wafers_processed": measured, "queued_at": httpx.TS(queued), "started_at": httpx.TSPtr(started),
			"expected_s": roundPtr(expectedRunS(typical, waferS, lotS, measured, chambers, batch != nil), 0), "is_rework": rework})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	rows, err = s.DB.Query(c, `SELECT ls.lot_code, l.product_code, ls.seq, ls.step_code, ls.operation, ls.recipe, ls.chambers, ls.batch_id,
			ls.wafers_in, ls.wafers_out, ls.scrapped, ls.reworked, ls.started_at, ls.ended_at, ls.theoretical_s, ls.outcome, ls.is_rework
		FROM prod.lot_steps ls JOIN prod.lots l ON l.lot_code = ls.lot_code
		WHERE ls.equipment_id = $1 AND ls.status = 'DONE' AND ls.ended_at >= $2 AND ls.ended_at <= $3
		ORDER BY ls.ended_at DESC, ls.id DESC LIMIT 500`, id, now.Add(-toolActivityHours*time.Hour), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code, product, step, op, recipe string
		var seq, in, scrapped, reworked int64
		var out *int64
		var chambers, batch, outcome *string
		var started *time.Time
		var ended time.Time
		var theo *float64
		var rework bool
		if err := rows.Scan(&code, &product, &seq, &step, &op, &recipe, &chambers, &batch, &in, &out, &scrapped, &reworked,
			&started, &ended, &theo, &outcome, &rework); err != nil {
			return nil, err
		}
		lc, pc := lot(code, product)
		recent = append(recent, map[string]any{"lot_code": lc, "product_code": pc, "seq": seq, "step_code": step, "operation": op,
			"recipe": recipe, "chambers": chambers, "batch_id": batch, "wafers_in": in, "wafers_out": out, "scrapped": scrapped,
			"reworked": reworked, "started_at": httpx.TSPtr(started), "ended_at": httpx.TS(ended), "theoretical_s": roundPtr(theo, 0),
			"outcome": outcome, "is_rework": rework})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res["current"], res["recent"] = current, recent
	return res, nil
}

// expectedRunS is how long a run in process takes: the tool's typical time for
// the step when it has run it lately, else the route's ideal time (cmd/simulator
// Fab.start: a batch takes its lot time, other tools the wafer time per chamber
// in use plus the lot overhead).
func expectedRunS(typical, waferS, lotS *float64, wafers int64, chambers *string, batch bool) *float64 {
	if typical != nil && *typical > 0 {
		return typical
	}
	if waferS == nil || lotS == nil {
		return nil
	}
	if batch {
		return lotS
	}
	n := 1
	if chambers != nil && *chambers != "" {
		n = len(strings.Split(*chambers, ","))
	}
	v := float64(wafers)**waferS/float64(n) + *lotS
	return &v
}
