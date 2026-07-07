package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInternalMuxServesResolveBehindBearer proves the M2M endpoints live on the
// internal mux and are bearer-gated, so moving them off the public listener
// (GHSA-rcf5-cfv3-jxvv) keeps them reachable in-cluster but not from a browser
// hitting the public console host.
func TestInternalMuxServesResolveBehindBearer(t *testing.T) {
	mux := newInternalMux(internalDeps{
		resolveToken: "sekret",
		approveToken: "sekret2",
		logger:       slog.New(slog.NewTextHandler(discard{}, nil)),
	})

	for _, path := range []string{"/internal/identity/resolve", "/internal/session/resolve", "/internal/approve-signup"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without bearer: status = %d, want 401", path, rec.Code)
		}
	}
}

// TestInternalMuxDoesNotMountWithoutToken proves an endpoint is absent (404, not
// a fail-open handler) when its token is unset.
func TestInternalMuxDoesNotMountWithoutToken(t *testing.T) {
	mux := newInternalMux(internalDeps{logger: slog.New(slog.NewTextHandler(discard{}, nil))})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmounted identity/resolve: status = %d, want 404", rec.Code)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
