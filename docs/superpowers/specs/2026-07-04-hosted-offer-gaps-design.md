# Hosted offer gaps: design

Date: 2026-07-04. Status: approved.

## Problem

The 2026-07-04 audit compared mitos.run marketing claims against the hosted
path on main. Three load-bearing claims are not delivered (GPU is a fourth,
explicitly out of scope here):

1. "Fork a running microVM": the hosted `POST /v1/fork` maps to
   `sandbox.create` (`internal/saas/gateway.go`), the control plane always
   builds `spec.source.poolRef` and never `spec.source.fromSandbox`
   (`internal/saas/controlplane/forward.go`), and hardcodes
   `fork_time_ms: 0.0`. The live-fork engine (#596,
   `internal/controller/sandboxfork_controller.go`) is real but unreachable
   through the hosted front door.
2. "Hard spend caps are on by default": the enforcement chain on main is
   wired (console drawdown driver runs `EnforceSpendCapFromLedger` per org;
   suspension lands in the Postgres `PgSuspensionStore`; the gateway reads
   the same store via `CachedSuspensionStore`), but no default cap is ever
   seeded, so an org that never sets a cap is never suspended and credit
   exhaustion alone does not suspend. Separately, #614: the billing
   StatusStore and the org-to-provider-customer map are in-memory, so
   money-relevant state is lost on console restart.
3. "Billed per second while running": #688, a husk sandbox that hits its
   lifetime or idle limit is stamped Terminated but the in-pod VM keeps
   running and keeps being scraped and billed until the object is deleted.

This violates operating principle 1 (no unverified claims) on the product's
own front door. The goal: make the claims true, in code, with tests.

## Decisions (user-approved)

- Default spend cap: seeded fixed default (matching the signup credit,
  $5), combined with a config-level fallback so the 19 pre-existing orgs
  are covered without a backfill migration. An explicit `spend_caps` row
  always wins over the default.
- Live fork wire: a new `POST /v1/sandboxes/<id>/fork` subresource. The
  legacy `POST /v1/fork` keeps meaning create-from-template. SDK `fork()`
  on a hosted sandbox handle switches to the new endpoint; this matches the
  v2 spec, where fork of warmed state is the semantic and `SandboxFork` is
  folded into `Sandbox.source.fromSandbox`.
- GPU claims: out of scope, untouched.
- #615 items 1 and 2 (plan-backed tier resolver, real LiveUsage at the
  gateway): deferred. Pre-Paddle every org genuinely is free tier;
  concurrency caps are a separate abuse-hardening track.

## Workstream C: #688, Terminated must mean stopped

Land first; it is the smallest and it is an active billing-honesty bug.

- `terminateLifetime` for a husk-backed claim deletes the claimed husk
  pods, mirroring `reconcileDelete`, instead of calling `terminateOnNode`
  with a pod name forkd never tracked (which returns NotFound and is
  swallowed as already-terminated).
- The pool reconciler then refills the warm slot; the scrape lister
  (`usage_scrape.go`, selects PodRunning by labels) loses the pod; billing
  stops at the terminate instant.
- The #687 single terminate-time tail event already shipped (v1.19.5), so
  the phantom-rebill guard-prune path stays closed. A regression test
  covers the interplay: lifetime-expire produces pod deletion, one final
  tail sample, and no post-Terminated usage records.
- Tests: envtest for the controller change (lifetime and idle expiry both
  delete the pods; claim reaches Terminated; pool refills), plus the
  billing regression above.

## Workstream B: spend caps on by default, durable billing state

- `billing.Config` gains `DefaultCap` (env-configurable in the console,
  default $5.00). `EnforceSpendCapFromLedger` applies it whenever no
  explicit `spend_caps` row exists for the org. An explicit row always
  wins, including an explicit higher cap.
- Org provisioning (onboarding) seeds an explicit `spend_caps` row at the
  default, so the console UI shows a concrete, adjustable cap for every
  new org.
- Resume is a first-class behavior: raising the cap or topping up credit
  unsuspends on the next drawdown cycle. If the current dunning state
  machine lacks the transition, add it. A suspended org must not be a dead
  end (journey rules).
- #614: `PgStatusStore` and `PgCustomers` in `internal/saas/pgstore` with
  migrations, following the existing patterns (`PgCreditLedger`,
  `PgSpendCapStore`, `PgSuspensionStore`); wired in `cmd/console` when
  Postgres is configured; in-memory stays the dev fallback. Tests cover
  restart survival for both stores.
- Tests: unit tests for default-cap resolution precedence (no row, row
  lower, row higher), an integration-shaped test that an org with no
  explicit cap suspends when its ledger spend passes the default, and the
  resume-on-topup and resume-on-raise cycles.

## Workstream A: hosted live fork

- Gateway: new route `POST /v1/sandboxes/<id>/fork` mapped to a new op
  `sandbox.fork`, gated by the same quota checks as `sandbox.create`
  (creation rate, per-sandbox size, suspension), emitting a
  `sandbox.forked` telemetry event on success.
- Control plane: the handler resolves the source sandbox via the existing
  `getOwned` org-label check (namespace is deterministic:
  `tenant.NamespaceForOrg`), then materializes a Sandbox with
  `spec.source.fromSandbox.Name = <source id>` in the org namespace with
  the org label. Request body: `{id?, secret_inheritance?, pause_source?}`.
  One child per call; SDK `fork(n)` loops, as the SDKs' `_fork_one`
  already does. Response mirrors the create response, with a real
  `fork_time_ms` measured value replacing the hardcoded `0.0`.
- Secrets: the default stays `reissue`. A fork of a secret-holding
  sandbox without `secret_inheritance: "inherit"` returns the LLM-legible
  structured error (`{error:{code, message, cause, remediation}}`) naming
  the remedy; the existing controller gate (`SecretInheritanceDenied`) is
  surfaced through the wire, not duplicated.
- Billing: verify that fork children pods carry the org and claim labels
  the usage scraper selects on, so forked sandboxes are billed exactly
  like created ones. Fix in the same PR if they do not.
- SDKs: Python and TypeScript hosted `fork()` switch to the new endpoint.
  Go, Ruby, Rust, Java parity is a tracked follow-up issue, not this
  slice.
- Documented constraint: children pin to the source's node by construction
  (the fork snapshot is node-local), so fork fan-out is bounded by that
  node's capacity. Documented, not hidden.
- Threat-model delta in the same PR: a new authenticated fork surface and
  secret-inheritance exposure over the hosted wire.

## Packaging and sequencing

Three independent PR tracks, landed C then B then A, each developed in a
`.claude/worktrees/` worktree, tests-first, docs updated in the same PR,
conventional commits with DCO sign-off. After all three land and roll to
prod, the pricing and landing copy is true without edits (the GPU row
excluded, untouched, per scope).

## Acceptance

- C: a hosted sandbox that hits lifetime or idle limit stops running and
  stops billing at the terminate instant; proven by envtest plus the
  billing regression test.
- B: a fresh org with no explicit cap suspends at $5 of metered spend and
  unsuspends on top-up or cap raise; billing status and customer mapping
  survive a console restart; proven by unit and restart-survival tests.
- A: `sandbox.fork()` from the Python or TypeScript SDK against
  api.mitos.run produces a child that inherits the parent's current
  runtime state (verified live: write a file plus mutate memory in the
  parent, observe both in the child), is billed, and respects the secrets
  gate.
