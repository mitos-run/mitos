package billing

// BillingStatus is the org's billing standing: the state in the dunning state
// machine. It is the single field the dunning transitions and the webhook
// handler move. It is never a secret and is safe to log.
type BillingStatus string

const (
	// StatusActive: the org is in good standing; payments are succeeding.
	StatusActive BillingStatus = "active"
	// StatusPastDue: a payment failed and the org is in the dunning retry window.
	// The org keeps acting (a grace period) while retries are attempted.
	StatusPastDue BillingStatus = "past_due"
	// StatusSuspended: dunning exhausted its retries (or a spend cap fired); the
	// org is suspended via the #213 kill-switch and fails closed everywhere.
	StatusSuspended BillingStatus = "suspended"
)

// DunningEvent drives a transition in the dunning state machine. The machine is
// deliberately small and total: every (state, event) pair has a defined next
// state, so a failed-payment path can never land in an undefined status.
type DunningEvent string

const (
	// EventPaymentSucceeded: a charge cleared. From any state this recovers to
	// active (a past-due org that pays is active again; a suspended org that pays
	// recovers, since the human-review hold is the kill-switch's concern, see
	// docs/saas/pricing.md).
	EventPaymentSucceeded DunningEvent = "payment_succeeded"
	// EventPaymentFailed: a charge failed. From active this moves to past_due
	// (enter the retry window). From past_due it stays past_due (still retrying)
	// until retries are exhausted, which the caller signals with EventRetriesExhausted.
	EventPaymentFailed DunningEvent = "payment_failed"
	// EventRetriesExhausted: the dunning retry budget is spent; suspend.
	EventRetriesExhausted DunningEvent = "retries_exhausted"
	// EventSuspend: an explicit suspend (a spend cap fired, or an operator forces
	// it). Moves to suspended from any state.
	EventSuspend DunningEvent = "suspend"
)

// NextStatus is the dunning state machine: a pure, total transition function.
// It is pure so the transitions are exhaustively unit-tested with no side
// effects; the SIDE effects (suspend via the #213 kill-switch, alert hooks) are
// applied by the billing service AFTER it computes the next status from this
// function, never inside it.
//
// Transition table:
//
//	(active,    payment_succeeded)  -> active
//	(active,    payment_failed)     -> past_due      (enter dunning)
//	(active,    suspend)            -> suspended
//	(past_due,  payment_succeeded)  -> active        (recovery)
//	(past_due,  payment_failed)     -> past_due      (keep retrying)
//	(past_due,  retries_exhausted)  -> suspended
//	(past_due,  suspend)            -> suspended
//	(suspended, payment_succeeded)  -> active        (recovery after pay)
//	(suspended, *)                  -> suspended      (stay closed)
func NextStatus(s BillingStatus, ev DunningEvent) BillingStatus {
	switch ev {
	case EventPaymentSucceeded:
		// A successful payment always recovers the org to active. Whether a
		// human-review hold on the kill-switch blocks the actual un-suspend is the
		// kill-switch's concern; the billing STATUS reflects payment standing.
		return StatusActive
	case EventSuspend, EventRetriesExhausted:
		return StatusSuspended
	case EventPaymentFailed:
		switch s {
		case StatusActive:
			return StatusPastDue
		case StatusPastDue:
			return StatusPastDue
		case StatusSuspended:
			return StatusSuspended
		}
	}
	// Unknown event: do not move (fail-safe; never silently change standing).
	return s
}
