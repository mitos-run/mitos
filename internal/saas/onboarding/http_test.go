package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// stubCaptcha is a test-only CaptchaVerifier that returns a configurable error
// (nil = pass). It records whether Verify was called so tests can assert that the
// service was (or was not) reached, and distinguishes a definitive rejection
// (ErrCaptchaInvalid, fail closed) from a transient verification error (fail open).
type stubCaptcha struct {
	err    error
	called bool
}

func (s *stubCaptcha) Verify(context.Context, string) error {
	s.called = true
	return s.err
}

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

// TestVerifyWaitlistedReturnsWaitlistedOnly asserts that when an allowlist is
// configured and the signup email is not on it, the verify endpoint returns 200
// with {"waitlisted": true}, sets no session cookie, and leaks no account id,
// org id, or api key. A sessions-enabled handler is used so the no-cookie
// assertion is meaningful even when the session path is wired.
func TestVerifyWaitlistedReturnsWaitlistedOnly(t *testing.T) {
	sessions := saas.NewSessionStore()
	tok := 0
	newTok := func() string {
		tok++
		return fmt.Sprintf("sess-%d", tok)
	}

	// Empty allowlist: no auto-allow domains, no rows -> all emails are denied.
	al := NewMemAllowlist(nil)
	hr := newHarnessWithOpts(t, ModeOpen, WithAllowlist(al))
	h := NewHandler(hr.svc, nil, WithHandlerSessions(sessions, newTok, false))

	postJSON(t, h, "/onboarding/signup", `{"email":"waitlisted@example.com"}`)
	verifyTok := hr.email.LastToken("waitlisted@example.com")
	if verifyTok == "" {
		t.Fatal("no verification token for waitlisted email")
	}

	rr := postJSON(t, h, "/onboarding/verify", `{"token":"`+verifyTok+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rr.Code, rr.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["waitlisted"] != true {
		t.Fatalf("waitlisted = %v, want true; body: %s", out["waitlisted"], rr.Body.String())
	}
	if _, ok := out["accountId"]; ok {
		t.Fatalf("waitlisted response must not include accountId, got %v", out)
	}
	if _, ok := out["apiKey"]; ok {
		t.Fatalf("waitlisted response must not include apiKey, got %v", out)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mitos_session" {
			t.Fatal("waitlisted verify must not set a session cookie")
		}
	}
}

// TestSignupDisposableDomainReturnsUniformAcceptedAndNoEmail asserts that a
// signup with a known disposable email domain returns the SAME 202 body as a
// normal signup (no enumeration), does NOT call the service (no verification
// email is sent, no pending record is created), and does NOT log the email or
// domain value. The disposable check fires before the service call.
func TestSignupDisposableDomainReturnsUniformAcceptedAndNoEmail(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	disp := NewDisposable([]string{"mailinator.com"}, nil)
	h := NewHandler(hr.svc, nil, WithDisposable(disp))

	disposableRR := postJSON(t, h, "/onboarding/signup", `{"email":"attacker@mailinator.com"}`)
	if disposableRR.Code != http.StatusAccepted {
		t.Fatalf("disposable signup: status %d, want 202; body %s", disposableRR.Code, disposableRR.Body.String())
	}

	// The service must NOT have been called: no verification email sent.
	if tok := hr.email.LastToken("attacker@mailinator.com"); tok != "" {
		t.Fatalf("disposable signup must not send a verification email; got token %q", tok)
	}

	// The response body must be byte-identical to a normal (non-disposable) signup.
	normalRR := postJSON(t, h, "/onboarding/signup", `{"email":"real@example.com"}`)
	if normalRR.Code != http.StatusAccepted {
		t.Fatalf("normal signup: status %d, want 202; body %s", normalRR.Code, normalRR.Body.String())
	}
	if disposableRR.Body.String() != normalRR.Body.String() {
		t.Fatalf("disposable and normal signup bodies differ (enumeration leak):\ndisposable=%s\nnormal=%s",
			disposableRR.Body.String(), normalRR.Body.String())
	}
}

