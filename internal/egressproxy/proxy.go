// Package egressproxy implements a host-side HTTP forward proxy for sandboxed
// guests. Every upstream socket is obtained through the Dialer seam so that a
// live-forked guest cannot inherit an open upstream connection from its parent.
// Each connection is attributed to a sandbox by source IP, and the Logger
// receives only the sandbox ID, host:port, and byte counts: no headers, bodies,
// paths, query strings, or auth values are ever passed to it.
package egressproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
)

// SandboxResolver maps a guest source IP to the sandbox that owns it.
type SandboxResolver interface {
	Lookup(srcIP net.IP) (sandboxID string, ok bool)
}

// Dialer opens a host-side upstream TCP connection to the given host:port. All
// upstream sockets must be obtained through this seam so the host process owns
// every socket; a forked guest must never inherit an already-open upstream.
type Dialer interface {
	Dial(ctx context.Context, hostport string) (net.Conn, error)
}

// Logger records egress events. Implementations must accept only sandboxID,
// hostport, and byte counts. No header names, header values, paths, query
// strings, or auth tokens are ever forwarded here.
type Logger interface {
	Egress(sandboxID, hostport string, bytesUp, bytesDown int64)
}

// Proxy is a host-owned HTTP forward proxy. It handles CONNECT tunnel requests
// and plain HTTP forward requests from sandboxed guests.
type Proxy struct {
	resolver SandboxResolver
	dialer   Dialer
	logger   Logger

	// mu guards the listener handle and the closed flag so Close can race with
	// ListenAndServe (Close may be invoked before the listener is bound).
	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// NewProxy constructs a Proxy with the given resolver, dialer, and logger seams.
func NewProxy(r SandboxResolver, d Dialer, l Logger) *Proxy {
	return &Proxy{resolver: r, dialer: d, logger: l}
}

// Close stops the listener started by ListenAndServe, unblocking its Accept
// loop and making ListenAndServe return nil (a clean shutdown, not an error).
// It is safe to call before ListenAndServe has bound the listener (the bind is
// then skipped) and safe to call more than once. It mirrors the dnsproxy
// Server.Shutdown parity so forkd can shut the proxy down gracefully on signal.
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

// Serve handles one client connection. srcIP is the guest IP used to attribute
// the connection to a sandbox via the SandboxResolver. Serve closes client when
// it returns.
func (p *Proxy) Serve(client net.Conn, srcIP net.IP) {
	defer client.Close()

	br := bufio.NewReader(client)

	// Read the HTTP request line.
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimRight(line, "\r\n")

	// parseRequestTarget errors embed the raw request line or URI; callers must
	// NOT log or forward them as they may contain paths, query strings, or auth
	// values. Serve already discards the error and writes a static 400 response.
	_, hostport, isConnect, err := parseRequestTarget(line)
	if err != nil {
		_, _ = fmt.Fprintf(client, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}

	// Attribution: resolve the guest IP to its sandbox ID before any headers
	// are drained so the resolver decision is made without reading auth values.
	sandboxID, ok := p.resolver.Lookup(srcIP)
	if !ok {
		drainHeaders(br)
		_, _ = fmt.Fprintf(client, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}

	if isConnect {
		// Drain CONNECT request headers: they are never logged or forwarded.
		drainHeaders(br)
		p.serveConnect(client, sandboxID, hostport)
	} else {
		// Plain HTTP: collect headers for replay to upstream, but never log them.
		headers := collectHeaders(br)
		p.servePlain(client, br, sandboxID, hostport, line, headers)
	}
}

// serveConnect establishes an HTTP CONNECT tunnel. It dials the upstream
// host-side, replies with 200, then bidirectionally copies bytes until either
// side closes. When either copy direction finishes, both connections are closed
// (net.Conn.Close is idempotent) so the other direction unblocks promptly.
// Logger.Egress is called exactly once after both goroutines exit, with the
// cumulative up and down byte counts. The deferred upstream.Close is a backstop
// only; the goroutines drive the actual shutdown.
func (p *Proxy) serveConnect(client net.Conn, sandboxID, hostport string) {
	upstream, err := p.dialer.Dial(context.Background(), hostport)
	if err != nil {
		_, _ = fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	_, err = fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	if err != nil {
		return
	}

	var upBytes, downBytes int64
	var wg sync.WaitGroup
	wg.Add(2)

	// UP: client (guest) -> upstream.
	// On return, close both ends so DOWN's blocked read unblocks promptly.
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		upBytes = n
		upstream.Close()
		client.Close()
	}()

	// DOWN: upstream -> client (guest).
	// On return, close both ends so UP's blocked read unblocks promptly.
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		downBytes = n
		client.Close()
		upstream.Close()
	}()

	wg.Wait()
	p.logger.Egress(sandboxID, hostport, upBytes, downBytes)
}

// servePlain handles a plain HTTP forward request. It dials the upstream
// host-side, replays the request (first line + headers + body), streams the
// response back to the client, and logs only sandbox ID, host:port, and byte
// counts.
func (p *Proxy) servePlain(client net.Conn, br *bufio.Reader, sandboxID, hostport, firstLine string, headers []string) {
	upstream, err := p.dialer.Dial(context.Background(), hostport)
	if err != nil {
		_, _ = fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	var upBytes, downBytes int64

	// Replay the request line.
	n, _ := fmt.Fprintf(upstream, "%s\r\n", firstLine)
	upBytes += int64(n)

	// Replay headers (never logged).
	for _, h := range headers {
		n2, _ := io.WriteString(upstream, h)
		upBytes += int64(n2)
	}

	// Stream any remaining body bytes (e.g. POST body).
	n3, _ := io.Copy(upstream, br)
	upBytes += n3

	// Stream the response back to the client.
	n4, _ := io.Copy(client, upstream)
	downBytes = n4

	p.logger.Egress(sandboxID, hostport, upBytes, downBytes)
}

// drainHeaders reads and discards all HTTP header lines (never logging them)
// up to and including the blank line that terminates the header block.
func drainHeaders(br *bufio.Reader) {
	for {
		h, err := br.ReadString('\n')
		if err != nil || strings.TrimRight(h, "\r\n") == "" {
			return
		}
	}
}

// collectHeaders reads all HTTP header lines (including the terminating blank
// line) into a slice so they can be forwarded to the upstream. Values are
// forwarded verbatim without being logged.
func collectHeaders(br *bufio.Reader) []string {
	var headers []string
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			break
		}
		headers = append(headers, h)
		if strings.TrimRight(h, "\r\n") == "" {
			break
		}
	}
	return headers
}

// parseRequestTarget parses the first line of an HTTP request and returns the
// method, the host:port target (host and port only; never path or query), a
// flag indicating a CONNECT tunnel, and any parse error.
//
// For CONNECT the target is the authority directly (host:port).
// For plain HTTP the target is an absolute-form URI; only the host and port are
// extracted, stripping path, query, and fragment so they never reach the Logger.
//
// IMPORTANT: error values returned by this function embed the raw request line
// or URI. Callers must never log or forward these errors; they may contain
// paths, query strings, or auth values. Serve discards them and writes a static
// 400 response.
func parseRequestTarget(line string) (method, hostport string, isConnect bool, err error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", "", false, fmt.Errorf("malformed request line: %q", line)
	}
	method = parts[0]
	target := parts[1]

	if strings.ToUpper(method) == "CONNECT" {
		// CONNECT target IS the host:port authority.
		return method, target, true, nil
	}

	// Plain HTTP: target is an absolute URI. Extract only host:port.
	u, uerr := url.Parse(target)
	if uerr != nil {
		return "", "", false, fmt.Errorf("parse request URI: %w", uerr)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return method, net.JoinHostPort(host, port), false, nil
}
