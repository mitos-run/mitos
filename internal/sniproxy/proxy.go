package sniproxy

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"mitos.run/mitos/internal/egressproxy"
)

// peekTimeout bounds how long Serve waits for the guest to send a complete
// ClientHello, so a guest cannot slowloris the host by opening a connection and
// never sending the handshake. It is cleared before the (unbounded) splice copy.
const peekTimeout = 30 * time.Second

// SandboxResolver maps a guest source IP to the sandbox that owns it, so each
// connection is attributed for logging. It mirrors egressproxy.SandboxResolver.
type SandboxResolver interface {
	Lookup(srcIP net.IP) (sandboxID string, ok bool)
}

// Dialer opens a host-side upstream TCP connection to the given host:port. All
// upstream sockets are obtained through this seam so the host process owns every
// socket; a forked guest must never inherit an already-open upstream.
type Dialer interface {
	Dial(ctx context.Context, hostport string) (net.Conn, error)
}

// Logger records SNI egress decisions. The SNI server_name is NOT a secret (it
// travels in cleartext in the ClientHello on the wire), so it may be logged,
// exactly as the DNS proxy logs the queried name. No bytes after the ClientHello
// are ever inspected or logged.
type Logger interface {
	// Allow records a spliced (allowed) connection with the matched server name,
	// port, and cumulative byte counts.
	Allow(sandboxID, serverName string, port int, bytesUp, bytesDown int64)
	// Deny records a refused connection with the server name (may be empty), the
	// port, and a stable reason: "unknown_sandbox", "malformed_clienthello",
	// "missing_sni", "not_allowlisted", or "destination_denied".
	Deny(sandboxID, serverName string, port int, reason string)
}

// Proxy is a host-owned transparent TLS SNI filter. For each accepted connection
// it peeks the ClientHello SNI, checks it against the sandbox's domain allowlist,
// and splices to the original destination on allow or closes on deny.
type Proxy struct {
	resolver  SandboxResolver
	allowlist Allowlist
	dialer    Dialer
	logger    Logger

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// NewProxy constructs a Proxy from its seams. logger must be non-nil; callers
// that want no logging pass a no-op logger.
func NewProxy(r SandboxResolver, a Allowlist, d Dialer, l Logger) *Proxy {
	return &Proxy{resolver: r, allowlist: a, dialer: d, logger: l}
}

// Close stops the listener started by ListenAndServe, unblocking its Accept loop
// and making ListenAndServe return nil (a clean shutdown). It is safe before the
// listener is bound and safe to call more than once. It mirrors the egressproxy
// Proxy.Close parity so forkd can shut the SNI proxy down on signal.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	ln := p.ln
	p.mu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Close()
}

// Serve handles one transparently-redirected TLS connection. srcIP attributes it
// to a sandbox; dstIP and dstPort are the ORIGINAL destination the guest dialed
// (recovered host-side, e.g. via SO_ORIGINAL_DST), which is the exact target the
// connection is spliced to on allow. Serve closes client when it returns.
//
// Decision order (every refusal closes the connection fail-closed, with no dial):
//  1. attribute srcIP to a sandbox (unknown source is refused);
//  2. peek the ClientHello SNI (malformed/non-TLS is refused);
//  3. deny an empty SNI (the documented missing-SNI policy);
//  4. check the per-sandbox domain allowlist (exact or anchored wildcard, by
//     name and port);
//  5. reapply the host-side denied-IP floor to the original destination, because
//     the splice dial leaves the host OUTPUT path and so bypasses the per-sandbox
//     nftables FORWARD metadata drop (same rationale as the egress proxy);
//  6. dial the original destination and splice (replay the peeked ClientHello,
//     then copy bidirectionally).
func (p *Proxy) Serve(client net.Conn, srcIP, dstIP net.IP, dstPort int) {
	defer client.Close()

	sandboxID, ok := p.resolver.Lookup(srcIP)
	if !ok {
		// No source attribution: the proxy is not an open relay. Nothing to log
		// against a sandbox; record the denial with an empty id.
		p.logger.Deny("", "", dstPort, "unknown_sandbox")
		return
	}

	// Bound the peek so a guest cannot slowloris us; clear it before the splice.
	_ = client.SetReadDeadline(time.Now().Add(peekTimeout))
	serverName, peeked, err := peekClientHello(client)
	if err != nil {
		// Errors from peekClientHello embed parser detail only, never guest bytes
		// beyond the malformed ClientHello; we log a stable reason, not the error.
		p.logger.Deny(sandboxID, "", dstPort, "malformed_clienthello")
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	if serverName == "" {
		// Documented policy: a TLS connection with no SNI is denied (fail closed).
		// A guest cannot reach an allowlisted name by omitting the SNI.
		p.logger.Deny(sandboxID, "", dstPort, "missing_sni")
		return
	}

	if !p.allowlist.Allowed(srcIP, serverName, dstPort) {
		p.logger.Deny(sandboxID, serverName, dstPort, "not_allowlisted")
		return
	}

	// Host-side denied-IP floor (defense in depth): reuse the egress proxy's
	// denylist so a connection whose original destination is cloud metadata,
	// loopback (forkd's own gRPC/sandbox API), link-local, or the unspecified
	// address is refused even if its SNI is allowlisted.
	if egressproxy.IsDeniedIP(dstIP) {
		p.logger.Deny(sandboxID, serverName, dstPort, "destination_denied")
		return
	}

	hostport := net.JoinHostPort(dstIP.String(), strconv.Itoa(dstPort))
	upstream, err := p.dialer.Dial(context.Background(), hostport)
	if err != nil {
		// A reachability failure, not a policy denial: close without a Deny event.
		return
	}
	defer upstream.Close()

	p.splice(client, upstream, sandboxID, serverName, dstPort, peeked)
}

// splice replays the peeked ClientHello to the upstream, then copies bytes
// bidirectionally until either side closes, closing both ends when either copy
// direction finishes so the other unblocks promptly. Logger.Allow is called once
// with the cumulative byte counts. It mirrors egressproxy.serveConnect.
func (p *Proxy) splice(client, upstream net.Conn, sandboxID, serverName string, port int, peeked []byte) {
	var upBytes, downBytes int64

	// Replay the buffered ClientHello first so the upstream sees a normal TLS
	// stream from byte zero.
	if n, werr := upstream.Write(peeked); werr != nil {
		return
	} else {
		upBytes += int64(n)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// UP: client (guest) -> upstream.
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		upBytes += n
		upstream.Close()
		client.Close()
	}()

	// DOWN: upstream -> client (guest).
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		downBytes = n
		client.Close()
		upstream.Close()
	}()

	wg.Wait()
	p.logger.Allow(sandboxID, serverName, port, upBytes, downBytes)
}
