# SaaS accounts, organizations, and the customer API gateway

Status: foundational slice (issue #210). This is the customer-facing FRONT DOOR
for the hosted offering. It is layered ABOVE the internal mTLS and per-sandbox
token plane (#4, `internal/admission/claim_principal.go`); it does not replace
it. It is the ROOT dependency for the SaaS epic (#208): billing (#212), quota
and abuse controls (#213), usage metering (#211), the web console (#214), and
onboarding (#215) all sit on the account, org, and key model defined here.

PRODUCTION GATE: this front door adds a new public attack surface (an
internet-facing listener, customer-presented credentials, cross-tenant
isolation). It is NOT cleared for production tenants until the external security
review (#194) covers it. See `docs/threat-model.md` (the "Customer front door"
section).

## What this slice ships

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
with the doc, the JSON Schema, and `llms.txt` (the #28 sync tests stay green):

- `forbidden` (403): the key is valid but not permitted, a missing scope or a
  cross-org resource. Distinct from `unauthorized` (no valid credential).
- `quota_exceeded` (429): the org hit a hosted-plan quota at the gateway.
  Distinct from the per-sandbox `budget_exhausted` and `rate_limited`.

A missing, malformed, unknown, expired, or revoked key all collapse to
`unauthorized` so a probe cannot distinguish them.

## Seams for the rest of the epic

- Store seam (`Store`): #211 (usage) and the Postgres migration follow-up.
- Quota seam (`QuotaEnforcer`, default `AllowAllQuota`): #213 implements the real
  enforcer; the gateway already calls `Check` after authn and org-resolution and
  before forwarding.
- Control-plane forward seam (`ControlPlane`): the real target forwards over the
  internal mTLS plane to the controller; this slice ships an injectable interface
  and a stub binary target.
- Session seam (`SessionStore`): the browser OAuth login flow (#215) plugs in
  here; this slice is token-based.
- Billing (#212) reads the org and usage model; metered events key off the org.

## Documented follow-ups (NOT in this slice)

- Postgres store implementation and migrations.
- The real control-plane forward target (mTLS to the controller).
- Browser-based OAuth login (`mitos auth login` without `--token`).
- The hosted deployment, TLS termination, and rate-limiting at the edge.
- The external security review (#194) that gates production.
