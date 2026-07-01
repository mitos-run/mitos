package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// TestSetSpendCapPersistsCap asserts that a caller with billing.manage (org
// owner) can POST a spend cap and that a subsequent billing view reflects the
// new caps. This is the happy-path for the spend-cap guardrail.
func TestSetSpendCapPersistsCap(t *testing.T) {
	f := newFixture(t)
	body := `{"soft_cents":2000,"hard_cents":5000}`
	w := f.req(t, "POST", "/console/billing/spend-cap", body, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// Subsequent billing view must reflect the new caps (persistence check).
	bw := f.req(t, "GET", "/console/billing", "", f.aliceAcct, f.aliceOrg)
	var v BillingView
	decode(t, bw, &v)
	if v.SoftCapCents != 2000 {
		t.Errorf("soft_cap_cents = %d, want 2000", v.SoftCapCents)
	}
	if v.HardCapCents != 5000 {
		t.Errorf("hard_cap_cents = %d, want 5000", v.HardCapCents)
	}
}

// TestSetSpendCapForbiddenWithoutBillingManage asserts that a member without
// billing.manage gets 403. Viewers have only PermReadOnly; they cannot alter
// spend caps even from within the org.
func TestSetSpendCapForbiddenWithoutBillingManage(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)

	// owner signs up and gets RoleOwner in their org.
	_, org, err := accounts.SignUp(ctx, "cap-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}
	// viewer signs up and is seeded into the owner's org as RoleViewer.
	viewer, _, err := accounts.SignUp(ctx, "cap-viewer@example.com")
	if err != nil {
		t.Fatalf("SignUp viewer: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: viewer.ID,
		OrgID:     org.ID,
		Role:      saas.RoleViewer,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}

	con := New(Deps{
		Accounts: accounts,
		Billing:  BillingReader{Caps: billing.NewMemSpendCapStore(), Rates: billing.DefaultRates()},
	})

	r := httptest.NewRequest("POST", "/console/billing/spend-cap", strings.NewReader(`{"soft_cents":1000,"hard_cents":2000}`))
	r = r.WithContext(WithCaller(r.Context(), viewer.ID, org.ID))
	w := httptest.NewRecorder()
	con.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403 (billing.manage required)", w.Code)
	}
}

// TestSetSpendCapRejectsNegativeSoft asserts that a negative soft_cents is 400.
func TestSetSpendCapRejectsNegativeSoft(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/billing/spend-cap", `{"soft_cents":-1,"hard_cents":100}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for negative soft_cents", w.Code)
	}
}

// TestSetSpendCapRejectsNegativeHard asserts that a negative hard_cents is 400.
func TestSetSpendCapRejectsNegativeHard(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/billing/spend-cap", `{"soft_cents":0,"hard_cents":-1}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for negative hard_cents", w.Code)
	}
}

// TestSetSpendCapRejectsSoftExceedsHard asserts that soft > hard (when both are
// positive) is 400. A zero value means "not set" and is exempt from the ordering
// check.
func TestSetSpendCapRejectsSoftExceedsHard(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/billing/spend-cap", `{"soft_cents":5000,"hard_cents":2000}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for soft_cents > hard_cents", w.Code)
	}
}

// TestSetSpendCapAllowsZeroSoftOrHard asserts that a zero value for soft or
// hard is accepted (zero means "not set" for that threshold).
func TestSetSpendCapAllowsZeroSoftOrHard(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/billing/spend-cap", `{"soft_cents":0,"hard_cents":5000}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for zero soft (no soft alert)", w.Code)
	}
}
