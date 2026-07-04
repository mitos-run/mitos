package usage

import (
	"sync"
	"time"
)

// This file carries the claim-release half of the issue #682 (was #664) fix:
// presence between the LAST scrape and terminate was never recorded, so a
// sandbox alive ~100s billed 60 vcpu-seconds and a sub-minute sandbox billed
// nothing. The controller records a Termination at claim release (it knows the
// exact instant); the HuskSource turns it into a FINAL sample on the next
// cycle, closing the half-open window through the ordinary Integrate path so
// every idempotency and hold-then-gap property keeps applying.

// Termination is one claim-release event for a husk-pod sandbox, recorded by
// the controller's terminate paths. All fields come from the CONTROL PLANE
// (pod name, trusted controller-stamped labels, claim status), never from
// anything the pod reported; it carries ids and instants only, no secrets.
type Termination struct {
	// VMID is the husk pod name, the id the pod's own metering report samples
	// are keyed on (and the key the source's last-sample memory uses).
	VMID string
	// APIID is the customer-visible sandbox id (the claiming Sandbox's name,
	// the mitos.run/claim label value; issue #663). Empty falls back to VMID.
	APIID string
	// OrgID is the owning org from the TRUSTED mitos.run/org pod label. Empty
	// means unattributed (self-host single-tenant): never billable, ignored.
	OrgID string
	// StartedAt is the claim's start time (Status.StartedAt); zero if unknown.
	// It only matters for a sandbox that terminated before its first scrape.
	StartedAt time.Time
	// At is the release/terminate instant.
	At time.Time
}

// terminationLogCap bounds the pending buffer (boring failure behavior: a
// stalled collector must not grow the controller heap without limit). On
// overflow the OLDEST events are dropped first: the most recent terminations
// are the ones the next cycle can still bill accurately, and losing a tail
// sample only ever under-bills (customer-favorable), never double-bills.
const terminationLogCap = 4096

// TerminationLog is the small thread-safe queue between the controller's
// terminate paths (producers, reconciler goroutines) and the HuskSource's
// Collect (the single consumer). It is in-memory only: a controller restart
// loses pending events, which under-bills the affected tails and nothing else.
// All methods are nil-safe so the self-host path (collector off, no log wired)
// costs nothing and needs no call-site checks.
type TerminationLog struct {
	mu      sync.Mutex
	pending []Termination
}

// NewTerminationLog builds an empty termination log.
func NewTerminationLog() *TerminationLog { return &TerminationLog{} }

// Record appends one termination event. Nil-safe no-op. At the cap the oldest
// pending event is dropped (see terminationLogCap).
func (l *TerminationLog) Record(t Termination) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= terminationLogCap {
		copy(l.pending, l.pending[1:])
		l.pending = l.pending[:len(l.pending)-1]
	}
	l.pending = append(l.pending, t)
}

// Drain returns and clears the pending events. Nil-safe (returns nil).
func (l *TerminationLog) Drain() []Termination {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.pending
	l.pending = nil
	return out
}
