# Billing-grade usage metering pipeline

This document describes how Mitos turns per-node CoW-aware operational metering
(`internal/metering`, the forkd `GET /v1/metering` endpoint, docs/metering.md)
into per-organization, time-integrated, auditable usage records, and the
org-scoped public usage API on top of them. The implementation is
`internal/usage`.

This pipeline does NOT re-implement metering. The node `metering.Report` is the
source of truth for instantaneous footprint and CoW deduplication; this pipeline
AGGREGATES those reports across nodes and across time into billable usage
records, idempotently, so the records survive node loss, controller restart, and
late or duplicate samples without double-billing.

## What it produces: billable units

A `UsageRecord` is scoped to `(OrgID, SandboxID, Window)` and carries these
billable units, integrated over the record's window:

- **vCPU-seconds** (`VCPUSeconds`): the sandbox's allocated vCPU count integrated
  over the wall-clock seconds it was alive in the window. A 2-vCPU sandbox alive
  for the whole 60s window contributes 120 vCPU-seconds.
- **GiB-seconds of memory** (`MemGiBSeconds`): the sandbox's CoW-aware resident
  memory (in GiB) integrated over the window's seconds. This integrates the
  CoW-aware figure, never the naive per-fork sum (see "CoW reconciliation").
- **storage GiB-hours** (`StorageGiBHours`): the sandbox's CoW-aware backing
  storage (in GiB) integrated over the window's hours.
- **egress bytes** (`EgressBytes`): network egress attributed to the sandbox in
  the window, from the per-sandbox nftables egress counter. This is
  a monotonic cumulative counter on the source, so the window's egress is the
  delta of the counter across the window, never a re-integration.
- **GPU-seconds** (`GPUSeconds`): the sandbox's billable GPU-seconds in the
  window. Like egress this is a monotonic cumulative counter on the
  source (`Sample.GPUSeconds` is already wall-seconds times GPU count), so the
  window value is the counter delta.

The split between *rate* units and *counter* units is load-bearing:

- **Rate units** (vCPU, memory, storage) are instantaneous quantities the source
  reports as a level. The pipeline integrates them: `level * elapsed_seconds`,
  trapezoid-free (left-rectangle: each sample's level holds until the next
  sample). Per-second granularity.
- **Counter units** (egress bytes, GPU-seconds) are already cumulative on the
  source. The pipeline sums the positive steps between consecutive in-window
  samples, so a missed scrape never loses counter progress and a duplicate scrape
  never adds it twice. A DECREASE between consecutive samples means the cumulative
  counter RESET (a sandbox restart zeroes its nftables egress / GPU counter): the
  post-reset value is counted as fresh progress from zero, never a negative bill.
  For a counter that never resets this is exactly last-minus-first; the
  step-sum is what keeps a restart within the window honest rather than negative.

## Time integration

A `Sample` is one scrape of one sandbox at an instant: it carries the org tag,
the sandbox id, the node, the timestamp, the instantaneous rate levels (vCPUs,
CoW-aware memory bytes, CoW-aware disk bytes), and the cumulative counters
(egress bytes, GPU-seconds). The integrator (`Integrate`) is PURE: given an
ordered set of samples for one sandbox it produces the `UsageRecord`s, one per
fixed-width window.

