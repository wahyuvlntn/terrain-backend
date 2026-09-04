// Package config loads runtime configuration from environment variables.
package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable of the backend. All values come from the
// environment (optionally pre-loaded from a .env file).
type Config struct {
	// HTTP
	HTTPAddr    string
	HTTPTLS     bool // serve HTTPS (disable when behind a TLS terminating proxy)
	TLSCertFile string
	TLSKeyFile  string
	// Organisation/project aliases (used in MQTT usernames/topics). When
	// StrictProjectPath is set, API calls for other org/project paths are
	// rejected.
	OrgAlias          string
	ProjectAlias      string
	StrictProjectPath bool
	// CORS origins for development (comma separated, "*" allowed).
	CORSOrigins []string

	// Optional hosting of the built web UI (single page app).
	StaticDir string

	// Database
	DatabaseURL string

	// Object storage (MinIO / S3)
	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	MinioBucket    string
	MinioUseSSL    bool
	MinioRegion    string
	// Private bucket of the AI document library (RAG PDFs). Never served
	// under /public, so it must differ from MinioBucket.
	DocsBucket string

	// Agentic AI services (docs/agentic/DESIGN.md)
	AgentURL    string // ai-agent base URL proxied under /api/agent/ (empty = assistant disabled)
	AgentSecret string // shared secret sent to the AI services as X-Agent-Secret
	// Login passwords of the database roles afdd_ai / afdd_ai_ro, set at
	// start-up when not empty (the roles are NOLOGIN until then).
	AIDBPassword   string
	AIDBROPassword string

	// Embedded MQTT broker
	MQTTEnabled        bool
	MQTTTCPAddr        string
	MQTTTLSAddr        string
	MQTTPublicPort     string // port advertised to devices for MQTT_SERVER gateways
	MQTTWSAddr         string
	MQTTWSTLS          bool
	MQTTClientUsername string // credentials used by the web UI (VITE_MQTT_USERNAME)
	MQTTClientPassword string
	NotificationTopic  string

	// Sessions
	SessionTTL time.Duration

	// Bootstrap superuser (created when no superuser exists)
	BootstrapUsername string
	BootstrapPassword string

	// Engine
	EvalInterval         time.Duration
	LogInterval          time.Duration
	OfflineTimeout       time.Duration
	RetentionDays        int
	LicenseEnforced      bool
	LicensePublicKey     string // base64 ed25519 public key used to verify license files
	BuildingLevel        int    // depth of the location tree used as "building" (0 = roots)
	DefaultTimezone      string
	ScriptTimeout        time.Duration
	EarlyWarningCooldown time.Duration

	// Misc
	LogLevel string
	DataDir  string
}

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		HTTPAddr:          env("HTTP_ADDR", ":4500"),
		HTTPTLS:           envBool("HTTP_TLS", true),
		TLSCertFile:       env("TLS_CERT_FILE", ""),
		TLSKeyFile:        env("TLS_KEY_FILE", ""),
		OrgAlias:          env("ORG_ALIAS", "local"),
		ProjectAlias:      env("PROJECT_ALIAS", "digital_twin_afdd"),
		StrictProjectPath: envBool("STRICT_PROJECT_PATH", false),
		CORSOrigins:       splitList(env("CORS_ORIGINS", "")),
		StaticDir:         env("STATIC_DIR", ""),

		DatabaseURL: env("DATABASE_URL", "postgres://afdd:afdd@localhost:5432/afdd?sslmode=disable"),

		MinioEndpoint:  env("MINIO_ENDPOINT", "localhost:9000"),
		MinioAccessKey: env("MINIO_ACCESS_KEY", "minioadmin"),
		MinioSecretKey: env("MINIO_SECRET_KEY", "minioadmin"),
		MinioBucket:    env("MINIO_BUCKET", "afdd"),
		MinioUseSSL:    envBool("MINIO_USE_SSL", false),
		MinioRegion:    env("MINIO_REGION", ""),
		DocsBucket:     env("MINIO_DOCS_BUCKET", "afdd-docs"),

		AgentURL:       strings.TrimRight(env("AGENT_URL", "http://ai-agent:8000"), "/"),
		AgentSecret:    env("AGENT_SHARED_SECRET", ""),
		AIDBPassword:   env("AI_DB_PASSWORD", ""),
		AIDBROPassword: env("AI_DB_RO_PASSWORD", ""),

		MQTTEnabled:        envBool("MQTT_ENABLED", true),
		MQTTTCPAddr:        env("MQTT_TCP_ADDR", ":1883"),
		MQTTTLSAddr:        env("MQTT_TLS_ADDR", ":8883"),
		MQTTPublicPort:     env("MQTT_PUBLIC_PORT", ""),
		MQTTWSAddr:         env("MQTT_WS_ADDR", ":9002"),
		MQTTWSTLS:          envBool("MQTT_WS_TLS", true),
		MQTTClientUsername: env("MQTT_CLIENT_USERNAME", "afdd_ui"),
		MQTTClientPassword: env("MQTT_CLIENT_PASSWORD", "indie123"),
		NotificationTopic:  env("MQTT_NOTIFICATION_TOPIC", "AFDD_NOTIFICATION"),

		SessionTTL: envDuration("SESSION_TTL", 7*24*time.Hour),

		BootstrapUsername: env("BOOTSTRAP_USERNAME", "superuser"),
		BootstrapPassword: env("BOOTSTRAP_PASSWORD", ""),

		EvalInterval:         envDuration("EVAL_INTERVAL", 10*time.Second),
		LogInterval:          envDuration("LOG_INTERVAL", time.Minute),
		OfflineTimeout:       envDuration("OFFLINE_TIMEOUT", 5*time.Minute),
		RetentionDays:        envInt("RETENTION_DAYS", 730),
		LicenseEnforced:      envBool("LICENSE_ENFORCED", false),
		LicensePublicKey:     env("LICENSE_PUBLIC_KEY", ""),
		BuildingLevel:        envInt("BUILDING_LEVEL", 0),
		DefaultTimezone:      env("DEFAULT_TIMEZONE", "Asia/Singapore"),
		ScriptTimeout:        envDuration("SCRIPT_TIMEOUT", 500*time.Millisecond),
		EarlyWarningCooldown: envDuration("EARLY_WARNING_COOLDOWN", time.Hour),

		LogLevel: env("LOG_LEVEL", "info"),
		DataDir:  env("DATA_DIR", "./data"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if c.EvalInterval < time.Second {
		c.EvalInterval = time.Second
	}
	if c.LogInterval < time.Second {
		c.LogInterval = time.Second
	}
	if c.BuildingLevel < 0 {
		c.BuildingLevel = 0
	}
	if c.DocsBucket == "" {
		c.DocsBucket = "afdd-docs"
	}
	if c.DocsBucket == c.MinioBucket {
		return nil, fmt.Errorf("MINIO_DOCS_BUCKET must differ from MINIO_BUCKET (%q is served publicly under /public)", c.MinioBucket)
	}
	if c.AgentURL != "" {
		if u, err := url.Parse(c.AgentURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("AGENT_URL must be an absolute http(s) URL, got %q", c.AgentURL)
		}
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadDotEnv loads KEY=VALUE pairs from a file without overriding variables
// that are already present in the environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}
