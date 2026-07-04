# Observability

This document describes the three observability signals the control plane
exports today (distributed traces, a structured audit log, and Prometheus
metrics) and the `kubectl mitos` plugin for operators. Each section marks what
is PROVEN in CI versus what remains OPEN.

The governing rule is the secret-safety rule from CLAUDE.md: no signal ever
carries secret values, file content, env values, or bearer tokens. Traces carry
ids, counts, and timings; the audit log carries command strings, paths, and
byte counts; metric labels are pool names and fixed reason codes.

## Distributed tracing

### The trace model

A claim's life produces one trace spanning two processes:

```
controller.reconcileClaim         (controller)
  controller.forkOnNode           (controller)
    forkd.Fork                     (forkd, via gRPC)
      engine.fork                  (forkd, KVM snapshot restore)
```

- `controller.reconcileClaim` opens when the Sandbox reconciler runs.
  Attributes: `claim.name`, `claim.namespace`, and `pool` (the pool the claim
  resolves to).
- `controller.forkOnNode` covers node selection plus the gRPC call to the chosen
  forkd. Attributes: `node` (the selected node), `snapshot` (the snapshot id).
- `forkd.Fork` is the server span on the forkd side of the gRPC call.
  Attributes: `snapshot.id`, `sandbox.id`, and `fork_time_ms` once the fork
  completes.
- `engine.fork` covers the KVM snapshot restore inside forkd. Attributes:
  `snapshot.id`, `sandbox.id`, `fork_time_ms`.

### Cross-process propagation

The controller and forkd are distinct processes. The trace id crosses the gRPC
boundary by W3C trace-context propagation installed on the gRPC client and
server stats handlers (`observability.GRPCClientStatsHandler` and
`observability.GRPCServerStatsHandler`). The `forkd.Fork` span is therefore a
child of `controller.forkOnNode` under the same trace id, so a single trace
covers the controller's decision through the KVM restore.

### Enabling it

Tracing is off by default and zero-cost when off (no exporter is installed).
Enable it by pointing both processes at an OTLP gRPC collector:

```
controller --otlp-endpoint=otel-collector:4317
forkd      --otlp-endpoint=otel-collector:4317
```

The endpoint is a `host:port` for an OTLP gRPC receiver. An empty value disables
tracing.

### Trace-to-revision link

When a claim terminates with a bound workspace, the dehydrate path captures the
sandbox `/workspace` into a new WorkspaceRevision. That capture is a child span
and the reconcile trace id is carried onto the revision, so a committed revision
resolves to the exact orchestrator request that produced it, and the reverse:

```
controller.reconcileClaim         (controller)
  workspace.dehydrate             (controller, on terminate)
```

- `workspace.dehydrate` opens when `dehydrateOnTerminate` runs, as a child of
  `controller.reconcileClaim` (same trace id). Attributes: `workspace.name`,
  `revision.name`, `content.manifest.digest` (the contentManifest digest, a
  content address), `captured.path.count`, and `memory.snapshot.paired` (a
  bool). Names, a digest, a count, and a bool only; no secret value.
- The active reconcile trace id is stamped on the new revision as the
  `mitos.run/trace-id` annotation BEFORE the revision is created, but only
  when tracing is enabled (the trace id is valid). With tracing off (the no-op
  provider) the annotation is omitted: a fake all-zero id is never written.
- The same trace id rides the `revision.created` feed CloudEvent as its
  `traceId` field (read from the annotation, empty when absent), so an external
  indexer correlates the revision event with the orchestrator trace without
  polling and without a secret. See the Audit/feed surface and `internal/eventfeed`.

This links the CONTROL-plane trace to the revision. The guest-side first-exec
and guest-ready spans (the in-VM telemetry tail) are the remaining piece; see
OPEN.

### Secret safety

Spans carry only ids (claim name/namespace, pool, node, snapshot id, sandbox
id, workspace and revision names, the contentManifest digest), counts, and
timings. The `mitos.run/trace-id` annotation and the feed `traceId` field
are opaque correlation ids, not secrets. No span attribute, annotation, or feed
field carries a secret value, env value, file content, or token.

### PROVEN

- The span tree and attributes above are asserted with an in-memory span
  recorder in CI (`internal/observability`, plus the controller and daemon
  span tests).
- Cross-process trace-id propagation over gRPC is asserted: a server span shares
  the client span's trace id.
