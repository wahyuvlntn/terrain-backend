package main

import (
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// FAB1 L2 process tools (docs/production/DESIGN.md section 2): their equipment
// models (parameters, alarms, faults, trips, PM), how the production model
// uses them (ToolDef: area, run class, chambers, qualifications, quality
// effects of their faults, scheduled maintenance) and their telemetry (tp
// tables evaluated by processTool from the production state).
// ---------------------------------------------------------------------------

// Process areas (the tool_type of the Production page).
const (
	areaLitho = "LITHO"
	areaEtch  = "ETCH"
	areaCVD   = "CVD"
	areaPVD   = "PVD"
	areaECP   = "ECP"
	areaCMP   = "CMP"
	areaDiff  = "DIFF"
	areaImp   = "IMP"
	areaWet   = "WET"
	areaStrip = "STRIP"
	areaMet   = "MET"
	areaTest  = "TEST"
)

// Run classes of the production model.
const (
	classCluster = "cluster" // single-wafer processing spread over the chambers
	classLitho   = "litho"   // track + scanner cell, one wafer after the other
	classBatch   = "batch"   // several lots at once (wet bench, furnace)
	classMetro   = "metro"   // measures sampled wafers
)

// qEffect is the change of a quality characteristic (key suffix: CDR resist
// CD, OVL overlay, CDE etch CD, THK thickness, CMP remaining thickness, RS
// sheet resistance, DOSE implant dose, RSADJ resistivity of a film) at fault
// severity 1: absolute (in the spec unit) and / or relative to the target.
type qEffect struct {
	Suffix   string
	Abs, Rel float64
}

// maintRule is a scheduled stop of a tool (scheduled down): a daily
// calibration (Unit daily: start hour window Min .. Max, plant time) or a
// consumable change after Min .. Max busy hours (busyh) or wafers.
type maintRule struct {
	Reason         string
	Unit           string
	Min, Max       float64
	DurMin, DurMax float64 // minutes
}

// ToolDef is how the production model uses a tool model.
type ToolDef struct {
	Area         string
	Class        string
	Chambers     []string
	Batch        int
	Quals        []string
	Effects      map[string][]qEffect
	Slow         map[string]float64 // run time stretch at severity 1
	Adders       float64            // defect adders a lot step leaves (negative: a clean removes them)
	FaultAdders  map[string]float64 // extra adders at severity 1
	Hold         map[string]float64 // faults that keep an engineering hold on above this severity
	Maint        []maintRule
	ChamberClean bool               // chamber-level cleans every 110 - 140 RF hours
	MeasNoise    map[string]float64 // metrology: extra repeatability (x spec sigma) at severity 1
	MeasBias     map[string]float64 // metrology: reading bias (x spec sigma) at severity 1
}

var pm3 = []string{"PM1", "PM2", "PM3"}

var daily = func(reason string, min float64) maintRule {
	return maintRule{Reason: reason, Unit: "daily", Min: 6, Max: 7, DurMin: min * .8, DurMax: min * 1.2}
}

// toolDefs by model key.
var toolDefs = map[string]*ToolDef{
	"scanarf": {Area: areaLitho, Class: classLitho, Chambers: []string{"STAGE"}, Quals: []string{stLithoArF},
		Effects: map[string][]qEffect{"lens_heating": {{"CDR", 3, 0}}, "laser_degradation": {{"CDR", -2.5, 0}}, "stage_drift": {{"OVL", 7, 0}}},
		Slow:    map[string]float64{"laser_degradation": .1}, Adders: .3, Hold: map[string]float64{"stage_drift": .55, "lens_heating": .55},
		Maint:   []maintRule{daily("Scanner baseline calibration", 30)}},
	"scankrf": {Area: areaLitho, Class: classLitho, Chambers: []string{"STAGE"}, Quals: []string{stLithoKrF},
		Effects: map[string][]qEffect{"lens_heating": {{"CDR", 4, 0}}, "laser_degradation": {{"CDR", -3, 0}}, "stage_drift": {{"OVL", 12, 0}}},
		Slow:    map[string]float64{"laser_degradation": .1}, Adders: .3, Hold: map[string]float64{"stage_drift": .55},
		Maint:   []maintRule{daily("Scanner baseline calibration", 25)}},
	"stepper": {Area: areaLitho, Class: classLitho, Chambers: []string{"STAGE"}, Quals: []string{stLithoI},
		Effects: map[string][]qEffect{"lamp_aging": {{"CDR", -3, 0}}, "stage_drift": {{"OVL", 20, 0}}},
		Slow:    map[string]float64{"lamp_aging": .25}, Adders: .3, Hold: map[string]float64{"stage_drift": .55},
		Maint: []maintRule{daily("Stepper baseline calibration", 20),
			{Reason: "Mercury lamp change", Unit: "busyh", Min: 700, Max: 900, DurMin: 50, DurMax: 80}}},
	"etch": {Area: areaEtch, Class: classCluster, Chambers: pm3, Quals: []string{stEtchC},
		Effects:      map[string][]qEffect{"esc_leak": {{"CDE", 1.5, 0}}},
		Slow:         map[string]float64{"rf_match": .3}, Adders: .8, Hold: map[string]float64{"esc_leak": .46, "rf_match": .7},
		ChamberClean: true},
	"etchd": {Area: areaEtch, Class: classCluster, Chambers: pm3, Quals: []string{stEtchD},
		Effects:      map[string][]qEffect{"focus_ring_wear": {{"CDE", 1.5, 0}}},
		Slow:         map[string]float64{"rf_match": .3}, Adders: .8, Hold: map[string]float64{"focus_ring_wear": .5, "rf_match": .7},
		ChamberClean: true},
	"cvd": {Area: areaCVD, Class: classCluster, Chambers: pm3, Quals: []string{stCVD},
		Effects:      map[string][]qEffect{"showerhead_clog": {{"THK", 0, -.07}}, "heater_drift": {{"THK", 0, -.02}}},
		Adders:       .6, Hold: map[string]float64{"showerhead_clog": .36, "heater_drift": .46}, ChamberClean: true},
	"pvd": {Area: areaPVD, Class: classCluster, Chambers: pm3, Quals: []string{stPVD},
		Effects: map[string][]qEffect{"cryo_degradation": {{"RSADJ", 0, .03}}},
		Adders:  .4, FaultAdders: map[string]float64{"shield_flaking": 15}, Hold: map[string]float64{"shield_flaking": .4},
		Maint:   []maintRule{{Reason: "Target change", Unit: "busyh", Min: 380, Max: 460, DurMin: 300, DurMax: 420}}},
	"ecp": {Area: areaECP, Class: classCluster, Chambers: []string{"CELL1", "CELL2", "CELL3", "CELL4"}, Quals: []string{stECP},
		Effects: map[string][]qEffect{"additive_depletion": {{"RSADJ", 0, .05}}},
		Slow:    map[string]float64{"anode_passivation": .2}, Adders: .8, FaultAdders: map[string]float64{"additive_depletion": 12},
		Hold:    map[string]float64{"additive_depletion": .45},
		Maint: []maintRule{daily("Bath analysis and dosing", 15),
			{Reason: "Anode change", Unit: "busyh", Min: 650, Max: 800, DurMin: 200, DurMax: 280}}},
	"cmp": {Area: areaCMP, Class: classCluster, Chambers: []string{"PLATENS"}, Quals: []string{stCMPD},
		Effects: map[string][]qEffect{"pad_glazing": {{"CMP", 25, 0}, {"RS", 0, -.04}}, "slurry_flow_low": {{"CMP", 8, 0}, {"RS", 0, .03}}},
		Adders:  2.0, FaultAdders: map[string]float64{"slurry_flow_low": 20, "pad_glazing": 8},
		Hold:    map[string]float64{"pad_glazing": .45, "slurry_flow_low": .4},
		Maint:   []maintRule{{Reason: "Pad change", Unit: "wafers", Min: 1100, Max: 1400, DurMin: 60, DurMax: 90}}},
	"furn": {Area: areaDiff, Class: classBatch, Chambers: []string{"TUBE"}, Batch: 4, Quals: []string{stFurnOx},
		Effects: map[string][]qEffect{"heater_zone_drift": {{"THK", 0, -.04}}},
		Adders:  .5, FaultAdders: map[string]float64{"particle_flaking": 15}, Hold: map[string]float64{"heater_zone_drift": .45, "particle_flaking": .45}},
	"rtp": {Area: areaDiff, Class: classCluster, Chambers: []string{"CH1"}, Quals: []string{stRTP},
		Effects: map[string][]qEffect{"pyrometer_drift": {{"RS", 0, .08}, {"THK", 0, -.04}}},
		Slow:    map[string]float64{"lamp_aging": .1}, Adders: .2, Hold: map[string]float64{"pyrometer_drift": .45}},
	"impmc": {Area: areaImp, Class: classCluster, Chambers: []string{"END STATION"}, Quals: []string{stImpMC, stImpLDD},
		Effects: map[string][]qEffect{"dose_drift": {{"DOSE", 0, .1}}},
		Slow:    map[string]float64{"source_degradation": .5}, Adders: .3, Hold: map[string]float64{"dose_drift": .45},
		Maint:   []maintRule{{Reason: "Ion source change", Unit: "busyh", Min: 300, Max: 360, DurMin: 200, DurMax: 260}}},
	"imphc": {Area: areaImp, Class: classCluster, Chambers: []string{"END STATION"}, Quals: []string{stImpHC, stImpLDD},
		Effects: map[string][]qEffect{"dose_drift": {{"DOSE", 0, .1}}},
		Slow:    map[string]float64{"source_degradation": .5}, Adders: .3, Hold: map[string]float64{"dose_drift": .45},
		Maint:   []maintRule{{Reason: "Ion source change", Unit: "busyh", Min: 250, Max: 320, DurMin: 200, DurMax: 260}}},
	"wet": {Area: areaWet, Class: classBatch, Chambers: []string{"TANKS"}, Batch: 2, Quals: []string{stWet},
		Adders: -1, FaultAdders: map[string]float64{"filter_clog": 40}, Hold: map[string]float64{"filter_clog": .3},
		Maint:  []maintRule{{Reason: "Bath change", Unit: "busyh", Min: 14, Max: 16, DurMin: 35, DurMax: 45}}},
	"swc": {Area: areaWet, Class: classCluster, Chambers: []string{"CH1", "CH2", "CH3", "CH4"}, Quals: []string{stSWC},
		Adders: -1.5, FaultAdders: map[string]float64{"nozzle_clog": 20}, Hold: map[string]float64{"nozzle_clog": .4}},
	"ash": {Area: areaStrip, Class: classCluster, Chambers: []string{"CH1", "CH2"}, Quals: []string{stAsh},
		Slow:   map[string]float64{"strip_rate_low": .3}, Adders: .5, FaultAdders: map[string]float64{"strip_rate_low": 15},
		Hold:   map[string]float64{"strip_rate_low": .5}},
	"met": {Area: areaMet, Class: classMetro, Chambers: []string{"STAGE"}, Quals: []string{stMet},
		Slow: map[string]float64{"lamp_aging": .5}, MeasNoise: map[string]float64{"lamp_aging": .6},
		Maint: []maintRule{daily("Daily calibration", 20)}},
	"cds": {Area: areaMet, Class: classMetro, Chambers: []string{"STAGE"}, Quals: []string{stCDSEM},
		Slow: map[string]float64{"emitter_aging": .4}, MeasNoise: map[string]float64{"emitter_aging": .8},
		Maint: []maintRule{daily("Daily calibration", 20)}},
	"ovl": {Area: areaMet, Class: classMetro, Chambers: []string{"STAGE"}, Quals: []string{stOVL},
		Slow: map[string]float64{"lamp_aging": .3}, MeasBias: map[string]float64{"lamp_aging": .8},
		Maint: []maintRule{daily("Daily calibration", 15)}},
	"def": {Area: areaMet, Class: classMetro, Chambers: []string{"STAGE"}, Quals: []string{stDefect},
		Slow: map[string]float64{"laser_degradation": .3}, MeasBias: map[string]float64{"laser_degradation": -.4},
		Maint: []maintRule{daily("Daily calibration", 20)}},
	"wat": {Area: areaTest, Class: classMetro, Chambers: []string{"PROBER"}, Quals: []string{stWAT},
		Slow: map[string]float64{"probe_card_wear": .3}, MeasBias: map[string]float64{"probe_card_wear": -2.5},
		Maint: []maintRule{daily("Tester calibration", 20),
			{Reason: "Probe card cleaning", Unit: "wafers", Min: 900, Max: 1100, DurMin: 25, DurMax: 40}}},
}

// toolQuals overrides the qualifications of individual tools.
var toolQuals = map[string][]string{
	"CMP_02": {stCMPCu}, "CMP_03": {stCMPCu},
	"FRN_03": {stFurnLP},
}

// toolDef returns the production definition of a tool model (nil: not a process tool).
func toolDef(kind string) *ToolDef { return toolDefs[kind] }

// ---------------------------------------------------------------------------
// Equipment models

// processParams are the parameters every process tool reports.
func processParams(extra ...ParamSpec) []ParamSpec {
	return append(append(statusParams(),
		numP("throughput_wph", "Throughput", "wph", 0),
		numP("wafers_today", "Wafers Today", "wafers", 0),
		numP("queue_lots", "Lots Waiting", "lots", 0),
		numP("lot_progress", "Run Progress", "%", 0),
	), extra...)
}

func toolModel(key, name, typ, series, desc string, params []ParamSpec, alarms []AlarmSpec, rels []RelationSpec, faults []FaultSpec,
	trip TripSpec, pm string, pmHours float64, mapping map[string][]string) *ModelSpec {
	return &ModelSpec{Key: key, Name: name, Type: typ, Brand: "Tool OEM", Series: series, Description: desc, Params: processParams(params...),
		Alarms: alarms, Relations: rels, Faults: faults, Trips: []TripSpec{trip}, PMCode: pm, PMRepeat: 3, PMHours: pmHours, Mapping: mapping}
}

func etchToolModel() *ModelSpec {
	return toolModel("etch", "ICP Etch Cluster ET-300", "Plasma Etch Tool", "ET-300",
		"300 mm inductively coupled plasma conductor etch cluster (Si trench, poly gate, spacer, Al, MIM): 3 chambers, vacuum transfer robot, 3 FOUP load ports",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("rf_source_power", "RF Source Power", "W", 0),
			numP("rf_reflected", "RF Reflected Power", "W", 1).W(">", 25).C(">", 40),
			numP("chamber_pressure", "Chamber Pressure", "mTorr", 2),
			numP("esc_temp", "ESC Temperature", "°C", 2),
			numP("he_flow", "Backside He Flow", "sccm", 2).W(">", 16).C(">", 19),
			numP("endpoint_time", "Endpoint Time", "s", 1).W(">", 72),
			numP("rf_hours", "RF Hours Since Chamber Clean", "h", 1).W(">", 135),
		},
		[]AlarmSpec{
			{"ETC-001", "Reflected RF power above 40 W - RF match network not tuning", true, "PRO-01", gt("rf_reflected", 40, true)},
			{"ETC-002", "ESC temperature deviates more than 2 K from 40 °C", false, "PRO-02", dev("esc_temp", 40, 2)},
			{"ETC-003", "Endpoint time above 78 s - etch rate low", false, "PRO-01", gt("endpoint_time", 78, true)},
			{"ETC-004", "Chamber pressure deviates more than 3 mTorr from 20 mTorr - check the foreline vacuum", false, "PRC-03", dev("chamber_pressure", 20, 3)},
			{"ETC-005", "RF hours above 145 h since the last chamber clean", false, "PRO-02", gt("rf_hours", 145, false)},
		},
		[]RelationSpec{
			{"RF Match Network Degradation", "ETC-010", [][]string{{"A:ETC-001"}, {"P:rf_reflected", "P:endpoint_time"}}},
			{"ESC Helium Leak Detection", "ETC-011", [][]string{{"A:ETC-002"}, {"P:he_flow"}}},
		},
		[]FaultSpec{
			fault("rf_match", "RF match network degradation", "ETC-010", "ETC-001", "ETC-003", "PM-ETC"),
			fault("esc_leak", "Electrostatic chuck helium leak", "ETC-011", "ETC-002", "PM-ETC"),
		},
		TripSpec{"ETC-001", "rf_reflected", 95, "RF generator tripped on high reflected power"}, "PM-ETC", 8,
		map[string][]string{
			"ETC-001": {"rf_reflected", "rf_source_power"}, "ETC-002": {"esc_temp", "he_flow"}, "ETC-003": {"endpoint_time", "rf_reflected"},
			"ETC-004": {"chamber_pressure"}, "ETC-005": {"rf_hours", "chambers_up"},
			"ETC-010": {"rf_reflected", "endpoint_time", "rf_source_power"}, "ETC-011": {"he_flow", "esc_temp"},
		})
}

