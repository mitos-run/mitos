# SaaS accounts, organizations, and the customer API gateway

Status: foundational. This is the customer-facing FRONT DOOR
for the hosted offering. It is layered ABOVE the internal mTLS and per-sandbox
token plane (`internal/admission/claim_principal.go`); it does not replace
it. It is the ROOT dependency for the hosted SaaS offering: billing, quota
and abuse controls, usage metering, the web console, and
onboarding all sit on the account, org, and key model defined here.

PRODUCTION GATE: this front door adds a new public attack surface (an
internet-facing listener, customer-presented credentials, cross-tenant
isolation). It is NOT cleared for production tenants until the external security
review covers it. See `docs/threat-model.md` (the "Customer front door"
section).

## What this layer ships

A real, fully unit-tested core:

- Data model (`internal/saas/model.go`): `Account`, `Organization`,
  `Membership`, `ApiKey`. Everything a customer owns is ORG-scoped: sandboxes,
  usage, and quota all hang off an organization, never a bare user. A new user
  gets a `Personal` organization by default (the Daytona-style default) and is
  its owner, so they can act immediately without creating a team.
- Pluggable store (`internal/saas/store.go`): the `Store` interface with an
  in-memory implementation (`MemStore`) as the tested default. Postgres is a
  documented follow-up; it plugs into the same interface.
- Key service (`internal/saas/keys.go`): prefix-tagged keys (`mitos_live_...`),
  hashed at rest with a process pepper, never stored in the clear. Verification
  is constant-time and checks shape, resolution, expiry, revocation, and scope.
  Only a masked prefix is shown after creation; the raw key is returned exactly
  once.
- Account service (`internal/saas/account.go`): sign-up (account plus personal
  org plus owner membership), org resolution, and the membership-guarded key
  verbs (create, list, revoke) the CLI and console call.
- Sessions (`internal/saas/session.go`): token-based session resolution, hashed
  at rest, the seam the browser OAuth login flow plugs into later.
- Public gateway (`internal/saas/gateway.go`, binary `cmd/gateway`): terminates
  customer key auth, resolves the org, attaches org context, enforces quota via
  the `QuotaEnforcer` seam, and forwards to the control plane via the
  `ControlPlane` seam. Maps internal failures to the public error envelope
  (`internal/apierr`).
- CLI (`internal/agentcli/auth.go`): `mitos auth login --token`, plus
  `mitos auth keys create|ls|revoke`, wired into the existing `mitos` CLI and
  unit-tested against a fake account service.

## Org-scoped authorization and cross-org isolation

The load-bearing security property: a key for organization A can NEVER touch
organization B's resources. This is enforced at three layers, each unit-tested:

1. Key resolution (`KeyService.Verify`): a key resolves ONLY to the org it was
   minted for. There is no input by which a verifier makes a key resolve to a
   different org; resolution is by salted hash lookup.
2. Gateway forward (`Gateway.ServeHTTP`): the `OrgID` the gateway forwards is
   taken SOLELY from the verified key, never from the request body or path. A
   key for org A that stuffs `"org":"org-b"` into the body is still forwarded
   with `OrgID = org-a`. Test: `TestGatewayCrossOrgIsolation`.
3. Management verbs (`AccountService`): create, list, and revoke reject a caller
   that is not a member of the target org, so a user cannot mint, list, or
   revoke another org's keys even with the org id or key id. Tests:
   `TestCreateKeyForOtherOrgIsRefused`, `TestListKeysForOtherOrgIsRefused`,
   `TestRevokeOtherOrgKeyIsRefused`.

## Key security properties (proven by tests)

`internal/saas/keys_test.go`:

- The raw key is returned once and only a salted hash plus a masked prefix is
  stored (`TestCreateKeyReturnsRawOnceAndStoresOnlyHash`).
- Verify accepts a genuine key (`TestVerifyAcceptsAGenuineKey`) and rejects: a
  forged key (`TestVerifyRejectsForgedKey`), a malformed credential
  (`TestVerifyRejectsMalformedKey`), an expired key
  (`TestVerifyRejectsExpiredKey`), a revoked key (`TestVerifyRejectsRevokedKey`),
  and a key without the required scope (`TestVerifyRejectsWrongScope`).
- A key for org A resolves to org A and only org A
  (`TestOrgAKeyCannotResolveOrgB`).
- Scopes are enforced with an implication graph and are backward compatible: a
  read-only key is refused every mutation, an execute key is refused create, a
  lifecycle key is refused in-sandbox exec, and a legacy scopeless key keeps full
  access (`internal/saas/scopes_test.go`, `internal/saas/gateway_scopes_test.go`).
- The pepper participates in the hash, so a store dump from one deployment cannot
  be replayed against another (`TestSaltChangesHash`).

The comparison is constant-time (`crypto/subtle.ConstantTimeCompare`) so a
timing side channel cannot probe the stored hash. Key and secret VALUES are never
logged or placed in an error; the gateway logs the key id, masked prefix, org id,
and op only.

## API key scopes (issue #784)

