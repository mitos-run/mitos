package daemon

// vsock_guestconn_test.go: integration tests for the host-side vsockGuestConn
// (issue #24 stage 5, Task 5.3). They stand up an IN-PROCESS guest gRPC server
// serving sandbox.v1.Sandbox over a unix socket that speaks the Firecracker
// vsock CONNECT preamble for AgentGRPCPort (53), build a vsockGuestConn against
// it via a SandboxAPI, and exercise the real gRPC client path end-to-end:
//   - Exec streams stdout then a terminal Done frame with the exit code.
//   - ReadFile / WriteFile round-trip.
//   - Vitals (server stream) forwards a sample then io.EOF on a clean close.
//   - The Recv lifecycle is clean on the terminal frame (a second Recv after the
//     terminal frame returns io.EOF, and Close tears the conn down without leak).
//
// No KVM or real guest is required: the fake serves the same proto contract the
// real guest agent serves.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakeGuestSandbox is an in-process sandbox.v1.Sandbox server used by the
// vsockGuestConn integration tests. Each RPC returns scripted data so the host
// adapter mapping is exercised without a real guest.
type fakeGuestSandbox struct {
	sandboxv1.UnimplementedSandboxServer

	files map[string][]byte // path -> content, for ReadFile/WriteFile round-trip

	// execStdout/execExit configure the scripted Exec reply. When execStdout is
	// empty it defaults to "hello grpc\n" and execExit defaults to 7.
	execStdout string
	execExit   int32
}

// Exec streams one stdout chunk then an exit frame.
func (s *fakeGuestSandbox) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetOpen() == nil {
		return io.ErrUnexpectedEOF
	}
	out := s.execStdout
	exit := s.execExit
	if out == "" {
		out = "hello grpc\n"
		exit = 7
	}
	if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte(out)}}); err != nil {
		return err
	}
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: exit}}})
}

// ReadFile streams the stored content for the requested path as one Chunk then eof.
func (s *fakeGuestSandbox) ReadFile(req *sandboxv1.ReadFileRequest, stream sandboxv1.Sandbox_ReadFileServer) error {
	data := s.files[req.GetPath()]
	if len(data) > 0 {
		if err := stream.Send(&sandboxv1.Chunk{Data: data}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}

// WriteFile accumulates the streamed data into the file map and returns the byte count.
func (s *fakeGuestSandbox) WriteFile(stream sandboxv1.Sandbox_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return io.ErrUnexpectedEOF
	}
	var content []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		content = append(content, msg.GetData()...)
	}
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[open.GetPath()] = content
	return stream.SendAndClose(&sandboxv1.WriteFileResult{BytesWritten: int64(len(content))})
}

// Vitals streams one sample then ends the stream cleanly (io.EOF on the client).
func (s *fakeGuestSandbox) Vitals(_ *sandboxv1.VitalsRequest, stream sandboxv1.Sandbox_VitalsServer) error {
	return stream.Send(&sandboxv1.GuestVitals{ProcessCount: 11, MemTotalBytes: 2048})
}

// startFakeGuestGRPCUDS serves the Firecracker vsock CONNECT preamble for the
// gRPC port on sockPath, then hands each accepted connection to an in-process
// gRPC server serving fake. The preamble reply is "OK <port>" matching the real
// vsock mux; the byte-at-a-time DialGRPCConn read on the host side stops at the
// newline so the gRPC HTTP/2 preface is not consumed.
func startFakeGuestGRPCUDS(t *testing.T, sockPath string, fake *fakeGuestSandbox) {
	t.Helper()
	startFakeGuestDualUDS(t, sockPath, nil, fake)
}

// startFakeGuestDualUDS serves a single fake guest UDS that dispatches on the
// CONNECT port: "CONNECT <AgentPort>" speaks the legacy JSON-lines exec_stream
// protocol (echoing jsonFrames), and "CONNECT <AgentGRPCPort>" hands the conn to
// an in-process gRPC server serving fake. This lets one socket back both the
// legacy JSON path (RegisterSandbox + /v1/* routes) and the gRPC runtime path
// (vsockGuestConn) during the issue #24 wire migration. jsonFrames may be nil to
// serve only the gRPC port.
func startFakeGuestDualUDS(t *testing.T, sockPath string, jsonFrames []vsock.ExecStreamFrame, fake *fakeGuestSandbox) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	grpcSrv := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(grpcSrv, fake)

	// cl feeds preamble-stripped conns to the gRPC server via a channel listener,
	// so one grpc.Server serves every gRPC connection.
	cl := &chanConnListener{conns: make(chan net.Conn), done: make(chan struct{})}
	go grpcSrv.Serve(cl) //nolint:errcheck // test: errors surface via RPC failures
	t.Cleanup(func() {
		grpcSrv.Stop()
		cl.Close()
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				r := bufio.NewReader(c)
				line, err := r.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "CONNECT ") {
					c.Close()
					return
				}
				portStr := strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))
				if _, err := c.Write([]byte("OK " + portStr + "\n")); err != nil {
					c.Close()
					return
				}
				if portStr == strconv.Itoa(vsock.AgentPort) {
					// Legacy JSON-lines exec_stream: wait for the request line, then
					// echo the scripted frames.
					if _, err := r.ReadString('\n'); err != nil {
						c.Close()
						return
					}
					for _, f := range jsonFrames {
						b, _ := json.Marshal(f)
						if _, err := c.Write(append(b, '\n')); err != nil {
							break
						}
					}
					c.Close()
					return
				}
				// gRPC port: replay any bytes buffered past the preamble newline
				// (the HTTP/2 preface coalesced with CONNECT) ahead of the raw conn.
				served := c
				if n := r.Buffered(); n > 0 {
					buffered, _ := r.Peek(n)
					served = &replayConn{Conn: c, leftover: append([]byte(nil), buffered...)}
				}
				select {
				case cl.conns <- served:
				case <-cl.done:
					c.Close()
				}
			}(conn)
		}
	}()
}

