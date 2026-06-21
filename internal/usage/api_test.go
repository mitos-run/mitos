package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedTwoOrgs puts one record for orgA and one for orgB in a store.
func seedTwoOrgs(t *testing.T) *MemUsageStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemUsageStore()
	if err := store.UpsertRecord(ctx, UsageRecord{OrgID: "orgA", SandboxID: "sbxA", Window: baseTime, VCPUSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRecord(ctx, UsageRecord{OrgID: "orgB", SandboxID: "sbxB", Window: baseTime, VCPUSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestUsageAPIOrgScoping is the cross-org isolation test: a request carrying
// org A's context sees ONLY org A's usage, even if it names org B in the query.
func TestUsageAPIOrgScoping(t *testing.T) {
	store := seedTwoOrgs(t)
	h := NewUsageHandler(store, DefaultPriceList())

	// Build a request whose context carries orgA, but which tries to name orgB.
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?org=orgB", nil)
	req = req.WithContext(WithOrg(req.Context(), "orgA"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.OrgID != "orgA" {
		t.Errorf("response OrgID = %q, want orgA (the context org, never the query)", resp.OrgID)
	}
	for _, r := range resp.Records {
		if r.OrgID != "orgA" {
			t.Errorf("leaked a record for org %q to org A", r.OrgID)
		}
	}
	if resp.Totals.VCPUSeconds != 60 {
		t.Errorf("totals VCPUSeconds = %v, want 60 (orgA only, not orgB's 120)", resp.Totals.VCPUSeconds)
	}
}

// TestUsageAPINoOrgContextRejected checks a request without an org context is
// refused, never served as some default org.
func TestUsageAPINoOrgContextRejected(t *testing.T) {
	store := seedTwoOrgs(t)
	h := NewUsageHandler(store, DefaultPriceList())
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no org context is attached", rec.Code)
	}
}

// TestUsageAPICost checks the response carries a cost computed from the price
// list over the org's totals.
func TestUsageAPICost(t *testing.T) {
	store := seedTwoOrgs(t)
	pl := PriceList{VCPUSecond: 0.001}
	h := NewUsageHandler(store, pl)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req = req.WithContext(WithOrg(req.Context(), "orgA"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 60 vCPU-seconds * 0.001 = 0.06.
	if !approx(resp.Cost.Total, 0.06) {
		t.Errorf("cost = %v, want 0.06", resp.Cost.Total)
	}
}

// TestUsageAPIWindowFilter checks the optional from/to query bounds the records.
func TestUsageAPIWindowFilter(t *testing.T) {
	ctx := context.Background()
	store := NewMemUsageStore()
	w0 := baseTime
	w1 := baseTime.Add(time.Hour)
	_ = store.UpsertRecord(ctx, UsageRecord{OrgID: "orgA", SandboxID: "s", Window: w0, VCPUSeconds: 10})
	_ = store.UpsertRecord(ctx, UsageRecord{OrgID: "orgA", SandboxID: "s", Window: w1, VCPUSeconds: 20})

	h := NewUsageHandler(store, DefaultPriceList())
	// Only ask for the second window onward.
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?from="+w1.Format(time.RFC3339), nil)
	req = req.WithContext(WithOrg(req.Context(), "orgA"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Totals.VCPUSeconds != 20 {
		t.Errorf("windowed totals = %v, want 20", resp.Totals.VCPUSeconds)
	}
}
