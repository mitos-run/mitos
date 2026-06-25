package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

func TestDataRetentionRoundTripOrgScoped(t *testing.T) {
	// PUT /console/retention requires PermManageSettings; seed an owner for orgA.
	// A second account for orgB is seeded as owner of its personal org.
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()
	ownerA, orgA, err := accounts.SignUp(ctx, "dr-owner-a@example.com")
	if err != nil {
		t.Fatalf("SignUp A: %v", err)
	}
	ownerB, orgB, err := accounts.SignUp(ctx, "dr-owner-b@example.com")
	if err != nil {
		t.Fatalf("SignUp B: %v", err)
	}

	c := New(Deps{Accounts: accounts, DataRetention: NewMemDataRetentionStore()})
	put := httptest.NewRequest("PUT", "/console/retention",
		strings.NewReader(`{"sandbox_metadata_days":30,"logs_days":14,"usage_days":365,"legal_hold":true}`)).
		WithContext(WithCaller(ctx, ownerA.ID, orgA.ID))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, put)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status %d, body=%s", rr.Code, rr.Body.String())
	}
	// orgB sees the zero/default policy, not orgA's.
	getB := httptest.NewRequest("GET", "/console/retention", nil).
		WithContext(WithCaller(ctx, ownerB.ID, orgB.ID))
	rrB := httptest.NewRecorder()
	c.ServeHTTP(rrB, getB)
	if strings.Contains(rrB.Body.String(), "365") {
		t.Fatalf("orgA policy leaked into orgB: %s", rrB.Body.String())
	}
	// orgA reads back its policy.
	getA := httptest.NewRequest("GET", "/console/retention", nil).
		WithContext(WithCaller(ctx, ownerA.ID, orgA.ID))
	rrA := httptest.NewRecorder()
	c.ServeHTTP(rrA, getA)
	if !strings.Contains(rrA.Body.String(), "365") || !strings.Contains(rrA.Body.String(), "true") {
		t.Fatalf("orgA policy not persisted: %s", rrA.Body.String())
	}
}
