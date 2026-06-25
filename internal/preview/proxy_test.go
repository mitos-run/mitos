package preview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testGrantSigner returns a GrantSigner for tests.
func testGrantSigner(t *testing.T) *GrantSigner {
	t.Helper()
	gs, err := NewGrantSigner([]byte("grant-secret-for-testing-1234567"))
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}
	return gs
}

// testSessionCodec returns a SessionCodec for tests.
func testSessionCodec(t *testing.T) *SessionCodec {
	t.Helper()
	sc, err := NewSessionCodec([]byte("session-secret-for-testing-12345"))
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}
	return sc
}

// newAuthProxy builds a Proxy with all auth components wired, using a stub
// forkd backend and a route table seeded with one private route.
func newAuthProxy(t *testing.T, rt *RouteTable) (*Proxy, *GrantSigner, *SessionCodec) {
	t.Helper()
	s := testSigner(t)
	gs := testGrantSigner(t)
	sc := testSessionCodec(t)
	p := NewProxy(Config{
		Domain:      testDomain,
		Signer:      s,
		Routes:      rt,
		GrantSigner: gs,
		Sessions:    sc,
	})
	return p, gs, sc
}

const testDomain = "example.com"

// newTestProxy builds a Proxy over a route table and a signer keyed for tests.
func newTestProxy(t *testing.T, rt *RouteTable) (*Proxy, *Signer) {
	t.Helper()
	s := testSigner(t)
	p := NewProxy(Config{Domain: testDomain, Signer: s, Routes: rt})
	return p, s
}

// validURL builds a request URL carrying a freshly minted preview token.
func validURL(t *testing.T, s *Signer, sandboxID string, port int, ttl time.Duration) string {
	t.Helper()
	tok, err := s.Mint(sandboxID, port, time.Now().Add(ttl))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return "/?" + url.Values{"token": {tok}}.Encode()
}

func TestProxyForwardsToBackend(t *testing.T) {
	// Fake backend records the path it received and the bearer it was given.
	var gotAuth, gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "hello from backend")
	}))
	defer backend.Close()
	backendHost := strings.TrimPrefix(backend.URL, "http://")

	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: backendHost, Port: 8080, Token: "secret-token", Sharing: "link"})
	p, s := newTestProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, validURL(t, s, "sb-1", 8080, time.Hour), nil)
	req.Host = "sb-1.example.com"
	req.URL.Path = "/app/page"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello from backend" {
		t.Errorf("body = %q", rec.Body.String())
	}
	// The path to forkd includes the expose prefix.
	if gotPath != "/v1/sandboxes/sb-1/expose/8080/app/page" {
		t.Errorf("backend path = %q, want /v1/sandboxes/sb-1/expose/8080/app/page", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("backend auth = %q, want Bearer secret-token", gotAuth)
	}
}

func TestProxyRejectsUnknownHost(t *testing.T) {
	rt := NewRouteTable()
	p, s := newTestProxy(t, rt)
	req := httptest.NewRequest(http.MethodGet, validURL(t, s, "sb-1", 8080, time.Hour), nil)
	req.Host = "not-a-preview-host.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProxyRejectsNoRoute(t *testing.T) {
	rt := NewRouteTable() // empty: no route for sb-1
	p, s := newTestProxy(t, rt)
	req := httptest.NewRequest(http.MethodGet, validURL(t, s, "sb-1", 8080, time.Hour), nil)
	req.Host = "sb-1.example.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for missing route", rec.Code)
	}
}

func TestProxyRejectsMissingToken(t *testing.T) {
	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t", Sharing: "link"})
	p, _ := newTestProxy(t, rt)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "sb-1.example.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for missing token", rec.Code)
	}
}

func TestProxyRejectsExpiredToken(t *testing.T) {
	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t", Sharing: "link"})
	p, s := newTestProxy(t, rt)
	req := httptest.NewRequest(http.MethodGet, validURL(t, s, "sb-1", 8080, -time.Minute), nil)
	req.Host = "sb-1.example.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for expired token", rec.Code)
	}
}

