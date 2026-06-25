# Console B3d-4: per-project access enforcement (sandboxes) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Enforce per-project access on sandboxes: list filters to the sandboxes the caller may see, and inspect/terminate are denied for sandboxes in a project the caller has no role in. This is the security-critical payoff of B3d (custom roles + project membership + resource tags now gate real access).

**Architecture:** A single access-decision helper `canAccessSandbox(ctx, accountID, orgID, projectID, perm)` composes B3d-1 (the permission resolver), B3d-2 (project memberships), and B3d-3 (the sandbox project tag). The three sandbox handlers route their access decision through it. The decision is additive-then-restrictive: org-wide managers see all; unassigned resources keep org-wide semantics; an assigned resource is restricted to managers plus that project's members.

**Tech Stack:** Go (`internal/saas/console`). No UI behavior change (the list is server-filtered); a short honest note is added to the sandbox list.

## The access decision (exact, deny by default)

For a caller C in org O acting on a sandbox S (with `S.project_id = Pr`) requiring permission P:

1. Resolve C's ORG-WIDE permissions `orgPerms = permissionsFor(C, O)` (built-in role via rolePerms, or a custom role; B3d-1).
2. If `orgPerms[projects.manage]` is true (Owner/Admin): ALLOW. Managers of all projects see and act on every sandbox.
3. Else if `Pr == ""` (unassigned): ALLOW iff `orgPerms[P]`. Unassigned resources keep today's org-wide behavior.
4. Else (assigned to Pr): resolve C's PROJECT permissions in Pr `projPerms = projectPermissionsFor(C, O, Pr)` (the role of C's ProjectMembership in Pr, resolved built-in or custom; empty if C has no membership in Pr). ALLOW iff `projPerms[P]`. An org-wide Member/Viewer with no role in Pr is therefore DENIED an assigned sandbox.

`projectPermissionsFor(C, O, Pr)`: list `ProjectMembers.List(O, Pr)`, find C's entry; if none, return an empty set (deny); else resolve that role's permissions exactly like `permissionsFor` does (built-in via `Role.Can` over `knownPermissions`, else custom via `CustomRoles`).

## Global Constraints

- **Security invariants:** org from request context only; the decision never grants cross-org access (org-wide and project lookups all use the context org). Deny by default: unknown/missing project role denies; an inspect of an inaccessible sandbox returns 404 (do not leak existence); a terminate returns 403. Owner/Admin retain full access. This is a behavior change: assigning a sandbox to a project restricts it to managers plus that project's members; document it.
- **Punctuation (strict):** no em/en dashes. **Go:** gofmt + both golangci-lint clean; `fmt.Errorf("...: %w", err)`. **Commits:** conventional + DCO; explicit-path staging.
- **Threat-model:** the access surface moves; update `docs/threat-model.md` (Task 2).
- **TDD:** failing test first. `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/console/projectaccess.go` (create) - `canAccessSandbox` + `projectPermissionsFor`.
- `internal/saas/console/console.go` (modify) - route list/inspect/terminate through the decision.
- `internal/saas/console/projectaccess_test.go` (create) - the exhaustive access matrix.
- `docs/threat-model.md` (modify).
- `web/app/src/views/sandboxes/SandboxList.tsx` (modify) - a one-line honest note that the list shows sandboxes you can access.

---

### Task 1: access decision + enforcement (Go)

**Files:**
- Create: `internal/saas/console/projectaccess.go`, `internal/saas/console/projectaccess_test.go`
- Modify: `internal/saas/console/console.go`

Read `authz.go` (`permissionsFor`, `knownPermissions`, `builtinRoles`), `projectmembers.go` (`ProjectMembership`, `ProjectMembershipStore.List`), `customroles.go` (`CustomRoleStore.Get`), `console.go` (handleListSandboxes ~494, handleInspectSandbox ~517, handleTerminateSandbox ~535, and how each currently authorizes).

**Interfaces:**
- `projectPermissionsFor(ctx, accountID, orgID, projectID string) (map[saas.Permission]bool, error)`: find C's `ProjectMembership` in `(orgID, projectID)` via `c.deps.ProjectMembers.List`; no membership -> empty map; else resolve the role's permission set (built-in: iterate `knownPermissions` with `role.Can`; custom: `c.deps.CustomRoles.Get` then its permission slice; unknown -> empty).
- `canAccessSandbox(ctx, accountID, orgID, projectID string, perm saas.Permission) (bool, error)`: implement the 4-step decision above using `permissionsFor` and `projectPermissionsFor`.

