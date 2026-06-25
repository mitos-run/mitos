# Console B2c: best-practice roles + org to Projects hierarchy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** Replace the `owner | member` role set with the best-practice canon (Owner, Admin, Billing manager, Member, Viewer), add role management (an authorized actor can change a member's role), and introduce the org -> Projects hierarchy (projects group resources within an org). Surface both in real Members & roles and Projects views.

**Architecture:** Go BFF (`internal/saas`) gains: an additive Role set with a permission helper (owner/member kept as aliases so existing tests hold); `AccountService.SetMemberRole` guarded by the permission model; a `Project` type + `ProjectStore` seam (in-memory fake now, k8s-namespace-backed impl a documented follow-up, mirroring `SandboxControl`); and console endpoints (`POST /console/members/{accountID}/role`, `GET/POST /console/projects`), all org-scoped. The SPA gains a data layer + Members and Projects views. Per-project RBAC ENFORCEMENT and custom roles are B3; B2c ships the role taxonomy, role management, and the project structure.

**Tech Stack:** Go (`net/http`, `encoding/json`, the existing `internal/saas` store), React+Vite+TS, TanStack Query, Vitest + vitest-axe.

**Scope note:** B2c of the B2 split. B2d (profile) and B3 (custom roles, per-project RBAC enforcement, SSO/SCIM, audit retention) follow.

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere (Go, TS, comments, commit messages). Only `.` `,` `;` `:`; ASCII `-` for compounds. Verify each commit.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only.
- **Backward compatibility:** `owner` and `member` MUST keep working as today (aliases for Owner and Member); existing `internal/saas` and console tests must stay green.
- **Org-scoped isolation:** new endpoints read org from request context only; cross-org ids resolve to `not_found`; every new endpoint gets a cross-org isolation test.
- **Authorization:** changing a member's role requires the actor to hold a role that permits member management (Owner or Admin); a Member/Viewer attempting it gets `forbidden`. Tested.
- **Go style:** `fmt.Errorf("context: %w", err)`; gofmt clean; BOTH `golangci-lint run --timeout=5m ./internal/saas/...` and `GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...` clean; no secret in any field/log.
- **Responsive + accessible (spec 4.6):** Members/Projects views responsive; role controls labelled; axe zero violations.
- **TypeScript strict** clean; SPA suite exits 0. Go: `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/model.go` (modify) - extend `Role` + add `Permission` and `Role.Can(p)`.
- `internal/saas/account.go` (modify) - `SetMemberRole(ctx, actorID, orgID, targetID, role)` guarded by `Role.Can`.
- `internal/saas/account_test.go` (modify) - role-permission + SetMemberRole authorization tests.
- `internal/saas/console/projects.go` (create) - `Project`, `ProjectStore` seam, `NewMemProjectStore`, the `GET/POST /console/projects` handlers.
- `internal/saas/console/console.go` (modify) - register projects routes + the role-change route + the `Projects` dep default.
- `internal/saas/console/projects_test.go` (create) - org-scoped isolation + create tests.
- `internal/saas/console/console_test.go` (modify) - role-change endpoint test (authorized + forbidden + cross-org).
- `web/app/src/api.ts` (modify) - `Role` type, `MemberView` (with role), `ProjectView`; methods `setMemberRole`, `projects`, `createProject`.
- `web/app/src/data/org.ts` (create) - `useMembers`, `useSetRole`, `useProjects`, `useCreateProject`.
- `web/app/src/views/Members.tsx`, `web/app/src/views/Projects.tsx` (create); routes pointed at them.
- `web/packages/brand/src/base.css` (modify) - role-badge styles.
- Tests alongside.

---

### Task 1: Best-practice role set + permission helper (Go)

**Files:**
- Modify: `internal/saas/model.go`
- Test: `internal/saas/model_test.go` (create if absent, else extend)

