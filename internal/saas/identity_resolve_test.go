package saas

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityResolveHandler_NoToken(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	h := NewIdentityResolveHandler(accounts, "secret", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", bytes.NewBufferString(`{"email":"alice@example.com"}`))
	// No Authorization header.
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestIdentityResolveHandler_WrongToken(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	h := NewIdentityResolveHandler(accounts, "correct-secret", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", bytes.NewBufferString(`{"email":"alice@example.com"}`))
	r.Header.Set("Authorization", "Bearer wrong-secret")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestIdentityResolveHandler_Success(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)

	// Pre-seed an account with SignUp.
	acct, org, err := accounts.SignUp(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	h := NewIdentityResolveHandler(accounts, "test-secret", nil)

	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"email": "alice@example.com"})
	r := httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", bytes.NewBuffer(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		AccountID string   `json:"accountId"`
		OrgIDs    []string `json:"orgIds"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccountID != acct.ID {
		t.Errorf("accountId: got %q want %q", resp.AccountID, acct.ID)
	}
	if len(resp.OrgIDs) != 1 || resp.OrgIDs[0] != org.ID {
		t.Errorf("orgIds: got %v want [%s]", resp.OrgIDs, org.ID)
	}
}

func TestIdentityResolveHandler_EmptyConfiguredToken(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	h := NewIdentityResolveHandler(accounts, "", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", bytes.NewBufferString(`{"email":"alice@example.com"}`))
	r.Header.Set("Authorization", "Bearer anythingatall")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestIdentityResolveHandler_BadBody(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	h := NewIdentityResolveHandler(accounts, "test-secret", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/identity/resolve", bytes.NewBufferString(`not-json`))
	r.Header.Set("Authorization", "Bearer test-secret")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
