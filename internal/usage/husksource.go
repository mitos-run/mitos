package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"mitos.run/mitos/internal/metering"
)

// huskScrapeConcurrency bounds the number of in-flight husk-pod scrapes per
// cycle (issue #682, was #656). Fleet size is per-VM (one husk pod per
// sandbox), so a sequential scrape serialized N unreachable pods into
// N x scrapeTimeout during a partial outage, delaying metering for the healthy
// fleet. With the bounded pool the cycle duration is set by the slowest pool
// lane, not the fleet size, while the bound keeps the controller from opening
// an unbounded number of connections into tenant pod networks at once.
const huskScrapeConcurrency = 8

// HuskPod is one claimed, org-labeled husk pod the HuskSource scrapes. VMID is
// the pod's vm-id (the pod NAME: the id the pod reports as its single metering
// Sample AND the id the controller maps to an org); APIID is the API-VISIBLE
// sandbox id from the TRUSTED controller-stamped mitos.run/claim label (the
// claiming Sandbox's name, the id the customer saw; issue #663), NEVER from
// anything the pod returns; OrgID is the owning org from the TRUSTED
// mitos.run/org pod label, NEVER from anything the pod returns; Endpoint is the
// pod's in-pod sandbox HTTP endpoint (podIP:port) serving GET /v1/metering.
type HuskPod struct {
	VMID     string
	APIID    string
	OrgID    string
	Endpoint string
}

// HuskPodLister yields the claimed, org-labeled husk pods to scrape this cycle.
// It is the import-cycle-avoiding seam over the controller's pod cache (the
// controller wires the usage collector, so internal/usage must not import
// internal/controller): the controller's concrete adapter lists mitos.run/husk
// pods carrying a non-empty trusted mitos.run/org label and returns each pod's
// vm-id, org, and podIP:port. An empty slice with a nil error means genuinely no
// pods; a listing FAILURE returns the error so the cycle fails loudly instead of
// silently under-metering (an API/RBAC fault must never read as an empty fleet).
// The context carries the collector's cancellation into the Kubernetes List.
type HuskPodLister interface {
	ListHuskPods(ctx context.Context) ([]HuskPod, error)
}

// HuskSource is the live SampleSource that meters husk-pod sandboxes (issue #613).
// In production every sandbox VM runs inside its OWN husk pod, which forkd's
// engine never tracks, so the NodeRegistrySource reports nothing for them and
// usage_records stays empty. On each Collect this source lists the claimed
// org-labeled husk pods, scrapes each pod's GET /v1/metering (the pod's own
// single-VM report), and converts it to an org-tagged Sample.
//
// TRUST: the org is taken from the pod's TRUSTED mitos.run/org label (carried by
// the lister), NEVER from the report the pod returns; a pod is untrusted for org.
// Defense in depth: only the pod's OWN vm-id sample is accepted, so a pod can bill
// only its own vm-id/org, never another pod's.
//
// ROBUSTNESS: a pod that is unreachable, errors, returns a non-200, or fails to
// decode is SKIPPED and counted (SkippedPods), never failing the whole Collect.
// One bad pod must not zero out the bill for the healthy fleet.
//
// SECRET HYGIENE: only the vm-id, org id, and byte/second counts flow through a
// Sample; the scraped report is secret-free and the org label is an id, never a
// secret. The HTTP client, the pod lister, the vCPU function, and the clock are
// all injected seams so the source is unit-testable against an httptest server
// with no real cluster.
type HuskSource struct {
	pods   HuskPodLister
	vcpus  func(sandboxID string) int32
	client *http.Client
	scheme string
	now    func() time.Time

	// skipped counts pods skipped (unreachable/error/non-200/decode) across the
	// source's lifetime. It is a process counter the wiring surfaces as a metric or
	// logged count; it never carries pod identity or error text.
	skipped atomic.Int64
}

// NewHuskSource builds the live husk-pod source. vcpus may be nil (every sandbox
// treated as 1 vCPU, matching the collector default). client may be nil (a default
// client with a bounded timeout is used). scheme may be empty (defaults to "http").
// now may be nil (defaults to time.Now).
func NewHuskSource(
	pods HuskPodLister,
	vcpus func(sandboxID string) int32,
	client *http.Client,
	scheme string,
	now func() time.Time,
) *HuskSource {
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
	return &HuskSource{pods: pods, vcpus: vcpus, client: client, scheme: scheme, now: now}
}

