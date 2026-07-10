package husk

// Tests for the gRPC Control service migration: productionNotifier and
// productionGuestReady drive the Control service (Ping, NotifyForked, Configure)
// instead of the legacy JSON guest protocol on vsock.AgentPort 52.
//
// Strategy: stand up an in-process gRPC Control server on a temp unix socket,
// wire the production seams to it via the injectable dial func, and assert that
// NotifyForked and Configure are called with the correct field values. The test
// never touches a real VM or vsock; it exercises only the protocol layer and
// field mapping.
//
// Secret hygiene: entropy bytes and secret values are recorded by the fake
// server for structural assertions (length, key presence) only. The production
// code path never logs them, which is verified by
// TestActivateNeverLogsEntropyOrSecrets in stub_test.go.

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

// recordingControlServer is an in-process sandbox.internal.v1.Control
// implementation that records every RPC call for structural assertion.
// It is ONLY used in tests; secret values recorded here are never logged.
type recordingControlServer struct {
	internalv1.UnimplementedControlServer

	mu sync.Mutex

	pingCalls int

	notifyCalls    int
	gotNotifyReq   *internalv1.NotifyForkedRequest
	notifyErr      error
	notifyReseeded bool // returned in NotifyForkedResponse.ReseededRng

	configureCalls  int
	gotConfigureReq *internalv1.ConfigureRequest
	configureErr    error
}

func (s *recordingControlServer) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pingCalls++
	return &internalv1.PingResponse{UptimeSeconds: 1.0}, nil
}

func (s *recordingControlServer) NotifyForked(_ context.Context, req *internalv1.NotifyForkedRequest) (*internalv1.NotifyForkedResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyCalls++
	s.gotNotifyReq = req
	if s.notifyErr != nil {
		return nil, s.notifyErr
	}
	return &internalv1.NotifyForkedResponse{ReseededRng: s.notifyReseeded}, nil
}

func (s *recordingControlServer) Configure(_ context.Context, req *internalv1.ConfigureRequest) (*internalv1.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configureCalls++
	s.gotConfigureReq = req
	if s.configureErr != nil {
		return nil, s.configureErr
	}
	return &internalv1.ConfigureResponse{}, nil
}

// startRecordingGRPC starts an in-process Control gRPC server on a temp unix
// socket and returns the socket path and a cleanup function. The caller is
// responsible for invoking cleanup.
func startRecordingGRPC(t *testing.T, srv *recordingControlServer) (sockPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "husk-grpc-")
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
	go grpcSrv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures

	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

// dialUnix is a guestgrpc.DialFunc that dials over a unix socket instead of a
// vsock (Firecracker) connection. Injected into the production functions during
// testing so no real VM or vsock is needed.
func dialUnix(sockPath string) (*guestgrpc.Client, error) {
	return guestgrpc.DialUnix(sockPath)
}

