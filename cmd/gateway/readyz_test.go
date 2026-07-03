package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestReadyzServing asserts /readyz reports 200 while the gateway is serving,
// so the readiness probe keeps the replica in the Service endpoints.
func TestReadyzServing(t *testing.T) {
	var draining atomic.Bool
	rec := httptest.NewRecorder()
	newReadyzHandler(&draining)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz while serving: got %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestReadyzDraining asserts /readyz flips to 503 once shutdown has begun, so
// the Service stops routing new requests to a draining replica while the
// in-flight ones finish inside the shutdown timeout.
func TestReadyzDraining(t *testing.T) {
	var draining atomic.Bool
	draining.Store(true)
	rec := httptest.NewRecorder()
	newReadyzHandler(&draining)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Body.Len() == 0 {
		t.Error("readyz while draining: want an actionable message in the body")
	}
}
