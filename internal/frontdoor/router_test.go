package frontdoor_test

import (
	"testing"

	"mitos.run/mitos/internal/frontdoor"
)

func TestDecide(t *testing.T) {
	reserved := frontdoor.DefaultReserved()

	cases := []struct {
		path               string
		wantUpstream       string
		wantRequireSession bool
		wantIsRoot         bool
	}{
		// Root: marketing, IsRoot=true, no session.
		{"/", "marketing", false, true},

		// Marketing-reserved paths: no session.
		{"/pricing", "marketing", false, false},
		{"/pricing/enterprise", "marketing", false, false},
		{"/docs", "marketing", false, false},
		{"/docs/quickstart", "marketing", false, false},
		{"/use-cases", "marketing", false, false},
		{"/use-cases/ci", "marketing", false, false},
		{"/compare", "marketing", false, false},
		{"/blog", "marketing", false, false},
		{"/blog/post-1", "marketing", false, false},
		{"/about", "marketing", false, false},
		{"/assets/x.js", "marketing", false, false},

		// Auth + onboarding paths: console, no session.
		{"/login", "console", false, false},
		{"/signup", "console", false, false},
		{"/verify", "console", false, false},
		{"/auth/login", "console", false, false},
		{"/auth/callback", "console", false, false},
		{"/onboarding", "console", false, false},
		{"/onboarding/org", "console", false, false},

		// App paths: console, session required.
		{"/console", "console", true, false},
		{"/console/keys", "console", true, false},
		{"/app", "console", true, false},
		{"/api", "console", true, false},
		{"/settings", "console", true, false},
		{"/new", "console", true, false},

		// Org-slug paths: console, session required.
		{"/acme", "console", true, false},
		{"/acme/sandboxes", "console", true, false},
		{"/mycorp/keys", "console", true, false},

		// Reserved words are NOT treated as org slugs.
		// "/login" must go to console no-session, not slug route.
		// This is already covered above; add explicit check for slug-like names
		// that happen to be in the reserved set.
		{"/new/project", "console", true, false}, // "new" is reserved app path
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := frontdoor.Decide(tc.path, reserved)
			if got.Upstream != tc.wantUpstream {
				t.Errorf("Decide(%q).Upstream = %q, want %q", tc.path, got.Upstream, tc.wantUpstream)
			}
			if got.RequireSession != tc.wantRequireSession {
				t.Errorf("Decide(%q).RequireSession = %v, want %v", tc.path, got.RequireSession, tc.wantRequireSession)
			}
			if got.IsRoot != tc.wantIsRoot {
				t.Errorf("Decide(%q).IsRoot = %v, want %v", tc.path, got.IsRoot, tc.wantIsRoot)
			}
		})
	}
}
