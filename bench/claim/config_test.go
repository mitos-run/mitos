package main

import (
	"testing"
	"time"
)

func TestParseConfigClaimExecDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--kubeconfig", "/k", "--pool", "p"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeClaimExec {
		t.Errorf("mode = %q, want %q", cfg.mode, modeClaimExec)
	}
	if cfg.iterations != 20 {
		t.Errorf("iterations = %d, want 20", cfg.iterations)
	}
	if cfg.namespace != "default" {
		t.Errorf("namespace = %q, want default", cfg.namespace)
	}
}

func TestParseConfigRequiresKubeconfig(t *testing.T) {
	if _, err := parseConfig([]string{"--pool", "p"}); err == nil {
		t.Fatal("expected error for missing --kubeconfig")
	}
}

func TestParseConfigRequiresPool(t *testing.T) {
	if _, err := parseConfig([]string{"--kubeconfig", "/k"}); err == nil {
		t.Fatal("expected error for missing --pool")
	}
}

func TestParseConfigInvalidMode(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "bogus", "--kubeconfig", "/k", "--pool", "p"}); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestParseConfigSustained(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "sustained", "--kubeconfig", "/k", "--pool", "p", "--rate", "10", "--duration", "1m"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeSustained {
		t.Errorf("mode = %q, want %q", cfg.mode, modeSustained)
	}
	if cfg.rate != 10 {
		t.Errorf("rate = %v, want 10", cfg.rate)
	}
	if cfg.duration != time.Minute {
		t.Errorf("duration = %v, want 1m", cfg.duration)
	}
}

func TestParseConfigSustainedRejectsZeroRate(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "sustained", "--kubeconfig", "/k", "--pool", "p", "--rate", "0"}); err == nil {
		t.Fatal("expected error for --rate 0 in sustained mode")
	}
}

func TestParseConfigPoolRebuild(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "pool-rebuild", "--kubeconfig", "/k", "--pool", "p"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modePoolRebuild {
		t.Errorf("mode = %q, want %q", cfg.mode, modePoolRebuild)
	}
}

func TestParseConfigClaimExecRejectsZeroIterations(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "claim-exec", "--kubeconfig", "/k", "--pool", "p", "--iterations", "0"}); err == nil {
		t.Fatal("expected error for --iterations 0")
	}
}
