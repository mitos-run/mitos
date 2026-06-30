package sniproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test seams ---

type staticResolver struct {
	ip2id map[string]string
}

func (s staticResolver) Lookup(srcIP net.IP) (string, bool) {
	id, ok := s.ip2id[srcIP.String()]
	return id, ok
}

// stubAllowlist allows a fixed (serverName, port) set regardless of source IP,
// isolating the proxy decision/splice logic from the registry matcher (that is
// covered separately in allowlist_test.go).
type stubAllowlist struct {
	allow map[string]int // serverName -> allowed port
}

func (a stubAllowlist) Allowed(srcIP net.IP, serverName string, port int) bool {
	p, ok := a.allow[serverName]
	return ok && p == port
}

type recordingLogger struct {
	mu         sync.Mutex
	allowCalls int
	denyCalls  int
	lastReason string
	lastName   string
	upBytes    int64
	downBytes  int64
}

func (l *recordingLogger) Allow(sandboxID, serverName string, port int, up, down int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allowCalls++
	l.lastName = serverName
	l.upBytes = up
	l.downBytes = down
}

func (l *recordingLogger) Deny(sandboxID, serverName string, port int, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.denyCalls++
	l.lastReason = reason
	l.lastName = serverName
}

// fakeConn is an in-memory net.Conn: Read drains r (a bytes.Reader), Write
// captures into w. Read and Write touch distinct buffers so the bidirectional
// splice copies never race on the same buffer; the close state is shared and
// guarded by an atomic plus a done channel.
//
// holdOpen models an OPEN connection: when r is drained, Read BLOCKS until Close
// rather than returning EOF, so a peer's still-flowing direction is not truncated
// by this side reaching EOF first (real TLS connections do not half-close
// instantly). With holdOpen false, Read returns EOF when r is drained (a peer
// that closes after sending, e.g. a server that finishes its response).
type fakeConn struct {
	r        io.Reader
	w        *bytes.Buffer
	holdOpen bool
	closed   atomic.Bool
	done     chan struct{}
}

func newFakeConn(read []byte) *fakeConn {
	return &fakeConn{r: bytes.NewReader(read), w: &bytes.Buffer{}, done: make(chan struct{})}
}

// newOpenFakeConn models a peer that keeps its write half open after the given
// bytes until the connection is closed by the other splice direction.
func newOpenFakeConn(read []byte) *fakeConn {
	c := newFakeConn(read)
	c.holdOpen = true
	return c
}

func (c *fakeConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.EOF
	}
	n, err := c.r.Read(p)
	if err == io.EOF && c.holdOpen && n == 0 {
		<-c.done
		return 0, io.EOF
	}
	return n, err
}

func (c *fakeConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return c.w.Write(p)
}

