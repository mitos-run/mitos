package main

import (
	"context"
	"os"
	"time"

	"golang.org/x/term"
	"mitos.run/mitos/internal/agentcli"
)

// initValidateTimeout bounds the single validation call `mitos init` makes so a
// wrong endpoint fails with a clear error instead of hanging the first-run.
const initValidateTimeout = 30 * time.Second

// runInitCmd wires the real seams for `mitos init`: the environment defaults,
// the gateway key validator, and the no-echo terminal read. flagKey and flagURL
// are the global --api-key/--server values; the environment is the fallback
// (flag wins over env), matching the precedence of every other hosted verb.
func runInitCmd(ctx context.Context, flagKey, flagURL string, args []string) int {
	defaults := agentcli.InitOptions{APIKey: flagKey, Endpoint: flagURL}
	if defaults.APIKey == "" {
		defaults.APIKey = os.Getenv("MITOS_API_KEY")
	}
	if defaults.Endpoint == "" {
		defaults.Endpoint = os.Getenv("MITOS_BASE_URL")
	}

	deps := agentcli.InitDeps{
		// Validate issues the cheapest authenticated call the gateway serves
		// (GET /v1/sandboxes). HostedBackend redacts the key from any error, so
		// the returned error is safe to print.
		Validate: func(ctx context.Context, endpoint, apiKey string) error {
			vctx, cancel := context.WithTimeout(ctx, initValidateTimeout)
			defer cancel()
			_, err := agentcli.NewHostedBackend(endpoint, apiKey, nil).List(vctx, "")
			return err
		},
	}
	// Only offer the interactive paste when stdin is a real terminal; the read
	// is no-echo so the key never appears on screen or in scrollback.
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		deps.ReadKey = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
	}
	return agentcli.CmdInit(ctx, args, defaults, deps, os.Stdout, os.Stderr)
}
