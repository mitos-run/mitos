package preview

import (
	"crypto/tls"
	"testing"
)

func TestSelfSignedProviderIssuesPerHost(t *testing.T) {
	cp, err := NewSelfSignedProvider()
	if err != nil {
		t.Fatalf("NewSelfSignedProvider: %v", err)
	}
	hello := &tls.ClientHelloInfo{ServerName: "sb-1.example.com"}
	cert, err := cp.GetCertificate(hello)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("expected a certificate, got none")
	}
	// A second call for the same host must return a cached identical cert.
	cert2, err := cp.GetCertificate(hello)
	if err != nil {
		t.Fatalf("GetCertificate (cached): %v", err)
	}
	if string(cert.Certificate[0]) != string(cert2.Certificate[0]) {
		t.Error("expected per-host cert to be cached and stable across calls")
	}
}

func TestSelfSignedProviderRejectsEmptySNI(t *testing.T) {
	cp, err := NewSelfSignedProvider()
	if err != nil {
		t.Fatalf("NewSelfSignedProvider: %v", err)
	}
	if _, err := cp.GetCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("expected error for empty SNI (no on-demand host)")
	}
}

// Compile-time assertion that the self-signed provider satisfies CertProvider.
var _ CertProvider = (*SelfSignedProvider)(nil)
