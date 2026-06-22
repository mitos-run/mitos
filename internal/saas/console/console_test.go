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
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

// fixture is two orgs (alice and bob) wired through the console BFF, with each
// service seeded with one org's data, so every test can assert that a request
// authenticated as alice sees ONLY alice's data and never bob's.
type fixture struct {
	con       *Console
	accounts  *saas.AccountService
	usage     *usage.MemUsageStore
	ledger    *billing.MemCreditLedger
	status    *billing.MemStatusStore
	caps      *billing.MemSpendCapStore
	sandboxes *MemSandboxControl
	templates *MemTemplateLister
	audit     *MemAuditLog
	instr     *MemInstruments

	aliceAcct, bobAcct string
	aliceOrg, bobOrg   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	alice, aliceOrg, err := accounts.SignUp(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	f := &fixture{
		accounts:  accounts,
		usage:     usage.NewMemUsageStore(),
		ledger:    billing.NewMemCreditLedger(),
		status:    billing.NewMemStatusStore(),
		caps:      billing.NewMemSpendCapStore(),
		sandboxes: NewMemSandboxControl(),
		templates: NewMemTemplateLister(),
		audit:     NewMemAuditLog(),
		instr:     NewMemInstruments(),
		aliceAcct: alice.ID, bobAcct: bob.ID,
		aliceOrg: aliceOrg.ID, bobOrg: bobOrg.ID,
	}

	now := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)

	// Seed a key for each org so the keys list is non-trivial per org.
	if _, err := accounts.CreateKey(ctx, alice.ID, saas.CreateKeyRequest{OrgID: aliceOrg.ID, Name: "alice-key", Scopes: []string{saas.ScopeSandboxes}}); err != nil {
		t.Fatalf("seed alice key: %v", err)
	}
	if _, err := accounts.CreateKey(ctx, bob.ID, saas.CreateKeyRequest{OrgID: bobOrg.ID, Name: "bob-key", Scopes: []string{saas.ScopeSandboxes}}); err != nil {
		t.Fatalf("seed bob key: %v", err)
	}

	// Seed usage records per org.
	_ = f.usage.UpsertRecord(ctx, usage.UsageRecord{OrgID: aliceOrg.ID, SandboxID: "a-sb", Window: now, VCPUSeconds: 100, EgressBytes: 1 << 30})
	_ = f.usage.UpsertRecord(ctx, usage.UsageRecord{OrgID: bobOrg.ID, SandboxID: "b-sb", Window: now, VCPUSeconds: 999})

	// Seed billing per org.
	_ = billing.GrantSignupCredit(ctx, f.ledger, aliceOrg.ID, billing.USD(50), now)
	_ = billing.GrantSignupCredit(ctx, f.ledger, bobOrg.ID, billing.USD(123), now)
	_ = f.status.SetStatus(ctx, aliceOrg.ID, billing.StatusActive)
	_ = f.status.SetStatus(ctx, bobOrg.ID, billing.StatusSuspended)
	_ = f.caps.Set(ctx, billing.SpendCap{OrgID: aliceOrg.ID, SoftCap: billing.USD(80), HardCap: billing.USD(100)})

	// Seed live sandboxes per org.
	f.sandboxes.Add(SandboxView{ID: "sb-alice-1", OrgID: aliceOrg.ID, Template: "py", Node: "n1", Phase: "Running", VCPUs: 2})
	f.sandboxes.Add(SandboxView{ID: "sb-bob-1", OrgID: bobOrg.ID, Template: "node", Node: "n2", Phase: "Running", VCPUs: 4})

	// Seed templates per org.
	f.templates.Add(TemplateView{Name: "alice-tmpl", OrgID: aliceOrg.ID, Image: "alice/img"})
	f.templates.Add(TemplateView{Name: "bob-tmpl", OrgID: bobOrg.ID, Image: "bob/img"})

	// Seed measured proof metrics per org (the instrument-panel source).
	f.instr.Set(Instruments{OrgID: aliceOrg.ID, ActivateP50Millis: 27, ActivateP99Millis: 41, ForksServed: 10, CoWSavingsBytes: 2304 << 20, MarginalBytesPerFork: 3 << 20})
	f.instr.Set(Instruments{OrgID: bobOrg.ID, ActivateP50Millis: 31, ActivateP99Millis: 52, ForksServed: 3, CoWSavingsBytes: 512 << 20, MarginalBytesPerFork: 4 << 20})

	f.con = New(Deps{
		Accounts:    accounts,
		Usage:       f.usage,
		Billing:     BillingReader{Ledger: f.ledger, Status: f.status, Caps: f.caps, Rates: billing.DefaultRates()},
		Sandboxes:   f.sandboxes,
		Templates:   f.templates,
		Audit:       f.audit,
		Instruments: f.instr,
		Now:         func() time.Time { return now },
	})
	return f
}

