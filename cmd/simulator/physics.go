package main

import (
	"math"
	"math/rand/v2"
	"time"
)

// ar is a first-order autoregressive (Ornstein-Uhlenbeck) noise process with
// standard deviation sigma and correlation time tau (seconds). Correlated
// noise keeps signals smooth so thresholds are crossed cleanly.
type ar struct{ x, sigma, tau float64 }

func (a *ar) step(dt float64, r *rand.Rand) float64 {
	phi := math.Exp(-dt / a.tau)
	a.x = a.x*phi + a.sigma*math.Sqrt(1-phi*phi)*r.NormFloat64()
	return a.x
}

// Fault is an active degradation.
type Fault struct {
	Kind  string    `json:"kind"`
	Sev   float64   `json:"sev"`
	Rate  float64   `json:"rate"` // severity per second
	Start time.Time `json:"start"`
}

const maxSeverity = 1.35

// Asset is the simulated state of one equipment.
type Asset struct {
	Spec  *EquipSpec
	Model *ModelSpec
	Kind  string
	Index int // position in its group

	Duty       bool // commanded to run by the duty controller
	Trip       bool
	TripUntil  time.Time
	SpikeUntil time.Time // overload spike just before a trip
	TripSpec   *TripSpec
	Maint      bool // stopped for maintenance (forced status)
	Offline    bool
	OffUntil   time.Time

	Faults     map[string]*Fault
	FilterDays float64 // MAU natural pre-filter loading (days since last service)

	// Ctl holds the values written by operators from the application: control
	// modes (0 = automatic) and manual set points, by parameter alias.
	Ctl map[string]float64
	// ChwK / CwK scale a chiller's evaporator / condenser flow with the speed
	// of its dedicated CHW / CW pump (pump affinity: flow follows speed).
	ChwK, CwK float64

	noise  map[string]*ar
	smooth map[string]float64
	rng    *rand.Rand
	phase  float64 // dry pump process cycle offset (seconds)
	last   map[string]float64
}

func newAsset(spec *EquipSpec, m *ModelSpec, idx int, r *rand.Rand) *Asset {
	return &Asset{Spec: spec, Model: m, Kind: m.Key, Index: idx, Faults: map[string]*Fault{}, Ctl: map[string]float64{}, ChwK: 1, CwK: 1,
		noise: map[string]*ar{}, smooth: map[string]float64{}, rng: r, phase: r.Float64() * 1800, FilterDays: r.Float64() * 20}
}

// ramp moves a value toward target by at most rate (units per second).
func (a *Asset) ramp(key string, target, rate, dt float64) float64 {
	v, ok := a.smooth[key]
	if !ok {
		a.smooth[key] = target
		return target
	}
	step := rate * dt
	v += clamp(target-v, -step, step)
	a.smooth[key] = v
	return v
}

// ctl returns an operator-written control value (mode or set point), or def.
func (a *Asset) ctl(alias string, def float64) float64 {
	if v, ok := a.Ctl[alias]; ok {
		return v
	}
	return def
}

// manual reports whether a control mode is out of automatic.
func (a *Asset) manual(mode string) bool { return a.ctl(mode, 0) != 0 }

// n returns the noise of a named process.
func (a *Asset) n(key string, sigma, tau, dt float64) float64 {
	p, ok := a.noise[key]
	if !ok {
		p = &ar{sigma: sigma, tau: tau}
		p.x = sigma * a.rng.NormFloat64()
		a.noise[key] = p
	}
	return p.step(dt, a.rng)
}

// lag smooths a value toward a target with time constant tau (thermal mass).
func (a *Asset) lag(key string, target, tau, dt float64) float64 {
	v, ok := a.smooth[key]
	if !ok {
		a.smooth[key] = target
		return target
	}
	k := 1 - math.Exp(-dt/tau)
	v += (target - v) * k
	a.smooth[key] = v
	return v
}

