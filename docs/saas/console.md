# SaaS hosted web console: BFF and stack decision

Status: foundational. This ships the tested
backend-for-frontend (BFF) the console UI consumes; the SPA frontend and the
real cluster live-sandbox query are documented follow-ups below. Live log
streaming (issue #715) is wired for the husk-pod path; see the
`LogStreamer` bullet below for what is and is not covered.

The console is the human surface over the accounts/keys, usage and cost,
billing, quota, live sandboxes, and templates
services. It must match Daytona's breadth (keys, usage, billing, org/team
management) and beat both Daytona and E2B on live-sandbox inspection.

PRODUCTION GATE: the console is a new public surface. It is NOT cleared for
production tenants until the external security review covers it. See
`docs/threat-model.md`.

## Stack decision

Decision: a thin client over a server-enforced BFF.

- The verifiable, valuable core is the **BFF**: an org-scoped JSON API
  (`internal/saas/console`) that aggregates the existing services into the views
  the console needs, and enforces org-scoped data isolation SERVER-SIDE so the UI
  layer is thin and cannot leak across tenants. This is what ships and
  is tested today.
- The **UI layer** is a thin SPA (React or Next.js) that renders the BFF
  responses. It is the documented follow-up. We deliberately do NOT scaffold a
  half-built, untested SPA in this Go-centric repo: a full JS app plus a browser
  test harness cannot be meaningfully verified here, and a server that enforces
  isolation is the load-bearing property regardless of which UI framework renders
  it. A `cmd/console -dev` binary ships a minimal server-rendered index that
  lists the BFF endpoints, to prove the wiring end to end without a JS build.

Why a BFF rather than the UI calling each service directly: the console view is
a JOIN across five services (keys, usage, billing, sandboxes, templates). Doing
that join in the browser would (1) duplicate org-scoping logic in untrusted
client code, (2) expand the public surface to every backend service, and (3)
couple the UI to each service's wire shape. The BFF does the join once,
server-side, and exposes one org-scoped surface; the UI stays a thin renderer.

## What the BFF provides

`internal/saas/console.Console` is an `http.Handler` mounted at `/console/...`.
Every endpoint reads the caller account and org from the request CONTEXT
(attached by the gateway / session auth via `WithCaller`), never from a
query parameter, path, or body, and returns ONLY that org's data.

| Endpoint | Method | Reads | Backing service |
| --- | --- | --- | --- |
| `/console/keys` | GET | list keys (masked) | key service via `AccountService.ListKeys` |
| `/console/keys` | POST | create key (raw returned once) | `AccountService.CreateKey` |
| `/console/keys/{id}/revoke` | POST | revoke key | `AccountService.RevokeKey` |
| `/console/usage` | GET | current + historical usage and cost | `UsageStore` + `PriceList.Cost` |
| `/console/billing` | GET | plan/status, spend, credit balance, dunning, ledger | billing ledger + status + caps + rates |
| `/console/sandboxes` | GET | list running sandboxes | `SandboxControl` seam |
| `/console/sandboxes` | POST | create a sandbox from a template | `SandboxControl.Create` |
| `/console/sandboxes/{id}` | GET | inspect a sandbox | `SandboxControl` seam |
| `/console/sandboxes/{id}` | DELETE | terminate a sandbox | `SandboxControl` seam |
| `/console/sandboxes/{id}/fork` | POST | fork a sandbox into count copies (<=16) | `SandboxControl.Fork` |
| `/console/sandboxes/{id}/exec` | POST | run one command (<=60s timeout) | `SandboxControl.Exec` |
| `/console/sandboxes/{id}/logs/stream` | GET | live log tail over SSE | `LogStreamer` seam |
| `/console/members` | GET | org members + roles | `AccountService.ListMembers` |
| `/console/audit` | GET | org audit log | `AuditRecorder` seam |
| `/console/templates` | GET | list templates | `TemplateLister` seam |

Keys are always masked except the one-time raw key returned on create; the
stored hash is never serialized. The billing view's "invoices" are the credit
ledger entries today; real Stripe invoice objects are a follow-up.

## Org-scoped data isolation: the load-bearing property

A session or key for org A must NEVER observe org B's keys, usage, billing,
sandboxes, members, audit log, or templates through ANY console endpoint. This
is enforced at two layers and tested on every endpoint:

1. The handler reads the org ONLY from the request context. There is no code
   path that takes an org from the request.
2. The membership-guarded `AccountService` verbs and the org-scoped seams
   (`SandboxControl`, `TemplateLister`, `AuditRecorder`, the `UsageStore`, the
   billing stores) each filter to the supplied org. A cross-org sandbox id is
   reported as `not_found`, indistinguishable from a missing one, so a caller
   cannot probe another org's id space.

