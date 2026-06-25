package preview

import (
	"crypto/tls"
	"testing"
)


// TestServerTLSConfigNegotiatesPostQuantum proves that ServerTLSConfig produces
// a config that negotiates X25519MLKEM768 when the client offers it exclusively.
// A server with CurvePreferences pinned to classical curves would fail this
// client's handshake, so a passing test proves the PQ key exchange is live.
//
// The second assertion, cfg.CurvePreferences == nil, is the load-bearing
// invariant: setting CurvePreferences silently drops X25519MLKEM768 from Go's
// default preference list. Do NOT set CurvePreferences on the server config.
// If this test fails, the message is: "post-quantum key exchange regressed;
// do not set CurvePreferences".
func TestServerTLSConfigNegotiatesPostQuantum(t *testing.T) {
	provider, err := NewSelfSignedProvider()
	if err != nil {
		t.Fatalf("NewSelfSignedProvider: %v", err)
	}

	cfg := ServerTLSConfig(provider)

	// Load-bearing invariant: CurvePreferences must remain nil. Pinning curves
	// silently removes X25519MLKEM768 from Go's default preference list.
	// post-quantum key exchange regressed; do not set CurvePreferences
	if cfg.CurvePreferences != nil {
		t.Errorf("post-quantum key exchange regressed; do not set CurvePreferences: got %v, want nil", cfg.CurvePreferences)
	}

	// Start a TLS listener using the server config.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	errc := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		// Drive the handshake to completion, then close.
		if err := conn.(*tls.Conn).Handshake(); err != nil {
			errc <- err
			return
		}
		conn.Close()
		errc <- nil
	}()

	// Client offers ONLY X25519MLKEM768 at TLS 1.3 minimum. A server that does
	// not support this group cannot complete the handshake.
	clientCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test-only; no trust anchor needed
		CurvePreferences:   []tls.CurveID{tls.X25519MLKEM768},
		MinVersion:         tls.VersionTLS13,
		ServerName:         "test.example.com",
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("PQ-only client handshake failed (post-quantum key exchange regressed; do not set CurvePreferences): %v", err)
	}
	defer conn.Close()

	// ConnectionState().CurveID is available in Go 1.24+; Go 1.26 has it.
	cs := conn.ConnectionState()
	if cs.CurveID != tls.X25519MLKEM768 {
		t.Errorf("expected negotiated CurveID X25519MLKEM768 (%d), got %d (post-quantum key exchange regressed; do not set CurvePreferences)", tls.X25519MLKEM768, cs.CurveID)
	}

	if serverErr := <-errc; serverErr != nil {
		t.Errorf("server side handshake error: %v", serverErr)
	}
}

// TestServerTLSConfigFieldsAndInvariants checks the basic field values returned
// by ServerTLSConfig.
func TestServerTLSConfigFieldsAndInvariants(t *testing.T) {
	provider, err := NewSelfSignedProvider()
	if err != nil {
		t.Fatalf("NewSelfSignedProvider: %v", err)
	}

	cfg := ServerTLSConfig(provider)

	if cfg.GetCertificate == nil {
		t.Error("expected GetCertificate to be set")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2, got %d", cfg.MinVersion)
	}
	if cfg.CurvePreferences != nil {
		t.Errorf("post-quantum key exchange regressed; do not set CurvePreferences: got %v", cfg.CurvePreferences)
	}

	// Verify GetCertificate routes through the provider.
	cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "sb-1.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate via config: %v", err)
	}
	if cert == nil {
		t.Fatal("expected cert, got nil")
	}
}

