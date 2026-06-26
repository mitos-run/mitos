package saas

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientIPDefaultsToRemoteAddr asserts that with no trusted proxy hops the
// client IP is the connection RemoteAddr and a caller-set X-Forwarded-For is
// ignored: a spoofed header cannot move the per-IP rate-limit bucket off the real
// source.
func TestClientIPDefaultsToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4") // attacker-set, must be ignored.
	if got := clientIP(r, 0); got != "203.0.113.7" {
		t.Fatalf("clientIP with 0 trusted hops = %q, want 203.0.113.7 (RemoteAddr), spoofed XFF must not win", got)
	}
}

// TestClientIPTrustsRightmostUnderOneHop asserts that with exactly one trusted
// proxy hop the client IP is the rightmost X-Forwarded-For entry (the address the
// trusted proxy observed), not the attacker-prepended leftmost entry.
func TestClientIPTrustsRightmostUnderOneHop(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	r.RemoteAddr = "10.0.0.1:5555" // the trusted ingress.
	// The attacker prepends 1.2.3.4; the trusted ingress appended the real client
	// 9.9.9.9 to the right.
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 9.9.9.9")
	if got := clientIP(r, 1); got != "9.9.9.9" {
		t.Fatalf("clientIP with 1 trusted hop = %q, want 9.9.9.9 (rightmost, the trusted-observed client)", got)
	}
}

// TestClientIPSpoofShorterListFailsClosed asserts that when the X-Forwarded-For
// list is shorter than the trusted hop count (a spoof attempt) the gateway falls
// back to RemoteAddr rather than trusting an attacker-set entry.
func TestClientIPSpoofShorterListFailsClosed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	// Two trusted hops are expected but the attacker supplied only one entry.
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(r, 2); got != "10.0.0.1" {
		t.Fatalf("clientIP with a too-short XFF list = %q, want 10.0.0.1 (RemoteAddr fail-closed)", got)
	}
}

// TestClientIPContextRoundTrip asserts the context helpers round-trip the
// resolved IP so the quota adapter's IPOf seam reads the trusted source.
func TestClientIPContextRoundTrip(t *testing.T) {
	ctx := withClientIP(t.Context(), "198.51.100.2")
	if got := ClientIPFromContext(ctx); got != "198.51.100.2" {
		t.Fatalf("ClientIPFromContext = %q, want 198.51.100.2", got)
	}
	if got := ClientIPFromContext(t.Context()); got != "" {
		t.Fatalf("ClientIPFromContext with no value = %q, want empty", got)
	}
}
