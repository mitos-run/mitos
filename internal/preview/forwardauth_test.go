package preview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForwardAuth_Allow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify forwarded headers were set.
		if r.Header.Get("X-Forwarded-Method") == "" {
			t.Error("X-Forwarded-Method not set")
		}
		if r.Header.Get("X-Forwarded-Uri") == "" {
			t.Error("X-Forwarded-Uri not set")
		}
		// Respond with identity headers.
		w.Header().Set("X-Auth-Request-Email", "alice@example.com")
		w.Header().Set("X-Auth-Request-User", "alice")
		w.Header().Set("X-Auth-Request-Groups", "org1, org2")
		w.Header().Set("X-Auth-Request-Verified-Email", "true")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "http://preview.example.com/page", nil)
	req.Host = "preview.example.com"

	allow, id, copyHeaders, status, err := ForwardAuth(context.Background(), http.DefaultClient, srv.URL, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allow {
		t.Fatal("expected allow=true")
	}
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}
	if id == nil {
		t.Fatal("expected non-nil Identity")
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email: got %q want %q", id.Email, "alice@example.com")
	}
	if id.Sub != "alice" {
		t.Errorf("Sub: got %q want %q", id.Sub, "alice")
	}
	if !id.EmailVerified {
		t.Error("expected EmailVerified=true")
	}
	if len(id.OrgIDs) != 2 || id.OrgIDs[0] != "org1" || id.OrgIDs[1] != "org2" {
		t.Errorf("OrgIDs: got %v", id.OrgIDs)
	}
	if len(copyHeaders) == 0 {
		t.Error("expected non-empty copyHeaders")
	}
	if copyHeaders.Get("X-Auth-Request-Email") != "alice@example.com" {
		t.Errorf("copyHeaders missing X-Auth-Request-Email, got: %v", copyHeaders)
	}
}

func TestForwardAuth_Deny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "http://preview.example.com/secret", nil)
	req.Host = "preview.example.com"

	allow, id, _, status, err := ForwardAuth(context.Background(), http.DefaultClient, srv.URL, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allow {
		t.Fatal("expected allow=false")
	}
	if id != nil {
		t.Fatal("expected nil Identity on deny")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d want %d", status, http.StatusUnauthorized)
	}
}

func TestStripForwardAuthHeaders_RemovesXAuthRequest(t *testing.T) {
	h := http.Header{}
	h.Set("X-Auth-Request-Email", "attacker@evil.com")
	h.Set("X-Auth-Request-User", "attacker")
	h.Set("X-Auth-Request-Groups", "admin")
	h.Set("X-Forwarded-For", "1.2.3.4") // should survive

	StripForwardAuthHeaders(h)

	if h.Get("X-Auth-Request-Email") != "" {
		t.Error("X-Auth-Request-Email should have been stripped")
	}
	if h.Get("X-Auth-Request-User") != "" {
		t.Error("X-Auth-Request-User should have been stripped")
	}
	if h.Get("X-Auth-Request-Groups") != "" {
		t.Error("X-Auth-Request-Groups should have been stripped")
	}
}

func TestForwardAuth_ForwardsXForwardedFor(t *testing.T) {
	var capturedFor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "http://preview.example.com/page", nil)
	req.Host = "preview.example.com"
	req.RemoteAddr = "10.0.0.1:54321"

	_, _, _, _, err := ForwardAuth(context.Background(), http.DefaultClient, srv.URL, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFor != "10.0.0.1" {
		t.Errorf("X-Forwarded-For: got %q want %q", capturedFor, "10.0.0.1")
	}
}

func TestStripForwardAuthHeaders_PreservesOtherHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("Authorization", "Bearer tok")
	h.Set("Cookie", "session=abc")

	StripForwardAuthHeaders(h)

	if h.Get("X-Forwarded-For") != "1.2.3.4" {
		t.Error("X-Forwarded-For should not be stripped")
	}
	if h.Get("Authorization") != "Bearer tok" {
		t.Error("Authorization should not be stripped")
	}
	if h.Get("Cookie") != "session=abc" {
		t.Error("Cookie should not be stripped")
	}
}
