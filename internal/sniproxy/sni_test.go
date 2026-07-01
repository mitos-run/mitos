package sniproxy

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// captureConn is a net.Conn whose Write captures bytes into w and whose Read
// returns EOF, so a tls.Client handshake against it writes a complete
// ClientHello (then fails on the empty response). It is the generator for real,
// stdlib-produced ClientHello byte streams used across the peek tests.
type captureConn struct {
	w *bytes.Buffer
}

func (c captureConn) Read(p []byte) (int, error)         { return 0, io.EOF }
func (c captureConn) Write(p []byte) (int, error)        { return c.w.Write(p) }
func (c captureConn) Close() error                       { return nil }
func (c captureConn) LocalAddr() net.Addr                { return nil }
func (c captureConn) RemoteAddr() net.Addr               { return nil }
func (c captureConn) SetDeadline(t time.Time) error      { return nil }
func (c captureConn) SetReadDeadline(t time.Time) error  { return nil }
func (c captureConn) SetWriteDeadline(t time.Time) error { return nil }

// clientHelloBytes returns a real TLS ClientHello produced by the stdlib for the
// given server name and version window. An empty serverName omits the SNI
// extension (the no-SNI case). minVer/maxVer pin the produced ClientHello to TLS
// 1.2 or TLS 1.3.
func clientHelloBytes(t *testing.T, serverName string, minVer, maxVer uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := &tls.Config{
		ServerName:         serverName,
		MinVersion:         minVer,
		MaxVersion:         maxVer,
		InsecureSkipVerify: true, //nolint:gosec // test-only: we only capture the ClientHello bytes, never complete a handshake.
	}
	c := tls.Client(captureConn{w: &buf}, cfg)
	// Handshake fails (the captureConn returns EOF for the server response), but
	// the ClientHello has already been written to buf.
	_ = c.Handshake()
	if buf.Len() == 0 {
		t.Fatalf("generated empty ClientHello for %q", serverName)
	}
	return buf.Bytes()
}

func TestPeekClientHelloTLS12WithSNI(t *testing.T) {
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS12)
	name, peeked, err := peekClientHello(bytes.NewReader(hello))
	if err != nil {
		t.Fatalf("peekClientHello: %v", err)
	}
	if name != "api.example.com" {
		t.Fatalf("server name = %q, want api.example.com", name)
	}
	if !bytes.Equal(peeked, hello) {
		t.Fatalf("peeked bytes (%d) do not equal the ClientHello (%d)", len(peeked), len(hello))
	}
}

func TestPeekClientHelloTLS13WithSNI(t *testing.T) {
	hello := clientHelloBytes(t, "db.svc.internal.example.net", tls.VersionTLS13, tls.VersionTLS13)
	name, peeked, err := peekClientHello(bytes.NewReader(hello))
	if err != nil {
		t.Fatalf("peekClientHello: %v", err)
	}
	if name != "db.svc.internal.example.net" {
		t.Fatalf("server name = %q, want db.svc.internal.example.net", name)
	}
	if !bytes.Equal(peeked, hello) {
		t.Fatalf("peeked bytes do not equal the ClientHello")
	}
}

func TestPeekClientHelloNoSNI(t *testing.T) {
	// A ClientHello with no SNI extension: the peek must succeed (it is valid
	// TLS) and report an empty server name so the caller can apply the documented
	// missing-SNI deny policy.
	hello := clientHelloBytes(t, "", tls.VersionTLS12, tls.VersionTLS13)
	name, _, err := peekClientHello(bytes.NewReader(hello))
	if err != nil {
		t.Fatalf("peekClientHello: %v", err)
	}
	if name != "" {
		t.Fatalf("server name = %q, want empty (no SNI)", name)
	}
}

func TestPeekClientHelloNonTLS(t *testing.T) {
	// Plain HTTP bytes are not a TLS record: peek must fail closed.
	_, _, err := peekClientHello(bytes.NewReader([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")))
	if err == nil {
		t.Fatal("peekClientHello accepted non-TLS bytes, want error")
	}
}

func TestPeekClientHelloMalformedTruncated(t *testing.T) {
	// A valid ClientHello truncated mid-record: peek must fail closed.
	hello := clientHelloBytes(t, "api.example.com", tls.VersionTLS12, tls.VersionTLS13)
	_, _, err := peekClientHello(bytes.NewReader(hello[:len(hello)/2]))
	if err == nil {
		t.Fatal("peekClientHello accepted a truncated ClientHello, want error")
	}
}

func TestPeekClientHelloEmpty(t *testing.T) {
	_, _, err := peekClientHello(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("peekClientHello accepted empty input, want error")
	}
}
