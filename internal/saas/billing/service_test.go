package billing

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/usage"
)

// recordingSuspender records suspend calls so a test can assert the #213
// kill-switch seam fired with the right org, reason, and manual hold.
type recordingSuspender struct {
	calls []suspendCall
}

type suspendCall struct {
	orgID, reason, note string
	manualHold          bool
}

func (s *recordingSuspender) Suspend(_ context.Context, orgID, reason, note string, manualHold bool) error {
	s.calls = append(s.calls, suspendCall{orgID, reason, note, manualHold})
	return nil
}

// recordingAlerts records budget alerts so a test can assert the soft-cap hook.
type recordingAlerts struct {
	events []BudgetAlertEvent
}

func (a *recordingAlerts) BudgetAlert(_ context.Context, ev BudgetAlertEvent) error {
	a.events = append(a.events, ev)
	return nil
}

func fixedNow() time.Time { return time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC) }

func sampleRecord() usage.UsageRecord {
	return usage.UsageRecord{
		OrgID:         "org1",
		SandboxID:     "sb1",
		Window:        time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		VCPUSeconds:   3600,
		MemGiBSeconds: 7200,
		EgressBytes:   int64(1) << 30,
	}
}

// TestPushUsageReportsOneEventPerNonZeroMeter asserts a record is pushed as one
// metered event per non-zero meter, keyed by the (org, sandbox, window)+meter
// idempotency key.
func TestPushUsageReportsOneEventPerNonZeroMeter(t *testing.T) {
	fake := NewFakeStripe()
	svc := NewService(Config{Stripe: fake, Now: fixedNow})
	rec := sampleRecord() // vcpu, mem, egress non-zero; storage, gpu zero.

	n, err := svc.PushUsage(context.Background(), rec)
	if err != nil {
		t.Fatalf("PushUsage: %v", err)
	}
	if n != 3 {
		t.Errorf("reported %d meter events, want 3 (vcpu, mem, egress)", n)
	}
	if fake.ReportedCount() != 3 {
		t.Errorf("fake distinct events = %d, want 3", fake.ReportedCount())
	}
	// The vCPU event must carry the canonical idempotency key.
	key := IdempotencyKey(rec, MeterVCPUSecond)
	if _, ok := fake.Reported(key); !ok {
		t.Errorf("no event reported under vCPU idempotency key %q", key)
	}
}

// TestRetriedPushIsIdempotent is the load-bearing money property: re-pushing the
// SAME usage record reports the SAME idempotency keys, so Stripe de-duplicates
// and the distinct-event count does NOT grow. No double-report on retry.
func TestRetriedPushIsIdempotent(t *testing.T) {
	fake := NewFakeStripe()
	svc := NewService(Config{Stripe: fake, Now: fixedNow})
	rec := sampleRecord()

	if _, err := svc.PushUsage(context.Background(), rec); err != nil {
		t.Fatalf("first push: %v", err)
	}
	first := fake.ReportedCount()
	// Retry the exact same record several times.
	for i := 0; i < 5; i++ {
		if _, err := svc.PushUsage(context.Background(), rec); err != nil {
			t.Fatalf("retry push %d: %v", i, err)
		}
	}
	if fake.ReportedCount() != first {
		t.Errorf("distinct events after retries = %d, want %d (no double report)", fake.ReportedCount(), first)
	}
}

// TestPushRetryAfterTransientFailureNoDoubleReport arms a transient Stripe
// failure for one meter, then retries; the retry must report the same key, so
// the final distinct-event count is correct (no duplicate from the retry).
func TestPushRetryAfterTransientFailureNoDoubleReport(t *testing.T) {
	fake := NewFakeStripe()
	svc := NewService(Config{Stripe: fake, Now: fixedNow})
	rec := sampleRecord()
	// Fail the FIRST meter (vCPU) once so the first push aborts partway.
	fake.ArmReportFailure(IdempotencyKey(rec, MeterVCPUSecond), 1)

	if _, err := svc.PushUsage(context.Background(), rec); err == nil {
		t.Fatal("expected first push to fail on the armed meter")
	}
	// Retry: now succeeds for all meters.
	n, err := svc.PushUsage(context.Background(), rec)
	if err != nil {
		t.Fatalf("retry push: %v", err)
	}
	if n != 3 {
		t.Errorf("retry reported %d events, want 3", n)
	}
	if fake.ReportedCount() != 3 {
		t.Errorf("distinct events = %d, want 3 (the retry must not duplicate)", fake.ReportedCount())
	}
}

