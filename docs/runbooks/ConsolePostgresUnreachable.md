# Runbook: ConsolePostgresUnreachable

## Signal

`sum(rate(mitos_console_db_ping_failures_total[5m])) > 0` sustained for 10m.
Severity: critical.

`mitos_console_db_ping_failures_total` counts failed Postgres pings from the
console's `/readyz` readiness handler. The kubelet drives that probe every 10s
(the chart's periodSeconds), so the counter's rate is a continuous database
reachability signal without a dedicated prober. 10m sustained is far past any
transient failover blip: the durable store behind accounts, API keys,
sessions, billing status, the credit ledger, and the allowlist is down. The
series exists only when a durable Postgres is configured (in-memory dev
persistence exports nothing). Threshold is environment-tunable.

## Likely causes

- The Postgres instance or its Service is down (crashed pod, node loss,
  managed-DB outage or maintenance).
- The DSN Secret was rotated/broken by a deploy (bad host, credentials, or
  database name).
- Network path or NetworkPolicy change between the console and the database.
- Connection-pool exhaustion on the database side (pings time out at the 2s
  handler bound).

## Diagnosis

- `kubectl -n mitos get pods` for the console: failing readiness means it is
  already out of the Service (users see the console down, and
  `ConsoleUnavailable` may fire next).
- Reach the database directly from the cluster (a psql one-off pod) with the
  same DSN Secret. Never paste the DSN into logs or tickets; it embeds
  credentials.
- Check the gateway too: it shares the same database for key verification, so
  expect `GatewayErrorRateHigh` alongside; the gateway has no DB probe metric
  of its own yet.
- Managed-DB dashboard / events for failover or maintenance windows.

## Remediation

- Restore the database or fix the DSN Secret, then confirm `/readyz` returns
  200 and the counter rate returns to zero. The console needs no restart for
  a recovered database; a changed DSN Secret requires a pod restart to
  re-read the env.
- After recovery, check the billing webhook provider dashboard for deliveries
  that failed during the outage and let retries land them (see
  `BillingWebhookErrorRateHigh`).

## Escalation

A database loss with suspected data loss (not just unavailability) affects
the money path (credit ledger): involve the billing owner and restore from
backup deliberately; the ledger is append-only and idempotent, which makes
replays safe but silent gaps costly.

Related alerts: `ConsoleUnavailable` (the resulting outage),
`BillingWebhookErrorRateHigh`, `DrawdownFailing`, `GatewayErrorRateHigh`
(shared database).
