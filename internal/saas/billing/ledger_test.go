package billing

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/usage"
)

// TestCreditDrawdownAndTopUpBalance asserts the full credit lifecycle: a signup
// credit and a top-up add to the balance, a usage drawdown debits it, and the
// balance is the exact signed sum throughout.
func TestCreditDrawdownAndTopUpBalance(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	if err := GrantSignupCredit(ctx, l, "org1", USD(100), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := TopUp(ctx, l, "org1", USD(25), "pi_abc", now); err != nil {
		t.Fatalf("topup: %v", err)
	}
	bal, _ := l.Balance(ctx, "org1")
	if bal != USD(125) {
		t.Fatalf("balance after grant+topup = %d cents, want %d", int64(bal), int64(USD(125)))
	}

	// A usage record costing some cents draws the balance down.
	rates := DefaultRates()
	rec := usage.UsageRecord{OrgID: "org1", SandboxID: "sb1", Window: now, VCPUSeconds: 1_000_000} // $12.80.
	cost := rates.CostCents(rec)
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: rates, Now: fixedNow})
	res, err := svc.Drawdown(ctx, rec)
	if err != nil {
		t.Fatalf("drawdown: %v", err)
	}
	if res.Cost != cost || res.FromCredit != cost || res.Remaining != 0 {
		t.Errorf("drawdown = %+v, want fully covered by credit (cost %d)", res, int64(cost))
	}
	bal, _ = l.Balance(ctx, "org1")
	if bal != USD(125)-cost {
		t.Errorf("balance after drawdown = %d, want %d", int64(bal), int64(USD(125)-cost))
	}
}

// TestDrawdownNeverGoesNegative asserts a usage cost larger than the remaining
// balance debits only the available balance (the ledger floors at zero) and
// reports the uncovered remainder as the metered overage Stripe bills.
func TestDrawdownNeverGoesNegative(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(5), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rates := DefaultRates()
	rec := usage.UsageRecord{OrgID: "org1", SandboxID: "sb1", Window: now, VCPUSeconds: 1_000_000} // $12.80 > $5.
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: rates, Now: fixedNow})

	res, err := svc.Drawdown(ctx, rec)
	if err != nil {
		t.Fatalf("drawdown: %v", err)
	}
	if res.FromCredit != USD(5) {
		t.Errorf("FromCredit = %d, want 500 (the whole balance, no more)", int64(res.FromCredit))
	}
	if res.Remaining != res.Cost-USD(5) {
		t.Errorf("Remaining = %d, want cost-500 = %d", int64(res.Remaining), int64(res.Cost-USD(5)))
	}
	bal, _ := l.Balance(ctx, "org1")
	if bal < 0 {
		t.Fatalf("balance went NEGATIVE: %d cents", int64(bal))
	}
	if bal != 0 {
		t.Errorf("balance = %d, want 0 (drained, not negative)", int64(bal))
	}
}

// TestDrawdownIsIdempotent asserts replaying the SAME usage record does not
// double-debit: the drawdown is keyed by the (org, sandbox, window) usage key.
func TestDrawdownIsIdempotent(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(100), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rates := DefaultRates()
	rec := usage.UsageRecord{OrgID: "org1", SandboxID: "sb1", Window: now, VCPUSeconds: 100_000} // $1.28.
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: rates, Now: fixedNow})

	if _, err := svc.Drawdown(ctx, rec); err != nil {
		t.Fatalf("first drawdown: %v", err)
	}
	balAfterFirst, _ := l.Balance(ctx, "org1")
	// Replay the same record several times.
	for i := 0; i < 4; i++ {
		if _, err := svc.Drawdown(ctx, rec); err != nil {
			t.Fatalf("replay drawdown %d: %v", i, err)
		}
	}
	balAfterReplay, _ := l.Balance(ctx, "org1")
	if balAfterReplay != balAfterFirst {
		t.Errorf("balance changed on replay: %d -> %d (double debit)", int64(balAfterFirst), int64(balAfterReplay))
	}
}

// TestSignupAndTopUpAreIdempotentByKey asserts a re-run signup grant and a
// redelivered top-up (same payment ref) do not add the credit twice.
func TestSignupAndTopUpAreIdempotentByKey(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	_ = GrantSignupCredit(ctx, l, "org1", USD(100), now)
	_ = GrantSignupCredit(ctx, l, "org1", USD(100), now) // retried signup.
	_ = TopUp(ctx, l, "org1", USD(50), "pi_1", now)
	_ = TopUp(ctx, l, "org1", USD(50), "pi_1", now) // redelivered webhook.

	bal, _ := l.Balance(ctx, "org1")
	if bal != USD(150) {
		t.Errorf("balance = %d, want %d (no double grant/topup)", int64(bal), int64(USD(150)))
	}
}
