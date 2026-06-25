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

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/notaport/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "notaport")
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad port, got %d", w.Code)
	}
}