// TestProductionNotifierGRPC_NotifyForkedFields verifies that notifierGRPC
// sends NotifyForked with the correct generation, entropy length, network fields
// (all five), and volume table, and then sends Configure with env+secrets, using
// the gRPC Control service over a unix socket.
func TestProductionNotifierGRPC_NotifyForkedFields(t *testing.T) {
	rcs := &recordingControlServer{notifyReseeded: true}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	net0 := &vsock.NotifyForkedNetwork{
		GuestIP:    "10.200.1.2",
		GatewayIP:  "10.200.1.1",
		PrefixLen:  30,
		GuestMAC:   "02:ab:cd:ef:01:02",
		ResolverIP: "169.254.1.1",
	}
	vols := []vsock.VolumeMountEntry{
		{Device: "/dev/vdb", MountPath: "/data", ReadOnly: false},
		{Device: "/dev/vdc", MountPath: "/ro", ReadOnly: true},
	}
	req := ActivateRequest{
		Env:     map[string]string{"LANG": "en_US.UTF-8"},
		Secrets: map[string]string{"API_KEY": "s3cr3t"},
		Network: net0,
		Volumes: vols,
	}

	entropy := make([]byte, entropySize)
	for i := range entropy {
		entropy[i] = byte(i + 1) // deterministic, non-zero
	}

	if err := notifierGRPC(nil, sockPath, 7, entropy, req, dialUnix); err != nil {
		t.Fatalf("notifierGRPC: %v", err)
	}

	rcs.mu.Lock()
	defer rcs.mu.Unlock()

	// NotifyForked must be called exactly once.
	if rcs.notifyCalls != 1 {
		t.Fatalf("NotifyForked called %d times, want 1", rcs.notifyCalls)
	}
	got := rcs.gotNotifyReq
	if got == nil {
		t.Fatal("NotifyForkedRequest is nil")
	}

	// Generation must match the call argument.
	if got.Generation != 7 {
		t.Errorf("generation = %d, want 7", got.Generation)
	}
	// Entropy must be delivered with the correct byte count.
	// The VALUE is sensitive and must not be logged; assert length only.
	if len(got.Entropy) != entropySize {
		t.Errorf("entropy length = %d, want %d", len(got.Entropy), entropySize)
	}
	// HostWallClockNanos must be non-zero (set to time.Now().UnixNano()).
	if got.HostWallClockNanos == 0 {
		t.Error("host_wall_clock_nanos must not be zero")
	}

	// All five network fields must map exactly from vsock.NotifyForkedNetwork.
	if got.Network == nil {
		t.Fatal("network is nil; must carry per-fork identity")
	}
	n := got.Network
	if n.GuestIp != net0.GuestIP {
		t.Errorf("network.guest_ip = %q, want %q", n.GuestIp, net0.GuestIP)
	}
	if n.GatewayIp != net0.GatewayIP {
		t.Errorf("network.gateway_ip = %q, want %q", n.GatewayIp, net0.GatewayIP)
	}
	if n.PrefixLen != int32(net0.PrefixLen) {
		t.Errorf("network.prefix_len = %d, want %d", n.PrefixLen, net0.PrefixLen)
	}
	if n.GuestMac != net0.GuestMAC {
		t.Errorf("network.guest_mac = %q, want %q", n.GuestMac, net0.GuestMAC)
	}
	if n.ResolverIp != net0.ResolverIP {
		t.Errorf("network.resolver_ip = %q, want %q", n.ResolverIp, net0.ResolverIP)
	}

	// Volume table: two entries with device, mount_path, read_only.
	if len(got.Volumes) != len(vols) {
		t.Fatalf("volumes len = %d, want %d", len(got.Volumes), len(vols))
	}
	for i, v := range got.Volumes {
		if v.Device != vols[i].Device {
			t.Errorf("volume[%d].device = %q, want %q", i, v.Device, vols[i].Device)
		}
		if v.MountPath != vols[i].MountPath {
			t.Errorf("volume[%d].mount_path = %q, want %q", i, v.MountPath, vols[i].MountPath)
		}
		if v.ReadOnly != vols[i].ReadOnly {
			t.Errorf("volume[%d].read_only = %v, want %v", i, v.ReadOnly, vols[i].ReadOnly)
		}
	}

	// Configure must be called exactly once (env and secrets are present).
	if rcs.configureCalls != 1 {
		t.Fatalf("Configure called %d times, want 1", rcs.configureCalls)
	}
	cfg := rcs.gotConfigureReq
	if cfg == nil {
		t.Fatal("ConfigureRequest is nil")
	}
	// Env key present with correct value.
	if cfg.Env["LANG"] != "en_US.UTF-8" {
		t.Errorf("configure env LANG = %q, want %q", cfg.Env["LANG"], "en_US.UTF-8")
	}
	// Secret key must be present; the VALUE is sensitive and must not appear
	// in any log (enforced by TestActivateNeverLogsEntropyOrSecrets).
	if _, ok := cfg.Secrets["API_KEY"]; !ok {
		t.Error("secret API_KEY not delivered to Configure")
	}
}

