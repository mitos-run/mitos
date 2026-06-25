# Console B3d-1: per-verb permission enforcement and custom roles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Enforce the existing permission model on the console's mutating endpoints (a deliberate access tightening the user confirmed), make that enforcement consult org-defined custom roles, and surface a custom-role permission matrix in the UI.

**Architecture:** A console authz resolver maps the caller's role name (from `AccountService`) to a permission set, consulting built-in `rolePerms` plus a per-org `CustomRoleStore`. A `c.authorize(r, perm)` helper gates each mutating handler. Custom-role CRUD endpoints let Owner/Admin define named permission sets. The UI gains a permission-matrix editor. Org isolation is unchanged (org from context only); project scoping is NOT in this slice (it is B3d-2+).

**Tech Stack:** Go (`internal/saas`, `internal/saas/console`), React+Vite+TS, Vitest + vitest-axe.

## Global Constraints

- **Security invariants:** org is sourced from request context only (`c.caller`); never from body/path/query. Custom roles can only grant permissions from the org's own vocabulary; a custom role can never exceed built-in Owner. Deny by default: an unknown role name or a missing permission denies. Only callers with `settings.manage` (Owner/Admin) may manage custom roles.
- **Permission mapping (explicit; verify each handler):** secrets create/delete -> `secrets.manage`; projects create -> `projects.manage`; data-retention PUT + audit retention/sinks -> `settings.manage` (NEW permission); keys create/revoke + sandbox terminate -> `resources.use`; member role change -> `members.manage` (already enforced in `AccountService.SetMemberRole`, leave as is). Own-account session revoke endpoints are NOT gated (self-scoped).
- **rolePerms after this slice:** Owner gets every permission incl `settings.manage`; Admin gets members/projects/secrets/settings/resources/read; Billing gets billing/read; Member gets resources/read; Viewer gets read.
- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere.
- **Go style:** `fmt.Errorf("...: %w", err)`; gofmt clean; BOTH golangci-lint invocations clean. Secret values never logged.
- **Commits:** conventional + DCO (`git commit -s`). Staging: explicit paths only.
- **Threat-model:** the security surface moves; update `docs/threat-model.md` in this branch (Task 6).
- **TDD:** failing test first. TypeScript strict; web suite + `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/model.go` (modify) - add `PermManageSettings`; add it to Owner/Admin in `rolePerms`.
- `internal/saas/account.go` (modify) - expose `MemberRole(ctx, accountID, orgID) (Role, error)` (public wrapper over the existing private `memberRole`).
- `internal/saas/console/customroles.go` (create) - `CustomRole`, `CustomRoleStore` seam + `MemCustomRoleStore` fake, CRUD handlers.
- `internal/saas/console/authz.go` (create) - `permissionsFor` resolver + `authorize` helper.
- `internal/saas/console/console.go` (modify) - `Deps.CustomRoles` default; wire authz into mutating handlers; register role routes.
- Go tests alongside each.
- `web/app/src/api.ts`, `web/app/src/data/roles.ts` (create), `web/app/src/views/Roles.tsx` (create) or extend Members; `web/app/src/nav/routes.tsx` (route). Tests alongside.
- `web/packages/brand/src/base.css` (modify) - matrix styles.
- `docs/threat-model.md` (modify).

---

### Task 1: permission vocabulary + caller role accessor (Go, saas)

**Files:**
- Modify: `internal/saas/model.go`, `internal/saas/account.go`
- Test: `internal/saas/model_test.go` (or existing), `internal/saas/account_test.go`

**Interfaces:**
- Produces: `PermManageSettings Permission = "settings.manage"`; `rolePerms[RoleOwner]` and `rolePerms[RoleAdmin]` include it. `(*AccountService).MemberRole(ctx, accountID, orgID) (Role, error)` returns the caller's role in the org (wraps the existing private `memberRole`); a non-member returns `ErrForbidden` or `ErrNotFound` consistent with the existing private method.

