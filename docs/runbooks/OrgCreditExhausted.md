# Runbook: OrgCreditExhausted

## Signal

`sum(increase(mitos_drawdown_credit_exhausted_total[1h])) > 12` sustained for
1h.

`mitos_drawdown_credit_exhausted_total` counts newly settled usage records
whose cost exceeded the org's remaining prepaid credit (the settle result
carried a nonzero unbacked remainder). At the default 5m drawdown cadence, 12
in an hour is roughly ONE org settling beyond its credit every cycle for the
whole hour: a volume signal, not a single boundary record as an org's balance
crosses zero. The unbacked remainder is not billed anywhere today (metered
overage billing does not exist yet), so sustained volume is unpaid
consumption. Threshold is environment-tunable.

## Likely causes

- One or more orgs ran out their prepaid balance and kept consuming (expected
  mechanically; the question is why enforcement did not stop them).
- The suspension / spend-cap enforcement path is not engaging (billing
  suspender or kill-switch misconfigured), so exhausted orgs keep forking.
- Top-up is broken (see `BillingWebhookVerifyFailures` /
  `BillingWebhookErrorRateHigh`): customers WANT to pay but the credit never
  lands, so their balance stays exhausted.
- A pricing/rate-table change made normal usage suddenly exceed balances.

## Diagnosis

- Identify the orgs: the metric is deliberately label-free (no org id in
  metrics), so use the console billing view or query the credit ledger for
  orgs with non-positive balances and recent `usage_drawdown` entries.
- Check whether those orgs are suspended (the gateway suspension store) or
  still actively creating sandboxes.
- Check the billing webhook alerts: is a top-up path failure the real cause?
- Compare `mitos_usage_vcpu_seconds_total{org}` growth for the exhausted orgs
  to see whether consumption continued after exhaustion.

## Remediation

- If enforcement should have engaged: suspend the exhausted orgs via the
  kill-switch/billing suspender and fix why the automatic path did not.
- If top-up is broken: fix the webhook path first, then the balances resolve
  themselves.
- If this is legitimate growth: contact the customers (upsell/top-up) and
  consider raising signup credit or caps deliberately.

## Escalation

Deciding to bill, forgive, or suspend accumulated unpaid consumption is a
business/money decision: escalate to the billing owner with the ledger
evidence; never write manual ledger entries first.

Related alerts: `BillingWebhookVerifyFailures`, `BillingWebhookErrorRateHigh`
(broken top-up), `DrawdownFailing` (settle path itself broken).
