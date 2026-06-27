package onboarding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// harness wires the onboarding service over in-memory dependencies with a
// deterministic clock and id generator, returning the pieces a test asserts on.
type harness struct {
	svc    *Service
	store  *saas.MemStore
	ledger *billing.MemCreditLedger
	email  *FakeEmailSender
	events *MemEventRecorder
	now    *time.Time
}

func newHarness(t *testing.T, mode Mode) *harness {
	t.Helper()
	store := saas.NewMemStore()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var n int
	idgen := func() string {
		n++
		return "id-" + string(rune('a'+n))
	}

	keys := saas.NewKeyService(store, saas.WithClock(clock), saas.WithIDGen(idgen))
	accounts := saas.NewAccountService(store, keys, saas.WithClock(clock), saas.WithIDGen(idgen))
	ledger := billing.NewMemCreditLedger()
	email := NewFakeEmailSender()
	events := NewMemEventRecorder()

	tok := 0
	tokengen := func() (string, error) {
		tok++
		return "tok-" + string(rune('0'+tok)), nil
	}

	svc := NewService(accounts, store, NewMemPendingStore(), ledger, email,
		WithMode(mode),
		WithClock(clock),
		WithIDGen(idgen),
		WithTokenGen(tokengen),
		WithEventRecorder(events),
	)
	return &harness{svc: svc, store: store, ledger: ledger, email: email, events: events, now: &now}
}

func TestSignupVerifyProvisionsOrgCreditAndKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	res, err := h.svc.SignUp(ctx, "dev@example.com", "")
	if err != nil {
		t.Fatalf("sign up: %v", err)
	}
	if res.Waitlisted {
		t.Fatal("open mode must not waitlist")
	}
	if res.PendingID == "" {
		t.Fatal("open-mode signup must return a pending id")
	}

	token := h.email.LastToken("dev@example.com")
	if token == "" {
		t.Fatal("verification email token was not sent")
	}

	vr, err := h.svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Account + Personal org created.
	if vr.Account.Email != "dev@example.com" {
		t.Fatalf("account email = %q", vr.Account.Email)
	}
	if !vr.Org.Personal {
		t.Fatal("verify must auto-create a Personal org")
	}
	if vr.Account.PersonalOrgID != vr.Org.ID {
		t.Fatal("account must point at its personal org")
	}

	// First key issued, raw shown exactly once and scoped to sandboxes.
	if vr.FirstKey.RawKey == "" {
		t.Fatal("first key raw value must be returned once")
	}
	if !vr.FirstKey.Record.HasScope(saas.ScopeSandboxes) {
		t.Fatal("first key must carry the sandboxes scope")
	}

	// Credit landed on the org and matches the default signup credit.
	bal, err := h.ledger.Balance(ctx, vr.Org.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != billing.DefaultSignupCredit() {
		t.Fatalf("balance = %v, want %v", bal, billing.DefaultSignupCredit())
	}
	if vr.GrantedCredit != billing.DefaultSignupCredit() {
		t.Fatalf("granted credit = %v", vr.GrantedCredit)
	}

	// Funnel events recorded: signup_started, verified, key_issued.
	got := map[EventName]bool{}
	for _, e := range h.events.Events(ctx) {
		got[e.Name] = true
	}
	for _, want := range []EventName{EventSignupStarted, EventVerified, EventKeyIssued} {
		if !got[want] {
			t.Fatalf("missing funnel event %q", want)
		}
	}
}

func TestVerifyRejectsInvalidToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)
	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	_, err := h.svc.Verify(ctx, "tok-not-a-real-token")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)
	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	token := h.email.LastToken("dev@example.com")

	// Advance the clock past the 24h default TTL.
	*h.now = h.now.Add(25 * time.Hour)
	_, err := h.svc.Verify(ctx, token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

func TestReVerifyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)
	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	token := h.email.LastToken("dev@example.com")

	first, err := h.svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	second, err := h.svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if !second.AlreadyDone {
		t.Fatal("re-verify must be flagged AlreadyDone")
	}
	if second.Account.ID != first.Account.ID {
		t.Fatal("re-verify must return the same account")
	}
	if second.FirstKey.RawKey != "" {
		t.Fatal("re-verify must not issue a second key")
	}

	// Credit must have landed exactly once.
	bal, err := h.ledger.Balance(ctx, first.Org.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != billing.DefaultSignupCredit() {
		t.Fatalf("credit landed more than once: balance = %v", bal)
	}
	entries, _ := h.ledger.Entries(ctx, first.Org.ID)
	signupGrants := 0
	for _, e := range entries {
		if e.Kind == billing.KindSignupCredit {
			signupGrants++
		}
	}
	if signupGrants != 1 {
		t.Fatalf("signup credit granted %d times, want 1", signupGrants)
	}
}

func TestWaitlistModeRecordsEntryAndDoesNotProvision(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeWaitlist)

	res, err := h.svc.SignUp(ctx, "dev@example.com", "")
	if err != nil {
		t.Fatalf("sign up: %v", err)
	}
	if !res.Waitlisted {
		t.Fatal("waitlist mode must waitlist the signup")
	}

	// No account was provisioned.
	if _, err := h.store.GetAccountByEmail(ctx, "dev@example.com"); !errors.Is(err, saas.ErrNotFound) {
		t.Fatalf("waitlist mode must not provision an account, got %v", err)
	}

	// No verify email was sent.
	if h.email.LastToken("dev@example.com") != "" {
		t.Fatal("waitlist mode must not send a verify email")
	}

	// The waitlist entry was recorded.
	wl, err := h.svc.JoinWaitlist(ctx)
	if err != nil {
		t.Fatalf("waitlist: %v", err)
	}
	if len(wl) != 1 || wl[0].Email != "dev@example.com" {
		t.Fatalf("waitlist = %+v", wl)
	}

	// Verify is disabled in waitlist mode.
	if _, err := h.svc.Verify(ctx, "anything"); !errors.Is(err, ErrWaitlistMode) {
		t.Fatalf("verify in waitlist mode: %v, want ErrWaitlistMode", err)
	}
}

func TestOpenModeRejectsDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)
	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	token := h.email.LastToken("dev@example.com")
	if _, err := h.svc.Verify(ctx, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// A second signup for the now-provisioned email is a conflict.
	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); !errors.Is(err, saas.ErrConflict) {
		t.Fatalf("duplicate signup: %v, want ErrConflict", err)
	}
}

func TestVerifyErrorsNeverLeakTokenOrEmail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)
	if _, err := h.svc.SignUp(ctx, "secret@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	_, err := h.svc.Verify(ctx, "tok-leaky-raw-token")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "tok-leaky-raw-token") {
		t.Fatal("error leaked the raw token")
	}
	if strings.Contains(err.Error(), "secret@example.com") {
		t.Fatal("error leaked the email")
	}
}

func TestCustomSignupCreditAmount(t *testing.T) {
	ctx := context.Background()
	store := saas.NewMemStore()
	clock := func() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }
	keys := saas.NewKeyService(store, saas.WithClock(clock))
	accounts := saas.NewAccountService(store, keys, saas.WithClock(clock))
	ledger := billing.NewMemCreditLedger()
	email := NewFakeEmailSender()

	svc := NewService(accounts, store, NewMemPendingStore(), ledger, email,
		WithMode(ModeOpen), WithClock(clock), WithSignupCredit(billing.USD(200)))

	if _, err := svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	token := email.LastToken("dev@example.com")
	vr, err := svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	bal, _ := ledger.Balance(ctx, vr.Org.ID)
	if bal != billing.USD(200) {
		t.Fatalf("balance = %v, want $200", bal)
	}
}

func TestDefaultModeIsWaitlist(t *testing.T) {
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	svc := NewService(accounts, store, NewMemPendingStore(), billing.NewMemCreditLedger(), NewFakeEmailSender())
	if svc.Mode() != ModeWaitlist {
		t.Fatalf("default mode = %q, want waitlist (the #208 gate)", svc.Mode())
	}
}

// TestSignUpPersistsUseCase asserts that a valid use-case slug passed to SignUp
// is stored on the pending signup and surfaced in VerifyResult.UseCase.
func TestSignUpPersistsUseCase(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	_, err := h.svc.SignUp(ctx, "dev@example.com", "rollouts")
	if err != nil {
		t.Fatalf("sign up: %v", err)
	}

	token := h.email.LastToken("dev@example.com")
	if token == "" {
		t.Fatal("no verification token sent")
	}
	pending, err := h.svc.pending.GetPendingByTokenHash(ctx, hashString(token))
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.UseCase != "rollouts" {
		t.Fatalf("pending.UseCase = %q, want %q", pending.UseCase, "rollouts")
	}

	vr, err := h.svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vr.UseCase != "rollouts" {
		t.Fatalf("VerifyResult.UseCase = %q, want %q", vr.UseCase, "rollouts")
	}
}

// TestSignUpDropsInvalidUseCase asserts that an invalid use-case slug is silently
// replaced with "" rather than causing an error or being stored as-is.
func TestSignUpDropsInvalidUseCase(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	_, err := h.svc.SignUp(ctx, "dev@example.com", "INVALID UC!")
	if err != nil {
		t.Fatalf("sign up must not error on an invalid uc: %v", err)
	}

	token := h.email.LastToken("dev@example.com")
	if token == "" {
		t.Fatal("no verification token sent")
	}
	pending, err := h.svc.pending.GetPendingByTokenHash(ctx, hashString(token))
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.UseCase != "" {
		t.Fatalf("pending.UseCase = %q, want empty string for invalid input", pending.UseCase)
	}
}

// TestVerifyRecoversFromHalfProvisionedAccount proves Verify is idempotent even
// when a PRIOR verify attempt provisioned the account but crashed before marking
// the pending signup verified (e.g. the credit grant or key issue errored). The
// pending stays unverified and the email is taken, so a naive re-verify calls
// SignUp again and fails with a duplicate-email conflict, stranding the user. The
// fix loads the existing account on conflict and completes onboarding.
func TestVerifyRecoversFromHalfProvisionedAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	if _, err := h.svc.SignUp(ctx, "dev@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	token := h.email.LastToken("dev@example.com")
	if token == "" {
		t.Fatal("no verification token")
	}

	// Simulate the prior verify attempt provisioning the account (email now taken)
	// but never reaching MarkVerified: provision out-of-band over the same store.
	accts := saas.NewAccountService(h.store, saas.NewKeyService(h.store))
	if _, _, err := accts.SignUp(ctx, "dev@example.com"); err != nil {
		t.Fatalf("seed half-provisioned account: %v", err)
	}

	// Re-verify with the original token must COMPLETE onboarding, not conflict.
	res, err := h.svc.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify after half-provision must recover, got error: %v", err)
	}
	if res.Account.ID == "" || res.Org.ID == "" {
		t.Fatalf("verify must return account+org, got %+v", res)
	}
	if res.FirstKey.RawKey == "" {
		t.Fatalf("verify must issue a usable first key, got %+v", res.FirstKey)
	}
}
