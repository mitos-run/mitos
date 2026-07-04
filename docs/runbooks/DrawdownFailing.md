# Runbook: DrawdownFailing

## Signal

`sum(rate(mitos_drawdown_cycle_errors_total[30m])) > 0` sustained for 30m.

`mitos_drawdown_cycle_errors_total` counts failed operations inside the
console's usage drawdown driver (introduced by issue #602, the end-to-end
usage metering wiring): the org list, an org's usage record list, an org's
settled-window read, a per-record settle, or the marker prune. Each failure
is counted and skipped without aborting the cycle, so a nonzero rate means
SOME metered usage is not settling against prepaid credit while the rest
still does. A system that meters usage while no money moves is the
issue #662 failure signature (there it was per-record rounding; here it is
failing operations, but the customer-visible symptom is the same). At the
default 5m cadence a 30m window is about 6 cycles: persistent, not one flaky
tick. Threshold is environment-tunable.

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

- Fix the failing dependency (usage API service, token Secret, Postgres).
  Back-settling is automatic ONLY within the driver's lookback window (a
  compile-time constant, `drawdownLookback` = 2h in `cmd/console/drawdown.go`):
  every tick re-lists that window, so records that failed less than 2h ago
  settle on the next clean cycle with no action. Records whose window fell
  OUT of the lookback while the failure persisted are NOT replayed
  automatically; settling them is a deliberate operation for the billing
  owner (a one-off console run with a widened lookback constant is the
  documented path). Any replay, automatic or manual, is safe: the
  processed-window markers (`processed_usage_windows`, issue #672) and the
  ledger's (org, sandbox, window) idempotency key make double-debit
  impossible regardless of how often a record is re-listed.
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
