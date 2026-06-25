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

	// 502 (not 401) because newServer calls AllowTokenless() on its sandboxAPI,
	// so the tokenless auth passes and the request fails at route resolution for
	// the unknown sandbox "ghost".
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unknown sandbox through the standalone server, got %d (body %q)", w.Code, w.Body.String())
	}
}
