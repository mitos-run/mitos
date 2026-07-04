# Runbook: ConsoleUnavailable

## Signal

`max(up{container="console"}) == 0 or absent(up{container="console"})`
sustained for 5m. Severity: critical.

`up` comes from the mitos-console PodMonitor scraping the console's
cluster-internal metrics listener. Zero (or absent) means NO console replica
answered its scrape for 5m: the pods are down, crash-looping, unschedulable,
or the Deployment is gone. Users cannot sign in, manage keys, view usage, or
top up credit; the billing webhook endpoint (`/webhooks/billing`) is down with
it, so provider events queue in the provider's retry buffer. This rule is
rendered only when PodMonitors are enabled, because the `up` series does not
exist without the scrape.

## Likely causes

- CrashLoopBackOff after a bad rollout (config or migration failure at
  startup: a malformed `MITOS_CONSOLE_RATES` fails startup by design).
- Postgres down at boot: the console fails fast on a bad DSN.
- Scheduling failure (resources, node loss) with replicas: 1 (the chart
  default, because OIDC state is per-pod).
- The PodMonitor selector or metrics port broken by a chart change (scrape
  gone while pods are actually fine; users unaffected).

## Diagnosis

- `kubectl -n mitos get pods -l app.kubernetes.io/component=console` and
  `kubectl -n mitos describe deploy mitos-console`.
- Pod logs: startup fail-fast lines name the misconfigured knob (rates parse,
  persistence, OIDC discovery warnings).
- Can you load the console URL? If the UI works while this alert fires, only
  the SCRAPE is broken: check the PodMonitor and the `metrics` container
  port.
- Is `ConsolePostgresUnreachable` in recent history? A dead database first
  pulls the console from the Service (readiness), then a restart loop can
  take it fully down.

## Remediation

- Bad rollout: roll back the console image or fix the offending env value;
  startup errors are actionable by design.
- Database: follow `ConsolePostgresUnreachable` first.
- Scheduling: free capacity or loosen constraints; with replicas: 1 a single
  node drain takes the console down until rescheduled (the chart's PDB is
  off by default for exactly that reason; do not enable it at 1 replica).
- Scrape-only breakage: fix the PodMonitor/port; no user impact, but the
  alert is blind until then.
- After recovery, let the billing provider's retries land any webhook events
  missed during the outage.

## Escalation

If the console is down because of a shared control-plane outage (API server,
etcd), escalate to the cluster on-call: the engine alerts will be firing too.

Related alerts: `ConsolePostgresUnreachable` (most common cause),
`BillingWebhookErrorRateHigh` (aftermath), `CanaryDown` (sandbox path,
independent of the console).
