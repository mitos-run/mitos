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
- The pepper participates in the hash, so a store dump from one deployment cannot
  be replayed against another (`TestSaltChangesHash`).

The comparison is constant-time (`crypto/subtle.ConstantTimeCompare`) so a
timing side channel cannot probe the stored hash. Key and secret VALUES are never
logged or placed in an error; the gateway logs the key id, masked prefix, org id,
and op only.

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
  payment retries exhausted). Recovery is NOT auto-lifted: a payment recovers
  the billing status, lifting the kill-switch is an operator review decision.
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
