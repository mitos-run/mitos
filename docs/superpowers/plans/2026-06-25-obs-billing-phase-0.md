# Observability and billing spine, Phase 0 (the keystone)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make billing real and unblock org-scoped observability with one change: stamp the trusted `mitos.run/org` label on husk pods and claims, wire a live multi-node metering scraper (`usage.SampleSource`), and a real `usage.OrgResolver`, so the existing `internal/usage` to `internal/saas/billing` pipeline runs on live cluster data.

**Architecture:** The per-sandbox metering report (`internal/metering`, served by forkd at `GET /v1/metering`) already carries the five billable dimensions. `internal/usage` already aggregates it idempotently into per-(org, sandbox, window) `UsageRecord`s, and `internal/saas/billing` already drains those into credits/Stripe. The only missing pieces are the live `SampleSource` (scrape every forkd node) and a real `OrgResolver` (sandbox to org), plus the `mitos.run/org` label that the resolver reads. See `docs/design/observability-and-billing-spine.md`.

**Tech Stack:** Go. The controller NodeRegistry (to find forkd nodes to scrape), `internal/tenant` (org label + per-org namespace), `internal/usage` (collector seams), `internal/metering` (the report shape).

## Global Constraints

- Branch `feat/observability-billing-spine` off main.
- SECURITY (billing trust boundary): the org for a sandbox is derived from the TRUSTED control-plane identity (the per-org namespace `mitos-org-<id>` the gateway places the sandbox in, via `internal/tenant`), NEVER from client input or a client-set label. The controller stamps `mitos.run/org`; clients cannot.
- Secret values, argv, env, file bytes NEVER enter the usage report, metrics, or logs. Only counts/seconds/bytes/ids.
- No em (U+2014) or en (U+2013) dashes anywhere. DCO `git commit -s`. Conventional commits. Stage explicit paths only.
- Idempotency preserved: `usage.Integrate` is idempotent; the live scraper must not double-count (use the scrape timestamp window + the existing delta logic).
- The threat-model (`docs/threat-model.md`) gets a row update if the surface moves (org attribution is a new billing surface).
- Self-host with no orgs must keep working: when a sandbox has no org namespace (single-tenant self-host), the resolver returns a stable default org (e.g. "default") or marks the record unattributed, without breaking the pipeline.

## Task 0.1: derive org from the trusted namespace (tenant helper)

**Files:** `internal/tenant/tenant.go` (+ test). Confirm whether `OrgFromNamespace` exists; if not, add it as the inverse of `NamespaceForOrg`.

- [ ] Write a failing test: `OrgFromNamespace("mitos-org-acme") == ("acme", true)`; a non-org namespace (e.g. "default", "mitos") returns `("", false)`.
- [ ] Implement `OrgFromNamespace(ns string) (orgID string, ok bool)` (strip the `mitos-org-` prefix; return ok=false otherwise). Keep it the exact inverse of `NamespaceForOrg`.
- [ ] Test passes; commit.

## Task 0.2: stamp `mitos.run/org` on husk pods and claims (controller)

**Files:** `internal/controller/huskpod.go` (the husk pod builder that already stamps `mitos.run/pool` and `mitos.run/claim` near lines 39, 56, 916). Also wherever a claim binds a sandbox so the Sandbox object carries the org label.

- [ ] Determine the org for the pod: from the owning SandboxPool/Sandbox namespace via `tenant.OrgFromNamespace`. If ok, stamp `tenant.OrgLabelKey` (`mitos.run/org`) on the husk pod labels alongside the existing pool/claim labels. If not an org namespace (self-host), do NOT stamp (or stamp a configured default). The value MUST come from the namespace, not any client-provided field.
- [ ] Write a failing controller/envtest test: a SandboxPool in namespace `mitos-org-acme` produces husk pods labeled `mitos.run/org=acme`; a pool in `default` has no org label. Assert the label is the namespace-derived org, and that a client-set `mitos.run/org` on the input is ignored/overwritten by the controller (trust boundary).
- [ ] Implement; test passes; envtest green; commit.

## Task 0.3: real OrgResolver (sandbox to org)

**Files:** `internal/usage/collector.go` (the `OrgResolver` interface + `StaticOrgs` default) + a new live resolver (e.g. `internal/usage/k8sresolver.go` or in the controller wiring).

- [ ] Implement an `OrgResolver` that maps a sandbox id to its org by reading the trusted `mitos.run/org` label off the sandbox's husk pod (or the Sandbox object / its namespace) via the controller cache or a lister. Return `(orgID, true)` when attributed; `("", false)` or a stable default when not (self-host).
- [ ] Write a failing test against a fake lister/cache: a sandbox whose pod carries `mitos.run/org=acme` resolves to "acme"; an unlabeled sandbox resolves to the default/unattributed path.
- [ ] Implement; test passes; commit.