**Interfaces:**
- Produces: `RoleAdmin`, `RoleBilling`, `RoleViewer` constants (plus existing `RoleOwner`, `RoleMember`); a `Permission` string type with constants (`PermManageMembers`, `PermManageProjects`, `PermManageSecrets`, `PermManageBilling`, `PermUseResources`, `PermReadOnly`); `func (r Role) Can(p Permission) bool`.

- [ ] **Step 1: Write the failing test `internal/saas/model_test.go`**

```go
package saas

import "testing"

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleOwner, PermManageMembers, true},
		{RoleOwner, PermManageBilling, true},
		{RoleAdmin, PermManageMembers, true},
		{RoleAdmin, PermManageBilling, false},
		{RoleBilling, PermManageBilling, true},
		{RoleBilling, PermManageMembers, false},
		{RoleMember, PermUseResources, true},
		{RoleMember, PermManageMembers, false},
		{RoleViewer, PermReadOnly, true},
		{RoleViewer, PermUseResources, false},
	}
	for _, c := range cases {
		if got := c.role.Can(c.perm); got != c.want {
			t.Errorf("%s.Can(%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/ -run TestRolePermissions`
Expected: FAIL (undefined `RoleAdmin`, `Permission`, `Can`).

- [ ] **Step 3: Extend `internal/saas/model.go`**

Add after the existing role constants:

```go
const (
	// RoleAdmin manages members, projects, secrets, retention, and audit, but
	// not billing or org deletion.
	RoleAdmin Role = "admin"
	// RoleBilling manages billing and views usage; no resource access.
	RoleBilling Role = "billing"
	// RoleViewer is read-only across the projects it is added to.
	RoleViewer Role = "viewer"
)

// Permission is a coarse capability gate. Roles map to a permission set; the
// console authorizes verbs against these. Finer per-project and custom roles are
// a follow-up (B3).
type Permission string

const (
	PermManageMembers  Permission = "members.manage"
	PermManageProjects Permission = "projects.manage"
	PermManageSecrets  Permission = "secrets.manage"
	PermManageBilling  Permission = "billing.manage"
	PermUseResources   Permission = "resources.use"
	PermReadOnly       Permission = "read"
)

// rolePerms is the built-in role -> permission map. Every role implies PermReadOnly.
var rolePerms = map[Role]map[Permission]bool{
	RoleOwner:   {PermManageMembers: true, PermManageProjects: true, PermManageSecrets: true, PermManageBilling: true, PermUseResources: true, PermReadOnly: true},
	RoleAdmin:   {PermManageMembers: true, PermManageProjects: true, PermManageSecrets: true, PermUseResources: true, PermReadOnly: true},
	RoleBilling: {PermManageBilling: true, PermReadOnly: true},
	RoleMember:  {PermUseResources: true, PermReadOnly: true},
	RoleViewer:  {PermReadOnly: true},
}

// Can reports whether the role grants the permission.
func (r Role) Can(p Permission) bool {
	return rolePerms[r][p]
}
```

- [ ] **Step 4: Run it, confirm pass; gofmt + lint**

