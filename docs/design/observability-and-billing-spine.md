# Observability and billing: one telemetry spine

Status: design (issue #164). Author: design pass 2026-06-25.

## Thesis

Observability and billing are not two systems. They are two readouts of one
telemetry spine. Issue #164 should be framed as wiring the spine to live
sources and propagating one label, not as building observability from scratch.
Most of the substrate already exists as pure, unit-tested Go:

- `internal/usage`: idempotent usage aggregation (the `UsageRecord` model).
- `internal/saas/billing`: credits, spend caps, dunning, a Stripe provider seam.
- `internal/observability`: pure Hubble and OpenCost reconcilers.
- `internal/daemon/vitals_api.go`: labeled guest Vitals over vsock.
- `internal/metering`: the per-sandbox metering report (egress, GPU, CoW memory).
- `internal/tenant`: the `mitos.run/org` label and per-org namespaces.

What is missing is the live wiring (a metering scraper, a real org resolver,
live Cilium and OpenCost) and one propagated label. That wiring is the shared
substrate that powers both the user-facing observability and SaaS billing.

## The spine

```
guest microVM ──vsock Vitals (steal, balloon, processes)──┐
per-sandbox nftables egress counter ──────────────────────┤
fork engine UFFD CoW page accounting ─────────────────────┤→ forkd GET /v1/metering
                                                          │      (per sandbox)
                                                          │            │
                                                          │   scrape + usage.Integrate (idempotent, CoW amortized)
                                                          │            │
                                                          │   UsageRecord{org, sandbox, window, vcpu_s, mem_gib_s, storage_gib_h, egress, gpu_s}
                                                          │            ├──→ billing drawdown ──→ Stripe / credits / spend caps
                                                          │            ├──→ per-org Prometheus series ──→ Grafana
                                                          │            └──→ console Usage and Cost / invoice
Cilium + Hubble (eBPF) ───────────────────────────────────┴─ per-flow logs + policy drops, reconciled vs the egress counter
OpenCost ───────────────────────────────────────────────────  namespace cost, reconciled vs priced resource-seconds
```

The join key for every consumer is one label: `mitos.run/org` on husk pods and
claims. Stamp it once and the usage resolver, the Hubble egress resolver, the
OpenCost namespace attribution, and per-org dashboards all resolve identity from
the same place.

## The billing unit

Five resource-second dimensions, already modeled in `internal/usage`:

1. vCPU-seconds.
2. memory-GiB-seconds, CoW amortized. Shared template pages are billed once
   across forks, not per fork. This is the differentiator.
3. storage-GiB-hours (snapshot plus workspace).
4. egress bytes (also GiB for pricing).
5. GPU-seconds.

The CoW-amortized memory line is both the pricing edge and the trust story: the
marginal fork is nearly free, and the bill can prove it. Priced resource-seconds
reconcile against OpenCost namespace spend, so the operator dashboard number and
the customer invoice number are provably the same figure. Per-sandbox budgets
and spend caps already exist on the CRD (`SandboxBudget`: MaxForks,
MaxCpuSeconds, MaxEgressBytes).

## Technology choices

The right tool per layer, not one tool everywhere.

- Layer 1, network: Cilium with Hubble (eBPF). In-kernel L3/L4/L7 flow
  visibility and identity-aware policy, no sidecars, low overhead. This is the
  correct place for eBPF. Per-sandbox egress flows (destination, port, DNS,
  allowed vs denied) resolve to a claim and org via pod labels; the pure
  resolver already exists in `internal/observability/hubble.go`. Reconcile
  Hubble flow bytes against the existing nftables `EgressBytes` counter for two
  independent egress sources.
- Layer 3, guest telemetry: the in-guest agent reading `/proc` over vsock. NOT
  eBPF. Sandboxes are Firecracker microVMs with their own kernel; host eBPF
  cannot introspect guest processes. The agent already samples CPU steal,
  balloon vs used memory, and the process table.
- Resource-seconds billing: the fork engine UFFD page accounting. NOT eBPF.
  cgroup or eBPF accounting cannot capture CoW shared-page amortization, which
  is the whole pricing model. The engine already knows shared vs unique pages.
- Tracing: OpenTelemetry. The orchestrator to claim to fork span tree exists;
  the missing tail is a guest-ready and first-exec span, closed from the same
  vsock sample that feeds Vitals.
- Metrics fabric: Prometheus and Grafana (already used).
- Cost: OpenCost for namespace cost attribution, reconciled against priced
  resource-seconds.

Future eBPF upside, out of scope for #164: Tetragon (Cilium security
observability) for syscall-level runtime security and audit with sandbox
identity. A strong enterprise and compliance feature later.

## User-facing metrics and DX

Users ask five questions; each maps to a surface and a metric.

| Question | Metric | Surface |
|---|---|---|
| Is my sandbox starved? | CPU steal, mem vs balloon, process table (Vitals) | `kubectl mitos ps --processes` / `top`; console live view |
| How fast did it start? | fork latency, time-to-ready, first-exec (OTel trace) | trace view; a cold vs warm headline |
| What is it costing me? | the five resource-seconds, priced, per sandbox/pool/org | console Usage and Cost; usage API; Grafana |
| Is my fleet efficient? | active sandboxes, CoW savings, marginal bytes/fork, warm pool | console instrument cockpit (the Pareto proof) |
| What is leaving my sandbox? | egress flows and bytes, policy drops | Hubble; console egress panel |
| Am I near my budget? | spend vs soft and hard cap, budget burn | console Billing; spend-cap alerts |

The DX principle: the same numbers everywhere. The operator Grafana spend panel,
the customer console invoice, and the OpenCost reconcile all derive from one
priced rate table (`billing.FromPriceList` already bridges the usage `PriceList`
to billing `Rates`). Provable cost is a UX feature and a sales asset.

## Phasing

- Phase 0, keystone: stamp `mitos.run/org` on husk pods and claims; wire the
  live multi-node `GET /v1/metering` scraper (`usage.SampleSource`) and a real
  `usage.OrgResolver` (claim or namespace to org). One change unblocks billing
  and org-scoped observability together. Highest leverage.
- Phase 1, Layer 3 and metric promotion: bridge labeled Vitals to Prometheus
  (steal, balloon, process count); promote the per-sandbox metering rows (egress,
  GPU, unique and shared memory) to org/claim/pool-labeled gauges; close the
  trace tail with guest-ready and first-exec spans.
- Phase 2, Layer 1: deploy Cilium and Hubble; wire the relay to the existing
  resolver; per-sandbox egress flows and drops labeled by claim and org;
  reconcile flow bytes vs nftables egress.
- Phase 3, Layer 2: deploy OpenCost; wire the existing reconcile; prove
  dashboard cost equals invoice.
- Phase 4, UX and packaging: build the console Usage and Cost and Billing rich
  views; per-org Grafana; the cost-per-fork headline; Helm-package the Cilium,
  OpenCost, and Prometheus dependencies; the real Stripe SDK and durable stores
  (hosted gated).

## Operating principles applied

- No unverified claims: every cost figure is reproducible from the priced
  resource-seconds and reconciled against OpenCost; bench numbers stay in `bench/`.
- Security: org attribution is a billing trust boundary; the org label is set by
  the controller from tenant identity, never from client input. Secret values,
  argv, env, and file bytes never enter metrics, traces, or the usage report.
  The threat model is updated in the same PR as any surface change.
- Honest Kubernetes semantics: sandboxes are not pods; per-sandbox cost is
  attributed from the metering report and the engine, not from pod-level cgroup
  accounting alone.
