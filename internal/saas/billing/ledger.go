package billing

import (
	"context"
	"errors"
	"fmt"
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
	// KindBoxGrant is the monthly usage credit granted for a reserved Box
	// capacity purchase (see reservation.go's ApplyMonthlyGrant). A positive
	// entry, keyed per (org, month, box) so re-running the monthly grant job
	// never double-credits the same org's same box in the same month.
	KindBoxGrant EntryKind = "box_grant"
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

// ScopedLedgerReader is the OPTIONAL time-scoped read a ledger can implement
// so period-bounded evaluations (the drawdown driver's spend-cap check) read
// one month of entries instead of the org's lifetime history. Both the
// in-memory and the Postgres ledgers implement it (the Postgres read is
// indexed on (org_id, at), migration 0012); the spend-cap evaluation falls
// back to the full Entries scan for a ledger that does not.
type ScopedLedgerReader interface {
	// EntriesSince returns the org's entries with At at or after since, in
	// append order.
	EntriesSince(ctx context.Context, orgID string, since time.Time) ([]LedgerEntry, error)
}

// ErrDuplicateEntry is returned when an entry with an existing idempotency key
// is appended, or when a settle's processed-window marker already exists. It
// makes the drawdown idempotent: replaying the same usage record is a no-op,
// never a second debit.
var ErrDuplicateEntry = errors.New("billing: ledger entry with this key already exists")

// ProcessedWindow identifies one settled usage window: the (org, sandbox,
// window) key of a usage record the drawdown has already priced and settled.
// Markers live NEXT TO the ledger (a table in the durable implementation, a
// map in the in-memory one), never IN it, so a zero-cent settle no longer
// writes a customer-visible zero-amount ledger row (issue #672). A marker is
// prunable once its window falls out of the drawdown lookback horizon: the
// driver never lists that window again, so the marker has nothing to guard.
type ProcessedWindow struct {
	OrgID     string
	SandboxID string
	Window    time.Time
}

// Key returns the marker's drawdown idempotency key, identical to the ledger
// key the same window's debit carries, so the two dedup mechanisms compare
// under one canonical form.
func (w ProcessedWindow) Key() string {
	return DrawdownKey(w.OrgID, w.SandboxID, w.Window)
}

// DrawdownKey is the canonical (org, sandbox, window) drawdown idempotency
// key. It keys both the credit-ledger debit row and the processed-window
// marker, and the drawdown driver computes it per listed record to skip
// already-settled windows before pricing.
func DrawdownKey(orgID, sandboxID string, window time.Time) string {
	return fmt.Sprintf("drawdown:%s|%s|%s", orgID, sandboxID, window.UTC().Format(time.RFC3339Nano))
}

// CreditLedger is the per-org append-only credit ledger. The in-memory
// implementation is the tested default; the durable implementation is
// pgstore.PgCreditLedger. Balance is always the signed sum of entries, so the
// ledger cannot drift. It NEVER stores a payment secret.
//
// The ledger also owns each org's carried DRAWDOWN REMAINDER (issue #662): the
// signed sub-cent milli-cent balance left over after the accumulator settles
// whole cents, and the PROCESSED-WINDOW markers (issue #672) recording which
// usage windows have already settled. Both live on the ledger, not in separate
// stores, so an implementation can commit a debit, its remainder, and its
// marker in ONE atomic step (SettleWindow); with separate stores a crash
// between the writes would skew the carry or drop the marker.
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
	// SettleWindow commits one drawdown settle in one ATOMIC step: the
	// processed-window marker for w, the org's new drawdown remainder, and,
	// ONLY when e.Amount is nonzero, the ledger entry e (a zero-cent settle
	// marks the window and advances the carry without a customer-visible
	// zero-amount ledger row, issue #672). If the marker for w already exists,
	// or e.Amount is nonzero and e.Key already exists, it returns
	// ErrDuplicateEntry and NOTHING changes, so a replayed drawdown can neither
	// double-debit nor double-count the carry, and no crash can land any one of
	// the three writes without the others. This is the issue #666
	// AppendWithRemainder single-transaction path, extended with the marker.
	SettleWindow(ctx context.Context, e LedgerEntry, remainderMilliCents int64, w ProcessedWindow) error
	// SettledWindowKeys returns the set of drawdown idempotency keys
	// (DrawdownKey form) already settled for the org since the given instant:
	// the union of the processed-window markers (by window time) and the
	// ledger's keyed usage_drawdown entries (by settle time). The ledger half
	// exists for BACKWARD COMPATIBILITY with windows settled before the marker
	// table existed (their only trace is the keyed ledger row) and is removable
	// once one lookback horizon has passed since that deploy; every settle
	// since writes a marker. A drawdown's settle time is always after its
	// window closes, so filtering ledger entries on At >= since never hides a
	// row whose window is inside [since, now).
	SettledWindowKeys(ctx context.Context, orgID string, since time.Time) (map[string]bool, error)
	// PruneProcessedWindows deletes processed-window markers whose window is
	// before olderThan and returns how many were removed. The drawdown driver
	// calls it with the start of its lookback: a window that old is never
	// listed again, so its marker guards nothing and the marker set stays
	// bounded.
	PruneProcessedWindows(ctx context.Context, olderThan time.Time) (int64, error)
}

