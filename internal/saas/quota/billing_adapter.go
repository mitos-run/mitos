package quota

import "context"

// BillingSuspender adapts a KillSwitch to the billing.Suspender seam (issue
// #212) so a breached hard spend cap or an exhausted dunning sequence suspends
// the org through the SAME kill-switch the abuse controls use. It lives in this
// package (not billing) so billing does not import quota: billing depends only
// on its own narrow Suspender interface, and this adapter satisfies it. The
// reason string from billing is mapped to a SuspensionReason; an unrecognized
// reason falls back to ReasonManual so the suspension is never silently dropped.
type BillingSuspender struct {
	ks *KillSwitch
}

// NewBillingSuspender wraps a KillSwitch as a billing.Suspender.
func NewBillingSuspender(ks *KillSwitch) *BillingSuspender {
	return &BillingSuspender{ks: ks}
}

// Suspend implements the billing.Suspender seam by driving the kill-switch. A
// spend-cap or dunning suspension carries the supplied manualHold so a runaway
// agent's org is held for human review and not auto-lifted back into the same
// bill. reason and note are non-secret.
func (b *BillingSuspender) Suspend(ctx context.Context, orgID, reason, note string, manualHold bool) error {
	var r SuspensionReason
	switch reason {
	case "spend_cap":
		r = ReasonSpendCap
	case "dunning":
		r = ReasonDunning
	default:
		r = ReasonManual
	}
	return b.ks.Suspend(ctx, orgID, r, note, manualHold)
}