// TestProductionNotifierGRPC_SkipsConfigureWhenEmpty verifies that Configure is
// NOT called when the ActivateRequest has no env or secrets, matching the
// legacy JSON path's "skip when there is nothing to deliver" behavior.
func TestProductionNotifierGRPC_SkipsConfigureWhenEmpty(t *testing.T) {
	rcs := &recordingControlServer{notifyReseeded: true}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	entropy := make([]byte, entropySize)
	req := ActivateRequest{
		Network: &vsock.NotifyForkedNetwork{
			GuestIP: "10.0.0.2", GatewayIP: "10.0.0.1", PrefixLen: 30,
		},
		// No Env or Secrets.
	}

	if err := notifierGRPC(nil, sockPath, 1, entropy, req, dialUnix); err != nil {
		t.Fatalf("notifierGRPC: %v", err)
	}

	rcs.mu.Lock()
	defer rcs.mu.Unlock()
	if rcs.configureCalls != 0 {
		t.Errorf("Configure called %d times with empty env+secrets, want 0", rcs.configureCalls)
	}
}

// TestProductionNotifierGRPC_FailsClosedOnNotReseeded verifies that notifierGRPC
// returns an error when the guest reports ReseededRng=false, mirroring the
// legacy JSON path's fail-closed behavior: a VM that did not reseed its CRNG
// must never be served.
func TestProductionNotifierGRPC_FailsClosedOnNotReseeded(t *testing.T) {
	rcs := &recordingControlServer{notifyReseeded: false} // guest did NOT reseed
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	entropy := make([]byte, entropySize)
	req := ActivateRequest{}

	if err := notifierGRPC(nil, sockPath, 1, entropy, req, dialUnix); err == nil {
		t.Fatal("expected error when guest did not reseed its RNG; got nil")
	}
}

// TestProductionGuestReadyGRPC_PingRoundTrip verifies that guestReadyGRPC
// returns nil when the Control server answers Ping, mirroring the legacy
// JSON readiness check.
func TestProductionGuestReadyGRPC_PingRoundTrip(t *testing.T) {
	rcs := &recordingControlServer{}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	ctx := context.Background()
	if _, err := guestReadyGRPC(ctx, sockPath, 5*time.Second, dialUnix); err != nil {
		t.Fatalf("guestReadyGRPC: %v", err)
	}

	rcs.mu.Lock()
	pings := rcs.pingCalls
	rcs.mu.Unlock()
	if pings < 1 {
		t.Errorf("expected at least 1 Ping call, got %d", pings)
	}
}

// TestProductionGuestReadyGRPC_Timeout verifies that guestReadyGRPC returns an
// error within a bounded time when no server is reachable, not hang indefinitely.
func TestProductionGuestReadyGRPC_Timeout(t *testing.T) {
	dir, err := os.MkdirTemp("", "husk-grpc-timeout-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	noSock := filepath.Join(dir, "nosuchsocket.sock")

	ctx := context.Background()
	start := time.Now()
	timeout := 150 * time.Millisecond
	if _, err := guestReadyGRPC(ctx, noSock, timeout, dialUnix); err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
	elapsed := time.Since(start)
	// Must NOT block indefinitely. Allow 3x the timeout for CI headroom.
	if elapsed > 3*time.Second {
		t.Errorf("guestReadyGRPC took %v; should bound to roughly the timeout", elapsed)
	}
}

// TestProductionGuestReadyGRPC_RetryBackoffIsTight pins the retry backoff of the
// post-resume readiness probe. The guest agent's vsock listener is not accepting
// the instant Firecracker resumes the VM, so the FIRST dial almost always fails
// and the retry delay lands directly in the claim's activate latency. A fixed
// 20ms backoff therefore charged ~20ms to every warm claim even though the guest
// answers within a millisecond of the first failure.
//
// The dial seam here fails exactly once and then succeeds against a live server,
// so the elapsed time is the backoff and nothing else: no server-startup race,
// no sleep to tune. A 20ms fixed backoff fails this; a sub-millisecond first
// retry passes it.
func TestProductionGuestReadyGRPC_RetryBackoffIsTight(t *testing.T) {
	rcs := &recordingControlServer{}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	var attempts int
	flaky := func(p string) (*guestgrpc.Client, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("simulated: guest vsock listener not up yet")
		}
		return dialUnix(p)
	}

	start := time.Now()
	c, err := guestReadyGRPC(context.Background(), sockPath, 5*time.Second, flaky)
	if err != nil {
		t.Fatalf("guestReadyGRPC: %v", err)
	}
	if c != nil {
		defer c.Close() //nolint:errcheck // test cleanup
	}
	elapsed := time.Since(start)

	if attempts < 2 {
		t.Fatalf("expected the seam to retry after the forced failure, got %d attempt(s)", attempts)
	}
	// The old fixed 20ms backoff lands at ~20ms. The measured span also covers a real
	// dial + gRPC handshake + Ping over a unix socket, so bound at 15ms: still well
	// under the 20ms regression this pins, but with room for CI scheduler noise.
	if elapsed >= 15*time.Millisecond {
		t.Errorf("first-retry backoff too slow: guestReadyGRPC took %v after one failed dial; want < 15ms", elapsed)
	}
}

