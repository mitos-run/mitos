package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// decode401 parses the structured error envelope out of a 401 response.
func decode401(t *testing.T, w *httptest.ResponseRecorder) (code, remediation string) {
	t.Helper()
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope from %q: %v", w.Body.String(), err)
	}
	return env.Error.Code, env.Error.Remediation
}

// assertConsole401Body asserts a console-surface 401 body: code unauthorized,
// a remediation that names the SPA sign-in path, and no trace of the sandbox
// API's per-sandbox-token wording (issue #631: the remediation must match the
// surface).
func assertConsole401Body(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	code, remediation := decode401(t, w)
	if code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", code)
	}
	if !strings.Contains(remediation, "/login") {
		t.Errorf("remediation %q does not name the /login sign-in path", remediation)
	}
	if strings.Contains(w.Body.String(), "sandbox-token") || strings.Contains(w.Body.String(), "per-sandbox") {
		t.Errorf("console 401 body carries sandbox API wording: %s", w.Body.String())
	}
}

// TestSessionMiddleware401BodyNamesSignIn asserts the unauthenticated console
// 401 body carries the console-surface remediation (sign in at /login), not
// the sandbox API's per-sandbox-token wording, for both a missing and a
// forged session cookie.
func TestSessionMiddleware401BodyNamesSignIn(t *testing.T) {
	h, _, _ := sessionSetup(t)

	t.Run("missing cookie", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/console/capabilities", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertConsole401Body(t, w)
	})

	t.Run("forged cookie", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/console/capabilities", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "forged"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertConsole401Body(t, w)
	})
}

// TestConsoleCallerRefusal401BodyNamesSignIn asserts the defense-in-depth
// caller() refusal (a request that reached the BFF with no caller context
// attached) also carries the console-surface 401 body, not the sandbox
// wording.
func TestConsoleCallerRefusal401BodyNamesSignIn(t *testing.T) {
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	con := New(Deps{Accounts: accounts})
	r := httptest.NewRequest("GET", "/console/keys", nil)
	w := httptest.NewRecorder()
	con.ServeHTTP(w, r)
	assertConsole401Body(t, w)
}
