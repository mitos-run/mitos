package frontdoor_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/frontdoor"
)

func TestHTTPResolver_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert method and path.
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		// Assert bearer token.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer good-token" {
			t.Errorf("unexpected Authorization header: %q", auth)
		}

		// Decode body and assert session field is present.
		var body struct {
			Session string `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		if body.Session == "" {
			t.Error("expected non-empty session in body")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accountId": "a",
			"orgId":     "o",
		})
	}))
	defer srv.Close()

	r := frontdoor.NewHTTPSessionResolver(srv.URL, "good-token", nil)

	id, err := r.Resolve(context.Background(), "sess")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.AccountID != "a" || id.OrgID != "o" {
		t.Errorf("unexpected identity: %+v", id)
	}
}

func TestHTTPResolver_401_ErrNoSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := frontdoor.NewHTTPSessionResolver(srv.URL, "tok", nil)

	_, err := r.Resolve(context.Background(), "bad-sess")
	if !errors.Is(err, frontdoor.ErrNoSession) {
		t.Errorf("expected ErrNoSession, got: %v", err)
	}
}

func TestHTTPResolver_500_NonNoSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := frontdoor.NewHTTPSessionResolver(srv.URL, "tok", nil)

	_, err := r.Resolve(context.Background(), "some-sess")
	if err == nil {
		t.Fatal("expected a non-nil error for 500")
	}
	if errors.Is(err, frontdoor.ErrNoSession) {
		t.Error("expected error to NOT be ErrNoSession for 500")
	}
	// Should mention the status code.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status code 500, got: %v", err)
	}
}
