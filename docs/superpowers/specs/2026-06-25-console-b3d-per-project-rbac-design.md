# Console B3d: custom roles and per-project RBAC enforcement: design

Status: draft for review. Date: 2026-06-25.

## Why

Section 5.4 of the console enterprise design specifies two RBAC capabilities beyond
the built-in roles already shipped (owner/admin/billing/member/viewer plus a coarse
`rolePerms` map): (1) org-defined **custom roles** mapping a name to a permission set,
and (2) **per-project RBAC**, where a membership grants a role scoped to a project
(Admin of project A, Viewer of project B). The user scoped B3d as FULL per-project
enforcement: resources carry a project, and access is gated by the caller's role in
that project.

This is the authorization surface of the product, so it is decomposed into slices that
each preserve the existing invariants and can be reviewed independently. The
enforcement-critical slices carry a threat-model delta and require a named-human
security review before merge (per CLAUDE.md operating principle 2).

## Invariants (must hold after every slice)

1. **Org isolation is absolute and unchanged.** Org is sourced from request context only
   (`c.caller`), never from a body/path/query. A session for org A never sees org B.
   Project scoping is ADDITIVE strictly WITHIN an org; it never widens cross-org access.
2. **Deny by default.** A caller with no role in a project, and no org-wide role granting
   the permission, is denied. Owner/Admin are org-wide and implicitly cover all projects.
3. **No fabricated capability.** The UI never shows a permission the BFF does not enforce.
4. **Backward compatible.** Existing `owner`/`member` and the built-in roles keep working;
   custom roles and project scoping are additive.

## Model

- **Permission** (existing, extend as needed): a coarse verb gate (`members.manage`,
  `projects.manage`, `secrets.manage`, `resources.use`, `billing.manage`, `read`).
- **Role** (existing): built-ins map to permission sets via `rolePerms`. `Role.Can(p)`.
- **CustomRole** (new): `{ OrgID, Name, Permissions []Permission }`, stored per-org. A
  membership may reference a built-in role name or a custom role name; permission checks
  resolve either to a permission set.
- **Membership** (existing `{ AccountID, OrgID, Role }`) gains an optional `ProjectID`.
  A membership with empty `ProjectID` is org-wide (today's behavior). A membership with a
  `ProjectID` grants its role only within that project. An account may hold one org-wide
  membership plus zero or more project-scoped memberships.
- **Resource project tag** (new): a sandbox, secret, template, or workspace carries a
  `ProjectID` (possibly empty = unassigned/org-default). The BFF seams expose it.

Authorization decision for "caller may do permission P on resource R in project Pr":
grant if the caller's org-wide role Can(P), OR the caller has a (custom or built-in) role
in project Pr whose permission set includes P. Else deny.

## Slices

Each slice: TDD, org-isolation tested, no em/en dashes, both golangci-lint invocations
clean, web suite + typecheck + build green, screenshots for UI.

### B3d-1: custom roles (store + enforce + UI). LOWER RISK.

- BFF `CustomRoleStore` seam (per-org): list, upsert, delete a named role with a
  permission set. `NewMemCustomRoleStore` fake. Endpoints `GET/POST/DELETE
  /console/roles`. Org-scoped, in the auth-gate table, isolation-tested.
- Resolution: `permissionsFor(orgID, roleName)` returns the built-in set if the name is a
  built-in, else the custom role's set; unknown name resolves to the empty set (deny).
- Enforce: the console authz that already consults `Role.Can` also consults custom roles
  for the caller's role name. (Where the console gates a verb today, route it through the
  resolver so a custom role is honored.)
- UI: a "Roles" section in the Members view (or its own view) with a permission MATRIX
  (rows = permissions, columns/toggles per role) to create/edit a custom role. The matrix
  only lists permissions the BFF actually enforces.
- Threat-model: note the new endpoints; no cross-org or privilege-escalation path (a
  custom role can never exceed the org's own permission vocabulary; only Owner/Admin may
  manage roles).

### B3d-2: project membership (store + endpoints + UI). LOWER RISK.

- Extend membership with `ProjectID`; a `ProjectMembershipStore` (or extend the account
  service) to assign/list/revoke a role for an account within a project. Endpoints under
  `/console/projects/{id}/members`. Org-scoped; only members of the org; project belongs
  to the org; isolation-tested.
- UI: in the Projects view, manage a project's members and their per-project roles; in the
  Members view, show a member's per-project roles.
- No resource enforcement yet (resources are not project-tagged until B3d-3); the view
  states plainly that project roles take effect once resources are assigned to projects.

### B3d-3: resource project tagging. ENFORCEMENT-ADJACENT (review).

- Add `ProjectID` to the BFF resource shapes (sandbox, secret, template, workspace) and the
  seams, with a default/unassigned value. Where resources are created, allow assigning a
  project; a "move to project" action lists/relabels.
- For the Kubernetes path this is a label on the resource (honest k8s semantics: a label,
  not a pod-scoped mechanism); for sandbox-server it is a field. Controller/CRD change is
  the heaviest part and is its own PR.
- No behavior change to access yet (tagging only); but it is the data enforcement will read,
  so it gets review.

### B3d-4: per-project enforcement. SECURITY-CRITICAL (named-human review + threat-model delta).

- Gate resource reads/mutations by the authorization decision above: the caller must have
  the permission via an org-wide role OR a role in the resource's project. List endpoints
  filter to the projects the caller can see; mutations are denied without the project
  permission.
- Exhaustive isolation + escalation tests: a Viewer of project A cannot mutate project B;
  an account with only a project-A role cannot see project-B resources; org isolation still
  holds across all of it.
- Threat-model delta in the same PR. This slice does NOT auto-merge; it requires a named
  human security review.

## Sequencing and review gates

`B3d-1 -> B3d-2 -> B3d-3 -> B3d-4`. B3d-1 and B3d-2 are additive surfaces that can land with
standard review. B3d-3 and especially B3d-4 change what data governs access and the access
decision itself; they require a threat-model delta and a named-human security review before
merge, and must ship after the fork-correctness and failure/GC gates are green (already are).

## Out of scope

- SSO/SAML (B3e) and SCIM (B3f); they are separate auth-critical phases.
- Pricing/tiering; the open-core line keeps every RBAC capability in OSS.
