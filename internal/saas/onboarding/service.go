package onboarding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// Mode selects whether the funnel provisions on signup or only collects a
// waitlist entry. Until the #208 production gates pass (#163 chaos and residual
// GC, #194 external security review, and multitenancy), a deployment runs in
// ModeWaitlist; once green it flips to ModeOpen for public self-serve.
type Mode string

const (
	// ModeWaitlist records a waitlist entry on signup and provisions nothing. This
	// is the default until the production gates pass.
	ModeWaitlist Mode = "waitlist"
	// ModeOpen is full public self-serve: signup issues a verify token that, once
	// accepted, provisions the org, credit, and first key.
	ModeOpen Mode = "open"
)

// Errors returned by the onboarding service. None of them ever carries a raw
// email or a raw verify token.
var (
	// ErrWaitlistMode is returned by Verify when the funnel is in waitlist mode, so
	// there is no provisioning path to run.
	ErrWaitlistMode = errors.New("onboarding: funnel is in waitlist mode; verification is disabled")
	// ErrTokenInvalid is returned when a presented verify token does not resolve to
	// a pending signup. It does not distinguish "unknown" from "wrong" so a probe
	// learns nothing.
	ErrTokenInvalid = errors.New("onboarding: verification token is invalid")
	// ErrTokenExpired is returned when a verify token resolved but is past expiry.
	ErrTokenExpired = errors.New("onboarding: verification token has expired")
	// ErrInvalidEmail is returned by SignUp when the address is syntactically
	// acceptable but cannot be reduced to a canonical identity (for example the
	// local part is empty after stripping a leading plus tag). The HTTP handler
	// maps it to the uniform accepted response so the signup contract stays
	// byte-identical and no 500 is emitted for odd client input.
	ErrInvalidEmail = errors.New("onboarding sign up: invalid email")
)

// OrgProvisioner is the seam that materializes the tenant isolation stack for a
// freshly verified org: in cluster mode it creates the cluster-scoped Org custom
// resource (api/v1.Org, name = org id), which the OrgReconciler turns into a
// per-org namespace (mitos-org-<id>) with its quota and default-deny policy.
//
// It is OPTIONAL: a pure dev deployment with no Kubernetes client supplies none,
// and Verify skips provisioning with a warning rather than failing the signup.
// Implementations MUST be idempotent: a re-verify (or a controller retry) calls
// Provision again with the same id, and an already-existing Org is a success, not
// an error.
type OrgProvisioner interface {
	// Provision ensures the tenant Org resource exists for orgID with the given
	// display name. It MUST treat an already-existing org as success.
	Provision(ctx context.Context, orgID, displayName string) error
}

// EmailSender is the seam that delivers the verification email. The fake
// (FakeEmailSender) is the tested default; the real SMTP/provider is a follow-up
// behind this interface. The raw token is passed so the sender can build the
// verify link, but the sender must treat it as a secret: it is never logged.
type EmailSender interface {
	// SendVerification delivers a verification message to email carrying token.
	// Implementations must not log the raw token or store it in cleartext.
	SendVerification(ctx context.Context, email, token string) error
	// SendApproved tells an allowlisted user they are in and can sign in to run
	// their first fork. It carries no secret; the email is delivered to the
	// user's inbox and is never logged.
	SendApproved(ctx context.Context, email string) error
}

// FakeEmailSender is the in-memory EmailSender used in tests. It captures the
// most recent token per email so a test can drive the verify step, standing in
// for a human clicking the link. It is NOT for production: a real sender never
// retains tokens.
type FakeEmailSender struct {
	mu       sync.Mutex
	sent     map[string]string // email -> last token sent
	approved map[string]bool   // email -> approval sent
}

// NewFakeEmailSender returns an empty fake sender.
func NewFakeEmailSender() *FakeEmailSender {
	return &FakeEmailSender{
		sent:     map[string]string{},
		approved: map[string]bool{},
	}
}

// SendVerification records the token for email so a test can retrieve it.
func (f *FakeEmailSender) SendVerification(_ context.Context, email, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[email] = token
	return nil
}

