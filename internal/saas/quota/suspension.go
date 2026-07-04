package quota

import (
	"context"
	"sync"
	"time"
)

// SuspensionReason classifies why an org was suspended, for audit and for the
// manual-review hook. It is never a secret and is safe to log.
type SuspensionReason string

const (
	// ReasonManual is an operator-initiated suspension (the manual review hook).
	ReasonManual SuspensionReason = "manual"
	// ReasonAbuseSignal is an automated suspension fired by an abuse signal (for
	// example a sudden egress spike or a crypto-miner heuristic). The signal source
	// is a documented seam (AbuseSignal); this slice ships the suspend mechanism and
	// the seam, not the detectors.
	ReasonAbuseSignal SuspensionReason = "abuse_signal"
	// ReasonEmergencyStop is a pool-wide or org-wide emergency stop (the big red
	// button): suspend everything now, ask questions later.
	ReasonEmergencyStop SuspensionReason = "emergency_stop"
	// ReasonSpendCap is an automated suspension fired when an org crosses its hard
	// spend cap (issue #212), so a runaway agent cannot generate an unbounded bill.
	// It carries a manual hold: the org is not auto-lifted back into the same bill.
	ReasonSpendCap SuspensionReason = "spend_cap"
	// ReasonDunning is a suspension fired when dunning exhausts its payment-retry
	// budget (issue #212): the org's charges keep failing, so it fails closed until
	// a payment succeeds.
	ReasonDunning SuspensionReason = "dunning"
)

// Suspension records that an org is suspended: when, why, and a non-secret note.
// A suspended org fails closed everywhere: its keys are rejected at the gateway
// (the kill-switch verb revokes/freezes), new claims are denied by the enforcer,
// and its running sandboxes are frozen by the control plane (the freeze action is
// a documented seam; this package owns the decision, not the VM operation).
type Suspension struct {
	OrgID      string
	Reason     SuspensionReason
	Note       string
	At         time.Time
	ManualHold bool // set when a human must review before lifting.
}

// SuspensionStore is the pluggable record of suspended orgs. The in-memory
// implementation is the tested default; a durable store is a follow-up behind the
// same interface. It NEVER holds a key value; suspension is keyed by org id only.
type SuspensionStore interface {
	// Suspend marks the org suspended. It is idempotent: re-suspending updates the
	// reason and note but keeps the first suspension time.
	Suspend(ctx context.Context, s Suspension) error
	// Lift clears an org's suspension. It returns whether the org was suspended. A
	// suspension with ManualHold set may only be lifted by Lift (the manual-review
	// hook), never automatically.
	Lift(ctx context.Context, orgID string) (bool, error)
	// IsSuspended reports whether the org is currently suspended and, if so, the
	// record.
	IsSuspended(ctx context.Context, orgID string) (Suspension, bool, error)
}

// MemSuspensionStore is the in-memory SuspensionStore. Safe for concurrent use.
type MemSuspensionStore struct {
	mu  sync.RWMutex
	out map[string]Suspension
}

// NewMemSuspensionStore returns an empty store.
func NewMemSuspensionStore() *MemSuspensionStore {
	return &MemSuspensionStore{out: map[string]Suspension{}}
}

func (s *MemSuspensionStore) Suspend(_ context.Context, sus Suspension) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.out[sus.OrgID]; ok {
		sus.At = prev.At // keep the original suspension time.
	}
	s.out[sus.OrgID] = sus
	return nil
}

func (s *MemSuspensionStore) Lift(_ context.Context, orgID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.out[orgID]
	delete(s.out, orgID)
	return ok, nil
}

func (s *MemSuspensionStore) IsSuspended(_ context.Context, orgID string) (Suspension, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sus, ok := s.out[orgID]
	return sus, ok, nil
}

// AbuseSignal is the seam an automated abuse detector implements to drive
// automated suspension. The KillSwitch polls or is notified by signals; this
// slice ships the seam and the suspend mechanism it triggers, not the detectors
// themselves (egress-rate heuristics, crypto-miner fingerprints, and the
// reputation feed are documented follow-ups).
type AbuseSignal interface {
	// FiredOrgs returns the org ids the signal currently flags for suspension and a
	// human-legible, non-secret reason per org.
	FiredOrgs(ctx context.Context) (map[string]string, error)
}

