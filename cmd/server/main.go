// Command server runs the AFDD (asset monitoring & digital maintenance)
// backend: HTTP API, embedded MQTT broker, data hub, evaluation engine and
// background scheduler.
//
// Usage:
//
//	server [serve]                       run the backend (default)
//	server migrate                       apply database migrations and exit
//	server create-user -username U -password P [-role superuser|admin|basic] [-name N] [-email E]
//	server reset-password -username U -password P
//	server license keygen                print a new license signing key pair
//	server license system-id             print this installation's system id
//	server healthcheck                   exit 0 when the local API reports healthy
//	server license issue -key PRIV -type BASE|EQUIPMENT [-assets a,b] [-expires 2027-01-31] [-system-id ID] [-description D] [-key-id K]
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"afdd/internal/api"
	"afdd/internal/auth"
	"afdd/internal/broker"
	"afdd/internal/config"
	"afdd/internal/datahub"
	"afdd/internal/db"
	"afdd/internal/engine"
	"afdd/internal/license"
	"afdd/internal/maint"
	"afdd/internal/notify"
	"afdd/internal/scheduler"
	"afdd/internal/storage"
	"afdd/internal/tlsutil"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "migrate":
		err = migrate()
	case "create-user":
		err = createUser(args)
	case "reset-password":
		err = resetPassword(args)
	case "license":
		err = licenseCmd(args)
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `commands:
  serve            run the backend (default)
  migrate          apply database migrations
  create-user      -username U -password P [-role superuser|admin|basic] [-name N] [-email E]
  reset-password   -username U -password P
  healthcheck      query the local /healthz endpoint
  license keygen | system-id | issue -key PRIV -type BASE|EQUIPMENT [-assets a,b] [-expires YYYY-MM-DD] [-system-id ID] [-description D]
  version`

func setupLogging(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}

func openDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	d, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func migrate() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)
	d, err := openDB(context.Background(), cfg)
	if err != nil {
		return err
	}
	d.Close()
	fmt.Println("migrations applied")
	return nil
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)
	slog.Info("starting AFDD backend", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	database, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	// The AI services log in with these roles; the core backend runs without
	// them, so a failure is reported but not fatal.
	if err := db.EnsureServiceRoles(ctx, database, cfg.AIDBPassword, cfg.AIDBROPassword); err != nil {
		slog.Error("enable AI service database roles", "err", err)
	}

	storeOpts := storage.Options{Endpoint: cfg.MinioEndpoint, AccessKey: cfg.MinioAccessKey, SecretKey: cfg.MinioSecretKey,
		Bucket: cfg.MinioBucket, UseSSL: cfg.MinioUseSSL, Region: cfg.MinioRegion}
	store, err := storage.New(ctx, storeOpts)
	if err != nil {
		return err
	}
	// Private bucket of the AI document library (never served under /public).
	docsOpts := storeOpts
	docsOpts.Bucket = cfg.DocsBucket
	docsStore, err := storage.New(ctx, docsOpts)
	if err != nil {
		return err
	}

	tlsCfg, err := tlsutil.Load(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.DataDir)
	if err != nil {
		return err
	}

	authSvc := auth.NewService(database, cfg.SessionTTL)
	created, generated, err := authSvc.EnsureBootstrapUser(ctx, cfg.BootstrapUsername, cfg.BootstrapPassword)
	if err != nil {
		return err
	}
	if created {
		if generated != "" {
			slog.Warn("created initial superuser with a generated password - change it after first login",
				"username", cfg.BootstrapUsername, "password", generated)
		} else {
			slog.Info("created initial superuser", "username", cfg.BootstrapUsername)
		}
	}

	var mqttBroker *broker.Broker
	var publisher notify.Publisher
	if cfg.MQTTEnabled {
		opts := broker.Options{TCPAddr: cfg.MQTTTCPAddr, TLSAddr: cfg.MQTTTLSAddr, TLSConfig: tlsCfg, WSAddr: cfg.MQTTWSAddr,
			UIUsername: cfg.MQTTClientUsername, UIPassword: cfg.MQTTClientPassword, NotificationTopic: cfg.NotificationTopic}
		if cfg.MQTTWSTLS {
			opts.WSTLS = tlsCfg
		}
		mqttBroker = broker.New(opts)
		if err := mqttBroker.Start(); err != nil {
			return fmt.Errorf("start mqtt broker: %w", err)
		}
		defer mqttBroker.Close()
		publisher = mqttBroker
	}

	notifier := notify.New(database, publisher, cfg.NotificationTopic)

	hub := datahub.New(database, mqttBroker, cfg.ScriptTimeout, cfg.DataDir, cfg.OrgAlias+"."+cfg.ProjectAlias)
	if err := hub.Start(ctx); err != nil {
		return fmt.Errorf("start data hub: %w", err)
	}
	defer hub.Stop()

	licMgr, err := license.NewManager(database, cfg.LicensePublicKey)
	if err != nil {
		return err
	}

	loc, err := time.LoadLocation(cfg.DefaultTimezone)
	if err != nil {
		slog.Warn("invalid DEFAULT_TIMEZONE, using UTC", "tz", cfg.DefaultTimezone)
		loc = time.UTC
	}

	eng := engine.New(database, hub, notifier, engine.Options{EvalInterval: cfg.EvalInterval, LogInterval: cfg.LogInterval,
		OfflineTimeout: cfg.OfflineTimeout, ScriptTimeout: cfg.ScriptTimeout, LicenseEnforced: cfg.LicenseEnforced,
		EarlyWarningCool: cfg.EarlyWarningCooldown})
	maintSvc := maint.New(database, notifier, loc)
	maintSvc.OnEquipmentChanged = eng.Invalidate
	eng.SetHooks(maintSvc)

	srv := api.New(api.Deps{Config: cfg, DB: database, Store: store, Auth: authSvc, Notify: notifier, Hub: hub, Engine: eng,
		Maint: maintSvc, License: licMgr, Broker: mqttBroker, DocsStore: docsStore})

	sched := scheduler.New(scheduler.Deps{DB: database, Notify: notifier, Maint: maintSvc, CleanSessions: authSvc.CleanupExpired,
		RefreshKPIs: srv.RefreshKPIs, RetentionDays: cfg.RetentionDays})
	// The engine logs into monthly partitions of ts.parameter_values: make sure
	// the current and the coming months exist before it starts. Not fatal: the
	// migration created months ahead and the scheduled job retries.
	if err := sched.MaintainPartitions(ctx); err != nil {
		slog.Error("partition maintenance at start-up", "err", err)
	}

	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		eng.Run(ctx)
	}()

	sched.Start(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute, // large uploads
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.HTTPTLS {
			httpSrv.TLSConfig = tlsCfg
			slog.Info("https listening", "addr", cfg.HTTPAddr)
			err = httpSrv.ListenAndServeTLS("", "")
		} else {
			slog.Info("http listening", "addr", cfg.HTTPAddr)
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			stop()
			return fmt.Errorf("http server: %w", err)
		}
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	sched.Wait()
	<-engineDone
	return nil
}

func withDB(fn func(ctx context.Context, d *db.DB) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging("warn")
	ctx := context.Background()
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	return fn(ctx, d)
}

func createUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password")
	role := fs.String("role", auth.RoleSuperuser, "superuser | admin | basic")
	name := fs.String("name", "", "full name")
	email := fs.String("email", "", "email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *password == "" {
		return errors.New("-username and -password are required")
	}
	switch *role {
	case auth.RoleSuperuser, auth.RoleAdmin, auth.RoleBasic:
	default:
		return fmt.Errorf("invalid role %q", *role)
	}
	if msg := auth.ValidatePasswordPolicy(*password); msg != "" {
		return errors.New(msg)
	}
	hash, err := auth.HashPassword(*password)
	if err != nil {
		return err
	}
	return withDB(func(ctx context.Context, d *db.DB) error {
		if _, err := d.Exec(ctx, `INSERT INTO core.users (username, password_hash, full_name, email, role) VALUES ($1,$2,$3,$4,$5)`,
			*username, hash, *name, *email, *role); err != nil {
			if db.IsUniqueViolation(err) {
				return errors.New("a user with this username already exists")
			}
			return err
		}
		fmt.Printf("user %s created (%s)\n", *username, *role)
		return nil
	})
}

func resetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "new password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *password == "" {
		return errors.New("-username and -password are required")
	}
	if msg := auth.ValidatePasswordPolicy(*password); msg != "" {
		return errors.New(msg)
	}
	hash, err := auth.HashPassword(*password)
	if err != nil {
		return err
	}
	return withDB(func(ctx context.Context, d *db.DB) error {
		tag, err := d.Exec(ctx, `UPDATE core.users SET password_hash = $2, updated_at = now() WHERE lower(username) = lower($1) AND NOT is_deleted`,
			*username, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("user not found")
		}
		if _, err := d.Exec(ctx, `DELETE FROM core.sessions WHERE user_id = (SELECT id FROM core.users WHERE lower(username) = lower($1) AND NOT is_deleted)`,
			*username); err != nil {
			return err
		}
		fmt.Println("password updated")
		return nil
	})
}

func licenseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: license keygen | system-id | issue ...")
	}
	switch args[0] {
	case "keygen":
		pub, priv, err := license.GenerateKeys()
		if err != nil {
			return err
		}
		fmt.Println("LICENSE_PUBLIC_KEY=" + pub)
		fmt.Println("private key (keep secret, used with 'license issue -key'):")
		fmt.Println(priv)
		return nil
	case "system-id":
		return withDB(func(ctx context.Context, d *db.DB) error {
			m, err := license.NewManager(d, "")
			if err != nil {
				return err
			}
			id, err := m.SystemID(ctx)
			if err != nil {
				return err
			}
			fmt.Println(id)
			return nil
		})
	case "issue":
		fs := flag.NewFlagSet("license issue", flag.ContinueOnError)
		key := fs.String("key", "", "base64 ed25519 private key")
		typ := fs.String("type", license.TypeEquipment, "BASE | EQUIPMENT")
		assets := fs.String("assets", "", "comma separated asset keys (EQUIPMENT licenses)")
		count := fs.Int("count", 0, "generate this many random asset keys instead of -assets")
		expires := fs.String("expires", "", "expiry date YYYY-MM-DD (empty = never)")
		systemID := fs.String("system-id", "", "bind the license to a system id")
		desc := fs.String("description", "", "description")
		keyID := fs.String("key-id", "", "license key (random when empty)")
		product := fs.String("product", "AFDD", "product name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		priv, err := license.DecodePrivateKey(*key)
		if err != nil {
			return fmt.Errorf("invalid -key: %w", err)
		}
		t := strings.ToUpper(*typ)
		if t != license.TypeBase && t != license.TypeEquipment {
			return fmt.Errorf("invalid -type %q", *typ)
		}
		c := license.Content{Key: *keyID, Type: t, Description: *desc, SystemID: *systemID, Product: *product}
		if c.Key == "" {
			c.Key = randomHex(16)
		}
		for _, a := range strings.Split(*assets, ",") {
			if a = strings.TrimSpace(a); a != "" {
				c.AssetKeys = append(c.AssetKeys, a)
			}
		}
		for i := 0; i < *count; i++ {
			c.AssetKeys = append(c.AssetKeys, randomHex(8))
		}
		if t == license.TypeEquipment && len(c.AssetKeys) == 0 {
			return errors.New("an EQUIPMENT license needs -assets or -count")
		}
		if *expires != "" {
			e, err := time.Parse("2006-01-02", *expires)
			if err != nil {
				return fmt.Errorf("invalid -expires: %w", err)
			}
			c.ExpiredAt = e.Add(24*time.Hour - time.Millisecond).UnixMilli()
		}
		out, err := license.Issue(priv, c)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	return fmt.Errorf("unknown license command %q", args[0])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// healthcheck queries the local /healthz endpoint (used by the Docker
// HEALTHCHECK).
func healthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	if cfg.HTTPTLS {
		scheme = "https"
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local self check
	}}
	resp, err := client.Get(scheme + "://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: status %d", resp.StatusCode)
	}
	return nil
}
