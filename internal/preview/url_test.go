package preview

import (
	"net/url"
	"testing"
	"time"
)

func TestMintURL(t *testing.T) {
	s := testSigner(t)
	u, err := MintURL(s, "example.com", "sb-1", 8080, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("MintURL produced unparseable URL %q: %v", u, err)
	}
	if parsed.Scheme != "https" {
		t.Errorf("scheme = %q, want https", parsed.Scheme)
	}
	if parsed.Host != "sb-1.example.com" {
		t.Errorf("host = %q, want sb-1.example.com", parsed.Host)
	}
	tok := parsed.Query().Get("token")
	if tok == "" {
		t.Fatal("URL carries no token")
	}
	// The minted token must verify and name the same sandbox and port.
	claims, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("token in URL does not verify: %v", err)
	}
	if claims.SandboxID != "sb-1" || claims.Port != 8080 {
		t.Errorf("claims = %+v", claims)
	}
}

func TestMintURLValidatesArgs(t *testing.T) {
	s := testSigner(t)
	if _, err := MintURL(s, "", "sb-1", 8080, time.Now().Add(time.Hour)); err == nil {
		t.Error("expected error for empty domain")
	}
	if _, err := MintURL(s, "example.com", "sb-1", 0, time.Now().Add(time.Hour)); err == nil {
		t.Error("expected error for bad port")
	}
}
