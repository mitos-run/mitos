package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

func newHandler(t *testing.T, mode Mode) (*Handler, *harness) {
	t.Helper()
	h := newHarness(t, mode)
	return NewHandler(h.svc, nil), h
}

func postJSON(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestSignupReturnsAcceptedAndSendsEmail(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	rr := postJSON(t, h, "/onboarding/signup", `{"email":"new@example.com"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202; body %s", rr.Code, rr.Body.String())
	}
	if hr.email.LastToken("new@example.com") == "" {
		t.Fatal("signup did not send a verification email")
	}
	// Body must not contain a token.
	if strings.Contains(rr.Body.String(), hr.email.LastToken("new@example.com")) {
		t.Fatal("signup response leaked the verification token")
	}
}

// TestSignupDoesNotEnumerate asserts a signup for an EXISTING email returns the
// exact same status and body as a fresh signup, so a probe cannot tell whether
// an account already exists.
func TestSignupDoesNotEnumerate(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	// Pre-create an account for taken@example.com.
	if _, _, err := hr.svc.accounts.SignUp(context.Background(), "taken@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	fresh := postJSON(t, h, "/onboarding/signup", `{"email":"fresh@example.com"}`)
	taken := postJSON(t, h, "/onboarding/signup", `{"email":"taken@example.com"}`)

	if fresh.Code != taken.Code {
		t.Fatalf("status differs: fresh=%d taken=%d (enumeration leak)", fresh.Code, taken.Code)
	}
	if fresh.Body.String() != taken.Body.String() {
		t.Fatalf("body differs between fresh and taken email (enumeration leak):\nfresh=%s\ntaken=%s", fresh.Body.String(), taken.Body.String())
	}
}

func TestSignupNormalizesEmail(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	rr := postJSON(t, h, "/onboarding/signup", `{"email":"  MixedCase@Example.COM "}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rr.Code)
	}
	if hr.email.LastToken("mixedcase@example.com") == "" {
		t.Fatal("email was not normalized to lowercase/trimmed before signup")
	}
}

func TestSignupRejectsBadEmail(t *testing.T) {
	h, _ := newHandler(t, ModeOpen)
	for _, body := range []string{`{"email":"not-an-email"}`, `{"email":""}`, `{"email":"a@"}`, `{"email":"Foo <foo@x.com>"}`} {
		rr := postJSON(t, h, "/onboarding/signup", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status %d, want 400", body, rr.Code)
		}
	}
}

func TestVerifyHappyPathReturnsKey(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	postJSON(t, h, "/onboarding/signup", `{"email":"verify@example.com"}`)
	tok := hr.email.LastToken("verify@example.com")

	rr := postJSON(t, h, "/onboarding/verify", `{"token":"`+tok+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["orgId"] == "" || out["accountId"] == "" {
		t.Fatalf("missing account/org in response: %v", out)
	}
	if out["apiKey"] == nil || out["apiKey"] == "" {
		t.Fatalf("missing one-time api key in response: %v", out)
	}
}

func TestVerifyGetLinkTarget(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	postJSON(t, h, "/onboarding/signup", `{"email":"link@example.com"}`)
	tok := hr.email.LastToken("link@example.com")

	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/onboarding/verify?token="+tok, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET verify status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
}

// TestVerifyBadTokenIsGeneric asserts an unknown token yields a generic 400 that
// reveals nothing.
func TestVerifyBadTokenIsGeneric(t *testing.T) {
	h, _ := newHandler(t, ModeOpen)
	rr := postJSON(t, h, "/onboarding/verify", `{"token":"totally-bogus"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "totally-bogus") {
		t.Fatal("verify error echoed the presented token")
	}
}

func TestSignupRejectsUnknownFields(t *testing.T) {
	h, _ := newHandler(t, ModeOpen)
	rr := postJSON(t, h, "/onboarding/signup", `{"email":"x@y.com","admin":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (unknown field rejected)", rr.Code)
	}
}

// TestVerifyTokenIsSingleUse asserts a used token cannot be reused to provision a
// second time; the second verify is the idempotent already-done path, not a new
// key issue.
func TestVerifyTokenIsSingleUse(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	postJSON(t, h, "/onboarding/signup", `{"email":"once@example.com"}`)
	tok := hr.email.LastToken("once@example.com")

	first := postJSON(t, h, "/onboarding/verify", `{"token":"`+tok+`"}`)
	second := postJSON(t, h, "/onboarding/verify", `{"token":"`+tok+`"}`)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("verify codes first=%d second=%d", first.Code, second.Code)
	}
	var out2 map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &out2)
	if out2["alreadyDone"] != true {
		t.Fatalf("second verify should be idempotent already-done, got %v", out2)
	}
	if _, ok := out2["apiKey"]; ok {
		t.Fatal("re-verify must not issue a second api key")
	}
}

