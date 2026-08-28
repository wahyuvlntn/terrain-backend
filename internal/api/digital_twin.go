package api

import (
	"math"
	"sort"
	"strings"
	"time"

	"afdd/internal/datahub"
	"afdd/internal/engine"
	"afdd/internal/httpx"
	"afdd/internal/maint"
)

// Digital twin read models for the 3D twin page: equipment bound to 3D models by
// alias, live parameter values bound to 3D instruments, and rolling trends with an
// individuals SPC chart (configured limits, or limits computed from the window).
// Every handler needs the digitaltwin "read" permission (plus the location scope).
func (s *Server) registerDigitalTwin() {
	s.handle("digital_twin/getassets", AccessUser, s.twinAssets)
	s.handle("digital_twin/getassetparameters", AccessUser, s.twinAssetParameters)
	s.handle("digital_twin/getparametertrend", AccessUser, s.twinParameterTrend)
	s.handle("digital_twin/getassetsummary", AccessUser, s.twinAssetSummary)
	s.handle("digital_twin/getliveoverview", AccessUser, s.twinLiveOverview)
}

// twinAssets lists every visible equipment with its alias and current state.
func (s *Server) twinAssets(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	ids, err := s.resolveAssets(c, c.assetFilter())
	if err != nil {
		return nil, err
	}
	infos, err := s.assetInfos(c, ids)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, ids)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		a := infos[id]
		if a == nil {
			continue
		}
		st := states[id]
		row := map[string]any{
			"id": id, "alias": a.Alias, "name": a.Name, "type_name": a.TypeName, "model_name": a.ModelName,
			"location_id": a.LocationID, "status": statusName(st), "condition": conditionName(st),
			"status_start_time": nil, "condition_start_time": nil,
		}
		if st != nil {
			row["status_start_time"] = httpx.TSPtr(st.StatusSince)
			row["condition_start_time"] = httpx.TSPtr(st.ConditionSince)
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i]["name"].(string)) < strings.ToLower(out[j]["name"].(string))
	})
	return out, nil
}