func dielectricEtchModel() *ModelSpec {
	return toolModel("etchd", "CCP Dielectric Etch DE-300", "Plasma Etch Tool", "DE-300",
		"300 mm dual-frequency capacitively coupled plasma dielectric etch cluster (contact, via, trench, salicide block, pad openings): 3 chambers with in-situ resist strip",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("rf_hf_power", "HF RF Power (60 MHz)", "W", 0),
			numP("rf_lf_power", "LF Bias Power (2 MHz)", "W", 0),
			numP("rf_reflected", "RF Reflected Power", "W", 1).W(">", 28).C(">", 45),
			numP("chamber_pressure", "Chamber Pressure", "mTorr", 2),
			numP("wall_temp", "Chamber Wall Temperature", "°C", 1).W(">", 64).C(">", 66),
			numP("endpoint_time", "Endpoint Time", "s", 1).W(">", 88),
			numP("rf_hours", "RF Hours Since Chamber Clean", "h", 1).W(">", 135),
		},
		[]AlarmSpec{
			{"ETD-001", "Reflected RF power above 45 W - match network not tuning", true, "PRO-01", gt("rf_reflected", 45, true)},
			{"ETD-002", "Chamber wall temperature deviates more than 5 K from 60 °C", false, "PRO-02", dev("wall_temp", 60, 5)},
			{"ETD-003", "Endpoint time above 95 s - etch rate low", false, "PRO-01", gt("endpoint_time", 95, true)},
			{"ETD-004", "Chamber pressure deviates more than 4 mTorr from 35 mTorr - check the foreline vacuum", false, "PRC-03", dev("chamber_pressure", 35, 4)},
			{"ETD-005", "RF hours above 145 h since the last chamber clean", false, "PRO-02", gt("rf_hours", 145, false)},
		},
		[]RelationSpec{
			{"Dielectric Etch RF Match Degradation", "ETD-010", [][]string{{"A:ETD-001"}, {"P:rf_reflected", "P:endpoint_time"}}},
			{"Focus Ring Wear Detection", "ETD-011", [][]string{{"A:ETD-002"}, {"P:wall_temp"}}},
		},
		[]FaultSpec{
			fault("rf_match", "RF match network degradation", "ETD-010", "ETD-001", "ETD-003", "PM-ETD"),
			fault("focus_ring_wear", "Focus ring and edge ring wear", "ETD-011", "ETD-002", "PM-ETD"),
		},
		TripSpec{"ETD-001", "rf_reflected", 110, "RF generator tripped on high reflected power"}, "PM-ETD", 8,
		map[string][]string{
			"ETD-001": {"rf_reflected", "rf_hf_power", "rf_lf_power"}, "ETD-002": {"wall_temp"}, "ETD-003": {"endpoint_time", "rf_reflected"},
			"ETD-004": {"chamber_pressure"}, "ETD-005": {"rf_hours", "chambers_up"},
			"ETD-010": {"rf_reflected", "endpoint_time"}, "ETD-011": {"wall_temp", "rf_hours", "endpoint_time"},
		})
}

