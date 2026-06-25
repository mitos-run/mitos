package preview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// fakeOIDCVerifier is a test saas.IDTokenVerifier: it returns fixed claims for any token.
type fakeOIDCVerifier struct {
	claims saas.OIDCClaims
	err    error
}

func (f fakeOIDCVerifier) Verify(_ context.Context, _ string) (saas.OIDCClaims, error) {
	return f.claims, f.err
}

// fakeOIDCExchanger is a test Exchanger: AuthCodeURL returns a sentinel URL;
// Exchange returns a fixed raw id_token string.
type fakeOIDCExchanger struct {
	authURL    string
	rawIDToken string
	err        error
}

func (f fakeOIDCExchanger) AuthCodeURL(state string) string {
	return f.authURL + "?state=" + state
}

func (f fakeOIDCExchanger) Exchange(_ context.Context, _ string) (string, error) {
	return f.rawIDToken, f.err
}

// newTestAuthOrigin builds an AuthOrigin wired up with a one-route table ("openclaw"),
// fresh crypto secrets, and the supplied verifier/exchanger.
func newTestAuthOrigin(
	t *testing.T,
	v saas.IDTokenVerifier,
	ex Exchanger,
	resolver *Resolver,
	groupsToOrgs func(saas.OIDCClaims) []string,
) *AuthOrigin {
	t.Helper()

	grantSecret := []byte("test-grant-secret-32bytes-xyzabc1234")
	gs, err := NewGrantSigner(grantSecret)
	if err != nil {
		t.Fatalf("NewGrantSigner: %v", err)
	}

	ssoSecret := []byte("test-sso-secret-32bytes-abcdef1234!")
	sso, err := NewSessionCodec(ssoSecret)
	if err != nil {
		t.Fatalf("NewSessionCodec: %v", err)
	}

	stateSecret := []byte("test-state-secret-32bytes-zyxwvu!!")
	stateSC, err := NewSessionCodec(stateSecret)
	if err != nil {
		t.Fatalf("NewSessionCodec(state): %v", err)
	}

	rt := NewRouteTable()
	rt.Upsert(Route{
		Label:        "openclaw",
		SandboxID:    "sb-1",
		NodeEndpoint: "10.0.0.1:9091",
		Port:         8080,
		Sharing:      "org",
	})

	return &AuthOrigin{
		Verifier:     v,
		Exchanger:    ex,
		Grants:       gs,
		SSO:          sso,
		StateCodec:   stateSC,
		Resolver:     resolver,
		Routes:       rt,
		ExposeDomain: "example.com",
		GroupsToOrgs: groupsToOrgs,
	}
}

// TestServeStart_NoSSO_RedirectsToProvider verifies that a request to /start with a
// valid rd and no SSO cookie results in a 302 to the provider and sets a state cookie.
func TestServeStart_NoSSO_RedirectsToProvider(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "tok"}
	v := fakeOIDCVerifier{claims: saas.OIDCClaims{Subject: "sub1", Email: "u@example.com", EmailVerified: true}}
	ao := newTestAuthOrigin(t, v, ex, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/start?rd=openclaw&path=%2Fdashboard", nil)
	w := httptest.NewRecorder()
	ao.ServeStart(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example/auth") {
		t.Errorf("Location: got %q, want prefix https://idp.example/auth", loc)
	}

	// A state cookie must be set.
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == ssoStateCookieName {
			found = true
			if !c.Secure {
				t.Error("state cookie must be Secure")
			}
			if !c.HttpOnly {
				t.Error("state cookie must be HttpOnly")
			}
			if c.MaxAge <= 0 {
				t.Error("state cookie must have a positive MaxAge")
			}
		}
	}
	if !found {
		t.Errorf("state cookie %q not found in response", ssoStateCookieName)
	}
}

// TestServeStart_UnknownRd_Rejects verifies the open-redirect defense: an rd that
// does not resolve to a real route must be rejected with 400 and no redirect.
func TestServeStart_UnknownRd_Rejects(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "tok"}
	v := fakeOIDCVerifier{claims: saas.OIDCClaims{Subject: "sub1", Email: "u@example.com", EmailVerified: true}}
	ao := newTestAuthOrigin(t, v, ex, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/start?rd=does-not-resolve&path=%2F", nil)
	w := httptest.NewRecorder()
	ao.ServeStart(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (open-redirect defense)", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "" {
		t.Error("no Location header should be set on 400")
	}
}