Tests in `internal/saas/console/console_test.go` assert, per endpoint, that a
request authenticated as org A sees only A's data and that cross-org access is
denied (403 for membership-guarded verbs) or reported as not-found (404 for a
cross-org sandbox id). `memseams_test.go` asserts the seams enforce scoping at
the seam, not just the handler.

## The instance-operator plane (`/console/admin/...`)

A SEPARATE authorization plane from everything above: every other endpoint is
scoped to the CALLER'S OWN org (org RBAC, resolved by `permissionsFor`).
`/console/admin/...` instead lets a deployment OPERATOR see every org, the
node inventory, and the signup waitlist. It is gated by `isInstanceAdmin`
(`internal/saas/console/admin.go`), never by an org role, so a org
"owner"/"admin" is not automatically an instance admin:

- an account whose email is in `MITOS_CONSOLE_INSTANCE_ADMINS`
  (case-insensitive), the hosted-deployment path; or
- the community-edition fallback: exactly one org exists on the deployment
  and the caller is that org's owner. Gated on `Edition == "community"` so a
  hosted deployment's first customer is never silently promoted.

| Endpoint | Method | Reads |
| --- | --- | --- |
| `/console/admin/overview` | GET | org count, running sandboxes across all orgs (plus `running_sandboxes_orgs`, how many orgs that rollup actually scanned), node readiness (or `null` if no `NodeSource`), signup mode |
| `/console/admin/orgs` | GET | every org's plan tier, member count, running sandboxes, month-to-date usage (capped at the oldest 200 orgs for the rollup; `total` is always the true count) |
| `/console/admin/nodes` | GET | the cluster's k8s nodes (name, ready, `mitos.run/kvm` label, `mitos.run/dedicated` taint, allocatable cpu/mem); `{"available": false}` with no Kubernetes client configured |
| `/console/admin/waitlist` | GET | recorded waitlist entries (email, recorded time) |
| `/console/admin/waitlist/{id}/approve` | POST | grants allowlist access to the entry's email and sends the "you're in" notification (`onboarding.ApproveWaitlistEntry`, the SAME mechanism `POST /internal/approve-signup` uses); 404 if the id does not decode to an email currently on the waitlist; idempotent (`"already_approved": true`, no second notification) if the email already held allowlist access |
| `/console/admin/audit` | GET | this plane's own audit events (see below) |

`Orgs` (`console.OrgDirectory`) is the one seam here that is deliberately NOT
org-scoped: it lists every organization, because it backs this operator
surface rather than a tenant-facing view (`saas.Store` satisfies it
directly). `Nodes` (`console.NodeSource`) is `nil` when no Kubernetes client
is configured; unlike every other seam in this package, `New` does NOT fill
that in with an in-memory default, since "no cluster" is a real, permanent
state the handler must report honestly rather than paper over.

`running_sandboxes` on the overview shares the SAME 200-org rollup cap
`/console/admin/orgs` has; `running_sandboxes_orgs` is how many orgs it
actually scanned (the capped subset), so the SPA can show the same "showing
first N of Orgs orgs" honesty disclosure the orgs table has, instead of
silently implying the tile covers every org once a deployment crosses the cap.

Waitlist collection over HTTP: as of issue #718, a deployment in waitlist
mode (self-serve signup disabled) mounts `POST /onboarding/waitlist {email}`
(see `docs/saas/onboarding.md`), a minimal intake distinct from the full
signup funnel, so `/console/admin/waitlist` is no longer plumbed to a
`PendingStore` that nothing can ever write to.

Every `/console/admin/...` handler is audited (`admin.*` actions,
`TargetType` `"system"` for the read views, `"waitlist"` for approve),
recorded under the reserved `OrgID` `console.InstanceAuditOrgID`
(`"_instance"`, never a real org id) so these events are not invisible to
every org-scoped audit view the way an empty `OrgID` would be; `GET
/console/admin/audit` is the one place they surface. A FAILED
`authorizeAdmin` attempt (unauthenticated, or authenticated but not an
instance admin) is also audited as `admin.denied` (`TargetType` `"system"`,
`Detail` the requested path only), so denied probing of this plane is
itself visible there.

## Live-sandbox and log-streaming approach

Live sandboxes are the deliberate differentiator. The BFF shapes the view and
enforces org-scoping NOW behind two seams:

