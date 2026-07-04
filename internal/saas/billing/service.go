package billing

import (
	"context"
	"fmt"
	"time"

	"mitos.run/mitos/internal/usage"
)

// Suspender is the narrow seam the billing service drives the #213 kill-switch
// through. quota.KillSwitch satisfies it (via the SuspenderAdapter in
// quota_adapter.go), so a breached HARD spend cap suspends the org through the
// exact same mechanism the abuse controls use, and billing does not import the
// quota package directly (no cycle, and billing stays testable with a fake
// suspender). reason and note are non-secret and safe to log.
type Suspender interface {
	// Suspend suspends the org so it fails closed everywhere. manualHold marks
	// that a human must review before it is lifted; the automated billing paths
	// pass false so the matching payment recovery can lift the suspension.
	Suspend(ctx context.Context, orgID, reason, note string, manualHold bool) error
}

// SuspensionLifter is the recovery half of the kill-switch seam (issue #615):
// the reason-scoped automated lift. quota.BillingSuspender satisfies it (the
// same adapter that satisfies Suspender), so the paths that suspend can also
// recover: a paid top-up lifts a spend_cap suspension, a payment-recovered
// subscription lifts a dunning suspension. The lift is reason-scoped and NEVER
// touches a manual-hold suspension, so billing recovery cannot lift an abuse,
// emergency-stop, or operator-held suspension.
type SuspensionLifter interface {
	// LiftReason lifts the org's suspension if it carries exactly this reason
	// and no manual hold, reporting whether anything was lifted.
	LiftReason(ctx context.Context, orgID, reason string) (bool, error)
}

// AlertSink receives soft-cap budget alerts. A SOFT cap does not suspend; it
// fires an alert (email, webhook, console banner) so the org can act before the
// hard cap. The sink is a seam: the test uses a recording fake; the real sink is
// the notification follow-up. Alerts carry org id, the cap, and current spend;
// NO secret.
type AlertSink interface {
	BudgetAlert(ctx context.Context, alert BudgetAlertEvent) error
}

// BudgetAlertEvent is a soft-cap breach notification.
type BudgetAlertEvent struct {
	OrgID   string
	SoftCap Money
	HardCap Money
	Spend   Money
	At      time.Time
}

// SpendCap is an org's budget envelope: a soft cap that fires an alert and a
// hard cap that suspends. The hard cap is the runaway-agent backstop: when the
// org's billable spend in the cap period crosses it, the org is suspended via
// the #213 kill-switch so a looping agent cannot generate an unbounded bill. A
// zero cap means "not set" (no alert / no suspend on that threshold).
type SpendCap struct {
	OrgID   string
	SoftCap Money
	HardCap Money
}

// SpendCapStore holds per-org spend caps. In-memory tested default; durable
// store is a follow-up.
type SpendCapStore interface {
	Get(ctx context.Context, orgID string) (SpendCap, bool, error)
	Set(ctx context.Context, cap SpendCap) error
}

// StatusStore holds each org's BillingStatus (the dunning state). In-memory
// tested default; durable store is a follow-up.
type StatusStore interface {
	Status(ctx context.Context, orgID string) (BillingStatus, error)
	SetStatus(ctx context.Context, orgID string, s BillingStatus) error
}

// Service is the billing core: it pushes #211 usage records to Stripe as
// metered usage (idempotently), draws down the credit ledger, enforces spend
// caps (soft alert / hard suspend via #213), and runs the dunning state machine
// over the webhook events. It is built entirely over the seams above so the
// whole core is unit-tested against FakeStripe with no keys and no network.
type Service struct {
	stripe  StripeClient
	ledger  CreditLedger
	caps    SpendCapStore
	status  StatusStore
	suspend Suspender
	alerts  AlertSink
	rates   Rates
	now     func() time.Time
}

// Config wires a Service. now defaults to time.Now; rates defaults to
// DefaultRates. The stores default to in-memory implementations so a caller can
// stand up a working service with just a StripeClient, a Suspender, and an
// AlertSink.
type Config struct {
	Stripe  StripeClient
	Ledger  CreditLedger
	Caps    SpendCapStore
	Status  StatusStore
	Suspend Suspender
	Alerts  AlertSink
	Rates   Rates
	Now     func() time.Time
}