func TestProxyRejectsWrongSandboxToken(t *testing.T) {
	// Token minted for sb-2 but requested against sb-1's host must be rejected.
	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t", Sharing: "link"})
	p, s := newTestProxy(t, rt)
	req := httptest.NewRequest(http.MethodGet, validURL(t, s, "sb-2", 8080, time.Hour), nil)
	req.Host = "sb-1.example.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for cross-sandbox token", rec.Code)
	}
}

func TestProxyRejectsTamperedToken(t *testing.T) {
	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t", Sharing: "link"})
	p, s := newTestProxy(t, rt)
	good, _ := s.Mint("sb-1", 8080, time.Now().Add(time.Hour))
	parts := strings.SplitN(good, ".", 2)
	bad := mutate(parts[0]) + "." + parts[1]
	req := httptest.NewRequest(http.MethodGet, "/?"+url.Values{"token": {bad}}.Encode(), nil)
	req.Host = "sb-1.example.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for tampered token", rec.Code)
	}
}

func TestProxyAcceptsBearerHeaderToken(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	rt := NewRouteTable()
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: strings.TrimPrefix(backend.URL, "http://"), Port: 8080, Token: "t", Sharing: "link"})
	p, s := newTestProxy(t, rt)
	tok, _ := s.Mint("sb-1", 8080, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "sb-1.example.com"
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (token from Authorization header)", rec.Code)
	}
}

// Task 3 tests: proxy resolves label and reverse-proxies to forkd expose handler.

func TestProxyRoutesToForkdExposeBackend(t *testing.T) {
	// A stand-in forkd that asserts the expose path, the bearer, and that the
	// preview token did not leak downstream.
	var gotPath, gotAuth, gotToken string
	forkd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.URL.Query().Get("token")
		_, _ = io.WriteString(w, "ok")
	}))
	defer forkd.Close()

	secret := []byte("0123456789abcdef")
	signer, _ := NewSigner(secret)
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: strings.TrimPrefix(forkd.URL, "http://"), Port: 8000, Token: "per-sandbox-bearer", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})

	tok, _ := signer.Mint("sbx1", 8000, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/app/page?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/sandboxes/sbx1/expose/8000/app/page" {
		t.Fatalf("forkd path=%q", gotPath)
	}
	if gotAuth != "Bearer per-sandbox-bearer" {
		t.Fatalf("forkd auth=%q (must be the per-sandbox bearer)", gotAuth)
	}
	if gotToken != "" {
		t.Fatalf("preview token leaked downstream: %q", gotToken)
	}
}

func TestProxyRejectsReservedLabel(t *testing.T) {
	signer, _ := NewSigner([]byte("0123456789abcdef"))
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: NewRouteTable()})
	req := httptest.NewRequest(http.MethodGet, "https://api.mitos.run/", nil)
	req.Host = "api.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reserved label must 404, got %d", rec.Code)
	}
}

func TestProxyRejectsTokenForWrongSandbox(t *testing.T) {
	signer, _ := NewSigner([]byte("0123456789abcdef"))
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: "127.0.0.1:1", Port: 8000, Token: "t", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})
	tok, _ := signer.Mint("OTHER", 8000, time.Now().Add(time.Minute)) // token for a different sandbox
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token for another sandbox must 403, got %d", rec.Code)
	}
}

func TestProxyCleansDotSegments(t *testing.T) {
	// A traversal attempt in the sub-path must be resolved within the expose
	// prefix and can never escape above it.
	var gotPath string
	forkd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok")
	}))
	defer forkd.Close()

	signer, _ := NewSigner([]byte("0123456789abcdef"))
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: strings.TrimPrefix(forkd.URL, "http://"), Port: 8000, Token: "per-sandbox-bearer", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})

	tok, _ := signer.Mint("sbx1", 8000, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/app/../../../etc/passwd?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/sandboxes/sbx1/expose/8000/etc/passwd" {
		t.Fatalf("forkd path=%q, want /v1/sandboxes/sbx1/expose/8000/etc/passwd (traversal must stay within the expose prefix)", gotPath)
	}
}