func cvdToolModel() *ModelSpec {
	return toolModel("cvd", "PECVD Cluster CV-300", "CVD Tool", "CV-300",
		"300 mm plasma-enhanced CVD cluster (TEOS / SiN / low-k, HDP oxide and W fill recipes): 3 heated chambers with showerhead, remote plasma clean",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("heater_temp", "Heater Temperature", "°C", 1).W("<", 396).C("<", 393),
			numP("chamber_pressure", "Chamber Pressure", "Torr", 3),
			numP("rf_power", "RF Power", "W", 0),
			numP("teos_flow", "TEOS Flow", "mg/min", 0),
			numP("dep_rate", "Deposition Rate", "nm/s", 3).W("<", 4.88).C("<", 4.8),
			numP("film_buildup", "Chamber Film Build-up", "µm", 2).W(">", 11).C(">", 12),
		},
		[]AlarmSpec{
			{"CVD-001", "Heater temperature deviates more than 5 K from 400 °C", false, "PRO-02", dev("heater_temp", 400, 5)},
			{"CVD-002", "Deposition rate below 4.8 nm/s - film thickness at risk", false, "PRO-02", lt("dep_rate", 4.8, true)},
			{"CVD-003", "Chamber film build-up above 12 µm - chamber clean overdue", false, "PRO-02", gt("film_buildup", 12, false)},
			{"CVD-004", "Chamber pressure deviates more than 0.3 Torr from 2.5 Torr - check the foreline vacuum", false, "PRC-03", dev("chamber_pressure", 2.5, 0.3)},
		},
		[]RelationSpec{
			{"Showerhead Clogging Detection", "CVD-010", [][]string{{"A:CVD-002"}, {"P:dep_rate"}}},
			{"Heater Control Drift", "CVD-011", [][]string{{"A:CVD-001"}, {"P:heater_temp"}}},
		},
		[]FaultSpec{
			fault("showerhead_clog", "Showerhead clogging", "CVD-010", "CVD-002", "PM-CVD"),
			fault("heater_drift", "Heater control drift", "CVD-011", "CVD-001", "PM-CVD"),
		},
		TripSpec{"CVD-004", "chamber_pressure", 3.6, "Chamber pressure fault - run aborted"}, "PM-CVD", 8,
		map[string][]string{
			"CVD-001": {"heater_temp"}, "CVD-002": {"dep_rate", "teos_flow", "film_buildup"}, "CVD-003": {"film_buildup", "chambers_up"},
			"CVD-004": {"chamber_pressure"}, "CVD-010": {"dep_rate", "teos_flow"}, "CVD-011": {"heater_temp", "dep_rate"},
		})
}

func pvdToolModel() *ModelSpec {
	return toolModel("pvd", "PVD Cluster PV-300", "PVD Tool", "PV-300",
		"300 mm physical vapour deposition cluster (TaN/Ta barrier and Cu seed, Ti/TiN, NiPt, AlCu): DC magnetron chambers with cryopumps, degas and pre-clean",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("dc_power", "DC Target Power", "kW", 1),
			numP("chamber_pressure", "Process Pressure", "mTorr", 2),
			numP("base_pressure", "Base Pressure", "µTorr", 2).W(">", 3).C(">", 5),
			numP("target_life", "Target Life Used", "%", 1).W(">", 90),
			numP("substrate_temp", "Substrate Temperature", "°C", 1),
			numP("dep_rate", "Deposition Rate", "Å/s", 1),
		},
		[]AlarmSpec{
			{"PVD-001", "Base pressure above 5 µTorr - cryopump regeneration needed", false, "PRC-03", gt("base_pressure", 5, false)},
			{"PVD-002", "Target life above 95 % - schedule the target change", false, "PRO-02", gt("target_life", 95, false)},
			{"PVD-003", "Process pressure deviates more than 0.8 mTorr from 3 mTorr", false, "PRO-02", dev("chamber_pressure", 3, 0.8)},
			{"PVD-004", "Deposition rate below 9 Å/s", false, "PRO-02", lt("dep_rate", 9, true)},
		},
		[]RelationSpec{
			{"Cryopump Degradation", "PVD-010", [][]string{{"A:PVD-001"}, {"P:base_pressure"}}},
			{"Shield Flaking Risk", "PVD-011", [][]string{{"A:PVD-004"}}},
		},
		[]FaultSpec{
			fault("cryo_degradation", "Cryopump degradation", "PVD-010", "PVD-001", "PM-PVD"),
			fault("shield_flaking", "Process shield flaking", "PVD-011", "PVD-004", "PM-PVD"),
		},
		TripSpec{"PVD-003", "chamber_pressure", 8, "Process pressure interlock - DC power off"}, "PM-PVD", 8,
		map[string][]string{
			"PVD-001": {"base_pressure"}, "PVD-002": {"target_life", "dep_rate"}, "PVD-003": {"chamber_pressure", "dc_power"},
			"PVD-004": {"dep_rate", "dc_power"}, "PVD-010": {"base_pressure", "chamber_pressure"}, "PVD-011": {"dep_rate", "target_life"},
		})
}

func ecpToolModel() *ModelSpec {
	return toolModel("ecp", "Cu Plating ECP-300", "Electroplating Tool", "ECP-300",
		"300 mm copper electrochemical plating: 4 plating cells, bath with organic additives (accelerator, suppressor, leveller), spin rinse dry and edge bevel removal",
		[]ParamSpec{
			numP("chambers_up", "Plating Cells Available", "", 0),
			numP("plating_current", "Plating Current", "A", 2).W("<", 11).C("<", 10),
			numP("cu_conc", "Cu Concentration", "g/L", 1),
			numP("acid_conc", "Acid Concentration", "g/L", 1),
			numP("accelerator", "Accelerator Level", "ml/L", 2).W("<", 5).C("<", 4.5),
			numP("bath_temp", "Bath Temperature", "°C", 1),
			numP("anode_life", "Anode Life Used", "%", 1).W(">", 90),
		},
		[]AlarmSpec{
			{"ECP-001", "Accelerator below 4.5 ml/L - fill and resistivity at risk", false, "PRO-04", lt("accelerator", 4.5, false)},
			{"ECP-002", "Cu concentration deviates more than 5 g/L from 50 g/L", false, "PRO-04", CondSpec{Param: "cu_conc", Op: "dev", Value: 5, SetPoint: 50}},
			{"ECP-003", "Bath temperature deviates more than 2 K from 25 °C", false, "THR-01", CondSpec{Param: "bath_temp", Op: "dev", Value: 2, SetPoint: 25}},
			{"ECP-004", "Anode life above 95 % - schedule the anode change", false, "PRO-04", gt("anode_life", 95, false)},
		},
		[]RelationSpec{
			{"Additive Depletion Detection", "ECP-010", [][]string{{"A:ECP-001"}, {"P:accelerator"}}},
			{"Anode Passivation", "ECP-011", [][]string{{"A:ECP-004", "P:accelerator"}, {"P:plating_current"}}},
		},
		[]FaultSpec{
			fault("additive_depletion", "Organic additive depletion", "ECP-010", "ECP-001", "PM-ECP"),
			fault("anode_passivation", "Anode passivation", "ECP-011", "ECP-004", "PM-ECP"),
		},
		TripSpec{"ECP-003", "bath_temp", 31, "Bath over-temperature interlock"}, "PM-ECP", 6,
		map[string][]string{
			"ECP-001": {"accelerator", "plating_current"}, "ECP-002": {"cu_conc", "acid_conc"}, "ECP-003": {"bath_temp"}, "ECP-004": {"anode_life"},
			"ECP-010": {"accelerator", "cu_conc"}, "ECP-011": {"plating_current", "anode_life"},
		})
}

func cmpToolModel() *ModelSpec {
	return toolModel("cmp", "CMP Polisher CM-300", "CMP Tool", "CM-300",
		"300 mm chemical mechanical polisher: 3 platens, 4 heads, pad conditioners, slurry delivery, integrated brush clean and dry, in-situ endpoint",
		[]ParamSpec{
			numP("platen_speed", "Platen Speed", "rpm", 0),
			numP("head_pressure", "Head Down Force", "psi", 2),
			numP("slurry_flow", "Slurry Flow", "ml/min", 0).W("<", 170).C("<", 150),
			numP("removal_rate", "Removal Rate", "nm/min", 0).W("<", 250).C("<", 230),
			numP("pad_temp", "Pad Temperature", "°C", 1).W(">", 50),
			numP("pad_life", "Pad Life Used", "%", 1).W(">", 90),
		},
		[]AlarmSpec{
			{"CMP-001", "Removal rate below 230 nm/min - pad glazing or worn pad", false, "PRO-08", lt("removal_rate", 230, true)},
			{"CMP-002", "Slurry flow below 150 ml/min", true, "PRO-08", lt("slurry_flow", 150, true)},
			{"CMP-003", "Pad temperature above 55 °C", false, "PRO-08", gt("pad_temp", 55, true)},
			{"CMP-004", "Pad life above 95 % - pad change due", false, "PRO-08", gt("pad_life", 95, false)},
		},
		[]RelationSpec{
			{"Pad Glazing Detection", "CMP-010", [][]string{{"A:CMP-001"}, {"P:removal_rate"}}},
			{"Slurry Delivery Fault", "CMP-011", [][]string{{"A:CMP-002"}, {"P:slurry_flow"}}},
		},
		[]FaultSpec{
			fault("pad_glazing", "Pad glazing / conditioner wear", "CMP-010", "CMP-001", "PM-CMP"),
			fault("slurry_flow_low", "Slurry delivery restriction", "CMP-011", "CMP-002", "PM-CMP"),
		},
		TripSpec{"CMP-003", "pad_temp", 62, "Platen over-temperature - polish aborted"}, "PM-CMP", 8,
		map[string][]string{
			"CMP-001": {"removal_rate", "pad_life", "head_pressure"}, "CMP-002": {"slurry_flow"}, "CMP-003": {"pad_temp", "platen_speed"},
			"CMP-004": {"pad_life"}, "CMP-010": {"removal_rate", "pad_temp"}, "CMP-011": {"slurry_flow", "removal_rate"},
		})
}

