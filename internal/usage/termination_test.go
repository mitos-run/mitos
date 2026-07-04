package usage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The issue #682 (was #664) gap: presence between the last 1-minute scrape and
// terminate was never recorded. A sandbox alive ~100s billed 60 vcpu-seconds,
// and a sub-minute sandbox billed nothing. These tests pin the fix: the
// controller records a Termination at claim release, and the husk source turns
// it into a FINAL sample (closing the half-open window), or into a synthesized
// start/end pair for a sandbox that terminated before its first scrape.

// mutableHuskPods is a HuskPodLister whose pod set a test can change between
// cycles (a pod disappears when its claim is released).
type mutableHuskPods struct{ pods []HuskPod }

func (m *mutableHuskPods) ListHuskPods(context.Context) ([]HuskPod, error) { return m.pods, nil }

// termBase is window-aligned so the integration math stays obvious.
var termBase = time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)

// TestTerminationLogNilSafe asserts a nil log records and drains as a no-op:
// the self-host path runs without the usage collector and must never wire (or
// nil-check) anything.
func TestTerminationLogNilSafe(t *testing.T) {
	var log *TerminationLog
	log.Record(Termination{VMID: "husk-x", OrgID: "acme", At: termBase})
	if got := log.Drain(); got != nil {
		t.Fatalf("nil log Drain = %v, want nil", got)
	}
}

// TestTerminationLogBounded asserts the pending buffer is bounded (boring
// failure behavior: a stalled collector must not grow the controller heap
// without limit) and drops the OLDEST events first, keeping the most recent
// terminations, the ones the next cycle can still bill accurately.
func TestTerminationLogBounded(t *testing.T) {
	log := NewTerminationLog()
	for i := 0; i < terminationLogCap+10; i++ {
		log.Record(Termination{VMID: fmt.Sprintf("husk-%d", i), OrgID: "acme", At: termBase})
	}
	got := log.Drain()
	if len(got) != terminationLogCap {
		t.Fatalf("pending after overflow = %d, want the cap %d", len(got), terminationLogCap)
	}
	if got[len(got)-1].VMID != fmt.Sprintf("husk-%d", terminationLogCap+9) {
		t.Errorf("newest termination was dropped; last kept = %s", got[len(got)-1].VMID)
	}
	if got[0].VMID == "husk-0" {
		t.Errorf("oldest termination survived overflow; want oldest-first eviction")
	}
	if again := log.Drain(); len(again) != 0 {
		t.Errorf("second Drain = %d events, want 0 (Drain consumes)", len(again))
	}
}

// TestHuskSourceFinalSampleAtTermination is the core #664 fix proof: a pod
// scraped at t0 whose claim is released at t0+40s yields a FINAL sample at the
// release instant carrying the pod's last known levels, so integrating the
// cycle's samples bills the tail [t0, t0+40] instead of dropping it. The final
// sample is emitted exactly once: the termination is consumed.
func TestHuskSourceFinalSampleAtTermination(t *testing.T) {
	srv := meteringServer(t, singleVMReport("husk-final"))
	defer srv.Close()

	clock := termBase
	pods := &mutableHuskPods{pods: []HuskPod{{
		VMID: "husk-final", APIID: "sb-final", OrgID: "acme",
		Endpoint: srv.Listener.Addr().String(),
	}}}
	terms := NewTerminationLog()
	src := NewHuskSource(pods, nil, srv.Client(), "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	first, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].SandboxID != "sb-final" {
		t.Fatalf("precondition: want 1 scraped sample for sb-final, got %+v", first)
	}

	// The claim is released 40s after the scrape; the pod is gone next cycle.
	releasedAt := termBase.Add(40 * time.Second)
	terms.Record(Termination{
		VMID: "husk-final", APIID: "sb-final", OrgID: "acme",
		StartedAt: termBase.Add(-30 * time.Second), At: releasedAt,
	})
	pods.pods = nil
	clock = termBase.Add(60 * time.Second)

	second, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("want exactly 1 final sample, got %+v", second)
	}
	final := second[0]
	if !final.Timestamp.Equal(releasedAt) {
		t.Errorf("final sample timestamp = %v, want the release instant %v", final.Timestamp, releasedAt)
	}
	if final.SandboxID != "sb-final" || final.OrgID != "acme" {
		t.Errorf("final sample identity = (%s, %s), want (sb-final, acme)", final.SandboxID, final.OrgID)
	}
	if final.VCPUs != first[0].VCPUs || final.MemUniqueBytes != first[0].MemUniqueBytes {
		t.Errorf("final sample levels = %+v, want the last scraped levels %+v", final, first[0])
	}
	if final.EgressBytes != first[0].EgressBytes {
		// Cloning the cumulative counter means a ZERO counter delta over the
		// tail: the final sample closes the rate window without inventing
		// egress the pod never reported.
		t.Errorf("final sample egress = %d, want the last scraped cumulative %d (zero delta)", final.EgressBytes, first[0].EgressBytes)
	}

	// The tail is billed: t0 -> t0+40 at the held level.
	recs := Integrate(append(append([]Sample{}, first...), second...), DefaultConfig())
	var vcpu float64
	for _, r := range recs {
		vcpu += r.VCPUSeconds
	}
	if vcpu != 40 {
		t.Errorf("integrated vcpu-seconds = %v, want 40 (the [last scrape, terminate] tail)", vcpu)
	}

	// The termination was consumed: nothing more is emitted.
	clock = termBase.Add(120 * time.Second)
	third, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("final sample must be emitted exactly once, got %+v on the next cycle", third)
	}
}

