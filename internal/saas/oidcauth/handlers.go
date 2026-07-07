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
	"strings"

	"golang.org/x/oauth2"

	"mitos.run/mitos/internal/saas"
)

// stateCookie holds the CSRF state between /auth/login and /auth/callback.
const stateCookie = "mitos_oidc_state"

// Exchanger is the OAuth2 transport seam: build the provider auth URL and
// exchange an authorization code for a raw OIDC ID token. The real
// implementation wraps golang.org/x/oauth2 + go-oidc (see verifier.go).
type Exchanger interface {
	AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string
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

// allowedConnectors is the set of connector IDs the SPA is permitted to hint.
// Anything outside this set is silently ignored so Dex shows its own chooser.
var allowedConnectors = map[string]bool{
	"github": true,
	"google": true,
}

// Login starts the authorization-code flow: it mints a CSRF state, stores it in
// a short-lived cookie, and redirects to the provider. An optional
// ?connector=github|google query param is forwarded to Dex as connector_id so
// the provider-specific OAuth screen is shown immediately, skipping Dex's
// built-in chooser.
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
	var opts []oauth2.AuthCodeOption
	if connector := r.URL.Query().Get("connector"); allowedConnectors[connector] {
		opts = append(opts, oauth2.SetAuthURLParam("connector_id", connector))
	}
	http.Redirect(w, r, h.cfg.Exchanger.AuthCodeURL(state, opts...), http.StatusFound)
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

// Logout expires the session cookie. When the configured cookie name is the
// hardened __Host- prefixed one, it ALSO expires the legacy unprefixed name so a
// session cookie set before the __Host- rollout is not left behind (which would
// keep the user signed in, because the reader falls back to the legacy name).
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	for _, name := range logoutCookieNames(h.cfg.CookieName) {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   h.cfg.Secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1, // expire now; net/http renders this as Max-Age=0
		})
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// logoutCookieNames returns the cookie names Logout must expire: the configured
// name, plus its un-prefixed variant when it carries the __Host- prefix so a
// pre-rollout legacy cookie is cleared too.
func logoutCookieNames(configured string) []string {
	names := []string{configured}
	if legacy := strings.TrimPrefix(configured, "__Host-"); legacy != configured {
		names = append(names, legacy)
	}
	return names
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
