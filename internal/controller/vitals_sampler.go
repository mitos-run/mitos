package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"mitos.run/mitos/internal/usage"
)

// nodeVitalsPath is the forkd operational endpoint that serves the node-level
// labeled guest vitals batch. Like /v1/metering it is node-scoped operator
// telemetry (the same access class as /metrics and /healthz), NOT per-sandbox
// traffic, so the sampler scrapes it without a per-sandbox bearer token and it
// returns only the control-plane labels and numeric guest vitals, never a secret.
const nodeVitalsPath = "/v1/vitals/node"

// scrapedVitals mirrors the daemon NodeVitals JSON wire shape so the controller
// can decode the scrape without importing internal/daemon (which would pull the
// firecracker/vsock host stack into the controller binary). Only the fields the
// sampler needs are decoded.
type scrapedVitals struct {
	Sandboxes []scrapedVitalsEntry `json:"sandboxes"`
	Skipped   int                  `json:"skipped"`
}

// scrapedVitalsEntry is one sandbox's vitals from the node report. SandboxID is
// used ONLY to resolve the trusted org label; it never becomes a metric label.
// Pool is a control-plane object name (the SandboxPool), a bounded metric label.
// The numeric vitals are the only values exported; no process command, argv,
// pid, or env field is decoded or exported. The node endpoint serves a NUMERIC
// process_count (never the per-process table), so this struct decodes that
// number directly and never holds any process object.
type scrapedVitalsEntry struct {
	SandboxID string `json:"sandbox_id"`
	Pool      string `json:"pool"`
	Vitals    struct {
		StealFraction      float64 `json:"steal_fraction"`
		MemUsedKB          uint64  `json:"mem_used_kb"`
		BalloonReclaimedKB uint64  `json:"balloon_reclaimed_kb"`
		// ProcessCount is the LENGTH of the guest's process table, decoded as a
		// number. The node endpoint never sends a per-process command/argv/pid/state,
		// and this struct has no field to receive one, so nothing can leak into a metric.
		ProcessCount int `json:"process_count"`
	} `json:"vitals"`
}

// vitalsKey is the (org, pool) bucket a guest's vitals aggregate into. Both are
// bounded, trusted control-plane values, so the series count is bounded by
// (org x pool), never by the sandbox count.
type vitalsKey struct {
	org  string
	pool string
}

// vitalsAgg is the per-(org, pool) aggregate the sampler publishes. cpu_steal is
// the MAX across the bucket (the worst-starved sandbox); balloon, used, and
// process_count are SUMs (the additive org/pool fleet footprint). See
// vitalsMetrics for why each aggregation was chosen.
type vitalsAgg struct {
	maxStealPercent float64
	sumBalloonBytes uint64
	sumUsedBytes    uint64
	sumProcessCount int
}

// aggregateVitals folds the scraped per-sandbox vitals into per-(org, pool)
// aggregates. It resolves each sandbox's org via the trusted resolver; a sandbox
// with no resolvable org is SKIPPED (returned in the unattributed count), never
// attributed to a guessed or empty org, so an unbillable/unknown sandbox cannot
// pollute another org's series. It is PURE and unit-testable without a cluster.
//
// SECRET HYGIENE: it reads only the numeric vitals and the numeric process_count;
// it never touches a process command, argv, pid, or env (the node endpoint does
// not even send one). The only strings it emits are the org and pool labels, both
// bounded control-plane values.
func aggregateVitals(entries []scrapedVitalsEntry, orgFor func(sandboxID string) (string, bool)) (map[vitalsKey]vitalsAgg, int) {
	out := map[vitalsKey]vitalsAgg{}
	unattributed := 0
	for _, e := range entries {
		org, ok := orgFor(e.SandboxID)
		if !ok || org == "" {
			// No trusted org: do not attribute this sandbox to any series.
			unattributed++
			continue
		}
		k := vitalsKey{org: org, pool: e.Pool}
		a := out[k]
		stealPercent := e.Vitals.StealFraction * 100.0
		if stealPercent > a.maxStealPercent {
			a.maxStealPercent = stealPercent
		}
		a.sumBalloonBytes += e.Vitals.BalloonReclaimedKB * 1024
		a.sumUsedBytes += e.Vitals.MemUsedKB * 1024
		a.sumProcessCount += e.Vitals.ProcessCount
		out[k] = a
	}
	return out, unattributed
}

