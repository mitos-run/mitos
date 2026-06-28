package frontdoor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/frontdoor"
)

// fakeResolver resolves token "good" to a fixed Identity; all others return
// ErrNoSession. It never stores the token value to avoid any accidental leak.
type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, token string) (frontdoor.Identity, error) {
	if token == "good" {
		return frontdoor.Identity{AccountID: "acct-1", OrgID: "org-1"}, nil
	}
	return frontdoor.Identity{}, frontdoor.ErrNoSession
}

// mktServer starts a marketing upstream that always echoes "MKT".
func mktServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "MKT")
	}))
}

// consoleServer starts a console upstream that echoes the values of
// X-Mitos-Account and X-Mitos-Org it received, separated by "|".
func consoleServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct := r.Header.Get("X-Mitos-Account")
		org := r.Header.Get("X-Mitos-Org")
		_, _ = io.WriteString(w, acct+"|"+org)
	}))
}

// newProxy builds a Proxy for the given upstream servers.
func newProxy(t *testing.T, mkt, con *httptest.Server) *frontdoor.Proxy {
	t.Helper()
	p, err := frontdoor.NewProxy(frontdoor.ProxyConfig{
		MarketingURL: mkt.URL,
		ConsoleURL:   con.URL,
		Resolver:     fakeResolver{},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	return p
}

func TestProxy_Marketing(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	con := consoleServer(t)
	defer con.Close()

	p := newProxy(t, mkt, con)

	req := httptest.NewRequest(http.MethodGet, "/pricing", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "MKT" {
		t.Errorf("body = %q, want %q", body, "MKT")
	}
}

func TestProxy_ConsoleWithSession(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	con := consoleServer(t)
	defer con.Close()

	p := newProxy(t, mkt, con)

	req := httptest.NewRequest(http.MethodGet, "/console/keys", nil)
	// Forge an inbound identity header that must be stripped.
	req.Header.Set("X-Mitos-Account", "evil-forged")
	req.Header.Set("X-Mitos-Org", "evil-org")
	req.AddCookie(&http.Cookie{Name: "mitos_session", Value: "good"})

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	parts := strings.SplitN(body, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected body format: %q", body)
	}
	acct, org := parts[0], parts[1]
	// The resolved identity must be injected.
	if acct != "acct-1" {
		t.Errorf("X-Mitos-Account seen by console = %q, want %q", acct, "acct-1")
	}
	if org != "org-1" {
		t.Errorf("X-Mitos-Org seen by console = %q, want %q", org, "org-1")
	}
	// The forged header must have been replaced (evil-forged is not present).
	if strings.Contains(body, "evil") {
		t.Errorf("forged identity header leaked to upstream: body = %q", body)
	}
}

func TestProxy_ConsoleNoSession_Redirects(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	con := consoleServer(t)
	defer con.Close()

	p := newProxy(t, mkt, con)

	req := httptest.NewRequest(http.MethodGet, "/console/keys", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	want := "/login?next=%2Fconsole%2Fkeys"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestProxy_RootWithSession_GoesToConsole(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	con := consoleServer(t)
	defer con.Close()

	p := newProxy(t, mkt, con)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "mitos_session", Value: "good"})
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// Console echoes "account|org"; verify the injected identity reached it.
	const want = "acct-1|org-1"
	if body := rr.Body.String(); body != want {
		t.Errorf("authed root body = %q, want %q (console with injected identity)", body, want)
	}
}

func TestProxy_RootNoSession_GoesToMarketing(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	con := consoleServer(t)
	defer con.Close()

	p := newProxy(t, mkt, con)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "MKT" {
		t.Errorf("anon root body = %q, want %q", body, "MKT")
	}
}

func TestProxy_ForgeProtection_HeadersStripped(t *testing.T) {
	mkt := mktServer(t)
	defer mkt.Close()
	// con is not used here; we build a custom console server below.

	// Even on a no-session console path, inbound X-Mitos-* headers must be
	// stripped (we send a forged one and confirm it doesn't reach the upstream).
	// Use /login which goes to console with no session.
	loginCon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo any X-Mitos-Account that arrived.
		_, _ = io.WriteString(w, r.Header.Get("X-Mitos-Account"))
	}))
	defer loginCon.Close()

	pp, err := frontdoor.NewProxy(frontdoor.ProxyConfig{
		MarketingURL: mkt.URL,
		ConsoleURL:   loginCon.URL,
		Resolver:     fakeResolver{},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("X-Mitos-Account", "evil")
	rr := httptest.NewRecorder()
	pp.ServeHTTP(rr, req)

	if body := rr.Body.String(); body != "" {
		t.Errorf("forged X-Mitos-Account leaked to upstream: %q", body)
	}
}

// TestProxy_MarketingPagesDialOverride verifies that when MarketingPagesAddrs
// is non-empty, the marketing reverse proxy dials one of the listed addresses
// instead of resolving the marketing host via DNS, and that the upstream Host
// header is pinned to the marketing URL host (mitos.run in production).
//
// The test uses http:// so TLS is not in the path; the DialContext override is
// the only mechanism that lets the request reach the local stub rather than the
// real network host. InsecureSkipVerify is never set.
func TestProxy_MarketingPagesDialOverride(t *testing.T) {
	var capturedHost string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHost = r.Host
		_, _ = io.WriteString(w, "PAGES")
	}))
	defer stub.Close()

	con := consoleServer(t)
	defer con.Close()

	// Use a host whose DNS we do not control. The DialContext override in
	// buildPagesMarketingReverseProxy must redirect dials to this host to the
	// stub address instead.
	const fakeHost = "mitos.run"
	p, err := frontdoor.NewProxy(frontdoor.ProxyConfig{
		MarketingURL:        "http://" + fakeHost,
		MarketingPagesAddrs: []string{stub.Listener.Addr().String()},
		ConsoleURL:          con.URL,
		Resolver:            fakeResolver{},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	// Anonymous request to a marketing path; no session cookie.
	req := httptest.NewRequest(http.MethodGet, "/pricing", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dial override did not reach stub)", rr.Code)
	}
	if body := rr.Body.String(); body != "PAGES" {
		t.Errorf("body = %q, want %q (dial override did not reach stub)", body, "PAGES")
	}
	if capturedHost != fakeHost {
		t.Errorf("upstream Host header = %q, want %q (Host not pinned by Director)", capturedHost, fakeHost)
	}
}