An API key is minted with one or more SCOPES that shrink its blast radius below
the whole org, so a team can embed a browser-safe or CI-safe key that can do
strictly less than "everything the org can do". Scopes are enforced in the
gateway authz path (`requiredScopeFor` in `internal/saas/gateway.go`), and the
key model applies a small implication graph (`ApiKey.HasScope`,
`scopeSatisfies` in `internal/saas/model.go`).

The scope vocabulary:

- `read`: read-only surfaces (list, get, status, template list).
- `execute`: acting inside an existing sandbox (exec, files, run_code, the
  runtime proxy). It does NOT permit creating or destroying sandboxes.
- `lifecycle`: creating, forking, and terminating sandboxes, plus the
  pause/resume state verbs.
- `admin`: organization management (API keys, billing). It is orthogonal to the
  resource scopes (an admin key gains no sandbox read/execute/lifecycle reach on
  its own). There is no API-key-reachable admin operation on the public gateway
  yet: key and billing management run through the session-authenticated console,
  so this scope is minted, persisted, and shown today and gates the gateway
  admin surface when it lands.

Implication rules (never the reverse):

- `read` is the floor every resource scope grants, so a key that can act on a
  sandbox can always list and status it (no dead end).
- `execute` and `lifecycle` are orthogonal to each other; a key can carry one
  without the other.
