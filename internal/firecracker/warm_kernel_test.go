package firecracker

// Tests for the pre-snapshot kernel warmup (warm_kernel): the template build
// runs one trivial cell through Sandbox.RunCodeStream so the code-interpreter
// kernel (ipykernel behind /opt/mitos/kernel_driver.py) is already started
// when the snapshot is taken, and every fork wakes with a warm kernel instead
// of paying the ~5s lazy start on its first run_code.
//
// Strategy mirrors grpc_ready_test.go: an in-process gRPC server on a temp
// unix socket implements Control (Ping, for WaitReadyUnix) and Sandbox
// (RunCodeStream). Tests verify:
//   1. warmKernelGRPC opens RunCodeStream with the trivial python cell and
//      drains the reply stream to a zero exit_code (success).
//   2. Fork-correctness invariant: the warmup cell never touches random or
//      numpy, so the kernel's Python PRNGs stay unseeded in the snapshot and
//      each fork seeds fresh after the per-fork CRNG reseed.
//   3. A KernelUnavailable error frame (image without ipykernel) surfaces as
//      an error from warmKernelGRPC.
//   4. A nonzero exit without an error frame surfaces as an error.
//   5. maybeWarmKernel fails OPEN: a warmup error is logged, never returned,
//      so a non-python template build is unaffected.
//   6. maybeWarmKernel is a no-op when the flag is off.

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

// warmKernelSandboxServer is the in-process Sandbox gRPC server for the warmup
// tests. It records RunCodeStream requests and replies with the configured
// frames, so tests can drive success, KernelUnavailable, and nonzero-exit runs.
type warmKernelSandboxServer struct {
	sandboxv1.UnimplementedSandboxServer

	mu       sync.Mutex
	requests []*sandboxv1.RunCodeStreamRequest

	// frames are sent in order, then the stream closes (io.EOF client-side).
	frames []*sandboxv1.RunCodeResponse
	// streamErr, if non-nil, is returned instead of streaming frames.
	streamErr error
}

func (s *warmKernelSandboxServer) RunCodeStream(req *sandboxv1.RunCodeStreamRequest, stream sandboxv1.Sandbox_RunCodeStreamServer) error {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	frames := s.frames
	streamErr := s.streamErr
	s.mu.Unlock()

	if streamErr != nil {
		return streamErr
	}
	for _, f := range frames {
		if err := stream.Send(f); err != nil {
			return err
		}
	}
	return nil
}

// startWarmKernelGRPC starts an in-process gRPC server with Control (Ping) and
// the warmup Sandbox service on a temp unix socket.
func startWarmKernelGRPC(t *testing.T, sandbox *warmKernelSandboxServer) (sockPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc-warm-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	sockPath = filepath.Join(dir, "warm.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	grpcSrv := grpc.NewServer()
	internalv1.RegisterControlServer(grpcSrv, &recordingControlServerFC{})
	sandboxv1.RegisterSandboxServer(grpcSrv, sandbox)
	go grpcSrv.Serve(lis) //nolint:errcheck // test server; errors surface via RPC failures
	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

func newWarmTestTM(sockPath string) *TemplateManager {
	return &TemplateManager{
		waitReady: func(ctx context.Context, vsockPath string, timeout time.Duration) (*guestgrpc.Client, error) {
			return guestgrpc.WaitReadyUnix(ctx, sockPath, timeout)
		},
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) {},
	}
}

func exitFrame(code int32) *sandboxv1.RunCodeResponse {
	return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: code}}
}

