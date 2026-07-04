package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mitos.run/mitos/internal/metering"
)

// staticHuskPods is a fixed HuskPodLister for the husk-source tests.
type staticHuskPods []HuskPod

func (p staticHuskPods) ListHuskPods(context.Context) ([]HuskPod, error) { return []HuskPod(p), nil }

// singleVMReport is a husk pod's own metering report: exactly one sample whose id
// is the pod's vm-id, matching what husk.Stub.Metering() produces.
func singleVMReport(vmID string) metering.Report {
	return metering.Aggregate([]metering.Sample{{ID: vmID, MemoryUnique: giB, EgressBytes: 42}})
}

// TestHuskSourceCollectsClaimedPod asserts the husk source scrapes a claimed
// pod's /v1/metering, converts its single-VM report to an org-tagged Sample, and
// that the org came from the TRUSTED pod label (the lister), not from the report.
// The pod carries NO APIID, so this doubles as the fallback case for issue #663:
// with no API-visible id, the sample keeps the pod name as its SandboxID.
func TestHuskSourceCollectsClaimedPod(t *testing.T) {
	srv := meteringServer(t, singleVMReport("husk-pod-acme"))
	defer srv.Close()

	at := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	src := NewHuskSource(
		staticHuskPods{{VMID: "husk-pod-acme", OrgID: "acme", Endpoint: srv.Listener.Addr().String()}},
		nil,
		srv.Client(),
		"http",
		func() time.Time { return at },
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.SandboxID != "husk-pod-acme" {
		t.Errorf("SandboxID = %q, want husk-pod-acme", s.SandboxID)
	}
	if s.OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme (from the trusted pod label)", s.OrgID)
	}
	if !s.Timestamp.Equal(at) {
		t.Errorf("Timestamp = %v, want %v", s.Timestamp, at)
	}
	if src.SkippedPods() != 0 {
		t.Errorf("SkippedPods = %d, want 0", src.SkippedPods())
	}
}

// TestHuskSourceEmitsAPIVisibleSandboxID is the issue #663 fix proof: a husk
// pod's report is trusted only for its OWN vm-id (the pod name), but the Sample
// the source emits must carry the API-VISIBLE sandbox id from the controller's
// trusted claim label (HuskPod.APIID), so usage_records reconcile to the sb-...
// id the customer saw, not the husk pod name.
func TestHuskSourceEmitsAPIVisibleSandboxID(t *testing.T) {
	// The pod reports its own vm-id, which is the POD NAME (--vm-id = POD_NAME).
	srv := meteringServer(t, singleVMReport("python-husk-2blsp"))
	defer srv.Close()

	src := NewHuskSource(
		staticHuskPods{{
			VMID:     "python-husk-2blsp",
			APIID:    "sb-82813f5c",
			OrgID:    "acme",
			Endpoint: srv.Listener.Addr().String(),
		}},
		nil,
		srv.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d: %+v", len(samples), samples)
	}
	if samples[0].SandboxID != "sb-82813f5c" {
		t.Errorf("SandboxID = %q, want sb-82813f5c (the API-visible id from the trusted claim label)", samples[0].SandboxID)
	}
	if samples[0].OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme", samples[0].OrgID)
	}
}

// TestHuskSourceAPIIDDoesNotWeakenVMIDTrust asserts the report-id trust check
// stays keyed on the pod name even when an APIID is present: a pod whose report
// carries any id other than its OWN vm-id (here, the API id itself) bills
// NOTHING. The pod is untrusted for identity; only the controller's claim label
// selects the billing id.
func TestHuskSourceAPIIDDoesNotWeakenVMIDTrust(t *testing.T) {
	// The pod tries to report under the API-visible id instead of its vm-id.
	srv := meteringServer(t, singleVMReport("sb-82813f5c"))
	defer srv.Close()

	src := NewHuskSource(
		staticHuskPods{{
			VMID:     "python-husk-2blsp",
			APIID:    "sb-82813f5c",
			OrgID:    "acme",
			Endpoint: srv.Listener.Addr().String(),
		}},
		nil,
		srv.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("a report id that is not the pod's own vm-id must be rejected even when it equals the APIID, got %+v", samples)
	}
}

// TestHuskSourceSkipsUnreachablePod asserts an unreachable pod is SKIPPED and
// counted while a healthy pod's sample is still collected. One bad pod must never
// zero out the others.
func TestHuskSourceSkipsUnreachablePod(t *testing.T) {
	good := meteringServer(t, singleVMReport("husk-good"))
	defer good.Close()

	src := NewHuskSource(
		staticHuskPods{
			{VMID: "husk-good", OrgID: "acme", Endpoint: good.Listener.Addr().String()},
			{VMID: "husk-dead", OrgID: "acme", Endpoint: "127.0.0.1:1"}, // nothing listening
		},
		nil,
		good.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not fail when a pod is unreachable: %v", err)
	}
	if len(samples) != 1 || samples[0].SandboxID != "husk-good" {
		t.Fatalf("want 1 sample from the healthy pod, got %+v", samples)
	}
	if src.SkippedPods() != 1 {
		t.Errorf("SkippedPods = %d, want 1 (the unreachable pod)", src.SkippedPods())
	}
}

// TestHuskSourceIgnoresForeignVMID asserts a pod that returns a report for a
// DIFFERENT vm-id than the trusted label says it owns bills NOTHING: a pod can
// only bill its OWN vm-id/org. This is the defense-in-depth cross-tenant guard.
func TestHuskSourceIgnoresForeignVMID(t *testing.T) {
	// The pod claims (via its report) to be metering a victim's vm-id.
	srv := meteringServer(t, singleVMReport("victim-pod-vm"))
	defer srv.Close()

	src := NewHuskSource(
		// The trusted label says this pod is "attacker-pod" owned by org "attacker".
		staticHuskPods{{VMID: "attacker-pod", OrgID: "attacker", Endpoint: srv.Listener.Addr().String()}},
		nil,
		srv.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("a foreign vm-id sample must be ignored, got %+v", samples)
	}
}

