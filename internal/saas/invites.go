package saas

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// InvitationState is the lifecycle state of an org invitation.
type InvitationState string

const (
	// InvitationPending is the state of a freshly created (or resent)
	// invitation that has not yet been accepted, revoked, or observed past
	// its expiry.
	InvitationPending InvitationState = "pending"
	// InvitationAccepted marks an invitation that was successfully accepted;
	// terminal.
	InvitationAccepted InvitationState = "accepted"
	// InvitationExpired is never written to storage: it is the value
	// Invitation.EffectiveState computes for a pending invitation observed
	// past its ExpiresAt. It exists as a state name so the console API and
	// SPA have a single vocabulary for "this invite can no longer be
	// accepted, but nobody explicitly revoked it."
	InvitationExpired InvitationState = "expired"
	// InvitationRevoked marks an invitation an org admin explicitly revoked;
	// terminal. In this implementation a revoke deletes the row (see
	// RevokeInvite / RemoveInvitation), so this value is not currently
	// observed in storage, but is kept as a named state for API stability
	// and any future soft-revoke implementation.
	InvitationRevoked InvitationState = "revoked"
)

// Invitation is a pending (or resolved) offer for an account to join OrgID
// at Role. The raw invite token is NEVER stored, only its sha256 hash
// (TokenHash), mirroring the onboarding verify-token pattern
// (internal/saas/onboarding/service.go); the raw value exists only in the
// one email it was sent in.
type Invitation struct {
	ID        string
	OrgID     string
	Email     string
	Role      Role
	TokenHash string
	State     InvitationState
	InviterID string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// EffectiveState reports the invitation's state as observed at now: a
// pending invitation past its ExpiresAt reads as expired even though the
// stored State stays "pending" until an explicit transition (accept)
// changes it. Nothing ever writes the expired state back; this is a pure
// read, matching the "mark expired lazily on read" rule.
func (i Invitation) EffectiveState(now time.Time) InvitationState {
	if i.State == InvitationPending && !i.ExpiresAt.IsZero() && !now.Before(i.ExpiresAt) {
		return InvitationExpired
	}
	return i.State
}

// InvitationTTL is the lifetime of a freshly created (or resent) invitation.
const InvitationTTL = 7 * 24 * time.Hour

// Errors returned by InvitationService. None of these ever carry the raw
// invite token.
var (
	// ErrInvitePending is returned by CreateInvite when a still-pending
	// invitation already covers this (org, email) pair.
	ErrInvitePending = errors.New("saas: an invitation is already pending for this email")
	// ErrInviteExpired is returned by AcceptInvite when the token resolves to
	// an invitation whose effective state is expired.
	ErrInviteExpired = errors.New("saas: this invitation has expired")
	// ErrInviteNotPending is returned by AcceptInvite/ResendInvite when the
	// invitation has already been accepted (or otherwise is not pending).
	ErrInviteNotPending = errors.New("saas: this invitation is no longer pending")
	// ErrInviteEmailMismatch is returned by AcceptInvite when the signed-in
	// account's email does not satisfy the invite email-match rule.
	ErrInviteEmailMismatch = errors.New("saas: this invitation was sent to a different email address")
)

// consumerEmailDomains is the closed set of personal webmail domains that
// never match the accept email-match rule by domain alone: only an EXACT
// address match works for these. Any other domain is treated as a corporate
// domain, so a verified account on the same domain as the invited address
// may accept even when the local part differs (the "invited team@corp.com,
// any verified corp.com colleague can claim it" case).
var consumerEmailDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"yahoo.com":      true,
	"icloud.com":     true,
	"me.com":         true,
	"proton.me":      true,
	"protonmail.com": true,
	"gmx.de":         true,
	"gmx.net":        true,
	"web.de":         true,
	"mail.com":       true,
}

// emailDomain returns the lowercased domain of email, or "" if email has no
// (non-trailing) '@'.
func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// InviteEmailMatches reports whether accountEmail may accept an invitation
// addressed to inviteEmail: an exact case-insensitive match always
// satisfies the rule. Otherwise, when both addresses share a domain that is
// NOT a known consumer webmail domain, the accounts are treated as
// colleagues at the same organization and the match succeeds. A consumer
// domain (gmail, outlook, ...) never matches on domain alone; it requires an
// exact address match. Exported so the onboarding auto-join hook
// (internal/saas/onboarding/service.go) enforces the identical rule the
// explicit accept endpoint does.
func InviteEmailMatches(inviteEmail, accountEmail string) bool {
	a := strings.ToLower(strings.TrimSpace(inviteEmail))
	b := strings.ToLower(strings.TrimSpace(accountEmail))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	da, db := emailDomain(a), emailDomain(b)
	if da == "" || db == "" || da != db {
		return false
	}
	return !consumerEmailDomains[da]
}

