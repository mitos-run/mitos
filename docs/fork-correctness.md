# Fork-engine correctness

The CoW snapshot-fork is the product. It must be correct before it is fast.
This document enumerates every known correctness hazard of restoring multiple
VMs from one snapshot, the policy we have chosen for each, the test that
verifies it, and its current status.

**Status legend:** `open` = not implemented, no test. `partial` = implemented,
test missing or incomplete. `done` = implemented + test runs in the
`fork-correctness` CI job.

| # | Hazard | Policy | Test | Status |
|---|--------|--------|------|--------|
| 1 | Shared RNG state after restore | Reseed CRNG on every fork via host entropy over vsock (NotifyForked); FAIL CLOSED on EVERY engine when the guest does not reseed; PLUS a baked-in virtio-rng device for a continuous host entropy source in every fork | go: `TestForkNotifiesAgentWithFreshEntropy`, `TestForkGenerationIncrementsAcrossForks`, `TestForkFailsWhenNotifyForkedErrors`, `TestForkFailsWhenGuestDoesNotReseed`, `TestForkRunningFailsWhenGuestDoesNotReseed` (daemon), `TestRealModeForkFailsClosedWhenGuestDoesNotReseed` (sandbox-server), `TestEntropyRequestJSON` + `TestDefaultVMConfigEnablesEntropy` (firecracker, virtio-rng config builder); KVM: two forks of one snapshot assert distinct `/dev/urandom`, distinct kernel UUID (`/proc/sys/kernel/random/uuid`), and distinct TLS client random (32 bytes from getrandom) | **done** (guest reseed + reseed handshake done; virtio-rng device NOW WIRED into the template build and baked into every snapshot, `firecracker.VMConfig.EntropyDevice` default-on, `Client.SetEntropy` -> `PUT /entropy`, darwin-unit-tested for the config/JSON shape, live behavior KVM-gated. Fail-closed is enforced on ALL engines: the husk path (`internal/husk` `productionNotifier`), the raw-forkd path (`internal/daemon/sandbox_api.go` `NotifyForked` now RETURNS the guest response and `internal/daemon/server.go` `notifyForked` refuses to mark the sandbox Ready when it is nil or reports `ReseededRNG:false`), and sandbox-server real-mode (`cmd/sandbox-server/main.go` `reseedFork`, the same gate). A guest that signals `ReseededRNG:false` with `OK:true` is reaped, not served; a transport `OK:false` and a `ReseededRNG:false` both fail the fork. The `ReseededRNG:false` failure mode is covered by the go tests listed above. The KVM fork-correctness phase (`kvm-test.yaml`, the `firecracker-test` job) boots two forks of one snapshot and asserts, with hard `exit 1` gates, distinct `/dev/urandom`, distinct kernel UUID, distinct TLS client random, each fork's wall clock within 2s of the runner, and the per-fork generation file; this phase runs and passes on every KVM CI run, so the RNG/UUID/TLS distinctness and clock resync are OBSERVED on KVM, not just unit-asserted. Both former `done` gaps are now closed: (a) the baked virtio-rng device is OBSERVED live on KVM, the fork-correctness phase asserts each restored guest's `/sys/class/misc/hw_random/rng_current` is `virtio_rng.N` (the guest kernel binds `CONFIG_HW_RANDOM_VIRTIO`), so the device is present and selected, not merely set in the VM JSON; and (b) the reseed is proven end-to-end THROUGH the production daemon notify code, not only test-agent-direct, the husk activate-correctness KVM phase drives `NotifyForked` via the husk stub `productionNotifier` (the same notify path the tenant husk activation uses) and asserts distinct RNG plus a stepped clock across two real activations, failing closed if a guest does not reseed.) |
| 2 | Stale wall clock after restore | kvm-clock resync + agent clock step from host wall clock in NotifyForked | go: `TestForkNotifiesAgentWithFreshEntropy` (carries `HostWallClockNanos`); KVM: each fork `WALLCLOCK_NS` within 2s of the runner clock | **done; CLOCK_MONOTONIC residual is an accepted OS limitation, not a gap** (guest wall-clock step implemented and tested IN the fork-correctness CI job: the go test carries `HostWallClockNanos`, and the KVM phase asserts each fork's `WALLCLOCK_NS` within 2s of the runner on every `firecracker-test` run, which is exactly what the legend defines as `done`. CLOCK_MONOTONIC cannot be stepped: Linux rejects `clock_settime(CLOCK_MONOTONIC)` with EINVAL, and a PAUSED-across-restore VM resumes the monotonic clock continuously so clean-restore monotonic timers do not mis-fire; the narrow mixed wall/monotonic-derived-deadline residual is handled by the existing SIGUSR2 userspace reset signal. This is an assessed, documented OS-level limitation accepted as out of scope for the reseed contract, NOT unfinished implementation or a missing test. See section 2 for the full assessment.) |
| 3 | Secrets duplicated into live forks | Per-fork credential reissue; inheritance requires opt-in | `TestLiveForkOfSecretHolderIsRejectedByDefault`, `TestLiveForkOptInProceedsPastTheGate`, `TestForkBearerTokenIsFreshlyReissuedNotInheritedFromParent`, `TestForkDeliversConfigureToAgent`, KVM `test-agent` configure check | **done** (the chosen default-safe policy is fully implemented and tested: a live fork of a secret-holding sandbox is REJECTED by default with a typed `Rejected` condition, and an opt-in is recorded as an audit condition, so secrets are never duplicated into a fork without an explicit, audited decision. Per-fork PLATFORM credential reissue is also enforced and tested: each fork's sandbox-API bearer token is freshly minted (`mintAPIToken`, 32 bytes crypto/rand) and DISTINCT from its parent's, so a fork cannot authenticate to the sandbox API as its parent. Documented residual, NOT part of the safe default and not a gap in the mitigation: revoke-and-reissue of TENANT secret VALUES over vsock (so a fork could safely inherit FRESH secrets instead of being rejected) requires the secret to be a dynamically reissuable/revocable credential from a broker; static Kubernetes Secret values have no upstream to revoke, so this is an additive better-UX future tracked as issue #7, and capability-token per-fork attenuation lands with the #25 runtime wiring. The hazard itself, secret duplication into forks, is fully closed today by the tested default-deny gate.) |
| 4 | Duplicate MAC/IP/TCP state in forks | Fresh NIC identity per fork; parent TCP dead in fork | `TestPrepareForkNetworkDistinctPerFork`, `TestBuildSetMACCarriesAddress` (guestnet netlink MAC set); KVM: `cmd/net-fork-smoke` boots two forks of one snapshot, asserts distinct guest MAC + distinct guest IP | **done** (per-fork networking is wired and each fork wakes with a DISTINCT guest network identity: a unique /30 (distinct guest IP and host tap) and a freshly derived locally-administered guest MAC, `netconf.deriveMAC(sandboxID)`, delivered over the NotifyForked network config and applied to eth0 by the guest agent, `internal/guestnet.Configure` sets the MAC (link down, set hw addr, link up) then flushes the old address and assigns the per-fork IP. Distinctness is unit-tested and OBSERVED on KVM: `cmd/net-fork-smoke` forks two sandboxes from one snapshot and asserts they have DIFFERENT guest MACs, each its derived MAC and NOT the shared placeholder baked in the snapshot, and DIFFERENT guest IPs. Parent TCP is dead in the fork because the agent FLUSHES the source's eth0 address and assigns the fork's own IP on restore, so a socket inherited in the snapshot, bound to the now-absent source address, can no longer send; the address replacement is KVM-observed (distinct IPs). Documented residual, not a gap in the identity: the upstream-socket inheritance hazard for networked live forks is now tracked as a SEPARATE concern in row 8 (the conntrack flush and proxy upstream-socket reset are implemented by #336, and the open-socket-death assertion on KVM is covered by the networked-live-fork acceptance phase); egress allow/deny is separately proven by the guest-egress KVM phases.) |
| 5 | Misleading memory accounting | Report lifetime unique bytes, not just T=0 dirty pages | `TestUpdateMetricsPopulatesMemoryGauge`, `TestSampleMetricsTicksAndStopsOnContextCancel`, `TestUpdateMetricsLabelsMemoryPerSandbox`; KVM: `cmd/mem-smoke` forks, samples `Metering().TotalUnique` at T=0, touches 64 MiB in the guest, re-samples, asserts >= 32 MiB growth | **done** (lifetime re-sampling wired: `Engine.Metering` re-stats `smaps_rollup` each pass and `Server.SampleMetrics` refreshes the gauges periodically; the KVM growth assertion is OBSERVED on every `firecracker-test` run, the `kvm-test.yaml` lifetime-memory phase runs `cmd/mem-smoke --min-growth-bytes 33554432` and fails the job if metered unique bytes do not grow >= 32 MiB after a 64 MiB guest workload; and per-sandbox labels are now exported, `mitos_sandbox_memory_unique_bytes{sandbox,template}` is a GaugeVec set per live sandbox each pass and Reset so a terminated sandbox drops its series. Residual (not a row-5 gap): published density numbers after a representative workload are a `bench/` task.) |
| 6 | Incompatible snapshot restored (crash/corruption) | Refuse on load: the manifest records the producing environment (format version, Firecracker version, CPU model, kernel, config hash); require exact VMM match, exact CPU-model match, format version in the supported set (kernel informational); `--allow-incompatible-snapshots` dev escape hatch | go: `internal/snapcompat` `TestCheck*`, `internal/fork/compat_test.go`; KVM: record a real manifest, assert compatible passes and a VMM / format-version mismatch is refused | **done** (load-gate enforcement after the digest verify, before any Firecracker launch; CPU templates + live cross-version restore open) |
| 7 | forkd crash orphans its own VMs (in-memory map lost on restart) | Persist a per-VM journal (`<dataDir>/sandboxes/<id>.json`, atomic write) on create, remove on clean terminate; `NewEngine` reconciles it before serving: re-adopt a still-running pre-crash VM into the live map (PID-recycle-guarded: `/proc/<pid>/exe` must resolve to the recorded firecracker binary, else comm is `firecracker`) so the GC can reconcile it, or reap a dead/recycled VM's leaked artifacts (jailer chroot, rootfs CoW clone, fork network, jailer uid, volume backings) and drop the record. The adopted VM's exact /30 network block is pinned from its recorded guest IP (`netconf.Allocator.MarkInUse`), not re-Acquired, so a later fresh fork cannot be handed the same /30 and Release frees the right block. Fail-open; the startup reap never kills (only adoption enables a later GC-driven kill). That GC-driven kill (`reapAdopted`) RE-RUNS the same PID-recycle guard against the recorded firecracker binary immediately before signalling, so a pid recycled to an unrelated process between adoption and Terminate is never SIGKILLed (its artifacts are still reaped). Journal holds ids/pids/paths/uids/IPs only, never secrets | go: `internal/fork/journal_test.go`, `internal/fork/reconcile_engine_test.go` (live-pid adopt, dead-pid reap, recycled-pid reject, fail-open, adopted-terminate reap, reap-adopted skips kill on recycled pid + kills when still ours, adopted network block pinned) and `internal/netconf/identity_test.go` (`TestMarkInUse*`) with injected pid/verifier seams; `internal/firecracker` `TestJailerState`, `TestUIDAllocatorMarkInUse` | **done** (journal + reconcile + reap + TOCTOU re-verification + block pinning done and unit-verified on darwin, AND now proven on real KVM: the `kvm-test.yaml` crash-reconcile phase (`cmd/crash-reap-smoke`, the `firecracker-test` job) forks a real Firecracker, ABANDONS the engine without terminating to model a forkd crash, then constructs a SECOND engine on the same data dir whose startup reconcile must leave no orphan. Two modes both assert no leaked process, journal record, or sandbox dir: `adopt-reap` re-adopts the still-live VM then Terminates it (the GC-driven kill of a re-adopted orphan), and `reap-dead` kills the VM first so the reconcile reaps the dead orphan's artifacts. The typed `OrphanReaped` claim condition for a GC'd re-adopted orphan is implemented (`internal/controller/gc.go` `stampOrphanReaped`, catalogued in docs/conditions.md). Residual (not a row-7 gap): a process-level `kill -9` of the forkd binary itself, rather than the in-process abandon the smoke uses, is exercised by the issue #12 cluster chaos suite.) |
| 8 | Fork inherits a live upstream socket (shared 4-tuple/seq/TLS state across parent and child for outbound connections) | Host-owned upstream sockets via the per-sandbox egress proxy (`internal/egressproxy`): the proxy owns every upstream TCP connection on the host side, so the guest's outbound socket is always to the proxy, never to the far end directly. On a live fork the child gets a fresh /30 + NIC rebind: the eth0 re-address is the PRIMARY reset (removing the old guest IP kills any inherited guest-side socket bound to it). A best-effort host conntrack flush on the CHILD's fresh guest IP (`conntrack -D -s <child guest IP>`) is a belt-and-suspenders only: it rarely matches anything because the child's fresh IP has no prior host flows, and flushing the SOURCE/parent IP is deliberately AVOIDED (it would disrupt the still-running parent). NotifyForked carries ResetUpstreams=true so the guest drops stale ARP/neighbor state and writes the proxy env; the child then re-dials through the proxy as a fresh connection with no inherited 4-tuple or TLS session. Engine.ForkRunning fails closed for networked sandboxes when networking is on but the proxy is not active. | KVM: networked-live-fork acceptance phase (Task 9 of this PR), asserting the child opens a FRESH upstream connection at the stub server and does NOT reuse the parent's inherited socket | **partial** (host-owned upstream sockets + per-fork eth0 re-address + conntrack flush + ResetUpstreams implemented and unit-tested (#336); KVM networked-live-fork acceptance phase lands in this PR; observed on KVM CI when that phase runs) |

The husk-pods activate path (issue #18, `internal/husk`, `cmd/husk-stub`) runs
the SAME RNG-reseed + clock-step `NotifyForked` handshake as the engine fork
path (rows 1 and 2), plus env/secret delivery via `Configure` (row 3), on every
`Activate`. It fails closed: a VM whose guest does not report `ReseededRNG` is
left unserved. The SAME fail-closed reseed check is now enforced on EVERY engine:
the husk path (`internal/husk` `productionNotifier`), the raw-forkd path
(`internal/daemon/server.go` `notifyForked` inspects the `NotifyForked` response
and reaps a fork that reports `ReseededRNG:false`), and sandbox-server real-mode
(`cmd/sandbox-server/main.go` `reseedFork`). The "always strict" claim holds on
all three; see row 1. The KVM husk activate-correctness phase proves it by activating
two VMs from one bench snapshot and asserting distinct `/dev/urandom`, each wall
clock within 2s of the runner, and a delivered env var plus secret readable in
each guest with the secret value absent from the host-side logs. See
[docs/husk-pods.md](husk-pods.md).

### Husk fork children

A husk live fork (`internal/controller/sandboxfork_controller.go` `reconcileHuskFork`)
produces children by SNAPSHOTTING the source pod's running VM (the husk stub
`ForkSnapshot` op: pause, Full snapshot to `<dataDir>/forks/<fork-id>/{mem,vmstate}`,
freeze the source rootfs to `<dataDir>/forks/<fork-id>/rootfs.ext4`, then ALWAYS
resume the source) and ACTIVATING each child husk pod from that fork
snapshot. Each child activates through the SAME `Activate` path a warm pod uses,
so it runs the identical RNG-reseed + clock-step `NotifyForked` fork-correctness
handshake (`internal/husk` `productionNotifier` -> Rust agent `NotifyForkedHandler`):
every child reseeds its kernel CRNG with fresh host entropy
and steps its wall clock, and a child whose guest does not report `ReseededRNG` is
left unserved (fail closed). So a husk fork child is NOT a CRNG/clock clone of the
source or its siblings; it inherits exactly the per-fork reseed an engine fork
gets. Each child is its own husk pod + VM + per-activation rootfs CoW clone, so
guest writes never cross between children or back to the source. The Python-level
PRNG caveat in section 1 applies to husk fork children identically.

Memory/disk consistency (the disk half of the fork). The fork snapshot's
`vmstate` is baked against the SOURCE sandbox's rootfs (`path_on_host`), so a
restored child's guest memory (page cache, ext4 superblock, in-flight metadata)
reflects the SOURCE disk, not the pristine template disk. The child's
per-activation rootfs CoW clone is therefore made from a FROZEN copy of the
source rootfs the source stub captures INSIDE the fork snapshot's paused window,
written next to `mem`/`vmstate` at `<dataDir>/forks/<fork-id>/rootfs.ext4` and
mounted read-only at the child's snapshot mount (threaded as
`HuskPodOptions.ForkSourceRootfsPath` = `huskForkRootfsInPodPath()` and passed to
the stub as `--template-rootfs`), NOT the source's LIVE rootfs and NOT the
template rootfs. The freeze is a reflink CoW copy taken while the VM is paused, so
it is a point-in-time PAIR with the memory checkpoint. This is what makes the
always-resume safe: the source resumes immediately after the checkpoint (so a
post-fork exec against the SOURCE, the fork-the-winner-and-continue loop, does not
time out), and because children clone from the FROZEN copy rather than the live
rootfs, the resumed source can keep writing its own disk without ever drifting a
child's clone out of sync with the child's restored memory. Leaving the source
paused so its live rootfs stayed frozen for the whole child fan-out was the
implicit old mechanism and the v1.24.1 production bug: nothing resumed the source
(`pauseSource` on the hosted path), so a post-fork exec against it hung for 30s.
Cloning from the template instead would rebind the child to a disk that does not
match its restored memory: any data the source wrote since boot would be lost in
children and the cached-vs-on-disk mismatch could corrupt the child fs. This
mirrors the raw-forkd `ForkRunning` path (`internal/fork/engine.go`), which clones
the child's rootfs from the SOURCE's OWN writable rootfs clone
(`source.rootfsClone`, `<dataDir>/sandboxes/<source-id>/rootfs.ext4`) captured
while the source is paused, then resumes the source; the husk path now captures
the same paused-window point-in-time disk explicitly as a frozen file. Issue #596:
before that fix the raw-forkd path threaded `source.rootfsPath` (the read-only
TEMPLATE the source was cloned from), so raw-forkd children silently lost every
on-disk write the source made. The clone is still per-child (keyed by the child
pod name), so children remain independent; only the clone SOURCE is the frozen
source rootfs. Verified by `internal/husk`
`TestForkSnapshotFreezesRootfsInsidePausedWindow` and
`TestForkSnapshotResumesSourceOnHostedPath`, by `internal/controller`
`TestHuskForkRootfsInPodPathIsFrozenSnapshotCopy`,
`TestBuildHuskPodForkChildClonesFromSourceRootfs`, and
`TestHuskForkChildPodHasFullHuskShape`, by `internal/fork`
`TestLiveForkRootfsBaseIsSourceClone`, and end to end on KVM by
`cmd/live-state-fork-smoke`.

Single coherent fork point. The source VM is snapshotted EXACTLY ONCE per
`Sandbox` with `source.fromSandbox` (guarded by `Status.ForkSnapshotTaken`, persisted so it survives a
controller restart mid-fork). Children take several reconcile passes to reach
Ready; re-snapshotting on each pass would re-pause the source and overwrite the
fork `mem`/`vmstate`, so a child activated in a later pass would restore a NEWER
source memory state than an earlier child and the N children would not share one
fork point. Verified by `internal/controller`
`TestHuskForkSnapshotTakenExactlyOnce`.

### Live copy-on-write fork: memory-source correctness (milestone m4b)

The live-cow fork path (`--live-cow-fork`, DEFAULT OFF, SEPARATE from
`--multi-vm-fork`) changes only WHERE a co-located fork child's guest MEMORY comes
from: instead of restoring the child from the disk fork snapshot `mem` file, the
child shares the PARENT's live resident memory through the Mitos-patched
Firecracker (a `MAP_SHARED` memfd the child `MAP_PRIVATE`s for kernel
copy-on-write). It does NOT change any fork-correctness re-seed: a live-cow child
still runs the full fail-closed RNG/clock/secret/network re-seed of the sections
below (a fork of a running source is still `ForkRunning`), and it still pairs with
the frozen source rootfs so the disk-half invariant of "Husk fork children" above
holds unchanged. Live-cow is a memory-SOURCE optimization, not a new fork policy.

**Spill gate: a vmstate-only fork snapshot is only restorable by a CO-LOCATED child.**
When an armed live-cow source with child import on takes its one-time fork snapshot it
writes no `mem` file, on the promise that each child boots its guest RAM from the source
pod's shared memfd. Only a child co-located INSIDE that pod can keep that promise. A fork
whose replica count exceeds the pod's co-location budget spills the remaining children
into their own husk pods, and such a child has no path to the source's memfd: it can
restore only from disk. With no `mem` file it has nothing to restore, never activates,
and the fork wedges at "<budget>/<replicas> husk forks ready" with no error. The
controller therefore decides `ForkSnapshotRequest.RequireMemFile` from the SAME
co-location budget the fan-out uses, BEFORE the snapshot is taken, and the source falls
back to the Full `CreateSnapshot(mem, vmstate)` whenever any child will spill. A fully
co-located fork keeps the fast vmstate-only path.

Since m7 the restored source's memfd is populated LAZILY: it is created empty and the
write-protect handler serves userfaultfd MISSING faults out of the snapshot mem file.
This does not touch any re-seed either, but it adds one obligation to the fork point.
A co-located child maps the parent memfd `MAP_PRIVATE`, so a page the parent never
faulted in is a HOLE the child would read as zeros instead of the snapshot's bytes.
`Freeze` therefore fills every unpopulated chunk from the mem file BEFORE it
write-protects the region (populate-on-freeze), which keeps warm-claim activate O(1)
and charges the fill only to the forks that need it. Pages installed after the freeze
land write-protected, so copy-before-unprotect still sees every parent write.

The hazard this path introduces, and closes, is the RESUMED-PARENT LEAK. Sharing
the parent's memfd with `MAP_PRIVATE` alone is NOT sufficient: if the parent
RESUMES and writes a page a child has not yet copied, that post-fork write would
leak forward into the child, so the child would no longer see a point-in-time-T
image. The m2 mechanism closes it with userfaultfd write-protect
(`internal/fork/wpfork*.go`, the parent-side WP handler):

- At the fork point the handler FREEZES the guest: `UFFDIO_WRITEPROTECT` over the
  whole guest region, so every page is write-protected in the parent's mapping.
- The parent resumes. When it writes a still-protected page the writing vCPU thread
  takes a write-protect fault and BLOCKS in the kernel; the new value has NOT
  landed.
- The handler reads the fault, copies the page's pre-write (fork-time) bytes into a
  PRIVATE frozen memfd and marks the page frozen, and only THEN unprotects the page
  and wakes the writer. The write then lands in the live image, but the fork-time
  bytes are safe in the frozen image.
- A co-located child source-selects per page (exactly as Firecracker's restore-side
  UFFD handler does): a clobbered page is read from the frozen memfd (fork-time
  bytes), an untouched page live from the shared memfd (still fork-time bytes, cheap
  CoW). Either way the child sees a consistent point-in-time-T image.

Child side, PER FAULT (lazy UFFD import). The co-located child now boots its guest
RAM LAZILY through Firecracker's native userfaultfd backend (the husk-side handler
`internal/fork/childuffd*.go`), faulting only its working set instead of eagerly
copying the whole guest RAM, so the vmstate-only source win is not eaten by an
equal-and-opposite child copy. The per-page source selection above then runs PER
FAULT with a fault-time re-check that preserves the no-leak invariant: on a page
fault the handler reads the LIVE frozen bit; a frozen page is served from FROZEN
(fork-time bytes, stable forever once the WP handler wrote them); a not-yet-frozen
page is served from the live source memfd (still fork-time, since an unfrozen page is
provably unwritten because the WP handler freezes BEFORE it lets a write land), and
then the bit is RE-CHECKED after the live read so a page frozen concurrently is
overridden from FROZEN. So a resumed source's post-fork overwrite is always served as
its fork-time value, never the mutated value. Unit-tested over a real userfaultfd in
`internal/fork` `TestChildUFFDLazyImportComposesAndNoLeak` and end to end on KVM in
`internal/husk` `TestForkSnapshotLiveCowChildImportColocatedBootsFromSourceMemfdKVM`.

The ordering FREEZE -> COPY -> UNPROTECT is the whole correctness argument: because
the copy completes before the unprotect, there is no window in which the parent's
new value is visible while the fork-time value is lost (the torn-read hazard is
closed). Precondition: the kernel must support write-protect over the memfd
(`CONFIG_HAVE_ARCH_USERFAULTFD_WP`, `UFFD_FEATURE_PAGEFAULT_FLAG_WP`); the handler
and the patched Firecracker both fail closed to the paused-parent (m1) contract,
and the husk falls back to the disk snapshot restore, when it is absent or off
Linux, so turning the flag on never breaks a fork.

Verified on real KVM by `internal/fork` `TestLiveCowForkInheritanceAndNoLeak` (the
`firecracker-test` job, `go test ./internal/fork/...`): it drives the WP handler
end to end over a real userfaultfd write-protect and a real memfd, writes a
fork-time marker, resumes a "parent" that overwrites the marker page, and asserts
BOTH halves of the invariant, INHERITANCE (the child reads the parent's fork-time
memory, both the marker page and an untouched page) and NO LEAK (the resumed
parent's overwrite does NOT reach the child, which still reads the original
fork-time bytes). On a runner whose kernel lacks write-protect it self-skips with
the precise reason (the m2 precondition), never a false pass. The gate and env
plumbing are unit-tested off KVM (`internal/fork` `TestLiveCowParentEnv`,
`internal/husk` `TestLiveCowForkGateDefaultOff`/`TestLiveCowForkGateOn`,
`internal/firecracker` `TestLaunchEnv`, `internal/controller`
`TestBuildHuskPodThreadsLiveCowFork`).

Repeated forks of ONE source: per-fork frozen state. The armed WP handler
(`SetLiveCowParent`) is REUSED for every `ForkSnapshot` of the same parent VM,
because the userfaultfd is created once by Firecracker over the parent's live mapping
and delivered once at `Receive`; a fork does NOT get a fresh handler. Each `Freeze`
(each fork) therefore allocates its OWN frozen epoch: a fresh FROZEN memfd + fresh
1-bit-per-page selector (`newEpoch`, `internal/fork/wpfork_linux.go`). A co-located
child of that fork imports THAT epoch's memfds, so fork B can never hand its child a
page frozen for fork A, and fork A's still-live children keep reading fork A's
point-in-time pages after fork B happens. The single `Serve` loop fans each
write-protect fault into EVERY live epoch that has not yet captured the page (bit
clear), copying the page's pre-write bytes: because all epochs share one mapping and
one protection state, that pre-write value is the fork-time value for exactly those
epochs whose freeze preceded the write and that have not yet seen a write to the page.
An epoch that already froze the page is skipped, so a source write that lands AFTER a
later fork never overwrites an earlier fork's frozen bytes. Epochs are retained until
the handler `Close` (an earlier fork's child may still reference its memfds through
`/proc`), and the memfds are sparse, so the cost is bounded by the pages actually
rewritten, not by guest RAM per fork. Without this (a single frozen image reused
across forks) a second fork would inherit the pages the first fork froze and hand its
child stale bytes for any page rewritten between the two forks. Verified on real KVM
by `internal/fork` `TestLiveCowRepeatedForkPerForkFrozenState` (the `firecracker-test`
job): it forks one source twice with a source write of the SAME page P between and
after the forks (P: `M1` at fork A, `M2` at fork B, `M3` after fork B) and asserts fork
A's child reads `M1`, fork B's child reads `M2`, and `M3` leaks into neither; it FAILS
on the shared-state code (fork B's fresh-epoch page-0-clear check trips, and the child
reads fork A's stale value) and PASSES with per-fork epochs.

### Live copy-on-write fork: child-side memfd import (milestone m5)

The child side of the same path boots the co-located child's guest RAM from the
parent's live memory instead of the disk snapshot `mem` file. The child performs
one memory-attach: `MAP_PRIVATE` of the parent's shared guest memfd, then a
per-page overlay from the handler's FROZEN memfd for the pages the parent clobbered
after the fork point (`fork.ComposeChildFromImport` /
`composeChildGuestMemory`, `internal/fork/childmemfd*.go`). The per-page selector is
the SAME point-in-time source selection the parent-side proof asserts: a frozen page
(the parent overwrote it) is taken from FROZEN at its fork-time value; every other
page is the `MAP_PRIVATE` view of the shared memfd, which still holds the fork-time
value because the parent never wrote it (or the write is pinned in FROZEN). Because
the attach is an `O(1)` mapping plus a copy of only the handful of clobbered pages,
it does not read the guest RAM back off disk, which is the sub-100ms win.

The parent's armed WP handler publishes the child's coordinates as a
`ChildMemfdImport` (`ChildImport`: the parent memfd from the m1 export, the FROZEN
memfd, the handler's LIVE frozen bitmap memfd, and the page size). The bitmap is
passed as a live memfd, NOT a static snapshot: the child re-maps it `MAP_SHARED` and
reads the CURRENT per-page selector at attach time, so a page the handler freezes
AFTER the import is assembled but BEFORE the child attaches is still sourced from
FROZEN (the no-leak invariant holds end to end across that window, not just at the
instant the import was built). Each memfd also carries its `(st_ino, st_dev)`
identity, captured by the parent that owns it; the child re-`fstat`s the descriptor
it reopens through `/proc/<pid>/fd/<fd>` and refuses any mismatch or non-memfd
target, so an exporter that exited with its PID recycled to an unrelated process
cannot make the child attach a foreign descriptor (fail closed to the disk restore).
The husk writes that as the
`FIRECRACKER_MITOS_CHILD_MEMFD` export file and sets the env on the child
Firecracker (`Stub.liveCowChildImportEnv`, `SpawnVM`). The child Firecracker's
memory backing comes from the memfd + FROZEN overlay while its CPU + device vmstate
still restores from the snapshot `vmstate` file. FAIL-CLOSED: the disk `mem` path is
STILL passed to `LoadSnapshot`, so a child Firecracker that does not honor the env
(or a pod with no armed parent) restores from disk byte-for-byte; any error
assembling the import logs and falls back to the disk restore. Off, or where the
import is unavailable, the child restores from disk exactly as before.

Verified on real KVM by `internal/fork` `TestLiveCowChildBootsFromSharedMemfd` (the
`firecracker-test` job, `go test ./internal/fork/...`): it drives the PRODUCTION
child import (`handle.ChildImport` -> `ComposeChildFromImport`) over a real memfd +
real FROZEN image produced by the real WP handler, and asserts the child boots from
the SHARED memfd, NOT disk, with BOTH halves of the invariant, NO LEAK (a page the
resumed parent overwrote is read from FROZEN at its fork-time value, never the
parent's post-fork write) and INHERITANCE (an untouched page is read live from the
shared memfd). It also measures the child memory attach and asserts it is sub-100ms;
it records a same-size disk mem-file restore as a context log line ONLY, not an
assertion (at this micro scale that baseline faults from a just-written warm page
cache, so it is a RAM-speed read too; the real prod win is the child faulting from
the parent's resident RAM via CoW plus an O(1) attach at scale, measured on prod).
On a runner whose kernel lacks write-protect it self-skips with the m2 precondition
reason. A sibling KVM test `TestLiveCowChildImportRefreshesFrozenBitmap` pins the
LIVE-bitmap timing: it assembles the import, THEN has the parent overwrite a page,
THEN attaches, and asserts the child still reads the fork-time bytes from FROZEN (a
stale bitmap snapshot would leak the post-import write; the test demonstrates that
leak in-log against a t0 snapshot and proves the production live-bitmap attach does
not). The contract and
env plumbing are unit-tested off KVM (`internal/fork`
`TestChildMemfdEnv`/`TestChildMemfdImportRoundTrip`/`TestParseChildMemfdImportRejectsBad`,
`internal/husk`
`TestLiveCowChildImportEnvNoParent`/`TestLiveCowChildImportEnvArmed`/`TestLiveCowChildImportEnvFailClosed`).

The parent-arm production wiring (milestone m6b) now lands: when a source (default)
VM launches under `--live-cow-fork` with a real workdir, `prepareInstance` calls
`Stub.armLiveCowSource`, which binds the write-protect handshake socket and launches
the source Firecracker with the `FIRECRACKER_MITOS_*` env (m1 export + m2 WP offer).
A background goroutine (`serveLiveCowSource`) completes the handshake once the patched
source Firecracker connects during restore, arms the freezer via `SetLiveCowParent`
(so `liveCowSnapshotFreezer` and `liveCowChildImportEnv` stop returning nil), and runs
the copy-before-unprotect `Serve` loop for the life of the source. The handler is
retained on the Stub (`liveCowHandle`) and Closed at teardown (`closeLiveCowSource`),
unblocking a stuck `Receive` and stopping `Serve`. FAIL-SAFE: it arms ONLY with the
flag on AND a real workdir; the flag off, the mock/unit path, a bind failure, or a
non-Linux host all leave the freezer nil, so a fork takes the Full-snapshot fallback
and never breaks.

Pending (documented, not a gap in what ships): the child Firecracker's restore path
does not yet READ `FIRECRACKER_MITOS_CHILD_MEMFD` (the pinned
`mitos-run/firecracker` `mitos-fc-vmstate-only-v1.15.0` binary patches the PARENT
side: m1 memfd export + m2 WP offer + m6a vmstate-only snapshot; its `snapshot_file`
restore still `MAP_PRIVATE`s the disk mem file). The remaining work is the smallest
child-restore Firecracker patch that maps the guest region `MAP_PRIVATE` from the
passed memfd + FROZEN overlay when that env is set, built via the fork's
`build-patched-fc` workflow and pinned by a new sha256 in
`hack/install-firecracker-patched.sh`. This is the LAST prerequisite before
`--live-cow-fork` can be flipped on in prod: with the parent-arm wiring live, a fork
takes the vmstate-only path (no `mem` file), so the co-located child MUST import its
RAM from the parent memfd; until the child-restore patch ships the flag stays OFF
(default). The memory-attach mechanism itself is KVM-proven through the Go code path
above, so the Firecracker patch is a faithful port, not a new design.

### Live copy-on-write fork: source vmstate-only capture (issue #832)

The SOURCE side of the same path stops writing the guest RAM to disk at all. A Full
fork snapshot pauses the source and copies the ENTIRE guest RAM into a `mem` file
(the ~364ms `create_snapshot` stage measured on prod, issue #832) plus the small
device/CPU `vmstate`. On the live-cow path that `mem` file is redundant: the child
boots its guest RAM from the parent's shared memfd (the m5 section above), so the
source only needs to (1) FREEZE its guest at the fork point and (2) capture the
small `vmstate`.

The armed parent handle is the one `armLiveCowSource` (m6b, above) binds to the
running source VM: the freezer seam is no longer nil in production once the source
Firecracker completes the write-protect handshake. `forkSnapshotInstance`
(`internal/husk/multivm.go`) therefore branches on the armed
parent handle (`Stub.liveCowSnapshotFreezer`, gated on `--live-cow-fork` AND a
parent that satisfies the `Freeze()` seam) AND on `--live-cow-child-import`
(`Stub.liveCowChildImport`, see the child-restorability invariant below): inside the paused window it calls the
handle's `Freeze()` (the m2 `UFFDIO_WRITEPROTECT`-all, microseconds) and then
`vmm.CreateSnapshotVMStateOnly` (`internal/firecracker` PUT `/snapshot/create` with
the Mitos `MitosVmstateOnly` type and NO `mem_file_path`), so NO `mem` file is
written. The point-in-time-T guarantee is unchanged: the freeze is the SAME
write-protect the m4b/m5 sections prove correct, so a resumed source cannot leak a
post-fork write into the child, and the source rootfs is still frozen inside the
same paused window as its point-in-time pair. The fork-correctness re-seed
(RNG/clock/secret/network) is untouched; this is a source-capture optimization, not
a new fork policy.

FAIL CLOSED and never-breaks-a-fork: when the flag is off, no parent is armed, or
the pod is not live-cow (the default everywhere today), `forkSnapshotInstance` runs
the Full `CreateSnapshot(mem, vmstate)` byte-for-byte as before, and any freeze or
capture error resumes the source (bounded retry, never left frozen) before failing
closed. `CreateSnapshotVMStateOnly` REQUIRES the Mitos-patched Firecracker
vmstate-only snapshot mode (stock Firecracker rejects a `/snapshot/create` with no
`mem_file_path`); the gate guarantees it is only ever reached on the armed live-cow
path.

Verified on real KVM at two levels. The memory mechanism is proven by `internal/fork`
`TestLiveCowForkVmstateOnlyNoMemFile` (the `firecracker-test` job, `go test
./internal/fork/...`): the source FREEZEs and writes ONLY a `vmstate` file (asserting
NO `mem` file is written and a non-empty `vmstate` is), a resumed source overwrites a
page, and the child boots from the shared memfd through the production
`ComposeChildFromImport` and still reads the fork-time bytes (NO LEAK) plus an
untouched page (INHERITANCE), with the whole paused-window capture measured far below
the ~364ms Full mem write. The m6b PARENT-ARM wiring is proven end to end by
`internal/husk` `TestForkSnapshotLiveCowSourceArmedVmstateOnlyKVM` (same job, `go test
./internal/husk/...`): a real `--multi-vm --live-cow-fork` Stub restores a source VM,
`armLiveCowSource` arms the freezer via the real patched Firecracker's write-protect
handshake, and `ForkSnapshot` writes NO `mem` file (only `vmstate` + the frozen
`rootfs.ext4`), with the recorded `create_snapshot` stage far below the ~364ms Full
mem write; the resumed source still execs (never left frozen) and its running guest
takes write-protect faults the armed `Serve` loop resolves; and the production child
import (`handle.ChildImport` -> `ComposeChildFromImport`) composes the child's guest
RAM from the source memfd + FROZEN overlay with no disk mem file. Off KVM (or where
the source Firecracker does not offer the handshake) it self-skips with the reason.
The husk branch selection and the fail-closed resume are unit-tested off KVM
(`internal/husk`
`TestForkSnapshotLiveCowCapturesVMStateOnly`/`TestForkSnapshotFallsBackToFullWhenNoArmedParent`/`TestForkSnapshotLiveCowResumesSourceOnFreezeError`),
the source-arm gating and fail-safe are unit-tested
(`TestArmLiveCowSourceGatedOff`/`TestArmLiveCowSourceBindsAndEmitsEnv`), and the
vmstate-only Firecracker request is unit-tested in `internal/firecracker`
`TestCreateSnapshotVMStateOnlyOmitsMemFile`.

CHILD-RESTORABILITY INVARIANT (issue #832, the v1.32.2 prod hang fix): a fork
snapshot MUST be restorable by the consumer that will boot from it. The co-located
child (`SpawnVM` -> `activateInstance` -> `LoadSnapshotWithOverrides(mem, vmstate)`)
RESTORES FROM THE DISK fork snapshot unless a CHILD-SIDE memfd-import Firecracker
patch boots its guest RAM from the parent memfd. The shipped patched binary
(`mitos-fc-wp-on-restore`) patches the SOURCE (restore) side ONLY: no child-side
import exists, and `ComposeChildFromImport` has no production caller (it is exercised
only by the KVM/unit tests). So the vmstate-only capture (NO `mem` file) leaves the
co-located child with nothing to restore, and turning `--live-cow-fork` on hung every
fork (children stuck `phase=Restoring` until the client 120s deadline: `FORKERR
120.3` on the v1.32.2 canary), because the source armed and `forkSnapshotInstance`
skipped the mem write while the child still restored from disk.

The mem-skip is therefore gated on a SEPARATE capability, `--live-cow-child-import`
(`Options.LiveCowChildImport`, DEFAULT OFF), NOT on `--live-cow-fork` alone.
`forkSnapshotInstance` takes the vmstate-only path only when the source is armed AND
child import is enabled; otherwise an ARMED source still writes the Full disk `mem`
so every co-located child is restorable and the fork never hangs (the proven disk
path, ~832ms). This splits arming the source (safe on its own: the freezer goes
live, exercised on prod) from skipping the disk mem (safe only once a child-side
import binary is shipped and every co-located child imports the memfd). Re-enabling
`--live-cow-fork` is therefore safe on the shipped stack: the fork falls back Full
and clean. `--live-cow-child-import` is flipped on ONLY once a child-side
memfd-import Firecracker ships; the source mem-write elimination and the child memfd
import go live together. Guarded by
`TestForkSnapshotArmedSourceWritesMemWhenChildImportOff` (unit: an armed source with
child import off writes the disk `mem`, is not frozen, records no `freeze` stage) and
`TestForkSnapshotLiveCowArmedFullFallbackColocatedChildKVM` (KVM: the prod
restore->claim->arm->fork sequence, then a REAL co-located `SpawnVM` child restores
and execs with no hang).

### Workspace resume from a memory snapshot

A resumable workspace head (W4) restores a captured VM MEMORY image into a fresh
sandbox on activation (the "wake" of the sleep-consolidation demo). A resume is a
restore-from-snapshot exactly like a fork, so it inherits the SAME
fork-correctness hazards and the SAME mitigations: the resumed VM activates
through the standard `Activate` path and runs the RNG-reseed + clock-step
`NotifyForked` handshake, so the resumed guest reseeds its kernel CRNG with fresh
host entropy and steps its wall clock rather than waking with the captured
snapshot's stale CRNG/clock state. A resumed VM is therefore NOT a CRNG/clock
clone of the sandbox that captured it; it gets the same per-restore reseed an
engine fork or a husk fork child gets (sections 1 and 2). The Python-level PRNG
caveat in section 1 applies to a resumed sandbox identically (a long-lived
interpreter that seeded its PRNG before the checkpoint keeps that PRNG state
across the resume; reseed in-process after wake if it matters).

Disk/memory consistency: the resume pairs the memory image with the SAME
workspace filesystem state it was captured against (the revision's
`contentManifest`, hydrated alongside the memory restore), so the resumed guest
memory matches the disk it sees, the same disk-half invariant the husk fork child
maintains above. The memory image is principal-bound and is never served across
principals (`docs/threat-model.md`); that is a security property, not a
correctness one, but it is enforced fail-closed BEFORE any restore runs.

NOT WIRED (review finding). The above describes the resume reseed handshake on a
path whose PRODUCTION hook is nil today. The controller `resumeMemory` seam
(`internal/controller/workspace_binding.go`) defaults to a fail-closed error when
`ResumeMemory` is unset, and real memory restore is gated behind the
`--workspace-memory-snapshots` flag (off by default). So the resume-then-reseed
flow is object-level proven in envtest but the real VM-memory restore (and thus
the actual in-VM reseed on resume) is not wired into the shipped controller; the
correctness claim for resume holds only once that hook is bound on a KVM kubelet.

## 1. RNG and entropy after restore

Every VM restored from the same snapshot wakes up with byte-identical kernel
CRNG state, identical userspace PRNG state in any already-started runtime, and
identical TLS library state. Consequences: colliding UUIDs, predictable session
tokens, identical TLS ClientHello randoms, broken nonce-based crypto.

Implemented reseed path: the host delivers fresh entropy over vsock. forkd
calls `NotifyForked(generation, entropy)` immediately after restore
(`internal/daemon/server.go:notifyForked` generates a fresh generation plus 32
bytes of `crypto/rand` entropy). The guest agent on `NotifyForked` writes that
entropy into the kernel CRNG via the credited `RNDADDENTROPY` ioctl
(`guest/agent-rs/src/sys/entropy.rs`, the only caller of that ioctl), records
the generation at `/run/sandbox/fork-generation`, and signals userspace
runtimes; the reseed step is `guest/agent-rs/src/fork/reseed.rs` and the
orchestrator that drives reseed + clock step + userspace signal is
`handle_notify_forked` in `guest/agent-rs/src/fork/mod.rs`. VMGenID is not
exposed by Firecracker, so this host-entropy-over-vsock hook is our equivalent.

**Continuous virtio-rng device (wired):** in addition to the one-shot
NotifyForked reseed at fork time, every template snapshot now bakes a virtio-rng
device backed by the host RNG, so each restored fork has a CONTINUOUS host
entropy source feeding its CRNG, not just the single credited injection at the
instant of the fork. The device is attached before InstanceStart at template
build (`firecracker.Client.SetEntropy` -> `PUT /entropy`, driven by
`firecracker.VMConfig.EntropyDevice`, default-on via `DefaultVMConfig`); because
Firecracker bakes its device model into the snapshot and cannot add a device on
restore, every fork restores the device with no per-fork API call. The config
and JSON-building logic is darwin-unit-tested (`TestEntropyRequestJSON`,
`TestDefaultVMConfigEnablesEntropy`), and the live in-guest behavior is now
OBSERVED on KVM: the fork-correctness phase asserts each restored guest's
`/sys/class/misc/hw_random/rng_current` is `virtio_rng.N` (the guest kernel
binds `CONFIG_HW_RANDOM_VIRTIO`), so the device is present and selected in the
fork, not merely declared in the VM config. The NotifyForked credited reseed
remains the fail-closed gate; the virtio-rng device is the continuous
complement, not a replacement.

**Fail-closed scope.** The reseed is fail-closed on EVERY engine. The husk path
(`internal/husk` `productionNotifier`), the raw-forkd path
(`internal/daemon/server.go` `notifyForked` inspects the `NotifyForked` response
returned by `internal/daemon/sandbox_api.go` and reaps a fork whose guest
reports `ReseededRNG:false`), and sandbox-server real-mode
(`cmd/sandbox-server/main.go` `reseedFork`) all refuse to serve an un-reseeded
fork rather than emitting duplicate CRNG output. See row 1.

**Write-fallback over-report: FIXED.** Previously, when `RNDADDENTROPY` failed,
`reseedCRNG` fell back to writing the bytes to `/dev/urandom` (which mixes them
into the pool but does NOT credit entropy) and still returned `reseeded=true`.
Because the host fail-closed gate keys entirely on that boolean, the over-report
silently defeated the gate: a fork that could not be credibly reseeded would be
served sharing its siblings' CRNG output. The Rust reseed step
(`guest/agent-rs/src/fork/reseed.rs`, backed by the credited ioctl in
`guest/agent-rs/src/sys/entropy.rs`) returns `false` whenever the credited ioctl
fails and never falls back to an uncredited `/dev/urandom` write, so the host
reaps such a fork. The guest agent runs as PID 1 with full capabilities
on our shipped kernel, where `RNDADDENTROPY` succeeds, so the credited path is
the normal path; the fallback was the unsafe one. The reseed contract is now
"credited or refused" end to end.

Tests. go (`internal/daemon`): `TestForkNotifiesAgentWithFreshEntropy` asserts
forkd sends entropy, `TestForkGenerationIncrementsAcrossForks` asserts distinct
generations across forks, `TestForkFailsWhenNotifyForkedErrors` asserts a
real-engine fork fails closed when the guest cannot reseed. The host fail-closed
gate that reaps a fork whose guest reports `ReseededRNG:false` (transport
`OK:true` but uncredited reseed) is covered by `internal/daemon/delivery_test.go`.
guest (Rust, linux): the reseed unit tests in `guest/agent-rs/src/fork/reseed.rs`
(empty entropy is false; the credited-ioctl path) assert the reseed reports
failure on any uncredited path, which the host then reaps.

KVM (`kvm-test.yaml`): one snapshot is taken after the agent is up, two VMs are
restored from it, and `test-agent --mode notify` runs against each. The phase
asserts the two `URANDOM=` base64 samples differ (equal would be the shared-RNG
bug). It additionally asserts the two forks produce a DISTINCT kernel UUID
(`/proc/sys/kernel/random/uuid`, the same CRNG draw `uuid.uuid4()` and systemd
machine-id use) and a DISTINCT TLS client random (32 bytes from `getrandom`,
modelling the ClientHello random a TLS handshake would put on the wire), so the
proof covers machine identity and TLS nonces, not only the raw `/dev/urandom`
stream. This proves the GUEST applies the reseed; forkd end-to-end notify is
covered by the go tests above. The UUID and TLS assertions are gated (`exit 1`)
in this phase and run on every KVM CI run (the `firecracker-test` job), so they
are observed on KVM, not only unit-asserted. The N=8 variant remains a
follow-up.

**run_code kernel caveat.** A forked VM inherits the LIVE run_code kernel
(`/opt/mitos/kernel_driver.py`) and its entire Python namespace, because the
kernel process is part of the snapshot. Two ways the kernel gets into the
snapshot: by default it starts lazily on the first `run_code` (so a template
snapshotted before any `run_code` has NO kernel and every fork cold-starts it,
~5s on the first call); when the pool template sets `warmKernel: true`
(CreateTemplateRequest.warm_kernel), the template build runs one trivial cell
(`pass`) through `Sandbox.RunCodeStream` right before the snapshot, so the
kernel is captured ALREADY RUNNING and forks answer their first `run_code`
warm. The warmup cell deliberately draws no randomness and imports nothing:
CPython seeds its Mersenne Twister lazily from `os.urandom` on first use, so
an untouched `random` (and numpy) stays UNSEEDED in the snapshot and each fork
seeds it fresh, after the per-fork CRNG reseed, on its own first draw
(pinned by `TestWarmKernelCode_NeverDrawsRandomness`).

The post-fork `NotifyForked` reseed handles the kernel CRNG (`/dev/urandom`)
and signals userspace, but a Python-level PRNG that was already seeded INSIDE
the kernel before the fork (`random.seed(...)`, `numpy.random.seed(...)`, or
the implicit module-global `random` state once drawn from, e.g. by user code
run against the template or a pre-fork sandbox) is captured in the snapshot
and is therefore IDENTICAL across all forks until reseeded. This is the same
class as item 1 for any already-started runtime; it is not a host boundary,
and the warm_kernel warmup neither causes nor fixes it. Remediation: callers
who need per-fork randomness in the kernel should reseed after a fork
(`random.seed()`, `np.random.seed()` with no argument reseeds from fresh OS
entropy), or avoid seeding the PRNG before the fork point.

## 2. Clock correctness

A restored guest's wall clock is frozen at snapshot time. TLS certificate
validation (`notBefore`) and JWT `iat`/`exp` checks fail silently or, worse,
pass when they should fail.

Implemented clock step: `NotifyForked` carries `HostWallClockNanos`, stamped by
the host at send time in the gRPC NotifyForked path (`internal/daemon/sandbox_api.go`
for the raw-forkd path, `internal/husk/stub.go` for the husk path). The guest
agent reads it and calls `clock_settime(CLOCK_REALTIME)` when drift exceeds a
500ms tolerance, then signals userspace as in section 1
(`guest/agent-rs/src/fork/clock.rs`, orchestrated by
`handle_notify_forked` in `guest/agent-rs/src/fork/mod.rs`). kvm-clock
remaining the active clocksource and Firecracker's restore path updating it is
relied on but not separately asserted here.

**CLOCK_MONOTONIC: documented residual, no step possible.** `stepClock` steps
only `CLOCK_REALTIME`, and this is correct, not a missing step. Two facts make a
"monotonic step" both impossible and (for a clean restore) unnecessary:

1. Linux rejects `clock_settime(CLOCK_MONOTONIC)` with `EINVAL`. There is no
   syscall to step the monotonic clock; any claim of a monotonic step would be a
   fake fix. We therefore do not attempt one.
2. The VM is PAUSED across snapshot/restore. CLOCK_MONOTONIC resumes continuously
   from its snapshot value: it does NOT jump forward by the wall-time gap between
   snapshot and restore the way the wall clock does. A timer or deadline anchored
   purely to the monotonic clock therefore continues counting across the restore
   rather than mis-firing; it just measures less elapsed time than wall time did.

The residual hazard is narrow and is NOT "all monotonic timers mis-fire": it is
userspace code that DERIVED a monotonic deadline from a wall-clock baseline (mixed
the two clocks, for example `deadline_mono = now_mono + (exp_wall - now_wall)`).
After the wall clock is stepped, such a deadline can be off. The reset path for
that case is the existing `signalUserspace` SIGUSR2 (section 1): a runtime that
pinned a deadline to the old wall time re-derives it on the signal. This is a
DOCUMENTED residual (honesty over a fake fix): there is no correct monotonic step
to apply, the clean-restore case is already correct, and the mixed-clock case is
handled by the userspace reset signal rather than by a kernel call that does not
exist.

Tests. go: `TestForkNotifiesAgentWithFreshEntropy` covers that forkd sends the
notification carrying the host wall clock.

KVM (`kvm-test.yaml`): each restored fork's `WALLCLOCK_NS` (from in-guest
`date +%s%N`) is asserted within 2 seconds of the runner clock. This proves the
GUEST holds a correct wall clock after restore. The post-snapshot TLS-cert
validation variant remains a follow-up.

## 3. Live-fork memory hygiene (secrets)

A `Sandbox` with `spec.source.fromSandbox` of a running sandbox duplicates everything in guest memory,
including claim-time secrets, into every fork.

**Chosen policy (default-safe):** live forks of a sandbox that holds claim-time
secrets are **rejected** unless one of:

- `spec.secretInheritance: inherit` is set on the `Sandbox` (explicit
  opt-in, recorded in the fork's status), or
- the platform implements revoke-and-reissue for the secret class in question
  (each fork receives fresh credentials over vsock post-restore; the parent's
  copies that leaked into fork memory are revoked upstream). This is the
  long-term default; rejection is the stopgap.

The default-deny gate plus opt-in audit trail is implemented in the fork
controller (`internal/controller/sandboxfork_controller.go`): forks of
secret-holding sandboxes get a terminal typed `Rejected` condition without
`spec.secretInheritance: inherit`, and explicit opt-ins are recorded as an
audit condition. Secret *delivery* is implemented too: the controller resolves
Secret refs (`internal/controller/sandboxclaim_controller.go:resolveSecrets`)
and forkd delivers them over vsock post-restore
(`internal/daemon/server.go:deliverConfig`); never baked into snapshots,
never in Firecracker boot args or the FC API socket request log.

Per-fork reissue, what is and is not done. The PLATFORM credential a fork
receives, its sandbox-API bearer token, IS reissued per fork: the controller
mints a fresh 32-byte token (`mintAPIToken`) for every fork, distinct from the
parent's, so a fork cannot authenticate to the sandbox HTTP API as its parent
(`TestForkBearerTokenIsFreshlyReissuedNotInheritedFromParent`). What remains a
documented future, not a gap in the default-safe mitigation, is revoke-and-
reissue of TENANT secret VALUES over vsock so a fork could safely inherit FRESH
secrets instead of being rejected: that needs the secret to be a dynamically
reissuable/revocable credential from a broker (a static Kubernetes Secret value
has no upstream to revoke), tracked as issue #7; capability-token per-fork
attenuation lands with the #25 runtime wiring. The hazard, secret duplication
into forks, is fully closed today by the default-deny gate.

Test: claim with a secret, exec to confirm the secret is visible in the parent,
fork without opt-in → typed `Rejected` condition; fork with opt-in → secret
present and an audit annotation recorded; and a fork's bearer token is freshly
reissued and distinct from the parent's.

## 4. Network identity after fork

Forked guests must not wake up with the parent's MAC/IP/TCP state.

**Current reality:** per-fork networking is wired (`EngineOpts.NetManager` +
`NetAllocator`; `internal/netconf`, `internal/network`, host nftables egress).
The template snapshot bakes a stub NIC with a placeholder MAC and a placeholder
guest IP that never carries live traffic; on every fork the engine acquires a
distinct identity and the guest agent re-addresses eth0 on restore.

How the design is met:

- Distinct identity per fork: `netconf.Allocator.Acquire(sandboxID)` hands out a
  unique /30 (distinct host tap, host IP, and guest IP) and a freshly derived
  locally-administered guest MAC (`deriveMAC(sandboxID)`). The engine delivers
  both in the `NotifyForked` network config; `internal/guestnet.Configure` sets
  eth0's MAC (link down, set hw addr, link up), flushes the old address, and
  assigns the per-fork IP and default route. So a fork wakes with its OWN MAC
  and IP, not the parent's baked placeholder.
- Parent's open TCP connections are dead in the fork: the agent FLUSHES eth0's
  prior address and assigns the fork's own IP, so a socket that was open in the
  source snapshot, bound to the now-absent source address, can no longer send.
  Any in-flight connection at fork time is broken in the fork by design;
  reconnect logic belongs in the workload.

Proof: `TestPrepareForkNetworkDistinctPerFork` (distinct tap/MAC/IP per fork) and
`TestBuildSetMACCarriesAddress` (the netlink MAC-set) unit-test it; on KVM,
`cmd/net-fork-smoke` boots two forks of one snapshot, delivers each its
NotifyForked network config, reads each guest's eth0, and asserts the two forks
have DIFFERENT guest MACs (each its derived MAC, neither the placeholder) and
DIFFERENT guest IPs. Documented residual, not a gap in the identity: the
upstream-socket inheritance hazard for networked live forks is now tracked as a
SEPARATE concern in row 8 (the conntrack flush and proxy upstream-socket reset
are implemented by #336, and the open-socket-death assertion on KVM is covered
by the networked-live-fork acceptance phase); egress allow/deny is proven
separately by the guest-egress KVM phases.

### vsock UDS identity (device-identity-after-fork)

The host-side vsock device is itself a device-identity hazard. Firecracker
bakes the vsock `uds_path` string verbatim into the snapshot and rebinds that
exact path on every restore. If a template were snapshotted with an *absolute*
`uds_path`, every fork of that one snapshot would try to bind the same host
socket: the second and later restores fail with `VsockUnixBackend: Error
binding to the host-side Unix socket: Address in use (os error 98)`, and a
lingering socket file from a killed source VM blocks even the first restore.

Handling: template snapshots bake a *relative* `uds_path`
(`firecracker.VsockRelPath` = `"vsock.sock"`). A relative path is resolved by
each restored Firecracker process against its own working directory, so an
identical baked path plus a distinct per-VM cwd yields a distinct host socket
and forks never collide. In raw direct-exec (`forkd`) mode the working
directory is the per-sandbox `WorkDir`, set as the Firecracker process's
`cmd.Dir` in `internal/firecracker/client.go:StartVM`; the engine reports the
resolved socket via `Client.VsockHostPath`. Under the jailer the chroot already
isolates each VM (the relative path resolves against the per-VM chroot root), so
the hazard is moot there; raw mode is the one that depends on the relative path.
The invariant is locked in by `TestVsockHostPathPerCwd`
(`internal/firecracker/jailer_test.go`) and exercised end to end by the
`fork-correctness` CI phase, which restores two VMs from one snapshot in
separate working directories and asserts both bind distinct sockets and answer.

## 5. Memory accounting truthfulness

"~KB per fork" measured at T=0 is the dirty-page count immediately after
restore. It is not a density planning number; a `pip install` in the fork
makes pages unique.

Required implementation:

- `mitos_memory_unique_bytes` sampled periodically over the sandbox lifetime,
  not only at fork time. DONE for the aggregate gauge: `Engine.Metering`
  re-samples each live sandbox's `/proc/<pid>/smaps_rollup` on every pass, and
  `Server.SampleMetrics` (started by `cmd/forkd`) refreshes the gauge on an
  interval, so `/metrics` reports lifetime unique bytes rather than the T=0
  footprint recorded by `readMemoryStats` at fork. Per-sandbox-labeled series
  are now exported too: `mitos_sandbox_memory_unique_bytes{sandbox,template}` is
  a GaugeVec set once per live sandbox each sampling pass and Reset between
  passes so a terminated sandbox drops its series (no stale value).
- Published density numbers must include unique-memory-after-representative-
  workload (e.g., after `pip install numpy && python -c "import numpy"`)
  alongside the T=0 number, produced by `bench/`.

Test: `TestUpdateMetricsPopulatesMemoryGauge` and
`TestSampleMetricsTicksAndStopsOnContextCancel` cover the periodic wiring
locally. The KVM variant is now present as test code: `cmd/mem-smoke` forks a
sandbox, samples `Engine.Metering().TotalUnique` at T=0, allocates and TOUCHES
64 MiB in the guest over the agent, re-samples, and asserts the metered unique
bytes grew by at least a 32 MiB floor (a metric frozen at the T=0 footprint
fails). It is wired into the `kvm-test.yaml` lifetime-memory phase and runs on
every `firecracker-test` KVM run (`--min-growth-bytes 33554432`), so the growth
is OBSERVED on KVM, not only unit-asserted.

## 6. Snapshot compatibility on restore

A Firecracker snapshot is not portable across arbitrary hosts. Restoring memory
and device state captured under a different Firecracker version or on a
different CPU model is a crash or silent-corruption hazard: the guest can fault,
hang, or run with subtly wrong CPU feature assumptions. The snapshot format
itself can also change incompatibly between builds.

Contract fields. Every manifest records the producing environment as part of its
content-addressed identity (`internal/cas.Manifest`):

- `SnapshotFormatVersion` (current = `cas.CurrentSnapshotFormatVersion` = 1)
- `VMMVersion` (the producing Firecracker version)
- `CPUModel` (the host CPU model)
- `KernelVersion` (the guest kernel; informational)
- `ConfigHash` (the microvm machine config the snapshot was captured under)

Policy (`internal/snapcompat.Check`). A restore is refused unless the format
version is in the set this build supports, the Firecracker version matches
exactly, and the CPU model matches exactly. A kernel mismatch is informational
when the rest match (the guest kernel is baked into the snapshot image). The
check is part of the load gate: it runs after the content-addressed digest
verify and before any Firecracker launch, so an incompatible snapshot never
reaches a VM. Refusals carry an actionable message (rebuild the template on this
node, or schedule the fork on a matching node). A `--allow-incompatible-snapshots`
dev escape hatch logs loudly and proceeds; it is for development only.

Open: Firecracker CPU templates would relax the exact-CPU-model rule for a
defined family; live cross-Firecracker-version restore testing needs two FC
versions in CI. Both are tracked as follow-ups; the contract refuses them today.

See docs/snapshot-format.md for the full format-version and migration policy.

## CI job

`kvm-test.yaml` (GitHub Actions, KVM-capable runner) runs the RNG and clock
proofs above on every PR touching `internal/firecracker/`, `internal/fork/`,
`guest/`, `cmd/test-agent/`, `cmd/mem-smoke/`, or `internal/vsock/`. It takes one
snapshot after the agent is up, restores two VMs from it, and asserts distinct
`/dev/urandom`, distinct kernel UUID, distinct TLS client random, each wall
clock within 2s of the runner, and the fork-generation file matching the
generation sent. A separate lifetime-memory phase (`cmd/mem-smoke`) forks one
sandbox, touches 64 MiB in the guest, and asserts the metered unique bytes grow
(row 5). A jailer-boot phase restores the same snapshot under the jailer to
prove the chroot/uid mechanics. It does NOT prove the dropped capability set,
since the runner is root; the INTENDED capability list is computed in one tested
place (`cmd/forkd` `jailerRequiredCapabilities`, unit-tested on darwin against
the exact set CAP_SYS_ADMIN, CAP_CHOWN, CAP_SETUID, CAP_SETGID, CAP_MKNOD), and
the kernel actually enforcing the drop of everything else stays the KVM /
non-root-gated sub-step (`continue-on-error`, see issue #2).

The same job also runs a husk activate-correctness phase: it activates two VMs
from one bench template snapshot via two fresh dormant `cmd/husk-stub` processes
(each `Activate` runs the reseed + clock-step handshake and delivers env/secrets,
fail-closed) and asserts the same three properties as the fork-correctness phase
across the two activations: distinct `/dev/urandom`, each wall clock within 2s of
the runner, and a delivered env var plus secret readable in each guest with the
secret value absent from every host-side stub/client log.

Until every row above is `done`, fork correctness is the top engineering
priority and blocks feature work (see `ROADMAP.md`).

## Rust guest agent path (`guest/agent-rs`)

The Rust agent (`guest/agent-rs`) is the SOLE production guest agent since Phase
E (#310). The Go agent (`guest/agent`) and the legacy JSON vsock protocol (port
52) are removed. `guest/rootfs/build.sh` bakes only the Rust agent as /init;
all host-side callers (`internal/fork/uffd_engine.go`, `internal/daemon/sandbox_api.go`,
`internal/firecracker/template.go`, `internal/husk/stub.go`, and the smoke
binaries in `cmd/`) speak gRPC on vsock port 53 (AgentGRPCPort). There is no
rollback to a Go agent fallback; the cutover is complete.

SECURITY-SENSITIVE: `guest/agent-rs/src/sys/`, `guest/agent-rs/src/fork/`,
`guest/agent-rs/src/init/mod.rs`, and `guest/agent-rs/src/main.rs` require a
named human reviewer before any PR touching them is merged to main. See
`docs/threat-model.md` and `docs/security-review-policy.md`.

### Fork-correctness coverage for the Rust agent

The Rust agent implements all five fork-correctness hazards from the table above
via dedicated modules in `guest/agent-rs/src/fork/`:

| Hazard | Rust implementation | Status |
|---|---|---|
| 1. RNG/CRNG reseed | `fork/reseed.rs`: `sys::entropy::reseed_crng` writes the host-supplied entropy bytes into the kernel CRNG via `RNDADDENTROPY` (credited ioctl); reports `reseeded_rng: false` if the ioctl fails, triggering host fail-closed reap | **done (KVM fork-correctness suite exercises this as the sole production agent)** |
| 2. Wall-clock step | `fork/clock.rs`: `sys::clock::step_realtime_ns` calls `clock_settime(CLOCK_REALTIME)` when drift exceeds 500ms | **done (KVM-validated)** |
| 3. SIGUSR2 signal to userspace | `fork/signal.rs`: `sys::signal::signal_userspace` sends SIGUSR2 to user-space processes after reseed + clock step, EXCEPT processes in a registered serving-workload session (issue #460). SIGUSR2 default-terminates a process that installs no handler, so a captured-running serving workload (Run with Mitos) would be killed on every fork; `select_targets` reads each pid's session from `/proc/<pid>/stat` and skips the workload's whole session. Issue #467: SIGUSR2's default disposition is terminate, so the broadcast now delivers ONLY to processes that installed a SIGUSR2 handler (the `SigCgt` bit in `/proc/<pid>/status`), fail-safe skipping any process whose handler cannot be confirmed. A non-handler process is never signaled and so never silently killed; a registered serving workload's whole session is still excluded outright (so a handler-having app like nginx is left alone, not triggered). This does not weaken reseed: the kernel CRNG reseed (row 1) and virtio-rng are independent of this signal, which only resets userspace clock-derived deadlines. | **done (handler-gated broadcast + workload-session exclusion; unit-tested)** |
| 4. Per-fork network reconfiguration | `fork/network.rs`: sets eth0 MAC via rtnetlink, flushes old address, assigns per-fork IP and default route | **done (KVM-validated)** |
| 5. Per-fork volume mounts | `fork/volumes.rs`: mounts per-fork volume block devices at the paths specified in the `NotifyForkedRequest` | **done (KVM-validated)** |

The production KVM fork-correctness CI suite (`kvm-test.yaml`) boots the Rust
agent as /init for ALL phases: exec-via-vsock, fork-correctness (reseed, clock,
signal, network, volume), workspace transfer, SDK example, snapshot distribution,
and the jailer phase. These confirm the Rust agent is the live production path.