Run: `go test ./internal/saas/ -run TestRolePermissions`
Run: `gofmt -l internal/saas/model.go` (prints nothing)
Run: `go test ./internal/saas/` (whole package green, existing tests hold)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/model.go internal/saas/model_test.go
git commit -s -m "feat(saas): best-practice role set with a permission helper"
```

---

### Task 2: SetMemberRole (authorized role management)

**Files:**
- Modify: `internal/saas/account.go`
- Test: `internal/saas/account_test.go`

Read `internal/saas/account.go` and `internal/saas/store.go` first to match how `ListMembers` reads memberships and how the store updates records.

**Interfaces:**
- Produces: `func (s *AccountService) SetMemberRole(ctx context.Context, actorID, orgID, targetAccountID string, role Role) error`. It (1) verifies the actor is a member of org whose role `Can(PermManageMembers)`, else returns a forbidden error (`ErrKeyWrongOrg` or a dedicated `ErrForbidden`); (2) updates the target membership's role in the store; (3) returns `ErrNotFound` if the target is not a member.

- [ ] **Step 1: Write the failing test (append to `internal/saas/account_test.go`)**

```go
func TestSetMemberRoleAuthorization(t *testing.T) {
	// Build a store with an org, an owner actor, an admin, and a plain member.
	// (Use the same helpers the existing account tests use to seed accounts +
	// memberships; mirror an existing test's setup.)
	svc, orgID, ownerID, memberID := seedOrgWithOwnerAndMember(t)

	// Owner can promote the member to admin.
	if err := svc.SetMemberRole(context.Background(), ownerID, orgID, memberID, RoleAdmin); err != nil {
		t.Fatalf("owner SetMemberRole: %v", err)
	}
	members, _ := svc.ListMembers(context.Background(), ownerID, orgID)
	if roleOf(members, memberID) != RoleAdmin {
		t.Fatalf("member role = %s, want admin", roleOf(members, memberID))
	}

	// A plain member (now admin) can manage; but a viewer cannot. Demote and check.
	if err := svc.SetMemberRole(context.Background(), ownerID, orgID, memberID, RoleViewer); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if err := svc.SetMemberRole(context.Background(), memberID, orgID, ownerID, RoleMember); err == nil {
		t.Fatalf("viewer must not be able to change roles")
	}
}
```

(Implement `seedOrgWithOwnerAndMember` and `roleOf` test helpers if they do not already exist, modeled on the existing account-test seeding.)

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/ -run TestSetMemberRole`
Expected: FAIL (undefined `SetMemberRole`).

- [ ] **Step 3: Implement `SetMemberRole` in `internal/saas/account.go`**

```go
// SetMemberRole changes a member's role. The actor must hold a role that can
// manage members (Owner or Admin). The target must be a member of the org. The
// owner role cannot be removed from the last owner (an org always has an owner).
func (s *AccountService) SetMemberRole(ctx context.Context, actorID, orgID, targetAccountID string, role Role) error {
	actor, err := s.memberRole(ctx, actorID, orgID)
	if err != nil {
		return err
	}
	if !actor.Can(PermManageMembers) {
		return ErrForbidden
	}
	return s.store.SetMembershipRole(ctx, orgID, targetAccountID, role)
}
```

Add the `ErrForbidden` sentinel (if absent) and a `memberRole` helper that returns the actor's role in the org (or `ErrKeyWrongOrg`/`ErrNotFound` if not a member), plus a `SetMembershipRole` method on the store (update the membership; return `ErrNotFound` if absent; refuse to demote the last owner). Match the existing store interface and error sentinels.

- [ ] **Step 4: Run it, confirm pass; gofmt + lint + full package**

