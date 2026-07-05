// Package console is the backend-for-frontend (BFF) for the hosted web console
// (issue #214): an org-scoped HTTP/JSON API that aggregates the accounts/keys
// (#210), usage and cost (#211), billing (#212), quota (#213), live sandboxes,
// and templates services into the views the console UI renders. The UI layer (a
// thin SPA) is a documented follow-up; this slice ships the tested BFF the UI
// consumes. See docs/saas/console.md.
//
// The load-bearing property is ORG-SCOPED DATA ISOLATION: every endpoint reads
// the caller's org from the request context (attached by the #210 gateway /
// session auth, never from a query parameter, path, or body), and returns ONLY
// that org's data. A session or key for org A can never observe org B's keys,
// usage, billing, sandboxes, members, audit log, or templates through any
// console endpoint. Every endpoint has a cross-org isolation test.
//
// Security: a key's raw VALUE is never logged or returned except the one-time
// raw key on create; everywhere else only the masked prefix is shown. The audit
// log, the live-sandbox control, and the template listing are pluggable seams so
// the whole BFF is unit-tested without a live cluster, a browser, or a database.
package console

import (
	"context"
	"time"
)

// orgContextKey is the private context key the console reads the caller's org
// from. The org is attached by the gateway / session auth AFTER it verifies the
// caller and resolves the org; the console NEVER reads the org from a query
// parameter, path, or body, so a request can only ever see its own org's data.
type orgContextKey struct{}

// callerContextKey carries the resolved account id of the caller, used to
// enforce membership on the account-service-backed verbs (keys, members) so the
// console honors the same membership guard the CLI verbs do.
type callerContextKey struct{}

// WithCaller returns a context carrying the calling account id and org id. The
// gateway / session middleware calls this after it authenticates the caller; the
// console handlers read them with CallerFromContext and OrgFromContext.
func WithCaller(ctx context.Context, accountID, orgID string) context.Context {
	ctx = context.WithValue(ctx, callerContextKey{}, accountID)
	ctx = context.WithValue(ctx, orgContextKey{}, orgID)
	return ctx
}

// OrgFromContext returns the org id attached by the auth layer and whether one
// was present. A request with no org context is unauthenticated and is refused;
// it is never served as a default org.
func OrgFromContext(ctx context.Context) (string, bool) {
	org, ok := ctx.Value(orgContextKey{}).(string)
	return org, ok && org != ""
}

// CallerFromContext returns the calling account id attached by the auth layer
// and whether one was present.
func CallerFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(callerContextKey{}).(string)
	return id, ok && id != ""
}

// SandboxView is the console's shape of one running sandbox for an org. It is a
// view, not the control-plane record: the BFF shapes the columns the live-
// sandbox inspector needs and the real cluster query fills them. It carries no
// secret.
type SandboxView struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Template  string    `json:"template"`
	Node      string    `json:"node"`
	Phase     string    `json:"phase"`
	VCPUs     int32     `json:"vcpus"`
	MemBytes  int64     `json:"mem_bytes"`
	CreatedAt time.Time `json:"created_at"`
	// ProjectID is the project this sandbox is assigned to. Empty string means
	// unassigned. It is populated by the resource-project store in the list and
	// inspect handlers; it is never stored in the SandboxControl itself.
	ProjectID string `json:"project_id"`
	// Region is the placement value (issue #712 phase 0) this sandbox's tree
	// root was created in, read back from the tenant.RegionLabelKey label.
	// Empty means the deployment's registry default (either the sandbox
	// predates this field, or the request never named a region). A fork
	// always carries its parent's Region verbatim: a live CoW fork cannot
	// cross clusters, so region is a property of the tree, not of each
	// sandbox individually.
	Region string `json:"region"`
}

