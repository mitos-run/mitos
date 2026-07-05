// The instance-operator plane: GET/POST /console/admin/... (issue-tracked as
// Workstream 6, "instance operator plane /admin").
//
// This is a SEPARATE authorization plane from every other console endpoint.
// Every other handler in this package is scoped to the caller's OWN org via
// permissionsFor/authorize (org RBAC: owner/admin/billing/member/viewer, or a
// custom role). Instance-admin sits ABOVE that: it lets a deployment
// OPERATOR see every org, the node inventory, and the signup waitlist. It is
// deliberately resolved independently of permissionsFor/authorize so it can
// never be granted through a custom role or a built-in org role (a org
// "admin" or "owner" is NOT automatically an instance admin; see
// isInstanceAdmin). Every /console/admin/... handler goes through
// authorizeAdmin, never authorize, and is audited via the admin.* action
// namespace, always under InstanceAuditOrgID (see its doc) so these events
// are never invisible the way a normal org-scoped event with an empty OrgID
// would be. A FAILED authorizeAdmin attempt is also audited, as
// "admin.denied" (see authorizeAdmin), so denied probing of this plane is
// itself visible via GET /console/admin/audit.
//
// Two paths grant the capability (isInstanceAdmin):
//
//   - the caller's account email is in the deployment's configured
//     instance-admin allowlist (Deps.InstanceAdminEmails, case-insensitive
//     exact match; set via MITOS_CONSOLE_INSTANCE_ADMINS, the hosted-
//     deployment path); or
//   - the community-edition fallback: this deployment currently has EXACTLY
//     ONE organization and the caller is that org's owner, so a self-hoster
//     always has an operator seat with zero extra configuration. This
//     fallback is gated on Capabilities.Edition == "community" so a hosted
//     deployment's very first customer is never silently promoted to
//     instance admin.
package console

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// adminOrgRollupCap bounds how many orgs GET /console/admin/overview and GET
// /console/admin/orgs aggregate PER-ORG data (running-sandbox counts,
// month-to-date usage) over: each org costs one cluster/store read, so an
// unbounded scan is expensive on a deployment with many orgs. The org COUNT
// itself (overview's "orgs" field, and the orgs endpoint's "total") is
// always the TRUE total, uncapped; only the per-org rollup subset is capped,
// at the oldest adminOrgRollupCap orgs by creation time.
const adminOrgRollupCap = 200

// InstanceAuditOrgID is the reserved AuditEvent.OrgID value every
// instance-operator-plane event (the admin.* action namespace) is recorded
// under: an admin.* event has no single owning org (it is either a
// deployment-wide read like admin.overview.view, or a denied access attempt
// that predates knowing who the caller even is), so recording it with an
// empty OrgID left it permanently invisible to every audit view, which is
// itself org-scoped (see AuditEvent.OrgID's doc). No real organization may
// ever be assigned this id (see saas.Organization.ID's generation), so it
// can never collide with a tenant's own audit stream.
const InstanceAuditOrgID = "_instance"

// OrgDirectory is the org-wide read seam the instance-operator plane uses.
// Unlike every other console seam, it is NOT scoped to the caller's own org:
// it lists every organization on the deployment, because it backs the
// operator surface (gated on isInstanceAdmin), never a tenant-facing view.
// saas.Store satisfies this directly, mirroring the drawdown driver's own
// narrow org-iteration seam (cmd/console/drawdown.go's drawdownOrgLister).
type OrgDirectory interface {
	// ListOrgs returns every organization on the deployment, in no
	// particular order.
	ListOrgs(ctx context.Context) ([]saas.Organization, error)
	// ListOrgMembers returns every membership in orgID.
	ListOrgMembers(ctx context.Context, orgID string) ([]saas.Membership, error)
}

// nullOrgDirectory is the safe-to-instantiate default: no orgs, so the admin
// endpoints degrade to an honest empty state instead of panicking on a
// Console built without a real store wired in.
type nullOrgDirectory struct{}

func (nullOrgDirectory) ListOrgs(context.Context) ([]saas.Organization, error) { return nil, nil }
func (nullOrgDirectory) ListOrgMembers(context.Context, string) ([]saas.Membership, error) {
	return nil, nil
}

