package console

import (
	"net/http"
	"testing"
)

// TestSessionCookieNameForUsesHostPrefixWhenSecure asserts that a secure
// deployment sets the __Host- prefixed cookie (which the browser pins to the
// exact host, defeating cookie tossing from a sibling subdomain) and a
// non-secure (plain-http self-host/dev) deployment falls back to the unprefixed
// name, because the browser only honors __Host- on a Secure cookie (issue #733).
func TestSessionCookieNameForUsesHostPrefixWhenSecure(t *testing.T) {
	if got := SessionCookieNameFor(true); got != HostPrefixedSessionCookieName {
		t.Errorf("secure name = %q, want %q", got, HostPrefixedSessionCookieName)
	}
	if got := SessionCookieNameFor(false); got != SessionCookieName {
		t.Errorf("insecure name = %q, want %q", got, SessionCookieName)
	}
	if HostPrefixedSessionCookieName != "__Host-mitos_session" {
		t.Errorf("host-prefixed name = %q, want __Host-mitos_session", HostPrefixedSessionCookieName)
	}
}

// TestReadSessionCookiePrefersHostPrefixed asserts the reader prefers the
// hardened __Host- cookie and falls back to the legacy name so a session issued
// before the rollout still resolves, and returns "" when neither is present.
func TestReadSessionCookiePrefersHostPrefixed(t *testing.T) {
	t.Run("prefers host-prefixed", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "legacy"})
		r.AddCookie(&http.Cookie{Name: HostPrefixedSessionCookieName, Value: "hardened"})
		if got := ReadSessionCookie(r); got != "hardened" {
			t.Errorf("value = %q, want hardened", got)
		}
	})
	t.Run("falls back to legacy", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "legacy"})
		if got := ReadSessionCookie(r); got != "legacy" {
			t.Errorf("value = %q, want legacy", got)
		}
	})
	t.Run("empty when neither present", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		if got := ReadSessionCookie(r); got != "" {
			t.Errorf("value = %q, want empty", got)
		}
	})
}
