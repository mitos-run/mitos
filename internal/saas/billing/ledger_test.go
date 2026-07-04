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

// TestMemLedgerSettleWindowAtomicOnDuplicate asserts the combined
// marker-entry-remainder settle is all-or-nothing: a replayed window returns
// ErrDuplicateEntry and leaves the entries, the remainder, and the marker set
// exactly as they were. This is the invariant that makes a replayed drawdown
// unable to double-debit or double-count the carry.
func TestMemLedgerSettleWindowAtomicOnDuplicate(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	w := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now}
	e := LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -1, Key: w.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, e, 100, w); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if err := l.SettleWindow(ctx, e, 900, w); err != ErrDuplicateEntry {
		t.Fatalf("duplicate settle err = %v, want ErrDuplicateEntry", err)
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
	bal, _ := l.Balance(ctx, "org1")
	if bal != -1 {
		t.Errorf("balance after duplicate = %d, want -1 (the single debit)", int64(bal))
	}
}

// TestMemLedgerSettleWindowDuplicateLedgerKeyLeavesMarkerUnset covers the
// deploy transition: a window settled BEFORE the marker mechanism has only its
// keyed ledger row. If a settle for that window slips past the skip set, the
// keyed ledger insert must reject it and the marker must NOT land either (the
// whole settle is one atomic step).
func TestMemLedgerSettleWindowDuplicateLedgerKeyLeavesMarkerUnset(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	w := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now}
	// The pre-marker deploy wrote the keyed ledger row via plain Append.
	old := LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -2, Key: w.Key(), At: now, Note: "usage drawdown"}
	if err := l.Append(ctx, old); err != nil {
		t.Fatalf("legacy append: %v", err)
	}

	e := LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -2, Key: w.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, e, 250, w); err != ErrDuplicateEntry {
		t.Fatalf("settle over legacy key err = %v, want ErrDuplicateEntry", err)
	}
	rem, _ := l.Remainder(ctx, "org1")
	if rem != 0 {
		t.Errorf("remainder after rejected settle = %d, want 0 (untouched)", rem)
	}
	keys, err := l.SettledWindowKeys(ctx, "org1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("settled keys: %v", err)
	}
	// The window is still settled (the legacy ledger row says so), but only via
	// the ledger half; the failed settle must not have half-written a marker
	// alongside a rolled-back debit.
	if !keys[w.Key()] {
		t.Errorf("settled keys missing the legacy ledger key %q", w.Key())
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (the legacy row only)", len(entries))
	}
}

// TestMemLedgerZeroAmountSettleWritesMarkerNotEntry pins the issue #672 ledger
// hygiene: a zero-cent settle (sub-cent usage folding into the carry) marks
// the window processed and advances the remainder WITHOUT a customer-visible
// zero-amount ledger row.
func TestMemLedgerZeroAmountSettleWritesMarkerNotEntry(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	w := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now}
	e := LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: 0, Key: w.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, e, 77, w); err != nil {
		t.Fatalf("zero-amount settle: %v", err)
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 0 {
		t.Errorf("zero-amount settle wrote %d ledger entries, want 0", len(entries))
	}
	rem, _ := l.Remainder(ctx, "org1")
	if rem != 77 {
		t.Errorf("remainder = %d, want 77", rem)
	}
	// The marker still deduplicates a replay.
	if err := l.SettleWindow(ctx, e, 154, w); err != ErrDuplicateEntry {
		t.Fatalf("replayed zero-amount settle err = %v, want ErrDuplicateEntry", err)
	}
	keys, _ := l.SettledWindowKeys(ctx, "org1", now.Add(-time.Hour))
	if !keys[w.Key()] {
		t.Errorf("settled keys missing the marker key %q", w.Key())
	}
}

// TestMemLedgerSettledWindowKeysUnionsMarkersAndLegacyRows asserts the skip
// set consults BOTH dedup mechanisms during the transition horizon: windows
// settled before the marker table (keyed ledger rows) AND windows settled
// after (markers), each filtered to the since bound.
func TestMemLedgerSettledWindowKeysUnionsMarkersAndLegacyRows(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	// A legacy pre-marker settle: keyed ledger row only.
	legacyKey := DrawdownKey("org1", "sb1", now.Add(-10*time.Minute))
	if err := l.Append(ctx, LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: 0, Key: legacyKey, At: now}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	// A post-deploy settle: marker (zero amount, so no ledger row).
	w := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now.Add(-5 * time.Minute)}
	if err := l.SettleWindow(ctx, LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: 0, Key: w.Key(), At: now}, 10, w); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A non-drawdown keyed entry must never enter the skip set.
	if err := l.Append(ctx, LedgerEntry{OrgID: "org1", Kind: KindTopUp, Amount: 500, Key: "topup:pi_1", At: now}); err != nil {
		t.Fatalf("topup append: %v", err)
	}

	keys, err := l.SettledWindowKeys(ctx, "org1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("settled keys: %v", err)
	}
	if !keys[legacyKey] {
		t.Errorf("missing legacy ledger key %q", legacyKey)
	}
	if !keys[w.Key()] {
		t.Errorf("missing marker key %q", w.Key())
	}
	if keys["topup:pi_1"] {
		t.Errorf("top-up key leaked into the settled-window skip set")
	}
	if len(keys) != 2 {
		t.Errorf("settled keys = %d, want 2", len(keys))
	}

	// The since bound excludes older state: a since after everything yields none.
	none, err := l.SettledWindowKeys(ctx, "org1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("settled keys (future since): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("settled keys with future since = %d, want 0", len(none))
	}
}

