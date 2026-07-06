# Vendored Firecracker for cheap LIVE fork (Layer 2) Scoping Plan

> **For agentic workers:** this is a design and scoping doc, not a task-by-task
> implementation plan. It defines WHY we vendor a patched Firecracker, WHAT the
> patch points are, and a milestone ladder where each milestone (m1 to m5) is a
> future PR or issue. Do not start coding from this doc; each milestone gets its
> own plan with checkbox tasks. REQUIRED SUB-SKILL for those follow-ups:
> superpowers:writing-plans then superpowers:subagent-driven-development.

**Goal:** make a LIVE fork (`ForkRunning`) cost roughly `0.12 MiB/child` of new
host memory plus an ASYNC parent resume, matching the competitor
deeplethe/forkd v0.4 live BRANCH (source-pause window `56 ms p50` on a 1.5 GiB
source, per its published `bench/live-fork-pause-window/RESULTS-v0.4.md`).
Stock Firecracker v1.15.0 (the version pinned in `hack/install-firecracker.sh`
and `Dockerfile.forkd`) cannot do this. The user has greenlit vendoring a
patched Firecracker fork. This doc scopes that fork.

**Non-goal restatement of the honesty rule (CLAUDE.md principle 1):** the
`0.12 MiB/child` and the async-resume window are the competitor's numbers and
our TARGET. They are not a mitos claim yet. m1 measures our real per-child MiB
on KVM; nothing lands in README or marketing until `bench/` reproduces it.

**Architecture (target):** the running parent guest RAM is backed by a
`memfd` mapped `MAP_SHARED`, not the anonymous private mapping stock Firecracker
uses. Each child Firecracker maps that same memfd `MAP_PRIVATE`, so the kernel
gives the child copy-on-write over the parent's live resident pages: the child
pays only for pages it dirties (the `0.12 MiB/child` target is that dirty-set
floor plus per-VM bookkeeping). To keep the parent runnable while children fork
off its live memory, the parent guest memory is registered with
`UFFD_FEATURE_PAGEFAULT_FLAG_WP` so the fork path can snapshot a consistent page
image out of band (write-protect, copy dirty pages asynchronously, unprotect)
and resume the parent's vCPUs immediately after the vmstate dump. The vmstate
(vCPU plus device state) is captured WITHOUT rewriting the whole guest RAM to a
mem file, because the memfd already IS the shared memory image the children map.

**Constraints from CLAUDE.md:** no em or en dashes anywhere; security findings
block features and the threat model moves in the same PR as the surface; no
unverified numbers (every MiB and every millisecond must come from `bench/`);
`internal/fork`, `internal/firecracker`, and the guest agent are
security-sensitive paths that need a named human reviewer; the
`firecracker-test` (kvm-test.yaml) suite is a required check and must run
against the patched binary, not stock.

---

## 1. Why: the competitive gap and the two stock-FC limits

### 1.1 The gap

Our current `ForkRunning` (`internal/fork/engine.go:1619`) does a FULL
checkpoint of the running source: it calls `CreateSnapshot`
(`internal/fork/engine.go:1659`), which writes the ENTIRE guest RAM to a mem
file, symlinks that mem file as a throwaway template
(`internal/fork/engine.go:1672`), and forks a child that restores from it. Two
costs follow. First, every live fork pays a full guest-RAM write to disk before
any child exists, so a 1.5 GiB source pays a 1.5 GiB write on the pause path,
scaling the source-pause window with memory size. Second, the child restores
from that mem file; even through the UFFD backend the child's pages are sourced
from a file copy, not directly from the parent's live pages, so the sharing is
snapshot-to-child, not parent-to-child.

deeplethe/forkd v0.4 collapses the source-pause window to `56 ms p50` on a
1.5 GiB source and makes it disk-independent (its published result notes the
pause is memory-only, so the gap WIDENS on slower storage). It does this with a
shared-memory backend plus write-protect async resume: the child maps the
parent's live memory `MAP_PRIVATE` and the parent resumes before the copy
finishes. That is the capability we lack. Sources: deeplethe/forkd README and
`bench/live-fork-pause-window/RESULTS-v0.4.md`; the technique is the one
CodeSandbox documented in "Cloning microVMs by sharing memory through
userfaultfd" (they `memfd_create` the parent memory, hand the fd to Firecracker,
and CoW children off it).

