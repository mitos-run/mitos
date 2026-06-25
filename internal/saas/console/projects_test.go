package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

func TestProjectsAreOrgScoped(t *testing.T) {
	mem := NewMemProjectStore()
	_, _ = mem.Create(context.Background(), "orgA", "alpha", "")
	_, _ = mem.Create(context.Background(), "orgB", "beta", "")
	c := New(Deps{Projects: mem})

	req := httptest.NewRequest("GET", "/console/projects", nil).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out struct {
		Projects []Project `json:"projects"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Projects) != 1 || out.Projects[0].Name != "alpha" {
		t.Fatalf("orgA projects = %+v, want only alpha", out.Projects)
	}
}

func TestProjectCreate(t *testing.T) {
	// POST /console/projects requires PermManageProjects; seed an owner (owner has all perms).
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	owner, org, err := accounts.SignUp(context.Background(), "proj-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	c := New(Deps{Accounts: accounts, Projects: NewMemProjectStore()})
	body := strings.NewReader(`{"name":"gamma","description":"team gamma"}`)
	req := httptest.NewRequest("POST", "/console/projects", body).
		WithContext(WithCaller(context.Background(), owner.ID, org.ID))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status %d, body=%s", rr.Code, rr.Body.String())
	}
	var p Project
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if p.OrgID != org.ID {
		t.Errorf("created project OrgID = %q, want %q", p.OrgID, org.ID)
	}
	if p.Name != "gamma" {
		t.Errorf("created project Name = %q, want %q", p.Name, "gamma")
	}
}
