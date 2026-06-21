package billing

import "testing"

// TestDunningTransitions exhaustively asserts the pure dunning state machine:
// payment success recovers, the first failure enters past_due, further failures
// stay past_due, retries-exhausted and explicit suspend close the org, and a
// suspended org recovers on a successful payment.
func TestDunningTransitions(t *testing.T) {
	cases := []struct {
		from BillingStatus
		ev   DunningEvent
		want BillingStatus
	}{
		{StatusActive, EventPaymentSucceeded, StatusActive},
		{StatusActive, EventPaymentFailed, StatusPastDue},
		{StatusActive, EventSuspend, StatusSuspended},
		{StatusPastDue, EventPaymentSucceeded, StatusActive},
		{StatusPastDue, EventPaymentFailed, StatusPastDue},
		{StatusPastDue, EventRetriesExhausted, StatusSuspended},
		{StatusPastDue, EventSuspend, StatusSuspended},
		{StatusSuspended, EventPaymentSucceeded, StatusActive},
		{StatusSuspended, EventPaymentFailed, StatusSuspended},
		{StatusSuspended, EventRetriesExhausted, StatusSuspended},
	}
	for _, c := range cases {
		if got := NextStatus(c.from, c.ev); got != c.want {
			t.Errorf("NextStatus(%s, %s) = %s, want %s", c.from, c.ev, got, c.want)
		}
	}
}

// TestApplyDunningDrivesKillSwitchOnSuspend asserts the service applies the pure
// transition AND drives the #213 kill-switch when, and only when, the org
// transitions INTO suspended.
func TestApplyDunningDrivesKillSwitchOnSuspend(t *testing.T) {
	ctx := t.Context()
	sus := &recordingSuspender{}
	svc := NewService(Config{Stripe: NewFakeStripe(), Suspend: sus, Now: fixedNow})

	// A failure from active -> past_due does NOT suspend.
	st, err := svc.applyDunning(ctx, "org1", EventPaymentFailed)
	if err != nil {
		t.Fatalf("applyDunning failed: %v", err)
	}
	if st != StatusPastDue {
		t.Fatalf("status = %s, want past_due", st)
	}
	if len(sus.calls) != 0 {
		t.Errorf("kill-switch fired on past_due (should not)")
	}

	// Retries exhausted -> suspended DOES drive the kill-switch once.
	st, err = svc.applyDunning(ctx, "org1", EventRetriesExhausted)
	if err != nil {
		t.Fatalf("applyDunning exhausted: %v", err)
	}
	if st != StatusSuspended {
		t.Fatalf("status = %s, want suspended", st)
	}
	if len(sus.calls) != 1 || sus.calls[0].orgID != "org1" || sus.calls[0].reason != "dunning" {
		t.Fatalf("kill-switch calls = %+v, want one dunning suspend for org1", sus.calls)
	}

	// A recovery payment moves back to active without firing suspend again.
	st, err = svc.applyDunning(ctx, "org1", EventPaymentSucceeded)
	if err != nil {
		t.Fatalf("applyDunning recover: %v", err)
	}
	if st != StatusActive {
		t.Errorf("status after recovery = %s, want active", st)
	}
	if len(sus.calls) != 1 {
		t.Errorf("kill-switch fired again on recovery: %+v", sus.calls)
	}
}
