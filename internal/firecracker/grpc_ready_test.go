package firecracker

// Tests for the gRPC Control readiness probe and gRPC Sandbox.ExecStream
// init-command path in connectInitExecGRPC.
//
// Strategy: stand up an in-process gRPC server on a temp unix socket that
// implements BOTH sandbox.internal.v1.Control (Ping) AND sandbox.v1.Sandbox
// (ExecStream). Tests verify:
//   1. Readiness: waitReady calls Control.Ping on the in-process server.
//   2. ExecStream: init commands are run via Sandbox.ExecStream (not JSON vsock).
//   3. Zero-exit success: a command that exits 0 causes exec to return no error.
//   4. Non-zero exit failure: a command that exits non-zero causes exec to fail.
//   5. Stream error failure: a stream error on ExecStream causes exec to fail.
//   6. Timeout: connectInitExecGRPC bounds when the gRPC server is unreachable.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/guestgrpc"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
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

// recordingSandboxServerFC is the in-process Sandbox gRPC server for the
// firecracker-package tests. It implements ExecStream and records the requests
// it receives so tests can assert the correct command and cwd were sent.
type recordingSandboxServerFC struct {
	sandboxv1.UnimplementedSandboxServer

	mu       sync.Mutex
	requests []*sandboxv1.ExecStreamRequest

	// exitCode is the exit code the server sends in the terminal Exit frame.
	exitCode int32
	// streamErr, if non-nil, is returned from ExecStream instead of streaming.
	streamErr error
	// stderr is sent as a stderr frame before the Exit frame.
	stderr []byte
}

func (s *recordingSandboxServerFC) ExecStream(req *sandboxv1.ExecStreamRequest, stream sandboxv1.Sandbox_ExecStreamServer) error {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	exitCode := s.exitCode
	streamErr := s.streamErr
	stderr := s.stderr
	s.mu.Unlock()

	if streamErr != nil {
		return streamErr
	}
	if len(stderr) > 0 {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: stderr}}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: exitCode}}})
}

// fullGRPCServer bundles both the Control and Sandbox in-process servers.
type fullGRPCServer struct {
	ctrl    *recordingControlServerFC
	sandbox *recordingSandboxServerFC
}

// startFullGRPC starts an in-process gRPC server with both Control and Sandbox
// services on a temp unix socket and returns the socket path and a cleanup func.
func startFullGRPC(t *testing.T, s *fullGRPCServer) (sockPath string, cleanup func()) {
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
	internalv1.RegisterControlServer(grpcSrv, s.ctrl)
	sandboxv1.RegisterSandboxServer(grpcSrv, s.sandbox)
	go grpcSrv.Serve(lis) //nolint:errcheck // test server; errors surface via RPC failures
	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

// newGRPCTestTM builds a TemplateManager with the waitReady seam pointing at a
// unix socket (no real vsock). Used by the gRPC ExecStream tests.
func newGRPCTestTM(sockPath string) *TemplateManager {
	return &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
			return guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
		},
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}
}

// TestConnectInitExec_GRPCPingReadiness verifies that connectInitExecGRPC calls
// the waitReady seam (which drives Control.Ping) before returning the exec func.
func TestConnectInitExec_GRPCPingReadiness(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl:    &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{exitCode: 0},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	waitReadyCalled := false
	tm := &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
			client, err := guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
			if err != nil {
				return nil, err
			}
			waitReadyCalled = true
			return client, nil
		},
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}

	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	defer closeFn()

	if !waitReadyCalled {
		t.Error("expected waitReady to be called for gRPC Control.Ping readiness")
	}

	srv.ctrl.mu.Lock()
	pings := srv.ctrl.pingCalls
	srv.ctrl.mu.Unlock()
	if pings < 1 {
		t.Errorf("expected at least 1 Ping on Control server, got %d", pings)
	}
	if exec == nil {
		t.Error("expected non-nil exec func on success")
	}
}