// VitalsSamplerRunnable is the manager Runnable that samples the guests' live
// health on a fixed cadence and publishes per (org, pool) Prometheus gauges
// (issue #164 Phase 1.a). On each tick it scrapes every forkd node's
// GET /v1/vitals/node via the NodeRegistry, attributes each sandbox to its org
// via the TRUSTED mitos.run/org husk-pod label (the same resolver the usage
// scraper uses), aggregates per (org, pool), and Sets the guestVitalsMetrics
// gauges.
//
// It is OFF by default (gated by the --vitals-sampler flag in cmd/controller) so
// a self-host deployment that does not want guest telemetry is unaffected; hosted
// turns it on.
//
// SECRET HYGIENE: only sandbox ids (for org resolution, never a label), org ids,
// pool names, and numeric vitals flow; argv, env, process command lines, pids,
// and tokens never touch this path. A node that is unreachable or errors is
// skipped and counted, never failing the cycle.
type VitalsSamplerRunnable struct {
	Registry   *NodeRegistry
	Client     client.Client
	Cadence    time.Duration
	HTTPScheme string
	// TLSClient, when set, scrapes forkd over https with the controller's mTLS
	// config. Nil means a plain client (the forkd operational mux is http today).
	TLSClient *http.Client
}

// Start runs the sampler loop until ctx is canceled. It builds the trusted-label
// org resolver over the same husk-pod lookup the usage collector uses, ticks on
// Cadence, and on each tick scrapes the fleet and republishes the gauges. A
// transient cycle is best-effort: an unreachable node is skipped and counted, and
// the loop only exits on context cancel.
func (s *VitalsSamplerRunnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("vitals-sampler")

	cadence := s.Cadence
	if cadence <= 0 {
		cadence = usage.DefaultConfig().Window
	}
	lookup := &PodLabelLookup{Client: s.Client}
	resolver := usage.NewLabelOrgResolver(lookup)
	httpClient := s.TLSClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	scheme := s.HTTPScheme
	if scheme == "" {
		scheme = "http"
	}

	logger.Info("vitals sampler started", "cadence", cadence.String())
	s.cycle(ctx, logger, lookup, resolver, httpClient, scheme)
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.cycle(ctx, logger, lookup, resolver, httpClient, scheme)
		}
	}
}

// cycle scrapes every node once, aggregates per (org, pool), and republishes the
// gauges. It refreshes the husk-pod label snapshot ONCE per cycle (like the usage
// scraper) so org resolution is a map read, not a per-sandbox List. It logs only
// COUNTS (skipped nodes, unreachable guests, unattributed sandboxes), never node
// identity, sandbox id, or error text.
func (s *VitalsSamplerRunnable) cycle(ctx context.Context, logger logr.Logger, lookup *PodLabelLookup, resolver *usage.LabelOrgResolver, httpClient *http.Client, scheme string) {
	lookup.Refresh()

	var all []scrapedVitalsEntry
	skippedNodes, unreachableGuests := 0, 0
	for _, n := range s.Registry.ListNodes() {
		if n.HTTPEndpoint == "" {
			continue
		}
		nv, ok := s.scrape(ctx, httpClient, scheme, n.HTTPEndpoint)
		if !ok {
			// One bad node must not blind the operator to the healthy fleet.
			skippedNodes++
			continue
		}
		unreachableGuests += nv.Skipped
		all = append(all, nv.Sandboxes...)
	}

	aggs, unattributed := aggregateVitals(all, resolver.OrgFor)
	guestVitalsMetrics.observe(aggs)

	if skippedNodes > 0 || unreachableGuests > 0 || unattributed > 0 {
		logger.V(1).Info("vitals sample degraded",
			"skippedNodes", skippedNodes,
			"unreachableGuests", unreachableGuests,
			"unattributedSandboxes", unattributed)
	}
}

// scrapeTimeout bounds a single node scrape regardless of the client timeout, so
// a hung node cannot stall the whole cycle behind it.
const vitalsScrapeTimeout = 8 * time.Second

// scrape GETs /v1/vitals/node from one node and decodes the report. It returns
// ok=false (not an error) on any reachability, status, or decode failure so the
// caller can skip-and-count the node. The request has no body and the response
// carries only labels and numeric vitals, never a secret.
func (s *VitalsSamplerRunnable) scrape(ctx context.Context, httpClient *http.Client, scheme, endpoint string) (scrapedVitals, bool) {
	var nv scrapedVitals
	ctx, cancel := context.WithTimeout(ctx, vitalsScrapeTimeout)
	defer cancel()
	url := fmt.Sprintf("%s://%s%s", scheme, endpoint, nodeVitalsPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nv, false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nv, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nv, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&nv); err != nil {
		return nv, false
	}
	return nv, true
}