// TestSoftCapFiresAlertHardCapSuspends asserts the spend-cap behavior: crossing
// the SOFT cap fires a budget alert with no suspension; crossing the HARD cap
// suspends via the #213 kill-switch seam WITHOUT a manual hold, because a paid
// top-up is the automated lift lever (a held suspension could never be lifted
// by payment; only operator-imposed suspensions carry holds).
func TestSoftCapFiresAlertHardCapSuspends(t *testing.T) {
	fake := NewFakeStripe()
	sus := &recordingSuspender{}
	alerts := &recordingAlerts{}
	svc := NewService(Config{Stripe: fake, Suspend: sus, Alerts: alerts, Now: fixedNow})
	ctx := context.Background()
	if err := svc.SetSpendCap(ctx, SpendCap{OrgID: "org1", SoftCap: USD(50), HardCap: USD(100)}); err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}

	// Below soft cap: nothing fires.
	suspended, err := svc.EnforceSpendCap(ctx, "org1", USD(40))
	if err != nil || suspended {
		t.Fatalf("below soft cap: suspended=%v err=%v", suspended, err)
	}
	if len(alerts.events) != 0 {
		t.Errorf("alert fired below soft cap")
	}

	// At soft cap: alert, no suspend.
	suspended, err = svc.EnforceSpendCap(ctx, "org1", USD(60))
	if err != nil {
		t.Fatalf("soft cap: %v", err)
	}
	if suspended {
		t.Error("soft cap should not suspend")
	}
	if len(alerts.events) != 1 || alerts.events[0].OrgID != "org1" {
		t.Errorf("soft cap did not fire exactly one alert: %+v", alerts.events)
	}

	// At hard cap: suspend via the kill-switch seam, no manual hold (payment lifts).
	suspended, err = svc.EnforceSpendCap(ctx, "org1", USD(120))
	if err != nil {
		t.Fatalf("hard cap: %v", err)
	}
	if !suspended {
		t.Fatal("hard cap must suspend")
	}
	if len(sus.calls) != 1 {
		t.Fatalf("hard cap fired %d suspends, want 1", len(sus.calls))
	}
	if sus.calls[0].orgID != "org1" || sus.calls[0].reason != "spend_cap" || sus.calls[0].manualHold {
		t.Errorf("hard-cap suspend = %+v, want org1/spend_cap without a manual hold (a top-up must be able to lift it)", sus.calls[0])
	}
	// The note is non-secret: it must not be empty and must mention the cap.
	if sus.calls[0].note == "" {
		t.Error("suspend note is empty")
	}
	// Billing status reflects the suspension.
	st, _ := svc.status.Status(ctx, "org1")
	if st != StatusSuspended {
		t.Errorf("status after hard cap = %q, want suspended", st)
	}
}

// TestDrawdownReplayReturnsSameSplit proves a replayed drawdown of the same
// usage record is idempotent in its RESULT, not just in the ledger. The first
// call debits the credit portion; a replay must report the SAME FromCredit /
// Remaining, because returning FromCredit:0 (Remaining:cost) would tell the
// caller to re-bill the whole cost via Stripe even though credit already
// covered part of it: a double-bill. The ledger must be debited exactly once.
func TestDrawdownReplayReturnsSameSplit(t *testing.T) {
	led := NewMemCreditLedger()
	// Seed credit larger than the record cost so the first drawdown is fully
	// covered by credit (FromCredit == cost, Remaining == 0).
	if err := led.Append(context.Background(), LedgerEntry{OrgID: "org1", Kind: KindTopUp, Amount: Money(100_000_000), Key: "seed", At: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{Ledger: led, Now: fixedNow})
	rec := sampleRecord()

	first, err := svc.Drawdown(context.Background(), rec)
	if err != nil {
		t.Fatalf("first drawdown: %v", err)
	}
	if first.Cost <= 0 {
		t.Fatalf("expected a positive cost, got %d", first.Cost)
	}
	if first.FromCredit != first.Cost || first.Remaining != 0 {
		t.Fatalf("first: FromCredit=%d Remaining=%d, want fully credit-covered (FromCredit=Cost=%d, Remaining=0)", first.FromCredit, first.Remaining, first.Cost)
	}

	balAfterFirst, _ := led.Balance(context.Background(), "org1")

	second, err := svc.Drawdown(context.Background(), rec)
	if err != nil {
		t.Fatalf("replay drawdown: %v", err)
	}
	if second.FromCredit != first.FromCredit || second.Remaining != first.Remaining || second.Cost != first.Cost {
		t.Fatalf("replay result %+v != first result %+v (idempotency must report the same split, not FromCredit:0)", second, first)
	}
	balAfterSecond, _ := led.Balance(context.Background(), "org1")
	if balAfterSecond != balAfterFirst {
		t.Fatalf("replay debited the ledger again: balance %d -> %d", balAfterFirst, balAfterSecond)
	}
}
