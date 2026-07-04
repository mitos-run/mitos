package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/quota"
)

// TestNewBillingSuspenderMemFallback asserts the dev wiring (no Postgres pool)
// still returns a working suspender over an in-process store.
func TestNewBillingSuspenderMemFallback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	suspender, store := newBillingSuspender(nil, logger)
	if suspender == nil || store == nil {
		t.Fatal("newBillingSuspender(nil pool) must return a suspender and its store")
	}
	if err := suspender.Suspend(context.Background(), "org-dev", "dunning", "retries exhausted", false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sus, suspended, err := store.IsSuspended(context.Background(), "org-dev")
	if err != nil || !suspended {
		t.Fatalf("IsSuspended = %v, %t; want suspended", err, suspended)
	}
	if sus.Reason != quota.ReasonDunning {
		t.Errorf("reason = %q, want %q", sus.Reason, quota.ReasonDunning)
	}
}

// TestBillingServiceSuspendReachesSharedStore is the integration-shaped proof
// for the issue #615 residual wiring gap: a billing-driven suspension (here the
// hard spend cap, the runaway-agent backstop) travels through the
// billing.Suspender seam the console now wires and LANDS in the suspension
// store the gateway kill-switch reads. In production that store is the shared
// Postgres suspensions table (migration 0008), so the suspend takes effect at
// every gateway replica within the gateway's suspension-cache TTL (a few
// seconds); the mem store here stands in behind the same interface.
func TestBillingServiceSuspendReachesSharedStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	suspender, store := newBillingSuspender(nil, logger)

	caps := billing.NewMemSpendCapStore()
	svc := billing.NewService(billing.Config{
		Caps:    caps,
		Suspend: suspender,
	})
	ctx := context.Background()
	if err := svc.SetSpendCap(ctx, billing.SpendCap{OrgID: "org-hot", HardCap: 500}); err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}

	// Below the cap: no suspension.
	if suspended, err := svc.EnforceSpendCap(ctx, "org-hot", 499); err != nil || suspended {
		t.Fatalf("below-cap EnforceSpendCap = %t, %v; want false, nil", suspended, err)
	}
	if _, suspended, err := store.IsSuspended(ctx, "org-hot"); err != nil || suspended {
		t.Fatalf("below-cap store state = %t, %v; want not suspended", suspended, err)
	}

	// At the hard cap: the suspend must land in the SHARED store.
	suspended, err := svc.EnforceSpendCap(ctx, "org-hot", 500)
	if err != nil || !suspended {
		t.Fatalf("at-cap EnforceSpendCap = %t, %v; want true, nil", suspended, err)
	}
	sus, isSuspended, err := store.IsSuspended(ctx, "org-hot")
	if err != nil || !isSuspended {
		t.Fatalf("at-cap store state = %t, %v; want suspended", isSuspended, err)
	}
	if sus.Reason != quota.ReasonSpendCap {
		t.Errorf("reason = %q, want %q", sus.Reason, quota.ReasonSpendCap)
	}
	if sus.ManualHold {
		t.Error("an automated spend-cap suspension must NOT carry a manual hold (a paid top-up is its lift lever)")
	}
}
