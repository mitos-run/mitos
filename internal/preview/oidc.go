package preview

// oidc.go implements the OIDC relying-party flow for the central auth origin
// (auth.<expose-domain>). It handles /start and /auth/callback, issues HMAC
// grants, sets the SSO cookie, and defends against open redirects (rd validated
// against the live route table) and CSRF (signed state cookie, constant-time
// compare).
//
// PKCE note: the existing Exchanger.AuthCodeURL(state) does not accept a code
// challenge parameter. PKCE is a desirable follow-up if the Exchanger gains
// challenge support. For now, the signed CSRF state cookie (HMAC, binding rd
// and path) provides the primary protection.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/oidcauth"
)

// ssoCookieName is the __Host- prefixed SSO cookie name scoped to the auth
// origin. The __Host- prefix enforces Secure, Path=/, and no Domain attribute,
// so a tenant app on a different subdomain cannot read it.
const ssoCookieName = "__Host-mitos_sso"

// ssoStateCookieName is the __Host- prefixed CSRF state cookie, short-lived.
const ssoStateCookieName = "__Host-mitos_oidc_state"

// stateTTL is the maximum lifetime of the CSRF state cookie.
const stateTTL = 10 * time.Minute

// grantTTL is the lifetime of a single-use grant token.
const grantTTL = 30 * time.Second

// ssoTTL is the lifetime of the SSO session cookie.
const ssoTTL = 12 * time.Hour

// stateCookiePayload encodes the rd label and original path into the signed
// CSRF state so ServeCallback can recover them safely without a server-side
// state store.
type stateCookiePayload struct {
	RD   string `json:"rd"`
	Path string `json:"path"`
}

// Exchanger is the OAuth2 transport seam: build the provider auth URL and
// exchange an authorization code for a raw OIDC ID token. It matches
// oidcauth.Exchanger exactly so the real implementation is a drop-in.
type Exchanger interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (rawIDToken string, err error)
}

// Compile-time assertion: the oidcauth.Exchanger interface (from the real
// package) must be satisfied by our local Exchanger definition. This ensures
// the seam shapes stay in sync without importing the type directly.
var _ Exchanger = (oidcauth.Exchanger)(nil)

// AuthOrigin serves the OIDC relying-party endpoints for auth.<expose-domain>.
// It holds all injectable seams so the handler logic is unit-testable with fake
// implementations and no live IdP.
type AuthOrigin struct {
	// Verifier is the OIDC ID token verifier seam. The real implementation wraps
	// internal/saas/oidcauth; tests use a fake.
	Verifier saas.IDTokenVerifier

	// Exchanger is the OAuth2 code exchange seam.
	Exchanger Exchanger

	// Grants mints and verifies short-lived HMAC grant tokens.
	Grants *GrantSigner

	// SSO is the session codec for the auth-origin SSO cookie.
	SSO *SessionCodec

	// StateCodec is the session codec for CSRF state cookies. A separate codec
	// (with its own secret) prevents any cross-use with the SSO cookie.
	StateCodec *SessionCodec

	// Resolver optionally resolves a verified email to org IDs via the SaaS
	// identity endpoint. If nil or if the resolver returns ErrResolveDisabled,
	// GroupsToOrgs is used instead.
	Resolver *Resolver

	// Routes is the live route table. rd is validated against it before any
	// redirect, which is the open-redirect defense.
	Routes *RouteTable

	// ExposeDomain is the base domain (e.g. "example.com"). Tenant apps are
	// served at <label>.<ExposeDomain>.
	ExposeDomain string

	// GroupsToOrgs derives org IDs from OIDC claims when the Resolver is
	// disabled or absent. Used for self-host without the SaaS account service.
	GroupsToOrgs func(saas.OIDCClaims) []string
}

// ServeStart handles GET auth.<domain>/start?rd=<label>&path=<escaped path>.
//
// Open-redirect defense: rd must resolve to a real route before any redirect to
// <rd>.<domain> is issued.
//
// If a valid SSO cookie is present the provider round-trip is skipped: the
// cached identity is used to resolve orgs, a grant is minted, and the browser
// is sent straight to the app callback.
//
// Otherwise a signed CSRF state cookie (binding rd+path) is set and the browser
// is redirected to the provider.
func (a *AuthOrigin) ServeStart(w http.ResponseWriter, r *http.Request) {
	rd := r.URL.Query().Get("rd")
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}

	// Open-redirect defense: rd must resolve to a real route. Use the canonical
	// label from the route (not the raw rd input) for grant binding, which is
	// safe against a future label-normalization change.
	route, ok := a.Routes.Lookup(rd)
	if !ok {
		http.Error(w, "unknown destination", http.StatusBadRequest)
		return
	}

	// If the SSO cookie is valid, skip the provider.
	if c, err := r.Cookie(ssoCookieName); err == nil && c.Value != "" {
		id, decErr := a.SSO.Decode(c.Value, time.Now())
		if decErr == nil {
			a.mintGrantAndRedirect(w, r, id, route.Label, path)
			return
		}
	}

	// No valid SSO cookie: start the provider flow.
	// Pack rd+path into the CSRF state so the callback can recover them.
	statePl := stateCookiePayload{RD: rd, Path: path}
	stateRaw, err := json.Marshal(statePl)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Encode the state payload using the StateCodec; pack the JSON as the Sub
	// field of an Identity. This reuses the existing HMAC + expiry machinery
	// without a new signing type.
	stateVal, err := a.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(stateTTL),
	)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Set the CSRF state cookie (__Host-, short TTL).
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    stateVal,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
		MaxAge:   int(stateTTL.Seconds()),
		// Domain intentionally unset: __Host- prefix requires host-only binding.
	})

	// Redirect to the provider. stateVal is the state we send to the provider;
	// ServeCallback will compare the echoed state param against the cookie.
	http.Redirect(w, r, a.Exchanger.AuthCodeURL(stateVal), http.StatusFound)
}

