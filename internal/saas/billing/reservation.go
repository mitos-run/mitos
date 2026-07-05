package billing

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Reservation is one Box catalog entry: a fixed vCPU/RAM shape reserved for a
// flat monthly price. It is billed as a monthly usage-credit GRANT into the
// org's credit ledger (ApplyMonthlyGrant), not a separate subscription line:
// the org pays the flat MonthlyCents price and the ledger is credited with a
// larger PAYG-equivalent amount at a discount (see CreditCentsForReservation),
// then the org's usage draws down against that credit exactly like a top-up.
type Reservation struct {
	// Key is the catalog's stable identifier (e.g. "box_s"), used in the
	// monthly grant's idempotency key and as the wire value the console and
	// website reference.
	Key string
	// VCPU and MemGiB are the reserved shape.
	VCPU   int
	MemGiB int
	// MonthlyCents is the flat monthly price, in cents.
	MonthlyCents int64
}

// ILLUSTRATIVE, CONFIGURABLE Box catalog (the no-unverified-claims rule):
// these are not published prices, they are the shapes and prices this slice
// ships as its starting catalog. All three are priced ~30% under PAYG list,
// per the boxDiscount math documented on CreditCentsForReservation.
//
// Unexported: Reservation has no reference fields (no slice/map/pointer), so
// a value copy is already independent, but an EXPORTED var of struct type
// can still be reassigned or field-mutated from any importing package
// (billing.boxS.MonthlyCents = 0 would corrupt the shared catalog entry for
// every subsequent reader in the process). BoxS/BoxM/BoxL below are
// accessor functions instead, so a caller can only ever obtain a copy.
var (
	boxS = Reservation{Key: "box_s", VCPU: 2, MemGiB: 4, MonthlyCents: 1900}
	boxM = Reservation{Key: "box_m", VCPU: 4, MemGiB: 8, MonthlyCents: 3900}
	boxL = Reservation{Key: "box_l", VCPU: 8, MemGiB: 16, MonthlyCents: 7500}
)

// BoxS returns a copy of the smallest Box: 2 vCPU / 4 GiB for $19/month.
func BoxS() Reservation { return boxS }

// BoxM returns a copy of the mid Box: 4 vCPU / 8 GiB for $39/month.
func BoxM() Reservation { return boxM }

// BoxL returns a copy of the largest Box: 8 vCPU / 16 GiB for $75/month.
func BoxL() Reservation { return boxL }

// BoxCatalog returns the full Box catalog in S/M/L display order.
func BoxCatalog() []Reservation {
	return []Reservation{boxS, boxM, boxL}
}

// boxDiscount is the fraction of the granted PAYG-equivalent credit value a
// Box customer actually pays: paying boxDiscount (70%) of the credit's list
// value is the same thing as a 30% discount off that list value. ILLUSTRATIVE
// or a real deployment may tune it; it is a package constant (not per-Box) so
// every catalog entry carries the same discount by construction.
const boxDiscount = 0.70

// CreditCentsForReservation computes the usage credit (in cents) a Box's
// monthly price grants into the org's ledger. The mechanics: MonthlyCents is
// 30% BELOW the PAYG list value of the credit, i.e. MonthlyCents = list *
// 0.70, so list = MonthlyCents / 0.70. This is the "$19 buys $27 of PAYG
// credit" illustration:
//
//	Box S: $19.00 / 0.70 = $27.1428... -> 2714 cents ($27.14).
//	Box M: $39.00 / 0.70 = $55.7142... -> 5571 cents ($55.71).
//	Box L: $75.00 / 0.70 = $107.1428... -> 10714 cents ($107.14).
//
// The result is rounded to the nearest cent exactly once, matching the
// CostCents/CostMilliCents rounding discipline elsewhere in this package.
func CreditCentsForReservation(res Reservation) int64 {
	return int64(math.Round(float64(res.MonthlyCents) / boxDiscount))
}

// boxGrantYearMonthLayout is the accepted yearMonth format for
// ApplyMonthlyGrant: "2006-01" (YYYY-MM).
const boxGrantYearMonthLayout = "2006-01"

// BoxGrantKey returns the box_grant ledger idempotency key for one org's
// reservation in one calendar month: "box|<org>|<yyyy-mm>|<key>". The monthly
// grant job (or a manual replay) can call this directly to check whether a
// grant has already landed without going through ApplyMonthlyGrant.
func BoxGrantKey(orgID, yearMonth, resKey string) string {
	return fmt.Sprintf("box|%s|%s|%s", orgID, yearMonth, resKey)
}

// ApplyMonthlyGrant posts the monthly usage-credit grant for orgID's
// reservation res, for the calendar month yearMonth (format "2006-01"). It is
// idempotent per (org, month, box): a replay of the same three values returns
// ErrDuplicateEntry and changes nothing, so a retried or re-run monthly grant
// job never double-credits an org.
//
// yearMonth is parsed to validate its format AND to give the ledger entry a
// meaningful At (the first instant of that month, UTC) rather than the grant
// job's own run time, so the ledger statement reads by the month the
// capacity was reserved for, not the day the job happened to run.
func ApplyMonthlyGrant(ctx context.Context, ledger CreditLedger, orgID string, res Reservation, yearMonth string) error {
	at, err := time.Parse(boxGrantYearMonthLayout, yearMonth)
	if err != nil {
		return fmt.Errorf("billing: invalid year-month %q for box grant (want YYYY-MM): %w", yearMonth, err)
	}
	credit := CreditCentsForReservation(res)
	return ledger.Append(ctx, LedgerEntry{
		OrgID:  orgID,
		Kind:   KindBoxGrant,
		Amount: Money(credit),
		Key:    BoxGrantKey(orgID, yearMonth, res.Key),
		At:     at,
		Note:   fmt.Sprintf("box grant: %s (%d vCPU / %d GiB) reserved capacity for %s", res.Key, res.VCPU, res.MemGiB, yearMonth),
	})
}
