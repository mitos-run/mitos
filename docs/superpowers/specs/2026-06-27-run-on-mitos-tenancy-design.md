# Run-on-Mitos Tenancy: per-user vs shared instances

> Status: design draft for review. Surfaced by the OpenClaw live demo, which forced
> the question "who gets which instance, and who is allowed in" to the front.

## Goal

Let a `mitos.yaml` author declare, in one place, **how an exposed app is
multi-tenanted** and **who may reach it** - and have mitos route each caller to the
right instance the most efficient, k8s-native, hard-isolated way. The OpenClaw case
is the forcing function: a personal AI gateway must give every user their **own**
isolated instance, gated by **their** identity; it must never be one shared,
ungated box.

## Two independent axes

Tenancy and auth are orthogonal. `mitos.yaml` expresses both on the existing
`expose` block:

```yaml
expose:
  sharing:  authenticated      # WHO may reach it (the auth ladder, #407 - already built)
                               #   public | link | authenticated | org | private
  tenancy:  per-user           # WHICH instance they reach (NEW)
                               #   per-user | shared
```

- **`sharing` (auth) - already built (#407).** The edge proxy gates the URL by
  verified mitos identity (email -> org), with the public/link/authenticated/org/
  private ladder. `public` is ungated; the others require a session and resolve a
  caller `Identity`. Nothing new is needed here; flipping `sharing` from `public`
  to `authenticated` gates OpenClaw by the user immediately.
- **`tenancy` - the missing piece.** Given the resolved identity, which backend
  instance does the request reach?

### tenancy: shared (today's behavior)

One golden snapshot, one warm pool, one (or a few) fork(s) serve **all** callers.
Correct for stateless / read-only apps. Auth-gated or public, independent of
tenancy. This is what the expose path does today: a single `Sandbox` with
`spec.expose`, one route, all traffic.

### tenancy: per-user (the new model)

Each **authenticated identity** is routed to **its own fork**. On a request to
`<label>.<domain>`:

1. The edge resolves the caller `Identity` from `sharing` (must be a gated tier;
   `per-user` + `public` is a config error - no identity to key on).
2. The edge asks the controller for **this identity's instance** of this app.
3. If one exists and is Ready, route to it. If not, **fork-on-demand** from the
   golden snapshot (capture-running, ~86 ms) and route to the new fork.
4. Idle per-user instances **scale to zero** (reaped after a TTL); a fresh request
   re-forks instantly. A **warm pool** keeps N pre-forked, unclaimed instances so
   even a cold user gets a sub-second start.

Per-user state lives in the user's own microVM; per-user secrets are injected
per-fork (`secretInheritance: reissue`), never baked, so no cross-user bleed.

## Why this is the efficient, secure, k8s-native answer

- **Hard isolation:** each user instance is a **Firecracker microVM** in a husk
  pod, not a shared container or namespace. This is the strongest tenant boundary
  available in the k8s ecosystem - a kernel + VM boundary per user, not cgroup +
  seccomp. Honest k8s semantics (CLAUDE.md operating principle #3): these are not
  pods, so the isolation story is VM-level, stated plainly.
- **Cheap to multiply:** instances are **CoW forks** of one golden snapshot  - 
  guest memory pages are shared copy-on-write, so N users do not cost N x full RAM.
  Claim is ~86 ms (measured), so per-user fork-on-demand is interactive.
- **Scale to zero per user:** idle instances are reaped (the failure/GC machinery,
  #163, already reaps); the warm pool absorbs burst. A user who returns re-forks
  from the still-resident golden snapshot. This is the efficiency lever: you pay
  for active users, not registered users.
- **Per-fork secrets + fork-correctness:** the existing reseed/secret-reissue
  contract guarantees each user's instance has its own CRNG, clock, network
  identity, and secret values.

## k8s-native mechanism

- **Identity -> instance map.** The controller keys a per-user instance by
  `(app/label, identity)` - e.g. a `Sandbox` CR per active user-instance, labelled
  with a hash of the identity (never the raw email) for selection, owned by the
  golden pool. The edge forwards the resolved identity (it already extracts it for
  the auth ladder) to the controller's resolve/route endpoint.
- **Fork-on-demand.** The controller claims a warm husk for the identity (or forks
  fresh), records the `(label, identity) -> Sandbox` mapping, and the expose route
  for that request resolves to that Sandbox's endpoint. This reuses the claim path
  and the expose route-sync wholesale; the only new logic is "resolve-or-create by
  identity."
- **Reaping.** A per-user instance with no traffic for `idleTTL` is terminated
  (residual GC, #163). The mapping is dropped; the next request re-forks.
- **Per-org scoping.** Ties into per-org namespaces (#288): per-user instances live
  in the org's namespace, so tenancy nests inside the existing org boundary.

## mitos.yaml surface (proposed)

```yaml
serve:
  command: ...
  ready: { http: { path: /, port: 18789 } }
expose:
  label: openclaw
  sharing: authenticated        # or org / private - gates by mitos identity
  tenancy:
    mode: per-user              # or: shared
    idleTTL: 30m                # reap a user's instance after this idle window
    warm: 2                     # pre-forked unclaimed instances for instant cold start
# secrets stay per-fork (already modelled): injected via Configure, never baked
```

`shared` collapses to today's single-route behavior. `per-user` requires a gated
`sharing` tier (validation error otherwise).

## What is built vs missing

**Built:** the auth ladder + identity resolution (#407); the fork primitive and
capture-running (#460); per-fork secret injection and fork-correctness; the warm
pool and residual GC (#163); per-org namespaces (#288); the expose route-sync.

**Missing (this slice):**
1. `tenancy` field on `mitos.yaml` / `Sandbox.spec.expose` (API + validation:
   `per-user` requires a gated tier).
2. Controller **resolve-or-create-by-identity**: the `(label, identity) -> Sandbox`
   map, fork-on-demand keyed by the edge-forwarded identity, and the per-user warm
   pool.
3. Edge -> controller **identity hand-off** on the request path (the edge already
   has the verified identity; it must pass it to the route resolution).
4. Per-user **idle reaping** wired to `idleTTL`.

## Dependencies / related fixes this demo surfaced

- **#476** public-URL injection + proxy trust: an app must know its public URL to
  self-configure (origin allow-lists, redirect URIs). The proxy-trust half is
  landing (PR #478); URL injection (`MITOS_PUBLIC_URL`) is still open and is needed
  before per-user apps that template their own URL.
- **#475** workload-command change must re-trigger the snapshot build; **#461** the
  stale recorded digest on same-name rebuild. Both are the controller rebuild path
  and bite the iterate-on-config loop a `mitos.yaml` author lives in.

## Open questions for review

1. **Identity key granularity:** per-user (`sub`) vs per-org vs per-(user,device)?
   OpenClaw wants per-user; some apps may want per-org shared.
2. **Cold-start UX when warm pool is empty:** block ~the fork+boot time, or show an
   interstitial? Fork is ~86 ms but a cold golden (not yet built) is minutes.
3. **Cost ceilings:** cap concurrent per-user instances per org (quota), and the
   reap TTL defaults.
4. **State persistence across reap:** per-user instance is ephemeral; if a user's
   app has durable state, does it bind a per-user Workspace/volume that survives the
   reap, or is the snapshot the only state? (Likely: opt-in per-user volume.)
5. **Auth bridge depth (#476):** does mitos only gate the URL, or also forward a
   trusted identity assertion the app consumes (forward-auth headers) so the app
   need not run its own login?
