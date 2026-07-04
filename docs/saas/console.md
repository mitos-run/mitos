# SaaS hosted web console: BFF and stack decision

Status: foundational. This ships the tested
backend-for-frontend (BFF) the console UI consumes; the SPA frontend, the real
cluster live-sandbox query, and log streaming are documented follow-ups below.

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
  `ClusterBackend` uses. KNOWN GAP: `SandboxSpec` has no per-sandbox resource
  override (sizing lives on the pool template), so Create's requested
  vcpu/mem are recorded as display-only annotations, not enforced; making
  per-request sizing real needs either a CRD field or a catalog of
  per-size pool templates.
- `LogStreamer` (live log tail): a documented seam. Unlike create/fork/exec,
  there is currently NO real transport at all (no forkd/guest RPC exposes a
  sandbox's stdout/stderr), so the real cluster deployment wires an explicit
  `UnsupportedRawLogStreamer` that reports `ErrUnsupported`, mapped to HTTP 501
  on both the plain `GET .../logs` route and the SSE `GET .../logs/stream`
  route, rather than silently serving an always-empty, always-successful
  stream. The BFF's job is still to AUTHORIZE the stream first (the sandbox
  must belong to the caller's org, otherwise `not_found`), so authorization is
  proven independent of whether a transport exists. Building the real
  transport (an HTTP chunked or websocket bridge, or a genuinely new
  forkd/guest RPC) is the follow-up.

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

- The `LogStreamer` real transport: no forkd/guest RPC exposes a sandbox's
  stdout/stderr today; the cluster deployment reports HTTP 501 honestly rather
  than faking a stream. See the `LogStreamer` bullet above.
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
