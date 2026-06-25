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