// MemCreditLedger is the in-memory CreditLedger. Safe for concurrent use.
type MemCreditLedger struct {
	mu        sync.Mutex
	byOrg     map[string][]LedgerEntry
	seenKey   map[string]bool  // org+"\x00"+key -> exists, the idempotency guard.
	remainder map[string]int64 // org -> carried drawdown remainder, milli-cents.
	// processed holds the processed-window markers: org -> drawdown key ->
	// window time (kept for range filtering and pruning).
	processed map[string]map[string]time.Time
}

// NewMemCreditLedger returns an empty ledger.
func NewMemCreditLedger() *MemCreditLedger {
	return &MemCreditLedger{
		byOrg:     map[string][]LedgerEntry{},
		seenKey:   map[string]bool{},
		remainder: map[string]int64{},
		processed: map[string]map[string]time.Time{},
	}
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

// SettleWindow writes the processed-window marker, the remainder, and (when
// e.Amount is nonzero) the entry under one mutex hold: a duplicate marker or a
// duplicate entry key changes nothing, so a replayed drawdown can neither
// double-debit nor double-count the carry.
func (l *MemCreditLedger) SettleWindow(_ context.Context, e LedgerEntry, remainderMilliCents int64, w ProcessedWindow) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := w.Key()
	if _, dup := l.processed[w.OrgID][key]; dup {
		return ErrDuplicateEntry
	}
	if e.Amount != 0 {
		if err := l.appendLocked(e); err != nil {
			return err
		}
	}
	if l.processed[w.OrgID] == nil {
		l.processed[w.OrgID] = map[string]time.Time{}
	}
	l.processed[w.OrgID][key] = w.Window
	l.remainder[e.OrgID] = remainderMilliCents
	return nil
}

// SettledWindowKeys returns the org's settled drawdown keys since the given
// instant: markers by window time, plus keyed usage_drawdown entries by settle
// time (the backward-compatibility half for pre-marker settles).
func (l *MemCreditLedger) SettledWindowKeys(_ context.Context, orgID string, since time.Time) (map[string]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for key, window := range l.processed[orgID] {
		if !window.Before(since) {
			out[key] = true
		}
	}
	for _, e := range l.byOrg[orgID] {
		if e.Kind == KindUsageDrawdown && e.Key != "" && !e.At.Before(since) {
			out[e.Key] = true
		}
	}
	return out, nil
}

// PruneProcessedWindows removes markers whose window is before olderThan.
func (l *MemCreditLedger) PruneProcessedWindows(_ context.Context, olderThan time.Time) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for org, keys := range l.processed {
		for key, window := range keys {
			if window.Before(olderThan) {
				delete(keys, key)
				n++
			}
		}
		if len(keys) == 0 {
			delete(l.processed, org)
		}
	}
	return n, nil
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

// EntriesSince implements ScopedLedgerReader: the org's entries at or after
// since, in append order.
func (l *MemCreditLedger) EntriesSince(_ context.Context, orgID string, since time.Time) ([]LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []LedgerEntry
	for _, e := range l.byOrg[orgID] {
		if !e.At.Before(since) {
			out = append(out, e)
		}
	}
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
