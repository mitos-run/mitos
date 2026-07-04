package billingprovider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/quota"
)

// recordingSuspender records billing.Suspender calls for the webhook tests.
type recordingSuspender struct {
	calls []suspendCall
	err   error
}

type suspendCall struct {
	orgID, reason, note string
	manualHold          bool
}

func (s *recordingSuspender) Suspend(_ context.Context, orgID, reason, note string, manualHold bool) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, suspendCall{orgID, reason, note, manualHold})
	return nil
}

// TestWebhookSuspendedStatusFiresSuspender asserts a provider event carrying
// the suspended status (subscription canceled/paused, payment retries
// exhausted) drives the kill-switch through the Suspender seam on the
// transition INTO suspended, so the org fails closed at the gateway instead of
// only its billing status flipping (issue #615: previously the provider
// webhook set the status and no suspension reached any store).
func TestWebhookSuspendedStatusFiresSuspender(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)

	w := post(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(sus.calls) != 1 {
		t.Fatalf("suspender calls = %d, want 1", len(sus.calls))
	}
	call := sus.calls[0]
	if call.orgID != "org-alice" || call.reason != "dunning" || call.manualHold {
		t.Errorf("suspend call = %+v, want org-alice/dunning without a manual hold", call)
	}
	got, _ := status.Status(ctx, "org-alice")
	if got != billing.StatusSuspended {
		t.Errorf("billing status = %q, want suspended", got)
	}

	// Redelivery of the same event: the org is ALREADY suspended, so the
	// transition gate keeps the suspender quiet (the suspension time in the
	// store must not be churned by webhook retries).
	w = post(h)
	if w.Code != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", w.Code)
	}
	if len(sus.calls) != 1 {
		t.Errorf("suspender calls after redelivery = %d, want still 1 (transition-gated)", len(sus.calls))
	}
}

// TestWebhookSuspendFailureIsRetried asserts a suspender write failure is a
// 5xx BEFORE the status is applied: the provider retries the event, so a
// transient kill-switch store outage can neither drop the suspension nor leave
// the billing status claiming suspended while the gateway still admits the org.
func TestWebhookSuspendFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{err: errors.New("suspension store unavailable")}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)

	w := post(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so the provider retries", w.Code)
	}
	got, _ := status.Status(ctx, "org-alice")
	if got == billing.StatusSuspended {
		t.Error("billing status flipped to suspended although the kill-switch write failed; the retry would then be transition-gated away")
	}
}

// realSuspender builds the production adapter (quota.BillingSuspender over a
// mem suspension store) so the lift tests exercise the REAL reason-scoped lift
// semantics, not a fake's.
func realSuspender() (*quota.BillingSuspender, *quota.MemSuspensionStore) {
	store := quota.NewMemSuspensionStore()
	return quota.NewBillingSuspender(quota.NewKillSwitch(store, nil)), store
}

// TestWebhookTopUpLiftsSpendCapSuspension asserts the recovery half of the
// spend-cap loop: a cleared paid top-up lifts the org's spend_cap suspension
// (the org paid; the drawdown window resets at the payment) and restores the
// billing status from suspended to active, so the org is admitted at the
// gateway again without operator action.
func TestWebhookTopUpLiftsSpendCapSuspension(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	if err := status.SetStatus(ctx, "org-alice", billing.StatusSuspended); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	sus, store := realSuspender()
	if err := store.Suspend(ctx, quota.Suspension{OrgID: "org-alice", Reason: quota.ReasonSpendCap, Note: "hard cap"}); err != nil {
		t.Fatalf("seed suspension: %v", err)
	}
	ledger := billing.NewMemCreditLedger()
	h := NewWebhookHandler(
		fakeProvider{ev: Event{CustomerRef: "cus_alice", TopUp: &TopUpCredit{OrgID: "org-alice", AmountCents: 1000, Ref: "txn-1"}}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, ledger, fixedNow()).WithSuspender(sus)

	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-alice"); suspended {
		t.Error("spend-cap suspension not lifted by the paid top-up")
	}
	got, _ := status.Status(ctx, "org-alice")
	if got != billing.StatusActive {
		t.Errorf("billing status after top-up lift = %q, want active", got)
	}
	if bal, _ := ledger.Balance(ctx, "org-alice"); bal != 1000 {
		t.Errorf("balance = %d, want 1000 (the credit itself must still land)", bal)
	}
}

