package egressproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// staticResolver is a test double that resolves a fixed IP-to-sandbox map.
type staticResolver struct {
	ip2id map[string]string
}

func (s staticResolver) Lookup(srcIP net.IP) (string, bool) {
	id, ok := s.ip2id[srcIP.String()]
	return id, ok
}

// fakeDialer records which host:ports were dialed and returns pre-configured
// connections.
type fakeDialer struct {
	mu          sync.Mutex
	conns       map[string]net.Conn
	dialedConns map[string]bool
}

func (f *fakeDialer) Dial(_ context.Context, hostport string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialedConns == nil {
		f.dialedConns = make(map[string]bool)
	}
	if c, ok := f.conns[hostport]; ok {
		f.dialedConns[hostport] = true
		return c, nil
	}
	return nil, fmt.Errorf("fakeDialer: no conn configured for %s", hostport)
}

// dialed reports whether hostport was dialed.
func (f *fakeDialer) dialed(hostport string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialedConns[hostport]
}

// recordLogger captures Egress calls. It signals done after the first call so
// tests can synchronize on Egress completion without depending on connection
// close ordering.
type recordLogger struct {
	mu       sync.Mutex
	entries  []string
	lastUp   int64
	lastDown int64
	done     chan struct{}
	once     sync.Once
}

func newRecordLogger() *recordLogger {
	return &recordLogger{done: make(chan struct{})}
}

func (r *recordLogger) Egress(sandboxID, hostport string, bytesUp, bytesDown int64) {
	r.mu.Lock()
	r.entries = append(r.entries, sandboxID+" "+hostport)
	r.lastUp = bytesUp
	r.lastDown = bytesDown
	r.mu.Unlock()
	r.once.Do(func() { close(r.done) })
}

// newEchoConn returns a net.Conn backed by a net.Pipe whose far end runs an
// echo goroutine: bytes written to the returned conn are reflected back as
// reads. The echo goroutine exits cleanly when the returned conn is closed.
func newEchoConn() net.Conn {
	a, b := net.Pipe()
	go func() {
		_, _ = io.Copy(b, b)
		b.Close()
	}()
	return a
}

func TestServeConnectTunnelOpensUpstreamAndRedacts(t *testing.T) {
	up := newEchoConn()
	d := &fakeDialer{conns: map[string]net.Conn{"api.example.com:443": up}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))

	// Establish tunnel; include a secret header to verify redaction.
	fmt.Fprintf(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nAuthorization: Bearer SECRET\r\n\r\n")
	br := bufio.NewReader(client)
	status, _ := br.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("want 200 tunnel established, got %q", status)
	}
	br.ReadString('\n') // drain the blank line after the status line

	// Send data through the tunnel and read the echo to verify byte transfer.
	const payload = "PING"
	fmt.Fprint(client, payload)
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("reading echo: %v", err)
	}

	// Close the client to drive the copy goroutines to completion.
	client.Close()

	// Wait for Egress to signal completion before asserting log entries.
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Egress")
	}

	// The dialer was asked for the upstream host:port (host-owned upstream).
	if !d.dialed("api.example.com:443") {
		t.Fatal("upstream was not dialed host-side")
	}

	// Redaction: no secret, no header name, no path reached the log.
	rec.mu.Lock()
	entries := append([]string(nil), rec.entries...)
	rec.mu.Unlock()
	for _, e := range entries {
		if strings.Contains(e, "SECRET") || strings.Contains(e, "Authorization") {
			t.Fatalf("log leaked secret/header: %q", e)
		}
	}
	if len(entries) == 0 || entries[0] != "sbx-1 api.example.com:443" {
		t.Fatalf("egress log should be sandbox + hostport only, got %v", entries)
	}
}

func TestServeRejectsUnknownSource(t *testing.T) {
	p := NewProxy(staticResolver{}, &fakeDialer{}, newRecordLogger())
	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.99"))
	fmt.Fprintf(client, "CONNECT x:443 HTTP/1.1\r\n\r\n")
	st, _ := bufio.NewReader(client).ReadString('\n')
	if !strings.Contains(st, "403") {
		t.Fatalf("unknown source must be refused, got %q", st)
	}
}

// TestServeConnectClientClosesFirst verifies that when the client closes its
// end of the tunnel first (the common HTTP/1.1 close-after-request pattern),
// Serve returns promptly and Logger.Egress fires exactly once with non-zero
// byte counts. Before the fix, the UP goroutine did not close upstream on
// return: DOWN stayed blocked in io.Copy, wg.Wait never returned, Serve hung,
// and the upstream socket leaked on the host process indefinitely.
func TestServeConnectClientClosesFirst(t *testing.T) {
	up := newEchoConn()
	d := &fakeDialer{conns: map[string]net.Conn{"echo.test:9000": up}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.2": "sbx-2"}}, d, rec)

	client, server := net.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		p.Serve(server, net.ParseIP("10.0.0.2"))
	}()

	// Establish tunnel.
	fmt.Fprintf(client, "CONNECT echo.test:9000 HTTP/1.1\r\n\r\n")
	br := bufio.NewReader(client)
	status, _ := br.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("want 200, got %q", status)
	}
	br.ReadString('\n') // drain blank line

	// Send data and read the echo to exercise real byte transfer before closing.
	const payload = "hello"
	fmt.Fprint(client, payload)
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("reading echo: %v", err)
	}

	// Client closes first: simulates the common HTTP/1.1 close-after-request.
	client.Close()

	// Serve must return: no goroutine or upstream socket leak.
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve hung after client closed: goroutine and socket leak detected")
	}

	// Egress must fire with non-zero byte counts in both directions.
	select {
	case <-rec.done:
	case <-time.After(time.Second):
		t.Fatal("Egress was not called after client closed")
	}

	rec.mu.Lock()
	upBytes, downBytes := rec.lastUp, rec.lastDown
	rec.mu.Unlock()
	if upBytes == 0 || downBytes == 0 {
		t.Fatalf("expected non-zero byte counts, got up=%d down=%d", upBytes, downBytes)
	}
}
