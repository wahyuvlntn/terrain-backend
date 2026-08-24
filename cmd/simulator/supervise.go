package main

import (
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// Plant supervisory control: the BMS plant controller CHWS_01 and the loops
// it closes. Set points: CHW supply reset (pushed to the chillers), chiller
// staging on load, the tower leaving water set point (wet bulb + approach).
// Loops: variable-primary CHW pumps on the riser differential pressure with a
// minimum-flow bypass, tower fans on the tower leaving water and the 3-way
// tower bypass valve holding the minimum condenser water temperature. The
// loops run in internal steps of loopStep seconds, so their dynamics (and a
// detuned loop's hunting) do not depend on the caller's time step.
// ---------------------------------------------------------------------------

const (
	cpw          = 1.1628 // kW per (m³/h · K) of water
	chillerCapKW = 3517.0 // chiller capacity (1000 RT)

	// Fab CHW demand: the coil valves take the flow the load needs at the coil
	// temperature difference, which shrinks with a warmer CHW supply.
	coilDT       = 4.07   // K at the design CHW supply (6.5 °C)
	coilDTPerK   = 0.22   // extra coil flow (fraction) per K of warmer supply
	valveMaxFlow = 1833.0 // m³/h through the fully open coil valves at 120 kPa

	// Variable-primary CHW hydraulics: parallel pump + evaporator branches
	// (head = Hs·k² − c·q² per branch, k = speed / design speed) feed the
	// risers through the plant headers (− r·Q²). Calibrated so two pumps hold
	// 120 kPa at about 45 Hz at the design load and reach 48 Hz at 88 % load.
	chwpShutoffKPa = 205.7
	branchLossK    = 1.232e-4
	headerLossK    = 3.0e-5
	staticDPKPa    = 4.0
	chwpMinHz      = 30.0
	chwpMaxHz      = 48.0
	designChwFlow  = 550.0
	minFlowFrac    = 0.6   // the bypass holds 60 % of the design chiller flow
	bypassFullFlow = 385.0 // m³/h through the fully open bypass at 120 kPa

	// Cooling towers: the approach follows the heat load per cell, the fan
	// speed (airflow) and the heat transfer capacity:
	// approach = A0 · (Q/Qd)^0.6 · (100/speed)^0.8 / capacity.
	towerApproach0 = 2.48   // K at the design heat load per cell and 100 % fan
	towerDesignKW  = 3037.0 // design heat rejection per cell (two chillers at 74 %)
	towerFanKW     = 30.0   // fan power at 100 % speed
	fanMinPct      = 20.0

	loopStep = 5.0 // s

	// PI tunings (velocity form; the clamp of the output is the anti-windup)
	ctKp, ctTi   = 8.0, 120.0  // tower fans: % per K, s
	dpKp, dpTi   = 0.05, 30.0  // CHW pumps: Hz per kPa, s
	twvKp, twvTi = 10.0, 120.0 // 3-way valve: % per K, s

	// supervisory defaults
	defCHWSP      = 6.5
	defApproachSP = 3.5
	defCWMin      = 20.0
	defRiserDPSP  = 120.0
)

// dutyChillers is the number of duty chillers the plant controller asks for:
// the operator's count in Manual staging, else the load staging (initially
// from the present load).
func (p *Plant) dutyChillers() int {
	s := p.sys
	if s == nil {
		return 2
	}
	if s.manual("staging_mode") {
		def := s.Cmd["stage_n"]
		if def < 1 {
			def = 2
		}
		return int(clamp(math.Round(s.ctl("chillers_sp", def)), 1, 3))
	}
	if n, ok := s.Cmd["stage_n"]; ok && n >= 1 {
		return int(n)
	}
	n := 2.0
	if p.Env.FabLoad > 0 {
		n = clamp(math.Ceil(p.Env.FabLoad/(chillerCapKW*0.95)), 1, 3)
	}
	s.Cmd["stage_n"] = n
	return int(n)
}

// supervise runs the plant controller for dt seconds: the set points, then
// the CHW and CW loops in internal steps. It leaves the condenser water
// temperatures in env and the flows, loads and loop states the equipment
// physics reports in the assets.
func (p *Plant) supervise(t time.Time, env *Env, dt float64) {
	s := p.sys
	if s == nil {
		return
	}
	if dt < 0 {
		dt = 0
	}
	p.setPoints(s, t, env, dt)
	n := int(math.Ceil(dt/loopStep - 1e-9))
	if n < 1 {
		n = 1
	}
	h := dt / float64(n)
	for i := 0; i < n; i++ {
		p.chwLoop(s, env, h)
		p.cwLoop(s, env, h)
	}
	env.CWS, env.CWR = s.smooth["cw_supply"], s.smooth["cw_return"]
}

// setPoints updates the supervisory set points: the CHW supply set point
// (reset on the outdoor air once per hour, or the operator's value) pushed to
// every chiller when it changes, the tower leaving water set point and the
// load staging of the chillers.
func (p *Plant) setPoints(s *Asset, t time.Time, env *Env, dt float64) {
	sp, have := s.Cmd["chws_sp"]
	if s.manual("chws_reset_mode") {
		if !have {
			sp = defCHWSP
		}
		sp = s.ctl("chws_sp", sp)
		s.smooth["reset_hour"] = -1 // back in Reset, recompute at once
	} else if hour := float64(t.Unix() / 3600); !have || s.smooth["reset_hour"] != hour {
		// LBNL reset adapted to the tropics: 7.0 °C up to 26 °C outdoor air,
		// down to 6.0 °C at 31 °C (cooler water for the afternoon latent load)
		oa := env.DryBulb
		if v, ok := s.meas["oa_temp"]; ok {
			oa = v
		}
		sp = round(clamp(7.0-(oa-26)/(31-26)*1.0, 6.0, 7.0), 1)
		s.smooth["reset_hour"] = hour
	}
	s.Cmd["chws_sp"] = sp
	// supervisory push: a local write to one chiller lasts until the next change
	if pushed, ok := s.smooth["chws_sp_pushed"]; !ok || pushed != sp {
		for _, c := range p.group("chillers") {
			c.Ctl["chwst_sp"] = sp
		}
		s.smooth["chws_sp_pushed"] = sp
	}

	// tower leaving water set point (LBNL eq. 3) on the measured wet bulb,
	// averaged over about 10 min as a BMS does (sensor noise would otherwise
	// move the fans)
	wb := env.WetBulb
	if v, ok := s.meas["wet_bulb"]; ok {
		wb = v
	}
	wb = s.lagIn("wb_avg", wb, 600, dt)
	s.smooth["cwst_sp"] = math.Max(wb+s.ctl("approach_sp", defApproachSP), s.ctl("cw_min_temp", defCWMin))

	// staging: up when the load stays above 95 % of the running capacity for
	// 10 min, down when the remaining chillers would carry it below 80 % for
	// 30 min (LBNL ξ = 0.95 with a shorter wait upwards for safety)
	n := float64(p.dutyChillers())
	if s.manual("staging_mode") {
		s.Cmd["stage_n"] = n // a return to Load starts from the operator's count
		s.smooth["stage_up_s"], s.smooth["stage_dn_s"] = 0, 0
		return
	}
	load, running := env.FabLoad, true
	if m := s.meas; m != nil {
		load, running = m["cooling_load"], m["running"] == 1
	}
	up, dn := s.smooth["stage_up_s"], s.smooth["stage_dn_s"]
	if n < 3 && load > n*chillerCapKW*0.95 {
		up += dt
	} else {
		up = 0
	}
	if running && n > 1 && load < (n-1)*chillerCapKW*0.80 {
		dn += dt
	} else {
		dn = 0
	}
	switch {
	case up >= 600:
		n++
		up, dn = 0, 0
	case dn >= 1800:
		n--
		up, dn = 0, 0
	}
	s.Cmd["stage_n"] = n
	s.smooth["stage_up_s"], s.smooth["stage_dn_s"] = up, dn
}

// fabDemand is the CHW flow the fab coil valves take (m³/h).
func fabDemand(env *Env) float64 {
	chws := env.CHWS
	if chws == 0 {
		chws = defCHWSP
	}
	return env.FabLoad / (cpw * coilDT) * (1 + coilDTPerK*clamp(chws-6.5, -1.5, 4))
}

// chwBranch is a running CHW pump with its chiller's evaporator.
type chwBranch struct {
	pump  *Asset
	hs, k float64 // shutoff head at the design speed (kPa), speed ratio
	q     float64 // flow (m³/h)
}

// commonHead returns the head at the common pump header for a total flow q
// through the parallel branches and sets their flows (check valves: a branch
// whose pump cannot reach the header head has no flow).
func commonHead(br []chwBranch, q float64) float64 {
	top, equal := 0.0, true
	for i, b := range br {
		hk := b.hs * b.k * b.k
		if hk > top {
			top = hk
		}
		if i > 0 && math.Abs(hk-br[0].hs*br[0].k*br[0].k) > 1e-9 {
			equal = false
		}
	}
	if equal {
		qb := q / float64(len(br))
		for i := range br {
			br[i].q = qb
		}
		return top - branchLossK*qb*qb
	}
	flowAt := func(head float64) float64 {
		sum := 0.0
		for _, b := range br {
			sum += math.Sqrt(math.Max(0, b.hs*b.k*b.k-head) / branchLossK)
		}
		return sum
	}
	lo, hi := top-branchLossK*q*q, top
	for it := 0; it < 50; it++ {
		mid := (lo + hi) / 2
		if flowAt(mid) > q {
			lo = mid
		} else {
			hi = mid
		}
	}
	head := (lo + hi) / 2
	for i, b := range br {
		br[i].q = math.Sqrt(math.Max(0, b.hs*b.k*b.k-head) / branchLossK)
	}
	return head
}

// solveCHW solves the CHW hydraulics for the fab demand: the riser DP, the
// fab flow (less than the demand when the coil valves saturate at a low DP)
// and the minimum-flow bypass. It leaves the branch flows in br.
func solveCHW(br []chwBranch, demand float64) (dp, fab, bypass float64) {
	minFlow := minFlowFrac * designChwFlow * float64(len(br))
	bp := func(q float64) float64 { return math.Max(0, minFlow-q) }
	dpAt := func(q float64) float64 {
		qt := q + bp(q)
		return commonHead(br, qt) - headerLossK*qt*qt
	}
	qmax := func(dp float64) float64 { return valveMaxFlow * math.Sqrt(math.Max(dp, 0)/120) }
	fab = demand
	if dp = dpAt(fab); qmax(dp) < fab {
		lo, hi := 0.0, demand
		for it := 0; it < 40; it++ {
			mid := (lo + hi) / 2
			if qmax(dpAt(mid)) > mid {
				lo = mid
			} else {
				hi = mid
			}
		}
		fab = lo
		dp = dpAt(fab)
	}
	return math.Max(dp, 0), fab, bp(fab)
}

// pi advances a velocity-form PI controller by h seconds: the output is the
// plant controller command cmd, the previous error is kept in smooth[mem].
func (a *Asset) pi(cmd, mem string, e, kp, ti, h, lo, hi float64) float64 {
	u := a.Cmd[cmd] + kp*(e-a.smooth[mem]) + kp*h/ti*e
	u = clamp(u, lo, hi)
	a.Cmd[cmd], a.smooth[mem] = u, e
	return u
}

// lagIn advances a loop state of an asset (initialised at target).
func (a *Asset) lagIn(key string, target, tau, h float64) float64 {
	v, ok := a.smooth[key]
	if !ok {
		v = target
	} else {
		v = lagStep(v, target, tau, h)
	}
	a.smooth[key] = v
	return v
}

// chwLoop is one internal step of the CHW loop: CHW pump speeds (the riser DP
// command in Auto, the operator's speed in Manual), the hydraulics, the
// minimum-flow bypass, the riser DP PI on the measured DP and each running
// chiller's flow and share of the fab load.
func (p *Plant) chwLoop(s *Asset, env *Env, h float64) {
	r := pumpRatings["chwp"]
	pumps, chillers := p.group("chwp"), p.group("chillers")
	bias, _ := s.fval("riser_dp_bias")
	sp := s.ctl("riser_dp_sp", defRiserDPSP)
	demand := fabDemand(env)

	nRun, auto := 0, false
	for _, pm := range pumps {
		if pm.Running() {
			nRun++
			auto = auto || !pm.manual("speed_mode")
		}
	}
	start := false
	switch {
	case nRun > 0 && auto && s.smooth["dp_on"] != 1:
		// (re)start of the loop from the speed that holds the set point
		nb := float64(nRun)
		q := math.Max(demand, minFlowFrac*designChwFlow*nb)
		k2 := (sp/(1+bias) + (branchLossK/(nb*nb)+headerLossK)*q*q) / chwpShutoffKPa
		s.Cmd["chwp_speed_cmd"] = clamp(r.SpeedHz*math.Sqrt(math.Max(k2, 0)), chwpMinHz, chwpMaxHz)
		s.smooth["dp_on"], start = 1, true
	case nRun == 0:
		s.smooth["dp_on"] = 0
		s.Cmd["chwp_speed_cmd"] = 0
	case !auto:
		s.smooth["dp_on"] = 0 // every running pump in Manual: the command holds
	}

	br := make([]chwBranch, 0, len(pumps))
	for _, pm := range pumps {
		if !pm.Running() {
			pm.smooth["loop_hz"], pm.smooth["loop_flow"], pm.smooth["loop_head"] = 0, 0, 0
			pm.Cmd["speed_sp"] = 0
			continue
		}
		cmd := s.Cmd["chwp_speed_cmd"]
		if pm.manual("speed_mode") {
			cmd = pm.ctl("speed_sp", r.SpeedHz)
		}
		pm.Cmd["speed_sp"] = cmd
		hz := pm.lagIn("loop_hz", cmd, 6, h) // VFD acceleration ramp
		hs := chwpShutoffKPa * (1 - 0.28*pm.sev("impeller_wear")) * (1 - 0.08*pm.sev("cavitation"))
		br = append(br, chwBranch{pump: pm, hs: hs, k: hz / r.SpeedHz})
	}

	var dp, fab, bypass float64
	if len(br) > 0 {
		dp, fab, bypass = solveCHW(br, demand)
	} else {
		dp = s.lagIn("riser_dp", staticDPKPa, 120, h)
	}
	s.smooth["riser_dp"], s.smooth["fab_flow"], s.smooth["bypass_flow"] = dp, fab, bypass
	pos := 0.0
	if bypass > 0 {
		pos = clamp(100*bypass/(bypassFullFlow*math.Sqrt(math.Max(dp, 10)/120)), 0, 100)
	}
	s.lagIn("bypass_valve", pos, 30, h)
	for _, b := range br {
		b.pump.smooth["loop_flow"] = b.q
		// pump head: the branch loss splits between the pump and the evaporator
		b.pump.smooth["loop_head"] = b.hs*b.k*b.k - 0.5*branchLossK*b.q*b.q
	}

	// riser DP PI on the measured DP (a biased transmitter moves the loop)
	if s.smooth["dp_on"] == 1 {
		meas := (dp + s.noiseVal("dp_n")) * (1 + bias)
		e := sp - meas
		if start {
			s.smooth["pi_dp_e"] = e
		}
		s.pi("chwp_speed_cmd", "pi_dp_e", e, dpKp, dpTi, h, chwpMinHz, chwpMaxHz)
	}

	// each chiller takes the flow of its dedicated pump and its share of the
	// load the coils take (less than the fab load when their valves saturate)
	met := env.FabLoad
	if demand > 0 && fab < demand {
		met *= fab / demand
	}
	total := fab + bypass
	for i, c := range chillers {
		f := 0.0
		if i < len(pumps) && pumps[i].Running() {
			f = pumps[i].smooth["loop_flow"]
		}
		c.smooth["loop_chw_flow"] = f
		c.smooth["loop_load"] = 0
		if c.Running() && total > 0 {
			c.smooth["loop_load"] = met * f / total / chillerCapKW
		}
	}
}

// towerCapacity is the heat transfer capacity of a tower cell (1 = clean):
// fouling (LBNL, fixed factor) and fill scaling (developing).
func towerCapacity(ct *Asset) float64 {
	c := 1.0
	if v, ok := ct.fval("ct_fouling"); ok && v > 0 {
		c = v
	}
	return c / (1 + 1.06*ct.sev("fill_scaling"))
}

// towerApproach is the approach of a cell at a heat load factor, fan speed
// and capacity.
func towerApproach(loadK, fan, capy float64) float64 {
	return towerApproach0 * loadK * math.Pow(100/math.Max(fan, 10), 0.8) / capy
}

// fanFor is the fan speed that gives an approach at a heat load factor and
// capacity (the inverse of towerApproach), within the fan range.
func fanFor(app, loadK, capy float64) float64 {
	x := app * capy / (towerApproach0 * loadK) // (100 / speed)^0.8
	if x <= 0 {
		return 100
	}
	return clamp(100/math.Pow(x, 1/0.8), fanMinPct, 100)
}

// cwLoop is one internal step of the condenser water loop: CW pump flows,
// the heat the chillers reject, the tower cells (fan VFD, approach, basin and
// pipe transport to the leaving water sensor), the common tower fan PI on the
// average measured leaving water, the 3-way valve PI and the mixing of tower
// water and bypassed return water into the CW supply.
func (p *Plant) cwLoop(s *Asset, env *Env, h float64) {
	r := pumpRatings["cwp"]
	wb := env.WetBulb
	var flow float64
	cwp := p.group("cwp")
	for _, pm := range cwp {
		if !pm.Running() {
			pm.smooth["loop_hz"], pm.smooth["loop_flow"] = 0, 0
			pm.Cmd["speed_sp"] = 0
			continue
		}
		cmd := r.SpeedHz
		if pm.manual("speed_mode") {
			cmd = pm.ctl("speed_sp", r.SpeedHz)
		}
		pm.Cmd["speed_sp"] = cmd
		hz := pm.lagIn("loop_hz", cmd, 6, h)
		f := hz / r.SpeedHz * r.Flow * (1 - 0.35*pm.sev("impeller_wear") - 0.08*pm.sev("cavitation"))
		pm.smooth["loop_flow"] = f
		flow += f
	}
	s.smooth["cw_flow"] = flow

	// heat rejected: the chillers' cooling plus their compressor power (COP
	// of the last step)
	qRej := 0.0
	for i, c := range p.group("chillers") {
		cf := 0.0
		if i < len(cwp) && cwp[i].Running() {
			cf = cwp[i].smooth["loop_flow"]
		}
		c.smooth["loop_cond_flow"] = cf
		if !c.Running() {
			continue
		}
		q := math.Min(c.smooth["loop_load"], 1) * chillerCapKW
		cop := 6.3
		if c.last != nil && c.last["power_kw"] > 1 && c.last["load_pct"] > 0 {
			cop = clamp(c.last["load_pct"]/100*chillerCapKW/c.last["power_kw"], 2, 12)
		}
		qRej += q * (1 + 1/cop)
	}

	sp := s.smooth["cwst_sp"]
	var run []*Asset
	auto := false
	for _, ct := range p.group("towers") {
		if !ct.Running() {
			// a stopped cell: fan off, basin water at its standing temperature
			basin, ok := ct.smooth["basin"]
			if !ok {
				basin = wb + 2
			}
			ct.smooth["loop_fan"], ct.smooth["loop_app"] = 0, basin-wb
			ct.smooth["loop_d1"], ct.smooth["loop_d2"], ct.smooth["loop_out"] = basin, basin, basin
			ct.Cmd["fan_sp"] = 0
			continue
		}
		run = append(run, ct)
		auto = auto || !ct.manual("fan_mode")
	}
	loadK := 0.0
	if len(run) > 0 {
		perCell := qRej / float64(len(run))
		loadK = math.Pow(math.Max(perCell, 0.05*towerDesignKW)/towerDesignKW, 0.6)
	}
	start := false
	switch {
	case auto && s.smooth["ct_on"] != 1:
		// (re)start of the loop from the speed that gives the set point
		var bias, capy float64
		for _, ct := range run {
			b, _ := ct.fval("cwst_bias")
			bias += b / float64(len(run))
			capy += towerCapacity(ct) / float64(len(run))
		}
		s.Cmd["fan_speed_cmd"] = fanFor(sp-bias-wb, loadK, capy)
		s.smooth["ct_on"], start = 1, true
	case len(run) == 0:
		s.smooth["ct_on"] = 0
		s.Cmd["fan_speed_cmd"] = 0
	case !auto:
		s.smooth["ct_on"] = 0
	}

	var sumOut, sumMeas float64
	for _, ct := range run {
		spd := s.Cmd["fan_speed_cmd"]
		if ct.manual("fan_mode") {
			spd = ct.ctl("fan_speed_sp", 65)
		}
		ct.Cmd["fan_sp"] = spd
		fan := ct.lagIn("loop_fan", spd, 8, h) // VFD ramp
		appT := towerApproach(loadK, fan, towerCapacity(ct))
		if _, ok := ct.smooth["loop_app"]; !ok {
			ct.smooth["loop_d1"], ct.smooth["loop_d2"], ct.smooth["loop_out"] = wb+appT, wb+appT, wb+appT
		}
		// basin and fill thermal mass, then the transport to the sensor
		app := ct.lagIn("loop_app", appT, 90, h)
		d1 := ct.lagIn("loop_d1", wb+app, 15, h)
		d2 := ct.lagIn("loop_d2", d1, 15, h)
		out := ct.lagIn("loop_out", d2, 15, h)
		b, _ := ct.fval("cwst_bias")
		sumOut += out
		sumMeas += out + b + ct.noiseVal("cwst_s")
	}

	// tower fan PI on the average of the running cells' measured leaving
	// water (a biased cell sensor moves every fan); all Auto cells run at the
	// common command
	if s.smooth["ct_on"] == 1 {
		e := sumMeas/float64(len(run)) - sp
		if start {
			s.smooth["pi_ct_e"] = e
		}
		kp, ti := ctKp, ctTi
		if v, ok := s.fval("cw_pi_tuning"); ok && v > 0 {
			// detuned (LBNL): gain x6, integral time / 4 at intensity 1
			kp, ti = kp*(1+5*v), ti/(1+3*v)
		}
		s.pi("fan_speed_cmd", "pi_ct_e", e, kp, ti, h, fanMinPct, 100)
	}

	// tower leaving water header (equal flow through the running cells)
	tower := s.smooth["tower_supply"]
	switch {
	case len(run) > 0:
		tower = sumOut / float64(len(run))
	case flow > 1:
		tower = s.lagIn("tower_supply", s.smooth["cw_return"]-0.3, 600, h) // no cell running: hardly any cooling
	default:
		tower = s.lagIn("tower_supply", wb+2, 1800, h)
	}
	s.smooth["tower_supply"] = tower

	// 3-way tower bypass valve: its actuator follows the command unless stuck;
	// a leaking valve bypasses at least its leakage
	minPos := s.ctl("twv_min_pos", 0)
	act := s.lagIn("twv_act", math.Max(minPos, s.Cmd["twv_cmd"]), 15, h)
	pos := act
	if v, ok := s.fval("twv_stuck"); ok {
		pos = clamp(v*100, 0, 100)
		s.smooth["twv_act"] = pos
	}
	s.smooth["twv_pos"] = pos
	b := pos / 100
	if v, ok := s.fval("twv_leak"); ok {
		b = math.Max(b, v)
	}
	b = clamp(b, 0, 0.9)
	// mixing: supply = (1 − b)·tower + b·return, return = supply + range
	if flow > 1 {
		rng := qRej / (flow * cpw)
		supply := tower + b/(1-b)*rng
		s.smooth["cw_supply"], s.smooth["cw_return"] = supply, supply+rng
	} else {
		supply := s.lagIn("cw_supply", tower, 600, h)
		s.smooth["cw_return"] = supply
	}
	// valve PI holding the measured CW supply at or above the minimum
	e := s.ctl("cw_min_temp", defCWMin) - (s.smooth["cw_supply"] + s.noiseVal("cws"))
	u := s.pi("twv_pi", "pi_twv_e", e, twvKp, twvTi, h, 0, 100)
	s.Cmd["twv_cmd"] = math.Max(minPos, u)
}