// TestServeStart_ValidSSO_SkipsProvider verifies that a request with a valid SSO
// cookie skips the provider and immediately issues a grant redirect to the app.
func TestServeStart_ValidSSO_SkipsProvider(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "tok"}
	v := fakeOIDCVerifier{}
	ao := newTestAuthOrigin(t, v, ex, nil, func(_ saas.OIDCClaims) []string { return []string{"org-fallback"} })

	// Mint a valid SSO cookie.
	id := Identity{Sub: "sub1", Email: "u@example.com", EmailVerified: true, OrgIDs: []string{"org-1"}}
	cookieVal, err := ao.SSO.Encode(id, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Encode SSO: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/start?rd=openclaw&path=%2Fdashboard", nil)
	req.AddCookie(&http.Cookie{Name: ssoCookieName, Value: cookieVal})
	w := httptest.NewRecorder()
	ao.ServeStart(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://openclaw.example.com/__mitos_auth/cb?grant=") {
		t.Errorf("Location: got %q, want prefix to app cb with grant", loc)
	}

	// Extract grant and verify it carries the correct identity.
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	grant := u.Query().Get("grant")
	if grant == "" {
		t.Fatal("grant param missing from Location")
	}
	gotID, err := ao.Grants.Verify(grant, "openclaw", time.Now())
	if err != nil {
		t.Fatalf("Grants.Verify: %v", err)
	}
	if gotID.Sub != id.Sub || gotID.Email != id.Email {
		t.Errorf("grant identity mismatch: got %+v, want sub=%q email=%q", gotID, id.Sub, id.Email)
	}
}

// TestServeCallback_ValidState_SetsSSO verifies the full callback path: valid state
// cookie, exchange, verify, SSO cookie set, grant issued, 302 to app.
func TestServeCallback_ValidState_SetsSSO(t *testing.T) {
	claims := saas.OIDCClaims{Subject: "sub1", Email: "u@example.com", EmailVerified: true}
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{claims: claims}
	ao := newTestAuthOrigin(t, v, ex, nil, func(_ saas.OIDCClaims) []string { return []string{"org-x"} })

	// Mint a state value encoding rd=openclaw, path=/dashboard. The state value is
	// both the query param sent to the provider and stored in the cookie so that
	// ServeCallback can validate them via constant-time compare.
	statePl := stateCookiePayload{RD: "openclaw", Path: "/dashboard"}
	stateRaw, err := json.Marshal(statePl)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	// Reuse SessionCodec with the state payload packed as the Sub field.
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	// The state query param must match the cookie value (constant-time compared in ServeCallback).
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302; body: %s", resp.StatusCode, w.Body.String())
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://openclaw.example.com/__mitos_auth/cb?grant=") {
		t.Errorf("Location: got %q, want app cb with grant", loc)
	}

	// SSO cookie must be set.
	foundSSO := false
	for _, c := range resp.Cookies() {
		if c.Name == ssoCookieName {
			foundSSO = true
			if !c.Secure {
				t.Error("SSO cookie must be Secure")
			}
		}
	}
	if !foundSSO {
		t.Errorf("SSO cookie %q not found in response", ssoCookieName)
	}

	// Verify the grant carries the fake claims identity.
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	grant := u.Query().Get("grant")
	gotID, err := ao.Grants.Verify(grant, "openclaw", time.Now())
	if err != nil {
		t.Fatalf("Grants.Verify: %v", err)
	}
	if gotID.Sub != claims.Subject || gotID.Email != claims.Email {
		t.Errorf("grant identity: got %+v, want sub=%q email=%q", gotID, claims.Subject, claims.Email)
	}
	if len(gotID.OrgIDs) == 0 || gotID.OrgIDs[0] != "org-x" {
		t.Errorf("grant orgIDs: got %v, want [org-x]", gotID.OrgIDs)
	}
}

// TestServeCallback_MismatchedState_Rejects verifies the CSRF defense: a state cookie
// value that does not match the query state param must be rejected with 400.
func TestServeCallback_MismatchedState_Rejects(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{claims: saas.OIDCClaims{Subject: "sub1", Email: "u@example.com", EmailVerified: true}}
	ao := newTestAuthOrigin(t, v, ex, nil, nil)

	// Put a valid state value in the cookie but a DIFFERENT value in the query.
	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state=WRONG_STATE_VALUE", nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (CSRF defense)", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "" {
		t.Error("no Location on CSRF rejection")
	}
}

// TestServeCallback_WithResolver_CarriesResolverOrgs verifies that when a Resolver
// is set and succeeds, the grant identity carries the resolved orgs (not the fallback).
func TestServeCallback_WithResolver_CarriesResolverOrgs(t *testing.T) {
	claims := saas.OIDCClaims{Subject: "sub2", Email: "owner@example.com", EmailVerified: true}
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{claims: claims}

	resolverSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accountId": "acct-abc",
			"orgIds":    []string{"org-from-resolver"},
		})
	}))
	defer resolverSrv.Close()

	resolver := NewResolver(resolverSrv.URL, "resolver-token")
	ao := newTestAuthOrigin(t, v, ex, resolver, func(_ saas.OIDCClaims) []string { return []string{"org-fallback"} })

	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302", resp.StatusCode)
	}

	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	grant := u.Query().Get("grant")
	gotID, err := ao.Grants.Verify(grant, "openclaw", time.Now())
	if err != nil {
		t.Fatalf("Grants.Verify: %v", err)
	}
	if len(gotID.OrgIDs) != 1 || gotID.OrgIDs[0] != "org-from-resolver" {
		t.Errorf("orgIDs: got %v, want [org-from-resolver]", gotID.OrgIDs)
	}
}

