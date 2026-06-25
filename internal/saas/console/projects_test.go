package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	c := New(Deps{Projects: NewMemProjectStore()})
	body := strings.NewReader(`{"name":"gamma","description":"team gamma"}`)
	req := httptest.NewRequest("POST", "/console/projects", body).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status %d", rr.Code)
	}
	var p Project
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if p.OrgID != "orgA" {
		t.Errorf("created project OrgID = %q, want %q", p.OrgID, "orgA")
	}
	if p.Name != "gamma" {
		t.Errorf("created project Name = %q, want %q", p.Name, "gamma")
	}
}
