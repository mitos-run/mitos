package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
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

// hostPrefixedSessionCookieName and legacySessionCookieName are the two browser
// session cookie names the Mitos console may set after OIDC login. A secure
// deployment sets the hardened __Host- prefixed name; a plain-http self-host/dev
// deployment sets the legacy one. These mirror console.HostPrefixedSessionCookieName
// and console.SessionCookieName; they are duplicated here to keep the frontdoor
// free of a dependency on the console package.
const (
	hostPrefixedSessionCookieName = "__Host-mitos_session"
	legacySessionCookieName       = "mitos_session"
)

// readSessionCookie returns the session token from r, preferring the hardened
// __Host- cookie and falling back to the legacy name. Empty means neither is set.
func readSessionCookie(r *http.Request) string {
	if c, err := r.Cookie(hostPrefixedSessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if c, err := r.Cookie(legacySessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// ProxyConfig is the constructor input for NewProxy.
type ProxyConfig struct {
	// MarketingURL is the base URL of the marketing upstream (e.g. the Next.js
	// site). Required.
	MarketingURL string
	// MarketingPagesAddrs is the list of GitHub Pages anycast IP:port
	// addresses (e.g. ["185.199.108.153:443", "185.199.109.153:443"]) to
	// dial when proxying the marketing upstream. When non-empty, DNS
	// resolution for the marketing host is bypassed; TLS SNI and the
	// upstream Host header remain the marketing URL host (mitos.run). When
	// empty, the marketing host is resolved normally via DNS.
	MarketingPagesAddrs []string
	// ConsoleURL is the base URL of the console upstream. Required.
	ConsoleURL string
	// Resolver resolves session tokens to identities. Required.
	Resolver SessionResolver
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

	var mkt *httputil.ReverseProxy
	if len(cfg.MarketingPagesAddrs) > 0 {
		mkt = buildPagesMarketingReverseProxy(mktURL, cfg.MarketingPagesAddrs, "marketing", log)
	} else {
		mkt = buildReverseProxy(mktURL, "marketing", log)
	}
	con := buildReverseProxy(conURL, "console", log)

	return &Proxy{
		marketing: mkt,
		console:   con,
		resolver:  cfg.Resolver,
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

// buildPagesMarketingReverseProxy builds a marketing reverse proxy that dials
// pagesAddrs instead of resolving target.Host via DNS. It is used when the
// marketing site is hosted on GitHub Pages and mitos.run DNS points at our own
// gateway (which would loop if we resolved normally).
//
// Security properties:
//   - TLS SNI stays target.Host (mitos.run): Go's http.Transport derives SNI
//     from the URL host, not from the dialed address, so the Pages cert is
//     validated correctly.
//   - InsecureSkipVerify is never set: the GitHub Pages cert is valid for
//     mitos.run.
//   - The upstream Host header is pinned to target.Host by the wrapped
//     Director, regardless of the incoming client Host value.
//   - The dial override is scoped to the marketing upstream only; console
//     dials are unaffected.
//
// pagesAddrs are dialed round-robin using a lock-free atomic counter.
func buildPagesMarketingReverseProxy(target *url.URL, pagesAddrs []string, label string, log *slog.Logger) *httputil.ReverseProxy {
	rp := buildReverseProxy(target, label, log)

	// Wrap the Director to pin the upstream Host header to the marketing URL
	// host. This is necessary because req.Host may carry the original client
	// Host value (which is correct for mitos.run in production but may be
	// absent or wrong in tests and non-standard deployments).
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
	}

	// Construct the needle: the host:port string the http.Transport will pass
	// to DialContext when dialing the marketing upstream.
	targetPort := target.Port()
	if targetPort == "" {
		switch target.Scheme {
		case "https":
			targetPort = "443"
		default:
			targetPort = "80"
		}
	}
	needle := net.JoinHostPort(target.Hostname(), targetPort)

	// Clone http.DefaultTransport to inherit its production-ready defaults
	// (connection pooling, keep-alive, HTTP/2 support). Only DialContext is
	// overridden; TLSClientConfig is left untouched so ServerName defaults to
	// the URL host (mitos.run) for SNI, and InsecureSkipVerify stays false.
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		dt = &http.Transport{}
	}
	base := dt.Clone()

	origDial := base.DialContext
	if origDial == nil {
		d := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		origDial = d.DialContext
	}

	var counter atomic.Uint64
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == needle {
			n := counter.Add(1) % uint64(len(pagesAddrs))
			return origDial(ctx, network, pagesAddrs[n])
		}
		return origDial(ctx, network, addr)
	}
	rp.Transport = base
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

	dec := Decide(r.URL.Path)

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
		// Resolve the session so an authenticated request carries its account and
		// org to the console (X-Mitos-Account / X-Mitos-Org). We do NOT redirect
		// the anonymous case: the console owns auth. Its BFF (/console/*) returns
		// 401 so the SPA detects the signed-out state, and it serves the SPA shell
		// (which routes to login) for page loads. A frontdoor 302 here turned the
		// SPA's /console/capabilities probe into a redirect and hung every console
		// page on "loading".
		if id, authed := p.resolveSession(r); authed {
			p.injectIdentity(r, id)
		}
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

// resolveSession reads the session cookie (the hardened __Host- name preferred,
// legacy name as fallback) and resolves it. Returns (identity, true) on success,
// (zero, false) on any failure. Never logs the cookie value.
func (p *Proxy) resolveSession(r *http.Request) (Identity, bool) {
	token := readSessionCookie(r)
	if token == "" {
		// No cookie present.
		return Identity{}, false
	}
	id, err := p.resolver.Resolve(r.Context(), token)
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