func furnaceToolModel() *ModelSpec {
	return toolModel("furn", "Vertical Furnace VF-300", "Diffusion Furnace", "VF-300",
		"300 mm vertical batch furnace (100 product wafers plus monitors): oxidation, anneal and LPCVD nitride / poly recipes, 5-zone heater, boat elevator, wafer transfer robot",
		[]ParamSpec{
			numP("tube_temp", "Tube Temperature (centre zone)", "°C", 1),
			numP("temp_uniformity", "Zone Temperature Spread", "K", 2).W(">", 1.8).C(">", 2.5),
			numP("gas_flow", "Process Gas Flow", "slm", 2),
			numP("tube_pressure", "Tube Pressure", "Torr", 2),
			numP("boat_load", "Lots in Boat", "lots", 0),
			numP("tube_deposit", "Tube Deposit Thickness", "µm", 1).W(">", 22),
			numP("monitor_adders", "Monitor Wafer Adders", "adders", 0).W(">", 15).C(">", 25),
		},
		[]AlarmSpec{
			{"FRN-001", "Zone temperature spread above 2.5 K - thickness uniformity at risk", false, "THR-01", gt("temp_uniformity", 2.5, true)},
			{"FRN-002", "Process gas flow deviates more than 2.5 slm from 10 slm", false, "PRO-02", dev("gas_flow", 10, 2.5)},
			{"FRN-003", "Tube deposit above 25 µm - tube clean due", false, "PRO-02", gt("tube_deposit", 25, false)},
		},
		[]RelationSpec{
			{"Heater Zone Drift", "FRN-010", [][]string{{"A:FRN-001"}, {"P:temp_uniformity"}}},
			{"Tube Particle Flaking", "FRN-011", [][]string{{"A:FRN-003"}, {"P:monitor_adders"}}},
		},
		[]FaultSpec{
			fault("heater_zone_drift", "Heater zone thermocouple drift", "FRN-010", "FRN-001", "PM-FRN"),
			fault("particle_flaking", "Tube / boat particle flaking", "FRN-011", "FRN-003", "PM-FRN"),
		},
		TripSpec{"FRN-001", "temp_uniformity", 6, "Heater over-temperature interlock - recipe aborted"}, "PM-FRN", 12,
		map[string][]string{
			"FRN-001": {"temp_uniformity", "tube_temp"}, "FRN-002": {"gas_flow", "tube_pressure"}, "FRN-003": {"tube_deposit"},
			"FRN-010": {"temp_uniformity", "tube_temp"}, "FRN-011": {"monitor_adders", "tube_deposit"},
		})
}

func rtpToolModel() *ModelSpec {
	return toolModel("rtp", "RTP Anneal RT-300", "RTP Tool", "RT-300",
		"300 mm single-wafer rapid thermal processing (spike anneal, ISSG oxidation, silicide anneals): halogen lamp head, multi-point pyrometry",
		[]ParamSpec{
			numP("peak_temp", "Peak Temperature", "°C", 1),
			numP("ramp_rate", "Ramp Rate", "°C/s", 0),
			numP("lamp_power", "Lamp Power", "kW", 1).W(">", 95),
			numP("o2_ppm", "Chamber O2", "ppm", 1).W(">", 12).C(">", 20),
			numP("pyro_offset", "Pyrometer Offset (vs reference)", "K", 2).W(">", 4).C(">", 6),
		},
		[]AlarmSpec{
			{"RTP-001", "Pyrometer offset above 6 K - anneal temperature off target", false, "SEN-01", gt("pyro_offset", 6, false)},
			{"RTP-002", "Chamber O2 above 20 ppm", false, "PRC-02", gt("o2_ppm", 20, false)},
			{"RTP-003", "Lamp power above 100 kW - lamp aging", false, "PRO-02", gt("lamp_power", 100, true)},
		},
		[]RelationSpec{
			{"Pyrometer Drift Detection", "RTP-010", [][]string{{"A:RTP-001"}, {"P:pyro_offset"}}},
			{"Lamp Aging Detection", "RTP-011", [][]string{{"A:RTP-003"}, {"P:lamp_power"}}},
		},
		[]FaultSpec{
			fault("pyrometer_drift", "Pyrometer calibration drift", "RTP-010", "RTP-001", "PM-RTP"),
			fault("lamp_aging", "Lamp head aging", "RTP-011", "RTP-003", "PM-RTP"),
		},
		TripSpec{"RTP-002", "o2_ppm", 60, "Chamber purge interlock - O2 too high"}, "PM-RTP", 6,
		map[string][]string{
			"RTP-001": {"pyro_offset", "peak_temp"}, "RTP-002": {"o2_ppm"}, "RTP-003": {"lamp_power"},
			"RTP-010": {"pyro_offset", "peak_temp"}, "RTP-011": {"lamp_power", "ramp_rate"},
		})
}

// implanterModel builds the medium and high current implanters (shared codes).
func implanterModel(hc bool) *ModelSpec {
	key, name, series, unit, low := "impmc", "Medium Current Implanter MC-300", "MC-300", "µA", 600.0
	desc := "300 mm medium current ion implanter (wells, threshold adjust, LDD / halo): RF ion source, analyser magnet, electrostatic scan, single-wafer end station"
	if hc {
		key, name, series, unit, low = "imphc", "High Current Implanter HC-300", "HC-300", "mA", 8
		desc = "300 mm high current ion implanter (source / drain, LDD): high-current ribbon beam, decel lens, single-wafer end station with plasma flood gun"
	}
	return toolModel(key, name, "Ion Implanter", series, desc,
		[]ParamSpec{
			numP("beam_current", "Beam Current", unit, 1).W("<", low*1.08).C("<", low),
			numP("beam_energy", "Beam Energy", "keV", 1),
			numP("dose_uniformity", "Dose Uniformity (1σ)", "%", 2).W(">", 1.0).C(">", 1.5),
			numP("source_hours", "Ion Source Hours", "h", 0).W(">", 280),
			numP("endstation_vacuum", "End Station Pressure", "µTorr", 2).W(">", 6),
			numP("arc_current", "Source Arc Current", "A", 2),
		},
		[]AlarmSpec{
			{"IMP-001", "Beam current low - ion source degrading", false, "PRO-07", lt("beam_current", low, true)},
			{"IMP-002", "Dose uniformity above 1.5 % - dose control out of specification", true, "PRO-07", gt("dose_uniformity", 1.5, true)},
			{"IMP-003", "End station pressure above 8 µTorr", false, "PRC-03", gt("endstation_vacuum", 8, false)},
			{"IMP-004", "Ion source hours above 330 h - source change due", false, "PRO-07", gt("source_hours", 330, false)},
		},
		[]RelationSpec{
			{"Ion Source Degradation", "IMP-010", [][]string{{"A:IMP-001"}, {"P:beam_current", "P:source_hours"}}},
			{"Dose Control Drift", "IMP-011", [][]string{{"A:IMP-002"}, {"P:dose_uniformity"}}},
		},
		[]FaultSpec{
			fault("source_degradation", "Ion source degradation", "IMP-010", "IMP-001", "PM-IMP"),
			fault("dose_drift", "Faraday / dose control drift", "IMP-011", "IMP-002", "PM-IMP"),
		},
		TripSpec{"IMP-003", "endstation_vacuum", 25, "End station vacuum interlock - beam off"}, "PM-IMP", 10,
		map[string][]string{
			"IMP-001": {"beam_current", "arc_current"}, "IMP-002": {"dose_uniformity", "beam_current"}, "IMP-003": {"endstation_vacuum"},
			"IMP-004": {"source_hours"}, "IMP-010": {"beam_current", "source_hours", "arc_current"}, "IMP-011": {"dose_uniformity"},
		})
}

func metToolModel() *ModelSpec {
	return toolModel("met", "Optical Metrology MX-300", "Metrology Tool", "MX-300",
		"300 mm optical metrology: spectroscopic ellipsometry film thickness, optical CD (scatterometry) and an integrated four-point probe for sheet resistance",
		[]ParamSpec{
			numP("measure_time", "Measurement Time per Wafer", "s", 1),
			numP("lamp_intensity", "Lamp Intensity", "%", 1).W("<", 85).C("<", 75),
			numP("stage_temp", "Stage Temperature", "°C", 2),
		},
		[]AlarmSpec{
			{"MET-001", "Lamp intensity below 75 % - measurement repeatability at risk", false, "PRO-05", lt("lamp_intensity", 75, false)},
			{"MET-002", "Stage temperature deviates more than 0.5 K from 23 °C", false, "THR-01", CondSpec{Param: "stage_temp", Op: "dev", Value: 0.5, SetPoint: 23}},
		},
		[]RelationSpec{{"Lamp Aging Detection", "MET-010", [][]string{{"A:MET-001"}, {"P:lamp_intensity"}}}},
		[]FaultSpec{fault("lamp_aging", "Light source aging", "MET-010", "MET-001", "PM-MET")},
		TripSpec{"MET-002", "stage_temp", 24.2, "Wafer stage temperature interlock - handling robot stopped"}, "PM-MET", 8,
		map[string][]string{"MET-001": {"lamp_intensity", "measure_time"}, "MET-002": {"stage_temp"}, "MET-010": {"lamp_intensity", "measure_time"}})
}

func cdsemToolModel() *ModelSpec {
	return toolModel("cds", "CD-SEM CS-300", "Metrology Tool", "CS-300",
		"300 mm critical-dimension scanning electron microscope: Schottky field emission column, pattern recognition, low-voltage imaging of resist and etched features",
		[]ParamSpec{
			numP("beam_current", "Probe Current", "pA", 1),
			numP("resolution", "Image Resolution", "nm", 2).W(">", 2.4).C(">", 2.8),
			numP("measure_time", "Measurement Time per Wafer", "s", 1),
			numP("column_vacuum", "Column Vacuum", "×1e-5 Pa", 2).W(">", 4),
			numP("stage_temp", "Stage Temperature", "°C", 2),
		},
		[]AlarmSpec{
			{"CDS-001", "Image resolution worse than 2.8 nm - CD repeatability at risk", false, "PRO-05", gt("resolution", 2.8, false)},
			{"CDS-002", "Column vacuum worse than 6e-5 Pa", false, "PRC-03", gt("column_vacuum", 6, false)},
			{"CDS-003", "Stage temperature deviates more than 0.5 K from 23 °C", false, "THR-01", CondSpec{Param: "stage_temp", Op: "dev", Value: 0.5, SetPoint: 23}},
		},
		[]RelationSpec{{"Electron Emitter Aging", "CDS-010", [][]string{{"A:CDS-001"}, {"P:resolution"}}}},
		[]FaultSpec{fault("emitter_aging", "Field emitter aging", "CDS-010", "CDS-001", "PM-CDS")},
		TripSpec{"CDS-002", "column_vacuum", 15, "Column vacuum interlock - high voltage off"}, "PM-CDS", 8,
		map[string][]string{"CDS-001": {"resolution", "beam_current"}, "CDS-002": {"column_vacuum"}, "CDS-003": {"stage_temp"}, "CDS-010": {"resolution", "measure_time"}})
}