// NewService builds a Service from a Config, filling in in-memory store defaults
// and the default rate table / clock where the caller left them nil.
func NewService(cfg Config) *Service {
	if cfg.Ledger == nil {
		cfg.Ledger = NewMemCreditLedger()
	}
	if cfg.Caps == nil {
		cfg.Caps = NewMemSpendCapStore()
	}
	if cfg.Status == nil {
		cfg.Status = NewMemStatusStore()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if (cfg.Rates == Rates{}) {
		cfg.Rates = DefaultRates()
	}
	return &Service{
		stripe:  cfg.Stripe,
		ledger:  cfg.Ledger,
		caps:    cfg.Caps,
		status:  cfg.Status,
		suspend: cfg.Suspend,
		alerts:  cfg.Alerts,
		rates:   cfg.Rates,
		now:     cfg.Now,
	}
}

// IdempotencyKey is the Stripe idempotency key for pushing one usage record's
// one meter: the (org, sandbox, window) record key plus the meter. Because the
// usage record key is itself idempotent (issue #211), and we append the meter,
// re-pushing the SAME record reports the SAME key, so Stripe de-duplicates and a
// retried push never double-reports. This is the load-bearing money property.
func IdempotencyKey(rec usage.UsageRecord, unit MeterUnit) string {
	return fmt.Sprintf("%s|%s|%s|%s", rec.OrgID, rec.SandboxID, rec.Window.UTC().Format(time.RFC3339Nano), unit)
}

// PushUsage reports one finalized usage record to Stripe as metered usage, one
// event per non-zero meter, each under the (org, sandbox, window)+meter
// idempotency key. A retried PushUsage with the same record reports the same
// keys, so Stripe never double-reports. It returns the number of meter events
// reported (zero-quantity meters are skipped). It does NOT draw down credit or
// check caps; ApplyUsage does the full money path.
func (s *Service) PushUsage(ctx context.Context, rec usage.UsageRecord) (int, error) {
	customerID, err := s.stripe.EnsureCustomer(ctx, rec.OrgID)
	if err != nil {
		return 0, fmt.Errorf("ensure customer: %w", err)
	}
	n := 0
	for _, unit := range AllMeters() {
		q := QuantityFor(unit, rec)
		if q <= 0 {
			continue
		}
		ev := UsageEvent{
			CustomerID:     customerID,
			Unit:           unit,
			Quantity:       q,
			IdempotencyKey: IdempotencyKey(rec, unit),
		}
		if err := s.stripe.ReportUsage(ctx, ev); err != nil {
			return n, fmt.Errorf("report usage (%s): %w", unit, err)
		}
		n++
	}
	return n, nil
}

// DrawdownResult reports how a usage record's cost was settled against the
// org's prepaid credit: how much credit covered it and how much remains to be
// invoiced (the metered overage Stripe bills). Cost is the WHOLE-CENT amount
// this settle produced, including the org's carried sub-cent remainder from
// earlier windows (issue #662), so a run of sub-cent windows reports Cost 0
// until the accumulated carry rounds to a cent.
type DrawdownResult struct {
	Cost       Money
	FromCredit Money
	Remaining  Money // billed via metered usage beyond credit.
	// CarriedMilliCents is the org's drawdown remainder after this settle: the
	// signed sub-cent milli-cents carried into the next window. Negative means
	// round-half-up settled a cent slightly ahead of usage (the org prepaid up to
	// half a cent that offsets what accrues next).
	CarriedMilliCents int64
	// Replayed reports that this window was ALREADY settled by an earlier call:
	// nothing moved on this one. The drawdown driver counts a replayed result
	// separately and never adds its FromCredit to the cycle's settled total
	// (issue #672: settledCents must reflect only appends that actually landed).
	Replayed bool
}

// Drawdown prices a usage record in MILLI-cents, folds it into the org's
// carried sub-cent remainder, and settles the whole-cent part against the
// org's prepaid credit, capping the debit at the available balance so the
// ledger NEVER goes negative. Pricing per record in whole cents made steady
// small sandboxes unbillable (issue #662: a 1-vCPU minute is 76.8 milli-cents,
// rounded to 0 forever); the accumulator settles 1 cent as soon as the running
// total rounds to one, and the ledger schema stays cents-based.
//
// The settle rounds HALF UP (matching CostCents), so the carried remainder is
// signed in (-500, 500) and the cumulative debits track cumulative usage
// within half a cent at every point in time.
//
// Idempotency and atomicity: EVERY record with a nonzero milli-cent cost
// writes a processed-window marker keyed by the (org, sandbox, window) usage
// key, plus a ledger entry under the same key when the settled amount is
// nonzero (a zero-cent settle marks the window without a customer-visible
// zero-amount ledger row, issue #672). The marker, the entry, and the new
// remainder commit in ONE atomic ledger step (SettleWindow, the issue #666
// AppendWithRemainder path extended), and a replay hits ErrDuplicateEntry
// BEFORE any state moves, so a replayed window can neither double-debit nor
// double-count into the carry. Concurrent drawdown drivers settling the SAME
// org can still interleave the remainder read and write (last write wins,
// skewing the carry by under a cent per race, never the debits); the console
// runs one sequential driver, so this does not occur in the shipped wiring.
func (s *Service) Drawdown(ctx context.Context, rec usage.UsageRecord) (DrawdownResult, error) {
	costMilli := s.rates.CostMilliCents(rec)
	if costMilli == 0 {
		// A zero-usage record settles nothing and marks nothing: re-pricing it on
		// the next tick is free, and an idle org's ledger stays empty.
		return DrawdownResult{}, nil
	}
	carried, err := s.ledger.Remainder(ctx, rec.OrgID)
	if err != nil {
		return DrawdownResult{}, fmt.Errorf("read drawdown remainder: %w", err)
	}
	totalMilli := carried + costMilli
	// Round half up to whole cents. totalMilli >= carried + 1 > -500, so the
	// shifted numerator is non-negative and Go's truncating division floors it.
	cost := Money((totalMilli + 500) / 1000)
	newCarry := totalMilli - int64(cost)*1000
	bal, err := s.ledger.Balance(ctx, rec.OrgID)
	if err != nil {
		return DrawdownResult{}, fmt.Errorf("balance: %w", err)
	}
	fromCredit := cost
	if fromCredit > bal {
		fromCredit = bal
	}
	if fromCredit < 0 {
		fromCredit = 0
	}
	err = s.ledger.SettleWindow(ctx, LedgerEntry{
		OrgID:  rec.OrgID,
		Kind:   KindUsageDrawdown,
		Amount: -fromCredit,
		Key:    usageKey(rec),
		At:     s.now(),
		Note:   "usage drawdown",
	}, newCarry, ProcessedWindow{OrgID: rec.OrgID, SandboxID: rec.SandboxID, Window: rec.Window})
	if err == ErrDuplicateEntry {
		// Idempotent replay: this window was already settled and the remainder
		// already advanced; nothing moved on this call. Report the credit the
		// FIRST call debited (recovered from its ledger entry) and Remaining 0: a
		// replay must never instruct the caller to bill anything again, and the
		// original carry-inclusive split is not reconstructible from the record.
		prior, perr := s.priorDrawdownCredit(ctx, rec)
		if perr != nil {
			return DrawdownResult{}, perr
		}
		return DrawdownResult{Cost: prior, FromCredit: prior, Remaining: 0, CarriedMilliCents: carried, Replayed: true}, nil
	}
	if err != nil {
		return DrawdownResult{}, fmt.Errorf("append drawdown: %w", err)
	}
	return DrawdownResult{Cost: cost, FromCredit: fromCredit, Remaining: cost - fromCredit, CarriedMilliCents: newCarry}, nil
}

// priorDrawdownCredit returns the credit amount debited by the already-recorded
// drawdown for rec (its ledger entry's debit, as a positive Money), so a
// replayed Drawdown reports the same FromCredit it did the first time. The entry
// is found by the same (org, sandbox, window) idempotency key the drawdown wrote.
func (s *Service) priorDrawdownCredit(ctx context.Context, rec usage.UsageRecord) (Money, error) {
	entries, err := s.ledger.Entries(ctx, rec.OrgID)
	if err != nil {
		return 0, fmt.Errorf("read ledger for prior drawdown: %w", err)
	}
	key := usageKey(rec)
	for _, e := range entries {
		if e.Kind == KindUsageDrawdown && e.Key == key {
			// The drawdown stored a negative Amount (-fromCredit); return its
			// magnitude as the credit portion.
			if e.Amount < 0 {
				return -e.Amount, nil
			}
			return 0, nil
		}
	}
	// No matching entry: the first call debited nothing (a zero-cent settle
	// writes only the processed-window marker, no ledger row), so the replayed
	// window's prior credit is zero.
	return 0, nil
}

// usageKey is the (org, sandbox, window) ledger idempotency key for a drawdown.
func usageKey(rec usage.UsageRecord) string {
	return DrawdownKey(rec.OrgID, rec.SandboxID, rec.Window)
}

// SettledWindowKeys exposes the ledger's already-settled window keys (the
// union of processed-window markers and legacy keyed usage_drawdown rows) so
// the drawdown driver can skip settled windows BEFORE pricing them (issue
// #672). since bounds the read to the driver's lookback.
func (s *Service) SettledWindowKeys(ctx context.Context, orgID string, since time.Time) (map[string]bool, error) {
	return s.ledger.SettledWindowKeys(ctx, orgID, since)
}

// PruneProcessedWindows drops processed-window markers whose window is before
// olderThan; the drawdown driver calls it once per cycle with the start of its
// lookback so the marker set stays bounded.
func (s *Service) PruneProcessedWindows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.ledger.PruneProcessedWindows(ctx, olderThan)
}

