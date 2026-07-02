// Package saas is the customer-facing front door for the hosted offering: real
// external accounts, organizations, memberships, and scoped API keys, layered
// ABOVE the internal mTLS and per-sandbox token plane (issue #210). It does NOT
// replace the internal principal model (internal/admission/claim_principal.go);
// it sits in front of it. A customer presents a prefix-tagged API key to the
// public gateway; the gateway resolves the owning organization, attaches an org
// context, enforces quota, and forwards to the control plane.
//
// Everything a customer owns is ORG-scoped: sandboxes, usage, and quota all hang
// off an Organization, never off a bare user. A user belongs to one or more
// organizations and gets a Personal organization by default (the Daytona-style
// default), so a brand-new user can act immediately without creating a team.
//
// Security: an API key's raw value is NEVER stored. Only a salted hash is kept
// at rest, and only a masked value is ever shown after creation. Verification is
// constant-time. The store, the control-plane forward target, and the quota
// enforcer are all pluggable interfaces so this slice is fully unit-tested
// without a database, a live control plane, or a billing backend. Postgres, the
// real hosted deployment, browser OAuth, and the control-plane wiring are
// documented follow-ups (docs/saas/accounts-gateway.md); the in-memory store is
// the tested default.
package saas

import "time"

// Account is a person who can authenticate to the hosted offering. It is the
// human identity; ownership and billing attach to organizations, not accounts.
// The personal organization id is the org created with the account so a new
// user can act without first creating a team.
type Account struct {
	ID        string
	Email     string
	CreatedAt time.Time
	// PersonalOrgID is the id of the org created alongside the account.
	PersonalOrgID string
	// DisplayName is the human-readable name shown in the console. Optional.
	DisplayName string
	// Timezone is the IANA timezone string preferred by the account holder (for
	// example "Europe/Berlin"). Optional; empty means the console falls back to
	// browser-local time.
	Timezone string
	// Locale is the BCP 47 language tag for the account holder (for example
	// "en-GB"). Optional; empty means the console falls back to the browser
	// locale.
	Locale string
}

// Organization is the unit of ownership, billing, and quota. Every sandbox,
// every usage record, and every quota decision is scoped to exactly one
// organization. An organization with Personal=true is the auto-created default
// org for a single user and cannot be deleted out from under that user.
type Organization struct {
	ID        string
	Name      string
	CreatedAt time.Time
	// Personal marks the auto-created default org for a single account.
	Personal bool
}

// Role is a membership role within an organization. The role set is intentionally
// small for this slice; richer RBAC is a follow-up.
type Role string

const (
	// RoleOwner can manage the org, its members, and its keys.
	RoleOwner Role = "owner"
	// RoleMember can use the org's resources but not manage membership.
	RoleMember Role = "member"
)

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
	// PermManageSettings gates org-level settings (name, defaults, integrations).
	// Granted to Owner and Admin only.
	PermManageSettings Permission = "settings.manage"
)

// rolePerms is the built-in role -> permission map. Every role implies PermReadOnly.
var rolePerms = map[Role]map[Permission]bool{
	RoleOwner:   {PermManageMembers: true, PermManageProjects: true, PermManageSecrets: true, PermManageBilling: true, PermUseResources: true, PermReadOnly: true, PermManageSettings: true},
	RoleAdmin:   {PermManageMembers: true, PermManageProjects: true, PermManageSecrets: true, PermUseResources: true, PermReadOnly: true, PermManageSettings: true},
	RoleBilling: {PermManageBilling: true, PermReadOnly: true},
	RoleMember:  {PermUseResources: true, PermReadOnly: true},
	RoleViewer:  {PermReadOnly: true},
}

// Can reports whether the role grants the permission.
func (r Role) Can(p Permission) bool {
	return rolePerms[r][p]
}

// Membership records that an account belongs to an organization with a role. A
// single account may hold many memberships (one per org it belongs to).
type Membership struct {
	AccountID string
	OrgID     string
	Role      Role
	CreatedAt time.Time
}

// ApiKey is a scoped, org-bound credential a customer presents to the public
// gateway. The raw key is shown EXACTLY ONCE at creation and is never persisted;
// only Hash (a salted hash of the raw key) is stored. Prefix is the masked,
// safe-to-display leading segment (for example mitos_live_ab12), kept for
// listing and audit without ever revealing the secret.
//
// Scopes gate what the key may do; an empty scope set means no permissions.
// ExpiresAt of the zero time means the key never expires. RevokedAt of the zero
// time means the key is live; a non-zero RevokedAt permanently disables it.
type ApiKey struct {
	ID        string
	OrgID     string
	Name      string
	Prefix    string
	Hash      string
	Scopes    []string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// IsExpired reports whether the key has an expiry and it is at or before now.
func (k ApiKey) IsExpired(now time.Time) bool {
	if k.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(k.ExpiresAt)
}

// IsRevoked reports whether the key has been revoked.
func (k ApiKey) IsRevoked() bool {
	return !k.RevokedAt.IsZero()
}

// HasScope reports whether the key carries the named scope. The full
// lifecycle scope implies read-only access: a key that may create and
// terminate sandboxes may also list them; without the implication the default
// onboarding key (sandboxes only) is locked out of the gateway's read-only
// ops (#599). The reverse does not hold: a read-only key never satisfies a
// mutating requirement.
func (k ApiKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
		if scope == ScopeReadOnly && s == ScopeSandboxes {
			return true
		}
	}
	return false
}

// Known scope constants. Scopes are coarse for this slice: one for the sandbox
// lifecycle surface and one for read-only access. Finer scopes are a follow-up.
const (
	// ScopeSandboxes permits the sandbox lifecycle surface (create, exec, fork,
	// terminate) through the gateway.
	ScopeSandboxes = "sandboxes"
	// ScopeReadOnly permits read-only surfaces (list, status) only.
	ScopeReadOnly = "read"
)