// SendApproved records that an approval email was sent to email.
func (f *FakeEmailSender) SendApproved(_ context.Context, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved[email] = true
	return nil
}

// LastToken returns the most recent token sent to email, or "" if none. This is
// a TEST helper standing in for the user reading the email.
func (f *FakeEmailSender) LastToken(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[email]
}

// Approved returns true when SendApproved has been called for email. This is a
// TEST helper so A3's test can assert an approval was sent.
func (f *FakeEmailSender) Approved(email string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approved[email]
}

// E2ETokenSink captures raw verify tokens at signup time so a QA harness can
// retrieve them without a real mailbox. It is a pure in-memory seam; raw tokens
// are NEVER written to durable storage and NEVER logged.
//
// The no-op implementation (noopE2ETokenSink) is used when MITOS_CONSOLE_E2E is
// off; MemE2ETokenSink is the QA default when the flag is on.
type E2ETokenSink interface {
	// Record stores the raw token for email. Called by the onboarding Service at
	// signup time, immediately after the email is dispatched. Implementations MUST
	// NOT log the raw token.
	Record(email, rawToken string)
	// Last returns the most recent raw token stored for email, and true. Returns
	// ("", false) when no token has been recorded for email.
	Last(email string) (string, bool)
}

// noopE2ETokenSink is the default E2ETokenSink used when MITOS_CONSOLE_E2E is
// off. All operations are no-ops so no allocation occurs in production.
type noopE2ETokenSink struct{}

func (noopE2ETokenSink) Record(_, _ string)           {}
func (noopE2ETokenSink) Last(_ string) (string, bool) { return "", false }

// MemE2ETokenSink is the in-memory E2ETokenSink for QA use. It stores the most
// recent raw token per email in process memory only. Safe for concurrent use.
type MemE2ETokenSink struct {
	mu   sync.Mutex
	last map[string]string // email -> last rawToken; never persisted
}

// NewMemE2ETokenSink returns an empty MemE2ETokenSink.
func NewMemE2ETokenSink() *MemE2ETokenSink {
	return &MemE2ETokenSink{last: map[string]string{}}
}

// Record stores rawToken for email. The raw token is never logged.
func (s *MemE2ETokenSink) Record(email, rawToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[email] = rawToken
}

// Last returns the most recent raw token stored for email.
func (s *MemE2ETokenSink) Last(email string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.last[email]
	return t, ok
}