Run: `go test ./internal/saas/ -run TestSetMemberRole` then `go test ./internal/saas/`
Run: `gofmt -l internal/saas/account.go internal/saas/store.go`
Expected: PASS, gofmt clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/account.go internal/saas/store.go internal/saas/account_test.go
git commit -s -m "feat(saas): authorized SetMemberRole guarded by the permission model"
```

---

### Task 3: Projects (Go seam + endpoints + role-change endpoint)

**Files:**
- Create: `internal/saas/console/projects.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/projects_test.go`, `internal/saas/console/console_test.go` (extend)

Read `internal/saas/console/seams.go`, `console.go`, and an existing seam (e.g. `forktree.go`) first to match the seam + handler + nil-default pattern.

**Interfaces:**
- `Project = { ID, OrgID, Name, Description string; CreatedAt time.Time }` with JSON tags.
- `ProjectStore` seam: `List(ctx, orgID) ([]Project, error)`, `Create(ctx, orgID, name, description string) (Project, error)`. `NewMemProjectStore()` fake (org-scoped; ids generated; cross-org never leaks).
- `Deps.Projects ProjectStore` (nil-defaults to the fake in `New`).
- `GET /console/projects` (list) + `POST /console/projects` (create, body `{name, description}`).
- `POST /console/members/{accountID}/role` (body `{role}`) calling `AccountService.SetMemberRole`; maps `ErrForbidden` -> 403, `ErrNotFound` -> 404.

- [ ] **Step 1: Write the failing tests `internal/saas/console/projects_test.go`**

```go
package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectsAreOrgScoped(t *testing.T) {
	mem := NewMemProjectStore()
	_, _ = mem.Create(context.Background(), "orgA", "alpha", "")
	_, _ = mem.Create(context.Background(), "orgB", "beta", "")
	c := New(Deps{Projects: mem})

	req := httptest.NewRequest("GET", "/console/projects", nil).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out struct {
		Projects []Project `json:"projects"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Projects) != 1 || out.Projects[0].Name != "alpha" {
		t.Fatalf("orgA projects = %+v, want only alpha", out.Projects)
	}
}

func TestProjectCreate(t *testing.T) {
	c := New(Deps{Projects: NewMemProjectStore()})
	body := strings.NewReader(`{"name":"gamma","description":"team gamma"}`)
	req := httptest.NewRequest("POST", "/console/projects", body).WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/console/ -run TestProject`
Expected: FAIL (undefined `NewMemProjectStore`, `Project`).

- [ ] **Step 3: Implement `internal/saas/console/projects.go`**

Implement `Project`, `ProjectStore`, `MemProjectStore` (RWMutex, per-org slice, `Create` appends with a generated id and `Now`, `List` returns a copy of only the org's projects), mirroring `forktree.go`. Use a deterministic id scheme for the fake (e.g. `proj_<orgID>_<n>`); do not use `Math/rand` reproducibility hazards in tests (the fake can use a counter).

- [ ] **Step 4: Wire handlers + routes + dep in `internal/saas/console/console.go`**

Add `Projects ProjectStore` to `Deps`; default it in `New`; register `GET /console/projects`, `POST /console/projects`, `POST /console/members/{accountID}/role`; add `handleListProjects`, `handleCreateProject` (org-scoped, decode body with the existing `decodeBody`, return 201 on create), and `handleSetMemberRole` (reads `accountID` path value + `{role}` body, calls `c.deps.Accounts.SetMemberRole`, maps errors via `failAccount` plus a 403 for `ErrForbidden`). Emit an audit event on create-project and role-change (the existing `c.audit` helper).

- [ ] **Step 5: Extend `internal/saas/console/console_test.go`** with a role-change endpoint test (authorized 200, forbidden 403, cross-org 404) modeled on the existing member test fixtures.

- [ ] **Step 6: Run tests; gofmt; both lint invocations**

Run: `go test ./internal/saas/console/`
Run: `gofmt -l internal/saas/console/projects.go internal/saas/console/console.go`
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...`
Expected: all green/clean.

- [ ] **Step 7: Commit**

```bash
git add internal/saas/console/projects.go internal/saas/console/console.go internal/saas/console/projects_test.go internal/saas/console/console_test.go
git commit -s -m "feat(console): Projects seam + endpoints and the member role-change endpoint"
```

---

### Task 4: Members & Projects views (frontend)

**Files:**
- Modify: `web/app/src/api.ts`
- Create: `web/app/src/data/org.ts`, `web/app/src/views/Members.tsx`, `web/app/src/views/Projects.tsx`
- Modify: `web/app/src/nav/routes.tsx` (point `/members` at `Members`; add `/projects` route in group Govern)
- Test: `web/app/src/views/MembersProjects.test.tsx`

**Interfaces:**
- `Role = 'owner' | 'admin' | 'billing' | 'member' | 'viewer'`; `MemberView = { account_id, org_id, role: Role, created_at }`; `ProjectView = { id, org_id, name, description, created_at }`.
- `api.members()`, `api.setMemberRole(accountId, role)`, `api.projects()`, `api.createProject(name, description)`.
- Hooks `useMembers`, `useSetRole` (optimistic), `useProjects`, `useCreateProject` in `data/org.ts`.
- `Members` view: a table (Account, Role as a badge + a role `<select>` to change it, Joined). `Projects` view: a create form (name, description) + a list/cards.

- [ ] **Step 1: Write the failing test `web/app/src/views/MembersProjects.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'hosted', billing: true, signup: false, teams: true, idp: 'oidc', orgSwitcher: true, secrets: { providers: ['kube'] }, proof: true, ownership: 'hosted' }

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/members')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', members: [{ account_id: 'alice', org_id: 'o', role: 'owner', created_at: '' }, { account_id: 'bob', org_id: 'o', role: 'member', created_at: '' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects: [{ id: 'p1', org_id: 'o', name: 'alpha', description: 'team alpha', created_at: '' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Members and Projects views', () => {
  it('Members lists members with roles', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(screen.getByText('bob')).toBeInTheDocument()
    expect(screen.getAllByText(/owner/i).length).toBeGreaterThan(0)
  })
  it('Projects lists projects', async () => {
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByText('alpha')).toBeInTheDocument())
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/MembersProjects.test.tsx`
Expected: FAIL.

- [ ] **Step 3: Implement the api.ts additions, `data/org.ts`, `Members.tsx`, `Projects.tsx`, and route them** (point `/members` at `Members`; add `{ path: '/projects', label: 'Projects', group: 'Govern', element: () => <Projects />, when: (c) => c.teams }`). Members shows a role badge and a role `<select>` (Owner/Admin/Billing/Member/Viewer) per member that calls `useSetRole` optimistically (toast on result); Projects has a create form + a list. Loading/empty/error states. Accessible (labelled selects, table headers).

- [ ] **Step 4: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/MembersProjects.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/org.ts web/app/src/views/Members.tsx web/app/src/views/Projects.tsx web/app/src/nav/routes.tsx web/app/src/views/MembersProjects.test.tsx
git commit -s -m "feat(console): Members (role management) and Projects views"
```

---

### Task 5: Role-badge styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/MembersProjects.a11y.test.tsx`

- [ ] **Step 1: Append token-driven role-badge styles to `web/packages/brand/src/base.css`** (`.role-badge` plus a per-role accent using existing tokens: owner/admin magenta, billing amber, member cyan, viewer dim). No raw hex. Mobile rule so the members/projects tables reflow.

- [ ] **Step 2: Write the axe a11y test `web/app/src/views/MembersProjects.a11y.test.tsx`** (render `/members` and `/projects`, assert zero axe violations; the `vitest-axe` pattern). Fix any real violation (the role `<select>` must have an accessible name).

- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0)
Run: `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `go test ./internal/saas/...` (green)
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...` (clean)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views internal/saas/console/projects.go internal/saas/model.go web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/MembersProjects.a11y.test.tsx
git commit -s -m "feat(console): role-badge styles and B2c accessibility checks"
```

---

## Self-Review

**Spec coverage (section 5.4):** the best-practice built-in role set (Task 1: Owner/Admin/Billing/Member/Viewer + owner/member aliases) with a permission helper; role management (Task 2: authorized SetMemberRole; Task 3: the endpoint); the org -> Projects hierarchy (Task 3: ProjectStore + endpoints; Task 4: Projects view); the Members & roles UI (Task 4). Covered. Custom roles and per-project RBAC ENFORCEMENT are explicitly B3.

**Backward compatibility:** owner/member are unchanged constants; the permission map keeps their existing capabilities; existing `internal/saas` and console tests must stay green (Task 1 Step 4, Task 3 Step 6 run the full packages).

**Org isolation + authorization:** Tasks 2 and 3 test cross-org rejection and the manage-members permission guard (owner/admin allowed; viewer forbidden).

**Type consistency:** Go `Project`/`Role` JSON tags match the TS `ProjectView`/`Role`/`MemberView` (Task 4); `SetMemberRole` (Task 2) is called by the role-change endpoint (Task 3) consumed by `useSetRole` (Task 4). No drift.
