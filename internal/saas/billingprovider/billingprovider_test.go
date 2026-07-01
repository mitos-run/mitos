package billingprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/billing"
)

// errLedger is a CreditLedger whose Append fails with a non-duplicate error. It
// exercises the webhook's 500 path: a transient ledger failure on a cleared
// payment must make the provider retry, never ack a credit that did not land.
type errLedger struct{}

func (errLedger) Append(context.Context, billing.LedgerEntry) error {
	return errors.New("ledger unavailable")
}
func (errLedger) Balance(context.Context, string) (billing.Money, error) { return 0, nil }
func (errLedger) Entries(context.Context, string) ([]billing.LedgerEntry, error) {
	return nil, nil
}

// handleTopUp builds a WebhookHandler with the given provider and ledger, POSTs
// an empty body, and returns the response recorder.
func handleTopUp(t *testing.T, p Provider, ledger billing.CreditLedger, now func() time.Time) *httptest.ResponseRecorder {
	t.Helper()
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(p, fakeCustomers{"cus_alice": "org-alice"}, status, ledger, now)
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func fixedNow() func() time.Time {
	t := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func topUpProvider(orgID string, amountCents int64, ref string) fakeProvider {
	return fakeProvider{
		ev: Event{
			Status:      billing.StatusActive,
			CustomerRef: "cus_alice",
			TopUp: &TopUpCredit{
				OrgID:       orgID,
				AmountCents: amountCents,
				Ref:         ref,
			},
		},
	}
}

// TestWebhookCreditsTopUp asserts a top-up event with a valid ledger credits the
// org's balance and returns 200.
func TestWebhookCreditsTopUp(t *testing.T) {
	ledger := billing.NewMemCreditLedger()
	w := handleTopUp(t, topUpProvider("org1", 5000, "txn_1"), ledger, fixedNow())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	bal, err := ledger.Balance(context.Background(), "org1")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != billing.Money(5000) {
		t.Fatalf("balance = %d, want 5000", bal)
	}
}

// TestWebhookTopUpIdempotent asserts re-delivering the same top-up event does
// not double-credit the org: the ledger's idempotency key guards it and the
// handler still returns 200.
func TestWebhookTopUpIdempotent(t *testing.T) {
	ledger := billing.NewMemCreditLedger()
	p := topUpProvider("org1", 5000, "txn_1")

	w1 := handleTopUp(t, p, ledger, fixedNow())
	if w1.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d, want 200", w1.Code)
	}
	w2 := handleTopUp(t, p, ledger, fixedNow())
	if w2.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d, want 200", w2.Code)
	}
	bal, err := ledger.Balance(context.Background(), "org1")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != billing.Money(5000) {
		t.Fatalf("balance after redelivery = %d, want 5000 (idempotent)", bal)
	}
}

// TestWebhookNilLedgerNoPanic asserts a nil ledger (community edition, no
// crediting) silently skips the credit and returns 200 without panicking. nil
// now is also passed to confirm that code path is safe.
func TestWebhookNilLedgerNoPanic(t *testing.T) {
	w := handleTopUp(t, topUpProvider("org1", 5000, "txn_1"), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestWebhookTopUpEmptyOrgSkipped asserts a top-up carrying an empty org id is
// skipped: no ledger entry, no error, 200.
func TestWebhookTopUpEmptyOrgSkipped(t *testing.T) {
	ledger := billing.NewMemCreditLedger()
	w := handleTopUp(t, topUpProvider("", 5000, "txn_2"), ledger, fixedNow())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	bal, err := ledger.Balance(context.Background(), "")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d after empty-org top-up, want 0", bal)
	}
}

// TestWebhookTopUpLedgerErrorIs500 asserts a non-duplicate ledger error on the
// credit path returns 500, so the provider retries the delivery rather than
// treating a failed credit as acknowledged. ErrDuplicateEntry stays 200 (see the
// idempotency test); any OTHER error must not be swallowed.
func TestWebhookTopUpLedgerErrorIs500(t *testing.T) {
	w := handleTopUp(t, topUpProvider("org1", 5000, "txn_err"), errLedger{}, fixedNow())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on ledger error", w.Code)
	}
}

// TestWebhookTopUpZeroAmountSkipped asserts a top-up with AmountCents=0 is
// skipped: no ledger entry, no error, 200.
func TestWebhookTopUpZeroAmountSkipped(t *testing.T) {
	ledger := billing.NewMemCreditLedger()
	w := handleTopUp(t, topUpProvider("org1", 0, "txn_3"), ledger, fixedNow())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	bal, err := ledger.Balance(context.Background(), "org1")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d after zero-amount top-up, want 0", bal)
	}
}
