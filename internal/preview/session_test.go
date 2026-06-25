package preview

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	sc, err := NewSessionCodec(secret)
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}

	id := Identity{
		Sub:           "user-1",
		Email:         "user@example.com",
		EmailVerified: true,
		OrgIDs:        []string{"acme", "beta"},
	}
	exp := time.Now().Add(24 * time.Hour)

	val, err := sc.Encode(id, exp)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := sc.Decode(val, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
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
	if !reflect.DeepEqual(got.OrgIDs, id.OrgIDs) {
		t.Errorf("OrgIDs: got %v want %v", got.OrgIDs, id.OrgIDs)
	}
}

func TestSessionExpired(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	sc, err := NewSessionCodec(secret)
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	past := time.Now().Add(-1 * time.Minute)

	val, err := sc.Encode(id, past)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	_, err = sc.Decode(val, time.Now())
	if err == nil {
		t.Fatal("expected error for expired session, got nil")
	}
}

func TestSessionTampered(t *testing.T) {
	secret := []byte("supersecretkey1234567890")
	sc, err := NewSessionCodec(secret)
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	val, err := sc.Encode(id, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	tampered := []byte(val)
	tampered[0] ^= 0x01
	_, err = sc.Decode(string(tampered), time.Now())
	if err == nil {
		t.Fatal("expected error for tampered session, got nil")
	}
}

func TestSessionReusable(t *testing.T) {
	// Unlike grants, sessions are reusable until expiry.
	secret := []byte("supersecretkey1234567890")
	sc, err := NewSessionCodec(secret)
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}

	id := Identity{Sub: "user-1", Email: "user@example.com"}
	val, _ := sc.Encode(id, time.Now().Add(time.Hour))

	now := time.Now()
	if _, err := sc.Decode(val, now); err != nil {
		t.Fatalf("first Decode: %v", err)
	}
	if _, err := sc.Decode(val, now); err != nil {
		t.Fatalf("second Decode (sessions are reusable): %v", err)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	cookie := NewSessionCookie("somevalue", 30*time.Minute)

	if cookie.Name != SessionCookieName {
		t.Errorf("Name: got %q want %q", cookie.Name, SessionCookieName)
	}
	if !cookie.Secure {
		t.Error("Secure must be true for __Host- compliance")
	}
	if !cookie.HttpOnly {
		t.Error("HttpOnly must be true")
	}
	if cookie.Path != "/" {
		t.Errorf("Path: got %q want \"/\"", cookie.Path)
	}
	if cookie.Domain != "" {
		t.Errorf("Domain must be empty for __Host- compliance, got %q", cookie.Domain)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite: got %v want Lax", cookie.SameSite)
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("MaxAge must be positive, got %d", cookie.MaxAge)
	}
}

func TestSessionSecretTooShort(t *testing.T) {
	_, err := NewSessionCodec([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short secret, got nil")
	}
}