func TestProxyRejectsTokenForWrongPort(t *testing.T) {
	signer, _ := NewSigner([]byte("0123456789abcdef"))
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: "127.0.0.1:1", Port: 8000, Token: "t", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})
	tok, _ := signer.Mint("sbx1", 9999, time.Now().Add(time.Minute)) // correct sandbox id, wrong port
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token for the wrong port must 403, got %d", rec.Code)
	}
}

// TestProxyDropsInboundAuthorization is a security property test: the proxy must
// unconditionally delete any inbound Authorization header before forwarding to
// forkd, even when the route Token is empty. A signed query token authenticates
// the request; an attacker-supplied Authorization header must never reach forkd.
func TestProxyDropsInboundAuthorization(t *testing.T) {
	var capturedAuth string
	forkd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer forkd.Close()

	secret := []byte("0123456789abcdef")
	signer, _ := NewSigner(secret)
	routes := NewRouteTable()
	// Empty Token: the proxy sets no Authorization on the upstream request, so any
	// inbound Authorization surviving to forkd would be a raw client credential leak.
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: strings.TrimPrefix(forkd.URL, "http://"), Port: 8000, Token: "", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})

	// A valid signed token in the query so the gate passes.
	tok, _ := signer.Mint("sbx1", 8000, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	// Attacker-supplied credential in the inbound Authorization header.
	req.Header.Set("Authorization", "Bearer attacker-creds")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if capturedAuth != "" {
		t.Fatalf("inbound Authorization was not stripped: forkd received %q", capturedAuth)
	}
}

// ---- Task 8: auth ladder integration tests ----

// newForkdBackend returns a stub forkd that records the path and auth header it
// received, responds 200, and whose URL can be used as the NodeEndpoint.
func newForkdBackend(t *testing.T) (backend *httptest.Server, gotPath *string, gotAuth *string) {
	t.Helper()
	var gp, ga string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gp = r.URL.Path
		ga = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv, &gp, &ga
}

// privateRoute builds a Route for a private sandbox owned by org "acme".
func privateRoute(nodeEndpoint string) Route {
	return Route{
		Label:        "myapp",
		SandboxID:    "sbx-private",
		NodeEndpoint: nodeEndpoint,
		Port:         8000,
		Token:        "backend-token",
		Sharing:      "private",
		OrgID:        "acme",
	}
}

// validSessionCookie returns the __Host- cookie value for id with 1h TTL.
func validSessionCookie(t *testing.T, sc *SessionCodec, id Identity) string {
	t.Helper()
	val, err := sc.Encode(id, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionCodec.Encode: %v", err)
	}
	return val
}

