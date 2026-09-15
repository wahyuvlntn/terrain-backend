package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// FAB1 process equipment (docs/production/DESIGN.md section 2): the L2
// cleanroom tool set of a small 300 mm 40 nm logic line (31 tools in five
// bays) and the L1 sub-fab units that support them: dry vacuum pumps, local
// exhaust scrubbers (point-of-use abatement) and process chillers. A tool
// whose pump, scrubber or chiller is down is interlocked.
// ---------------------------------------------------------------------------

// FAB1 L2 bays and L1 sub-fab rooms.
const (
	locPhoto     = "Photo Bay"
	locEtchFilm  = "Etch & Thin Film Bay"
	locImpCMP    = "Implant & CMP Bay"
	locDiff      = "Diffusion Bay"
	locWetMet    = "Wet & Metrology Bay"
	locSubNorth  = "Sub-Fab North"
	locSubSouth  = "Sub-Fab South"
	locCleanroom = "FAB1 L2 Cleanroom"
)

// locationRenames are applied to existing locations before seeding.
var locationRenames = [][2]string{{"Sub-Fab Bay A (Etch)", locSubNorth}, {"Sub-Fab Bay B (CVD)", locSubSouth}}

var fabEquipmentTypes = []string{"Lithography Scanner", "Litho Track", "Plasma Etch Tool", "CVD Tool", "PVD Tool", "Electroplating Tool",
	"CMP Tool", "Diffusion Furnace", "RTP Tool", "Ion Implanter", "Wet Clean Station", "Asher", "Metrology Tool", "Inspection Tool",
	"Parametric Test System", "Exhaust Abatement System", "Process Chiller"}

var fabSeries = []string{"SC-1950i", "SC-860", "ST-365", "TK-300", "ET-300", "DE-300", "CV-300", "PV-300", "ECP-300", "CM-300", "VF-300",
	"RT-300", "MC-300", "HC-300", "WB-300", "SW-300", "AS-300", "MX-300", "CS-300", "OV-300", "DI-300", "PT-300", "iXL", "LS-300", "PC-300"}

var fabIssueCategories = []issueCategory{
	{"Process Equipment", [][2]string{{"PRO-01", "RF / Plasma"}, {"PRO-02", "Chamber Condition"}, {"PRO-03", "Wafer Handling"},
		{"PRO-04", "Process Chemistry"}, {"PRO-05", "Metrology Optics"}, {"PRO-06", "Lithography / Exposure"}, {"PRO-07", "Ion Beam / Dose"},
		{"PRO-08", "CMP Consumables"}}},
	{"Sub-Fab Support", [][2]string{{"PRC-05", "Exhaust Abatement"}, {"PRC-06", "Process Cooling"}}},
}