func overlayToolModel() *ModelSpec {
	return toolModel("ovl", "Overlay Metrology OV-300", "Metrology Tool", "OV-300",
		"300 mm image-based overlay metrology (box-in-box and AIM targets) with tool-induced shift correction",
		[]ParamSpec{
			numP("measure_time", "Measurement Time per Wafer", "s", 1),
			numP("lamp_intensity", "Lamp Intensity", "%", 1).W("<", 85).C("<", 75),
			numP("tis", "Tool-Induced Shift", "nm", 2).W(">", 0.8).C(">", 1.0),
			numP("stage_temp", "Stage Temperature", "°C", 2),
		},
		[]AlarmSpec{
			{"OVL-001", "Lamp intensity below 75 %", false, "PRO-05", lt("lamp_intensity", 75, false)},
			{"OVL-002", "Tool-induced shift above 1.0 nm - overlay readings biased", false, "SEN-01", gt("tis", 1.0, false)},
		},
		[]RelationSpec{{"Overlay Tool Lamp Aging", "OVL-010", [][]string{{"A:OVL-001"}, {"A:OVL-002"}, {"P:lamp_intensity"}}}},
		[]FaultSpec{fault("lamp_aging", "Light source aging (TIS drift)", "OVL-010", "OVL-001", "PM-OVL")},
		TripSpec{"OVL-002", "tis", 4, "Stage alignment fault - measurement aborted"}, "PM-OVL", 6,
		map[string][]string{"OVL-001": {"lamp_intensity", "measure_time"}, "OVL-002": {"tis"}, "OVL-010": {"lamp_intensity", "tis"}})
}

func defectToolModel() *ModelSpec {
	return toolModel("def", "Darkfield Inspection DI-300", "Inspection Tool", "DI-300",
		"300 mm laser darkfield wafer inspection for particles and pattern defects (adders against the previous inspection)",
		[]ParamSpec{
			numP("laser_power", "Laser Power", "W", 2).W("<", 1.25).C("<", 1.1),
			numP("sensitivity", "Defect Sensitivity", "nm", 1).W(">", 37).C(">", 40),
			numP("scan_time", "Scan Time per Wafer", "s", 0),
			numP("stage_temp", "Stage Temperature", "°C", 2),
		},
		[]AlarmSpec{
			{"DEF-001", "Laser power below 1.1 W - capture rate dropping", false, "PRO-05", lt("laser_power", 1.1, false)},
			{"DEF-002", "Sensitivity worse than 40 nm", false, "PRO-05", gt("sensitivity", 40, false)},
		},
		[]RelationSpec{{"Inspection Laser Degradation", "DEF-010", [][]string{{"A:DEF-001"}, {"P:laser_power", "P:sensitivity"}}}},
		[]FaultSpec{fault("laser_degradation", "Laser degradation", "DEF-010", "DEF-001", "PM-DEF")},
		TripSpec{"DEF-002", "sensitivity", 70, "Optics autofocus fault - scan aborted"}, "PM-DEF", 6,
		map[string][]string{"DEF-001": {"laser_power", "sensitivity"}, "DEF-002": {"sensitivity", "scan_time"}, "DEF-010": {"laser_power", "sensitivity"}})
}

func watToolModel() *ModelSpec {
	return toolModel("wat", "Parametric Tester PT-300", "Parametric Test System", "PT-300",
		"Wafer acceptance test: parametric tester with SMUs and a 300 mm prober measuring scribe-line test structures (Vt, Idsat, leakage, contact / via chains, sheet resistance)",
		[]ParamSpec{
			numP("contact_res", "Probe Contact Resistance", "Ω", 2).W(">", 2.4).C(">", 3),
			numP("chuck_temp", "Chuck Temperature", "°C", 2),
			numP("test_time", "Test Time per Wafer", "s", 0),
			numP("smu_leakage", "SMU Leakage", "pA", 1),
			numP("touchdowns", "Touchdowns Since Card Clean", "k", 2),
		},
		[]AlarmSpec{
			{"WAT-001", "Probe contact resistance above 3 Ω - false fails likely", false, "PRO-03", gt("contact_res", 3, false)},
			{"WAT-002", "Chuck temperature deviates more than 1 K from 25 °C", false, "THR-01", CondSpec{Param: "chuck_temp", Op: "dev", Value: 1, SetPoint: 25}},
		},
		[]RelationSpec{{"Probe Card Wear", "WAT-010", [][]string{{"A:WAT-001"}, {"P:contact_res"}}}},
		[]FaultSpec{fault("probe_card_wear", "Probe card tip wear and contamination", "WAT-010", "WAT-001", "PM-WAT")},
		TripSpec{"WAT-002", "chuck_temp", 31, "Chuck temperature interlock - test aborted"}, "PM-WAT", 6,
		map[string][]string{"WAT-001": {"contact_res", "touchdowns"}, "WAT-002": {"chuck_temp"}, "WAT-010": {"contact_res", "touchdowns"}})
}

// scannerModel builds the exposure tools: ArF immersion and KrF scanners, i-line stepper.
func scannerModel(kind string) *ModelSpec {
	key, name, series, desc := kind, "", "", ""
	focus, ovl, dose := 30.0, 5.5, 30.0
	switch kind {
	case "scanarf":
		name, series = "ArF Immersion Scanner SC-1950i", "SC-1950i"
		desc = "193 nm immersion step-and-scan system (NA 1.35) for the critical layers: dual wafer stages, immersion hood, ArF excimer laser"
	case "scankrf":
		name, series, focus, ovl, dose = "KrF Scanner SC-860", "SC-860", 60, 9, 28
		desc = "248 nm step-and-scan system (NA 0.80) for semi-critical and thick-metal layers: dual stages, KrF excimer laser"
	default:
		name, series, focus, ovl, dose = "i-line Stepper ST-365", "ST-365", 120, 16, 140
		desc = "365 nm i-line step-and-repeat system for wells and pad layers: mercury arc lamp"
	}
	params := []ParamSpec{
		numP("dose", "Exposure Dose", "mJ/cm²", 2),
		numP("focus_offset", "Focus Offset", "nm", 1).W(">", focus*.8).C(">", focus),
		numP("overlay_residual", "Stage Overlay Residual", "nm", 2).W(">", ovl*.8).C(">", ovl),
		numP("lens_temp", "Projection Lens Temperature", "°C", 3),
		numP("stage_temp", "Wafer Stage Temperature", "°C", 3),
	}
	alarms := []AlarmSpec{
		{"SCN-001", "Focus offset beyond the process window", false, "PRO-06", gt("focus_offset", focus, false)},
		{"SCN-002", "Stage overlay residual high - overlay at risk", false, "PRO-06", gt("overlay_residual", ovl, false)},
		{"SCN-003", "Wafer stage temperature deviates more than 0.05 K from 22 °C", false, "THR-01", CondSpec{Param: "stage_temp", Op: "dev", Value: 0.05, SetPoint: 22}},
	}
	rels := []RelationSpec{
		{"Lens Heating / Focus Drift", "SCN-010", [][]string{{"A:SCN-001"}, {"P:focus_offset"}}},
		{"Wafer Stage Drift", "SCN-012", [][]string{{"A:SCN-002"}, {"P:overlay_residual"}}},
	}
	faults := []FaultSpec{
		fault("lens_heating", "Lens heating / focus drift", "SCN-010", "SCN-001", "PM-SCN"),
		fault("stage_drift", "Wafer stage interferometer drift", "SCN-012", "SCN-002", "PM-SCN"),
	}
	mapping := map[string][]string{"SCN-001": {"focus_offset", "lens_temp"}, "SCN-002": {"overlay_residual", "stage_temp"},
		"SCN-003": {"stage_temp"}, "SCN-010": {"focus_offset", "lens_temp", "dose"}, "SCN-012": {"overlay_residual", "stage_temp"}}
	if kind == "stepper" {
		params = append(params, numP("lamp_intensity", "Mercury Lamp Intensity", "%", 1).W("<", 85).C("<", 75),
			numP("lamp_hours", "Lamp Hours", "h", 0).W(">", 800))
		alarms = append(alarms, AlarmSpec{"SCN-004", "Exposure source output low", false, "PRO-06", lt("lamp_intensity", 75, false)})
		rels = append(rels, RelationSpec{"Mercury Lamp Aging", "SCN-013", [][]string{{"A:SCN-004"}, {"P:lamp_intensity"}}})
		faults = []FaultSpec{fault("lamp_aging", "Mercury lamp aging", "SCN-013", "SCN-004", "PM-SCN"), faults[1]}
		mapping["SCN-004"], mapping["SCN-013"] = []string{"lamp_intensity", "dose"}, []string{"lamp_intensity", "lamp_hours", "dose"}
	} else {
		params = append(params, numP("laser_energy", "Laser Pulse Energy", "mJ", 2).W("<", 9).C("<", 8),
			numP("immersion_temp", "Immersion Water Temperature", "°C", 3))
		alarms = append(alarms, AlarmSpec{"SCN-004", "Exposure source output low", false, "PRO-06", lt("laser_energy", 8, false)})
		rels = append(rels, RelationSpec{"Excimer Laser Degradation", "SCN-011", [][]string{{"A:SCN-004"}, {"P:laser_energy"}}})
		faults = append(faults, fault("laser_degradation", "Excimer laser chamber degradation", "SCN-011", "SCN-004", "PM-SCN"))
		mapping["SCN-004"], mapping["SCN-011"] = []string{"laser_energy", "dose"}, []string{"laser_energy", "dose"}
	}
	_ = dose
	return toolModel(key, name, "Lithography Scanner", series, desc, params, alarms, rels, faults,
		TripSpec{"SCN-003", "stage_temp", 22.4, "Stage temperature interlock - exposure stopped"}, "PM-SCN", 12, mapping)
}