// TestProxyPrivateRouteValidSessionProxies: a private route with a session cookie
// whose OrgIDs contain route.OrgID proxies to the backend (Allow path).
func TestProxyPrivateRouteValidSessionProxies(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, _, sc := newAuthProxy(t, rt)

	id := Identity{Sub: "u1", Email: "alice@acme.com", EmailVerified: true, OrgIDs: []string{"acme"}}
	cookieVal := validSessionCookie(t, sc, id)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "myapp." + testDomain
	req.Header.Set("Cookie", SessionCookieName+"="+cookieVal)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestProxyPrivateRouteNonMatchingOrgForbids: a cookie whose OrgIDs do not
// contain route.OrgID returns 403.
func TestProxyPrivateRouteNonMatchingOrgForbids(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, _, sc := newAuthProxy(t, rt)

	id := Identity{Sub: "u2", Email: "bob@other.com", EmailVerified: true, OrgIDs: []string{"other-org"}}
	cookieVal := validSessionCookie(t, sc, id)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "myapp." + testDomain
	req.Header.Set("Cookie", SessionCookieName+"="+cookieVal)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for non-matching org; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProxyPrivateRouteNoCookieRedirectsToLogin: a private route with no session
// cookie 302s to auth.<domain>/start with the label and path encoded.
func TestProxyPrivateRouteNoCookieRedirectsToLogin(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, _, _ := newAuthProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "myapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 for missing cookie; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "auth."+testDomain+"/start") {
		t.Fatalf("redirect location=%q, want auth.%s/start", loc, testDomain)
	}
	if !strings.Contains(loc, "rd=myapp") {
		t.Fatalf("redirect location=%q should contain rd=myapp", loc)
	}
}

// TestProxyPublicRouteProxiesWithNoCookie: a public route proxies with no cookie.
func TestProxyPublicRouteProxiesWithNoCookie(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(Route{
		Label:        "pubapp",
		SandboxID:    "sbx-pub",
		NodeEndpoint: strings.TrimPrefix(backend.URL, "http://"),
		Port:         8000,
		Token:        "tok",
		Sharing:      "public",
	})
	p, _, _ := newAuthProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "pubapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for public route without cookie; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProxyAuthCallbackRedeemGrant: the /__mitos_auth/cb path with a valid grant
// sets the __Host- cookie and 302s to the clean path.
func TestProxyAuthCallbackRedeemGrant(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, gs, _ := newAuthProxy(t, rt)

	id := Identity{Sub: "u1", Email: "alice@acme.com", EmailVerified: true, OrgIDs: []string{"acme"}}
	grant, err := gs.Mint("myapp", id, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("Mint grant: %v", err)
	}

	q := url.Values{"grant": {grant}, "path": {"/dashboard"}}
	req := httptest.NewRequest(http.MethodGet, "/__mitos_auth/cb?"+q.Encode(), nil)
	req.Host = "myapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("cb status=%d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/dashboard" {
		t.Fatalf("cb redirect=%q, want /dashboard", loc)
	}
	// Check that the __Host- session cookie was set.
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			found = true
		}
	}
	if !found {
		t.Fatal("__Host- session cookie not set after grant redemption")
	}
}

// TestProxyAuthCallbackOpenRedirectDefense: a path of //evil.com must redirect
// to "/" (open-redirect defense), not to an external host.
func TestProxyAuthCallbackOpenRedirectDefense(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, gs, _ := newAuthProxy(t, rt)

	id := Identity{Sub: "u1", Email: "alice@acme.com", EmailVerified: true, OrgIDs: []string{"acme"}}
	grant, err := gs.Mint("myapp", id, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("Mint grant: %v", err)
	}

	// Use //evil.com as the path to attempt an open redirect.
	q := url.Values{"grant": {grant}, "path": {"//evil.com"}}
	req := httptest.NewRequest(http.MethodGet, "/__mitos_auth/cb?"+q.Encode(), nil)
	req.Host = "myapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("cb status=%d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/" {
		t.Fatalf("open redirect defense: got %q, want /", loc)
	}
}

// TestProxyForwardAuthAllow: a forwardAuth route with a 200 auth server proxies.
func TestProxyForwardAuthAllow(t *testing.T) {
	backend, _, _ := newForkdBackend(t)

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Auth-Request-User", "u1")
		w.Header().Set("X-Auth-Request-Email", "alice@acme.com")
		w.WriteHeader(http.StatusOK)
	}))
	defer authServer.Close()

	rt := NewRouteTable()
	rt.Upsert(Route{
		Label:          "faapp",
		SandboxID:      "sbx-fa",
		NodeEndpoint:   strings.TrimPrefix(backend.URL, "http://"),
		Port:           8000,
		Token:          "tok",
		Sharing:        "authenticated",
		ForwardAuthURL: authServer.URL,
	})
	p, _, _ := newAuthProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "faapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("forwardAuth allow: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestProxyForwardAuthDeny: a 401 from the auth server returns 401 to the client.
