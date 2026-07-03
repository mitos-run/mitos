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
	// that a human must review before it is lifted (a spend-cap suspension carries
	// a hold so a runaway agent's org is not auto-unsuspended into the same bill).
	Suspend(ctx context.Context, orgID, reason, note string, manualHold bool) error
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
// writes a ledger entry keyed by the (org, sandbox, window) usage key, even
// when the settled amount is 0 cents; that entry is both the debit and the
// processed-window marker. The entry and the new remainder commit in ONE
// atomic ledger step (AppendWithRemainder), and a replay hits
// ErrDuplicateEntry BEFORE any state moves, so a replayed window can neither
// double-debit nor double-count into the carry. Concurrent drawdown drivers
// settling the SAME org can still interleave the remainder read and write
// (last write wins, skewing the carry by under a cent per race, never the
// debits); the console runs one sequential driver, so this does not occur in
// the shipped wiring.
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
	err = s.ledger.AppendWithRemainder(ctx, LedgerEntry{
		OrgID:  rec.OrgID,
		Kind:   KindUsageDrawdown,
		Amount: -fromCredit,
		Key:    usageKey(rec),
		At:     s.now(),
		Note:   "usage drawdown",
	}, newCarry)
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
		return DrawdownResult{Cost: prior, FromCredit: prior, Remaining: 0, CarriedMilliCents: carried}, nil
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
	// No matching entry: the first call debited nothing (fromCredit was 0), so the
	// duplicate guard cannot have fired on a drawdown entry; treat as zero credit.
	return 0, nil
}

// usageKey is the (org, sandbox, window) ledger idempotency key for a drawdown.
func usageKey(rec usage.UsageRecord) string {
	return fmt.Sprintf("drawdown:%s|%s|%s", rec.OrgID, rec.SandboxID, rec.Window.UTC().Format(time.RFC3339Nano))
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
			note := fmt.Sprintf("hard spend cap reached: spend %d cents >= cap %d cents", int64(periodSpend), int64(cap.HardCap))
			if err := s.suspend.Suspend(ctx, orgID, "spend_cap", note, true); err != nil {
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