### 1.2 Stock-FC limit A: `/snapshot/create` always writes full guest RAM

`internal/firecracker/client.go:451` `CreateSnapshot` issues
`PUT /snapshot/create` with `SnapshotType: "Full"`
(`internal/firecracker/client.go:462`) and a REQUIRED `MemFilePath`
(`internal/firecracker/client.go:464`). Firecracker's snapshot API offers only
`Full` and `Diff`. `Full` dumps the complete guest RAM to the mem file. `Diff`
writes a SPARSE file of pages touched since the last snapshot (via KVM dirty-page
tracking or `mincore(2)`), so it still produces a mem file and still writes the
dirty set; it is not a vmstate-only capture. There is no stock mode that dumps
ONLY vCPU and device state and leaves the guest RAM where it lives. So even if we
back the guest with a memfd, stock FC would still insist on writing a mem file on
every checkpoint. Source: Firecracker `docs/snapshotting/snapshot-support.md`
(Full vs Diff semantics).

### 1.3 Stock-FC limit B: guest RAM is private anonymous mmap; UFFD is restore-side only

Firecracker backs a freshly booted guest with ANONYMOUS PRIVATE memory
(`MAP_ANONYMOUS | MAP_PRIVATE`); it only uses a `memfd` (`MAP_SHARED`) backing
when a `vhost-user-blk` device is configured, and upstream deliberately keeps
anonymous-private as the default because serving faults on shared memory is
slower (see `src/vmm/src/vstate/memory.rs`, which has both a `memfd_backed(...)`
constructor around line 566 and the default anonymous-private path). A child that
`MAP_PRIVATE`s the parent's memory can only CoW-share it if that memory is a
SHARED file mapping in the first place; over a private anonymous region there is
nothing shareable to map. That is why stock FC cannot hand a running parent's
live pages to a child.

Second, Firecracker's userfaultfd is RESTORE-SIDE ONLY. Its own docs
(`docs/snapshotting/handling-page-faults-on-snapshot-resume.md`) describe UFFD
strictly as a mechanism used "during snapshot restoration on the resume side":
Firecracker creates the guest userfaultfd for the RESTORED VM and passes it plus
the region layout to an external handler over a unix socket. Our handler
(`internal/fork/uffd_linux.go:139` `receive`) is exactly this restore-side
consumer: it accepts FC's connection, reads the region mappings JSON and the
uffd via `SCM_RIGHTS`, mmaps the snapshot mem file read-only `MAP_PRIVATE`
(`internal/fork/uffd_linux.go:122`), and services faults with `UFFDIO_COPY`
(`internal/fork/uffd_linux.go:214`, ioctl constant `0xc028aa03` at line 47). The
uffd is registered over the CHILD being restored, never over the running parent.
So the UFFD_WP async-parent-resume trick has nothing to attach to on stock FC:
there is no userfaultfd over the live parent and no write-protect registration on
it.

The FC pin that fixes both is in one place: `hack/install-firecracker.sh`
(`FC_VERSION=v1.15.0`, pinned per-arch sha256) and `Dockerfile.forkd`
(`ARG FC_VERSION=v1.15.0`, which runs that script). Repinning to a vendored
build is a one-file change plus a digest.

## 2. Firecracker architecture and the exact patch points

Firecracker's guest memory type is `GuestMemoryMmap` in
`src/vmm/src/vstate/memory.rs`, an alias over the rust-vmm `vm-memory` crate
(`vm_memory::GuestRegionCollection<GuestRegionMmapExt>`; each region is a
`vm_memory::MmapRegion` built via `MmapRegionBuilder`). vm-memory is the
rust-vmm crate that mmaps the VM's physical memory into the VMM process and
exposes it through the `GuestMemory` trait; Firecracker is one of its two origin
consumers. The three patch points, mirroring
`deeplethe/firecracker:forkd-v0.4-mem-backend-shared-v1.12` (Apache-2.0):

**(a) mem-backend-shared: memfd-back the RUNNING guest.**
`src/vmm/src/vstate/memory.rs` already contains `memfd_backed(size, ...)` which
does `create_memfd` then mmaps `MAP_SHARED`. Stock FC only reaches it for the
vhost-user-blk case. The patch makes the running guest use a `MAP_SHARED`
memfd backing unconditionally (or gated behind a new boot/machine-config flag,
e.g. `shared_mem: true`), and EXPORTS the memfd (over the API or a known fd path)
so the controller can hand it to children. Children then map that same memfd
`MAP_PRIVATE` for kernel-level CoW. This is the load-bearing change; everything
else hangs off it.

**(b) UFFD_WP registration on the running guest.** Add a memory-backend variant
so the running guest's shared region is registered with
`UFFDIO_REGISTER_MODE_WP` and the `UFFD_FEATURE_PAGEFAULT_FLAG_WP` feature (the
kernel write-protect mode documented in the userfaultfd manual and LWN's
"userfaultfd: write protection support"). The fork path then does
`UFFDIO_WRITEPROTECT` over the live region to freeze a consistent page image,
copies the dirty set out of band, and `UFFDIO_WRITEPROTECT` (unprotect) so the
parent's vCPUs resume immediately after the vmstate dump. Note the kernel
caveat: WP on shmem/hugetlbfs matured later than on anonymous memory (early
UFFD_WP was anonymous-only); the vendored build must run on a kernel new enough
for WP over the memfd-backed (shmem) region, and m1/m2 must assert the running
kernel supports it. The external-handler shape is the same one FC ships as
`src/firecracker/examples/uffd/on_demand_handler.rs`, extended write-side.

**(c) vmstate-only snapshot mode.** Add a snapshot mode to
`src/vmm/src/persist.rs` (the module that orchestrates create and restore) that
serializes ONLY the vCPU and device state and SKIPS the guest-RAM dump, because
the memfd already is the shared memory image the children map. This is the API
counterpart to limit A: a `SnapshotType` beyond `Full`/`Diff` (e.g. `VmStateOnly`)
or a `mem_file_path`-optional create.

**Upstream context to build on, not reinvent:** stock FC already carries the
`memfd_backed` plumbing (patch a wants to widen it, not author it); the UFFD
backend, region-layout handshake, and SCM_RIGHTS fd passing already exist for the
restore side (patch b adds the running-side registration); upstream keeps
anonymous-private as default explicitly for fault-latency reasons, which is a
KNOWN tradeoff we accept for the fork path and must benchmark (m1). vm-memory's
`MmapRegion`/`MmapRegionBuilder` support fd-backed regions, so no new mapping
primitive is needed. Sources: Firecracker `src/vmm/src/vstate/memory.rs`,
`src/vmm/src/persist.rs`, `docs/snapshotting/snapshot-support.md`,
`docs/snapshotting/handling-page-faults-on-snapshot-resume.md`; rust-vmm
`vm-memory` (DESIGN.md, GuestMemoryMmap/MmapRegion); LWN "userfaultfd: write
protection support" and the kernel userfaultfd admin-guide; CodeSandbox
"Cloning microVMs by sharing memory through userfaultfd."

## 3. Licensing

Firecracker is Apache-2.0. A vendored fork is a derivative work under Apache-2.0
section 4: we must keep the upstream `LICENSE` and `NOTICE`, retain all
copyright, patent, trademark, and attribution notices, and add a NOTICE entry
stating our modifications and their date. We do NOT relicense; the fork stays
Apache-2.0. Firecracker is a Firecracker trademark of Amazon, so the fork must
not imply endorsement and should be named as a mitos-maintained patch series over
a pinned upstream tag, not "Firecracker."

deeplethe/forkd's Firecracker fork
(`deeplethe/firecracker:forkd-v0.4-mem-backend-shared-v1.12`) is Apache-2.0, so
we MAY read and study it for the patch shape and even reuse code, provided we
carry its attribution and NOTICE per the same section 4. We treat it as a
reference for the three patch points, not as a dependency to import wholesale.
Our own `Dockerfile.forkd` already declares `image.licenses="Apache-2.0"`, which
stays correct.

