// Command simulator populates the AFDD backend with a realistic demo plant
// (a semiconductor fab's facility systems) and streams live MQTT telemetry
// with evolving faults so the whole application can be exercised.
//
//	simulator start    seed (once) and run the live simulation (default)
//	simulator seed     configure the plant and generate the history only
//	simulator run      live simulation only
//	simulator history  regenerate the history (replaces all demo history and orders)
//
// Configuration (environment):
//
//	DATABASE_URL        PostgreSQL of the backend (required)
//	API_URL             backend base URL                 (https://backend:4500)
//	MQTT_URL            backend MQTT listener            (tcp://backend:1883)
//	ORG_ALIAS, PROJECT_ALIAS                             (local, digital_twin_afdd)
//	DEMO_PASSWORD       password of the demo users       (Demo@2026)
//	HISTORY_DAYS        days of generated history        (60)
//	SIM_INTERVAL        telemetry period                 (5s)
//	SIM_FAULT_EVERY     mean time between new faults     (45m)
//	SIM_TRIP_EVERY      mean time between trips          (4h)
//	SIM_OFFLINE_EVERY   mean time between comm losses    (8h)
//	SIM_MAX_FAULTS      concurrent degradations          (4)
//	SIM_RAMP_MIN/MAX    minutes for a fault to develop   (20 / 60)
//	SIM_WORKFORCE       simulate the maintenance team    (true)
//	SIM_APPROVE_DELAY   idle time before the team acts   (15m)
//	SIM_JOBS            run digital twin simulation jobs (true)
//	SIM_TRANSPORT       mqtt (the backend's broker) | http (REST push gateways, for the cloud) (mqtt)
//	API_VERIFY_TLS      verify the backend's certificate (false: local self-signed)
//	DEFAULT_TIMEZONE    plant time zone                  (Asia/Singapore)
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const seedKey = "demo_seed"

// seedVersion is bumped whenever the plant configuration changes; an older
// installation is upgraded in place (the seeder only adds what is missing and
// re-applies thresholds, control definitions and changed alarms).
// 6: plant supervisory points and controls on CHWS_01, LBNL fault codes,
// SOPs and FDD issue relations, AI permission groups.
// 7: FAB1 L2 process tools (etch, CVD, metrology, wet clean), their location,
// gateway, codes, SOPs and technicians, products and routes, production
// permission.
// 8: FAB1 full 40 nm line: 31 process tools in five L2 bays (lithography,
// etch, deposition, CMP, diffusion, implant, cleans, metrology, test), sub-fab
// scrubbers, process chillers and four more dry pumps, renamed sub-fab rooms,
// codes, SOPs, technicians, the full product routes.
// 9: furnace monitor wafer adders, dielectric etch wall temperature, plating
// current and support unit limits.
const seedVersion = 9

// historyVersion is bumped when the generated history must be regenerated
// (e.g. new equipment that needs history); this replaces all demo history and
// orders. A configuration-only upgrade keeps the history: parameters added to
// existing models get values from the live simulation on.
// 4: supervisory control physics, LBNL fault episodes with FDD orders and
// operator set point changes.
// 5: process tools and the production model (lots, metrology, scrap) with
// the tools coupled to their dry pumps.
// 6: the full 40 nm flow (about a month per lot) on the new tool set, with
// the sub-fab interlocks.
// 7: no maintenance loops from readings of idle tools and restarting support
// units.
// 8: every process tool fault reaches its diagnosis (issue relations).
// 9: litho and implant load balance (IO LDD / ESD masks on i-line,
// realistic implanter and asher throughput).
// 10: planned cycle times (26 / 30 days) the lots are scheduled against.
// 11: simulated overhead transport (OHT vehicles, stockers, transfer
// records) sets when each lot reaches its tool.
const historyVersion = 11

// faultCatalogKey is the core.settings key of the fault catalog the simulator
// publishes for the backend (fault injection and simulation job validation).
const faultCatalogKey = "sim_fault_catalog"