Windows are fixed, wall-clock-aligned buckets (default 60s, `DefaultWindow`).
Aligning windows to the wall clock (not to a sandbox's birth) is what makes the
record key `(sandbox, window)` STABLE across collectors, restarts, and replays:
two independent collectors that scrape the same sandbox in the same minute
produce records with the same key, so the store deduplicates them.

For rate units the integral within a window is the sum over consecutive sample
pairs of `level(earlier) * (t_later - t_earlier)`, clipped to the window bounds.
The level of the earlier sample is held until the later sample (left-rectangle
integration), which is exact for a step-function level and conservative for a
monotonically changing one.

### Missed-scrape behavior (decision: bounded hold, then gap)

If consecutive samples are more than `MaxHold` apart (default 2 x the window),
the integrator does NOT interpolate the level across the entire span. It holds
the earlier level for `MaxHold` and then records a GAP for the remainder (the
gap contributes zero to rate units). This is the honest choice: a long silence
usually means the node or the sandbox was gone, not that it ran steadily the
whole time. Counter units are unaffected by gaps because they are read as a
delta of the cumulative counter, which already reflects whatever happened during
the silence. The chosen behavior is HOLD-THEN-GAP, not INTERPOLATE, and it is
tested.

## Idempotent, durable records

`UsageRecord`s are idempotent on `(OrgID, SandboxID, Window)`. The
`UsageStore.UpsertRecord` contract is: writing a record for a key that already
exists REPLACES it with the recomputed value rather than adding to it. Because
the integrator is pure and deterministic over the samples in a window, replaying
the same or overlapping samples recomputes the same record value, so:

- a **duplicate** sample (same sandbox, same timestamp) is dropped before
  integration (de-duplicated by `(sandbox, timestamp)`), so it cannot inflate a
  rate integral;
- a **late** sample that lands in an already-written window recomputes that
  window's record to the same or a more-complete value, never an additive one;
- a **node loss** or **controller restart** loses only in-flight samples; the
  windows already persisted are untouched, and re-scraping overlapping windows
  re-derives the same records.

The store is the pluggable seam (`UsageStore`), mirroring the accounts `Store`
pattern. `MemUsageStore` is the tested in-memory reference (lost on restart;
DEV ONLY). The durable backend is `pgstore.PgUsageStore`
(`internal/saas/pgstore/usagestore.go`): a Postgres `usage_records` table whose
primary key is exactly the idempotency key `(org_id, sandbox_id, window_start)`,
so `UpsertRecord` is an `INSERT ... ON CONFLICT DO UPDATE` that REPLACES the row
(never adds), and `ListRecords` / `TotalsByOrg` are org-scoped reads. Both stores
run the SAME behavioral contract (`internal/usage/usagestoretest`), so the
durable store is proven equivalent to the reference: idempotent upsert, per-org
isolation, half-open `[from, to)` period bounds, and per-org cumulative totals.

The controller wires the backend behind `--usage-database-dsn` (falling back to
the `MITOS_DATABASE_DSN` env var): when a DSN is set, the collector and the
internal usage API use `PgUsageStore` so metered consumption survives a
controller restart; absent a DSN they use the in-memory store. The DSN is a
secret and is never logged (only the chosen backend is). A configured-but-
unreachable DSN fails startup loud rather than silently falling back to a store
that would lose usage. The `TotalsByOrg` figure for the durable store is a direct
`SUM` aggregate over the table (there is no eviction to survive, unlike the
in-memory store's delta-tracked cumulative), so it is the true billing total.

## CoW reconciliation (no double-count of shared memory)

The memory rate level the integrator consumes is the CoW-aware figure, never the
naive per-fork sum. When a node report is converted to per-sandbox samples
(`SamplesFromReport`), each sandbox's memory level is its own unique memory plus
its amortized share of its template's shared-once set: the template's
`SharedOnce` is split evenly across the forks that map it, so summing every
fork's memory level across the node reconstructs exactly `UsedCoWAware`, never
`UsedNaive`. The shared template region is therefore billed ONCE across the forks
that share it, not once per fork.

For audit, both figures are kept visible: each sample carries `MemUniqueBytes`
and `MemSharedAmortizedBytes` separately, and `SamplesFromReport` also returns
the node's naive-vs-CoW totals so a reconciliation report can show the operator
the exact bytes the CoW model removed from the bill (the `CoWSavings` line from
docs/metering.md). The billable memory level is `unique + shared_amortized`; the
naive level a per-VM biller would have charged is `unique + shared_full`, and the
difference is the audit-visible CoW saving.

## Collection seam and the org mapping

The collector (`Collector`) scrapes each node on a fixed cadence and tags every
sample with org, sandbox, node, and timestamp through an injectable
`SampleSource`. The live multi-node scrape is now wired (issue #164, Phase 0):
`NodeRegistrySource` (`internal/usage/livesource.go`) lists the forkd nodes from
the controller NodeRegistry, HTTP GETs `GET /v1/metering` per node with a bounded
per-request timeout, parses the metering report, and converts each per-sandbox
row to an org-tagged `Sample`. A node that is unreachable or errors is skipped and
counted, never failing the whole cycle. The collector runs behind the
`--usage-collector` controller flag, which is OFF by default so a self-host
deployment that does not want metering is unaffected; it is turned on for hosted
multi-tenant. See `docs/design/observability-and-billing-spine.md`.

The **org tag** comes from the sandbox -> owning-org mapping, and it is a billing
trust boundary: the org is derived solely from control-plane identity, never from
client input. A sandbox is created in the org's hard-isolation namespace
`mitos-org-<id>`; the controller stamps the trusted `mitos.run/org` label on the
sandbox's husk pod, derived from that namespace via `tenant.OrgFromNamespace` (a
client-set `mitos.run/org` on the input is ignored). The `OrgResolver` seam
(`OrgFor(sandboxID) -> orgID`) is implemented live by `LabelOrgResolver`
(`internal/usage/k8sresolver.go`), which reads that controller-stamped label off
the husk pod. A self-host sandbox in a non-org namespace carries no org label and
is left unattributed (kept in node-reconciliation totals, dropped from billable),
never misbilled to a default org.

## Public usage API (org-scoped)

`UsageHandler` is an HTTP handler that serves an org's current and historical
usage and cost. It sits BEHIND the gateway front door: the gateway verifies
the customer key, resolves the org, and forwards with the org attached, so the
handler reads the org from the request context (`OrgFromContext`), never from a
query parameter. A request can therefore only ever read its OWN org's usage; the
cross-org isolation is enforced by the context org, not by the caller, and is
unit-tested (a request carrying org A's context never sees org B's records even
if it names org B in the path).

The response carries the per-window records, the rolled-up totals per billable
unit, and a cost estimate computed from a `PriceList` (a simple per-unit rate
table). Both E2B and Daytona lack a real usage API; this org-scoped, per-unit,
auditable endpoint is a deliberate differentiator.

## Stripe seam

The usage records are the input to Stripe metered billing, which is
now implemented in `internal/saas/billing` (see docs/saas/pricing.md):
`billing.Service.PushUsage` reads finalized `UsageRecord`s and pushes one metered
usage event per non-zero meter to Stripe. The idempotency key for the Stripe push
is the same `(org, sandbox, window)` record key plus the meter, so a retried push
never double-reports. The billing slice builds against a `StripeClient` interface
with a `FakeStripe` for tests; the real Stripe SDK adapter is a documented seam.
This usage pipeline produces the auditable records the push consumes.

## What is a seam / follow-up

- Real multi-node HTTP scrape of `GET /v1/metering` (implements `SampleSource`): DONE.
- Durable Postgres `UsageStore` (implements the upsert-by-key contract): DONE
  (`pgstore.PgUsageStore`, migration `0003_usage_records.sql`).
- Real `OrgResolver` reading the claim -> org label: DONE.
- Stripe metered-billing push: implemented in `internal/saas/billing`.

The in-memory defaults remain so the integration, idempotency, CoW
reconciliation, and org-scoping are fully verifiable on darwin without a cluster;
the Postgres store runs the same contract in CI against a Postgres service (set
`MITOS_TEST_DATABASE_DSN`), and skips locally when no database is configured.
