package billing

import (
	"context"
	"errors"
	"sync"
	"time"
)

// EntryKind classifies a credit-ledger entry. The ledger is append-only: a
// balance is the sum of its entries, never a mutated field, so the accounting is
// auditable and cannot silently go wrong.
type EntryKind string

const (
	// KindSignupCredit is the free credit granted at signup (the $100-$200 bar).
	// It is a positive entry.
	KindSignupCredit EntryKind = "signup_credit"
	// KindTopUp is a prepaid top-up purchase (the Daytona-style top-up ladder). A
	// positive entry, added after the payment clears.
	KindTopUp EntryKind = "top_up"
	// KindUsageDrawdown is a negative entry: metered usage drawing the balance
	// down. Keyed by the (org, sandbox, window) usage key so a replayed drawdown is
	// idempotent (the ledger refuses a duplicate key).
	KindUsageDrawdown EntryKind = "usage_drawdown"
	// KindAdjustment is a manual operator credit or debit (refund, comp, clawback).
	KindAdjustment EntryKind = "adjustment"
)

// LedgerEntry is one immutable line in an org's credit ledger. Amount is signed:
// positive adds balance (credit, top-up), negative draws it down (usage). Key is
// the idempotency key for drawdowns (the usage record key) so a replayed push
// never double-debits; for grants and top-ups it is a unique grant id.
type LedgerEntry struct {
	OrgID  string
	Kind   EntryKind
	Amount Money
	Key    string
	At     time.Time
	// Note is a non-secret, human-legible reason; it NEVER carries a payment
	// secret or a card detail.
	Note string
}

// ErrDuplicateEntry is returned when an entry with an existing idempotency key
// is appended. It makes the drawdown idempotent: replaying the same usage record
// is a no-op, never a second debit.
var ErrDuplicateEntry = errors.New("billing: ledger entry with this key already exists")

// CreditLedger is the per-org append-only credit ledger. The in-memory
// implementation is the tested default; the durable implementation is
// pgstore.PgCreditLedger. Balance is always the signed sum of entries, so the
// ledger cannot drift. It NEVER stores a payment secret.
//
// The ledger also owns each org's carried DRAWDOWN REMAINDER (issue #662): the
// signed sub-cent milli-cent balance left over after the accumulator settles
// whole cents. It lives on the ledger, not in a separate store, so an
// implementation can commit a debit and its remainder in ONE atomic step
// (AppendWithRemainder); with two stores a crash between the writes would skew
// the carry.
type CreditLedger interface {
	// Append adds an entry. If an entry with the same non-empty Key already exists
	// for the org, it returns ErrDuplicateEntry and changes nothing (idempotency).
	Append(ctx context.Context, e LedgerEntry) error
	// Balance returns the org's current balance: the signed sum of its entries. A
	// positive balance is remaining prepaid credit; the ledger never goes negative
	// because the drawdown driver caps each debit at the available balance.
	Balance(ctx context.Context, orgID string) (Money, error)
	// Entries returns the org's entries in append order, for audit and statements.
	Entries(ctx context.Context, orgID string) ([]LedgerEntry, error)
	// Remainder returns the org's carried drawdown remainder in milli-cents. An
	// org with no recorded remainder reads as 0. The value is signed: the
	// round-half-up settle in Service.Drawdown keeps it in (-500, 500); a
	// negative value means the org prepaid a sub-cent that offsets future usage.
	Remainder(ctx context.Context, orgID string) (int64, error)
	// AppendWithRemainder appends e and sets the org's drawdown remainder in one
	// ATOMIC step: on ErrDuplicateEntry (the replayed-window case) NEITHER the
	// entries nor the remainder change, so a replayed drawdown cannot
	// double-count the carry, and no crash can land the debit without its
	// remainder (or vice versa).
	AppendWithRemainder(ctx context.Context, e LedgerEntry, remainderMilliCents int64) error
}

// MemCreditLedger is the in-memory CreditLedger. Safe for concurrent use.
type MemCreditLedger struct {
	mu        sync.Mutex
	byOrg     map[string][]LedgerEntry
	seenKey   map[string]bool  // org+"\x00"+key -> exists, the idempotency guard.
	remainder map[string]int64 // org -> carried drawdown remainder, milli-cents.
}

// NewMemCreditLedger returns an empty ledger.
func NewMemCreditLedger() *MemCreditLedger {
	return &MemCreditLedger{byOrg: map[string][]LedgerEntry{}, seenKey: map[string]bool{}, remainder: map[string]int64{}}
}

func (l *MemCreditLedger) Append(_ context.Context, e LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(e)
}

// appendLocked is Append under an already-held mutex, shared by Append and
// AppendWithRemainder so the duplicate-key check has exactly one home.
func (l *MemCreditLedger) appendLocked(e LedgerEntry) error {
	if e.Key != "" {
		k := e.OrgID + "\x00" + e.Key
		if l.seenKey[k] {
			return ErrDuplicateEntry
		}
		l.seenKey[k] = true
	}
	l.byOrg[e.OrgID] = append(l.byOrg[e.OrgID], e)
	return nil
}

func (l *MemCreditLedger) Remainder(_ context.Context, orgID string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remainder[orgID], nil
}

// AppendWithRemainder appends e and sets the org's drawdown remainder under one
// mutex hold: a duplicate key changes nothing, so a replayed drawdown can
// neither double-debit nor double-count the carry.
func (l *MemCreditLedger) AppendWithRemainder(_ context.Context, e LedgerEntry, remainderMilliCents int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(e); err != nil {
		return err
	}
	l.remainder[e.OrgID] = remainderMilliCents
	return nil
}

func (l *MemCreditLedger) Balance(_ context.Context, orgID string) (Money, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var bal Money
	for _, e := range l.byOrg[orgID] {
		bal += e.Amount
	}
	return bal, nil
}

func (l *MemCreditLedger) Entries(_ context.Context, orgID string) ([]LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LedgerEntry, len(l.byOrg[orgID]))
	copy(out, l.byOrg[orgID])
	return out, nil
}

// GrantSignupCredit appends the free signup credit for a new org. It is keyed by
// a stable per-org grant id so a re-run (a retried signup) never grants twice.
func GrantSignupCredit(ctx context.Context, l CreditLedger, orgID string, amount Money, now time.Time) error {
	return l.Append(ctx, LedgerEntry{
		OrgID:  orgID,
		Kind:   KindSignupCredit,
		Amount: amount,
		Key:    "signup:" + orgID,
		At:     now,
		Note:   "signup credit",
	})
}

// TopUp appends a prepaid top-up after its payment has cleared. ref is the
// payment reference (a Stripe payment-intent id, NOT a secret) used as the
// idempotency key so a webhook redelivery of the same payment does not add the
// balance twice.
func TopUp(ctx context.Context, l CreditLedger, orgID string, amount Money, ref string, now time.Time) error {
	return l.Append(ctx, LedgerEntry{
		OrgID:  orgID,
		Kind:   KindTopUp,
		Amount: amount,
		Key:    "topup:" + ref,
		At:     now,
		Note:   "prepaid top-up",
	})
}

// TopUpLadder is the Daytona-style prepaid top-up ladder: the offered top-up
// amounts. These are ILLUSTRATIVE and CONFIGURABLE, not published prices.
func TopUpLadder() []Money {
	return []Money{USD(10), USD(25), USD(50), USD(100), USD(250)}
}

// DefaultSignupCredit is the illustrative free signup credit ($100), within the
// $100-$200 bar in the issue. Configurable per deployment; not a published
// promise.
func DefaultSignupCredit() Money { return USD(100) }
