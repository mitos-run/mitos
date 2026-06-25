package preview

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// CertProvider supplies a TLS certificate for a preview hostname on demand. It
// is the single seam behind which real ACME (CertMagic on-demand TLS) is wired:
// the routing and signing core never depend on a working ACME path, so they are
// unit testable without a public domain. A production deployment supplies a
// CertMagicProvider (see the doc comment below); tests and air-gapped clusters
// supply a SelfSignedProvider or a maintainer-provided wildcard cert.
//
// The signature matches tls.Config.GetCertificate so a CertProvider can be
// installed directly:
//
//	tls.Config{GetCertificate: provider.GetCertificate}
type CertProvider interface {
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
}

// SelfSignedProvider mints a self-signed certificate per requested SNI host and
// caches it. It exists so the proxy serves HTTPS in tests and on bare metal
// without ACME; a self-signed cert is NOT trusted by browsers and is never a
// substitute for real on-demand TLS in production. It implements the same
// per-hostname, mint-on-first-request shape as CertMagic on-demand TLS, so
// swapping in the real provider changes no proxy code.
type SelfSignedProvider struct {
	mu    sync.Mutex
	caKey *ecdsa.PrivateKey
	certs map[string]*tls.Certificate
}

// NewSelfSignedProvider returns a SelfSignedProvider with a fresh signing key.
func NewSelfSignedProvider() (*SelfSignedProvider, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("preview: generate self-signed key: %w", err)
	}
	return &SelfSignedProvider{caKey: key, certs: make(map[string]*tls.Certificate)}, nil
}

// GetCertificate returns a cached or freshly minted self-signed certificate for
// hello.ServerName. It rejects an empty SNI: on-demand TLS requires the client
// to send the preview hostname so the proxy knows which sandbox to certify.
func (p *SelfSignedProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return nil, errors.New("preview: no SNI server name; on-demand TLS requires the preview hostname")
	}
	host := hello.ServerName
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.certs[host]; ok {
		return c, nil
	}
	c, err := p.mint(host)
	if err != nil {
		return nil, err
	}
	p.certs[host] = c
	return c, nil
}

func (p *SelfSignedProvider) mint(host string) (*tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("preview: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &p.caKey.PublicKey, p.caKey)
	if err != nil {
		return nil, fmt.Errorf("preview: create self-signed cert: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: p.caKey}, nil
}

// WildcardProvider serves a single operator-provided certificate (a wildcard
// *.<expose-domain> cert) for every SNI host. The certificate is loaded once at
// startup via tls.LoadX509KeyPair; matching the hostname to the wildcard is the
// certificate's job, performed by the client. This is the production and bare
// metal TLS path (a cert-manager-issued or operator-provided wildcard), distinct
// from the self-signed test provider and the documented on-demand CertMagic seam.
type WildcardProvider struct {
	cert tls.Certificate
}

// NewWildcardProvider loads the cert and key from disk. It returns an error if
// either file is missing or unparseable, so a misconfigured deployment fails
// closed at startup rather than serving a broken handshake.
func NewWildcardProvider(certFile, keyFile string) (*WildcardProvider, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("preview: load wildcard cert: %w", err)
	}
	return &WildcardProvider{cert: cert}, nil
}

// GetCertificate returns the loaded wildcard certificate for any non-empty SNI.
func (p *WildcardProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return nil, errors.New("preview: no SNI server name")
	}
	c := p.cert
	return &c, nil
}

// ServerTLSConfig returns the TLS config the expose proxy serves with. It sets
// GetCertificate from the provider and a TLS 1.2 floor (TLS 1.3 is negotiated
// when the client supports it). It deliberately leaves CurvePreferences NIL so
// Go's default key-exchange preference applies, which on Go 1.24 and newer leads
// with the hybrid post-quantum group X25519MLKEM768 (FIPS 203 ML-KEM-768 plus
// X25519), giving harvest-now-decrypt-later confidentiality. DO NOT set
// CurvePreferences here: pinning the curve list silently drops the post-quantum
// default. The guardrail test in tls_pq_test.go fails if this regresses.
func ServerTLSConfig(cp CertProvider) *tls.Config {
	return &tls.Config{
		GetCertificate: cp.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

// CertMagicProvider documents the production on-demand TLS adapter. It is NOT
// compiled with the certmagic dependency in this slice: real ACME issuance
// needs a public domain, a DNS record for *.<domain> (or per-host A
// records), and a publicly reachable endpoint, none of which exist in CI, so
// wiring the heavy dependency now would add an unverifiable code path. The
// adapter is a thin follow-up: a maintainer with a domain installs certmagic
// and implements CertProvider as below.
//
// Production wiring (follow-up, requires the certmagic dependency):
//
//	import "github.com/caddyserver/certmagic"
//
//	type CertMagicProvider struct{ cache *certmagic.Cache; cfg *certmagic.Config }
//
//	func NewCertMagicProvider(domain, email string, decide func(name string) error) (*CertMagicProvider, error) {
//	    cfg := certmagic.NewDefault()
//	    cfg.OnDemand = &certmagic.OnDemandConfig{
//	        // DecisionFunc gates issuance: only mint for a host that parses to a
//	        // live preview route (ParseHost + RouteTable.Lookup), so the proxy is
//	        // not a CA-rate-limit amplifier for arbitrary SNI.
//	        DecisionFunc: func(_ context.Context, name string) error { return decide(name) },
//	    }
//	    cfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
//	        CA: certmagic.LetsEncryptProductionCA, Email: email, Agreed: true,
//	    })}
//	    return &CertMagicProvider{cfg: cfg}, nil
//	}
//
//	func (p *CertMagicProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
//	    return p.cfg.GetCertificate(hello)
//	}
//
// The DecisionFunc MUST consult the route table so the proxy only asks the CA
// for a hostname that resolves to a real Ready sandbox; this caps ACME rate-limit
// exposure. The bare-metal TLS story (self-hosted ACME such as step-ca, or a
// maintainer-provided wildcard *.<domain> cert loaded via
// tls.LoadX509KeyPair) is documented in docs/preview-urls.md.
type CertMagicProvider struct{}