- The trace-to-revision link is asserted in CI: the reconcile trace id is
  stamped on the WorkspaceRevision (`mitos.run/trace-id`) and equals the
  `workspace.dehydrate` span's trace id, the span is a child of the reconcile
  span with the expected attributes and no secret values, the annotation is
  omitted when tracing is off, and the `revision.created` feed event carries the
  trace id (empty when absent).

### OPEN

- The guest-side first-exec and guest-ready spans (the in-VM telemetry bridge
  over vsock: cpu steal, balloon pressure, in-guest process table) are the
  bare-metal tail and are not yet wired.
- Grafana dashboards and PrometheusRule alerts that pivot on the trace id are a
  1.0 maturity item.

## Audit log

### What it is

forkd can emit one structured JSON record per exec or file operation served by
its sandbox API. Each record is one line of JSON:

```json
{
  "sandbox_id": "sbx-abc123",
  "op": "exec",
  "detail": "python train.py",
  "bytes": 0,
  "unix": 1749643200,
  "ok": true
}
```

Fields:

- `sandbox_id`: the sandbox the operation ran against.
- `op`: the operation kind (for example `exec`, file read, file write).
- `detail`: a safe human summary. For exec it is the command string, truncated
  to 256 runes with an explicit truncation note. For file ops it is the path.
  It never contains file content or secret values.
- `bytes`: the byte COUNT of file content read or written. The content itself
  is never recorded.
- `unix`: the event time in Unix seconds.
- `ok`: whether the handler served the operation without error. For exec a
  non-zero exit code is still `ok: true` (the call succeeded); the exit code is
  reported in `detail`.

### What is logged versus never logged

Logged: sandbox id, operation, the command string or file path, and the byte
count.

Never logged: file content, env values, secret values, and bearer tokens.
Commands are not secret values, so the command string is recorded; the 256-rune
bound only keeps records small.

### Enabling it

Auditing is off by default. Enable it on forkd:

```
forkd --audit-log=/var/log/mitos/audit.jsonl   # append to a file
forkd --audit-log=-                                # or "stderr": write to stderr
```

An empty value disables auditing. File paths are opened append-only (mode
`0o600`). Audit writes never break the request path: an encoding error drops the
record rather than failing the operation.

### PROVEN

Content-safety is asserted in CI: the audit tests confirm records carry the
command/path and byte count but never the file content or secret values, and
that exec commands are truncated.

## Metrics

All metrics are Prometheus and exposed at `/metrics`. Node-level fork metrics
live on forkd's default registry; controller-level claim and pool metrics
register with controller-runtime's registry on the controller's `/metrics`
endpoint. No metric carries a secret value; labels are pool names and fixed
reason codes only.

### Node-level (forkd)

| Metric | Type | Meaning |
|--------|------|---------|
| `mitos_fork_duration_seconds` | histogram | Time to fork a sandbox from snapshot, as measured by forkd. |
| `mitos_active_sandboxes` | gauge | Currently running sandboxes on this node. |
| `mitos_memory_shared_bytes` | gauge | CoW-aware shared memory: each template's shared page set counted once. |
| `mitos_memory_unique_bytes` | gauge | Per-fork unique (dirty-page) memory summed over all sandboxes. |
| `mitos_cow_memory_savings_bytes` | gauge | Memory the CoW model reveals is not consumed per-fork (naive minus CoW-aware). |
| `mitos_metered_disk_bytes` | gauge | CoW-aware metered backing storage: template volume seeds counted once. |

### Controller-level