// TestVerifySetsCookieOnFreshVerify asserts that a successful first-time verify
// response includes a mitos_session Set-Cookie header with HttpOnly and
// SameSite=Lax, and that the session token is registered in the session store.
func TestVerifySetsCookieOnFreshVerify(t *testing.T) {
	sessions := saas.NewSessionStore()
	tok := 0
	newTok := func() string {
		tok++
		return fmt.Sprintf("sess-%d", tok)
	}

	hr := newHarness(t, ModeOpen)
	h := NewHandler(hr.svc, nil, WithHandlerSessions(sessions, newTok, false))

	postJSON(t, h, "/onboarding/signup", `{"email":"cookie@example.com"}`)
	verifyTok := hr.email.LastToken("cookie@example.com")

	rr := postJSON(t, h, "/onboarding/verify", `{"token":"`+verifyTok+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rr.Code, rr.Body.String())
	}

	var found *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mitos_session" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("fresh verify did not set the mitos_session cookie")
	}
	if found.Value == "" {
		t.Error("mitos_session cookie value must not be empty")
	}
	if !found.HttpOnly {
		t.Error("mitos_session cookie must have HttpOnly set")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("mitos_session cookie SameSite=%v, want Lax", found.SameSite)
	}
	// Confirm the token was registered in the session store and resolves.
	if accountID, err := sessions.Resolve(found.Value); err != nil || accountID == "" {
		t.Errorf("session token in cookie does not resolve to an account: %v", err)
	}
}

// TestVerifyResponseIncludesUseCase asserts that the verify response JSON
// includes a "useCase" field reflecting the slug provided at signup.
func TestVerifyResponseIncludesUseCase(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)
	postJSON(t, h, "/onboarding/signup", `{"email":"uc@example.com","uc":"rollouts"}`)
	tok := hr.email.LastToken("uc@example.com")
	if tok == "" {
		t.Fatal("no verification token")
	}

	rr := postJSON(t, h, "/onboarding/verify", `{"token":"`+tok+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["useCase"] != "rollouts" {
		t.Fatalf("useCase = %v, want %q", out["useCase"], "rollouts")
	}
}

// TestVerifyNoSessionCookieOnReVerify asserts that an idempotent re-verify
// (AlreadyDone true) does NOT set a new mitos_session cookie. The existing
// browser session (if any) must remain the user's only session.
func TestVerifyNoSessionCookieOnReVerify(t *testing.T) {
	sessions := saas.NewSessionStore()
	tok := 0
	newTok := func() string {
		tok++
		return fmt.Sprintf("sess-%d", tok)
	}

	hr := newHarness(t, ModeOpen)
	h := NewHandler(hr.svc, nil, WithHandlerSessions(sessions, newTok, false))

	postJSON(t, h, "/onboarding/signup", `{"email":"reverify@example.com"}`)
	verifyTok := hr.email.LastToken("reverify@example.com")

	// First verify: provisions the account and sets the cookie.
	first := postJSON(t, h, "/onboarding/verify", `{"token":"`+verifyTok+`"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first verify status %d, want 200", first.Code)
	}

	// Second verify: idempotent re-verify must NOT set a new cookie.
	second := postJSON(t, h, "/onboarding/verify", `{"token":"`+verifyTok+`"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second verify status %d, want 200", second.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &out)
	if out["alreadyDone"] != true {
		t.Fatalf("second verify alreadyDone=%v, want true", out["alreadyDone"])
	}
	for _, c := range second.Result().Cookies() {
		if c.Name == "mitos_session" {
			t.Fatal("re-verify must NOT set a mitos_session cookie")
		}
	}
}