// NodeView is the console's shape of one Kubernetes node: the read-only
// operator inventory GET /console/admin/nodes exposes. AllocatableCPU and
// AllocatableMem are the Kubernetes-formatted quantity strings (e.g. "16",
// "62Gi") rather than a parsed number, so the view never silently loses the
// unit.
type NodeView struct {
	Name           string `json:"name"`
	Ready          bool   `json:"ready"`
	KVM            bool   `json:"kvm"`
	Dedicated      bool   `json:"dedicated"`
	AllocatableCPU string `json:"allocatable_cpu"`
	AllocatableMem string `json:"allocatable_mem"`
}

// NodeSource is the k8s node-listing seam GET /console/admin/nodes reads.
// The real implementation lists corev1.Node objects over the SAME
// controller-runtime client the console's cluster sandbox adapter uses. A
// nil NodeSource (Deps.Nodes's zero value, deliberately NOT defaulted by
// New) means no Kubernetes client is configured on this deployment; the
// handler reports {"available": false} rather than an empty (and
// misleadingly "no nodes exist") list.
type NodeSource interface {
	Nodes(ctx context.Context) ([]NodeView, error)
}

// WaitlistEntry is the console's seam-level shape of one recorded waitlist
// signup: an email and when it was recorded. It carries no id: the
// underlying onboarding.PendingStore.Waitlist has none (its WaitlistEntry is
// Email+CreatedAt only); the wire-level id the SPA uses to target
// POST /console/admin/waitlist/{id}/approve is synthesized by the HTTP
// handler, not by this seam.
type WaitlistEntry struct {
	Email     string
	CreatedAt time.Time
}

// WaitlistSource is the seam over the onboarding funnel's waitlist. The real
// implementation (wired in cmd/console) reads
// onboarding.PendingStore.Waitlist and approves through
// onboarding.ApproveWaitlistEntry: add the canonical email to the signup
// allowlist and send the "you're in" email through the SAME EmailSender the
// funnel uses. Approve does NOT create an account, an org, or any
// invitation: it only lifts the allowlist gate an approved signup's own
// later Verify call will need to pass (see docs/saas/onboarding.md).
type WaitlistSource interface {
	// List returns every recorded waitlist entry.
	List(ctx context.Context) ([]WaitlistEntry, error)
	// Approve grants allowlist access to email and sends the approved
	// notification, UNLESS email already holds allowlist access (tracked via
	// the SAME allowlist row Approve itself would add), in which case it is
	// idempotent: no second row is added and no second notification is
	// sent, and alreadyApproved is true. Returns ErrWaitlistNotConfigured if
	// no allowlist/email seam is wired on this deployment.
	Approve(ctx context.Context, email string) (alreadyApproved bool, err error)
}

// ErrWaitlistNotConfigured is returned by WaitlistSource.Approve when the
// underlying onboarding allowlist/email seam is not wired on this
// deployment (the nullWaitlistSource default, or a deployment that never
// configured one).
var ErrWaitlistNotConfigured = errors.New("console: no waitlist/allowlist seam is configured on this deployment")

// nullWaitlistSource is the safe-to-instantiate default: an empty waitlist
// that refuses every approval with ErrWaitlistNotConfigured, an honest
// degraded state rather than a silent no-op success.
type nullWaitlistSource struct{}

func (nullWaitlistSource) List(context.Context) ([]WaitlistEntry, error) { return nil, nil }
func (nullWaitlistSource) Approve(context.Context, string) (bool, error) {
	return false, ErrWaitlistNotConfigured
}

// isInstanceAdmin resolves whether accountID holds the instance-operator
// capability on this deployment. See the package doc above for the two
// grant paths. It never returns true for an empty accountID, and every
// lookup failure (account not found, org-list error, no membership) is
// treated as "not an admin" rather than propagated: this is a boolean gate,
// never a request that can itself fail.
func (c *Console) isInstanceAdmin(ctx context.Context, accountID string) bool {
	if accountID == "" {
		return false
	}
	if len(c.deps.InstanceAdminEmails) > 0 && c.deps.Accounts != nil {
		if acct, err := c.deps.Accounts.GetAccount(ctx, accountID); err == nil {
			email := strings.ToLower(strings.TrimSpace(acct.Email))
			if email != "" {
				for _, allowed := range c.deps.InstanceAdminEmails {
					if email == allowed {
						return true
					}
				}
			}
		}
	}
	if c.deps.Capabilities.Edition == "community" && c.deps.Orgs != nil && c.deps.Accounts != nil {
		orgs, err := c.deps.Orgs.ListOrgs(ctx)
		if err == nil && len(orgs) == 1 {
			if role, err := c.deps.Accounts.MemberRole(ctx, accountID, orgs[0].ID); err == nil && role == saas.RoleOwner {
				return true
			}
		}
	}
	return false
}

