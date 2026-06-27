package preview

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config wires a Proxy: the base preview Domain, the Signer that verifies
// preview tokens, the RouteTable that maps a label to its backend, optional
// auth components (AuthOrigin, SessionCodec, GrantSigner), and an optional
// Logger. Logger is allowed to be nil (the proxy then discards logs).
// GrantSigner and Sessions are optional: when absent the proxy operates with
// only public and link tiers (private/org/authenticated return 401 with a
// clear log message).
type Config struct {
	Domain      string
	Signer      *Signer
	Routes      *RouteTable
	Logger      *slog.Logger
	AuthOrigin  *AuthOrigin   // optional; required for OIDC-backed tiers
	Sessions    *SessionCodec // optional; required to mint/decode per-app session cookies
	GrantSigner *GrantSigner  // optional; required to redeem grants from AuthOrigin
}

// Proxy is the per-sandbox preview reverse proxy. For each request it runs the
// full auth ladder: auth-origin host routing, grant redemption, identity/tier
// enforcement, and reverse-proxying to the forkd expose handler.
type Proxy struct {
	domain      string
	signer      *Signer
	routes      *RouteTable
	log         *slog.Logger
	authOrigin  *AuthOrigin
	sessions    *SessionCodec
	grantSigner *GrantSigner
	httpClient  *http.Client // used for forwardAuth subrequests
}

