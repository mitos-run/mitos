package fork

// Tests for the gRPC Control readiness probe migration in captureExecBestEffort
// (used by CaptureTemplateHotPages via the captureGuestReady Engine seam).
//
// Strategy: stand up an in-process gRPC Control + Sandbox server on a temp
// unix socket and verify that the readiness gate drives Control.Ping and that
// the best-effort /bin/true exec reaches the Sandbox.ExecStream handler. The
// captureGuestReady seam is injected with guestgrpc.WaitReadyUnix so no real
// Firecracker or vsock is needed.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/guestgrpc"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// controlAndSandboxServer is an in-process server that implements both
// sandbox.internal.v1.Control (for Ping) and sandbox.v1.Sandbox (for ExecStream),
// recording calls so tests can assert the right RPCs were made.
type controlAndSandboxServer struct {
	internalv1.UnimplementedControlServer
	sandboxv1.UnimplementedSandboxServer

	mu        sync.Mutex
	pingCalls int
	execCmds  []string // commands seen via ExecStream
}

func (s *controlAndSandboxServer) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pingCalls++
	return &internalv1.PingResponse{UptimeSeconds: 1.0}, nil
}

func (s *controlAndSandboxServer) ExecStream(req *sandboxv1.ExecStreamRequest, stream grpc.ServerStreamingServer[sandboxv1.ExecResponse]) error {
	s.mu.Lock()
	s.execCmds = append(s.execCmds, req.GetCommand())
	s.mu.Unlock()
	// Return exit code 0 so the client drains the stream cleanly.
	return stream.Send(&sandboxv1.ExecResponse{
		Msg: &sandboxv1.ExecResponse_Exit{
			Exit: &sandboxv1.ExecExit{ExitCode: 0},
		},
	})
}

// startControlAndSandboxGRPC starts an in-process gRPC server implementing both
// Control and Sandbox on a temp unix socket and returns the socket path and cleanup.
func startControlAndSandboxGRPC(t *testing.T, srv *controlAndSandboxServer) (sockPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fork-grpc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	sockPath = filepath.Join(dir, "agent.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	grpcSrv := grpc.NewServer()
	internalv1.RegisterControlServer(grpcSrv, srv)
	sandboxv1.RegisterSandboxServer(grpcSrv, srv)
	go grpcSrv.Serve(lis) //nolint:errcheck // test server; errors surface via RPC failures
	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

// TestCaptureExecBestEffort_PingAndExecStream verifies that captureExecBestEffort
// drives Control.Ping for readiness (via the injectable captureGuestReady seam) and
// then calls Sandbox.ExecStream with /bin/true to drive the first-exec working set.
// No real Firecracker or vsock is needed.
func TestCaptureExecBestEffort_PingAndExecStream(t *testing.T) {
	srv := &controlAndSandboxServer{}
	sockPath, cleanup := startControlAndSandboxGRPC(t, srv)
	defer cleanup()

	// Inject the unix-socket variant of WaitReady so no vsock is needed.
	seam := func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
		return guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
	}

	captureExecBestEffort(seam, "vsock.sock")

	// Give the server goroutine a moment to record the ExecStream call before
	// checking (the handler runs asynchronously after the client drain loop).
	time.Sleep(50 * time.Millisecond)

	srv.mu.Lock()
	pings := srv.pingCalls
	cmds := append([]string{}, srv.execCmds...)
	srv.mu.Unlock()

	if pings < 1 {
		t.Errorf("expected at least 1 Ping call for readiness, got %d", pings)
	}
	if len(cmds) < 1 || cmds[0] != "/bin/true" {
		t.Errorf("expected ExecStream with /bin/true, got %v", cmds)
	}
}

// TestCaptureExecBestEffort_GuestNotReady verifies that captureExecBestEffort is
// best-effort: when the guest is not reachable (timeout) the function returns
// without error and without panicking. The caller's working set (already faulted
// in at resume) is still captured.
func TestCaptureExecBestEffort_GuestNotReady(t *testing.T) {
	dir, err := os.MkdirTemp("", "fork-grpc-timeout-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	noSock := filepath.Join(dir, "nosuchsocket.sock")

	seam := func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
		return guestgrpc.WaitReadyUnix(ctx, noSock, 150*time.Millisecond)
	}

	start := time.Now()
	// Must not panic and must not block indefinitely.
	captureExecBestEffort(seam, "vsock.sock")
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("captureExecBestEffort should return quickly when guest unreachable, took %v", elapsed)
	}
}