func trackToolModel() *ModelSpec {
	return toolModel("track", "Coat/Develop Track TK-300", "Litho Track", "TK-300",
		"300 mm resist coat / bake / develop track linked in-line to its exposure tool: coater and developer cups, hot plates, chill plates, wafer robot",
		[]ParamSpec{
			numP("resist_temp", "Resist Temperature", "°C", 2),
			numP("peb_temp", "Post-Exposure Bake Temperature", "°C", 2),
			numP("dev_temp", "Developer Temperature", "°C", 2),
			numP("cup_exhaust", "Coater Cup Exhaust", "Pa", 0).W("<", 32).C("<", 30),
			numP("humidity", "Cell Humidity", "%", 1),
		},
		[]AlarmSpec{
			{"TRK-001", "PEB temperature deviates more than 0.8 K from 110 °C - CD at risk", false, "PRO-06", CondSpec{Param: "peb_temp", Op: "dev", Value: 0.8, SetPoint: 110}},
			{"TRK-002", "Coater cup exhaust below 30 Pa", false, "AIR-02", lt("cup_exhaust", 30, false)},
			{"TRK-003", "Cell humidity deviates more than 3 % from 45 %", false, "AIR-03", CondSpec{Param: "humidity", Op: "dev", Value: 3, SetPoint: 45}},
		},
		[]RelationSpec{
			{"Hot Plate Drift Detection", "TRK-010", [][]string{{"A:TRK-001"}}},
			{"Dispense Nozzle Contamination", "TRK-011", [][]string{{"A:TRK-002"}, {"P:cup_exhaust"}}},
		},
		[]FaultSpec{
			fault("hotplate_drift", "PEB hot plate drift", "TRK-010", "TRK-001", "PM-TRK"),
			fault("nozzle_contamination", "Resist dispense nozzle contamination", "TRK-011", "TRK-002", "PM-TRK"),
		},
		TripSpec{"TRK-001", "peb_temp", 118, "Hot plate over-temperature - track stopped"}, "PM-TRK", 8,
		map[string][]string{"TRK-001": {"peb_temp"}, "TRK-002": {"cup_exhaust", "humidity"}, "TRK-003": {"humidity"},
			"TRK-010": {"peb_temp"}, "TRK-011": {"cup_exhaust"}})
}

func swcToolModel() *ModelSpec {
	return toolModel("swc", "Single-Wafer Clean SW-300", "Wet Clean Station", "SW-300",
		"300 mm single-wafer spin clean (post-etch polymer removal, pre-metal cleans): 4 chambers with chemical and DI rinse nozzles, N2 dry",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("chem_temp", "Chemical Temperature", "°C", 1),
			numP("chem_flow", "Chemical Flow", "l/min", 2).W("<", 1.25).C("<", 1.1),
			numP("spin_speed", "Spin Speed", "rpm", 0),
			numP("particles", "Liquid Particle Count", "count/ml", 0).W(">", 20).C(">", 30),
			numP("di_resistivity", "DI Water Resistivity", "MΩ·cm", 2).W("<", 17.6),
		},
		[]AlarmSpec{
			{"SWC-001", "Chemical flow below 1.1 l/min - clean incomplete", false, "PRO-04", lt("chem_flow", 1.1, true)},
			{"SWC-002", "Liquid particle count above 30 /ml", false, "PRO-04", gt("particles", 30, false)},
			{"SWC-003", "DI water resistivity below 17.0 MΩ·cm", false, "WTR-03", lt("di_resistivity", 17, false)},
		},
		[]RelationSpec{{"Dispense Nozzle Clogging", "SWC-010", [][]string{{"A:SWC-001"}, {"A:SWC-002"}, {"P:chem_flow", "P:particles"}}}},
		[]FaultSpec{fault("nozzle_clog", "Dispense nozzle / filter clogging", "SWC-010", "SWC-001", "PM-SWC")},
		TripSpec{"SWC-001", "chem_flow", 0.2, "Chemical supply interlock - chamber stopped"}, "PM-SWC", 6,
		map[string][]string{"SWC-001": {"chem_flow"}, "SWC-002": {"particles"}, "SWC-003": {"di_resistivity"}, "SWC-010": {"chem_flow", "particles"}})
}

func asherToolModel() *ModelSpec {
	return toolModel("ash", "Plasma Asher AS-300", "Asher", "AS-300",
		"300 mm downstream plasma resist strip (post-implant and rework strips): 2 chambers, O2 / N2H2 chemistry, heated chuck",
		[]ParamSpec{
			numP("chambers_up", "Chambers Available", "", 0),
			numP("rf_power", "RF Power", "W", 0),
			numP("o2_flow", "O2 Flow", "sccm", 0),
			numP("chuck_temp", "Chuck Temperature", "°C", 1),
			numP("strip_rate", "Strip Rate", "µm/min", 2).W("<", 3.4).C("<", 3),
			numP("chamber_pressure", "Chamber Pressure", "Torr", 2),
		},
		[]AlarmSpec{
			{"ASH-001", "Strip rate below 3 µm/min - resist residue risk", false, "PRO-01", lt("strip_rate", 3, true)},
			{"ASH-002", "Chuck temperature deviates more than 15 K from 250 °C", false, "THR-01", dev("chuck_temp", 250, 15)},
			{"ASH-003", "Chamber pressure deviates more than 0.4 Torr from 1.0 Torr - check the foreline vacuum", false, "PRC-03", dev("chamber_pressure", 1, 0.4)},
		},
		[]RelationSpec{{"Strip Rate Degradation", "ASH-010", [][]string{{"A:ASH-001"}, {"P:strip_rate"}}}},
		[]FaultSpec{fault("strip_rate_low", "Plasma source degradation (low strip rate)", "ASH-010", "ASH-001", "PM-ASH")},
		TripSpec{"ASH-002", "chuck_temp", 290, "Chuck over-temperature interlock"}, "PM-ASH", 6,
		map[string][]string{"ASH-001": {"strip_rate", "rf_power"}, "ASH-002": {"chuck_temp"}, "ASH-003": {"chamber_pressure"}, "ASH-010": {"strip_rate", "rf_power"}})
}

func wetToolModel() *ModelSpec {
	return toolModel("wet", "Batch Wet Clean WB-300", "Wet Clean Station", "WB-300",
		"300 mm batch wet bench (2 lots per batch): SC1 / SC2 / SPM / HF / hot phosphoric tanks, quick dump rinses, spin rinse dryer, behind a lifting sash",
		[]ParamSpec{
			numP("bath_temp", "Bath Temperature", "°C", 1),
			numP("chem_conc", "Chemical Concentration", "%", 1).W("<", 85).C("<", 80),
			numP("bath_age", "Bath Age", "h", 1),
			numP("particles", "Bath Particle Count", "count/ml", 0).W(">", 25).C(">", 40),
			numP("di_resistivity", "DI Water Resistivity", "MΩ·cm", 2).W("<", 17.6),
		},
		[]AlarmSpec{
			{"WET-001", "Bath temperature deviates more than 2.5 K from 65 °C", false, "THR-01", CondSpec{Param: "bath_temp", Op: "dev", Value: 2.5, SetPoint: 65}},
			{"WET-002", "Chemical concentration below 80 % - bath change due", false, "PRO-04", lt("chem_conc", 80, false)},
			{"WET-003", "Bath particle count above 40 /ml", false, "PRO-04", gt("particles", 40, false)},
			{"WET-004", "DI water resistivity below 17.0 MΩ·cm", false, "WTR-03", lt("di_resistivity", 17, false)},
		},
		[]RelationSpec{{"Recirculation Filter Clogging", "WET-010", [][]string{{"A:WET-003"}, {"P:particles"}}}},
		[]FaultSpec{fault("filter_clog", "Recirculation filter clogging", "WET-010", "WET-003", "PM-WET")},
		TripSpec{"WET-001", "bath_temp", 72.5, "Bath over-temperature interlock"}, "PM-WET", 8,
		map[string][]string{"WET-001": {"bath_temp"}, "WET-002": {"chem_conc", "bath_age"}, "WET-003": {"particles", "bath_age"},
			"WET-004": {"di_resistivity"}, "WET-010": {"particles", "bath_age"}})
}

// processToolModels lists every process tool model.
func processToolModels() []*ModelSpec {
	return []*ModelSpec{
		scannerModel("scanarf"), scannerModel("scankrf"), scannerModel("stepper"), trackToolModel(),
		etchToolModel(), dielectricEtchModel(), cvdToolModel(), pvdToolModel(), ecpToolModel(), cmpToolModel(),
		furnaceToolModel(), rtpToolModel(), implanterModel(false), implanterModel(true),
		wetToolModel(), swcToolModel(), asherToolModel(),
		metToolModel(), cdsemToolModel(), overlayToolModel(), defectToolModel(), watToolModel(),
	}
}

// ---------------------------------------------------------------------------
// Telemetry

// tctx is what a tool parameter is computed from.
type tctx struct {
	a    *Asset
	ft   *FabTool
	t    time.Time
	dt   float64
	busy bool // processing
	up   bool // available (not stopped)
	chUp float64
}

func (c *tctx) sev(kind string) float64 { return c.a.sev(kind) }

// life returns a scheduled-maintenance counter as % of its interval.
func (c *tctx) life(reason string) float64 {
	if c.ft == nil || c.ft.LifeAt[reason] <= 0 {
		return 0
	}
	return 100 * c.ft.Life[reason] / c.ft.LifeAt[reason]
}

func (c *tctx) rfh() float64 {
	if c.ft == nil {
		return 0
	}
	return c.ft.maxRFH(c.t)
}

func (c *tctx) waferS(def float64) float64 {
	if c.busy && c.ft.Run != nil {
		return c.ft.Run.WaferS
	}
	return def
}

// tp is a tool parameter: a nominal value while processing and idle, noise,
// fault effects (added at severity 1), or a formula.
type tp struct {
	alias      string
	idle, busy float64
	sigma, tau float64
	fx         map[string]float64
	hold       bool
	fn         func(c *tctx) float64
}

func (p tp) value(c *tctx) float64 {
	if p.fn != nil {
		return p.fn(c)
	}
	v := p.idle
	if c.busy {
		v = p.busy
	}
	for k, d := range p.fx {
		v += d * c.sev(k)
	}
	if p.sigma > 0 {
		tau := p.tau
		if tau == 0 {
			tau = 120
		}
		v += c.a.n(p.alias, p.sigma, tau, c.dt)
	}
	if p.hold {
		return c.a.hold("h_"+p.alias, v, p.busy, c.busy)
	}
	return v
}

var pumpCoupled = func(base, gain, idle float64, sigma float64) func(c *tctx) float64 {
	return func(c *tctx) float64 {
		pin := c.a.pumpInlet()
		if c.busy {
			return base + gain*math.Max(0, pin-0.15) + c.a.n("cp", sigma, 60, c.dt)
		}
		return idle + gain/2*math.Max(0, pin-0.15) + math.Abs(c.a.n("cp", sigma/4, 60, c.dt))
	}
}

var chambersUp = tp{alias: "chambers_up", fn: func(c *tctx) float64 { return c.chUp }}

