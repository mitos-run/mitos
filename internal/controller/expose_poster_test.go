package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestExposePosterSyncPostsWithBearer(t *testing.T) {
	var capturedAuth string
	var capturedRoutes []ExposeRoute

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		var payload struct {
			Routes []ExposeRoute `json:"routes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		capturedRoutes = payload.Routes
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := NewExposePoster(srv.URL, "admin-secret")
	p.Backoff = time.Millisecond

	routes := []ExposeRoute{
		{Label: "my-sandbox", SandboxID: "sb-abc", NodeEndpoint: "node1:9091", Port: 8080, Token: "tok", Sharing: "link", Ready: true},
	}

	err := p.Sync(context.Background(), routes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedAuth != "Bearer admin-secret" {
		t.Errorf("Authorization header = %q; want %q", capturedAuth, "Bearer admin-secret")
	}
	if len(capturedRoutes) != 1 {
		t.Fatalf("got %d routes; want 1", len(capturedRoutes))
	}
	got := capturedRoutes[0]
	if got.Label != "my-sandbox" || got.SandboxID != "sb-abc" || got.Port != 8080 || got.Token != "tok" || got.Sharing != "link" || !got.Ready {
		t.Errorf("route mismatch: %+v", got)
	}
}

func TestExposePosterRetriesOn5xx(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := NewExposePoster(srv.URL, "tok")
	p.Backoff = time.Millisecond

	err := p.Sync(context.Background(), []ExposeRoute{{Label: "x", SandboxID: "s", NodeEndpoint: "n:1", Port: 80, Token: "t", Sharing: "private", Ready: true}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("got %d attempts; want 2", atomic.LoadInt32(&attempts))
	}
}

func TestExposePosterNoRetryOn4xx(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	p := NewExposePoster(srv.URL, "tok")
	p.Backoff = time.Millisecond

	err := p.Sync(context.Background(), []ExposeRoute{{Label: "x", SandboxID: "s", NodeEndpoint: "n:1", Port: 80, Token: "t", Sharing: "private", Ready: true}})
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("got %d attempts; want 1 (no retry on 4xx)", atomic.LoadInt32(&attempts))
	}
}

func TestExposePosterEmptyURLNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := NewExposePoster("", "")
	p.Backoff = time.Millisecond

	err := p.Sync(context.Background(), []ExposeRoute{{Label: "x"}})
	if err != nil {
		t.Fatalf("expected nil error for empty URL, got: %v", err)
	}
	if called {
		t.Error("server was called but should not have been for empty URL poster")
	}
}
