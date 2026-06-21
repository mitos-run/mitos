package preview

import (
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSignerRejectsShortKey(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("expected error for short key, got nil")
	}
	if _, err := NewSigner(nil); err == nil {
		t.Fatal("expected error for nil key, got nil")
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	s := testSigner(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := s.Mint("sb-abc", 8080, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok == "" {
		t.Fatal("Mint returned empty token")
	}
	claims, err := s.VerifyAt(tok, now)
	if err != nil {
		t.Fatalf("VerifyAt: %v", err)
	}
	if claims.SandboxID != "sb-abc" {
		t.Errorf("SandboxID = %q, want sb-abc", claims.SandboxID)
	}
	if claims.Port != 8080 {
		t.Errorf("Port = %d, want 8080", claims.Port)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := testSigner(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := s.Mint("sb-abc", 8080, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// One second after expiry must fail.
	if _, err := s.VerifyAt(tok, now.Add(time.Minute+time.Second)); err == nil {
		t.Fatal("expected expired token to be rejected, got nil")
	}
	// Far in the future must also fail (never-accept-after-expiry).
	if _, err := s.VerifyAt(tok, now.Add(48*time.Hour)); err == nil {
		t.Fatal("expected far-future verification to reject expired token")
	}
	// At exactly the boundary it is still valid (expiry is inclusive).
	if _, err := s.VerifyAt(tok, now.Add(time.Minute)); err != nil {
		t.Fatalf("expected token valid at expiry boundary: %v", err)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	s := testSigner(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := s.Mint("sb-abc", 8080, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Flip a character in the payload portion.
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token has no payload.signature shape: %q", tok)
	}
	tampered := mutate(parts[0]) + "." + parts[1]
	if _, err := s.VerifyAt(tampered, now); err == nil {
		t.Fatal("expected tampered payload to be rejected")
	}
	// Tamper the signature.
	tampered2 := parts[0] + "." + mutate(parts[1])
	if _, err := s.VerifyAt(tampered2, now); err == nil {
		t.Fatal("expected tampered signature to be rejected")
	}
	// Garbage.
	if _, err := s.VerifyAt("not-a-token", now); err == nil {
		t.Fatal("expected garbage token to be rejected")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	s := testSigner(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := s.Mint("sb-abc", 8080, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	other, err := NewSigner([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := other.VerifyAt(tok, now); err == nil {
		t.Fatal("expected token signed with a different key to be rejected")
	}
}

func TestMintRejectsBadArgs(t *testing.T) {
	s := testSigner(t)
	now := time.Unix(1_700_000_000, 0)
	if _, err := s.Mint("", 8080, now.Add(time.Hour)); err == nil {
		t.Error("expected error for empty sandbox id")
	}
	if _, err := s.Mint("sb", 0, now.Add(time.Hour)); err == nil {
		t.Error("expected error for zero port")
	}
	if _, err := s.Mint("sb", 70000, now.Add(time.Hour)); err == nil {
		t.Error("expected error for out-of-range port")
	}
}

// mutate flips the first ASCII letter/digit in s to produce a different but
// still base64url-shaped string.
func mutate(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == 'A' {
			b[i] = 'B'
			return string(b)
		}
		if c >= 'a' && c < 'z' {
			b[i] = c + 1
			return string(b)
		}
		if c >= '0' && c < '9' {
			b[i] = c + 1
			return string(b)
		}
	}
	return s + "x"
}
