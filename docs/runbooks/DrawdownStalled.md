# Runbook: DrawdownStalled

## Signal

`time() - mitos_drawdown_last_success_timestamp_seconds > 1800` sustained for
15m.

`mitos_drawdown_last_success_timestamp_seconds` is stamped after every
drawdown cycle that completes with ZERO failed operations, and initialized to
the driver start time at boot. 1800s is 6 missed cycles at the default 5m
interval (`MITOS_CONSOLE_DRAWDOWN_INTERVAL`): prepaid credits have not been
drawn down against metered usage for half an hour. The series exists only
when the driver is enabled, so a deployment without metering can never fire
this. Threshold is environment-tunable; scale it with the configured interval.

## Likely causes

- Every cycle is erroring (then `DrawdownFailing` fires too): usage API or
  Postgres down.
- The driver loop is wedged (a hung HTTP call without a deadline inside a
  cycle blocks the ticker).
- The console was restarted into a state where the driver is disabled (the
  in-memory usage store fallback disables it; check the startup log line
  `usage drawdown driver enabled/disabled`), while Prometheus still evaluates
  the last scrape's stale series for a few minutes.
- A custom, much longer `MITOS_CONSOLE_DRAWDOWN_INTERVAL` that the default
  threshold was never tuned for.

## Diagnosis

- Console startup log: was the driver enabled, and at what interval?
- Cycle logs: `usage drawdown cycle` lines appearing with `errors>0` means
  failing, not stalled (follow `DrawdownFailing`); NO cycle lines at all
  means wedged.
- Is `UsageCollectorStalled` firing? Then the upstream metering is also down
  and the pipeline is broken end to end.

## Remediation

- Failing cycles: follow `DrawdownFailing` (fix usage API / Postgres).
- Wedged loop: restart the console Deployment. Settlement is idempotent per
  (org, sandbox, window) and the 2h lookback replays everything recorded
  meanwhile, so a restart is always safe and recovery back-settles
  automatically.
- Interval mismatch: tune the alert threshold to at least 3x the configured
  interval.

## Escalation

If the lookback window (2h) was exceeded while stalled, records older than the
lookback will NOT be replayed automatically; involve the billing owner to
settle the gap deliberately (the usage records themselves are durable).

Related alerts: `DrawdownFailing`, `UsageCollectorStalled`,
`ConsolePostgresUnreachable`.
