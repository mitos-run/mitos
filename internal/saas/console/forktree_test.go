package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForkTreeIsOrgScoped(t *testing.T) {
	mem := NewMemForkTree()
	mem.Set("orgA", []ForkNode{{ID: "s1", ParentID: "", Phase: "Running", PrivateDirtyBytes: 3 << 20, SharedBytes: 200 << 20}})
	mem.Set("orgB", []ForkNode{{ID: "s9", ParentID: "", Phase: "Running"}})
	c := New(Deps{ForkTree: mem})

	// orgA sees only its own node.
	req := httptest.NewRequest("GET", "/console/forktree", nil)
	req = req.WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ForkTree
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "s1" {
		t.Fatalf("orgA tree = %+v, want exactly s1", got.Nodes)
	}
	// orgB's node must never appear in orgA's response.
	for _, n := range got.Nodes {
		if n.ID == "s9" {
			t.Fatalf("orgB node s9 leaked into orgA forktree")
		}
	}
}

func TestForkTreeRequiresOrgContext(t *testing.T) {
	c := New(Deps{ForkTree: NewMemForkTree()})
	req := httptest.NewRequest("GET", "/console/forktree", nil)
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no org context)", rr.Code)
	}
}
