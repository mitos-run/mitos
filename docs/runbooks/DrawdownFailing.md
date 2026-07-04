# Runbook: DrawdownFailing

## Signal

`sum(rate(mitos_drawdown_cycle_errors_total[30m])) > 0` sustained for 30m.

`mitos_drawdown_cycle_errors_total` counts failed operations inside the
console's usage drawdown driver (issue #602): the org list, an org's usage
record list, an org's settled-window read, a per-record settle, or the marker
prune. Each failure is counted and skipped without aborting the cycle, so a
nonzero rate means SOME metered usage is not settling against prepaid credit
while the rest still does. At the default 5m cadence a 30m window is about 6
cycles: persistent, not one flaky tick. Threshold is environment-tunable.

## Likely causes

- The controller's internal usage API is unreachable or its bearer token is
  wrong (`MITOS_USAGE_API_URL` / the usage token secretKeyRef): the per-org
  `ListRecords` calls fail.
- Postgres is degraded: the org list, settled-window reads, or ledger appends
  fail.
- One org's data consistently errors (a poisoned record) while others settle.

## Diagnosis

- Console logs: each cycle logs `usage drawdown cycle` with an `errors` count,
  and the failing operation logs its own warn line (`list records failed`,
  `read settled windows failed`, `prune processed windows failed`) with the
  org id. Constant orgs in the warn lines = a per-org problem; all orgs = a
  dependency problem.
- Is `DrawdownStalled` also firing? Then NO cycle is completing cleanly.
- Is `ConsolePostgresUnreachable` or `UsageCollectorFailing` firing? Fix
  upstream first.
- `curl` the usage API from inside the cluster with the token to confirm
  reachability (the URL is logged at startup; the token value never is).

## Remediation

- Fix the failing dependency (usage API service, token Secret, Postgres). No
  data is lost: the driver's 2h lookback re-lists unsettled records every
  tick, and settlement is idempotent per (org, sandbox, window), so recovery
  back-settles automatically and can never double-debit.
- A single poisoned org/record: inspect that org's usage records via the
  usage API; the failing record's key is in the logs.
- Verify recovery: the cycle log shows `errors=0` and nonzero `settledCents`
  when there is real usage.

## Escalation

If records repeatedly fail to settle for a reason inside the billing service
(pricing or ledger append), involve the billing owner: manual ledger repair is
a money-path operation.

Related alerts: `DrawdownStalled` (no clean cycle at all),
`UsageCollectorFailing` (no input being produced), `OrgCreditExhausted`
(settles landing but credit gone).
