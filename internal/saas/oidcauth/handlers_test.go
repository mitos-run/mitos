package oidcauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

// fakeExchanger stands in for the oauth2/go-oidc transport: it echoes a fixed
// auth URL and returns a fixed raw id_token, so the handler flow is tested
// without a live provider.
type fakeExchanger struct {
	rawIDToken string
	err        error
}

func (f fakeExchanger) AuthCodeURL(state string) string { return "https://idp.example/auth?state=" + state }
func (f fakeExchanger) Exchange(_ context.Context, _ string) (string, error) {
	return f.rawIDToken, f.err
}

// fakeVerifier verifies the fixed raw token to fixed claims.
type fakeVerifier struct {
	claims saas.OIDCClaims
	err    error
}

func (f fakeVerifier) Verify(_ context.Context, _ string) (saas.OIDCClaims, error) {
	return f.claims, f.err
}

func newHandlers(t *testing.T, ex Exchanger, v saas.IDTokenVerifier) *Handlers {
	t.Helper()
	store := saas.NewMemStore()
	accounts := saas.NewAccountService(store, saas.NewKeyService(store))
	sessions := saas.NewSessionStore()
	lm := saas.NewLoginManager(v, accounts, sessions, func() string { return "sess-token" })
	return NewHandlers(Config{Exchanger: ex, Login: lm, CookieName: "mitos_session", RedirectAfterLogin: "/"})
}

// TestLoginRedirectsWithStateCookie asserts /auth/login sets a state cookie and
// 302-redirects to the provider's auth URL carrying that same state.
func TestLoginRedirectsWithStateCookie(t *testing.T) {
	h := newHandlers(t, fakeExchanger{rawIDToken: "raw"}, fakeVerifier{})
	r := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()
	h.Login(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "state=") {
		t.Fatalf("redirect %q missing state", loc)
	}
	if !strings.Contains(strings.Join(w.Header().Values("Set-Cookie"), ";"), stateCookie) {
		t.Fatalf("login did not set the state cookie")
	}
}

// TestCallbackRejectsStateMismatch asserts a forged/mismatched state is refused
// and no session cookie is set.
func TestCallbackRejectsStateMismatch(t *testing.T) {
	h := newHandlers(t, fakeExchanger{rawIDToken: "raw"}, fakeVerifier{claims: saas.OIDCClaims{Email: "d@e.com", EmailVerified: true}})
	r := httptest.NewRequest("GET", "/auth/callback?state=evil&code=c", nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "expected"})
	w := httptest.NewRecorder()
	h.Callback(w, r)
	if w.Code == http.StatusFound {
		t.Fatalf("state mismatch should not redirect to a logged-in session")
	}
	if strings.Contains(strings.Join(w.Header().Values("Set-Cookie"), ";"), "mitos_session=") {
		t.Fatal("a session cookie was set despite a state mismatch")
	}
}

// TestCallbackHappyPathSetsSessionCookie asserts a valid state + a verified
// identity issues a session cookie and redirects into the app.
func TestCallbackHappyPathSetsSessionCookie(t *testing.T) {
	h := newHandlers(t, fakeExchanger{rawIDToken: "raw"}, fakeVerifier{claims: saas.OIDCClaims{Subject: "s", Email: "d@e.com", EmailVerified: true}})
	r := httptest.NewRequest("GET", "/auth/callback?state=abc&code=c", nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "abc"})
	w := httptest.NewRecorder()
	h.Callback(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	cookies := strings.Join(w.Header().Values("Set-Cookie"), " ; ")
	if !strings.Contains(cookies, "mitos_session=sess-token") {
		t.Fatalf("session cookie not set: %q", cookies)
	}
}

// TestLogoutClearsSessionCookie asserts /auth/logout expires the session cookie.
func TestLogoutClearsSessionCookie(t *testing.T) {
	h := newHandlers(t, fakeExchanger{}, fakeVerifier{})
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	w := httptest.NewRecorder()
	h.Logout(w, r)
	cookies := strings.Join(w.Header().Values("Set-Cookie"), " ; ")
	if !strings.Contains(cookies, "mitos_session=") || !strings.Contains(cookies, "Max-Age=0") {
		t.Fatalf("logout did not clear the session cookie: %q", cookies)
	}
}
