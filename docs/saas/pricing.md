# SaaS pricing and Stripe metered billing

Status: foundational. This wires the money for the hosted
offering: Stripe usage-based (metered) billing, plans, free signup credits,
prepaid top-ups, hard and soft spend caps, and dunning. It sits on the usage
records (`internal/usage`), the accounts/orgs/keys model
(`internal/saas`), and the kill-switch (`internal/saas/quota`).

CRITICAL: Stripe is an external service. Real charges need test-mode API keys,
and those keys are SECRETS. Nothing here makes a real charge. The whole billing
core is built against a
`StripeClient` INTERFACE with an in-memory `FakeStripe` for tests; the real
Stripe SDK adapter and the real webhook signature verification are documented
seams a maintainer runs with test-mode keys. The Stripe Go SDK is NOT a hard
dependency.

Security: a Stripe API key, the webhook signing secret, or any customer payment
method detail is NEVER logged, NEVER placed in an error message, and NEVER put
in a condition or webhook note. Ids and counts only. The webhook handler never
echoes the raw body or the `Stripe-Signature` header in any error.

## Pricing model

The pricing shape matches the category (per-second compute with DECOUPLED vCPU
and RAM), so a CPU-heavy and a RAM-heavy sandbox of the same wall-clock time
bill differently. The billable dimensions (`billing.MeterUnit`), each its own
Stripe meter and price:

- `vcpu_second`: per-vCPU-second compute.
- `mem_gib_second`: per-GiB-second RAM (decoupled from vCPU).
- `storage_gib_hour`: per-GiB-hour persisted sandbox storage.
- `egress_gib`: per-GiB outbound egress (see the egress decision below).
- `gpu_second`: per-GPU-second accelerator time.

These dimensions are exactly the billable units the `usage.UsageRecord`
already integrates (`VCPUSeconds`, `MemGiBSeconds`, `StorageGiBHours`,
`EgressBytes`, `GPUSeconds`), so a usage record maps one-to-one to metered
quantities with no re-derivation.

### Egress decision: METERED, not free-within-cap

Egress is METERED (billed per GiB), NOT free-within-a-cap. Unmetered egress is
the classic abuse subsidy: a sandbox platform that gives away outbound bandwidth
underwrites scraping, exfiltration relays, and outbound-attack traffic. So:

- Free-tier exposure is bounded by a HARD egress posture (deny-by-default and
  the abuse-port block), not by giving egress away.
- Paid orgs pay per GiB of egress, like every other dimension.

This keeps the directional economics honest and removes the abuse subsidy.

### Rates are illustrative and configurable, never published numbers

Rates live as a structured config (`billing.Rates`), quoted in MILLI-cents per
unit (thousandths of a cent) so a single per-second tick does not round to zero.
The values in `billing.DefaultRates()` are ILLUSTRATIVE placeholders, NOT
published prices (the no-unverified-claims rule). A deployment overrides them
from config (below). Only the SHAPE (decoupled per-second compute, storage
GiB-hours, metered egress) is committed here.

#### Configuring the rates

The console reads `MITOS_CONSOLE_RATES`: a single JSON object mapping onto
`billing.Rates` (parsed by `billing.ParseRatesConfig`), for example:

    MITOS_CONSOLE_RATES='{"vcpu_second_milli_cents":1.28,"mem_gib_second_milli_cents":0.16,"storage_gib_hour_milli_cents":10,"egress_gib_milli_cents":9000,"gpu_second_milli_cents":60}'

Semantics:

- Unset (or empty) uses `billing.DefaultRates()`, exactly as before.
- When set, the object REPLACES the default table ENTIRELY: an omitted key is a
  zero (free) rate, never the default for that key.
- Unknown keys, malformed JSON, and negative rates FAIL console startup with an
  actionable error (fail closed); a bad override never silently falls back to
  the defaults and bills at the wrong rates.

