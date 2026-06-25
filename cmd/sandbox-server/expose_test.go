package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStandaloneExposeHandlerReachable(t *testing.T) {
	s := newServer(t.TempDir(), "", false, 16, 86400) // real mode, no engine
	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/ghost/expose/8000/", nil)
	r.SetPathValue("id", "ghost")
	r.SetPathValue("port", "8000")
	w := httptest.NewRecorder()
	s.sandboxAPI.HandleExpose(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unknown sandbox through the standalone server, got %d (body %q)", w.Code, w.Body.String())
	}
}
