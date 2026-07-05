# Placement and regions

Status: Phase 0 of the multi-region/operator-defined-placement epic (#712).
Single cluster, region-SHAPED: every deployment already speaks in placement
values (an org has a home region, a sandbox create can request one, a Sandbox
CR carries a region label, a usage record carries a region dimension), but
every value resolves to the SAME cluster today. Phase 1 is what makes a
second value actually route somewhere different.

## What exists today (Phase 0)

**The placement registry** (`internal/saas/placement`): one operator-defined
key and an ordered list of named values, loaded once at boot. Hosted Mitos
calls the key `region`; a self-host operator can rename it to whatever fits
their fleet (`cluster`, `zone`, `dc`, ...) and list whatever values they run,
via two env vars on `cmd/console`:

- `MITOS_CONSOLE_PLACEMENT_KEY` (default `region` on the hosted edition,
  `cluster` on the self-hosted community edition).
- `MITOS_CONSOLE_PLACEMENT_VALUES`, a comma-separated `name[:display]` list
  (default `fra:Frankfurt (EU)` hosted, `default` community). The first value
  is the default; every parsed value is `available`.

**Discovery**: the registry is advertised, additively, in the existing
`GET /console/capabilities` document:

```json
"placement": {
  "key": "region",
  "values": [
    {"name": "fra", "display": "Frankfurt (EU)", "default": true, "available": true}
  ]
}
```

A single-value registry (the Phase 0 default everywhere, hosted and
self-host) means the console shows no region picker at all: `NewSandboxModal`
gates it on `values.length > 1`. Nothing about a single-cluster deployment
claims multi-region capability it does not have; this is the "no fake
multi-region UI" rule.

**Org home region**: `Organization.HomeRegion` is stamped once at org
creation from the registry's default, and is immutable afterward (a move is a
future, explicit copy, never an in-place update). It is read-only in the
console account profile (each membership row) and the instance-operator org
rollup (`GET /console/admin/orgs`).

**Sandbox create**: `POST /console/sandboxes` accepts an optional `region`
field. Empty means the org's home region. A non-empty value is validated
against the deployment's registry (`Registry.Valid`) before the cluster
adapter is ever called; an unknown or unavailable value is a 400 naming the
valid values in its remediation. The cluster adapter stamps
`mitos.run/region` on the Sandbox CR **only for a tree root** (a `Create`
call); it never appears on a CR the deployment did not stamp.

**Fork locality (the hard constraint)**: a live copy-on-write fork shares the
parent's guest memory on the parent's node (`internal/fork`'s uffd engine,
`docs/scheduling.md`'s CoW-aware bin-packing). A fork therefore CANNOT cross
clusters, ever, in any phase. Region is a property of the whole fork tree,
fixed at the root's creation, never a per-fork choice: `Control.Fork` copies
the source's `mitos.run/region` label onto every child verbatim, never
re-resolving it. This is the same class of constraint as GPU-fork being
impossible; no surveyed platform (Fly, Modal, E2B, Daytona, Vercel,
Northflank) even attempts cross-region CoW forking.

**Usage attribution**: `usage.UsageRecord` carries a `Region` dimension,
populated best-effort from the sandbox's region label at the same point
`OrgID` is attributed (the husk-pod scrape lister, and the claim-time label
copy in `stampClaimRegionLabel`). It is NOT part of the `(org, sandbox,
window)` idempotency key and changes no billing math; it exists so a
per-region cost rollup or residency audit has something to group by, ahead of
Phase 1 actually needing one. An old record, or one from a self-host
deployment that never set a region, simply reads back empty.

**SDK**: `mitos.create(region=...)` (Python) is optional passthrough, sent in
the fork request body only when set. The Daytona-compat shim's
`DaytonaConfig(target=...)`, previously accepted and silently ignored, now
maps onto `region`.

## What Phase 1 adds

A second cluster a value can actually resolve to:

- A region-routing wrapper over the `ControlPlane` seam
  (`internal/saas/controlplane`) that resolves org -> region -> the right
  per-cluster `K8sControlPlane`, instead of today's single client.
- Org provisioning into the home-region cluster (client selection by region)
  rather than always the one cluster this binary talks to.
- Per-region template replication: a CAS mirror so a pool template exists in
  every region an org actually uses (Harbor/ECR replication precedent,
  already flagged as open work in `docs/snapshot-distribution.md`).
- The console region picker actually appearing (today it renders nothing
  because every registry is single-value); an org-scoped `GET /v1/regions`-
  style listing.
- Quota becoming per-region at the enforcer (Daytona precedent: quotas apply
  to a default region).

## What Phase 2 adds

Residency and the runtime data path:

- Regional runtime ingress (exec/PTY/files/expose subdomains) so the
  resolving gateway is network-adjacent to the cluster serving it; today's
  single global endpoint (`api.mitos.run`) stays for identity, org routing,
  and billing regardless of phase.
- In-region storage for the residency-sensitive data: sandbox snapshots,
  workspaces, exec I/O, logs, templates, raw metering samples, backups, and
  org audit logs.
- EU residency documentation and DPA wording (see
  `docs/compliance-claims.md`), upgrading the claim from "customer content at
  rest stays in region" (Phase 0/1 wording) to a TLS-terminates-in-region
  claim.
- Region choice surfaced in org-creation onboarding, not just at sandbox
  create time.

## Non-goals (every phase)

- Cross-region live fork: physically incompatible with CoW locality, not a
  missing feature.
- In-place region migration: a move is a future, explicit, user-initiated
  copy, never an in-place update of `HomeRegion` or a Sandbox's region label.
- A Kubernetes federation layer of any kind (KubeFed is archived; Karmada
  solves object replication, not tenant routing/billing/residency).

See the epic (issue #712) for the full architecture decision and survey of
comparable platforms (Fly, Modal, E2B, Daytona, Vercel, Northflank, Upstash,
Sentry, GitHub Dedicated, Confluent, Temporal, GitLab Dedicated).
