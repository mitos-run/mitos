# Runbook: UsageCollectorFailing

## Signal

`sum(rate(mitos_usage_collect_cycle_failures_total[15m])) > 0` sustained for
15m.

`mitos_usage_collect_cycle_failures_total` counts failed collection cycles in
the controller's usage collector (`--usage-collector`): a scrape, integrate,
or upsert error ended the cycle without recording. At the default 60s cadence
(`controller.usage.interval`) a sustained 15m rate is around 15 consecutive
failed cycles. Metered usage records, the input to billing and credit
drawdown, are not being written for the failing windows. Threshold is
environment-tunable.

## Likely causes

- A forkd node's `GET /v1/metering` endpoint is unreachable (node loss,
  network change, forkd crash) and the failure ends the cycle.
- The husk-pod org attribution lookup is failing (API server slow or RBAC
  broken for pod reads).
- The usage store upsert is failing.
- A controller rollout broke the collector wiring (it logs a startup line
  when enabled).

## Diagnosis

- Controller logs: each failed cycle logs its (sanitized) error; the paired
  counter carries no cause by design.
- `mitos_usage_collect_cycle_duration_seconds` is set only on SUCCESS: if it
  is still changing, some cycles succeed (intermittent); if frozen,
  `UsageCollectorStalled` fires and nothing succeeds.
- Check forkd DaemonSet health and whether a specific node's metering
  endpoint refuses connections.
- Compare `mitos_usage_vcpu_seconds_total{org}`: flat per-org series during
  active sandbox load confirm records are not landing.

## Remediation

- Restore the failing dependency (forkd pod, network path, API server
  pressure). The collector is a periodic loop; it recovers on the next cycle
  with no restart needed.
- Usage lost during failed cycles for LIVE sandboxes is re-observed on the
  next successful scrape (metering totals are cumulative per sandbox);
  sandboxes that terminated entirely within the outage may be
  under-recorded. Note the outage window for billing.
- If only one node fails persistently, drain/restart its forkd.

## Escalation

Suspected permanent metering loss for terminated sandboxes in the outage
window is a billing-accuracy question: hand the window to the billing owner
rather than reconstructing records by hand.

Related alerts: `UsageCollectorStalled` (nothing succeeding),
`DrawdownStalled` (downstream effect: nothing new to settle).
