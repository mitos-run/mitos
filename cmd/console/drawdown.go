package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

// The usage drawdown driver (issue #602) is the missing link between the #211
// usage records and the #212 credit ledger: the controller's collector records
// per-(org, sandbox, window) usage, the billing service can price a record and
// draw it down against the org's prepaid credit, but nothing called Drawdown,
// so hosted credits never moved. This driver periodically replays each org's
// recent FINALIZED usage records through billing.Service.Drawdown. Idempotency
// is the service's contract (the processed-window marker and the ledger entry
// are keyed on the record's (org, sandbox, window)), so replaying the same
// lookback window every tick settles each record exactly once and never
// double-debits. The driver additionally SKIPS already-settled windows before
// pricing them (issue #672): the lookback re-lists them every tick by design,
// and pre-#672 each replay was re-priced and re-counted into settledCents.
//
// SECRET HYGIENE: the driver logs org/record/replay/error COUNTS plus the
// cycle's AGGREGATE settled cents and carried milli-cents (issues #662/#665:
// without the settled amount a zero-settling system looks healthy); never a
// per-org balance, a per-org cost, or any secret.

// defaultDrawdownInterval is how often the driver settles usage against credit
// when MITOS_CONSOLE_DRAWDOWN_INTERVAL is unset and a live usage store is
// configured.
const defaultDrawdownInterval = 5 * time.Minute

// drawdownLookback is how far back each tick lists records. It must comfortably
// exceed the interval so a missed tick (a console restart) still settles the
// records recorded meanwhile; the service's idempotency makes the overlap free.
const drawdownLookback = 2 * time.Hour

// drawdownOrgLister is the narrow org-iteration seam (saas.Store satisfies it).
type drawdownOrgLister interface {
	ListOrgs(ctx context.Context) ([]saas.Organization, error)
}

// drawdownRecordLister is the narrow usage-read seam (usage.UsageStore, in
// production the HTTP store over the controller's internal usage API).
type drawdownRecordLister interface {
	ListRecords(ctx context.Context, orgID string, from, to time.Time) ([]usage.UsageRecord, error)
}

// drawdowner is the narrow settlement seam (billing.Service satisfies it).
// SettledWindowKeys and PruneProcessedWindows carry the issue #672 fix: the
// driver skips already-settled windows BEFORE pricing them and prunes markers
// that fell out of the lookback, so a tick prices only genuinely new usage.
// EnforceSpendCapFromLedger is the issue #615 spend-cap production path: the
// driver evaluates each active org's cap right after settling its usage (the
// only moment period spend can newly cross the cap), so a breached hard cap
// SUSPENDS the org through the shared suspension store the gateway reads,
// instead of the cap being computed by nothing.
type drawdowner interface {
	Drawdown(ctx context.Context, rec usage.UsageRecord) (billing.DrawdownResult, error)
	SettledWindowKeys(ctx context.Context, orgID string, since time.Time) (map[string]bool, error)
	PruneProcessedWindows(ctx context.Context, olderThan time.Time) (int64, error)
	EnforceSpendCapFromLedger(ctx context.Context, orgID string) (bool, error)
}

// drawdownInterval resolves the driver cadence from the raw
// MITOS_CONSOLE_DRAWDOWN_INTERVAL value. Empty defaults to
// defaultDrawdownInterval when the usage store is live (the controller's
// internal usage API is configured) and to 0 (off) when the store is the
// in-memory dev fallback, which holds nothing real to settle. "0" or "off"
// disables explicitly; anything else must parse as a Go duration.
func drawdownInterval(raw string, usageStoreLive bool) (time.Duration, error) {
	switch raw {
	case "":
		if usageStoreLive {
			return defaultDrawdownInterval, nil
		}
		return 0, nil
	case "0", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse MITOS_CONSOLE_DRAWDOWN_INTERVAL: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("MITOS_CONSOLE_DRAWDOWN_INTERVAL is negative: %s", raw)
	}
	return d, nil
}

// drawdownStats is one cycle's counts and aggregate settled money (the only
// things the driver ever logs).
type drawdownStats struct {
	orgs    int
	records int
	drawn   int
	failed  int
	// replayed counts listed records whose window was ALREADY settled by an
	// earlier cycle: skipped before pricing via the processed-window skip set,
	// or (if the skip set missed one) deduplicated by the ledger at settle time.
	// The 2h lookback re-lists them every tick by design; before issue #672 each
	// one was re-priced and its prior credit re-counted into settledCents.
	replayed int
	// settledCents is the cycle's total credit debited across all orgs: the sum
	// of FromCredit over appends that actually LANDED this cycle, never over
	// replays (issue #672). A steadily zero value under nonzero records is the
	// issue #662 signature: usage is metered but no money moves.
	settledCents int64
	// carriedMilli is the sum over orgs of the sub-cent remainder carried into
	// the next cycle (each org's remainder after its last NEWLY settled record;
	// an org whose records were all replays contributes nothing this cycle).
	carriedMilli int64
	// pruned is how many processed-window markers aged out of the lookback and
	// were removed this cycle.
	pruned int64
	// suspended counts orgs suspended this cycle by the post-settle spend-cap
	// evaluation (issue #615): a nonzero value means a hard cap fired and the
	// org now fails closed at the gateway.
	suspended int
}

