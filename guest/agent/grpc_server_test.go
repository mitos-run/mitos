//go:build linux

package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// dialGuestGRPC starts the guest gRPC server (the same registration main() uses,
// via newGuestGRPCServer) on the server side of a net.Pipe and returns a
// ClientConn dialing over the client side. This exercises the real Exec and
// Control implementations end-to-end without a vsock; the transport shape is
// identical (grpc-go over a net.Conn, see internal/vsock/grpcconn.go).
func dialGuestGRPC(t *testing.T) *grpc.ClientConn {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	srv := newGuestGRPCServer()
	lis := &onePipeListener{conn: serverConn, done: make(chan struct{})}
	go srv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures
	t.Cleanup(func() {
		srv.Stop()
		serverConn.Close()
		clientConn.Close()
	})

	conn, err := vsock.DialGRPCOverConn(func() (net.Conn, error) { return clientConn, nil })
	if err != nil {
		t.Fatalf("DialGRPCOverConn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestGRPCExecEchoStreamsStdoutAndExit runs `echo hello` over the gRPC Exec bidi
// stream and asserts the merged stdout is "hello\n" with exit code 0. This
// proves the gRPC Exec path reuses the real spawn/stream engine.
func TestGRPCExecEchoStreamsStdoutAndExit(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec open: %v", err)
	}
	// /workspace is the in-VM default but does not exist in CI/test containers;
	// run in the test temp dir so the shell spawn succeeds.
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command:        "echo hello",
		Cwd:            t.TempDir(),
		TimeoutSeconds: 5,
	}}}); err != nil {
		t.Fatalf("Exec send open: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("Exec CloseSend: %v", err)
	}

	var stdout []byte
	var gotExit bool
	var exitCode int32
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Exec recv: %v", err)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout = append(stdout, m.Stdout...)
		case *sandboxv1.ExecResponse_Stderr:
			t.Fatalf("unexpected stderr: %q", m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			gotExit = true
			exitCode = m.Exit.ExitCode
			if m.Exit.Error != "" {
				t.Fatalf("spawn error: %q", m.Exit.Error)
			}
		}
	}

	if string(stdout) != "hello\n" {
		t.Errorf("stdout = %q, want %q", stdout, "hello\n")
	}
	if !gotExit {
		t.Fatal("no exit frame received")
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
}

// TestGRPCExecExitCodePropagates checks a non-zero exit propagates through the
// gRPC exit message, matching the JSON path's exit-code mapping.
func TestGRPCExecExitCodePropagates(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec open: %v", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command:        "exit 7",
		Cwd:            t.TempDir(),
		TimeoutSeconds: 5,
	}}}); err != nil {
		t.Fatalf("Exec send open: %v", err)
	}
	_ = stream.CloseSend()

	var exitCode int32 = -1
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Exec recv: %v", err)
		}
		if e, ok := resp.Msg.(*sandboxv1.ExecResponse_Exit); ok {
			exitCode = e.Exit.ExitCode
		}
	}
	if exitCode != 7 {
		t.Errorf("exit code = %d, want 7", exitCode)
	}
}

// TestGRPCControlPingReturnsUptime checks Control.Ping returns a non-negative
// uptime, reusing the same uptime source as the JSON ping.
func TestGRPCControlPingReturnsUptime(t *testing.T) {
	client := internalv1.NewControlClient(dialGuestGRPC(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Ping(ctx, &internalv1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.UptimeSeconds < 0 {
		t.Errorf("uptime = %f, want >= 0", resp.UptimeSeconds)
	}
}

// TestGRPCControlConfigureThenExecSeesEnv proves Control.Configure delivers env
// to the same configuredEnv the exec engine reads: a value configured over the
// gRPC control channel is visible to a subsequent gRPC Exec. This is the
// byte-for-byte reuse of handleConfigure + the shared exec engine. The value
// here is a test placeholder, not a real secret.
func TestGRPCControlConfigureThenExecSeesEnv(t *testing.T) {
	// Reset the package-level configured env so this test is independent of any
	// other test that mutated it.
	configuredMu.Lock()
	configuredEnv = map[string]string{}
	configuredMu.Unlock()

	conn := dialGuestGRPC(t)
	control := internalv1.NewControlClient(conn)
	sandbox := sandboxv1.NewSandboxClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := control.Configure(ctx, &internalv1.ConfigureRequest{
		Secrets: map[string]string{"GRPC_TEST_TOKEN": "abc123"},
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	stream, err := sandbox.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec open: %v", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command:        "printf %s \"$GRPC_TEST_TOKEN\"",
		Cwd:            t.TempDir(),
		TimeoutSeconds: 5,
	}}}); err != nil {
		t.Fatalf("Exec send open: %v", err)
	}
	_ = stream.CloseSend()

	var stdout []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Exec recv: %v", err)
		}
		if m, ok := resp.Msg.(*sandboxv1.ExecResponse_Stdout); ok {
			stdout = append(stdout, m.Stdout...)
		}
	}
	if string(stdout) != "abc123" {
		t.Errorf("configured env not visible to exec: stdout = %q, want %q", stdout, "abc123")
	}
}

// TestGRPCExecPtyAndArgsUnimplemented checks the two deferred Exec shapes return
// a clear error in this slice.
func TestGRPCExecPtyAndArgsUnimplemented(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		name string
		open *sandboxv1.ExecOpen
	}{
		{"pty", &sandboxv1.ExecOpen{Command: "sh", Pty: &sandboxv1.PtyOptions{}}},
		{"args", &sandboxv1.ExecOpen{Command: "echo", Args: []string{"hi"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := client.Exec(ctx)
			if err != nil {
				t.Fatalf("Exec open: %v", err)
			}
			if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: tc.open}}); err != nil {
				t.Fatalf("send: %v", err)
			}
			_ = stream.CloseSend()
			_, err = stream.Recv()
			if err == nil {
				t.Fatalf("expected Unimplemented error, got nil")
			}
		})
	}
}

// onePipeListener hands a single pre-dialed net.Conn to grpc.Server.Serve and
// blocks (then returns io.EOF after Close) on subsequent Accept calls, so a
// net.Pipe server side can back a gRPC server without a real socket.
type onePipeListener struct {
	conn   net.Conn
	done   chan struct{}
	served bool
}

func (l *onePipeListener) Accept() (net.Conn, error) {
	if !l.served {
		l.served = true
		return l.conn, nil
	}
	<-l.done
	return nil, io.EOF
}

func (l *onePipeListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *onePipeListener) Addr() net.Addr { return l.conn.LocalAddr() }