// authorizeAdmin is the single authorization gate for every
// /console/admin/... handler: it requires an authenticated session (like
// every console endpoint, via caller) AND the instance-admin capability. It
// deliberately does NOT go through permissionsFor/authorize: instance-admin
// is ABOVE org RBAC and must never be grantable via an org role or a custom
// role (see the package doc).
func (c *Console) authorizeAdmin(r *http.Request) (accountID string, e apierr.Error, ok bool) {
	accountID, _, e, ok = c.caller(r)
	if !ok {
		// accountID is "" here: caller() never resolved an identity (no
		// session, or an invalid one). The denial is still recorded so a
		// pattern of unauthenticated probing of this plane is visible, not
		// just probing by an authenticated-but-not-admin caller.
		c.auditAdminDenied(r, accountID)
		return "", e, false
	}
	if !c.isInstanceAdmin(r.Context(), accountID) {
		c.auditAdminDenied(r, accountID)
		return "", apierr.Get(apierr.CodeForbidden).
			WithCause("the caller does not hold the instance-operator capability on this deployment"), false
	}
	var noErr apierr.Error
	return accountID, noErr, true
}

// auditAdminDenied best-effort records a denied /console/admin/... access
// attempt: action "admin.denied", TargetType "system" (there is no single
// org/entity target for a denial), Detail is the requested PATH ONLY, never
// the method or the query string (a query string could carry something a
// misbehaving client should not have logged back to it). accountID may be
// "" (an entirely unauthenticated request); the event is recorded either
// way via c.audit, which is itself already best-effort (an audit failure
// never fails the request).
func (c *Console) auditAdminDenied(r *http.Request, accountID string) {
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.denied",
		TargetType: "system",
		Detail:     r.URL.Path,
		At:         c.deps.Now(),
	})
}

// adminOrgsCapped returns every org (all), sorted oldest-first by CreatedAt,
// and a subset (capped) bounded at adminOrgRollupCap for expensive per-org
// aggregation. capped aliases the head of all's backing array when no
// capping is needed. The true total is always len(all).
func (c *Console) adminOrgsCapped(ctx context.Context) (all []saas.Organization, capped []saas.Organization, err error) {
	all, err = c.deps.Orgs.ListOrgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })
	if len(all) <= adminOrgRollupCap {
		return all, all, nil
	}
	return all, all[:adminOrgRollupCap], nil
}