var fabAlarmCodes = [][2]string{
	{"SCN-001", "Focus Offset Beyond Process Window"},
	{"SCN-002", "High Stage Overlay Residual"},
	{"SCN-003", "Wafer Stage Temperature Deviation"},
	{"SCN-004", "Low Exposure Source Output"},
	{"SCN-010", "Lens Heating / Focus Drift"},
	{"SCN-011", "Excimer Laser Degradation"},
	{"SCN-012", "Wafer Stage Interferometer Drift"},
	{"SCN-013", "Mercury Lamp Aging"},
	{"TRK-001", "PEB Hot Plate Temperature Deviation"},
	{"TRK-002", "Low Coater Cup Exhaust"},
	{"TRK-003", "Litho Cell Humidity Deviation"},
	{"TRK-010", "PEB Hot Plate Drift"},
	{"TRK-011", "Resist Dispense Nozzle Contamination"},
	{"ETC-001", "High Reflected RF Power"},
	{"ETC-002", "ESC Temperature Deviation"},
	{"ETC-003", "Long Etch Endpoint Time"},
	{"ETC-004", "Etch Chamber Pressure Deviation"},
	{"ETC-005", "Etch Chamber Clean Overdue"},
	{"ETC-010", "RF Match Network Degradation"},
	{"ETC-011", "Electrostatic Chuck Helium Leak"},
	{"ETD-001", "High Reflected RF Power (Dielectric Etch)"},
	{"ETD-002", "Chamber Wall Temperature Deviation"},
	{"ETD-003", "Long Dielectric Etch Endpoint Time"},
	{"ETD-004", "Dielectric Etch Chamber Pressure Deviation"},
	{"ETD-005", "Dielectric Etch Chamber Clean Overdue"},
	{"ETD-010", "Dielectric Etch RF Match Degradation"},
	{"ETD-011", "Focus Ring Wear"},
	{"CVD-001", "CVD Heater Temperature Deviation"},
	{"CVD-002", "Low Deposition Rate"},
	{"CVD-003", "CVD Chamber Film Build-up"},
	{"CVD-004", "CVD Chamber Pressure Deviation"},
	{"CVD-010", "Showerhead Clogging"},
	{"CVD-011", "Heater Control Drift"},
	{"PVD-001", "High PVD Base Pressure"},
	{"PVD-002", "PVD Target End of Life"},
	{"PVD-003", "PVD Process Pressure Deviation"},
	{"PVD-004", "Low PVD Deposition Rate"},
	{"PVD-010", "Cryopump Degradation"},
	{"PVD-011", "Process Shield Flaking"},
	{"ECP-001", "Low Plating Bath Accelerator"},
	{"ECP-002", "Plating Bath Cu Concentration Deviation"},
	{"ECP-003", "Plating Bath Temperature Deviation"},
	{"ECP-004", "Anode End of Life"},
	{"ECP-010", "Plating Additive Depletion"},
	{"ECP-011", "Anode Passivation"},
	{"CMP-001", "Low CMP Removal Rate"},
	{"CMP-002", "Low Slurry Flow"},
	{"CMP-003", "High Pad Temperature"},
	{"CMP-004", "CMP Pad End of Life"},
	{"CMP-010", "Pad Glazing"},
	{"CMP-011", "Slurry Delivery Fault"},
	{"FRN-001", "Furnace Zone Temperature Spread"},
	{"FRN-002", "Furnace Process Gas Flow Deviation"},
	{"FRN-003", "Furnace Tube Deposit"},
	{"FRN-010", "Heater Zone Drift"},
	{"FRN-011", "Tube Particle Flaking"},
	{"RTP-001", "RTP Pyrometer Offset"},
	{"RTP-002", "High RTP Chamber Oxygen"},
	{"RTP-003", "High RTP Lamp Power"},
	{"RTP-010", "Pyrometer Calibration Drift"},
	{"RTP-011", "RTP Lamp Aging"},
	{"IMP-001", "Low Beam Current"},
	{"IMP-002", "Dose Uniformity Out of Specification"},
	{"IMP-003", "High End Station Pressure"},
	{"IMP-004", "Ion Source End of Life"},
	{"IMP-010", "Ion Source Degradation"},
	{"IMP-011", "Dose Control Drift"},
	{"WET-001", "Wet Bench Bath Temperature Deviation"},
	{"WET-002", "Low Chemical Concentration"},
	{"WET-003", "High Bath Particle Count"},
	{"WET-004", "Low DI Water Resistivity"},
	{"WET-010", "Recirculation Filter Clogging"},
	{"SWC-001", "Low Clean Chemical Flow"},
	{"SWC-002", "High Liquid Particle Count"},
	{"SWC-003", "Low DI Water Resistivity (Single-Wafer Clean)"},
	{"SWC-010", "Dispense Nozzle Clogging"},
	{"ASH-001", "Low Strip Rate"},
	{"ASH-002", "Asher Chuck Temperature Deviation"},
	{"ASH-003", "Asher Chamber Pressure Deviation"},
	{"ASH-010", "Plasma Source Degradation"},
	{"MET-001", "Low Metrology Lamp Intensity"},
	{"MET-002", "Metrology Stage Temperature Deviation"},
	{"MET-010", "Metrology Light Source Aging"},
	{"CDS-001", "CD-SEM Resolution Degraded"},
	{"CDS-002", "CD-SEM Column Vacuum"},
	{"CDS-003", "CD-SEM Stage Temperature Deviation"},
	{"CDS-010", "Field Emitter Aging"},
	{"OVL-001", "Low Overlay Tool Lamp Intensity"},
	{"OVL-002", "High Tool-Induced Shift"},
	{"OVL-010", "Overlay Tool Lamp Aging"},
	{"DEF-001", "Low Inspection Laser Power"},
	{"DEF-002", "Defect Sensitivity Degraded"},
	{"DEF-010", "Inspection Laser Degradation"},
	{"WAT-001", "High Probe Contact Resistance"},
	{"WAT-002", "Prober Chuck Temperature Deviation"},
	{"WAT-010", "Probe Card Wear"},
	{"SCR-001", "High Scrubber Inlet Pressure"},
	{"SCR-002", "Low Combustion Chamber Temperature"},
	{"SCR-003", "Low Scrubbing Water Flow"},
	{"SCR-004", "Low Scrubber Water pH"},
	{"SCR-010", "Scrubber Inlet Powder Clogging"},
	{"SCR-011", "Burner Flame Failure"},
	{"PCH-001", "High Coolant Supply Temperature"},
	{"PCH-002", "Low Coolant Flow"},
	{"PCH-003", "Low Coolant Reservoir Level"},
	{"PCH-004", "High Chiller Compressor Current"},
	{"PCH-010", "Refrigerant Charge Loss"},
	{"PCH-011", "Coolant Pump Wear"},
	{"PM-SCN", "Scanner Monthly Preventive Maintenance"},
	{"PM-TRK", "Track Monthly Preventive Maintenance"},
	{"PM-ETC", "Conductor Etch Monthly Preventive Maintenance"},
	{"PM-ETD", "Dielectric Etch Monthly Preventive Maintenance"},
	{"PM-CVD", "CVD Tool Monthly Preventive Maintenance"},
	{"PM-PVD", "PVD Tool Monthly Preventive Maintenance"},
	{"PM-ECP", "Plating Tool Monthly Preventive Maintenance"},
	{"PM-CMP", "CMP Tool Monthly Preventive Maintenance"},
	{"PM-FRN", "Furnace Monthly Preventive Maintenance"},
	{"PM-RTP", "RTP Tool Monthly Preventive Maintenance"},
	{"PM-IMP", "Implanter Monthly Preventive Maintenance"},
	{"PM-WET", "Wet Bench Monthly Preventive Maintenance"},
	{"PM-SWC", "Single-Wafer Clean Monthly Preventive Maintenance"},
	{"PM-ASH", "Asher Monthly Preventive Maintenance"},
	{"PM-MET", "Metrology Tool Monthly Preventive Maintenance"},
	{"PM-CDS", "CD-SEM Monthly Preventive Maintenance"},
	{"PM-OVL", "Overlay Tool Monthly Preventive Maintenance"},
	{"PM-DEF", "Inspection Tool Monthly Preventive Maintenance"},
	{"PM-WAT", "Parametric Tester Monthly Preventive Maintenance"},
	{"PM-SCR", "Scrubber Monthly Inlet Clean and Service"},
	{"PM-PCH", "Process Chiller Monthly Service"},
}

