package spa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServesIndexAtRoot asserts the embedded shell is served at /.
func TestServesIndexAtRoot(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Mitos console") {
		t.Fatalf("index missing marker: %s", w.Body.String())
	}
}

// TestUnknownRouteFallsBackToShell asserts a client-side route (no file
// extension) serves the app shell so deep links work.
func TestUnknownRouteFallsBackToShell(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/secrets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (shell fallback)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "root") {
		t.Fatalf("fallback did not serve the shell: %s", w.Body.String())
	}
}

// TestMissingAssetIs404 asserts a missing asset (with an extension) is a real
// 404, not the shell — so a broken asset reference is visible.
func TestMissingAssetIs404(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/assets/nope-123.js", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a missing asset", w.Code)
	}
}
