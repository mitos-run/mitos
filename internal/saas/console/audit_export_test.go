package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditExportIsOrgScopedNDJSON(t *testing.T) {
	audit := NewMemAuditLog()
	_ = audit.Record(context.Background(), AuditEvent{OrgID: "orgA", Action: "key.create", Target: "k1"})
	_ = audit.Record(context.Background(), AuditEvent{OrgID: "orgB", Action: "secret.create", Target: "s9"})
	c := New(Deps{Audit: audit})

	req := httptest.NewRequest("GET", "/console/audit/export", nil).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "key.create") {
		t.Fatalf("missing orgA event")
	}
	if strings.Contains(body, "secret.create") || strings.Contains(body, "s9") {
		t.Fatalf("orgB event leaked into orgA export")
	}
	// NDJSON: each non-empty line is a JSON object.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "{") {
			t.Fatalf("not NDJSON line: %q", line)
		}
	}
}

func TestAuditRetentionRoundTrip(t *testing.T) {
	c := New(Deps{Retention: NewMemRetentionStore()})
	put := httptest.NewRequest("PUT", "/console/audit/retention", strings.NewReader(`{"days":90}`)).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, put)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status %d", rr.Code)
	}
	get := httptest.NewRequest("GET", "/console/audit/retention", nil).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr2 := httptest.NewRecorder()
	c.ServeHTTP(rr2, get)
	if !strings.Contains(rr2.Body.String(), "90") {
		t.Fatalf("retention not persisted: %s", rr2.Body.String())
	}
}