// ---------------------------------------------------------------------------
// Sub-fab models

func scrubberModel() *ModelSpec {
	return &ModelSpec{Key: "scrub", Name: "Local Scrubber LS-300", Type: "Exhaust Abatement System", Brand: "Utility OEM", Series: "LS-300",
		Description: "Point-of-use burn-wet exhaust abatement for the process tools: natural-gas burner oxidises the process gases (silane, NF3, " +
			"fluorocarbons, hydrides), a water scrubber removes the acid gases and powder",
		Params: append(statusParams(),
			// (no parameter limits on the readings that drop when the unit
			// stops; the alarms are evaluated while it runs)
			numP("inlet_press", "Inlet Static Pressure", "Pa", 0),
			numP("process_flow", "Process Exhaust Flow", "slm", 0),
			numP("burner_temp", "Combustion Chamber Temperature", "°C", 0),
			numP("fuel_flow", "Fuel Gas Flow", "slm", 1),
			numP("water_flow", "Scrubbing Water Flow", "l/min", 1),
			numP("water_ph", "Scrubber Water pH", "", 2),
			numP("outlet_temp", "Exhaust Outlet Temperature", "°C", 1).W(">", 45),
		),
		Alarms: []AlarmSpec{
			{"SCR-001", "Inlet static pressure above -200 Pa - inlet clogging, tool exhaust at risk", false, "PRC-05", gt("inlet_press", -200, true)},
			{"SCR-002", "Combustion chamber below 750 °C - incomplete abatement", true, "PRC-05", lt("burner_temp", 750, true)},
			{"SCR-003", "Scrubbing water flow below 7 l/min", false, "WTR-01", lt("water_flow", 7, true)},
			{"SCR-004", "Scrubber water pH below 4.5", false, "WTR-03", lt("water_ph", 4.5, true)},
		},
		Relations: []RelationSpec{
			{"Scrubber Inlet Powder Clogging", "SCR-010", [][]string{{"A:SCR-001"}, {"P:inlet_press"}}},
			{"Burner Flame Failure", "SCR-011", [][]string{{"A:SCR-002"}, {"P:burner_temp", "P:fuel_flow"}}},
		},
		Faults: []FaultSpec{
			fault("inlet_clog", "Inlet powder clogging", "SCR-010", "SCR-001", "PM-SCR"),
			fault("burner_fault", "Burner igniter / flame sensor fault", "SCR-011", "SCR-002", "PM-SCR"),
		},
		Trips:  []TripSpec{{"SCR-002", "burner_temp", 600, "Burner flame failure - abatement down, process tools interlocked"}},
		PMCode: "PM-SCR", PMRepeat: 3, PMHours: 4,
		Mapping: map[string][]string{"SCR-001": {"inlet_press", "process_flow"}, "SCR-002": {"burner_temp", "fuel_flow"}, "SCR-003": {"water_flow"},
			"SCR-004": {"water_ph", "process_flow"}, "SCR-010": {"inlet_press", "water_flow"}, "SCR-011": {"burner_temp", "fuel_flow"}},
	}
}

