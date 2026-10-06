package main

import "strings"

// Extended AFDD issue mapping (seed version 10): a second, compound diagnosis
// for the degradations of the process tools and facility units. Each one is an
// issue relation whose paths need several symptoms of the same fault together
// (alarm and abnormal parameters), its diagnosis code, diagnostic / repair /
// severity SOPs, the alarm mapping and technician work notes. The code is added
// to the fault's repair codes, so the order of either diagnosis repairs it.

type afddDiag struct {
	Keys    []string // model keys
	Fault   string   // fault kind the diagnosis points to
	Code    string
	Name    string
	Rel     string // issue relation name
	Paths   [][]string
	Params  []string // alarm mapping (related parameters)
	Check   []string // diagnostic steps
	Repair  string   // repair SOP title
	Fix     []string // repair steps
	Minutes [2]int   // diagnostic, repair duration
	Notes   []string // technician findings, work done
}

var afddDiags = []afddDiag{
	// --- Lithography ----------------------------------------------------------
	{[]string{"scanarf", "scankrf"}, "lens_heating", "SCN-014", "Critical Dimension Drift from Focus", "Focus Drift with CD Impact (Compound)",
		[][]string{{"A:SCN-001", "P:focus_offset"}}, []string{"focus_offset", "lens_temp", "dose"},
		[]string{"Compare the focus offset against the lens temperature: an offset that follows the exposure load is lens heating.",
			"Check the resist CD of the last lots of the layer on the CD-SEM (Production page, quality tab).",
			"Expose a focus-exposure matrix on a test wafer and measure best focus against the qualification value."},
		"Requalify focus and the lens heating model", []string{"Run the focus and leveling calibration with the reference wafer.",
			"Run the lens heating calibration and load the new model coefficients.", "Expose a send-ahead wafer of the most critical layer and check the CD."},
		[2]int{40, 210}, []string{"Best focus 38 nm off the baseline after a long exposure run; CD of the gate layer 2.1 nm above target.",
			"Lens heating model recalibrated, focus matrix within 8 nm of the baseline, send-ahead CD on target."}},
	{[]string{"scanarf", "scankrf", "stepper"}, "stage_drift", "SCN-015", "Overlay Excursion Risk", "Stage Drift with Overlay Excursion (Compound)",
		[][]string{{"A:SCN-002", "P:overlay_residual"}}, []string{"overlay_residual", "stage_temp"},
		[]string{"Trend the stage overlay residual by stage (chuck 1 / chuck 2) and check whether one stage drifts alone.",
			"Read the overlay metrology results of the last lots (OVL tool) and compare the residual pattern with the stage grid.",
			"Check the interferometer mirror and stage temperature logs for a step change."},
		"Recalibrate the wafer stage interferometers", []string{"Run the stage grid and interferometer calibration on both chucks.",
			"Clean the stage mirror blocks if the calibration residual stays high.", "Expose and measure an overlay qualification wafer on both chucks."},
		[2]int{45, 240}, []string{"Chuck 2 residual 1.8 nm above chuck 1, grid signature in the overlay map.",
			"Interferometers recalibrated, mirror block cleaned. Overlay qualification 2.9 nm on both chucks."}},
	{[]string{"track"}, "nozzle_contamination", "TRK-012", "Resist Coating Defect Risk", "Coater Exhaust Loss with Dispense Contamination (Compound)",
		[][]string{{"A:TRK-002", "P:cup_exhaust"}}, []string{"cup_exhaust", "resist_temp"},
		[]string{"Check the coater cup exhaust damper and the exhaust duct for resist build-up.",
			"Inspect the dispense nozzle tip under the inspection light for dried resist.",
			"Coat a bare monitor wafer and run it on the defect inspection tool."},
		"Clean the coater cup and resist nozzle", []string{"Drain and clean the coater cup and the exhaust ring.", "Replace the resist nozzle and purge the dispense line.",
			"Coat and inspect a monitor wafer; coating defects below the limit."},
		[2]int{30, 120}, []string{"Cup exhaust 27 Pa, dried resist on the nozzle tip, 46 coating defects on the monitor wafer.",
			"Cup and exhaust ring cleaned, nozzle replaced. Exhaust 36 Pa, monitor wafer 4 defects."}},
	// --- Etch -----------------------------------------------------------------
	{[]string{"etch"}, "rf_match", "ETC-012", "Plasma Impedance Drift", "Etch Rate Loss with RF Mismatch (Compound)",
		[][]string{{"A:ETC-003", "P:rf_reflected"}}, []string{"endpoint_time", "rf_reflected", "rf_source_power"},
		[]string{"Read the match network load and tune capacitor positions per chamber; a capacitor at the end of its travel cannot tune.",
			"Compare the endpoint time of the chamber with the other chambers on the same recipe.",
			"Check the RF cable and connectors between generator and match for heat discolouration."},
		"Restore the RF delivery and requalify the etch rate", []string{"Replace the worn vacuum capacitor or RF cable and recalibrate the match network.",
			"Season the chamber and run the etch rate monitor.", "Check the CD of the first product lot after the release."},
		[2]int{40, 180}, []string{"PM2 endpoint 81 s against 63 s on PM1, tune capacitor at 97 %, reflected power 31 W.",
			"Tune capacitor replaced, match recalibrated. Etch rate monitor on baseline, reflected power 5 W."}},
	{[]string{"etch"}, "esc_leak", "ETC-013", "Wafer Backside Cooling Loss", "ESC Temperature and Helium Leak (Compound)",
		[][]string{{"A:ETC-002", "P:he_flow"}}, []string{"he_flow", "esc_temp"},
		[]string{"Run the helium leak-up test of the chuck and compare it with the qualification value.",
			"Check the CD and profile of the last lots for an edge-to-centre signature.", "Inspect the ESC surface for arcing marks and seal band wear."},
		"Replace the electrostatic chuck seal band", []string{"Vent the chamber, remove the process kit and the ESC.", "Replace the seal band (or the ESC) and refit with new O-rings.",
			"Pump down, run the leak-up test and season the chamber."},
		[2]int{35, 300}, []string{"He flow 18.4 sccm, leak-up rate 3.5 times the qualification value, ESC 43.2 °C.",
			"Seal band replaced. He flow 12.1 sccm, ESC 40.0 °C, CD monitor in specification."}},
	{[]string{"etchd"}, "rf_match", "ETD-012", "Dielectric Plasma Impedance Drift", "Dielectric Etch Rate Loss with RF Mismatch (Compound)",
		[][]string{{"A:ETD-003", "P:rf_reflected"}}, []string{"endpoint_time", "rf_reflected", "rf_hf_power", "rf_lf_power"},
		[]string{"Read the HF and LF match positions per chamber and the reflected power at the start of the main etch step.",
			"Compare contact etch endpoint times with the qualification lot.", "Check the bias RF cable and connectors."},
		"Restore the dual-frequency RF delivery", []string{"Replace the worn capacitor or cable and recalibrate both match networks.",
			"Season the chamber and run the oxide etch rate monitor.", "Check the contact CD on the first product lot."},
		[2]int{40, 180}, []string{"Endpoint 97 s, HF reflected power 33 W, LF match hunting at the step start.",
			"LF match capacitor replaced. Oxide etch rate monitor on baseline, reflected power 7 W."}},
	{[]string{"etchd"}, "focus_ring_wear", "ETD-013", "Edge Etch Rate Shift from Focus Ring Erosion", "Wall Temperature and Endpoint Drift (Compound)",
		[][]string{{"P:wall_temp", "P:endpoint_time"}}, []string{"wall_temp", "endpoint_time", "rf_hours"},
		[]string{"Measure the edge exclusion etch depth on the last monitor wafer against the centre.",
			"Read the focus ring RF hours and the last ring change from the chamber log.", "Check the wall temperature controller output."},
		"Replace the focus ring and edge ring", []string{"Vent the chamber and remove the process kit.", "Replace the focus ring and edge ring, clean the chamber liner.",
			"Season the chamber and run the edge uniformity monitor."},
		[2]int{30, 240}, []string{"Edge etch depth 9 % deeper than centre, focus ring at 410 RF hours, wall 65.1 °C.",
			"Focus and edge ring replaced, liner cleaned. Edge uniformity 2.1 %, wall 60.2 °C."}},
	// --- Deposition -------------------------------------------------------------
	{[]string{"cvd"}, "showerhead_clog", "CVD-012", "Film Thickness Non-uniformity Risk", "Deposition Rate Loss with Film Build-up (Compound)",
		[][]string{{"P:dep_rate", "P:film_buildup"}}, []string{"dep_rate", "film_buildup", "teos_flow"},
		[]string{"Measure the thickness map of the last monitor wafer; a thin centre points to blocked showerhead holes.",
			"Check the TEOS liquid flow controller reading against the set point.", "Read the RF hours since the last remote plasma clean."},
		"Clean the chamber and exchange the showerhead", []string{"Run an extended remote plasma clean.", "Exchange the showerhead if the thickness map keeps the centre-thin signature.",
			"Season the chamber and run the deposition rate and uniformity monitors."},
		[2]int{40, 240}, []string{"Thickness map centre-thin, uniformity 3.4 %, film build-up 11.6 µm.",
			"Showerhead exchanged after an extended clean. Deposition rate 5.00 nm/s, uniformity 1.0 %."}},
	{[]string{"cvd"}, "heater_drift", "CVD-013", "Film Property Drift from Heater Temperature", "Heater Deviation with Deposition Rate Loss (Compound)",
		[][]string{{"A:CVD-001", "P:dep_rate"}}, []string{"heater_temp", "dep_rate"},
		[]string{"Compare the controller thermocouple with the redundant thermocouple of the heater.", "Measure refractive index and stress on the last monitor wafer.",
			"Check the heater power output trend for a step change."},
		"Replace the heater thermocouple and retune the loop", []string{"Cool the heater to the safe temperature and vent the chamber.", "Replace the controller thermocouple and retune the heater loop.",
			"Run the film thickness, index and stress monitors."},
		[2]int{30, 150}, []string{"Controller thermocouple 7 K above the redundant one, film index 1.462 against 1.458.",
			"Thermocouple replaced, loop retuned. Thickness 501 nm, index 1.458."}},
	{[]string{"pvd"}, "cryo_degradation", "PVD-012", "Film Oxidation Risk from High Base Pressure", "Base Pressure Rise with Cryopump Alarm (Compound)",
		[][]string{{"A:PVD-001", "P:base_pressure"}}, []string{"base_pressure", "chamber_pressure"},
		[]string{"Read the cryopump second stage temperature and the time since the last regeneration.", "Run a rate-of-rise test of the chamber to exclude a leak.",
			"Check the sheet resistance of the last barrier monitor wafer."},
		"Regenerate or exchange the cryopump", []string{"Run a full cryopump regeneration.", "Exchange the cryopump if the second stage stays above 15 K.",
			"Pump down, check the base pressure and run the sheet resistance monitor."},
		[2]int{35, 200}, []string{"Base pressure 5.8 µTorr, second stage 17 K, rate of rise normal.",
			"Cryopump exchanged. Base pressure 0.6 µTorr, barrier Rs in specification."}},
	{[]string{"pvd"}, "shield_flaking", "PVD-013", "Particle Excursion Risk from Shield Flaking", "Deposition Rate Loss with Target Wear (Compound)",
		[][]string{{"A:PVD-004", "P:target_life"}}, []string{"dep_rate", "target_life", "dc_power"},
		[]string{"Run a particle monitor wafer through the chamber.", "Inspect the shields through the viewport for flaking deposits.",
			"Read the target kWh and the shield kit age from the chamber log."},
		"Exchange the target and shield kit", []string{"Vent the chamber and remove the shield kit and the target.", "Install the new target and a cleaned shield kit.",
			"Burn in the target and run the particle and thickness monitors."},
		[2]int{30, 300}, []string{"Particle monitor 38 adders, shield deposits flaking at the lower shield, target at 93 %.",
			"Target and shields exchanged, burn-in done. Particle monitor 4 adders, rate 12.1 Å/s."}},
	{[]string{"ecp"}, "additive_depletion", "ECP-012", "Void Formation Risk from Additive Depletion", "Accelerator Loss with Bath Alarm (Compound)",
		[][]string{{"A:ECP-001", "P:accelerator"}}, []string{"accelerator", "plating_current", "cu_conc"},
		[]string{"Run the CVS bath analysis for accelerator, suppressor and leveler.", "Check the dosing pump stroke count against the plated amp-hours.",
			"Cross-section a via chain on the last monitor wafer for voids."},
		"Re-dose the plating bath and repair the dosing", []string{"Correct the accelerator level with a manual dose to the target.", "Repair or recalibrate the dosing pump.",
			"Plate a fill monitor wafer and check the via cross-section."},
		[2]int{30, 120}, []string{"CVS accelerator 4.3 ml/L, dosing pump delivering 70 % of its stroke volume.",
			"Bath re-dosed, dosing pump diaphragm replaced. Accelerator 6.0 ml/L, fill monitor void-free."}},
	{[]string{"ecp"}, "anode_passivation", "ECP-013", "Plating Thickness Shortfall", "Plating Current Loss with Anode Wear (Compound)",
		[][]string{{"P:plating_current", "P:anode_life"}}, []string{"plating_current", "anode_life"},
		[]string{"Read the cell voltage at the plating current; a rising voltage means a passivated anode.", "Measure the Cu thickness on the last monitor wafer.",
			"Inspect the anode chamber membrane for film."},
		"Replace the anode and clean the anode chamber", []string{"Drain the anode chamber and remove the anode.", "Install a new anode and flush the membrane.",
			"Plate and measure a thickness monitor wafer."},
		[2]int{30, 180}, []string{"Cell voltage 18 % above baseline, Cu thickness 6 % low, black film on the anode.",
			"Anode replaced, membrane flushed. Plating current 12.0 A, thickness on target."}},
	// --- CMP, diffusion, implant ---------------------------------------------------
	{[]string{"cmp"}, "pad_glazing", "CMP-012", "Dishing / Under-polish Risk", "Removal Rate Loss with Pad Temperature Rise (Compound)",
		[][]string{{"A:CMP-001", "P:pad_temp"}}, []string{"removal_rate", "pad_temp", "pad_life"},
		[]string{"Inspect the pad surface for glazing and check the conditioner disk wear.", "Measure the post-polish thickness range on the last monitor wafer.",
			"Check the conditioner down force and sweep logs."},
		"Replace the pad and conditioner disk", []string{"Replace the polishing pad and the conditioner disk.", "Break in the new pad with the conditioning recipe.",
			"Polish and measure a removal rate monitor wafer."},
		[2]int{30, 90}, []string{"Pad glazed, conditioner disk worn, removal rate 226 nm/min, pad 53 °C.",
			"Pad and disk replaced, break-in done. Removal rate 301 nm/min, pad 42 °C."}},
	{[]string{"cmp"}, "slurry_flow_low", "CMP-013", "Scratch Defect Risk from Slurry Starvation", "Slurry Flow Loss with Removal Rate Drop (Compound)",
		[][]string{{"P:slurry_flow", "P:removal_rate"}}, []string{"slurry_flow", "removal_rate"},
		[]string{"Check the slurry flow controller and the point-of-use filter differential pressure.", "Inspect the slurry arm nozzle for dried slurry.",
			"Run a defect scan on the last polished monitor wafer for scratches."},
		"Restore the slurry delivery", []string{"Replace the point-of-use filter and flush the slurry line.", "Clean the slurry arm nozzle and recalibrate the flow controller.",
			"Polish a monitor wafer and run the scratch inspection."},
		[2]int{25, 75}, []string{"Slurry flow 148 ml/min, POU filter DP twice the clean value, 12 scratches on the monitor.",
			"Filter replaced, line flushed. Slurry flow 201 ml/min, monitor scratch-free."}},
	{[]string{"furn"}, "heater_zone_drift", "FRN-012", "Oxide Thickness Non-uniformity", "Zone Temperature Spread Escalation (Compound)",
		[][]string{{"A:FRN-001", "P:temp_uniformity"}}, []string{"temp_uniformity", "tube_temp"},
		[]string{"Read the profile thermocouple against the spike thermocouples of each zone.", "Measure the oxide thickness of the top, centre and bottom monitor wafers.",
			"Check the zone heater power outputs for a zone at its limit."},
		"Recalibrate the furnace zone thermocouples", []string{"Run the profile with the calibrated profile thermocouple.", "Replace the drifting spike thermocouple and update the zone offsets.",
			"Run a thickness monitor boat."},
		[2]int{40, 240}, []string{"Zone 3 spike thermocouple 2.6 K off the profile, bottom wafers 1.5 nm thin.",
			"Spike thermocouple replaced, offsets updated. Spread 0.7 K, thickness range 0.6 nm."}},
	{[]string{"furn"}, "particle_flaking", "FRN-013", "Monitor Wafer Particle Excursion", "Particle Adders on Monitor Wafers",
		[][]string{{"P:monitor_adders"}}, []string{"monitor_adders", "tube_deposit"},
		[]string{"Map the adders of the monitor wafers by boat slot; adders near the tube ends point to flaking deposits.",
			"Inspect the boat and the pedestal for flakes.", "Read the tube deposit thickness and the last tube clean date."},
		"Clean or exchange the tube and boat", []string{"Exchange the boat for a cleaned one.", "Clean the tube (or exchange it if the deposit is above the limit).",
			"Run a particle monitor boat."},
		[2]int{30, 360}, []string{"28 adders on the top slots, flakes on the boat rods.",
			"Boat exchanged, tube cleaned. Monitor boat 3 adders per wafer."}},
	{[]string{"rtp"}, "pyrometer_drift", "RTP-012", "Anneal Temperature Off Target", "Pyrometer Offset Escalation (Compound)",
		[][]string{{"A:RTP-001", "P:pyro_offset"}}, []string{"pyro_offset", "peak_temp"},
		[]string{"Run the TC wafer calibration and compare the pyrometer reading with the thermocouple wafer.", "Check the sheet resistance of the last anneal monitor.",
			"Inspect the pyrometer window for coating."},
		"Recalibrate the pyrometer", []string{"Clean the pyrometer window.", "Recalibrate the pyrometer against the TC wafer.", "Run the sheet resistance monitor."},
		[2]int{30, 120}, []string{"Pyrometer 6.8 K off the TC wafer, window coated, Rs 3 % low.",
			"Window cleaned, pyrometer recalibrated. Offset 0.4 K, Rs on target."}},
	{[]string{"rtp"}, "lamp_aging", "RTP-013", "Ramp Capability Loss from Lamp Aging", "Lamp Power Rise with Lamp Alarm (Compound)",
		[][]string{{"A:RTP-003", "P:lamp_power"}}, []string{"lamp_power", "ramp_rate"},
		[]string{"Read the lamp zone powers and the number of failed lamps from the tool log.", "Check the ramp rate of the last spike anneal recipes.",
			"Run the lamp zone test."},
		"Replace the aged lamps", []string{"Replace the failed and weak lamps.", "Run the lamp zone balance.", "Run the anneal monitor."},
		[2]int{25, 150}, []string{"Lamp power 102 kW, 7 lamps below 80 % output.", "Lamps replaced, zones balanced. Lamp power 86 kW."}},
	{[]string{"impmc", "imphc"}, "source_degradation", "IMP-012", "Ion Source End of Life", "Beam Current Loss with Source Hours (Compound)",
		[][]string{{"A:IMP-001", "P:source_hours"}}, []string{"beam_current", "source_hours", "arc_current"},
		[]string{"Read the arc voltage and filament current trend of the source.", "Check the beam current at the standard recipe against the qualification value.",
			"Check the source hours since the last source change."},
		"Change the ion source", []string{"Vent the source housing and exchange the source.", "Bake out and condition the new source.", "Tune the beam and run the dose monitor."},
		[2]int{25, 240}, []string{"Beam current 12 % low, filament current at its limit, source 335 h.",
			"Source exchanged and conditioned. Beam current on qualification, dose monitor in specification."}},
	{[]string{"impmc", "imphc"}, "dose_drift", "IMP-013", "Sheet Resistance Shift Risk from Dose Drift", "Dose Uniformity Escalation (Compound)",
		[][]string{{"A:IMP-002", "P:dose_uniformity"}}, []string{"dose_uniformity", "beam_current"},
		[]string{"Run the Faraday cup calibration and compare with the reference.", "Measure the sheet resistance map of the last dose monitor wafer.",
			"Check the scan waveform and the beam profile."},
		"Recalibrate the dose control", []string{"Clean the Faraday cups.", "Recalibrate the dose control and the scan waveform.", "Run a dose monitor wafer and check the Rs map."},
		[2]int{30, 150}, []string{"Dose uniformity 1.6 %, Faraday cup 2 reading 4 % low.",
			"Faraday cups cleaned, dose recalibrated. Uniformity 0.6 %."}},
	// --- Cleans, strip, metrology, test ----------------------------------------------
	{[]string{"wet"}, "filter_clog", "WET-011", "Particle Excursion on Product Wafers", "Bath Particle Alarm with Count Rise (Compound)",
		[][]string{{"A:WET-003", "P:particles"}}, []string{"particles", "bath_age"},
		[]string{"Read the recirculation filter differential pressure.", "Run a particle monitor wafer through the bench.", "Check the bath age against the change interval."},
		"Replace the recirculation filter and change the bath", []string{"Replace the recirculation filter cartridge.", "Drain, rinse and refill the bath.",
			"Recirculate and run a particle monitor wafer."},
		[2]int{25, 90}, []string{"Filter DP 2.4 times clean, 44 particles per ml, monitor wafer 31 adders.",
			"Filter replaced, bath changed. 5 particles per ml, monitor 3 adders."}},
	{[]string{"swc"}, "nozzle_clog", "SWC-011", "Incomplete Clean Risk from Chemical Starvation", "Chemical Flow Loss with Particle Rise (Compound)",
		[][]string{{"A:SWC-001", "P:particles"}}, []string{"chem_flow", "particles"},
		[]string{"Check the dispense nozzle and the point-of-use filter of the chamber.", "Run a particle monitor wafer in each chamber.", "Check the chemical flow per chamber."},
		"Clear the dispense nozzle and replace the filter", []string{"Replace the point-of-use filter.", "Clean or replace the dispense nozzle.", "Run the particle monitor in the chamber."},
		[2]int{25, 90}, []string{"CH3 flow 1.05 l/min, nozzle partly blocked, 33 particles per ml.",
			"Nozzle and filter replaced. Flow 1.48 l/min, monitor 2 adders."}},
	{[]string{"ash"}, "strip_rate_low", "ASH-011", "Resist Residue Risk", "Strip Rate Loss Escalation (Compound)",
		[][]string{{"A:ASH-001", "P:strip_rate"}}, []string{"strip_rate", "rf_power", "o2_flow"},
		[]string{"Run the strip rate monitor per chamber.", "Inspect the plasma source tube for coating.", "Check the O2 flow controller against the set point."},
		"Service the plasma source", []string{"Clean or replace the source quartz tube.", "Recalibrate the O2 mass flow controller.", "Run the strip rate monitor."},
		[2]int{25, 150}, []string{"Strip rate 2.9 µm/min, quartz tube coated.", "Quartz tube replaced. Strip rate 3.9 µm/min."}},
	{[]string{"met"}, "lamp_aging", "MET-011", "Measurement Repeatability Loss", "Lamp Intensity Escalation (Compound)",
		[][]string{{"A:MET-001", "P:lamp_intensity"}}, []string{"lamp_intensity", "measure_time"},
		[]string{"Measure the reference wafer 10 times and check the repeatability.", "Read the lamp hours.", "Compare the spectra with the qualification spectra."},
		"Replace the metrology lamp", []string{"Replace and align the lamp module.", "Run the system calibration.", "Match the reference wafer set."},
		[2]int{20, 90}, []string{"Lamp at 3,050 h, intensity 73 %, repeatability 3 times the specification.", "Lamp replaced and aligned. Intensity 99 %, reference matched."}},
	{[]string{"cds"}, "emitter_aging", "CDS-011", "CD Measurement Repeatability Loss", "Resolution Loss Escalation (Compound)",
		[][]string{{"A:CDS-001", "P:resolution"}}, []string{"resolution", "beam_current"},
		[]string{"Measure the pitch standard and check the resolution and repeatability.", "Read the emitter current and the emitter hours.", "Check the column alignment."},
		"Replace the field emitter", []string{"Vent the gun and replace the field emitter.", "Bake out and align the column.", "Run the pitch standard and the matching wafer."},
		[2]int{25, 300}, []string{"Resolution 2.9 nm, emitter current unstable.", "Emitter replaced, column aligned. Resolution 2.0 nm, matching in specification."}},
	{[]string{"ovl"}, "lamp_aging", "OVL-011", "Overlay Measurement Bias", "Tool-Induced Shift Escalation (Compound)",
		[][]string{{"A:OVL-002", "P:tis"}}, []string{"tis", "lamp_intensity"},
		[]string{"Measure the TIS wafer at 0° and 180°.", "Read the lamp intensity and hours.", "Compare the overlay of a reference lot with the matched tool."},
		"Replace the lamp and recalibrate TIS", []string{"Replace the lamp module.", "Run the optics and TIS calibration.", "Measure the reference lot."},
		[2]int{20, 90}, []string{"TIS 1.1 nm, lamp at 76 %.", "Lamp replaced, TIS 0.3 nm."}},
	{[]string{"def"}, "laser_degradation", "DEF-011", "Defect Capture Rate Loss", "Sensitivity Loss with Laser Power Drop (Compound)",
		[][]string{{"A:DEF-002", "P:laser_power"}}, []string{"sensitivity", "laser_power"},
		[]string{"Scan the PSL standard wafer and compare the capture rate.", "Read the laser power and the laser hours.", "Check the collection optics for contamination."},
		"Replace the inspection laser", []string{"Replace the laser module.", "Align the optics.", "Scan the PSL standard wafer."},
		[2]int{25, 180}, []string{"Capture rate 78 % on the PSL standard, laser power 1.12 W.", "Laser replaced, capture rate 97 %."}},
	{[]string{"wat"}, "probe_card_wear", "WAT-011", "False Fail Risk from Probe Contact", "Contact Resistance Escalation (Compound)",
		[][]string{{"A:WAT-001", "P:contact_res"}}, []string{"contact_res", "touchdowns"},
		[]string{"Check the contact resistance map of the probe card.", "Inspect the probe tips under the microscope.", "Re-test a failing wafer after a card clean."},
		"Clean or replace the probe card", []string{"Clean the probe card on the cleaning pad.", "Replace the card if the contact resistance stays high.", "Re-test the reference wafer."},
		[2]int{20, 60}, []string{"Contact resistance 3.2 Ω on 6 pins, tips contaminated.", "Card cleaned, 2 tips replanarised. Contact resistance 1.6 Ω."}},
	// --- Sub-fab support ------------------------------------------------------------
	{[]string{"vac"}, "deposition", "VAC-013", "Dry Pump Seizure Risk", "Motor Current with Body Temperature Rise (Compound)",
		[][]string{{"A:VAC-001", "P:body_temp"}}, []string{"dp_current", "body_temp", "exhaust_press"},
		[]string{"Check the exhaust line heater and the exhaust pressure.", "Listen to the pump for rubbing noise.", "Check the process chamber pressure of the served tool."},
		"Exchange the dry pump before it seizes", []string{"Schedule the exchange with the tool owner.", "Exchange the pump with a refurbished spare.", "Check the base pressure and the chamber pressure of the tool."},
		[2]int{20, 120}, []string{"Motor current 13.6 A, body 108 °C, exhaust line heater off.", "Pump exchanged, exhaust heater repaired. Current 9.1 A, body 86 °C."}},
	{[]string{"vac"}, "rotor_wear", "VAC-014", "Process Chamber Base Pressure Risk", "Inlet Pressure Alarm with Booster Current Rise (Compound)",
		[][]string{{"A:VAC-003", "P:booster_current"}}, []string{"inlet_press", "booster_current"},
		[]string{"Read the base pressure of the pump with the inlet valve closed.", "Compare the booster current with a pump of the same model.", "Check the chamber pump-down time of the served tool."},
		"Exchange the worn dry pump", []string{"Exchange the pump with a refurbished spare.", "Send the worn pump for a rotor overhaul.", "Check the chamber pump-down time."},
		[2]int{20, 120}, []string{"Base pressure 2.8 times the baseline, booster 4.9 A.", "Pump exchanged. Base pressure 0.011 mbar, pump-down time normal."}},
	{[]string{"pch"}, "refrigerant_low", "PCH-012", "Tool Cooling Capacity Shortfall", "Coolant Temperature Alarm with Compressor Current (Compound)",
		[][]string{{"A:PCH-001", "P:comp_current"}}, []string{"supply_temp", "comp_current", "heat_load"},
		[]string{"Read suction and discharge pressures and the superheat.", "Leak-test the refrigerant circuit.", "Check the coolant supply temperature at the served tools."},
		"Repair the refrigerant leak and recharge", []string{"Repair the leak.", "Evacuate and recharge to the nameplate charge.", "Confirm the coolant supply temperature at full heat load."},
		[2]int{30, 180}, []string{"Superheat 14 K, leak at the condenser flare joint.", "Joint brazed, circuit recharged. Supply 20.0 °C at full load."}},
	// --- Facility -------------------------------------------------------------------
	{[]string{"chiller"}, "cond_fouling", "CHL-012", "Chiller Efficiency Loss from Condenser Fouling", "Condenser Approach with Efficiency Loss (Compound)",
		[][]string{{"P:cond_approach", "P:efficiency"}}, []string{"cond_approach", "efficiency", "cond_press"},
		[]string{"Trend condenser approach and kW/RT against load for 14 days.", "Check condenser water flow and the tower supply temperature.",
			"Compare kW/RT with the other chillers at the same load."},
		"Clean the condenser tubes", []string{"Transfer duty to the standby chiller.", "Open the condenser water boxes and brush clean the tubes.",
			"Restart and confirm the condenser approach and kW/RT."},
		[2]int{40, 240}, []string{"Condenser approach 3.8 K, 0.74 kW/RT against 0.61 on CH-02 at the same load.",
			"Tubes brushed. Approach 1.3 K, 0.62 kW/RT."}},
	{[]string{"chiller"}, "refrigerant_leak", "CHL-013", "Capacity Loss from Low Refrigerant Charge", "CHW Supply Deviation with High Superheat (Compound)",
		[][]string{{"A:CHL-008", "P:superheat"}}, []string{"chwst", "superheat", "evap_press"},
		[]string{"Check the sight glass and the suction superheat.", "Leak-test the chiller with the electronic detector.", "Check the CHW supply against the active set point at full load."},
		"Repair the leak and recharge refrigerant", []string{"Repair the leak.", "Pressure test, evacuate and recharge.", "Confirm superheat and CHW supply on set point at full load."},
		[2]int{40, 300}, []string{"Superheat 5.6 K, CHW supply 1.3 K above set point, leak at the relief valve flange.",
			"Flange gasket replaced, 40 kg R-134a added. Superheat 0.8 K, supply on set point."}},
	{[]string{"chiller"}, "bearing_wear", "CHL-014", "Compressor Mechanical Failure Risk", "Vibration with Oil Temperature Rise (Compound)",
		[][]string{{"P:vibration", "P:oil_temp"}}, []string{"vibration", "oil_temp", "oil_dp"},
		[]string{"Take a vibration spectrum and look for bearing defect frequencies.", "Take an oil sample for wear metals.", "Check the oil cooler and the oil filter DP."},
		"Replace the compressor bearings", []string{"Schedule the OEM engineer and isolate the chiller.", "Replace the bearings and the oil.", "Run in and record the vibration spectrum."},
		[2]int{45, 480}, []string{"Vibration 5.2 mm/s with outer race frequency, oil 61 °C, iron 42 ppm.",
			"Bearings replaced, oil changed. Vibration 1.8 mm/s, oil 49 °C."}},
	{[]string{"mau"}, "coil_fouling", "AHU-013", "Cleanroom Humidity Excursion Risk", "Supply Air Temperature and Humidity with Valve Saturation (Compound)",
		[][]string{{"A:AHU-004", "A:AHU-003", "P:chw_valve"}}, []string{"sa_rh", "chw_valve", "sat"},
		[]string{"Check the coil face and the drain pan.", "Stroke the CHW valve actuator from the BMS.", "Compare the supply air RH with the other MAUs."},
		"Clean the cooling coil and repair the valve", []string{"Clean the coil with the coil cleaner and rinse.", "Repair or replace the valve actuator.", "Confirm supply air temperature and RH on set point."},
		[2]int{30, 180}, []string{"Coil face fouled, valve at 100 %, supply RH 54 %.", "Coil cleaned, actuator replaced. RH 47 %, valve 72 %."}},
	{[]string{"mau"}, "filter_clog", "AHU-014", "Airflow Loss from Filter Loading", "Filter DP with Fan Speed Saturation (Compound)",
		[][]string{{"P:prefilter_dp", "P:fan_speed"}}, []string{"prefilter_dp", "fan_speed", "airflow"},
		[]string{"Read the pre-filter and HEPA differential pressures.", "Check the fan speed and the airflow against design.", "Inspect the pre-filters."},
		"Replace the pre-filters", []string{"Replace the G4 / F7 pre-filters.", "Reset the filter timer.", "Confirm airflow at the design fan speed."},
		[2]int{20, 90}, []string{"Pre-filter DP 236 Pa, fan at 93 %.", "Pre-filters replaced. DP 95 Pa, fan 74 %."}},
	{[]string{"ct"}, "fill_scaling", "CT-015", "Condenser Water Temperature Excursion", "High CW Supply with Approach Rise (Compound)",
		[][]string{{"A:CT-001", "P:approach"}}, []string{"cwst", "approach", "conductivity"},
		[]string{"Inspect the fill for scale.", "Check the conductivity controller and the blowdown valve.", "Compare the cell approach with the other cells at the same fan speed."},
		"Descale the fill and repair the blowdown", []string{"Descale the fill packs.", "Repair the conductivity controller or blowdown valve.", "Confirm the cell approach."},
		[2]int{30, 300}, []string{"Approach 5.9 K, fill scaled, blowdown valve stuck closed.", "Fill descaled, valve replaced. Approach 3.3 K."}},
	{[]string{"ct"}, "makeup_valve", "CT-016", "Basin Low Level / Pump Suction Risk", "Basin Level Alarm with Level Drop (Compound)",
		[][]string{{"A:CT-004", "P:basin_level"}}, []string{"basin_level"},
		[]string{"Check the make-up float valve and the make-up water pressure.", "Check the basin for leaks.", "Check the CW pump suction pressure."},
		"Repair the make-up valve", []string{"Replace the float valve or its seat.", "Restore the make-up water supply.", "Confirm the basin level."},
		[2]int{20, 90}, []string{"Basin 38 %, float valve seat worn.", "Float valve replaced, basin 77 %."}},
	{[]string{"pcw", "chwp", "cwp"}, "cavitation", "PMP-013", "Impeller Cavitation Damage Risk", "Suction Pressure Alarm with Vibration (Compound)",
		[][]string{{"A:PMP-001", "P:vibration"}}, []string{"suction_press", "vibration"},
		[]string{"Read the suction strainer differential pressure.", "Listen for cavitation noise at the pump casing.", "Take a vibration reading at the pump bearings."},
		"Clean the suction strainer", []string{"Isolate the pump and clean the suction strainer.", "Vent the pump casing.", "Confirm suction pressure and vibration."},
		[2]int{20, 90}, []string{"Strainer DP 0.5 bar, cavitation noise, vibration 5.1 mm/s.", "Strainer cleaned. Suction 2.0 bar, vibration 1.7 mm/s."}},
	{[]string{"pcw", "chwp", "cwp"}, "bearing_wear", "PMP-014", "Pump Seizure Risk", "Bearing Temperature Alarm with Vibration (Compound)",
		[][]string{{"A:PMP-003", "P:vibration"}}, []string{"bearing_temp", "vibration"},
		[]string{"Take a vibration spectrum at both bearings.", "Check the grease condition.", "Check the coupling alignment."},
		"Replace the pump bearings", []string{"Transfer duty to the standby pump.", "Replace both bearings and align the coupling.", "Confirm bearing temperature and vibration."},
		[2]int{25, 240}, []string{"DE bearing 87 °C, vibration 5.4 mm/s.", "Bearings replaced, coupling aligned. 52 °C, 1.5 mm/s."}},
	{[]string{"cda"}, "intercooler_fouling", "CDA-013", "Compressor Efficiency Loss", "Intercooler Alarm with HP Element Temperature (Compound)",
		[][]string{{"A:CDA-005", "P:hp_elem_temp"}}, []string{"intercooler_temp", "hp_elem_temp", "motor_current"},
		[]string{"Check the cooling water flow and temperature at the intercooler.", "Compare the specific power with the commissioning value.", "Inspect the intercooler water side."},
		"Descale the intercooler", []string{"Isolate and drain the intercooler.", "Descale the water side.", "Confirm the intercooler and HP element outlet temperatures."},
		[2]int{25, 240}, []string{"Intercooler outlet 46 °C, HP element 207 °C, water side scaled.", "Intercooler descaled. 34 °C, HP element 172 °C."}},
	{[]string{"cda"}, "dryer_fault", "CDA-014", "Moisture Carry-over Risk", "Dew Point Alarm with Dew Point Rise (Compound)",
		[][]string{{"A:CDA-003", "P:dew_point"}}, []string{"dew_point"},
		[]string{"Check the dryer regeneration heater and the cycle valves.", "Read the dew point at the sub-fab drops.", "Check the desiccant condition."},
		"Repair the desiccant dryer", []string{"Replace the failed heater or valve.", "Top up or replace the desiccant.", "Confirm the pressure dew point."},
		[2]int{25, 180}, []string{"Dew point -28 °C, regeneration heater open circuit.", "Heater replaced. Dew point -44 °C."}},
}

