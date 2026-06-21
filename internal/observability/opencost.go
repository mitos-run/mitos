package observability

import "math"

// This file is the Layer 2 (OpenCost) per-claim / per-namespace cost-attribution
// reconcile of issue #164. OpenCost reports allocation cost per namespace; mitos
// meters each claim's resource-seconds (cpu-core-seconds and memory-GB-seconds).
// The reconcile prices the metered resource-seconds at the cluster rates and
// compares the sum against the OpenCost-reported namespace spend, flagging drift
// beyond a tolerance. The LIVE pull of OpenCost allocation and the cluster rate
// discovery are cluster-gated (documented in docs/observability.md); the pricing
// and tolerance arithmetic here is pure and unit-tested.

// Rates are the cluster resource prices the reconcile uses to price metered
// resource-seconds: dollars per CPU-core-hour and per memory-GB-hour. A real
// deployment reads these from the OpenCost cluster pricing config (or a custom
// price sheet) so the priced figure uses the same rates OpenCost itself bills.
type Rates struct {
	CPUCoreHour float64
	MemGBHour   float64
}

// ClaimUsage is one claim's metered resource-seconds over the reconcile window:
// cpu-core-seconds and memory-GB-seconds. These come from the mitos metering
// pipeline (the same resource-seconds the usage API and Stripe billing consume),
// so the reconcile checks billing self-consistency: what mitos metered, priced
// at cluster rates, should match what OpenCost attributes to the namespace.
type ClaimUsage struct {
	Claim          string
	CPUCoreSeconds float64
	MemGBSeconds   float64
}

// CostReconcile is the result of reconciling a namespace's OpenCost spend against
// its priced claim resource-seconds: the two figures, their relative drift, and
// whether the drift is within the requested tolerance. An operator alerts on
// WithinTolerance == false, which means a pricing or attribution discrepancy.
type CostReconcile struct {
	Namespace       string
	ReportedCost    float64
	ExpectedCost    float64
	RelativeDrift   float64
	WithinTolerance bool
}

// priceResourceSeconds prices one claim's metered resource-seconds at the
// cluster rates, converting seconds to hours (the rate denominator).
func priceResourceSeconds(u ClaimUsage, rates Rates) float64 {
	return u.CPUCoreSeconds/3600.0*rates.CPUCoreHour + u.MemGBSeconds/3600.0*rates.MemGBHour
}

// ReconcileNamespaceCost prices every claim's resource-seconds in a namespace at
// the cluster rates, sums them, and compares the sum against the
// OpenCost-reported namespace spend. tolerance is the allowed relative drift
// (e.g. 0.05 for 5%). The relative drift is |reported - expected| / expected; a
// zero expected cost reconciles only when the reported cost is also zero
// (otherwise the drift is reported as 1.0, never a divide-by-zero). The result
// is the per-namespace attribution record an operator queries or alerts on.
func ReconcileNamespaceCost(namespace string, reportedCost float64, claims []ClaimUsage, rates Rates, tolerance float64) CostReconcile {
	var expected float64
	for _, c := range claims {
		expected += priceResourceSeconds(c, rates)
	}

	var drift float64
	switch {
	case expected == 0 && reportedCost == 0:
		drift = 0
	case expected == 0:
		drift = 1.0 // nonzero report with no metered usage: full drift
	default:
		drift = math.Abs(reportedCost-expected) / expected
	}

	return CostReconcile{
		Namespace:       namespace,
		ReportedCost:    reportedCost,
		ExpectedCost:    expected,
		RelativeDrift:   drift,
		WithinTolerance: drift <= tolerance,
	}
}