// TestConnectInitExec_ExecStreamCalledWithCommandAndCwd verifies that when the
// returned exec func is called, it sends an ExecStream request with the correct
// command and cwd (/workspace) to the Sandbox service on the SAME gRPC client
// used for readiness (no second vsock connection is opened).
func TestConnectInitExec_ExecStreamCalledWithCommandAndCwd(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl:    &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{exitCode: 0},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	tm := newGRPCTestTM(sockPath)
	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	defer closeFn()

	res, err := exec("pip install numpy")
	if err != nil {
		t.Fatalf("exec returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}

	srv.sandbox.mu.Lock()
	reqs := srv.sandbox.requests
	srv.sandbox.mu.Unlock()

	if len(reqs) != 1 {
		t.Fatalf("expected 1 ExecStream request, got %d", len(reqs))
	}
	if reqs[0].Command != "pip install numpy" {
		t.Errorf("expected command %q, got %q", "pip install numpy", reqs[0].Command)
	}
	if reqs[0].Cwd != "/workspace" {
		t.Errorf("expected cwd /workspace, got %q", reqs[0].Cwd)
	}
	wantTimeout := int32(initExecTimeoutSecs)
	if reqs[0].TimeoutSeconds != wantTimeout {
		t.Errorf("expected timeout_seconds %d, got %d", wantTimeout, reqs[0].TimeoutSeconds)
	}
}

// TestConnectInitExec_ZeroExitSucceeds verifies that a command that exits 0
// causes the exec func to return nil error, matching the old JSON Exec behavior.
func TestConnectInitExec_ZeroExitSucceeds(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl:    &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{exitCode: 0},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	tm := newGRPCTestTM(sockPath)
	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	defer closeFn()

	res, err := exec("echo hello")
	if err != nil {
		t.Fatalf("exec returned error for zero exit: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
}

// TestConnectInitExec_NonZeroExitInResponse verifies that a non-zero exit code
// from ExecStream is surfaced in the ExecResponse.ExitCode field (not as an
// error), exactly matching vsock.Client.Exec semantics. runInitCommands then
// treats it as a hard failure.
func TestConnectInitExec_NonZeroExitInResponse(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl: &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{
			exitCode: 1,
			stderr:   []byte("No matching distribution found"),
		},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	tm := newGRPCTestTM(sockPath)
	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	defer closeFn()

	res, err := exec("pip install nope")
	if err != nil {
		t.Fatalf("non-zero exit must be in ExecResponse, not an error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "No matching distribution found") {
		t.Errorf("expected stderr to contain failure message, got %q", res.Stderr)
	}
}

// TestConnectInitExec_StreamError verifies that a gRPC stream error on
// ExecStream is surfaced as a non-nil error from the exec func, triggering the
// hard-error branch in runInitCommands.
func TestConnectInitExec_StreamError(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl: &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{
			streamErr: errors.New("guest agent crashed"),
		},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	tm := newGRPCTestTM(sockPath)
	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	defer closeFn()

	_, err = exec("echo hello")
	if err == nil {
		t.Fatal("expected error from exec when ExecStream returns an error")
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

// TestConnectInitExec_GRPCPingAndExecFunc verifies the happy path: when
// waitReady succeeds, connectInitExecGRPC returns a non-nil exec func and a
// non-nil cleanup.
func TestConnectInitExec_GRPCPingAndExecFunc(t *testing.T) {
	srv := &fullGRPCServer{
		ctrl:    &recordingControlServerFC{},
		sandbox: &recordingSandboxServerFC{exitCode: 0},
	}
	sockPath, cleanup := startFullGRPC(t, srv)
	defer cleanup()

	tm := newGRPCTestTM(sockPath)
	exec, closeFn, err := tm.connectInitExecGRPC("vsock.sock")
	if err != nil {
		t.Fatalf("connectInitExecGRPC: %v", err)
	}
	if exec == nil {
		t.Error("expected non-nil exec func on success")
	}
	if closeFn == nil {
		t.Error("expected non-nil cleanup func on success")
	}
	closeFn()
}