The Helm chart exposes it as `console.billing.rates` (a map rendered to the
JSON env value; use a values file or `--set-json`, since plain `--set`
stringifies fractional numbers). The SAME parsed table prices the console
billing view, the credit-drawdown driver, and (via `Rates.ToPriceList`) the
display-cost estimate, so what a user sees, what draws down, and what would be
billed never drift.

The controller's internal usage API has the matching display-side override:
`MITOS_USAGE_PRICELIST` (a JSON object mapping onto `usage.PriceList`, dollars
per unit; Helm value `controller.usage.priceList`), with the same strict
fail-closed parsing. Keep the two consistent: milli-cents per unit = dollars
per unit x 100000.

`billing.DefaultRates()` mirrors the magnitude of the placeholder
`usage.DefaultPriceList` re-expressed in milli-cents, and `billing.FromPriceList`
/ `Rates.ToPriceList` are the bridges between the two so the usage API's
display-cost estimate and the real billing rates derive from one table and
never drift. This is the reconciliation of the `PriceList` placeholder to the
billing model.

Directional competitor figures (Modal, Daytona, E2B published per-second or
per-resource prices) may be cited as vendor-published when comparing the model's
shape; our own numbers are never stated as facts until a deployment sets them.

## Money representation

All accounting is in integer MINOR UNITS (cents, `billing.Money`), the standard
Stripe representation, so the credit ledger and the spend-cap comparison are
exact with no float drift. Float rates are converted to integer cents exactly
once, at the metering->cost boundary (`Rates.CostCents`), accumulating in
milli-cents and rounding the total to the nearest cent once. The cap and the
ledger price usage with the SAME function, so a record costs the same to the cap
as to the ledger.

## Metered push and the idempotency key

`Service.PushUsage` reports a finalized usage record to Stripe as one metered
event per non-zero meter. The Stripe idempotency key for each event is

    {org}|{sandbox}|{window}|{meter}

i.e. the `(org, sandbox, window)` record key plus the meter. Because the
usage record key is itself idempotent (the store upserts by it and
`Integrate` is pure), re-pushing the SAME record reports the SAME keys, so
Stripe de-duplicates and a RETRIED push never double-reports. The real adapter
passes this as the Stripe `Idempotency-Key` request header; `FakeStripe` records
events keyed by it, so a retry overwrites rather than appends and the
distinct-event count does not grow. This is unit-tested, including a retry after
a transient mid-push failure.

## Free credits and the prepaid top-up ledger

`billing.CreditLedger` is a per-org, append-only ledger; a balance is the signed
sum of its entries, never a mutated field, so the accounting is auditable and
cannot silently drift.

- Signup credit (`GrantSignupCredit`): the free credit at signup. The default is
  $100 (`DefaultSignupCredit`), within the $100-$200 self-serve bar.
  Illustrative and configurable: `MITOS_CONSOLE_SIGNUP_CREDIT_CENTS` (Helm
  value `console.signupCreditCents`) overrides it per deployment in integer
  cents, e.g. 500 grants $5.00.
- Top-ups (`TopUp`): prepaid purchases on a Daytona-style ladder
  (`TopUpLadder`: $10/$25/$50/$100/$250, illustrative). Added after the payment
  clears.
- Drawdown (`KindUsageDrawdown`): metered usage drawing the balance down.

`Service.Drawdown` prices a usage record and debits the ledger, CAPPING the
debit at the available balance so the ledger NEVER goes negative; the uncovered
remainder is the metered overage Stripe bills. Grants, top-ups, and drawdowns
are each idempotent on their key (the grant id, the payment reference, the usage
record key), so a retried signup, a redelivered top-up webhook, or a replayed
usage push never double-credits or double-debits. All unit-tested.

## Spend caps and budget alerts

`billing.SpendCap` is an org's budget envelope: a SOFT cap and a HARD cap.
`Service.EnforceSpendCap`:

- SOFT cap breach: fires a budget alert through the `AlertSink` seam (email,
  webhook, console banner). No suspension.
