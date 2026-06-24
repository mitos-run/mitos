//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestMain intercepts the test binary entry point. When the environment variable
// AGENT_UNIX_SOCK is set, it starts the guest gRPC services on that Unix domain
// socket and blocks until SIGTERM/SIGINT. This mode is used exclusively by the
// cross-agent conformance harness (bench/agent-conformance) to run the Go agent
// as a unix-socket server on box2, where there is no vsock.
//
// When AGENT_UNIX_SOCK is not set, TestMain runs the normal test suite
// (m.Run()) so the existing agent tests are not affected.
//
// The AGENT_WORKSPACE env var, when set, overrides workspaceRoot for the
// duration of the server run. The conformance harness sets this to /tmp so
// the workspace allowlist in pathAllowed passes for /tmp paths without needing
// a real /workspace in the test environment.
func TestMain(m *testing.M) {
	sockPath := os.Getenv("AGENT_UNIX_SOCK")
	if sockPath == "" {
		// Normal test run.
		os.Exit(m.Run())
	}

	// Override the workspace root if requested (harness sets to /tmp).
	if ws := os.Getenv("AGENT_WORKSPACE"); ws != "" {
		workspaceRoot = ws
	}

	// Install a no-op signalUserspace so NotifyForked does not broadcast
	// SIGUSR2 to host processes when the conformance server runs on box2.
	// The real signal broadcast is gated by the vsock path in production.
	signalUserspaceImpl = func() int { return 0 }

	// Remove any leftover socket from a prior run.
	_ = os.Remove(sockPath)

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance-server: listen: %v\n", err)
		os.Exit(1)
	}

	srv := newGuestGRPCServer()

	// Signal "ready" so the harness knows it can connect.
	fmt.Println("READY")

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(lis)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		srv.GracefulStop()
	case serveErr := <-done:
		if serveErr != nil {
			fmt.Fprintf(os.Stderr, "conformance-server: serve: %v\n", serveErr)
			os.Exit(1)
		}
	}

	_ = os.Remove(sockPath)
}
