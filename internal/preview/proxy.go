package preview

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Config wires a Proxy: the base preview Domain, the Signer that verifies
// preview tokens, the RouteTable that maps a sandbox id to its backend, and an
// optional Logger. Logger is allowed to be nil (the proxy then discards logs).
type Config struct {
	Domain string
	Signer *Signer
	Routes *RouteTable
	Logger *slog.Logger
}

// Proxy is the per-sandbox preview reverse proxy. For each request it parses
// the <sandbox-id>.preview.<domain> vhost, verifies the signed expiring token,
// binds the token to the requested sandbox, looks up the backend, attaches the
// per-sandbox bearer (the same :9091 gate), and proxies to the backend.
type Proxy struct {
	domain string
	signer *Signer
	routes *RouteTable
	log    *slog.Logger
}

// NewProxy returns a Proxy over cfg. A nil cfg.Logger is replaced with a
// discard logger so the proxy never logs token values by accident.
func NewProxy(cfg Config) *Proxy {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Proxy{domain: cfg.Domain, signer: cfg.Signer, routes: cfg.Routes, log: log}
}

// ServeHTTP implements the preview request pipeline. Failures are deliberately
// terse so a token value never reaches a response body or log line; the reason
// is logged with the sandbox id and HTTP status only.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandboxID, ok := ParseHost(r.Host, p.domain)
	if !ok {
		http.Error(w, "unknown preview host", http.StatusNotFound)
		return
	}

	token := extractToken(r)
	if token == "" {
		http.Error(w, "missing preview token", http.StatusUnauthorized)
		return
	}
	claims, err := p.signer.Verify(token)
	if err != nil {
		// Do not echo err detail beyond the category; never log the token.
		p.log.Info("preview token rejected", "sandbox", sandboxID, "status", http.StatusUnauthorized)
		http.Error(w, "invalid or expired preview token", http.StatusUnauthorized)
		return
	}
	// The token must name the sandbox in the host: a token for another sandbox
	// is forbidden even though it verifies cryptographically.
	if claims.SandboxID != sandboxID {
		p.log.Info("preview token sandbox mismatch", "sandbox", sandboxID, "status", http.StatusForbidden)
		http.Error(w, "preview token does not authorize this sandbox", http.StatusForbidden)
		return
	}

	route, ok := p.routes.Lookup(sandboxID)
	if !ok {
		http.Error(w, "no route for sandbox", http.StatusNotFound)
		return
	}

	target := &url.URL{Scheme: "http", Host: route.Backend}
	rp := httputil.NewSingleHostReverseProxy(target)
	// Capture the per-sandbox token in a local so the closure does not retain
	// the Route; the token value is never logged.
	bearer := route.Token
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		baseDirector(req)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		// Strip the preview token from the forwarded query so the backend
		// never sees the bearer credential.
		stripQueryParam(req, "token")
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		p.log.Info("preview backend error", "sandbox", sandboxID, "status", http.StatusBadGateway)
		http.Error(w, "preview backend unavailable", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
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
