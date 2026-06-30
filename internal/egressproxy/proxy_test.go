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
	// allDials records every Dial call regardless of whether conns had an entry.
	// Denial tests assert this is empty (proving no upstream dial happened).
	allDials []string
}

func (f *fakeDialer) Dial(_ context.Context, hostport string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Record every dial attempt so denial tests can assert none happened.
	f.allDials = append(f.allDials, hostport)
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

// dialedAnything returns a copy of every Dial call target regardless of success.
// Denial tests use this to prove no upstream dial reached the network.
func (f *fakeDialer) dialedAnything() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.allDials...)
}

// recordLogger captures Egress calls. It signals done after the first call so
// tests can synchronize on Egress completion without depending on connection
// close ordering.
type recordLogger struct {
	mu       sync.Mutex
	entries  []string
	denials  []string
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

func (r *recordLogger) Deny(sandboxID, hostport string) {
	r.mu.Lock()
	r.denials = append(r.denials, sandboxID+" "+hostport+" denied")
	r.mu.Unlock()
}

func (r *recordLogger) denialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.denials)
}

// stubResolver resolves names from a fixed map; an unmapped name errors. It is
// the ipResolver seam so destination-screening tests never touch real DNS.
type stubResolver struct {
	hosts map[string][]net.IP
}

func (s stubResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	if ips, ok := s.hosts[host]; ok {
		return ips, nil
	}
	return nil, fmt.Errorf("stubResolver: no record for %s", host)
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
	// A public IP literal so destination screening dials it directly (no DNS).
	d := &fakeDialer{conns: map[string]net.Conn{"93.184.216.34:443": up}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))

	// Establish tunnel; include a secret header to verify redaction.
	fmt.Fprintf(client, "CONNECT 93.184.216.34:443 HTTP/1.1\r\nHost: 93.184.216.34:443\r\nAuthorization: Bearer SECRET\r\n\r\n")
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
	if !d.dialed("93.184.216.34:443") {
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
	if len(entries) == 0 || entries[0] != "sbx-1 93.184.216.34:443" {
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
	// Public IP literal (TEST-NET-3), so screening dials it directly.
	d := &fakeDialer{conns: map[string]net.Conn{"203.0.113.9:9000": up}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.2": "sbx-2"}}, d, rec)

	client, server := net.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		p.Serve(server, net.ParseIP("10.0.0.2"))
	}()

	// Establish tunnel.
	fmt.Fprintf(client, "CONNECT 203.0.113.9:9000 HTTP/1.1\r\n\r\n")
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

// readStatus reads the first response line from a client connection with a
// bounded deadline so a hung Serve fails the test instead of blocking forever.
func readStatus(t *testing.T, client net.Conn) string {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	st, err := bufio.NewReader(client).ReadString('\n')
	if err != nil && st == "" {
		t.Fatalf("reading status line: %v", err)
	}
	return st
}

// deniedTargets is the table of destinations the host-side denylist must refuse
// on BOTH the CONNECT and plain-HTTP paths: cloud metadata (IMDS), forkd's own
// loopback control ports, IPv6 loopback, IPv6 link-local, unspecified addresses
// (0.0.0.0 and :: which Linux routes to loopback enabling SSRF to forkd), and
// the NAT64 well-known prefix (64:ff9b::a9fe:a9fe = NAT64 of 169.254.169.254).
// Each must yield a refusal with NO upstream dial.
var deniedTargets = []struct {
	name   string
	target string // host:port as it appears in the request line
}{
	{"imds v4", "169.254.169.254:80"},
	{"forkd grpc loopback", "127.0.0.1:9090"},
	{"forkd sandbox api loopback", "127.0.0.1:9091"},
	{"ipv6 loopback", "[::1]:9090"},
	{"ipv6 link-local", "[fe80::1]:80"},
	// CRITICAL: unspecified addresses bypass deniedNets but Linux routes them
	// to loopback (0.0.0.0->127.0.0.1, ::->  ::1), enabling SSRF to forkd.
	{"unspecified v4 ssrf forkd grpc", "0.0.0.0:9090"},
	{"unspecified v4 ssrf forkd api", "0.0.0.0:9091"},
	{"unspecified v6 ssrf forkd grpc", "[::]:9090"},
	// IMPORTANT: NAT64 mapped IMDS (64:ff9b::a9fe:a9fe = NAT64 of 169.254.169.254)
	{"nat64 imds", "[64:ff9b::a9fe:a9fe]:80"},
}

func TestServeConnectRefusesDeniedDestinations(t *testing.T) {
	for _, tc := range deniedTargets {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDialer{conns: map[string]net.Conn{}}
			rec := newRecordLogger()
			p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

			client, server := net.Pipe()
			serveDone := make(chan struct{})
			go func() { defer close(serveDone); p.Serve(server, net.ParseIP("10.0.0.6")) }()

			fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", tc.target, tc.target)
			st := readStatus(t, client)
			if !strings.Contains(st, "403") {
				t.Fatalf("denied CONNECT %s must return 403, got %q", tc.target, st)
			}
			client.Close()
			select {
			case <-serveDone:
			case <-time.After(3 * time.Second):
				t.Fatal("Serve hung after denied CONNECT")
			}
			// No upstream dial may have happened for a denied target.
			// dialedAnything() records EVERY Dial call (not just conns hits),
			// so a bypass would be caught even if conns is empty.
			if got := d.dialedAnything(); len(got) != 0 {
				t.Fatalf("denied target was dialed upstream: %v", got)
			}
			if rec.denialCount() == 0 {
				t.Fatal("expected a denial to be logged")
			}
		})
	}
}

