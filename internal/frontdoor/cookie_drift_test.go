package frontdoor

import (
	"testing"

	"mitos.run/mitos/internal/saas/console"
)

// TestSessionCookieNamesMatchConsole guards the intentionally duplicated cookie
// name constants in proxy.go against silent drift from the console package. The
// frontdoor duplicates these to avoid a production dependency on console, so
// nothing but this test keeps them equal; a rename in either package must fail
// CI here rather than silently break session resolution in production.
func TestSessionCookieNamesMatchConsole(t *testing.T) {
	if hostPrefixedSessionCookieName != console.HostPrefixedSessionCookieName {
		t.Errorf("hostPrefixedSessionCookieName = %q, console.HostPrefixedSessionCookieName = %q; the duplicated constants have drifted",
			hostPrefixedSessionCookieName, console.HostPrefixedSessionCookieName)
	}
	if legacySessionCookieName != console.SessionCookieName {
		t.Errorf("legacySessionCookieName = %q, console.SessionCookieName = %q; the duplicated constants have drifted",
			legacySessionCookieName, console.SessionCookieName)
	}
}
