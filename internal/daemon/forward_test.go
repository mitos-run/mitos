package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/vsock"
)

// startFakeTunnelUDS serves the Firecracker UDS preamble, then for a tunnel
// request dials the in-test loopback target() and splices bytes. It mirrors the
// real guest agent's tunnel handler so the host-proxy can be proven without KVM.
func startFakeTunnelUDS(t *testing.T, sockPath string, target func(port int) (net.Conn, error)) {
	t.Helper()
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
				connectLine, err := br.ReadString('\n')
				if err != nil {
					c.Close()
					return
				}
				if strings.HasPrefix(connectLine, "CONNECT ") {
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
				tc, derr := target(req.Tunnel.Port)
				if derr != nil {
					b, _ := json.Marshal(vsock.TunnelAck{OK: false, Error: derr.Error()})
					c.Write(append(b, '\n'))
					c.Close()
					return
				}
				b, _ := json.Marshal(vsock.TunnelAck{OK: true})
				c.Write(append(b, '\n'))
				done := make(chan struct{}, 2)
				go func() { io.Copy(tc, br); tc.Close(); done <- struct{}{} }()
				go func() { io.Copy(c, tc); c.Close(); done <- struct{}{} }()
				<-done
				<-done
			}(conn)
		}
	}()
}

// newForwardAPI wires a tokenless SandboxAPI whose sb1 guest agent tunnels to a
// local echo server.
func newForwardAPI(t *testing.T) (*SandboxAPI, net.Listener) {
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

	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	startFakeTunnelUDS(t, sock, func(port int) (net.Conn, error) {
		return net.Dial("tcp", echo.Addr().String())
	})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api, echo
}

// TestForwardPortRoundTrips opens a forward, dials the returned host address,
// and asserts bytes round-trip through the host listener -> vsock tunnel ->
// guest echo and back.
func TestForwardPortRoundTrips(t *testing.T) {
	api, _ := newForwardAPI(t)
	defer api.CloseForwards("sb1")

	hostAddr, err := api.ForwardPort("sb1", 8000)
	if err != nil {
		t.Fatalf("ForwardPort: %v", err)
	}
	if hostAddr == "" {
		t.Fatal("ForwardPort returned an empty host address")
	}

	c, err := net.DialTimeout("tcp", hostAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial host forward: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("yo")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "g:yo" {
		t.Fatalf("round trip = %q, want %q", got, "g:yo")
	}
}

// TestForwardPortRejectsUnknownSandbox asserts a forward for a sandbox with no
// registered agent fails with a clean error, not a hang.
func TestForwardPortRejectsUnknownSandbox(t *testing.T) {
	api, _ := newForwardAPI(t)
	_, err := api.ForwardPort("nope", 8000)
	if err == nil {
		t.Fatal("expected ForwardPort to fail for an unknown sandbox")
	}
}

// TestForwardPortRejectsInvalidPort asserts an out-of-range guest port is
// refused before any listener is opened.
func TestForwardPortRejectsInvalidPort(t *testing.T) {
	api, _ := newForwardAPI(t)
	for _, p := range []int{0, -1, 70000} {
		if _, err := api.ForwardPort("sb1", p); err == nil {
			t.Fatalf("expected ForwardPort to reject port %d", p)
		}
	}
}

// TestCloseForwardsStopsListener asserts CloseForwards closes the host listener
// so a later dial to its address is refused.
func TestCloseForwardsStopsListener(t *testing.T) {
	api, _ := newForwardAPI(t)
	hostAddr, err := api.ForwardPort("sb1", 8000)
	if err != nil {
		t.Fatalf("ForwardPort: %v", err)
	}
	api.CloseForwards("sb1")
	// Give the accept loop a moment to unwind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", hostAddr, 200*time.Millisecond)
		if derr != nil {
			return // listener is closed: success
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("host listener still accepting after CloseForwards")
}

// TestForwardPortRespectsMaxForwards asserts the per-sandbox forward ceiling is
// enforced: opening more than the cap distinct forwards is rejected.
func TestForwardPortRespectsMaxForwards(t *testing.T) {
	api, _ := newForwardAPI(t)
	defer api.CloseForwards("sb1")
	api.SetMaxForwardsPerSandbox(2)

	var opened int
	var lastErr error
	for i := 0; i < 5; i++ {
		if _, err := api.ForwardPort("sb1", 8000+i); err != nil {
			lastErr = err
			break
		}
		opened++
	}
	if opened != 2 {
		t.Fatalf("opened %d forwards, want cap of 2 (lastErr=%v)", opened, lastErr)
	}
	if lastErr == nil {
		t.Fatal("expected the 3rd forward to be rejected by the cap")
	}
}
