// Package frontdoor implements the routing decision and reverse-proxy core for
// the Mitos front-door. It decides, per request, which upstream to use
// (marketing or console), whether a session is required, and whether the
// request is for the root path that requires special fork logic.
package frontdoor

import (
	stdpath "path"
	"strings"
)

// Decision is the result of routing a single request path.
type Decision struct {
	// Upstream is either "marketing" or "console".
	Upstream string
	// RequireSession is true when the request must be authenticated.
	RequireSession bool
	// IsRoot is true when the path is exactly "/". The proxy forks on session
	// presence: authed -> console, anon -> marketing.
	IsRoot bool
}

// marketingSegments are the first-segment values that map to the marketing
// upstream with no session requirement.
var marketingSegments = map[string]bool{
	"pricing":   true,
	"docs":      true,
	"use-cases": true,
	"compare":   true,
	"blog":      true,
	"about":     true,
	// Astro emits its bundled CSS/JS under /_astro/ (the build.assets default).
	// The console's Vite bundle lives under /assets/, so /assets is owned by the
	// console (see authSegments), NOT marketing. Routing /assets to marketing
	// black-holed the console SPA's JS/CSS and left every app page blank.
	"_astro": true,
}

// authSegments are the first-segment values that map to the console upstream
// but do NOT require a session (login, signup, verification, onboarding, and
// webhook ingress are all public-facing flows). Billing providers POST to
// /webhooks/* without a session; the console handler is signature-gated.
var authSegments = map[string]bool{
	"login":      true,
	"signup":     true,
	"verify":     true,
	"auth":       true,
	"onboarding": true,
	"webhooks":   true,
	// The console SPA (Vite) emits its JS/CSS under /assets/. These are public
	// (the login/signup pages load them before any session), so they belong on
	// the console upstream with no session requirement. Marketing's bundle lives
	// under /_astro/ (see marketingSegments), so there is no collision.
	"assets": true,
}

// appSegments are the first-segment values that map to the console upstream
// and DO require an active session.
var appSegments = map[string]bool{
	"console":  true,
	"app":      true,
	"api":      true,
	"settings": true,
	"new":      true,
}

// Decide returns the routing Decision for path. The package-level
// marketingSegments, authSegments, and appSegments maps are the single source
// of truth for routing.
//
// Precedence:
//  1. Exact "/" -> marketing, IsRoot=true, RequireSession=false.
//  2. First segment in the marketing set -> marketing, RequireSession=false.
//  3. First segment in the auth/public-console set -> console, RequireSession=false.
//  4. First segment in the app set -> console, RequireSession=true.
//  5. Otherwise (non-reserved first segment) -> org slug: console, RequireSession=true.
func Decide(path string) Decision {
	// Normalize the path so Decide is safe even when called outside the
	// net/http server context. stdpath.Clean("/") stays "/".
	if path == "" {
		path = "/"
	}
	path = stdpath.Clean(path)

	if path == "/" {
		return Decision{Upstream: "marketing", IsRoot: true}
	}

	// Extract the first path segment: strip the leading slash, then take
	// everything up to the next slash.
	stripped := strings.TrimPrefix(path, "/")
	seg := stripped
	if idx := strings.IndexByte(stripped, '/'); idx >= 0 {
		seg = stripped[:idx]
	}

	if marketingSegments[seg] {
		return Decision{Upstream: "marketing"}
	}
	if authSegments[seg] {
		return Decision{Upstream: "console"}
	}
	if appSegments[seg] {
		return Decision{Upstream: "console", RequireSession: true}
	}

	// Non-reserved segment: treat as an org slug. Session required.
	return Decision{Upstream: "console", RequireSession: true}
}