// TestHuskSourceHundredSecondSandboxBillsHundredSeconds is the issue's headline
// number: a sandbox alive ~100s (scraped at t0 and t0+60, terminated at
// t0+100) must bill 100 vcpu-seconds, not 60.
func TestHuskSourceHundredSecondSandboxBillsHundredSeconds(t *testing.T) {
	srv := meteringServer(t, singleVMReport("husk-100s"))
	defer srv.Close()

	clock := termBase
	pods := &mutableHuskPods{pods: []HuskPod{{
		VMID: "husk-100s", APIID: "sb-100s", OrgID: "acme",
		Endpoint: srv.Listener.Addr().String(),
	}}}
	terms := NewTerminationLog()
	src := NewHuskSource(pods, nil, srv.Client(), "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	var all []Sample
	collect := func() {
		t.Helper()
		s, err := src.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, s...)
	}

	collect() // t0
	clock = termBase.Add(60 * time.Second)
	collect() // t0+60

	terms.Record(Termination{
		VMID: "husk-100s", APIID: "sb-100s", OrgID: "acme",
		StartedAt: termBase, At: termBase.Add(100 * time.Second),
	})
	pods.pods = nil
	clock = termBase.Add(120 * time.Second)
	collect() // final sample at t0+100

	recs := Integrate(all, DefaultConfig())
	var vcpu float64
	for _, r := range recs {
		vcpu += r.VCPUSeconds
	}
	if vcpu != 100 {
		t.Errorf("integrated vcpu-seconds = %v, want 100 (60 from the scrapes + 40 tail)", vcpu)
	}
}

// TestHuskSourceSynthesizesNeverScrapedSandbox covers the sub-minute job that
// terminated before its first scrape: the source synthesizes a start/end
// sample pair over [StartedAt, At] carrying ONLY the known allocation (vCPUs);
// memory and disk are unknown and stay zero (customer-favorable: the customer
// is never billed for a level nobody measured).
func TestHuskSourceSynthesizesNeverScrapedSandbox(t *testing.T) {
	clock := termBase.Add(50 * time.Second)
	terms := NewTerminationLog()
	src := NewHuskSource(&mutableHuskPods{}, nil, nil, "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	terms.Record(Termination{
		VMID: "husk-short", APIID: "sb-short", OrgID: "acme",
		StartedAt: termBase, At: termBase.Add(45 * time.Second),
	})

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("want a synthesized start/end pair, got %+v", samples)
	}
	for _, s := range samples {
		if s.SandboxID != "sb-short" || s.OrgID != "acme" || s.VCPUs != 1 {
			t.Errorf("synthesized sample = %+v, want sb-short/acme with the 1 vCPU default", s)
		}
		if s.MemUniqueBytes != 0 || s.MemSharedAmortizedBytes != 0 || s.DiskBytes != 0 || s.EgressBytes != 0 || s.GPUSeconds != 0 {
			t.Errorf("synthesized sample carries unmeasured levels: %+v (must bill only the known vCPU allocation)", s)
		}
	}
	recs := Integrate(samples, DefaultConfig())
	var vcpu float64
	for _, r := range recs {
		vcpu += r.VCPUSeconds
	}
	if vcpu != 45 {
		t.Errorf("integrated vcpu-seconds = %v, want 45 (the sandbox's whole sub-minute life)", vcpu)
	}
}

