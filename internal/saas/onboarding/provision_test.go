package onboarding

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// captureProvisioner records the org id and display name it was asked to
// provision, and can be made to fail to drive the error path.
type captureProvisioner struct {
	mu    sync.Mutex
	calls []struct{ orgID, displayName, region string }
	err   error
}

func (c *captureProvisioner) Provision(_ context.Context, orgID, displayName, region string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.calls = append(c.calls, struct{ orgID, displayName, region string }{orgID, displayName, region})
	return nil
}

func (c *captureProvisioner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// signUpAndVerify drives a full signup + verify and returns the verify result.
func signUpAndVerify(t *testing.T, h *harness, email string) VerifyResult {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.SignUp(ctx, email, ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tok := h.email.LastToken(email)
	if tok == "" {
		t.Fatal("no token captured")
	}
	res, err := h.svc.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return res
}

// TestVerifyProvisionsOrgCR asserts a verified signup calls the OrgProvisioner
// with the org id and display name so the OrgReconciler can stand up the
// per-org namespace.
func TestVerifyProvisionsOrgCR(t *testing.T) {
	h := newHarness(t, ModeOpen)
	prov := &captureProvisioner{}
	h.svc.provision = prov

	res := signUpAndVerify(t, h, "tenant@example.com")

	if prov.count() != 1 {
		t.Fatalf("provisioner called %d times, want 1", prov.count())
	}
	got := prov.calls[0]
	if got.orgID != res.Org.ID {
		t.Fatalf("provisioned org id %q, want %q", got.orgID, res.Org.ID)
	}
	if got.displayName != res.Org.Name {
		t.Fatalf("provisioned display name %q, want %q", got.displayName, res.Org.Name)
	}
}

// TestVerifyProvisionsOrgCRWithHomeRegion asserts that when the account
// service is configured with a placement registry default home region
// (issue #712 phase 0), that region flows through SignUp's Personal org and
// on into the OrgProvisioner.Provision call, so a self-host operator's Org
// reconciler can label the tenant namespace's Org CR with it.
func TestVerifyProvisionsOrgCRWithHomeRegion(t *testing.T) {
	store := saas.NewMemStore()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	keys := saas.NewKeyService(store, saas.WithClock(clock))
	accounts := saas.NewAccountService(store, keys, saas.WithClock(clock), saas.WithHomeRegion("iad"))
	prov := &captureProvisioner{}
	tok := 0
	tokengen := func() (string, error) {
		tok++
		return "tok-" + string(rune('0'+tok)), nil
	}
	email := NewFakeEmailSender()
	svc := NewService(accounts, store, NewMemPendingStore(), billing.NewMemCreditLedger(), email,
		WithMode(ModeOpen),
		WithClock(clock),
		WithTokenGen(tokengen),
		WithOrgProvisioner(prov),
	)

	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "region@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tokVal := email.LastToken("region@example.com")
	if tokVal == "" {
		t.Fatal("no token captured")
	}
	res, err := svc.Verify(ctx, tokVal)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Org.HomeRegion != "iad" {
		t.Fatalf("org.HomeRegion = %q, want iad", res.Org.HomeRegion)
	}
	if prov.count() != 1 {
		t.Fatalf("provisioner called %d times, want 1", prov.count())
	}
	if got := prov.calls[0].region; got != "iad" {
		t.Errorf("Provision region = %q, want iad", got)
	}
}

// TestVerifyWithoutProvisionerSkipsWithWarning asserts that with no provisioner
// configured the verify still succeeds (account + org created in the store) and
// logs a warning instead of failing the signup.
func TestVerifyWithoutProvisionerSkipsWithWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := newHarness(t, ModeOpen)
	h.svc.provision = nil
	h.svc.logger = logger

	res := signUpAndVerify(t, h, "noprov@example.com")
	if res.Account.ID == "" || res.Org.ID == "" {
		t.Fatal("expected account and org to be provisioned in the store")
	}
	if !strings.Contains(buf.String(), "skipping tenant namespace provisioning") {
		t.Fatalf("expected skip warning in log, got %q", buf.String())
	}
}

// TestVerifyProvisionerErrorFailsVerify asserts a provisioner error fails the
// verify so the user can retry idempotently rather than landing in an org with
// no namespace.
func TestVerifyProvisionerErrorFailsVerify(t *testing.T) {
	h := newHarness(t, ModeOpen)
	h.svc.provision = &captureProvisioner{err: errors.New("apiserver down")}

	ctx := context.Background()
	if _, err := h.svc.SignUp(ctx, "fail@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tok := h.email.LastToken("fail@example.com")
	_, err := h.svc.Verify(ctx, tok)
	if err == nil {
		t.Fatal("expected verify to fail when the provisioner errors")
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatal("verify error leaked the raw token")
	}
}

// TestReVerifyDoesNotReprovision asserts the idempotent re-verify path (already
// verified) does not call the provisioner a second time: provisioning happened
// on the first verify and the OrgProvisioner itself is idempotent.
func TestReVerifyDoesNotReprovision(t *testing.T) {
	h := newHarness(t, ModeOpen)
	prov := &captureProvisioner{}
	h.svc.provision = prov

	ctx := context.Background()
	if _, err := h.svc.SignUp(ctx, "again@example.com", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tok := h.email.LastToken("again@example.com")
	if _, err := h.svc.Verify(ctx, tok); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := h.svc.Verify(ctx, tok); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if prov.count() != 1 {
		t.Fatalf("provisioner called %d times across two verifies, want 1", prov.count())
	}
}
