// Package sniproxy implements a host-side TLS SNI peek-and-splice egress filter
// for sandboxed guests. It complements the controlled DNS resolver
// (internal/dnsproxy, issue #47): the DNS proxy decides which NAMES a sandbox may
// resolve and pins their resolved IPs into nftables; this proxy enforces the SAME
// per-sandbox domain allowlist at TLS-connection time by reading the ClientHello
// SNI server_name WITHOUT terminating TLS, then splicing the connection through
// to the original destination on allow or closing it on deny.
//
// Enforcement is HOST-side and the guest cannot disable it: the connection is
// transparently redirected to this proxy by the per-node nftables datapath, the
// peek and the allowlist decision run in the forkd host process, and the splice
// dial is the host's. The guest never sees the decision and cannot route around
// it.
//
// HONEST LIMITATION: this is SNI-based filtering, not full TLS interception. The
// proxy trusts that the SNI in the ClientHello names the host the connection is
// actually for; it does not terminate TLS, validate the server certificate, or
// inspect any byte after the ClientHello. A client that controls both ends can
// send a benign SNI and speak to a different server (domain fronting), so SNI
// filtering is a policy and egress-shaping control, not a cryptographic
// guarantee. See docs/networking.md and docs/threat-model.md.
package sniproxy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// maxClientHelloBytes bounds how many bytes the peek reads while looking for a
// complete ClientHello, so a guest cannot OOM the host with an endless record. A
// single TLS record body is capped at 16 KiB by the spec; allow a little headroom
// for the 5-byte record header and a ClientHello that spans into a second record.
const maxClientHelloBytes = 18 * 1024

// errClientHelloNotParsed is returned when the bytes are not a parseable TLS
// ClientHello (truncated, malformed, or non-TLS). Callers MUST fail closed.
var errClientHelloNotParsed = errors.New("sniproxy: input is not a parseable TLS ClientHello")

// errAbortAfterPeek aborts the stdlib handshake the instant the ClientHello has
// been parsed, so TLS is never terminated. It is internal and never surfaced.
var errAbortAfterPeek = errors.New("sniproxy: aborting handshake after client hello peek")

// peekClientHello reads the TLS ClientHello from r and returns its SNI
// server_name plus the exact bytes consumed (so they can be replayed/spliced to
// the upstream). It NEVER terminates TLS: it drives the stdlib record/handshake
// parser only far enough to read and parse the ClientHello, then aborts before
// any ServerHello is written. serverName is "" when the ClientHello carries no
// SNI extension (a valid hello; the caller applies the deny-on-missing-SNI
// policy). err is non-nil when the bytes are not a parseable TLS ClientHello, so
// callers fail closed on malformed or non-TLS input.
//
// The read is bounded to maxClientHelloBytes so a guest cannot OOM the host with
// an endless record; the caller is expected to also impose a read deadline on the
// connection to bound slowloris.
func peekClientHello(r io.Reader) (serverName string, peeked []byte, err error) {
	var buf bytes.Buffer
	// TeeReader records every byte the stdlib parser consumes so we can replay the
	// exact ClientHello on splice. LimitReader caps the bytes the parser may read.
	tee := io.TeeReader(io.LimitReader(r, maxClientHelloBytes), &buf)

	var (
		captured bool
		name     string
	)
	hs := tls.Server(readOnlyConn{reader: tee}, &tls.Config{
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			// Reached only after the stdlib has fully parsed the ClientHello.
			captured = true
			name = info.ServerName
			// Abort before the handshake proceeds to write a ServerHello: we peek,
			// we never terminate TLS.
			return nil, errAbortAfterPeek
		},
	})
	hsErr := hs.Handshake()

	if !captured {
		// The parser failed before a ClientHello was produced: truncated,
		// malformed, or non-TLS bytes. Fail closed.
		if hsErr == nil {
			hsErr = errClientHelloNotParsed
		}
		return "", buf.Bytes(), fmt.Errorf("%w: %v", errClientHelloNotParsed, hsErr)
	}
	return name, buf.Bytes(), nil
}

// readOnlyConn adapts an io.Reader to a net.Conn for tls.Server: reads pass
// through, writes are refused (so a handshake that tries to reply aborts), and
// the address/deadline methods are inert. Because GetConfigForClient aborts the
// handshake right after the ClientHello is parsed, Write is never reached on the
// peek path; it returns an error purely as a backstop.
type readOnlyConn struct {
	reader io.Reader
}

func (c readOnlyConn) Read(p []byte) (int, error)         { return c.reader.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c readOnlyConn) Close() error                       { return nil }
func (c readOnlyConn) LocalAddr() net.Addr                { return nil }
func (c readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (c readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (c readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (c readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }
