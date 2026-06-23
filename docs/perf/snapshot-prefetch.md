# Snapshot-resume page-fault prefetch

Tracking: issue #167. Status: IMPLEMENTED and MEASURED. The hugepage build-time
plumbing, the Firecracker userfaultfd restore backend, the manifest hot-page +
hugepage descriptors, capture-at-template, and the off-vs-on prefetch benchmark
are all in place, and the fault-count + claim->first-exec numbers are measured on
a userfaultfd-capable node (see the Measured section below and
`bench/results/2026-06-21-kvm-perf-correctness.md`). The remaining open item is
concurrent-density measurement under a real claim storm.

## Critical prerequisite (learned on real hardware)

Firecracker REFUSES to restore a hugetlbfs-backed snapshot through the plain
file-mapping backend: it returns "Cannot restore hugetlbfs backed snapshot by
mapping the memory file. Please use uffd." The two levers below are therefore
COUPLED on the restore path: hugepage restore is impossible WITHOUT the
userfaultfd backend, and both require a kernel built with `CONFIG_USERFAULTFD=y`.
A minimal/rescue kernel commonly omits it (the Hetzner rescue kernel has
`# CONFIG_USERFAULTFD is not set`, so `userfaultfd(2)` returns ENOSYS and
Firecracker fails restore with "Failed to UFFD object: System error"). `mitos
doctor` checks for this; see docs/platforms/host-prerequisites.md.

## The problem

Restoring a Firecracker snapshot is two costs, not one. The KVM restore that
reloads device state and remaps the guest memory file is fast (on the order of
~0.8ms). What follows is a tail of lazy page faults: the guest memory file is
mapped lazily, so the first time the resumed guest touches a page that is not
yet resident, it faults, and the page is brought in on demand. A freshly resumed
sandbox that immediately starts doing work touches thousands to tens of
thousands of distinct pages in its first moments, and each one is a fault. That
fault tail, not the restore, dominates claim->first-exec.

Browser Use reported the shape of this on their stack: a "0.8ms restore plus
~40ms of lazy faults is a ~40ms sandbox", and that backing the guest memory with
2 MiB hugepages plus a userfaultfd handler that PRELOADS a captured hot-page
working set before resume cut resume-to-ready from 9.8s to 3.1s and page faults
from ~100k to ~1.1k. Those are their numbers on their workload; we cite them as
the motivation, not as a Mitos measurement.

## The two levers

### 1. Hugepage-backed guest memory (2 MiB pages)

Firecracker can back guest memory with hugetlbfs (2 MiB huge pages) instead of
4 KiB base pages. With 2 MiB pages, one fault moves 2 MiB of memory rather than
4 KiB, so the same resident working set is reached in ~512x fewer faults. The
fault COUNT is what the userfaultfd handler and the kernel pay per-event
overhead on, so cutting the count is most of the win even before prefetch.

This requires the host to have hugepages reserved and Firecracker configured to
use a hugetlbfs-backed memory file. It is a host and template-build concern, and
it only exists on Linux/KVM.

### 2. userfaultfd prefetch of a captured hot-page set

`userfaultfd(2)` lets a userspace handler service page faults for a memory
range. On restore, Mitos maps the guest memory file with userfaultfd registered
over its range and runs a handler. Before resuming the VM, the handler PRELOADS
the snapshot's captured hot-page working set: it faults in exactly the pages the
guest is known to touch first, so the post-resume fault storm is paid up front,
sequentially, off the critical path the guest itself would otherwise serialize
on. The pages NOT in the hot set are still served lazily by the same handler.

The handler skeleton lives in `internal/fork/prefetch_linux.go` (Linux build
tag) with a non-Linux stub in `internal/fork/prefetch_other.go`. The skeleton
defines the real shape (`newPrefetchHandler` -> `Register` -> `Preload` ->
`Serve` -> `Close`, plus `CaptureTrace` for the capture path); the
syscall-level register / `UFFDIO_COPY` wiring is the bare-metal follow-up,
because it needs a live KVM host and a hugepage-backed memory file to exercise.

## Capturing the hot-page set

The hot-page set is the list of guest memory page offsets to preload. It is
captured by running a resume in CAPTURE mode: the userfaultfd handler records
every fault it services as a `fork.FaultRecord` (the faulting byte offset), and
the resulting trace is reduced to a shippable descriptor by
`fork.SelectHotPages`.

`SelectHotPages` is PURE and platform-independent (it has no userfaultfd
dependency), so it is fully unit-tested on any host including darwin. The
reduction:

1. floor each fault offset to its containing page (the prefetch unit is a page);
2. count faults per page (per-page frequency is the hotness signal);
3. when a cap is set, keep only the hottest pages (frequency desc, ties by
   lowest offset for determinism), so a pathological trace cannot ask the
   handler to preload the whole image;
4. emit the kept offsets sorted ascending, so the handler prefetches the memory
   file sequentially and the descriptor's identity does not depend on capture
   order.

### When capture runs

Capture runs where a resume already happens with no tenant on the critical path:

- **At CreateTemplate / template build**: the template VM is booted, init runs,
  then it is paused and snapshotted. A capture resume of that snapshot produces
  the hot-page set for the template, stamped onto the template manifest.
- **At first warm**: the first fork of a pool can run in capture mode, and its
  trace refines the template's hot-page set for the pool's real workload.

Either way the set is captured off the tenant-facing claim path and shipped with
the snapshot, so a tenant claim never pays for capture.

## Shipping the set: the manifest hot-page descriptor

The hot-page set is an OPTIONAL, content-addressed field on the snapshot
manifest, `cas.HotPageSet` on `cas.Manifest.HotPages`:

```
type HotPageSet struct {
    PageSizeBytes int64   // prefetch unit (2 MiB for hugepage-backed memory)
    File          string  // manifest file the offsets index into ("mem")
    Offsets       []int64 // byte offsets of the hot pages within File
}
```

### Compatibility with the snapshot format freeze (#32)

The field is purely additive. A snapshot that never captured a hot-page set
carries a nil `HotPages`, and the manifest's canonical encoding OMITS the field
entirely in that case, so the canonical bytes and the digest are byte-identical
to a snapshot built before the field existed. An empty (non-nil, zero-offset)
set is treated the same way: identity-neutral, omitted. This is proven by
`TestManifestNilHotPagesPreservesLegacyDigest`.

Therefore the field does NOT require a `SnapshotFormatVersion` bump: it does not
change the on-disk snapshot layout or the restore contract, and an old build
that does not understand `hotPages` simply restores lazily (it ignores the field
and pays the full fault tail, exactly as today). The format-version interaction
is: the field is forward-compatible within the current version; a future
version bump is needed only if the descriptor itself changes incompatibly (for
example a different offset encoding), at which point the format-version policy in
docs/snapshot-format.md governs the transition.

### Content-addressing and the #33 CoW reconciliation

When the hot-page set is present and non-empty it IS part of the
content-addressed digest. The canonical encoding sorts and de-duplicates the
offsets, so two snapshots that prefetch the SAME pages produce the SAME digest,
and two that prefetch different pages produce different digests (proven by
`TestManifestHotPagesChangeDigest` and
`TestManifestHotPagesCanonicalOrderInvariant`).

This reconciles cleanly with the #33 copy-on-write fork story. Under #33, N forks
of one template share the template's restored page set, and CoW-aware metering
(docs/metering.md) counts that shared set ONCE rather than once per fork. The
hot-page set is a property of the TEMPLATE snapshot, not of an individual fork:
every fork of a template restores from the same snapshot, with the same manifest
digest, and therefore the same hot-page set. So:

- The hot-page descriptor itself is shared content. Because it is part of the
  manifest's content address, all N forks reference one identical descriptor;
  it is stored and counted once, never N times.
- The pages it names are the shared, read-mostly template region: the pages a
  freshly resumed guest touches first are overwhelmingly the ones common to
  every fork (kernel text, libc, the warmed runtime), which is exactly the
  region CoW shares and metering counts once. Prefetching them does not create
  per-fork private dirty pages; it just makes the already-shared pages resident
  sooner. The CoW accounting is unchanged: prefetched shared pages still count
  once across forks.
- Per-fork PRIVATE faults (pages a specific fork dirties) are NOT in the
  template hot-page set and remain lazily served per fork, so prefetch never
  inflates a fork's unique set.

In short: the hot-page set rides the same content-addressed sharing as the
snapshot it describes, so it is counted once across forks exactly like the page
set it accelerates.

## Bench plan

