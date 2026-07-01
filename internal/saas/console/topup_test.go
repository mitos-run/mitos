package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billingprovider"
)

// stubTopUp is a TopUpLinker that records the TopUp it received and returns a
// fixed URL for a known org, or ErrNotFound for any other org.
type stubTopUp struct {
	wantOrg string
	url     string
	got     billingprovider.TopUp
}

func (s *stubTopUp) CheckoutURL(_ context.Context, in billingprovider.TopUp) (string, error) {
	s.got = in
	if in.OrgID != s.wantOrg {
		return "", ErrNotFound
	}
	return s.url, nil
}

const (
	testTopUpProductID = "pro_topup_test"
	testTopUpCurrency  = "EUR"
	testTopUpURL       = "https://checkout.paddle.com/checkout/custom/abc"
)

// TestTopUpReturnsCheckoutURL asserts that a billing.manage caller GETs the
// hosted checkout URL and that the handler passes AmountCents, ProductID,
// Currency, and OrgID to the linker correctly.
func TestTopUpReturnsCheckoutURL(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{
		Accounts:       f.accounts,
		TopUp:          stub,
		TopUpProductID: testTopUpProductID,
		TopUpCurrency:  testTopUpCurrency,
	})
	w := f.req(t, "GET", "/console/billing/topup?amount=2500", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	decode(t, w, &resp)
	if resp.URL != testTopUpURL {
		t.Fatalf("url = %q, want the checkout URL", resp.URL)
	}
	if stub.got.AmountCents != 2500 {
		t.Errorf("AmountCents = %d, want 2500", stub.got.AmountCents)
	}
	if stub.got.ProductID != testTopUpProductID {
		t.Errorf("ProductID = %q, want %q", stub.got.ProductID, testTopUpProductID)
	}
	if stub.got.Currency != testTopUpCurrency {
		t.Errorf("Currency = %q, want %q", stub.got.Currency, testTopUpCurrency)
	}
	if stub.got.OrgID != f.aliceOrg {
		t.Errorf("OrgID = %q, want aliceOrg", stub.got.OrgID)
	}
}

// TestTopUpAmountZeroIs400 asserts that amount=0 is rejected with 400.
func TestTopUpAmountZeroIs400(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=0", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("amount=0 status = %d, want 400", w.Code)
	}
}

// TestTopUpAmountNegativeIs400 asserts that a negative amount is rejected.
func TestTopUpAmountNegativeIs400(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=-1", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("amount=-1 status = %d, want 400", w.Code)
	}
}

// TestTopUpAmountNonNumericIs400 asserts that a non-numeric amount is rejected.
func TestTopUpAmountNonNumericIs400(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=abc", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("amount=abc status = %d, want 400", w.Code)
	}
}

// TestTopUpAmountOverCeilingIs400 asserts that an amount above the ceiling
// (1_000_000 cents) is rejected.
func TestTopUpAmountOverCeilingIs400(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=1000001", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("amount=1000001 status = %d, want 400 (over ceiling)", w.Code)
	}
}

// TestTopUpAtCeilingIs200 asserts that the ceiling amount (1_000_000 cents)
// itself is accepted.
func TestTopUpAtCeilingIs200(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=1000000", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("amount=1000000 status = %d, want 200 (at ceiling is valid)", w.Code)
	}
}

// TestTopUpEmptyCheckoutURLIs500 asserts that a provider returning an empty url
// with no error is treated as a failure (mirrors the portal handler guard),
// never surfaced to the client as a 200 with a blank url.
func TestTopUpEmptyCheckoutURLIs500(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: ""}
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpProductID: testTopUpProductID, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=2500", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("empty checkout url status = %d, want 500", w.Code)
	}
}

// TestTopUpEmptyProductIDIs400 asserts that when the top-up product id is not
// configured, the endpoint returns 400 rather than attempting a checkout.
func TestTopUpEmptyProductIDIs400(t *testing.T) {
	f := newFixture(t)
	stub := &stubTopUp{wantOrg: f.aliceOrg, url: testTopUpURL}
	// TopUpProductID is deliberately omitted (empty = not configured).
	f.con = New(Deps{Accounts: f.accounts, TopUp: stub, TopUpCurrency: testTopUpCurrency})
	w := f.req(t, "GET", "/console/billing/topup?amount=2500", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty product id status = %d, want 400", w.Code)
	}
}

// TestTopUpForbiddenWithoutBillingManage asserts that a member without
// billing.manage (RoleViewer) is refused with 403.
func TestTopUpForbiddenWithoutBillingManage(t *testing.T) {
	ctx := context.Background()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)

	_, org, err := accounts.SignUp(ctx, "topup-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}
	viewer, _, err := accounts.SignUp(ctx, "topup-viewer@example.com")
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

	stub := &stubTopUp{wantOrg: org.ID, url: testTopUpURL}
	con := New(Deps{
		Accounts:       accounts,
		TopUp:          stub,
		TopUpProductID: testTopUpProductID,
		TopUpCurrency:  testTopUpCurrency,
	})

	r := httptest.NewRequest("GET", "/console/billing/topup?amount=2500", nil)
	r = r.WithContext(WithCaller(r.Context(), viewer.ID, org.ID))
	w := httptest.NewRecorder()
	con.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403 (billing.manage required)", w.Code)
	}
}

// TestTopUpNoCustomerMappingIs404 asserts that when the org has no billing
// customer linked, the endpoint returns 404 mirroring portal.go behavior.
func TestTopUpNoCustomerMappingIs404(t *testing.T) {
	f := newFixture(t)
	// stub returns ErrNotFound for aliceOrg because wantOrg does not match.
	stub := &stubTopUp{wantOrg: "different-org", url: testTopUpURL}
	f.con = New(Deps{
		Accounts:       f.accounts,
		TopUp:          stub,
		TopUpProductID: testTopUpProductID,
		TopUpCurrency:  testTopUpCurrency,
	})
	w := f.req(t, "GET", "/console/billing/topup?amount=2500", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no customer status = %d, want 404", w.Code)
	}
}