// InviteEmailSender is the seam InvitationService delivers invite email
// through. It mirrors onboarding.EmailSender's SendInvite method signature so
// the SAME concrete SMTP sender (internal/saas/onboarding/smtp.go) satisfies
// both without saas importing onboarding, which would cycle (onboarding
// already imports saas, and onboarding/http.go imports console).
type InviteEmailSender interface {
	// SendInvite delivers an invite email to email carrying the raw token.
	// Implementations MUST NOT log or store the raw token.
	SendInvite(ctx context.Context, email, orgName, inviterName, token string) error
}

// FakeInviteEmailSender is an in-memory InviteEmailSender for tests. It
// records the most recent raw token sent per recipient email. NOT for
// production use: a real sender never retains tokens.
type FakeInviteEmailSender struct {
	sent map[string]string
}

// NewFakeInviteEmailSender returns an empty fake sender.
func NewFakeInviteEmailSender() *FakeInviteEmailSender {
	return &FakeInviteEmailSender{sent: map[string]string{}}
}

// SendInvite records token for email so a test can retrieve it.
func (f *FakeInviteEmailSender) SendInvite(_ context.Context, email, _, _, token string) error {
	if f.sent == nil {
		f.sent = map[string]string{}
	}
	f.sent[email] = token
	return nil
}

// LastToken returns the most recent raw invite token sent to email, or "" if
// none.
func (f *FakeInviteEmailSender) LastToken(email string) string { return f.sent[email] }

// InvitationService is the application-level surface over org invitations:
// create, list, revoke, resend, look up (pre-auth), and accept. It is the
// seam the console invites handlers (internal/saas/console/invites.go) and
// the onboarding auto-join hook build on.
type InvitationService struct {
	store    Store
	email    InviteEmailSender
	now      func() time.Time
	idgen    func() string
	tokengen func() (string, error)
	tokenTTL time.Duration
}

// InvitationServiceOption configures an InvitationService.
type InvitationServiceOption func(*InvitationService)

// WithInvitationClock sets the time source for deterministic tests.
func WithInvitationClock(now func() time.Time) InvitationServiceOption {
	return func(s *InvitationService) { s.now = now }
}

// WithInvitationIDGen sets the id generator for deterministic tests.
func WithInvitationIDGen(gen func() string) InvitationServiceOption {
	return func(s *InvitationService) { s.idgen = gen }
}

// WithInvitationTokenGen sets the invite-token generator for deterministic
// tests. The generated value is a secret; it is hashed before storage and
// never logged.
func WithInvitationTokenGen(gen func() (string, error)) InvitationServiceOption {
	return func(s *InvitationService) { s.tokengen = gen }
}

// WithInvitationTTL overrides the invitation lifetime. Default is
// InvitationTTL (7 days).
func WithInvitationTTL(d time.Duration) InvitationServiceOption {
	return func(s *InvitationService) { s.tokenTTL = d }
}

