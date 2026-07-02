package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// envFunc builds a getenv func over a map for deterministic config tests.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, envFunc(map[string]string{"MITOS_API_KEY": "mitos_live_test"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.pool != "python" {
		t.Errorf("default pool = %q, want python", cfg.pool)
	}
	if cfg.interval != 60*time.Second {
		t.Errorf("default interval = %s, want 60s", cfg.interval)
	}
	if cfg.listenAddr != ":9102" {
		t.Errorf("default listen = %q, want :9102", cfg.listenAddr)
	}
	// Default staleness window is three intervals.
	if cfg.stalenessWindow != 3*time.Minute {
		t.Errorf("default staleness = %s, want 3m", cfg.stalenessWindow)
	}
}

func TestParseConfigRequiresAPIKey(t *testing.T) {
	_, err := parseConfig(nil, envFunc(nil))
	if err == nil {
		t.Fatal("expected an error when no api key is set")
	}
}

func TestParseConfigAPIKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("  mitos_live_fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig([]string{"--api-key-file", keyPath}, envFunc(nil))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.apiKey != "mitos_live_fromfile" {
		t.Errorf("api key = %q, want trimmed mitos_live_fromfile", cfg.apiKey)
	}
}

func TestParseConfigFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"MITOS_API_KEY":         "mitos_live_test",
		"MITOS_CANARY_POOL":     "node",
		"MITOS_CANARY_INTERVAL": "30s",
	}
	cfg, err := parseConfig([]string{"--pool", "python", "--interval", "10s"}, envFunc(env))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.pool != "python" {
		t.Errorf("flag pool should override env: got %q", cfg.pool)
	}
	if cfg.interval != 10*time.Second {
		t.Errorf("flag interval should override env: got %s", cfg.interval)
	}
}

func TestParseConfigRejectsNonPositiveInterval(t *testing.T) {
	_, err := parseConfig([]string{"--interval", "0s"}, envFunc(map[string]string{"MITOS_API_KEY": "x"}))
	if err == nil {
		t.Fatal("expected an error for a non-positive interval")
	}
}