// ucPattern is the compile-time-compiled regex for use-case slugs. A valid slug
// is lowercase alphanumeric words joined by single hyphens, up to 40 characters.
// Examples: "ai-coding", "data-pipelines", "research". Invalid slugs (wrong
// case, spaces, special chars) are silently dropped to "" rather than erroring,
// so a bad client parameter never blocks onboarding.
var ucPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validateUseCase returns s if it matches the use-case slug format (lowercase
// alphanum words joined by hyphens, max 40 chars). Returns "" otherwise.
func validateUseCase(s string) string {
	if s == "" || len(s) > 40 || !ucPattern.MatchString(s) {
		return ""
	}
	return s
}

// PendingSignup is an unverified signup awaiting email verification. It holds the
// email and the HASH of the verify token (never the raw token). Verified marks an
// already-completed signup so re-verification is idempotent rather than a second
// provisioning.
type PendingSignup struct {
	// ID is the pre-account signup id; it is the funnel subject until an account
	// exists, then the account id takes over.
	ID string
	// Email is the DELIVERY address: the original typed address after lowercasing.
	// Verification emails are sent here so the user receives mail where they expect
	// it. Never use this field for identity or dedup; use CanonicalEmail instead.
	Email string
	// CanonicalEmail is the IDENTITY: the folded form of Email with plus-tags
	// stripped (all providers) and Gmail dots removed and domain normalised to
	// gmail.com. All dedup checks, the allowlist gate, and account provisioning
	// key on this value. If empty on an in-flight record created before B1b, the
	// gate falls back to Email for back-compat.
	CanonicalEmail string
	TokenHash      string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	// Verified is set once the signup has been provisioned; a re-verify with the
	// same token then returns the same account idempotently.
	Verified bool
	// AccountID is the provisioned account id, set on first successful verify.
	AccountID string
	// UseCase is the marketing use-case slug carried from the signup page (e.g.
	// "ai-coding"). It is optional: an absent or invalid slug is stored as "".
	// The value is validated and normalised before storage; the console uses it
	// to pre-seed the welcome flow. It is never treated as a secret.
	UseCase string
	// Waitlisted is set to true when the allowlist gate rejected this signup at
	// verify time. The pending record is NOT marked Verified, so a later approve
	// and re-verify with the same token can still provision.
	Waitlisted bool
}

// WaitlistEntry records a signup captured while the funnel is in waitlist mode.
// It holds the email and the time; no provisioning happened. The real notify or
// invite flow is a follow-up.
type WaitlistEntry struct {
	Email     string
	CreatedAt time.Time
}

// PendingStore persists pending signups and waitlist entries behind a seam so the
// funnel is unit-tested without a database. The in-memory implementation
// (MemPendingStore) is the tested default; a durable store is a follow-up.
type PendingStore interface {
	// PutPending stores a pending signup, keyed by its token hash for verify lookup.
	PutPending(ctx context.Context, p PendingSignup) error
	// GetPendingByTokenHash returns the pending signup whose token hash matches, or
	// ErrPendingNotFound.
	GetPendingByTokenHash(ctx context.Context, tokenHash string) (PendingSignup, error)
	// MarkVerified records that a pending signup was provisioned, storing the
	// resulting account id, so a re-verify is idempotent.
	MarkVerified(ctx context.Context, tokenHash, accountID string) error
	// AddWaitlist appends a waitlist entry.
	AddWaitlist(ctx context.Context, e WaitlistEntry) error
	// Waitlist returns the recorded waitlist entries in append order.
	Waitlist(ctx context.Context) ([]WaitlistEntry, error)
}

// ErrPendingNotFound is returned by a PendingStore when no pending signup matches.
var ErrPendingNotFound = errors.New("onboarding: pending signup not found")

// MemPendingStore is the in-memory PendingStore. Safe for concurrent use.
type MemPendingStore struct {
	mu       sync.Mutex
	byHash   map[string]PendingSignup
	waitlist []WaitlistEntry
}

// NewMemPendingStore returns an empty in-memory pending store.
func NewMemPendingStore() *MemPendingStore {
	return &MemPendingStore{byHash: map[string]PendingSignup{}}
}

func (s *MemPendingStore) PutPending(_ context.Context, p PendingSignup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[p.TokenHash] = p
	return nil
}

func (s *MemPendingStore) GetPendingByTokenHash(_ context.Context, tokenHash string) (PendingSignup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byHash[tokenHash]
	if !ok {
		return PendingSignup{}, ErrPendingNotFound
	}
	return p, nil
}

func (s *MemPendingStore) MarkVerified(_ context.Context, tokenHash, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byHash[tokenHash]
	if !ok {
		return ErrPendingNotFound
	}
	p.Verified = true
	p.AccountID = accountID
	s.byHash[tokenHash] = p
	return nil
}

func (s *MemPendingStore) AddWaitlist(_ context.Context, e WaitlistEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitlist = append(s.waitlist, e)
	return nil
}

func (s *MemPendingStore) Waitlist(_ context.Context) ([]WaitlistEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WaitlistEntry, len(s.waitlist))
	copy(out, s.waitlist)
	return out, nil
}

// Service is the onboarding funnel. It composes the #210 AccountService (signup,
// org, first key) with the #212 credit ledger (signup credit grant) and the
// funnel instrumentation, gated by the waitlist-vs-open mode. Every external
// dependency is an interface so the whole flow is deterministic and unit-tested.
type Service struct {
	mode      Mode
	accounts  *saas.AccountService
	store     saas.Store
	pending   PendingStore
	ledger    billing.CreditLedger
	credit    billing.Money
	email     EmailSender
	provision OrgProvisioner
	events    EventRecorder
	logger    *slog.Logger
	now       func() time.Time
	idgen     func() string
	tokengen  func() (string, error)
	tokenTTL  time.Duration
	keyScopes []string
	sink      E2ETokenSink // QA seam; no-op when MITOS_CONSOLE_E2E is off
	allowlist Allowlist    // nil means allow all (community / self-host default)
}

