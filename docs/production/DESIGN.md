# FAB1 production simulation, monitoring and planning: design and contracts

Status: implementation contract, revised 2026-10-13 (full-flow fab, seed version 9, history
version 10). FAB1 is a scaled-down 300 mm logic fab running the complete 40 nm front-end and
back-end-of-line flow of two products on 31 process tools in five cleanroom bays, with the dry
pumps, scrubbers and process chillers that support them in the sub-fab. A discrete-event
production model runs inside the plant simulator (live, in the generated history and in
planning simulations); the **Production** page monitors output, WIP, cycle time, quality and SEMI
E10 / E79 OEE; the **Simulation** page and the AI agent run production planning what-ifs.

Names, columns, endpoints and JSON shapes below are contracts. The code is the reference for
every number not listed here (`cmd/simulator/fab*.go`, `tools.go`, `simprod.go`).

---

## 0. Decisions

| Topic | Decision | Why |
|---|---|---|
| Scope | The whole wafer flow: substrate to wafer acceptance test (WAT), every layer of the products' process (wells, isolation, gate, implants, silicide, contacts, 6 / 8 metal levels, passivation, pads) | A lot really takes weeks and visits the same tools many times ("re-entrant flow"); the earlier 2-3 layer module did not show that |
| Scale | About a tenth of a production fab: 4.5 lots (112 wafers) started per day, 31 tools, one tool per type where a real fab has 5-50; the flow is condensed to ~250-300 steps (a real 40 nm flow has 500-700) | Small enough for a demo, the same structure and dynamics as the real thing |
| Products | Two real product classes on one 40 nm low-power CMOS platform (section 1) | The user asked for real existing products |
| Time states | Equipment status intervals (`ts.asset_status`) are the single source of time states (SEMI E10: Running productive, Idle standby, Planned down scheduled, Unplanned down unscheduled, Offline non-scheduled) | Same numbers as Equipment Overview and the twin |
| Support coupling | A process tool needs its dry pump, local scrubber and process chiller (section 2); a support unit down takes its tools down (unscheduled, or scheduled when the unit is in planned maintenance) | The vacuum / abatement / cooling chain is visible in 3D and in the data |
| Persistence | The simulator writes `prod.*` directly; the backend only reads them; the fab state (WIP, chambers, PM counters, holds) is saved with the simulator state | Same pattern as the rest of the demo plant |
| History | `historyVersion` 11 regenerates everything (60 days after a 42-day warm-up during which tools are treated as up); approved by the user (v10: planned cycle times; v11: simulated overhead transport) | One coherent dataset |
| Transport | The overhead hoist transport (OHT) is simulated (section 2a): a lot reaches its tool when a vehicle has carried its FOUP there, and the 3D twin shows exactly those vehicles and FOUPs | The user asked for vehicle movement driven by the simulator state, not a scripted loop |
| Permissions | Group **`production`** (`read`); planning jobs need `simulation` and are human-submitted (the agent can only propose) | Read-only monitoring, human in the loop |

---

## 1. Products and route

| Product | Model of | Technology | Mix | Planned cycle time | Die | Steps / masks |
|---|---|---|---|---|---|---|
| `MCU40` | 40 nm low-power microcontroller (Cortex-M0+ class, cf. Raspberry Pi RP2040) | 40 nm LP CMOS, 1P6M Cu / low-k, AlCu pads | 60 % | 26 days | 2 mm² | 254 / 33 (ArF 12, KrF 12, i-line 9) |
| `WSOC40` | 40 nm Wi-Fi / Bluetooth SoC (cf. Espressif ESP32 class) | 1P8M Cu / low-k with 2 thick RF metals, MIM capacitor, deep n-well | 40 % | 30 days | 21 mm² | 294 / 39 (ArF 14, KrF 15, i-line 10) |

Lot = 25 wafers, code `<product>-<YYMMDD>-<seq3>`; 5 % hot lots (priority 1). The route
(`fabroute.go: route40`) is built per module and layer: FEOL (pad oxide, STI, wells, gate oxides,
poly gate, LDD / halo, spacer, source / drain, salicide), MOL (contact etch stop, PMD, W contacts)
and BEOL (Cu damascene per metal: IMD dep, litho, etch, clean, barrier / seed, plating, CMP;
passivation and pad opening), then the final defect inspection and WAT. A photo layer is coat →
expose → develop → overlay / CD metrology on the track + exposure tool of its class (ArF
immersion for critical layers, KrF for semi-critical, i-line for wells, implants and pads).
Measurement steps carry a characteristic key (`<layer>.CDR`, `.OVL`, `.CDE`, `.THK`, `.CMP`, `.RS`,
`DEFECTS`, `WAT`) with target / LSL / USL and a sampling plan; Q-time limits guard the
time-critical sequences (pad oxide → nitride 8 h, liner 6 h, gate oxide → poly 4 h, salicide 2 h,
barrier / seed → plating 4 h).