- `SandboxControl` (list / inspect / terminate / create / fork / exec): the
  in-memory `MemSandboxControl` is the tested default. The REAL implementation
  (`internal/saas/console/clustersandbox`) queries and mutates the control
  plane (`v1.Sandbox` CRDs) scoped to one org. Create writes a `v1.Sandbox`
  sourced from the chosen pool template; Fork creates COUNT separate top-level
  `v1.Sandbox` objects (each `replicas=1`, `source.fromSandbox` set to the
  source), deliberately differing from `agentcli.ClusterBackend.Fork`'s
  one-object-with-`replicas=N` shape so every fork stays independently
  addressable through this same seam and visible as its own node in the fork
  tree; Exec dials the sandbox's own HTTP endpoint with its token Secret's
  bearer token, the same transport and Secret convention the CLI's
  `ClusterBackend` uses. Fork's partial-failure handling (issue #716): the
  Kubernetes API has no multi-object transaction, so `clustersandbox.Control.
  Fork` never rolls survivors back on an error partway through count; it
  returns whatever ids landed alongside the error instead of discarding
  them. `POST /console/sandboxes/{id}/fork` surfaces this as HTTP 207 with
  `{"ids": [...], "error": "..."}` (still 200 `{"ids": [...]}` on full
  success), audits `sandbox.fork` with the survivor count either way, and
  the SPA (the ForkTree panel and SandboxDetail's header Fork action) shows
  "created K of N" with the reason instead of a misleading plain success.
  KNOWN GAP: `SandboxSpec` has no per-sandbox resource
  override (sizing lives on the pool template), so Create's requested
  vcpu/mem are recorded as display-only annotations, not enforced; making
  per-request sizing real needs either a CRD field or a catalog of
  per-size pool templates.
- `LogStreamer` (live log tail): `clustersandbox.Control` implements it
  directly (`StreamLogs`, `internal/saas/console/clustersandbox/logs.go`),
  the same org-scoping pattern as Get/Terminate/Exec (`s.get` first; a
  cross-org or missing sandbox id is `ErrNotFound` and the pod-log transport
  is never touched). The real transport is the husk stub pod's Kubernetes
  pod-log stream with `PodLogOptions.Follow: true` (a client-go typed
  clientset; a controller-runtime client cannot open the pod-logs
  subresource), the SAME source `cmd/kubectl-mitos logs` already reads
  one-shot: `api/v1` `SandboxStatus.Pod` is the husk pod name. Lines are
  pumped through a bounded, drop-oldest buffer (512 lines) so a slow client
  cannot make the console hold unbounded memory; canceling the request
  context closes the upstream pod-log stream, actually stopping the follow,
  not just abandoning it; the pod's log stream ending (the container exited,
  the sandbox was deleted) is a clean EOF, not an error. KNOWN GAP: a sandbox
  on the raw-forkd path has no husk pod (`SandboxStatus.Pod` is empty), so it
  has no log source yet and `StreamLogs` reports `ErrUnsupported` (HTTP 501)
  for that one sandbox honestly, rather than a permanently-empty stream that
  looks like a quiet, working one; the same applies, deployment-wide, if the
  pod-logs clientset could not be built at startup. The BFF's job is still to
  AUTHORIZE the stream first, so authorization is proven independent of
  whether a transport exists.

## What ships today

- `internal/saas/console`: the BFF handler, the org-scoped seams
  (`SandboxControl`, `LogStreamer`, `TemplateLister`, `AuditRecorder`) and their
  in-memory tested defaults, and the full cross-org isolation test suite.
- `internal/saas`: `Store.ListOrgMembers` and `AccountService.ListMembers`, the
  org-scoped members read the console needs (membership-guarded).
- `internal/usage`: `PriceList.Cost` exported so the console cost view reuses the
  exact usage-API estimator.
- `cmd/console`: the BFF binary with a minimal `-dev` server-rendered index
  proving the wiring. The `-dev` header auth is a LOCAL smoke-test shim only; in
  production the org context is attached by the gateway / session auth.

## Documented follow-ups

- The `LogStreamer` real transport is wired for the husk-pod path (see the
  `LogStreamer` bullet above). Remaining gap: a sandbox on the raw-forkd path
  has no husk pod and so no log source yet; a genuinely new forkd/guest RPC
  that exposes a raw-forkd sandbox's own stdout/stderr (as opposed to the
  husk stub console) is the follow-up for that path, and continues to report
  HTTP 501 honestly until then.
- Per-request sandbox resource sizing (vcpus/mem_gib on create): `SandboxSpec`
  has no per-sandbox override field, so a console-created sandbox's actual
  resources are still whatever its pool template configures; the requested
  sizing is recorded for display only. See the `SandboxControl` bullet above.
- The `TemplateLister` over the `SandboxPool` CRDs (inline `spec.template`).
- Real Stripe invoice objects in the billing view (today it shows the credit
  ledger entries).
- Retention ENFORCEMENT for the audit log: the console stores the per-org
  retention policy (days) and the durable Postgres audit log
  (`pgstore.PgAuditLog`, wired when a database DSN is configured) keeps the
  trail across restarts, but the pruning sweep that applies the policy is the
  controller GC follow-up (issue #163). Nothing prunes today, in-memory or
  durable.
- Session-cookie auth and the gateway mount that attaches the verified org
  context in production.
