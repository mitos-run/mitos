// TestGRPCOverConn proves that grpc-go can serve and consume RPCs over a raw
// net.Conn pair, which is the shape a vsock connection provides (see
// grpcconn.go for the transport-choice rationale).
//
// Service choices:
//   - Unary check: sandbox.v1.Sandbox/Stat. The Stat RPC is a simple
//     unary call; the fake returns a fixed FileInfo for any path.
//   - Server-streaming check: sandbox.v1.Sandbox/ReadFile. A server-streaming
//     RPC that maps directly to the exec/file IO use-case that Stage 5 needs.
//     The fake handler emits three Chunk messages (simulating a file split into
//     three segments) and then closes, which exercises the streaming path.
//
// Note: sandbox.internal.v1 cannot be imported here because the Go toolchain
// enforces that packages under an "internal" path segment may only be imported
// by code rooted at the parent of that "internal" directory
// (proto/sandbox/internal/v1 is only importable from proto/sandbox/...).
// sandbox.v1 has no such restriction and covers both the unary and streaming
// cases needed to de-risk the Stage 5 guest flip.

package vsock

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakeSandboxServer implements sandbox.v1.Sandbox with Stat (unary) and
// ReadFile (server-stream); all other methods delegate to the unimplemented
// stub.
type fakeSandboxServer struct {
	sandboxv1.UnimplementedSandboxServer
}

// Stat returns a fixed FileInfo for any path so the unary check is trivial.
func (s *fakeSandboxServer) Stat(_ context.Context, req *sandboxv1.StatRequest) (*sandboxv1.FileInfo, error) {
	return &sandboxv1.FileInfo{Path: req.Path, Size: 99}, nil
}

// ReadFile streams three fixed Chunk messages and sets Eof on the last one.
func (s *fakeSandboxServer) ReadFile(req *sandboxv1.ReadFileRequest, stream grpc.ServerStreamingServer[sandboxv1.Chunk]) error {
	payloads := []string{"hello", " ", "world"}
	for i, p := range payloads {
		eof := i == len(payloads)-1
		if err := stream.Send(&sandboxv1.Chunk{Data: []byte(p), Eof: eof}); err != nil {
			return err
		}
	}
	return nil
}

// startGRPCOverPipe starts an in-process gRPC server on the server side of a
// net.Pipe() pair and returns a *grpc.ClientConn dialing over the client side.
// The server is stopped via t.Cleanup.
func startGRPCOverPipe(t *testing.T) *grpc.ClientConn {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	srv := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(srv, &fakeSandboxServer{})

	// Wrap the server-side conn in a one-shot listener so grpc.Server.Serve
	// can consume it. The listener returns io.EOF on subsequent Accept calls
	// so the serve loop exits cleanly when the server is stopped.
	lis := &singleConnListener{conn: serverConn}
	go srv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures
	t.Cleanup(func() {
		srv.Stop()
		serverConn.Close()
		clientConn.Close()
	})

	// The dialer ignores the target address and returns the pre-dialed client
	// side of the pipe; this is identical to the vsock usage pattern where the
	// dialer returns an already-established vsock net.Conn.
	conn, err := DialGRPCOverConn(func() (net.Conn, error) {
		return clientConn, nil
	})
	if err != nil {
		t.Fatalf("DialGRPCOverConn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestGRPCOverConn_Unary verifies that a unary Stat RPC round-trips over a
// net.Pipe() pair via DialGRPCOverConn.
func TestGRPCOverConn_Unary(t *testing.T) {
	conn := startGRPCOverPipe(t)
	client := sandboxv1.NewSandboxClient(conn)

	resp, err := client.Stat(context.Background(), &sandboxv1.StatRequest{Path: "/workspace"})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if resp.Path != "/workspace" || resp.Size != 99 {
		t.Errorf("Stat = %+v, want path=/workspace size=99", resp)
	}
}

// TestGRPCOverConn_ServerStream verifies that a server-streaming ReadFile RPC
// round-trips over a net.Pipe() pair via DialGRPCOverConn. This exercises the
// streaming path that exec/file IO in Stage 5 will use.
func TestGRPCOverConn_ServerStream(t *testing.T) {
	conn := startGRPCOverPipe(t)
	client := sandboxv1.NewSandboxClient(conn)

	stream, err := client.ReadFile(context.Background(), &sandboxv1.ReadFileRequest{Path: "/test"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var got []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, chunk.Data...)
		if chunk.Eof {
			break
		}
	}

	want := "hello world"
	if string(got) != want {
		t.Errorf("ReadFile data = %q, want %q", got, want)
	}
}

// singleConnListener is a net.Listener that returns exactly one pre-dialed
// net.Conn and then blocks on subsequent Accept calls until the listener is
// closed. It is used to hand a net.Pipe() server-side conn to grpc.Server.Serve
// without needing a real listening socket.
type singleConnListener struct {
	conn net.Conn
	done chan struct{}
	once chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.once == nil {
		l.once = make(chan struct{})
		l.done = make(chan struct{})
		// First call: return the pre-dialed conn and signal that it has been
		// consumed.
		close(l.once)
		return l.conn, nil
	}
	// Subsequent calls: block until the listener is closed.
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
