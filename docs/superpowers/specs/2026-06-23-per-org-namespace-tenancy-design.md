# Per-org namespace tenancy (approach A: per-org pools) — design

Date: 2026-06-23
Status: design. Scopes the controller/control-plane half of hosted multi-tenancy.
Tracks: [#288](https://github.com/mitos-run/mitos/issues/288), under the
[#208](https://github.com/mitos-run/mitos/issues/208) hosted-SaaS epic. Sibling
to the closed [#172](https://github.com/mitos-run/mitos/issues/172) (dedicated
*nodes*, not namespaces). Consumes the convention in `internal/tenant` and the
console consumption side already merged (PRs #277/#285/#287).

## Decision

**Hard isolation: one namespace per org (`mitos-org-<id>`), with per-org
SandboxPools.** Each org's pools, warm husk pods, claims, sandboxes, and secrets
live in its own namespace. This keeps "namespace = tenant boundary" honest end to
end and matches the `internal/tenant` convention (`mitos.run/org` label +
`NamespaceForOrg`) that the secret provider and the console sandbox query already
use.

Approach B (shared pools + moving an activated sandbox into the org namespace at
claim time) was rejected: the activation crossing a namespace boundary is complex
and weakens the boundary story. The cost objection to A (warm capacity per org)
is addressed by `minWarm: 0` autoscale + a shared cold-start tier (§5).

## What exists today (the starting point)

- The **microVM / husk pod is the isolation boundary** (`docs/threat-model.md`).
  forkd is a node-level privileged DaemonSet in the system namespace, shared
  across tenants; it is NOT per-org.
- **Husk pods run in `pool.Namespace`** (`internal/controller/huskpod.go`,
  `husknetworkpolicy.go`, `huskpdb.go`); claims run in `claim.Namespace`.
- **Pools autoscale**: the dormant warm count is
  `clamp(inUse + targetSpare, minWarm, maxWarm)` (`PoolAutoscaleSpec`), so
  `minWarm: 0` lets a pool scale to zero when idle.
- The **gateway resolves the verified `OrgID`** and forwards it
  (`internal/saas/gateway.go`), but the claim-creating control plane is still a
  seam (stubbed in `cmd/gateway`); nothing applies the org to the claim yet.
- `internal/tenant` defines `OrgLabelKey = "mitos.run/org"` and
  `NamespaceForOrg(org) = "mitos-org-<id>"`. The kube secret provider and
  `clustersandbox` already read/write within that namespace.

## Architecture

```
mitos-system (shared)                 mitos-org-<A>  (enforce=privileged)        mitos-org-<B>
├── controller (per-org RBAC)         ├── SandboxPool(s)  (autoscale minWarm:0)  ├── SandboxPool(s)
├── forkd DaemonSet (node-level)      ├── warm husk pods   <─ scale to zero      ├── warm husk pods
├── device-plugin / kernel DS         ├── SandboxClaims / v1alpha2 Sandboxes     ├── ...
└── (optional) shared warm tier       ├── org Secrets (kube provider)            │
                                      ├── ResourceQuota + LimitRange             │
                                      ├── default-deny NetworkPolicy + egress    │
                                      └── mitos-pool-secrets RoleBinding         └── ...
        every org object carries  labels[mitos.run/org]=<id>
```

- **Per-org namespace** `mitos-org-<id>`, `enforce=privileged` PSA. Privileged
  PSA is required because the husk pod needs `/dev/kvm` (device plugin) +
  `NET_ADMIN` (in-pod egress firewall) and, for name-egress pools, a short-lived
  privileged init container. **PSA level is about what a pod in the namespace may
  request, not cross-tenant access** — the cross-org boundary is the namespace +
  NetworkPolicy + the microVM, and that is unchanged by privileged PSA.
- **Per-org SandboxPool(s)**, `autoscale.minWarm: 0` by default so an idle org
  costs no warm capacity; paid tiers may raise `minWarm`/`targetSpare` for
  warm-start latency (a pricing lever).
- **forkd stays node-level** in `mitos-system`; it is the snapshot builder for
  every tenant's husk pods regardless of namespace. No per-org forkd.

## Claim / sandbox flow

1. SDK → gateway: API key → verified `OrgID` (already implemented).
2. The claim-creating control plane (the real `ControlPlane.Forward` impl) sets
   `labels[mitos.run/org] = OrgID` and creates the `SandboxClaim` **in
   `NamespaceForOrg(OrgID)`**, bound to one of the org's pools.
3. The controller reconciles the claim against the org's pool and **propagates
   the org label onto the `v1alpha2.Sandbox`** and into `VitalsLabels` (so
   per-tenant metering, #211/#33, attributes correctly).
4. The console (`clustersandbox`, merged) lists/inspects/terminates by querying
   the org namespace + label; the kube secret provider injects org secrets from
   the same namespace. Both already work against this shape.

## Namespace lifecycle

A small **org-namespace reconciler** (or the control plane on org creation)
provisions `mitos-org-<id>` and tears it down on org deletion. Each namespace
gets:

- PSA labels `pod-security.kubernetes.io/enforce: privileged` (+ audit/warn).
- A `ResourceQuota` (per-org sandbox/CPU/memory ceilings — also the abuse-control
  surface, ties to #213) and a `LimitRange`.
- A **default-deny `NetworkPolicy`** plus the per-template egress allowlist the
  husk pods already use; cross-org pods cannot reach each other (separate
  namespaces + deny-by-default).
- The per-org `mitos-pool-secrets` `RoleBinding` (the chart already defines the
  ClusterRole; bind it per org namespace).
- The org's default `SandboxPool` (`minWarm: 0`).

Provisioning model: prefer **GitOps-free, controller-reconciled** from an
`Org`/account record so namespace state self-heals; the chart ships the templates
and the ClusterRole, the reconciler stamps the per-org instances.

## RBAC

Extend the existing `namespacedSecretsRBAC` narrowing (already in the chart for
Secrets) to the per-org namespaces for sandboxes/claims/pods/pools: the
controller binds itself per org namespace rather than holding a cluster-wide
grant, so a controller compromise is bounded to namespaces it has adopted. The
console SA already has read on sandboxes/claims (PR #287); scope it per-org-ns
too when signup provisions a namespace.

## §5 Warm-capacity economics (the cost objection to A)

Per-org pools risk paying for `minWarm` warm pods × N orgs. Mitigations, in order:

1. **`minWarm: 0` default** — an idle org holds zero warm pods; first claim pays a
   cold start (snapshot restore, the tens-of-ms class once a holder exists, but a
   full cold pod schedule otherwise).
2. **Shared cold-start tier** — an optional shared warm pool of generic
   base-image snapshots in `mitos-system`; a cold org claim can be served from it
   while the org's own pool spins up, then subsequent claims use the org pool.
   (Hybrid; keep the served sandbox's ownership/labels org-scoped.)
3. **Tiered `minWarm`** — paid plans set `minWarm`/`targetSpare > 0` for
   warm-start latency; free/idle orgs stay at zero. This is a pricing lever, not
   just an ops knob (ties to `docs/saas/pricing.md`).

Net: the warm cost scales with *active* orgs and paid warm guarantees, not with
total orgs.

## Scale & limits

- **Namespace/etcd pressure**: thousands of org namespaces is fine for etcd, but
  watch informer cache sizes; the controller should use label-scoped informers.
- **Per-org `ResourceQuota`** caps blast radius and is the abuse-control primitive
  (#213).
- **Cluster-wide ceilings** (`MaxSandboxes`) still apply on top of per-org quotas.

## Gates (project rule, #208)

Built in parallel, but public self-serve untrusted multi-tenancy does NOT switch
on until fork-correctness (#1), failure/GC (#163), and the external security
review (#194) are green, and the `docs/threat-model.md` per-boundary checklist
passes. Until then, run in waitlist / design-partner mode.

## Work breakdown (refines #288 under approach A)

- [ ] `internal/tenant` imported by controller + control plane (no parallel scheme).
- [ ] Org-namespace reconciler: provision/teardown `mitos-org-<id>` with PSA,
      ResourceQuota, LimitRange, default-deny NetworkPolicy, pool-secrets
      RoleBinding, and the default `minWarm:0` SandboxPool.
- [ ] Claim creation stamps `mitos.run/org` + targets the org namespace (real
      `ControlPlane.Forward`).
- [ ] Controller propagates the org label → `v1alpha2.Sandbox` + `VitalsLabels`.
- [ ] Per-org controller RBAC (extend `namespacedSecretsRBAC` to sandboxes/pools).
- [ ] Shared cold-start tier (optional, phase 2 of this track).
- [ ] Real usage `OrgResolver` (#211) via a label-scoped informer.
- [ ] `kind-e2e` slice: two orgs cannot list / terminate / network-reach each
      other's sandboxes; per-org ResourceQuota enforced.
- [ ] Helm: chart renders the per-org templates + the org-namespace reconciler
      config; golden tests.

## Risks & open questions

- **Privileged PSA per org** — acceptable (boundary is the microVM + netpol, not
  PSA), but document it in the threat model and ensure no host escape path is
  added by per-org namespaces.
- **Cold-start UX vs cost** — the shared cold tier (mitigation 2) is the main
  complexity; if it slips, low-traffic orgs accept a cold start. Decide whether
  the cold tier is in-scope for v1 of this track or a fast-follow.
- **Org-namespace provisioning ownership** — controller-reconciled from an `Org`
  record (recommended) vs the control plane creating namespaces imperatively.
  Pick one; the reconciled model self-heals.
- **forkd reachability across namespaces** — confirm the mTLS control channel
  from the node-level forkd to per-org-namespace husk pods needs no per-namespace
  policy exception beyond the default egress allowlist.