// NewProxy returns a Proxy over cfg. A nil cfg.Logger is replaced with a
// discard logger so the proxy never logs token values by accident.
func NewProxy(cfg Config) *Proxy {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Proxy{
		domain:      cfg.Domain,
		signer:      cfg.Signer,
		routes:      cfg.Routes,
		log:         log,
		authOrigin:  cfg.AuthOrigin,
		sessions:    cfg.Sessions,
		grantSigner: cfg.GrantSigner,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// ServeHTTP implements the full expose authentication and authorization
// pipeline. Failures are deliberately terse so a token, grant, or cookie value
// never reaches a response body or log line.
//
// Pipeline:
//
//  1. If the host is auth.<domain>: route /start and /auth/callback to the
//     AuthOrigin (OIDC relying party); all other paths 404. Only when
//     AuthOrigin is configured; else 404.
//  2. Parse <label>.<domain> (ParseHost). Reject reserved or unknown labels
//     (404).
//  3. If the path is /__mitos_auth/cb: redeem the grant, set the per-app
//     __Host- session cookie, and 302 to the validated clean path.
//  4. Strip any inbound X-Auth-Request-* headers (anti-spoofing). Compute
//     clientIP from RemoteAddr.
//  5. Look up the route. Evaluate identity and apply the tier:
//     - ForwardAuthURL set: perform the forwardAuth subrequest; on non-2xx
//     return that status; on 2xx copy identity headers and use the forwardAuth
//     identity.
//     - private/org/authenticated: read the __Host- session cookie; if
//     absent/expired 302 to auth.<domain>/start (or 401 if no AuthOrigin).
//     - link: verify the signed query token; on success set the app session
//     cookie and 302 to a clean URL (cookie exchange); on cookie hit skip
//     the token.
//     - public: id stays nil.
//  6. Authorize(route, id, clientIP). On Allow: reverse-proxy to forkd.
//     DenyForbidden: 403. DenyUnauthenticated: 302 to login or 401.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: Auth-origin host routing.
	authHost := "auth." + p.domain
	reqHost := strings.ToLower(strings.TrimSpace(r.Host))
	if h := reqHost; strings.HasPrefix(h, authHost+":") || h == authHost {
		if p.authOrigin == nil {
			http.Error(w, "auth origin not configured", http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/start":
			p.authOrigin.ServeStart(w, r)
		case "/auth/callback":
			p.authOrigin.ServeCallback(w, r)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	// Step 2: Parse the label from the vhost; reject reserved/unknown.
	label, ok := ParseHost(r.Host, p.domain)
	if !ok || IsReservedLabel(label) {
		http.Error(w, "unknown expose host", http.StatusNotFound)
		return
	}

	// Step 3: Grant redemption at /__mitos_auth/cb.
	if r.URL.Path == "/__mitos_auth/cb" {
		p.serveAuthCallback(w, r, label)
		return
	}

	// Step 4: Strip inbound X-Auth-Request-* to prevent header spoofing.
	StripForwardAuthHeaders(r.Header)

	// Compute client IP from RemoteAddr; X-Forwarded-For is only trusted when
	// an explicit trusted-proxy layer is in front (not wired here, so we use
	// the direct address only).
	clientIP := parseClientIP(r.RemoteAddr)

	// Step 5: Route lookup and identity resolution.
	route, ok := p.routes.Lookup(label)
	if !ok {
		http.Error(w, "no route for label", http.StatusNotFound)
		return
	}

	// Network gate FIRST (spec order: network -> forwardAuth -> tier). An
	// out-of-network client is rejected before any outbound forwardAuth
	// subrequest or any cookie issuance, so the network layer cannot be used
	// for SSRF amplification and a constraint is never outlived by a session.
	if !NetworkAllows(route, clientIP) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var id *Identity
	var forwardAuthHeaders http.Header

	if route.ForwardAuthURL != "" {
		// ForwardAuth path: subrequest to the external auth server.
		allow, faID, copyHdr, status, err := ForwardAuth(r.Context(), p.httpClient, route.ForwardAuthURL, r)
		if err != nil {
			p.log.Info("forwardauth transport error", "label", label)
			http.Error(w, "auth check failed", http.StatusBadGateway)
			return
		}
		if !allow {
			// Return the auth server's status verbatim (typically 401 or 403).
			http.Error(w, http.StatusText(status), status)
			return
		}
		id = faID
		forwardAuthHeaders = copyHdr
	} else {
		// Tier-based identity resolution.
		switch route.Sharing {
		case "private", "org", "authenticated":
			resolved, err := p.readSession(r)
			if err != nil {
				// No valid session: start the login flow.
				p.redirectToLogin(w, r, label)
				return
			}
			id = resolved

		case "link":
			// Check for a per-app session cookie first (cookie exchange means the
			// signed token only appears once).
			if resolved, err := p.readSession(r); err == nil {
				id = resolved
				break
			}
			// No valid session cookie: require the signed query token.
			token := extractToken(r)
			if token == "" {
				http.Error(w, "missing expose token", http.StatusUnauthorized)
				return
			}
			claims, err := p.signer.Verify(token)
			if err != nil {
				p.log.Info("expose token rejected", "label", label, "status", http.StatusUnauthorized)
				http.Error(w, "invalid or expired token", http.StatusUnauthorized)
				return
			}
			// The signed link must name the sandbox and port the label resolves to.
			if claims.SandboxID != route.SandboxID || claims.Port != route.Port {
				p.log.Info("expose token route mismatch", "label", label, "status", http.StatusForbidden)
				http.Error(w, "token does not authorize this route", http.StatusForbidden)
				return
			}
			// Cookie exchange: set the session cookie and 302 to a clean URL
			// (strip the token from the address bar), then return.
			if p.sessions != nil {
				linkID := Identity{Sub: "link", OrgIDs: []string{route.OrgID}}
				if err := p.mintSessionAndRedirect(w, r, linkID); err != nil {
					p.log.Info("link cookie exchange failed", "label", label)
					http.Error(w, "session error", http.StatusInternalServerError)
					return
				}
				return
			}
			// No session codec configured: the signed-token verification that just
			// passed is the authorization proof. Set a non-nil identity so the
			// link-tier Authorize check (id != nil) passes.
			id = &Identity{Sub: "link"}

		case "public":
			id = nil

		default:
			p.log.Info("unknown sharing tier", "label", label, "tier", route.Sharing)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Step 6: Authorize and proxy.
	decision := Authorize(route, id, clientIP)
	switch decision {
	case Allow:
		// fall through to reverse proxy below
	case DenyForbidden:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case DenyUnauthenticated:
		p.redirectToLogin(w, r, label)
		return
	default:
		// Defense in depth: a future Decision value must never fall through to
		// Allow. Fail closed.
		p.log.Info("unexpected authorization decision", "label", label, "decision", decision.String())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	p.reverseProxy(w, r, route, forwardAuthHeaders)
}

// serveAuthCallback handles the /__mitos_auth/cb path: verifies the HMAC grant,
// sets the per-app __Host- session cookie, and 302s to the validated clean path.
func (p *Proxy) serveAuthCallback(w http.ResponseWriter, r *http.Request, label string) {
	if p.grantSigner == nil || p.sessions == nil {
		http.Error(w, "auth not configured", http.StatusUnauthorized)
		return
	}

	grant := r.URL.Query().Get("grant")
	if grant == "" {
		http.Error(w, "missing grant", http.StatusBadRequest)
		return
	}

	id, err := p.grantSigner.Verify(grant, label, time.Now())
	if err != nil {
		// Fail closed: do not reveal whether the grant was expired or invalid.
		p.log.Info("grant rejected", "label", label)
		http.Error(w, "invalid or expired grant", http.StatusUnauthorized)
		return
	}

	// Validate the redirect path as a safe same-origin relative path. This
	// rejects scheme-relative ("//evil.com"), backslash-authority ("/\evil.com",
	// which browsers normalize to "//evil.com"), and CRLF header-injection
	// attempts. Anything unsafe collapses to "/".
	redirectPath := safeRelPath(r.URL.Query().Get("path"))

	// Set the per-app __Host- session cookie.
	sessionVal, err := p.sessions.Encode(id, time.Now().Add(time.Hour))
	if err != nil {
		p.log.Info("session encode failed", "label", label)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, NewSessionCookie(sessionVal, time.Hour))

	http.Redirect(w, r, redirectPath, http.StatusFound)
}

// readSession reads and decodes the __Host- session cookie from r.
// Returns a pointer to the Identity on success, or an error if absent/expired.
func (p *Proxy) readSession(r *http.Request) (*Identity, error) {
	if p.sessions == nil {
		return nil, errNoSessionCodec
	}
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, err
	}
	id, err := p.sessions.Decode(c.Value, time.Now())
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// errNoSessionCodec is returned by readSession when no codec is configured.
var errNoSessionCodec = errMsg("no session codec configured")

type errMsg string

func (e errMsg) Error() string { return string(e) }

// redirectToLogin sends a 302 to auth.<domain>/start with the validated label
// and the original request path. This is issued whenever an authenticated tier
// needs identity but no valid session is present. If no sessions codec is
// configured, 401 is returned instead (the proxy cannot accept or mint cookies).
func (p *Proxy) redirectToLogin(w http.ResponseWriter, r *http.Request, label string) {
	if p.sessions == nil {
		p.log.Info("sessions not configured; cannot redirect to login", "label", label)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	reqPath := r.URL.Path
	if r.URL.RawQuery != "" {
		reqPath += "?" + r.URL.RawQuery
	}
	// rd is the validated label (already parsed; not raw user input beyond the
	// label already confirmed by ParseHost).
	dest := "https://auth." + p.domain + "/start?" + url.Values{
		"rd":   {label},
		"path": {reqPath},
	}.Encode()
	http.Redirect(w, r, dest, http.StatusFound)
}

// mintSessionAndRedirect sets a per-app session cookie for id and 302s to a
// clean URL (the current path without the token query parameter). This is the
// link-tier cookie exchange: the signed token appears only once.
func (p *Proxy) mintSessionAndRedirect(w http.ResponseWriter, r *http.Request, id Identity) error {
	sessionVal, err := p.sessions.Encode(id, time.Now().Add(time.Hour))
	if err != nil {
		return err
	}
	http.SetCookie(w, NewSessionCookie(sessionVal, time.Hour))

	// Build the clean URL: remove the token query parameter.
	q := r.URL.Query()
	q.Del("token")
	cleanPath := r.URL.Path
	if encoded := q.Encode(); encoded != "" {
		cleanPath += "?" + encoded
	}
	if cleanPath == "" {
		cleanPath = "/"
	}
	http.Redirect(w, r, cleanPath, http.StatusFound)
	return nil
}

// reverseProxy proxies the request to the forkd expose backend for route,
// injecting the per-sandbox bearer and the forwardAuth identity headers.
func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request, route Route, forwardAuthHeaders http.Header) {
	target := &url.URL{Scheme: "http", Host: route.NodeEndpoint}
	rp := httputil.NewSingleHostReverseProxy(target)
	// Capture route fields in locals so the closure does not retain the Route;
	// token values are never logged.
	bearer := route.Token
	prefix := "/v1/sandboxes/" + route.SandboxID + "/expose/" + strconv.Itoa(route.Port)
	nodeEndpoint := route.NodeEndpoint
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		baseDirector(req)
		// Forkd routes by the path; prepend the slice-1 expose prefix. Clean the
		// sub-path first so dot-segments cannot escape above the expose prefix, and
		// zero RawPath so the client re-encodes from the sanitized Path.
		sub := path.Clean("/" + req.URL.Path)
		req.URL.Path = prefix + sub
		req.URL.RawPath = ""
		req.Host = nodeEndpoint
		// Tell forkd (and the exposed guest app) the real public front-door host and
		// scheme. Without this the backend only ever sees the internal node endpoint
		// (and forkd's "guest" placeholder), so a reverse-proxy-aware app cannot
		// reconstruct its origin for local-vs-remote / origin / CSRF checks (#476).
		// The edge terminates TLS, so the public scheme is https unless an upstream
		// already set it.
		if req.Header.Get("X-Forwarded-Host") == "" {
			req.Header.Set("X-Forwarded-Host", r.Host)
		}
		if req.Header.Get("X-Forwarded-Proto") == "" {
			if r.TLS != nil {
				req.Header.Set("X-Forwarded-Proto", "https")
			} else {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		// Defense in depth: drop any client-supplied Authorization before
		// conditionally setting the per-sandbox bearer, so an empty-Token route
		// never forwards a client credential to forkd.
		req.Header.Del("Authorization")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		// Strip the preview token from the forwarded query so the backend
		// never sees the bearer credential.
		stripQueryParam(req, "token")
		// Copy forwardAuth identity headers to the upstream request.
		for k, vs := range forwardAuthHeaders {
			req.Header[k] = vs
		}
	}
	rp.FlushInterval = -1 // SSE-safe
	label := route.Label
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		p.log.Info("expose backend error", "label", label, "status", http.StatusBadGateway)
		http.Error(w, "expose backend unavailable", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// parseClientIP extracts the IP address from an addr of the form "host:port"
// (net.Conn.RemoteAddr format). Returns nil on parse failure.
func parseClientIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr may be a bare IP (no port) in some test scenarios.
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
}

// safeRelPath returns p if it is a safe same-origin relative path, else "/".
// It rejects:
//   - empty or non-slash-leading paths (not a relative path),
//   - "//..." scheme-relative URLs (resolve to an attacker authority),
//   - any backslash (browsers normalize "\" to "/" in the authority, so
//     "/\evil.com" becomes "//evil.com"),
//   - CR or LF (header-injection / response-splitting).
func safeRelPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	return p
}

// extractToken returns the preview token from the "token" query parameter or a
// "Bearer" Authorization header, in that order. The query form is the signed
// URL; the header form is for callers that prefer not to leak the token in a
// referer.
func extractToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// stripQueryParam removes a single query parameter from the forwarded request.
func stripQueryParam(req *http.Request, key string) {
	q := req.URL.Query()
	if q.Has(key) {
		q.Del(key)
		req.URL.RawQuery = q.Encode()
	}
}

// discard is an io.Writer that drops everything; it backs the default logger so
// a misconfigured Proxy never writes token values anywhere.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