// toolPhysics are the parameter formulas by model key.
var toolPhysics = map[string][]tp{
	"etch": {
		chambersUp,
		{alias: "rf_source_power", busy: 1500, sigma: 6, tau: 30},
		{alias: "rf_reflected", fn: func(c *tctx) float64 {
			if !c.busy {
				return 0
			}
			return math.Max(0, 6+55*c.sev("rf_match")+c.a.n("refl", .8, 60, c.dt))
		}},
		{alias: "chamber_pressure", fn: pumpCoupled(20, 6, .8, .25)},
		{alias: "esc_temp", idle: 40, busy: 40, sigma: .2, fx: map[string]float64{"esc_leak": 3.5}},
		{alias: "he_flow", busy: 12, sigma: .25, fx: map[string]float64{"esc_leak": 9}, fn: func(c *tctx) float64 {
			if !c.busy {
				return 0
			}
			return 12 + 9*c.sev("esc_leak") + c.a.n("he", .25, 120, c.dt)
		}},
		{alias: "endpoint_time", idle: 62, busy: 62, sigma: 1.2, tau: 300, fx: map[string]float64{"rf_match": 18.6}, hold: true},
		{alias: "rf_hours", fn: func(c *tctx) float64 { return c.rfh() }},
	},
	"etchd": {
		chambersUp,
		{alias: "rf_hf_power", busy: 1800, sigma: 8, tau: 30},
		{alias: "rf_lf_power", busy: 2500, sigma: 10, tau: 30},
		{alias: "rf_reflected", fn: func(c *tctx) float64 {
			if !c.busy {
				return 0
			}
			return math.Max(0, 8+60*c.sev("rf_match")+c.a.n("refl", 1, 60, c.dt))
		}},
		{alias: "chamber_pressure", fn: pumpCoupled(35, 8, 1.0, .3)},
		{alias: "wall_temp", idle: 60, busy: 60, sigma: .4, fx: map[string]float64{"focus_ring_wear": 6}},
		{alias: "endpoint_time", idle: 75, busy: 75, sigma: 1.5, tau: 300, fx: map[string]float64{"rf_match": 22, "focus_ring_wear": 6}, hold: true},
		{alias: "rf_hours", fn: func(c *tctx) float64 { return c.rfh() }},
	},
	"cvd": {
		chambersUp,
		{alias: "heater_temp", idle: 400, busy: 400, sigma: .35, fx: map[string]float64{"heater_drift": -9}},
		{alias: "chamber_pressure", fn: func(c *tctx) float64 {
			pin := c.a.pumpInlet()
			if c.busy {
				return 2.5 + 0.4*math.Max(0, pin-0.15) + c.a.n("cp", .012, 60, c.dt)
			}
			return 0.01 + math.Abs(c.a.n("cp", .003, 60, c.dt))
		}},
		{alias: "rf_power", busy: 800, sigma: 4, tau: 30},
		{alias: "teos_flow", fn: func(c *tctx) float64 {
			if !c.busy {
				return 0
			}
			return 1200*(1-0.08*c.sev("showerhead_clog")) + c.a.n("teos", 6, 60, c.dt)
		}},
		{alias: "dep_rate", fn: func(c *tctx) float64 {
			film := c.rfh() * filmPerRFH
			return c.a.hold("dep", 5.0*(1-0.07*c.sev("showerhead_clog")-0.02*c.sev("heater_drift")-0.0006*film)+c.a.n("dr", .02, 300, c.dt), 5, c.busy)
		}},
		{alias: "film_buildup", fn: func(c *tctx) float64 { return c.rfh() * filmPerRFH }},
	},
	"pvd": {
		chambersUp,
		{alias: "dc_power", busy: 20, sigma: .1, tau: 30},
		{alias: "chamber_pressure", fn: func(c *tctx) float64 {
			if c.busy {
				return 3 + 2*math.Max(0, c.a.pumpInlet()-0.15) + c.a.n("cp", .05, 60, c.dt)
			}
			return 0.001
		}},
		{alias: "base_pressure", idle: .5, busy: .6, sigma: .04, tau: 600, fx: map[string]float64{"cryo_degradation": 6}},
		{alias: "target_life", fn: func(c *tctx) float64 { return c.life("Target change") }},
		{alias: "substrate_temp", idle: 25, busy: 200, sigma: 1.5},
		{alias: "dep_rate", fn: func(c *tctx) float64 {
			return c.a.hold("dr", 12*(1-0.15*c.life("Target change")/100-0.3*c.sev("shield_flaking"))+c.a.n("dr", .1, 300, c.dt), 12, c.busy)
		}},
	},
	"ecp": {
		chambersUp,
		{alias: "plating_current", idle: 12, busy: 12, sigma: .05, fx: map[string]float64{"anode_passivation": -2.5}, hold: true},
		{alias: "cu_conc", idle: 50, busy: 50, sigma: .6, tau: 3600},
		{alias: "acid_conc", idle: 10, busy: 10, sigma: .15, tau: 3600},
		{alias: "accelerator", idle: 6, busy: 6, sigma: .08, tau: 1800, fx: map[string]float64{"additive_depletion": -2.2}},
		{alias: "bath_temp", idle: 25, busy: 25, sigma: .12, tau: 600},
		{alias: "anode_life", fn: func(c *tctx) float64 { return c.life("Anode change") }},
	},
	"cmp": {
		{alias: "platen_speed", busy: 93, sigma: .3, tau: 30},
		{alias: "head_pressure", busy: 2.5, sigma: .02, tau: 30},
		{alias: "slurry_flow", fn: func(c *tctx) float64 {
			// the flow of the last polish (the slurry is off between runs)
			return c.a.hold("sf", 200-90*c.sev("slurry_flow_low")+c.a.n("sf", 3, 60, c.dt), 200, c.busy)
		}},
		{alias: "removal_rate", fn: func(c *tctx) float64 {
			rr := 300*(1-0.35*c.sev("pad_glazing")-0.1*c.life("Pad change")/100) + c.a.n("rr", 4, 300, c.dt)
			return c.a.hold("rr", rr, 300, c.busy)
		}},
		{alias: "pad_temp", idle: 26, busy: 42, sigma: .5, fx: map[string]float64{"pad_glazing": 6}},
		{alias: "pad_life", fn: func(c *tctx) float64 { return c.life("Pad change") }},
	},
	"furn": {
		{alias: "tube_temp", fn: func(c *tctx) float64 {
			target := 650.0
			if c.busy {
				target = 900
				if c.ft != nil && c.ft.Run != nil && c.ft.Run.Recipe != "" {
					target = recipeTemp(c.ft.Run.Recipe)
				}
			}
			return c.a.lag("tt", target, 900, c.dt) + c.a.n("tt", .3, 300, c.dt)
		}},
		{alias: "temp_uniformity", idle: .5, busy: .8, sigma: .05, fx: map[string]float64{"heater_zone_drift": 2.6}},
		{alias: "gas_flow", busy: 10, sigma: .1, tau: 60},
		{alias: "tube_pressure", fn: func(c *tctx) float64 {
			if c.ft != nil && contains(c.ft.Quals, stFurnLP) {
				if c.busy {
					return 0.25 + 0.5*math.Max(0, c.a.pumpInlet()-0.15) + c.a.n("tp", .005, 60, c.dt)
				}
				return 0.05
			}
			return 760 + c.a.n("tp", .5, 600, c.dt)
		}},
		{alias: "boat_load", fn: func(c *tctx) float64 {
			if c.busy && c.ft.Run != nil {
				return float64(len(c.ft.Run.Lots))
			}
			return 0
		}},
		{alias: "monitor_adders", fn: func(c *tctx) float64 {
			// particle monitor wafers of the boat (refreshed with every run)
			return c.a.hold("ma", math.Max(0, 3+25*c.sev("particle_flaking")+c.a.n("ma", 1.2, 600, c.dt)), 3, c.busy)
		}},
		{alias: "tube_deposit", fn: func(c *tctx) float64 {
			if c.ft == nil || !contains(c.ft.Quals, stFurnLP) {
				return 0
			}
			return c.life("Tube clean") * 0.2
		}},
	},
	"rtp": {
		{alias: "peak_temp", fn: func(c *tctx) float64 {
			if !c.busy {
				return c.a.lag("pk", 30, 60, c.dt)
			}
			target := 1000.0
			if c.ft.Run != nil {
				target = recipeTemp(c.ft.Run.Recipe)
			}
			return target - 12*c.sev("pyrometer_drift") + c.a.n("pk", 1.2, 30, c.dt)
		}},
		{alias: "ramp_rate", busy: 150, sigma: 1.5, hold: true},
		{alias: "lamp_power", busy: 85, sigma: .6, fx: map[string]float64{"lamp_aging": 18}, hold: true},
		{alias: "o2_ppm", idle: 4, busy: 4, sigma: .5, tau: 600},
		{alias: "pyro_offset", idle: 0, busy: 0, sigma: .3, tau: 1200, fx: map[string]float64{"pyrometer_drift": 9}},
	},
	"impmc": implanterPhysics(900, 180),
	"imphc": implanterPhysics(12, 20),
	"wet": {
		{alias: "bath_temp", idle: 65, busy: 65, sigma: .12, tau: 300},
		{alias: "chem_conc", fn: func(c *tctx) float64 { return 100 - 0.55*c.bathAge() + c.a.n("cc", .3, 600, c.dt) }},
		{alias: "bath_age", fn: func(c *tctx) float64 { return c.bathAge() }},
		{alias: "particles", fn: func(c *tctx) float64 {
			return math.Max(0, 4+0.45*c.bathAge()+70*c.sev("filter_clog")+c.a.n("pc", .6, 300, c.dt))
		}},
		{alias: "di_resistivity", idle: 18.1, busy: 18.1, sigma: .04, tau: 600},
	},
	"swc": {
		chambersUp,
		{alias: "chem_temp", idle: 25, busy: 45, sigma: .3},
		{alias: "chem_flow", fn: func(c *tctx) float64 {
			// the flow of the last clean (dispense off between runs)
			return c.a.hold("cf", 1.5-0.5*c.sev("nozzle_clog")+c.a.n("cf", .02, 60, c.dt), 1.5, c.busy)
		}},
		{alias: "spin_speed", busy: 1500, sigma: 5, tau: 30},
		{alias: "particles", idle: 3, busy: 4, sigma: .5, tau: 300, fx: map[string]float64{"nozzle_clog": 32}},
		{alias: "di_resistivity", idle: 18.1, busy: 18.1, sigma: .04, tau: 600},
	},
	"ash": {
		chambersUp,
		{alias: "rf_power", busy: 2500, sigma: 10, tau: 30},
		{alias: "o2_flow", busy: 4000, sigma: 15, tau: 60},
		{alias: "chuck_temp", idle: 250, busy: 250, sigma: 1, tau: 300},
		{alias: "strip_rate", idle: 4, busy: 4, sigma: .04, tau: 600, fx: map[string]float64{"strip_rate_low": -1.6}, hold: true},
		{alias: "chamber_pressure", fn: pumpCoupled(1.0, 1.5, 0.05, .02)},
	},
	"met": {
		{alias: "measure_time", fn: func(c *tctx) float64 {
			return c.a.hold("mt", c.waferS(45)*(1+0.5*c.sev("lamp_aging"))+c.a.n("mt", .4, 120, c.dt), 45, c.busy)
		}},
		{alias: "lamp_intensity", idle: 100, busy: 100, sigma: .3, tau: 300, fx: map[string]float64{"lamp_aging": -30}},
		{alias: "stage_temp", idle: 23, busy: 23, sigma: .03, tau: 300},
	},
	"cds": {
		{alias: "beam_current", idle: 8, busy: 8, sigma: .05, fx: map[string]float64{"emitter_aging": -2}},
		{alias: "resolution", idle: 1.8, busy: 1.8, sigma: .03, tau: 600, fx: map[string]float64{"emitter_aging": 1.3}},
		{alias: "measure_time", fn: func(c *tctx) float64 {
			return c.a.hold("mt", c.waferS(120)*(1+0.4*c.sev("emitter_aging"))+c.a.n("mt", 1, 120, c.dt), 120, c.busy)
		}},
		{alias: "column_vacuum", idle: 2, busy: 2.1, sigma: .05, tau: 600},
		{alias: "stage_temp", idle: 23, busy: 23, sigma: .03, tau: 300},
	},
	"ovl": {
		{alias: "measure_time", fn: func(c *tctx) float64 {
			return c.a.hold("mt", c.waferS(75)*(1+0.3*c.sev("lamp_aging"))+c.a.n("mt", .6, 120, c.dt), 75, c.busy)
		}},
		{alias: "lamp_intensity", idle: 100, busy: 100, sigma: .3, tau: 300, fx: map[string]float64{"lamp_aging": -30}},
		{alias: "tis", idle: .3, busy: .3, sigma: .03, tau: 600, fx: map[string]float64{"lamp_aging": .9}},
		{alias: "stage_temp", idle: 23, busy: 23, sigma: .03, tau: 300},
	},
	"def": {
		{alias: "laser_power", idle: 1.5, busy: 1.5, sigma: .01, tau: 600, fx: map[string]float64{"laser_degradation": -.55}},
		{alias: "sensitivity", idle: 32, busy: 32, sigma: .3, tau: 600, fx: map[string]float64{"laser_degradation": 11}},
		{alias: "scan_time", fn: func(c *tctx) float64 {
			return c.a.hold("st", c.waferS(420)*(1+0.3*c.sev("laser_degradation"))+c.a.n("st", 3, 120, c.dt), 420, c.busy)
		}},
		{alias: "stage_temp", idle: 23, busy: 23, sigma: .03, tau: 300},
	},
	"wat": {
		{alias: "contact_res", fn: func(c *tctx) float64 {
			return 1.2 + 0.008*c.life("Probe card cleaning") + 2.4*c.sev("probe_card_wear") + c.a.n("cr", .05, 300, c.dt)
		}},
		{alias: "chuck_temp", idle: 25, busy: 25, sigma: .08, tau: 300},
		{alias: "test_time", fn: func(c *tctx) float64 {
			return c.a.hold("tt", c.waferS(360)*(1+0.3*c.sev("probe_card_wear"))+c.a.n("tt", 4, 120, c.dt), 360, c.busy)
		}},
		{alias: "smu_leakage", idle: 5, busy: 5, sigma: .3, tau: 600},
		{alias: "touchdowns", fn: func(c *tctx) float64 {
			if c.ft == nil {
				return 0
			}
			return c.ft.Life["Probe card cleaning"] * 9 / 1000
		}},
	},
	"scanarf": scannerPhysics(30, 1, true),
	"scankrf": scannerPhysics(28, 2, true),
	"stepper": scannerPhysics(140, 4, false),
	"track": {
		{alias: "resist_temp", idle: 23, busy: 23, sigma: .02, tau: 300},
		{alias: "peb_temp", idle: 110, busy: 110, sigma: .08, tau: 300, fx: map[string]float64{"hotplate_drift": 1.2}},
		{alias: "dev_temp", idle: 23, busy: 23, sigma: .03, tau: 300},
		{alias: "cup_exhaust", idle: 40, busy: 40, sigma: .5, tau: 300, fx: map[string]float64{"nozzle_contamination": -11}},
		{alias: "humidity", idle: 45, busy: 45, sigma: .3, tau: 1800},
	},
}

