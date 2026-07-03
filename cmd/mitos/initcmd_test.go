package main

import (
	"testing"

	"mitos.run/mitos/internal/agentcli"
)

// The hosted credential precedence is flag > env > the config file written by
// `mitos init`. These tests pin the config-file leg and that the higher layers
// still win over it.
func TestResolveHostedCredsReadsInitConfigAsLastFallback(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_BASE_URL", "")
	if err := agentcli.WriteCLIConfig(agentcli.CLIConfig{
		Endpoint: "https://mitos.example.com",
		APIKey:   "mitos_live_ZnJvbS10aGUtY29uZmlnLWZpbGU",
	}); err != nil {
		t.Fatal(err)
	}

	key, url := resolveHostedCreds("", "")
	if key != "mitos_live_ZnJvbS10aGUtY29uZmlnLWZpbGU" {
		t.Fatalf("api key not read from the init config file")
	}
	if url != "https://mitos.example.com" {
		t.Fatalf("base url = %q, want the init config endpoint", url)
	}
}

func TestResolveHostedCredsFlagAndEnvWinOverConfig(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	t.Setenv("MITOS_API_KEY", "mitos_live_ZnJvbS10aGUtZW52aXJvbm1lbnQ")
	t.Setenv("MITOS_BASE_URL", "")
	if err := agentcli.WriteCLIConfig(agentcli.CLIConfig{
		Endpoint: "https://mitos.example.com",
		APIKey:   "mitos_live_ZnJvbS10aGUtY29uZmlnLWZpbGU",
	}); err != nil {
		t.Fatal(err)
	}

	// Env beats the file for the key; the file still fills the missing URL.
	key, url := resolveHostedCreds("", "")
	if key != "mitos_live_ZnJvbS10aGUtZW52aXJvbm1lbnQ" {
		t.Fatalf("env key must win over the config file")
	}
	if url != "https://mitos.example.com" {
		t.Fatalf("base url = %q, want the config endpoint filling the gap", url)
	}

	// Flags beat everything.
	key, url = resolveHostedCreds("mitos_live_ZnJvbS10aGUtZmxhZy12YWx1ZQ", "https://flag.example.com")
	if key != "mitos_live_ZnJvbS10aGUtZmxhZy12YWx1ZQ" || url != "https://flag.example.com" {
		t.Fatalf("flags must win over env and config; got url %q", url)
	}
}

func TestResolveHostedCredsNoConfigStaysClusterMode(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_BASE_URL", "")

	key, url := resolveHostedCreds("", "")
	if key != "" || url != "" {
		t.Fatalf("with no flag, env, or config the CLI must fall through to cluster mode; got (%q, %q)", key, url)
	}
}