// TestWarmKernelGRPC_DrainsToExitZero verifies the success path: the warmup
// opens RunCodeStream with the trivial python cell, drains stdout and the
// terminal exit_code frame, and returns nil on exit 0.
func TestWarmKernelGRPC_DrainsToExitZero(t *testing.T) {
	srv := &warmKernelSandboxServer{
		frames: []*sandboxv1.RunCodeResponse{
			{Msg: &sandboxv1.RunCodeResponse_Stdout{Stdout: []byte("")}},
			exitFrame(0),
		},
	}
	sockPath, cleanup := startWarmKernelGRPC(t, srv)
	defer cleanup()

	tm := newWarmTestTM(sockPath)
	if err := tm.warmKernelGRPC("vsock.sock"); err != nil {
		t.Fatalf("warmKernelGRPC: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 1 {
		t.Fatalf("expected exactly 1 RunCodeStream request, got %d", len(srv.requests))
	}
	req := srv.requests[0]
	if req.Language != "python" {
		t.Errorf("warmup language = %q, want %q", req.Language, "python")
	}
	if req.Code != WarmKernelCode {
		t.Errorf("warmup code = %q, want %q", req.Code, WarmKernelCode)
	}
	if req.TimeoutSeconds != WarmKernelTimeoutSecs {
		t.Errorf("warmup timeout = %d, want %d", req.TimeoutSeconds, WarmKernelTimeoutSecs)
	}
}

// TestWarmKernelCode_NeverDrawsRandomness pins that the warmup cell draws no
// randomness. That is hygiene, NOT the fork-correctness guarantee it was once
// documented to be: random.Random seeds itself in its constructor and the random module
// builds its shared instance at import, so ipykernel's own imports already fix the
// Mersenne state before this cell runs. The guarantee lives in the guest, where
// kernel_driver.py reseeds the kernel's PRNGs on the post-fork SIGUSR2 (see
// guest/rootfs/kernel_driver_test.py). Keeping this cell inert keeps the snapshot free
// of randomness the driver does not know how to reseed.
func TestWarmKernelCode_NeverDrawsRandomness(t *testing.T) {
	for _, banned := range []string{"random", "numpy", "np.", "import", "uuid", "secrets"} {
		if strings.Contains(WarmKernelCode, banned) {
			t.Errorf("WarmKernelCode %q must not contain %q: the warmup cell must draw no randomness, so the snapshot holds no PRNG state the guest-side post-fork reseed does not know how to reseed. It does NOT keep the kernel's PRNGs unseeded: ipykernel's own imports seed random before this cell runs", WarmKernelCode, banned)
		}
	}
}

// TestWarmKernelGRPC_KernelUnavailableErrors verifies that an image without
// the kernel (the guest replies with a KernelUnavailable error frame and exit
// 127, see guest/agent-rs/src/kernel/driver.rs) surfaces as an error naming
// the kernel failure. The fail-open decision lives in maybeWarmKernel.
func TestWarmKernelGRPC_KernelUnavailableErrors(t *testing.T) {
	srv := &warmKernelSandboxServer{
		frames: []*sandboxv1.RunCodeResponse{
			{Msg: &sandboxv1.RunCodeResponse_Error{Error: &sandboxv1.RunError{
				Name:  "KernelUnavailable",
				Value: "kernel unavailable: driver /opt/mitos/kernel_driver.py not found",
			}}},
			exitFrame(127),
		},
	}
	sockPath, cleanup := startWarmKernelGRPC(t, srv)
	defer cleanup()

	tm := newWarmTestTM(sockPath)
	err := tm.warmKernelGRPC("vsock.sock")
	if err == nil {
		t.Fatal("expected an error for a KernelUnavailable warmup, got nil")
	}
	if !strings.Contains(err.Error(), "KernelUnavailable") {
		t.Errorf("error %q should name KernelUnavailable", err)
	}
}

// TestWarmKernelGRPC_NonzeroExitErrors verifies a nonzero exit without an
// error frame is still reported as a failed warmup.
func TestWarmKernelGRPC_NonzeroExitErrors(t *testing.T) {
	srv := &warmKernelSandboxServer{frames: []*sandboxv1.RunCodeResponse{exitFrame(1)}}
	sockPath, cleanup := startWarmKernelGRPC(t, srv)
	defer cleanup()

	tm := newWarmTestTM(sockPath)
	err := tm.warmKernelGRPC("vsock.sock")
	if err == nil {
		t.Fatal("expected an error for a nonzero warmup exit, got nil")
	}
	if !strings.Contains(err.Error(), "exited 1") {
		t.Errorf("error %q should report the nonzero exit", err)
	}
}

// TestWarmKernelGRPC_MissingExitErrors verifies a stream that ends without a
// terminal exit_code frame is reported as a failed warmup (the kernel never
// confirmed the cell ran).
func TestWarmKernelGRPC_MissingExitErrors(t *testing.T) {
	srv := &warmKernelSandboxServer{frames: []*sandboxv1.RunCodeResponse{
		{Msg: &sandboxv1.RunCodeResponse_Stdout{Stdout: []byte("partial")}},
	}}
	sockPath, cleanup := startWarmKernelGRPC(t, srv)
	defer cleanup()

	tm := newWarmTestTM(sockPath)
	if err := tm.warmKernelGRPC("vsock.sock"); err == nil {
		t.Fatal("expected an error for a warmup stream without an exit_code frame, got nil")
	}
}

// TestMaybeWarmKernel_FailsOpen verifies the build-safety contract: a warmup
// failure (no kernel in the image, a transport hiccup) is logged and the build
// CONTINUES, so warm_kernel can never break a non-python template build.
func TestMaybeWarmKernel_FailsOpen(t *testing.T) {
	called := 0
	tm := &TemplateManager{
		warmKernel: func(vsockPath string) error {
			called++
			return errors.New("kernel warmup cell failed: KernelUnavailable: no ipykernel in this image")
		},
	}
	// maybeWarmKernel must swallow the error; a panic or process exit here
	// would fail the test.
	tm.maybeWarmKernel("tmpl-warm", "vsock.sock", true)
	if called != 1 {
		t.Fatalf("expected the warmup seam to be called once, got %d", called)
	}
}

// TestMaybeWarmKernel_DisabledSkips verifies the warmup never runs when the
// warm_kernel flag is off (the default), keeping existing builds byte-for-byte
// unaffected.
func TestMaybeWarmKernel_DisabledSkips(t *testing.T) {
	called := 0
	tm := &TemplateManager{
		warmKernel: func(vsockPath string) error {
			called++
			return nil
		},
	}
	tm.maybeWarmKernel("tmpl-cold", "vsock.sock", false)
	if called != 0 {
		t.Fatalf("expected no warmup call when disabled, got %d", called)
	}
}