// asAlice / asBob build an authenticated request whose context carries the org
// the BFF must scope to. The org is taken ONLY from the context, never the URL.
func (f *fixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
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

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
}

// --- Keys ---

func TestKeysListReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/keys", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OrgID string    `json:"org_id"`
		Keys  []KeyView `json:"keys"`
	}
	decode(t, w, &resp)
	if resp.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want alice", resp.OrgID)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].Name != "alice-key" {
		t.Fatalf("keys = %+v, want only alice-key", resp.Keys)
	}
	// Masked only: no raw key, no hash, prefix is masked.
	if !strings.HasSuffix(resp.Keys[0].Prefix, "...") {
		t.Errorf("key prefix %q is not masked", resp.Keys[0].Prefix)
	}
	if strings.Contains(w.Body.String(), "hash") || strings.Contains(w.Body.String(), "mitos_live_") && !strings.Contains(w.Body.String(), "...") {
		t.Errorf("keys body leaked secret material: %s", w.Body.String())
	}
}

func TestKeysCreateReturnsRawOnceAndMasksThereafter(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/keys", `{"name":"new","scopes":["sandboxes"]}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		RawKey string  `json:"raw_key"`
		Key    KeyView `json:"key"`
	}
	decode(t, w, &created)
	if !strings.HasPrefix(created.RawKey, "mitos_live_") {
		t.Fatalf("raw key not returned on create: %q", created.RawKey)
	}
	// The list afterward must show the masked prefix, never the raw key.
	lw := f.req(t, "GET", "/console/keys", "", f.aliceAcct, f.aliceOrg)
	if strings.Contains(lw.Body.String(), created.RawKey) {
		t.Errorf("raw key leaked into the keys list")
	}
}

func TestKeysCreateIsScopedToCallerOrg(t *testing.T) {
	f := newFixture(t)
	// Alice authenticated for bob's org: she is not a member, must be forbidden.
	w := f.req(t, "POST", "/console/keys", `{"name":"x"}`, f.aliceAcct, f.bobOrg)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org create status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestKeysRevokeRefusesCrossOrgKey(t *testing.T) {
	f := newFixture(t)
	// Find bob's key id.
	bobKeys, _ := f.accounts.ListKeys(context.Background(), f.bobAcct, f.bobOrg)
	if len(bobKeys) == 0 {
		t.Fatal("expected bob to have a key")
	}
	// Alice tries to revoke bob's key (authenticated as alice for her own org).
	w := f.req(t, "POST", "/console/keys/"+bobKeys[0].ID+"/revoke", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org revoke status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	// Bob's key must still verify (not revoked).
	if bobKeys[0].IsRevoked() {
		t.Errorf("bob key should not be revoked by alice")
	}
}

// --- Usage ---

func TestUsageReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/usage", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp usage.UsageResponse
	decode(t, w, &resp)
	if resp.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want alice", resp.OrgID)
	}
	if len(resp.Records) != 1 || resp.Records[0].SandboxID != "a-sb" {
		t.Fatalf("usage records = %+v, want only alice a-sb", resp.Records)
	}
	if resp.Totals.VCPUSeconds != 100 {
		t.Errorf("vcpu seconds = %v, want 100 (alice only, not bob's 999)", resp.Totals.VCPUSeconds)
	}
}

// --- Billing ---

func TestBillingReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/billing", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var v BillingView
	decode(t, w, &v)
	if v.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want alice", v.OrgID)
	}
	if v.Status != billing.StatusActive {
		t.Errorf("status = %q, want active (not bob's suspended)", v.Status)
	}
	if v.BalanceCents != int64(billing.USD(50)) {
		t.Errorf("balance = %d, want alice 5000 (not bob's 12300)", v.BalanceCents)
	}
	if v.HardCapCents != int64(billing.USD(100)) {
		t.Errorf("hard cap = %d, want alice 10000", v.HardCapCents)
	}
	if v.SpendCents == 0 {
		t.Errorf("expected non-zero spend from alice usage")
	}
	for _, e := range v.LedgerEntries {
		if e.OrgID != f.aliceOrg {
			t.Errorf("ledger entry for foreign org %q leaked", e.OrgID)
		}
	}
}

