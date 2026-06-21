package quota

import (
	"context"
	"time"

	"mitos.run/mitos/internal/usage"
)

// LiveCounter is the live-count seam: it reports an org's CURRENTLY running
// sandboxes and their aggregate footprint, read from the controller's running-
// sandbox set. Concurrency and aggregate caps want the live truth (what is
// running right now), which the controller knows authoritatively, so this is the
// preferred source. The #211 usage store (UsageBackedLiveUsage below) is the
// fallback when a direct live count is not wired.
type LiveCounter interface {
	// Count returns the org's live footprint now.
	Count(ctx context.Context, orgID string) (LiveUsage, error)
}

// liveCounterSource adapts a LiveCounter to a LiveUsageSource.
type liveCounterSource struct{ c LiveCounter }

// NewLiveCounterSource wraps a LiveCounter as the enforcer's LiveUsageSource.
func NewLiveCounterSource(c LiveCounter) LiveUsageSource { return liveCounterSource{c: c} }

func (s liveCounterSource) Live(ctx context.Context, orgID string) (LiveUsage, error) {
	return s.c.Count(ctx, orgID)
}

// UsageBackedLiveUsage derives an org's live footprint from the issue #211 usage
// store: it reads the org's records in the most recent window and reports the
// peak concurrent-sandbox count and aggregate vCPU-seconds-derived footprint as a
// best-effort live signal. It is the seam that connects the quota enforcer to the
// metering pipeline so the aggregate cap has a real, auditable input WITHOUT a
// new datapath; the authoritative live count (LiveCounter) is preferred for the
// concurrency cap because the usage store lags one window.
//
// HONEST CAVEAT: the usage store is time-integrated and lags by up to one window,
// so it is a conservative aggregate-cap input, not an instantaneous count. The
// concurrency cap should use LiveCounter where available; this source is the
// fallback and the aggregate-resource input.
type UsageBackedLiveUsage struct {
	Store  usage.UsageStore
	Window time.Duration
	Now    func() time.Time
}

// Live reads the org's records in the trailing window and reports the distinct
// sandbox count as the live concurrency proxy. vCPU/mem/storage are left at the
// caller's spec-derived values; the store carries integrated units, not
// instantaneous levels, so this source intentionally reports only the count it
// can derive honestly.
func (u UsageBackedLiveUsage) Live(ctx context.Context, orgID string) (LiveUsage, error) {
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	win := u.Window
	if win <= 0 {
		win = 2 * usage.DefaultConfig().Window
	}
	from := now().Add(-win)
	recs, err := u.Store.ListRecords(ctx, orgID, from, time.Time{})
	if err != nil {
		return LiveUsage{}, err
	}
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.SandboxID] = true
	}
	return LiveUsage{ConcurrentSandboxes: len(seen)}, nil
}
