# Console B3d-3: resource project tagging (sandboxes) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Let a sandbox be assigned to a project, so per-project access can be enforced in B3d-4. A console-level resource-to-project mapping store, a `project_id` on the sandbox view (populated on list and inspect), and a `PUT /console/sandboxes/{id}/project` endpoint, surfaced in the sandbox list and detail UI.

**Architecture:** A console `ResourceProjectStore` maps `(orgID, resourceType, resourceID) -> projectID`. The sandbox list/inspect handlers join each `SandboxView` with its stored project. A gated PUT endpoint sets the mapping (validating the project belongs to the org, reusing the B3d-2 `validateProjectInOrg` helper). This is bounded to sandboxes (the primary forkable resource); secrets/templates can follow the same pattern. The deeper controller/CRD label propagation is a separate follow-up; this slice keeps the assignment in the console mapping, which B3d-4 reads for enforcement. The UI states assignment plainly.

**Tech Stack:** Go (`internal/saas/console`), React+Vite+TS, Vitest + vitest-axe.

## Global Constraints

- **Security invariants (unchanged):** org from request context only; the resource-project mapping is per-org and never cross-org; setting a sandbox's project requires `projects.manage`; the target project must belong to the caller's org (reuse `validateProjectInOrg`); an empty `project_id` unassigns. Deny by default.
- **Honesty:** the UI shows the assigned project (or "Unassigned") and states that per-project access enforcement applies once enabled (B3d-4); it does not claim enforcement that is not yet active.
- **Punctuation (strict):** no em/en dashes. **Go:** gofmt + both golangci-lint clean; `fmt.Errorf("...: %w", err)`. **Commits:** conventional + DCO; explicit-path staging.
- **TDD:** failing test first. TypeScript strict; web suite + `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/console/resourceprojects.go` (create) - `ResourceProjectStore` seam + `MemResourceProjectStore` fake, the set-project handler.
- `internal/saas/console/seams.go` (modify) - add `ProjectID string json:"project_id"` to `SandboxView`.
- `internal/saas/console/console.go` (modify) - populate `ProjectID` in list/inspect; `Deps.ResourceProjects` default; route; auth-gate entry.
- Go tests alongside.
- `web/app/src/api.ts` (modify - SandboxView gains project_id; `api.setSandboxProject`), `web/app/src/data/sandboxes.ts` (modify - hook), `web/app/src/views/sandboxes/SandboxList.tsx` + `SandboxDetail.tsx` (modify - show + set project), tests.

---

### Task 1: resource-project store + sandbox tagging endpoint (Go)

**Files:**
- Create: `internal/saas/console/resourceprojects.go`
- Modify: `internal/saas/console/seams.go`, `internal/saas/console/console.go`
- Test: `internal/saas/console/resourceprojects_test.go`

Read `projectmembers.go` (the `validateProjectInOrg` helper and the seam/fake/nil-default pattern), `authz.go` (`c.authorize`), `console.go` (handleListSandboxes at ~484, handleInspectSandbox, routes, auth-gate table), `seams.go` (SandboxView).

**Interfaces:**
- `ResourceProjectStore`: `Project(ctx, orgID, resourceType, resourceID string) (string, error)` (returns "" if unassigned), `SetProject(ctx, orgID, resourceType, resourceID, projectID string) error`. `NewMemResourceProjectStore()` fake (per-org keyed). `Deps.ResourceProjects` nil-defaults.
- `SandboxView` gains `ProjectID string json:"project_id"`.
- In `handleListSandboxes` and `handleInspectSandbox`, after fetching, set each box's `ProjectID` from `c.deps.ResourceProjects.Project(ctx, orgID, "sandbox", box.ID)`.
- `PUT /console/sandboxes/{id}/project` body `{ project_id string }` -> gated `projects.manage`; if `project_id` is non-empty, validate it belongs to the org (`validateProjectInOrg`); then `SetProject(ctx, orgID, "sandbox", id, project_id)`. Empty `project_id` unassigns (skip the project validation when empty).