func TestServePlainRefusesDeniedDestination(t *testing.T) {
	d := &fakeDialer{conns: map[string]net.Conn{}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); p.Serve(server, net.ParseIP("10.0.0.6")) }()

	// Plain-HTTP path to the IMDS endpoint must be refused before any dial.
	fmt.Fprintf(client, "GET http://169.254.169.254/latest/meta-data/ HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n")
	st := readStatus(t, client)
	if !strings.Contains(st, "403") && !strings.Contains(st, "502") {
		t.Fatalf("denied plain-HTTP must return 403/502, got %q", st)
	}
	client.Close()
	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve hung after denied plain-HTTP")
	}
	if got := d.dialedAnything(); len(got) != 0 {
		t.Fatalf("denied plain-HTTP target was dialed upstream: %v", got)
	}
}

// TestServeRefusesRebindToDeniedIP proves DNS-rebinding safety: a name that
// resolves to a denied IP is refused even though the request used a hostname,
// and no upstream dial happens.
func TestServeRefusesRebindToDeniedIP(t *testing.T) {
	d := &fakeDialer{conns: map[string]net.Conn{}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)
	// rebind.evil resolves to the IMDS address.
	p.resolveIP = stubResolver{hosts: map[string][]net.IP{
		"rebind.evil": {net.ParseIP("169.254.169.254")},
	}}

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))
	fmt.Fprintf(client, "CONNECT rebind.evil:80 HTTP/1.1\r\nHost: rebind.evil\r\n\r\n")
	st := readStatus(t, client)
	if !strings.Contains(st, "403") {
		t.Fatalf("rebinding name to denied IP must be refused, got %q", st)
	}
	if got := d.dialedAnything(); len(got) != 0 {
		t.Fatalf("rebinding target was dialed upstream: %v", got)
	}
}

// TestServeAllowsPublicViaResolver proves a normal public name still works: it
// resolves to a non-denied IP, the proxy dials that vetted IP, and the tunnel
// is established.
func TestServeAllowsPublicViaResolver(t *testing.T) {
	up := newEchoConn()
	// The vetted IP literal is what gets dialed (rebind-safe), not the name.
	d := &fakeDialer{conns: map[string]net.Conn{"93.184.216.34:443": up}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)
	p.resolveIP = stubResolver{hosts: map[string][]net.IP{
		"public.example.com": {net.ParseIP("93.184.216.34")},
	}}

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))
	fmt.Fprintf(client, "CONNECT public.example.com:443 HTTP/1.1\r\nHost: public.example.com:443\r\n\r\n")
	st := readStatus(t, client)
	if !strings.HasPrefix(st, "HTTP/1.1 200") {
		t.Fatalf("public target must establish a tunnel, got %q", st)
	}
	if !d.dialed("93.184.216.34:443") {
		t.Fatal("public target was not dialed at its vetted IP")
	}
	client.Close()
}

// TestServeRejectsOverCapHeader proves the preamble byte cap (I2): a request
// preamble larger than the cap with no newline is rejected and Serve returns
// promptly instead of buffering unboundedly (host OOM) or hanging.
func TestServeRejectsOverCapHeader(t *testing.T) {
	d := &fakeDialer{conns: map[string]net.Conn{}}
	rec := newRecordLogger()
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); p.Serve(server, net.ParseIP("10.0.0.6")) }()

	// Write a giant request line with no terminating newline. net.Pipe is
	// synchronous, so push the write off the test goroutine; once the proxy
	// stops reading at the cap, the remaining write unblocks when Serve closes.
	go func() {
		_, _ = io.WriteString(client, "GET http://x/"+strings.Repeat("a", maxPreambleBytes+4096))
		client.Close()
	}()

	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve hung on an over-cap headerless preamble: byte cap not enforced")
	}
	if got := d.dialedAnything(); len(got) != 0 {
		t.Fatalf("over-cap preamble must not dial upstream: %v", got)
	}
}