// twinAssetParameters returns every displayed parameter of an equipment with its
// latest value, condition, thresholds, SPC configuration and, for writable
// parameters (set points and control modes), its control definition.
func (s *Server) twinAssetParameters(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT ep.id FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
		WHERE ep.equipment_id = $1 AND ep.is_displayed ORDER BY mp.id`, id)
	if err != nil {
		return nil, err
	}
	var epids []int64
	for rows.Next() {
		var epid int64
		if err := rows.Scan(&epid); err != nil {
			rows.Close()
			return nil, err
		}
		epids = append(epids, epid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	controls, err := s.twinControls(c, id)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(epids))
	for _, epid := range epids {
		p, err := s.loadEP(c, epid)
		if err != nil {
			return nil, err
		}
		value, ts, cond := s.twinLatest(c, p)
		var control any
		if ctl := controls[p.Alias]; ctl != nil {
			control = ctl
		}
		out = append(out, map[string]any{
			"control": control,
			"id":      p.EPID, "alias": p.Alias, "name": p.Name, "unit": p.Unit, "data_type": p.DataType, "scale": p.Scale,
			"value": value, "timestamp": ts, "condition": cond,
			"warning":  twinLimits(p.WarnType, p.WarnLower, p.WarnUpper),
			"critical": twinLimits(p.CritType, p.CritLower, p.CritUpper),
			"spc": map[string]any{"ucl": p.UCL, "lcl": p.LCL, "center_line": p.CenterLine, "sigma": p.Sigma,
				"usl": p.USL, "lsl": p.LSL},
		})
	}
	return out, nil
}

// twinLatest returns the live value from the data hub (or the last logged value
// when the tag is stale), its timestamp and the threshold condition.
func (s *Server) twinLatest(c *Ctx, p *epInfo) (any, any, string) {
	if value, ts, cond, ok := s.twinHubValue(p); ok {
		return value, ts, cond
	}
	var num *float64
	var text *string
	var at time.Time
	err := s.DB.QueryRow(c, `SELECT value, value_text, ts FROM ts.parameter_values WHERE equipment_parameter_id = $1 ORDER BY ts DESC LIMIT 1`,
		p.EPID).Scan(&num, &text, &at)
	if err == nil {
		if num != nil {
			return *num, httpx.TS(at), engine.CondOffline
		}
		if text != nil {
			return *text, httpx.TS(at), engine.CondOffline
		}
	}
	return nil, nil, engine.CondOffline
}

// twinHubValue returns the live value of a parameter from the data hub, its
// timestamp and threshold condition. ok is false when the hub has no good value
// (no tag, unknown tag, bad quality, gateway inactive or disconnected).
func (s *Server) twinHubValue(p *epInfo) (value any, ts any, cond string, ok bool) {
	if p.Gateway == nil || p.Tag == nil {
		return nil, nil, "", false
	}
	v, found := s.Hub.Value(*p.Gateway, *p.Tag)
	if !found || v.Q != datahub.QualityGood || v.T <= 0 {
		return nil, nil, "", false
	}
	tv := engine.ToValue(v.V, v.T, true, p.DataType, p.Scale)
	at := httpx.TS(time.UnixMilli(v.T))
	if num, isNum := tv.Num(); isNum {
		if p.DataType == "number" {
			return num, at, engine.ParamCondition(p.paramDef(), num), true
		}
		return num, at, engine.CondNormal, true
	}
	return v.V, at, engine.CondNormal, true
}

// twinLiveOverviewMax caps the aliases of one getliveoverview request.
const twinLiveOverviewMax = 60

// twinLiveOverview returns the current state and every displayed parameter of
// several equipment at once, for the BMS plant overview. `aliases` is a
// comma-separated list (or an array), matched case-insensitively; the result
// keeps the requested order and silently leaves out unknown aliases and
// equipment outside the user's location scope. Values and conditions follow
// twinLatest, batched: one query for the equipment with their open status and
// condition, one for their parameters, hub lookups in memory and one query for
// the last logged value of the parameters without a good live value.
func (s *Server) twinLiveOverview(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	list, _ := c.F.StringList("aliases")
	keys := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, a := range list {
		k := strings.ToLower(strings.TrimSpace(a))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) > twinLiveOverviewMax {
		return nil, httpx.BadRequestf("Too many aliases (at most %d)", twinLiveOverviewMax)
	}
	out := []map[string]any{}
	if len(keys) == 0 {
		return out, nil
	}
	scope, err := c.Scope()
	if err != nil {
		return nil, err
	}

	// Equipment with their open status / condition interval (at most one each,
	// enforced by the asset_status_open_uk / asset_condition_open_uk indexes).
	rows, err := s.DB.Query(c, `SELECT e.id, e.alias, e.name, t.name, e.location_id, st.status_id, st.start_ts, cd.condition, cd.start_ts
		FROM asset.equipment e JOIN asset.models m ON m.id = e.model_id JOIN asset.types t ON t.id = m.type_id
		LEFT JOIN ts.asset_status st ON st.asset_id = e.id AND st.end_ts IS NULL
		LEFT JOIN ts.asset_condition cd ON cd.asset_id = e.id AND cd.end_ts IS NULL
		WHERE lower(e.alias) = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	units := map[string]map[string]any{}
	params := map[int64]map[string]any{}
	var ids []int64
	for rows.Next() {
		var id, loc int64
		var alias, name, typeName string
		var status *int
		var cond *string
		st := &currentState{}
		if err := rows.Scan(&id, &alias, &name, &typeName, &loc, &status, &st.StatusSince, &cond, &st.ConditionSince); err != nil {
			rows.Close()
			return nil, err
		}
		if !scope.AllowsLocation(loc) {
			continue
		}
		if status != nil {
			st.Status = *status
		}
		if cond != nil {
			st.Condition = *cond
		}
		params[id] = map[string]any{}
		units[strings.ToLower(alias)] = map[string]any{
			"id": id, "alias": alias, "name": name, "type_name": typeName,
			"status": statusName(st), "status_start_time": httpx.TSPtr(st.StatusSince),
			"condition": conditionName(st), "condition_start_time": httpx.TSPtr(st.ConditionSince),
			"params": params[id],
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) > 0 {
		prows, err := s.DB.Query(c, `SELECT ep.id, ep.equipment_id, mp.alias, mp.name, mp.unit, mp.data_type, mp.scale, g.alias, ep.tag_name,
			ep.warning_lower_limit, ep.warning_upper_limit, ep.warning_threshold_type, ep.critical_lower_limit, ep.critical_upper_limit,
			ep.critical_threshold_type, ep.spc_usl, ep.spc_lsl, ep.spc_ucl, ep.spc_lcl, ep.spc_center_line, ep.spc_sigma
			FROM asset.equipment_parameters ep JOIN asset.model_parameters mp ON mp.id = ep.model_parameter_id
			LEFT JOIN dh.gateways g ON g.id = ep.gateway_id
			WHERE ep.equipment_id = ANY($1) AND ep.is_displayed ORDER BY ep.equipment_id, mp.id`, ids)
		if err != nil {
			return nil, err
		}
		entry := func(p *epInfo, value, ts any, cond string) map[string]any {
			return map[string]any{"value": value, "unit": p.Unit, "scale": p.Scale, "condition": cond, "timestamp": ts}
		}
		// Parameters without a good hub value fall back to their last logged
		// value (condition OFFLINE), fetched below in one query.
		var stale []*epInfo
		for prows.Next() {
			p := &epInfo{}
			if err := prows.Scan(&p.EPID, &p.AssetID, &p.Alias, &p.Name, &p.Unit, &p.DataType, &p.Scale, &p.Gateway, &p.Tag,
				&p.WarnLower, &p.WarnUpper, &p.WarnType, &p.CritLower, &p.CritUpper, &p.CritType, &p.USL, &p.LSL, &p.UCL, &p.LCL,
				&p.CenterLine, &p.Sigma); err != nil {
				prows.Close()
				return nil, err
			}
			if _, dup := params[p.AssetID][p.Alias]; dup {
				continue
			}
			if value, ts, cond, ok := s.twinHubValue(p); ok {
				params[p.AssetID][p.Alias] = entry(p, value, ts, cond)
				continue
			}
			params[p.AssetID][p.Alias] = entry(p, nil, nil, engine.CondOffline)
			stale = append(stale, p)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return nil, err
		}

		if len(stale) > 0 {
			epids := make([]int64, len(stale))
			for i, p := range stale {
				epids[i] = p.EPID
			}
			// One backward index probe per parameter on the (equipment_parameter_id,
			// ts) primary key. A DISTINCT ON over the same parameters reads and sorts
			// their whole history (about 6 s for the 13 plant units).
			lrows, err := s.DB.Query(c, `SELECT x.id, v.value, v.value_text, v.ts FROM unnest($1::bigint[]) AS x(id)
				CROSS JOIN LATERAL (SELECT value, value_text, ts FROM ts.parameter_values
					WHERE equipment_parameter_id = x.id ORDER BY ts DESC LIMIT 1) v`, epids)
			if err != nil {
				return nil, err
			}
			type logged struct {
				num  *float64
				text *string
				at   time.Time
			}
			last := map[int64]logged{}
			for lrows.Next() {
				var epid int64
				var l logged
				if err := lrows.Scan(&epid, &l.num, &l.text, &l.at); err != nil {
					lrows.Close()
					return nil, err
				}
				last[epid] = l
			}
			lrows.Close()
			if err := lrows.Err(); err != nil {
				return nil, err
			}
			for _, p := range stale {
				l, ok := last[p.EPID]
				switch {
				case ok && l.num != nil:
					params[p.AssetID][p.Alias] = entry(p, *l.num, httpx.TS(l.at), engine.CondOffline)
				case ok && l.text != nil:
					params[p.AssetID][p.Alias] = entry(p, *l.text, httpx.TS(l.at), engine.CondOffline)
				}
			}
		}
	}

	for _, k := range keys {
		if u := units[k]; u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// twinParameterTrend returns the last `minutes` of a parameter (bucket averaged),
// its thresholds, statistics and an individuals SPC chart with rule violations.
// Every point carries `running` (inside a RUNNING interval of the equipment); the
// SPC limits, violations and stats use running points only, and
// `stats.excluded` counts the non-null points left out.
func (s *Server) twinParameterTrend(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	p, err := s.loadEP(c, c.F.Int("parameter_id", 0))
	if err != nil {
		return nil, err
	}
	minutes := c.F.Int("minutes", 60)
	if minutes < 5 {
		minutes = 5
	}
	if minutes > 7*24*60 {
		minutes = 7 * 24 * 60
	}
	to := time.Now()
	from := to.Add(-time.Duration(minutes) * time.Minute)
	pts, bucket, err := s.seriesBuckets(c, p.EPID, from, to, 360)
	if err != nil {
		return nil, err
	}
	running, err := s.twinRunningFlags(c, p.AssetID, pts, bucket, from, to)
	if err != nil {
		return nil, err
	}
	return twinTrend(p, minutes, pts, running), nil
}

// twinTrend builds the getparametertrend payload of a series: the points with
// their running flag, thresholds, statistics and the individuals SPC chart.
// It is shared by the live trend and the simulation trend (simulation/gettrend).
func twinTrend(p *epInfo, minutes int64, pts []point, running []bool) map[string]any {
	// SPC describes the process while the unit runs: samples taken while it was
	// stopped (standby, trip, maintenance) are kept in the series but excluded
	// from the limits, the rule checks and the statistics.
	var vals []float64
	excluded := 0
	for i, pt := range pts {
		if pt.val == nil {
			continue
		}
		if running[i] {
			vals = append(vals, *pt.val)
		} else {
			excluded++
		}
	}

	def := p.paramDef()
	cl, sigma, hasLimits := twinCenterSigma(def)
	computed := false
	if !hasLimits && len(vals) >= 8 {
		// Individuals chart over the window. Process signals are autocorrelated
		// (thermal lag), which makes the moving-range estimate understate the
		// spread, so the sample standard deviation is used; the moving range is
		// only the fallback for a perfectly flat window.
		mean, sd, _ := computeStats(twinPhaseOne(vals))
		sg := sd
		if sg <= 0 {
			var mr float64
			for i := 1; i < len(vals); i++ {
				mr += math.Abs(vals[i] - vals[i-1])
			}
			sg = mr / float64(len(vals)-1) / 1.128
		}
		if sg > 0 {
			ucl, lcl := mean+3*sg, mean-3*sg
			def.CenterLine, def.Sigma, def.UCL, def.LCL = &mean, &sg, &ucl, &lcl
			cl, sigma, hasLimits, computed = mean, sg, true, true
		}
	}

	violations := []map[string]any{}
	if hasLimits {
		// vals holds the running values in order, so the trailing window of a
		// point is its last <= 15 running values (stopped samples are skipped).
		i := 0
		for k, pt := range pts {
			if pt.val == nil || !running[k] {
				continue
			}
			start := i - 14
			if start < 0 {
				start = 0
			}
			if rules := engine.SPCViolations(vals[start:i+1], def); len(rules) > 0 {
				violations = append(violations, map[string]any{"timestamp": httpx.TS(pt.ts), "value": *pt.val, "rules": rules})
			}
			i++
		}
	}

	stats := map[string]any{"count": len(vals), "excluded": excluded, "mean": nil, "std_dev": nil, "min": nil, "max": nil, "cp": nil, "cpk": nil}
	if mean, sd, ok := computeStats(vals); ok {
		lo, hi := vals[0], vals[0]
		for _, v := range vals {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		st := spcStats{mean: mean, sd: sd, ok: true, usl: p.USL, lsl: p.LSL}
		stats["mean"], stats["std_dev"], stats["min"], stats["max"] = round2(mean), round2(sd), lo, hi
		if v := st.cp(); v != nil && !math.IsInf(*v, 0) {
			stats["cp"] = round2(*v)
		}
		if v := st.cpk(); v != nil && !math.IsInf(*v, 0) {
			stats["cpk"] = round2(*v)
		}
	}

	spc := map[string]any{"center_line": nil, "sigma": nil, "ucl": def.UCL, "lcl": def.LCL, "usl": p.USL, "lsl": p.LSL, "computed": computed}
	if hasLimits {
		spc["center_line"], spc["sigma"] = cl, sigma
	}
	data := pointsJSON(pts)
	for i := range data {
		data[i]["running"] = running[i]
	}
	return map[string]any{
		"id": p.EPID, "name": p.Name, "unit": p.Unit, "minutes": minutes, "data": data,
		"warning":  twinLimits(p.WarnType, p.WarnLower, p.WarnUpper),
		"critical": twinLimits(p.CritType, p.CritLower, p.CritUpper),
		"spc":      spc, "stats": stats, "violations": violations,
	}
}

// twinPhaseOne is the in-control baseline of a window (Phase I revised limits):
// points beyond 3 sigma of the trial limits are dropped and the limits recomputed
// until none remain, so an excursion such as a chiller trip does not drag the
// centre line and inflate sigma for the rest of the window.
func twinPhaseOne(vals []float64) []float64 {
	base := vals
	for pass := 0; pass < 5; pass++ {
		mean, sd, ok := computeStats(base)
		if !ok || sd <= 0 {
			break
		}
		kept := make([]float64, 0, len(base))
		for _, v := range base {
			if math.Abs(v-mean) <= 3*sd {
				kept = append(kept, v)
			}
		}
		if len(kept) == len(base) || len(kept) < 8 {
			break
		}
		base = kept
	}
	return base
}

// twinRunningFlags marks the points of a series that lie inside a RUNNING status
// interval of the equipment. Interval bounds are compared truncated to the second:
// the engine stamps a status change with the exact time but the values logged in
// the same cycle with the whole second, so a sample of that cycle belongs to the
// new status. A bucket-averaged point (bucket > 0, ts = bucket start) is running
// when its midpoint is and no other status overlaps the bucket, so buckets that
// average a start or stop stay out of SPC. An equipment without any status row
// overlapping the window (no status history) counts as running throughout.
func (s *Server) twinRunningFlags(c *Ctx, assetID int64, pts []point, bucket time.Duration, from, to time.Time) ([]bool, error) {
	rows, err := s.DB.Query(c, `SELECT status_id, start_ts, end_ts FROM ts.asset_status
		WHERE asset_id = $1 AND start_ts < $3 AND (end_ts IS NULL OR end_ts > $2) ORDER BY start_ts`, assetID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var spans []statusSpan
	for rows.Next() {
		var sp statusSpan
		if err := rows.Scan(&sp.status, &sp.start, &sp.end); err != nil {
			return nil, err
		}
		spans = append(spans, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runningFlags(pts, bucket, spans), nil
}

// statusSpan is a status interval of an equipment (end nil = still open).
type statusSpan struct {
	status int
	start  time.Time
	end    *time.Time
}

// runningFlags applies the rules of twinRunningFlags to the status intervals
// overlapping the window (no interval = running throughout).
func runningFlags(pts []point, bucket time.Duration, spans []statusSpan) []bool {
	flags := make([]bool, len(pts))
	type span struct {
		start time.Time
		end   *time.Time // nil = still open
	}
	var runs, others []span
	seen := len(spans) > 0
	for _, st := range spans {
		sp := span{start: st.start.Truncate(time.Second)}
		if st.end != nil {
			end := st.end.Truncate(time.Second)
			sp.end = &end
		}
		if st.status == engine.StatusRunning {
			runs = append(runs, sp)
		} else {
			others = append(others, sp)
		}
	}
	contains := func(r span, t time.Time) bool {
		return !t.Before(r.start) && (r.end == nil || t.Before(*r.end))
	}
	for i, pt := range pts {
		if !seen {
			flags[i] = true
			continue
		}
		t := pt.ts
		if bucket > 0 {
			t = t.Add(bucket / 2)
		}
		for _, r := range runs {
			if contains(r, t) {
				flags[i] = true
				break
			}
		}
		if flags[i] && bucket > 0 {
			b0, b1 := pt.ts, pt.ts.Add(bucket)
			for _, r := range others {
				if r.start.Before(b1) && (r.end == nil || r.end.After(b0)) {
					flags[i] = false
					break
				}
			}
		}
	}
	return flags
}

func twinLimits(typ string, lower, upper *float64) map[string]any {
	out := map[string]any{"lower": nil, "upper": nil}
	if strings.Contains(typ, ">") {
		out["upper"] = upper
	}
	if strings.Contains(typ, "<") {
		out["lower"] = lower
	}
	return out
}

// twinCenterSigma mirrors the engine: explicit centre line and sigma, else
// derived from the control limits.
func twinCenterSigma(p *engine.ParamDef) (float64, float64, bool) {
	if p.CenterLine != nil && p.Sigma != nil && *p.Sigma > 0 {
		return *p.CenterLine, *p.Sigma, true
	}
	if p.UCL != nil && p.LCL != nil && *p.UCL > *p.LCL {
		cl := (*p.UCL + *p.LCL) / 2
		if p.CenterLine != nil {
			cl = *p.CenterLine
		}
		return cl, (*p.UCL - *p.LCL) / 6, true
	}
	return 0, 0, false
}

// twinAssetSummary is the equipment dashboard of the twin overlay: current state,
// status / condition history and uptime over a rolling window (`hours`, default
// 24), 7-day uptime, the most recent alarms and maintenance orders.
func (s *Server) twinAssetSummary(c *Ctx) (any, error) {
	if err := c.Require("digitaltwin", "read"); err != nil {
		return nil, err
	}
	id := c.F.Int("asset_id", 0)
	if err := s.visibleAsset(c, id); err != nil {
		return nil, err
	}
	hours := c.F.Int("hours", 24)
	if hours < 1 {
		hours = 1
	}
	if hours > 24*7 {
		hours = 24 * 7
	}
	now := time.Now().UTC()
	from := now.Add(-time.Duration(hours) * time.Hour)
	infos, err := s.assetInfos(c, []int64{id})
	if err != nil {
		return nil, err
	}
	a := infos[id]
	if a == nil {
		return nil, httpx.NotFound("err_asset_not_found")
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	states, err := s.currentStates(c, []int64{id})
	if err != nil {
		return nil, err
	}
	st := states[id]

	intervals := func(sql string, name func(any) string) ([]interval, error) {
		rows, err := s.DB.Query(c, sql, id, from, now)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []interval{}
		for rows.Next() {
			var v any
			var x, y time.Time
			if err := rows.Scan(&v, &x, &y); err != nil {
				return nil, err
			}
			if y.After(x) {
				out = append(out, interval{Value: name(v), Start: httpx.TS(x), End: httpx.TS(y)})
			}
		}
		return out, rows.Err()
	}
	statuses, err := intervals(`SELECT status_id, GREATEST(start_ts, $2), LEAST(COALESCE(end_ts, $3), $3)
		FROM ts.asset_status WHERE asset_id = $1 AND start_ts < $3 AND COALESCE(end_ts, $3) > $2 ORDER BY start_ts`,
		func(v any) string {
			switch n := v.(type) {
			case int32:
				return engine.StatusName(int(n))
			case int64:
				return engine.StatusName(int(n))
			case int16:
				return engine.StatusName(int(n))
			}
			return ""
		})
	if err != nil {
		return nil, err
	}
	conditions, err := intervals(`SELECT condition, GREATEST(start_ts, $2), LEAST(COALESCE(end_ts, $3), $3)
		FROM ts.asset_condition WHERE asset_id = $1 AND start_ts < $3 AND COALESCE(end_ts, $3) > $2 ORDER BY start_ts`,
		func(v any) string { str, _ := v.(string); return str })
	if err != nil {
		return nil, err
	}

	uptime := func(since time.Time) (any, error) {
		durs, err := s.statusDurations(c, []int64{id}, since, now)
		if err != nil {
			return nil, err
		}
		up := computeUptime(durs[id], elapsed(since, now))
		return map[string]any{"uptime": nz(derefFloat(up.UptimePct)), "running_hours": up.RunningHours, "total_hours": up.TotalHours}, nil
	}
	upWindow, err := uptime(from)
	if err != nil {
		return nil, err
	}
	upWeek, err := uptime(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		return nil, err
	}
	counts, err := s.alarmCounts(c, []int64{id}, from, now)
	if err != nil {
		return nil, err
	}
	rh, err := s.runningHours(c, []int64{id})
	if err != nil {
		return nil, err
	}

	alarms := []map[string]any{}
	arows, err := s.DB.Query(c, `SELECT name, severity, message, alarm_type, start_ts, end_ts FROM ts.alarm_events
		WHERE asset_id = $1 ORDER BY start_ts DESC LIMIT 10`, id)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var name, sev, msg, typ string
		var start time.Time
		var end *time.Time
		if err := arows.Scan(&name, &sev, &msg, &typ, &start, &end); err != nil {
			arows.Close()
			return nil, err
		}
		alarms = append(alarms, map[string]any{"name": name, "severity": sev, "message": msg, "type": typ,
			"start": httpx.TS(start), "end": httpx.TSPtr(end), "active": end == nil})
	}
	arows.Close()

	orders := []map[string]any{}
	mrows, err := s.DB.Query(c, `SELECT o.id, o.name, o.maintenance_type_id, o.status_id, o.planned_start, COALESCE(o.updated_at, o.created_at),
			(SELECT c.code || ' - ' || c.name FROM dm.maintenance_order_issues i JOIN dm.alarm_codes c ON c.id = i.alarm_code_id
			 WHERE i.order_id = o.id AND NOT i.is_other ORDER BY i.position LIMIT 1), o.description
		FROM dm.maintenance_orders o WHERE o.asset_id = $1 AND o.planned_start < $2
		ORDER BY COALESCE(o.updated_at, o.created_at) DESC LIMIT 6`, id, now.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var oid int64
		var name, desc string
		var typ, status int
		var planned, updated time.Time
		var issue *string
		if err := mrows.Scan(&oid, &name, &typ, &status, &planned, &updated, &issue, &desc); err != nil {
			mrows.Close()
			return nil, err
		}
		if issue == nil && desc != "" {
			d := desc
			issue = &d
		}
		typeName := "Preventive"
		if typ == maint.TypeCorrective {
			typeName = "Corrective"
		}
		orders = append(orders, map[string]any{"id": oid, "name": name, "type": typeName, "status": maint.StatusLabel(status),
			"issue": issue, "planned_start": httpx.TS(planned), "updated_at": httpx.TS(updated)})
	}
	mrows.Close()

	res := map[string]any{
		"id": id, "alias": a.Alias, "name": a.Name, "type_name": a.TypeName, "model_name": a.ModelName,
		"location": tree.path(a.LocationID, " > "), "image": imageURL(a.Image),
		"status": statusName(st), "status_start_time": nil, "condition": conditionName(st), "condition_start_time": nil,
		"hours": hours, "from": httpx.TS(from), "to": httpx.TS(now), "statuses": statuses, "conditions": conditions,
		"uptime_window": upWindow, "uptime_7d": upWeek, "running_hours": rh[id],
		"warning_counts": counts[id][0], "critical_counts": counts[id][1], "alarms": alarms, "maintenances": orders,
	}
	if st != nil {
		res["status_start_time"] = httpx.TSPtr(st.StatusSince)
		res["condition_start_time"] = httpx.TSPtr(st.ConditionSince)
	}
	return res, nil
}
