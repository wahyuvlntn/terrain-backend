package main

import "strings"

// InstrSpec is a maintenance instruction (SOP). Content is Markdown rendered
// by the instruction editor (MDX): it must not contain angle or curly braces.
type InstrSpec struct {
	Code    string
	Type    int // 1 Diagnostic, 2 Repair, 3 Severity
	Title   string
	Minutes int
	Desc    string
	Content string
}

const loto = "Apply lock-out / tag-out (LOTO) on the motor starter and isolate the equipment before opening any panel."

func steps(intro string, items ...string) string {
	var b strings.Builder
	if intro != "" {
		b.WriteString(intro + "\n\n")
	}
	b.WriteString("### Procedure\n\n")
	for i, it := range items {
		b.WriteString(itoa(i+1) + ". " + it + "\n")
	}
	return b.String()
}

func itoa(i int) string {
	const d = "0123456789"
	if i < 10 {
		return d[i : i+1]
	}
	return itoa(i/10) + d[i%10:i%10+1]
}

func withSafety(s string) string {
	return "**Safety:** " + loto + " Wear safety glasses, gloves and hearing protection in plant rooms.\n\n" + s
}

var instructions = []InstrSpec{
	// --- Chiller ---------------------------------------------------------------
	{"CHL-005", 1, "Verify condenser fouling from approach trend", 45, "Confirm condenser heat transfer loss using approach, condenser pressure and water-side data.",
		steps("Condenser approach above 3 K together with high condenser pressure indicates tube fouling or loss of condenser water flow.",
			"Trend condenser approach, condenser pressure and condenser water leaving temperature for the last 14 days on the equipment overview page.",
			"Check condenser water flow at the strainer differential gauge and confirm both CW pumps are running at design speed.",
			"Compare condenser water entering temperature with the cooling tower supply - a difference above 0.5 K points to a sensor problem.",
			"Check the purge unit run time; frequent purging means non-condensables, not fouling.",
			"Record the readings in the work report and mark the diagnosis valid if water flow and sensors are normal.")},
	{"CHL-005", 2, "Mechanical brush cleaning of condenser tubes", 240, "Isolate the condenser water side, open the water boxes and brush clean the condenser tubes.",
		withSafety(steps("Schedule on the standby chiller rotation. Expected duration 4 hours with two technicians.",
			"Transfer duty to the standby chiller and confirm stable chilled water supply at 6.5 °C.",
			"Stop the chiller, close condenser water isolation valves and drain the condenser water boxes.",
			"Remove the water box covers and inspect the tube sheet for scale, debris and corrosion.",
			"Brush every tube with the rotary tube cleaner and flush with clean water.",
			"Inspect the sacrificial anodes and replace if more than 50 % consumed.",
			"Refit covers with new gaskets, refill, vent air and check for leaks.",
			"Restart and confirm condenser approach is below 1.5 K at more than 60 % load."))},
	{"CHL-005", 3, "Severity assessment - high condenser pressure", 15, "Decide whether the chiller can stay in service while fouling is being corrected.",
		steps("",
			"Condenser approach between 3 and 4.5 K - schedule cleaning within 2 weeks; efficiency penalty about 8 to 12 %.",
			"Condenser pressure above 1,080 kPa - unload the chiller to 80 % and schedule cleaning within 72 hours.",
			"Repeated high pressure trips - transfer load to the standby chiller immediately.")},
	{"CHL-006", 1, "Refrigerant leak check and charge verification", 60, "Locate refrigerant leaks and confirm charge level before adding refrigerant.",
		steps("Low evaporator pressure with high superheat and evaporator approach indicates low refrigerant charge.",
			"Check the refrigerant monitor in the plant room for R-134a concentration alarms.",
			"Inspect the evaporator sight glass and liquid line for flashing.",
			"Use an electronic leak detector on flanges, the relief valve, service valves and the purge unit.",
			"Pressure-test suspicious joints with soap solution.",
			"Record the leak location and estimated leak rate in the work report.")},
	{"CHL-006", 2, "Repair leak and recharge refrigerant", 180, "Repair the leaking joint, evacuate if required and recharge R-134a to nameplate charge.",
		withSafety(steps("Refrigerant handling must be done by a licensed technician. Log every kilogram added or recovered.",
			"Recover refrigerant from the affected section into a certified recovery cylinder.",
			"Replace the leaking gasket, valve core or flare joint.",
			"Pressure test with dry nitrogen for 30 minutes.",
			"Evacuate to 500 microns and confirm the vacuum holds for 15 minutes.",
			"Charge R-134a by weight and confirm superheat and evaporator approach return to normal.",
			"Update the refrigerant log book."))},
	{"CHL-010", 1, "Vibration analysis of compressor bearings", 45, "Measure vibration spectrum and oil condition to confirm bearing wear.",
		steps("",
			"Take vibration readings at the compressor drive end and non-drive end in three axes.",
			"Look for bearing defect frequencies and rising high-frequency energy in the spectrum.",
			"Check oil sump temperature and oil pressure differential trends.",
			"Take an oil sample for wear-metal analysis.",
			"Compare with the baseline readings from commissioning.")},
	{"CHL-010", 2, "Replace compressor bearings", 360, "Replace worn compressor bearings and oil, then verify vibration levels.",
		withSafety(steps("Major repair - coordinate with the OEM service engineer. Plan 6 hours on standby rotation.",
			"Transfer load to the standby chiller and isolate the unit.",
			"Recover refrigerant from the compressor section.",
			"Remove the compressor end cover and replace thrust and journal bearings.",
			"Replace the oil filter and change compressor oil.",
			"Evacuate, recharge and restart. Vibration must be below 2.8 mm/s at full load."))},
	// --- MAU -----------------------------------------------------------------
	{"AHU-010", 1, "Inspect pre-filter bank", 30, "Confirm pre-filter loading from differential pressure and visual inspection.",
		steps("",
			"Read the pre-filter differential pressure gauge and compare with the BMS value.",
			"Open the filter section access door and inspect the filters for dust load and damage.",
			"Check fan speed - a VSD at more than 80 % to hold airflow confirms filter resistance.",
			"Check the outdoor air intake louvre and bird screen for blockage.")},
	{"AHU-010", 2, "Replace pre-filters", 90, "Replace the G4/F7 pre-filter bank and reset the differential pressure baseline.",
		withSafety(steps("Pre-filter replacement can be done with the fan running at reduced speed only if the cleanroom team approves; otherwise stop the MAU.",
			"Reduce fan speed or stop the MAU after informing the cleanroom control room.",
			"Remove the used filters and bag them for disposal.",
			"Clean the filter frame and check the gaskets.",
			"Install new filters with airflow arrows in the correct direction.",
			"Restart and confirm pre-filter differential pressure is below 100 Pa at design airflow."))},
	{"AHU-011", 1, "Check cooling coil and CHW valve", 45, "Determine whether the coil is fouled or the chilled water valve is not opening.",
		steps("Supply air temperature drifting above set point while the valve command is near 100 % indicates a coil or valve problem.",
			"Command the CHW valve from 0 to 100 % and observe actuator travel.",
			"Measure chilled water entering and leaving temperature across the coil.",
			"Inspect the coil face for dust and biological growth.",
			"Check the condensate drain pan and trap.")},
	{"AHU-011", 2, "Clean cooling coil and service CHW valve", 150, "Chemically clean the cooling coil and recalibrate or replace the CHW valve actuator.",
		withSafety(steps("",
			"Stop the MAU and isolate the chilled water valves.",
			"Apply approved non-acid coil cleaner and rinse with low pressure water.",
			"Clean the drain pan and treat with biocide tablets.",
			"Stroke the CHW valve and recalibrate the actuator; replace if travel is incomplete.",
			"Restart and confirm supply air at 17 °C and 45 % RH."))},
	{"AHU-012", 1, "Inspect fan drive belts", 30, "Check belt tension, wear and alignment of the supply fan drive.",
		steps("",
			"Stop the fan and apply LOTO.",
			"Check belt deflection with a tension gauge.",
			"Inspect belts for glazing, cracks and missing teeth.",
			"Check pulley alignment with a laser alignment tool.")},
	{"AHU-012", 2, "Replace and tension fan belts", 75, "Replace the matched belt set, align pulleys and set tension.",
		withSafety(steps("",
			"Remove the belt guard and slacken the motor base.",
			"Replace all belts as a matched set.",
			"Align pulleys within 0.5 mm.",
			"Tension to the manufacturer value and refit the guard.",
			"Run for 30 minutes, re-check tension and confirm design airflow."))},
	// --- Cooling tower -------------------------------------------------------
	{"CT-010", 1, "Gearbox vibration and oil inspection", 45, "Assess the condition of the fan gearbox and drive shaft.",
		steps("",
			"Measure vibration at the gearbox input and output bearings.",
			"Check gearbox oil level and colour; take an oil sample.",
			"Inspect the drive shaft couplings and the fan blade pitch.",
			"Listen for abnormal gear noise at full fan speed.")},
	{"CT-010", 2, "Overhaul fan gearbox", 300, "Replace gearbox bearings and seals, refill oil and balance the fan.",
		withSafety(steps("Work at height - use the tower fan deck harness points.",
			"Stop the tower cell and transfer load to the other cells.",
			"Drain gearbox oil and remove the gearbox.",
			"Replace bearings and oil seals; inspect gears for pitting.",
			"Refit, align the drive shaft, refill with ISO VG 220 oil.",
			"Balance the fan and confirm vibration below 2.8 mm/s."))},
	{"CT-011", 1, "Inspect fill media and water treatment", 45, "Inspect the fill for scale and review the water treatment log.",
		steps("",
			"Isolate the cell and inspect the fill pack from the access door.",
			"Check the distribution basin nozzles for blockage.",
			"Review conductivity, pH and inhibitor dosing records.",
			"Check the blowdown valve operation.")},
	{"CT-011", 2, "Descale fill and restore water treatment", 240, "Chemically descale the fill pack and correct the treatment programme.",
		withSafety(steps("",
			"Coordinate with the water treatment vendor for descaling chemicals.",
			"Circulate descaling solution with the fan off for the recommended time.",
			"Flush and neutralise the basin.",
			"Clean distribution nozzles and replace damaged fill sections.",
			"Restore conductivity set point to 1,500 µS/cm and verify blowdown."))},
	{"CT-012", 1, "Check make-up water supply", 20, "Verify the float valve and make-up water supply pressure.",
		steps("",
			"Check the basin level gauge against the BMS reading.",
			"Inspect the make-up float valve and solenoid valve.",
			"Check the make-up water supply pressure and the strainer.")},
	{"CT-012", 2, "Repair make-up water valve", 60, "Replace the faulty float or solenoid valve and restore basin level.",
		steps("",
			"Isolate the make-up supply.",
			"Replace the float valve seat or the solenoid coil.",
			"Clean the strainer.",
			"Restore supply and confirm basin level recovers to 70 to 80 %.")},
	// --- PCW pump -------------------------------------------------------------
	{"PMP-010", 1, "Check suction conditions", 30, "Confirm cavitation by checking suction pressure, strainer and noise.",
		steps("",
			"Read suction and discharge gauges and compare with the BMS.",
			"Measure the suction strainer differential pressure.",
			"Listen for gravel-like cavitation noise at the pump casing.",
			"Check the expansion tank pre-charge and system make-up pressure.")},
	{"PMP-010", 2, "Clean suction strainer", 90, "Isolate the pump and clean the suction strainer basket.",
		withSafety(steps("Transfer duty to the standby pump before isolating.",
			"Close suction and discharge isolation valves.",
			"Drain the strainer housing and remove the basket.",
			"Clean the basket and inspect the mesh for damage.",
			"Refit with a new gasket, refill, vent and return the pump to auto.",
			"Confirm suction pressure above 1.8 bar at duty flow."))},
	{"PMP-011", 1, "Bearing condition assessment", 30, "Measure vibration and bearing temperature to confirm bearing deterioration.",
		steps("",
			"Measure vibration velocity at both bearing housings.",
			"Measure bearing housing temperature with an IR thermometer.",
			"Check the grease condition and re-greasing records.",
			"Check coupling alignment.")},
	{"PMP-011", 2, "Replace pump bearings", 240, "Replace pump and motor bearings and re-align the coupling.",
		withSafety(steps("",
			"Transfer duty to the standby pump and isolate.",
			"Remove the coupling guard and coupling spacer.",
			"Pull the bearing housing and replace both bearings.",
			"Grease with the specified grease and refit.",
			"Laser-align the coupling to within 0.05 mm.",
			"Run test - vibration below 2.8 mm/s and bearing temperature below 65 °C."))},
	{"PMP-012", 1, "Pump performance test", 45, "Compare actual pump curve with the manufacturer curve.",
		steps("",
			"Record flow, suction and discharge pressure and motor power at three speeds.",
			"Plot the operating points on the manufacturer pump curve.",
			"A head drop of more than 10 % at the same flow indicates impeller wear.")},
	{"PMP-012", 2, "Replace impeller and wear rings", 300, "Dismantle the pump, replace the impeller and wear rings.",
		withSafety(steps("",
			"Isolate and drain the pump.",
			"Remove the back pull-out assembly.",
			"Replace the impeller and casing wear rings; check clearances.",
			"Replace the mechanical seal.",
			"Reassemble, align, refill and run test against the pump curve."))},
	{"PMP-005", 1, "Investigate motor overload trip", 30, "Find the cause of the motor overload trip before restarting.",
		steps("",
			"Check the VSD fault log and trip current.",
			"Megger test the motor windings.",
			"Rotate the pump by hand to check for seizure.",
			"Check for closed discharge valve or system blockage.")},
	{"PMP-005", 2, "Reset and restart after overload", 30, "Reset the VSD and restart the pump after the cause is cleared.",
		steps("",
			"Clear the fault on the VSD.",
			"Restart in local mode and monitor current for 15 minutes.",
			"Return to auto and record the trip in the work report.")},
	// --- CDA compressor -------------------------------------------------------
	{"CDA-010", 1, "Check intercooler performance", 30, "Confirm intercooler fouling from temperatures and cooling water flow.",
		steps("",
			"Record intercooler outlet temperature and cooling water inlet/outlet temperatures.",
			"Check cooling water flow and strainer.",
			"An outlet temperature more than 12 K above cooling water inlet indicates fouling.")},
	{"CDA-010", 2, "Clean intercooler", 240, "Descale the water side of the intercooler and aftercooler.",
		withSafety(steps("Compressor must be depressurised and isolated.",
			"Isolate cooling water and drain the coolers.",
			"Circulate descaling solution through the water side.",
			"Flush, neutralise and leak test.",
			"Restart and confirm HP element outlet below 180 °C."))},
	{"CDA-011", 1, "Dryer performance check", 30, "Check dew point sensor, regeneration heater and desiccant.",
		steps("",
			"Verify the dew point sensor with a portable hygrometer.",
			"Check regeneration heater current and outlet temperature.",
			"Check the dryer cycle valves for correct switching.")},
	{"CDA-011", 2, "Replace desiccant and service dryer", 300, "Replace the desiccant charge and regeneration components.",
		withSafety(steps("",
			"Bypass and depressurise the dryer.",
			"Remove the spent desiccant.",
			"Fill with new desiccant and replace the dust filters.",
			"Test the regeneration heater.",
			"Recommission and confirm pressure dew point below -40 °C."))},
	{"CDA-012", 1, "Ultrasonic leak survey", 120, "Survey the CDA distribution for leaks using an ultrasonic detector.",
		steps("Rising compressor load at constant production means air is being lost in the distribution.",
			"Compare CDA demand with the fab tool schedule.",
			"Survey sub-fab drops, quick couplings and regulators with an ultrasonic detector.",
			"Tag each leak with location and estimated loss.")},
	{"CDA-012", 2, "Repair compressed air leaks", 180, "Repair tagged leaks and verify the demand reduction.",
		steps("",
			"Isolate each leaking branch with the area owner.",
			"Replace couplings, fittings and regulator diaphragms.",
			"Re-survey and confirm compressor load is back to baseline.")},
	{"CDA-004", 1, "Investigate compressor motor trip", 30, "Check motor, VSD and element condition after an overload trip.",
		steps("",
			"Read the controller event log.",
			"Check motor insulation resistance.",
			"Check the element for seizure by rotating the coupling.")},
	// --- Dry pump -------------------------------------------------------------
	{"VAC-010", 1, "Check pump for by-product build-up", 30, "Confirm deposition from current, temperature and exhaust pressure trends.",
		steps("Rising motor current and body temperature during process steps indicate process by-product deposition inside the pump.",
			"Trend dry pump current, body temperature and exhaust pressure for the last 7 days.",
			"Check the exhaust line heater jacket temperature.",
			"Coordinate with the process engineer on recent recipe changes.")},
	{"VAC-010", 2, "Pump exchange and refurbishment", 240, "Exchange the dry pump with a refurbished spare and send the unit for cleaning.",
		withSafety(steps("Harsh process pump - follow the toxic gas purge procedure before breaking vacuum.",
			"Coordinate a tool-down window with the process team.",
			"Purge the pump with N2 for 30 minutes.",
			"Disconnect inlet, exhaust, cooling water and N2 lines.",
			"Install the refurbished pump and leak check with helium.",
			"Return the tool to production after base pressure is confirmed."))},
	{"VAC-011", 1, "Pumping speed check", 30, "Measure base pressure and booster current to confirm rotor wear.",
		steps("",
			"Record base pressure with the chamber isolated.",
			"Compare booster current with the baseline at the same gas load.",
			"Check for inlet leaks with a helium leak detector.")},
	{"VAC-011", 2, "Rotor and booster overhaul", 360, "Send the pump for rotor and booster overhaul and install a spare.",
		withSafety(steps("",
			"Follow the pump exchange procedure.",
			"Raise an RMA with the pump vendor for rotor overhaul.",
			"Update the asset records with the spare pump serial number."))},
	{"VAC-012", 1, "Check N2 purge line", 20, "Find the restriction in the N2 purge supply.",
		steps("",
			"Check the N2 supply pressure regulator.",
			"Check the purge flow meter and the inline filter.",
			"Inspect the purge solenoid valve.")},
	{"VAC-012", 2, "Clear N2 purge blockage", 60, "Replace the purge filter and restore purge flow.",
		steps("",
			"Isolate N2 to the pump.",
			"Replace the inline filter and clean the restrictor.",
			"Restore N2 and confirm purge flow above 45 slm.")},
	{"VAC-001", 1, "Investigate dry pump overcurrent", 20, "Check motor current against process gas load.",
		steps("",
			"Compare current with process step timing.",
			"Check for exhaust restriction.",
			"Check pump body temperature.")},
	// --- Preventive maintenance checklists -----------------------------------
	{"PM-CHL", 2, "Chiller monthly inspection checklist", 240, "Monthly preventive maintenance checklist for centrifugal chillers.",
		withSafety(steps("Perform on the standby chiller of the weekly rotation.",
			"Record operating log: pressures, temperatures, oil, current at stable load.",
			"Check refrigerant monitor and perform leak check of flanges.",
			"Check oil level, oil heater and oil filter differential pressure.",
			"Inspect starter contacts and tighten terminations (thermal scan).",
			"Check condenser and evaporator flow switches.",
			"Calibrate chilled water supply temperature sensor.",
			"Run the chiller and verify condenser approach below 1.5 K."))},
	{"PM-MAU", 2, "MAU monthly filter and fan service", 180, "Monthly MAU service: filters, fan drive, coil and humidifier checks.",
		withSafety(steps("Inform the cleanroom control room before stopping the MAU.",
			"Replace pre-filters (G4/F7) and record differential pressure before and after.",
			"Inspect HEPA final filter differential pressure.",
			"Check fan belts, bearings and lubrication.",
			"Inspect the cooling coil and clean the drain pan.",
			"Calibrate supply air temperature and humidity sensors.",
			"Verify airflow and duct static pressure after restart."))},
	{"PM-CT", 2, "Cooling tower monthly maintenance", 180, "Monthly cooling tower service: water treatment, fan drive and basin.",
		withSafety(steps("",
			"Inspect fill, drift eliminators and distribution nozzles.",
			"Check gearbox oil level and fan blade condition.",
			"Clean the basin strainer and check the make-up float valve.",
			"Verify chemical dosing pumps and conductivity controller.",
			"Take a water sample for Legionella and bacteria testing."))},
	{"PM-PCW", 2, "PCW pump monthly inspection", 120, "Monthly PCW pump inspection: vibration, seal, bearings and strainer.",
		withSafety(steps("Perform on the standby pump of the rotation.",
			"Record vibration at both bearings.",
			"Inspect the mechanical seal for leakage.",
			"Re-grease bearings according to schedule.",
			"Check suction strainer differential pressure and clean if above 0.3 bar.",
			"Check coupling and baseplate bolts."))},
	{"PM-CDA", 2, "Compressor monthly service", 240, "Monthly CDA compressor service: filters, coolers, dryer and drains.",
		withSafety(steps("Perform on the trim compressor during low demand.",
			"Replace the air intake filter if differential pressure is above 50 mbar.",
			"Check intercooler and aftercooler temperatures.",
			"Test the electronic condensate drains.",
			"Check dryer dew point and regeneration cycle.",
			"Inspect motor and VSD cooling fans."))},
	{"PM-VAC", 2, "Dry pump preventive overhaul", 360, "Scheduled dry pump overhaul based on running hours.",
		withSafety(steps("Coordinate a tool-down window with the process team.",
			"Purge the pump with N2 and exchange it with a refurbished spare.",
			"Check exhaust line heater and N2 purge flow.",
			"Leak check with helium and verify base pressure.",
			"Reset the running hour counter in the work report."))},
	// --- General --------------------------------------------------------------
	{"GEN-001", 2, "Replace mechanical seal", 180, "Replace a leaking mechanical seal.",
		withSafety(steps("",
			"Isolate and drain the equipment.",
			"Remove the coupling and seal gland.",
			"Replace the mechanical seal cartridge.",
			"Refill, vent and check for leaks during run test."))},
	{"GEN-002", 1, "Investigate abnormal noise", 30, "Locate the source of abnormal noise and assess urgency.",
		steps("",
			"Use a stethoscope or ultrasonic detector to locate the noise.",
			"Measure vibration at the nearest bearing.",
			"Check for loose guards, panels and fixings.")},
	{"GEN-003", 2, "Repair damaged insulation or panel", 90, "Repair damaged insulation, casing or access panel.",
		steps("",
			"Remove the damaged insulation or panel section.",
			"Check for condensation or corrosion underneath.",
			"Install new insulation with vapour barrier and seal joints.")},
}