Enforcement edits in `console.go`:
- `handleListSandboxes`: after building the boxes with their `project_id` (B3d-3), filter to those where `canAccessSandbox(ctx, accountID, orgID, box.ProjectID, saas.PermReadOnly)` is true. (You now need the accountID from the caller; it is already available from `c.caller`.)
- `handleInspectSandbox`: after fetching + populating project_id, if `!canAccessSandbox(... PermReadOnly)` return 404 (`apierr.CodeNotFound`, cause "the sandbox does not exist or is not accessible"). Do not leak existence.
- `handleTerminateSandbox`: REPLACE its current top-level `authorize(resources.use)` gate with: `accountID, orgID, e, ok := c.caller(r)`; fetch the sandbox (to get its project_id); then if `!canAccessSandbox(... saas.PermUseResources)` return 403 (`apierr.CodeForbidden`, cause "the caller's role does not grant access to this sandbox"). This composes org-wide AND project access (a project Admin with org-wide Viewer can terminate in their project; an org-wide Member cannot terminate an assigned sandbox in a project they are not in).

- [ ] **Step 1: Write the exhaustive failing tests** (`projectaccess_test.go`). Seed: org O; accounts OWNER, ADMIN (org-wide admin), MEMBER (org-wide member), VIEWER (org-wide viewer), and PVIEWER (org-wide viewer who is ALSO a project Admin of project P via a ProjectMembership). Two sandboxes: SU (unassigned) and SP (assigned to project P). Assert:
  - GET /console/sandboxes as ADMIN returns both SU and SP.
  - GET as MEMBER returns SU but NOT SP (assigned, member not in P).
  - GET as PVIEWER returns SP (project Admin) and SU (org-wide viewer reads unassigned).
  - GET /console/sandboxes/SP as MEMBER -> 404; as ADMIN -> 200; as PVIEWER -> 200.
  - DELETE /console/sandboxes/SP as MEMBER -> 403; as PVIEWER -> 200 (project Admin has resources.use in P); as ADMIN -> 200.
  - DELETE /console/sandboxes/SU as MEMBER -> 200 (org-wide resources.use, unassigned).
  - Org isolation: a second org cannot see or terminate O's sandboxes (404/403).
  Build the console with a real AccountService + MemProjectStore (seed P) + MemSandboxControl (seed SU, SP) + MemProjectMembershipStore (seed PVIEWER as Admin of P) + MemResourceProjectStore (tag SP -> P). Use WithCaller per request.

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/saas/console/ -run 'ProjectAccess|Enforce'` Expected: FAIL.

- [ ] **Step 3: Implement** projectaccess.go and wire the three handlers.

- [ ] **Step 4: Run the full package; gofmt; both lint.** Fix any pre-existing sandbox test whose caller now lacks access (give it a sufficient role, or assign the box appropriately). Report each adjustment.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/projectaccess.go internal/saas/console/projectaccess_test.go internal/saas/console/console.go
git commit -s -m "feat(console): enforce per-project access on sandboxes"
```

---

### Task 2: threat-model delta, UI note, final verification

**Files:**
- Modify: `docs/threat-model.md`, `web/app/src/views/sandboxes/SandboxList.tsx`

- [ ] **Step 1: Threat-model delta.** Read the file; match its format. Add rows: per-project access enforcement on sandboxes (deny by default; assigned sandboxes restricted to org managers plus project members; inspect of an inaccessible sandbox is 404 to avoid leaking existence; terminate is 403); the decision is org-context-scoped so it never widens cross-org access; the behavior change (assignment restricts). No em/en dashes.
- [ ] **Step 2: UI note.** Add a short honest line under the sandbox list PageHeader, e.g. "You see the sandboxes you can access." (only meaningful copy; no dead control). Keep it token-styled.
- [ ] **Step 3: Final verification.** `go test ./internal/saas/...` (green) ; both golangci-lint clean ; `pnpm -C web/app test` (0) ; `typecheck` ; `build` ; dash grep empty over changed files.
- [ ] **Step 4: Commit.**

```bash
git add docs/threat-model.md web/app/src/views/sandboxes/SandboxList.tsx
git commit -s -m "feat(console): B3d-4 threat-model delta and sandbox access note"
```

---

## Self-Review

**Spec coverage (B3d-4):** per-project access enforcement on a real resource (sandboxes): list filtered, inspect/terminate gated, composing custom roles + project membership + resource tags. Exhaustive access-matrix tests (Task 1). Threat-model delta (Task 2). Covered. Secrets/templates enforcement follows the same helper in a later slice.

**Security invariants:** deny by default (no project role -> empty perms -> deny); org from context (org-wide and project lookups both use the context org; no cross-org widening); inspect 404 to avoid existence leak; Owner/Admin retain full access; the behavior change (assignment restricts) is documented.

**Composition correctness:** `canAccessSandbox` uses `permissionsFor` (B3d-1) for the org-wide path and `projectPermissionsFor` (new, over B3d-2 memberships + B3d-1 resolver) for the project path; the sandbox project tag (B3d-3) selects which path. No drift.
