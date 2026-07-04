# Runbook: BillingWebhookVerifyFailures

## Signal

`sum(rate(mitos_billing_webhook_verify_failures_total[15m])) > 0` sustained
for 15m.

`mitos_billing_webhook_verify_failures_total` counts requests to
`/webhooks/billing` refused because the provider signature did not verify
(forged, replayed, malformed, or signed with a different secret). The endpoint
is public by design and authenticated ONLY by that signature, so legitimate
verify-failure volume is zero: any sustained rate is actionable. Threshold is
environment-tunable but should stay at zero tolerance.

## Likely causes

- MISCONFIGURED SECRET (the urgent case): the provider-side webhook secret was
  rotated but `MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET` (or the Stripe signing
  secret) was not updated, so EVERY legitimate event is rejected. Top-ups and
  billing status syncs silently stop landing.
- Someone is probing the public endpoint with forged or replayed events.
- A replayed delivery outside the 5m timestamp tolerance (a provider outage
  followed by a very late retry burst).

## Diagnosis

- Check the provider dashboard (Paddle: Notifications > webhook logs) for
  failed deliveries. Failing deliveries there = secret mismatch, OUR problem.
  Clean provider logs while our counter climbs = forged traffic, not the
  provider.
- Compare against `mitos_billing_webhook_errors_total`: verify failures are
  400s BEFORE any state is touched; handler errors are 5xx AFTER
  verification. Both climbing suggests a broader problem.
- Console logs around the webhook mount confirm which provider is active
  ("billing enabled" with the provider name).

## Remediation

- Secret mismatch: update the webhook secret Secret referenced by
  `console.billing.paddle.webhookSecretRef` and restart the console. Then
  REPLAY the failed deliveries from the provider dashboard so the missed
  top-ups and status changes land (the handler is idempotent per transaction
  ref; replays cannot double-credit).
- Forged traffic: no action needed for correctness (the endpoint refuses and
  never mutates state); consider edge rate-limiting the path if the volume is
  a nuisance.

## Escalation

Sustained forged-event volume aimed at the billing endpoint is a security
event: follow SECURITY.md. Money-path correctness questions go to the billing
owner before any manual ledger intervention.

Related alerts: `BillingWebhookErrorRateHigh` (verified events failing to
apply), `ConsolePostgresUnreachable` (store behind the handler).