func (c *fakeConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
	}
	return nil
}
func (c *fakeConn) LocalAddr() net.Addr                { return nil }
func (c *fakeConn) RemoteAddr() net.Addr               { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

type fakeDialer struct {
	conn   *fakeConn
	dialed string
	err    error
}

func (d *fakeDialer) Dial(ctx context.Context, hostport string) (net.Conn, error) {
	d.dialed = hostport
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

const (
	testGuestIP = "10.0.0.5"
	testDstIP   = "93.184.216.34" // example.com, a public address (not denylisted)
)

func newTestProxy(allow map[string]int, dialer *fakeDialer, logger Logger) *Proxy {
	return NewProxy(
		staticResolver{ip2id: map[string]string{testGuestIP: "sb-1"}},
		stubAllowlist{allow: allow},
		dialer,
		logger,
	)
}

func TestServeAllowSplicesBytes(t *testing.T) {
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS13)
	appData := []byte("application-layer bytes after the handshake")
	clientInput := append(append([]byte{}, hello...), appData...)

	const downstream = "server response bytes"
	upstream := newFakeConn([]byte(downstream))
	client := newOpenFakeConn(clientInput)
	dialer := &fakeDialer{conn: upstream}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(client, net.ParseIP(testGuestIP), net.ParseIP(testDstIP), 443)

	// Splice: the upstream must receive the FULL original client stream (the
	// replayed ClientHello peek + the trailing app data).
	if got := upstream.w.Bytes(); !bytes.Equal(got, clientInput) {
		t.Fatalf("upstream received %d bytes, want the full client stream (%d bytes)", len(got), len(clientInput))
	}
	// And the downstream response must reach the client.
	if got := client.w.String(); got != downstream {
		t.Fatalf("client received %q, want %q", got, downstream)
	}
	if dialer.dialed != net.JoinHostPort(testDstIP, "443") {
		t.Fatalf("dialed %q, want %s:443", dialer.dialed, testDstIP)
	}
	if logger.allowCalls != 1 || logger.denyCalls != 0 {
		t.Fatalf("allow=%d deny=%d, want allow=1 deny=0", logger.allowCalls, logger.denyCalls)
	}
	if logger.upBytes != int64(len(clientInput)) {
		t.Fatalf("logged upBytes=%d, want %d", logger.upBytes, len(clientInput))
	}
}

func TestServeDenyUnlistedSNIClosesNoDial(t *testing.T) {
	hello := clientHelloBytes(t, "evil.example.org", tls.VersionTLS12, tls.VersionTLS13)
	upstream := newFakeConn(nil)
	dialer := &fakeDialer{conn: upstream}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(newFakeConn(hello), net.ParseIP(testGuestIP), net.ParseIP(testDstIP), 443)

	if dialer.dialed != "" {
		t.Fatalf("denied connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.allowCalls != 0 {
		t.Fatalf("allow=%d deny=%d, want allow=0 deny=1", logger.allowCalls, logger.denyCalls)
	}
	if logger.lastReason != "not_allowlisted" {
		t.Fatalf("deny reason = %q, want not_allowlisted", logger.lastReason)
	}
}

func TestServeDenyMissingSNI(t *testing.T) {
	hello := clientHelloBytes(t, "", tls.VersionTLS12, tls.VersionTLS13)
	dialer := &fakeDialer{conn: newFakeConn(nil)}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(newFakeConn(hello), net.ParseIP(testGuestIP), net.ParseIP(testDstIP), 443)

	if dialer.dialed != "" {
		t.Fatalf("missing-SNI connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.lastReason != "missing_sni" {
		t.Fatalf("deny=%d reason=%q, want deny=1 reason=missing_sni", logger.denyCalls, logger.lastReason)
	}
}

func TestServeDenyMalformedClientHello(t *testing.T) {
	dialer := &fakeDialer{conn: newFakeConn(nil)}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(newFakeConn([]byte("GET / HTTP/1.1\r\n\r\n")), net.ParseIP(testGuestIP), net.ParseIP(testDstIP), 443)

	if dialer.dialed != "" {
		t.Fatalf("malformed connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.lastReason != "malformed_clienthello" {
		t.Fatalf("deny=%d reason=%q, want deny=1 reason=malformed_clienthello", logger.denyCalls, logger.lastReason)
	}
}

func TestServeDenyUnknownSandbox(t *testing.T) {
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS13)
	dialer := &fakeDialer{conn: newFakeConn(nil)}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	// Source IP not in the resolver.
	p.Serve(newFakeConn(hello), net.ParseIP("10.0.0.99"), net.ParseIP(testDstIP), 443)

	if dialer.dialed != "" {
		t.Fatalf("unknown-sandbox connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.lastReason != "unknown_sandbox" {
		t.Fatalf("deny=%d reason=%q, want deny=1 reason=unknown_sandbox", logger.denyCalls, logger.lastReason)
	}
}

func TestServeDenyWrongPort(t *testing.T) {
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS13)
	dialer := &fakeDialer{conn: newFakeConn(nil)}
	logger := &recordingLogger{}

	// Allowlist permits api.example.com on 443; the connection targets 8443.
	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(newFakeConn(hello), net.ParseIP(testGuestIP), net.ParseIP(testDstIP), 8443)

	if dialer.dialed != "" {
		t.Fatalf("wrong-port connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.lastReason != "not_allowlisted" {
		t.Fatalf("deny=%d reason=%q, want deny=1 reason=not_allowlisted", logger.denyCalls, logger.lastReason)
	}
}

func TestServeDenyDeniedDestination(t *testing.T) {
	// SNI on the allowlist but the original destination resolves to a denylisted
	// address (cloud metadata): the host-side floor must refuse before dialing.
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS13)
	dialer := &fakeDialer{conn: newFakeConn(nil)}
	logger := &recordingLogger{}

	p := newTestProxy(map[string]int{"api.example.com": 443}, dialer, logger)
	p.Serve(newFakeConn(hello), net.ParseIP(testGuestIP), net.ParseIP("169.254.169.254"), 443)

	if dialer.dialed != "" {
		t.Fatalf("denied-destination connection must not dial; dialed %q", dialer.dialed)
	}
	if logger.denyCalls != 1 || logger.lastReason != "destination_denied" {
		t.Fatalf("deny=%d reason=%q, want deny=1 reason=destination_denied", logger.denyCalls, logger.lastReason)
	}
}
