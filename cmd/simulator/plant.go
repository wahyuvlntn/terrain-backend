package main

import (
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Plant definition: a 300 mm semiconductor wafer fab in Singapore. The
// simulator models the facility systems the fab depends on - the central
// utility building (chillers, cooling towers, process cooling water pumps,
// clean dry air compressors) and the fab itself (cleanroom make-up air units
// and the sub-fab support units) - and the wafer production of FAB1: 31
// process tools of a 40 nm logic line and the lots they run (fabplant.go,
// tools.go, fabroute.go, fab.go).
// ---------------------------------------------------------------------------

// Threshold is an equipment parameter limit ("X > _" or "X < _").
type Threshold struct {
	Type  string
	Value float64
}

// ParamSpec is a model variable.
type ParamSpec struct {
	Alias, Name, Unit string
	Bool              bool
	Scale             int
	Warn, Crit        *Threshold
	Ctl               *ControlSpec // writable from the application (set point or control mode)
}

// ControlSpec makes a parameter writable from the digital twin: a set point
// (range and step) or a control mode (options; value 0 is always automatic).
// The seeder stores it on the model parameter; the simulator applies writes
// received on the gateway's command topic.
type ControlSpec struct {
	Min, Max, Step float64
	Options        []ControlOption
	// Mode is the mode parameter that must be out of automatic before the set
	// point can be written (empty: always writable).
	Mode string
	// Target is the measured parameter the control drives.
	Target string
	// Default is the manual set point taken when a mode leaves automatic while
	// no set point is in effect (equipment stopped); 0 = mid-range.
	Default float64
}

// ControlOption is one value of a control mode.
type ControlOption struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

// SP makes a numeric parameter a writable set point within [min, max] in steps
// of step, driving target; mode (optional) is its control mode parameter.
func (p ParamSpec) SP(min, max, step float64, mode, target string) ParamSpec {
	p.Ctl = &ControlSpec{Min: min, Max: max, Step: step, Mode: mode, Target: target}
	return p
}

// Def sets the default manual value of a set point (see ControlSpec.Default).
func (p ParamSpec) Def(v float64) ParamSpec {
	if p.Ctl != nil {
		c := *p.Ctl
		c.Default = v
		p.Ctl = &c
	}
	return p
}

// Modes makes a parameter a writable control mode with one option per label
// (values 0, 1, ...; 0 is automatic), driving target.
func (p ParamSpec) Modes(target string, labels ...string) ParamSpec {
	c := &ControlSpec{Target: target}
	for i, l := range labels {
		c.Options = append(c.Options, ControlOption{Value: float64(i), Label: l})
	}
	p.Ctl = c
	return p
}

func numP(alias, name, unit string, scale int) ParamSpec {
	return ParamSpec{Alias: alias, Name: name, Unit: unit, Scale: scale}
}

func boolP(alias, name string) ParamSpec { return ParamSpec{Alias: alias, Name: name, Bool: true} }

// W sets the warning threshold; op is ">" or "<".
func (p ParamSpec) W(op string, v float64) ParamSpec {
	p.Warn = &Threshold{Type: "X " + op + " _", Value: v}
	return p
}

// C sets the critical threshold.
func (p ParamSpec) C(op string, v float64) ParamSpec {
	p.Crit = &Threshold{Type: "X " + op + " _", Value: v}
	return p
}

// CondSpec is the condition of a model alarm. The JavaScript stored in the
// model and the Go evaluation used for the history are generated from it.
type CondSpec struct {
	Param    string
	Op       string // ">", "<" or "dev" (absolute deviation from SetPoint)
	Value    float64
	SetPoint float64
	// SetPointParam (optional) takes the set point of "dev" from a parameter.
	SetPointParam string
	Running       bool // only while the equipment runs
}

// Script returns the JavaScript alarm script.
func (c CondSpec) Script() string {
	run := ""
	if c.Running {
		run = "running.v == true && "
	}
	switch c.Op {
	case "dev":
		sp := fnum(c.SetPoint)
		if c.SetPointParam != "" {
			sp = c.SetPointParam + ".v"
		}
		return fmt.Sprintf("%sMath.abs(%s.v - %s) > %s", run, c.Param, sp, fnum(c.Value))
	default:
		return fmt.Sprintf("%s%s.v %s %s", run, c.Param, c.Op, fnum(c.Value))
	}
}

// Eval evaluates the condition on simulated values.
func (c CondSpec) Eval(v map[string]float64) bool {
	if c.Running && v["running"] != 1 {
		return false
	}
	x, ok := v[c.Param]
	if !ok {
		return false
	}
	switch c.Op {
	case ">":
		return x > c.Value
	case "<":
		return x < c.Value
	case "dev":
		sp := c.SetPoint
		if c.SetPointParam != "" {
			sp = v[c.SetPointParam]
		}
		d := x - sp
		if d < 0 {
			d = -d
		}
		return d > c.Value
	}
	return false
}

func fnum(f float64) string {
	s := fmt.Sprintf("%.4f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// AlarmSpec is a model alarm; its name is the alarm code.
type AlarmSpec struct {
	Code     string
	Message  string
	Critical bool
	Issue    string // feedback issue
	Cond     CondSpec
}

func gt(p string, v float64, running bool) CondSpec {
	return CondSpec{Param: p, Op: ">", Value: v, Running: running}
}

func lt(p string, v float64, running bool) CondSpec {
	return CondSpec{Param: p, Op: "<", Value: v, Running: running}
}

func dev(p string, sp, band float64) CondSpec {
	return CondSpec{Param: p, Op: "dev", Value: band, SetPoint: sp, Running: true}
}

// devP is a deviation from the set point held in parameter spParam.
func devP(p, spParam string, band float64) CondSpec {
	return CondSpec{Param: p, Op: "dev", Value: band, SetPointParam: spParam, Running: true}
}

// RelationSpec is an issue relation (Issue Mapping). Each path is a list of
// inputs ("A:<code>" active alarm, "P:<alias>" abnormal parameter); paths
// with more than one input go through a compound (AND) node.
type RelationSpec struct {
	Name   string
	Target string
	Paths  [][]string
}

// FaultSpec is a degradation the simulator can inject.
type FaultSpec struct {
	Kind  string
	Label string
	// Codes whose completed maintenance repairs the fault.
	Codes []string
	// Manual faults (the fixed-intensity LBNL faults) are injected on demand
	// only: never at random in the live plant and never sped up when the live
	// simulation starts. The history plans a few labelled episodes of each.
	Manual bool
	// Intensities are the documented fault intensities (Fault.Value); Unit is
	// their unit. Faults without intensities develop a severity instead.
	Intensities []float64
	Unit        string
}

// fault is a degradation that develops over time (severity 0 .. maxSeverity).
func fault(kind, label string, codes ...string) FaultSpec {
	return FaultSpec{Kind: kind, Label: label, Codes: codes}
}

// lbnl is a fixed-intensity fault of the LBNL chiller plant FDD data set,
// injected on demand with one of its documented intensities.
func lbnl(kind, label, unit string, intensities []float64, codes ...string) FaultSpec {
	return FaultSpec{Kind: kind, Label: label, Codes: codes, Manual: true, Intensities: intensities, Unit: unit}
}

// fddRelation is the issue relation of an external FDD alarm (an analytics
// rule code the engine raises as an alarm): the active code itself leads to a
// corrective early-warning order that carries the code's SOPs.
func fddRelation(code string) RelationSpec {
	return RelationSpec{Name: codeName(code) + " (FDD)", Target: code, Paths: [][]string{{"A:" + code}}}
}

// faultSpec returns the model's definition of a fault kind (nil when unknown).
func (m *ModelSpec) faultSpec(kind string) *FaultSpec {
	for i := range m.Faults {
		if m.Faults[i].Kind == kind {
			return &m.Faults[i]
		}
	}
	return nil
}

// FaultCatalogEntry is one injectable fault kind of a model (the shape of the
// backend's simulation/getfaultcatalog). Intensities are empty for the
// developing faults, whose value is a fixed severity (0 .. 1.35; 0 = develops
// at the live pace).
type FaultCatalogEntry struct {
	Kind        string    `json:"kind"`
	AssetKind   string    `json:"asset_kind"`
	Assets      []string  `json:"assets"`
	Intensities []float64 `json:"intensities"`
	Unit        string    `json:"unit"`
	Label       string    `json:"label"`
	Code        string    `json:"code"`
	Manual      bool      `json:"manual"`
}

// faultCatalog lists every fault kind of the plant's models with the units
// that can take it (LBNL faults first).
func faultCatalog(p *Plant) []FaultCatalogEntry {
	var lbnlFaults, others []FaultCatalogEntry
	for _, m := range models() {
		var units []string
		for _, a := range p.Assets {
			if a.Model.Name == m.Name {
				units = append(units, a.Spec.Alias)
			}
		}
		if len(units) == 0 {
			continue
		}
		for _, f := range m.Faults {
			e := FaultCatalogEntry{Kind: f.Kind, AssetKind: m.Key, Assets: units, Intensities: f.Intensities, Unit: f.Unit, Label: f.Label, Manual: f.Manual}
			if e.Intensities == nil {
				e.Intensities = []float64{}
			}
			if !f.Manual {
				e.Unit = "severity"
			}
			if len(f.Codes) > 0 {
				e.Code = f.Codes[0]
			}
			if f.Manual {
				lbnlFaults = append(lbnlFaults, e)
			} else {
				others = append(others, e)
			}
		}
	}
	return append(lbnlFaults, others...)
}

// randomFaults returns the faults that may develop at random (not Manual).
func (m *ModelSpec) randomFaults() []FaultSpec {
	var out []FaultSpec
	for _, f := range m.Faults {
		if !f.Manual {
			out = append(out, f)
		}
	}
	return out
}

// TripSpec is a sudden trip (unplanned down).
type TripSpec struct {
	Code   string // alarm raised just before the trip
	Param  string // parameter spiking before the trip
	Spike  float64
	Reason string
}

// ModelSpec describes an equipment model.
type ModelSpec struct {
	Key         string // physics key
	Name        string
	Description string
	Type        string
	Brand       string
	Series      string
	Base        string // base model name for extended models
	IsBase      bool
	Params      []ParamSpec
	Alarms      []AlarmSpec
	Relations   []RelationSpec
	Faults      []FaultSpec
	Trips       []TripSpec
	PMCode      string
	PMRepeat    int // 3 monthly, 4 yearly
	PMHours     float64
	RunHourWarn int
	RunHourCrit int
	AutoPM      bool
	// Alarm mapping: alarm code -> related variables.
	Mapping map[string][]string
	// Alarm overrides for extended models (code -> condition).
	Overrides map[string]CondSpec
}

func statusParams() []ParamSpec {
	return []ParamSpec{boolP("running", "Run Status"), boolP("trip", "Trip Alarm"), boolP("maint_mode", "Maintenance Mode")}
}

// ---------------------------------------------------------------------------
// Master data

var equipmentTypes = append([]string{"Centrifugal Chiller", "Cooling Tower", "Process Cooling Water Pump", "Chilled Water Pump", "Condenser Water Pump",
	"Air Compressor", "Make-up Air Unit", "Dry Vacuum Pump", "Chilled Water System"}, fabEquipmentTypes...)
var brands = []string{"Daikin", "Baltimore Aircoil", "Grundfos", "Atlas Copco", "Edwards", "Tool OEM", "Utility OEM"}
var series = append([]string{"WSC Centrifugal", "Vision AHU", "Series 3000", "NBG", "NKG", "ZR VSD+", "iXH", "Plant Controls"}, fabSeries...)

type issueCategory struct {
	Name   string
	Issues [][2]string // code, name
}

var issueCategories = append([]issueCategory{
	{"Refrigeration", [][2]string{{"REF-01", "High Refrigerant Pressure"}, {"REF-02", "Low Pressure / Refrigerant Charge"}, {"REF-03", "Oil System"}}},
	{"Mechanical", [][2]string{{"MEC-01", "Vibration"}, {"MEC-02", "Bearing"}, {"MEC-03", "Belt / Drive Train"}, {"MEC-04", "Seal Leak"}}},
	{"Thermal", [][2]string{{"THR-01", "Temperature Deviation"}, {"THR-02", "Heat Transfer"}}},
	{"Air Side", [][2]string{{"AIR-01", "Filter Loading"}, {"AIR-02", "Airflow"}, {"AIR-03", "Humidity Control"}}},
	{"Water Side", [][2]string{{"WTR-01", "Flow"}, {"WTR-02", "Pressure"}, {"WTR-03", "Water Quality"}, {"WTR-04", "Level"}}},
	{"Electrical", [][2]string{{"ELE-01", "Motor Overcurrent"}}},
	{"Process Utilities", [][2]string{{"PRC-01", "By-product Deposition"}, {"PRC-02", "Purge Gas"}, {"PRC-03", "Vacuum Performance"}, {"PRC-04", "Compressed Air Quality"}}},
	{"Sensors & Controls", [][2]string{{"SEN-01", "Sensor Bias / Calibration"}, {"CTL-01", "Control Loop Tuning"}, {"VLV-01", "Valve Leakage / Stuck"}}},
}, fabIssueCategories...)

// fddCodes are the alarm codes of the LBNL fault detection rules (analytics
// rule code = alarm code); the engine raises them as external alarms.
var fddCodes = []string{"CHL-011", "CT-013", "CT-014", "CHS-005", "CHS-006", "CHS-007", "CHS-008"}

func isFDDCode(code string) bool { return contains(fddCodes, code) }

// alarmCodes lists every alarm code (master data, "CODE - Name").
var alarmCodes = append([][2]string{
	{"CHL-001", "High Condenser Pressure"},
	{"CHL-002", "Low Evaporator Pressure"},
	{"CHL-003", "High Compressor Discharge Temperature"},
	{"CHL-004", "Low Oil Pressure Differential"},
	{"CHL-005", "Condenser Tube Fouling"},
	{"CHL-006", "Refrigerant Leak / Low Charge"},
	{"CHL-007", "High Condenser Approach Temperature"},
	{"CHL-008", "Chilled Water Supply Temperature Deviation"},
	{"CHL-009", "High Compressor Vibration"},
	{"CHL-010", "Compressor Bearing Wear"},
	{"CHL-011", "Chiller CHW Leaving Temperature Sensor Bias"},
	{"AHU-001", "High Pre-Filter Differential Pressure"},
	{"AHU-002", "High HEPA Filter Differential Pressure"},
	{"AHU-003", "Supply Air Temperature Deviation"},
	{"AHU-004", "High Supply Air Humidity"},
	{"AHU-005", "Fan Motor Overcurrent"},
	{"AHU-006", "Low Supply Airflow"},
	{"AHU-010", "Pre-Filter Clogging"},
	{"AHU-011", "Cooling Coil Fouling / CHW Valve Fault"},
	{"AHU-012", "Fan Belt Slippage"},
	{"CT-001", "High Condenser Water Supply Temperature"},
	{"CT-002", "High Fan Vibration"},
	{"CT-003", "High Water Conductivity"},
	{"CT-004", "Low Basin Water Level"},
	{"CT-005", "High Tower Approach"},
	{"CT-010", "Fan Gearbox / Bearing Failure"},
	{"CT-011", "Fill Media Scaling"},
	{"CT-012", "Make-up Water Valve Fault"},
	{"CT-013", "Tower Leaving Water Temperature Sensor Bias"},
	{"CT-014", "Cooling Tower Heat Exchanger Fouling"},
	{"PMP-001", "Low Suction Pressure"},
	{"PMP-002", "High Pump Vibration"},
	{"PMP-003", "High Bearing Temperature"},
	{"PMP-004", "Low Flow"},
	{"PMP-005", "Motor Overload"},
	{"PMP-010", "Cavitation / Suction Strainer Clogging"},
	{"PMP-011", "Pump Bearing Failure"},
	{"PMP-012", "Impeller Wear"},
	{"CDA-001", "High HP Element Outlet Temperature"},
	{"CDA-002", "Low Discharge Pressure"},
	{"CDA-003", "High Pressure Dew Point"},
	{"CDA-004", "Motor Overload"},
	{"CDA-005", "High Intercooler Outlet Temperature"},
	{"CDA-010", "Intercooler Fouling"},
	{"CDA-011", "Desiccant Dryer Malfunction"},
	{"CDA-012", "Compressed Air Leak in Distribution"},
	{"VAC-001", "High Dry Pump Motor Current"},
	{"VAC-002", "High Pump Body Temperature"},
	{"VAC-003", "Poor Inlet Pressure"},
	{"VAC-004", "Low N2 Purge Flow"},
	{"VAC-005", "Low Cooling Water Flow"},
	{"VAC-010", "Process By-product Deposition"},
	{"VAC-011", "Rotor / Booster Wear"},
	{"VAC-012", "N2 Purge Line Blockage"},
	{"CHS-001", "High CHW Supply Header Temperature"},
	{"CHS-002", "Low Riser Differential Pressure"},
	{"CHS-003", "Low CHW Delta T"},
	{"CHS-004", "Poor Chiller Plant Efficiency"},
	{"CHS-005", "Riser Differential Pressure Sensor Bias"},
	{"CHS-006", "Condenser 3-Way Valve Leakage"},
	{"CHS-007", "Condenser 3-Way Valve Stuck"},
	{"CHS-008", "CW Supply Temperature Loop Hunting (PI Tuning)"},
	{"PM-CHL", "Chiller Monthly Preventive Maintenance"},
	{"PM-MAU", "MAU Monthly Filter and Fan Service"},
	{"PM-CT", "Cooling Tower Monthly Maintenance"},
	{"PM-PCW", "PCW Pump Monthly Inspection"},
	{"PM-CDA", "Compressor Monthly Service"},
	{"PM-VAC", "Dry Pump Preventive Overhaul"},
	{"GEN-001", "Mechanical Seal Leak"},
	{"GEN-002", "Abnormal Noise"},
	{"GEN-003", "Damaged Insulation / Panel"},
}, fabAlarmCodes...)

func codeName(code string) string {
	for _, c := range alarmCodes {
		if c[0] == code {
			return c[1]
		}
	}
	return code
}

// ---------------------------------------------------------------------------
// Models

func chillerModel() *ModelSpec {
	p := append(statusParams(),
		numP("load_pct", "Compressor Load", "%", 1),
		numP("chwst", "Chilled Water Supply Temp", "°C", 2),
		numP("chwrt", "Chilled Water Return Temp", "°C", 2),
		numP("chw_flow", "Chilled Water Flow", "m³/h", 0),
		numP("cond_flow", "Condenser Water Flow", "m³/h", 0),
		numP("cwet", "Condenser Water Entering Temp", "°C", 2),
		numP("cwlt", "Condenser Water Leaving Temp", "°C", 2),
		numP("evap_press", "Evaporator Pressure", "kPa", 0).W("<", 325),
		numP("cond_press", "Condenser Pressure", "kPa", 0).W(">", 1000).C(">", 1080),
		numP("evap_approach", "Evaporator Approach", "K", 2).W(">", 2.5).C(">", 3.5),
		numP("cond_approach", "Condenser Approach", "K", 2).W(">", 3.0).C(">", 4.5),
		numP("superheat", "Suction Superheat", "K", 2).W(">", 3.5).C(">", 5.0),
		numP("discharge_temp", "Compressor Discharge Temp", "°C", 1).W(">", 60),
		numP("oil_dp", "Oil Pressure Differential", "kPa", 0),
		numP("oil_temp", "Oil Sump Temp", "°C", 1).W(">", 58).C(">", 65),
		numP("vibration", "Compressor Vibration", "mm/s", 2).W(">", 4.5).C(">", 7.1),
		numP("motor_current", "Motor Current", "A", 0).W(">", 900),
		numP("power_kw", "Input Power", "kW", 1),
		numP("efficiency", "Efficiency", "kW/RT", 3).W(">", 0.7),
		numP("chwst_sp", "CHW Supply Temp Set Point", "°C", 1).SP(5.5, 8, 0.1, "", "chwst"),
		numP("run_mode", "Run Mode", "", 0).Modes("", "Auto", "On", "Off"),
		numP("chwst_asp", "Active CHWST Set Point", "°C", 2),
	)
	return &ModelSpec{
		Key: "chiller", Name: "Daikin WSC-126 Centrifugal Chiller", Type: "Centrifugal Chiller", Brand: "Daikin", Series: "WSC Centrifugal",
		Description: "1000 RT water-cooled centrifugal chiller, R-134a, 6.5/12 °C",
		Params:      p,
		Alarms: []AlarmSpec{
			{"CHL-001", "Condenser pressure above 1080 kPa - check condenser water flow and tube condition", true, "REF-01", gt("cond_press", 1080, false)},
			{"CHL-002", "Evaporator pressure below 300 kPa - possible low refrigerant charge", true, "REF-02", lt("evap_press", 300, true)},
			{"CHL-003", "Compressor discharge temperature above 65 °C", false, "THR-01", gt("discharge_temp", 65, false)},
			{"CHL-004", "Oil pressure differential below 90 kPa", true, "REF-03", lt("oil_dp", 90, true)},
			{"CHL-007", "Condenser approach above 3.0 K - heat rejection degrading", false, "THR-02", gt("cond_approach", 3.0, true)},
			{"CHL-008", "Chilled water supply deviates more than 1.0 K from its active set point", false, "THR-01", devP("chwst", "chwst_asp", 1.0)},
			{"CHL-009", "Compressor vibration above 7.1 mm/s (ISO 10816 zone C)", true, "MEC-01", gt("vibration", 7.1, false)},
		},
		Relations: []RelationSpec{
			{"Condenser Tube Fouling Diagnosis", "CHL-005", [][]string{{"A:CHL-007", "P:cond_press"}, {"A:CHL-001"}}},
			{"Refrigerant Leak Detection", "CHL-006", [][]string{{"P:superheat", "P:evap_approach"}, {"A:CHL-002"}}},
			{"Compressor Bearing Degradation", "CHL-010", [][]string{{"A:CHL-009", "P:oil_temp"}, {"A:CHL-004"}}},
			fddRelation("CHL-011"),
		},
		Faults: []FaultSpec{
			fault("cond_fouling", "Condenser tube fouling", "CHL-005", "CHL-001", "CHL-007"),
			fault("refrigerant_leak", "Refrigerant leak", "CHL-006", "CHL-002", "CHL-008"),
			fault("bearing_wear", "Compressor bearing wear", "CHL-010", "CHL-009", "CHL-004"),
			lbnl("chwst_bias", "CHW leaving temperature sensor bias", "K", []float64{-2, -1, 1, 2}, "CHL-011"),
		},
		Trips:  []TripSpec{{"CHL-003", "discharge_temp", 71, "Chiller tripped on high compressor discharge temperature"}},
		PMCode: "PM-CHL", PMRepeat: 3, PMHours: 4,
		RunHourWarn: 4000, RunHourCrit: 6000,
		Mapping: map[string][]string{
			"CHL-001": {"cond_press", "cwlt", "cond_approach"}, "CHL-002": {"evap_press", "superheat", "evap_approach"},
			"CHL-003": {"discharge_temp", "cond_press"}, "CHL-004": {"oil_dp", "oil_temp"}, "CHL-005": {"cond_approach", "cond_press", "cwlt", "efficiency"},
			"CHL-006": {"evap_press", "superheat", "evap_approach", "chwst"}, "CHL-007": {"cond_approach", "cwlt"},
			"CHL-008": {"chwst", "chwst_asp", "chwst_sp", "chwrt", "load_pct"}, "CHL-009": {"vibration"}, "CHL-010": {"vibration", "oil_temp", "oil_dp", "motor_current"},
			"CHL-011": {"chwst", "chwrt", "chw_flow", "load_pct", "chwst_asp"},
		},
	}
}

func mauModel() *ModelSpec {
	p := append(statusParams(),
		numP("oat", "Outdoor Air Temp", "°C", 1),
		numP("oa_rh", "Outdoor Air RH", "%", 0),
		numP("sat", "Supply Air Temp", "°C", 2),
		numP("sa_rh", "Supply Air RH", "%", 1).W(">", 52),
		numP("prefilter_dp", "Pre-Filter Differential Pressure", "Pa", 0).W(">", 200).C(">", 250),
		numP("hepa_dp", "HEPA Filter Differential Pressure", "Pa", 0).W(">", 400),
		numP("fan_speed", "Fan Speed", "%", 1).W(">", 90),
		numP("fan_current", "Fan Motor Current", "A", 1).W(">", 52),
		numP("airflow", "Supply Airflow", "m³/h", 0),
		numP("chw_valve", "CHW Valve Position", "%", 0).W(">", 95),
		numP("duct_static", "Duct Static Pressure", "Pa", 0),
	)
	return &ModelSpec{
		Key: "mau", Name: "Daikin Vision MAU-60K", Type: "Make-up Air Unit", Brand: "Daikin", Series: "Vision AHU",
		Description: "60,000 m³/h cleanroom make-up air unit with pre-filter, cooling coil and HEPA stage",
		Params:      p,
		Alarms: []AlarmSpec{
			{"AHU-001", "Pre-filter differential pressure above 250 Pa", false, "AIR-01", gt("prefilter_dp", 250, false)},
			{"AHU-002", "HEPA filter differential pressure above 450 Pa", false, "AIR-01", gt("hepa_dp", 450, false)},
			{"AHU-003", "Supply air temperature deviates more than 1.5 K from 17 °C set point", false, "THR-01", dev("sat", 17, 1.5)},
			{"AHU-004", "Supply air relative humidity above 55 % - cleanroom humidity at risk", true, "AIR-03", gt("sa_rh", 55, true)},
			{"AHU-005", "Fan motor current above 62 A", true, "ELE-01", gt("fan_current", 62, false)},
			{"AHU-006", "Supply airflow below 50,000 m³/h", true, "AIR-02", lt("airflow", 50000, true)},
		},
		Relations: []RelationSpec{
			{"Pre-Filter Clogging Diagnosis", "AHU-010", [][]string{{"A:AHU-001"}, {"P:prefilter_dp", "P:fan_current"}}},
			{"Cooling Coil Performance", "AHU-011", [][]string{{"A:AHU-003", "P:chw_valve"}, {"A:AHU-004"}}},
			{"Fan Drive Slippage", "AHU-012", [][]string{{"A:AHU-006", "P:fan_speed"}}},
		},
		Faults: []FaultSpec{
			fault("filter_clog", "Pre-filter clogging", "AHU-010", "AHU-001", "PM-MAU"),
			fault("coil_fouling", "Cooling coil fouling", "AHU-011", "AHU-003", "AHU-004"),
			fault("belt_slip", "Fan belt slippage", "AHU-012", "AHU-006"),
		},
		Trips:  []TripSpec{{"AHU-005", "fan_current", 66, "Fan VFD tripped on motor overcurrent"}},
		PMCode: "PM-MAU", PMRepeat: 3, PMHours: 3,
		Mapping: map[string][]string{
			"AHU-001": {"prefilter_dp", "fan_speed"}, "AHU-002": {"hepa_dp"}, "AHU-003": {"sat", "chw_valve", "oat"},
			"AHU-004": {"sa_rh", "chw_valve", "oa_rh"}, "AHU-005": {"fan_current", "fan_speed"}, "AHU-006": {"airflow", "fan_speed", "duct_static"},
			"AHU-010": {"prefilter_dp", "fan_current", "fan_speed"}, "AHU-011": {"sat", "sa_rh", "chw_valve"}, "AHU-012": {"airflow", "fan_speed", "fan_current"},
		},
	}
}

func coolingTowerModel() *ModelSpec {
	p := append(statusParams(),
		numP("cwst", "Condenser Water Supply Temp", "°C", 2),
		numP("cwrt", "Condenser Water Return Temp", "°C", 2),
		numP("wet_bulb", "Ambient Wet Bulb Temp", "°C", 2),
		numP("approach", "Tower Approach", "K", 2).W(">", 5.0).C(">", 6.5),
		numP("fan_speed", "Fan Speed", "%", 1),
		numP("fan_current", "Fan Motor Current", "A", 1).W(">", 45),
		numP("fan_power", "Fan Power", "kW", 1),
		numP("fan_vibration", "Fan Gearbox Vibration", "mm/s", 2).W(">", 4.5).C(">", 7.1),
		numP("basin_level", "Basin Water Level", "%", 0).W("<", 55).C("<", 40),
		numP("conductivity", "Water Conductivity", "µS/cm", 0).W(">", 2200).C(">", 2600),
		numP("fan_mode", "Fan Control Mode", "", 0).Modes("fan_speed", "Auto", "Manual"),
		numP("fan_speed_sp", "Fan Speed Set Point", "%", 0).SP(30, 100, 1, "fan_mode", "fan_speed").Def(65),
	)
	return &ModelSpec{
		Key: "ct", Name: "BAC S3E-1424 Cooling Tower", Type: "Cooling Tower", Brand: "Baltimore Aircoil", Series: "Series 3000",
		Description: "Induced-draft crossflow cooling tower cell, 1,250 RT heat rejection",
		Params:      p,
		Alarms: []AlarmSpec{
			{"CT-001", "Condenser water supply above 32.5 °C", false, "THR-01", gt("cwst", 32.5, true)},
			{"CT-002", "Fan gearbox vibration above 7.1 mm/s", true, "MEC-01", gt("fan_vibration", 7.1, false)},
			{"CT-003", "Water conductivity above 2,500 µS/cm - check blowdown and chemical dosing", false, "WTR-03", gt("conductivity", 2500, false)},
			{"CT-004", "Basin water level below 40 %", true, "WTR-04", lt("basin_level", 40, false)},
			{"CT-005", "Tower approach above 6.0 K", false, "THR-02", gt("approach", 6.0, true)},
		},
		Relations: []RelationSpec{
			{"Fan Drive Train Failure", "CT-010", [][]string{{"A:CT-002"}, {"P:fan_vibration", "P:fan_current"}}},
			{"Fill Scaling Diagnosis", "CT-011", [][]string{{"P:approach", "P:conductivity"}, {"A:CT-005"}}},
			{"Make-up Water Supply Fault", "CT-012", [][]string{{"A:CT-004"}, {"P:basin_level"}}},
			fddRelation("CT-013"),
			fddRelation("CT-014"),
		},
		Faults: []FaultSpec{
			fault("gearbox_wear", "Fan gearbox wear", "CT-010", "CT-002"),
			fault("fill_scaling", "Fill media scaling", "CT-011", "CT-005", "CT-003", "CT-001"),
			fault("makeup_valve", "Make-up valve fault", "CT-012", "CT-004"),
			lbnl("cwst_bias", "Tower leaving water temperature sensor bias", "K", []float64{-2, -1, 1, 2}, "CT-013"),
			lbnl("ct_fouling", "Tower heat exchanger fouling (capacity factor)", "capacity factor", []float64{0.95, 0.80, 0.65}, "CT-014"),
		},
		Trips:  []TripSpec{{"CT-002", "", 0, "Fan tripped on vibration switch"}},
		PMCode: "PM-CT", PMRepeat: 3, PMHours: 3,
		Mapping: map[string][]string{
			"CT-001": {"cwst", "wet_bulb", "approach"}, "CT-002": {"fan_vibration", "fan_current"}, "CT-003": {"conductivity"},
			"CT-004": {"basin_level"}, "CT-005": {"approach", "fan_speed"}, "CT-010": {"fan_vibration", "fan_current"},
			"CT-011": {"approach", "conductivity", "cwst"}, "CT-012": {"basin_level"},
			"CT-013": {"cwst", "cwrt", "approach", "fan_speed"}, "CT-014": {"approach", "fan_speed", "fan_power", "cwst", "wet_bulb"},
		},
	}
}

// PumpRating sizes a centrifugal pump for the physics and its thresholds.
type PumpRating struct {
	Flow     float64 // rated flow (m³/h)
	DP       float64 // rated differential pressure (bar)
	PowerKW  float64 // shaft power at the rated point
	Suction  float64 // normal suction pressure (bar)
	SpeedHz  float64
	MinFlow  float64 // low-flow alarm
	SuctWarn float64 // low suction pressure warning / critical (cavitation)
	SuctCrit float64
	// Spec is the normal specific energy (kWh/m³) the specific energy alarms
	// are scaled from; 0 = the rated point (PowerKW / Flow).
	Spec float64
}

var pumpRatings = map[string]PumpRating{
	"pcw": {Flow: 318, DP: 4.2, PowerKW: 52, Suction: 2.05, SpeedHz: 48, MinFlow: 220, SuctWarn: 1.4, SuctCrit: 1.0},
	// Variable primary: the CHW pumps follow the riser DP and the minimum-flow
	// bypass holds 60 % of the design chiller flow (330 m³/h), so the low-flow
	// alarm sits just below what normal operation reaches. They run well below
	// their rated head (about 0.061 kWh/m³ at the design load, at most 0.076 at
	// full speed): the specific energy alarms (0.080 / 0.096) are scaled from
	// that operating point, so a worn impeller shows before the riser DP sags.
	"chwp": {Flow: 550, DP: 3.1, PowerKW: 58, Suction: 2.6, SpeedHz: 46, MinFlow: 320, SuctWarn: 1.9, SuctCrit: 1.5, Spec: 0.064},
	"cwp":  {Flow: 640, DP: 2.4, PowerKW: 54, Suction: 1.6, SpeedHz: 47, MinFlow: 500, SuctWarn: 1.1, SuctCrit: 0.8},
}

// pumpModel builds a centrifugal pump model; the variants share alarm codes and
// diagnostics, with thresholds scaled from the pump rating.
func pumpModel(key, name, typ, series, desc, label string) *ModelSpec {
	r := pumpRatings[key]
	amps := r.PowerKW * 1000 / (1.732 * 400 * 0.88)
	spec := r.PowerKW / r.Flow
	if r.Spec > 0 {
		spec = r.Spec
	}
	round := func(v float64, d int) float64 { k := math.Pow(10, float64(d)); return math.Round(v*k) / k }
	p := append(statusParams(),
		numP("speed", "Pump Speed", "Hz", 1),
		numP("suction_press", "Suction Pressure", "bar", 2).W("<", r.SuctWarn).C("<", r.SuctCrit),
		numP("discharge_press", "Discharge Pressure", "bar", 2),
		numP("flow", "Flow Rate", "m³/h", 1),
		numP("motor_current", "Motor Current", "A", 1).W(">", round(amps*1.15, 0)),
		numP("power_kw", "Input Power", "kW", 1),
		numP("spec_energy", "Specific Energy", "kWh/m³", 3).W(">", round(spec*1.25, 3)).C(">", round(spec*1.5, 3)),
		numP("vibration", "Pump Vibration", "mm/s", 2).W(">", 4.5).C(">", 7.1),
		numP("bearing_temp", "Bearing Temperature", "°C", 1).W(">", 75).C(">", 85),
		numP("motor_temp", "Motor Winding Temperature", "°C", 1),
		numP("speed_mode", "Speed Control Mode", "", 0).Modes("speed", "Auto", "Manual"),
		// the VFD maximum is the design speed: faster, the motor overloads (PMP-005)
		numP("speed_sp", "Speed Set Point", "Hz", 1).SP(30, r.SpeedHz, 0.5, "speed_mode", "speed").Def(r.SpeedHz),
	)
	ampTrip := round(amps*1.25, 0)
	return &ModelSpec{
		Key: key, Name: name, Type: typ, Brand: "Grundfos", Series: series, Description: desc, Params: p,
		Alarms: []AlarmSpec{
			{"PMP-001", fmt.Sprintf("Suction pressure below %s bar - risk of cavitation", fnum(r.SuctCrit)), true, "WTR-02", lt("suction_press", r.SuctCrit, true)},
			{"PMP-002", "Pump vibration above 7.1 mm/s", true, "MEC-01", gt("vibration", 7.1, false)},
			{"PMP-003", "Bearing temperature above 85 °C", false, "MEC-02", gt("bearing_temp", 85, false)},
			{"PMP-004", fmt.Sprintf("Flow below %s m³/h", fnum(r.MinFlow)), false, "WTR-01", lt("flow", r.MinFlow, true)},
			{"PMP-005", fmt.Sprintf("Motor current above %s A", fnum(ampTrip)), true, "ELE-01", gt("motor_current", ampTrip, false)},
		},
		Relations: []RelationSpec{
			{label + " Cavitation Detection", "PMP-010", [][]string{{"P:suction_press", "P:vibration"}, {"A:PMP-001"}}},
			{label + " Bearing Failure Prediction", "PMP-011", [][]string{{"A:PMP-002", "P:bearing_temp"}, {"A:PMP-003"}}},
			{label + " Impeller Wear Detection", "PMP-012", [][]string{{"A:PMP-004"}, {"P:spec_energy"}}},
		},
		Faults: []FaultSpec{
			fault("cavitation", "Suction strainer clogging / cavitation", "PMP-010", "PMP-001"),
			fault("bearing_wear", "Pump bearing wear", "PMP-011", "PMP-002", "PMP-003"),
			fault("impeller_wear", "Impeller wear", "PMP-012", "PMP-004"),
		},
		Trips:  []TripSpec{{"PMP-005", "motor_current", round(amps*1.35, 0), "Pump tripped on motor overload"}},
		PMCode: "PM-PCW", PMRepeat: 3, PMHours: 2,
		Mapping: map[string][]string{
			"PMP-001": {"suction_press", "flow"}, "PMP-002": {"vibration"}, "PMP-003": {"bearing_temp", "vibration"},
			"PMP-004": {"flow", "discharge_press"}, "PMP-005": {"motor_current", "motor_temp"}, "PMP-010": {"suction_press", "vibration", "flow"},
			"PMP-011": {"vibration", "bearing_temp", "motor_current"}, "PMP-012": {"flow", "spec_energy", "discharge_press"},
		},
	}
}

func pcwPumpModel() *ModelSpec {
	p := append(statusParams(),
		numP("speed", "Pump Speed", "Hz", 1),
		numP("suction_press", "Suction Pressure", "bar", 2).W("<", 1.4).C("<", 1.0),
		numP("discharge_press", "Discharge Pressure", "bar", 2),
		numP("flow", "Flow Rate", "m³/h", 1),
		numP("motor_current", "Motor Current", "A", 1).W(">", 115),
		numP("spec_energy", "Specific Energy", "kWh/m³", 3).W(">", 0.21).C(">", 0.25),
		numP("vibration", "Pump Vibration", "mm/s", 2).W(">", 4.5).C(">", 7.1),
		numP("bearing_temp", "Bearing Temperature", "°C", 1).W(">", 75).C(">", 85),
		numP("motor_temp", "Motor Winding Temperature", "°C", 1),
	)
	return &ModelSpec{
		Key: "pcw", Name: "Grundfos NBG 150-125-315 PCW Pump", Type: "Process Cooling Water Pump", Brand: "Grundfos", Series: "NBG",
		Description: "75 kW end-suction process cooling water pump with VSD, 320 m³/h at 42 m",
		Params:      p,
		Alarms: []AlarmSpec{
			{"PMP-001", "Suction pressure below 1.0 bar - risk of cavitation", true, "WTR-02", lt("suction_press", 1.0, true)},
			{"PMP-002", "Pump vibration above 7.1 mm/s", true, "MEC-01", gt("vibration", 7.1, false)},
			{"PMP-003", "Bearing temperature above 85 °C", false, "MEC-02", gt("bearing_temp", 85, false)},
			{"PMP-004", "Flow below 220 m³/h", false, "WTR-01", lt("flow", 220, true)},
			{"PMP-005", "Motor current above 125 A", true, "ELE-01", gt("motor_current", 125, false)},
		},
		Relations: []RelationSpec{
			{"Pump Cavitation Detection", "PMP-010", [][]string{{"P:suction_press", "P:vibration"}, {"A:PMP-001"}}},
			{"Pump Bearing Failure Prediction", "PMP-011", [][]string{{"A:PMP-002", "P:bearing_temp"}, {"A:PMP-003"}}},
			{"Impeller Wear Detection", "PMP-012", [][]string{{"A:PMP-004"}, {"P:spec_energy"}}},
		},
		Faults: []FaultSpec{
			fault("cavitation", "Suction strainer clogging / cavitation", "PMP-010", "PMP-001"),
			fault("bearing_wear", "Pump bearing wear", "PMP-011", "PMP-002", "PMP-003"),
			fault("impeller_wear", "Impeller wear", "PMP-012", "PMP-004"),
		},
		Trips:  []TripSpec{{"PMP-005", "motor_current", 132, "Pump tripped on motor overload"}},
		PMCode: "PM-PCW", PMRepeat: 3, PMHours: 2,
		Mapping: map[string][]string{
			"PMP-001": {"suction_press", "flow"}, "PMP-002": {"vibration"}, "PMP-003": {"bearing_temp", "vibration"},
			"PMP-004": {"flow", "discharge_press"}, "PMP-005": {"motor_current", "motor_temp"}, "PMP-010": {"suction_press", "vibration", "flow"},
			"PMP-011": {"vibration", "bearing_temp", "motor_current"}, "PMP-012": {"flow", "spec_energy", "discharge_press"},
		},
	}
}

func chwPumpModel() *ModelSpec {
	return pumpModel("chwp", "Grundfos NKG 200-150-315 CHW Pump", "Chilled Water Pump", "NKG",
		"75 kW end-suction primary chilled water pump, dedicated to one chiller, 550 m³/h at 31 m", "CHW Pump")
}

func cwPumpModel() *ModelSpec {
	return pumpModel("cwp", "Grundfos NKG 250-200-315 CW Pump", "Condenser Water Pump", "NKG",
		"75 kW end-suction condenser water pump, dedicated to one chiller / tower pair, 640 m³/h at 24 m", "CW Pump")
}

func compressorModel() *ModelSpec {
	p := append(statusParams(),
		numP("discharge_press", "Discharge Pressure", "bar(g)", 2).W("<", 7.0).C("<", 6.8),
		numP("load_pct", "Compressor Load", "%", 1).W(">", 95),
		numP("flow", "Free Air Delivery", "Nm³/min", 1),
		numP("motor_current", "Motor Current", "A", 0).W(">", 560),
		numP("lp_elem_temp", "LP Element Outlet Temp", "°C", 1),
		numP("hp_elem_temp", "HP Element Outlet Temp", "°C", 1).W(">", 200).C(">", 220),
		numP("intercooler_temp", "Intercooler Outlet Temp", "°C", 1).W(">", 40).C(">", 45),
		numP("dew_point", "Pressure Dew Point", "°C", 1).W(">", -35).C(">", -30),
		numP("cooling_water_temp", "Cooling Water Inlet Temp", "°C", 1),
	)
	return &ModelSpec{
		Key: "cda", Name: "Atlas Copco ZR 315 VSD+", Type: "Air Compressor", Brand: "Atlas Copco", Series: "ZR VSD+",
		Description: "315 kW oil-free two-stage water-cooled compressor with MD desiccant dryer (clean dry air)",
		Params:      p,
		Alarms: []AlarmSpec{
			{"CDA-001", "HP element outlet temperature above 220 °C", true, "THR-01", gt("hp_elem_temp", 220, false)},
			{"CDA-002", "CDA header pressure below 6.8 bar(g)", false, "PRC-04", lt("discharge_press", 6.8, true)},
			{"CDA-003", "Pressure dew point above -30 °C - CDA quality out of spec", true, "PRC-04", gt("dew_point", -30, false)},
			{"CDA-004", "Motor current above 600 A", true, "ELE-01", gt("motor_current", 600, false)},
			{"CDA-005", "Intercooler outlet temperature above 45 °C", false, "THR-02", gt("intercooler_temp", 45, true)},
		},
		Relations: []RelationSpec{
			{"Intercooler Fouling Diagnosis", "CDA-010", [][]string{{"A:CDA-005", "P:hp_elem_temp"}, {"A:CDA-001"}}},
			{"Dryer Performance Monitoring", "CDA-011", [][]string{{"A:CDA-003"}, {"P:dew_point"}}},
			{"Distribution Leak Detection", "CDA-012", [][]string{{"P:load_pct", "P:discharge_press"}, {"A:CDA-002"}}},
		},
		Faults: []FaultSpec{
			fault("intercooler_fouling", "Intercooler fouling", "CDA-010", "CDA-005", "CDA-001"),
			fault("dryer_fault", "Desiccant dryer malfunction", "CDA-011", "CDA-003"),
			fault("air_leak", "Compressed air leak", "CDA-012", "CDA-002"),
		},
		Trips:  []TripSpec{{"CDA-004", "motor_current", 640, "Compressor tripped on motor overload"}},
		PMCode: "PM-CDA", PMRepeat: 3, PMHours: 4,
		RunHourWarn: 2000, RunHourCrit: 3000,
		Mapping: map[string][]string{
			"CDA-001": {"hp_elem_temp", "intercooler_temp", "cooling_water_temp"}, "CDA-002": {"discharge_press", "load_pct", "flow"},
			"CDA-003": {"dew_point"}, "CDA-004": {"motor_current", "load_pct"}, "CDA-005": {"intercooler_temp", "cooling_water_temp"},
			"CDA-010": {"intercooler_temp", "hp_elem_temp", "lp_elem_temp"}, "CDA-011": {"dew_point"}, "CDA-012": {"load_pct", "discharge_press", "flow"},
		},
	}
}

func dryPumpParams() []ParamSpec {
	return append(statusParams(),
		numP("inlet_press", "Inlet Pressure", "mbar", 3).W(">", 0.8).C(">", 1.5),
		numP("dp_current", "Dry Pump Motor Current", "A", 2).W(">", 12).C(">", 14),
		numP("booster_current", "Booster Motor Current", "A", 2).W(">", 4.5),
		numP("body_temp", "Pump Body Temperature", "°C", 1).W(">", 100).C(">", 110),
		numP("exhaust_press", "Exhaust Pressure", "mbar", 0).W(">", 1150),
		numP("n2_flow", "N2 Purge Flow", "slm", 1).W("<", 38).C("<", 30),
		numP("cooling_water_flow", "Cooling Water Flow", "l/min", 1).W("<", 3.0).C("<", 2.5),
	)
}

func dryPumpBase() *ModelSpec {
	return &ModelSpec{
		Key: "vac", Name: "Edwards iXH Dry Pump (Base)", Type: "Dry Vacuum Pump", Brand: "Edwards", Series: "iXH", IsBase: true,
		Description: "Base definition for Edwards iXH harsh-process dry pumps (screw pump with roots booster)",
		Params:      dryPumpParams(),
		Alarms: []AlarmSpec{
			{"VAC-001", "Dry pump motor current above 13 A", false, "PRC-01", gt("dp_current", 13, false)},
			{"VAC-002", "Pump body temperature above 110 °C", true, "THR-01", gt("body_temp", 110, false)},
			{"VAC-003", "Inlet pressure above 1.5 mbar while running", false, "PRC-03", gt("inlet_press", 1.5, true)},
			{"VAC-004", "N2 purge flow below 30 slm", true, "PRC-02", lt("n2_flow", 30, false)},
			{"VAC-005", "Cooling water flow below 2.5 l/min", true, "WTR-01", lt("cooling_water_flow", 2.5, false)},
		},
		RunHourWarn: 1000, RunHourCrit: 1300, AutoPM: true,
	}
}

func dryPumpModel(name, desc string, overrides map[string]CondSpec) *ModelSpec {
	base := dryPumpBase()
	m := *base
	m.Name, m.Description, m.IsBase, m.Base = name, desc, false, base.Name
	m.Overrides = overrides
	m.Relations = []RelationSpec{
		{"By-product Deposition Detection", "VAC-010", [][]string{{"A:VAC-001", "P:body_temp"}, {"A:VAC-002"}}},
		{"Rotor Wear Detection", "VAC-011", [][]string{{"A:VAC-003"}, {"P:inlet_press", "P:booster_current"}}},
		{"N2 Purge Blockage", "VAC-012", [][]string{{"A:VAC-004"}, {"P:n2_flow"}}},
	}
	m.Faults = []FaultSpec{
		fault("deposition", "Process by-product deposition", "VAC-010", "VAC-001", "VAC-002", "PM-VAC"),
		fault("rotor_wear", "Rotor and booster wear", "VAC-011", "VAC-003", "PM-VAC"),
		fault("purge_block", "N2 purge line blockage", "VAC-012", "VAC-004"),
	}
	m.Trips = []TripSpec{{"VAC-001", "dp_current", 15.5, "Dry pump tripped on motor overcurrent"}}
	m.PMCode, m.PMRepeat, m.PMHours = "PM-VAC", 4, 6
	m.Mapping = map[string][]string{
		"VAC-001": {"dp_current", "body_temp"}, "VAC-002": {"body_temp", "dp_current", "exhaust_press"}, "VAC-003": {"inlet_press", "booster_current"},
		"VAC-004": {"n2_flow"}, "VAC-005": {"cooling_water_flow", "body_temp"}, "VAC-010": {"dp_current", "body_temp", "exhaust_press"},
		"VAC-011": {"inlet_press", "booster_current", "dp_current"}, "VAC-012": {"n2_flow"},
	}
	return &m
}

// chwSystemModel is the BMS plant controller of the chiller plant as a whole:
// the points it exposes on top of the individual chillers, pumps and towers
// (CHW headers, riser differential pressure, bypass, condenser water loop,
// 3-way tower bypass valve, plant efficiency) and its supervisory set points
// (CHW supply reset, chiller staging, tower approach, riser DP).
func chwSystemModel() *ModelSpec {
	p := append(statusParams(),
		numP("chws_temp", "CHW Supply Header Temp", "°C", 2).W(">", 7.5).C(">", 8.5),
		numP("chwr_temp", "CHW Return Header Temp", "°C", 2).W(">", 13.5),
		numP("delta_t", "CHW Header Delta T", "K", 2).W("<", 3.2),
		numP("plant_flow", "Plant CHW Flow", "m³/h", 0),
		numP("fab_flow", "Fab CHW Flow", "m³/h", 0),
		numP("bypass_flow", "Bypass Flow", "m³/h", 0),
		numP("bypass_valve", "Bypass Valve Position", "%", 1).W(">", 40),
		numP("riser_dp", "Riser Differential Pressure", "kPa", 1).W("<", 95).C("<", 80),
		numP("cooling_load", "Plant Cooling Load", "kW", 0),
		numP("plant_kw", "Plant Power", "kW", 0),
		numP("plant_eff", "Plant Efficiency", "kW/RT", 3).W(">", 0.85),
		numP("chillers_running", "Chillers Running", "", 0),
		numP("oa_temp", "Outdoor Air Temp", "°C", 1),
		numP("wet_bulb", "Outdoor Wet Bulb Temp", "°C", 2),
		numP("chws_reset_mode", "CHW Set Point Mode", "", 0).Modes("chws_temp", "Reset", "Manual"),
		numP("chws_sp", "CHW Supply Set Point", "°C", 1).SP(5.5, 8.0, 0.1, "chws_reset_mode", "chws_temp"),
		numP("staging_mode", "Chiller Staging Mode", "", 0).Modes("chillers_running", "Load", "Manual"),
		numP("chillers_sp", "Duty Chillers Set Point", "", 0).SP(1, 3, 1, "staging_mode", "chillers_running").Def(2),
		numP("approach_sp", "Tower Approach Set Point", "K", 1).SP(2.0, 8.0, 0.1, "", "cwst_sp").Def(3.5),
		numP("cw_min_temp", "Min Condenser Water Temp", "°C", 1).SP(15.0, 28.0, 0.5, "", "cw_supply_temp").Def(20.0),
		numP("cwst_sp", "Tower Leaving Water Set Point", "°C", 2),
		numP("tower_supply_temp", "Tower Leaving Water Header Temp", "°C", 2),
		numP("cw_supply_temp", "CW Supply Temp", "°C", 2),
		numP("cw_return_temp", "CW Return Temp", "°C", 2),
		numP("cw_flow", "Condenser Water Flow", "m³/h", 0),
		numP("fan_speed_cmd", "Tower Fan Speed Command", "%", 1),
		numP("twv_min_pos", "3-Way Valve Min Position", "%", 0).SP(0, 50, 1, "", "twv_pos").Def(0),
		numP("twv_cmd", "3-Way Valve Command", "%", 1),
		numP("twv_pos", "3-Way Valve Position", "%", 1),
		numP("riser_dp_sp", "Riser DP Set Point", "kPa", 0).SP(80, 160, 1, "", "riser_dp").Def(120),
		numP("chwp_speed_cmd", "CHW Pump Speed Command", "Hz", 1),
	)
	return &ModelSpec{
		Key: "chws", Name: "CUB Chilled Water Plant Controller", Type: "Chilled Water System", Brand: "Daikin", Series: "Plant Controls",
		Description: "Chiller plant controller (CHW and CW loops)",
		Params:      p,
		Alarms: []AlarmSpec{
			{"CHS-001", "CHW supply header above 8.5 °C - fab cooling coils losing capacity", true, "THR-01", gt("chws_temp", 8.5, true)},
			{"CHS-002", "Riser differential pressure below 80 kPa - remote MAU coils starved of flow", true, "WTR-02", lt("riser_dp", 80, true)},
			{"CHS-003", "Header delta T below 3.0 K - low delta T syndrome, excess pumping", false, "THR-02", lt("delta_t", 3.0, true)},
			{"CHS-004", "Plant efficiency worse than 0.85 kW/RT", false, "THR-02", gt("plant_eff", 0.85, true)},
		},
		Relations: []RelationSpec{fddRelation("CHS-005"), fddRelation("CHS-006"), fddRelation("CHS-007"), fddRelation("CHS-008")},
		Faults: []FaultSpec{
			lbnl("riser_dp_bias", "Riser differential pressure sensor bias (fraction of reading)", "fraction", []float64{-0.20, -0.10, 0.10, 0.20}, "CHS-005"),
			lbnl("twv_leak", "Condenser 3-way valve leakage (bypassed fraction)", "fraction", []float64{0.25, 0.50, 0.75}, "CHS-006"),
			lbnl("twv_stuck", "Condenser 3-way valve stuck (position fraction)", "fraction", []float64{0.50, 0.75}, "CHS-007"),
			lbnl("cw_pi_tuning", "Tower fan speed PI loop detuned", "", []float64{1}, "CHS-008"),
		},
		Mapping: map[string][]string{
			"CHS-001": {"chws_temp", "chillers_running", "cooling_load"}, "CHS-002": {"riser_dp", "plant_flow", "fab_flow"},
			"CHS-003": {"delta_t", "bypass_valve", "bypass_flow"}, "CHS-004": {"plant_eff", "plant_kw", "cooling_load"},
			"CHS-005": {"riser_dp", "riser_dp_sp", "chwp_speed_cmd", "plant_flow"},
			"CHS-006": {"twv_pos", "twv_min_pos", "cw_supply_temp", "tower_supply_temp", "cw_return_temp"},
			"CHS-007": {"twv_pos", "twv_cmd", "cw_supply_temp"},
			"CHS-008": {"fan_speed_cmd", "tower_supply_temp", "cwst_sp"},
		},
	}
}

// models returns every model; base models come before their extensions.

func models() []*ModelSpec {
	out := []*ModelSpec{
		chillerModel(), coolingTowerModel(), pcwPumpModel(), chwPumpModel(), cwPumpModel(), compressorModel(), mauModel(), chwSystemModel(),
		dryPumpBase(),
		dryPumpModel("Edwards iXH1220H Dry Pump (Etch)", "iXH1220H for etch process chambers (corrosive and condensable chemistries)", nil),
		dryPumpModel("Edwards iXH610 Dry Pump (CVD)", "iXH610 for CVD process chambers; runs hotter to avoid condensation",
			map[string]CondSpec{"VAC-002": gt("body_temp", 115, false)}),
	}
	out = append(out, subFabModels()...)
	return append(out, processToolModels()...)
}

// ---------------------------------------------------------------------------
// Locations (BUILDING_LEVEL 0: buildings > floors > rooms)

type LocSpec struct {
	Name, Parent string
	Image        string // image style
	Avail, Perf  float64
}

var locations = []LocSpec{
	{"FAB1 Wafer Fab", "", "fab", 95, 90},
	{"FAB1 L1 Sub-Fab", "FAB1 Wafer Fab", "floor", 95, 90},
	{locSubNorth, "FAB1 L1 Sub-Fab", "room", 95, 90},
	{locSubSouth, "FAB1 L1 Sub-Fab", "room", 95, 90},
	{locCleanroom, "FAB1 Wafer Fab", "floor", 90, 85},
	{locPhoto, locCleanroom, "room", 90, 85},
	{locEtchFilm, locCleanroom, "room", 90, 85},
	{locImpCMP, locCleanroom, "room", 90, 85},
	{locDiff, locCleanroom, "room", 90, 85},
	{locWetMet, locCleanroom, "room", 90, 85},
	{"FAB1 L4 Mechanical Deck", "FAB1 Wafer Fab", "floor", 97, 90},
	{"MAU Plant Room North", "FAB1 L4 Mechanical Deck", "room", 97, 90},
	{"MAU Plant Room South", "FAB1 L4 Mechanical Deck", "room", 97, 90},
	{"CUB Central Utility Building", "", "cub", 98, 90},
	{"CUB L1 Utility Floor", "CUB Central Utility Building", "floor", 98, 90},
	{"Chiller Plant Room", "CUB L1 Utility Floor", "room", 98, 90},
	{"PCW Pump Room", "CUB L1 Utility Floor", "room", 98, 90},
	{"CDA Compressor Room", "CUB L1 Utility Floor", "room", 98, 90},
	{"CUB Roof", "CUB Central Utility Building", "floor", 98, 90},
	{"Cooling Tower Yard", "CUB Roof", "room", 98, 90},
}

// buildingOf returns the root (building) of a location.
func buildingOf(name string) string {
	for {
		var parent string
		for _, l := range locations {
			if l.Name == name {
				parent = l.Parent
			}
		}
		if parent == "" {
			return name
		}
		name = parent
	}
}

// ---------------------------------------------------------------------------
// Data sources and equipment

type GatewaySpec struct{ Alias, Name, Description string }

var gateways = []GatewaySpec{
	{"cub_bms", "CUB Building Management System", "MQTT bridge of the central utility building BMS (chillers, towers, PCW pumps, CDA)"},
	{"fab1_hvac", "FAB1 HVAC Controller", "MQTT bridge of the FAB1 cleanroom HVAC controllers (make-up air units)"},
	{"fab1_subfab", "FAB1 Sub-Fab Monitoring", "Sub-fab equipment monitoring gateway (dry vacuum pumps)"},
	{"fab1_tools", "FAB1 Tool Automation Host", "Equipment automation host of the FAB1 L2 process tools (SECS/GEM to MQTT)"},
}

// EquipSpec is one piece of equipment.
type EquipSpec struct {
	Alias, Name, Model, Location, Gateway, Installed, Group string
	Critical, Supporting                                    [][2]string
}

func equipment() []*EquipSpec {
	var out []*EquipSpec
	add := func(e *EquipSpec) { out = append(out, e) }
	for i := 1; i <= 3; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("CH_%02d", i), Name: fmt.Sprintf("Chiller CH-%02d", i), Model: "Daikin WSC-126 Centrifugal Chiller",
			Location: "Chiller Plant Room", Gateway: "cub_bms", Installed: fmt.Sprintf("2019-0%d-15", 2+i), Group: "chillers",
			Critical:   [][2]string{{"Chilled water supply", "Supplies 6.5 °C chilled water to all FAB1 MAU cooling coils and the PCW heat exchangers"}},
			Supporting: [][2]string{{"Redundancy", "N+1 plant - two duty chillers and one standby, rotated weekly"}}})
	}
	for i := 1; i <= 3; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("CT_%02d", i), Name: fmt.Sprintf("Cooling Tower CT-%02d", i), Model: "BAC S3E-1424 Cooling Tower",
			Location: "Cooling Tower Yard", Gateway: "cub_bms", Installed: fmt.Sprintf("2019-0%d-20", 2+i), Group: "towers",
			Critical:   [][2]string{{"Heat rejection", "Rejects condenser heat of the paired chiller"}},
			Supporting: [][2]string{{"Water treatment", "Chemical dosing and conductivity-controlled blowdown"}}})
	}
	for i := 1; i <= 3; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("CHWP_%02d", i), Name: fmt.Sprintf("CHW Pump CHWP-%02d", i), Model: "Grundfos NKG 200-150-315 CHW Pump",
			Location: "Chiller Plant Room", Gateway: "cub_bms", Installed: fmt.Sprintf("2019-0%d-16", 2+i), Group: "chwp",
			Critical:   [][2]string{{"Chilled water circulation", fmt.Sprintf("Primary chilled water pump of chiller CH-%02d (runs with its chiller)", i)}},
			Supporting: [][2]string{{"Flow proving", "Chiller evaporator flow switch interlock"}}})
	}
	for i := 1; i <= 3; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("CWP_%02d", i), Name: fmt.Sprintf("CW Pump CWP-%02d", i), Model: "Grundfos NKG 250-200-315 CW Pump",
			Location: "Chiller Plant Room", Gateway: "cub_bms", Installed: fmt.Sprintf("2019-0%d-17", 2+i), Group: "cwp",
			Critical:   [][2]string{{"Condenser water circulation", fmt.Sprintf("Condenser water pump of chiller CH-%02d and its cooling tower", i)}},
			Supporting: [][2]string{{"Strainer", "Suction strainer differential pressure monitored for cavitation"}}})
	}
	add(&EquipSpec{Alias: "CHWS_01", Name: "CHW Plant CHWS-01", Model: "CUB Chilled Water Plant Controller",
		Location: "Chiller Plant Room", Gateway: "cub_bms", Installed: "2019-05-20", Group: "system",
		Critical:   [][2]string{{"Chilled water distribution", "Supplies chilled water to the FAB1 MAU cooling coils through the supply / return risers"}},
		Supporting: [][2]string{{"Variable primary control", "CHW pump speed follows the riser differential pressure; the bypass valve holds minimum chiller flow"}}})
	for i := 1; i <= 4; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("PCWP_%02d", i), Name: fmt.Sprintf("PCW Pump PCWP-%02d", i), Model: "Grundfos NBG 150-125-315 PCW Pump",
			Location: "PCW Pump Room", Gateway: "cub_bms", Installed: fmt.Sprintf("2020-0%d-10", i), Group: "pcw",
			Critical:   [][2]string{{"Process cooling water", "Circulates PCW to process tools in FAB1 (3 duty + 1 standby)"}},
			Supporting: [][2]string{{"VSD control", "Speed controlled on PCW header differential pressure"}}})
	}
	for i := 1; i <= 2; i++ {
		add(&EquipSpec{Alias: fmt.Sprintf("CDA_%02d", i), Name: fmt.Sprintf("CDA Compressor CDA-%02d", i), Model: "Atlas Copco ZR 315 VSD+",
			Location: "CDA Compressor Room", Gateway: "cub_bms", Installed: fmt.Sprintf("2021-0%d-05", 3+i), Group: "cda",
			Critical:   [][2]string{{"Clean dry air", "Supplies 7.5 bar(g), -40 °C PDP clean dry air to process tools"}},
			Supporting: [][2]string{{"Load sharing", "CDA-01 base load, CDA-02 trim"}}})
	}
	for i := 1; i <= 6; i++ {
		room := "MAU Plant Room North"
		if i > 3 {
			room = "MAU Plant Room South"
		}
		add(&EquipSpec{Alias: fmt.Sprintf("MAU_%02d", i), Name: fmt.Sprintf("Make-up Air Unit MAU-%02d", i), Model: "Daikin Vision MAU-60K",
			Location: room, Gateway: "fab1_hvac", Installed: fmt.Sprintf("2019-1%d-01", i%3), Group: "mau",
			Critical:   [][2]string{{"Cleanroom pressurisation", "Fresh air make-up for FAB1 cleanroom (ISO class 5)"}},
			Supporting: [][2]string{{"Humidity control", "Dehumidifies outdoor air to 45 % RH supply"}}})
	}
	// FAB1 sub-fab support units and L2 process tools (after the facility
	// equipment, so the facility units keep their random streams)
	out = append(out, subFabEquipment()...)
	out = append(out, toolEquipment()...)
	return out
}

