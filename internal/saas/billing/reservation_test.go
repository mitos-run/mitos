package billing

import (
	"context"
	"errors"
	"testing"
)

// TestBoxCatalogValues asserts the catalog matches the committed, illustrative
// shapes and prices: Box S 2 vCPU/4 GiB $19, Box M 4 vCPU/8 GiB $39, Box L 8
// vCPU/16 GiB $75.
func TestBoxCatalogValues(t *testing.T) {
	cases := []struct {
		res          Reservation
		vcpu, memGiB int
		monthlyCents int64
	}{
		{BoxS(), 2, 4, 1900},
		{BoxM(), 4, 8, 3900},
		{BoxL(), 8, 16, 7500},
	}
	for _, c := range cases {
		if c.res.VCPU != c.vcpu || c.res.MemGiB != c.memGiB || c.res.MonthlyCents != c.monthlyCents {
			t.Errorf("%s = %+v, want vcpu=%d memGiB=%d monthlyCents=%d", c.res.Key, c.res, c.vcpu, c.memGiB, c.monthlyCents)
		}
	}
	catalog := BoxCatalog()
	if len(catalog) != 3 {
		t.Fatalf("BoxCatalog() has %d entries, want 3", len(catalog))
	}
}

// TestBoxAccessorsReturnIndependentCopies asserts mutating a Reservation
// obtained from BoxS/BoxM/BoxL (or from BoxCatalog's slice) never affects a
// later call: the package's real catalog entries are unexported package
// vars a caller can no longer reach directly, only ever getting a copy back.
func TestBoxAccessorsReturnIndependentCopies(t *testing.T) {
	s := BoxS()
	s.MonthlyCents = 0
	s.Key = "corrupted"
	if again := BoxS(); again.MonthlyCents != 1900 || again.Key != "box_s" {
		t.Fatalf("BoxS() after mutating a prior copy = %+v, want the unmodified catalog entry", again)
	}

	catalog := BoxCatalog()
	catalog[0].MonthlyCents = 0
	if again := BoxCatalog(); again[0].MonthlyCents != 1900 {
		t.Fatalf("BoxCatalog()[0] after mutating a prior copy = %+v, want the unmodified catalog entry", again[0])
	}
}

// TestCreditCentsForReservationDiscountMath asserts the exact discount
// mechanics documented on CreditCentsForReservation: a Box's monthly price is
// ~30% under the PAYG-list value of the credit it grants, so the granted
// credit is the price divided by (1 - 0.30). Box S's $19 must buy ~$27.14 of
// PAYG credit, matching the design's "$19 buys $27" illustration.
func TestCreditCentsForReservationDiscountMath(t *testing.T) {
	cases := []struct {
		res    Reservation
		credit int64
	}{
		{BoxS(), 2714},  // $19.00 / 0.70 = $27.1428... -> 2714 cents.
		{BoxM(), 5571},  // $39.00 / 0.70 = $55.7142... -> 5571 cents.
		{BoxL(), 10714}, // $75.00 / 0.70 = $107.1428... -> 10714 cents.
	}
	for _, c := range cases {
		got := CreditCentsForReservation(c.res)
		if got != c.credit {
			t.Errorf("CreditCentsForReservation(%s) = %d, want %d", c.res.Key, got, c.credit)
		}
	}
}

// TestApplyMonthlyGrantCreditsLedgerOnce asserts a single grant call credits
// the org's ledger balance by the reservation's discounted credit value, with
// a box_grant entry keyed by (org, month, box).
func TestApplyMonthlyGrantCreditsLedgerOnce(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxS(), "2026-07"); err != nil {
		t.Fatalf("ApplyMonthlyGrant: %v", err)
	}
	bal, err := l.Balance(ctx, "org1")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := Money(CreditCentsForReservation(BoxS())); bal != want {
		t.Errorf("balance = %d, want %d", int64(bal), int64(want))
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 1 || entries[0].Kind != KindBoxGrant {
		t.Fatalf("entries = %+v, want exactly one box_grant entry", entries)
	}
	if entries[0].Key != "box|org1|2026-07|box_s" {
		t.Errorf("entry key = %q, want %q", entries[0].Key, "box|org1|2026-07|box_s")
	}
}

// TestApplyMonthlyGrantIsIdempotentPerOrgMonth asserts a replayed grant call
// for the same org, month, and box is a no-op: it returns ErrDuplicateEntry
// and the balance is unaffected (credited exactly once), matching the ledger's
// own idempotency contract.
func TestApplyMonthlyGrantIsIdempotentPerOrgMonth(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxM(), "2026-07"); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	err := ApplyMonthlyGrant(ctx, l, "org1", BoxM(), "2026-07")
	if !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("second grant err = %v, want ErrDuplicateEntry", err)
	}
	bal, _ := l.Balance(ctx, "org1")
	if want := Money(CreditCentsForReservation(BoxM())); bal != want {
		t.Errorf("balance after replay = %d, want %d (credited once)", int64(bal), int64(want))
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (no duplicate row)", len(entries))
	}
}

// TestApplyMonthlyGrantDistinguishesMonthAndBox asserts the idempotency key
// includes both the month and the box: a different month, or a different box
// in the same month, grants a SEPARATE credit rather than colliding.
func TestApplyMonthlyGrantDistinguishesMonthAndBox(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxS(), "2026-07"); err != nil {
		t.Fatalf("box S july: %v", err)
	}
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxS(), "2026-08"); err != nil {
		t.Fatalf("box S august: %v", err)
	}
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxL(), "2026-07"); err != nil {
		t.Fatalf("box L july: %v", err)
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 distinct grants", len(entries))
	}
}

// TestApplyMonthlyGrantRejectsMalformedYearMonth asserts a badly formed
// yearMonth is rejected before touching the ledger (no partial entry).
func TestApplyMonthlyGrantRejectsMalformedYearMonth(t *testing.T) {
	ctx := context.Background()
	l := NewMemCreditLedger()
	if err := ApplyMonthlyGrant(ctx, l, "org1", BoxS(), "not-a-month"); err == nil {
		t.Fatal("expected an error for a malformed year-month")
	}
	entries, _ := l.Entries(ctx, "org1")
	if len(entries) != 0 {
		t.Errorf("entries = %d, want 0 (rejected before any ledger write)", len(entries))
	}
}
