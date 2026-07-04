package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/metering"
)

// TestMeteringEndpointExemptFromToken asserts the in-pod GET /v1/metering route
// is reachable WITHOUT the per-sandbox bearer token (mirroring forkd, whose
// /v1/metering is unauthenticated), and that a request carrying a (bogus) token
// still works. It is served from the same mux as the token-gated exec/files API,
// so it must NOT be routed through the bearer middleware.
func TestMeteringEndpointExemptFromToken(t *testing.T) {
	api := daemon.NewSandboxAPI(t.TempDir())
	api.SetSingleSandbox(huskSandboxID)
	// Register a token so the exec/files surface is genuinely gated; the metering
	// route must bypass that gate regardless.
	api.RegisterToken(huskSandboxID, "the-secret-token")

	report := metering.Aggregate([]metering.Sample{{ID: "husk-pod-1"}})
	mux := newSandboxMux(api, func() metering.Report { return report })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No Authorization header at all.
	resp, err := http.Get(srv.URL + "/v1/metering")
	if err != nil {
		t.Fatalf("GET /v1/metering without token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/metering without token status = %d, want 200 (must be exempt from the bearer gate)", resp.StatusCode)
	}
	var got metering.Report
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode metering report: %v", err)
	}
	if len(got.Sandboxes) != 1 || got.Sandboxes[0].ID != "husk-pod-1" {
		t.Fatalf("metering report = %+v, want one sandbox husk-pod-1", got.Sandboxes)
	}

	// A request WITH a (wrong) token still works: the route is unauthenticated.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/metering", nil)
	req.Header.Set("Authorization", "Bearer some-other-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/metering with token: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/metering with token status = %d, want 200", resp2.StatusCode)
	}
}

// TestMeteringEndpointEmptyBeforeActivate asserts the endpoint returns a clean
// EMPTY report (200, no sandboxes) when the source has no active VM yet, so a
// scrape during the warm window is never a 5xx.
func TestMeteringEndpointEmptyBeforeActivate(t *testing.T) {
	api := daemon.NewSandboxAPI(t.TempDir())
	api.SetSingleSandbox(huskSandboxID)
	mux := newSandboxMux(api, func() metering.Report { return metering.Report{} })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/metering")
	if err != nil {
		t.Fatalf("GET /v1/metering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty report, not an error)", resp.StatusCode)
	}
	var got metering.Report
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode metering report: %v", err)
	}
	if len(got.Sandboxes) != 0 {
		t.Fatalf("empty-state report must have no sandboxes, got %d", len(got.Sandboxes))
	}
}