| Metric | Type | Meaning |
|--------|------|---------|
| `mitos_claim_pending_total` | counter | Times a claim was requeued because no node had a ready snapshot (the claim stayed Pending). |
| `mitos_orphan_sweeps_total` | counter | Orphan sandbox VMs terminated by the garbage collector. |
| `mitos_claim_errors_total{pool,reason}` | counter | Claims that failed terminally, by pool and coarse reason (`fork`, `secret`, `volume`, `token`). |
| `mitos_pool_ready_snapshots{pool}` | gauge | Ready snapshots per pool, as of the last pool reconcile. |
| `mitos_pool_warm_dormant{pool}` | gauge | Dormant (unclaimed, warm) husk pods per pool, as of the last reconcile. |
| `mitos_pool_warm_in_use{pool}` | gauge | Claimed/active husk pods per pool (the demand the autoscaler sizes against). |
| `mitos_pool_desired_warm{pool}` | gauge | Autoscaler target dormant count per pool. |
| `mitos_pool_warm_scale_up_total{pool}` | counter | Times the autoscaler raised the dormant target, by pool. |
| `mitos_pool_warm_scale_down_total{pool}` | counter | Times the autoscaler lowered the dormant target, by pool. |
| `mitos_pool_refill_latency_seconds` | histogram | Seconds from creating a husk pod to it becoming a ready dormant warm slot (warm-pool refill cost). |
| `mitos_claim_wait_for_warm_seconds` | histogram | Seconds a claim waited from creation to activating a warm husk pod. |
| `mitos_husk_pod_created_total{pool}` | counter | Husk pods the controller created to fill the warm pool, by pool. |
| `mitos_husk_pod_lost_total{pool}` | counter | Times an active claim re-pended because its backing husk pod was lost (drain, eviction, deletion), by pool. |
| `mitos_node_lost_total{node}` | counter | Ready claims marked NodeLost after their node went unhealthy (raw-forkd path), by node. |
| `mitos_snapshot_distribution_lag_seconds{template}` | histogram | Seconds to distribute a template snapshot to a deficit node by pull from a holder, by template. Populated ONLY on the multi-node distribution path (a peer token configured AND a holder exists); the series is empty on a single-node cluster, which correctly means "no pull-based distribution happened", not "zero lag". |

Fleet health: `mitos_husk_pod_created_total` against a flat `mitos_pool_warm_dormant` reveals churn (pods lost as fast as created); `mitos_husk_pod_lost_total` and `mitos_node_lost_total` are the disruption signals the warm pool absorbs; `mitos_pool_refill_latency_seconds` is how fast a scaled-up or drained pool refills.

Counter versus gauge for pending: `mitos_claim_pending_total` is a counter of
pending-requeue EVENTS, not a live gauge of currently-pending claims. A counter
is exact and lock-free to bump at the requeue site; an honest live gauge of
currently-pending claims would need a periodic recount with its own staleness
window. The counter directly answers "how often are claims failing to place".

### PROVEN

The controller metric increments are asserted in CI (the increments fire on the
pending-requeue, orphan-sweep, claim-error, and pool-reconcile paths).

### OPEN

