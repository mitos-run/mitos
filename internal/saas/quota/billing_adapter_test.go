package quota

import (
	"context"
	"testing"
)

// TestBillingSuspenderDrivesKillSwitch asserts the billing.Suspender adapter
// (issue #212) suspends an org through the real KillSwitch, mapping the
// spend_cap reason and carrying the manual hold, so a breached hard spend cap
// fails the org closed exactly like the abuse controls do.
func TestBillingSuspenderDrivesKillSwitch(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	ks := NewKillSwitch(store, nil)
	bs := NewBillingSuspender(ks)

	if err := bs.Suspend(ctx, "org1", "spend_cap", "hard cap reached", true); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sus, ok, err := store.IsSuspended(ctx, "org1")
	if err != nil || !ok {
		t.Fatalf("org not suspended: ok=%v err=%v", ok, err)
	}
	if sus.Reason != ReasonSpendCap {
		t.Errorf("reason = %q, want %q", sus.Reason, ReasonSpendCap)
	}
	if !sus.ManualHold {
		t.Error("spend-cap suspension must carry a manual hold")
	}
}

// TestBillingSuspenderUnknownReasonFallsBack asserts an unrecognized reason
// maps to ReasonManual so a suspension is never silently dropped.
func TestBillingSuspenderUnknownReasonFallsBack(t *testing.T) {
	ctx := context.Background()
	store := NewMemSuspensionStore()
	bs := NewBillingSuspender(NewKillSwitch(store, nil))
	if err := bs.Suspend(ctx, "org2", "mystery", "n", false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sus, _, _ := store.IsSuspended(ctx, "org2")
	if sus.Reason != ReasonManual {
		t.Errorf("reason = %q, want manual fallback", sus.Reason)
	}
}