func processChillerModel() *ModelSpec {
	return &ModelSpec{Key: "pch", Name: "Process Chiller PC-300", Type: "Process Chiller", Brand: "Utility OEM", Series: "PC-300",
		Description: "Recirculating process chiller for the tools' chamber walls, electrostatic chucks, RF generators and implanter source / " +
			"end station: refrigeration circuit rejecting to PCW, Galden / water-glycol loop at 20 °C",
		Params: append(statusParams(),
			numP("supply_temp", "Coolant Supply Temperature", "°C", 2),
			numP("return_temp", "Coolant Return Temperature", "°C", 2),
			numP("coolant_flow", "Coolant Flow", "l/min", 1),
			numP("heat_load", "Heat Load", "kW", 1),
			numP("comp_current", "Compressor Current", "A", 1).W(">", 46).C(">", 52),
			numP("tank_level", "Reservoir Level", "%", 1).W("<", 60).C("<", 45),
		),
		Alarms: []AlarmSpec{
			{"PCH-001", "Coolant supply above 23 °C - tool temperatures at risk", false, "PRC-06", gt("supply_temp", 23, true)},
			{"PCH-002", "Coolant flow below 63 l/min", true, "PRC-06", lt("coolant_flow", 63, true)},
			{"PCH-003", "Reservoir level below 45 %", false, "WTR-04", lt("tank_level", 45, false)},
			{"PCH-004", "Compressor current above 52 A", false, "ELE-01", gt("comp_current", 52, true)},
		},
		Relations: []RelationSpec{
			{"Refrigerant Charge Loss", "PCH-010", [][]string{{"A:PCH-001"}, {"P:supply_temp", "P:comp_current"}}},
			{"Coolant Pump Wear", "PCH-011", [][]string{{"A:PCH-002"}, {"P:coolant_flow"}}},
		},
		Faults: []FaultSpec{
			fault("refrigerant_low", "Refrigerant charge loss", "PCH-010", "PCH-001", "PM-PCH"),
			fault("pump_wear", "Coolant pump impeller / seal wear", "PCH-011", "PCH-002", "PM-PCH"),
		},
		Trips:  []TripSpec{{"PCH-002", "coolant_flow", 20, "Low coolant flow interlock - chiller stopped, process tools interlocked"}},
		PMCode: "PM-PCH", PMRepeat: 3, PMHours: 3,
		Mapping: map[string][]string{"PCH-001": {"supply_temp", "heat_load"}, "PCH-002": {"coolant_flow"}, "PCH-003": {"tank_level"},
			"PCH-004": {"comp_current", "heat_load"}, "PCH-010": {"supply_temp", "comp_current"}, "PCH-011": {"coolant_flow", "return_temp"}},
	}
}

func subFabModels() []*ModelSpec {
	return []*ModelSpec{
		dryPumpModel("Edwards iXL750 Dry Pump (Clean)", "iXL750 for clean vacuum duty (PVD load locks and degas, implanter end stations): runs cool, no process deposits", nil),
		scrubberModel(), processChillerModel(),
	}
}