## Task 0.4: live multi-node SampleSource (scrape forkd /v1/metering)

**Files:** a new `internal/usage/source_*.go` (the live `SampleSource`) wired from the controller (it has the NodeRegistry of forkd nodes) or a small collector loop. Reuse the forkd `GET /v1/metering` endpoint + the existing report parsing in `internal/metering`.

- [x] Implement a `SampleSource` that, for each known forkd node (from the NodeRegistry), pulls `GET /v1/metering` (the per-sandbox report), and yields the samples for `usage.Integrate`. Preserve idempotency: integrate by the existing window/delta logic; a re-scrape of the same data must not double-count. Handle a node being unreachable (skip + log a count, do not fail the whole collection). DONE: `internal/usage/livesource.go` (`NodeRegistrySource`), over the `usage.NodeLister` import-cycle-avoiding seam; the controller wires `RegistryNodeLister` (`internal/controller/usage_scrape.go`).
- [x] Secret hygiene: the report carries ids/bytes/seconds only; assert nothing else is logged. DONE: only sandbox ids, org ids, bytes, and seconds flow through `Sample`; the skipped-node signal is a count with no node identity or error text.
- [x] Write a failing test against a mock forkd metering server (httptest): two scrapes of the same report integrate to the same totals (idempotent); a second window with higher counters integrates the delta; org is attached via the OrgResolver. DONE: `internal/usage/livesource_test.go` (healthy node collected + org-tagged, bad node skipped + counted, two identical scrapes Integrate to the same totals).
- [x] Implement; test passes; commit.

## Task 0.5: wire the collector loop + a minimal per-org usage Prometheus series

**Files:** the controller or a small collector goroutine that runs the SampleSource on an interval, feeds `usage.Integrate`, and stores the records (the existing store seam or an in-memory store for now). Plus a first per-org Prometheus gauge so the SAME data is observable.

- [x] Run the collector on an interval (configurable, default e.g. 60s), behind a flag (off by default for self-host that does not want it; on for hosted). Feed the live SampleSource + OrgResolver into `usage.Integrate`; persist to the usage store (or the documented store seam). DONE: `--usage-collector` (default off) + `--usage-collector-interval` (default 60s) in `cmd/controller/main.go`; `controller.UsageCollectorRunnable` (`internal/controller/usage_collector.go`) ticks the collector; records land in `usage.MemUsageStore` (durable store is a documented follow-up).
- [x] Add ONE dual-use Prometheus series: `mitos_usage_vcpu_seconds_total{org}` (and optionally mem/egress) emitted from the same integrated records, so the billing data is immediately visible on the operator dashboard. Org/claim labels only; no secrets. DONE: `usage.Metrics` (`internal/usage/usagemetrics.go`) exposes `mitos_usage_vcpu_seconds_total{org}` (plus opt-in mem/egress GaugeVecs), registered on the controller-runtime registry and fed from the same integrated records via `Collector.OnRecords`. Label is org only (no sandbox-id cardinality, no secrets).
- [x] Write a failing test: after the collector runs against the mock source, the usage store has the expected per-org record AND the Prometheus gauge reflects it. DONE: `internal/usage/usagemetrics_test.go` (uses `prometheus/testutil.GatherAndCompare`).
- [x] Implement; test passes; commit.

## Task 0.6: docs + threat-model

- [ ] Update `docs/threat-model.md`: add/extend a row for org-scoped usage attribution (the org label is controller-set from the trusted namespace, never client input; the usage report carries no secrets; billing drawdown is idempotent). Dash-free.
- [ ] Update `docs/metering.md` or `docs/saas/usage-pipeline.md` to note the live scraper + org resolver are now wired (Phase 0). Reference `docs/design/observability-and-billing-spine.md`.
- [ ] Commit.

## Self-Review

- The org label is the trust boundary: it MUST be controller-set from the namespace, never client input. Every task reinforces this.
- Idempotency of `usage.Integrate` is preserved by the live SampleSource (no double-count on re-scrape).
- Self-host with no orgs keeps working (default/unattributed path).
- One dual-use Prometheus series proves the spine: the billing data is observable.
- After Phase 0: billing runs on live data, and per-org usage is on the dashboard. Phases 1 to 4 (Vitals to Prometheus, Hubble, OpenCost, console UX) build on this same labeled spine.