// NewInvitationService builds an InvitationService over store. email may be
// nil, in which case CreateInvite/ResendInvite persist the invitation but
// send no mail (useful for tests that drive tokens directly through the
// store).
func NewInvitationService(store Store, email InviteEmailSender, opts ...InvitationServiceOption) *InvitationService {
	s := &InvitationService{
		store:    store,
		email:    email,
		now:      time.Now,
		idgen:    randomID,
		tokengen: randomInviteToken,
		tokenTTL: InvitationTTL,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// randomInviteToken returns a high-entropy url-safe invite token. The value
// is a secret: it is hashed before storage and never logged.
func randomInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashInviteToken returns the hex sha256 of v, the form stored at rest.
func hashInviteToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// displayNameOrEmailSaas mirrors console.displayNameOrEmail (unexported in
// that package) for saas-side email composition: prefer the display name,
// falling back to the email when unset.
func displayNameOrEmailSaas(a Account) string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.Email
}

// CreateInvite invites email to join orgID at role, recorded as sent by
// inviterID. It refuses (ErrInvitePending) when a still-pending invitation
// already covers this (org, email) pair, so re-inviting the same address
// twice does not silently fork two live tokens for one person. The invite
// email is sent before this method returns; the raw token is NEVER returned
// to the caller, only delivered in that one email.
func (s *InvitationService) CreateInvite(ctx context.Context, orgID, inviterID, email string, role Role) (Invitation, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return Invitation{}, fmt.Errorf("create invite: email is required")
	}
	if role == "" {
		role = RoleMember
	}
	now := s.now()

	existing, err := s.store.ListInvitations(ctx, orgID)
	if err != nil {
		return Invitation{}, fmt.Errorf("create invite: list existing: %w", err)
	}
	for _, inv := range existing {
		if strings.EqualFold(inv.Email, email) && inv.EffectiveState(now) == InvitationPending {
			return Invitation{}, ErrInvitePending
		}
	}

	rawToken, err := s.tokengen()
	if err != nil {
		return Invitation{}, fmt.Errorf("create invite: generate token: %w", err)
	}
	inv := Invitation{
		ID:        s.idgen(),
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		TokenHash: hashInviteToken(rawToken),
		State:     InvitationPending,
		InviterID: inviterID,
		CreatedAt: now,
		ExpiresAt: now.Add(s.tokenTTL),
	}
	if err := s.store.CreateInvitation(ctx, inv); err != nil {
		return Invitation{}, fmt.Errorf("create invite: store: %w", err)
	}
	if err := s.sendInvite(ctx, inv, rawToken); err != nil {
		return Invitation{}, fmt.Errorf("create invite: send email: %w", err)
	}
	return inv, nil
}

// sendInvite composes and delivers the invite email for inv, resolving the
// org and inviter display names best-effort (a lookup failure falls back to
// the raw id rather than failing the invite). A nil email sender is a no-op.
func (s *InvitationService) sendInvite(ctx context.Context, inv Invitation, rawToken string) error {
	if s.email == nil {
		return nil
	}
	orgName := inv.OrgID
	if org, err := s.store.GetOrg(ctx, inv.OrgID); err == nil {
		orgName = org.Name
	}
	inviterName := inv.InviterID
	if acct, err := s.store.GetAccount(ctx, inv.InviterID); err == nil {
		inviterName = displayNameOrEmailSaas(acct)
	}
	return s.email.SendInvite(ctx, inv.Email, orgName, inviterName, rawToken)
}

// ListInvites returns orgID's invitations, most recently created first. The
// STORED state is returned; callers that display state to a user should
// apply EffectiveState(now) themselves (the console view does this).
func (s *InvitationService) ListInvites(ctx context.Context, orgID string) ([]Invitation, error) {
	invs, err := s.store.ListInvitations(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	sort.Slice(invs, func(i, j int) bool { return invs[i].CreatedAt.After(invs[j].CreatedAt) })
	return invs, nil
}

// findInOrg returns the invitation with id inside orgID, or ErrNotFound if it
// does not exist or belongs to a different org (the two are indistinguishable
// to the caller, the cross-org isolation backstop).
func (s *InvitationService) findInOrg(ctx context.Context, orgID, id string) (Invitation, error) {
	invs, err := s.store.ListInvitations(ctx, orgID)
	if err != nil {
		return Invitation{}, fmt.Errorf("find invite: %w", err)
	}
	for _, inv := range invs {
		if inv.ID == id {
			return inv, nil
		}
	}
	return Invitation{}, ErrNotFound
}

// RevokeInvite deletes the invitation identified by id from orgID and
// returns the deleted record so the caller can build an audit event (the
// email would no longer be readable once the row is gone).
func (s *InvitationService) RevokeInvite(ctx context.Context, orgID, id string) (Invitation, error) {
	inv, err := s.findInOrg(ctx, orgID, id)
	if err != nil {
		return Invitation{}, err
	}
	if err := s.store.RemoveInvitation(ctx, id); err != nil {
		return Invitation{}, fmt.Errorf("revoke invite: %w", err)
	}
	return inv, nil
}

// ResendInvite re-sends the invite with a FRESH token and a renewed expiry.
// The raw token is never persisted, so a resend cannot recover the original
// one; it mints a new one instead. The old invitation row is deleted and
// replaced by a new one (new id) carrying the same org, email, role, and
// inviter. Only a pending (including lazily-expired) invitation may be
// resent; an already-accepted invitation returns ErrInviteNotPending.
func (s *InvitationService) ResendInvite(ctx context.Context, orgID, id string) (Invitation, error) {
	inv, err := s.findInOrg(ctx, orgID, id)
	if err != nil {
		return Invitation{}, err
	}
	now := s.now()
	switch inv.EffectiveState(now) {
	case InvitationPending, InvitationExpired:
		// resendable
	default:
		return Invitation{}, ErrInviteNotPending
	}
	if err := s.store.RemoveInvitation(ctx, id); err != nil {
		return Invitation{}, fmt.Errorf("resend invite: remove old: %w", err)
	}
	rawToken, err := s.tokengen()
	if err != nil {
		return Invitation{}, fmt.Errorf("resend invite: generate token: %w", err)
	}
	fresh := Invitation{
		ID:        s.idgen(),
		OrgID:     inv.OrgID,
		Email:     inv.Email,
		Role:      inv.Role,
		TokenHash: hashInviteToken(rawToken),
		State:     InvitationPending,
		InviterID: inv.InviterID,
		CreatedAt: now,
		ExpiresAt: now.Add(s.tokenTTL),
	}
	if err := s.store.CreateInvitation(ctx, fresh); err != nil {
		return Invitation{}, fmt.Errorf("resend invite: store: %w", err)
	}
	if err := s.sendInvite(ctx, fresh, rawToken); err != nil {
		return Invitation{}, fmt.Errorf("resend invite: send email: %w", err)
	}
	return fresh, nil
}

// InviteLookup is the public, pre-auth summary of an invitation: enough for
// the accept page to render "Alice invited you to Acme" before the viewer
// signs in. The invited email is masked to a hint; the invitation id and
// token hash are never included.
type InviteLookup struct {
	OrgName     string
	InviterName string
	EmailHint   string
	Role        Role
	State       InvitationState
}

// maskEmail returns a partially-masked form of email for pre-auth display,
// e.g. "jo***@example.com". Malformed input (no '@') is returned unmodified.
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return email
	}
	local, domain := email[:at], email[at:]
	keep := 2
	if len(local) < keep {
		keep = len(local)
	}
	stars := len(local) - keep
	if stars < 1 {
		stars = 1
	}
	return local[:keep] + strings.Repeat("*", stars) + domain
}

