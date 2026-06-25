package daemon

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakeTunnelGuestSandbox is an in-process Sandbox gRPC server whose
// PortForward handler dials a local TCP address (the test echo server) and
// splices bytes between the gRPC bidi stream and that TCP connection.
type fakeTunnelGuestSandbox struct {
	sandboxv1.UnimplementedSandboxServer
	target func(port int) (net.Conn, error)
}

// PortForward receives the open Frame, dials the target, splices bytes until
// either side closes, then sends a Close frame.
func (s *fakeTunnelGuestSandbox) PortForward(stream sandboxv1.Sandbox_PortForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return io.ErrUnexpectedEOF
	}
	tc, derr := s.target(int(open.GetPort()))
	if derr != nil {
		return derr
	}
	defer tc.Close()

	// Guest-to-client: read from tcp conn and send data frames.
	guestDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := tc.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if serr := stream.Send(&sandboxv1.Frame{
					Msg: &sandboxv1.Frame_Data{Data: chunk},
				}); serr != nil {
					guestDone <- serr
					return
				}
			}
			if rerr != nil {
				// Send a close frame before returning so the client knows we are done.
				_ = stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Close{Close: true}})
				guestDone <- nil
				return
			}
		}
	}()

	// Client-to-guest: receive data frames and write to tcp conn.
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

// newForwardAPI wires a tokenless SandboxAPI whose sb1 guest agent tunnels to a
// local echo server via the gRPC PortForward RPC.
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
						c.Write(append([]byte("g:"), buf[:n]...)) //nolint:errcheck // test
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
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTunnelGuestSandbox{
		target: func(_ int) (net.Conn, error) {
			return net.Dial("tcp", echo.Addr().String())
		},
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api, echo
}

// TestForwardPortRoundTrips opens a forward, dials the returned host address,
// and asserts bytes round-trip through the host listener -> gRPC PortForward
// tunnel -> guest echo and back.
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
