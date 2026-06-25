package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/metering"
)

// twoSandboxReport is a node metering report with two sandboxes, both forks of
// the same template (so a shared-once set is amortized across them). The numbers
// are chosen so the per-sandbox memory level reconstructs report.UsedCoWAware.
func twoSandboxReport() metering.Report {
	return metering.Aggregate([]metering.Sample{
		{ID: "sb-acme", Template: "tmpl", MemoryUnique: giB, MemoryShared: 2 * giB, DiskUnique: giB, EgressBytes: 100, GPUSeconds: 5},
		{ID: "sb-acme2", Template: "tmpl", MemoryUnique: giB, MemoryShared: 2 * giB, DiskUnique: giB, EgressBytes: 200, GPUSeconds: 7},
	})
}

// meteringServer is an httptest server that serves the given report at
// GET /v1/metering, mirroring forkd's operational endpoint.
func meteringServer(t *testing.T, report metering.Report) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/metering", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			t.Errorf("encode report: %v", err)
		}
	})
	return httptest.NewServer(mux)
}

// staticEndpoints is a fixed NodeLister: a name -> HTTP endpoint map.
type staticEndpoints map[string]string

func (s staticEndpoints) ListNodeEndpoints() []NodeEndpoint {
	out := make([]NodeEndpoint, 0, len(s))
	for name, ep := range s {
		out = append(out, NodeEndpoint{Name: name, HTTPEndpoint: ep})
	}
	return out
}

// TestNodeRegistrySourceCollectsHealthyNode asserts the live source scrapes a
// node's /v1/metering, converts the per-sandbox rows to org-tagged Samples, and
// that the org came from the injected resolver (the trusted label path).
func TestNodeRegistrySourceCollectsHealthyNode(t *testing.T) {
	srv := meteringServer(t, twoSandboxReport())
	defer srv.Close()

	orgs := StaticOrgs{"sb-acme": "acme", "sb-acme2": "acme"}
	at := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	src := NewNodeRegistrySource(
		staticEndpoints{"n1": srv.Listener.Addr().String()},
		orgs,
		nil,
		srv.Client(),
		"http",
		func() time.Time { return at },
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 samples, got %d", len(samples))
	}
	for _, s := range samples {
		if s.OrgID != "acme" {
			t.Errorf("sample %s org = %q, want acme", s.SandboxID, s.OrgID)
		}
		if s.Node != "n1" {
			t.Errorf("sample %s node = %q, want n1", s.SandboxID, s.Node)
		}
		if !s.Timestamp.Equal(at) {
			t.Errorf("sample %s timestamp = %v, want %v", s.SandboxID, s.Timestamp, at)
		}
	}
	if src.SkippedNodes() != 0 {
		t.Errorf("SkippedNodes = %d, want 0", src.SkippedNodes())
	}
}

// TestNodeRegistrySourceSkipsBadNode asserts a node that errors (500) or is
// unreachable is SKIPPED and counted, while the healthy node's samples are still
// collected. One bad node must never zero out the others.
func TestNodeRegistrySourceSkipsBadNode(t *testing.T) {
	good := meteringServer(t, twoSandboxReport())
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	orgs := StaticOrgs{"sb-acme": "acme", "sb-acme2": "acme"}
	src := NewNodeRegistrySource(
		staticEndpoints{
			"good":        good.Listener.Addr().String(),
			"bad":         bad.Listener.Addr().String(),
			"unreachable": "127.0.0.1:1", // nothing listening
		},
		orgs,
		nil,
		good.Client(),
		"http",
		nil,
	)

	samples, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not fail when a node is bad: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 samples from the healthy node, got %d", len(samples))
	}
	if src.SkippedNodes() != 2 {
		t.Errorf("SkippedNodes = %d, want 2 (the 500 and the unreachable node)", src.SkippedNodes())
	}
}

// TestNodeRegistrySourceIdempotent asserts two identical scrapes Integrate to the
// same per-(sandbox, window) totals: a re-scrape of identical reports does NOT
// double-count after Integrate. This is the live-source half of the pipeline
// idempotency contract.
func TestNodeRegistrySourceIdempotent(t *testing.T) {
	srv := meteringServer(t, twoSandboxReport())
	defer srv.Close()

	orgs := StaticOrgs{"sb-acme": "acme", "sb-acme2": "acme"}
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	// Two scrapes 30s apart in the same window, identical reports.
	calls := 0
	now := func() time.Time {
		t := base.Add(time.Duration(calls) * 30 * time.Second)
		calls++
		return t
	}
	src := NewNodeRegistrySource(
		staticEndpoints{"n1": srv.Listener.Addr().String()},
		orgs,
		nil,
		srv.Client(),
		"http",
		now,
	)

	first, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	second, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}

	all := append(append([]Sample{}, first...), second...)
	recs := Integrate(all, DefaultConfig())

	// Integrate the first scrape alone: the union of two identical scrapes 30s
	// apart in one window must yield the SAME counter totals (egress/GPU are read
	// as a delta of the cumulative counter; identical readings delta to zero, so
	// the window's egress/gpu equal a single report's, never doubled).
	for _, r := range recs {
		if r.OrgID != "acme" {
			t.Errorf("record %s org = %q, want acme", r.SandboxID, r.OrgID)
		}
		// Two identical egress readings (300 total across the report's two
		// sandboxes, but per sandbox 100 and 200) delta to zero within the window.
		if r.EgressBytes != 0 {
			t.Errorf("record %s EgressBytes = %d, want 0 (identical re-scrape must not add egress)", r.SandboxID, r.EgressBytes)
		}
		if r.GPUSeconds != 0 {
			t.Errorf("record %s GPUSeconds = %d, want 0 (identical re-scrape must not add gpu)", r.SandboxID, r.GPUSeconds)
		}
	}
}
