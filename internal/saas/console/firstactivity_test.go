package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFirstActivityFalseWhenNoForksAndNoSandboxes asserts that the endpoint
// returns {"active": false} when the org has zero forks served and no live
// sandboxes: the org has not yet run anything.
func TestFirstActivityFalseWhenNoForksAndNoSandboxes(t *testing.T) {
	// Empty instruments (ForksServed=0) and empty sandboxes are the defaults.
	c := New(Deps{})
	r := httptest.NewRequest("GET", "/console/first-activity", nil)
	r = r.WithContext(WithCaller(r.Context(), "acct-1", "org-1"))
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got FirstActivityView
	decode(t, w, &got)
	if got.Active {
		t.Errorf("active = true, want false (no forks served, no sandboxes)")
	}
}

// TestFirstActivityTrueWhenForksServed asserts that forks_served > 0 is
// sufficient for active=true, even when no live sandboxes are present.
func TestFirstActivityTrueWhenForksServed(t *testing.T) {
	instr := NewMemInstruments()
	instr.Set(Instruments{OrgID: "org-1", ForksServed: 1})
	c := New(Deps{Instruments: instr})
	r := httptest.NewRequest("GET", "/console/first-activity", nil)
	r = r.WithContext(WithCaller(r.Context(), "acct-1", "org-1"))
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got FirstActivityView
	decode(t, w, &got)
	if !got.Active {
		t.Errorf("active = false, want true (forks_served=1)")
	}
}

// TestFirstActivityTrueWhenSandboxPresent asserts that a live sandbox is
// sufficient for active=true even when forks_served is zero.
func TestFirstActivityTrueWhenSandboxPresent(t *testing.T) {
	sandboxes := NewMemSandboxControl()
	sandboxes.Add(SandboxView{ID: "sb-1", OrgID: "org-1", Phase: "Running"})
	c := New(Deps{Sandboxes: sandboxes})
	r := httptest.NewRequest("GET", "/console/first-activity", nil)
	r = r.WithContext(WithCaller(r.Context(), "acct-1", "org-1"))
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got FirstActivityView
	decode(t, w, &got)
	if !got.Active {
		t.Errorf("active = false, want true (one sandbox present)")
	}
}

// TestFirstActivityRequiresAuth asserts that a request carrying no caller
// context is refused, matching the instruments handler's auth gate.
func TestFirstActivityRequiresAuth(t *testing.T) {
	c := New(Deps{})
	r := httptest.NewRequest("GET", "/console/first-activity", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated first-activity returned 200; must be refused")
	}
}
