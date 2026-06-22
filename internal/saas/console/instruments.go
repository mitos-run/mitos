package console

import (
	"context"
	"net/http"

	"mitos.run/mitos/internal/apierr"
)

// Instruments is the org-scoped proof snapshot the console's instrument-panel
// home renders (spec §9, #276). Every field is MEASURED from the org's own
// telemetry: the console never fabricates head-to-head competitor numbers (the
// integrity rule mirrors docs/saas/pricing.md's no-unverified-numbers rule).
//
// The values realize the README's Pareto thesis from the org's live data:
// warm-claim activate latency (the ~27 ms class), forks served, and CoW density
// — the same #33 metering primitive that also bills.
type Instruments struct {
	OrgID string `json:"org_id"`
	// ActivateP50Millis / ActivateP99Millis: warm-claim activate latency for the
	// org, measured on the same activate path as bench/husk-activate-latency.sh.
	ActivateP50Millis float64 `json:"activate_p50_ms"`
	ActivateP99Millis float64 `json:"activate_p99_ms"`
	// ForksServed is the total number of forks served for the org.
	ForksServed int64 `json:"forks_served"`
	// CoWSavingsBytes is UsedNaive - UsedCoWAware: memory NOT spent because forks
	// share their parent's pages. The headline CoW-density proof.
	CoWSavingsBytes int64 `json:"cow_savings_bytes"`
	// MarginalBytesPerFork is the mean private-dirty (unique) set per fork: the
	// marginal physical cost of one additional fork.
	MarginalBytesPerFork int64 `json:"marginal_bytes_per_fork"`
}

// InstrumentsSource is the org-scoped seam the instrument panel reads. The REAL
// implementation aggregates the #211 usage pipeline and the #33 CoW-aware
// metering report for one org; this slice ships an injectable interface and an
// in-memory tested default. Snapshot MUST return only the named org's measured
// metrics.
type InstrumentsSource interface {
	Snapshot(ctx context.Context, orgID string) (Instruments, error)
}

// handleInstruments returns the caller org's measured proof snapshot. Unlike
// /console/capabilities this is org data, so it requires an authenticated caller.
func (c *Console) handleInstruments(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	snap, err := c.deps.Instruments.Snapshot(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the instrument source could not be read"))
		return
	}
	snap.OrgID = orgID
	writeJSON(w, http.StatusOK, snap)
}
