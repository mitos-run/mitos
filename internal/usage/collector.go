package usage

import (
	"context"
	"fmt"
	"time"

	"mitos.run/mitos/internal/metering"
)

// SampleSource is the collection seam. The real implementation scrapes
// GET /v1/metering from each forkd node on a fixed cadence, converts each node
// report to per-sandbox Samples via SamplesFromReport (tagging org, sandbox,
// node, timestamp), and returns the batch. That multi-node HTTP scrape needs a
// live cluster, so it is a documented follow-up; the aggregation and integration
// are unit-tested against a mock SampleSource (see staticSource in the tests).
type SampleSource interface {
	// Collect returns the samples gathered in one scrape cycle across all nodes.
	Collect(ctx context.Context) ([]Sample, error)
}

// OrgResolver maps a sandbox to its owning organization. A sandbox is created
// through a SandboxClaim, and the claim carries the verified org from the gateway
// request that created it (issue #210), so the owning org of a sandbox is the org
// of the claim that created it. The tested default is a static map (StaticOrgs);
// the real resolver reads the claim -> org label the gateway stamps on the
// SandboxClaim, a documented controller-wiring follow-up.
type OrgResolver interface {
	// OrgFor returns the owning org id of a sandbox and whether it is known. An
	// unknown sandbox (false) is dropped from billable samples but stays in the
	// node reconciliation totals so the physical footprint remains auditable.
	OrgFor(sandboxID string) (string, bool)
}

// StaticOrgs is the tested default OrgResolver: a fixed sandbox -> org map.
type StaticOrgs map[string]string

// OrgFor implements OrgResolver.
func (m StaticOrgs) OrgFor(sandboxID string) (string, bool) {
	org, ok := m[sandboxID]
	return org, ok
}

// ReportSource adapts a function that yields per-node metering reports into a
// SampleSource, applying the org and vCPU resolution and the CoW amortization of
// SamplesFromReport. It is the seam the real multi-node scraper builds on: the
// scraper supplies reportsFn (the HTTP fan-out) and the resolvers, and this
// adapter handles the CoW-correct sample conversion uniformly.
type ReportSource struct {
	// reportsFn returns the current (node, report) pairs for one scrape cycle.
	reportsFn func(ctx context.Context) ([]NodeReport, error)
	orgs      OrgResolver
	vcpus     func(sandboxID string) int32
	now       func() time.Time
}

// NodeReport pairs a node name with its metering report for one scrape.
type NodeReport struct {
	Node   string
	Report metering.Report
}

// NewReportSource builds a ReportSource. vcpus may be nil, in which case every
// sandbox is treated as 1 vCPU (a conservative default until the real
// sandbox-spec lookup is wired). now may be nil, defaulting to time.Now.
func NewReportSource(
	reportsFn func(ctx context.Context) ([]NodeReport, error),
	orgs OrgResolver,
	vcpus func(sandboxID string) int32,
	now func() time.Time,
) *ReportSource {
	if vcpus == nil {
		vcpus = func(string) int32 { return 1 }
	}
	if now == nil {
		now = time.Now
	}
	return &ReportSource{reportsFn: reportsFn, orgs: orgs, vcpus: vcpus, now: now}
}

// Collect fans out the report function, converts each node report to CoW-correct
// per-sandbox samples, and returns the union tagged with a single scrape
// timestamp so all samples in one cycle share an instant.
func (s *ReportSource) Collect(ctx context.Context) ([]Sample, error) {
	reports, err := s.reportsFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect node reports: %w", err)
	}
	at := s.now()
	var out []Sample
	for _, nr := range reports {
		samples, _ := SamplesFromReport(nr.Node, at, nr.Report, s.orgs.OrgFor, s.vcpus)
		out = append(out, samples...)
	}
	return out, nil
}

// MultiSource composes several SampleSources into one, unioning their samples on
// each Collect. It lets the collector meter BOTH the raw-forkd node source and the
// husk-pod source (issue #613) in a single CollectOnce. The two sources cover
// DISJOINT sandbox sets (a sandbox is either a raw-forkd fork tracked by forkd's
// engine or a husk-pod VM tracked by its own pod, never both), so the union is
// exactly the fleet's samples with no double counting; cross-source timestamp skew
// is harmless because Integrate groups by sandbox and no sandbox spans both.
//
// Each sub-source already SKIPS-AND-COUNTS its own unreachable endpoints, so a
// sub-source Collect error is a programmer-level fault, not an unreachable
// node/pod; MultiSource propagates it so the collector logs and retries next tick.
type MultiSource struct {
	sources []SampleSource
}

// NewMultiSource builds a MultiSource over the given sub-sources, in order.
func NewMultiSource(sources ...SampleSource) *MultiSource {
	return &MultiSource{sources: sources}
}

