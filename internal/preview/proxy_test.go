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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: backendHost, Token: "secret-token"})
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
	if gotPath != "/app/page" {
		t.Errorf("backend path = %q, want /app/page", gotPath)
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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.1:9091", Token: "t"})
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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.1:9091", Token: "t"})
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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.1:9091", Token: "t"})
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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.1:9091", Token: "t"})
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
	rt.Upsert(Route{SandboxID: "sb-1", Backend: strings.TrimPrefix(backend.URL, "http://"), Token: "t"})
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
