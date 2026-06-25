package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// accountFixture wires a console with two accounts (alice and bob), each with
// their own personal org. A fake SessionLister seeds sessions for both so the
// cross-account isolation tests can assert that session list calls never return
// the other account's sessions.
type accountFixture struct {
	con         *Console
	sessions    *MemSessionLister
	aliceAcct   string
	aliceOrg    string
	bobAcct     string
	bobOrg      string
	aliceSessID string
	bobSessID   string
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	alice, aliceOrg, err := accounts.SignUp(ctx, "alice@account-test.example")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "bob@account-test.example")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	sessions := NewMemSessionLister()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	aliceSessID := sessions.Add(SessionRecord{
		ID:        "sess-alice-1",
		AccountID: alice.ID,
		Label:     "browser",
		CreatedAt: now,
	})
	bobSessID := sessions.Add(SessionRecord{
		ID:        "sess-bob-1",
		AccountID: bob.ID,
		Label:     "cli",
		CreatedAt: now,
	})

	con := New(Deps{
		Accounts: accounts,
		Sessions: sessions,
		Audit:    NewMemAuditLog(),
		Now:      func() time.Time { return now },
	})
	return &accountFixture{
		con:         con,
		sessions:    sessions,
		aliceAcct:   alice.ID,
		aliceOrg:    aliceOrg.ID,
		bobAcct:     bob.ID,
		bobOrg:      bobOrg.ID,
		aliceSessID: aliceSessID,
		bobSessID:   bobSessID,
	}
}

func (f *accountFixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(WithCaller(r.Context(), acct, org))
	w := httptest.NewRecorder()
	f.con.ServeHTTP(w, r)
	return w
}

// --- Profile GET ---

func TestAccountProfileReturnsCallerProfile(t *testing.T) {
	f := newAccountFixture(t)
	w := f.req(t, "GET", "/console/account", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.AccountID != f.aliceAcct {
		t.Errorf("account_id = %q, want alice", resp.AccountID)
	}
	if resp.Email != "alice@account-test.example" {
		t.Errorf("email = %q, want alice@account-test.example", resp.Email)
	}
}

func TestAccountProfileNeverReturnsOtherAccount(t *testing.T) {
	f := newAccountFixture(t)
	// Alice authenticated but we ensure her profile contains her account_id only.
	w := f.req(t, "GET", "/console/account", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.bobAcct) {
		t.Errorf("alice profile response leaked bob account id: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bob@") {
		t.Errorf("alice profile response leaked bob email: %s", w.Body.String())
	}
}

// --- Profile PATCH ---

func TestAccountProfilePatchUpdatesCallerOnly(t *testing.T) {
	f := newAccountFixture(t)
	body := `{"display_name":"Alice A","timezone":"Europe/Berlin","locale":"de-DE"}`
	w := f.req(t, "PATCH", "/console/account", body, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DisplayName != "Alice A" {
		t.Errorf("display_name = %q, want Alice A", resp.DisplayName)
	}
	if resp.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q, want Europe/Berlin", resp.Timezone)
	}
	if resp.Locale != "de-DE" {
		t.Errorf("locale = %q, want de-DE", resp.Locale)
	}
	// Bob's profile must be unchanged.
	bw := f.req(t, "GET", "/console/account", "", f.bobAcct, f.bobOrg)
	var bobResp AccountView
	if err := json.Unmarshal(bw.Body.Bytes(), &bobResp); err != nil {
		t.Fatalf("decode bob: %v", err)
	}
	if bobResp.DisplayName == "Alice A" || bobResp.Timezone == "Europe/Berlin" {
		t.Errorf("patch leaked into bob profile: %+v", bobResp)
	}
}

func TestAccountProfilePatchAudits(t *testing.T) {
	f := newAccountFixture(t)
	f.req(t, "PATCH", "/console/account", `{"display_name":"New Name"}`, f.aliceAcct, f.aliceOrg)
	// Verify the audit event was recorded by checking that the audit log records the profile update.
	// We cannot query audit directly from the fixture here, but we can call /console/audit
	// as alice and look for the profile.update action.
	w := f.req(t, "GET", "/console/audit", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("audit status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "profile.update") {
		t.Errorf("profile.update not in audit log: %s", w.Body.String())
	}
}

// --- Sessions GET ---

func TestAccountSessionsListOnlyCallerSessions(t *testing.T) {
	f := newAccountFixture(t)
	w := f.req(t, "GET", "/console/account/sessions", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Sessions []SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	// Must have alice's session only; never bob's.
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1 (alice only); got %+v", len(resp.Sessions), resp.Sessions)
	}
	if resp.Sessions[0].ID != f.aliceSessID {
		t.Errorf("session id = %q, want alice sess id %q", resp.Sessions[0].ID, f.aliceSessID)
	}
	if strings.Contains(w.Body.String(), f.bobSessID) {
		t.Errorf("alice sessions response leaked bob session: %s", w.Body.String())
	}
}

func TestAccountSessionsNoCrossAccountLeak(t *testing.T) {
	f := newAccountFixture(t)
	// Request as bob; alice's session must not appear.
	w := f.req(t, "GET", "/console/account/sessions", "", f.bobAcct, f.bobOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.aliceSessID) {
		t.Errorf("bob sessions response leaked alice session: %s", w.Body.String())
	}
}

// --- Session DELETE one ---

func TestAccountSessionRevokeOwn(t *testing.T) {
	f := newAccountFixture(t)
	w := f.req(t, "DELETE", "/console/account/sessions/"+f.aliceSessID, "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// After revoke, the session should no longer appear in the list.
	lw := f.req(t, "GET", "/console/account/sessions", "", f.aliceAcct, f.aliceOrg)
	if strings.Contains(lw.Body.String(), f.aliceSessID) {
		t.Errorf("revoked session still appears in list: %s", lw.Body.String())
	}
}

func TestAccountSessionRevokeRefusesCrossAccount(t *testing.T) {
	f := newAccountFixture(t)
	// Alice tries to revoke bob's session: must return 404 (indistinguishable from not found).
	w := f.req(t, "DELETE", "/console/account/sessions/"+f.bobSessID, "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-account revoke = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	// Bob's session must still be there.
	bw := f.req(t, "GET", "/console/account/sessions", "", f.bobAcct, f.bobOrg)
	if !strings.Contains(bw.Body.String(), f.bobSessID) {
		t.Errorf("bob session disappeared after alice's failed revoke: %s", bw.Body.String())
	}
}

// --- Sessions DELETE all ---

func TestAccountSessionsRevokeAll(t *testing.T) {
	f := newAccountFixture(t)
	// Seed a second session for alice.
	f.sessions.Add(SessionRecord{
		ID:        "sess-alice-2",
		AccountID: f.aliceAcct,
		Label:     "cli",
		CreatedAt: time.Now(),
	})
	w := f.req(t, "DELETE", "/console/account/sessions", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	lw := f.req(t, "GET", "/console/account/sessions", "", f.aliceAcct, f.aliceOrg)
	var resp struct {
		Sessions []SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("expected 0 sessions after RevokeAll, got %d: %+v", len(resp.Sessions), resp.Sessions)
	}
	// Bob's sessions must survive.
	bw := f.req(t, "GET", "/console/account/sessions", "", f.bobAcct, f.bobOrg)
	if !strings.Contains(bw.Body.String(), f.bobSessID) {
		t.Errorf("bob session was removed by alice's RevokeAll: %s", bw.Body.String())
	}
}