// KillSwitch is the control logic for org suspension and the emergency stop. It
// is the kill-switch verb (issue #36, folded into #213): suspend an org so its
// keys fail closed and new claims are rejected; lift a suspension after manual
// review; and stop a whole set of orgs at once (the pool-wide / org-wide
// emergency stop). It owns the DECISION; the VM-freeze and key-revoke effects are
// driven through the gateway's existing key-verify path (a suspended org's keys
// are rejected) and a documented freeze seam.
//
// Policy basis: a suspension under ReasonAbuseSignal or ReasonEmergencyStop
// enforces the technical floor of the Acceptable Use Policy
// (docs/legal/acceptable-use-policy.md). The AUP is the human-readable policy a
// suspension acts on; this type is the runtime mechanism that makes it effective.
type KillSwitch struct {
	store SuspensionStore
	now   func() time.Time
}

// NewKillSwitch builds a kill switch over a suspension store. A nil clock
// defaults to time.Now.
func NewKillSwitch(store SuspensionStore, now func() time.Time) *KillSwitch {
	if now == nil {
		now = time.Now
	}
	return &KillSwitch{store: store, now: now}
}

// Suspend suspends one org. After this returns, the enforcer denies the org's
// requests and IsSuspended reports it; the gateway's key verify (wired via the
// enforcer's pre-forward check) rejects the org's keys so they fail closed.
func (k *KillSwitch) Suspend(ctx context.Context, orgID string, reason SuspensionReason, note string, manualHold bool) error {
	return k.store.Suspend(ctx, Suspension{
		OrgID:      orgID,
		Reason:     reason,
		Note:       note,
		At:         k.now(),
		ManualHold: manualHold,
	})
}

// Lift clears an org's suspension (the manual-review hook). It returns whether
// the org had been suspended.
func (k *KillSwitch) Lift(ctx context.Context, orgID string) (bool, error) {
	return k.store.Lift(ctx, orgID)
}

// LiftReason is the reason-scoped AUTOMATED lift (issue #615): it clears the
// org's suspension only when the current suspension carries exactly this
// reason AND no manual hold. The billing recovery paths use it (a paid top-up
// lifts spend_cap, a payment-recovered subscription lifts dunning), so a
// recovery event can never lift a suspension it does not own: an abuse or
// emergency-stop suspension, or ANY suspension held for human review, survives
// every automated lift and clears only through Lift (the operator hook). It
// returns whether a suspension was lifted; an unsuspended org is a no-op.
func (k *KillSwitch) LiftReason(ctx context.Context, orgID string, reason SuspensionReason) (bool, error) {
	sus, suspended, err := k.store.IsSuspended(ctx, orgID)
	if err != nil {
		return false, err
	}
	if !suspended || sus.Reason != reason || sus.ManualHold {
		return false, nil
	}
	return k.store.Lift(ctx, orgID)
}

// EmergencyStop suspends every org in orgs at once with ReasonEmergencyStop and a
// manual hold, so none lifts automatically. This is the pool-wide / org-wide big
// red button. It returns the number of orgs newly suspended.
func (k *KillSwitch) EmergencyStop(ctx context.Context, orgs []string, note string) (int, error) {
	n := 0
	for _, orgID := range orgs {
		if err := k.store.Suspend(ctx, Suspension{
			OrgID:      orgID,
			Reason:     ReasonEmergencyStop,
			Note:       note,
			At:         k.now(),
			ManualHold: true,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ProcessSignals suspends every org a signal currently flags (the automated
// suspension path). Each automated suspension carries ManualHold so a human must
// review before it is lifted. It returns the org ids it suspended.
func (k *KillSwitch) ProcessSignals(ctx context.Context, signal AbuseSignal) ([]string, error) {
	fired, err := signal.FiredOrgs(ctx)
	if err != nil {
		return nil, err
	}
	var suspended []string
	for orgID, reason := range fired {
		if err := k.store.Suspend(ctx, Suspension{
			OrgID:      orgID,
			Reason:     ReasonAbuseSignal,
			Note:       reason,
			At:         k.now(),
			ManualHold: true,
		}); err != nil {
			return suspended, err
		}
		suspended = append(suspended, orgID)
	}
	return suspended, nil
}
