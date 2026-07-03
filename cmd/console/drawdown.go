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
// is the service's contract (the ledger entry is keyed on the record's
// (org, sandbox, window)), so replaying the same lookback window every tick
// settles each record exactly once and never double-debits.
//
// SECRET HYGIENE: the driver logs org/record/error COUNTS plus the cycle's
// AGGREGATE settled cents and carried milli-cents (issues #662/#665: without
// the settled amount a zero-settling system looks healthy); never a per-org
// balance, a per-org cost, or any secret.

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
type drawdowner interface {
	Drawdown(ctx context.Context, rec usage.UsageRecord) (billing.DrawdownResult, error)
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
	// settledCents is the cycle's total credit debited across all orgs (the sum
	// of every record's FromCredit). A steadily zero value under nonzero records
	// is the issue #662 signature: usage is metered but no money moves.
	settledCents int64
	// carriedMilli is the sum over orgs of the sub-cent remainder carried into
	// the next cycle (each org's remainder after its last settled record).
	carriedMilli int64
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
		var orgCarried int64
		for _, rec := range recs {
			stats.records++
			res, err := svc.Drawdown(ctx, rec)
			if err != nil {
				stats.failed++
				continue
			}
			stats.drawn++
			stats.settledCents += int64(res.FromCredit)
			// The org's current remainder is the one after its LAST settled record
			// (a replayed record reports the untouched current remainder).
			orgCarried = res.CarriedMilliCents
		}
		stats.carriedMilli += orgCarried
	}
	logger.Info("usage drawdown cycle",
		"orgs", stats.orgs, "records", stats.records, "drawnDown", stats.drawn, "errors", stats.failed,
		"settledCents", stats.settledCents, "carriedMilliCents", stats.carriedMilli)
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
