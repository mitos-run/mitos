package saas

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionResolveHandler_NoBearer(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	sessionStore := NewSessionStore()
	sessions := NewSessionService(sessionStore, accounts)
	h := NewSessionResolveHandler(sessions, "secret", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/session/resolve", bytes.NewBufferString(`{"session":"sometoken"}`))
	// No Authorization header.
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSessionResolveHandler_WrongBearer(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	sessionStore := NewSessionStore()
	sessions := NewSessionService(sessionStore, accounts)
	h := NewSessionResolveHandler(sessions, "correct-secret", nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/session/resolve", bytes.NewBufferString(`{"session":"sometoken"}`))
	r.Header.Set("Authorization", "Bearer wrong-secret")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSessionResolveHandler_ValidSession(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	sessionStore := NewSessionStore()
	sessions := NewSessionService(sessionStore, accounts)

	// Seed an account and a session token via the SessionService the handler uses.
	acct, org, err := accounts.SignUp(context.Background(), "bob@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	rawToken := "test-session-token-abc"
	sessionStore.Issue(acct.ID, rawToken)

	h := NewSessionResolveHandler(sessions, "test-secret", nil)

	body, _ := json.Marshal(map[string]string{"session": rawToken})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/session/resolve", bytes.NewBuffer(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		AccountID string `json:"accountId"`
		OrgID     string `json:"orgId"`
		Orgs      []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AccountID != acct.ID {
		t.Errorf("accountId: got %q want %q", resp.AccountID, acct.ID)
	}
	if resp.OrgID != org.ID {
		t.Errorf("orgId: got %q want %q", resp.OrgID, org.ID)
	}
	if len(resp.Orgs) != 1 || resp.Orgs[0].ID != org.ID {
		t.Errorf("orgs: got %v want [{id:%s name:%s}]", resp.Orgs, org.ID, org.Name)
	}
}

func TestSessionResolveHandler_UnknownSession(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	sessionStore := NewSessionStore()
	sessions := NewSessionService(sessionStore, accounts)
	h := NewSessionResolveHandler(sessions, "test-secret", nil)

	body, _ := json.Marshal(map[string]string{"session": "nonexistent-token"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/session/resolve", bytes.NewBuffer(body))
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}
