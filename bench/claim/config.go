package main

import (
	"flag"
	"fmt"
	"time"
)

const (
	// modeClaimExec measures claim -> first-exec end to end through the
	// controller: create a SandboxClaim, wait for Ready, run the first exec
	// over the sandbox HTTP API. Issue #15 item 1.
	modeClaimExec = "claim-exec"
	// modeSustained drives a sustained claim arrival rate and records achieved
	// claims/sec and per-node density. Issue #15 item 2.
	modeSustained = "sustained"
	// modePoolRebuild measures pool-update -> all-nodes-ready propagation.
	// Issue #15 item 3.
	modePoolRebuild = "pool-rebuild"

	// pollInterval is how often the harness polls the cluster for a claim or
	// pool transition.
	pollInterval = 50 * time.Millisecond
)

// config holds the claim-bench flags. Defaults are conservative so a maintainer
// can point it at a real cluster with only --kubeconfig and --pool.
type config struct {
	mode       string
	kubeconfig string
	namespace  string
	pool       string
	iterations int
	// rate is the target claim arrival rate (claims/sec) for the sustained mode.
	rate float64
	// duration is how long the sustained mode keeps arriving claims.
	duration time.Duration
	// maxConcurrent caps simultaneously-live claims in the sustained mode so a
	// run does not exhaust the pool before the window ends. Zero means no cap.
	maxConcurrent int
	timeout       time.Duration
	jsonPath      string
}

// parseConfig parses the harness flags. It validates the mode and the
// per-mode required values so a misconfigured run fails fast with a clear
// message rather than against the cluster.
func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("claim-bench", flag.ContinueOnError)
	fs.StringVar(&cfg.mode, "mode", modeClaimExec, "benchmark mode: claim-exec|sustained|pool-rebuild")
	fs.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to the kubeconfig for the target cluster (required)")
	fs.StringVar(&cfg.namespace, "namespace", "default", "namespace to create claims in")
	fs.StringVar(&cfg.pool, "pool", "", "SandboxPool to claim from (required for claim-exec and sustained)")
	fs.IntVar(&cfg.iterations, "iterations", 20, "claim-exec: number of sequential claims to measure")
	fs.Float64Var(&cfg.rate, "rate", 5.0, "sustained: target claim arrival rate (claims/sec)")
	fs.DurationVar(&cfg.duration, "duration", 30*time.Second, "sustained: how long to keep arriving claims")
	fs.IntVar(&cfg.maxConcurrent, "max-concurrent", 0, "sustained: cap on simultaneously-live claims (0 = no cap)")
	fs.DurationVar(&cfg.timeout, "timeout", 120*time.Second, "per-claim timeout waiting for Ready / all-nodes-ready")
	fs.StringVar(&cfg.jsonPath, "json", "", "optional path to write the result JSON")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	switch cfg.mode {
	case modeClaimExec, modeSustained, modePoolRebuild:
	default:
		return config{}, fmt.Errorf("invalid --mode %q: want %s, %s, or %s", cfg.mode, modeClaimExec, modeSustained, modePoolRebuild)
	}
	if cfg.kubeconfig == "" {
		return config{}, fmt.Errorf("--kubeconfig is required")
	}
	if (cfg.mode == modeClaimExec || cfg.mode == modeSustained || cfg.mode == modePoolRebuild) && cfg.pool == "" {
		return config{}, fmt.Errorf("--pool is required for mode %q", cfg.mode)
	}
	if cfg.mode == modeClaimExec && cfg.iterations < 1 {
		return config{}, fmt.Errorf("--iterations must be >= 1, got %d", cfg.iterations)
	}
	if cfg.mode == modeSustained {
		if cfg.rate <= 0 {
			return config{}, fmt.Errorf("--rate must be > 0 in sustained mode, got %v", cfg.rate)
		}
		if cfg.duration <= 0 {
			return config{}, fmt.Errorf("--duration must be > 0 in sustained mode, got %v", cfg.duration)
		}
		if cfg.maxConcurrent < 0 {
			return config{}, fmt.Errorf("--max-concurrent must not be negative, got %d", cfg.maxConcurrent)
		}
	}
	return cfg, nil
}
