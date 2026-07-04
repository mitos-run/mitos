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

// Suspend implements the billing.Suspender seam by driving the kill-switch.
// manualHold is caller-supplied: the automated billing paths pass false so the
// matching recovery event (a paid top-up for spend_cap, payment recovery for
// dunning) can lift via LiftReason; a held suspension is an operator decision.
// reason and note are non-secret.
func (b *BillingSuspender) Suspend(ctx context.Context, orgID, reason, note string, manualHold bool) error {
	r, ok := billingReason(reason)
	if !ok {
		r = ReasonManual
	}
	return b.ks.Suspend(ctx, orgID, r, note, manualHold)
}

// LiftReason implements the billing.SuspensionLifter seam: the reason-scoped
// automated lift for billing recovery events. An unknown reason string lifts
// NOTHING (it maps to no billing-owned quota reason; ReasonManual is the
// operator's, never billing's to lift). Manual holds survive; see
// KillSwitch.LiftReason.
func (b *BillingSuspender) LiftReason(ctx context.Context, orgID, reason string) (bool, error) {
	r, ok := billingReason(reason)
	if !ok {
		return false, nil
	}
	return b.ks.LiftReason(ctx, orgID, r)
}

// billingReason maps the billing seam's reason strings to the quota reasons
// billing owns. Anything else is not billing's (ok=false).
func billingReason(reason string) (SuspensionReason, bool) {
	switch reason {
	case "spend_cap":
		return ReasonSpendCap, true
	case "dunning":
		return ReasonDunning, true
	default:
		return "", false
	}
}
