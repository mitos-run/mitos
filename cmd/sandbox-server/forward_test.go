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

	"mitos.run/mitos/internal/vsock"
)

// startTunnelEchoAgent serves the Firecracker UDS preamble and the tunnel
// protocol on sockPath, splicing each tunnel to a local echo server so the
// host-proxy can be proven without KVM. It mirrors the real guest agent.
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
						c.Write(append([]byte("g:"), buf[:n]...))
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
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					c.Close()
					return
				}
				if strings.HasPrefix(line, "CONNECT ") {
					c.Write([]byte("OK 52\n"))
				}
				reqLine, err := br.ReadBytes('\n')
				if err != nil {
					c.Close()
					return
				}
				var req vsock.Request
				if err := json.Unmarshal(reqLine, &req); err != nil || req.Tunnel == nil {
					c.Close()
					return
				}
				target, derr := net.Dial("tcp", echo.Addr().String())
				if derr != nil {
					b, _ := json.Marshal(vsock.TunnelAck{OK: false, Error: derr.Error()})
					c.Write(append(b, '\n'))
					c.Close()
					return
				}
				b, _ := json.Marshal(vsock.TunnelAck{OK: true})
				c.Write(append(b, '\n'))
				done := make(chan struct{}, 2)
				go func() { io.Copy(target, br); target.Close(); done <- struct{}{} }()
				go func() { io.Copy(c, target); c.Close(); done <- struct{}{} }()
				<-done
				<-done
			}(conn)
		}
	}()
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
