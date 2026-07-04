# Runbook: UsageCollectorStalled

## Signal

`changes(mitos_usage_collect_cycle_duration_seconds[30m]) == 0` sustained for
15m.

`mitos_usage_collect_cycle_duration_seconds` is a gauge the controller's usage
collector sets to the wall duration of each SUCCESSFUL collection cycle. At
the default 60s cadence a 30m window holds about 30 cycles, and a real wall
duration is never bit-identical across them, so ZERO changes over 30m means no
cycle has succeeded in that window: the loop is wedged, or every cycle is
failing. This staleness rule exists because the failure counter alone cannot
see a WEDGED loop (a hung cycle increments nothing). The series is absent when
the collector is disabled, so a deployment without metering never fires this.
Threshold (the window) is environment-tunable; scale it with
`controller.usage.interval`.

## Likely causes

- Every cycle failing (then `UsageCollectorFailing` fires too): follow that
  runbook's causes.
- The collector goroutine is wedged on a hung scrape (a node that accepts TCP
  but never answers can hold a cycle open).
- The controller restarted with `--usage-collector` off (a values change
  flipped `controller.usage.collector`), while the last scraped value briefly
  persists.
- A much longer custom collection interval than the 30m window assumes.

## Diagnosis

- Is `UsageCollectorFailing` firing? Failing, not wedged: follow it.
- Controller logs: per-cycle log lines stopping entirely (no success, no
  failure) is the wedge signature.
- Confirm the flag: the controller logs the collector startup line when
  enabled.
- Check `up` for the controller PodMonitor: a dead controller stalls this
  series too, but the engine alerts will already be screaming.

## Remediation

- Wedged loop: restart the controller Deployment. Metering totals are
  cumulative per live sandbox, so the next successful scrape re-observes
  running sandboxes; only sandboxes that terminated entirely within the stall
  may be under-recorded (note the window for billing).
- Failing cycles: follow `UsageCollectorFailing`.
- Interval mismatch: tune the rule window to at least several configured
  intervals.

## Escalation

Same as `UsageCollectorFailing`: permanent metering loss for
terminated-in-window sandboxes goes to the billing owner.

Related alerts: `UsageCollectorFailing`, `DrawdownStalled` (downstream:
nothing new to settle).