// scrubber computes a local scrubber from the gas load of its tools.
func (a *Asset) scrubber(v map[string]float64, t time.Time, env Env, run bool, dt float64) {
	clog, burn := a.sev("inlet_clog"), a.sev("burner_fault")
	load := servedLoad(a, t)
	if run && a.smooth["was_run"] == 0 {
		// the controller releases the abatement once the burner is lit
		a.smooth["bt"] = math.Max(a.smooth["bt"], 820)
	}
	a.smooth["was_run"] = b2f(run)
	if !run {
		v["inlet_press"] = -15 + a.n("ip", 3, 60, dt)
		v["process_flow"], v["fuel_flow"], v["water_flow"] = 0, 0, 0
		v["burner_temp"] = a.lag("bt", 40, 900, dt)
		v["water_ph"] = 7 + a.n("ph", 0.05, 600, dt)
		v["outlet_temp"] = a.lag("ot", env.DryBulb-3, 900, dt)
		return
	}
	v["process_flow"] = 40 + 160*load + a.n("pf", 3, 60, dt)
	v["inlet_press"] = -450 + 270*math.Pow(clog, 1.3) + 25*load + a.n("ip", 6, 60, dt)
	v["burner_temp"] = a.lag("bt", 850-130*burn+12*load, 120, dt) + a.n("btn", 3, 60, dt)
	v["fuel_flow"] = 30 + 4*load - 6*burn + a.n("ff", 0.3, 60, dt)
	v["water_flow"] = 12 - 2.5*clog + a.n("wf", 0.15, 120, dt)
	v["water_ph"] = 7.1 - 0.7*load - 1.2*clog + a.n("ph", 0.06, 600, dt)
	v["outlet_temp"] = a.lag("ot", 32+4*load+0.3*(env.DryBulb-28), 600, dt) + a.n("otn", 0.2, 120, dt)
}

// processChiller computes a process chiller from the heat load of its tools.
func (a *Asset) processChiller(v map[string]float64, t time.Time, env Env, run bool, dt float64) {
	ref, pw := a.sev("refrigerant_low"), a.sev("pump_wear")
	q := 0.0
	for _, s := range a.served {
		if s.Running() {
			q += 4
			if s.tool != nil && s.tool.busy(t) {
				q += 9
			}
		}
	}
	v["tank_level"] = 80 + a.n("tl", 0.4, 3600, dt)
	if run && a.smooth["was_run"] == 0 {
		// the chiller releases its tools once the loop is back on temperature
		if v, ok := a.smooth["st"]; ok && v > 20.3 {
			a.smooth["st"] = 20.3
		}
	}
	a.smooth["was_run"] = b2f(run)
	if !run {
		v["coolant_flow"], v["heat_load"], v["comp_current"] = 0, 0, 0
		v["supply_temp"] = a.lag("st", 24, 1800, dt)
		v["return_temp"] = a.lag("rt", 24, 1800, dt)
		return
	}
	flow := 90*(1-0.35*pw) + a.n("cf", 0.6, 120, dt)
	st := a.lag("st", 20+3.8*ref*(0.4+q/40)+0.015*(q-25), 300, dt) + a.n("stn", 0.05, 120, dt)
	v["heat_load"] = q + a.n("hl", 0.3, 120, dt)
	v["coolant_flow"] = flow
	v["supply_temp"] = st
	v["return_temp"] = st + q/(3.6*math.Max(flow, 10)/60)
	v["comp_current"] = 18 + 0.6*q + 7*ref + a.n("cc", 0.3, 60, dt)
}

// servedLoad is the mean process gas load of the tools a support unit serves.
func servedLoad(a *Asset, t time.Time) float64 {
	if len(a.served) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range a.served {
		if s.tool != nil && s.tool.busy(t) {
			sum += 1
		}
	}
	return sum / float64(len(a.served))
}

// ---------------------------------------------------------------------------
// Equipment

// toolSupport maps a process tool to its dry pump, scrubber and process
// chiller ("" none).
var toolSupport = map[string][3]string{
	"ETCH_01": {"DP_01", "SCR_01", "PCH_01"}, "ETCH_02": {"DP_02", "SCR_01", "PCH_01"}, "ETCH_03": {"DP_03", "SCR_01", "PCH_01"},
	"CVD_01": {"DP_04", "SCR_02", "PCH_02"}, "CVD_02": {"DP_05", "SCR_02", "PCH_02"}, "PVD_01": {"DP_07", "", "PCH_02"},
	"FRN_03": {"DP_06", "SCR_03", ""},
	"IMP_01": {"DP_08", "SCR_03", "PCH_03"}, "IMP_02": {"DP_09", "SCR_03", "PCH_03"},
	"ASH_01": {"DP_10", "SCR_03", ""}, "RTP_01": {"", "", "PCH_03"},
}

