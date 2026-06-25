package preview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolver_Resolve_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check bearer token.
		presented, ok := cutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || presented != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Check method and path.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/internal/identity/resolve" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Check body has email.
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Email != "bob@example.com" {
			http.Error(w, "unexpected email", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accountId": "acct-123",
			"orgIds":    []string{"org-a", "org-b"},
		})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "test-token")
	accountID, orgIDs, err := r.Resolve(context.Background(), "bob@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if accountID != "acct-123" {
		t.Errorf("accountID: got %q want %q", accountID, "acct-123")
	}
	if len(orgIDs) != 2 || orgIDs[0] != "org-a" || orgIDs[1] != "org-b" {
		t.Errorf("orgIDs: got %v", orgIDs)
	}
}

func TestResolver_Resolve_DisabledWhenEmpty(t *testing.T) {
	// A test server that would fail if called.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_ = srv.URL // avoid unused

	r := NewResolver("", "some-token")
	accountID, orgIDs, err := r.Resolve(context.Background(), "alice@example.com")
	if !errors.Is(err, ErrResolveDisabled) {
		t.Fatalf("expected ErrResolveDisabled, got: %v", err)
	}
	if accountID != "" {
		t.Errorf("accountID should be empty, got %q", accountID)
	}
	if orgIDs != nil {
		t.Errorf("orgIDs should be nil, got %v", orgIDs)
	}
	if called {
		t.Error("HTTP server should not have been called")
	}
}

func TestResolver_Resolve_NonTwoxx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "test-token")
	_, _, err := r.Resolve(context.Background(), "bob@example.com")
	if err == nil {
		t.Fatal("expected non-nil error on non-2xx response")
	}
	// Token must not appear in error message.
	if contains(err.Error(), "test-token") {
		t.Error("token should not appear in error message")
	}
}

// cutPrefix is a local helper because strings.CutPrefix is in strings package
// but we want to avoid importing it just for test helpers.
func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(substr) > 0 && containsStr(s, substr)))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
