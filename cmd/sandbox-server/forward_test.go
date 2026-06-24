package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// startTunnelEchoAgent serves the Firecracker UDS preamble for gRPC
// (AgentGRPCPort 53) and implements the PortForward RPC, splicing each stream
// to a local echo server. This replaces the old JSON-tunnel fake.
func startTunnelEchoAgent(t *testing.T, sockPath string) {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write(append([]byte("g:"), buf[:n]...)) //nolint:errcheck // test
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	srv := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(srv, &tunnelEchoSandbox{echoAddr: echo.Addr().String()})

	// chanConnListener feeds preamble-stripped conns to the gRPC server.
	cl := &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
	go srv.Serve(cl) //nolint:errcheck // test
	t.Cleanup(func() { srv.Stop(); cl.Close() })

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
				if _, werr := c.Write([]byte("OK " + portStr + "\n")); werr != nil {
					c.Close()
					return
				}
				var served net.Conn = c
				if n := r.Buffered(); n > 0 {
					leftover, _ := r.Peek(n)
					served = &prefixConn{Conn: c, data: append([]byte(nil), leftover...)}
				}
				select {
				case cl.ch <- served:
				case <-cl.done:
					c.Close()
				}
			}(conn)
		}
	}()

}

// tunnelEchoSandbox is a gRPC Sandbox server that implements PortForward by
// dialing echoAddr and splicing bytes between the gRPC stream and the echo.
type tunnelEchoSandbox struct {
	sandboxv1.UnimplementedSandboxServer
	echoAddr string
}

func (s *tunnelEchoSandbox) PortForward(stream sandboxv1.Sandbox_PortForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetOpen() == nil {
		return io.ErrUnexpectedEOF
	}
	tc, derr := net.Dial("tcp", s.echoAddr)
	if derr != nil {
		return derr
	}
	defer tc.Close()

	guestDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := tc.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if serr := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Data{Data: chunk}}); serr != nil {
					guestDone <- serr
					return
				}
			}
			if rerr != nil {
				_ = stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Close{Close: true}})
				guestDone <- nil
				return
			}
		}
	}()

	for {
		select {
		case err := <-guestDone:
			return err
		default:
		}
		frame, rerr := stream.Recv()
		if rerr == io.EOF {
			return <-guestDone
		}
		if rerr != nil {
			tc.Close()
			return rerr
		}
		if frame.GetClose() {
			tc.Close()
			return <-guestDone
		}
		if data := frame.GetData(); len(data) > 0 {
			if _, werr := tc.Write(data); werr != nil {
				tc.Close()
				return werr
			}
		}
	}
}

type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}
func (l *chanListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "chan" }
func (dummyAddr) String() string  { return "chan" }

type prefixConn struct {
	net.Conn
	data []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.data) > 0 {
		n := copy(b, p.data)
		p.data = p.data[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// TestForwardEndpointMockModeUnsupported asserts the standalone forward endpoint
// returns a clean unsupported error in mock mode (no guest to tunnel to).
func TestForwardEndpointMockModeUnsupported(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400) // mock
	s.sandboxes["sb1"] = &sandboxInfo{ID: "sb1"}

	body, _ := json.Marshal(map[string]any{"guest_port": 8000})
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb1/forward", bytes.NewReader(body))
	r.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handleForward(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}

// TestForwardEndpointRoundTrips builds a real-mode server, registers a
// tunnel-capable fake agent for sb1, then POSTs the forward endpoint and dials
// the returned host address, asserting bytes round-trip through the tunnel.
func TestForwardEndpointRoundTrips(t *testing.T) {
	// A unix socket path is bounded (~104 bytes on darwin), so root the data dir
	// under /tmp rather than the long t.TempDir() path.
	dir, err := os.MkdirTemp("/tmp", "sbfwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sandboxes", "sb1", "vsock.sock")
	startTunnelEchoAgent(t, sock)

	s := newServer(dir, "", false, 16, 86400) // real mode (no engine)
	if err := s.sandboxAPI.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	s.sandboxAPI.RegisterStreamPath("sb1", sock)
	s.sandboxes["sb1"] = &sandboxInfo{ID: "sb1"}

	body, _ := json.Marshal(map[string]any{"guest_port": 8000})
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb1/forward", bytes.NewReader(body))
	r.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handleForward(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Host      string `json:"host"`
		GuestPort int    `json:"guest_port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Host == "" {
		t.Fatal("forward response missing host address")
	}
	if resp.GuestPort != 8000 {
		t.Fatalf("guest_port = %d, want 8000", resp.GuestPort)
	}

	c, err := net.DialTimeout("tcp", resp.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial forward host %q: %v", resp.Host, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hey")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "g:hey" {
		t.Fatalf("round trip = %q, want %q", got, "g:hey")
	}
}

// TestForwardEndpointUnknownSandbox asserts a forward for an unknown sandbox is
// a clean 404, not a hang or a panic.
func TestForwardEndpointUnknownSandbox(t *testing.T) {
	s := newServer(t.TempDir(), "", false, 16, 86400)
	body, _ := json.Marshal(map[string]any{"guest_port": 8000})
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/ghost/forward", bytes.NewReader(body))
	r.SetPathValue("id", "ghost")
	w := httptest.NewRecorder()
	s.handleForward(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
