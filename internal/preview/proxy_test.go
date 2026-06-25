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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: backendHost, Port: 8080, Token: "secret-token"})
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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t"})
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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t"})
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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t"})
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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: "10.0.0.1:9091", Port: 8080, Token: "t"})
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
	rt.Upsert(Route{Label: "sb-1", SandboxID: "sb-1", NodeEndpoint: strings.TrimPrefix(backend.URL, "http://"), Port: 8080, Token: "t"})
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
