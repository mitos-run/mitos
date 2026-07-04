# Runbook: BillingWebhookErrorRateHigh

## Signal

`sum(rate(mitos_billing_webhook_errors_total[15m])) > 0` sustained for 15m.

`mitos_billing_webhook_errors_total` counts SIGNATURE-VERIFIED billing webhook
events answered 5xx: the customer link write, the top-up credit append, the
customer-to-org lookup, or the billing status write failed. A 5xx makes the
provider RETRY (that is deliberate: a transient store failure must not drop a
paid top-up), so nothing is lost yet; but provider retry budgets are finite,
and sustained failures mean top-ups and status changes are queueing at the
provider instead of landing in the ledger. Threshold is environment-tunable
but should stay at zero tolerance.

## Likely causes

- Postgres is down or degraded (the customers map, credit ledger, and status
  store are all Postgres-backed in production). Almost always the cause.
- A schema migration failure after a rollout (writes erroring on a missing
  column/table).
- Running without a database in a hosted profile (dev-only in-memory stores
  do not fail this way, but a misconfigured DSN fails every write).

## Diagnosis

- Is `ConsolePostgresUnreachable` firing too? Then this is the database, fix
  that first.
- Console logs: the handler answers `customer link failed`, `credit top-up
  failed`, `customer lookup failed`, or `status update failed`; the log lines
  around them name the store error (values are never logged).
- Provider dashboard: the failed deliveries with 5xx responses list exactly
  which events are pending; they retry automatically.

## Remediation

- Restore Postgres / fix the DSN Secret; the provider's automatic retries then
  land the queued events. The handler is idempotent per transaction ref
  (duplicate top-ups are acknowledged, never double-credited), so retries and
  manual replays are safe.
- After recovery, verify in the provider dashboard that no delivery exhausted
  its retries; replay any that did.
- Confirm a recent transaction is visible in the org's credit ledger (console
  billing view) before closing.

## Escalation

If events exhausted provider retries and were dropped, reconciliation against
the provider's transaction log is a money-path operation: involve the billing
owner before writing manual ledger entries.

Related alerts: `BillingWebhookVerifyFailures` (refused before verification),
`ConsolePostgresUnreachable` (the usual root cause).
