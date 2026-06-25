# Console B3d-2: per-project membership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Let Owner/Admin assign a role to an account scoped to a single project (Admin of project A, Viewer of project B), with a project-membership store, endpoints under `/console/projects/{id}/members`, and a project-detail UI to manage them. Resource enforcement by project role is the NEXT slice (B3d-3 tags resources, B3d-4 enforces); this slice stores and surfaces the assignments and says so honestly.

**Architecture:** A console-level `ProjectMembership { AccountID, ProjectID, Role }` + a per-org `ProjectMembershipStore` seam. Endpoints are gated by the B3d-1 `authorize` helper (manage -> projects.manage, read -> read) and validate that the project belongs to the caller's org by listing the org's projects. A project-detail view (`/projects/$id`) lists members and assigns/revokes roles.

**Tech Stack:** Go (`internal/saas/console`), React+Vite+TS, TanStack Router/Query, Vitest + vitest-axe.

## Global Constraints

- **Security invariants (unchanged from B3d-1):** org from request context only; project membership is per-org and never cross-org; managing project membership requires `projects.manage` (Owner/Admin); a project id is only valid if it belongs to the caller's org (validated by listing the org's projects). Deny by default.
- **Honesty:** the UI states plainly that per-project roles take effect on resources once resources are assigned to projects (B3d-3+); it must not claim enforcement that does not exist yet.
- **Punctuation (strict):** no em/en dashes. **Go:** gofmt + both golangci-lint invocations clean; `fmt.Errorf("...: %w", err)`. **Commits:** conventional + DCO; explicit-path staging.
- **TDD:** failing test first. TypeScript strict; web suite + `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/console/projectmembers.go` (create) - `ProjectMembership`, `ProjectMembershipStore` seam + `MemProjectMembershipStore` fake, the list/assign/revoke handlers.
- `internal/saas/console/console.go` (modify) - `Deps.ProjectMembers` default; routes; auth-gate entries.
- Go tests alongside.
- `web/app/src/api.ts`, `web/app/src/data/projectmembers.ts` (create), `web/app/src/views/projects/ProjectDetail.tsx` (create), `web/app/src/views/Projects.tsx` (link rows to the detail), `web/app/src/nav/routes.tsx` (route). Tests alongside.
- `web/packages/brand/src/base.css` (modify) if needed.

---

### Task 1: project-membership store + endpoints (Go)

**Files:**
- Create: `internal/saas/console/projectmembers.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/projectmembers_test.go`

Read `projects.go` (the ProjectStore seam + MemProjectStore), `customroles.go` / `retention.go` (seam + fake + nil-default pattern), `authz.go` (`c.authorize(r, perm)`), `console.go` (routes, the auth-gate table in console_test.go, `caller`, decodeBody, writeJSON, apierr).

**Interfaces:**
- `ProjectMembership = { AccountID string; ProjectID string; Role saas.Role }` (JSON `account_id`, `project_id`, `role`).
- `ProjectMembershipStore`: `List(ctx, orgID, projectID) ([]ProjectMembership, error)`, `Assign(ctx, orgID, projectID, accountID string, role saas.Role) error`, `Revoke(ctx, orgID, projectID, accountID string) error`. `NewMemProjectMembershipStore()` fake (keyed by org then project; never cross-org). `Deps.ProjectMembers` nil-defaults to the fake.
- `GET /console/projects/{id}/members` -> `{ project_id, members: [ProjectMembership] }`, gated `read`.
- `POST /console/projects/{id}/members` body `{ account_id, role }` -> assign; gated `projects.manage`.
- `DELETE /console/projects/{id}/members/{accountID}` -> revoke; gated `projects.manage`.
- All three: resolve `{id}` via `r.PathValue("id")`, validate it belongs to the caller's org (list `c.deps.Projects.List(ctx, orgID)` and confirm the id is present; else 404). Org from context only.