// runningSandboxCount returns how many of orgID's sandboxes are currently in
// the Running phase.
func (c *Console) runningSandboxCount(ctx context.Context, orgID string) (int, error) {
	boxes, err := c.deps.Sandboxes.List(ctx, orgID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range boxes {
		if b.Phase == "Running" {
			n++
		}
	}
	return n, nil
}

// planFor resolves orgID's current plan via Deps.Plans, failing closed to
// PlanFree on an unconfigured seam or a lookup error (never assumes a paid
// plan it cannot verify).
func (c *Console) planFor(ctx context.Context, orgID string) billing.Plan {
	if c.deps.Plans != nil {
		if p, err := c.deps.Plans.GetPlan(ctx, orgID); err == nil {
			return p
		}
	}
	return billing.PlanFree
}

// monthUsageCents returns orgID's month-to-date usage cost, priced with the
// SAME rate table the billing view and drawdown driver use
// (Deps.Billing.Rates), so the admin org table's figure matches what the org
// itself sees on its own billing page.
func (c *Console) monthUsageCents(ctx context.Context, orgID string, monthStart time.Time) (int64, error) {
	if c.deps.Usage == nil {
		return 0, nil
	}
	records, err := c.deps.Usage.ListRecords(ctx, orgID, monthStart, time.Time{})
	if err != nil {
		return 0, err
	}
	var total billing.Money
	for _, rec := range records {
		total += c.deps.Billing.Rates.CostCents(rec)
	}
	return int64(total), nil
}

// signupMode reports the onboarding funnel's current mode as the SPA-facing
// string, derived from the SAME server-controlled flag that gates whether
// mountOnboarding mounts the public signup endpoints (cmd/console/onboarding.go).
func signupMode(signupEnabled bool) string {
	if signupEnabled {
		return "open"
	}
	return "waitlist"
}

// AdminOverview is the response shape of GET /console/admin/overview: the
// deployment-wide counts an operator lands on first. NodesReady/NodesTotal
// are nil when no NodeSource is configured (Deps.Nodes == nil), so the SPA
// can render an honest "not available in this deployment" state rather than
// a fabricated 0/0. FailedOrgs is omitted (omitempty) when zero; when
// nonzero it reports how many orgs' per-org reads failed and were skipped
// from RunningSandboxes rather than failing the whole request (see
// handleAdminOverview).
type AdminOverview struct {
	Orgs             int `json:"orgs"`
	RunningSandboxes int `json:"running_sandboxes"`
	// RunningSandboxesOrgs is how many orgs RunningSandboxes was actually
	// rolled up over: the oldest min(Orgs, adminOrgRollupCap). The SPA
	// compares this to Orgs the SAME way the orgs table compares its own
	// Orgs/Total, to show an honest "showing sandboxes from the first N of
	// Orgs orgs" disclosure when the 200-org cap is hit, instead of
	// implying RunningSandboxes covers every org on a large deployment.
	RunningSandboxesOrgs int    `json:"running_sandboxes_orgs"`
	FailedOrgs           int    `json:"failed_orgs,omitempty"`
	NodesReady           *int   `json:"nodes_ready"`
	NodesTotal           *int   `json:"nodes_total"`
	SignupMode           string `json:"signup_mode"`
}

// handleAdminOverview serves GET /console/admin/overview.
func (c *Console) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	all, capped, err := c.adminOrgsCapped(r.Context())
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the org directory could not be read"))
		return
	}
	running := 0
	failedOrgs := 0
	for _, org := range capped {
		n, err := c.runningSandboxCount(r.Context(), org.ID)
		if err != nil {
			// A per-org sandbox-count failure is NOT fatal to the overview,
			// the same principle the Nodes handling below applies: the rest
			// of the deployment-wide summary is still useful, so this org is
			// skipped from RunningSandboxes (rather than aborting the whole
			// response for every org over one bad read) and counted in
			// FailedOrgs so the operator can see the rollup is partial.
			if c.deps.Log != nil {
				c.deps.Log.Warn("admin overview: running sandbox count failed", "org", org.ID, "err", err.Error())
			}
			failedOrgs++
			continue
		}
		running += n
	}
	view := AdminOverview{
		Orgs:                 len(all),
		RunningSandboxes:     running,
		RunningSandboxesOrgs: len(capped),
		FailedOrgs:           failedOrgs,
		SignupMode:           signupMode(c.deps.Capabilities.Signup),
	}
	if c.deps.Nodes != nil {
		if nodes, err := c.deps.Nodes.Nodes(r.Context()); err == nil {
			ready, total := 0, len(nodes)
			for _, n := range nodes {
				if n.Ready {
					ready++
				}
			}
			view.NodesReady = &ready
			view.NodesTotal = &total
		}
		// A Nodes error is NOT fatal to the overview: the rest of the
		// deployment-wide summary is still useful, so NodesReady/NodesTotal
		// simply stay nil (the same honest "not available" shape as an
		// unconfigured NodeSource) rather than failing the whole request.
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.overview.view",
		TargetType: "system",
		Detail:     "viewed the instance operator overview",
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, view)
}

// startOfMonth returns t truncated to the first instant of its (UTC)
// calendar month, the "month-to-date" lower bound monthUsageCents uses.
func startOfMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// --- GET /console/admin/orgs ---

// AdminOrgView is one row of the instance-operator org table: enough to spot
// an outlier org (heavy usage, many members, lots running) without leaving
// the operator plane.
type AdminOrgView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Tier            string `json:"tier"`
	Members         int    `json:"members"`
	Running         int    `json:"running"`
	MonthUsageCents int64  `json:"month_usage_cents"`
}

// AdminOrgsResponse is the response shape of GET /console/admin/orgs. Total
// is always the true, uncapped org count (see handleAdminOrgs's doc).
// FailedOrgs is omitted (omitempty), matching AdminOverview's convention,
// when zero; when nonzero it reports how many orgs' per-org reads failed and
// were skipped from Orgs rather than failing the whole request.
type AdminOrgsResponse struct {
	Orgs       []AdminOrgView `json:"orgs"`
	Total      int            `json:"total"`
	FailedOrgs int            `json:"failed_orgs,omitempty"`
}