// TestHuskSourceSynthesizedPairClampsOldStart asserts the never-scraped pair
// never reaches back further than MaxHold before the terminate instant: a
// stale StartedAt (a long-lived sandbox whose scrape history was lost, for
// example across a controller restart) must not rewrite long-settled windows.
// Billing at most the MaxHold tail is the customer-favorable floor.
func TestHuskSourceSynthesizedPairClampsOldStart(t *testing.T) {
	endAt := termBase.Add(2 * time.Hour)
	clock := endAt.Add(10 * time.Second)
	terms := NewTerminationLog()
	src := NewHuskSource(&mutableHuskPods{}, nil, nil, "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	terms.Record(Termination{
		VMID: "husk-old", APIID: "sb-old", OrgID: "acme",
		StartedAt: termBase, At: endAt,
	})

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("want a clamped start/end pair, got %+v", samples)
	}
	wantStart := endAt.Add(-DefaultConfig().MaxHold)
	if !samples[0].Timestamp.Equal(wantStart) {
		t.Errorf("pair start = %v, want clamped to At-MaxHold %v", samples[0].Timestamp, wantStart)
	}
}

// TestHuskSourceTerminationRecordedOnceGuard asserts a vm-id already finalized
// is never billed again, even when a second termination event arrives (a
// lifetime-expiry terminate followed by the object delete records twice).
// Without the guard the second event would synthesize a fresh start/end pair
// and double-bill the sandbox.
func TestHuskSourceTerminationRecordedOnceGuard(t *testing.T) {
	srv := meteringServer(t, singleVMReport("husk-twice"))
	defer srv.Close()

	clock := termBase
	pods := &mutableHuskPods{pods: []HuskPod{{
		VMID: "husk-twice", APIID: "sb-twice", OrgID: "acme",
		Endpoint: srv.Listener.Addr().String(),
	}}}
	terms := NewTerminationLog()
	src := NewHuskSource(pods, nil, srv.Client(), "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	if _, err := src.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Lifetime expiry records the terminate, then the delete records it again.
	terms.Record(Termination{VMID: "husk-twice", APIID: "sb-twice", OrgID: "acme", StartedAt: termBase, At: termBase.Add(30 * time.Second)})
	terms.Record(Termination{VMID: "husk-twice", APIID: "sb-twice", OrgID: "acme", StartedAt: termBase, At: termBase.Add(35 * time.Second)})
	pods.pods = nil
	clock = termBase.Add(60 * time.Second)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("want exactly 1 final sample for a double-recorded termination, got %+v", samples)
	}

	// A third event on a later cycle must also stay silent (the finalized guard,
	// not just same-cycle dedupe).
	terms.Record(Termination{VMID: "husk-twice", APIID: "sb-twice", OrgID: "acme", StartedAt: termBase, At: termBase.Add(40 * time.Second)})
	clock = termBase.Add(120 * time.Second)
	again, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a finalized vm-id must never bill again, got %+v", again)
	}
}

// TestHuskSourceIgnoresUnattributedTermination asserts a termination without a
// trusted org bills nothing: the self-host single-tenant path has no org and
// must stay out of billable samples, exactly like the live scrape path.
func TestHuskSourceIgnoresUnattributedTermination(t *testing.T) {
	clock := termBase
	terms := NewTerminationLog()
	src := NewHuskSource(&mutableHuskPods{}, nil, nil, "http", func() time.Time { return clock })
	src.SetTerminations(terms)

	terms.Record(Termination{VMID: "husk-selfhost", StartedAt: termBase.Add(-30 * time.Second), At: termBase})

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("an unattributed termination must not bill, got %+v", samples)
	}
}
