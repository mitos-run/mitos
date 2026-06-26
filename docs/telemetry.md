# Product telemetry

Mitos can emit a small set of privacy-first PRODUCT-USAGE events so the operators
of a hosted deployment can measure adoption. This is separate from
[observability](observability.md) (OpenTelemetry distributed tracing): tracing
records request-path spans for debugging, while product telemetry records a few
discrete usage events for analytics. They share no data model and no transport.

The defining property is privacy:

- **Opt-in and off by default.** A default install emits nothing and runs a no-op
  emitter. Telemetry turns on ONLY with an explicit opt-in AND a configured sink
  endpoint.
- **DO_NOT_TRACK is honored.** A `DO_NOT_TRACK=1` (or `true`) environment variable
  force-disables telemetry, overriding every other setting.
- **No PII.** The organization id is sent only as a salted one-way hash; with no
  salt configured the id is dropped entirely. Account emails, IP addresses,
  secrets, environment variable values, and sandbox contents are never collected.

## What is collected

When telemetry is enabled, the hosted binaries emit these events and no others:

| Event | Emitted by | When | Properties |
| --- | --- | --- | --- |
| `sandbox.created` | gateway | a customer API create succeeds | `success` (always true), `pool` (the non-identifying pool name, when the control plane reports it) |
| `signup.started` | console / onboarding | a self-serve signup begins | `funnel_subject` (an opaque, non-PII signup id) |
| `signup.verified` | console / onboarding | a signup completes email verification | `funnel_subject` (an opaque, non-PII signup id) |

Every event also carries:

- `name`: the event name above.
- `org_hash`: the salted hash of the organization id, OR omitted when no salt is
  configured. The raw organization id is never sent.
- `timestamp`: the event time.

The on-wire shape is JSON. The HTTP sink POSTs a batch as
`{"events": [ ... ]}`; the stdout sink writes one JSON event per line.

### The no-PII guarantee

- The organization id is hashed with HMAC-SHA256 keyed by a configured salt, so
  the hash is not reversible to the id and not guessable without the salt. With no
  salt set, the id is dropped (fail closed), never sent raw.
- Account emails are never passed to telemetry. The onboarding funnel keys on an
  opaque signup id, not the email.
- Event properties pass a deny-list that drops any key whose name suggests PII or a
  secret (for example `email`, `ip`, `token`, `secret`, `password`, `auth`,
  `cookie`, `address`, `phone`, `name`, `key`, `credential`). The documented
  convention for callers is to attach only counts, names, and tiers. The deny-list
  is a guardrail against accidental PII, not a substitute for the convention.
- The salt and any collector token are secrets. They are never logged. The startup
  log line states only whether telemetry is enabled and the sink name; it never
  prints the salt, the endpoint, or any credential.

## Enabling telemetry (hosted operators)

Telemetry is configured by environment variables on the gateway and console
binaries:

| Variable | Meaning |
| --- | --- |
| `MITOS_TELEMETRY_ENABLED` | `true`/`1` to opt in. Default off. |
| `MITOS_TELEMETRY_ENDPOINT` | the collector URL the network sink POSTs JSON to. Required; empty keeps telemetry disabled. |
| `MITOS_TELEMETRY_SALT` | the HMAC salt used to hash the org id. Secret. With no salt the org id is dropped. |
| `MITOS_TELEMETRY_TOKEN` | optional bearer token for the collector. Secret. |
| `MITOS_TELEMETRY_OPTOUT` | `true`/`1` to force-disable even when enabled. |
| `DO_NOT_TRACK` | `true`/`1` force-disables telemetry unconditionally. |

Telemetry runs only when `MITOS_TELEMETRY_ENABLED` is truthy AND
`MITOS_TELEMETRY_ENDPOINT` is set AND neither `MITOS_TELEMETRY_OPTOUT` nor
`DO_NOT_TRACK` is set. When anything is missing or ambiguous, the binary fails
closed to a no-op emitter.

### Helm

The chart wires the same env behind the `telemetry` values block. It is off by
default and renders no telemetry env unless both `telemetry.enabled` and
`telemetry.endpoint` are set:

```yaml
telemetry:
  enabled: true
  endpoint: "https://collector.example.com/ingest"
  # The salt and token are SECRETS, injected via secretKeyRef ONLY. Create the
  # Secret out of band, then point the refs at it.
  saltSecretRef:
    name: mitos-telemetry
    key: salt
  tokenSecretRef:
    name: mitos-telemetry
    key: token
```

```sh
kubectl create secret generic mitos-telemetry \
  --namespace mitos-system \
  --from-literal=salt="$(head -c 32 /dev/urandom | base64)" \
  --from-literal=token='<optional-collector-token>'
```

With no `saltSecretRef.name` set, the salt env is omitted and the org id is
dropped from every event.

## Pointing telemetry at your own pipeline (self-host)

A self-host operator who wants product analytics on their own infrastructure has
two options:

- **HTTP sink:** set `MITOS_TELEMETRY_ENDPOINT` to your own collector (a
  PostHog-compatible, Segment-compatible, or custom JSON ingest endpoint). The
  binary POSTs batches of sanitized events; no data leaves your network except to
  the endpoint you configured.
- **Stdout sink:** use the `StdoutSink` to write JSON-Lines to stdout and ship the
  lines with your existing log forwarding, with no network egress from Mitos. The
  lines carry only sanitized events (hashed org id, deny-listed properties).

## How to disable telemetry

Telemetry is already off by default. To guarantee it stays off regardless of any
other configuration, set either:

- `DO_NOT_TRACK=1` (the cross-vendor signal), or
- `MITOS_TELEMETRY_OPTOUT=1` (the Mitos-specific opt-out, `telemetry.optOut=true`
  in Helm).

Either one force-disables telemetry: the binary runs a no-op emitter and emits
nothing.