// handleAdminOrgs serves GET /console/admin/orgs: every org's id/name/plan
// tier/member count/running-sandbox count/month-to-date usage, capped at
// adminOrgRollupCap oldest orgs (see its doc); Total is always the true,
// uncapped org count so the SPA can show "showing 200 of 1,203 orgs"
// honestly rather than silently truncating.
func (c *Console) handleAdminOrgs(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	all, capped, err := c.adminOrgsCapped(r.Context())
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the org directory could not be read"))
		return
	}
	monthStart := startOfMonth(c.deps.Now())
	out := make([]AdminOrgView, 0, len(capped))
	failedOrgs := 0
	for _, org := range capped {
		// A per-org read failure (membership, sandbox count, or usage) is NOT
		// fatal to the whole table: the same principle handleAdminOverview
		// applies to the Nodes read. Skip just this org, log the real error
		// for an operator to act on, and count it in failedOrgs, rather than
		// failing every org's row because one org's read hiccuped.
		members, err := c.deps.Orgs.ListOrgMembers(r.Context(), org.ID)
		if err != nil {
			if c.deps.Log != nil {
				c.deps.Log.Warn("admin orgs: org membership read failed", "org", org.ID, "err", err.Error())
			}
			failedOrgs++
			continue
		}
		running, err := c.runningSandboxCount(r.Context(), org.ID)
		if err != nil {
			if c.deps.Log != nil {
				c.deps.Log.Warn("admin orgs: running sandbox count failed", "org", org.ID, "err", err.Error())
			}
			failedOrgs++
			continue
		}
		usageCents, err := c.monthUsageCents(r.Context(), org.ID, monthStart)
		if err != nil {
			if c.deps.Log != nil {
				c.deps.Log.Warn("admin orgs: usage rollup read failed", "org", org.ID, "err", err.Error())
			}
			failedOrgs++
			continue
		}
		out = append(out, AdminOrgView{
			ID:              org.ID,
			Name:            org.Name,
			Tier:            string(c.planFor(r.Context(), org.ID)),
			Members:         len(members),
			Running:         running,
			MonthUsageCents: usageCents,
		})
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.orgs.view",
		TargetType: "system",
		Detail:     "viewed the instance operator org list",
		At:         c.deps.Now(),
	})
	resp := AdminOrgsResponse{Orgs: out, Total: len(all), FailedOrgs: failedOrgs}
	writeJSON(w, http.StatusOK, resp)
}

// --- Waitlist (GET /console/admin/waitlist, POST .../{id}/approve) ---

// AdminWaitlistEntryView is the wire shape of one waitlist entry. ID is
// synthesized here (a reversible encoding of Email), NOT part of
// WaitlistEntry: the underlying onboarding.PendingStore.Waitlist carries no
// id, so the approve action's path segment is a presentation-layer concern
// of this handler, not the seam.
type AdminWaitlistEntryView struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// encodeWaitlistID/decodeWaitlistID convert between a waitlist entry's email
// and the opaque, URL-safe path segment POST .../waitlist/{id}/approve
// takes. Reversible (not a lookup key into any store), so no server-side
// waitlist index is needed to support the approve action.
func encodeWaitlistID(email string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(email))
}

