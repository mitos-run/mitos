package guestgrpc_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakeControlServer is an in-process implementation of the Control service
// that returns a fixed uptime from Ping. NotifyForked and Configure are stubs.
type fakeControlServer struct {
	internalv1.UnimplementedControlServer
}

func (s *fakeControlServer) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	return &internalv1.PingResponse{UptimeSeconds: 1.0}, nil
}

// fakeSandboxServer is a minimal sandbox.v1.Sandbox implementation for tests.
type fakeSandboxServer struct {
	sandboxv1.UnimplementedSandboxServer
}

// singleConnListener is a net.Listener that returns exactly one pre-dialed
// net.Conn and then blocks on subsequent Accept calls until Close is called.
// It mirrors the pattern in internal/vsock/grpcconn_test.go.
type singleConnListener struct {
	conn net.Conn
	done chan struct{}
	once chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.once == nil {
		l.once = make(chan struct{})
		l.done = make(chan struct{})
		close(l.once)
		return l.conn, nil
	}
	<-l.done
	return nil, io.EOF
}

func (l *singleConnListener) Close() error {
	if l.done != nil {
		select {
		case <-l.done:
		default:
			close(l.done)
		}
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// startFakeGuestGRPC starts an in-process gRPC server serving both Control
// and Sandbox over a net.Pipe pair. It returns a dial factory and a cleanup
// function that tears down the server and both ends of the pipe.
//
// The caller is responsible for invoking cleanup; t.Cleanup is not used here
// so that tests that close the client conn first can close the net.Pipe ends
// in the right order (conn closes before srv.Stop) to avoid grpc-go write
// deadlocks on net.Pipe.
func startFakeGuestGRPC(t *testing.T) (dial func() (net.Conn, error), cleanup func()) {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	srv := grpc.NewServer()
	internalv1.RegisterControlServer(srv, &fakeControlServer{})
	sandboxv1.RegisterSandboxServer(srv, &fakeSandboxServer{})

	lis := &singleConnListener{conn: serverConn}
	go srv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures

	cleanup = func() {
		// Close both pipe ends first so any pending server write unblocks, then
		// stop the server so its goroutines drain cleanly.
		serverConn.Close()
		clientConn.Close()
		srv.Stop()
	}

	return func() (net.Conn, error) { return clientConn, nil }, cleanup
}

// tempSock creates a short unix socket path under os.TempDir so the path
// stays well within the 104-byte sun_path limit on macOS. The directory is
// removed via t.Cleanup.
func tempSock(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gg-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// startUnixGRPC starts an in-process gRPC server on a temp unix socket and
// tears it down via t.Cleanup.
func startUnixGRPC(t *testing.T, sockPath string) {
	t.Helper()
	srv := grpc.NewServer()
	internalv1.RegisterControlServer(srv, &fakeControlServer{})
	sandboxv1.RegisterSandboxServer(srv, &fakeSandboxServer{})

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	go srv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures
	t.Cleanup(func() {
		srv.Stop()
		lis.Close()
	})
}

// TestDialUnix_PingRoundTrip verifies that a Client built over DialGRPCOverConn
// (the same internal path DialUnix uses) exposes working typed stubs: a Ping
// on the Control client must round-trip and return the fake uptime.
func TestDialUnix_PingRoundTrip(t *testing.T) {
	dialConn, cleanup := startFakeGuestGRPC(t)
	defer cleanup()

	cc, err := vsock.DialGRPCOverConn(dialConn)
	if err != nil {
		t.Fatalf("DialGRPCOverConn: %v", err)
	}

	client := &guestgrpc.Client{
		Conn:    cc,
		Sandbox: sandboxv1.NewSandboxClient(cc),
		Control: internalv1.NewControlClient(cc),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Control.Ping(ctx, &internalv1.PingRequest{})
	if err != nil {
		t.Fatalf("Control.Ping: %v", err)
	}
	if resp.UptimeSeconds != 1.0 {
		t.Errorf("UptimeSeconds = %v, want 1.0", resp.UptimeSeconds)
	}

	// Close client before cleanup so the pipe ends close in the right order.
	client.Close() //nolint:errcheck // test
}

// TestWaitReadyUnix_RoundTrips verifies that WaitReadyUnix returns a ready
// Client after a successful Ping against a real unix-socket server.
func TestWaitReadyUnix_RoundTrips(t *testing.T) {
	sockPath := tempSock(t, "a.sock")
	startUnixGRPC(t, sockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := guestgrpc.WaitReadyUnix(ctx, sockPath, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReadyUnix: %v", err)
	}
	defer client.Close() //nolint:errcheck // test

	resp, err := client.Control.Ping(ctx, &internalv1.PingRequest{})
	if err != nil {
		t.Fatalf("post-WaitReadyUnix Ping: %v", err)
	}
	if resp.UptimeSeconds != 1.0 {
		t.Errorf("UptimeSeconds = %v, want 1.0", resp.UptimeSeconds)
	}
}

// TestDialUnix_ExposesTypedClients verifies that DialUnix populates both the
// Sandbox and Control typed clients and that Control.Ping round-trips.
func TestDialUnix_ExposesTypedClients(t *testing.T) {
	sockPath := tempSock(t, "b.sock")
	startUnixGRPC(t, sockPath)

	client, err := guestgrpc.DialUnix(sockPath)
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	defer client.Close() //nolint:errcheck // test

	if client.Sandbox == nil {
		t.Fatal("Sandbox client is nil")
	}
	if client.Control == nil {
		t.Fatal("Control client is nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Control.Ping(ctx, &internalv1.PingRequest{})
	if err != nil {
		t.Fatalf("DialUnix Control.Ping: %v", err)
	}
	if resp.UptimeSeconds != 1.0 {
		t.Errorf("UptimeSeconds = %v, want 1.0", resp.UptimeSeconds)
	}
}

// TestClient_CloseReturnsNil verifies that the first call to Close on a
// connected, ping-verified Client returns nil (grpc-go closes the HTTP/2
// transport cleanly when no active RPCs are in flight).
func TestClient_CloseReturnsNil(t *testing.T) {
	sockPath := tempSock(t, "c.sock")
	startUnixGRPC(t, sockPath)

	client, err := guestgrpc.DialUnix(sockPath)
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}

	// Do one Ping to fully establish the HTTP/2 connection before closing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, pingErr := client.Control.Ping(ctx, &internalv1.PingRequest{})
	cancel()
	if pingErr != nil {
		t.Fatalf("pre-close Ping: %v", pingErr)
	}

	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
