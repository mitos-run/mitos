package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// inviteRateLimitPerOrg and inviteRateLimitWindow bound invite CREATION
// (both CreateInvite and ResendInvite, which mints a fresh token, count
// against the same bucket) so a compromised or careless member cannot mail-
// bomb arbitrary addresses through an org's invite flow.
const (
	inviteRateLimitPerOrg = 50
	inviteRateLimitWindow = 24 * time.Hour
)

// inviteRateLimiter is an in-memory per-org sliding-window cap. DOCUMENTED
// V1 LIMITATION: this counter lives in one process's memory, so a
// multi-replica console deployment does not share it across pods; a durable
// (Postgres or Redis) limiter is a follow-up once a single org's invite
// traffic can hit more than one replica.
type inviteRateLimiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	hits   map[string][]time.Time
}

func newInviteRateLimiter(limit int, window time.Duration) *inviteRateLimiter {
	return &inviteRateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// Allow records an attempt for key at now and reports whether it is within
// the cap. A nil receiver or a non-positive limit always allows (disabled).
func (l *inviteRateLimiter) Allow(key string, now time.Time) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	var fresh []time.Time
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= l.limit {
		l.hits[key] = fresh
		return false
	}
	l.hits[key] = append(fresh, now)
	return true
}

// InvitationView is the console's JSON shape of one org invitation. State is
// the EFFECTIVE state (lazy expiry applied at read time), never the raw
// stored state, so the SPA never needs to compute expiry itself.
type InvitationView struct {
	ID          string               `json:"id"`
	OrgID       string               `json:"org_id"`
	Email       string               `json:"email"`
	Role        saas.Role            `json:"role"`
	State       saas.InvitationState `json:"state"`
	InviterID   string               `json:"inviter_id"`
	InviterName string               `json:"inviter_name"`
	CreatedAt   time.Time            `json:"created_at"`
	ExpiresAt   time.Time            `json:"expires_at"`
}

func (c *Console) invitationView(ctx context.Context, inv saas.Invitation, now time.Time) InvitationView {
	v := InvitationView{
		ID:        inv.ID,
		OrgID:     inv.OrgID,
		Email:     inv.Email,
		Role:      inv.Role,
		State:     inv.EffectiveState(now),
		InviterID: inv.InviterID,
		CreatedAt: inv.CreatedAt,
		ExpiresAt: inv.ExpiresAt,
	}
	if acct, err := c.deps.Accounts.GetAccount(ctx, inv.InviterID); err == nil {
		v.InviterName = displayNameOrEmail(acct)
	}
	return v
}

// invitationsUnavailable writes the standard "not configured" response
// shared by every invite handler when the deployment has not wired an
// InvitationService (Deps.Invitations is nil).
func (c *Console) invitationsUnavailable(w http.ResponseWriter) {
	apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
		WithCause("invitations are not enabled on this deployment"))
}

// --- List (GET /console/invites) ---

func (c *Console) handleListInvites(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	// Reading invites requires org membership, mirroring the members/audit read gate.
	if _, err := c.deps.Accounts.ListMembers(r.Context(), accountID, orgID); err != nil {
		c.failAccount(w, err, "the invitations could not be listed for this organization")
		return
	}
	invs, err := c.deps.Invitations.ListInvites(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the invitations could not be listed"))
		return
	}
	now := c.deps.Now()
	out := make([]InvitationView, 0, len(invs))
	for _, inv := range invs {
		out = append(out, c.invitationView(r.Context(), inv, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "invitations": out})
}

// --- Create (POST /console/invites) ---

type createInviteRequest struct {
	Email string    `json:"email"`
	Role  saas.Role `json:"role"`
}

func (c *Console) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageMembers)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req createInviteRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).WithCause("the invite body is not valid JSON"))
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).WithCause("an email address is required"))
		return
	}
	if !c.inviteRateLimit.Allow(orgID, c.deps.Now()) {
		apierr.Encode(w, apierr.Get(apierr.CodeRateLimited).
			WithCause("too many invitations created for this organization in the last 24 hours"))
		return
	}
	inv, err := c.deps.Invitations.CreateInvite(r.Context(), orgID, accountID, req.Email, req.Role)
	if err != nil {
		if errors.Is(err, saas.ErrInvitePending) {
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
				WithCause("an invitation is already pending for this email"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the invitation could not be created"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "invite.create", Target: inv.ID, TargetType: "invite", TargetName: inv.Email,
		Detail: "invited " + inv.Email + " as " + string(inv.Role),
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusCreated, c.invitationView(r.Context(), inv, c.deps.Now()))
}

// --- Revoke (DELETE /console/invites/{id}) ---

func (c *Console) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageMembers)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	inv, err := c.deps.Invitations.RevokeInvite(r.Context(), orgID, id)
	if err != nil {
		if errors.Is(err, saas.ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the invitation does not exist or does not belong to this organization"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the invitation could not be revoked"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "invite.revoke", Target: inv.ID, TargetType: "invite", TargetName: inv.Email,
		Detail: "revoked invitation to " + inv.Email,
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "revoked": id})
}

