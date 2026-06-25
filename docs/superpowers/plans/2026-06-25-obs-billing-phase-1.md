# Observability and billing spine, Phase 1 (guest metrics + trace tail)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the per-sandbox guest health and resource usage observable as Prometheus metrics (labeled by the trusted org/pool), and close the end-to-end fork trace, all on the Phase 0 labeled spine. Code only, no infra.

**Architecture:** Phase 0 landed the org trust boundary, the live metering scraper, and the per-org usage metric. Phase 1 adds: a guest Vitals sampler that publishes labeled guest health metrics, promotes the per-sandbox metering rows to labeled gauges, and adds the guest-ready and first-exec spans to the existing OTel trace. See `docs/design/observability-and-billing-spine.md`.

**Tech Stack:** Go. `internal/daemon/vitals_api.go` (the labeled `/v1/vitals` endpoint), the Prometheus registry pattern from `internal/usage/usagemetrics.go`, `internal/observability/tracing.go` (OTel tracer), the controller NodeRegistry.

## Global Constraints

- Branch `feat/obs-billing-phase-1` off main (which has Phase 0).
- SECRET HYGIENE (strict): metrics carry only NUMBERS and the trusted org/pool labels. NEVER export per-process command lines, argv, env, secret values, file paths, or any free-form string as a metric label or value. `process_count` is a count; do NOT publish a per-process series.
- CARDINALITY: prefer org and pool labels. A per-sandbox label is allowed only where bounded and justified; document the bound. No unbounded label (no per-pid, no per-command).
- The org label is the trusted `mitos.run/org` (Phase 0 `LabelOrgResolver`), never client input.
- Samplers are behind a flag, default OFF (self-host unaffected), like the Phase 0 collector.
- No em (U+2014) or en (U+2013) dashes. DCO `git commit -s`. Conventional commits. Stage explicit paths only.

## Task 1.a: guest Vitals to Prometheus

**Files:** a new sampler (e.g. `internal/controller/vitals_sampler.go` or wired alongside the usage collector) + a metrics file mirroring `internal/usage/usagemetrics.go`.

- [x] A sampler, behind a default-off flag (`--vitals-sampler`), that on an interval scrapes the new node-scoped `GET /v1/vitals/node` operational endpoint (added on the forkd operational mux next to `/v1/metering`, so the controller needs no per-sandbox bearer token), resolves org via the trusted `mitos.run/org` label, aggregates per (org, pool), and publishes Prometheus gauges: `mitos_guest_cpu_steal_percent`, `mitos_guest_mem_balloon_bytes`, `mitos_guest_mem_used_bytes`, `mitos_guest_process_count`. Labels: org, pool (bounded). cpu_steal is the per-bucket MAX (worst-starved sandbox); memory and process_count are SUMs (fleet footprint); documented in each gauge's Help text. Reuses the Phase 0 NodeRegistry + the trusted `LabelOrgResolver`. A sandbox whose guest is unreachable is skipped (counted), never failing the cycle; a node down is skipped (counted).
- [x] SECRET-CLEAN: only the numeric fields are exported; the process table is reduced to its length (`process_count`), never per-process command lines, argv, pids, or env. Test asserts the label set is exactly {org, pool}.
- [x] TDD: tests against a mock `/v1/vitals/node` server (`scrape`) plus the pure `aggregateVitals` and the metric `observe`, asserting the four gauges reflect the snapshot labeled by org/pool with no secret/command label, and a sandbox with no org is skipped+counted. Daemon-side `TestHandleNodeVitals_Batch` asserts the node endpoint skips+counts an unreachable guest.
- [x] `go test`, build, vet, gofmt, lint (darwin + linux) clean; committed.

## Task 1.b: per-sandbox metering rows to labeled gauges

**Files:** extend the Phase 0 usage metrics (`internal/usage/usagemetrics.go`) or a sibling, fed from the same collector records.

- [x] Promote the per-sandbox metering dimensions already in the store's per-org cumulative Totals to org-labeled gauges, fed from the SAME store-fed cumulative (`OnTotals`), so they match the billing figure: egress bytes (`mitos_usage_egress_bytes_total`) and memory GiB-seconds (`mitos_usage_mem_gib_seconds_total`) already existed; this adds GPU-seconds (`mitos_usage_gpu_seconds_total`). Label is org only: `Totals` keys on (org, sandbox, window) and carries no pool, so a pool label is a documented follow-up that needs the metering report to carry the husk pod's pool. No per-sandbox cardinality.
- [x] TDD: `TestMetricsObserveAllDimensions` asserts the egress and GPU gauges reflect the per-org totals, an empty-org row emits no series, and the label set is exactly {org}.
- [x] Clean; committed.

## Task 1.c: close the OTel trace tail

**Files:** the daemon/controller fork path; `internal/observability/tracing.go` (the tracer).

- [ ] Add a `guest-ready` span and a `first-exec` span so the trace is orchestrator to claim to fork to guest-ready to first-exec end to end. Hook the guest-readiness signal (the daemon knows when the guest agent answers) and the first exec on the daemon path; the W3C trace context already propagates controller to forkd, so continue it into these spans (stamp/read the trace id consistent with the existing `mitos.run/trace-id` annotation). Spans cost nothing when tracing is off (the no-op tracer).
- [ ] SECRET-CLEAN: span attributes carry only ids and durations, never argv/env/secret/output.
- [ ] TDD: with a recording tracer, a fork produces the guest-ready and first-exec spans as children of the fork span with the right names and a trace id; with tracing off, no cost/panic.
- [ ] Clean; commit.

## Self-Review

- Every metric is numbers plus trusted org/pool labels; no command/argv/env/secret anywhere.
- Cardinality is org/pool bounded; any per-sandbox use is documented and bounded.
- Samplers default off; self-host unaffected.
- 1.b reuses the Phase 0 store-fed cumulative records so the dashboard health/usage and the billing figure stay one number.
- After Phase 1: the operator and customer can see is-my-sandbox-starved (Vitals), per-org resource usage (gauges), and the full cold/warm start trace (guest-ready, first-exec). Phases 2 to 4 (Cilium/Hubble eBPF, OpenCost optional reconcile, console UX + neutral payment seam) build on this.
