// Package billing wires the money for the hosted offering (issue #212): Stripe
// metered usage-based billing, plans, free signup credits, prepaid top-ups,
// hard/soft spend caps, and dunning, layered on top of the per-org UsageRecords
// from issue #211 and coordinated with the kill-switch from issue #213.
//
// CRITICAL: Stripe is an external service and real charges need test-mode API
// keys that are NOT available in this slice. Everything here is built against a
// StripeClient INTERFACE with a FakeStripe in-memory implementation for tests.
// The real Stripe SDK adapter and the real webhook-signature verification are
// documented seams (see docs/saas/pricing.md and the StripeClient comment); a
// maintainer with test-mode keys runs them behind this interface. NOTHING in
// this slice makes a real charge, and no Stripe SDK is a hard dependency.
//
// Security: a Stripe API key, a signing secret, a customer payment method, or
// any other payment secret is NEVER logged, NEVER placed in an error message,
// and NEVER put in a condition or webhook note. Keys are passed by reference
// (an env-resolved handle), counts and ids only are logged.
package billing

import "mitos.run/mitos/internal/usage"

// Money is an integer count of the account currency's minor unit (cents for
// USD), the standard Stripe representation. Using an integer minor unit avoids
// float rounding drift in the credit ledger and the spend-cap comparison: every
// balance, charge, and cap is exact. Conversions from the float per-unit rates
// happen once, at the metering->cost boundary (CostCents), and round to the
// nearest cent.
type Money int64

// USD builds Money from whole dollars, a convenience for caps and credits that
// are naturally expressed in dollars (the $100 signup credit, a $50 cap).
func USD(dollars int64) Money { return Money(dollars * 100) }

// Dollars renders Money as a float dollar amount for display only. It is never
// used in accounting arithmetic; the ledger and caps work on the integer cents.
func (m Money) Dollars() float64 { return float64(m) / 100 }

// MeterUnit identifies a Stripe metered billing dimension. The pricing shape
// matches the category (per-second compute with DECOUPLED vCPU and RAM, plus
// storage and egress), so each unit maps to its own Stripe meter/price and is
// reported independently. This is the structured config the usage.PriceList
// placeholder from #211 is reconciled to.
type MeterUnit string

const (
	// MeterVCPUSecond is per-vCPU-second compute, billed decoupled from RAM so a
	// CPU-heavy and a RAM-heavy sandbox of the same wall-clock cost differently.
	MeterVCPUSecond MeterUnit = "vcpu_second"
	// MeterMemGiBSecond is per-GiB-second RAM, the decoupled memory dimension.
	MeterMemGiBSecond MeterUnit = "mem_gib_second"
	// MeterStorageGiBHour is per-GiB-hour persisted sandbox storage.
	MeterStorageGiBHour MeterUnit = "storage_gib_hour"
	// MeterEgressGiB is per-GiB outbound egress. See the egress decision in
	// docs/saas/pricing.md: egress is METERED (not free-within-cap) because
	// unmetered egress is the classic abuse subsidy; the free tier's hard egress
	// CAP (issue #213) bounds a free org's exposure, and paid orgs pay per GiB.
	MeterEgressGiB MeterUnit = "egress_gib"
	// MeterGPUSecond is per-GPU-second accelerator time.
	MeterGPUSecond MeterUnit = "gpu_second"
)

// Rates is the structured, configurable per-unit price table: the decoupled
// per-second compute model in one place. The values in DefaultRates are
// ILLUSTRATIVE and CONFIGURABLE, never published prices (the no-unverified-
// claims rule). A hosted deployment overrides them from config; only the SHAPE
// (decoupled vCPU + RAM per second, storage GiB-hours, metered egress) is
// committed here. All rates are in account-currency MINOR UNITS (cents) per
// unit so the cost computation stays in integer money end to end.
type Rates struct {
	// VCPUSecondMilliCents and the rest are quoted in MILLI-cents per unit
	// (thousandths of a cent) because a single vCPU-second costs a tiny fraction
	// of a cent; quoting in cents would round every per-second tick to zero. The
	// CostCents accumulation sums milli-cents across all usage, then divides to
	// cents once at the end, so sub-cent rates accumulate exactly.
	VCPUSecondMilliCents     float64
	MemGiBSecondMilliCents   float64
	StorageGiBHourMilliCents float64
	EgressGiBMilliCents      float64
	GPUSecondMilliCents      float64
}