**Release**: exponential inter-arrival around `starts_per_day` (clamped), while WIP < `wip_cap`.
**Dispatch**: FIFO by priority, or critical ratio (`CR`, due-date driven). **Batching**: furnaces
start with `furnace_min_lots` lots of a recipe or when the first waited `furnace_wait_h`; wet
benches pair 2 lots (the first waits up to 20 min).

---

## 2. Equipment (simulator `tools.go`, `fabplant.go`)

| Area (bay) | Tools | Class |
|---|---|---|
| Photo | `TRK_01..03` coat / develop tracks linked in-line to `SCN_01` (ArF immersion), `SCN_02` (KrF), `STP_01` (i-line) | litho cell |
| Etch & thin film | `ETCH_01` ICP conductor etch, `ETCH_02/03` CCP dielectric etch, `CVD_01/02` PECVD, `PVD_01` sputter (3-4 chambers each) | cluster |
| Wet & metrology | `WET_01/02` batch wet benches, `SWC_01` single-wafer clean, `MET_01` optical film / OCD, `CDS_01` CD-SEM, `OVL_01` overlay, `DEF_01` darkfield inspection, `WAT_01` parametric tester | batch / cluster / metrology |
| Diffusion | `FRN_01/02` oxidation / anneal, `FRN_03` LPCVD nitride / poly, `RTP_01` RTP, `ASH_01` plasma asher | batch / cluster |
| Implant & CMP | `IMP_01` medium current, `IMP_02` high current implanters, `CMP_01` oxide / W, `CMP_02/03` Cu CMP, `ECP_01` Cu plating | cluster |

Every tool model has `throughput_wph`, `wafers_today`, `queue_lots`, `lot_progress` and the status
booleans, plus its process parameters, alarms, issue relations (every fault reaches its diagnosis),
degradation faults that move its parameters and its product characteristics, trips, chamber cleans
/ source and lamp lifetimes / pad and target life, daily monitor or calibration runs and monthly
PMs through the CMMS.

Sub-fab support units: dry pumps `DP_01..10` (iXH1220H for etch / asher, iXH610 for CVD / LPCVD,
iXL750 clean duty for PVD and the implanters), local scrubbers `SCR_01..03` (burn-wet), process
chillers `PCH_01..03` (PCW-cooled). `toolSupport` maps each vacuum / cooled tool to its pump,
scrubber and chiller (e.g. `ETCH_01` → `DP_01`, `SCR_01`, `PCH_01`; `IMP_01` → `DP_08`, `SCR_03`,
`PCH_03`).

---

## 3. Quality model and dispositions

Each measured characteristic is the sum of tool-, chamber- and lot-level contributions (scanner
focus / dose / stage drift, etch chamber state, film build-up, CMP pad wear, furnace zone spread,
...) plus noise; litho errors are lot-level. Every step also accumulates a risk that sets the WAT
score (sites in spec, LSL 92 %). Out of spec:

| Disposition | When | Effect |
|---|---|---|
| `REWORK_LITHO` | resist CD / overlay out of spec | strip (asher) + clean, back to coat; after 2 reworks the out-of-spec wafers are scrapped |
| `REDEP`, `CMP` | film thickness / CMP removal out of spec, reworkable | strip and redeposit / touch-up polish detour |
| `RECLEAN` | defect adders > 30 | back to the clean step once, then scrap the worst wafers |
| `HOLD` | engineering characteristic out of spec | engineering hold (3 h, released 08:00-20:00, at most 24 h) |
| `SCRAP` | etch CD / WAT out of spec | wafers scrapped |

Scrap reasons include the characteristic keys, `DEFECTS`, `BREAKAGE` (1 per 200,000 moves) and
`ROBOT_FAULT` (wafers in a chamber when the tool trips). 1 % of lots get a random engineering hold.

---

## 2a. Overhead transport (simulator `amhs.go`, network `fab1_amhs.json`)