func TestBillingBobSeesOwnSuspendedStatus(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/billing", "", f.bobAcct, f.bobOrg)
	var v BillingView
	decode(t, w, &v)
	if v.Status != billing.StatusSuspended || v.BalanceCents != int64(billing.USD(123)) {
		t.Errorf("bob billing = %+v, want suspended + 12300", v)
	}
}

// --- Live sandboxes ---

func TestSandboxListReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/sandboxes", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Sandboxes []SandboxView `json:"sandboxes"`
	}
	decode(t, w, &resp)
	if len(resp.Sandboxes) != 1 || resp.Sandboxes[0].ID != "sb-alice-1" {
		t.Fatalf("sandboxes = %+v, want only sb-alice-1", resp.Sandboxes)
	}
}

func TestSandboxInspectRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	// Alice tries to inspect bob's sandbox by id: must be not_found (indistinguishable).
	w := f.req(t, "GET", "/console/sandboxes/sb-bob-1", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org inspect status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSandboxTerminateRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "DELETE", "/console/sandboxes/sb-bob-1", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org terminate status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	// Bob's sandbox must survive.
	if _, err := f.sandboxes.Get(context.Background(), f.bobOrg, "sb-bob-1"); err != nil {
		t.Errorf("bob sandbox was terminated by alice: %v", err)
	}
}

func TestSandboxTerminateOwnSucceedsAndAudits(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "DELETE", "/console/sandboxes/sb-alice-1", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("terminate own status = %d body=%s", w.Code, w.Body.String())
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg)
	if len(events) == 0 || events[0].Action != "sandbox.terminate" {
		t.Errorf("expected a sandbox.terminate audit event, got %+v", events)
	}
	// The audit event must NOT have landed in bob's log.
	bobEvents, _ := f.audit.List(context.Background(), f.bobOrg)
	if len(bobEvents) != 0 {
		t.Errorf("alice terminate leaked into bob audit log: %+v", bobEvents)
	}
}

// --- Members ---

func TestMembersReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/members", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Members []MemberView `json:"members"`
	}
	decode(t, w, &resp)
	if len(resp.Members) != 1 || resp.Members[0].AccountID != f.aliceAcct {
		t.Fatalf("members = %+v, want only alice", resp.Members)
	}
}

func TestMembersRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	// Alice authenticated for bob's org id.
	w := f.req(t, "GET", "/console/members", "", f.aliceAcct, f.bobOrg)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org members status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// --- Audit ---

func TestAuditReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	// Generate an audit event in alice's org (a key create).
	f.req(t, "POST", "/console/keys", `{"name":"audited"}`, f.aliceAcct, f.aliceOrg)
	w := f.req(t, "GET", "/console/audit", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Events []AuditEvent `json:"events"`
	}
	decode(t, w, &resp)
	if len(resp.Events) == 0 {
		t.Fatal("expected at least one audit event for alice")
	}
	for _, e := range resp.Events {
		if e.OrgID != f.aliceOrg {
			t.Errorf("audit event for foreign org %q leaked", e.OrgID)
		}
	}
}

func TestAuditRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/audit", "", f.aliceAcct, f.bobOrg)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org audit status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// --- Templates ---

func TestTemplatesReturnsOnlyCallerOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/templates", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Templates []TemplateView `json:"templates"`
	}
	decode(t, w, &resp)
	if len(resp.Templates) != 1 || resp.Templates[0].Name != "alice-tmpl" {
		t.Fatalf("templates = %+v, want only alice-tmpl", resp.Templates)
	}
}

// --- Auth gate ---

func TestEveryEndpointRefusesMissingOrgContext(t *testing.T) {
	f := newFixture(t)
	endpoints := []struct{ method, target string }{
		{"GET", "/console/keys"},
		{"POST", "/console/keys"},
		{"POST", "/console/keys/x/revoke"},
		{"GET", "/console/usage"},
		{"GET", "/console/billing"},
		{"GET", "/console/sandboxes"},
		{"GET", "/console/sandboxes/x"},
		{"DELETE", "/console/sandboxes/x"},
		{"GET", "/console/members"},
		{"GET", "/console/audit"},
		{"GET", "/console/templates"},
	}
	for _, ep := range endpoints {
		// No WithCaller: the request carries no org context.
		r := httptest.NewRequest(ep.method, ep.target, nil)
		w := httptest.NewRecorder()
		f.con.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without org context = %d, want 401", ep.method, ep.target, w.Code)
		}
	}
}