// EnforceSpendCap checks an org's period spend against its caps and fires the
// right effect: a soft-cap breach fires a budget alert (no suspension); a
// hard-cap breach SUSPENDS the org via the #213 kill-switch with a manual hold
// so a runaway agent cannot generate an unbounded bill and is not auto-lifted
// back into it. It returns whether the org was suspended. periodSpend is the
// org's total billable spend in the current cap period (the caller sums it from
// the usage records / invoices).
func (s *Service) EnforceSpendCap(ctx context.Context, orgID string, periodSpend Money) (suspended bool, err error) {
	cap, ok, err := s.caps.Get(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("get spend cap: %w", err)
	}
	if !ok {
		return false, nil
	}
	if cap.HardCap > 0 && periodSpend >= cap.HardCap {
		if s.suspend != nil {
			// Note is non-secret: org id, the cap, the spend. No payment detail.
			// NO manual hold: a paid top-up is the automated lift lever for a
			// spend-cap suspension (the SuspensionLifter seam), and the spend
			// window resets at the payment, so the org is not lifted back into
			// the same breach. A held suspension remains the operator's tool.
			note := fmt.Sprintf("hard spend cap reached: spend %d cents >= cap %d cents", int64(periodSpend), int64(cap.HardCap))
			if err := s.suspend.Suspend(ctx, orgID, "spend_cap", note, false); err != nil {
				return false, fmt.Errorf("suspend on spend cap: %w", err)
			}
		}
		if err := s.status.SetStatus(ctx, orgID, StatusSuspended); err != nil {
			return true, fmt.Errorf("set suspended status: %w", err)
		}
		return true, nil
	}
	if cap.SoftCap > 0 && periodSpend >= cap.SoftCap && s.alerts != nil {
		if err := s.alerts.BudgetAlert(ctx, BudgetAlertEvent{
			OrgID:   orgID,
			SoftCap: cap.SoftCap,
			HardCap: cap.HardCap,
			Spend:   periodSpend,
			At:      s.now(),
		}); err != nil {
			return false, fmt.Errorf("budget alert: %w", err)
		}
	}
	return false, nil
}