The bench hook is `cmd/bench --mode prefetch`, backed by the pure, unit-tested
aggregation `benchstat.AggregatePrefetch` / `benchstat.PrefetchComparison`. The
plan, on the bare-metal reference node (#16):

- Two arms: prefetch-OFF (lazy-fault baseline) and prefetch-ON (hot-page set
  preloaded before resume).
- Per resume, record (a) the page-fault count the userfaultfd handler services
  and (b) the claim->first-exec latency.
- Report per-arm distributions plus the headline FAULT REDUCTION (off mean -
  on mean) and the claim->first-exec P50/P99 on vs off.

The aggregation and its JSON serialization are tested today
(`internal/benchstat/prefetch_test.go`); the missing piece is the real
fault-count source from the userfaultfd handler. Until that lands, `--mode
prefetch` fails with a clear not-yet-measurable message rather than emitting a
fabricated number. Off any non-KVM host the engine fails to construct before the
mode runs at all, so no number is ever invented off bare metal.

### Measured (2026-06-21, userfaultfd-capable node)

Measured on an i7-6700 reinstalled to stock Debian 12 (CONFIG_USERFAULTFD=y),
python:3.12-slim, N=20. Full table and analysis in
`bench/results/2026-06-21-kvm-perf-correctness.md`. Headline:

- Fault count per resume: hugepages alone cut it ~78x (1877 base-page faults vs
  24 huge-page faults at the same workload, prefetch off). Prefetch cuts the
  residual further: 1877 -> 45 on 4 KiB, 24 -> 2 on 2 MiB.
- claim->first-exec p50 improves with prefetch WITHIN the UFFD path: 153 -> 117 ms
  on 4 KiB, 116 -> 109 ms on 2 MiB.
- Honest nuance: the UFFD path adds per-fault handler overhead, so its absolute
  claim->exec (108-153 ms) is higher than the plain file-mapped 4 KiB baseline
  (~51-67 ms). The win is the fault-count collapse (the per-fault cost is what
  scales under concurrent load) plus the within-UFFD prefetch latency gain;
  hugepage restore has no file-mapped alternative, so for hugepages the UFFD path
  is the only path. Concurrent-density gains under a real claim storm remain to be
  measured (this run measures single-fork resume).

## Security surface

The hot-page descriptor does not move the snapshot trust boundary. It is part of
the content-addressed manifest digest, so it rides the same verify-on-load
integrity control as the rest of the snapshot (docs/snapshot-distribution.md,
docs/threat-model.md): a tampered hot-page list changes the manifest digest and
fails verify before any restore. The offsets the handler would preload are
offsets into the SAME guest memory file the restore already maps and the same
file the digest already covers, so prefetch reads no data the lazy path would
not read anyway, just sooner. No new external input, trust boundary, or
host-path write is introduced by the descriptor or the selection logic. The
actual userfaultfd handler is a security-sensitive restore-path change and gets
its threat-model review when its syscall wiring lands (it is gated and not wired
in this slice).

## How the userfaultfd backend works (implemented)

On a UFFD-backed restore the engine (`internal/fork/uffd_engine.go`,
`uffd_linux.go`) binds a private per-fork unix socket under the sandbox dir and
points Firecracker at it via `mem_backend` (`backend_type: "Uffd"`) on
`/snapshot/load` (paused). Firecracker connects, creates the userfaultfd over the
guest memory, and sends the handler ONE handshake message: the region mappings as
JSON plus the userfaultfd descriptor as SCM_RIGHTS ancillary data. The handler
mmaps the snapshot mem file read-only, PRELOADS the captured hot-page set (if any)
with `UFFDIO_COPY` before the engine resumes, then serves the remaining faults
lazily for the life of the VM, sourcing each page from the mem file. The engine
chooses the UFFD backend when the node is configured for hugepages, the snapshot's
manifest records a hugepage backing, or a hot-page set is present; the handler is
closed on Terminate and on any fork failure.

The snapshot is SELF-DESCRIBING: `cas.Manifest.HugePages` records the page backing
(omitempty, #32-safe like `HotPages`), so any node, even one whose own config does
not request hugepages, knows it must restore a hugepage snapshot through UFFD.

## What is implemented now vs deferred

Implemented and tested (darwin unit + Linux build; KVM integration on a
userfaultfd-capable node):

- `cas.HotPageSet` and `cas.Manifest.HugePages` manifest descriptors, optional and
  additive, content-addressed when present, identity-neutral when empty (#32-safe).
- `fork.SelectHotPages` capture-selection and the pure region/offset arithmetic
  (`internal/fork/uffd.go`), unit-tested on any host.
- The Firecracker userfaultfd restore backend: handshake (SCM_RIGHTS recv +
  mappings), `UFFDIO_COPY` preload and lazy serve, fault counting
  (`internal/fork/uffd_linux.go`); engine restore-path integration and
  `LoadSnapshotUFFD` (`mem_backend`).
- Hugepage build-time plumbing (`huge_pages` machine-config, `tmpl-smoke
  --huge-pages`).
- `engine.CaptureTemplateHotPages` (capture-at-template, re-stamps the manifest).
- `benchstat.AggregatePrefetch` / `PrefetchComparison` and `cmd/bench --mode
  prefetch` real off-vs-on arms over the UFFD restore.

Deferred:

- The real fault-count + claim->first-exec MEASUREMENT and its `bench/results/`
  entry, on a `CONFIG_USERFAULTFD=y` node (the Hetzner rescue kernel lacks it, so
  the measurement runs on a stock-kernel install).
- Capture-at-first-warm (refining the template set with a pool's real workload);
  capture-at-template is implemented.