## 4. Mitos integration

The engine side changes are small because the seams already exist:

- **`ForkRunning` (`internal/fork/engine.go:1619`)** replaces the full checkpoint
  with a vmstate-only capture. Today it calls `CreateSnapshot` at
  `internal/fork/engine.go:1659` (full mem write) and symlinks the mem file at
  `internal/fork/engine.go:1672`. The live path instead: (1) issues the new
  vmstate-only create (no mem file), (2) hands the child the PARENT's memfd fd
  rather than a mem-file path, (3) keeps the existing fresh-per-fork network
  identity and rootfs-clone logic unchanged (`liveForkRootfsBase`,
  `e.fork(...)` at `internal/fork/engine.go:1706`). The pause window shrinks to
  the vmstate dump plus WP freeze, not a full RAM write.

- **`internal/firecracker/client.go`**: `CreateSnapshot`
  (`internal/firecracker/client.go:451`) gains a vmstate-only sibling (or a mode
  arg) that omits `MemFilePath` and sets the new snapshot type. `LoadSnapshotUFFD`
  (`internal/firecracker/client.go:541`) is REUSED as-is for the child: it
  already loads paused through `MemBackend{BackendType: "Uffd", ...}`
  (`internal/firecracker/client.go:546`). The only change is what the handler is
  backed by.

- **UFFD handler (`internal/fork/uffd_linux.go`)**: the child handler's `memPath`
  points at the PARENT's memfd instead of a snapshot mem file. `newUFFDHandler`
  already mmaps the backing file `MAP_PRIVATE` read-only
  (`internal/fork/uffd_linux.go:122`) and copies pages via `UFFDIO_COPY`
  (`copyPage`, `internal/fork/uffd_linux.go:205`); backing it with the parent
  memfd is a path substitution, not new fault-handling code. The
  running-parent-side `UFFD_WP` registration and the `UFFDIO_WRITEPROTECT` ioctl
  are NEW: they live beside `uffdioCopy` (constant at
  `internal/fork/uffd_linux.go:47`, ioctl call at `:214`) as a sibling
  `uffdioWriteprotect` constant plus a WP register/unprotect helper, on a
  forkd-owned handler over the parent (distinct from the per-child restore
  handler).

**Build and pin (recommendation):** maintain a `mitos-run/firecracker` fork repo,
a thin patch series (three commits: a, b, c) REBASED onto each upstream tag we
adopt, released as a tagged binary. `hack/install-firecracker.sh` then points at
our release URL and a new pinned sha256 (keep the digest-verify and the
firecracker/jailer version-match check exactly as they are), and
`Dockerfile.forkd` (`ARG FC_VERSION`) consumes it unchanged. This beats a patch
series applied inside `Dockerfile.forkd` (which would rebuild FC from source on
every image build and drift from the pinned-digest supply-chain model) and beats
a raw vendored binary with no source provenance. Maintenance-burden minimizer:
keep the patch as three small, independently reviewable commits over an upstream
TAG (not `main`), so a Firecracker bump is a rebase of three commits, and gate
adoption of a new upstream tag on the m5 CI passing.

**CI:** the `firecracker-test`/kvm-test.yaml suite is a required check and MUST
run against the patched binary. m5 repins the CI's FC install to the vendored
release and adds a live-fork phase (two live forks off one running parent,
asserting CoW sharing and async resume). The existing fork-correctness KVM phase
(distinct RNG/UUID/TLS, clock step) must stay green on the patched build.

## 5. Security and fork-correctness surface

A shared guest-memory backend is isolation-sensitive: the whole product promise
is per-child KVM isolation, and a `MAP_SHARED` memfd that multiple VMs map is a
new cross-VM object. What must be re-proven, tracked as a threat-model delta in
the m4 PR (docs/threat-model.md):

