# Runbook: GatewayAuthDenialSpike

## Signal

`sum(rate(mitos_gateway_auth_denials_total[5m])) > 1` sustained for 10m.

`mitos_gateway_auth_denials_total{reason}` counts requests denied at
authentication: `missing_key` (no bearer presented), `unauthorized`
(malformed, unknown, expired, or revoked key; collapsed like the public 401 so
the metric cannot be used to probe which), `forbidden` (valid key, disallowed
scope or wrong org). Quota and rate-limit denials are deliberately excluded.
The healthy baseline is near zero because deployed SDKs hold valid keys, so a
sustained 1/s is far above organic retry noise. Threshold is
environment-tunable.

## Likely causes

- Credential scanning or brute-force probing of the public endpoint.
- A key that was revoked or expired while still deployed in a customer
  integration that retries in a loop.
- An auth regression after a rollout (every request suddenly denied).
- A misconfigured client (wrong header shape) hammering the API.

## Diagnosis

- Split by reason: `sum by (reason) (rate(mitos_gateway_auth_denials_total[5m]))`.
  - Mostly `missing_key` or `unauthorized` from varied sources: scanning.
  - Steady `unauthorized` at a constant rate: one broken integration retrying.
  - `forbidden`: a valid key being used beyond its scope, possibly a confused
    customer script or a probing insider.
- Gateway logs carry the key id and masked prefix for denials with a resolved
  key (never the key value); use them to identify the org.
- Did total request volume rise with the denials (attack) or stay flat with
  successes dropping to zero (regression)?

## Remediation

- Scanning: confirm the per-IP rate limit is engaged (it is on by default via
  the quota enforcer); consider tightening `gateway.enforce.trustedProxyHops`
  correctness so the limiter keys the real client IP.
- Broken integration: identify the org from the key id in the logs and contact
  the customer; the denial itself is correct behavior.
- Regression: roll back the gateway; verify a known-good key against /v1
  before closing.

## Escalation

A sustained, distributed credential-scanning campaign against the public front
door is a security event: record it and follow SECURITY.md. The gateway is a
security-sensitive surface (docs/threat-model.md section 7b).

Related alerts: `GatewayErrorRateHigh` (5xx, not auth), `CanaryDown` (the
canary's key failing would also fire this).