// --- Resend (POST /console/invites/{id}/resend) ---

func (c *Console) handleResendInvite(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageMembers)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	if !c.inviteRateLimit.Allow(orgID, c.deps.Now()) {
		apierr.Encode(w, apierr.Get(apierr.CodeRateLimited).
			WithCause("too many invitations created for this organization in the last 24 hours"))
		return
	}
	inv, err := c.deps.Invitations.ResendInvite(r.Context(), orgID, id)
	if err != nil {
		switch {
		case errors.Is(err, saas.ErrNotFound):
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the invitation does not exist or does not belong to this organization"))
		case errors.Is(err, saas.ErrInviteNotPending):
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
				WithCause("this invitation has already been accepted"))
		default:
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the invitation could not be resent"))
		}
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "invite.resend", Target: inv.ID, TargetType: "invite", TargetName: inv.Email,
		Detail: "resent invitation to " + inv.Email,
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, c.invitationView(r.Context(), inv, c.deps.Now()))
}

// --- Public lookup (pre-auth) ---

// InviteLookupView is the PUBLIC, unauthenticated response for
// GET /console/invites/lookup. It carries no secret and reveals nothing an
// unauthenticated holder of the link could not already infer.
type InviteLookupView struct {
	OrgName     string               `json:"org_name"`
	InviterName string               `json:"inviter_name"`
	EmailHint   string               `json:"email_hint"`
	Role        saas.Role            `json:"role"`
	State       saas.InvitationState `json:"state"`
}

// LookupInvite is a PUBLIC handler: the binary mounts it directly on the top-
// level mux OUTSIDE the session middleware (exactly like GET
// /auth/connectors), NOT through Console's own /console/ mux, so it never
// requires a session. It resolves an invite token to its public summary so
// the pre-auth accept page can render "X invited you to Y" before the
// viewer signs in.
func (c *Console) LookupInvite(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	token := r.URL.Query().Get("token")
	res, err := c.deps.Invitations.LookupInvite(r.Context(), token)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("this invitation link is invalid or has expired"))
		return
	}
	writeJSON(w, http.StatusOK, InviteLookupView{
		OrgName: res.OrgName, InviterName: res.InviterName, EmailHint: res.EmailHint,
		Role: res.Role, State: res.State,
	})
}

// --- Accept (session required, POST /console/invites/accept) ---

type acceptInviteRequest struct {
	Token string `json:"token"`
}

func (c *Console) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	if c.deps.Invitations == nil {
		c.invitationsUnavailable(w)
		return
	}
	accountID, _, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req acceptInviteRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).WithCause("the accept body is not valid JSON"))
		return
	}
	acct, err := c.deps.Accounts.GetAccount(r.Context(), accountID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the caller's account could not be read"))
		return
	}
	inv, err := c.deps.Invitations.AcceptInvite(r.Context(), accountID, acct.Email, req.Token)
	if err != nil {
		switch {
		case errors.Is(err, saas.ErrNotFound):
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).WithCause("this invitation link is invalid"))
		case errors.Is(err, saas.ErrInviteExpired):
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).WithCause("this invitation has expired"))
		case errors.Is(err, saas.ErrInviteNotPending):
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).WithCause("this invitation has already been used"))
		case errors.Is(err, saas.ErrInviteEmailMismatch):
			apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
				WithCause("this invitation was sent to a different email address"))
		default:
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the invitation could not be accepted"))
		}
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: inv.OrgID, ActorID: accountID,
		Action: "invite.accept", Target: inv.ID, TargetType: "invite", TargetName: inv.Email,
		Detail: "accepted invitation to join as " + string(inv.Role),
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": inv.OrgID, "role": inv.Role})
}

// --- Member removal (DELETE /console/members/{accountID}) ---

func (c *Console) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	targetID := r.PathValue("accountID")
	// Resolve the TARGET's own name before removing it, best-effort, so the
	// audit sentence reads "removed Carol" instead of a bare account id.
	var targetName string
	if acct, err := c.deps.Accounts.GetAccount(r.Context(), targetID); err == nil {
		targetName = displayNameOrEmail(acct)
	}
	if err := c.deps.Accounts.RemoveMember(r.Context(), accountID, orgID, targetID); err != nil {
		switch {
		case errors.Is(err, saas.ErrForbidden):
			apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
				WithCause("the caller does not have permission to remove members"))
		case errors.Is(err, saas.ErrLastOwner):
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
				WithCause("the last owner of an organization cannot be removed"))
		case errors.Is(err, saas.ErrNotFound):
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the target account is not a member of this organization"))
		default:
			c.failAccount(w, err, "the member could not be removed")
		}
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "member.remove", Target: targetID, TargetType: "member", TargetName: targetName,
		Detail: "removed member " + targetName,
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "removed": targetID})
}