// LookupInvite resolves a raw invite token to its public summary WITHOUT
// requiring a session. It is the seam GET /console/invites/lookup uses. The
// effective state (expired included) is computed at now.
func (s *InvitationService) LookupInvite(ctx context.Context, rawToken string) (InviteLookup, error) {
	if rawToken == "" {
		return InviteLookup{}, ErrNotFound
	}
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInviteToken(rawToken))
	if err != nil {
		return InviteLookup{}, err
	}
	out := InviteLookup{
		EmailHint: maskEmail(inv.Email),
		Role:      inv.Role,
		State:     inv.EffectiveState(s.now()),
	}
	if org, err := s.store.GetOrg(ctx, inv.OrgID); err == nil {
		out.OrgName = org.Name
	}
	if acct, err := s.store.GetAccount(ctx, inv.InviterID); err == nil {
		out.InviterName = displayNameOrEmailSaas(acct)
	}
	return out, nil
}

// AcceptInvite resolves rawToken, checks it is still effectively pending and
// that accountEmail satisfies InviteEmailMatches against the invite's
// address, then adds accountID as a member of the invitation's org at its
// role and marks the invitation accepted. It returns the accepted
// invitation (OrgID and Role populated) so the caller can build a response
// and an audit event.
func (s *InvitationService) AcceptInvite(ctx context.Context, accountID, accountEmail, rawToken string) (Invitation, error) {
	if rawToken == "" {
		return Invitation{}, ErrNotFound
	}
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInviteToken(rawToken))
	if err != nil {
		return Invitation{}, err
	}
	now := s.now()
	switch inv.EffectiveState(now) {
	case InvitationExpired:
		return Invitation{}, ErrInviteExpired
	case InvitationPending:
		// proceed
	default:
		return Invitation{}, ErrInviteNotPending
	}
	if !InviteEmailMatches(inv.Email, accountEmail) {
		return Invitation{}, ErrInviteEmailMismatch
	}
	if err := s.store.PutMembership(ctx, Membership{
		AccountID: accountID,
		OrgID:     inv.OrgID,
		Role:      inv.Role,
		CreatedAt: now,
	}); err != nil {
		return Invitation{}, fmt.Errorf("accept invite: store membership: %w", err)
	}
	if err := s.store.UpdateInvitationState(ctx, inv.ID, InvitationAccepted); err != nil {
		return Invitation{}, fmt.Errorf("accept invite: mark accepted: %w", err)
	}
	inv.State = InvitationAccepted
	return inv, nil
}