// Collect scrapes every claimed org-labeled husk pod once and returns the union
// of org-tagged Samples tagged with a single scrape timestamp so all samples in
// one cycle share an instant (the property Integrate's windowing relies on). A
// pod that is unreachable, errors, or returns a non-200 is skipped and counted;
// Collect itself never returns an error for an unreachable pod.
//
// The scrapes fan out over a bounded worker pool (huskScrapeConcurrency; issue
// #682) so the cycle duration is set by the slowest pool lane, never by the
// fleet size: N unreachable pods no longer serialize into N x scrapeTimeout.
// The single shared scrape timestamp and the skip-and-count semantics are
// unchanged; results keep the lister's pod order. Collect itself is meant for
// ONE caller (the collector loop): the injected vcpus func must be safe for
// concurrent use (the nil default is).
func (s *HuskSource) Collect(ctx context.Context) ([]Sample, error) {
	at := s.now()
	pods, err := s.pods.ListHuskPods(ctx)
	if err != nil {
		// A total listing failure fails the cycle loudly: the collector logs it
		// and retries next cycle. Returning empty here would silently zero the
		// bill for the whole fleet, the exact failure mode issue #613 closes.
		return nil, fmt.Errorf("list husk pods: %w", err)
	}
	billable := pods[:0:0]
	for _, pod := range pods {
		if pod.OrgID == "" || pod.VMID == "" {
			// An unattributed pod (no trusted org label) or one with no vm-id is not
			// billable. Skip it without counting it as a scrape failure.
			continue
		}
		billable = append(billable, pod)
	}

	results := make([][]Sample, len(billable))
	workers := huskScrapeConcurrency
	if len(billable) < workers {
		workers = len(billable)
	}
	if workers > 0 {
		next := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					results[i] = s.podSamples(ctx, billable[i], at)
				}
			}()
		}
		for i := range billable {
			next <- i
		}
		close(next)
		wg.Wait()
	}

	var out []Sample
	for _, samples := range results {
		out = append(out, samples...)
	}
	return out, nil
}

// podSamples scrapes one billable husk pod and converts its report to
// org-tagged Samples stamped with the cycle's shared timestamp. A failed scrape
// is skip-and-counted (nil samples), never an error: one bad pod must not zero
// out the bill for the healthy fleet. It is called from the Collect worker
// pool, so it touches only the pod, the atomic skip counter, and the injected
// seams; it never writes shared source state.
func (s *HuskSource) podSamples(ctx context.Context, pod HuskPod, at time.Time) []Sample {
	report, ok := s.scrape(ctx, pod)
	if !ok {
		s.skipped.Add(1)
		return nil
	}
	// Attribute ONLY this pod's own vm-id to its org, from the TRUSTED label.
	// Any other sample id the (untrusted) pod returns resolves unattributed and
	// is dropped, so a pod can bill only its own vm-id/org (defense in depth).
	orgOf := func(sandboxID string) (string, bool) {
		if sandboxID == pod.VMID {
			return pod.OrgID, true
		}
		return "", false
	}
	samples, _ := SamplesFromReport(pod.VMID, at, report, orgOf, s.vcpus)
	// Emit the sample keyed by the API-VISIBLE sandbox id from the TRUSTED
	// claim label (issue #663), so usage_records reconcile to the sb-... id
	// the customer saw, never the internal husk pod name. The trust check
	// above stays keyed on the pod's vm-id: the pod cannot choose its billing
	// id, only the controller's label does. Fallback: a pod whose lister
	// carried no APIID (label absent; pre-#663 lister or a bespoke self-host
	// lister) keeps the pod name, preserving the old behavior rather than
	// dropping the sample. The Sample's Node field keeps the pod name (the
	// vm-id), so support can still map a record back to the pod.
	if pod.APIID != "" {
		for i := range samples {
			samples[i].SandboxID = pod.APIID
		}
	}
	return samples
}

// scrape GETs GET /v1/metering from one husk pod and decodes the Report. It
// returns ok=false (not an error) on any reachability, status, or decode failure
// so the caller can skip-and-count the pod. It carries no secret: the request has
// no body and the response is the pod's metering Report (ids, byte/second counts
// only). It applies a bounded per-request deadline derived from ctx so a hung pod
// cannot stall the whole cycle behind it, even when a custom client (no
// http.Client.Timeout) is injected. It reuses the same meteringPath and
// scrapeTimeout as the forkd node source.
func (s *HuskSource) scrape(ctx context.Context, pod HuskPod) (metering.Report, bool) {
	var report metering.Report
	if pod.Endpoint == "" {
		return report, false
	}
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	url := fmt.Sprintf("%s://%s%s", s.scheme, pod.Endpoint, meteringPath)
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

// SkippedPods returns the cumulative count of husk-pod scrapes skipped because the
// pod was unreachable, errored, returned a non-200, or failed to decode. It is the
// degradation signal the wiring exposes (a metric or a logged count); it never
// carries pod identity or error text.
func (s *HuskSource) SkippedPods() int64 { return s.skipped.Load() }