// ---------------------------------------------------------------------------
// People

type GroupSpec struct {
	Alias, Name, Description string
	Buildings                []string
	Perms                    map[string][]string
}

type UserSpec struct {
	Username, Name, Email, EmployeeID, Group string
	Role                                     string // admin | supervisor | technician | operator | engineer
	Buildings                                []string
	Types                                    []string
}

func readAll(groups ...string) map[string][]string {
	m := map[string][]string{}
	for _, g := range groups {
		m[g] = append(m[g], "read")
	}
	return m
}

var userGroups = func() []GroupSpec {
	viewer := []string{"locationoverview", "equipmentoverview", "alarmsreport", "maintenanceorder", "maintenanceperformance",
		"maintenanceinstruction", "personnelmanagement", "earlywarningdiagnostics", "earlywarningdashboard"}
	mgmt := readAll(viewer...)
	for g, extra := range map[string][]string{
		"maintenanceorder":        {"create", "update", "delete", "approve", "report"},
		"maintenanceinstruction":  {"create", "update", "delete"},
		"personnelmanagement":     {"update"},
		"earlywarningdiagnostics": {"update"},
		"equipmentoverview":       {"update"},
		"alarmsreport":            {"update"},
		"locationoverview":        {"update"},
		"notificationmanager":     {"read", "update"},
		"digitaltwin":             {"read", "update"},
		// AI assistant, AI insights (apply / dismiss recommendations, rules,
		// documents) and digital twin simulations (run and approve)
		"aiassistant": {"read"},
		"aiinsights":  {"read", "update"},
		"simulation":  {"read", "create"},
		"production":  {"read"},
	} {
		mgmt[g] = append(mgmt[g], extra...)
	}
	tech := readAll(viewer...)
	tech["maintenanceorder"] = append(tech["maintenanceorder"], "create", "update", "report")
	tech["earlywarningdiagnostics"] = append(tech["earlywarningdiagnostics"], "update")
	tech["digitaltwin"] = []string{"read"}
	tech["aiassistant"] = []string{"read"}
	tech["aiinsights"] = []string{"read"}
	tech["simulation"] = []string{"read"}
	tech["production"] = []string{"read"}
	ops := readAll("locationoverview", "equipmentoverview", "alarmsreport", "maintenanceorder", "earlywarningdiagnostics", "earlywarningdashboard", "maintenanceinstruction")
	ops["maintenanceorder"] = append(ops["maintenanceorder"], "create")
	// control room operators run the plant: they may write set points and
	// modes, and run (and approve) digital twin simulations
	ops["digitaltwin"] = []string{"read", "update"}
	ops["aiassistant"] = []string{"read"}
	ops["aiinsights"] = []string{"read"}
	ops["simulation"] = []string{"read", "create"}
	ops["production"] = []string{"read"}
	all := []string{"FAB1 Wafer Fab", "CUB Central Utility Building"}
	return []GroupSpec{
		{"facility_management", "Facility Management", "Facility managers, supervisors and reliability engineers", all, mgmt},
		{"cub_maintenance", "CUB Maintenance Team", "Technicians of the central utility building", []string{"CUB Central Utility Building"}, tech},
		{"fab_facilities", "FAB1 Facilities Team", "Technicians for cleanroom HVAC, sub-fab vacuum and process equipment", []string{"FAB1 Wafer Fab"}, tech},
		{"control_room", "Facility Control Room", "24/7 facility monitoring operators", all, ops},
	}
}()

