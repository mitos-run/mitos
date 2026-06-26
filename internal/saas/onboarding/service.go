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
}

// FakeEmailSender is the in-memory EmailSender used in tests. It captures the
// most recent token per email so a test can drive the verify step, standing in
// for a human clicking the link. It is NOT for production: a real sender never
// retains tokens.
type FakeEmailSender struct {
	mu   sync.Mutex
	sent map[string]string // email -> last token sent
}

// NewFakeEmailSender returns an empty fake sender.
func NewFakeEmailSender() *FakeEmailSender {
	return &FakeEmailSender{sent: map[string]string{}}
}

// SendVerification records the token for email so a test can retrieve it.
func (f *FakeEmailSender) SendVerification(_ context.Context, email, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[email] = token
	return nil
}

// LastToken returns the most recent token sent to email, or "" if none. This is
// a TEST helper standing in for the user reading the email.
func (f *FakeEmailSender) LastToken(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[email]
}

// PendingSignup is an unverified signup awaiting email verification. It holds the
// email and the HASH of the verify token (never the raw token). Verified marks an
// already-completed signup so re-verification is idempotent rather than a second
// provisioning.
type PendingSignup struct {
	// ID is the pre-account signup id; it is the funnel subject until an account
	// exists, then the account id takes over.
	ID        string
	Email     string
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Verified is set once the signup has been provisioned; a re-verify with the
	// same token then returns the same account idempotently.
	Verified bool
	// AccountID is the provisioned account id, set on first successful verify.
	AccountID string
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

// WithOrgProvisioner sets the tenant Org-CR provisioner. When unset, a verified
// signup creates the account and org in the store but provisions no Kubernetes
// namespace (pure dev mode); Verify logs a warning in that case.
func WithOrgProvisioner(p OrgProvisioner) Option { return func(s *Service) { s.provision = p } }

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

// SignUp begins onboarding for an email. In waitlist mode it records a waitlist
// entry and records the waitlisted event, provisioning nothing. In open mode it
// creates a pending signup, sends a verify token by email, and records the
// signup_started event. It NEVER logs the email or the token.
func (s *Service) SignUp(ctx context.Context, email string) (SignupResult, error) {
	if email == "" {
		return SignupResult{}, fmt.Errorf("onboarding sign up: email is required")
	}
	now := s.now()

	if s.mode == ModeWaitlist {
		if err := s.pending.AddWaitlist(ctx, WaitlistEntry{Email: email, CreatedAt: now}); err != nil {
			return SignupResult{}, fmt.Errorf("onboarding sign up: record waitlist: %w", err)
		}
		// The waitlist subject keys on the email hash so the analytics event carries
		// no PII; the same hash deterministically tracks the entry without storing
		// the address in the event stream.
		s.events.Record(ctx, Event{Subject: hashString(email), Name: EventWaitlisted, At: now})
		return SignupResult{Waitlisted: true}, nil
	}

	// Reject a duplicate email up front so a re-signup does not strand a second
	// pending token for an address that already has an account.
	if _, err := s.store.GetAccountByEmail(ctx, email); err == nil {
		return SignupResult{}, saas.ErrConflict
	}

	rawToken, err := s.tokengen()
	if err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: generate token: %w", err)
	}
	pendingID := s.idgen()
	pending := PendingSignup{
		ID:        pendingID,
		Email:     email,
		TokenHash: hashString(rawToken),
		CreatedAt: now,
		ExpiresAt: now.Add(s.tokenTTL),
	}
	if err := s.pending.PutPending(ctx, pending); err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: store pending: %w", err)
	}
	if err := s.email.SendVerification(ctx, email, rawToken); err != nil {
		return SignupResult{}, fmt.Errorf("onboarding sign up: send verification: %w", err)
	}
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
		return VerifyResult{Account: acct, Org: org, AlreadyDone: true}, nil
	}

	now := s.now()
	if !pending.ExpiresAt.IsZero() && !now.Before(pending.ExpiresAt) {
		return VerifyResult{}, ErrTokenExpired
	}

	// Provision: account + Personal org (#210). A prior verify attempt may have
	// provisioned the account but crashed before MarkVerified (the credit grant or
	// key issue below errored), leaving the email taken while this pending signup
	// is still unverified. In that case SignUp returns ErrConflict; load the
	// existing account+org and finish onboarding idempotently rather than stranding
	// the user (the documented re-verify idempotency must hold even after a partial
	// prior attempt, not only after a fully successful one).
	acct, org, err := s.accounts.SignUp(ctx, pending.Email)
	if errors.Is(err, saas.ErrConflict) {
		existing, gerr := s.store.GetAccountByEmail(ctx, pending.Email)
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