// ServeCallback handles GET auth.<domain>/auth/callback?code=...&state=...
//
// It validates the state param against the CSRF state cookie (constant-time).
// On mismatch it rejects with 400. On match it exchanges the code, verifies the
// ID token, resolves orgs, sets the SSO cookie, mints a grant, and 302s to the
// app callback.
//
// Fail closed: any error in exchange, verify, or grant mint returns a non-2xx
// response with no redirect.
func (a *AuthOrigin) ServeCallback(w http.ResponseWriter, r *http.Request) {
	stateParam := r.URL.Query().Get("state")

	// Retrieve the CSRF state cookie.
	stateCookie, err := r.Cookie(ssoStateCookieName)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}

	// Constant-time compare: state query param must match the cookie value.
	if subtle.ConstantTimeCompare([]byte(stateParam), []byte(stateCookie.Value)) != 1 {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// Decode the state to recover rd and path. The state is the encoded
	// Identity with Sub carrying the JSON stateCookiePayload.
	stateID, err := a.StateCodec.Decode(stateCookie.Value, time.Now())
	if err != nil {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	var statePl stateCookiePayload
	if err := json.Unmarshal([]byte(stateID.Sub), &statePl); err != nil {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	rd := statePl.RD
	path := statePl.Path
	if path == "" {
		path = "/"
	}

	// Open-redirect re-validation: rd must still resolve to a real route. Use the
	// canonical label from the route for grant binding (safe against a future
	// label-normalization change).
	route, ok := a.Routes.Lookup(rd)
	if !ok {
		http.Error(w, "unknown destination", http.StatusBadRequest)
		return
	}

	// Exchange the authorization code for a raw ID token.
	rawIDToken, err := a.Exchanger.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}

	// Verify the ID token (never log rawIDToken or the resulting claims email).
	claims, err := a.Verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "token verification failed", http.StatusBadGateway)
		return
	}

	// Resolve org IDs.
	orgIDs, err := a.resolveOrgs(r.Context(), claims)
	if err != nil {
		http.Error(w, "identity resolution failed", http.StatusBadGateway)
		return
	}

	id := Identity{
		Sub:           claims.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		OrgIDs:        orgIDs,
	}

	// Set the SSO cookie on the auth origin.
	ssoVal, err := a.SSO.Encode(id, time.Now().Add(ssoTTL))
	if err != nil {
		http.Error(w, "session encoding failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     ssoCookieName,
		Value:    ssoVal,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
		MaxAge:   int(ssoTTL.Seconds()),
		// Domain intentionally unset: __Host- prefix requires host-only binding.
	})

	// Clear the CSRF state cookie. A __Host- cookie deletion must carry Secure
	// (and the matching attributes) to be honored by conformant browsers, else
	// the used state cookie lingers until its TTL.
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	a.mintGrantAndRedirect(w, r, id, route.Label, path)
}

// mintGrantAndRedirect mints a short-lived single-use grant bound to the
// canonical route label and 302s the browser to
// https://<label>.<domain>/__mitos_auth/cb?grant=...&path=...
// Grant value is a bearer credential and must not be logged.
func (a *AuthOrigin) mintGrantAndRedirect(w http.ResponseWriter, r *http.Request, id Identity, label, path string) {
	grant, err := a.Grants.Mint(label, id, time.Now().Add(grantTTL))
	if err != nil {
		http.Error(w, "grant issuance failed", http.StatusInternalServerError)
		return
	}
	dest := fmt.Sprintf("https://%s.%s/__mitos_auth/cb", label, a.ExposeDomain)
	q := url.Values{
		"grant": {grant},
		"path":  {path},
	}
	http.Redirect(w, r, dest+"?"+q.Encode(), http.StatusFound)
}

// resolveOrgs resolves org IDs for the given OIDC claims. It tries the Resolver
// first; on ErrResolveDisabled (or nil Resolver) it falls back to GroupsToOrgs.
// If GroupsToOrgs is also nil the identity carries empty orgs, which is safe
// (the enforcement pipeline will reject org/private tier checks without orgs).
//
// Verified-email invariant: if the IdP did not verify the email (EmailVerified
// is false) the caller receives no org memberships. The identity is still
// "authenticated" (the token verified), but the org/private tiers will 403
// because OrgIDs is empty. The audience domain selector is also unaffected
// because Authorize already gate-checks EmailVerified independently.
func (a *AuthOrigin) resolveOrgs(ctx context.Context, claims saas.OIDCClaims) ([]string, error) {
	if !claims.EmailVerified {
		// Unverified email: return empty orgs without an error. The caller is
		// authenticated but belongs to no org; org/private routes 403 correctly.
		return nil, nil
	}
	if a.Resolver != nil {
		_, orgIDs, err := a.Resolver.Resolve(ctx, claims.Email)
		if err == nil {
			return orgIDs, nil
		}
		if !errors.Is(err, ErrResolveDisabled) {
			return nil, fmt.Errorf("resolve orgs: %w", err)
		}
		// ErrResolveDisabled: fall through to GroupsToOrgs.
	}
	if a.GroupsToOrgs != nil {
		return a.GroupsToOrgs(claims), nil
	}
	return nil, nil
}
