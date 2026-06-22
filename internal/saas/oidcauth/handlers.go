// Package oidcauth wires the browser OIDC login flow for the console: it drives
// the authorization-code redirect, exchanges the code, and turns the verified
// identity into a session cookie via the saas.LoginManager. The actual token
// verification lives behind saas.IDTokenVerifier (the real go-oidc verifier is
// in verifier.go); these handlers are transport glue, tested with a fake
// Exchanger so the flow is verified without a live provider.
package oidcauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"mitos.run/mitos/internal/saas"
)

// stateCookie holds the CSRF state between /auth/login and /auth/callback.
const stateCookie = "mitos_oidc_state"

// Exchanger is the OAuth2 transport seam: build the provider auth URL and
// exchange an authorization code for a raw OIDC ID token. The real
// implementation wraps golang.org/x/oauth2 + go-oidc (see verifier.go).
type Exchanger interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (rawIDToken string, err error)
}

// Config wires the handlers.
type Config struct {
	Exchanger          Exchanger
	Login              *saas.LoginManager
	CookieName         string // the session cookie the console reads (console.SessionCookieName)
	RedirectAfterLogin string // where to send the browser after a successful login
	Secure             bool   // set the Secure flag on issued cookies (true behind TLS)
}

// Handlers serves /auth/login, /auth/callback, and /auth/logout.
type Handlers struct{ cfg Config }

// NewHandlers builds the auth handlers.
func NewHandlers(cfg Config) *Handlers {
	if cfg.RedirectAfterLogin == "" {
		cfg.RedirectAfterLogin = "/"
	}
	return &Handlers{cfg: cfg}
}

// Login starts the authorization-code flow: it mints a CSRF state, stores it in
// a short-lived cookie, and redirects to the provider.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	state := randToken()
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.Redirect(w, r, h.cfg.Exchanger.AuthCodeURL(state), http.StatusFound)
}

// Callback validates the state, exchanges the code, signs the identity in via
// the LoginManager, and sets the session cookie.
func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	stored, err := r.Cookie(stateCookie)
	if err != nil || stored.Value == "" || stored.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	rawIDToken, err := h.cfg.Exchanger.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	_, sessionToken, err := h.cfg.Login.SignIn(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "login rejected", http.StatusForbidden)
		return
	}
	// Clear the state cookie and set the session cookie.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.cfg.RedirectAfterLogin, http.StatusFound)
}

// Logout expires the session cookie.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1, // expire now; net/http renders this as Max-Age=0
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
