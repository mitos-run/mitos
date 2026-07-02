package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"mitos.run/mitos/internal/usage"
)

// UsageCollectorRunnable is the manager Runnable that runs the live multi-node
// metering scraper on a fixed cadence (issue #164). On each tick it scrapes every
// forkd node's GET /v1/metering via the NodeRegistry, attributes each sandbox to
// its org via the TRUSTED mitos.run/org husk-pod label, integrates the samples
// idempotently into per-(org, sandbox, window) UsageRecords, upserts them into the
// usage store, and publishes the per-org Prometheus series from the store's
// CUMULATIVE per-org Totals (the same monotonic number the bill rolls up).
//
// It is OFF by default (gated by the --usage-collector flag in cmd/controller) so
// a self-host deployment that does not want metering is unaffected; hosted turns
// it on. The store is the in-memory store for now (a durable Postgres store is the
// documented follow-up, keyed by the same (org, sandbox, window) idempotency key).
//
// SECRET HYGIENE: only sandbox ids, org ids, byte counts, and seconds flow; argv,
// env, file bytes, and tokens never touch this path. A node that is unreachable or
// errors is skipped and counted, never failing the cycle (one bad node must not
// zero out the bill for the healthy fleet).
type UsageCollectorRunnable struct {
	Registry   *NodeRegistry
	Client     client.Client
	Cadence    time.Duration
	HTTPScheme string
	// TLSClient, when set, is the HTTP client carrying the controller's mTLS
	// config used to scrape forkd over https. Nil means a plain client (the forkd
	// operational mux is http today; https is the documented follow-up).
	TLSClient *http.Client

	// Store is the usage store the integrated records land in. Exposed so a test
	// (or a future durable store) can inspect or substitute it; defaults to an
	// in-memory store.
	Store usage.UsageStore
}

// Start runs the collector loop until ctx is canceled. It builds the live
// SampleSource over the NodeRegistry and the trusted-label OrgResolver, wires the
// per-org metric publisher as the records observer, and ticks on Cadence (default
// the usage Config window). A transient cycle error is logged and the loop
// continues; the loop only exits on context cancel.
func (u *UsageCollectorRunnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("usage-collector")

	cfg := usage.DefaultConfig()
	cadence := u.Cadence
	if cadence <= 0 {
		cadence = cfg.Window
	}

	// The forkd node source meters RAW-forkd sandboxes (forks forkd's engine
	// tracks). In production every sandbox VM instead runs inside its own husk pod,
	// which forkd's engine never tracks, so the husk source meters those by scraping
	// each claimed husk pod's own in-pod GET /v1/metering (issue #613). A sandbox is
	// either a raw-forkd fork or a husk-pod VM, never both, so the two sources cover
	// disjoint sandbox sets and their union is the whole fleet with no double count.
	nodeSource := usage.NewNodeRegistrySource(
		RegistryNodeLister{Registry: u.Registry},
		usage.NewLabelOrgResolver(&PodLabelLookup{Client: u.Client}),
		nil, // 1 vCPU per sandbox until the sandbox-spec vCPU lookup is wired
		u.TLSClient,
		u.HTTPScheme,
		nil,
	)
	huskSource := usage.NewHuskSource(
		&HuskPodScrapeLister{Client: u.Client},
		nil, // 1 vCPU per husk-pod VM until the sandbox-spec vCPU lookup is wired
		u.TLSClient,
		u.HTTPScheme,
		nil,
	)
	source := usage.NewMultiSource(nodeSource, huskSource)

	store := u.Store
	if store == nil {
		store = usage.NewMemUsageStore()
		u.Store = store
	}

	collector := usage.NewCollector(source, store, cfg)
	// Drive the per-org series from the store's cumulative per-org Totals (the same
	// number the bill rolls up), so the metric is monotonic and never drops a known
	// org that was quiet this cycle. The in-memory store implements TotalsProvider.
	collector.OnTotals = usageMetrics.Observe

	logger.Info("usage collector started", "cadence", cadence.String())
	// Run an immediate first cycle so the metric is populated without waiting a
	// full cadence, then tick.
	u.cycle(ctx, logger, collector, nodeSource, huskSource)
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			u.cycle(ctx, logger, collector, nodeSource, huskSource)
		}
	}
}

// cycle runs one scrape-integrate-upsert-publish cycle and logs a COUNT of skipped
// nodes and skipped husk pods (never node/pod identity or error text) on a
// transient cycle error. The two skip counters are the degradation signals an
// operator alerts on.
func (u *UsageCollectorRunnable) cycle(ctx context.Context, logger logr.Logger, collector *usage.Collector, nodeSource *usage.NodeRegistrySource, huskSource *usage.HuskSource) {
	if err := collector.CollectOnce(ctx); err != nil {
		// The cycle error carries only ids/window text from the store path, never a
		// secret; still log it sparingly.
		logger.Error(err, "usage collection cycle failed",
			"skippedNodes", nodeSource.SkippedNodes(),
			"skippedHuskPods", huskSource.SkippedPods())
		return
	}
	if skipped := nodeSource.SkippedNodes(); skipped > 0 {
		logger.V(1).Info("usage collection skipped unreachable nodes", "skippedNodesCumulative", skipped)
	}
	if skipped := huskSource.SkippedPods(); skipped > 0 {
		logger.V(1).Info("usage collection skipped unreachable husk pods", "skippedHuskPodsCumulative", skipped)
	}
}

// usageMetrics is the per-org usage Prometheus view, registered ONCE on the
// controller-runtime metrics registry so the series appear on the controller's
// /metrics endpoint alongside the other controller metrics. The series carry an
// org label only (no sandbox-id cardinality, no secrets). They are populated by
// the collector's OnTotals hook from the store's CUMULATIVE per-org Totals (the
// same number the bill rolls up), so the dashboard number and the billed number
// are identical and the series is monotonic.
var usageMetrics = usage.NewMetrics()

func init() {
	usageMetrics.MustRegister(ctrlmetrics.Registry)
}
