package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDataRetentionRoundTripOrgScoped(t *testing.T) {
	c := New(Deps{DataRetention: NewMemDataRetentionStore()})
	put := httptest.NewRequest("PUT", "/console/retention", strings.NewReader(`{"sandbox_metadata_days":30,"logs_days":14,"usage_days":365,"legal_hold":true}`)).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, put)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status %d", rr.Code)
	}
	// orgB sees the zero/default policy, not orgA's.
	getB := httptest.NewRequest("GET", "/console/retention", nil).WithContext(WithCaller(context.Background(), "acct", "orgB"))
	rrB := httptest.NewRecorder()
	c.ServeHTTP(rrB, getB)
	if strings.Contains(rrB.Body.String(), "365") {
		t.Fatalf("orgA policy leaked into orgB: %s", rrB.Body.String())
	}
	// orgA reads back its policy.
	getA := httptest.NewRequest("GET", "/console/retention", nil).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rrA := httptest.NewRecorder()
	c.ServeHTTP(rrA, getA)
	if !strings.Contains(rrA.Body.String(), "365") || !strings.Contains(rrA.Body.String(), "true") {
		t.Fatalf("orgA policy not persisted: %s", rrA.Body.String())
	}
}
