package oidcauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"mitos.run/mitos/internal/saas"
)

// fakeExchanger stands in for the oauth2/go-oidc transport: it builds a real
// URL via a dummy oauth2.Config so that any opts (e.g. connector_id) are
// reflected in the redirect, and returns a fixed raw id_token.
type fakeExchanger struct {
	rawIDToken string
	err        error
}

func (f fakeExchanger) AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string {
	cfg := &oauth2.Config{
		ClientID:    "fake",
		RedirectURL: "https://idp.example/callback",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://idp.example/auth"},
	}
	return cfg.AuthCodeURL(state, opts...)
}

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

// TestLogoutAlsoClearsLegacyCookieName asserts that when the configured cookie
// name is the hardened __Host- prefixed one, logout ALSO expires the legacy
// unprefixed name, so a session cookie set before the __Host- rollout is not
// left behind and silently keeping the user signed in (issue #733, item 1).
func TestLogoutAlsoClearsLegacyCookieName(t *testing.T) {
	store := saas.NewMemStore()
	accounts := saas.NewAccountService(store, saas.NewKeyService(store))
	sessions := saas.NewSessionStore()
	lm := saas.NewLoginManager(fakeVerifier{}, accounts, sessions, func() string { return "sess-token" })
	h := NewHandlers(Config{
		Exchanger:          fakeExchanger{},
		Login:              lm,
		CookieName:         "__Host-mitos_session",
		Secure:             true,
		RedirectAfterLogin: "/",
	})
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	w := httptest.NewRecorder()
	h.Logout(w, r)
	// Inspect parsed cookies by EXACT name: "__Host-mitos_session" contains the
	// substring "mitos_session", so a substring check would false-pass without a
	// distinct legacy clear.
	cleared := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 || (c.MaxAge == 0 && c.Value == "") {
			cleared[c.Name] = true
		}
	}
	if !cleared["__Host-mitos_session"] {
		t.Errorf("logout did not clear the hardened cookie; cleared=%v", cleared)
	}
	if !cleared["mitos_session"] {
		t.Errorf("logout did not clear the legacy cookie; cleared=%v", cleared)
	}
}

// TestLoginConnectorHint asserts that ?connector=github or ?connector=google
// adds connector_id to the provider redirect URL, while an absent or unknown
// connector value omits connector_id so Dex shows its own chooser.
func TestLoginConnectorHint(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantPresent bool
		wantValue   string
	}{
		{"github connector", "/auth/login?connector=github", true, "github"},
		{"google connector", "/auth/login?connector=google", true, "google"},
		{"no connector", "/auth/login", false, ""},
		{"unknown connector rejected", "/auth/login?connector=evil", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandlers(t, fakeExchanger{rawIDToken: "raw"}, fakeVerifier{})
			r := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			h.Login(w, r)
			if w.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", w.Code)
			}
			loc := w.Header().Get("Location")
			parsed, err := url.Parse(loc)
			if err != nil {
				t.Fatalf("parse location %q: %v", loc, err)
			}
			got := parsed.Query().Get("connector_id")
			if tc.wantPresent {
				if got != tc.wantValue {
					t.Fatalf("connector_id = %q, want %q; location: %s", got, tc.wantValue, loc)
				}
			} else {
				if got != "" {
					t.Fatalf("connector_id should be absent, got %q; location: %s", got, loc)
				}
			}
		})
	}
}