// CreateSandboxRequest is the input to SandboxControl.Create: the template
// (pool) to provision the sandbox from, plus the requested vCPU/memory sizing.
// The console handler validates VCPUs/MemGiB against its static bounds (issue
// #322) before calling Create; a real adapter may not be able to enforce the
// requested sizing itself (see clustersandbox.Control.Create's own doc for why)
// but the seam still carries the request so an adapter that CAN enforce it
// (the in-memory fake, and any future control-plane surface) has the value.
// ProjectID is intentionally NOT here: project assignment is a separate,
// separately-permissioned write (ResourceProjectStore.SetProject), performed by
// the handler after Create succeeds, not by the seam itself.
type CreateSandboxRequest struct {
	Template string
	VCPUs    int32
	MemGiB   int32
	// Region is the placement value (issue #712 phase 0) requested for this
	// sandbox's tree root. Empty means "the org's home region" (which itself
	// falls back to the deployment's registry default): the console handler
	// leaves it empty rather than resolving it, so an adapter that never
	// implements multi-cluster placement (every Phase 0 adapter) can ignore
	// it entirely. When non-empty the console handler has ALREADY validated
	// it against the deployment's placement.Registry (Registry.Valid) before
	// Create is called; an adapter does not need to re-validate it.
	Region string
}

// ExecResult is the outcome of SandboxControl.Exec: exactly the shape the
// console's POST .../exec endpoint returns to the SPA. It carries no secret:
// Stdout/Stderr are the command's own output, never an env var or credential.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// SandboxControl is the live-sandbox seam the console inspects, creates, forks,
// execs in, and terminates running sandboxes through. The REAL implementation
// queries the control plane (the controller's claim/sandbox records) scoped to
// one org; this slice ships an injectable interface and an in-memory fake so
// the BFF shapes the view and enforces org scoping NOW, and the cluster query
// is a documented follow-up.
//
// Every method takes an orgID and the implementation MUST scope its effect to
// that org: List returns only the org's sandboxes; Get, Terminate, Fork, and
// Exec refuse a sandbox that does not belong to the org (returning
// ErrNotFound), so the BFF's org-scoping is enforced even if a caller learns
// another org's sandbox id. A method whose real backend genuinely does not
// exist on this deployment yet (no fabricated success) returns ErrUnsupported,
// which the console maps to HTTP 501.
type SandboxControl interface {
	// List returns the org's running sandboxes.
	List(ctx context.Context, orgID string) ([]SandboxView, error)
	// Get returns one of the org's sandboxes by id, or ErrNotFound if it does not
	// exist OR belongs to a different org (the two are indistinguishable to the
	// caller, the cross-org isolation backstop).
	Get(ctx context.Context, orgID, sandboxID string) (SandboxView, error)
	// Terminate terminates one of the org's sandboxes. It returns ErrNotFound if
	// the sandbox does not exist or belongs to a different org.
	Terminate(ctx context.Context, orgID, sandboxID string) error
	// Create provisions a new sandbox for org from req.Template and returns its
	// view. The caller (the console handler) has already validated req against
	// the static vcpu/mem bounds; Create itself does not re-derive them.
	Create(ctx context.Context, orgID string, req CreateSandboxRequest) (SandboxView, error)
	// Fork creates count new sandboxes, each forked from sandboxID, and returns
	// their ids in creation order. Returns ErrNotFound if sandboxID does not
	// exist or belongs to a different org; count has already been bounded
	// (1..16) by the caller.
	Fork(ctx context.Context, orgID, sandboxID string, count int) ([]string, error)
	// Exec runs cmd inside the org's sandbox, bounded by timeoutSec, and
	// returns its result. The console handler substitutes a 30 second default
	// (defaultExecTimeoutSec in sandbox_ops.go) before this seam is ever
	// reached, so an implementation never sees timeoutSec == 0 from the
	// console path. Returns ErrNotFound if sandboxID does not exist or belongs
	// to a different org.
	Exec(ctx context.Context, orgID, sandboxID, cmd string, timeoutSec int) (ExecResult, error)
}

// LogStreamer is the documented seam for live sandbox log streaming. The console
// streams a sandbox's logs over the SAME SDK exec/log transport the rest of the
// platform uses (forkd :9091 -> vsock -> guest agent); the BFF only authorizes
// the stream (the sandbox must belong to the caller's org) and then proxies the
// transport. The real wiring is a documented follow-up (docs/saas/console.md);
// this interface is the seam it plugs into and the place org-scoping is enforced.
type LogStreamer interface {
	// StreamLogs streams sandboxID's logs for org into w until ctx is done. The
	// implementation MUST verify the sandbox belongs to org before streaming;
	// otherwise it returns ErrNotFound.
	StreamLogs(ctx context.Context, orgID, sandboxID string, w LogSink) error
}