// SetSpendCap sets an org's spend cap.
func (s *Service) SetSpendCap(ctx context.Context, cap SpendCap) error {
	return s.caps.Set(ctx, cap)
}

// EnforceSpendCapFromLedger derives the org's period spend from the credit
// ledger and enforces the spend cap over it. The period is the current
// CALENDAR MONTH (UTC), and the window RESETS at the org's latest in-month
// paid top-up: spend is the sum of usage-drawdown debits settled at or after
// max(month start, latest top-up). The reset is what makes the payment-driven
// lift coherent: a top-up lifts a spend_cap suspension (the org paid and
// chose to continue), and without the reset the very next cycle would re-read
// the same pre-payment spend and re-suspend immediately. Re-suspension
// happens only when the org burns past the cap AGAIN after paying. It is the
// PRODUCTION caller of EnforceSpendCap: the console's drawdown driver runs it
// after settling an active org's usage each cycle (issue #615).
//
// Read cost: the scan is month-bounded, not lifetime-bounded. When the ledger
// implements ScopedLedgerReader (both the in-memory and Postgres ledgers do;
// Postgres serves it from the (org_id, at) index, migration 0012) only the
// current month's entries are read; the full Entries scan is the fallback for
// a ledger that does not. Orgs with NO configured cap short-circuit before
// any ledger read at all.
//
// HONEST LIMIT: drawdown debits are prepaid-credit spend. Metered overage
// beyond credit is not yet included, because pre-#618 no invoice source
// exists (the drawdown caps its debit at the available balance and nothing
// bills the remainder); once overage billing lands, its invoiced amount must
// be folded into this sum. Until then the hard cap binds prepaid spend, which
// is the only money that moves. The window reset keys on settle time (entry
// At), so usage settled after a top-up counts toward the new window even if
// it accrued before the payment; the skew is bounded by the drawdown lookback.
func (s *Service) EnforceSpendCapFromLedger(ctx context.Context, orgID string) (suspended bool, err error) {
	cap, ok, err := s.caps.Get(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("get spend cap: %w", err)
	}
	if !ok || (cap.HardCap <= 0 && cap.SoftCap <= 0) {
		return false, nil
	}
	now := s.now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var entries []LedgerEntry
	if sr, isScoped := s.ledger.(ScopedLedgerReader); isScoped {
		entries, err = sr.EntriesSince(ctx, orgID, periodStart)
	} else {
		entries, err = s.ledger.Entries(ctx, orgID)
	}
	if err != nil {
		return false, fmt.Errorf("read ledger for spend cap: %w", err)
	}
	// The spend window starts at the later of the month start and the org's
	// latest in-month paid top-up (signup credit is not a payment and does not
	// reset the window).
	windowStart := periodStart
	for _, e := range entries {
		if e.Kind == KindTopUp && e.Amount > 0 && !e.At.UTC().Before(periodStart) && e.At.UTC().After(windowStart) {
			windowStart = e.At.UTC()
		}
	}
	var spend Money
	for _, e := range entries {
		if e.Kind == KindUsageDrawdown && e.Amount < 0 && !e.At.UTC().Before(windowStart) {
			spend += -e.Amount
		}
	}
	return s.EnforceSpendCap(ctx, orgID, spend)
}

// applyDunning runs the dunning state machine for an org over one event and
// applies the side effects: a transition INTO suspended drives the #213
// kill-switch; any transition persists the new status. It returns the new
// status. It is the single place the pure NextStatus function is paired with the
// suspend side effect.
func (s *Service) applyDunning(ctx context.Context, orgID string, ev DunningEvent) (BillingStatus, error) {
	cur, err := s.status.Status(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	next := NextStatus(cur, ev)
	if next == StatusSuspended && cur != StatusSuspended && s.suspend != nil {
		note := fmt.Sprintf("dunning: %s", ev)
		if err := s.suspend.Suspend(ctx, orgID, "dunning", note, false); err != nil {
			return cur, fmt.Errorf("suspend on dunning: %w", err)
		}
	}
	if err := s.status.SetStatus(ctx, orgID, next); err != nil {
		return cur, fmt.Errorf("set status: %w", err)
	}
	return next, nil
}
