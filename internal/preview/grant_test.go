package preview

import (
	"testing"
	"time"
)

func TestGrantRoundTrip(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{
		Sub:           "user-1",
		Email:         "user@example.com",
		EmailVerified: true,
		OrgIDs:        []string{"org-a", "org-b"},
	}
	exp := time.Now().Add(5 * time.Minute)

	tok, err := gs.Mint("app1", id, exp)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	got, err := gs.Verify(tok, "app1", time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Sub != id.Sub {
		t.Errorf("Sub: got %q want %q", got.Sub, id.Sub)
	}
	if got.Email != id.Email {
		t.Errorf("Email: got %q want %q", got.Email, id.Email)
	}
	if got.EmailVerified != id.EmailVerified {
		t.Errorf("EmailVerified: got %v want %v", got.EmailVerified, id.EmailVerified)
	}
	if len(got.OrgIDs) != len(id.OrgIDs) {
		t.Errorf("OrgIDs len: got %d want %d", len(got.OrgIDs), len(id.OrgIDs))
	}
}

func TestGrantExpired(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	past := time.Now().Add(-1 * time.Minute)

	tok, err := gs.Mint("app1", id, past)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = gs.Verify(tok, "app1", time.Now())
	if err == nil {
		t.Fatal("expected error for expired grant, got nil")
	}
}

func TestGrantTamperedPayload(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	tok, err := gs.Mint("app1", id, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Flip a byte in the payload part (before the dot).
	dotIdx := -1
	for i, c := range tok {
		if c == '.' {
			dotIdx = i
			break
		}
	}
	if dotIdx < 2 {
		t.Fatal("unexpected token shape")
	}
	tampered := []byte(tok)
	tampered[0] ^= 0x01
	_, err = gs.Verify(string(tampered), "app1", time.Now())
	if err == nil {
		t.Fatal("expected error for tampered payload, got nil")
	}
}

func TestGrantTamperedTag(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	tok, err := gs.Mint("app1", id, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Flip a byte in the tag part (after the dot).
	dotIdx := -1
	for i, c := range tok {
		if c == '.' {
			dotIdx = i
			break
		}
	}
	tampered := []byte(tok)
	tampered[dotIdx+1] ^= 0x01
	_, err = gs.Verify(string(tampered), "app1", time.Now())
	if err == nil {
		t.Fatal("expected error for tampered tag, got nil")
	}
}

func TestGrantWrongKey(t *testing.T) {
	secret1 := []byte("supersecretkey1234567890")
	secret2 := []byte("differentsecret12345678")
	gs1, _ := NewGrantSigner(secret1)
	gs2, _ := NewGrantSigner(secret2)

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	tok, err := gs1.Mint("app1", id, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = gs2.Verify(tok, "app1", time.Now())
	if err == nil {
		t.Fatal("expected error for wrong key, got nil")
	}
}

func TestGrantWrongLabel(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	tok, err := gs.Mint("app1", id, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = gs.Verify(tok, "app2", time.Now())
	if err == nil {
		t.Fatal("expected error for wrong label, got nil")
	}
}

func TestGrantSingleUse(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	tok, err := gs.Mint("app1", id, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	now := time.Now()
	if _, err := gs.Verify(tok, "app1", now); err != nil {
		t.Fatalf("first Verify failed: %v", err)
	}
	_, err = gs.Verify(tok, "app1", now)
	if err == nil {
		t.Fatal("expected error for second Verify of same token (single-use), got nil")
	}
}

func TestGrantSingleUseFreshTokenStillVerifies(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	gs, err := NewGrantSigner(secret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	exp := time.Now().Add(5 * time.Minute)

	tok1, _ := gs.Mint("app1", id, exp)
	tok2, _ := gs.Mint("app1", id, exp)

	now := time.Now()
	if _, err := gs.Verify(tok1, "app1", now); err != nil {
		t.Fatalf("tok1 first Verify failed: %v", err)
	}
	// tok1 is used; tok2 should still verify (different nonce).
	if _, err := gs.Verify(tok2, "app1", now); err != nil {
		t.Fatalf("tok2 Verify failed (fresh nonce should be allowed): %v", err)
	}
}

func TestGrantSecretTooShort(t *testing.T) {
	_, err := NewGrantSigner([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short secret, got nil")
	}
}
