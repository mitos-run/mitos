// internal/daemon/expose_route_test.go
package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleExposeRejectsMissingBearer(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/8000/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "8000")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", w.Code)
	}
}

func TestHandleExposeRejectsWrongBearer(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/8000/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "8000")
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bearer, got %d", w.Code)
	}
}

func TestHandleExposeRejectsBadPort(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	// "notaport" is not an integer; "0" and "65536" are out of the 1-65535 range.
	// All three must be rejected with 400 before any proxy dial, with a valid
	// bearer presented so the port check is what fires.
	for _, port := range []string{"notaport", "0", "65536"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/"+port+"/", nil)
		r.SetPathValue("id", "sb1")
		r.SetPathValue("port", port)
		r.Header.Set("Authorization", "Bearer secret-token")
		w := httptest.NewRecorder()
		api.handleExpose(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad port %q, got %d", port, w.Code)
		}
	}
}

// TestHandleExposeRejectsBadPortWithoutAuth proves that an unauthenticated
// caller with a malformed port receives 401 (auth-first), not 400, so port
// validity is not leaked to unauthenticated callers.
func TestHandleExposeRejectsBadPortWithoutAuth(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/notaport/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "notaport")
	// No Authorization header.
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad port without auth, got %d", w.Code)
	}
}

// TestHandleExposeNoTrailingSlashIsRouted proves the bare (no trailing slash)
// app-root form /v1/sandboxes/{id}/expose/{port} reaches handleExpose and its
// bearer gate, rather than falling through to the JSON catch-all (which would
// answer a confusing 400 invalid json). A missing Authorization header must
// surface as 401 from the bearer gate.
func TestHandleExposeNoTrailingSlashIsRouted(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/8000", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "8000")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for no-slash form without bearer, got %d", w.Code)
	}
}
