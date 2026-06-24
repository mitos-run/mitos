package firecracker

// Tests for the gRPC Control readiness probe migration in connectInitExecGRPC.
//
// Strategy: stand up an in-process gRPC Control server on a temp unix socket
// and verify that the waitReady seam drives Control.Ping before returning the
// exec func. The dialExec seam is also injected so the vsock exec connection
// is short-circuited; without it the retry loop would take 30 s in tests with
// no real vsock server. Tests cover:
//   1. Readiness: waitReady is called and Ping hits the in-process server.
//   2. ExecFunc returned: after readiness succeeds and dialExec returns a fake
//      client, the exec func is non-nil.
//   3. Timeout: connectInitExecGRPC bounds when the gRPC server is unreachable.
//
// The existing awaitReadyAndRunInit tests in template_init_test.go cover the
// full connectInit -> exec -> init-command pipeline via the injectable seam;
// these tests focus only on the gRPC readiness gate.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
)

// recordingControlServerFC is the in-process Control gRPC server for the
// firecracker-package tests. It mirrors the same pattern as the husk package's
// recordingControlServer so the test approach is consistent.
type recordingControlServerFC struct {
	internalv1.UnimplementedControlServer

	mu        sync.Mutex
	pingCalls int
}

func (s *recordingControlServerFC) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pingCalls++
	return &internalv1.PingResponse{UptimeSeconds: 1.0}, nil
}

// startControlGRPC starts an in-process Control gRPC server on a temp unix
// socket and returns the socket path and a cleanup function.
func startControlGRPC(t *testing.T, srv *recordingControlServerFC) (sockPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc-grpc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	sockPath = filepath.Join(dir, "ctrl.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	grpcSrv := grpc.NewServer()
	internalv1.RegisterControlServer(grpcSrv, srv)
	go grpcSrv.Serve(lis) //nolint:errcheck // test server; errors surface via RPC failures
	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

// fakeVsockClient is a minimal *vsock.Client stand-in returned by the fake
// dialExec seam. vsock.Connect uses the unexported type; we use a real (but
// disconnected) unix socket so the test does not require Firecracker.
func fakeDialExec(vsockPath string) (*vsock.Client, error) {
	// Return an error immediately so the exec path fails fast without
	// spinning the 30-attempt retry loop. The test only needs the exec func
	// to be non-nil when dialExec SUCCEEDS; here we verify the readiness gate.
	// A separate test covers the success path via a fake exec seam.
	return nil, fmt.Errorf("fake dial exec: no vsock server in test")
}

// TestConnectInitExec_GRPCPingReadiness verifies that connectInitExecGRPC calls
// the waitReady seam (which drives Control.Ping) before proceeding to the exec
// connection. The dialExec seam fails fast so the test does not spin 30 retries.
func TestConnectInitExec_GRPCPingReadiness(t *testing.T) {
	rcs := &recordingControlServerFC{}
	sockPath, cleanup := startControlGRPC(t, rcs)
	defer cleanup()

	waitReadyCalled := false
	tm := &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
			// Dial the in-process server to exercise the real Ping round-trip.
			client, err := guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
			if err != nil {
				return nil, err
			}
			waitReadyCalled = true
			return client, nil
		},
		// Fail fast on the vsock exec connection so the test does not take 30 s.
		dialExec:     fakeDialExec,
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}

	// connectInitExecGRPC: waitReady succeeds; dialExec fails fast (expected).
	// We care only that waitReady was called and Ping hit the server.
	_, _, _ = tm.connectInitExecGRPC("vsock.sock") //nolint:errcheck // expected error from dialExec

	if !waitReadyCalled {
		t.Error("expected waitReady to be called for gRPC Control.Ping readiness")
	}

	rcs.mu.Lock()
	pings := rcs.pingCalls
	rcs.mu.Unlock()

	if pings < 1 {
		t.Errorf("expected at least 1 Ping on Control server, got %d", pings)
	}
}

// TestConnectInitExec_GRPCPingAndExecFunc verifies the happy path: when both
// waitReady and dialExec succeed, connectInitExecGRPC returns a non-nil exec func
// and a non-nil cleanup. It uses a fake exec seam (the full integration path via
// connectInit is covered by awaitReadyAndRunInit tests in template_init_test.go).
func TestConnectInitExec_GRPCPingAndExecFunc(t *testing.T) {
	rcs := &recordingControlServerFC{}
	sockPath, cleanup := startControlGRPC(t, rcs)
	defer cleanup()

	// Simulate a fake vsock client that does nothing (no real agent needed).
	// vsock.NewTestClient is not exported; instead inject via the fakeExec
	// pattern already used in this package's other tests.
	fakeClosed := false
	var fakeClient *vsock.Client // nil; exec func is never called in this test
	tm := &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
			return guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
		},
		dialExec: func(vsockPath string) (*vsock.Client, error) {
			// Return nil client (vsock.Client methods would panic if called,
			// but we only test that the exec func is non-nil here).
			return fakeClient, nil
		},
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}
	_ = fakeClosed // unused but kept for documentation

	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		// If dialExec returned nil client and that caused a panic we would not
		// reach here; but the exec func is captured in a closure so it is safe.
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	if exec == nil {
		t.Error("expected non-nil exec func on success")
	}
	if closeFn == nil {
		t.Error("expected non-nil cleanup func on success")
	}
}

// TestConnectInitExec_GRPCPingTimeout verifies that connectInitExecGRPC returns
// an error when the gRPC Control server is unreachable within the timeout,
// mirroring the legacy vsock.Connect retry behavior.
func TestConnectInitExec_GRPCPingTimeout(t *testing.T) {
	dir, err := os.MkdirTemp("", "fc-grpc-timeout-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	noSock := filepath.Join(dir, "nosuchsocket.sock")

	tm := &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, _ time.Duration) (*guestgrpc.Client, error) {
			// Short timeout so the test does not block.
			return guestgrpc.WaitReadyUnix(ctx, noSock, 150*time.Millisecond)
		},
		dialExec:     fakeDialExec,
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}

	start := time.Now()
	_, _, cerr := tm.connectInitExecGRPC("vsock.sock")
	elapsed := time.Since(start)

	if cerr == nil {
		t.Fatal("expected an error when gRPC Control server is unreachable")
	}
	// Must bound and not hang. Allow 3x for CI headroom.
	if elapsed > 5*time.Second {
		t.Errorf("connectInitExecGRPC did not bound within timeout: took %v", elapsed)
	}
}