// TestMemLedgerPruneProcessedWindows asserts pruning removes only markers
// whose window predates the horizon and reports the removed count.
func TestMemLedgerPruneProcessedWindows(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()

	oldW := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now.Add(-3 * time.Hour)}
	newW := ProcessedWindow{OrgID: "org1", SandboxID: "sb1", Window: now.Add(-5 * time.Minute)}
	for _, w := range []ProcessedWindow{oldW, newW} {
		if err := l.SettleWindow(ctx, LedgerEntry{OrgID: w.OrgID, Kind: KindUsageDrawdown, Amount: 0, Key: w.Key(), At: now}, 0, w); err != nil {
			t.Fatalf("settle %v: %v", w.Window, err)
		}
	}

	n, err := l.PruneProcessedWindows(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
	keys, _ := l.SettledWindowKeys(ctx, "org1", time.Time{})
	if keys[oldW.Key()] {
		t.Errorf("pruned marker %q still present", oldW.Key())
	}
	if !keys[newW.Key()] {
		t.Errorf("in-horizon marker %q was pruned", newW.Key())
	}
}

// TestDrawdownReplayIsFlagged asserts a replayed Drawdown reports
// Replayed=true (so the driver counts it out of settledCents) while a first
// settle reports Replayed=false.
func TestDrawdownReplayIsFlagged(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(100), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: DefaultRates(), Now: fixedNow})
	rec := usage.UsageRecord{OrgID: "org1", SandboxID: "sb1", Window: now, VCPUSeconds: 100_000} // $1.28.

	first, err := svc.Drawdown(ctx, rec)
	if err != nil {
		t.Fatalf("first drawdown: %v", err)
	}
	if first.Replayed {
		t.Errorf("first drawdown reported Replayed=true, want false")
	}
	replay, err := svc.Drawdown(ctx, rec)
	if err != nil {
		t.Fatalf("replay drawdown: %v", err)
	}
	if !replay.Replayed {
		t.Errorf("replayed drawdown reported Replayed=false, want true")
	}
	if replay.FromCredit != first.FromCredit || replay.Remaining != 0 {
		t.Errorf("replay = %+v, want prior FromCredit %d and Remaining 0", replay, int64(first.FromCredit))
	}
}

// TestDrawdownZeroCentSettleKeepsLedgerClean is the issue #672 acceptance at
// the service level: sub-cent windows that settle 0 cents no longer write
// zero-amount usage_drawdown ledger rows; only the windows that actually move
// money appear in the customer-visible ledger.
func TestDrawdownZeroCentSettleKeepsLedgerClean(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	now := fixedNow()
	if err := GrantSignupCredit(ctx, l, "org1", USD(5), now); err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc := NewService(Config{Stripe: NewFakeStripe(), Ledger: l, Rates: DefaultRates(), Now: fixedNow})

	// 10 sub-cent minutes settle exactly 1 cent (see
	// TestDrawdownAccumulatesSubCentUsage): exactly ONE drawdown row.
	for m := 0; m < 10; m++ {
		if _, err := svc.Drawdown(ctx, minuteRecord("org1", "sb1", m)); err != nil {
			t.Fatalf("drawdown minute %d: %v", m, err)
		}
	}
	entries, _ := l.Entries(ctx, "org1")
	var drawdownRows, zeroRows int
	for _, e := range entries {
		if e.Kind != KindUsageDrawdown {
			continue
		}
		drawdownRows++
		if e.Amount == 0 {
			zeroRows++
		}
	}
	if drawdownRows != 1 {
		t.Errorf("usage_drawdown rows = %d, want 1 (only the settled cent)", drawdownRows)
	}
	if zeroRows != 0 {
		t.Errorf("zero-amount usage_drawdown rows = %d, want 0 (issue #672)", zeroRows)
	}
	// Every priced window is still marked processed.
	keys, _ := l.SettledWindowKeys(ctx, "org1", time.Time{})
	if len(keys) != 10 {
		t.Errorf("settled window keys = %d, want 10", len(keys))
	}
}