- HARD cap breach: SUSPENDS the org through the kill-switch (the
  `Suspender` seam, satisfied by `quota.BillingSuspender` wrapping
  `quota.KillSwitch`), so a runaway agent cannot generate an unbounded bill.
  The automated suspend carries NO manual hold: a cleared paid top-up is the
  lift lever (`SuspensionLifter.LiftReason`, reason-scoped), and the spend
  window resets at the payment, so the org is not lifted back into the same
  breach; it re-suspends only by burning past the cap AGAIN after paying.
  Operator-imposed suspensions with a manual hold survive every automated
  lift and clear only through the operator Lift hook.

In production the drawdown driver evaluates the cap each cycle for every org
that settled usage (`Service.EnforceSpendCapFromLedger`): period spend is the
sum of the org's usage-drawdown debits since max(calendar month start, latest
in-month paid top-up), read via the month-scoped `ScopedLedgerReader` path
(Postgres serves it from the `(org_id, at)` index, migration 0012). Metered
overage beyond credit is not yet part of period spend: pre-#618 no invoice
source exists, so prepaid drawdown is the only money that moves.

The hard-cap suspend reaches the kill-switch through a narrow `Suspender`
interface so `internal/saas/billing` does not import `internal/saas/quota` (no
cycle); the adapter that maps billing reasons to `quota.SuspensionReason`
(`ReasonSpendCap`, `ReasonDunning`) lives in the quota package. Unit-tested:
crossing the hard cap fires exactly one kill-switch suspend (no hold);
crossing the soft cap fires exactly one alert; a top-up lifts the spend-cap
suspension and a manual hold survives it.

## Dunning state machine

`billing.NextStatus` is a pure, total transition function over the org billing
status (`active`, `past_due`, `suspended`):

| from \ event      | payment_succeeded | payment_failed | retries_exhausted | suspend   |
|-------------------|-------------------|----------------|-------------------|-----------|
| active            | active            | past_due       | suspended         | suspended |
| past_due          | active            | past_due       | suspended         | suspended |
| suspended         | active            | suspended      | suspended         | suspended |

It is pure so the transitions are exhaustively unit-tested with no side effects.
`Service.applyDunning` pairs it with the side effects: a transition INTO
suspended drives the kill-switch; every transition persists the new status.
The billing status reflects payment standing; whether a kill-switch manual hold
blocks the actual un-suspend is the kill-switch's concern.

## Webhooks

`billing.WebhookHandler` verifies and dispatches Stripe webhooks. It maps
`invoice.payment_succeeded` and `invoice.payment_failed` to dunning events and
runs the dunning machine (updating status and, on a transition into suspended,
driving the kill-switch). An event type the slice does not act on returns the
current status unchanged and is not an error. Unit-tested with a fake-verified
event.

Signature verification is a SEAM (`billing.SignatureVerifier`). The real
verifier needs the endpoint signing secret and constant-time checks the
`Stripe-Signature` header; that secret is not wired here, so the
test uses `FakeVerifier` (parses and trusts the body). The HTTP entry point
never echoes the body or the signature in any error.

## Seams and follow-ups

These are interfaces with tested in-memory or fake defaults; the real
implementations are follow-ups behind the same interface, runnable by a
maintainer with test-mode keys:

- Real Stripe SDK adapter (implements `StripeClient`): a small file in
  `internal/saas/billing` (for example `stripe_sdk.go`, build-tagged or guarded
  so the Stripe Go SDK is not a hard dependency) wrapping the
  Stripe Go SDK for products/prices, metered usage reporting (passing the
  idempotency key as the `Idempotency-Key` header), subscriptions, invoices, and
  payment methods. Exercised against test-mode keys.
- Real webhook signature verification (implements `SignatureVerifier`): the
  Stripe signing-secret check over the `Stripe-Signature` header.
- Durable stores (Postgres `CreditLedger`, `SpendCapStore`, `StatusStore`).
- The notification `AlertSink` (email / webhook / console banner).

PRODUCTION GATE: real-charge wiring is NOT enabled here and must be reviewed
(keys handling, no-secret-logging, the no-unverified-numbers rule on any
published price) before it goes live for tenants.
