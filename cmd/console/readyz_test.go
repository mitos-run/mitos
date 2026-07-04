package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakePinger is a test double for the Postgres pool readiness seam.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// TestReadyzInMemoryIsReady asserts /readyz reports 200 when no Postgres pool
// is configured: the in-memory dev deployment has no external dependency to
// probe, so it is ready as soon as it serves.
func TestReadyzInMemoryIsReady(t *testing.T) {
	rec := httptest.NewRecorder()
	newReadyzHandler(nil, nil)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz without postgres: got %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestReadyzPostgresReachable asserts /readyz reports 200 when the configured
// Postgres pool answers the ping.
func TestReadyzPostgresReachable(t *testing.T) {
	rec := httptest.NewRecorder()
	newReadyzHandler(fakePinger{}, nil)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz with reachable postgres: got %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestReadyzPostgresUnreachable asserts /readyz reports 503 with an actionable
// message when the configured Postgres pool is unreachable, and that the body
// NEVER carries the ping error text (pgx connect errors embed DSN-derived
// host/user detail, which must not cross an unauthenticated endpoint).
func TestReadyzPostgresUnreachable(t *testing.T) {
	leaky := errors.New("failed to connect to `host=db.internal user=mitos`: hostname resolving error")
	rec := httptest.NewRecorder()
	newReadyzHandler(fakePinger{err: leaky}, nil)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unreachable postgres: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	body := rec.Body.String()
	if strings.Contains(body, "db.internal") || strings.Contains(body, "user=") {
		t.Errorf("readyz body leaks DSN-derived detail from the ping error: %q", body)
	}
	if !strings.Contains(body, "postgres") {
		t.Errorf("readyz body should name the unreachable dependency actionably: %q", body)
	}
}

// TestReadyzCountsPingFailures asserts a failed ping increments the Postgres
// ping-failure counter (the ConsolePostgresUnreachable alert signal) and a
// successful ping does not.
func TestReadyzCountsPingFailures(t *testing.T) {
	c := newDBPingFailuresCounter()
	prometheus.NewRegistry().MustRegister(c)
	h := newReadyzHandler(fakePinger{err: errors.New("down")}, c)
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if got := testutil.ToFloat64(c); got != 2 {
		t.Errorf("ping failures = %v, want 2", got)
	}
	ok := newReadyzHandler(fakePinger{}, c)
	ok(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if got := testutil.ToFloat64(c); got != 2 {
		t.Errorf("ping failures = %v after a healthy ping, want 2 (unchanged)", got)
	}
}