// Option configures a Service.
type Option func(*Service)

// WithMode sets the funnel mode (waitlist vs open). Default is ModeWaitlist.
func WithMode(m Mode) Option { return func(s *Service) { s.mode = m } }

// WithSignupCredit overrides the free signup credit amount. Default is the #212
// billing.DefaultSignupCredit (coordinated with the ledger's default).
func WithSignupCredit(m billing.Money) Option { return func(s *Service) { s.credit = m } }

// WithClock sets the time source for deterministic tests.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithIDGen sets the id generator for deterministic tests.
func WithIDGen(gen func() string) Option { return func(s *Service) { s.idgen = gen } }

// WithTokenGen sets the verify-token generator for deterministic tests. The
// generated value is a secret; it is hashed before storage and never logged.
func WithTokenGen(gen func() (string, error)) Option {
	return func(s *Service) { s.tokengen = gen }
}

// WithTokenTTL sets the verify-token lifetime. Default is 24 hours.
func WithTokenTTL(d time.Duration) Option { return func(s *Service) { s.tokenTTL = d } }

// WithEventRecorder sets the funnel instrumentation sink.
func WithEventRecorder(r EventRecorder) Option { return func(s *Service) { s.events = r } }

// WithE2ETokenSink sets the QA token sink so a test harness can retrieve the
// raw verify token without a mailbox. When nil or unset, a no-op sink is used.
// This option MUST only be called when MITOS_CONSOLE_E2E is truthy.
func WithE2ETokenSink(sink E2ETokenSink) Option {
	return func(s *Service) {
		if sink != nil {
			s.sink = sink
		}
	}
}

// WithOrgProvisioner sets the tenant Org-CR provisioner. When unset, a verified
// signup creates the account and org in the store but provisions no Kubernetes
// namespace (pure dev mode); Verify logs a warning in that case.
func WithOrgProvisioner(p OrgProvisioner) Option { return func(s *Service) { s.provision = p } }

// WithAllowlist wires an Allowlist gate that Verify consults before provisioning.
// When unset (nil), Verify provisions every email that passes token validation so
// community and self-host deployments are unaffected. Do NOT default-construct a
// restrictive allowlist.
func WithAllowlist(a Allowlist) Option { return func(s *Service) { s.allowlist = a } }