// TestActivateHandshakeReusesTheReadinessConnection pins that the warm-claim activate
// opens exactly ONE guest connection.
//
// guestReadyGRPC dials the guest agent over vsock, Pings it, and then closed the
// connection it had just proved healthy; notifierGRPC immediately dialled a fresh one
// for the fork-correctness handshake. That second connect plus HTTP/2 setup sits
// squarely on the activate critical path (the handshake stage measured ~16 ms of a
// ~68 ms activate on the reference node), and it buys nothing: the readiness probe
// only succeeds when the guest is already answering on that exact connection.
//
// The seam therefore hands the proven client to the notifier. A nil client (the unit
// and mock paths, which inject fakes) still makes the notifier dial for itself, so the
// fallback is preserved.
func TestActivateHandshakeReusesTheReadinessConnection(t *testing.T) {
	rcs := &recordingControlServer{notifyReseeded: true}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	var dials int
	counting := func(p string) (*guestgrpc.Client, error) {
		dials++
		return dialUnix(p)
	}

	client, err := guestReadyGRPC(context.Background(), sockPath, 5*time.Second, counting)
	if err != nil {
		t.Fatalf("guestReadyGRPC: %v", err)
	}
	if client == nil {
		t.Fatal("guestReadyGRPC must hand back the connection it proved healthy, not nil")
	}
	defer client.Close() //nolint:errcheck // test cleanup
	if dials != 1 {
		t.Fatalf("readiness probe made %d dials, want 1", dials)
	}

	entropy := make([]byte, entropySize)
	if err := notifierGRPC(client, sockPath, 1, entropy, ActivateRequest{}, counting); err != nil {
		t.Fatalf("notifierGRPC with a reused client: %v", err)
	}
	if dials != 1 {
		t.Errorf("the handshake dialled again (%d total dials): it must reuse the readiness connection", dials)
	}

	rcs.mu.Lock()
	notified := rcs.notifyCalls
	rcs.mu.Unlock()
	if notified != 1 {
		t.Errorf("guest received %d NotifyForked calls over the reused connection, want 1", notified)
	}
}

// TestNotifierDialsWhenGivenNoConnection preserves the fallback: a nil client (the unit
// and mock paths) makes the notifier open its own connection.
func TestNotifierDialsWhenGivenNoConnection(t *testing.T) {
	rcs := &recordingControlServer{notifyReseeded: true}
	sockPath, cleanup := startRecordingGRPC(t, rcs)
	defer cleanup()

	var dials int
	counting := func(p string) (*guestgrpc.Client, error) {
		dials++
		return dialUnix(p)
	}
	entropy := make([]byte, entropySize)
	if err := notifierGRPC(nil, sockPath, 1, entropy, ActivateRequest{}, counting); err != nil {
		t.Fatalf("notifierGRPC with a nil client must dial for itself: %v", err)
	}
	if dials != 1 {
		t.Errorf("notifier made %d dials with a nil client, want 1", dials)
	}
}
