package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"mitos.run/mitos/internal/metering"
)

// meteringPath is the forkd operational endpoint that serves the node-level
// CoW-aware metering Report as JSON. It is node-scoped operational data (the same
// access class as /metrics and /healthz), not per-sandbox traffic, so it carries
// no per-sandbox bearer auth and returns only ids, template names, and byte/second
// counts, never secret values.
const meteringPath = "/v1/metering"

// NodeEndpoint is one forkd node the live source scrapes: its name (a hostname,
// never a secret) and its HTTP sandbox API endpoint (host:port). It is the narrow
// shape the controller's NodeRegistry is adapted to, so internal/usage does NOT
// import internal/controller (which would be an import cycle: the controller
// wires the usage collector).
type NodeEndpoint struct {
	Name         string
	HTTPEndpoint string
}

// NodeLister is the import-cycle-avoiding seam over the controller's NodeRegistry:
// it yields the current forkd nodes to scrape. The controller wires a tiny
// concrete adapter around its *NodeRegistry (ListNodes -> ListNodeEndpoints) so
// this package depends only on the interface, never on internal/controller.
type NodeLister interface {
	// ListNodeEndpoints returns the forkd nodes to scrape this cycle. An empty
	// slice (no nodes) yields no samples, not an error.
	ListNodeEndpoints() []NodeEndpoint
}

// NodeRegistrySource is the live, multi-node SampleSource (issue #164). On each
// Collect it lists the forkd nodes from a NodeLister, HTTP GETs GET /v1/metering
// from each node's forkd HTTP endpoint, parses the internal/metering Report, and
// converts every per-sandbox row to an org-tagged Sample via SamplesFromReport
// (the same CoW-correct conversion the offline ReportSource uses).
//
// ROBUSTNESS: a node that is unreachable, errors, or returns a non-200 is SKIPPED
// and counted (SkippedNodes), never failing the whole Collect. One bad node must
// not zero out the others, so node loss degrades gracefully instead of dropping
// the bill for the healthy fleet.
//
// SECRET HYGIENE: only sandbox ids, org ids, byte counts, and seconds flow
// through a Sample; argv, env, file bytes, and tokens never touch this path. The
// HTTP client, the node lister, the org resolver, and the clock are all injected
// seams so the source is unit-testable against an httptest server without a real
// cluster.
type NodeRegistrySource struct {
	nodes  NodeLister
	orgs   OrgResolver
	vcpus  func(sandboxID string) int32
	client *http.Client
	scheme string
	now    func() time.Time

	// skipped counts nodes skipped (unreachable/error/non-200) across the source's
	// lifetime. It is a process counter the wiring can surface as a metric or log a
	// count from; it never carries node identity or error text.
	skipped atomic.Int64
}

// NewNodeRegistrySource builds the live multi-node source. vcpus may be nil (every
// sandbox treated as 1 vCPU until the sandbox-spec lookup is wired). client may be
// nil (a default client with a bounded timeout is used). scheme may be empty
// (defaults to "http"; the controller passes "https" once forkd's operational mux
// is TLS). now may be nil (defaults to time.Now).
func NewNodeRegistrySource(
	nodes NodeLister,
	orgs OrgResolver,
	vcpus func(sandboxID string) int32,
	client *http.Client,
	scheme string,
	now func() time.Time,
) *NodeRegistrySource {
	if vcpus == nil {
		vcpus = func(string) int32 { return 1 }
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if scheme == "" {
		scheme = "http"
	}
	if now == nil {
		now = time.Now
	}
	return &NodeRegistrySource{nodes: nodes, orgs: orgs, vcpus: vcpus, client: client, scheme: scheme, now: now}
}

// Collect scrapes every known forkd node once, converts each node's metering
// report to org-tagged per-sandbox Samples, and returns the union tagged with a
// single scrape timestamp so all samples in one cycle share an instant (the
// property Integrate's windowing relies on). A node that is unreachable, errors,
// or returns a non-200 is skipped and counted; Collect itself only returns an
// error for a programmer-level fault, never for an unreachable node.
func (s *NodeRegistrySource) Collect(ctx context.Context) ([]Sample, error) {
	// Refresh the org resolver's per-cycle snapshot ONCE so the husk pods are listed
	// a single time for the whole cycle, not once per sandbox (an O(n^2) blow-up at
	// fleet scale). A non-refreshable resolver (the test static map) is unaffected.
	if rl, ok := s.orgs.(RefreshableLookup); ok {
		rl.Refresh()
	}
	at := s.now()
	var out []Sample
	for _, node := range s.nodes.ListNodeEndpoints() {
		report, ok := s.scrape(ctx, node)
		if !ok {
			// Skip-and-count: one bad node must not fail the whole collection or
			// zero out the healthy nodes. No node identity or error text is carried
			// past the counter (the wiring logs a count at most).
			s.skipped.Add(1)
			continue
		}
		// regionOf is nil: the NodeRegistrySource path (forkd-managed engine
		// nodes) has no placement concept, unlike the husk-pod path
		// (HuskSource), so every sample here carries an empty Region.
		samples, _ := SamplesFromReport(node.Name, at, report, s.orgs.OrgFor, s.vcpus, nil)
		out = append(out, samples...)
	}
	return out, nil
}

// scrapeTimeout bounds a single node scrape regardless of the injected HTTP
// client's own Timeout. A custom TLSClient (the controller's mTLS client) carries
// no http.Client.Timeout, so the per-request context deadline here is what stops a
// hung node from stalling the whole cycle behind it.
const scrapeTimeout = 8 * time.Second

// scrape GETs GET /v1/metering from one node and decodes the Report. It returns
// ok=false (not an error) on any reachability, status, or decode failure so the
// caller can skip-and-count the node. It carries no secret: the request has no
// body and the response is the metering Report (ids, template names, byte/second
// counts only). It applies a bounded per-request deadline derived from ctx so the
// timeout holds even when a custom client (no http.Client.Timeout) is injected.
func (s *NodeRegistrySource) scrape(ctx context.Context, node NodeEndpoint) (metering.Report, bool) {
	var report metering.Report
	if node.HTTPEndpoint == "" {
		return report, false
	}
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	url := fmt.Sprintf("%s://%s%s", s.scheme, node.HTTPEndpoint, meteringPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return report, false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return report, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return report, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return report, false
	}
	return report, true
}

// SkippedNodes returns the cumulative count of node scrapes skipped because the
// node was unreachable, errored, or returned a non-200. It is the live-source
// degradation signal the wiring exposes (a metric or a logged count); it never
// carries node identity or error text.
func (s *NodeRegistrySource) SkippedNodes() int64 { return s.skipped.Load() }
