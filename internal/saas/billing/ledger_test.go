package billing

import (
	"context"
	"testing"
	"time"

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

// minuteRecord is a realistic finalized one-minute usage window of a steady
// 1-vCPU sandbox: 60 vCPU-seconds, nothing else. At DefaultRates it costs 76.8
// milli-cents, below one cent, the shape that made drawdown settle zero
// forever (issue #662).
func minuteRecord(org, sb string, minute int) usage.UsageRecord {
	base := fixedNow()
	return usage.UsageRecord{
		OrgID:       org,
		SandboxID:   sb,
		Window:      base.Add(time.Duration(minute) * time.Minute),
		VCPUSeconds: 60,
	}
}

// TestDrawdownAccumulatesSubCentUsage is the issue #662 acceptance: a 1-vCPU
// sandbox running 10 minutes at DefaultRates produces a NONZERO debit. Each
// one-minute window is 76.8 milli-cents; per-record cent rounding billed it 0
// forever. The milli-cent accumulator carries the sub-cent remainder per org
// and settles a whole cent as soon as the running total rounds to one.
func TestDrawdownAccumulatesSubCentUsage(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(5), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: DefaultRates(), Now: fixedNow})

	var settled Money
	var last DrawdownResult
	for m := 0; m < 10; m++ {
		res, err := svc.Drawdown(ctx, minuteRecord("org1", "sb1", m))
		if err != nil {
			t.Fatalf("drawdown minute %d: %v", m, err)
		}
		settled += res.FromCredit
		last = res
	}
	if settled == 0 {
		t.Fatalf("10 minutes of 1 vCPU at DefaultRates settled ZERO cents (issue #662: compute is unbillable)")
	}
	// Exact accounting: each window prices to 77 milli-cents (76.8 rounded to
	// nearest). The running total crosses the half-cent rounding threshold at
	// minute 7 (539 milli-cents settles 1 cent, carry -461); minutes 8-10 carry
	// -384, -307, -230. Total settled: 1 cent.
	if settled != 1 {
		t.Errorf("settled = %d cents, want exactly 1 (770 milli-cents of usage, rounded)", int64(settled))
	}
	if last.CarriedMilliCents != -230 {
		t.Errorf("carried remainder after minute 10 = %d milli-cents, want -230", last.CarriedMilliCents)
	}
	bal, _ := l.Balance(ctx, "org1")
	if bal != USD(5)-1 {
		t.Errorf("balance = %d cents, want %d (500 credit minus the 1 settled cent)", int64(bal), int64(USD(5)-1))
	}
}

// TestDrawdownRemainderCarriesAcrossTicksAndReplays proves the two properties
// the accumulator must hold together: the sub-cent remainder carries from one
// drawdown tick to the next, and a replayed window (the driver re-lists a
// lookback window every tick) neither double-debits NOR double-counts into the
// remainder.
func TestDrawdownRemainderCarriesAcrossTicksAndReplays(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(5), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: DefaultRates(), Now: fixedNow})

	// Tick 1 settles minutes 0-2: remainder 77, 154, 231 milli-cents.
	for m := 0; m < 3; m++ {
		if _, err := svc.Drawdown(ctx, minuteRecord("org1", "sb1", m)); err != nil {
			t.Fatalf("tick 1 minute %d: %v", m, err)
		}
	}
	remAfterTick1, err := l.Remainder(ctx, "org1")
	if err != nil {
		t.Fatalf("remainder: %v", err)
	}
	if remAfterTick1 != 231 {
		t.Fatalf("remainder after tick 1 = %d milli-cents, want 231 (3 x 77)", remAfterTick1)
	}
	balAfterTick1, _ := l.Balance(ctx, "org1")

	// Tick 2 replays the SAME minutes 0-2 (the lookback overlap) plus the new
	// minute 3. The replays must not move the balance or the remainder.
	for m := 0; m < 3; m++ {
		res, err := svc.Drawdown(ctx, minuteRecord("org1", "sb1", m))
		if err != nil {
			t.Fatalf("tick 2 replay minute %d: %v", m, err)
		}
		if res.Remaining != 0 {
			t.Errorf("replay minute %d reported Remaining=%d, want 0 (a replay must never instruct re-billing)", m, int64(res.Remaining))
		}
	}
	remAfterReplay, _ := l.Remainder(ctx, "org1")
	if remAfterReplay != remAfterTick1 {
		t.Fatalf("replay moved the remainder: %d -> %d milli-cents (double-counted carry)", remAfterTick1, remAfterReplay)
	}
	balAfterReplay, _ := l.Balance(ctx, "org1")
	if balAfterReplay != balAfterTick1 {
		t.Fatalf("replay moved the balance: %d -> %d (double debit)", int64(balAfterTick1), int64(balAfterReplay))
	}

	// The new minute continues from the carried remainder: 231 + 77 = 308.
	if _, err := svc.Drawdown(ctx, minuteRecord("org1", "sb1", 3)); err != nil {
		t.Fatalf("tick 2 minute 3: %v", err)
	}
	remAfterNew, _ := l.Remainder(ctx, "org1")
	if remAfterNew != 308 {
		t.Errorf("remainder after new minute = %d milli-cents, want 308 (carry continued across ticks)", remAfterNew)
	}
}

// TestDrawdownZeroUsageWritesNothing asserts an all-zero usage record settles
// to a zero result without a ledger entry or a remainder row: an idle org's
// ledger stays clean.
func TestDrawdownZeroUsageWritesNothing(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: DefaultRates(), Now: fixedNow})

	res, err := svc.Drawdown(ctx, usage.UsageRecord{OrgID: "org1", SandboxID: "sb1", Window: fixedNow()})
	if err != nil {
		t.Fatalf("drawdown: %v", err)
	}
	if res != (DrawdownResult{}) {
		t.Errorf("zero-usage result = %+v, want the zero DrawdownResult", res)
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 0 {
		t.Errorf("zero-usage record wrote %d ledger entries, want 0", len(entries))
	}
	rem, _ := l.Remainder(ctx, "org1")
	if rem != 0 {
		t.Errorf("zero-usage record set remainder %d, want 0", rem)
	}
}

// TestMemLedgerAppendWithRemainderAtomicOnDuplicate asserts the combined
// append-and-set-remainder is all-or-nothing: a duplicate idempotency key
// returns ErrDuplicateEntry and leaves BOTH the entries and the remainder
// exactly as they were. This is the invariant that makes a replayed drawdown
// unable to double-count the carry.
func TestMemLedgerAppendWithRemainderAtomicOnDuplicate(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	e := LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: 0, Key: "w1", At: now, Note: "usage drawdown"}
	if err := l.AppendWithRemainder(ctx, e, 100); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := l.AppendWithRemainder(ctx, e, 900); err != ErrDuplicateEntry {
		t.Fatalf("duplicate append err = %v, want ErrDuplicateEntry", err)
	}
	rem, err := l.Remainder(ctx, "org1")
	if err != nil {
		t.Fatalf("remainder: %v", err)
	}
	if rem != 100 {
		t.Errorf("remainder after duplicate = %d, want 100 (untouched)", rem)
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 1 {
		t.Errorf("entries after duplicate = %d, want 1", len(entries))
	}
}