func implanterPhysics(beam, energy float64) []tp {
	return []tp{
		{alias: "beam_current", fn: func(c *tctx) float64 {
			v := beam * (1 - 0.4*c.sev("source_degradation") - 0.08*c.life("Ion source change")/100)
			return c.a.hold("bc", v*(1+c.a.n("bcn", .01, 120, c.dt)), beam, c.busy)
		}},
		{alias: "beam_energy", busy: energy, sigma: energy * .002, hold: true},
		{alias: "dose_uniformity", idle: .5, busy: .5, sigma: .03, tau: 300, fx: map[string]float64{"dose_drift": 1.2}, hold: true},
		{alias: "source_hours", fn: func(c *tctx) float64 {
			if c.ft == nil {
				return 0
			}
			return c.ft.Life["Ion source change"]
		}},
		{alias: "endstation_vacuum", fn: func(c *tctx) float64 {
			return 2 + 6*math.Max(0, c.a.pumpInlet()-0.15) + b2f(c.busy)*0.6 + c.a.n("ev", .1, 300, c.dt)
		}},
		{alias: "arc_current", idle: 0, busy: 2.5, sigma: .03, fx: map[string]float64{"source_degradation": .8}},
	}
}

func scannerPhysics(dose float64, ovl float64, laser bool) []tp {
	ps := []tp{
		{alias: "dose", busy: dose, sigma: dose * .004, tau: 60, fx: map[string]float64{"laser_degradation": -dose * .02, "lamp_aging": -dose * .03}, hold: true},
		{alias: "focus_offset", idle: 4, busy: 4, sigma: 1.5, tau: 600, fx: map[string]float64{"lens_heating": 30}},
		{alias: "overlay_residual", idle: ovl, busy: ovl, sigma: ovl * .08, tau: 600, fx: map[string]float64{"stage_drift": 6}},
		{alias: "lens_temp", idle: 22, busy: 22.003, sigma: .001, tau: 600, fx: map[string]float64{"lens_heating": .02}},
		{alias: "stage_temp", idle: 22, busy: 22, sigma: .004, tau: 300},
	}
	if laser {
		ps = append(ps, tp{alias: "laser_energy", idle: 10, busy: 10, sigma: .05, tau: 300, fx: map[string]float64{"laser_degradation": -2.6}},
			tp{alias: "immersion_temp", idle: 22, busy: 22, sigma: .003, tau: 300})
	} else {
		ps = append(ps, tp{alias: "lamp_intensity", fn: func(c *tctx) float64 {
			return 100 - 0.012*c.life("Mercury lamp change")*8 - 30*c.sev("lamp_aging") + c.a.n("li", .3, 300, c.dt)
		}}, tp{alias: "lamp_hours", fn: func(c *tctx) float64 {
			if c.ft == nil {
				return 0
			}
			return c.ft.Life["Mercury lamp change"]
		}})
	}
	return ps
}

func (c *tctx) bathAge() float64 {
	if c.ft == nil {
		return 0
	}
	return c.ft.Life["Bath change"]
}

// recipeTemp is the process temperature of a thermal recipe (°C).
func recipeTemp(recipe string) float64 {
	for k, v := range map[string]float64{"PADOX": 1000, "LPSIN": 780, "LINER": 1050, "DENSIFY": 1000, "WELL": 1100, "GOX": 850,
		"POLY": 620, "ALLOY": 400, "ISSG": 1050, "SPIKE": 1050, "RTA1": 280, "RTA2": 450} {
		if containsStr(recipe, k) {
			return v
		}
	}
	return 900
}

func containsStr(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// pumpInlet is the inlet pressure of the tool's dry pump (mbar; the pumps
// step before the tools).
func (a *Asset) pumpInlet() float64 {
	if a.vacPump != nil && a.vacPump.last != nil {
		return a.vacPump.last["inlet_press"]
	}
	return 0.12
}

// hold keeps the last value of a reading that only updates while processing.
func (a *Asset) hold(key string, v, def float64, busy bool) float64 {
	if busy {
		a.smooth[key] = v
		return v
	}
	if x, ok := a.smooth[key]; ok {
		return x
	}
	return def
}

// processTool reports a process tool from its production state: running =
// processing (productive), trip = equipment trip, a support unit down
// (dry pump, abatement, process chiller) or an assist / engineering hold,
// maintenance mode = maintenance, a scheduled stop of a support unit or a
// scheduled stop of the tool (calibration, consumable change). A litho track
// follows its cell.
func (a *Asset) processTool(v map[string]float64, t time.Time, up bool, dt float64) {
	ft := a.tool
	if a.cell != nil {
		ft = a.cell
	}
	busy := ft != nil && up && ft.busy(t)
	stop, sched := false, false
	chUp := 3.0
	if ft != nil {
		stop, sched = ft.stopped(t)
		chUp = float64(len(ft.upChambers(t)))
		v["throughput_wph"] = ft.throughput(t)
		v["wafers_today"] = ft.today(t, ft.loc)
		v["queue_lots"] = float64(ft.Queue)
		v["lot_progress"] = ft.progress(t)
	} else {
		v["throughput_wph"], v["wafers_today"], v["queue_lots"], v["lot_progress"] = 0, 0, 0, 0
	}
	v["running"] = b2f(busy && !stop)
	v["trip"] = b2f(a.Trip || a.Interlock == engineUnplanned || (up && stop))
	v["maint_mode"] = b2f(a.Maint || a.Interlock == engineScheduled || (up && sched))
	if !up || stop || sched {
		chUp = 0
	}
	c := &tctx{a: a, ft: ft, t: t, dt: dt, busy: busy, up: up, chUp: chUp}
	for _, p := range toolPhysics[a.Kind] {
		v[p.alias] = p.value(c)
	}
}