type seedMarker struct {
	Version        int       `json:"version"`
	HistoryVersion int       `json:"history_version"`
	Configured     time.Time `json:"configured_at"`
	History        time.Time `json:"history_at"`
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(key, "")); err == nil && d > 0 {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if f, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return f
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd := "start"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cmd); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("simulator failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string) error {
	dbURL := env("DATABASE_URL", "")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	loc, err := time.LoadLocation(env("DEFAULT_TIMEZONE", "Asia/Singapore"))
	if err != nil {
		loc = time.FixedZone("SGT", 8*3600)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if cmd == "start" || cmd == "run" {
		lease, err := holdLease(ctx, db)
		if err != nil {
			return err
		}
		defer lease.Release()
	}
	api := env("API_URL", "https://backend:4500")
	if err := waitFor(ctx, api, db); err != nil {
		return err
	}
	client := newClient(db, api, env("ORG_ALIAS", "local"), env("PROJECT_ALIAS", "digital_twin_afdd"))
	// SIM_TRANSPORT=http: REST push gateways instead of the backend's MQTT
	// broker (the cloud deployment has none)
	if strings.EqualFold(env("SIM_TRANSPORT", "mqtt"), "http") {
		httpLink = newHTTPTransport(api, strings.EqualFold(env("API_VERIFY_TLS", "false"), "true"), db)
		gatewayType = "REST_SERVER"
		slog.Info("telemetry over HTTPS", "api", api)
	}
	seed := uint64(time.Now().UnixNano())
	if s := env("SIM_SEED", ""); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			seed = n
		}
	}
	switch cmd {
	case "seed":
		return doSeed(ctx, db, client, loc, seed)
	case "run":
		return doRun(ctx, db, client, loc, seed)
	case "history":
		return doHistory(ctx, db, client, loc, seed)
	case "start":
		if err := doSeed(ctx, db, client, loc, seed); err != nil {
			return err
		}
		return doRun(ctx, db, client, loc, seed)
	}
	return fmt.Errorf("unknown command %q (use start, seed or run)", cmd)
}

// httpLink is the HTTPS transport (SIM_TRANSPORT=http), nil for MQTT.
var httpLink *httpTransport

// simLeaseKey is the simulator's advisory lock ("AFDD" + 3; the backend's
// processing worker holds ...0001, the AI analytics worker ...0002).
const simLeaseKey int64 = 0x4146_4444_0003