// DefaultRates returns the illustrative, configurable default rate table. These
// are NOT published prices; they mirror the magnitude of the #211 placeholder
// usage.DefaultPriceList (dollars-per-unit) re-expressed in milli-cents per
// unit so the two models reconcile. A real deployment sets its own rates.
//
//	usage.DefaultPriceList (USD/unit) -> milli-cents/unit (x 100000):
//	  vcpu_second   0.0000128 -> 1.28
//	  mem_gib_sec   0.0000016 -> 0.16
//	  storage_gib_h 0.0001    -> 10
//	  egress_gib    0.09      -> 9000
//	  gpu_second    0.0006    -> 60
func DefaultRates() Rates {
	return Rates{
		VCPUSecondMilliCents:     1.28,
		MemGiBSecondMilliCents:   0.16,
		StorageGiBHourMilliCents: 10,
		EgressGiBMilliCents:      9000,
		GPUSecondMilliCents:      60,
	}
}

// FromPriceList reconciles a #211 usage.PriceList (dollars per unit) into the
// structured billing Rates (milli-cents per unit). This is the single bridge
// between the usage API's display-cost estimate and the real billing rates, so
// the two never drift: a deployment configures one table and derives the other.
func FromPriceList(pl usage.PriceList) Rates {
	const dollarsToMilliCents = 100000.0 // $1 = 100 cents = 100000 milli-cents.
	return Rates{
		VCPUSecondMilliCents:     pl.VCPUSecond * dollarsToMilliCents,
		MemGiBSecondMilliCents:   pl.MemGiBSecond * dollarsToMilliCents,
		StorageGiBHourMilliCents: pl.StorageGiBHour * dollarsToMilliCents,
		EgressGiBMilliCents:      pl.EgressGiB * dollarsToMilliCents,
		GPUSecondMilliCents:      pl.GPUSecond * dollarsToMilliCents,
	}
}

const bytesPerGiB = float64(1 << 30)

// QuantityFor returns the metered quantity (in the meter's natural unit) a
// single usage record contributes to a given meter. Egress and GPU are deltas
// of cumulative counters; vCPU, memory, and storage are time-integrated levels.
// Quantities are floats because Stripe meters accept fractional quantities; the
// money rounding happens only in CostCents.
func QuantityFor(unit MeterUnit, rec usage.UsageRecord) float64 {
	switch unit {
	case MeterVCPUSecond:
		return rec.VCPUSeconds
	case MeterMemGiBSecond:
		return rec.MemGiBSeconds
	case MeterStorageGiBHour:
		return rec.StorageGiBHours
	case MeterEgressGiB:
		return float64(rec.EgressBytes) / bytesPerGiB
	case MeterGPUSecond:
		return float64(rec.GPUSeconds)
	default:
		return 0
	}
}

// AllMeters is the canonical, stable order of the metered dimensions. The
// metered push iterates this so the per-meter usage events are reported in a
// deterministic order (which keeps the FakeStripe assertions stable).
func AllMeters() []MeterUnit {
	return []MeterUnit{MeterVCPUSecond, MeterMemGiBSecond, MeterStorageGiBHour, MeterEgressGiB, MeterGPUSecond}
}

// CostCents computes the exact integer-cent cost of one usage record under a
// rate table. It accumulates each dimension's contribution in milli-cents
// (float, but bounded and summed before any rounding) and rounds the TOTAL to
// the nearest cent exactly once, so sub-cent per-tick rates never round to zero
// mid-accumulation. This is the function the spend-cap and the credit-drawdown
// both price usage with, so a record costs the same to the cap as to the ledger.
func (r Rates) CostCents(rec usage.UsageRecord) Money {
	milliCents := QuantityFor(MeterVCPUSecond, rec)*r.VCPUSecondMilliCents +
		QuantityFor(MeterMemGiBSecond, rec)*r.MemGiBSecondMilliCents +
		QuantityFor(MeterStorageGiBHour, rec)*r.StorageGiBHourMilliCents +
		QuantityFor(MeterEgressGiB, rec)*r.EgressGiBMilliCents +
		QuantityFor(MeterGPUSecond, rec)*r.GPUSecondMilliCents
	// milli-cents -> cents, round to nearest.
	cents := (milliCents + 500) / 1000
	if cents < 0 {
		cents = 0
	}
	return Money(int64(cents))
}