// diagByCode returns the extended diagnosis of a code, or nil.
func diagByCode(code string) *afddDiag {
	for i := range afddDiags {
		if afddDiags[i].Code == code {
			return &afddDiags[i]
		}
	}
	return nil
}

// addDiagnoses adds the extended diagnoses to the models of the plant
// definition (relation, alarm mapping, repair code of the fault).
func addDiagnoses(ms []*ModelSpec) []*ModelSpec {
	for _, m := range ms {
		if m.IsBase {
			continue
		}
		for _, d := range afddDiags {
			if !contains(d.Keys, m.Key) {
				continue
			}
			m.Relations = append(m.Relations, RelationSpec{Name: d.Rel, Target: d.Code, Paths: d.Paths})
			if m.Mapping != nil {
				m.Mapping[d.Code] = d.Params
			}
			for i := range m.Faults {
				if m.Faults[i].Kind == d.Fault {
					m.Faults[i].Codes = append(m.Faults[i].Codes, d.Code)
				}
			}
		}
	}
	return ms
}

// diagInstructions writes the diagnostic, repair and severity SOPs of the
// extended diagnoses.
func diagInstructions(paramName func(key, alias string) string) []InstrSpec {
	var out []InstrSpec
	for _, d := range afddDiags {
		tool := toolDef(d.Keys[0]) != nil || d.Keys[0] == "track"
		var symptoms, names []string
		for _, in := range d.Paths[0] {
			if strings.HasPrefix(in, "A:") {
				symptoms = append(symptoms, "alarm "+in[2:]+" ("+codeName(in[2:])+")")
			} else {
				symptoms = append(symptoms, "abnormal "+paramName(d.Keys[0], in[2:]))
			}
		}
		for _, p := range d.Params {
			names = append(names, paramName(d.Keys[0], p))
		}
		unit, safety, last := "unit", withSafety, "Return the unit to automatic control and confirm that the alarm and the abnormal parameters have cleared."
		if tool {
			unit, safety, last = "tool", toolSafety, release
		}
		intro := "The issue relation '" + d.Rel + "' raises this diagnosis when " + strings.Join(symptoms, " and ") +
			" occur together. It is a later stage of the degradation than a single symptom and calls for planned repair."
		check := append([]string{"Open the early warning on the Early Warning & Diagnostics page and trend " + strings.Join(names, ", ") + " for 7 days."}, d.Check...)
		check = append(check, "Mark the diagnosis valid in the work report when the findings confirm it; otherwise mark it invalid and record what was found.")
		out = append(out,
			InstrSpec{d.Code, 1, "Confirm " + strings.ToLower(d.Name[:1]) + d.Name[1:], d.Minutes[0],
				"Confirm the diagnosis from " + strings.Join(names, ", ") + " before the repair is planned.", steps(intro, check...)},
			InstrSpec{d.Code, 2, d.Repair, d.Minutes[1], "Correct the root cause and verify the " + unit + " afterwards.",
				safety(steps("", append(append([]string{}, d.Fix...), last)...))},
			InstrSpec{d.Code, 3, "Severity assessment - " + d.Name, 15, "Decide whether the " + unit + " can stay in service until the repair.",
				steps("Use the severity of the early warning (WARNING / CRITICAL) and the trend of the symptoms.",
					"WARNING: keep the "+unit+" in service and plan the repair within 7 days or at the next PM window.",
					"CRITICAL, or a symptom still rising after 24 h: plan the repair within 24 h"+map[bool]string{true: " and put the lots of the affected chambers on hold for engineering review.", false: " and start the standby unit."}[tool],
					"Record the decision and the reason in the work report.")},
		)
	}
	return out
}

func init() {
	for _, d := range afddDiags {
		alarmCodes = append(alarmCodes, [2]string{d.Code, d.Name})
		workNotes[d.Code] = d.Notes
	}
	names := map[string]map[string]string{}
	for _, m := range models() {
		if names[m.Key] == nil {
			names[m.Key] = map[string]string{}
		}
		for _, p := range m.Params {
			names[m.Key][p.Alias] = p.Name
		}
	}
	instructions = append(instructions, diagInstructions(func(key, alias string) string {
		if n := names[key][alias]; n != "" {
			return n
		}
		return alias
	})...)
}
