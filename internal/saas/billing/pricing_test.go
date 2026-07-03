package billing

import (
	"testing"
	"time"

	"mitos.run/mitos/internal/usage"
)

// TestFromPriceListReconcilesUsagePlaceholder asserts the #211 usage.PriceList
// placeholder reconciles into the structured billing Rates (the same magnitude,
// re-expressed in milli-cents), so the display-cost estimate and the real
// billing rates derive from one table and never drift.
func TestFromPriceListReconcilesUsagePlaceholder(t *testing.T) {
	r := FromPriceList(usage.DefaultPriceList())
	want := DefaultRates()
	if !approxRates(r, want) {
		t.Errorf("FromPriceList(DefaultPriceList) = %+v, want %+v", r, want)
	}
}

func approxRates(a, b Rates) bool {
	const eps = 1e-6
	d := func(x, y float64) bool {
		if x-y > eps || y-x > eps {
			return false
		}
		return true
	}
	return d(a.VCPUSecondMilliCents, b.VCPUSecondMilliCents) &&
		d(a.MemGiBSecondMilliCents, b.MemGiBSecondMilliCents) &&
		d(a.StorageGiBHourMilliCents, b.StorageGiBHourMilliCents) &&
		d(a.EgressGiBMilliCents, b.EgressGiBMilliCents) &&
		d(a.GPUSecondMilliCents, b.GPUSecondMilliCents)
}

// TestCostCentsRoundsOnceAtEnd asserts sub-cent per-tick rates accumulate
// before a single final rounding, so a record of many cheap units is not
// rounded to zero mid-accumulation.
func TestCostCentsRoundsOnceAtEnd(t *testing.T) {
	rates := DefaultRates()
	// 1,000,000 vCPU-seconds at 1.28 milli-cents each = 1,280,000 milli-cents =
	// 1280 cents = $12.80. A per-tick round would have floored each tick to 0.
	rec := usage.UsageRecord{OrgID: "o1", SandboxID: "s1", Window: time.Unix(0, 0).UTC(), VCPUSeconds: 1_000_000}
	got := rates.CostCents(rec)
	if got != Money(1280) {
		t.Errorf("CostCents = %d cents, want 1280", int64(got))
	}
}

// TestQuantityForEgressIsGiB asserts egress quantity is reported in GiB, the
// meter's natural unit, from the record's byte counter.
func TestQuantityForEgressIsGiB(t *testing.T) {
	rec := usage.UsageRecord{EgressBytes: int64(2) << 30} // 2 GiB.
	if q := QuantityFor(MeterEgressGiB, rec); q != 2 {
		t.Errorf("egress quantity = %v GiB, want 2", q)
	}
}

// TestCostMilliCentsPricesSubCentWindows asserts the milli-cent pricer keeps
// the sub-cent cost of a realistic one-minute window instead of rounding it to
// zero: 60 vCPU-seconds at the default 1.28 milli-cents/vCPU-s is 76.8
// milli-cents, which CostCents rounds to 0 but CostMilliCents must report as
// 77 (nearest milli-cent). This is the pricing half of issue #662.
func TestCostMilliCentsPricesSubCentWindows(t *testing.T) {
	rates := DefaultRates()
	rec := usage.UsageRecord{OrgID: "o1", SandboxID: "s1", Window: time.Unix(0, 0).UTC(), VCPUSeconds: 60}
	if got := rates.CostMilliCents(rec); got != 77 {
		t.Errorf("CostMilliCents = %d, want 77 (76.8 rounded to nearest)", got)
	}
	if got := rates.CostCents(rec); got != 0 {
		t.Errorf("CostCents = %d, want 0 (the whole-cent view of a sub-cent window)", int64(got))
	}
}

// TestCostCentsDerivesFromMilliCents asserts the two pricers agree: the cent
// cost is always the milli-cent cost rounded half up, so the spend estimate the
// console shows and the drawdown's accumulator never disagree on a record.
func TestCostCentsDerivesFromMilliCents(t *testing.T) {
	rates := DefaultRates()
	recs := []usage.UsageRecord{
		{VCPUSeconds: 60},
		{VCPUSeconds: 3600, MemGiBSeconds: 7200, EgressBytes: int64(1) << 30},
		{VCPUSeconds: 1_000_000},
		{GPUSeconds: 7},
	}
	for i, rec := range recs {
		m := rates.CostMilliCents(rec)
		want := Money((m + 500) / 1000)
		if got := rates.CostCents(rec); got != want {
			t.Errorf("rec %d: CostCents = %d, want %d ((%d milli + 500) / 1000)", i, int64(got), int64(want), m)
		}
	}
}
