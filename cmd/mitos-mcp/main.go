// Command mitos-mcp exposes the mitos sandbox lifecycle (create, exec,
// file IO, fork, terminate) as Model Context Protocol tools over a stdio
// JSON-RPC transport. Any MCP-speaking agent can drive sandboxes through it
// without an SDK integration.
//
// It speaks MCP on stdin/stdout: stdout is the JSON-RPC channel and carries
// nothing else. ALL logging goes to stderr. The launch-time bearer token
// (--token / MITOS_API_KEY) scopes what the server can do on the backend and
// is NEVER logged.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mitos.run/mitos/internal/credfile"
	"mitos.run/mitos/internal/mcp"
)

func main() {
	// Default to the hosted production control plane. Self-hosted or local
	// standalone users opt out by setting MITOS_BASE_URL or passing --server.
	defaultServer := envOr("MITOS_BASE_URL", "https://mitos.run")
	defaultToken := os.Getenv("MITOS_API_KEY")

	server := flag.String("server", defaultServer, "Base URL of the sandbox-server (env MITOS_BASE_URL).")
	token := flag.String("token", defaultToken, "Bearer token; scopes what this server may do (env MITOS_API_KEY). Never logged.")
	enableWorkspace := flag.Bool("enable-workspace-tools", false, "Advertise the workspace tools in tools/list (dispatch deferred, issue #21).")
	flag.Parse()

	// Precedence: --token (or its MITOS_API_KEY default) wins; when both are
	// empty, fall back to the CLI login profile written by `mitos auth login`,
	// so one login authenticates the mcp server too. A missing file is not an
	// error: the server then runs tokenless against a standalone server. The
	// token VALUE is never logged.
	if *token == "" {
		if t, err := credfile.Token(); err == nil {
			*token = t
		}
	}

	// Log to stderr only: stdout is the MCP JSON-RPC channel. Never log the token.
	logger := log.New(os.Stderr, "mitos-mcp ", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	backend := mcp.NewHTTPBackend(*server, *token, nil)
	srv := mcp.New(backend, mcp.Options{EnableWorkspaceTools: *enableWorkspace})

	logger.Printf("starting: server=%s workspace_tools=%v token=%s", *server, *enableWorkspace, tokenState(*token))

	if err := srv.Run(ctx, os.Stdin, os.Stdout); err != nil {
		logger.Printf("server stopped: %v", err)
		os.Exit(1)
	}
}

// envOr returns the environment variable value or a fallback default.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// tokenState reports whether a token is configured without ever revealing it.
func tokenState(token string) string {
	if token == "" {
		return "unset"
	}
	return "set"
}
