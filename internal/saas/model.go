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
	// HomeRegion is the org's data-residency anchor (issue #712 phase 0): the
	// placement.Registry value name stamped at org creation time, immutable
	// afterward. Empty means "the deployment's registry default", so old rows
	// and community installs that predate this field keep working without a
	// backfill. It is read-only after creation; a region move is a future,
	// explicit copy operation, never an in-place update.
	HomeRegion string
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

// canGrantRole reports whether actorRole may grant targetRole to someone
// else, whether by inviting them (InvitationService.CreateInvite) or by
// changing an existing member's role (AccountService.SetMemberRole). Only an
// owner may grant the owner role; any other built-in role (admin, billing,
// member, viewer) may be granted by anyone who already holds
// PermManageMembers. This is the single ceiling both call sites enforce, so
// an admin can never mint a new owner through either path. It does not
// itself check whether actorRole holds PermManageMembers at all; callers
// already gate that separately before reaching this check. It is defined
// only in terms of the built-in Role values: a custom role (see
// console.CustomRole) grants a permission set, never the literal "owner"
// role string, so it never trips this ceiling.
func canGrantRole(actorRole, targetRole Role) bool {
	if targetRole == RoleOwner {
		return actorRole == RoleOwner
	}
	return true
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
// Scopes gate what the key may do. An EMPTY scope set means FULL access (every
// scope): this is the backward-compatibility rule for keys minted before scopes
// existed, or persisted without an explicit scope set, so an existing caller is
// never locked out by the scope model (issue #784). A newly minted key defaults
// to the full scope set unless scopes are named at mint. ExpiresAt of the zero
// time means the key never expires. RevokedAt of the zero time means the key is
// live; a non-zero RevokedAt permanently disables it.
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

// HasScope reports whether the key satisfies the named required scope, applying
// the scope-implication graph (scopeSatisfies). A key with NO scopes recorded
// FAILS CLOSED: it satisfies nothing. There are no genuine pre-scopes records to
// preserve (the api_keys.scopes column is NOT NULL DEFAULT '{}' from the first
// migration, and scope enforcement shipped in the same commit that introduced
// api keys), so an empty scope set is not a legacy full-access key; it is a key
// that grants nothing, exactly as the gateway already treats it. Defaulting an
// empty set to full access would silently turn every inert empty-scope key
// (which the console form can mint) into an admin credential. A key satisfies a
// requirement only when a scope it explicitly holds satisfies it; the reverse of
// the implication never holds (a read-only key never satisfies a mutating
// requirement).
func (k ApiKey) HasScope(required string) bool {
	for _, held := range k.Scopes {
		if scopeSatisfies(held, required) {
			return true
		}
	}
	return false
}

// Known scope constants (issue #784). A key is minted with one or more of these
// to shrink its blast radius below the whole org. ScopeSandboxes is the legacy
// full-lifecycle scope retained for backward compatibility with keys minted
// before the finer split.
const (
	// ScopeReadOnly permits read-only surfaces: list, get, status.
	ScopeReadOnly = "read"
	// ScopeExecute permits acting INSIDE an existing sandbox: exec, files,
	// run_code (the runtime proxy). It does not permit creating or destroying
	// sandboxes.
	ScopeExecute = "execute"
	// ScopeLifecycle permits creating, forking, and terminating sandboxes (and
	// the pause/resume state verbs). It does not by itself permit in-sandbox
	// execution.
	ScopeLifecycle = "lifecycle"
	// ScopeAdmin permits organization management surfaces (API keys, billing).
	// It is orthogonal to the resource scopes: an admin key does not itself gain
	// sandbox read, execute, or lifecycle access. There is no API-key-reachable
	// admin operation on the public gateway yet (key and billing management run
	// through the session-authenticated console), so this scope is minted,
	// persisted, and displayed today and gates the gateway admin surface when it
	// lands.
	ScopeAdmin = "admin"
	// ScopeSandboxes is the LEGACY full-lifecycle scope (the pre-#784 onboarding
	// default). It is retained so every existing key keeps working: it satisfies
	// read, execute, and lifecycle (but not admin).
	ScopeSandboxes = "sandboxes"
)

// scopeImplied lists, for a held scope, the required scopes it additionally
// satisfies beyond an exact match. read is the floor every resource scope grants
// so a key that can act on a sandbox can always list and status it (no dead
// end). execute and lifecycle are orthogonal to each other. admin implies
// nothing and is implied by nothing. The legacy sandboxes scope satisfies the
// whole resource surface.
var scopeImplied = map[string][]string{
	ScopeSandboxes: {ScopeReadOnly, ScopeExecute, ScopeLifecycle},
	ScopeLifecycle: {ScopeReadOnly},
	ScopeExecute:   {ScopeReadOnly},
}

// scopeSatisfies reports whether holding held satisfies a requirement for
// required, by exact match or through the scopeImplied graph.
func scopeSatisfies(held, required string) bool {
	if held == required {
		return true
	}
	for _, imp := range scopeImplied[held] {
		if imp == required {
			return true
		}
	}
	return false
}

// knownScopes is the closed vocabulary CreateKey validates a mint request
// against, so a typo'd scope is rejected rather than silently minting a key that
// grants nothing. The legacy ScopeSandboxes is accepted (existing tooling still
// mints it).
var knownScopes = map[string]bool{
	ScopeReadOnly:  true,
	ScopeExecute:   true,
	ScopeLifecycle: true,
	ScopeAdmin:     true,
	ScopeSandboxes: true,
}

// FullScopes returns the full scope set, the union of the resource scopes plus
// admin. It is the closed vocabulary a named scope is validated against and the
// admin-inclusive set an operator must ASK for explicitly; it is NOT the mint
// default (see DefaultScopes). The legacy ScopeSandboxes is deliberately
// omitted: new keys carry the finer scopes.
func FullScopes() []string {
	return []string{ScopeReadOnly, ScopeExecute, ScopeLifecycle, ScopeAdmin}
}

// DefaultScopes returns the scope set a key is minted with when the request
// names none. It is the resource set (read, execute, lifecycle) and DELIBERATELY
// EXCLUDES admin: minting is a routine action any org member may perform, so the
// default must not hand out a management credential. It matches the CLI's own
// `--scopes` default; admin is granted only when explicitly requested. A caller
// who wants a management key asks for `admin` by name.
func DefaultScopes() []string {
	return []string{ScopeReadOnly, ScopeExecute, ScopeLifecycle}
}