// LogSink receives streamed log lines. It is the narrow seam the HTTP handler
// (or a websocket) adapts; keeping it abstract lets the BFF be tested without a
// live transport.
type LogSink interface {
	Write(line []byte) error
}

// TemplateView is the console's shape of one sandbox template available to an
// org. It ties to the #220 SandboxTemplate CRD; the BFF exposes a read view so
// the console can list and inspect templates. It carries no secret.
type TemplateView struct {
	Name        string    `json:"name"`
	OrgID       string    `json:"org_id"`
	Description string    `json:"description"`
	Image       string    `json:"image"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TemplateLister is the read seam for sandbox templates. The REAL implementation
// lists SandboxTemplate CRDs scoped to the org's namespace; this slice ships an
// injectable interface and an in-memory fake. List MUST return only the org's
// templates.
type TemplateLister interface {
	List(ctx context.Context, orgID string) ([]TemplateView, error)
}

// AuditEvent is one immutable, non-secret line in an org's audit log: who did
// what, when. It NEVER carries a key value or any secret; Detail is a non-secret,
// human-legible summary (for example "created key abc12" or "revoked key xyz").
//
// ActorName and TargetName are best-effort, human-legible labels resolved at
// record time (see Console.audit): ActorName from the actor's account
// (display name, falling back to email), TargetName from whatever the call
// site already has in hand (a key's name, a project's name, and so on). Both
// may be empty (an account lookup failure, or no name being available for the
// target kind); the console UI falls back to the raw id when empty. ActorType
// is one of "user", "api_key", "system"; TargetType is one of "session",
// "key", "sandbox", "member", "project", "secret", "sink", "profile", "org",
// "waitlist" (the instance-operator plane's waitlist-approve action) and
// "system" (the instance-operator plane's other read views, which have no
// single org/entity target).
type AuditEvent struct {
	// OrgID scopes the event to one organization for every normal (non-admin)
	// action. The reserved value console.InstanceAuditOrgID ("_instance") is
	// used instead for every instance-operator-plane (admin.*) event, which
	// has no single owning org; a real org id can never take this value (see
	// InstanceAuditOrgID's doc), so the two namespaces never collide.
	OrgID      string    `json:"org_id"`
	ActorID    string    `json:"actor_id"`
	ActorName  string    `json:"actor_name"`
	ActorType  string    `json:"actor_type"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	TargetType string    `json:"target_type"`
	TargetName string    `json:"target_name"`
	Detail     string    `json:"detail"`
	At         time.Time `json:"at"`
}

// DefaultAuditListLimit is the page size GET /console/audit reads: enough for
// the audit table's default view without pulling an org's entire history on
// every load. MaxAuditListLimit is the cap the NDJSON export
// (GET /console/audit/export) reads instead: it deliberately matches
// MemAuditLog's own per-org retention cap, so an export can read everything
// the store is guaranteed to still hold, durable or in-memory.
const (
	DefaultAuditListLimit = 200
	MaxAuditListLimit     = 10000
)

// AuditRecorder is the org-scoped audit-log seam. No audit log existed in the
// accounts service, so the console defines this minimal interface and ships an
// in-memory tested implementation (MemAuditLog). The console records key
// create/revoke and sandbox terminate, and reads the log for the org/members
// audit view. List MUST return only the named org's events.
type AuditRecorder interface {
	// Record appends an audit event. The event carries no secret.
	Record(ctx context.Context, ev AuditEvent) error
	// List returns up to limit of the org's audit events in reverse-chronological
	// order (most recent first). limit <= 0 is treated as DefaultAuditListLimit.
	// It only ever returns the named org's events.
	List(ctx context.Context, orgID string, limit int) ([]AuditEvent, error)
}