func (a *Asset) sev(kind string) float64 {
	if f := a.Faults[kind]; f != nil {
		return math.Min(f.Sev, maxSeverity)
	}
	return 0
}

// Running reports whether the equipment is producing.
func (a *Asset) Running() bool { return a.Duty && !a.Trip && !a.Maint }

// Env is the plant environment at an instant.
type Env struct {
	DryBulb, WetBulb, RH float64
	Load                 float64 // plant cooling load per running chiller (0..1)
	CWS                  float64 // condenser water supply temperature from the towers
	ChillerLoad          float64
	CDALoad              float64
}

// Weather is the Singapore weather and plant load model.
type Weather struct {
	db, wb, load *ar
	rng          *rand.Rand
}

func newWeather(r *rand.Rand) *Weather {
	return &Weather{db: &ar{sigma: 0.9, tau: 3 * 3600}, wb: &ar{sigma: 0.35, tau: 3 * 3600}, load: &ar{sigma: 0.025, tau: 3600}, rng: r}
}

func (w *Weather) at(t time.Time, loc *time.Location, dt float64) Env {
	lt := t.In(loc)
	h := float64(lt.Hour()) + float64(lt.Minute())/60
	day := math.Sin(2 * math.Pi * (h - 9) / 24) // peak 15:00
	db := 27.9 + 3.3*day + w.db.step(dt, w.rng)
	wb := 25.4 + 0.9*day + w.wb.step(dt, w.rng)
	if wb > db-0.5 {
		wb = db - 0.5
	}
	rh := clamp(100-5.2*(db-wb), 55, 98)
	load := 0.74 + 0.06*day + w.load.step(dt, w.rng)
	return Env{DryBulb: db, WetBulb: wb, RH: rh, Load: clamp(load, 0.55, 0.95)}
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// psatR134a is the R-134a saturation pressure (kPa abs) at temperature t (°C).
func psatR134a(t float64) float64 { return math.Exp(5.682 + 0.0353*t - 0.00009*t*t) }

// Values computes all parameter values of an asset (booleans as 0/1).
func (a *Asset) Values(t time.Time, env Env, dt float64) map[string]float64 {
	v := map[string]float64{}
	run := a.Running()
	v["running"] = b2f(run)
	v["trip"] = b2f(a.Trip)
	v["maint_mode"] = b2f(a.Maint)
	spike := !a.SpikeUntil.IsZero() && t.Before(a.SpikeUntil)
	switch a.Kind {
	case "chiller":
		a.chiller(v, env, run, dt)
	case "mau":
		a.mau(v, env, run, dt)
	case "ct":
		a.tower(v, t, env, run, dt)
	case "pcw", "chwp", "cwp":
		a.pump(v, env, run, dt)
	case "cda":
		a.compressor(v, t, env, run, dt)
	case "vac":
		a.dryPump(v, t, env, run, dt)
	}
	if spike && a.TripSpec != nil && a.TripSpec.Param != "" {
		v[a.TripSpec.Param] = a.TripSpec.Spike + a.n("spike", 0.5, 20, dt)
	}
	a.last = v
	return v
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (a *Asset) chiller(v map[string]float64, env Env, run bool, dt float64) {
	foul, leak, brg := a.sev("cond_fouling"), a.sev("refrigerant_leak"), a.sev("bearing_wear")
	sp := a.ctl("chwst_sp", 6.5)
	v["chwst_sp"] = sp
	v["run_mode"] = a.ctl("run_mode", 0)
	if !run {
		// a start pulls down to the set point itself (no ramp)
		a.smooth["chwst_asp"] = sp
		v["chwst_asp"] = sp
		room := 24 + a.n("room", 0.3, 1800, dt)
		v["load_pct"], v["chw_flow"], v["motor_current"] = 0, 0, 0
		v["chwst"] = a.lag("chwst", 11.5, 1800, dt) + a.n("chwst_s", 0.05, 60, dt)
		v["chwrt"] = v["chwst"] + 0.1
		v["cwet"] = a.lag("cwet", 28.5, 1800, dt)
		v["cwlt"] = v["cwet"] + 0.1
		eq := psatR134a(room)
		v["evap_press"] = eq + a.n("ep", 2, 120, dt)
		v["cond_press"] = eq + 5 + a.n("cp", 2, 120, dt)
		v["evap_approach"], v["cond_approach"], v["superheat"], v["efficiency"] = 0, 0, 0, 0
		v["discharge_temp"] = a.lag("dt", room+1, 1800, dt)
		v["oil_dp"] = 0
		v["oil_temp"] = a.lag("oil", 38, 1800, dt) // sump heater
		v["vibration"] = 0.1 + math.Abs(a.n("vib", 0.03, 30, dt))
		v["power_kw"] = 3.2 + a.n("pw", 0.1, 60, dt)
		return
	}
	load := clamp(env.ChillerLoad*100+a.n("load", 1.2, 600, dt), 20, 100)
	// The unit controller ramps its active set point toward the operator's at
	// 0.6 K/min (soft set point change) and the capacity control holds the
	// leaving water at the active one, so a set point change doesn't raise the
	// deviation alarm (CHL-008 compares against the active set point).
	asp := a.ramp("chwst_asp", sp, 0.01, dt)
	v["chwst_asp"] = asp
	chwst := asp + a.n("chwst", 0.06, 20, dt)
	if leak > 0.6 {
		chwst += (leak - 0.6) * 2.2
	}
	chwst = a.lag("chwst", chwst, 45, dt)
	// Evaporator / condenser flows follow the dedicated pumps' speed.
	flow := 550*a.ChwK + a.n("flow", 4, 600, dt)
	cwFlow := 680 * a.CwK
	q := load / 100 * 3517 // kW cooling
	dtw := q / (flow * 1.1628)
	cwet := env.CWS + a.n("cwet", 0.06, 300, dt)
	condApp := 1.2 + 4.5*foul + a.n("capp", 0.05, 300, dt)
	evapApp := 0.9 + 3.5*leak + a.n("eapp", 0.04, 300, dt)
	evapSat := chwst - evapApp - load/100*0.4
	// lift: condensing above the leaving condenser water (range 4 K at the design flow)
	cop := 6.4 - 0.11*((cwet+4/a.CwK+condApp)-evapSat-31) - 0.6*leak
	power := q / cop * (1 + 0.04*brg)
	cwlt := cwet + (q+power)/(cwFlow*1.1628)
	condSat := cwlt + condApp
	v["load_pct"] = load
	v["chwst"] = chwst
	v["chwrt"] = chwst + dtw
	v["chw_flow"] = flow
	v["cwet"] = cwet
	v["cwlt"] = cwlt
	v["evap_press"] = psatR134a(evapSat) - 45*leak + a.n("ep", 2, 60, dt)
	v["cond_press"] = psatR134a(condSat) + a.n("cp", 3, 60, dt)
	v["evap_approach"] = evapApp
	v["cond_approach"] = condApp
	v["superheat"] = 0.6 + 6*leak + a.n("sh", 0.1, 120, dt)
	v["discharge_temp"] = condSat + 9 + 10*leak + 2*foul + a.n("dt", 0.2, 120, dt)
	v["oil_dp"] = 180 - 90*brg + a.n("odp", 2, 60, dt)
	v["oil_temp"] = a.lag("oil", 47+15*brg+0.03*(load-70), 900, dt) + a.n("ot", 0.15, 120, dt)
	v["vibration"] = 1.7 + 6*brg + 0.005*load + a.n("vib", 0.08, 30, dt)
	v["power_kw"] = power + a.n("pw", 2, 60, dt)
	v["motor_current"] = v["power_kw"] * 1000 / (1.732 * 400 * 0.92)
	v["efficiency"] = v["power_kw"] / (q / 3.517)
}

// chwSystem derives the BMS plant points from the running chillers, pumps and
// towers and the fab demand (MAU cooling coil valves): header temperatures,
// flows, the minimum-flow bypass, riser differential pressure and kW/RT.
func (a *Asset) chwSystem(v map[string]float64, p *Plant, out map[*Asset]map[string]float64, dt float64) {
	var flow, tsFlow, load, power float64
	nCh := 0
	for _, c := range p.group("chillers") {
		if !c.Running() {
			continue
		}
		cv := out[c]
		nCh++
		flow += cv["chw_flow"]
		tsFlow += cv["chw_flow"] * cv["chwst"]
		load += cv["load_pct"] / 100 * 3517
		power += cv["power_kw"]
	}
	for _, g := range []string{"chwp", "cwp"} {
		for _, pm := range p.group(g) {
			if pm.Running() {
				power += out[pm]["motor_current"] * 1.732 * 400 * 0.88 / 1000
			}
		}
	}
	for _, ct := range p.group("towers") {
		if ct.Running() {
			power += out[ct]["fan_current"] * 1.732 * 400 * 0.85 / 1000
		}
	}
	valve, nMau := 0.0, 0
	for _, m := range p.group("mau") {
		if m.Running() {
			valve += out[m]["chw_valve"]
			nMau++
		}
	}
	if nMau > 0 {
		valve /= float64(nMau)
	}
	v["running"] = b2f(nCh > 0)
	v["chillers_running"] = float64(nCh)
	v["plant_kw"] = power
	if nCh == 0 || flow <= 0 {
		v["chws_temp"] = a.lag("chws", 12, 1800, dt) + a.n("chws_n", 0.05, 120, dt)
		v["chwr_temp"] = v["chws_temp"] + 0.2
		v["delta_t"] = 0.2
		v["plant_flow"], v["fab_flow"], v["bypass_flow"], v["bypass_valve"], v["cooling_load"], v["plant_eff"] = 0, 0, 0, 0, 0, 0
		v["riser_dp"] = a.lag("dp", 4, 120, dt) + a.n("dp_n", 0.5, 60, dt)
		return
	}
	ts := tsFlow/flow + 0.06 + a.n("ts", 0.03, 120, dt) // pick-up between evaporator and header
	// The bypass opens at low load to hold the chillers' minimum evaporator
	// flow; fully open it passes about a third of the plant flow.
	bpv := clamp(a.lag("bpv", 6+110*(0.6-p.Env.ChillerLoad), 300, dt)+a.n("bpv_n", 0.6, 120, dt), 0, 100)
	bypass := flow * bpv / 100 * 0.35
	fab := flow - bypass
	// The pumps hold the riser set-point until the coil valves saturate or a
	// duty chiller (and its primary pump) is missing.
	dpT := 120 - 1.1*math.Max(0, valve-82)
	if nCh < 2 {
		dpT -= 28
	}
	v["chws_temp"] = ts
	v["chwr_temp"] = ts + load/(fab*1.1628) + a.n("tr", 0.03, 120, dt)
	v["delta_t"] = v["chwr_temp"] - ts
	v["plant_flow"] = flow
	v["fab_flow"] = fab
	v["bypass_flow"] = bypass
	v["bypass_valve"] = bpv
	v["riser_dp"] = a.lag("dp", dpT, 90, dt) + a.n("dp_n", 1.2, 60, dt)
	v["cooling_load"] = load
	v["plant_eff"] = power / (load / 3.517)
}

func (a *Asset) mau(v map[string]float64, env Env, run bool, dt float64) {
	clog, coil, belt := a.sev("filter_clog"), a.sev("coil_fouling"), a.sev("belt_slip")
	v["oat"] = env.DryBulb + a.n("oat", 0.15, 300, dt)
	v["oa_rh"] = env.RH + a.n("oarh", 1, 600, dt)
	pre := 85 + 2.2*a.FilterDays + 170*clog
	if !run {
		v["sat"] = a.lag("sat", env.DryBulb-2, 1200, dt)
		v["sa_rh"] = a.lag("sarh", 48, 1200, dt) + a.n("sarh", 0.5, 300, dt)
		v["prefilter_dp"], v["hepa_dp"], v["fan_speed"], v["fan_current"], v["airflow"], v["chw_valve"], v["duct_static"] = 0, 0, 0, 0, 0, 0, 0
		return
	}
	sat := 17.0 + a.n("sat", 0.12, 10, dt)
	if coil > 0.5 {
		sat += (coil - 0.5) * 4.5
	}
	v["sat"] = sat
	v["sa_rh"] = 45 + math.Max(0, coil-0.4)*14 + a.n("sarh", 0.5, 900, dt)
	v["prefilter_dp"] = pre + a.n("pre", 1.5, 600, dt)
	v["hepa_dp"] = 262 + 0.04*a.FilterDays + a.n("hepa", 2, 600, dt)
	speed := clamp(66+(pre-85)*0.05+25*belt+a.n("spd", 0.3, 60, dt), 30, 100)
	v["fan_speed"] = speed
	v["airflow"] = 60000*(1-0.32*belt) + a.n("af", 250, 120, dt)
	v["fan_current"] = 41*math.Pow(speed/66, 2.6)*(1-0.18*belt) + a.n("fc", 0.4, 60, dt)
	need := 52 + 6*(env.DryBulb-28) + 0.2*(env.RH-80)
	v["chw_valve"] = clamp(need*(1+0.9*coil)+a.n("valve", 1.5, 120, dt), 0, 100)
	v["duct_static"] = 500 - 90*belt + a.n("ds", 4, 60, dt)
}

func (a *Asset) tower(v map[string]float64, t time.Time, env Env, run bool, dt float64) {
	gear, scale, makeup := a.sev("gearbox_wear"), a.sev("fill_scaling"), a.sev("makeup_valve")
	wb := env.WetBulb
	v["wet_bulb"] = wb + a.n("wb", 0.05, 300, dt)
	v["basin_level"] = 74 - 45*makeup + a.n("lvl", 0.5, 600, dt)
	v["conductivity"] = 1450 + 120*math.Sin(2*math.Pi*float64(t.Unix()%21600)/21600) + 1100*scale + a.n("cond", 20, 1800, dt)
	// Fan speed control: in Auto the approach controller sets the speed from
	// the wet bulb; in Manual the operator's set point holds.
	manual := a.manual("fan_mode")
	v["fan_mode"] = a.ctl("fan_mode", 0)
	base := clamp(62+5*(wb-25.3)+10*scale, 30, 100) // controller output (without noise)
	if !run {
		basin := a.lag("basin", wb+2, 1800, dt)
		v["cwst"], v["cwrt"] = basin, basin+0.05
		v["approach"] = basin - wb
		v["fan_speed"], v["fan_current"] = 0, 0
		v["fan_speed_sp"] = 0
		if manual {
			v["fan_speed_sp"] = a.ctl("fan_speed_sp", base)
		}
		v["fan_vibration"] = 0.1 + math.Abs(a.n("vib", 0.03, 30, dt))
		return
	}
	sp := clamp(base+a.n("spd", 0.5, 300, dt), 30, 100)
	if manual {
		sp = a.ctl("fan_speed_sp", sp)
	}
	speed := a.lag("fan_spd", sp, 8, dt) // VFD acceleration ramp
	// Heat rejection follows the airflow: a fan slower than the controller's
	// speed leaves a wider approach, a faster one a closer approach. The basin
	// and fill take a minute or two to settle.
	design := 3.3 + 3.5*scale
	appT := clamp(design*math.Pow(base/math.Max(speed, 10), 0.8), 1.0, 14)
	app := a.lag("app_t", appT, 90, dt) + a.n("app", 0.05, 300, dt)
	v["fan_speed_sp"] = sp
	v["approach"] = app
	v["cwst"] = wb + app
	v["cwrt"] = v["cwst"] + 4.2*env.Load/0.74 + a.n("rng", 0.05, 300, dt)
	v["fan_speed"] = speed
	v["fan_current"] = 30*math.Pow(speed/65, 2.7) + 9*gear + a.n("fc", 0.3, 300, dt)
	v["fan_vibration"] = 2.1 + 6*gear + a.n("vib", 0.07, 30, dt)
}

func (a *Asset) pump(v map[string]float64, env Env, run bool, dt float64) {
	cav, brg, imp := a.sev("cavitation"), a.sev("bearing_wear"), a.sev("impeller_wear")
	r := pumpRatings[a.Kind]
	ratedAmps := r.PowerKW * 1000 / (1.732 * 400 * 0.88)
	// Speed control: Auto runs the pump at its design speed, Manual at the
	// operator's set point (VFD).
	manual := a.manual("speed_mode")
	v["speed_mode"] = a.ctl("speed_mode", 0)
	if !run {
		v["speed_sp"] = 0
		if manual {
			v["speed_sp"] = a.ctl("speed_sp", r.SpeedHz)
		}
		v["speed"], v["flow"], v["motor_current"], v["spec_energy"] = 0, 0, 0, 0
		v["suction_press"] = r.Suction - 0.05 + a.n("sp", 0.01, 120, dt)
		v["discharge_press"] = v["suction_press"] + 0.05
		v["vibration"] = 0.1 + math.Abs(a.n("vib", 0.03, 30, dt))
		v["bearing_temp"] = a.lag("brg", 31, 1800, dt)
		v["motor_temp"] = a.lag("mt", 31, 1800, dt)
		return
	}
	sp := r.SpeedHz
	if manual {
		sp = a.ctl("speed_sp", sp)
	}
	v["speed_sp"] = sp
	hz := a.lag("spd_t", sp, 6, dt) // VFD acceleration ramp
	// affinity laws: flow ~ speed, head ~ speed², power ~ speed³
	k := hz / r.SpeedHz
	v["speed"] = hz + a.n("spd", 0.1, 60, dt)
	v["suction_press"] = r.Suction - 1.2*cav + a.n("sp", 0.02, 60, dt)
	v["discharge_press"] = v["suction_press"] + r.DP*k*k*(1-0.28*imp) + a.n("dp", 0.03, 60, dt)
	flow := k*r.Flow*(1-0.35*imp-0.08*cav) + a.n("flow", 2*r.Flow/318, 60, dt)
	power := (r.PowerKW*(1-0.08*imp)+6*brg*r.PowerKW/52)*k*k*k + a.n("pw", 0.4, 60, dt)
	v["flow"] = flow
	v["motor_current"] = power * 1000 / (1.732 * 400 * 0.88)
	v["spec_energy"] = power / flow
	v["vibration"] = 1.5 + 6*brg + 3.5*cav + a.n("vib", 0.06, 30, dt)
	v["bearing_temp"] = a.lag("brg", 51+35*brg+0.2*(env.DryBulb-28), 900, dt) + a.n("bt", 0.2, 300, dt)
	v["motor_temp"] = a.lag("mt", 66+0.15*(v["motor_current"]-ratedAmps), 900, dt) + a.n("mtn", 0.3, 300, dt)
}

func (a *Asset) compressor(v map[string]float64, t time.Time, env Env, run bool, dt float64) {
	icf, dryer, leak := a.sev("intercooler_fouling"), a.sev("dryer_fault"), a.sev("air_leak")
	v["dew_point"] = -45 + 22*dryer + a.n("pdp", 0.4, 600, dt)
	v["cooling_water_temp"] = env.CWS + 0.3 + a.n("cwt", 0.1, 300, dt)
	if !run {
		v["load_pct"], v["flow"], v["motor_current"] = 0, 0, 0
		v["discharge_press"] = 7.45 + a.n("p", 0.02, 60, dt)
		v["lp_elem_temp"] = a.lag("lp", 45, 900, dt)
		v["hp_elem_temp"] = a.lag("hp", 45, 900, dt)
		v["intercooler_temp"] = a.lag("ic", 32, 900, dt)
		return
	}
	base := 78.0
	if a.Index == 1 {
		base = 55 + 15*math.Sin(2*math.Pi*float64(t.Unix()%1200)/1200)
	}
	raw := base + 40*leak + a.n("load", 1.5, 120, dt)
	load := clamp(raw, 20, 100)
	press := 7.5 + a.n("p", 0.02, 30, dt)
	if raw > 100 {
		press -= (raw - 100) * 0.05
	}
	v["load_pct"] = load
	v["discharge_press"] = press
	v["flow"] = 48 * load / 100
	v["motor_current"] = 120 + 4.6*load + a.n("cur", 3, 60, dt)
	v["lp_elem_temp"] = a.lag("lp", 168+0.2*(load-78)+10*icf, 300, dt) + a.n("lpn", 0.4, 60, dt)
	v["intercooler_temp"] = a.lag("ic", 33+0.5*(v["cooling_water_temp"]-30)+15*icf, 300, dt) + a.n("icn", 0.15, 60, dt)
	v["hp_elem_temp"] = a.lag("hp", 172+0.25*(load-78)+50*icf, 300, dt) + a.n("hpn", 0.4, 60, dt)
}

func (a *Asset) dryPump(v map[string]float64, t time.Time, env Env, run bool, dt float64) {
	dep, rotor, purge := a.sev("deposition"), a.sev("rotor_wear"), a.sev("purge_block")
	cvd := a.Model.Name == "Edwards iXH610 Dry Pump (CVD)"
	bodyBase := 84.0
	if cvd {
		bodyBase = 90
	}
	v["n2_flow"] = 52 - 27*purge + a.n("n2", 0.5, 120, dt)
	v["cooling_water_flow"] = 5.2 + a.n("cw", 0.05, 300, dt)
	if !run {
		v["inlet_press"] = 0.02 + math.Abs(a.n("ip", 0.002, 60, dt))
		v["dp_current"], v["booster_current"] = 0, 0
		v["body_temp"] = a.lag("body", 35, 2400, dt)
		v["exhaust_press"] = 1013 + a.n("ex", 1, 60, dt)
		return
	}
	cycle := math.Mod(float64(t.Unix())+a.phase, 1800)
	proc := 0.0
	if cycle < 1080 {
		proc = 1
	}
	gas := a.lag("gas", proc, 60, dt)
	// Degradation adds to the readings in every step so alarms do not
	// toggle with the process cycle.
	v["inlet_press"] = (0.02+0.10*gas)*(1+a.n("ipn", 0.04, 30, dt)) + 1.4*math.Pow(rotor, 1.5)
	v["dp_current"] = 8.2 + 0.3*gas + 5.0*dep + 1.2*rotor + a.n("dpc", 0.08, 120, dt)
	v["booster_current"] = 2.6 + 0.3*gas + 2.4*rotor + a.n("bc", 0.05, 120, dt)
	v["body_temp"] = a.lag("body", bodyBase+4*gas+26*dep, 900, dt) + a.n("bt", 0.3, 300, dt)
	v["exhaust_press"] = 1018 + 10*gas + 140*dep + a.n("ex", 2, 120, dt)
}

// round applies a parameter's decimal places.
func round(v float64, scale int) float64 {
	p := math.Pow(10, float64(scale))
	return math.Round(v*p) / p
}