// WithLogger sets the structured logger. It is used only for non-secret
// operational lines (for example the skip-provisioning warning); it never logs an
// email or a token. When unset, a discarding logger is used.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds an onboarding service. accounts and ledger are required; the
// remaining dependencies default to the in-memory tested implementations.
func NewService(accounts *saas.AccountService, store saas.Store, pending PendingStore, ledger billing.CreditLedger, email EmailSender, opts ...Option) *Service {
	s := &Service{
		mode:      ModeWaitlist,
		accounts:  accounts,
		store:     store,
		pending:   pending,
		ledger:    ledger,
		credit:    billing.DefaultSignupCredit(),
		email:     email,
		events:    nopRecorder{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       time.Now,
		idgen:     randomID,
		tokengen:  randomToken,
		tokenTTL:  24 * time.Hour,
		keyScopes: []string{saas.ScopeSandboxes},
		sink:      noopE2ETokenSink{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Mode reports the funnel mode.
func (s *Service) Mode() Mode { return s.mode }

// SignupResult is the outcome of SignUp. In waitlist mode only Waitlisted is set.
// In open mode PendingID is the funnel subject and a verify token has been sent
// by email; the raw token is NEVER returned here (it goes only to the user's
// inbox via the EmailSender).
type SignupResult struct {
	// Waitlisted is true when the funnel is in waitlist mode and the signup was
	// recorded as a waitlist entry instead of provisioning.
	Waitlisted bool
	// PendingID is the pre-account signup id (the funnel subject) in open mode.
	PendingID string
}

// SignUp begins onboarding for an email and an optional use-case slug. In
// waitlist mode it records a waitlist entry and records the waitlisted event,
// provisioning nothing. In open mode it creates a pending signup (carrying the
// validated use-case slug), sends a verify token by email, and records the
// signup_started event. It NEVER logs the email or the token. An invalid or
// absent useCase is stored as "".
func (s *Service) SignUp(ctx context.Context, email, useCase string) (SignupResult, error) {
	if email == "" {
		return SignupResult{}, ErrInvalidEmail
	}
	// Compute the canonical identity. A syntactically valid but un-canonicalizable
	// address (e.g. local part empty after plus-tag stripping) also returns
	// ErrInvalidEmail so the HTTP handler can map both cases to the uniform 202.
	canonical, ok := canonicalEmail(email)
	if !ok {
		return SignupResult{}, ErrInvalidEmail
	}
	now := s.now()

	if s.mode == ModeWaitlist {
		if err := s.pending.AddWaitlist(ctx, WaitlistEntry{Email: email, CreatedAt: now}); err != nil {
			return SignupResult{}, fmt.Errorf("onboarding sign up: record waitlist: %w", err)
		}
		// The waitlist subject keys on the canonical identity hash so the analytics
		// event carries no PII and folded variants of one identity map to one
		// subject (consistent with the open-mode waitlist event).
		s.events.Record(ctx, Event{Subject: hashString(canonical), Name: EventWaitlisted, At: now})
		return SignupResult{Waitlisted: true}, nil
	}

	// Reject a duplicate identity up front so a re-signup does not strand a second
	// pending token for a canonical address that already has an account. The check
	// uses the canonical form so that folded Gmail variants (u.ser+x@gmail.com vs
	// user@gmail.com) are treated as the same person.
	if _, err := s.store.GetAccountByEmail(ctx, canonical); err == nil {
		return SignupResult{}, saas.ErrConflict
	}

	rawToken, err := s.tokengen()
	if err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: generate token: %w", err)
	}
	pendingID := s.idgen()
	pending := PendingSignup{
		ID:             pendingID,
		Email:          email,     // delivery address: user receives mail here
		CanonicalEmail: canonical, // identity: dedup, allowlist gate, provisioning
		TokenHash:      hashString(rawToken),
		CreatedAt:      now,
		ExpiresAt:      now.Add(s.tokenTTL),
		UseCase:        validateUseCase(useCase),
	}
	if err := s.pending.PutPending(ctx, pending); err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: store pending: %w", err)
	}
	// Delivery is always to the ORIGINAL typed address so the user receives the
	// verification link where they expect it, regardless of canonicalization.
	if err := s.email.SendVerification(ctx, email, rawToken); err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: send verification: %w", err)
	}
	// QA seam: record the raw token in the E2E sink so a test harness can
	// retrieve it via the gated endpoint. The no-op sink makes this a zero-cost
	// call in production. The raw token is NEVER logged here.
	s.sink.Record(email, rawToken)
	s.events.Record(ctx, Event{Subject: pendingID, Name: EventSignupStarted, At: now})
	return SignupResult{PendingID: pendingID}, nil
}

// VerifyResult is the outcome of a successful Verify: the provisioned account and
// org, the masked first key record, and the raw first key shown EXACTLY ONCE. On
// an idempotent re-verify, Account and Org are returned but FirstKey is empty
// (the key was already issued and the raw value cannot be reproduced).
type VerifyResult struct {
	Account       saas.Account
	Org           saas.Organization
	FirstKey      saas.CreatedKey
	AlreadyDone   bool
	GrantedCredit billing.Money
	// UseCase is the marketing use-case slug carried from the pending signup (e.g.
	// "ai-coding"). It is "" when none was provided. The console uses it to route
	// the user to the relevant getting-started flow after verification.
	UseCase string
	// Waitlisted is true when an allowlist is configured and the signup email is
	// not on it. Account, Org, and FirstKey are zero; GrantedCredit is 0. The
	// pending record is marked waitlisted but NOT verified, so re-verify with the
	// same token will provision once the email is added to the allowlist.
	Waitlisted bool
}

// Verify accepts a raw verify token and, if valid and unexpired, provisions the
// account and Personal org (#210), grants the free signup credit (#212), and
// issues the first API key (#210), recording the verified and key_issued funnel
// events. Re-verifying with the same token after success is idempotent: it
// returns the same account and org with AlreadyDone set, and does NOT grant a
// second credit or issue a second key. An unknown or expired token is rejected
// with a typed error that never reveals the token.
func (s *Service) Verify(ctx context.Context, rawToken string) (VerifyResult, error) {
	if s.mode == ModeWaitlist {
		return VerifyResult{}, ErrWaitlistMode
	}
	if rawToken == "" {
		return VerifyResult{}, ErrTokenInvalid
	}
	h := hashString(rawToken)
	pending, err := s.pending.GetPendingByTokenHash(ctx, h)
	if err != nil {
		return VerifyResult{}, ErrTokenInvalid
	}

	// Idempotent re-verify: the signup is already provisioned. Return the existing
	// account and org without re-granting credit or re-issuing a key.
	if pending.Verified {
		acct, gerr := s.store.GetAccount(ctx, pending.AccountID)
		if gerr != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: load verified account: %w", gerr)
		}
		org, oerr := s.store.GetOrg(ctx, acct.PersonalOrgID)
		if oerr != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: load verified org: %w", oerr)
		}
		return VerifyResult{Account: acct, Org: org, AlreadyDone: true, UseCase: pending.UseCase}, nil
	}

	now := s.now()
	if !pending.ExpiresAt.IsZero() && !now.Before(pending.ExpiresAt) {
		return VerifyResult{}, ErrTokenExpired
	}

	// identity is the canonical form of the signup email: the stable key used for
	// the allowlist gate, event hashing, and account provisioning. Fall back to
	// pending.Email for in-flight records that pre-date B1b and therefore have an
	// empty CanonicalEmail field, so users with tokens issued before this change
	// can still complete verification. Never log the identity (PII).
	identity := pending.CanonicalEmail
	if identity == "" {
		identity = pending.Email
	}

	// Allowlist gate: consult only when an allowlist is configured. A nil allowlist
	// means allow all so community and self-host deployments are unaffected.
	// Use the canonical identity so the row an operator added via the approve
	// endpoint (also keyed on canonical) matches correctly.
	if s.allowlist != nil {
		allowed, aerr := s.allowlist.IsAllowed(ctx, identity)
		if aerr != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: allowlist check: %w", aerr)
		}
		if !allowed {
			// Mark the pending record waitlisted without setting Verified: the token
			// remains valid so a later approve + re-verify with the same token can
			// provision. PutPending is idempotent on repeated waitlist hits.
			pending.Waitlisted = true
			if perr := s.pending.PutPending(ctx, pending); perr != nil {
				return VerifyResult{}, fmt.Errorf("onboarding verify: mark waitlisted: %w", perr)
			}
			// Funnel event keyed on the identity hash so no PII enters the event stream.
			s.events.Record(ctx, Event{Subject: hashString(identity), Name: EventWaitlisted, At: now})
			return VerifyResult{Waitlisted: true}, nil
		}
	}

	// Provision: account + Personal org (#210). The account is stored under the
	// canonical identity so all folded variants of the same address share one
	// account and one signup credit. A prior verify attempt may have provisioned
	// the account but crashed before MarkVerified (the credit grant or key issue
	// below errored), leaving the identity taken while this pending signup is still
	// unverified. In that case SignUp returns ErrConflict; load the existing
	// account+org and finish onboarding idempotently rather than stranding the user
	// (the documented re-verify idempotency must hold even after a partial prior
	// attempt, not only after a fully successful one).
	acct, org, err := s.accounts.SignUp(ctx, identity)
	if errors.Is(err, saas.ErrConflict) {
		existing, gerr := s.store.GetAccountByEmail(ctx, identity)
		if gerr != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: load account after conflict: %w", gerr)
		}
		porg, oerr := s.store.GetOrg(ctx, existing.PersonalOrgID)
		if oerr != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: load org after conflict: %w", oerr)
		}
		acct, org, err = existing, porg, nil
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("onboarding verify: provision account: %w", err)
	}

	// Provision the tenant isolation stack: create the cluster-scoped Org custom
	// resource so the OrgReconciler stands up the per-org namespace (#288). This is
	// the signup -> namespace integration. In pure dev mode no provisioner is wired,
	// so skip with a warning rather than failing the signup. A provisioner that
	// errors DOES fail the verify so the user can retry it idempotently rather than
	// landing in an org with no namespace; the provisioner is required to be
	// idempotent so a retry is safe.
	if s.provision != nil {
		if err := s.provision.Provision(ctx, org.ID, org.Name); err != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: provision tenant org: %w", err)
		}
	} else {
		s.logger.Warn("onboarding verify: no org provisioner configured; skipping tenant namespace provisioning", "org", org.ID)
	}

	// Grant the free signup credit (#212). The ledger keys the grant by org id so a
	// retried verify never double-grants; here MarkVerified also blocks a re-run.
	if err := billing.GrantSignupCredit(ctx, s.ledger, org.ID, s.credit, now); err != nil && !errors.Is(err, billing.ErrDuplicateEntry) {
		return VerifyResult{}, fmt.Errorf("onboarding verify: grant signup credit: %w", err)
	}

	// Issue the first API key (#210).
	created, err := s.accounts.CreateKey(ctx, acct.ID, saas.CreateKeyRequest{
		OrgID:  org.ID,
		Name:   "default",
		Scopes: s.keyScopes,
	})
	if err != nil {
		return VerifyResult{}, fmt.Errorf("onboarding verify: issue first key: %w", err)
	}

	if err := s.pending.MarkVerified(ctx, h, acct.ID); err != nil {
		return VerifyResult{}, fmt.Errorf("onboarding verify: mark verified: %w", err)
	}

	// The funnel subject transitions from the pending id to the account id; record
	// both events under the pending id so the funnel can be followed from
	// signup_started through verified and key_issued.
	s.events.Record(ctx, Event{Subject: pending.ID, Name: EventVerified, At: now})
	s.events.Record(ctx, Event{Subject: pending.ID, Name: EventKeyIssued, At: s.now()})

	return VerifyResult{
		Account:       acct,
		Org:           org,
		FirstKey:      created,
		GrantedCredit: s.credit,
		UseCase:       pending.UseCase,
	}, nil
}