- `mitos_snapshot_distribution_lag_seconds` is defined, registered, observed at
  the pull site, and alerted on (`SnapshotDistributionLagHigh`). Its VALUE is
  populated only on the multi-node distribution path; a single-node cluster
  leaves the series empty by design (#3).
- Per-sandbox egress observability is the nftables egress counter (#211) and cost
  attribution is the CoW-aware metering pipeline (#33); the Hubble and OpenCost
  layers were dropped as the wrong tools (see "Observability layers" below).

The Grafana dashboard, PrometheusRule alerts with runbook URLs, and the
conditions/reason-code catalogue that ride on these metrics are shipped; see
"Dashboards, alerts, runbooks" below.

## `kubectl mitos` plugin

The `kubectl-mitos` binary is a kubectl plugin that lists mitos.run sandbox
objects. It resolves the cluster connection from the standard kubeconfig
resolution (`KUBECONFIG`, `--kubeconfig`, or in-cluster).

### Subcommands

```
kubectl mitos ls [-n namespace] [-A]          list Sandboxes
kubectl mitos ps [name] [-n namespace] [-A]   list fork Sandboxes, or one sandbox's forks
```

- `ls` prints Sandboxes with columns NAME, POOL, PHASE, NODE, ENDPOINT, AGE.
- `ps` prints fork Sandboxes with columns NAME, SOURCE, READY, AGE. Given a sandbox
  name, it filters to forks whose source is that sandbox.
- `-n` scopes to a namespace (default `default`); `-A` lists all namespaces.

Ages render kubectl-style (`30s`, `2m`, `3h`, `5d`); missing node, endpoint, or
source cells render as `-`.

### Installing it

kubectl discovers plugins named `kubectl-<name>` on PATH. Build the binary and
put it on PATH as `kubectl-mitos`:

```
go build -o kubectl-mitos ./cmd/kubectl-mitos/
mv kubectl-mitos /usr/local/bin/        # any directory on PATH
kubectl mitos ls
```

### PROVEN

The table formatting is asserted in CI: columns, values, kubectl-style age
strings, empty-list messages, and missing-cell dashes
(`internal/cli/sandboxtable`).

### OPEN

`kubectl mitos top/tree/exec/logs` are documented follow-ups; invoking
them prints a "not yet implemented" notice.

## Dashboards, alerts, runbooks

The `deploy/monitoring/` kustomize layer packages a Grafana dashboard and a
PrometheusRule over the metrics above, with one runbook per alert. It is OPT-IN:
it is NOT part of the default `kubectl apply -k deploy/` base, because it has
external dependencies. Install it separately once those are present:

```
kubectl apply -k deploy/monitoring/
```

### Artifacts

- `deploy/monitoring/prometheusrule.yaml`: a `monitoring.coreos.com/v1`
  PrometheusRule with five alerts: ClaimErrorRateHigh, ClaimsPendingSustained,
  WarmPoolStarved, OrphanSweepSpike, and ForkLatencyHigh. Each alert links a
  `docs/runbooks/` file via its `runbook_url` annotation.
- `deploy/monitoring/dashboard.json` plus `dashboard-configmap.yaml`: a Grafana
  dashboard (fork latency p50/p99, active sandboxes, CoW memory density, metered
  disk, claims pending, claim error rate by pool/reason, pool ready snapshots,
  orphan sweeps), wrapped in a ConfigMap.
- `docs/runbooks/*.md`: one runbook per alert (signal, likely causes, diagnosis
  with `kubectl mitos ls/ps/top` and the metrics to check, remediation,
  escalation).
- `docs/conditions.md`: the normative reason-code catalogue the runbooks cite.

### Dependencies

- The PrometheusRule needs the Prometheus Operator (the
  `monitoring.coreos.com/v1` PrometheusRule CRD) and an operator whose
  `ruleSelector` matches the rule's labels.
- The dashboard ConfigMap uses the Grafana sidecar convention: the
  `grafana_dashboard: "1"` label, picked up by a Grafana running the dashboard
  sidecar (the kube-prometheus-stack default).

### Thresholds and the latency target

Every alert threshold is environment-tunable: the numbers in the PrometheusRule
are defensible starting points, not established SLOs, and each runbook says to
tune them from the cluster's observed baseline. In particular the ForkLatencyHigh
threshold is NOT the bare-metal latency target: the `<=10ms` p99 fork is a
bare-metal TARGET, while the alert fires on a looser, cluster-specific budget so a
busy or virtualized node does not page on the target itself.

### PROVEN

- The PrometheusRule is promtool-validated in CI (the manifests job extracts
  `.spec.groups` and runs `promtool check rules`), so a bad PromQL expression
  fails the build.
- The dashboard and the alerts reference only metrics the control plane actually
  exports (verified by grepping the metric names in `deploy/monitoring/` against
  `internal/`).
- The reason-code catalogue in `docs/conditions.md` covers every condition reason
  the controllers emit.

### OPEN

- Per-cluster threshold tuning is left to the operator (the runbooks say to tune).
- Hubble flow panels and OpenCost cost-attribution dashboards were dropped as the
  wrong tools for this architecture; per-sandbox egress and cost are served by the
  nftables egress counter (#211) and CoW-aware metering (#33) instead (see
  "Observability layers" below).

The dashboard and the PrometheusRule alerts are packaged BOTH as the
`deploy/monitoring/` kustomize layer and in the Helm chart under an opt-in
`monitoring.enabled` toggle (`deploy/charts/mitos`). The
`SnapshotDistributionLagHigh` alert ships in both layers; its metric is
populated only on the multi-node distribution path.

### SaaS control-plane alerts (chart only, issue #617)

The chart's PrometheusRule additionally carries a `mitos-saas` group covering
the hosted control plane: gateway 5xx rate and auth-denial spikes
(`mitos_gateway_requests_total`, `mitos_gateway_auth_denials_total`), billing
webhook signature failures and handler errors
(`mitos_billing_webhook_verify_failures_total`,
`mitos_billing_webhook_errors_total`), drawdown driver failures and stalls
(`mitos_drawdown_cycle_errors_total`,
`mitos_drawdown_last_success_timestamp_seconds`), org credit exhaustion
volume (`mitos_drawdown_credit_exhausted_total`), usage collector failures
and stalls (`mitos_usage_collect_cycle_failures_total`,
`mitos_usage_collect_cycle_duration_seconds` staleness), Postgres
reachability as seen by the console readiness probe
(`mitos_console_db_ping_failures_total`), and console availability (`up` from
the console PodMonitor). Each alert links a runbook in `docs/runbooks/`.

The group lives ONLY in the chart (the canary precedent): the gateway and
console ship only via the chart, and every rule is gated on the component
that emits its series (`gateway.enabled`, `console.enabled`,
`controller.usage.collector`), so a self-host install without the hosted
surfaces renders no dead rules. The gateway and console export these metrics
on a separate cluster-internal listener (`--metrics-addr`, :9100), scraped by
their own PodMonitors; the public Services never expose it. The chart-rendered
rule (including this group) is promtool-validated in the helm-lint workflow.

Deliberately absent: a gateway-side database probe metric (the gateway readyz
has no DB ping today; console reachability stands in for the shared database)
and a gateway-unavailable rule (a dead gateway exports nothing; `CanaryDown`
is the user-path signal for that).

## Observability layers: the guest telemetry bridge (Hubble and OpenCost dropped)

Layer 3 (the guest telemetry bridge) is built. Layers 1 (Hubble flow logs) and 2
(OpenCost cost attribution) were considered and dropped as the wrong tools for a
microVM-on-nftables architecture; the correct, already-shipped solutions are
named below (issue #164).

### Layer 1: Cilium Hubble flow logs (dropped)

Not built, intentionally. Sandbox egress is enforced by host-side nftables (a
per-sandbox tap + /30 + default-deny chain), not Cilium NetworkPolicy, so a
denial happens in nftables before Cilium's eBPF datapath ever sees the packet:
Hubble cannot observe the per-sandbox policy drop, and attributing one to a claim
would imply a pod-scoped CNI mechanism governs sandboxes, which it does not (the
honest-Kubernetes-semantics principle). Per-sandbox egress observability is
served instead by the nftables per-sandbox egress counter the metering pipeline
reads (#211), which sits on the datapath the traffic actually travels.

### Layer 2: OpenCost cost attribution (dropped)

Not built, intentionally. OpenCost attributes cost per pod from pod resource
usage, which double-counts the shared copy-on-write memory across forks of one
template, the exact error CoW-aware metering (#33) was built to eliminate. Cost
attribution is served instead by the CoW-aware metering pipeline
(`internal/metering`, `GET /v1/metering`, `mitos_cow_memory_savings_bytes`),
which counts each template's shared page set once.

### Layer 3: guest telemetry bridge (over vsock)

The guest agent samples CPU steal (`/proc/stat` steal field), memory vs balloon
(`/proc/meminfo`), and the in-guest process table (`/proc/<pid>/stat`), and
serves them over the Connect `sandbox.v1.Sandbox/Vitals` (server-stream of
samples) and `Processes` (unary process table) RPCs, bearer-gated like exec.
`kubectl mitos ps <name> --processes` consumes the REAL in-guest process table;
when the guest is unreachable (claim not running, no KVM behind it) it falls back
to the fork Sandbox listing, so it never renders a fabricated table.

The claim/pool/workspace labels rendered alongside the table are Kubernetes
control-plane metadata the guest cannot know (the guest is just a VM), so the
`ps` consumer resolves them from the Sandbox object (`sandboxLabels`: claim is
the sandbox name, pool is `spec.source.poolRef`, workspace is
`spec.workspaceRef`), not over the guest RPC. A label the object does not carry
renders as a dash; nothing is guessed. The labels are object names, never secrets.

The snapshot carries no secrets: process entries are the program name (`comm`)
and resource counters, never argv or the process environment.

PROVEN: the `/proc/stat` steal parse, the process-table snapshot, the balloon
arithmetic, the vsock message shapes, the CRD-sourced labels, and the `ps`
consumer are unit-tested on darwin against fixtures (`internal/guestvitals`,
`internal/vsock`, `internal/daemon/vitals_api_test.go`,
`cmd/kubectl-mitos/ps_guest_test.go`).

GATED: the real in-VM `/proc` sampling runs only on a KVM guest; the guest
collector reads real `/proc` and is exercised by the fixture tests in
`guest/agent-rs/src/service/vitals.rs` so the next KVM run drives it end to end.
