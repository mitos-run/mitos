# Console B3c: data-retention policies Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** A per-org data-retention policy for terminated-sandbox metadata, sandbox logs, and usage records, plus a legal-hold toggle, surfaced in a "Data and retention" view. The console stores and exposes the policy; the controller GC (#163) enforces it.

**Architecture:** Go BFF gains a `DataRetentionStore` seam (per-org policy) + `GET/PUT /console/retention`. The SPA gains a "Data and retention" view. B3b's audit-specific retention (`/console/audit/retention`) is untouched and stays on the Audit page; this view covers the other resources + legal hold. No GC is implemented here (it is the controller's job, #163); the console persists the policy and a "what gets deleted when" preview.

**Tech Stack:** Go (`internal/saas/console`), React+Vite+TS, TanStack Query, Vitest + vitest-axe.

**Scope note:** B3c of the B3 split. B3d (custom roles + per-project RBAC) follows; B3e (SAML) + B3f (SCIM) are auth-critical (threat-model deltas + named-human review).

## Global Constraints

- **Punctuation (strict):** no em/en dashes anywhere. Only `.` `,` `;` `:`; ASCII `-`. Verify each commit.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only.
- **Org-scoped isolation:** the retention endpoint reads org from request context only; per-org; cross-org never leaks; added to the auth-gate table; isolation tested.
- **Honesty:** the view states plainly that the GC enforces the policy (the console stores it); a legal hold pauses deletion. No fabricated "deleted N records" claim.
- **Go style:** `fmt.Errorf("...: %w", err)`; gofmt clean; BOTH golangci-lint invocations clean.
- **Responsive + accessible (spec 4.6):** the view is responsive + labelled; axe zero violations.
- **TypeScript strict** clean; SPA suite exits 0; `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/console/retention.go` (create) - `DataRetentionPolicy`, `DataRetentionStore` seam + fake, handlers.
- `internal/saas/console/console.go` (modify) - routes + dep default.
- Go tests alongside.
- `web/app/src/api.ts`, `web/app/src/data/retention.ts` (create), `web/app/src/views/Retention.tsx` (create), `web/app/src/nav/routes.tsx` (route).
- `web/packages/brand/src/base.css` (modify).
- Tests alongside.

---

### Task 1: Data-retention store + endpoint (Go)

**Files:**
- Create: `internal/saas/console/retention.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/retention_test.go`

Read `internal/saas/console/audit_export.go` (the B3b `RetentionStore` pattern), `forktree.go`/`projects.go` (seam + nil-default), `console.go` (Deps, New, routes, caller, decodeBody, writeJSON, apierr), and `console_test.go` (the auth-gate table) first.

**Interfaces:**
- `DataRetentionPolicy = { SandboxMetadataDays, LogsDays, UsageDays int; LegalHold bool }` (JSON tags `sandbox_metadata_days`, `logs_days`, `usage_days`, `legal_hold`).
- `DataRetentionStore` seam: `Get(ctx, orgID) (DataRetentionPolicy, error)`, `Set(ctx, orgID, DataRetentionPolicy) error`. `NewMemDataRetentionStore()` fake (per-org; defaults to a zero policy = keep forever). `Deps.DataRetention` nil-defaults to the fake.
- `GET /console/retention` -> the org's policy; `PUT /console/retention` body the policy.

- [ ] **Step 1: Write the failing test `internal/saas/console/retention_test.go`**

```go
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
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/console/ -run TestDataRetention`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/saas/console/retention.go`** (the policy + store + fake + `handleGetDataRetention`/`handleSetDataRetention`). Wire `Deps.DataRetention` nil-default + the 2 routes in `console.go`. Add both routes to the auth-gate table. A code comment notes the GC (#163) enforces the policy and that a legal hold pauses deletion.

- [ ] **Step 4: Run; full package; gofmt; both lint**

Run: `go test ./internal/saas/console/` ; `gofmt -l internal/saas/console/retention.go internal/saas/console/console.go`
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...`
Expected: green/clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/console/retention.go internal/saas/console/console.go internal/saas/console/retention_test.go
git commit -s -m "feat(console): per-org data-retention policy store and endpoint"
```

---

### Task 2: Data-and-retention view (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/nav/routes.tsx`
- Create: `web/app/src/data/retention.ts`, `web/app/src/views/Retention.tsx`
- Test: `web/app/src/views/Retention.test.tsx`

**Interfaces:**
- `DataRetentionPolicy` type (TS, snake_case to match Go); `api.dataRetention()`, `api.setDataRetention(policy)`.
- Hooks `useDataRetention`, `useSetDataRetention`.
- `Retention` view: a form with number inputs for sandbox-metadata, logs, usage retention (days; 0 = keep forever, labelled), a legal-hold toggle, a Save button, and a short honest "what gets deleted when" preview (e.g. "Terminated sandbox metadata older than N days is removed by the garbage collector. A legal hold pauses all deletion."). Route `/retention` in the Govern group, label "Data and retention".

- [ ] **Step 1: Write the failing test `web/app/src/views/Retention.test.tsx`** (render `/retention`, mock capabilities + retention `{sandbox_metadata_days:30, logs_days:14, usage_days:365, legal_hold:false}`; assert the inputs show the values and the legal-hold control renders; toggling legal hold and saving calls the PUT).

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/Retention.test.tsx`
Expected: FAIL.

- [ ] **Step 3: Implement api.ts additions, `data/retention.ts`, `Retention.tsx`, and route `/retention`.** Labelled inputs; a legal-hold checkbox; Save -> toast; the honest GC-enforcement note.

- [ ] **Step 4: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Retention.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/retention.ts web/app/src/views/Retention.tsx web/app/src/nav/routes.tsx web/app/src/views/Retention.test.tsx
git commit -s -m "feat(console): Data and retention view with per-resource policy and legal hold"
```

---

### Task 3: Styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/Retention.a11y.test.tsx`

- [ ] **Step 1: Append token-driven styles** for any new classes the view uses (reuse `.settings-section`/`.form-row`/input styles). No raw hex. Mobile rule.
- [ ] **Step 2: Write the axe a11y test** for `/retention` (inputs + legal-hold toggle labelled; zero violations). Fix any real violation.
- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0) ; `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `go test ./internal/saas/...` (green) ; both golangci-lint invocations clean
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views/Retention.tsx internal/saas/console/retention.go web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/Retention.a11y.test.tsx
git commit -s -m "feat(console): retention view styles and B3c accessibility checks"
```

---

## Self-Review

**Spec coverage (section 5.3):** per-org data-retention for terminated-sandbox metadata, logs, and usage (Task 1, 2), a legal-hold toggle (Task 1, 2), and the "Data and retention" view with a delete-when preview (Task 2). Covered. GC enforcement is #163's job (noted honestly; the console stores + exposes the policy).

**Org isolation:** the retention endpoint is per-org, context-sourced, isolation-tested (Task 1), auth-gate added.

**Type consistency:** Go `DataRetentionPolicy` JSON tags match the TS type (Task 2); the endpoint (Task 1) backs the hooks (Task 2). No drift.