// Collect runs each sub-source's Collect and returns the concatenation of their
// samples. A sub-source error aborts the cycle (returned to the collector), so a
// real fault is visible rather than silently dropping half the bill.
func (m *MultiSource) Collect(ctx context.Context) ([]Sample, error) {
	var out []Sample
	for _, src := range m.sources {
		samples, err := src.Collect(ctx)
		if err != nil {
			// Name the failing sub-source (by type) so the collector's cycle log
			// says WHICH half of the union failed, node or husk.
			return nil, fmt.Errorf("sample source %T: %w", src, err)
		}
		out = append(out, samples...)
	}
	return out, nil
}

// Collector ties a SampleSource to a UsageStore. On each cycle it scrapes
// samples, integrates them into per-(sandbox, window) records, and upserts those
// records. Because the integration is pure and the upsert replaces by key, a
// cycle that re-scrapes overlapping windows (a duplicate scrape, a restart) leaves
// the stored records unchanged: the end-to-end idempotency on (sandbox, window).
type Collector struct {
	src   SampleSource
	store UsageStore
	cfg   Config

	// OnTotals, when set, is called AFTER each cycle's records are upserted with
	// the store's CUMULATIVE per-org Totals (from a store that implements
	// TotalsProvider), so the per-org Prometheus series (issue #164) is driven from
	// the same monotonic cumulative the bill rolls up, NOT from this cycle's pruned
	// sample buffer. It is purely observational. Nil disables it; it is also skipped
	// if the store does not implement TotalsProvider (no cumulative to read).
	OnTotals func(map[string]Totals)

	// buf is the rolling sample buffer. A live scrape yields only the CURRENT
	// instant's level for each sandbox, but the rate units (vCPU-seconds, memory
	// GiB-seconds) are an integral of level over time, which needs at least two
	// samples spanning an interval. So the collector retains recent samples and
	// integrates the buffer each cycle rather than each lone scrape in isolation.
	// Integrate is pure and dedupes by (sandbox, timestamp), so re-upserting the
	// recomputed window record replaces (never adds): the buffer preserves the
	// (sandbox, window) idempotency. Old samples are pruned past the retention
	// horizon so the buffer is bounded and settled windows are not re-walked.
	buf []Sample
}

// NewCollector builds a collector over a sample source and a usage store.
func NewCollector(src SampleSource, store UsageStore, cfg Config) *Collector {
	if cfg.Window <= 0 {
		cfg = DefaultConfig()
	}
	return &Collector{src: src, store: store, cfg: cfg}
}

// retention is how far back the sample buffer is kept. It spans the current
// window plus MaxHold so the leading edge of the current window can still be
// integrated against the trailing sample of the previous one, while settled
// windows fall out of the buffer and are never re-walked.
func (c *Collector) retention() time.Duration { return c.cfg.Window + c.cfg.MaxHold }

// CollectOnce runs a single scrape-buffer-integrate-upsert cycle. It is the unit
// the Run loop calls on each tick and the unit the idempotency tests drive
// directly. It appends the scrape to the rolling buffer, prunes samples past the
// retention horizon, and integrates the buffer so the rate units accumulate
// across cycles while staying idempotent on (sandbox, window).
func (c *Collector) CollectOnce(ctx context.Context) error {
	samples, err := c.src.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect samples: %w", err)
	}
	c.buf = append(c.buf, samples...)
	c.pruneBuffer()
	recs := Integrate(c.buf, c.cfg)
	for _, r := range recs {
		if err := c.store.UpsertRecord(ctx, r); err != nil {
			return fmt.Errorf("upsert usage record (sandbox %s window %s): %w", r.SandboxID, r.Window, err)
		}
	}
	if c.OnTotals != nil {
		if tp, ok := c.store.(TotalsProvider); ok {
			c.OnTotals(tp.TotalsByOrg())
		}
	}
	return nil
}

// pruneBuffer drops samples older than the retention horizon, measured from the
// newest sample in the buffer (a wall clock independent of the test clock). It
// keeps the buffer bounded and stops settled windows from being re-integrated on
// every cycle. It is a no-op on an empty buffer.
func (c *Collector) pruneBuffer() {
	if len(c.buf) == 0 {
		return
	}
	var newest time.Time
	for _, s := range c.buf {
		if s.Timestamp.After(newest) {
			newest = s.Timestamp
		}
	}
	cutoff := newest.Add(-c.retention())
	kept := c.buf[:0]
	for _, s := range c.buf {
		if !s.Timestamp.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	c.buf = kept
}

// Run scrapes on a fixed cadence until the context is canceled. Each tick is a
// CollectOnce; a transient collect error is logged-and-skipped by the caller (Run
// returns the context error on cancel, and CollectOnce errors stop the loop only
// if the caller chooses to treat them as fatal). The cadence equals the config
// Window so each cycle covers one window of fresh samples.
func (c *Collector) Run(ctx context.Context, cadence time.Duration, onError func(error)) error {
	if cadence <= 0 {
		cadence = c.cfg.Window
	}
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.CollectOnce(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