// TestServeCallback_ResolverDisabled_UsesGroupsToOrgs verifies that when the Resolver
// returns ErrResolveDisabled, GroupsToOrgs is used as the fallback.
func TestServeCallback_ResolverDisabled_UsesGroupsToOrgs(t *testing.T) {
	claims := saas.OIDCClaims{Subject: "sub3", Email: "dev@example.com", EmailVerified: true}
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{claims: claims}

	// A resolver with an empty URL returns ErrResolveDisabled.
	disabledResolver := NewResolver("", "")
	ao := newTestAuthOrigin(t, v, ex, disabledResolver, func(_ saas.OIDCClaims) []string {
		return []string{"groups-org"}
	})

	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302", resp.StatusCode)
	}

	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	grant := u.Query().Get("grant")
	gotID, err := ao.Grants.Verify(grant, "openclaw", time.Now())
	if err != nil {
		t.Fatalf("Grants.Verify: %v", err)
	}
	if len(gotID.OrgIDs) != 1 || gotID.OrgIDs[0] != "groups-org" {
		t.Errorf("orgIDs: got %v, want [groups-org]", gotID.OrgIDs)
	}
}

// TestServeCallback_UnverifiedEmail_EmptyOrgIDs verifies that a callback where the
// IdP emits email_verified=false yields a grant/identity with empty OrgIDs. The
// request still succeeds (the identity is authenticated), but a subsequent
// org/private Authorize call would 403 because no org memberships are resolved.
// This closes the path where an attacker registers a matching email at an IdP
// that does not enforce ownership before issuing tokens and uses the unverified
// address to gain org access.
func TestServeCallback_UnverifiedEmail_EmptyOrgIDs(t *testing.T) {
	// email_verified=false: the resolver and GroupsToOrgs must both be bypassed.
	claims := saas.OIDCClaims{Subject: "sub-unverified", Email: "attacker@example.com", EmailVerified: false}
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{claims: claims}

	// Wire both a resolver (which would return orgs) and a GroupsToOrgs fallback
	// (which would also return orgs). Neither must fire when EmailVerified is false.
	resolverSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accountId": "acct-attacker",
			"orgIds":    []string{"org-should-not-be-granted"},
		})
	}))
	defer resolverSrv.Close()

	resolver := NewResolver(resolverSrv.URL, "resolver-token")
	ao := newTestAuthOrigin(t, v, ex, resolver, func(_ saas.OIDCClaims) []string {
		return []string{"groups-org-should-not-be-granted"}
	})

	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d want 302; unverified email should still complete the flow (authenticated), body: %s", resp.StatusCode, w.Body.String())
	}

	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	grant := u.Query().Get("grant")
	gotID, err := ao.Grants.Verify(grant, "openclaw", time.Now())
	if err != nil {
		t.Fatalf("Grants.Verify: %v", err)
	}
	if len(gotID.OrgIDs) != 0 {
		t.Errorf("expected empty OrgIDs for unverified email; got %v", gotID.OrgIDs)
	}
	// Confirm the identity still carries the correct email and EmailVerified=false.
	if gotID.Email != claims.Email {
		t.Errorf("email: got %q want %q", gotID.Email, claims.Email)
	}
	if gotID.EmailVerified {
		t.Error("EmailVerified should be false in the grant identity for an unverified email")
	}
}

// TestServeCallback_VerifyError_Fails verifies that a token verifier error causes a
// non-200 response and no redirect (fail closed).
func TestServeCallback_VerifyError_Fails(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", rawIDToken: "raw-id-token"}
	v := fakeOIDCVerifier{err: errors.New("token verification failed")}
	ao := newTestAuthOrigin(t, v, ex, nil, nil)

	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode == http.StatusFound {
		t.Fatal("should not redirect on verify error; fail closed")
	}
}

// TestServeCallback_ExchangeError_Fails verifies that a code-exchange error causes a
// 502 with NO Location header and NO SSO cookie set (fail closed on the exchange path).
func TestServeCallback_ExchangeError_Fails(t *testing.T) {
	ex := fakeOIDCExchanger{authURL: "https://idp.example/auth", err: errors.New("exchange failed")}
	v := fakeOIDCVerifier{claims: saas.OIDCClaims{Subject: "sub1", Email: "u@example.com", EmailVerified: true}}
	ao := newTestAuthOrigin(t, v, ex, nil, nil)

	statePl := stateCookiePayload{RD: "openclaw", Path: "/"}
	stateRaw, _ := json.Marshal(statePl)
	stateVal, err := ao.StateCodec.Encode(
		Identity{Sub: string(stateRaw)},
		time.Now().Add(10*time.Minute),
	)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authcode&state="+url.QueryEscape(stateVal), nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: stateVal})
	w := httptest.NewRecorder()
	ao.ServeCallback(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502 (fail closed on exchange error)", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "" {
		t.Error("no Location header should be set on exchange error")
	}
	for _, c := range resp.Cookies() {
		if c.Name == ssoCookieName && c.Value != "" {
			t.Error("SSO cookie must not be set on exchange error")
		}
	}
}