// holdLease waits until this process holds the simulator's advisory lock: one
// simulator at a time, also while a Cloud Run worker pool replaces its
// instance (the new one waits until the old one is gone).
func holdLease(ctx context.Context, db *pgxpool.Pool) (*pgxpool.Conn, error) {
	waiting := false
	for {
		conn, err := db.Acquire(ctx)
		if err == nil {
			var ok bool
			if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, simLeaseKey).Scan(&ok); err == nil && ok {
				slog.Info("simulator lease acquired")
				return conn, nil
			}
			conn.Release()
		}
		if err != nil {
			slog.Warn("simulator lease", "err", err)
		} else if !waiting {
			waiting = true
			slog.Info("another simulator is running, standing by")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitFor waits until the backend API and database are reachable and the
// schema exists.
func waitFor(ctx context.Context, api string, db *pgxpool.Pool) error {
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := hc.Get(api + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && exists(ctx, db, `SELECT 1 FROM core.users WHERE role = 'superuser'`) {
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backend not ready at %s: %w", api, err)
		}
		slog.Info("waiting for backend", "url", api, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func doSeed(ctx context.Context, db *pgxpool.Pool, c *Client, loc *time.Location, seed uint64) error {
	var m seedMarker
	getSetting(ctx, db, seedKey, &m)
	if m.HistoryVersion == 0 && !m.History.IsZero() {
		// markers written before history_version: the history matches the version
		m.HistoryVersion = m.Version
	}
	configure := m.Version < seedVersion || m.History.IsZero()
	history := m.HistoryVersion < historyVersion || m.History.IsZero()
	if !configure && !history {
		slog.Info("demo data already seeded", "configured_at", m.Configured.Format(time.RFC3339), "history_at", m.History.Format(time.RFC3339))
		return nil
	}
	if !m.History.IsZero() {
		slog.Info("upgrading demo plant", "from_version", m.Version, "to_version", seedVersion, "regenerate_history", history)
	}
	if configure {
		s := &Seeder{c: c, db: db, password: env("DEMO_PASSWORD", "Demo@2026")}
		if err := s.Run(ctx); err != nil {
			return err
		}
		m.Version, m.Configured = seedVersion, time.Now().UTC()
		if err := putSetting(ctx, db, seedKey, m); err != nil {
			return err
		}
	}
	if history {
		if err := doHistory(ctx, db, c, loc, seed); err != nil {
			return err
		}
		// set only once the history is complete, so an interrupted run regenerates it
		m.History, m.HistoryVersion = time.Now().UTC(), historyVersion
		if err := putSetting(ctx, db, seedKey, m); err != nil {
			return err
		}
	}
	slog.Info("demo plant ready")
	return nil
}

func doRun(ctx context.Context, db *pgxpool.Pool, c *Client, loc *time.Location, seed uint64) error {
	plant := newPlant(seed, loc)
	var st SimState
	if getSetting(ctx, db, stateKey, &st) {
		restoreState(plant, st)
		slog.Info("simulation state restored", "saved_at", st.SavedAt.Format(time.RFC3339))
		// production resumes where it stood instead of replaying the time
		// the simulator was stopped
		if gap := time.Since(st.SavedAt); plant.fab != nil && st.Fab != nil && gap > 2*time.Minute {
			plant.fab.shift(gap)
			slog.Info("production schedule shifted", "by", gap.Round(time.Second))
		}
	}
	if f := plant.fab; f != nil {
		if !prodReady(ctx, db) {
			slog.Warn("schema prod missing: production records are not stored")
			plant.fab = nil
			for _, a := range plant.Assets {
				a.tool, a.Coupled = nil, false
			}
		} else {
			var maxID int64
			if err := db.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM prod.lot_steps`).Scan(&maxID); err == nil {
				f.NextStep = max(f.NextStep, maxID+1)
			}
			if err := upsertProducts(ctx, db); err != nil {
				slog.Warn("products", "err", err)
			}
		}
	}
	opts := LiveOptions{
		MQTTURL: env("MQTT_URL", "tcp://backend:1883"), HTTP: httpLink, Org: env("ORG_ALIAS", "local"), Prj: env("PROJECT_ALIAS", "digital_twin_afdd"),
		Interval: envDur("SIM_INTERVAL", 5*time.Second), FaultEvery: envDur("SIM_FAULT_EVERY", 45*time.Minute),
		TripEvery: envDur("SIM_TRIP_EVERY", 4*time.Hour), OfflineEvry: envDur("SIM_OFFLINE_EVERY", 8*time.Hour),
		MaxFaults: envInt("SIM_MAX_FAULTS", 4), RampMin: envFloat("SIM_RAMP_MIN", 20), RampMax: envFloat("SIM_RAMP_MAX", 60),
	}
	// Faults carried over from the history develop at history speed (days);
	// bring them to the live pace so they surface within the session. Fixed-
	// intensity (Manual) faults keep their intensity.
	minRate := 1.25 / (opts.RampMax * 60)
	for _, a := range plant.Assets {
		for _, f := range a.Faults {
			if fs := a.Model.faultSpec(f.Kind); fs != nil && fs.Manual {
				continue
			}
			if f.Rate < minRate {
				f.Rate = minRate
			}
		}
	}
	if err := putSetting(ctx, db, faultCatalogKey, faultCatalog(plant)); err != nil {
		slog.Warn("publish fault catalog", "err", err)
	}
	live := newLive(ctx, db, plant, opts, seed)
	if strings.EqualFold(env("SIM_WORKFORCE", "true"), "true") {
		bot := newBot(ctx, db, c, plant, live, envDur("SIM_APPROVE_DELAY", 15*time.Minute), loc, seed)
		go bot.Run()
		slog.Info("maintenance team simulation enabled")
	}
	if strings.EqualFold(env("SIM_JOBS", "true"), "true") {
		runner := newJobRunner(ctx, db, loc, live.Snapshot, jobOptions(opts))
		go runner.Run()
		slog.Info("simulation job runner enabled")
	}
	slog.Info("live simulation started", "equipment", len(plant.Assets), "mqtt", opts.MQTTURL, "interval", opts.Interval)
	return live.Run()
}

// doHistory (re)generates the history and the simulator state.
func doHistory(ctx context.Context, db *pgxpool.Pool, c *Client, loc *time.Location, seed uint64) error {
	plant := newPlant(seed, loc)
	if _, err := runHistory(ctx, db, plant, envInt("HISTORY_DAYS", 60), seed); err != nil {
		return err
	}
	if err := putSetting(ctx, db, stateKey, captureState(plant)); err != nil {
		return err
	}
	// Direct database changes (forced statuses) need an engine reload.
	ch := findModel("Daikin WSC-126 Centrifugal Chiller")
	_, err := c.Super(ctx, "model_manager/update_running_hour_limit", map[string]any{"model_id": idOf(ctx, db, `SELECT id FROM asset.models WHERE name = $1`, ch.Name),
		"warning_hour": ch.RunHourWarn, "critical_hour": ch.RunHourCrit, "is_notify_warning": true, "is_notify_critical": true,
		"is_auto_create_maintenance": false})
	return err
}