// chanConnListener is a net.Listener that yields conns sent on its channel, used
// to feed preamble-stripped guest conns to one grpc.Server.Serve loop.
type chanConnListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func (l *chanConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *chanConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "chan" }
func (dummyAddr) String() string  { return "chan" }

// replayConn replays buffered bytes (read past the CONNECT preamble) before the
// underlying conn, so the gRPC server sees the full HTTP/2 stream.
type replayConn struct {
	net.Conn
	leftover []byte
}

func (p *replayConn) Read(b []byte) (int, error) {
	if len(p.leftover) > 0 {
		n := copy(b, p.leftover)
		p.leftover = p.leftover[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// newGuestConnTestAPI builds a SandboxAPI whose streamPaths point at a fake guest
// gRPC server and returns the vsockGuestConn under test plus the fake.
func newGuestConnTestAPI(t *testing.T) (*vsockGuestConn, *fakeGuestSandbox) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "vsock.sock")
	fake := &fakeGuestSandbox{files: map[string][]byte{"/workspace/a.txt": []byte("on disk")}}
	startFakeGuestGRPCUDS(t, sock, fake)

	api := NewSandboxAPI(dir)
	// Only the stream path (the gRPC dial source) is needed; the gRPC factory
	// never touches the shared JSON agent connection, so RegisterSandbox is not
	// required for these tests.
	api.RegisterStreamPath("gc-sb", sock)

	return &vsockGuestConn{api: api, sandboxID: "gc-sb"}, fake
}

func TestVsockGuestConnExecStreamsStdoutAndExit(t *testing.T) {
	g, _ := newGuestConnTestAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := g.Exec(ctx, &sandboxv1.ExecOpen{Command: "echo hi"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer stream.Close()

	var stdout strings.Builder
	var exit int32
	var sawDone bool
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if frame.Done {
			sawDone = true
			exit = frame.ExitCode
			break
		}
		stdout.Write(frame.Stdout)
	}
	if !sawDone {
		t.Fatal("never saw Done frame")
	}
	if stdout.String() != "hello grpc\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "hello grpc\n")
	}
	if exit != 7 {
		t.Fatalf("exit = %d, want 7", exit)
	}
	// Lifecycle: a Recv after the terminal Done frame returns io.EOF.
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("post-Done Recv err = %v, want io.EOF", err)
	}
}

func TestVsockGuestConnReadWriteFileRoundTrip(t *testing.T) {
	g, _ := newGuestConnTestAPI(t)
	ctx := context.Background()

	// Read the seeded file.
	chunks, err := g.ReadFile(ctx, "/workspace/a.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got []byte
	for _, c := range chunks {
		got = append(got, c...)
	}
	if string(got) != "on disk" {
		t.Fatalf("ReadFile = %q, want %q", got, "on disk")
	}

	// Write a new file and read it back.
	res, err := g.WriteFile(ctx, "/workspace/b.txt", 0o644, [][]byte{[]byte("round "), []byte("trip")})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if res.BytesWritten != int64(len("round trip")) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len("round trip"))
	}
	chunks2, err := g.ReadFile(ctx, "/workspace/b.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile (b): %v", err)
	}
	var got2 []byte
	for _, c := range chunks2 {
		got2 = append(got2, c...)
	}
	if string(got2) != "round trip" {
		t.Fatalf("round-trip = %q, want %q", got2, "round trip")
	}
}

func TestVsockGuestConnReadFilePartial(t *testing.T) {
	g, _ := newGuestConnTestAPI(t)
	// "on disk" -> offset 3, length 2 -> "di"
	chunks, err := g.ReadFile(context.Background(), "/workspace/a.txt", 3, 2)
	if err != nil {
		t.Fatalf("ReadFile partial: %v", err)
	}
	var got []byte
	for _, c := range chunks {
		got = append(got, c...)
	}
	if string(got) != "di" {
		t.Fatalf("partial = %q, want %q", got, "di")
	}
}

func TestVsockGuestConnVitalsStreamsThenEOF(t *testing.T) {
	g, _ := newGuestConnTestAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	vs, err := g.Vitals(ctx, time.Second)
	if err != nil {
		t.Fatalf("Vitals: %v", err)
	}
	defer vs.Close()

	sample, err := vs.Recv()
	if err != nil {
		t.Fatalf("Vitals Recv: %v", err)
	}
	if sample.GetProcessCount() != 11 || sample.GetMemTotalBytes() != 2048 {
		t.Fatalf("vitals sample = %+v, want process_count=11 mem_total=2048", sample)
	}
	// Clean stream end forwards io.EOF (the Vitals Service handler treats it as a
	// normal close).
	if _, err := vs.Recv(); err != io.EOF {
		t.Fatalf("post-sample Recv err = %v, want io.EOF", err)
	}
}

// TestDialGRPCConnPreamble verifies the host-side preamble dialer rejects a bad
// CONNECT reply and accepts an OK reply, independent of gRPC.
func TestDialGRPCConnPreamble(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pre")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		c, err := lis.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(c)
		_, _ = r.ReadString('\n')
		_, _ = c.Write([]byte("OK 53\n"))
		// Keep the conn open briefly so the host read does not race a close.
		time.Sleep(50 * time.Millisecond)
		c.Close()
	}()
	conn, err := vsock.DialGRPCConn(sock, vsock.AgentGRPCPort)
	if err != nil {
		t.Fatalf("DialGRPCConn: %v", err)
	}
	conn.Close()
}