- **Network** (generated by `blender/fab_twin_scripts/fab_layout.py` `amhs_network()`, embedded in the
  simulator, also written into the app's `digital-twin.fab1-layout.ts` as `FAB1_AMHS`): one-way. The
  interbay loop `IB` (south rail eastbound, north rail westbound, 72.3 m) and one spur per bay that
  leaves the loop on a diverge curve, passes over the load ports of both sides of the bay, turns at
  the bay end and merges back (22.6 m each), plus STK-01's 5.3 m bypass spur `entry` off the south
  rail. Every load port and stocker port is a point (track, s) on a spur, so no vehicle ever stops on
  the loop; 71 ports. Stockers: **STK-01** (entry area: starts, ships, default buffer), **STK-02**
  (diffusion, ashers), **STK-03** (photo bay).
- **Fleet**: 10 vehicles at 2 m/s, 10 s per hoist cycle (pick or drop). Idle vehicles circulate on
  the loop. Vehicles never overlap: one vehicle at a time is on a spur (`AMHS.Res`, planned stays with
  a 1 s margin; a vehicle that would enter a taken spur drives another 36 s lap, with the FOUP when it
  carries one), and a vehicle leaving a spur holds a vehicle length before the merge until it keeps
  1.4 m from every vehicle on the loop for as long as it stays there (all move at the fleet speed on
  the loop, so the gap holds). Of the three vehicles that could reach the pickup first, the one that
  picks first takes the job. `TestAmhsNoOverlap` replays a day of plans every 0.5 s.
- **Transfers** (`prod.transfers`): `deliver` (a tool is dispatched a lot whose FOUP is elsewhere: the
  vehicle that reaches the pickup first drives there, picks, carries along the shortest path, drops
  on the tool's freest load port; the run starts when the FOUP is there; batch tools take the FOUP
  inside), `store` (a finished or held lot not dispatched at once goes to the buffer stocker of its
  next area), `ship` (a finished or scrapped lot goes to STK-01). New lots start in STK-01. Every lot
  has a location (`prod.lots.location`: `TOOL:n` port, `STK-0n`, or a batch tool's alias).
- **Calibration** (tests, nominal fab): about 2,100 transfers a day, 1.4 min mean from request to
  delivery (max ~9 min when a spur is busy), fleet busy ~18 %; two vehicles: 2.9 min. Lot starts use
  their own random stream, so transport changes do not change which lots start when. The generated
  history (v11) was made before the no-overlap rules (0.9 min mean); live data and plans use them.
- **Live state for the twin**: every 5 s tick the simulator writes `core.settings`
  `demo_amhs_state` (`AmhsSnapshot`): per vehicle its plan segments that end after half a minute ago
  (runs `t0 / t1 / track / s0 / s1`, hoist cycles with port and lot, merge waits), its idle anchors
  before and after them, the FOUPs on load ports with their time windows, stocker counts. The app
  shows it 15 s behind the server clock, so every plan is known before it shows.

---

## 4. KPIs (backend `production.go`)

Per tool over a window: uptime (P + SB) / T; SEMI E79 availability A = (P + SB) / (T − N),
operational O = P / (P + SB), rate R = Σ theoretical / Σ actual run time of the runs that ended,
quality Q = good / processed wafers; OEE = A × O × R × Q. Fab level: lot / wafer starts and outs,
moves, WIP by area and by layer, cycle time per product (days, days per mask), X-factor (cycle
time / ideal time), on-time delivery against the planned cycle time, line yield, scrap and rework
by reason, OEE by area. Overhead transport: transfers, mean minutes from request to delivery, the
fleet's busy share (`busy_s` of the transfers over vehicles × window).

---

## 5. Data model (schema `prod`, migrations `0008_production.sql`, `0009_production_flow.sql`, `0010_amhs.sql`)

```sql
prod.products     (code PK, name, technology, layers, mix, masks, metals, cycle_days, die_mm2, ideal_h, created_at)
prod.route_steps  (product_code FK, seq, step_code, layer TEXT, module, area, operation, tool_type, recipe,
                   mask, makes, meas_key, spec_name, unit, target, lsl, usl, on_fail, qtime_h,
                   ideal_wafer_s, ideal_lot_s, sample_wafers, sample_rate, param, PRIMARY KEY (product_code, seq))
prod.lots         (lot_code PK, product_code FK, priority, wafers_start, wafers, status, current_seq, step_code,
                   equipment_id, hold_reason, rework_count, released_at, due_at, completed_at, location)
prod.lot_steps    (id PK, lot_code FK, seq, step_code, operation, equipment_id, chambers, recipe, status,
                   queued_at, started_at, ended_at, wafers_in, wafers_out, scrapped, reworked,
                   theoretical_s, actual_s, outcome, batch_id, is_rework)
prod.metrology    (id PK, lot_code, lot_step_id, measured_at, param, char_key, unit, wafer, value, wiw_range,
                   target, lsl, usl, metrology_equipment_id, source_equipment_id, source_chamber,
                   source_step_code, pass)
prod.scrap        (id PK, lot_code, at, wafers, reason, step_code, equipment_id, chamber)
prod.transfers    (id PK, lot_code, product_code, kind deliver|store|ship, from_loc, to_loc, vehicle,
                   requested_at, picked_at, delivered_at, distance_m, busy_s)
sim.jobs.kind     += 'production'
```

---

## 6. Backend API (all require `production.read` + the location scope of the tools)

| Script | Purpose |
|---|---|
| `production/getoverview` | KPI strip (with `transfers`, `transport_min`, `oht_util`, `vehicles`), losses, OEE by area, WIP by area / layer, products |
| `production/gettools` | per tool: status, current lot / step, chambers, queue, A / O / R / Q / OEE, throughput, scrap |
| `production/gettrend` | daily / shift points: OEE factors, wafer starts / outs, moves, cycle time, WIP |
| `production/gettimeline` | tool status intervals with lot runs (Gantt) |
| `production/getlots` / `getlot` | WIP and completed lots (filters: status, product, area, hold) with the FOUP `location`; lot detail with route, steps, metrology, scrap and the last 50 `transfers` |
| `digital_twin/getamhs` | (permission `digitaltwin.read`, a visible process tool) the live transport state `{served_at, updated_at, state}`; lot codes are replaced by "FOUP" without `production.read` |
| `production/getqualitykeys` / `getquality` | characteristics with data; points, SPC limits, capability by chamber, scrap Pareto |
| `production/getplanning` | planning options: current plan, products, tools, limits |
| `production/submitplan` | create a production planning job (human only; `simulation` permission) |
| `production/proposeplan` | the agent's proposal (PROPOSED job, approved by a human in chat) |

---

## 7. Production planning jobs (`simprod.go`, `production_plan.go`)

```json
{
  "kind": "production", "horizon_days": 30, "initial": "live" | "nominal", "seed": 0,
  "breakdowns": true,
  "plan": {"starts_per_day": 4.5, "mix": {"MCU40": 0.6, "WSOC40": 0.4}, "hot_share": 0.05,
           "dispatch": "FIFO" | "CR", "furnace_wait_h": 10, "furnace_min_lots": 3, "vehicles": 10},
  "actions": [{"kind": "tool_down", "asset": "SCN_01", "at_day": 3, "duration_h": 24, "reason": "PM"},
              {"kind": "add_tool", "asset": "CMP_02", "count": 1}],
  "variants": [{"name": "baseline", "plan": {}, "actions": []}, {"name": "+1 lot/day", "plan": {"starts_per_day": 5.5}}]
}
```

Up to 3 variants, 90 days, 60 actions. Each variant runs a copy of the live fab (its WIP and tools
as they are) or a nominal empty fab in 5-minute steps; breakdowns and PMs are seeded identically for
all variants. `summary.variants[v]` = KPIs (outs, wafers out, cycle time per product, on time, WIP at
the end, line yield, area and tool utilization, bottleneck, transfers, mean transport minutes, OHT
busy %), a daily series and the planned events;
`summary.reason` compares against the first variant. Jobs complete `SUCCEEDED` in the simulator (no
post-analysis). The agent's MCP tools `get_production_planning_options` and
`propose_production_plan` validate a spec and propose it for approval.

---

## 8. Frontend and twin

- `/production` (permission `production`): Overview (KPIs, output, WIP by area, line profile, cycle
  time, products), Tools (state timeline, OEE by area), Lots (filters, route map by layer), Quality
  (characteristic selector, SPC, capability, scrap Pareto).
- `/simulation`: "New production plan" form (variants, plan fields, planned downs / added tools) and
  the results (variant cards with deltas, daily wafers out / WIP / cycle time, utilization by area,
  busiest tools).
- Twin: every tool and support unit is a 3D model bound to its asset (badges, dashboards, controller
  screens, status signal towers) in its bay / sub-fab room; the layout is generated from
  `blender/fab_twin_scripts/fab_layout.py` into `digital-twin.fab1-layout.ts`.
- Transport in 3D (`viewer/twin-amhs.ts`): the cleanroom GLB has one vehicle per simulated vehicle
  (`dt_vehicle` V01..V10, hoist children `dt_hoist` / `dt_hoist_belt` / `dt_hoist_foup`); the twin
  polls `digital_twin/getamhs` every 3 s while the scope has vehicles and moves each vehicle along its
  plan (hoist down for picks / drops, FOUP shown while it carries) and draws the FOUPs on load ports
  as instanced meshes. No state, a stale one (> 60 s) or a replay: the vehicles stand parked on the
  loop. Production page: Transport KPI tile, "FOUP at" and the transfer list in the lot detail; plan
  form: OHT vehicles per variant.
