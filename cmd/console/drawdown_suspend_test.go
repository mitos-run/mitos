package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

// TestDrawdownCycleSuspendsBreachedCapInSharedStore is the PRODUCTION-path
// acceptance for the issue #615 residual wiring gap: it drives runDrawdownOnce
// (the loop main actually starts) over a REAL billing.Service built exactly
// like cmd/console/main.go builds it, with the Suspender from
// newBillingSuspender, and asserts that settling usage that breaches the org's
// hard spend cap writes a suspension the SHARED suspension store reports via
// IsSuspended. In production that store is the Postgres suspensions table
// (migration 0008) the gateway kill-switch reads, so this is the console-side
// suspend becoming a gateway-side deny within the gateway's cache TTL; the mem
// store here stands in behind the same interface.
func TestDrawdownCycleSuspendsBreachedCapInSharedStore(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	suspender, susStore := newBillingSuspender(nil, logger)
	ledger := billing.NewMemCreditLedger()
	caps := billing.NewMemSpendCapStore()
	svc := billing.NewService(billing.Config{
		Ledger:  ledger,
		Caps:    caps,
		Suspend: suspender,
		// Rates default to billing.DefaultRates(), as in main.
	})

	// The org holds prepaid credit (so the drawdown actually debits) and a tiny
	// hard cap the settle will cross: 3600 vCPU-seconds at the default rate is
	// 4608 milli-cents, settling 5 cents against the 3-cent cap.
	if err := billing.TopUp(ctx, ledger, "org-hot", 1000, "topup-1", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	if err := svc.SetSpendCap(ctx, billing.SpendCap{OrgID: "org-hot", HardCap: 3}); err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}

	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-hot"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-hot": {{OrgID: "org-hot", SandboxID: "sb-1", Window: now.Add(-10 * time.Minute), VCPUSeconds: 3600}},
	}}

	stats := runDrawdownOnce(ctx, logger, orgs, store, svc, 2*time.Hour, now, nil)
	if stats.drawn != 1 || stats.failed != 0 {
		t.Fatalf("stats = %+v, want drawn=1 failed=0", stats)
	}
	if stats.suspended != 1 {
		t.Errorf("stats.suspended = %d, want 1", stats.suspended)
	}

	sus, suspended, err := susStore.IsSuspended(ctx, "org-hot")
	if err != nil || !suspended {
		t.Fatalf("shared store IsSuspended = %t, %v; want suspended after the cycle", suspended, err)
	}
	if string(sus.Reason) != "spend_cap" {
		t.Errorf("reason = %q, want spend_cap", sus.Reason)
	}
	if sus.ManualHold {
		t.Error("an automated spend-cap suspension must NOT carry a manual hold (a paid top-up is its lift lever)")
	}
}
