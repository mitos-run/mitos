package vsock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var tunnelSeq atomic.Int64

// startFakeTunnelAgent serves the Firecracker vsock UDS preamble then, for a
// tunnel request, calls onOpen(port). onOpen returns the in-guest TCP conn to
// splice to the vsock stream (or an error to refuse the open). The fake agent
// mirrors the real guest agent: it writes a one-line TunnelAck, and on success
// pipes bytes bidirectionally between the spliced conn and the vsock stream
// until either side closes.
func startFakeTunnelAgent(t *testing.T, onOpen func(port int) (net.Conn, error)) string {
	t.Helper()
	sockPath := fmt.Sprintf("/tmp/test-tunnel-agent-%d-%d.sock", os.Getpid(), tunnelSeq.Add(1))
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close(); os.Remove(sockPath) })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				// CONNECT preamble line.
				connectLine, err := br.ReadString('\n')
				if err != nil {
					c.Close()
					return
				}
				if strings.HasPrefix(connectLine, "CONNECT ") {
					fmt.Fprintf(c, "OK %s\n", strings.TrimSpace(strings.TrimPrefix(connectLine, "CONNECT ")))
				}
				// Tunnel request line. A bufio.Reader does not over-consume past
				// the newline into an unreachable buffer beyond what stays in br,
				// and br is the splice source below, so coalesced payload survives.
				reqLine, err := br.ReadBytes('\n')
				if err != nil {
					c.Close()
					return
				}
				var req Request
				if err := json.Unmarshal(reqLine, &req); err != nil || req.Tunnel == nil {
					c.Close()
					return
				}
				target, oerr := onOpen(req.Tunnel.Port)
				if oerr != nil {
					b, _ := json.Marshal(TunnelAck{OK: false, Error: oerr.Error()})
					c.Write(append(b, '\n'))
					c.Close()
					return
				}
				b, _ := json.Marshal(TunnelAck{OK: true})
				c.Write(append(b, '\n'))
				// Splice host<->target. Read the host side via br so any bytes the
				// reader buffered after the request line are forwarded.
				done := make(chan struct{}, 2)
				go func() { io.Copy(target, br); target.Close(); done <- struct{}{} }()
				go func() { io.Copy(c, target); c.Close(); done <- struct{}{} }()
				<-done
				<-done
			}(conn)
		}
	}()
	return sockPath
}

// TestTunnelRoundTripsBytesBidirectionally proves the host-side Tunnel splices
// bytes both directions against a fake guest agent backed by a local echo TCP
// server, without KVM.
func TestTunnelRoundTripsBytesBidirectionally(t *testing.T) {
	// In-test "guest" TCP server: echoes with a prefix so we can tell the
	// direction apart.
	echoLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLis.Close()
	go func() {
		for {
			c, err := echoLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write(append([]byte("echo:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()

	sock := startFakeTunnelAgent(t, func(port int) (net.Conn, error) {
		return net.Dial("tcp", echoLis.Addr().String())
	})

	sc, err := DialStreamUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	tun, err := sc.Tunnel(8000)
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	defer tun.Close()

	if _, err := tun.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	_ = tun.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := tun.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("round trip = %q, want %q", got, "echo:ping")
	}
}

// TestTunnelRefusedReturnsTypedError proves a tunnel to a guest port that is not
// listening returns a clean error from Tunnel, not a hang.
func TestTunnelRefusedReturnsTypedError(t *testing.T) {
	sock := startFakeTunnelAgent(t, func(port int) (net.Conn, error) {
		return nil, fmt.Errorf("dial 127.0.0.1:%d: connection refused", port)
	})
	sc, err := DialStreamUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	_, err = sc.Tunnel(9999)
	if err == nil {
		t.Fatal("expected Tunnel to fail when the guest port is not listening")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %v, want it to carry the guest dial cause", err)
	}
}

// TestTunnelCloseTearsDownGuestSide proves closing the host tunnel ends the
// guest-side copy (the spliced conn is closed), so neither side leaks.
func TestTunnelCloseTearsDownGuestSide(t *testing.T) {
	closed := make(chan struct{})
	sock := startFakeTunnelAgent(t, func(port int) (net.Conn, error) {
		a, b := net.Pipe()
		// b is the "guest TCP conn" handed to the splice. When the host closes
		// the tunnel, the guest io.Copy ends and Close()s b, which unblocks a.
		go func() {
			buf := make([]byte, 1)
			_, _ = a.Read(buf) // blocks until b is closed -> read returns err
			close(closed)
			a.Close()
		}()
		return b, nil
	})
	sc, err := DialStreamUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	tun, err := sc.Tunnel(8000)
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	tun.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the host tunnel did not tear down the guest side")
	}
}
