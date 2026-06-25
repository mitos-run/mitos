# Console B3b: audit retention + export and SIEM sinks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Make the audit log enterprise-grade: an NDJSON export, a per-org retention policy, and an audit-sink registry (export to SIEMs) with a working webhook sink that receives events as they are recorded.

**Architecture:** Go BFF gains: an `/console/audit/export` NDJSON endpoint; a per-org retention store (the policy; GC enforcement is the controller's job, #163); an `AuditSink` registry seam (config CRUD per org) plus a `DispatchingRecorder` that wraps the existing `AuditRecorder` and best-effort delivers each recorded event to the org's enabled sinks; and a testable `webhook` sink. S3/Splunk/Datadog are accepted config types with drivers as documented follow-ups (the registry + dispatch are the load-bearing seam). The SPA's Audit view gains retention, export, and sink-management panels.

**Tech Stack:** Go (`internal/saas/console`, `net/http`, `encoding/json`), React+Vite+TS, TanStack Query, Vitest + vitest-axe.

**Scope note:** B3b of the B3 split. B3c (data-retention), B3d (RBAC), B3e (SAML), B3f (SCIM) follow. The auth-critical B3e/B3f get threat-model deltas + named-human review; B3b is moderate-risk (audit-data egress) and follows the seam pattern.

## Global Constraints

- **Punctuation (strict):** no em/en dashes anywhere. Only `.` `,` `;` `:`; ASCII `-`. Verify each commit.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only.
- **Org-scoped isolation:** every new endpoint reads org from request context only; a cross-org id never leaks; the export returns only the caller org's events; the sink registry is per-org. Each new endpoint gets a cross-org isolation test.
- **Audit integrity:** an audit sink failure NEVER fails the user action or the Record (best-effort delivery, logged). No secret value is ever exported or delivered (the existing AuditEvent carries no secret; keep it that way). Sink configs may carry an endpoint URL but NEVER a credential in a logged/returned field; a credential-shaped field is write-only.
- **Go style:** `fmt.Errorf("...: %w", err)`; gofmt clean; BOTH golangci-lint invocations clean; production code not errcheck-excluded.
- **Responsive + accessible (spec 4.6):** new Audit panels responsive + labelled; axe zero violations.
- **TypeScript strict** clean; SPA suite exits 0; `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/console/audit_export.go` (create) - the NDJSON export handler + retention store + handlers.
- `internal/saas/console/audit_sinks.go` (create) - `AuditSink`, `SinkConfig`, `SinkRegistry` seam + in-memory fake + the `webhook` sink + `DispatchingRecorder`.
- `internal/saas/console/console.go` (modify) - register routes + deps defaults.
- Go tests alongside.
- `web/app/src/api.ts`, `web/app/src/data/audit.ts` (create), `web/app/src/views/Audit.tsx` (modify) - retention/export/sinks panels.
- `web/packages/brand/src/base.css` (modify) - panel styles.
- Tests alongside.

---

### Task 1: Audit export + retention policy (Go)

**Files:**
- Create: `internal/saas/console/audit_export.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/audit_export_test.go`

**Interfaces:**
- `GET /console/audit/export` -> `text/plain` (NDJSON: one JSON `AuditEvent` per line) for the caller org, `Content-Disposition: attachment; filename="audit.ndjson"`.
- A per-org retention policy: `RetentionStore` seam (`Get(ctx, orgID) (int, error)` days, `Set(ctx, orgID, days int) error`) + in-memory fake; `GET /console/audit/retention` -> `{days}`, `PUT /console/audit/retention` body `{days}`. `Deps.Retention` nil-defaults to the fake.

- [ ] **Step 1: Write the failing test `internal/saas/console/audit_export_test.go`**

```go
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
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/console/ -run 'TestAuditExport|TestAuditRetention'`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement `internal/saas/console/audit_export.go`** (the `RetentionStore` seam + `MemRetentionStore`, `handleAuditExport` writing NDJSON via `json.Encoder` per event for the caller org, `handleGetRetention`/`handleSetRetention`). Wire `Deps.Retention` nil-default + the 3 routes in `console.go`. Add the routes to the auth-gate table.

- [ ] **Step 4: Run; full package; gofmt; both lint**

Run: `go test ./internal/saas/console/` ; `gofmt -l internal/saas/console/audit_export.go internal/saas/console/console.go`
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...`
Expected: green/clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/console/audit_export.go internal/saas/console/console.go internal/saas/console/audit_export_test.go
git commit -s -m "feat(console): audit NDJSON export and per-org retention policy"
```

---

### Task 2: Audit sink registry + webhook sink + dispatch (Go)

**Files:**
- Create: `internal/saas/console/audit_sinks.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/audit_sinks_test.go`

**Interfaces:**
- `SinkConfig = { ID, OrgID, Type, Endpoint string; Enabled bool; CreatedAt time.Time }` (Type in `webhook|s3|splunk|datadog`; JSON tags; NO credential field returned). 
- `SinkRegistry` seam: `List(ctx, orgID) []SinkConfig`, `Add(ctx, orgID, type, endpoint string) (SinkConfig, error)`, `Delete(ctx, orgID, id string) error` (org-scoped; cross-org delete -> ErrNotFound). In-memory fake.
- `AuditSink` interface: `Deliver(ctx, SinkConfig, AuditEvent) error`; a `webhookSink` that POSTs the event as JSON to `Endpoint`.
- `DispatchingRecorder`: wraps an `AuditRecorder` + the `SinkRegistry` + a sink dispatcher; `Record` calls the inner recorder, then BEST-EFFORT delivers to the org's enabled sinks (failures logged, never returned). `List` delegates.
- `GET/POST/DELETE /console/audit/sinks` endpoints (org-scoped). `Deps.Sinks` nil-defaults to an empty registry; the console may wrap `deps.Audit` with `DispatchingRecorder` when `Sinks` is set.

- [ ] **Step 1: Write the failing test `internal/saas/console/audit_sinks_test.go`**

```go
package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSinksOrgScopedCRUD(t *testing.T) {
	reg := NewMemSinkRegistry()
	c := New(Deps{Sinks: reg})
	post := httptest.NewRequest("POST", "/console/audit/sinks", nil)
	post = httptest.NewRequest("POST", "/console/audit/sinks", strings_NewReader(`{"type":"webhook","endpoint":"https://siem.example/hook"}`)).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, post)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create sink status %d", rr.Code)
	}
	// orgB sees no orgA sink.
	get := httptest.NewRequest("GET", "/console/audit/sinks", nil).WithContext(WithCaller(context.Background(), "acct", "orgB"))
	rr2 := httptest.NewRecorder()
	c.ServeHTTP(rr2, get)
	if want := `"sinks":[]`; !contains(rr2.Body.String(), want) {
		t.Fatalf("orgB should see no sinks, got %s", rr2.Body.String())
	}
}

func TestDispatchDeliversToEnabledSink(t *testing.T) {
	reg := NewMemSinkRegistry()
	cfg, _ := reg.Add(context.Background(), "orgA", "webhook", "https://x")
	var mu sync.Mutex
	delivered := 0
	fake := sinkFunc(func(_ context.Context, c SinkConfig, _ AuditEvent) error {
		if c.ID == cfg.ID {
			mu.Lock(); delivered++; mu.Unlock()
		}
		return nil
	})
	rec := NewDispatchingRecorder(NewMemAuditLog(), reg, fake)
	_ = rec.Record(context.Background(), AuditEvent{OrgID: "orgA", Action: "key.create"})
	rec.WaitForDispatch() // test helper that blocks until best-effort dispatch drains
	mu.Lock(); got := delivered; mu.Unlock()
	if got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
}
```

(Provide `strings_NewReader`/`contains` via the standard library in the real test; `sinkFunc` is a tiny adapter; `WaitForDispatch` makes the best-effort dispatch deterministic in tests, e.g. a `sync.WaitGroup` the recorder exposes for tests.)

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/console/ -run 'TestSinks|TestDispatch'`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/saas/console/audit_sinks.go`** (the config + registry + fake; the `webhookSink` using `http.Client` with a short timeout; the `DispatchingRecorder` whose `Record` delegates then dispatches to enabled sinks via a `sync.WaitGroup` so tests can wait; a `sinkFunc` adapter type). Wire `Deps.Sinks` + routes + (optionally) wrap `deps.Audit` with `DispatchingRecorder` when `Sinks` is set, in `console.go`. Add the routes to the auth-gate table. Dispatch failures are logged via `deps.Log`, never returned. No credential is logged.

- [ ] **Step 4: Run; full package; gofmt; both lint; go build**

Run: `go test ./internal/saas/console/` ; `go build ./...` ; `gofmt -l internal/saas/console/audit_sinks.go internal/saas/console/console.go`
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...`
Expected: green/clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/console/audit_sinks.go internal/saas/console/console.go internal/saas/console/audit_sinks_test.go
git commit -s -m "feat(console): audit sink registry with a webhook sink and best-effort dispatch"
```

---

### Task 3: Audit view retention + export + sinks (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/views/Audit.tsx`
- Create: `web/app/src/data/audit.ts`
- Test: `web/app/src/views/Audit.test.tsx` (extend or add)

**Interfaces:**
- `SinkView = { id, type, endpoint, enabled, created_at }`; methods `auditRetention()`, `setAuditRetention(days)`, `auditSinks()`, `addAuditSink(type, endpoint)`, `deleteAuditSink(id)`, `auditExportUrl()` (returns `/console/audit/export` for an anchor download).
- Hooks in `data/audit.ts`: `useAuditRetention`, `useSetRetention`, `useAuditSinks`, `useAddSink`, `useDeleteSink`.
- `Audit.tsx` gains a "Retention and export" panel (a days number input + Save, and an Export NDJSON download link/button) and a "Sinks" panel (list type+endpoint+enabled with Delete; an add form: type select + endpoint input + Add). The existing filterable event table stays.

- [ ] **Step 1: Write the failing test** (extend `web/app/src/views/Audit.test.tsx`): render `/audit` (mock capabilities + audit + retention + sinks), assert the retention input shows the current days, and a sink row renders; the Export control links to `/console/audit/export`.

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/Audit.test.tsx`
Expected: FAIL.

- [ ] **Step 3: Implement the api.ts additions, `data/audit.ts`, and the Audit.tsx panels.** Export is a plain anchor to `/console/audit/export` (browser download). Retention save -> toast. Sink add/delete -> optimistic + toast.

- [ ] **Step 4: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Audit.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/audit.ts web/app/src/views/Audit.tsx web/app/src/views/Audit.test.tsx
git commit -s -m "feat(console): audit retention, NDJSON export, and sink management UI"
```

---

### Task 4: Styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/Audit.a11y.test.tsx`

- [ ] **Step 1: Append token-driven panel styles** (`.panel` section, form rows; reuse `.tbl`/`.badge`/form input styles). No raw hex. Mobile rule.
- [ ] **Step 2: Write the axe a11y test** for `/audit` (retention input + sink form labelled; assert zero violations). Fix any real violation.
- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0) ; `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `go test ./internal/saas/...` (green) ; both golangci-lint invocations clean
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views/Audit.tsx internal/saas/console/audit_export.go internal/saas/console/audit_sinks.go web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/Audit.a11y.test.tsx
git commit -s -m "feat(console): audit panel styles and B3b accessibility checks"
```

---

## Self-Review

**Spec coverage (section 5.2):** NDJSON export (Task 1), per-org retention policy (Task 1; GC enforcement is #163's job, noted), audit sink registry + webhook sink + stream-on-write dispatch (Task 2), and the retention/export/sinks UI (Task 3). Covered. S3/Splunk/Datadog drivers are accepted config types with drivers as documented follow-ups.

**Audit integrity:** sink delivery is best-effort and never fails Record (Task 2); no secret/credential is exported, delivered, returned, or logged; the export and registry are org-scoped (Tasks 1, 2 isolation tests).

**Type consistency:** Go `SinkConfig` JSON tags match the TS `SinkView` (Task 3); the export/retention/sink endpoints (Tasks 1, 2) back the hooks (Task 3). No drift.