- [ ] **Step 1: Write failing tests** (`projectmembers_test.go`): an admin assigns account X role viewer in project P and lists it back; a member (no projects.manage) gets 403 on POST and DELETE but 200 on GET; assigning to a project id that is not in the caller's org returns 404; orgB cannot list orgA's project members (isolation). Use the in-memory Accounts + ProjectStore + WithCaller like authz_enforce_test.go / customroles_test.go. Seed a project via the ProjectStore first.

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/saas/console/ -run 'ProjectMember'` Expected: FAIL.

- [ ] **Step 3: Implement** projectmembers.go (type + store + fake + 3 handlers using `c.authorize`), wire `Deps.ProjectMembers` nil-default + the 3 routes in console.go, and add the 3 routes to the auth-gate table in console_test.go.

- [ ] **Step 4: Run; gofmt; both lint.** `go test ./internal/saas/console/` ; both golangci-lint invocations. Expected: green/clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/projectmembers.go internal/saas/console/console.go internal/saas/console/projectmembers_test.go internal/saas/console/console_test.go
git commit -s -m "feat(console): per-project membership store and endpoints"
```

---

### Task 2: project-detail UI with member management (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/views/Projects.tsx`, `web/app/src/nav/routes.tsx`
- Create: `web/app/src/data/projectmembers.ts`, `web/app/src/views/projects/ProjectDetail.tsx`, `web/app/src/views/projects/ProjectDetail.test.tsx`

**Interfaces:**
- `ProjectMembership` TS type (snake_case to match Go); `api.projectMembers(projectId)`, `api.assignProjectMember(projectId, accountId, role)`, `api.revokeProjectMember(projectId, accountId)`. Hooks `useProjectMembers(projectId)`, `useAssignProjectMember(projectId)`, `useRevokeProjectMember(projectId)` (invalidate `['project-members', projectId]`).
- `ProjectDetail` view: route `/projects/$id` (hidden from nav; reached by clicking a project). `<PageHeader>` with the project name; a members table (account + role + revoke) and an assign form (account id input + role select with the built-in roles + a Save). An honest note: "Per-project roles take effect on resources once resources are assigned to projects." Use the existing role-select / table patterns from Members.tsx.
- `Projects.tsx`: make each project row link to `/projects/$id` (a router Link on the name).

- [ ] **Step 1: Write the failing test** (`ProjectDetail.test.tsx`): render `/projects/p1`, mock capabilities + project members `[{account_id:'a@x', project_id:'p1', role:'viewer'}]`; assert the member renders, and assigning a new member (type account + pick role + Save) calls the POST.

- [ ] **Step 2: Run, confirm fail.** `pnpm -C web/app test src/views/projects/ProjectDetail.test.tsx` Expected: FAIL.

- [ ] **Step 3: Implement** the api additions, hooks, ProjectDetail view + route, and the Projects row link.

- [ ] **Step 4: Run the test, full suite, typecheck.** Expected: PASS, clean.

- [ ] **Step 5: Commit.**

```bash
git add web/app/src/api.ts web/app/src/data/projectmembers.ts web/app/src/views/projects/ProjectDetail.tsx web/app/src/views/projects/ProjectDetail.test.tsx web/app/src/views/Projects.tsx web/app/src/nav/routes.tsx
git commit -s -m "feat(console): project detail view with per-project member management"
```

---

### Task 3: a11y, styles, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css` (only if new classes are needed)
- Create: `web/app/src/views/projects/ProjectDetail.a11y.test.tsx`

- [ ] **Step 1: Axe a11y test** for `/projects/$id` (members table + assign form labelled; zero violations). Fix any real violation.
- [ ] **Step 2: Styles** for any new classes (token-driven, responsive); reuse `.tbl`, `.page-header`, form classes where possible.
- [ ] **Step 3: Final verification.** `pnpm -C web/app test` (0) ; `typecheck` ; `build` ; `go test ./internal/saas/...` (green) ; both golangci-lint clean ; dash grep empty over changed files.
- [ ] **Step 4: Commit.**

```bash
git add web/packages/brand/src/base.css web/app/src/views/projects/ProjectDetail.a11y.test.tsx
git commit -s -m "feat(console): project detail styles and accessibility checks"
```

---

## Self-Review

**Spec coverage (B3d-2):** project-membership store + endpoints (Task 1), project-detail UI to assign/list/revoke per-project roles (Task 2), honest framing that enforcement on resources is a later slice (Task 2). Covered.

**Security invariants:** org from context; project validated to belong to the org; manage gated by projects.manage; isolation tested (Task 1).

**Type consistency:** Go `ProjectMembership` JSON tags match the TS type (Task 2); the endpoints (Task 1) back the hooks (Task 2). No drift.
