package main

import (
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// FAB1 products and process flow (docs/production/DESIGN.md section 1): one
// 40 nm low-power CMOS platform (single poly, Cu / low-k dual damascene
// interconnect, Al pads) with two products modelled on real 40 nm chips. The
// flow is a condensed but genuine sequence of the platform's modules (about
// 215 / 255 steps against 600 - 1,000 in a commercial flow): wafer start, STI,
// wells and threshold implants, dual gate oxide, poly gate, LDD, spacer,
// source / drain, NiPt silicide, W contacts, Cu metal levels, passivation, Al
// pads, alloy, final inspection and the wafer acceptance test (WAT).
// ---------------------------------------------------------------------------

// Step types: the qualification of a route step (the tools that can run it).
const (
	stLithoArF = "LITHO_ARF" // ArF immersion scanner (critical layers)
	stLithoKrF = "LITHO_KRF" // KrF scanner (semi-critical layers, thick metals)
	stLithoI   = "LITHO_I"   // i-line stepper (wells, pad layers)
	stEtchC    = "ETCH_C"    // conductor etch (Si, poly, spacer, Al, MIM)
	stEtchD    = "ETCH_D"    // dielectric etch (oxide, low-k, nitride)
	stCVD      = "CVD"       // PECVD / HDP / W CVD
	stPVD      = "PVD"       // barrier, seed, NiPt, Al
	stECP      = "ECP"       // Cu electroplating
	stCMPD     = "CMP_D"     // oxide and W CMP
	stCMPCu    = "CMP_CU"    // Cu CMP
	stFurnOx   = "FURN_OX"   // oxidation / anneal furnace (batch)
	stFurnLP   = "FURN_LP"   // LPCVD furnace (batch)
	stRTP      = "RTP"       // rapid thermal processing
	stImpMC    = "IMP_MC"    // medium current implant (wells, Vt)
	stImpLDD   = "IMP_LDD"   // LDD / halo (either implanter)
	stImpHC    = "IMP_HC"    // high current implant (source / drain)
	stWet      = "WET"       // batch wet clean (2 lots)
	stSWC      = "SWC"       // single-wafer clean
	stAsh      = "ASH"       // plasma resist strip
	stMet      = "MET"       // film thickness, OCD, sheet resistance
	stCDSEM    = "CDSEM"     // resist CD
	stOVL      = "OVL"       // overlay
	stDefect   = "DEFECT"    // darkfield defect inspection
	stWAT      = "WAT"       // wafer acceptance (parametric) test
)

// stepTimes are the ideal times of a step type: per wafer (per chamber for
// cluster tools, per measured wafer for metrology) and per lot (overheads;
// for batch tools the process time of a batch).
var stepTimes = map[string][2]float64{
	stLithoArF: {25, 540}, stLithoKrF: {18, 420}, stLithoI: {15, 360},
	stEtchC: {150, 300}, stEtchD: {150, 300}, stCVD: {100, 300}, stPVD: {90, 240}, stECP: {240, 300},
	stCMPD: {100, 360}, stCMPCu: {110, 360}, stRTP: {70, 240},
	stImpMC: {18, 420}, stImpLDD: {18, 420}, stImpHC: {30, 480},
	stWet: {0, 1800}, stSWC: {60, 180}, stAsh: {30, 180},
	stMet: {60, 180}, stCDSEM: {120, 240}, stOVL: {75, 180}, stDefect: {420, 240}, stWAT: {360, 900},
	stFurnOx: {0, 3 * 3600}, stFurnLP: {0, 5 * 3600},
}

// Dispositions of a measurement out of spec.
const (
	failLitho    = "REWORK_LITHO" // strip the resist, clean and expose again (twice at most)
	failRedep    = "REWORK_REDEP" // hold, strip the film and deposit again (once)
	failRepolish = "REWORK_CMP"   // above spec: polish again (once); below spec: scrap
	failScrap    = "SCRAP"        // wafers out of spec are scrapped
	failReclean  = "RECLEAN"      // clean again (once), then scrap the two worst wafers
	failHold     = "HOLD"         // engineering disposition (hold, then use as is)
)

// QSpec is the specification of a quality characteristic (a key such as
// POLY.CDE: gate CD after etch) and the process variation that makes it.
type QSpec struct {
	Key    string
	Name   string
	Param  string // thickness | cd | overlay | rs
	Unit   string
	Target float64
	LSL    float64 // NaN = none
	USL    float64
	Sigma  float64 // wafer-to-wafer variation (absolute)
	Rel    bool    // tool effects are relative (thickness, Rs) rather than absolute (CD, overlay)
}

func (q *QSpec) pass(v float64) bool {
	return (math.IsNaN(q.LSL) || v >= q.LSL) && (math.IsNaN(q.USL) || v <= q.USL)
}

// half is half the tolerance (for the normalised deviation).
func (q *QSpec) half() float64 {
	switch {
	case !math.IsNaN(q.LSL) && !math.IsNaN(q.USL):
		return (q.USL - q.LSL) / 2
	case !math.IsNaN(q.USL):
		return math.Abs(q.USL - q.Target)
	default:
		return math.Abs(q.Target - q.LSL)
	}
}

// RouteStep is one step of a product route (prod.route_steps).
type RouteStep struct {
	Seq    int
	Code   string // <layer>-<operation>, e.g. POLY-ET, M3-CMP
	Layer  string // mask layer / module of the step (STI, POLY, M3 ...)
	Module string // FEOL | MOL | BEOL | TEST
	Op     string
	Type   string
	Recipe string
	WaferS float64
	LotS   float64
	Mask   string  // the mask exposed (litho steps)
	Makes  []string // quality keys this step produces on every wafer
	Meas   string   // quality key measured (metrology steps)
	Param  string   // metrology parameter (thickness | cd | overlay | rs | defects | wat)
	Sample int      // wafers measured (0 = all)
	Rate   float64  // share of lots sampled
	OnFail string   // disposition of a failed measurement
	Back   int      // step to return to on rework (seq)
	QTime  float64  // queue-time limit after the previous step (hours; 0 none)
}

func (s *RouteStep) litho() bool { return strings.HasPrefix(s.Type, "LITHO") }

// stepArea is the process area of a step type.
func stepArea(typ string) string {
	switch typ {
	case stLithoArF, stLithoKrF, stLithoI:
		return areaLitho
	case stEtchC, stEtchD:
		return areaEtch
	case stCVD:
		return areaCVD
	case stPVD:
		return areaPVD
	case stECP:
		return areaECP
	case stCMPD, stCMPCu:
		return areaCMP
	case stFurnOx, stFurnLP, stRTP:
		return areaDiff
	case stImpMC, stImpLDD, stImpHC:
		return areaImp
	case stWet, stSWC:
		return areaWet
	case stAsh:
		return areaStrip
	case stWAT:
		return areaTest
	}
	return areaMet
}

// Product is a product and its route (prod.products).
type Product struct {
	Code, Name, Tech string
	Masks            int
	Metals           int
	Mix              float64
	CycleDays        float64 // planned cycle time (due date)
	DieMM2           float64
	Route            []RouteStep
	Specs            map[string]*QSpec
}

// spec returns the quality specification of a key.
func (p *Product) spec(key string) *QSpec { return p.Specs[key] }

type routeBuilder struct {
	p *Product
}

func (b *routeBuilder) add(s RouteStep) {
	if s.WaferS == 0 && s.LotS == 0 {
		t := stepTimes[s.Type]
		s.WaferS, s.LotS = t[0], t[1]
	}
	if s.Rate == 0 {
		s.Rate = 1
	}
	if s.Module == "" {
		s.Module = "FEOL"
	}
	s.Seq = len(b.p.Route) + 1
	b.p.Route = append(b.p.Route, s)
}

func (b *routeBuilder) q(key, name, param, unit string, target, lsl, usl, sigma float64, rel bool) {
	b.p.Specs[key] = &QSpec{Key: key, Name: name, Param: param, Unit: unit, Target: target, LSL: lsl, USL: usl, Sigma: sigma, Rel: rel}
}

// seqOf returns the seq of a step code added so far.
func (b *routeBuilder) seqOf(code string) int {
	for i := len(b.p.Route) - 1; i >= 0; i-- {
		if b.p.Route[i].Code == code {
			return b.p.Route[i].Seq
		}
	}
	panic("route step " + code + " not found")
}

// photo adds a litho step and its CD (critical) and overlay checks.
func (b *routeBuilder) photo(layer, module, typ, recipe string, cd float64, ovl float64, first bool) {
	b.p.Masks++
	cdr, ov := layer+".CDR", layer+".OVL"
	makes := []string{ov}
	critical := typ == stLithoArF
	if critical {
		makes = append(makes, cdr)
		b.q(cdr, layer+" resist CD", "cd", "nm", cd, cd*0.92, cd*1.08, cd*0.018, false)
	}
	b.q(ov, layer+" overlay", "overlay", "nm", 0, -ovl, ovl, ovl*0.25, false)
	b.add(RouteStep{Code: layer + "-PH", Layer: layer, Module: module, Op: layer + " photo (coat, expose, develop)", Type: typ,
		Recipe: recipe, Mask: layer, Makes: makes})
	ph := b.seqOf(layer + "-PH")
	if critical {
		b.add(RouteStep{Code: layer + "-CD", Layer: layer, Module: module, Op: layer + " resist CD", Type: stCDSEM, Recipe: "CD-" + layer,
			Meas: cdr, Param: "cd", Sample: 3, OnFail: failLitho, Back: ph})
	}
	if !first {
		n := 3
		if typ == stLithoI {
			n = 2
		}
		b.add(RouteStep{Code: layer + "-OVL", Layer: layer, Module: module, Op: layer + " overlay", Type: stOVL, Recipe: "OVL-" + layer,
			Meas: ov, Param: "overlay", Sample: n, OnFail: failLitho, Back: ph})
	}
}

// implant adds an implant layer: photo, overlay, implant, strip, clean.
func (b *routeBuilder) implant(layer, op, litho, imp, recipe string) {
	ovl := map[string]float64{stLithoI: 45, stLithoKrF: 20, stLithoArF: 10}[litho]
	b.photo(layer, "FEOL", litho, "IMP-"+layer, 0, ovl, false)
	b.add(RouteStep{Code: layer + "-IMP", Layer: layer, Op: op, Type: imp, Recipe: recipe, Makes: []string{layer + ".DOSE"}})
	b.add(RouteStep{Code: layer + "-STR", Layer: layer, Op: layer + " resist strip", Type: stAsh, Recipe: "ASH-IMP"})
	b.add(RouteStep{Code: layer + "-CLN", Layer: layer, Op: layer + " post-strip clean", Type: stWet, Recipe: "SPM-SC1"})
}

// metal adds a Cu metal level: single damascene (M1) or a via + trench dual
// damascene level, thin (ArF) or thick (KrF).
func (b *routeBuilder) metal(n int, thick bool) {
	m := fmt.Sprintf("M%d", n)
	litho, cd, ovl, rs, tol := stLithoArF, 64.0, 12.0, 0.11, 0.015
	if thick {
		litho, cd, ovl, rs, tol = stLithoKrF, 0, 25, 0.022, 0.0035
	}
	if n == 1 {
		rs, tol = 0.16, 0.02
	}
	dep := "LK-DD-3500A"
	if n == 1 {
		dep = "LK-M1-1500A"
	}
	if thick {
		dep = "USG-THICK-9000A"
	}
	imd := map[bool]float64{false: 350, true: 900}[thick]
	if n == 1 {
		imd = 150
	}
	b.q(m+".THK", m+" inter-metal dielectric thickness", "thickness", "nm", imd, imd*0.95, imd*1.05, imd*0.009, true)
	b.add(RouteStep{Code: m + "-DEP", Layer: m, Module: "BEOL", Op: m + " inter-metal dielectric (low-k + cap)", Type: stCVD, Recipe: dep,
		Makes: []string{m + ".THK"}})
	b.add(RouteStep{Code: m + "-THK", Layer: m, Module: "BEOL", Op: m + " dielectric thickness", Type: stMet, Recipe: "THK-IMD", Meas: m + ".THK",
		Param: "thickness", Sample: 3, Rate: 0.5, OnFail: failHold})
	if n > 1 {
		v := fmt.Sprintf("V%d", n-1)
		b.photo(v, "BEOL", litho, v+"-VIA", cd*0.85, ovl, false)
		b.add(RouteStep{Code: v + "-ET", Layer: v, Module: "BEOL", Op: v + " via etch (in-situ strip)", Type: stEtchD, Recipe: "VIA-ETCH"})
	}
	b.photo(m, "BEOL", litho, m+"-TRENCH", cd, ovl, false)
	b.add(RouteStep{Code: m + "-ET", Layer: m, Module: "BEOL", Op: m + " trench etch (in-situ strip)", Type: stEtchD, Recipe: "TRENCH-ETCH"})
	b.add(RouteStep{Code: m + "-CLN", Layer: m, Module: "BEOL", Op: m + " post-etch polymer clean", Type: stSWC, Recipe: "PET-CU"})
	b.add(RouteStep{Code: m + "-BS", Layer: m, Module: "BEOL", Op: m + " TaN/Ta barrier and Cu seed", Type: stPVD, Recipe: "TAN-TA-CUSEED", Makes: []string{m + ".RSADJ"},
		QTime: 4})
	b.add(RouteStep{Code: m + "-ECP", Layer: m, Module: "BEOL", Op: m + " Cu electroplating", Type: stECP, Recipe: "CU-ECP", Makes: []string{m + ".RSADJ"}})
	b.q(m+".RS", m+" Cu sheet resistance", "rs", "Ω/sq", rs, rs-tol, rs+tol, rs*0.025, true)
	b.add(RouteStep{Code: m + "-CMP", Layer: m, Module: "BEOL", Op: m + " Cu CMP", Type: stCMPCu, Recipe: "CU-CMP", Makes: []string{m + ".RS"}})
	b.add(RouteStep{Code: m + "-RS", Layer: m, Module: "BEOL", Op: m + " sheet resistance", Type: stMet, Recipe: "RS-4PP", Meas: m + ".RS",
		Param: "rs", Sample: 3, OnFail: failScrap})
	if n > 1 {
		b.add(RouteStep{Code: m + "-DEF", Layer: m, Module: "BEOL", Op: m + " defect inspection", Type: stDefect, Recipe: "DF-BEOL",
			Param: "defects", Sample: 1, Rate: 0.3, OnFail: failReclean, Back: b.seqOf(m + "-CLN")})
	}
}

// route40 builds the 40 nm route of a product: thin and thick Cu metal levels,
// optional deep n-well and MIM capacitor (RF products).
func route40(p *Product, thin, thick int, rf bool) {
	b := &routeBuilder{p: p}
	p.Specs = map[string]*QSpec{}
	nan := math.NaN()
	// --- wafer start and STI hard mask
	b.add(RouteStep{Code: "WS-CLN", Layer: "START", Op: "Lot start: laser mark and pre-oxidation clean", Type: stWet, Recipe: "RCA-SC1SC2"})
	b.q("PADOX.THK", "Pad oxide thickness", "thickness", "Å", 110, 100, 120, 1.2, true)
	b.add(RouteStep{Code: "PAD-OX", Layer: "STI", Op: "Pad oxidation (110 Å)", Type: stFurnOx, Recipe: "PADOX-110A", LotS: 3 * 3600, QTime: 8,
		Makes: []string{"PADOX.THK"}})
	b.add(RouteStep{Code: "PADOX-THK", Layer: "STI", Op: "Pad oxide thickness", Type: stMet, Recipe: "THK-PADOX", Meas: "PADOX.THK", Param: "thickness",
		Sample: 3, Rate: 0.5, OnFail: failHold})
	b.q("SIN.THK", "STI hard mask nitride thickness", "thickness", "nm", 110, 104, 116, 1.1, true)
	b.add(RouteStep{Code: "SIN-DEP", Layer: "STI", Op: "LPCVD nitride hard mask (1100 Å)", Type: stFurnLP, Recipe: "LPSIN-1100A", LotS: 5 * 3600,
		Makes: []string{"SIN.THK"}})
	b.add(RouteStep{Code: "SIN-THK", Layer: "STI", Op: "Nitride thickness", Type: stMet, Recipe: "THK-SIN", Meas: "SIN.THK", Param: "thickness",
		Sample: 3, OnFail: failRedep, Back: b.seqOf("SIN-DEP")})
	// --- shallow trench isolation (first critical mask)
	b.photo("STI", "FEOL", stLithoArF, "STI-AR", 60, 10, true)
	b.q("STI.CDE", "Active area CD after etch", "cd", "nm", 52, 48, 56, 0.7, false)
	b.add(RouteStep{Code: "STI-ET", Layer: "STI", Op: "STI trench etch (2800 Å, in-situ strip)", Type: stEtchC, Recipe: "STI-SI-2800A",
		Makes: []string{"STI.CDE"}})
	b.add(RouteStep{Code: "STI-CLN", Layer: "STI", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-SI"})
	b.add(RouteStep{Code: "STI-ECD", Layer: "STI", Op: "Active area CD (OCD)", Type: stMet, Recipe: "OCD-STI", Meas: "STI.CDE", Param: "cd",
		Sample: 3, OnFail: failScrap})
	b.add(RouteStep{Code: "STI-PLC", Layer: "STI", Op: "Pre-liner clean (SC1 / SC2 / HF)", Type: stWet, Recipe: "RCA-SC1SC2"})
	b.add(RouteStep{Code: "STI-LNR", Layer: "STI", Op: "Trench liner oxidation", Type: stFurnOx, Recipe: "LINER-OX", LotS: 2.5 * 3600, QTime: 6})
	b.q("FILL.THK", "STI fill oxide thickness", "thickness", "nm", 500, 475, 525, 5, true)
	b.add(RouteStep{Code: "STI-FILL", Layer: "STI", Op: "HDP oxide trench fill (5000 Å)", Type: stCVD, Recipe: "HDP-OX-5000A", WaferS: 180, LotS: 300,
		Makes: []string{"FILL.THK"}})
	b.add(RouteStep{Code: "FILL-THK", Layer: "STI", Op: "Fill oxide thickness", Type: stMet, Recipe: "THK-FILL", Meas: "FILL.THK", Param: "thickness",
		Sample: 3, Rate: 0.5, OnFail: failHold})
	b.add(RouteStep{Code: "STI-ANL", Layer: "STI", Op: "Fill densification anneal", Type: stFurnOx, Recipe: "DENSIFY-1000C", LotS: 3 * 3600})
	b.q("STI.CMP", "Remaining nitride after STI CMP", "thickness", "nm", 75, 65, 85, 2.5, false)
	b.add(RouteStep{Code: "STI-CMP", Layer: "STI", Op: "STI oxide CMP (stop on nitride)", Type: stCMPD, Recipe: "STI-CMP", Makes: []string{"STI.CMP"}})
	b.add(RouteStep{Code: "STI-PCC", Layer: "STI", Op: "Post-CMP brush scrub", Type: stSWC, Recipe: "SCRUB-CMP"})
	b.add(RouteStep{Code: "STI-CMT", Layer: "STI", Op: "Remaining nitride thickness", Type: stMet, Recipe: "THK-STI", Meas: "STI.CMP",
		Param: "thickness", Sample: 3, OnFail: failRepolish, Back: b.seqOf("STI-CMP")})
	b.add(RouteStep{Code: "SIN-STR", Layer: "STI", Op: "Hot phosphoric nitride strip", Type: stWet, Recipe: "HOT-PHOS"})
	b.add(RouteStep{Code: "STI-DEF", Layer: "STI", Op: "STI defect inspection", Type: stDefect, Recipe: "DF-FEOL", Param: "defects", Sample: 1,
		Rate: 0.5, OnFail: failReclean, Back: b.seqOf("SIN-STR")})
	// --- wells (i-line) and threshold adjust implants (KrF)
	if rf {
		b.implant("DNW", "Deep n-well implant (RF isolation)", stLithoI, stImpMC, "P-1500KEV")
	}
	b.implant("NW", "N-well implant (P)", stLithoI, stImpMC, "P-400KEV-2E13")
	b.implant("PW", "P-well implant (B)", stLithoI, stImpMC, "B-180KEV-2E13")
	b.add(RouteStep{Code: "WELL-ANL", Layer: "WELL", Op: "Well drive-in anneal", Type: stFurnOx, Recipe: "WELL-DRIVE", LotS: 2 * 3600})
	for _, vt := range [][2]string{{"VTN", "NMOS Vt adjust (B)"}, {"VTP", "PMOS Vt adjust (As)"}, {"HVT", "High-Vt implant"}, {"LVT", "Low-Vt implant"}} {
		b.implant(vt[0], vt[1], stLithoKrF, stImpMC, "VT-"+vt[0])
	}
	// --- dual gate oxide and poly
	b.add(RouteStep{Code: "GOX-PRE", Layer: "GATE", Op: "Pre-gate clean (HF, SC1)", Type: stWet, Recipe: "PRE-GOX"})
	b.q("GOX1.THK", "IO gate oxide thickness", "thickness", "Å", 65, 62, 68, 0.6, true)
	b.add(RouteStep{Code: "GOX1", Layer: "GATE", Op: "IO gate oxidation (65 Å)", Type: stFurnOx, Recipe: "IO-GOX-65A", LotS: 3.5 * 3600,
		Makes: []string{"GOX1.THK"}, QTime: 4})
	b.add(RouteStep{Code: "GOX1-THK", Layer: "GATE", Op: "IO gate oxide thickness", Type: stMet, Recipe: "THK-GOX1", Meas: "GOX1.THK",
		Param: "thickness", Sample: 3, OnFail: failScrap})
	b.photo("DGO", "FEOL", stLithoKrF, "DGO", 0, 20, false)
	b.add(RouteStep{Code: "DGO-ET", Layer: "DGO", Op: "Core oxide removal (BOE)", Type: stWet, Recipe: "BOE-OX-STRIP"})
	b.add(RouteStep{Code: "DGO-STR", Layer: "DGO", Op: "Resist strip", Type: stAsh, Recipe: "ASH-STD"})
	b.add(RouteStep{Code: "DGO-CLN", Layer: "DGO", Op: "Post-strip clean", Type: stWet, Recipe: "SPM-SC1"})
	b.q("GOX2.THK", "Core gate oxide thickness", "thickness", "Å", 20, 18.8, 21.2, 0.25, true)
	b.add(RouteStep{Code: "GOX2", Layer: "GATE", Op: "Core gate oxidation (ISSG, 20 Å)", Type: stRTP, Recipe: "ISSG-20A", Makes: []string{"GOX2.THK"}})
	b.add(RouteStep{Code: "GOX2-THK", Layer: "GATE", Op: "Core gate oxide thickness", Type: stMet, Recipe: "THK-GOX2", Meas: "GOX2.THK",
		Param: "thickness", Sample: 3, OnFail: failScrap})
	b.q("POLY.THK", "Poly gate thickness", "thickness", "nm", 80, 76, 84, 0.7, true)
	b.add(RouteStep{Code: "POLY-DEP", Layer: "GATE", Op: "LPCVD poly-Si gate (800 Å)", Type: stFurnLP, Recipe: "LP-POLY-800A", LotS: 4.5 * 3600,
		Makes: []string{"POLY.THK"}})
	b.add(RouteStep{Code: "POLY-THK", Layer: "GATE", Op: "Poly thickness", Type: stMet, Recipe: "THK-POLY", Meas: "POLY.THK", Param: "thickness",
		Sample: 3, OnFail: failScrap})
	b.implant("HRP", "High-resistance poly resistor implant", stLithoI, stImpMC, "B-HRPOLY")
	b.photo("POLY", "FEOL", stLithoArF, "POLY-AR", 50, 10, false)
	b.q("POLY.CDE", "Gate CD after etch", "cd", "nm", 40, 37.5, 42.5, 0.55, false)
	b.add(RouteStep{Code: "POLY-ET", Layer: "POLY", Op: "Gate etch (in-situ strip)", Type: stEtchC, Recipe: "GATE-ETCH", Makes: []string{"POLY.CDE"}})
	b.add(RouteStep{Code: "POLY-CLN", Layer: "POLY", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-POLY"})
	b.add(RouteStep{Code: "GATE-CD", Layer: "POLY", Op: "Gate CD (OCD)", Type: stMet, Recipe: "OCD-GATE", Meas: "POLY.CDE", Param: "cd",
		Sample: 3, OnFail: failScrap})
	b.add(RouteStep{Code: "POLY-DEF", Layer: "POLY", Op: "Gate defect inspection", Type: stDefect, Recipe: "DF-GATE", Param: "defects", Sample: 1,
		Rate: 0.5, OnFail: failReclean})
	// --- LDD, spacer, source / drain
	b.implant("NLDD", "NMOS LDD and halo implant", stLithoKrF, stImpLDD, "AS-LDD-HALO")
	b.implant("PLDD", "PMOS LDD and halo implant", stLithoKrF, stImpLDD, "BF2-LDD-HALO")
	b.implant("IONLDD", "IO NMOS LDD implant (2.5 V devices)", stLithoI, stImpLDD, "P-IOLDD")
	b.implant("IOPLDD", "IO PMOS LDD implant (2.5 V devices)", stLithoI, stImpLDD, "B-IOLDD")
	b.q("SPC.THK", "Spacer nitride thickness", "thickness", "nm", 35, 33, 37, 0.35, true)
	b.add(RouteStep{Code: "SPC-DEP", Layer: "SPACER", Op: "Spacer nitride deposition (350 Å)", Type: stCVD, Recipe: "SIN-SPACER-350A",
		Makes: []string{"SPC.THK"}})
	b.add(RouteStep{Code: "SPC-THK", Layer: "SPACER", Op: "Spacer nitride thickness", Type: stMet, Recipe: "THK-SPC", Meas: "SPC.THK", Param: "thickness",
		Sample: 3, OnFail: failRedep, Back: b.seqOf("SPC-DEP")})
	b.add(RouteStep{Code: "SPC-ET", Layer: "SPACER", Op: "Spacer etch", Type: stEtchC, Recipe: "SPACER-ETCH"})
	b.add(RouteStep{Code: "SPC-CLN", Layer: "SPACER", Op: "Post-spacer clean", Type: stSWC, Recipe: "PET-SPC"})
	b.implant("NSD", "NMOS source / drain implant (As)", stLithoKrF, stImpHC, "AS-SD-3E15")
	b.implant("PSD", "PMOS source / drain implant (B)", stLithoKrF, stImpHC, "B-SD-3E15")
	b.implant("ESD", "ESD protection implant", stLithoI, stImpHC, "AS-ESD")
	b.q("SD.RS", "Source / drain sheet resistance", "rs", "Ω/sq", 110, 100, 120, 2.2, true)
	b.add(RouteStep{Code: "SD-ANL", Layer: "SD", Op: "Spike anneal (1050 °C)", Type: stRTP, Recipe: "SPIKE-1050C", Makes: []string{"SD.RS"}})
	b.add(RouteStep{Code: "SD-RS", Layer: "SD", Op: "S/D sheet resistance", Type: stMet, Recipe: "RS-SD", Meas: "SD.RS", Param: "rs",
		Sample: 3, OnFail: failScrap})
	// --- salicide block and NiPt silicide
	b.q("SAB.THK", "Salicide block oxide thickness", "thickness", "nm", 30, 27, 33, 0.4, true)
	b.add(RouteStep{Code: "SAB-DEP", Layer: "SAB", Op: "Salicide block oxide (300 Å)", Type: stCVD, Recipe: "SAB-OX-300A", Makes: []string{"SAB.THK"}})
	b.add(RouteStep{Code: "SAB-THK", Layer: "SAB", Op: "Salicide block thickness", Type: stMet, Recipe: "THK-SAB", Meas: "SAB.THK", Param: "thickness",
		Sample: 3, Rate: 0.5, OnFail: failHold})
	b.photo("SAB", "FEOL", stLithoKrF, "SAB", 0, 20, false)
	b.add(RouteStep{Code: "SAB-ET", Layer: "SAB", Op: "Salicide block etch (in-situ strip)", Type: stEtchD, Recipe: "SAB-ETCH"})
	b.add(RouteStep{Code: "SAB-CLN", Layer: "SAB", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-OX"})
	b.add(RouteStep{Code: "SAL-PRE", Layer: "SAL", Op: "Pre-silicide HF dip", Type: stWet, Recipe: "HF-DIP"})
	b.add(RouteStep{Code: "SAL-DEP", Layer: "SAL", Op: "NiPt deposition (100 Å)", Type: stPVD, Recipe: "NIPT-100A", Makes: []string{"SAL.RSADJ"}, QTime: 2})
	b.add(RouteStep{Code: "SAL-RTA1", Layer: "SAL", Op: "Silicide anneal 1 (280 °C)", Type: stRTP, Recipe: "NISI-RTA1"})
	b.add(RouteStep{Code: "SAL-STR", Layer: "SAL", Op: "Selective unreacted metal strip", Type: stWet, Recipe: "SPM-SEL"})
	b.q("SAL.RS", "NiSi sheet resistance", "rs", "Ω/sq", 12, 10.5, 13.5, 0.3, true)
	b.add(RouteStep{Code: "SAL-RTA2", Layer: "SAL", Op: "Silicide anneal 2 (450 °C)", Type: stRTP, Recipe: "NISI-RTA2", Makes: []string{"SAL.RS"}})
	b.add(RouteStep{Code: "SAL-RS", Layer: "SAL", Op: "Silicide sheet resistance", Type: stMet, Recipe: "RS-SAL", Meas: "SAL.RS", Param: "rs",
		Sample: 3, OnFail: failScrap})
	// --- contacts (MOL)
	b.q("CESL.THK", "Contact etch stop nitride thickness", "thickness", "nm", 30, 27, 33, 0.4, true)
	b.add(RouteStep{Code: "CESL-DEP", Layer: "PMD", Module: "MOL", Op: "Contact etch stop nitride", Type: stCVD, Recipe: "CESL-SIN-300A",
		Makes: []string{"CESL.THK"}})
	b.add(RouteStep{Code: "CESL-THK", Layer: "PMD", Module: "MOL", Op: "Etch stop nitride thickness", Type: stMet, Recipe: "THK-CESL", Meas: "CESL.THK",
		Param: "thickness", Sample: 3, Rate: 0.5, OnFail: failHold})
	b.add(RouteStep{Code: "PMD-DEP", Layer: "PMD", Module: "MOL", Op: "Pre-metal dielectric (HDP, 5000 Å)", Type: stCVD, Recipe: "PMD-HDP-5000A",
		WaferS: 160, LotS: 300})
	b.q("PMD.CMP", "PMD thickness after CMP", "thickness", "nm", 300, 280, 320, 5, false)
	b.add(RouteStep{Code: "PMD-CMP", Layer: "PMD", Module: "MOL", Op: "PMD oxide CMP", Type: stCMPD, Recipe: "PMD-CMP", Makes: []string{"PMD.CMP"}})
	b.add(RouteStep{Code: "PMD-PCC", Layer: "PMD", Module: "MOL", Op: "Post-CMP brush scrub", Type: stSWC, Recipe: "SCRUB-CMP"})
	b.add(RouteStep{Code: "PMD-THK", Layer: "PMD", Module: "MOL", Op: "PMD thickness", Type: stMet, Recipe: "THK-PMD", Meas: "PMD.CMP",
		Param: "thickness", Sample: 3, OnFail: failRepolish, Back: b.seqOf("PMD-CMP")})
	b.photo("CT", "MOL", stLithoArF, "CT-AR", 55, 10, false)
	b.q("CT.CDE", "Contact CD after etch", "cd", "nm", 50, 46, 54, 0.75, false)
	b.add(RouteStep{Code: "CT-ET", Layer: "CT", Module: "MOL", Op: "Contact etch (in-situ strip)", Type: stEtchD, Recipe: "CT-ETCH", Makes: []string{"CT.CDE"}})
	b.add(RouteStep{Code: "CT-CLN", Layer: "CT", Module: "MOL", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-CT"})
	b.add(RouteStep{Code: "CT-ECD", Layer: "CT", Module: "MOL", Op: "Contact CD (OCD)", Type: stMet, Recipe: "OCD-CT", Meas: "CT.CDE", Param: "cd",
		Sample: 3, OnFail: failScrap})
	b.add(RouteStep{Code: "CT-BAR", Layer: "CT", Module: "MOL", Op: "Ti/TiN contact barrier", Type: stPVD, Recipe: "TI-TIN"})
	b.add(RouteStep{Code: "CT-W", Layer: "CT", Module: "MOL", Op: "W CVD contact fill (2500 Å)", Type: stCVD, Recipe: "W-CVD-2500A", WaferS: 150, LotS: 300})
	b.add(RouteStep{Code: "CT-CMP", Layer: "CT", Module: "MOL", Op: "W CMP", Type: stCMPD, Recipe: "W-CMP"})
	b.add(RouteStep{Code: "CT-PCC", Layer: "CT", Module: "MOL", Op: "Post-CMP brush scrub", Type: stSWC, Recipe: "SCRUB-CMP"})
	b.add(RouteStep{Code: "CT-DEF", Layer: "CT", Module: "MOL", Op: "Contact defect inspection", Type: stDefect, Recipe: "DF-MOL", Param: "defects",
		Sample: 1, Rate: 0.5, OnFail: failReclean, Back: b.seqOf("CT-CLN")})
	// --- Cu interconnect
	for n := 1; n <= thin+thick; n++ {
		b.metal(n, n > thin)
		if rf && n == thin+thick-1 {
			// MIM capacitor under the top metal
			b.add(RouteStep{Code: "MIM-DEP", Layer: "MIM", Module: "BEOL", Op: "MIM stack (TiN / SiN / TiN)", Type: stPVD, Recipe: "MIM-STACK"})
			b.photo("MIM", "BEOL", stLithoKrF, "MIM", 0, 25, false)
			b.add(RouteStep{Code: "MIM-ET", Layer: "MIM", Module: "BEOL", Op: "MIM top plate etch (in-situ strip)", Type: stEtchC, Recipe: "MIM-ETCH"})
			b.add(RouteStep{Code: "MIM-CLN", Layer: "MIM", Module: "BEOL", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-MIM"})
		}
	}
	// --- passivation, Al pads, alloy, final checks
	b.q("PAS1.THK", "Passivation thickness", "thickness", "nm", 600, 570, 630, 6, true)
	b.add(RouteStep{Code: "PAS1-DEP", Layer: "PAD", Module: "BEOL", Op: "Passivation oxide / nitride", Type: stCVD, Recipe: "PASS-OX-SIN",
		Makes: []string{"PAS1.THK"}})
	b.add(RouteStep{Code: "PAS1-THK", Layer: "PAD", Module: "BEOL", Op: "Passivation thickness", Type: stMet, Recipe: "THK-PAS1", Meas: "PAS1.THK",
		Param: "thickness", Sample: 3, Rate: 0.5, OnFail: failHold})
	b.photo("PAD", "BEOL", stLithoI, "PAD-VIA", 0, 45, false)
	b.add(RouteStep{Code: "PAD-ET", Layer: "PAD", Module: "BEOL", Op: "Pad via etch (in-situ strip)", Type: stEtchD, Recipe: "PAD-VIA-ETCH"})
	b.add(RouteStep{Code: "PAD-CLN", Layer: "PAD", Module: "BEOL", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-OX"})
	b.add(RouteStep{Code: "AP-DEP", Layer: "AP", Module: "BEOL", Op: "AlCu pad metal (1.2 µm)", Type: stPVD, Recipe: "ALCU-12KA"})
	b.photo("AP", "BEOL", stLithoI, "AP", 0, 45, false)
	b.add(RouteStep{Code: "AP-ET", Layer: "AP", Module: "BEOL", Op: "Al etch (in-situ strip)", Type: stEtchC, Recipe: "AL-ETCH"})
	b.add(RouteStep{Code: "AP-CLN", Layer: "AP", Module: "BEOL", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-AL"})
	b.q("PAS2.THK", "Final passivation nitride thickness", "thickness", "nm", 500, 475, 525, 5, true)
	b.add(RouteStep{Code: "PAS2-DEP", Layer: "PAS2", Module: "BEOL", Op: "Final passivation nitride", Type: stCVD, Recipe: "PASS2-SIN",
		Makes: []string{"PAS2.THK"}})
	b.add(RouteStep{Code: "PAS2-THK", Layer: "PAS2", Module: "BEOL", Op: "Final passivation thickness", Type: stMet, Recipe: "THK-PAS2", Meas: "PAS2.THK",
		Param: "thickness", Sample: 3, Rate: 0.5, OnFail: failHold})
	b.photo("PAS2", "BEOL", stLithoI, "PAS2", 0, 45, false)
	b.add(RouteStep{Code: "PAS2-ET", Layer: "PAS2", Module: "BEOL", Op: "Pad opening etch (in-situ strip)", Type: stEtchD, Recipe: "PAD-OPEN"})
	b.add(RouteStep{Code: "PAS2-CLN", Layer: "PAS2", Module: "BEOL", Op: "Post-etch clean", Type: stSWC, Recipe: "PET-PAS"})
	b.add(RouteStep{Code: "ALLOY", Layer: "PAS2", Module: "BEOL", Op: "Forming gas alloy (400 °C)", Type: stFurnOx, Recipe: "H2-ALLOY", LotS: 1.5 * 3600})
	b.add(RouteStep{Code: "FIN-DEF", Layer: "FINAL", Module: "TEST", Op: "Final defect inspection", Type: stDefect, Recipe: "DF-FINAL", Param: "defects",
		Sample: 2, OnFail: failReclean, Back: b.seqOf("PAS2-CLN")})
	b.q("WAT", "WAT sites in specification", "wat", "%", 100, 92, nan, 0.8, false)
	b.add(RouteStep{Code: "WAT", Layer: "FINAL", Module: "TEST", Op: "Wafer acceptance test (parametric)", Type: stWAT, Recipe: "WAT-40LP",
		Meas: "WAT", Param: "wat", OnFail: failScrap})
	p.Metals = thin + thick
}

func fabProducts() []*Product {
	mcu := &Product{Code: "MCU40", Name: "40 nm low-power microcontroller (Cortex-M0+ class, cf. Raspberry Pi RP2040)",
		Tech: "40 nm LP CMOS, 1P6M Cu / low-k, AlCu pads", Mix: 0.6, CycleDays: 26, DieMM2: 2.0}
	route40(mcu, 5, 1, false)
	soc := &Product{Code: "WSOC40", Name: "40 nm Wi-Fi / Bluetooth SoC (cf. Espressif ESP32 class)",
		Tech: "40 nm LP CMOS, 1P8M Cu / low-k with 2 thick RF metals, MIM capacitor, deep n-well", Mix: 0.4, CycleDays: 30, DieMM2: 21.0}
	route40(soc, 6, 2, true)
	return []*Product{mcu, soc}
}
