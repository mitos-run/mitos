package main

import (
	"net/http"
	"sync/atomic"
)

// newReadyzHandler returns the /readyz readiness handler. Readiness is split
// from liveness (/healthz stays a static 200): once shutdown has begun the
// handler reports 503 so the Service stops routing NEW requests to this
// replica while srv.Shutdown drains the in-flight ones. The gateway has no
// cheap in-process check for its control-plane upstream, so drain state is the
// only readiness signal; a real upstream probe is deliberately not added here.
func newReadyzHandler(draining *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining: this replica is shutting down; retry against another replica"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
