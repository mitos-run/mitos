package preview

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// Config wires a Proxy: the base preview Domain, the Signer that verifies
// preview tokens, the RouteTable that maps a label to its backend, and an
// optional Logger. Logger is allowed to be nil (the proxy then discards logs).
type Config struct {
	Domain string
	Signer *Signer
	Routes *RouteTable
	Logger *slog.Logger
}

// Proxy is the per-sandbox preview reverse proxy. For each request it parses
// the <label>.<domain> vhost, rejects reserved labels, verifies the signed
// expiring token, binds the token to the requested sandbox and port, looks up
// the forkd node endpoint, and reverse-proxies to the forkd expose handler.
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

// ServeHTTP implements the expose request pipeline. Failures are deliberately
// terse so a token value never reaches a response body or log line; the reason
// is logged with the label and HTTP status only.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	label, ok := ParseHost(r.Host, p.domain)
	if !ok || IsReservedLabel(label) {
		http.Error(w, "unknown expose host", http.StatusNotFound)
		return
	}

	token := extractToken(r)
	if token == "" {
		http.Error(w, "missing expose token", http.StatusUnauthorized)
		return
	}
	claims, err := p.signer.Verify(token)
	if err != nil {
		// Do not echo err detail beyond the category; never log the token.
		p.log.Info("expose token rejected", "label", label, "status", http.StatusUnauthorized)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}

	route, ok := p.routes.Lookup(label)
	if !ok {
		http.Error(w, "no route for label", http.StatusNotFound)
		return
	}

	// The signed link must name the sandbox and port the label resolves to: a
	// leaked link cannot be replayed against another label.
	if claims.SandboxID != route.SandboxID || claims.Port != route.Port {
		p.log.Info("expose token route mismatch", "label", label, "status", http.StatusForbidden)
		http.Error(w, "token does not authorize this route", http.StatusForbidden)
		return
	}

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
		// Forkd routes by the path; prepend the slice-1 expose prefix.
		req.URL.Path = prefix + req.URL.Path
		req.Host = nodeEndpoint
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		// Strip the preview token from the forwarded query so the backend
		// never sees the bearer credential.
		stripQueryParam(req, "token")
	}
	rp.FlushInterval = -1 // SSE-safe
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		p.log.Info("expose backend error", "label", label, "status", http.StatusBadGateway)
		http.Error(w, "expose backend unavailable", http.StatusBadGateway)
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