// runDrawdownOnce settles one cycle: list every org, fetch each org's usage
// records in [now-lookback, now-window) (the upper bound excludes the still
// OPEN current window: the drawdown is idempotent on the record key, so
// settling a window mid-accumulation would lock in a partial cost), and draw
// each record down against the org's credit. A failing org or record is
// counted and skipped, never aborting the rest of the cycle.
func runDrawdownOnce(ctx context.Context, logger *slog.Logger, orgs drawdownOrgLister, store drawdownRecordLister, svc drawdowner, lookback time.Duration, now time.Time) drawdownStats {
	var stats drawdownStats
	list, err := orgs.ListOrgs(ctx)
	if err != nil {
		logger.Error("usage drawdown: list orgs failed", "err", err.Error())
		stats.failed++
		return stats
	}
	stats.orgs = len(list)
	from := now.Add(-lookback)
	to := now.Add(-usage.DefaultConfig().Window)
	for _, org := range list {
		recs, err := store.ListRecords(ctx, org.ID, from, to)
		if err != nil {
			// Error text carries no values; the org id is a non-secret identifier.
			logger.Warn("usage drawdown: list records failed", "org", org.ID, "err", err.Error())
			stats.failed++
			continue
		}
		if len(recs) == 0 {
			continue
		}
		// The lookback re-lists every settled window each tick by design (late
		// or out-of-order settles must still land), so skip the already-settled
		// ones BEFORE pricing: one indexed read per org instead of one priced
		// replay per record (issue #672). Without the skip set the ledger's
		// dedup would still protect the money, but settledCents would count the
		// replays, so an org whose skip set cannot be read is deferred whole to
		// the next tick rather than settled blind.
		settled, err := svc.SettledWindowKeys(ctx, org.ID, from)
		if err != nil {
			logger.Warn("usage drawdown: read settled windows failed", "org", org.ID, "err", err.Error())
			stats.failed++
			continue
		}
		var orgCarried int64
		var orgSettled bool
		for _, rec := range recs {
			stats.records++
			if settled[billing.DrawdownKey(rec.OrgID, rec.SandboxID, rec.Window)] {
				stats.replayed++
				continue
			}
			res, err := svc.Drawdown(ctx, rec)
			if err != nil {
				stats.failed++
				continue
			}
			if res.Replayed {
				// The skip set missed it (settled between the read and now, or by
				// a concurrent writer); the ledger deduplicated it and nothing
				// moved, so it counts as a replay, never into settledCents.
				stats.replayed++
				continue
			}
			stats.drawn++
			stats.settledCents += int64(res.FromCredit)
			// The org's current remainder is the one after its LAST settled record.
			orgCarried = res.CarriedMilliCents
			orgSettled = true
		}
		if orgSettled {
			stats.carriedMilli += orgCarried
		}
		// Spend-cap evaluation for every ACTIVE org (any records in the lookback,
		// settled or replayed): settling is the only moment period spend can newly
		// cross the cap, and the lookback keeps a recently-active org in scope so
		// a cap evaluation that failed one tick retries on the next. Idle orgs
		// skip the scan (an uncapped or inactive org never pays it). A breached
		// HARD cap suspends the org into the SAME durable suspensions table the
		// gateway kill-switch reads, so the org fails closed at the gateway within
		// the gateway's suspension-cache TTL (a few seconds). The suspend is
		// idempotent, so re-evaluating a breached org on later ticks is harmless.
		// Only the org id (non-secret) and counts are logged; never a balance.
		capSuspended, err := svc.EnforceSpendCapFromLedger(ctx, org.ID)
		if err != nil {
			logger.Warn("usage drawdown: spend cap evaluation failed", "org", org.ID, "err", err.Error())
			stats.failed++
			continue
		}
		if capSuspended {
			stats.suspended++
			logger.Info("usage drawdown: hard spend cap breached; org suspended", "org", org.ID)
		}
	}
	// Markers whose window predates the lookback can never be listed again;
	// drop them so the marker set stays bounded. A prune failure only delays
	// the cleanup to the next tick.
	pruned, err := svc.PruneProcessedWindows(ctx, from)
	if err != nil {
		logger.Warn("usage drawdown: prune processed windows failed", "err", err.Error())
		stats.failed++
	} else {
		stats.pruned = pruned
	}
	logger.Info("usage drawdown cycle",
		"orgs", stats.orgs, "records", stats.records, "drawnDown", stats.drawn,
		"replayedRecords", stats.replayed, "errors", stats.failed,
		"settledCents", stats.settledCents, "carriedMilliCents", stats.carriedMilli,
		"prunedMarkers", stats.pruned, "suspendedOrgs", stats.suspended)
	return stats
}

// startDrawdownDriver runs the drawdown loop in the background until ctx is
// canceled, one cycle every interval. The first cycle runs after one interval
// (not at startup) so a crash-looping console does not hammer the usage API.
func startDrawdownDriver(ctx context.Context, logger *slog.Logger, interval time.Duration, orgs drawdownOrgLister, store drawdownRecordLister, svc drawdowner) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runDrawdownOnce(ctx, logger, orgs, store, svc, drawdownLookback, time.Now())
			}
		}
	}()
}