- **Write isolation.** Children map the parent memfd `MAP_PRIVATE`, so a child
  write must never be visible to the parent or to a sibling. This is the core
  claim to prove on KVM (write in child A, read the same guest page in parent and
  in child B, assert unchanged). The existing threat model already documents the
  snapshot mem/vmstate as a shared READ-ONLY object; the live memfd is a new
  shared object whose isolation rests on `MAP_PRIVATE` CoW plus the WP freeze
  being consistent, and that has to be stated and tested, not assumed.
- **No stale-page leak.** The WP-frozen page image handed to a child must be a
  consistent point-in-time image; a torn read (parent dirtied a page mid-copy)
  would leak parent state forward. m2 must prove the freeze-copy-unprotect
  sequence is atomic with respect to guest writes.
- **Fork-correctness invariants still hold (docs/fork-correctness.md).** The RNG
  reseed, clock step, and secret-inheritance gates are NotifyForked-driven and
  run AFTER restore, so they are orthogonal to how memory is backed; but a live
  fork that shares the parent's live CRNG/clock/secret state is EXACTLY the
  hazard those rows close, so the m4 integration must run the identical
  fail-closed NotifyForked handshake on the live path (it already does for the
  current full-checkpoint live fork; the memfd change must not bypass it). Row 1
  (shared RNG), row 2 (stale clock), and row 3 (secret duplication into live
  forks) must be re-asserted on the patched build in CI, since the memory image
  the child wakes on is now the parent's LIVE image rather than a file copy.
- **Kernel WP support is a precondition, not a runtime option.** If the kernel
  lacks WP over shmem, the live path must fail closed to the current
  full-checkpoint path, never silently produce a torn image.

## 6. Milestones (smallest-first; each a future PR or issue)

- **m1: stand up the fork + smallest mem-backend-shared patch + throwaway KVM
  proof.** Create the `mitos-run/firecracker` fork over the v1.15.0 tag; apply
  ONLY patch (a) (widen `memfd_backed` so the running guest is memfd/`MAP_SHARED`
  and export the fd). A throwaway KVM harness boots a parent, has a second process
  `MAP_PRIVATE` the parent's memfd, and MEASURES real per-child MiB (RSS delta /
  `smaps` private-dirty) to compare against the `0.12 MiB/child` target. No mitos
  integration yet. Deliverable: a number in `bench/`, plus go/no-go on the memfd
  backend.
- **m2: UFFD_WP async parent resume.** Apply patch (b): register the running
  guest with `UFFD_FEATURE_PAGEFAULT_FLAG_WP`, add the freeze-copy-unprotect
  sequence, and prove the parent resumes before the copy completes AND that the
  frozen image is consistent (no torn read). Measure the source-pause window
  against the `56 ms` target.
- **m3: vmstate-only snapshot.** Apply patch (c) in `persist.rs`; add the
  vmstate-only `SnapshotType` and the mem-file-optional `/snapshot/create`. Prove
  a child restores correctly from vmstate-only plus the parent memfd.
- **m4: mitos `ForkRunning` integration behind a flag.** Wire the engine and
  firecracker client per section 4, gated behind a forkd flag (e.g.
  `--live-fork-shared-mem`, default off). Threat-model delta and
  fork-correctness re-assertion land in THIS PR (CLAUDE.md principle 2).
- **m5: CI on the patched FC.** Repin `hack/install-firecracker.sh` and the
  kvm-test.yaml install to the vendored release; add the live-fork KVM phase;
  keep the fork-correctness and jailer phases green.

**Riskiest milestone: m2 (UFFD_WP async parent resume).** It is the one with no
existing analog in our codebase (our UFFD is entirely restore-side), it depends
on kernel write-protect over SHMEM/memfd (which matured after anonymous-only WP
and varies by kernel), and correctness is a torn-read isolation hazard rather
than a mere performance regression: a subtly non-atomic freeze leaks parent state
into a child and silently breaks isolation. m1 is cheaper and de-risks the memfd
backing first, which is why it goes first; m2 is where a spike could still say
"kernel/FC cannot do this safely on our node kernel" and force a rethink.

**Out of scope:** GPU or device-memory fork (no CoW path exists); cross-node live
fork/migration; changing the cold-fork path (this doc only touches
`ForkRunning`); any README or marketing number before `bench/` reproduces it.