- [ ] **Step 1: Write failing tests.**

```go
func TestSettingsPermissionOnAdminNotMember(t *testing.T) {
	if !RoleAdmin.Can(PermManageSettings) {
		t.Fatal("admin must have settings.manage")
	}
	if RoleMember.Can(PermManageSettings) {
		t.Fatal("member must NOT have settings.manage")
	}
	if RoleViewer.Can(PermManageSettings) {
		t.Fatal("viewer must NOT have settings.manage")
	}
}
```

For `MemberRole`, add a test in `account_test.go` using the existing in-memory store pattern: a known owner membership returns `RoleOwner`; a non-member returns a non-nil error.

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/saas/ -run 'TestSettingsPermission|MemberRole'` Expected: FAIL.

- [ ] **Step 3: Implement.** Add `PermManageSettings` and extend `rolePerms` (Owner, Admin). Add the public `MemberRole` wrapper:

```go
// MemberRole returns the account's role in the org. It is the read accessor the
// console authz layer uses to resolve a caller's permissions.
func (s *AccountService) MemberRole(ctx context.Context, accountID, orgID string) (Role, error) {
	return s.memberRole(ctx, accountID, orgID)
}
```

- [ ] **Step 4: Run; gofmt; both lint.** `go test ./internal/saas/...` ; `gofmt -l internal/saas/model.go internal/saas/account.go` ; both `golangci-lint run` invocations on `./internal/saas/...`. Expected: green/clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/model.go internal/saas/account.go internal/saas/model_test.go internal/saas/account_test.go
git commit -s -m "feat(saas): add settings.manage permission and public MemberRole accessor"
```

---

### Task 2: custom-role store + permission resolver + authorize helper (Go, console)

**Files:**
- Create: `internal/saas/console/customroles.go`, `internal/saas/console/authz.go`
- Modify: `internal/saas/console/console.go` (Deps + default)
- Test: `internal/saas/console/authz_test.go`

Read `console.go` (Deps, New, `caller`, `apierr` usage), `retention.go` / `projects.go` (the seam + nil-default pattern) first.

**Interfaces:**
- `CustomRole = { Name string; Permissions []saas.Permission }` (JSON: `name`, `permissions`).
- `CustomRoleStore`: `List(ctx, orgID) ([]CustomRole, error)`, `Get(ctx, orgID, name) (CustomRole, bool, error)`, `Upsert(ctx, orgID, CustomRole) error`, `Delete(ctx, orgID, name) error`. `NewMemCustomRoleStore()` fake, per-org, never cross-org. `Deps.CustomRoles` nil-defaults to the fake.
- `permissionsFor(ctx, accountID, orgID) (map[saas.Permission]bool, error)`: resolve the caller's role via `c.deps.Accounts.MemberRole`; if it is a built-in role, return its `rolePerms` set; else look it up in the `CustomRoleStore` and return that permission set; unknown name returns an empty (deny) set, not an error.
- `authorize(r, perm) (accountID, orgID string, e apierr.Error, ok bool)`: calls `caller(r)`; on success resolves `permissionsFor`; if the permission is absent returns a 403 apierr (`CodeForbidden`, cause "the caller's role does not grant <perm>") with ok=false.

- [ ] **Step 1: Write failing tests** in `authz_test.go`: a built-in Admin resolves to a set containing `secrets.manage` but not `billing.manage`; a custom role "auditor" with `{read}` resolves to read-only and is denied `secrets.manage`; a caller whose org has a custom role does not see another org's custom roles (store isolation).

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/saas/console/ -run 'TestAuthz|CustomRole'` Expected: FAIL.

- [ ] **Step 3: Implement** `customroles.go` (type + store + fake) and `authz.go` (resolver + helper). Wire `Deps.CustomRoles` nil-default in `console.go`.

- [ ] **Step 4: Run; gofmt; both lint.** Expected: green/clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/customroles.go internal/saas/console/authz.go internal/saas/console/console.go internal/saas/console/authz_test.go
git commit -s -m "feat(console): permission resolver, custom-role store, and authorize helper"
```