- [ ] **Step 1: Write failing tests** (`resourceprojects_test.go`): an admin PUTs `{project_id:P}` (P seeded in the org) for sandbox S, then GET /console/sandboxes shows S with `project_id == P`; a member (no projects.manage) gets 403 on PUT; PUT with a project id not in the org returns 404; PUT with empty project_id unassigns (subsequent list shows ""). Use New(Deps{Accounts, Projects (seeded), Sandboxes (a MemSandboxControl with a seeded box), ResourceProjects}) + WithCaller + seeded admin/member memberships.

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/saas/console/ -run 'ResourceProject|SandboxProject'` Expected: FAIL.

- [ ] **Step 3: Implement** resourceprojects.go (store + fake + handler), the SandboxView field, the list/inspect population, the Deps default + route + auth-gate entry.

- [ ] **Step 4: Run; gofmt; both lint.** Expected: green/clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/resourceprojects.go internal/saas/console/seams.go internal/saas/console/console.go internal/saas/console/resourceprojects_test.go internal/saas/console/console_test.go
git commit -s -m "feat(console): resource-to-project mapping and sandbox project tagging"
```

---

### Task 2: sandbox project UI (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/data/sandboxes.ts`, `web/app/src/views/sandboxes/SandboxList.tsx`, `web/app/src/views/sandboxes/SandboxDetail.tsx`
- Test: extend `web/app/src/views/sandboxes/SandboxList.test.tsx` (or SandboxDetail.test.tsx)

**Interfaces:**
- `SandboxView` TS type gains `project_id?: string`. `api.setSandboxProject(id, projectId)` (PUT). Hook `useSetSandboxProject()` (invalidate ['sandboxes'] and the detail).
- SandboxList: add a "Project" column showing the project id or "Unassigned".
- SandboxDetail: a "Project" control - a select of the org's projects (from useProjects) plus an "Unassigned" option - that calls useSetSandboxProject on change, with a short honest note that per-project access enforcement applies when enabled.

- [ ] **Step 1: Write the failing test** (extend a sandboxes test): render the list with a sandbox carrying `project_id:'p1'`; assert the Project column shows it; render the detail and assert changing the project select calls the PUT.

- [ ] **Step 2: Run, confirm fail.** Expected: FAIL.

- [ ] **Step 3: Implement** the api/type/hook additions and the list column + detail control.

- [ ] **Step 4: Run the test, full suite, typecheck.** Expected: PASS, clean.

- [ ] **Step 5: Commit.**

```bash
git add web/app/src/api.ts web/app/src/data/sandboxes.ts web/app/src/views/sandboxes/SandboxList.tsx web/app/src/views/sandboxes/SandboxDetail.tsx web/app/src/views/sandboxes/SandboxList.test.tsx
git commit -s -m "feat(console): show and assign a sandbox's project"
```

---

### Task 3: a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css` (only if needed)
- Create/extend: a sandbox a11y test if the detail control needs it

- [ ] **Step 1: a11y.** Ensure the project select in SandboxDetail is labelled; extend or add an axe test covering it; zero violations.
- [ ] **Step 2: Styles** for any new classes (token-driven, responsive); reuse existing where possible.
- [ ] **Step 3: Final verification.** `pnpm -C web/app test` (0) ; `typecheck` ; `build` ; `go test ./internal/saas/...` (green) ; both golangci-lint clean ; dash grep empty over changed files.
- [ ] **Step 4: Commit.**

```bash
git add web/packages/brand/src/base.css web/app/src/views/sandboxes/
git commit -s -m "feat(console): sandbox project control styles and accessibility"
```

---

## Self-Review

**Spec coverage (B3d-3):** resource-to-project mapping + sandbox `project_id` (Task 1), set-project endpoint gated by projects.manage with org-validated project (Task 1), UI to show + assign (Task 2), honest framing that enforcement applies in B3d-4 (Task 2). Covered. Secrets/templates tagging and controller-label propagation are deliberate follow-ups.

**Security invariants:** org from context; mapping per-org; project validated to belong to org; manage gated; isolation tested (Task 1).

**Type consistency:** Go `SandboxView.project_id` matches the TS type; the PUT endpoint backs the hook. No drift.
