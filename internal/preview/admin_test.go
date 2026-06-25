package preview

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRouteSyncRequiresToken(t *testing.T) {
	routes := NewRouteTable()
	h := NewAdminHandler(routes, "admin-secret", nil)
	body := `{"routes":[{"Label":"openclaw","SandboxID":"sbx1","NodeEndpoint":"10.0.0.7:9091","Port":8000,"Token":"t","Sharing":"link","Ready":true}]}`

	// No token: 401, no route synced.
	r := httptest.NewRequest(http.MethodPost, "/internal/routes", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d", w.Code)
	}
	if _, ok := routes.Lookup("openclaw"); ok {
		t.Fatal("route synced without auth")
	}

	// Correct token: 204 and route present.
	r = httptest.NewRequest(http.MethodPost, "/internal/routes", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("authed status=%d body=%s", w.Code, w.Body.String())
	}
	if r2, ok := routes.Lookup("openclaw"); !ok || r2.Port != 8000 {
		t.Fatalf("route not synced: %+v", r2)
	}
}

func TestAdminRouteSyncRejectsEmptyBearer(t *testing.T) {
	routes := NewRouteTable()
	h := NewAdminHandler(routes, "admin-secret", nil)
	body := `{"routes":[{"Label":"openclaw","SandboxID":"sbx1","NodeEndpoint":"10.0.0.7:9091","Port":8000,"Token":"t","Sharing":"link","Ready":true}]}`

	// "Bearer " with an empty token after the prefix must not authorize: it
	// guards the constant-time compare against an empty presented token.
	r := httptest.NewRequest(http.MethodPost, "/internal/routes", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("empty-bearer status=%d", w.Code)
	}
	if _, ok := routes.Lookup("openclaw"); ok {
		t.Fatal("route synced with empty bearer")
	}
}

func TestAdminRouteSyncRejectsWrongMethod(t *testing.T) {
	routes := NewRouteTable()
	h := NewAdminHandler(routes, "admin-secret", nil)

	// Only POST mutates the route table; a GET with a valid bearer must not
	// sync and must return a non-2xx status (the inner mux returns 404).
	r := httptest.NewRequest(http.MethodGet, "/internal/routes", nil)
	r.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatalf("wrong-method status=%d, want >= 400", w.Code)
	}
	if _, ok := routes.Lookup("openclaw"); ok {
		t.Fatal("route synced via non-POST method")
	}
}