// TestWebhookActiveStatusLiftsDunningSuspension asserts payment recovery: a
// provider event whose normalized status transitions the org from suspended
// back to active lifts a dunning suspension, so a paid-up org is admitted
// again without operator action.
func TestWebhookActiveStatusLiftsDunningSuspension(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	if err := status.SetStatus(ctx, "org-alice", billing.StatusSuspended); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	sus, store := realSuspender()
	if err := store.Suspend(ctx, quota.Suspension{OrgID: "org-alice", Reason: quota.ReasonDunning, Note: "retries exhausted"}); err != nil {
		t.Fatalf("seed suspension: %v", err)
	}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusActive, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)

	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-alice"); suspended {
		t.Error("dunning suspension not lifted on the active-status transition")
	}
	got, _ := status.Status(ctx, "org-alice")
	if got != billing.StatusActive {
		t.Errorf("billing status = %q, want active", got)
	}
}

// TestWebhookLiftNeverTouchesManualHold asserts a manual-review hold survives
// BOTH automated recovery paths: neither a top-up nor an active-status
// transition lifts a held suspension; only the operator hook does.
func TestWebhookLiftNeverTouchesManualHold(t *testing.T) {
	ctx := context.Background()
	sus, store := realSuspender()
	if err := store.Suspend(ctx, quota.Suspension{OrgID: "org-alice", Reason: quota.ReasonSpendCap, Note: "operator hold", ManualHold: true}); err != nil {
		t.Fatalf("seed held spend-cap suspension: %v", err)
	}

	// Top-up path: held spend_cap suspension survives; the credit still lands
	// and the webhook still acks (a held org paying is not an error).
	status := billing.NewMemStatusStore()
	ledger := billing.NewMemCreditLedger()
	h := NewWebhookHandler(
		fakeProvider{ev: Event{CustomerRef: "cus_alice", TopUp: &TopUpCredit{OrgID: "org-alice", AmountCents: 500, Ref: "txn-2"}}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, ledger, fixedNow()).WithSuspender(sus)
	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("top-up status = %d, want 200", w.Code)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-alice"); !suspended {
		t.Fatal("a manual-hold suspension was lifted by a top-up")
	}

	// Active-status path: a held dunning suspension survives too.
	if err := store.Suspend(ctx, quota.Suspension{OrgID: "org-bob", Reason: quota.ReasonDunning, Note: "held for review", ManualHold: true}); err != nil {
		t.Fatalf("seed held dunning suspension: %v", err)
	}
	status2 := billing.NewMemStatusStore()
	if err := status2.SetStatus(ctx, "org-bob", billing.StatusSuspended); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	h2 := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusActive, CustomerRef: "cus_bob"}},
		fakeCustomers{"cus_bob": "org-bob"}, nil, status2, nil, nil).WithSuspender(sus)
	if w := post(h2); w.Code != http.StatusOK {
		t.Fatalf("active status = %d, want 200", w.Code)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-bob"); !suspended {
		t.Fatal("a manual-hold suspension was lifted by an active-status transition")
	}
}

// TestWebhookNonSuspendedStatusSkipsSuspender asserts a non-suspended status
// (past_due, active) never touches the suspender: dunning grace and recovery
// are status-only transitions.
func TestWebhookNonSuspendedStatusSkipsSuspender(t *testing.T) {
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusPastDue, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)
	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sus.calls) != 0 {
		t.Errorf("suspender fired for a past_due event: %+v", sus.calls)
	}
}
