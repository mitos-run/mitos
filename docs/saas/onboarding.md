# Self-serve onboarding funnel

The hosted offering's onboarding funnel takes a developer from signup to a first
successful `run_code` in minutes, with no card on the free tier, exactly one SDK
package, and a copy-paste snippet that works first try. This is the most-cited
reason developers pick a sandbox provider, so the funnel is built as a tested
core with clean seams, and the "fast self-serve UX" claim is measured, not
asserted.

Implementation: `internal/saas/onboarding`. It composes the
accounts/keys layer, the credit ledger, and the free-tier defaults.

## The funnel

In open self-serve mode the flow is:

1. **Sign up (email).** `Service.SignUp(ctx, email)` creates a pending,
   unverified signup and sends a verification token by email. The raw token goes
   only to the user's inbox; the store keeps only its hash.
2. **Verify (token).** The user clicks the link; `Service.Verify(ctx, rawToken)`
   accepts the token and, in one step, provisions everything below.
3. **Org auto-created.** A Personal organization is created (Daytona-style), so a
   brand-new user can act immediately without first creating a team. This reuses
   `AccountService.SignUp`.
4. **Free signup credit granted.** The free-tier signup credit lands on the new
   org via the credit ledger (`billing.GrantSignupCredit`). The grant is
   idempotent per org, so a retried verify never double-grants.
5. **First API key issued.** A scoped key (the `sandboxes` scope) is minted for
   the org; the raw key is shown exactly once.

The user then drops straight into the [quickstart](../quickstart.md): one
`pip install mitos-run` (installs the `mitos-run` distribution; you still `import
mitos`), one `mitos.create(...)`, code execution, no second SDK.

### Idempotency and rejection

- An **invalid** verify token is rejected with `ErrTokenInvalid`; an **expired**
  token (past the 24h default TTL) with `ErrTokenExpired`. Neither error reveals
  whether the token was unknown or merely wrong, and neither carries the raw
  token or the email.
- **Re-verifying** with the same token after success is idempotent: it returns
  the same account and org with `AlreadyDone` set, and grants no second credit
  and issues no second key.

## Free-credit grant

The signup credit is wired to the credit ledger and defaults to
`billing.DefaultSignupCredit()` (the $100 illustrative default, inside the
$100-$200 self-serve bar; coordinated with the ledger's default and configurable
per deployment via `WithSignupCredit`). The credit lands exactly once per org;
the unit suite asserts the balance after a re-verify is still a single grant.
These amounts are illustrative defaults, not published promises (no unverified
claims).

## Availability gate: waitlist vs open self-serve

Per the hosted-SaaS gate, the funnel does NOT run open public self-serve
until the production gates pass:

- chaos suite and residual garbage collection,
- external security review,
- multitenancy isolation.

Until then the funnel runs in **waitlist / design-partner mode** (`ModeWaitlist`,
the default). In waitlist mode `SignUp` records a waitlist entry and provisions
nothing: no account, no org, no credit, no key, no verify email. A deployment
flips to `ModeOpen` only once the gates above are green. The mode is a single
flag (`WithMode`) so the gate is one explicit, auditable decision, and both modes
are unit-tested.

`POST /onboarding/signup` (the full funnel) is mounted only when
`MITOS_CONSOLE_SIGNUP` is on; a waitlist-mode deployment does NOT mount it
(there is no verify token to redeem and no account is ever provisioned in
this mode). Instead `cmd/console/onboarding.go`'s `mountWaitlistOnly` mounts a
minimal, standalone `POST /onboarding/waitlist {"email": "..."}` (issue
#718): always the same uniform 202 whether the join is fresh, a duplicate
(`Service.signUp` dedupes by canonical identity so a repeat submission is a
no-op, not a second row), or rate-limited (a modest fixed per-IP cap), so it
never enumerates waitlist membership. It shares the SAME `PendingStore` the
open-mode path and `console.Deps.Waitlist`'s `waitlistAdapter` read, so a
waitlist entry recorded here is immediately visible to
`GET /console/admin/waitlist`.

## Funnel instrumentation and metrics

So the UX claim is verified, the funnel records timestamped events through an
`EventRecorder` seam (in-memory implementation is the tested default; a real
analytics sink and the live time-to-first-sandbox dashboard are follow-ups):

| Event | Meaning |
| --- | --- |
| `signup_started` | email submitted, unverified |
| `waitlisted` | signup landed on the waitlist (waitlist mode) |
| `verified` | verify token accepted, org provisioned |
| `key_issued` | first API key issued |
| `first_sandbox_created` | org created its first sandbox |
| `first_exec` | org ran code for the first time |

Events carry no PII and no secrets: the waitlist subject is keyed on a hash of
the email, never the address itself.

`AggregateFunnel` (and `Service.FunnelStats`) turns the event stream into:

- **per-step conversion rate** along the funnel order, and
- **median transition time** per step, plus
- the headline **median time-to-first-sandbox** (`signup_started` ->
  `first_exec`).

Aggregation is pure and deterministic and takes the earliest timestamp per
(subject, step) so a replayed event cannot skew timing. The unit suite asserts
the conversion and timing math.

## Seams and follow-ups

The verifiable core is unit-tested: the signup -> verify -> provision flow, the
idempotent credit grant, the funnel stats, and the waitlist-vs-open gate. These
are documented seams / follow-ups:

- **Email provider.** `EmailSender` is an interface; the tested default is
  `FakeEmailSender`. The real SMTP/provider integration is a follow-up. The raw
  token is never logged by any sender.
- **Web signup UI.** The hosted console drives this service; what ships here is
  the service, not the UI.
- **Live dashboard.** The in-memory `EventRecorder` is the seam for a real
  analytics sink and the live time-to-first-sandbox dashboard.
- **Durable store.** `PendingStore` and the accounts `Store` are in-memory here; the
  Postgres store is a follow-up behind the same interfaces.

## Security notes

- Email addresses are PII and verify tokens are secrets: tokens are stored only
  as a hash, never logged, never placed in an error; raw emails are not logged.
- Verify errors are indistinguishable between unknown and wrong tokens, so a
  probe learns nothing.
