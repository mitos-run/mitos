package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Instruments (proof surface, #276) ---

// TestInstrumentsReturnsCallerOrgMeasurements asserts the instrument-panel read
// returns the caller org's MEASURED proof metrics: warm-claim activate
// latency, forks served, and CoW density (the same #33 metering primitive that
// bills). These are the org's own numbers — never fabricated competitor numbers.
func TestInstrumentsReturnsCallerOrgMeasurements(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/instruments", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got Instruments
	decode(t, w, &got)
	if got.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want %q", got.OrgID, f.aliceOrg)
	}
	if got.ActivateP50Millis != 27 {
		t.Errorf("activate_p50_ms = %v, want 27 (seeded)", got.ActivateP50Millis)
	}
	if got.ForksServed != 10 {
		t.Errorf("forks_served = %v, want 10 (seeded)", got.ForksServed)
	}
	if got.CoWSavingsBytes <= 0 {
		t.Errorf("cow_savings_bytes = %v, want > 0", got.CoWSavingsBytes)
	}
}

// TestInstrumentsScopedToCallerOrg asserts a request authenticated as bob never
// observes alice's measurements: the proof read is org-scoped like every other
// console endpoint.
func TestInstrumentsScopedToCallerOrg(t *testing.T) {
	f := newFixture(t)
	wb := f.req(t, "GET", "/console/instruments", "", f.bobAcct, f.bobOrg)
	if wb.Code != http.StatusOK {
		t.Fatalf("bob status = %d body=%s", wb.Code, wb.Body.String())
	}
	var bob Instruments
	decode(t, wb, &bob)
	if bob.OrgID != f.bobOrg {
		t.Errorf("org_id = %q, want bob %q", bob.OrgID, f.bobOrg)
	}
	if bob.ForksServed == 10 {
		t.Errorf("bob saw alice's forks_served (10); cross-org leak: %+v", bob)
	}
}

// TestInstrumentsRequiresAuth asserts the instrument read is NOT public: unlike
// /console/capabilities it returns org data, so a request with no caller context
// is refused.
func TestInstrumentsRequiresAuth(t *testing.T) {
	c := New(Deps{})
	r := httptest.NewRequest("GET", "/console/instruments", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated instruments returned 200; must be refused")
	}
}
