package config

import (
	"os"
	"testing"
)

// unset removes environment variables for the duration of the test.
func unset(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "") // restores the original value afterwards
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

var aiKeys = []string{"AGENT_URL", "AGENT_SHARED_SECRET", "MINIO_DOCS_BUCKET", "MINIO_BUCKET", "AI_DB_PASSWORD", "AI_DB_RO_PASSWORD"}

func TestAIDefaults(t *testing.T) {
	unset(t, aiKeys...)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentURL != "http://ai-agent:8000" || c.AgentSecret != "" || c.DocsBucket != "afdd-docs" ||
		c.AIDBPassword != "" || c.AIDBROPassword != "" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestAIOverrides(t *testing.T) {
	unset(t, aiKeys...)
	t.Setenv("AGENT_URL", " https://agent.internal:9443/ ")
	t.Setenv("AGENT_SHARED_SECRET", "s3cret")
	t.Setenv("MINIO_DOCS_BUCKET", "docs")
	t.Setenv("AI_DB_PASSWORD", "rw")
	t.Setenv("AI_DB_RO_PASSWORD", "ro")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentURL != "https://agent.internal:9443" || c.AgentSecret != "s3cret" || c.DocsBucket != "docs" ||
		c.AIDBPassword != "rw" || c.AIDBROPassword != "ro" {
		t.Errorf("overrides: %+v", c)
	}

	// An empty AGENT_URL disables the assistant; an empty docs bucket falls back.
	t.Setenv("AGENT_URL", "")
	t.Setenv("MINIO_DOCS_BUCKET", "")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c.AgentURL != "" || c.DocsBucket != "afdd-docs" {
		t.Errorf("empty values: agent %q, bucket %q", c.AgentURL, c.DocsBucket)
	}
}

func TestAIValidation(t *testing.T) {
	for _, bad := range []string{"ai-agent:8000", "ftp://ai-agent", "http://", "://x"} {
		unset(t, aiKeys...)
		t.Setenv("AGENT_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("AGENT_URL %q accepted", bad)
		}
	}
	// The documents bucket must not be the public one.
	unset(t, aiKeys...)
	t.Setenv("MINIO_BUCKET", "afdd")
	t.Setenv("MINIO_DOCS_BUCKET", "afdd")
	if _, err := Load(); err == nil {
		t.Error("docs bucket equal to the public bucket accepted")
	}
}
