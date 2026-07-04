package quota

import (
	"context"
	"testing"
)

// suspendedOrg seeds the store with one suspension.
func suspendedOrg(t *testing.T, store SuspensionStore, orgID string, reason SuspensionReason, hold bool) {
	t.Helper()
	if err := store.Suspend(context.Background(), Suspension{OrgID: orgID, Reason: reason, Note: "test", ManualHold: hold}); err != nil {
		t.Fatalf("seed suspend: %v", err)
	}
}

// TestLiftReasonLiftsMatchingAutoSuspension asserts the reason-scoped auto-lift:
// a suspension with the matching reason and NO manual hold is lifted (the
// payment-driven recovery paths use this), and the org is admitted again.
func TestLiftReasonLiftsMatchingAutoSuspension(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	ks := NewKillSwitch(store, nil)
	suspendedOrg(t, store, "org-1", ReasonSpendCap, false)

	lifted, err := ks.LiftReason(ctx, "org-1", ReasonSpendCap)
	if err != nil || !lifted {
		t.Fatalf("LiftReason = %t, %v; want true, nil", lifted, err)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-1"); suspended {
		t.Error("org still suspended after a matching-reason lift")
	}
}

// TestLiftReasonIgnoresOtherReasons asserts a suspension under a DIFFERENT
// reason is never touched: a top-up must not lift an abuse or emergency-stop
// suspension.
func TestLiftReasonIgnoresOtherReasons(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	ks := NewKillSwitch(store, nil)
	suspendedOrg(t, store, "org-1", ReasonAbuseSignal, false)

	lifted, err := ks.LiftReason(ctx, "org-1", ReasonSpendCap)
	if err != nil || lifted {
		t.Fatalf("LiftReason across reasons = %t, %v; want false, nil", lifted, err)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-1"); !suspended {
		t.Error("an abuse suspension was lifted by a spend-cap recovery")
	}
}

// TestLiftReasonNeverLiftsManualHold asserts a manual-review hold survives the
// auto-lift even when the reason matches: only the explicit Lift (the human
// hook) clears a held suspension.
func TestLiftReasonNeverLiftsManualHold(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	ks := NewKillSwitch(store, nil)
	suspendedOrg(t, store, "org-1", ReasonSpendCap, true)

	lifted, err := ks.LiftReason(ctx, "org-1", ReasonSpendCap)
	if err != nil || lifted {
		t.Fatalf("LiftReason on a held suspension = %t, %v; want false, nil", lifted, err)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-1"); !suspended {
		t.Error("a manual-hold suspension was auto-lifted")
	}
	// The manual hook still works.
	if ok, err := ks.Lift(ctx, "org-1"); err != nil || !ok {
		t.Fatalf("manual Lift = %t, %v; want true, nil", ok, err)
	}
}

// TestLiftReasonNotSuspendedIsNoOp asserts lifting an unsuspended org reports
// false with no error (idempotent recovery paths call it blindly).
func TestLiftReasonNotSuspendedIsNoOp(t *testing.T) {
	ks := NewKillSwitch(NewMemSuspensionStore(), nil)
	lifted, err := ks.LiftReason(context.Background(), "org-none", ReasonDunning)
	if err != nil || lifted {
		t.Fatalf("LiftReason on unsuspended org = %t, %v; want false, nil", lifted, err)
	}
}

// TestBillingSuspenderLiftReason asserts the billing-facing adapter maps the
// billing reason strings to the quota reasons and never lifts on an unknown
// reason string.
func TestBillingSuspenderLiftReason(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	bs := NewBillingSuspender(NewKillSwitch(store, nil))
	suspendedOrg(t, store, "org-sc", ReasonSpendCap, false)
	suspendedOrg(t, store, "org-du", ReasonDunning, false)
	suspendedOrg(t, store, "org-man", ReasonManual, false)

	if lifted, err := bs.LiftReason(ctx, "org-sc", "spend_cap"); err != nil || !lifted {
		t.Errorf("spend_cap lift = %t, %v; want true, nil", lifted, err)
	}
	if lifted, err := bs.LiftReason(ctx, "org-du", "dunning"); err != nil || !lifted {
		t.Errorf("dunning lift = %t, %v; want true, nil", lifted, err)
	}
	// An unknown billing reason maps to no quota reason; nothing is lifted.
	if lifted, err := bs.LiftReason(ctx, "org-man", "mystery"); err != nil || lifted {
		t.Errorf("unknown-reason lift = %t, %v; want false, nil", lifted, err)
	}
	if _, suspended, _ := store.IsSuspended(ctx, "org-man"); !suspended {
		t.Error("an unknown reason string lifted a manual suspension")
	}
}
