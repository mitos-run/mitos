# Runbook: GatewayErrorRateHigh

## Signal

`sum(rate(mitos_gateway_requests_total{code_class="5xx"}[5m])) /
sum(rate(mitos_gateway_requests_total[5m])) > 0.05`, with a
`> 0.1` req/s traffic floor, sustained for 10m.

`mitos_gateway_requests_total{code_class}` counts every completed public
gateway response by status class. A 5xx from the gateway means a customer got
an internal error from the API front door: the control-plane forward failed,
the runtime proxy could not reach the sandbox, or a durable store behind key
verification failed. The traffic floor stops a single failed request on a
near-idle deployment from reading as 100 percent. Threshold is
environment-tunable.

## Likely causes

- The control plane (Kubernetes API from the gateway's client) is slow or
  unreachable: every forward returns the internal envelope.
- Postgres is down, so key verification errors (see
  `ConsolePostgresUnreachable`; the gateway shares the same database).
- Sandbox runtime endpoints unreachable (forkd node loss, network policy
  change): runtime proxy calls fail while lifecycle calls still work.
- A bad rollout of the gateway or controller.
- Capacity exhaustion surfacing as create timeouts (compare
  `ClaimsPendingSustained` and `WarmPoolStarved`).

## Diagnosis

- Gateway logs: the `gateway forward` lines carry op and org; the failure
  lines name which seam failed (control plane vs body vs verify).
- Per-request latency: every forwarded request also emits ONE
  `gateway forward done` line with `status`, `forward_ms` (the control-plane
  round trip: for a create that includes the CR write and the readiness wait),
  `write_ms` and `bytes` (writing the response to the client, including a
  streamed runtime body), and `write_error` when the client write failed. A
  client-observed hang attributes to a leg from this line alone: a huge
  `forward_ms` is the control plane, a huge `write_ms` is the response path or
  the client, and a REQUEST with an entry line but no done line is still stuck
  between the two.
- Is `CanaryDown` also firing? Then the full user path is broken, not one op.
- Split by op in the logs: create-only failures point at capacity or the
  controller; runtime-only failures point at forkd/vsock.
- `kubectl -n mitos get pods` for the gateway, controller, and forkd health.
- Check the database: the gateway fails key verification closed when Postgres
  is unreachable.

## Remediation

- Control-plane outage: restore the controller / API server path; the gateway
  recovers without a restart.
- Postgres outage: restore the database (see `ConsolePostgresUnreachable`).
- Bad rollout: roll back the gateway image; the Deployment drains gracefully.
- Capacity: follow `WarmPoolStarved` / `ClaimsPendingSustained`.

## Escalation

If the 5xx source is inside the fork path (creates fail with fork errors),
escalate to the `internal/fork` / `internal/daemon` on-call
(security-sensitive paths).

Related alerts: `CanaryDown` (user-path proof), `GatewayAuthDenialSpike`
(4xx, not 5xx), `ConsolePostgresUnreachable` (shared database).
