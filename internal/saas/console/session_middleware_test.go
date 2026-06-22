package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"mitos.run/mitos/internal/saas"
)

// sessionSetup builds an accounts service with one signed-up account and a
// session service that has issued it a token, plus a console wrapped in the
// production SessionMiddleware. It returns the raw session token to set as the
// cookie.
func sessionSetup(t *testing.T) (handler http.Handler, token, orgID string) {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()
	acct, org, err := accounts.SignUp(ctx, "dev@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if _, err := accounts.CreateKey(ctx, acct.ID, saas.CreateKeyRequest{OrgID: org.ID, Name: "k", Scopes: []string{saas.ScopeSandboxes}}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	sessions := saas.NewSessionStore()
	token = "raw-session-token"
	sessions.Issue(acct.ID, token)
	svc := saas.NewSessionService(sessions, accounts)

	con := New(Deps{Accounts: accounts})
	return SessionMiddleware(svc)(con), token, org.ID
}

// TestSessionMiddlewareAttachesCallerFromCookie asserts a valid session cookie
// is resolved to its account and active org and attached as the caller context,
// so a downstream BFF endpoint returns that org's data.
func TestSessionMiddlewareAttachesCallerFromCookie(t *testing.T) {
	h, token, orgID := sessionSetup(t)
	r := httptest.NewRequest("GET", "/console/keys", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OrgID string `json:"org_id"`
	}
	decode(t, w, &resp)
	if resp.OrgID != orgID {
		t.Errorf("org_id = %q, want %q (the session's active org)", resp.OrgID, orgID)
	}
}

// TestSessionMiddlewareRejectsMissingCookie asserts a request with no session
// cookie is refused (401) and never reaches the BFF.
func TestSessionMiddlewareRejectsMissingCookie(t *testing.T) {
	h, _, _ := sessionSetup(t)
	r := httptest.NewRequest("GET", "/console/keys", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestSessionMiddlewareRejectsForgedCookie asserts an unknown/forged session
// token is refused (401).
func TestSessionMiddlewareRejectsForgedCookie(t *testing.T) {
	h, _, _ := sessionSetup(t)
	r := httptest.NewRequest("GET", "/console/keys", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "forged"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
