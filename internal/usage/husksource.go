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

// HuskPod is one claimed, org-labeled husk pod the HuskSource scrapes. VMID is
// the pod's vm-id (the id the pod reports as its single metering Sample AND the
// id the controller maps to an org); OrgID is the owning org from the TRUSTED
// mitos.run/org pod label, NEVER from anything the pod returns; Endpoint is the
// pod's in-pod sandbox HTTP endpoint (podIP:port) serving GET /v1/metering.
type HuskPod struct {
	VMID     string
	OrgID    string
	Endpoint string
}

// HuskPodLister yields the claimed, org-labeled husk pods to scrape this cycle.
// It is the import-cycle-avoiding seam over the controller's pod cache (the
// controller wires the usage collector, so internal/usage must not import
// internal/controller): the controller's concrete adapter lists mitos.run/husk
// pods carrying a non-empty trusted mitos.run/org label and returns each pod's
// vm-id, org, and podIP:port. An empty slice yields no samples, not an error.
type HuskPodLister interface {
	ListHuskPods() []HuskPod
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
func (s *HuskSource) Collect(ctx context.Context) ([]Sample, error) {
	at := s.now()
	var out []Sample
	for _, pod := range s.pods.ListHuskPods() {
		if pod.OrgID == "" || pod.VMID == "" {
			// An unattributed pod (no trusted org label) or one with no vm-id is not
			// billable. Skip it without counting it as a scrape failure.
			continue
		}
		report, ok := s.scrape(ctx, pod)
		if !ok {
			s.skipped.Add(1)
			continue
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
		out = append(out, samples...)
	}
	return out, nil
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