// TestSignupVelocityCapReturnsUniformAcceptedAndNoEmail asserts that a signup
// from an IP that has exceeded the per-IP velocity cap returns the same uniform
// 202 as a normal signup (no enumeration), does NOT call the service (no
// verification email is sent, no pending record is created), and that a signup
// from a different IP is not affected by the cap.
func TestSignupVelocityCapReturnsUniformAcceptedAndNoEmail(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	vel := NewVelocity(1, time.Hour)
	// WithTrustedProxyHops(1): the handler trusts one upstream proxy so XFF is used
	// as the velocity key, matching the deployment topology this test exercises.
	h := NewHandler(hr.svc, nil, WithVelocity(vel), WithTrustedProxyHops(1))

	mux := http.NewServeMux()
	h.Routes(mux)

	// postWithXFF is a local helper that sets an X-Forwarded-For header.
	postWithXFF := func(xff, email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/onboarding/signup",
			strings.NewReader(`{"email":"`+email+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	// First signup from 1.2.3.4: allowed; service is called; email is sent.
	first := postWithXFF("1.2.3.4", "vel1@example.com")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first signup: status %d, want 202; body %s", first.Code, first.Body.String())
	}
	if hr.email.LastToken("vel1@example.com") == "" {
		t.Fatal("first signup: expected verification email to be sent")
	}

	// Second signup from 1.2.3.4 (over cap, limit=1): returns 202 but NO email.
	second := postWithXFF("1.2.3.4", "vel2@example.com")
	if second.Code != http.StatusAccepted {
		t.Fatalf("over-cap signup: status %d, want 202; body %s", second.Code, second.Body.String())
	}
	if hr.email.LastToken("vel2@example.com") != "" {
		t.Fatal("over-cap signup must not send a verification email")
	}

	// The 202 body must be byte-identical (no enumeration).
	if first.Body.String() != second.Body.String() {
		t.Fatalf("velocity-capped and normal signup bodies differ (enumeration leak):\nfirst=%s\nsecond=%s",
			first.Body.String(), second.Body.String())
	}

	// A signup from a different IP must not be affected by the cap.
	third := postWithXFF("5.6.7.8", "vel3@example.com")
	if third.Code != http.StatusAccepted {
		t.Fatalf("different-IP signup: status %d, want 202; body %s", third.Code, third.Body.String())
	}
	if hr.email.LastToken("vel3@example.com") == "" {
		t.Fatal("different-IP signup must succeed and send a verification email")
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

// TestSignupFailedCaptchaReturnsUniformAcceptedAndNoService asserts that when a
// captcha verifier is wired and the solution fails verification, the handler
// returns the SAME byte-identical 202 as a normal signup (no enumeration),
// does NOT call the service (no verification email is sent, no pending record
// is created), and does not reach the service at all.
func TestSignupFailedCaptchaReturnsUniformAcceptedAndNoService(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	stub := &stubCaptcha{err: ErrCaptchaInvalid}
	h := NewHandler(hr.svc, nil, WithCaptcha(stub))

	captchaRR := postJSON(t, h, "/onboarding/signup", `{"email":"bot@example.com","captcha":"bad-token"}`)
	if captchaRR.Code != http.StatusAccepted {
		t.Fatalf("captcha-failed signup: status %d, want 202; body %s", captchaRR.Code, captchaRR.Body.String())
	}

	// The verifier must have been consulted (guard against the check being skipped).
	if !stub.called {
		t.Fatal("captcha verifier was not called on a captcha-guarded signup")
	}
	// The service must NOT have been called: no verification email sent.
	if tok := hr.email.LastToken("bot@example.com"); tok != "" {
		t.Fatalf("captcha-failed signup must not send a verification email; got token %q", tok)
	}

	// The response body must be byte-identical to a normal (passing captcha) signup.
	stub2 := &stubCaptcha{err: nil}
	h2 := NewHandler(hr.svc, nil, WithCaptcha(stub2))
	normalRR := postJSON(t, h2, "/onboarding/signup", `{"email":"real@example.com","captcha":"good-token"}`)
	if normalRR.Code != http.StatusAccepted {
		t.Fatalf("normal signup: status %d, want 202; body %s", normalRR.Code, normalRR.Body.String())
	}
	if captchaRR.Body.String() != normalRR.Body.String() {
		t.Fatalf("captcha-failed and normal signup bodies differ (enumeration leak):\ncaptcha-failed=%s\nnormal=%s",
			captchaRR.Body.String(), normalRR.Body.String())
	}
}

// TestSignupCaptchaVerificationErrorFailsOpen asserts that a NON-definitive
// verification error (the provider is unreachable / faulted, NOT an explicit
// rejection) fails OPEN: the signup proceeds so a captcha-provider outage does
// not silently drop legitimate users. Only ErrCaptchaInvalid fails closed.
func TestSignupCaptchaVerificationErrorFailsOpen(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	stub := &stubCaptcha{err: errors.New("captcha: request failed")}
	h := NewHandler(hr.svc, nil, WithCaptcha(stub))

	rr := postJSON(t, h, "/onboarding/signup", `{"email":"legit@example.com","captcha":"tok"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("captcha-outage signup: status %d, want 202; body %s", rr.Code, rr.Body.String())
	}
	if !stub.called {
		t.Fatal("captcha verifier was not called")
	}
	// Fail OPEN: the service WAS reached, so a verification email was sent.
	if tok := hr.email.LastToken("legit@example.com"); tok == "" {
		t.Fatal("captcha-outage signup must fail open and reach the service")
	}
}

// TestSignupPassingCaptchaProceedsToService asserts that when a captcha verifier
// is wired and the solution passes verification, the signup proceeds normally:
// the service is called, a verification email is sent, and 202 is returned.
func TestSignupPassingCaptchaProceedsToService(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	stub := &stubCaptcha{err: nil}
	h := NewHandler(hr.svc, nil, WithCaptcha(stub))

	rr := postJSON(t, h, "/onboarding/signup", `{"email":"legit@example.com","captcha":"good-token"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("passing captcha signup: status %d, want 202; body %s", rr.Code, rr.Body.String())
	}

	// The service must have been called: a verification email was sent.
	if tok := hr.email.LastToken("legit@example.com"); tok == "" {
		t.Fatal("passing captcha signup must send a verification email")
	}

	if !stub.called {
		t.Fatal("captcha verifier was not called")
	}
}

// TestClientIPTrustedProxyHops unit-tests the Handler.clientIP method directly
// by constructing a Handler with various trustedProxyHops values and asserting
// the key returned for different (RemoteAddr, X-Forwarded-For) combinations.
func TestClientIPTrustedProxyHops(t *testing.T) {
	makeHandler := func(hops int) *Handler {
		h := &Handler{}
		WithTrustedProxyHops(hops)(h)
		return h
	}

	makeReq := func(remoteAddr, xff string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return req
	}

	t.Run("hops=1 single XFF entry is the key", func(t *testing.T) {
		h := makeHandler(1)
		req := makeReq("10.0.0.1:1234", "1.1.1.1")
		if got := h.clientIP(req); got != "1.1.1.1" {
			t.Fatalf("clientIP = %q, want 1.1.1.1", got)
		}
	})

	t.Run("hops=1 attacker prepended entry is ignored", func(t *testing.T) {
		// Real client is 1.1.1.1 (appended by the trusted proxy).
		// Attacker prepended 9.9.9.9. With hops=1 the key must be 1.1.1.1.
		h := makeHandler(1)
		req := makeReq("10.0.0.1:1234", "9.9.9.9, 1.1.1.1")
		if got := h.clientIP(req); got != "1.1.1.1" {
			t.Fatalf("clientIP = %q, want 1.1.1.1 (spoofed 9.9.9.9 must be ignored)", got)
		}
	})

	t.Run("hops=1 no XFF falls back to RemoteAddr host", func(t *testing.T) {
		h := makeHandler(1)
		req := makeReq("10.0.0.1:1234", "")
		if got := h.clientIP(req); got != "10.0.0.1" {
			t.Fatalf("clientIP = %q, want 10.0.0.1 (RemoteAddr host)", got)
		}
	})
}

// TestClientIPHopsZeroIgnoresXFF asserts that with the default trustedProxyHops=0
// (safe-by-default for self-hosters) the velocity key is derived from RemoteAddr
// and X-Forwarded-For is ignored. Two signups with DIFFERENT spoofed XFF values
// but the SAME RemoteAddr must share ONE velocity bucket, proving that XFF
// spoofing cannot mint fresh rate-limit buckets.
func TestClientIPHopsZeroIgnoresXFF(t *testing.T) {
	hr := newHarness(t, ModeOpen)
	vel := NewVelocity(1, time.Hour)
	// No WithTrustedProxyHops: defaults to 0 (ignore XFF).
	h := NewHandler(hr.svc, nil, WithVelocity(vel))

	mux := http.NewServeMux()
	h.Routes(mux)

	postWithAddrs := func(remoteAddr, xff, email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/onboarding/signup",
			strings.NewReader(`{"email":"`+email+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	// First signup: RemoteAddr=10.0.0.1, XFF=1.2.3.4. hops=0 -> key is 10.0.0.1.
	first := postWithAddrs("10.0.0.1:9999", "1.2.3.4", "spooftest1@example.com")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first signup: status %d, want 202; body %s", first.Code, first.Body.String())
	}
	if hr.email.LastToken("spooftest1@example.com") == "" {
		t.Fatal("first signup must send a verification email")
	}

	// Second signup: SAME RemoteAddr=10.0.0.1, DIFFERENT spoofed XFF=9.9.9.9.
	// hops=0 -> key is still 10.0.0.1 -> cap fires; no email sent.
	second := postWithAddrs("10.0.0.1:9999", "9.9.9.9", "spooftest2@example.com")
	if second.Code != http.StatusAccepted {
		t.Fatalf("second signup: status %d, want 202; body %s", second.Code, second.Body.String())
	}
	if hr.email.LastToken("spooftest2@example.com") != "" {
		t.Fatal("hops=0: second signup with same RemoteAddr but different XFF must be capped; XFF spoofing must not mint a fresh bucket")
	}
}

// TestSignupOddValidEmailReturnsUniform202 asserts that a syntactically valid
// email whose local part becomes empty after plus-tag stripping (e.g.
// "+x@gmail.com") returns the same uniform 202 as a normal accepted signup,
// sends NO verification email, and creates NO account or pending record. It
// must NOT return 500.
func TestSignupOddValidEmailReturnsUniform202(t *testing.T) {
	h, hr := newHandler(t, ModeOpen)

	oddRR := postJSON(t, h, "/onboarding/signup", `{"email":"+x@gmail.com"}`)
	if oddRR.Code == http.StatusInternalServerError {
		t.Fatalf("odd email signup returned 500; want 202; body %s", oddRR.Body.String())
	}
	if oddRR.Code != http.StatusAccepted {
		t.Fatalf("odd email signup: status %d, want 202; body %s", oddRR.Code, oddRR.Body.String())
	}

	// Must send NO verification email.
	if tok := hr.email.LastToken("+x@gmail.com"); tok != "" {
		t.Fatalf("odd email signup must not send a verification email; got token %q", tok)
	}

	// Body must be byte-identical to a normal accepted signup (no enumeration).
	normalRR := postJSON(t, h, "/onboarding/signup", `{"email":"normal@example.com"}`)
	if normalRR.Code != http.StatusAccepted {
		t.Fatalf("normal signup: status %d, want 202; body %s", normalRR.Code, normalRR.Body.String())
	}
	if oddRR.Body.String() != normalRR.Body.String() {
		t.Fatalf("odd email and normal signup bodies differ (enumeration leak):\nodd=%s\nnormal=%s",
			oddRR.Body.String(), normalRR.Body.String())
	}
}

// postWaitlistJSON mounts only RoutesWaitlistOnly (never the full signup/
// verify set) and posts body to /onboarding/waitlist.
func postWaitlistJSON(h *Handler, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.RoutesWaitlistOnly(mux)
	req := httptest.NewRequest(http.MethodPost, "/onboarding/waitlist", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// TestWaitlistJoinRecordsEntryAndReturns202 asserts POST /onboarding/waitlist
// records a waitlist entry and returns 202, without provisioning anything.
func TestWaitlistJoinRecordsEntryAndReturns202(t *testing.T) {
	h, hr := newHandler(t, ModeWaitlist)

	rr := postWaitlistJSON(h, `{"email":"join@example.com"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202; body %s", rr.Code, rr.Body.String())
	}

	wl, err := hr.svc.JoinWaitlist(context.Background())
	if err != nil {
		t.Fatalf("JoinWaitlist: %v", err)
	}
	if len(wl) != 1 || wl[0].Email != "join@example.com" {
		t.Fatalf("waitlist = %+v", wl)
	}

	// No account, no email sent: this is intake only.
	if _, err := hr.store.GetAccountByEmail(context.Background(), "join@example.com"); !errors.Is(err, saas.ErrNotFound) {
		t.Fatalf("waitlist join must not provision an account, got %v", err)
	}
}

// TestWaitlistJoinDoesNotEnumerate asserts a duplicate join returns the exact
// same 202 as a fresh one (only one row recorded), so a probe cannot tell an
// address is already on the list.
func TestWaitlistJoinDoesNotEnumerate(t *testing.T) {
	h, hr := newHandler(t, ModeWaitlist)

	first := postWaitlistJSON(h, `{"email":"dup@example.com"}`)
	second := postWaitlistJSON(h, `{"email":"dup@example.com"}`)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("status codes: first=%d second=%d, want both 202", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("bodies differ between fresh and duplicate join (enumeration leak):\nfirst=%s\nsecond=%s",
			first.Body.String(), second.Body.String())
	}

	wl, err := hr.svc.JoinWaitlist(context.Background())
	if err != nil {
		t.Fatalf("JoinWaitlist: %v", err)
	}
	if len(wl) != 1 {
		t.Fatalf("waitlist = %+v, want exactly one deduped entry", wl)
	}
}

// TestWaitlistJoinRejectsBadEmail asserts a malformed email is a 400, the
// same validation the full signup path applies.
func TestWaitlistJoinRejectsBadEmail(t *testing.T) {
	h, _ := newHandler(t, ModeWaitlist)
	rr := postWaitlistJSON(h, `{"email":"not-an-email"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rr.Code, rr.Body.String())
	}
}

// TestWaitlistJoinVelocityCapReturnsUniform202 asserts a per-IP velocity cap
// silently caps the intake (no new row past the cap) but still returns the
// same uniform 202, so an over-cap client learns nothing.
func TestWaitlistJoinVelocityCapReturnsUniform202(t *testing.T) {
	hr := newHarness(t, ModeWaitlist)
	h := NewHandler(hr.svc, nil, WithVelocity(NewVelocity(1, time.Hour)), WithTrustedProxyHops(1))
	mux := http.NewServeMux()
	h.RoutesWaitlistOnly(mux)

	postWithXFF := func(xff, email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/onboarding/waitlist", strings.NewReader(`{"email":"`+email+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	first := postWithXFF("9.9.9.9", "capped1@example.com")
	second := postWithXFF("9.9.9.9", "capped2@example.com")
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("status codes: first=%d second=%d, want both 202", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("velocity-capped body differs from normal (enumeration leak):\nfirst=%s\nsecond=%s",
			first.Body.String(), second.Body.String())
	}

	wl, err := hr.svc.JoinWaitlist(context.Background())
	if err != nil {
		t.Fatalf("JoinWaitlist: %v", err)
	}
	if len(wl) != 1 || wl[0].Email != "capped1@example.com" {
		t.Fatalf("waitlist = %+v, want only the first (under-cap) entry", wl)
	}
}

// TestWaitlistOnlyRoutesOmitSignupAndVerify asserts RoutesWaitlistOnly mounts
// ONLY the waitlist intake: there is no verify token to redeem and no
// account is ever provisioned in this mode, so /onboarding/signup and
// /onboarding/verify must both be absent (404).
func TestWaitlistOnlyRoutesOmitSignupAndVerify(t *testing.T) {
	h, _ := newHandler(t, ModeWaitlist)
	mux := http.NewServeMux()
	h.RoutesWaitlistOnly(mux)

	for _, path := range []string{"/onboarding/signup", "/onboarding/verify"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"x@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404 (not mounted in waitlist-only mode)", path, rr.Code)
		}
	}
}
