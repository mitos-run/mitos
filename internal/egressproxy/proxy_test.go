package egressproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// connWrapper wraps a net.Conn and closes egressDone when Close is called.
// fakeDialer.dialed blocks on egressDone, which fires only after serveConnect's
// deferred upstream.Close executes. Because that defer runs after
// Logger.Egress, any goroutine that unblocks from dialed() is guaranteed to see
// the complete log entry: Egress write happens-before upstream.Close happens-
// before close(egressDone) happens-before <-egressDone (channel close/receive
// rule), establishing the required happens-before across goroutines for the
// race detector.
type connWrapper struct {
	net.Conn
	once       sync.Once
	egressDone chan struct{}
}

func (c *connWrapper) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.egressDone) })
	return err
}

// staticResolver is a test double that resolves a fixed IP-to-sandbox map.
type staticResolver struct {
	ip2id map[string]string
}

func (s staticResolver) Lookup(srcIP net.IP) (string, bool) {
	id, ok := s.ip2id[srcIP.String()]
	return id, ok
}

// fakeDialer records which host:ports were dialed and returns pre-configured
// connections wrapped in connWrapper so that dialed() can synchronize with the
// proxy's deferred upstream.Close (which fires after Logger.Egress).
type fakeDialer struct {
	mu      sync.Mutex
	conns   map[string]net.Conn
	dialed_ map[string]*connWrapper
}

func (f *fakeDialer) Dial(_ context.Context, hostport string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialed_ == nil {
		f.dialed_ = make(map[string]*connWrapper)
	}
	if c, ok := f.conns[hostport]; ok {
		cw := &connWrapper{Conn: c, egressDone: make(chan struct{})}
		f.dialed_[hostport] = cw
		return cw, nil
	}
	return nil, fmt.Errorf("fakeDialer: no conn configured for %s", hostport)
}

// dialed reports whether hostport was dialed. For connections that produce a
// connWrapper it blocks until upstream.Close has been called (which happens
// after Logger.Egress inside serveConnect), providing the happens-before
// guarantee for safe entry inspection.
func (f *fakeDialer) dialed(hostport string) bool {
	f.mu.Lock()
	cw, ok := f.dialed_[hostport]
	f.mu.Unlock()
	if !ok {
		return false
	}
	<-cw.egressDone
	return true
}

// recordLogger captures Egress calls as "sandboxID hostport" strings. Byte
// counts are accepted and discarded. No header or body content is ever stored,
// satisfying the redaction requirement.
// The entries slice is written in the Serve goroutine and read by the test after
// fakeDialer.dialed() returns, so no mutex is required: dialed() blocks until
// the deferred upstream.Close fires after Logger.Egress, providing the required
// happens-before via the egressDone channel.
type recordLogger struct {
	entries []string
}

func (r *recordLogger) Egress(sandboxID, hostport string, _, _ int64) {
	r.entries = append(r.entries, sandboxID+" "+hostport)
}

// newEchoConn returns a net.Conn whose peer is closed immediately so reads
// return EOF and writes return an error. This makes the bidirectional copy in
// serveConnect terminate promptly: the downstream copy exits on the first read,
// which closes client (server), which unblocks the upstream copy.
func newEchoConn() net.Conn {
	a, b := net.Pipe()
	b.Close()
	return a
}

func TestServeConnectTunnelOpensUpstreamAndRedacts(t *testing.T) {
	// upstream stub echoes
	up := newEchoConn()
	d := &fakeDialer{conns: map[string]net.Conn{"api.example.com:443": up}}
	rec := &recordLogger{}
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))
	// minimal CONNECT
	fmt.Fprintf(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nAuthorization: Bearer SECRET\r\n\r\n")
	br := bufio.NewReader(client)
	status, _ := br.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("want 200 tunnel established, got %q", status)
	}
	// the dialer was asked for the upstream host:port (host-owned upstream)
	if !d.dialed("api.example.com:443") {
		t.Fatal("upstream was not dialed host-side")
	}
	// redaction: no secret, no header name, no path reached the log
	for _, e := range rec.entries {
		if strings.Contains(e, "SECRET") || strings.Contains(e, "Authorization") {
			t.Fatalf("log leaked secret/header: %q", e)
		}
	}
	if rec.entries[0] != "sbx-1 api.example.com:443" {
		t.Fatalf("egress log should be sandbox + hostport only, got %q", rec.entries[0])
	}
}

func TestServeRejectsUnknownSource(t *testing.T) {
	p := NewProxy(staticResolver{}, &fakeDialer{}, &recordLogger{})
	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.99"))
	fmt.Fprintf(client, "CONNECT x:443 HTTP/1.1\r\n\r\n")
	st, _ := bufio.NewReader(client).ReadString('\n')
	if !strings.Contains(st, "403") {
		t.Fatalf("unknown source must be refused, got %q", st)
	}
}