// linkSupport connects the process tools to their support units.
func (p *Plant) linkSupport() {
	aliases := make([]string, 0, len(toolSupport))
	for alias := range toolSupport {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		a := p.ByAlias[alias]
		if a == nil {
			continue
		}
		s := toolSupport[alias]
		a.vacPump, a.abate, a.coolant = p.ByAlias[s[0]], p.ByAlias[s[1]], p.ByAlias[s[2]]
		for _, u := range []*Asset{a.abate, a.coolant} {
			if u != nil {
				u.served = append(u.served, a)
			}
		}
	}
}

// supportInterlock is the interlock of a process tool from its support
// units: unscheduled when one is down unplanned, scheduled when one is in
// planned maintenance (0 none).
func supportInterlock(a *Asset) int {
	il := 0
	for _, s := range []*Asset{a.vacPump, a.abate, a.coolant} {
		if s == nil || s.Running() {
			continue
		}
		st := engineUnplanned
		if s.Maint && !s.Trip && s.Forced == engineScheduled {
			st = engineScheduled
		}
		if il == 0 || st == engineUnplanned {
			il = st
		}
	}
	return il
}

type toolSpec struct {
	Alias, Name, Model, Location, Installed, Critical, Supporting string
}

// subFabEquipment are the L1 support units.
func subFabEquipment() []*EquipSpec {
	var out []*EquipSpec
	pumps := []struct{ model, room, tool string }{
		{"Edwards iXH1220H Dry Pump (Etch)", locSubNorth, "Conductor etch tool ETCH-01"},
		{"Edwards iXH1220H Dry Pump (Etch)", locSubNorth, "Dielectric etch tool ETCH-02"},
		{"Edwards iXH1220H Dry Pump (Etch)", locSubNorth, "Dielectric etch tool ETCH-03"},
		{"Edwards iXH610 Dry Pump (CVD)", locSubNorth, "CVD tool CVD-01"},
		{"Edwards iXH610 Dry Pump (CVD)", locSubNorth, "CVD tool CVD-02"},
		{"Edwards iXH610 Dry Pump (CVD)", locSubSouth, "LPCVD furnace FRN-03"},
		{"Edwards iXL750 Dry Pump (Clean)", locSubNorth, "PVD tool PVD-01"},
		{"Edwards iXL750 Dry Pump (Clean)", locSubSouth, "Medium current implanter IMP-01"},
		{"Edwards iXL750 Dry Pump (Clean)", locSubSouth, "High current implanter IMP-02"},
		{"Edwards iXH1220H Dry Pump (Etch)", locSubSouth, "Plasma asher ASH-01"},
	}
	for i, p := range pumps {
		out = append(out, &EquipSpec{Alias: fmt.Sprintf("DP_%02d", i+1), Name: fmt.Sprintf("Dry Pump DP-%02d", i+1), Model: p.model, Location: p.room,
			Gateway: "fab1_subfab", Installed: fmt.Sprintf("2022-%02d-18", i%9+1), Group: "vac",
			Critical:   [][2]string{{"Process vacuum", p.tool + " foreline vacuum"}},
			Supporting: [][2]string{{"Abatement", "Exhaust routed to the tool's local scrubber"}}})
	}
	for i, s := range [][2]string{{locSubNorth, "Etch tools ETCH-01..03"}, {locSubNorth, "CVD tools CVD-01/02"},
		{locSubSouth, "Implanters IMP-01/02, LPCVD furnace FRN-03 and asher ASH-01"}} {
		out = append(out, &EquipSpec{Alias: fmt.Sprintf("SCR_%02d", i+1), Name: fmt.Sprintf("Local Scrubber SCR-%02d", i+1), Model: "Local Scrubber LS-300",
			Location: s[0], Gateway: "fab1_subfab", Installed: fmt.Sprintf("2022-%02d-22", i+2), Group: "scrub",
			Critical:   [][2]string{{"Exhaust abatement", s[1] + " - the tools are interlocked while the scrubber is down"}},
			Supporting: [][2]string{{"Utilities", "Natural gas, city water and the acid exhaust main"}}})
	}
	for i, s := range [][2]string{{locSubNorth, "Etch tools ETCH-01..03 (chamber walls, ESC, RF)"}, {locSubNorth, "CVD-01/02 and PVD-01"},
		{locSubSouth, "Implanters IMP-01/02 and RTP-01"}} {
		out = append(out, &EquipSpec{Alias: fmt.Sprintf("PCH_%02d", i+1), Name: fmt.Sprintf("Process Chiller PCH-%02d", i+1), Model: "Process Chiller PC-300",
			Location: s[0], Gateway: "fab1_subfab", Installed: fmt.Sprintf("2022-%02d-24", i+2), Group: "pch",
			Critical:   [][2]string{{"Process cooling", s[1] + " - the tools are interlocked while the chiller is down"}},
			Supporting: [][2]string{{"Heat rejection", "Condenser cooled by the PCW loop from the CUB"}}})
	}
	return out
}

