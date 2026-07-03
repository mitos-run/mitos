package main

import (
	"context"
	"net/http"
	"time"
)

// pgPinger is the narrow readiness seam over the durable Postgres pool
// (pgxpool.Pool satisfies it). A nil pinger means the binary runs on the
// in-memory stores, which have no external dependency to probe.
type pgPinger interface {
	Ping(ctx context.Context) error
}

// readyzPingTimeout bounds the readiness ping so a hung Postgres cannot stall
// the kubelet probe; the probe's own periodSeconds stays the pacing.
const readyzPingTimeout = 2 * time.Second

// newReadyzHandler returns the /readyz readiness handler. Readiness is split
// from liveness (/healthz stays a static 200): a console whose configured
// Postgres is unreachable must NOT receive traffic, because every durable
// surface (accounts, keys, sessions, credit ledger) would fail. The 503 body
// is a fixed actionable message; the ping error text is NEVER echoed because
// pgx connect errors embed DSN-derived host/user detail and this endpoint is
// unauthenticated.
func newReadyzHandler(db pgPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readyzPingTimeout)
			defer cancel()
			if err := db.Ping(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("not ready: postgres is unreachable; check the database Service, the DSN Secret, and network connectivity"))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