// TestHuskSourceSkipsUnattributedPod asserts a pod with no trusted org label is
// skipped entirely (not billed, not counted as a scrape failure): the self-host
// single-tenant path where a husk pod carries no mitos.run/org.
func TestHuskSourceSkipsUnattributedPod(t *testing.T) {
	srv := meteringServer(t, singleVMReport("husk-noorg"))
	defer srv.Close()

	src := NewHuskSource(
		staticHuskPods{{VMID: "husk-noorg", OrgID: "", Endpoint: srv.Listener.Addr().String()}},
		nil,
		srv.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("an unattributed pod must not be billed, got %+v", samples)
	}
	if src.SkippedPods() != 0 {
		t.Errorf("SkippedPods = %d, want 0 (an unattributed pod is not a scrape failure)", src.SkippedPods())
	}
}

// TestHuskSourceScrapesConcurrentlyBounded is the issue #682 (was #656) fix
// proof: the husk source scrapes claimed pods through a BOUNDED worker pool, so
// the cycle duration is set by the slowest pool lane, not by the fleet size.
// Twelve pods whose handlers each hold the request briefly must overlap in
// flight (peak concurrency at least 2; the sequential implementation pins the
// peak at 1) while never exceeding the pool bound. The single shared scrape
// timestamp per cycle and the zero-skip accounting must survive the fan-out.
func TestHuskSourceScrapesConcurrentlyBounded(t *testing.T) {
	const podCount = 12

	var inflight, peak atomic.Int64
	pods := make(staticHuskPods, 0, podCount)
	for i := 0; i < podCount; i++ {
		vmID := fmt.Sprintf("husk-conc-%d", i)
		report := singleVMReport(vmID)
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/metering", func(w http.ResponseWriter, _ *http.Request) {
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			// Hold the request long enough that a concurrent implementation
			// overlaps requests; a sequential one never does.
			time.Sleep(50 * time.Millisecond)
			inflight.Add(-1)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(report); err != nil {
				t.Errorf("encode report: %v", err)
			}
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		pods = append(pods, HuskPod{VMID: vmID, OrgID: "acme", Endpoint: srv.Listener.Addr().String()})
	}

	scrapedAt := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	src := NewHuskSource(pods, nil, &http.Client{}, "http", func() time.Time { return scrapedAt })

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != podCount {
		t.Fatalf("want %d samples, got %d", podCount, len(samples))
	}
	for _, s := range samples {
		if !s.Timestamp.Equal(scrapedAt) {
			t.Fatalf("sample %s timestamp = %v, want the single shared cycle instant %v", s.SandboxID, s.Timestamp, scrapedAt)
		}
	}
	if src.SkippedPods() != 0 {
		t.Errorf("SkippedPods = %d, want 0", src.SkippedPods())
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("peak in-flight scrapes = %d, want at least 2 (sequential scraping serializes the cycle; issue #682)", got)
	}
	if got := peak.Load(); got > huskScrapeConcurrency {
		t.Errorf("peak in-flight scrapes = %d, want at most the pool bound %d", got, huskScrapeConcurrency)
	}
}

// TestHuskSourceConcurrentScrapePreservesSkipAndCount asserts the worker-pool
// fan-out keeps the skip-and-count semantics: unreachable pods are skipped and
// counted while every healthy pod still bills, exactly as in the sequential
// implementation.
func TestHuskSourceConcurrentScrapePreservesSkipAndCount(t *testing.T) {
	good := meteringServer(t, singleVMReport("husk-ok"))
	defer good.Close()

	pods := staticHuskPods{
		{VMID: "husk-dead-1", OrgID: "acme", Endpoint: "127.0.0.1:1"},
		{VMID: "husk-ok", OrgID: "acme", Endpoint: good.Listener.Addr().String()},
		{VMID: "husk-dead-2", OrgID: "acme", Endpoint: "127.0.0.1:1"},
	}
	src := NewHuskSource(pods, nil, &http.Client{}, "http", nil)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not fail on unreachable pods: %v", err)
	}
	if len(samples) != 1 || samples[0].SandboxID != "husk-ok" {
		t.Fatalf("want exactly the healthy pod's sample, got %+v", samples)
	}
	if src.SkippedPods() != 2 {
		t.Errorf("SkippedPods = %d, want 2", src.SkippedPods())
	}
}

// staticSampleSource is a fixed SampleSource for the MultiSource test.
type staticSampleSource struct {
	samples []Sample
	err     error
}

func (s staticSampleSource) Collect(context.Context) ([]Sample, error) {
	return s.samples, s.err
}

// TestMultiSourceUnionsSources asserts MultiSource unions the samples of both a
// forkd node source and a husk source in a single Collect.
func TestMultiSourceUnionsSources(t *testing.T) {
	a := staticSampleSource{samples: []Sample{{SandboxID: "raw-forkd-sb", OrgID: "acme"}}}
	b := staticSampleSource{samples: []Sample{{SandboxID: "husk-pod-sb", OrgID: "beta"}}}
	multi := NewMultiSource(a, b)

	samples, err := multi.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 unioned samples, got %d", len(samples))
	}
	got := map[string]string{}
	for _, s := range samples {
		got[s.SandboxID] = s.OrgID
	}
	if got["raw-forkd-sb"] != "acme" || got["husk-pod-sb"] != "beta" {
		t.Fatalf("union = %v, want both sources' samples", got)
	}
}