var cubTypes = []string{"Centrifugal Chiller", "Cooling Tower", "Process Cooling Water Pump", "Chilled Water Pump", "Condenser Water Pump", "Air Compressor", "Chilled Water System"}
var fabTypes = append([]string{"Make-up Air Unit", "Dry Vacuum Pump"}, fabEquipmentTypes...)

var users = []UserSpec{
	{"sarah_lim", "Sarah Lim", "sarah.lim@sgfab.example", "FM-0001", "facility_management", "admin", nil, nil},
	{"kevin_ong", "Kevin Ong", "kevin.ong@sgfab.example", "FM-0007", "facility_management", "engineer", nil, nil},
	{"rajesh_kumar", "Rajesh Kumar", "rajesh.kumar@sgfab.example", "FM-0012", "facility_management", "supervisor", []string{"CUB Central Utility Building"}, cubTypes},
	{"nurul_aisyah", "Nurul Aisyah", "nurul.aisyah@sgfab.example", "FM-0015", "facility_management", "supervisor", []string{"FAB1 Wafer Fab"}, fabTypes},
	{"ahmad_faizal", "Ahmad Faizal", "ahmad.faizal@sgfab.example", "MT-0103", "cub_maintenance", "technician", []string{"CUB Central Utility Building"}, []string{"Centrifugal Chiller", "Cooling Tower", "Chilled Water System"}},
	{"lim_kokwai", "Lim Kok Wai", "lim.kokwai@sgfab.example", "MT-0108", "cub_maintenance", "technician", []string{"CUB Central Utility Building"}, []string{"Centrifugal Chiller", "Air Compressor"}},
	{"kumar_selvam", "Kumar Selvam", "kumar.selvam@sgfab.example", "MT-0114", "cub_maintenance", "technician", []string{"CUB Central Utility Building"}, []string{"Process Cooling Water Pump", "Condenser Water Pump", "Cooling Tower"}},
	{"chen_jiahui", "Chen Jia Hui", "chen.jiahui@sgfab.example", "MT-0121", "cub_maintenance", "technician", []string{"CUB Central Utility Building"}, []string{"Air Compressor", "Process Cooling Water Pump", "Chilled Water Pump"}},
	{"hafiz_rahman", "Muhammad Hafiz", "hafiz.rahman@sgfab.example", "MT-0204", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Make-up Air Unit"}},
	{"tan_meiling", "Tan Mei Ling", "tan.meiling@sgfab.example", "MT-0209", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Make-up Air Unit", "Dry Vacuum Pump"}},
	{"daniel_wong", "Daniel Wong", "daniel.wong@sgfab.example", "MT-0215", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Dry Vacuum Pump", "Exhaust Abatement System", "Process Chiller"}},
	{"priya_nair", "Priya Nair", "priya.nair@sgfab.example", "MT-0222", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Dry Vacuum Pump", "Make-up Air Unit", "Exhaust Abatement System"}},
	{"aaron_lee", "Aaron Lee", "aaron.lee@sgfab.example", "MT-0230", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Plasma Etch Tool", "CVD Tool", "PVD Tool"}},
	{"nadia_hassan", "Nadia Hassan", "nadia.hassan@sgfab.example", "MT-0236", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Metrology Tool", "Inspection Tool", "Parametric Test System", "Wet Clean Station"}},
	{"wei_jie", "Wei Jie Tan", "weijie.tan@sgfab.example", "MT-0241", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Lithography Scanner", "Litho Track"}},
	{"rahim_osman", "Rahim Osman", "rahim.osman@sgfab.example", "MT-0247", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"Ion Implanter", "Diffusion Furnace", "RTP Tool", "Asher"}},
	{"grace_tan", "Grace Tan", "grace.tan@sgfab.example", "MT-0252", "fab_facilities", "technician", []string{"FAB1 Wafer Fab"}, []string{"CMP Tool", "Electroplating Tool", "Wet Clean Station"}},
	{"jason_teo", "Jason Teo", "jason.teo@sgfab.example", "CR-0301", "control_room", "operator", nil, nil},
	{"siti_rahmah", "Siti Rahmah", "siti.rahmah@sgfab.example", "CR-0305", "control_room", "operator", nil, nil},
}

func userByName(username string) *UserSpec {
	for i := range users {
		if users[i].Username == username {
			return &users[i]
		}
	}
	return nil
}

// techniciansFor returns the technicians responsible for a building and type.
func techniciansFor(building, typ string) []string {
	var out []string
	for _, u := range users {
		if u.Role != "technician" {
			continue
		}
		okB, okT := false, false
		for _, b := range u.Buildings {
			okB = okB || b == building
		}
		for _, t := range u.Types {
			okT = okT || t == typ
		}
		if okB && okT {
			out = append(out, u.Username)
		}
	}
	return out
}

func supervisorFor(building string) string {
	for _, u := range users {
		if u.Role == "supervisor" && len(u.Buildings) > 0 && u.Buildings[0] == building {
			return u.Username
		}
	}
	return "rajesh_kumar"
}

func operators() []string { return []string{"jason_teo", "siti_rahmah"} }
