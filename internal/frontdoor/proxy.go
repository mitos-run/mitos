package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// ErrNoSession is returned by SessionResolver.Resolve when the token does not
// correspond to a valid active session.
var ErrNoSession = errors.New("frontdoor: no session")

// Identity carries the resolved account and org for an authenticated request.
// Values are never logged.
type Identity struct {
	AccountID string
	OrgID     string
}

// SessionResolver resolves a raw session token to the Identity behind it.
// Implementations must return ErrNoSession when the token is absent, invalid,
// or expired. They must never log the token value.
type SessionResolver interface {
	Resolve(ctx context.Context, sessionToken string) (Identity, error)
}

// sessionCookieName is the browser session cookie name set by the Mitos
// console after OIDC login. Matches console.SessionCookieName.
const sessionCookieName = "mitos_session"

// ProxyConfig is the constructor input for NewProxy.
type ProxyConfig struct {
	// MarketingURL is the base URL of the marketing upstream (e.g. the Next.js
	// site). Required.
	MarketingURL string
	// ConsoleURL is the base URL of the console upstream. Required.
	ConsoleURL string
	// Resolver resolves session tokens to identities. Required.
	Resolver SessionResolver
	// Reserved is the set of first-path-segments that are platform-reserved.
	// Pass DefaultReserved(). Required.
	Reserved map[string]bool
	// Logger is optional; when nil, a discard logger is used so no token
	// values can leak through a nil-pointer dereference on the logger.
	Logger *slog.Logger
}

// Proxy is the Mitos front-door reverse proxy. It routes each request to
// the marketing or console upstream, validates sessions, injects identity
// headers, and redirects unauthenticated users. It implements http.Handler.
type Proxy struct {
	marketing *httputil.ReverseProxy
	console   *httputil.ReverseProxy
	resolver  SessionResolver
	reserved  map[string]bool
	log       *slog.Logger
}

// NewProxy constructs a Proxy from cfg. Returns an error when any required
// field is missing or a URL cannot be parsed.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.MarketingURL == "" {
		return nil, fmt.Errorf("frontdoor: MarketingURL is required")
	}
	if cfg.ConsoleURL == "" {
		return nil, fmt.Errorf("frontdoor: ConsoleURL is required")
	}
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("frontdoor: Resolver is required")
	}
	if cfg.Reserved == nil {
		return nil, fmt.Errorf("frontdoor: Reserved is required")
	}

	mktURL, err := url.Parse(cfg.MarketingURL)
	if err != nil {
		return nil, fmt.Errorf("frontdoor: invalid MarketingURL: %w", err)
	}
	conURL, err := url.Parse(cfg.ConsoleURL)
	if err != nil {
		return nil, fmt.Errorf("frontdoor: invalid ConsoleURL: %w", err)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}

	mkt := buildReverseProxy(mktURL, "marketing", log)
	con := buildReverseProxy(conURL, "console", log)

	return &Proxy{
		marketing: mkt,
		console:   con,
		resolver:  cfg.Resolver,
		reserved:  cfg.Reserved,
		log:       log,
	}, nil
}

// buildReverseProxy creates a single-host reverse proxy for target, with
// SSE-safe FlushInterval=-1 and a terse error handler that logs label+status
// but never logs any request body or identity values.
func buildReverseProxy(target *url.URL, label string, log *slog.Logger) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1 // SSE-safe; also avoids buffering for streaming
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		log.Info("upstream error", "upstream", label, "status", http.StatusBadGateway)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	return rp
}

// ServeHTTP implements http.Handler. For every request it:
//
//  1. Strips all inbound X-Mitos-* headers (forge protection).
//  2. Calls Decide to determine the upstream and session requirement.
//  3. Reads and resolves the session cookie when needed.
//  4. On IsRoot: authed -> console; anon -> marketing.
//  5. On RequireSession: anon -> 302 /login?next=<path>; authed -> inject
//     X-Mitos-Account / X-Mitos-Org and proxy to console.
//  6. Otherwise -> proxy to the decided upstream with no session work.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: strip all inbound X-Mitos-* headers before any other work.
	// This must happen before Decide so that even public-upstream paths
	// cannot carry forged identity headers forward.
	stripMitosHeaders(r.Header)

	dec := Decide(r.URL.Path, p.reserved)

	if dec.IsRoot {
		id, authed := p.resolveSession(r)
		if authed {
			p.injectIdentity(r, id)
			p.console.ServeHTTP(w, r)
		} else {
			p.marketing.ServeHTTP(w, r)
		}
		return
	}

	if dec.RequireSession {
		id, authed := p.resolveSession(r)
		if !authed {
			// 302 to /login?next=<escaped original path>
			next := url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, "/login?next="+next, http.StatusFound)
			return
		}
		p.injectIdentity(r, id)
		p.console.ServeHTTP(w, r)
		return
	}

	// No session work needed: proxy to the decided upstream.
	switch dec.Upstream {
	case "marketing":
		p.marketing.ServeHTTP(w, r)
	default:
		p.console.ServeHTTP(w, r)
	}
}

// resolveSession reads the mitos_session cookie and resolves it. Returns
// (identity, true) on success, (zero, false) on any failure. Never logs
// the cookie value.
func (p *Proxy) resolveSession(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		// No cookie present.
		return Identity{}, false
	}
	id, err := p.resolver.Resolve(r.Context(), c.Value)
	if err != nil {
		// Token invalid or expired; log only that resolution failed.
		p.log.Info("session resolve failed", "status", "no-session")
		return Identity{}, false
	}
	return id, true
}

// injectIdentity sets X-Mitos-Account and X-Mitos-Org on the outbound request.
// The identity values are not logged.
func (p *Proxy) injectIdentity(r *http.Request, id Identity) {
	r.Header.Set("X-Mitos-Account", id.AccountID)
	r.Header.Set("X-Mitos-Org", id.OrgID)
}

// stripMitosHeaders removes all X-Mitos-* headers from h. This is the
// forge-protection step: it runs before any routing decision so that no
// client-supplied identity header can propagate to any upstream, including
// public-facing ones.
func stripMitosHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-mitos-") {
			delete(h, k)
		}
	}
}

// discardWriter is an io.Writer that drops everything. It backs the default
// logger so a misconfigured Proxy never writes token values anywhere.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