---

### Task 3: enforce permissions on the mutating endpoints (Go, console)

**Files:**
- Modify: `internal/saas/console/console.go`, `secrets.go`, `audit_export.go`/`audit_sinks.go`, `retention.go`, `projects.go` (whichever own the handlers)
- Test: `internal/saas/console/authz_enforce_test.go`

**Interfaces:**
- Consumes: `c.authorize(r, perm)` from Task 2.
- Each mutating handler begins with `accountID, orgID, e, ok := c.authorize(r, PERM); if !ok { apierr.Encode(w, e); return }` using the mapping in Global Constraints. The own-account session revoke handlers keep using `c.caller` (NOT gated).

- [ ] **Step 1: Write failing tests** in `authz_enforce_test.go`: with a member caller (role=member), `POST /console/secrets` returns 403; `POST /console/projects` returns 403; `PUT /console/retention` returns 403; `POST /console/keys` returns 200 (resources.use); with an admin caller all return non-403. With a viewer caller, `DELETE /console/sandboxes/{id}` returns 403. Use the in-memory Accounts with seeded memberships and `WithCaller` like the existing console tests. Assert org isolation is unaffected (the existing `TestEveryEndpointRefusesMissingOrgContext` still passes).

- [ ] **Step 2: Run, confirm fail.** Expected: FAIL (handlers not yet gated).

- [ ] **Step 3: Implement** the `c.authorize` gate at the top of each mutating handler per the mapping. Keep the audit emission and the rest unchanged.

- [ ] **Step 4: Run full console package; gofmt; both lint.** `go test ./internal/saas/console/` Expected: green. Fix any existing test that assumed an ungated mutation by giving its caller a sufficient role.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/console.go internal/saas/console/secrets.go internal/saas/console/retention.go internal/saas/console/projects.go internal/saas/console/audit_sinks.go internal/saas/console/authz_enforce_test.go
git commit -s -m "feat(console): enforce role permissions on mutating endpoints"
```

---

### Task 4: custom-role CRUD endpoints (Go, console)

**Files:**
- Modify: `internal/saas/console/console.go` (routes), `internal/saas/console/customroles.go` (handlers)
- Test: `internal/saas/console/customroles_test.go`

**Interfaces:**
- `GET /console/roles` -> `{ org_id, builtins: [{name, permissions}], custom: [CustomRole] }` (lists built-in role permission sets + custom roles so the UI can render the matrix). Gated by `read`.
- `POST /console/roles` body `CustomRole` -> upsert; gated by `settings.manage`. Reject a name colliding with a built-in role (owner/admin/billing/member/viewer) with 400. Reject permissions outside the known vocabulary with 400.
- `DELETE /console/roles/{name}` -> delete; gated by `settings.manage`.
- All three in the auth-gate table; org-scoped; isolation-tested.

- [ ] **Step 1: Write failing tests** in `customroles_test.go`: an admin upserts a custom role and reads it back; a member is 403 on POST and DELETE; a custom role named "admin" is rejected 400; orgB does not see orgA's custom role.

- [ ] **Step 2: Run, confirm fail.** Expected: FAIL.

- [ ] **Step 3: Implement** the three handlers (using `c.authorize`) and register the routes; add them to the auth-gate table.

- [ ] **Step 4: Run; gofmt; both lint.** Expected: green/clean.

- [ ] **Step 5: Commit.**

```bash
git add internal/saas/console/customroles.go internal/saas/console/console.go internal/saas/console/customroles_test.go internal/saas/console/console_test.go
git commit -s -m "feat(console): custom-role CRUD endpoints gated by settings.manage"
```

---

### Task 5: roles UI (permission matrix) (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/nav/routes.tsx`
- Create: `web/app/src/data/roles.ts`, `web/app/src/views/Roles.tsx`, `web/app/src/views/Roles.test.tsx`
- Modify: `web/packages/brand/src/base.css`