- `admin` implies nothing and is implied by nothing.
- The LEGACY `sandboxes` scope (the pre-#784 onboarding default) satisfies
  `read`, `execute`, and `lifecycle` (but not `admin`), so every existing key
  keeps working unchanged.

Operation to required-scope map enforced at the gateway:

| Operation | Required scope |
|---|---|
| `sandbox.list`, `sandbox.status`, `template.list` | `read` |
| exec, files, run_code (runtime proxy) | `execute` |
| `sandbox.create`, `sandbox.fork`, `sandbox.terminate`, `sandbox.pause`, `sandbox.resume`, `template.ensure` | `lifecycle` |
| any unmapped op | `lifecycle` (fail closed) |

Backward compatibility (mandatory):

- A key stored with NO scopes recorded is a legacy full-access key and satisfies
  EVERY scope, so an existing caller is never locked out
  (`TestLegacyScopelessKeyHasFullAccess`, `TestGatewayLegacyScopelessKeyRetainsFullAccess`).
- A newly minted key defaults to the full scope set when none is named at mint
  (`CreateKey` calls `FullScopes()`; `TestCreateKeyDefaultsToFullScopes`), so
  existing tooling that mints without scopes still gets a full key.
- A scope outside the known vocabulary is refused at mint (`ErrUnknownScope`,
  `TestCreateKeyRejectsUnknownScope`) rather than silently minting a key that
  grants nothing.

A scope denial is a `forbidden` (403) whose remediation names the exact scope
the operation needs and whose context reports `op` and `required_scope`, so an
agent can self-correct without probing (the #28 LLM-legible error rule). The
scope name is not a secret.

The optional PER-RESOURCE qualifier (a scope narrowed to a named sandbox or
pool) is a documented FOLLOW-UP, not part of this slice: it is the next step
toward the capability-budget model in `docs/api/v2-spec.md`.

## Public error envelope

The gateway reuses the LLM-legible envelope (`internal/apierr`,
`docs/api/errors.md`). Two codes were added for the front door and kept in sync
with the doc, the JSON Schema, and `llms.txt` (the error-catalogue sync tests stay green):

- `forbidden` (403): the key is valid but not permitted, a missing scope or a
  cross-org resource. Distinct from `unauthorized` (no valid credential).
- `quota_exceeded` (429): the org hit a hosted-plan quota at the gateway.
  Distinct from the per-sandbox `budget_exhausted` and `rate_limited`.

A missing, malformed, unknown, expired, or revoked key all collapse to
`unauthorized` so a probe cannot distinguish them.

## Live fork: POST /v1/sandboxes/{id}/fork

The per-sandbox fork route is a TRUE live fork on the hosted API (it used to
be a template re-fork stopgap): the control plane resolves the org-owned
source from the path and submits a Sandbox whose source is
`spec.source.fromSandbox`, so the child inherits the source's current memory
and on-disk filesystem, not a fresh boot of the cold template. The flat
`POST /v1/fork` route is unchanged: it names no source sandbox and stays a
create-from-template.

Request body (all fields optional):

- `id`: the child sandbox id. DNS-1123 validated (typed 400 on violation);
  generated when omitted.
- `pause_source`: freeze the source VM while it is checkpointed so its memory
  and disk are captured consistently. It pauses only the caller's own
  org-owned sandbox.
- `secret_inheritance`: `"reissue"` (default: the child gets fresh
  credentials) or `"inherit"` (explicit opt-in: the child duplicates the
  source's in-memory secrets). A source that holds secrets is NOT forkable
  without the opt-in: the controller's default-deny surfaces as a 403 whose
  remediation names the exact field and value to send.

Response (201): `{id, endpoint, token, phase, template_id, fork_time_ms}`,
the same shape as create. `template_id` is the SOURCE's pool; `fork_time_ms`
is the engine-measured child startup latency (wall-clock fallback), never a
hardcoded zero. The child's token is freshly issued; the source's token never
opens the child.

Semantics and limits:

- The source is resolved org-scoped (`getOwned`): a foreign or missing source
  id answers an indistinguishable 404 and never forks.
- The source must be Ready (a live fork copies running memory); anything else
  is an instant 409 with remediation.
- One child per call; the SDKs' `fork(n)` calls the route n times.
- A fork counts as a create for quota (concurrency cap, creation-rate bucket)
  and is billed like any claimed sandbox.
- Children run on the source's node by construction (the fork copies
  already-resident guest memory in place), so fan-out is bounded by that
  node's capacity.

## Quota enforcement status (what is real today)

The gateway is the ONLY enforcement point for the hosted tier caps; the
control plane does NOT re-check them. `cmd/gateway` wires the real
`quota.Enforcer` by default (`--enforce-quota=true`) and the startup log names
the exact mode. Honest per-cap status (issue #615):

- Kill-switch suspensions: ENFORCED. The gateway reads the durable Postgres
  `suspensions` table (behind a short-TTL fail-closed cache), the same table
  the console's billing paths write, so a billing-driven or abuse-driven
  suspend binds every replica within the cache TTL (a few seconds). Two
  billing paths fire it in production: the drawdown cycle evaluates each
  active org's HARD spend cap right after settling its usage
  (`billing.Service.EnforceSpendCapFromLedger`: period spend is the calendar
  month's usage-drawdown debits; metered overage is not yet included since no
  invoice source exists pre-#618), and the provider webhook suspends on the
  transition into the suspended status (subscription canceled or paused,
  payment retries exhausted). Recovery is payment-driven and reason-scoped: a
  cleared paid top-up lifts a spend_cap suspension (the spend window resets at
  the payment) and an active-status transition lifts a dunning suspension;
  the automated lift can never touch another reason or any manual-hold
  suspension, which clear only through the operator hook.
- Request-rate and creation-rate caps: ENFORCED, per org and per source IP.
- Concurrent-sandbox cap: ENFORCED at the gateway from LIVE Kubernetes state.
  `controlplane.LiveCounter` counts the org's Sandbox objects in a
  non-terminal phase (namespace plus org label scoped, the same model the
  control plane uses, including single-tenant mode) on every create, counting
  `spec.replicas` per object (a fork fan-out of N is N VMs). A count failure
  DENIES the create (fail closed): an unreachable apiserver never reads as
  "zero live sandboxes"; the gateway runs one startup self-check so a
  persistent RBAC or scheme misconfiguration (which would deny every create)
  is loud at boot. Honest limits: admission is check-then-create with NO
  reservation, so a parallel create burst can overshoot the cap by up to the
  tier's creation-rate burst; Terminating counts as live (it holds capacity),
  so a wedged teardown consumes a slot until resolved; a create asking for
  `replicas: N` is admitted as +1 rather than +N (the body is not parsed at
  enforcement time; the fan-out is counted from the next create on); each
  create costs one uncached org-scoped LIST against the apiserver, fine at
  current fleet sizes.
- Per-sandbox size caps (vCPU, memory, storage): NOT yet enforced. The
  gateway adapter's `SizeOf` seam is unwired, so a create is checked with a
  zero spec and the size cap cannot trip.
- Aggregate resource caps (vCPU, memory, storage across the org): NOT yet
  enforced. Sandboxes carry no per-create resource fields, so honest
  aggregates need each sandbox's pool-resolved footprint; the live counter
  deliberately reports zero for these rather than guessing. Wiring `SizeOf`,
  the pool-resolved aggregates, admission-side replicas (+N at create time),
  and a cached or paginated counter is the deferred remainder of issue #615.
- Tier resolution: every org currently resolves to the FREE tier (the
  tightest caps, deny-by-default egress). A plan-backed resolver is pending
  the issue #615 / #618 product decisions; failing to the smallest tier is
  the deliberate interim posture, never a default to a wider one.

## Seams for the rest of the offering

- Store seam (`Store`): usage and the Postgres migration follow-up.
- Quota seam (`QuotaEnforcer`): the real `quota.Enforcer` is wired by default
  in `cmd/gateway` (see the status section above); `AllowAllQuota` remains the
  explicit opt-out for trusted single-tenant deployments and the bypass is
  logged at startup. The gateway calls `Check` after authn and org-resolution
  and before forwarding.
- Control-plane forward seam (`ControlPlane`): the real target forwards over the
  internal mTLS plane to the controller; this layer ships an injectable interface
  and a stub binary target.
- Session seam (`SessionStore`): the browser OAuth login flow plugs in
  here; this layer is token-based.
- Billing reads the org and usage model; metered events key off the org.

## Documented follow-ups

- Postgres store implementation and migrations.
- The real control-plane forward target (mTLS to the controller).
- Browser-based OAuth login (`mitos auth login` without `--token`).
- The hosted deployment, TLS termination, and rate-limiting at the edge.
- The external security review that gates production.
