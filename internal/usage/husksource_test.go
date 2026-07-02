package usage

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/metering"
)

// staticHuskPods is a fixed HuskPodLister for the husk-source tests.
type staticHuskPods []HuskPod

func (p staticHuskPods) ListHuskPods() []HuskPod { return []HuskPod(p) }

// singleVMReport is a husk pod's own metering report: exactly one sample whose id
// is the pod's vm-id, matching what husk.Stub.Metering() produces.
func singleVMReport(vmID string) metering.Report {
	return metering.Aggregate([]metering.Sample{{ID: vmID, MemoryUnique: giB, EgressBytes: 42}})
}

// TestHuskSourceCollectsClaimedPod asserts the husk source scrapes a claimed
// pod's /v1/metering, converts its single-VM report to an org-tagged Sample, and
// that the org came from the TRUSTED pod label (the lister), not from the report.
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