**Interfaces:**
- `Permission` string-union type matching the Go vocabulary; `CustomRole` TS type; `api.roles()`, `api.upsertRole(role)`, `api.deleteRole(name)`. Hooks `useRoles`, `useUpsertRole`, `useDeleteRole`.
- `Roles` view in the Govern group, route `/roles`, label "Roles": a permission MATRIX (rows = permissions with plain-language labels, columns = roles) showing the built-in roles read-only and letting Owner/Admin add/edit a custom role via toggles, then Save. Honest: the matrix lists only the permissions the BFF enforces (the ones from Task 3).

- [ ] **Step 1: Write the failing test** (`Roles.test.tsx`): render `/roles`, mock capabilities + roles; assert the built-in roles render with their permissions and a new-custom-role form toggles a permission and Save calls the POST.

- [ ] **Step 2: Run, confirm fail.** `pnpm -C web/app test src/views/Roles.test.tsx` Expected: FAIL.

- [ ] **Step 3: Implement** the api additions, hooks, view, and route.

- [ ] **Step 4: Run the test, full suite, typecheck.** Expected: PASS, clean.

- [ ] **Step 5: Commit.**

```bash
git add web/app/src/api.ts web/app/src/data/roles.ts web/app/src/views/Roles.tsx web/app/src/views/Roles.test.tsx web/app/src/nav/routes.tsx
git commit -s -m "feat(console): roles view with a permission matrix and custom roles"
```

---

### Task 6: threat-model delta, styles, a11y, final verification

**Files:**
- Modify: `docs/threat-model.md`, `web/packages/brand/src/base.css`
- Create: `web/app/src/views/Roles.a11y.test.tsx`

- [ ] **Step 1: Threat-model delta.** Add rows for the new authz surface: per-verb permission enforcement on the console (deny by default), custom-role management restricted to `settings.manage`, the invariant that custom roles cannot exceed the org vocabulary, and that org isolation is unchanged. No em/en dashes.
- [ ] **Step 2: Append matrix styles** (token-driven, no raw hex; responsive). 
- [ ] **Step 3: Axe a11y test** for `/roles` (matrix is a real table with labelled toggles; zero violations).
- [ ] **Step 4: Final verification.** `pnpm -C web/app test` (0) ; `typecheck` ; `build` ; `go test ./internal/saas/...` (green) ; both golangci-lint invocations clean ; dash grep empty over changed files.
- [ ] **Step 5: Commit.**

```bash
git add docs/threat-model.md web/packages/brand/src/base.css web/app/src/views/Roles.a11y.test.tsx
git commit -s -m "feat(console): B3d-1 threat-model delta, roles styles, accessibility checks"
```

---

## Self-Review

**Spec coverage (B3d-1 in the design):** custom roles store + resolver + enforcement (Tasks 2, 3), per-verb gating tightening (Task 3), custom-role CRUD (Task 4), permission-matrix UI (Task 5), threat-model delta (Task 6). Covered. Per-project scoping is explicitly NOT here (B3d-2+).

**Security invariants:** org from context only (unchanged); deny by default (resolver returns empty set for unknown roles; handlers 403 without the permission); custom-role management gated by settings.manage; custom roles cannot exceed the org vocabulary (Task 4 validates permissions); isolation tested at the store and endpoint level.

**Behavior change is intentional and reviewed:** Viewers lose all writes; Members lose secret/project/settings management but keep resources.use + keys. This is the tightening the user confirmed; the explicit mapping is in Global Constraints for reviewer scrutiny.

**Type consistency:** Go `Permission` strings match the TS union (Task 5); `CustomRole` JSON tags match; the resolver (Task 2) backs both enforcement (Task 3) and CRUD (Task 4).