// toolEquipment are the L2 process tools (group "tool").
func toolEquipment() []*EquipSpec {
	specs := []toolSpec{
		{"SCN_01", "ArF Immersion Scanner SCN-01", "ArF Immersion Scanner SC-1950i", locPhoto, "2022-02-10",
			"Exposure of the critical layers (STI, gate, contact, thin metal and via levels)", "Linked in-line to track TRK-01"},
		{"SCN_02", "KrF Scanner SCN-02", "KrF Scanner SC-860", locPhoto, "2022-02-24",
			"Exposure of the semi-critical layers (threshold, LDD and source / drain implants, salicide block, thick metals)", "Linked in-line to track TRK-02"},
		{"STP_01", "i-line Stepper STP-01", "i-line Stepper ST-365", locPhoto, "2022-03-08",
			"Exposure of the well and pad layers", "Linked in-line to track TRK-03"},
		{"TRK_01", "Litho Track TRK-01", "Coat/Develop Track TK-300", locPhoto, "2022-02-10", "Resist coat, bake and develop for scanner SCN-01", "ArF immersion resist and top coat"},
		{"TRK_02", "Litho Track TRK-02", "Coat/Develop Track TK-300", locPhoto, "2022-02-24", "Resist coat, bake and develop for scanner SCN-02", "KrF resist"},
		{"TRK_03", "Litho Track TRK-03", "Coat/Develop Track TK-300", locPhoto, "2022-03-08", "Resist coat, bake and develop for stepper STP-01", "i-line resist"},
		{"ETCH_01", "Conductor Etch ETCH-01", "ICP Etch Cluster ET-300", locEtchFilm, "2022-03-25",
			"Silicon trench, poly gate, spacer, Al pad and MIM etch (3 ICP chambers)", "Dry pump DP-01, scrubber SCR-01, chiller PCH-01"},
		{"ETCH_02", "Dielectric Etch ETCH-02", "CCP Dielectric Etch DE-300", locEtchFilm, "2022-04-12",
			"Contact, via, trench and pad opening etch (3 CCP chambers)", "Dry pump DP-02, scrubber SCR-01, chiller PCH-01"},
		{"ETCH_03", "Dielectric Etch ETCH-03", "CCP Dielectric Etch DE-300", locEtchFilm, "2022-04-26",
			"Contact, via, trench and pad opening etch (3 CCP chambers)", "Dry pump DP-03, scrubber SCR-01, chiller PCH-01"},
		{"CVD_01", "CVD Tool CVD-01", "PECVD Cluster CV-300", locEtchFilm, "2022-05-10",
			"Trench fill, spacer, PMD, W contact fill, inter-metal dielectric and passivation films", "Dry pump DP-04, scrubber SCR-02, chiller PCH-02"},
		{"CVD_02", "CVD Tool CVD-02", "PECVD Cluster CV-300", locEtchFilm, "2022-05-24",
			"Trench fill, spacer, PMD, W contact fill, inter-metal dielectric and passivation films", "Dry pump DP-05, scrubber SCR-02, chiller PCH-02"},
		{"PVD_01", "PVD Tool PVD-01", "PVD Cluster PV-300", locEtchFilm, "2022-06-07",
			"NiPt, Ti/TiN, TaN/Ta barrier and Cu seed, AlCu pad metal", "Dry pump DP-07, chiller PCH-02"},
		{"IMP_01", "Medium Current Implanter IMP-01", "Medium Current Implanter MC-300", locImpCMP, "2022-06-21",
			"Well, threshold adjust, LDD and halo implants", "Dry pump DP-08, scrubber SCR-03, chiller PCH-03"},
		{"IMP_02", "High Current Implanter IMP-02", "High Current Implanter HC-300", locImpCMP, "2022-07-05",
			"Source / drain and LDD implants", "Dry pump DP-09, scrubber SCR-03, chiller PCH-03"},
		{"CMP_01", "Oxide / W CMP CMP-01", "CMP Polisher CM-300", locImpCMP, "2022-07-19", "STI, PMD oxide and W contact CMP", "Slurry delivery from the sub-fab"},
		{"CMP_02", "Cu CMP CMP-02", "CMP Polisher CM-300", locImpCMP, "2022-08-02", "Cu damascene CMP", "Slurry delivery from the sub-fab"},
		{"CMP_03", "Cu CMP CMP-03", "CMP Polisher CM-300", locImpCMP, "2022-08-16", "Cu damascene CMP", "Slurry delivery from the sub-fab"},
		{"ECP_01", "Cu Plating ECP-01", "Cu Plating ECP-300", locImpCMP, "2022-08-30", "Cu electroplating of the damascene levels (4 plating cells)", "Bath chemistry dosing from the sub-fab"},
		{"FRN_01", "Oxidation Furnace FRN-01", "Vertical Furnace VF-300", locDiff, "2022-03-15",
			"Pad, liner and gate oxidation, densification, well drive and alloy anneals (batch of 4 lots)", "Exhaust to the central acid exhaust"},
		{"FRN_02", "Oxidation Furnace FRN-02", "Vertical Furnace VF-300", locDiff, "2022-03-29",
			"Pad, liner and gate oxidation, densification, well drive and alloy anneals (batch of 4 lots)", "Exhaust to the central acid exhaust"},
		{"FRN_03", "LPCVD Furnace FRN-03", "Vertical Furnace VF-300", locDiff, "2022-04-05",
			"LPCVD silicon nitride and poly-Si (batch of 4 lots)", "Dry pump DP-06, scrubber SCR-03"},
		{"RTP_01", "RTP Tool RTP-01", "RTP Anneal RT-300", locDiff, "2022-09-13", "ISSG gate oxidation, spike and silicide anneals", "Chiller PCH-03"},
		{"ASH_01", "Plasma Asher ASH-01", "Plasma Asher AS-300", locDiff, "2022-09-27", "Post-implant and rework resist strip (2 chambers)", "Dry pump DP-10, scrubber SCR-03"},
		{"WET_01", "Wet Bench WET-01", "Batch Wet Clean WB-300", locWetMet, "2022-07-08", "Pre-furnace, HF and strip cleans (2 lots per batch)", "Chemicals from the sub-fab bulk chemical distribution"},
		{"WET_02", "Wet Bench WET-02", "Batch Wet Clean WB-300", locWetMet, "2022-08-08", "SPM resist strip, hot phosphoric and pre-silicide cleans (2 lots per batch)", "Chemicals from the sub-fab bulk chemical distribution"},
		{"SWC_01", "Single-Wafer Clean SWC-01", "Single-Wafer Clean SW-300", locWetMet, "2022-10-04", "Post-etch polymer removal and scrub cleans (4 chambers)", "Chemicals from the sub-fab bulk chemical distribution"},
		{"MET_01", "Optical Metrology MET-01", "Optical Metrology MX-300", locWetMet, "2022-07-12", "Film thickness, optical CD and sheet resistance", "Daily reference wafer calibration"},
		{"CDS_01", "CD-SEM CDS-01", "CD-SEM CS-300", locWetMet, "2022-07-26", "Resist CD after develop on the critical layers", "Daily calibration on the pitch standard"},
		{"OVL_01", "Overlay Metrology OVL-01", "Overlay Metrology OV-300", locWetMet, "2022-08-09", "Layer-to-layer overlay after develop", "Daily TIS calibration"},
		{"DEF_01", "Defect Inspection DEF-01", "Darkfield Inspection DI-300", locWetMet, "2022-08-23", "Particle and pattern defect inspection", "Daily calibration on the PSL standard"},
		{"WAT_01", "Parametric Tester WAT-01", "Parametric Tester PT-300", locWetMet, "2022-09-06", "Wafer acceptance test of every finished lot", "Probe card cleaned every 1,000 wafers"},
	}
	var out []*EquipSpec
	for _, s := range specs {
		out = append(out, &EquipSpec{Alias: s.Alias, Name: s.Name, Model: s.Model, Location: s.Location, Gateway: "fab1_tools", Installed: s.Installed,
			Group: "tool", Critical: [][2]string{{"Wafer processing", s.Critical}}, Supporting: [][2]string{{"Support", s.Supporting}}})
	}
	return out
}
