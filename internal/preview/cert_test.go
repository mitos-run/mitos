package preview

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"
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

// writeTempWildcardCert generates a self-signed *.example.com cert+key pair and
// writes them to temp PEM files. Returns (certFile, keyFile, cleanup).
func writeTempWildcardCert(t *testing.T) (string, string, func()) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "*.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"*.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certFile, err := os.CreateTemp(t.TempDir(), "cert*.pem")
	if err != nil {
		t.Fatalf("temp cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	certFile.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyFile, err := os.CreateTemp(t.TempDir(), "key*.pem")
	if err != nil {
		t.Fatalf("temp key file: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	keyFile.Close()

	cleanup := func() {}
	return certFile.Name(), keyFile.Name(), cleanup
}

func TestWildcardProviderServesLoadedCert(t *testing.T) {
	certFile, keyFile, cleanup := writeTempWildcardCert(t)
	defer cleanup()

	wp, err := NewWildcardProvider(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewWildcardProvider: %v", err)
	}

	cert, err := wp.GetCertificate(&tls.ClientHelloInfo{ServerName: "openclaw.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("expected a certificate, got none")
	}
	// The returned cert must be the loaded wildcard cert (compare leaf DER bytes).
	if string(cert.Certificate[0]) != string(wp.cert.Certificate[0]) {
		t.Error("returned cert DER does not match loaded wildcard cert")
	}
}

func TestWildcardProviderRejectsEmptySNI(t *testing.T) {
	certFile, keyFile, cleanup := writeTempWildcardCert(t)
	defer cleanup()

	wp, err := NewWildcardProvider(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewWildcardProvider: %v", err)
	}
	if _, err := wp.GetCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("expected error for empty SNI")
	}
}

func TestNewWildcardProviderMissingFile(t *testing.T) {
	_, err := NewWildcardProvider("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected error for missing cert/key files")
	}
}

// Compile-time assertion that the wildcard provider satisfies CertProvider.
var _ CertProvider = (*WildcardProvider)(nil)