// instructionModels returns the model keys an instruction applies to.
func instructionModels(code string) []string {
	switch {
	case strings.HasPrefix(code, "CHL") || code == "PM-CHL":
		return []string{"chiller"}
	case strings.HasPrefix(code, "AHU") || code == "PM-MAU":
		return []string{"mau"}
	case strings.HasPrefix(code, "CT") || code == "PM-CT":
		return []string{"ct"}
	case strings.HasPrefix(code, "PMP") || code == "PM-PCW":
		return []string{"pcw", "chwp", "cwp"}
	case strings.HasPrefix(code, "CDA") || code == "PM-CDA":
		return []string{"cda"}
	case strings.HasPrefix(code, "VAC") || code == "PM-VAC":
		return []string{"vac"}
	case strings.HasPrefix(code, "CHS"):
		return []string{"chws"}
	case code == "GEN-001":
		return []string{"pcw", "chwp", "cwp", "chiller"}
	case code == "GEN-002":
		return []string{"chiller", "ct", "pcw", "chwp", "cwp", "cda", "mau", "vac"}
	case code == "GEN-003":
		return []string{"mau", "ct"}
	}
	return nil
}

// workNotes are technician work-update notes per issue code.
var workNotes = map[string][]string{
	"CHL-005": {"Water boxes opened - heavy scale on lower tube passes. Brushing in progress.", "All 412 tubes brushed and flushed. Anodes 40 % consumed, kept."},
	"CHL-006": {"Leak found at the evaporator relief valve flange. Gasket replaced.", "Pressure test held for 30 min. Added 38 kg R-134a, superheat back to 0.7 K."},
	"CHL-010": {"Vibration spectrum shows bearing outer race defect frequency. OEM engineer on site.", "Thrust and journal bearings replaced, oil changed. Vibration 1.9 mm/s at 85 % load."},
	"CHL-003": {"Discharge temperature trip reset. Hot gas bypass valve checked, chiller restarted."},
	"AHU-010": {"Pre-filters heavily loaded with outdoor dust after haze period.", "New G4/F7 filters installed. Pre-filter DP 92 Pa at design airflow."},
	"AHU-011": {"Coil face fouled, CHW valve actuator stroking only 70 %.", "Coil cleaned and actuator replaced. Supply air stable at 17.0 °C."},
	"AHU-012": {"Belts glazed and loose - 3 of 4 belts slipping.", "Matched belt set replaced and tensioned. Airflow back to 60,200 m³/h."},
	"AHU-005": {"VFD tripped on overcurrent. Motor megger test OK, fault reset."},
	"CT-010":  {"Gearbox oil dark with metal particles. Output bearing noisy.", "Gearbox bearings and seals replaced, fan balanced. Vibration 2.3 mm/s."},
	"CT-011":  {"Fill pack scaled on the air inlet side. Conductivity controller found faulty.", "Fill descaled, controller replaced. Approach 3.4 K."},
	"CT-012":  {"Float valve seat worn, valve not opening fully.", "Float valve replaced, basin level 76 %."},
	"CT-002":  {"Vibration switch tripped. Fan blade pitch re-checked, restarted."},
	"PMP-010": {"Suction strainer 70 % blocked with debris from the recent PCW tie-in.", "Strainer cleaned, suction pressure 2.0 bar."},
	"PMP-011": {"DE bearing rough, grease degraded.", "Both bearings replaced, coupling aligned. Vibration 1.6 mm/s."},
	"PMP-012": {"Pump curve test shows 18 % head loss at duty flow.", "Impeller and wear rings replaced. Flow back to 320 m³/h."},
	"PMP-005": {"Overload trip - discharge valve found partially closed after valve exercise. Valve opened, pump restarted."},
	"CDA-010": {"Intercooler water side heavily scaled.", "Coolers descaled. HP element outlet 172 °C."},
	"CDA-011": {"Regeneration heater element open circuit.", "Heater replaced, desiccant topped up. PDP -44 °C."},
	"CDA-012": {"Ultrasonic survey found 14 leaks in sub-fab drops, total about 3.5 Nm³/min.", "Leaks repaired, compressor load back to 78 %."},
	"CDA-004": {"Motor overload trip - VSD heatsink fan failed. Fan replaced, restarted."},
	"VAC-010": {"Heavy powder build-up in exhaust port. Pump exchanged with refurbished spare.", "Spare pump running, base pressure 0.012 mbar."},
	"VAC-011": {"Base pressure 3x baseline, booster current high. Pump exchanged.", "Spare installed, rotor overhaul RMA raised."},
	"VAC-012": {"N2 inline filter clogged.", "Filter replaced, purge flow 51 slm."},
	"VAC-001": {"Overcurrent trip during process step. Exhaust line heater found off, reset."},
	"PM-CHL":  {"Monthly inspection done. Oil filter DP normal, starter thermal scan OK."},
	"PM-MAU":  {"Pre-filters replaced, belts checked, coil and drain pan cleaned."},
	"PM-CT":   {"Basin cleaned, dosing pumps verified, water sample taken."},
	"PM-PCW":  {"Bearings re-greased, seal OK, strainer DP 0.1 bar."},
	"PM-CDA":  {"Intake filter replaced, drains tested, dryer cycle verified."},
	"PM-VAC":  {"Pump exchanged as per running hour schedule. Spare serial recorded."},
}

func noteFor(code string, i int) string {
	n := workNotes[code]
	if len(n) == 0 {
		return "Inspection completed, findings recorded."
	}
	if i >= len(n) {
		i = len(n) - 1
	}
	return n[i]
}

// requestTexts are operator descriptions of manual maintenance requests.
var requestTexts = map[string]string{
	"CHL-003": "Chiller tripped on high compressor discharge temperature - standby chiller started automatically.",
	"AHU-005": "MAU fan VFD tripped. Cleanroom pressure alarm in FAB1, please check urgently.",
	"CT-002":  "Cooling tower fan stopped on vibration switch.",
	"PMP-005": "PCW pump tripped on motor overload. Standby pump took over.",
	"CDA-004": "CDA compressor tripped, header pressure dipped briefly.",
	"VAC-001": "Dry pump tripped during etch step - chamber down.",
	"GEN-001": "Water dripping from pump mechanical seal area.",
	"GEN-002": "Unusual grinding noise reported by night shift.",
	"GEN-003": "Insulation on chilled water pipe damaged, condensation dripping on floor.",
}
