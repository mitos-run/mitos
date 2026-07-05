package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// errSendApproved is a test EmailSender whose SendApproved always returns an
// error so the 500-on-send-failure path can be exercised.
type errSendApproved struct{}

func (errSendApproved) SendVerification(_ context.Context, _, _ string) error { return nil }
func (errSendApproved) SendApproved(_ context.Context, _ string) error {
	return errors.New("smtp: connection refused")
}
func (errSendApproved) SendInvite(_ context.Context, _, _, _, _ string) error { return nil }

// fixedNow is the deterministic clock injected in tests. Using a concrete time
// makes Add calls reproducible regardless of wall-clock drift.
var fixedNow = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

// newTestApproveHandler builds an approveSignupHandler directly (same package
// access) with a fixed clock. Use this instead of NewApproveSignupHandler in
// tests that need the now field to be deterministic.
func newTestApproveHandler(al Allowlist, em EmailSender, tok string) http.Handler {
	return &approveSignupHandler{
		allowlist: al,
		email:     em,
		token:     tok,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       fixedNow,
	}
}

// postApprove fires a POST to the handler with the given Authorization header
// value and JSON body. Pass authHeader="" to omit the Authorization header.
func postApprove(t *testing.T, h http.Handler, authHeader, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/approve-signup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// decodeEmail decodes {"email": "..."} from rr and returns the value.
func decodeEmail(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return resp["email"]
}

// TestApproveSignup_SuccessAddsAllowlistAndSendsEmail covers the happy path:
// correct bearer + valid email -> 200, allowlist row added, approval email sent.
func TestApproveSignup_SuccessAddsAllowlistAndSendsEmail(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "s3cr3t")

	rr := postApprove(t, h, "Bearer s3cr3t", `{"email":"user@example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := decodeEmail(t, rr); got != "user@example.com" {
		t.Fatalf("response email = %q, want user@example.com", got)
	}

	ok, err := al.IsAllowed(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if !ok {
		t.Fatal("allowlist does not contain the approved email after a successful approve")
	}
	if !em.Approved("user@example.com") {
		t.Fatal("SendApproved was not called for the approved email")
	}
}

// TestApproveSignup_Canonicalization checks that a mixed-case address is
// stored and returned in its canonical form (lowercased and plus-tag stripped).
// Updated for B1b: canonicalEmail strips the plus-tag for all providers, so
// "Foo+x@Example.com" becomes "foo@example.com" (not "foo+x@example.com").
func TestApproveSignup_Canonicalization(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	rr := postApprove(t, h, "Bearer tok", `{"email":"Foo+x@Example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// canonicalEmail strips the plus-tag for all providers:
	// "Foo+x@Example.com" -> normalizeEmail -> "foo+x@example.com"
	//   -> drop plus-tag -> "foo@example.com"
	const want = "foo@example.com"
	if got := decodeEmail(t, rr); got != want {
		t.Fatalf("response email = %q, want %q", got, want)
	}

	ok, err := al.IsAllowed(context.Background(), want)
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if !ok {
		t.Fatalf("allowlist does not contain canonical email %q", want)
	}
	// The allowlist row is the canonical identity, but the approval email is
	// delivered to the ORIGINAL typed address (normalized), so a plus-tagged inbox
	// on a non-Gmail provider still receives it.
	const delivery = "foo+x@example.com"
	if !em.Approved(delivery) {
		t.Fatalf("SendApproved was not called with the delivery email %q", delivery)
	}
	if em.Approved(want) {
		t.Fatalf("SendApproved should deliver to the original %q, not the canonical %q", delivery, want)
	}
}

// TestApproveSignup_CanonicalGmailStoresCanonical confirms that approving a
// Gmail address with dots and a plus-tag stores the canonical identity in the
// allowlist. This ensures the verify gate matches any folded variant of that
// identity without requiring a separate approval for each variant.
func TestApproveSignup_CanonicalGmailStoresCanonical(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	rr := postApprove(t, h, "Bearer tok", `{"email":"U.ser+x@Gmail.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// "U.ser+x@Gmail.com" -> normalizeEmail -> "u.ser+x@gmail.com"
	//   -> drop plus-tag -> "u.ser@gmail.com"
	//   -> gmail domain, strip dots -> "user@gmail.com"
	const want = "user@gmail.com"
	if got := decodeEmail(t, rr); got != want {
		t.Fatalf("response email = %q, want %q", got, want)
	}

	ok, err := al.IsAllowed(context.Background(), want)
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if !ok {
		t.Fatalf("allowlist does not contain canonical email %q after approving a folded variant", want)
	}
}

// TestApproveSignup_MissingBearer401 checks that a request with no
// Authorization header is rejected with 401 and causes no side effects.
func TestApproveSignup_MissingBearer401(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	// No Authorization header.
	rr := postApprove(t, h, "", `{"email":"u@x.com"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	ok, _ := al.IsAllowed(context.Background(), "u@x.com")
	if ok {
		t.Fatal("allowlist must not be modified on 401")
	}
	if em.Approved("u@x.com") {
		t.Fatal("SendApproved must not be called on 401")
	}
}

// TestApproveSignup_WrongBearer401 checks that a mismatched bearer token
// results in 401 with no side effects.
func TestApproveSignup_WrongBearer401(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "correct-token")

	rr := postApprove(t, h, "Bearer wrong-token", `{"email":"u@x.com"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", rr.Code)
	}
	ok, _ := al.IsAllowed(context.Background(), "u@x.com")
	if ok {
		t.Fatal("allowlist must not be modified on wrong-bearer 401")
	}
	if em.Approved("u@x.com") {
		t.Fatal("SendApproved must not be called on wrong-bearer 401")
	}
}

// TestApproveSignup_EmptyHandlerToken401 checks that a handler constructed
// with an empty token rejects every request with 401 (fail-closed).
func TestApproveSignup_EmptyHandlerToken401(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "") // empty token -> fail-closed

	rr := postApprove(t, h, "Bearer anything", `{"email":"u@x.com"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for empty handler token, got %d", rr.Code)
	}
	// The empty-token case is the most dangerous misconfiguration (a fully open
	// endpoint), so prove it has NO side effects, not just the status code.
	if ok, _ := al.IsAllowed(context.Background(), "u@x.com"); ok {
		t.Fatal("allowlist must not be modified when the handler token is empty")
	}
	if em.Approved("u@x.com") {
		t.Fatal("SendApproved must not be called when the handler token is empty")
	}
}

// TestApproveSignup_MissingEmail400 checks that a body with an absent or
// empty email field is rejected with 400 and causes no side effects.
func TestApproveSignup_MissingEmail400(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	for _, body := range []string{`{}`, `{"email":""}`} {
		rr := postApprove(t, h, "Bearer tok", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %q: expected 400, got %d", body, rr.Code)
		}
	}
}

// TestApproveSignup_MalformedEmail400 checks that an invalid email is
// rejected with 400 and causes no side effects.
func TestApproveSignup_MalformedEmail400(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	rr := postApprove(t, h, "Bearer tok", `{"email":"not-an-email"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed email, got %d", rr.Code)
	}
	// Nothing should be added or sent.
	ok, _ := al.IsAllowed(context.Background(), "not-an-email")
	if ok {
		t.Fatal("allowlist must not be modified on 400")
	}
}

// TestApproveSignup_ReApproveIdempotent checks that approving the same email
// twice returns 200 on both calls and leaves exactly one allowlist row.
func TestApproveSignup_ReApproveIdempotent(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()
	h := newTestApproveHandler(al, em, "tok")

	for i := 0; i < 2; i++ {
		rr := postApprove(t, h, "Bearer tok", `{"email":"repeat@example.com"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	ok, err := al.IsAllowed(context.Background(), "repeat@example.com")
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if !ok {
		t.Fatal("allowlist does not contain the email after two approvals")
	}
}

// TestApproveSignup_SendApprovedFailure500 checks that when SendApproved
// returns an error the handler returns 500 with a generic message that
// contains neither the email nor any secret. The allowlist Add already ran
// (idempotent), so the operator can retry to re-send.
func TestApproveSignup_SendApprovedFailure500(t *testing.T) {
	al := NewMemAllowlist(nil)
	h := newTestApproveHandler(al, errSendApproved{}, "tok")

	rr := postApprove(t, h, "Bearer tok", `{"email":"err@example.com"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on SendApproved failure, got %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "err@example.com") {
		t.Fatalf("500 response must not contain the email address: %q", body)
	}
	if strings.Contains(body, "tok") {
		t.Fatalf("500 response must not contain the bearer token: %q", body)
	}

	// The allowlist row must have been added before the send failure.
	ok, err := al.IsAllowed(context.Background(), "err@example.com")
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if !ok {
		t.Fatal("allowlist must contain the row even when SendApproved fails")
	}
}

// --- ApproveWaitlistEntry (the exported helper the console instance-admin
// waitlist adapter reuses, so an admin approving a waitlist entry produces
// the SAME allowlist row and email as POST /internal/approve-signup) ---

// TestApproveWaitlistEntry_SuccessAddsAllowlistAndSendsEmail mirrors the HTTP
// handler's own happy-path test, calling the exported function directly.
func TestApproveWaitlistEntry_SuccessAddsAllowlistAndSendsEmail(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()

	canonical, err := ApproveWaitlistEntry(context.Background(), al, em, "User+tag@Example.com", "approved via admin", fixedNow())
	if err != nil {
		t.Fatalf("ApproveWaitlistEntry: %v", err)
	}
	if canonical != "user@example.com" {
		t.Fatalf("canonical = %q, want user@example.com", canonical)
	}
	if ok, _ := al.IsAllowed(context.Background(), "user@example.com"); !ok {
		t.Fatal("allowlist does not contain the approved canonical email")
	}
	if !em.Approved("user+tag@example.com") {
		t.Fatal("SendApproved was not called with the delivery-form email")
	}
}

// TestApproveWaitlistEntry_InvalidEmail asserts a malformed email returns
// ErrInvalidEmail and touches neither the allowlist nor the email sender.
func TestApproveWaitlistEntry_InvalidEmail(t *testing.T) {
	al := NewMemAllowlist(nil)
	em := NewFakeEmailSender()

	_, err := ApproveWaitlistEntry(context.Background(), al, em, "not-an-email", "", fixedNow())
	if !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("err = %v, want ErrInvalidEmail", err)
	}
	if ok, _ := al.IsAllowed(context.Background(), "not-an-email"); ok {
		t.Fatal("allowlist must not be modified on an invalid email")
	}
}

// TestApproveWaitlistEntry_SendFailureLeavesAllowlistRow asserts a
// SendApproved failure surfaces its own error (not ErrInvalidEmail, not the
// allowlist-add sentinel) while the allowlist row still lands (idempotent
// retry-safe), matching the HTTP handler's own 500 behavior.
func TestApproveWaitlistEntry_SendFailureLeavesAllowlistRow(t *testing.T) {
	al := NewMemAllowlist(nil)

	_, err := ApproveWaitlistEntry(context.Background(), al, errSendApproved{}, "err@example.com", "", fixedNow())
	if err == nil {
		t.Fatal("expected an error when SendApproved fails")
	}
	if errors.Is(err, ErrInvalidEmail) || errors.Is(err, errApproveAllowlistAdd) {
		t.Fatalf("err = %v, want a bare send-failure error", err)
	}
	if ok, _ := al.IsAllowed(context.Background(), "err@example.com"); !ok {
		t.Fatal("allowlist must contain the row even when SendApproved fails")
	}
}