// JoinWaitlist returns the recorded waitlist entries (operator surface for the
// design-partner invite flow). The real invite/notify path is a follow-up.
func (s *Service) JoinWaitlist(ctx context.Context) ([]WaitlistEntry, error) {
	return s.pending.Waitlist(ctx)
}

// RecordFirstSandbox records that subject created its first sandbox. The gateway
// or control plane calls this the first time an org provisions a sandbox so the
// funnel can measure time-to-first-sandbox. subject is the funnel key (the
// pending signup id) so the event aligns with signup_started.
func (s *Service) RecordFirstSandbox(ctx context.Context, subject string) {
	s.events.Record(ctx, Event{Subject: subject, Name: EventFirstSandboxCreated, At: s.now()})
}

// RecordFirstExec records that subject ran code for the first time, terminating
// the time-to-first-sandbox funnel.
func (s *Service) RecordFirstExec(ctx context.Context, subject string) {
	s.events.Record(ctx, Event{Subject: subject, Name: EventFirstExec, At: s.now()})
}

// FunnelStats aggregates the recorded events into funnel statistics.
func (s *Service) FunnelStats(ctx context.Context) FunnelStats {
	return AggregateFunnel(s.events.Events(ctx))
}

// hashString returns the hex sha256 of v. Used to store a verify token as a hash
// (never the raw token) and to derive a non-PII analytics subject from an email.
func hashString(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a high-entropy url-safe verify token. The value is a
// secret: it is hashed before storage and never logged.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomID returns a short opaque url-safe id for pending signups.
func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