func decodeWaitlistID(id string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// handleAdminWaitlist serves GET /console/admin/waitlist.
func (c *Console) handleAdminWaitlist(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	entries, err := c.deps.Waitlist.List(r.Context())
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the waitlist could not be read"))
		return
	}
	out := make([]AdminWaitlistEntryView, 0, len(entries))
	for _, en := range entries {
		out = append(out, AdminWaitlistEntryView{ID: encodeWaitlistID(en.Email), Email: en.Email, CreatedAt: en.CreatedAt})
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.waitlist.view",
		TargetType: "system",
		Detail:     "viewed the signup waitlist",
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// waitlistApproveNotConfiguredError is the 501 returned when no
// allowlist/email seam backs Deps.Waitlist (the nullWaitlistSource default,
// or a deployment that never wired one): an honest "not available" rather
// than a fabricated success. Built directly (like failSandbox's
// ErrUnsupported case) rather than via apierr.Catalogue, which is normative
// for the forkd/sandbox-server runtime API, a different surface than this
// console BFF's own error shape.
func waitlistApproveNotConfiguredError(err error) apierr.Error {
	return apierr.Error{
		Code:        "not_implemented",
		Message:     "waitlist approval is not available on this deployment yet",
		Cause:       err.Error(),
		Remediation: "Configure the onboarding allowlist and email sender (see docs/saas/onboarding.md) to enable waitlist approval.",
		Status:      http.StatusNotImplemented,
	}
}

// handleAdminWaitlistApprove serves POST /console/admin/waitlist/{id}/approve.
// It reuses the onboarding funnel's own approval mechanism
// (onboarding.ApproveWaitlistEntry, via the WaitlistSource seam): it adds the
// waitlisted email's canonical form to the signup allowlist and sends the
// "you're in" notification through the funnel's configured EmailSender. It
// does NOT create an account, an org, or any invitation; the entry's owner
// still completes signup/verify themselves once past the allowlist gate.
//
// The id must decode to an email that IS currently on the recorded
// waitlist; a well-formed id for an email that was never waitlisted (a
// fabricated or stale id) is a 404, not a silent approve of an arbitrary
// address. Approving an email that was already approved by an earlier call
// is idempotent (see WaitlistSource.Approve): no second notification is
// sent, and the response carries "already_approved": true instead of a
// misleading identical "fresh approval" response.
func (c *Console) handleAdminWaitlistApprove(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	email, ok := decodeWaitlistID(r.PathValue("id"))
	if !ok {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).WithCause("the waitlist entry id is not valid"))
		return
	}
	entries, err := c.deps.Waitlist.List(r.Context())
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the waitlist could not be read"))
		return
	}
	onWaitlist := false
	for _, en := range entries {
		if strings.EqualFold(en.Email, email) {
			onWaitlist = true
			break
		}
	}
	if !onWaitlist {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).WithCause("no waitlist entry exists for this id"))
		return
	}
	alreadyApproved, err := c.deps.Waitlist.Approve(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrWaitlistNotConfigured) {
			apierr.Encode(w, waitlistApproveNotConfiguredError(err))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the waitlist entry could not be approved"))
		return
	}
	detail := "approved waitlist entry"
	if alreadyApproved {
		detail = "waitlist entry was already approved; no notification re-sent"
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.waitlist.approve",
		Target:     email,
		TargetType: "waitlist",
		Detail:     detail,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"email": email, "approved": true, "already_approved": alreadyApproved})
}

// --- GET /console/admin/nodes ---

// adminNodesResponse is the wire shape of GET /console/admin/nodes.
// Available is false when no NodeSource is configured (Deps.Nodes == nil,
// the honest "not available in this deployment" state) OR when the
// configured source failed to list nodes (a cluster hiccup is reported the
// same way as "not configured": either way the operator gets no real data,
// and a stale/fabricated node list would be worse than an honest blank).
type adminNodesResponse struct {
	Available bool       `json:"available"`
	Nodes     []NodeView `json:"nodes"`
}

// handleAdminNodes serves GET /console/admin/nodes.
func (c *Console) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	resp := adminNodesResponse{Nodes: []NodeView{}}
	if c.deps.Nodes != nil {
		if nodes, err := c.deps.Nodes.Nodes(r.Context()); err == nil {
			resp.Available = true
			if nodes != nil {
				resp.Nodes = nodes
			}
		}
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.nodes.view",
		TargetType: "system",
		Detail:     "viewed the node inventory",
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /console/admin/audit ---

// handleAdminAudit serves GET /console/admin/audit: the instance-operator
// plane's own audit visibility (issue #714). Every admin.* event (including
// a DENIED authorizeAdmin attempt, action "admin.denied") is recorded under
// InstanceAuditOrgID, so this is the one place an operator can see them; a
// normal org's own GET /console/audit never surfaces them, since it is
// scoped to that org's OrgID and admin.* events carry none. Reads through
// the SAME AuditRecorder every org-scoped audit view reads, so the wire
// shape (AuditEvent) is identical.
func (c *Console) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	accountID, e, ok := c.authorizeAdmin(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	events, err := c.deps.Audit.List(r.Context(), InstanceAuditOrgID, DefaultAuditListLimit)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the instance operator audit log could not be read"))
		return
	}
	if events == nil {
		events = []AuditEvent{}
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      InstanceAuditOrgID,
		ActorID:    accountID,
		Action:     "admin.audit.view",
		TargetType: "system",
		Detail:     "viewed the instance operator audit log",
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