func TestProxyForwardAuthDeny(t *testing.T) {
	backend, _, _ := newForkdBackend(t)

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer authServer.Close()

	rt := NewRouteTable()
	rt.Upsert(Route{
		Label:          "faapp",
		SandboxID:      "sbx-fa",
		NodeEndpoint:   strings.TrimPrefix(backend.URL, "http://"),
		Port:           8000,
		Token:          "tok",
		Sharing:        "authenticated",
		ForwardAuthURL: authServer.URL,
	})
	p, _, _ := newAuthProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "faapp." + testDomain
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forwardAuth deny: status=%d, want 401", rec.Code)
	}
}

// TestProxyAuthCallbackBackslashOpenRedirect: a backslash in the path is a
// scheme-relative open redirect (browsers normalize \ to / in the authority).
// Each variant must redirect to "/".
func TestProxyAuthCallbackBackslashOpenRedirect(t *testing.T) {
	backend, _, _ := newForkdBackend(t)
	rt := NewRouteTable()
	rt.Upsert(privateRoute(strings.TrimPrefix(backend.URL, "http://")))
	p, gs, _ := newAuthProxy(t, rt)

	id := Identity{Sub: "u1", Email: "alice@acme.com", EmailVerified: true, OrgIDs: []string{"acme"}}

	// rawPath is the percent-encoded value placed directly in the query string,
	// so the handler's r.URL.Query().Get("path") decodes it once, exactly as a
	// browser-supplied query would arrive. This exercises the DECODED value the
	// defense must reject.
	cases := []struct {
		name    string
		rawPath string
	}{
		// Backslash authority escape: the decoded "/\evil.com" normalizes to
		// "//evil.com" in browsers.
		{"backslash", "%2F%5Cevil.com"},
		// URL-encoded backslash that decodes to "/\evil.com".
		{"encoded backslash", "/%5Cevil.com"},
		// CRLF header injection attempt that decodes to "/app\r\nX:1".
		{"crlf injection", "/app%0d%0aX:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant, err := gs.Mint("myapp", id, time.Now().Add(30*time.Second))
			if err != nil {
				t.Fatalf("Mint grant: %v", err)
			}
			rawQuery := "grant=" + url.QueryEscape(grant) + "&path=" + tc.rawPath
			req := httptest.NewRequest(http.MethodGet, "/__mitos_auth/cb?"+rawQuery, nil)
			req.Host = "myapp." + testDomain
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("cb status=%d, want 302; body=%s", rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")
			if loc != "/" {
				t.Fatalf("open redirect defense (%s): got %q, want /", tc.name, loc)
			}
		})
	}
}

// TestProxyNetworkGateBeforeForwardAuth: an out-of-CIDR client on a forwardAuth
// route is 403'd before any outbound subrequest. The forwardAuth server must
// never be called (SSRF amplification defense + spec order network->forwardAuth).
func TestProxyNetworkGateBeforeForwardAuth(t *testing.T) {
	backend, _, _ := newForkdBackend(t)

	var authCalls int
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer authServer.Close()

	rt := NewRouteTable()
	rt.Upsert(Route{
		Label:          "faapp",
		SandboxID:      "sbx-fa",
		NodeEndpoint:   strings.TrimPrefix(backend.URL, "http://"),
		Port:           8000,
		Token:          "tok",
		Sharing:        "authenticated",
		ForwardAuthURL: authServer.URL,
		// Allow only an unrelated network; the test client is 192.0.2.1.
		Network: []string{"10.0.0.0/8"},
	})
	p, _, _ := newAuthProxy(t, rt)

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Host = "faapp." + testDomain
	req.RemoteAddr = "192.0.2.1:5555" // outside 10.0.0.0/8
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-network client: status=%d, want 403", rec.Code)
	}
	if authCalls != 0 {
		t.Fatalf("forwardAuth server was called %d times; out-of-network client must be 403'd before any subrequest", authCalls)
	}
}
