# Prepare-time restore: move the snapshot restore off the warm-claim hot path

Status: slices 1 and 2 implemented (default off, not yet canaried on prod); slice 3 designed.

## Why

A warm claim currently pays the whole microVM restore while a tenant waits. Measured on
prod (`mitos-kvm-1`), one matched claim, controller stage log against the husk stub's own:

| stage | ms |
|---|---|
| controller `activate_rpc` | 129.7 |
| husk-reported activate total | 112.8 |
| ... `guest_ready` | 40.6 |
| ... `egress_filter` | 29.6 |
| ... `vmstate_restore` | 22.9 |
| ... `handshake` | 17.2 |
| ... `resume` | 2.4 |
| RPC + mTLS overhead between the two | ~17 |

Under sustained load the same stages mean about 62 ms in total, so treat the numbers
above as one cold-ish sample and the ordering, not the absolute values, as the signal.

Everything except `handshake` is work the pod could have done while it sat dormant with
no tenant attached. `guest_ready` is largely demand fault-in of guest RAM under the lazy
UFFD restore (see `bench/results/2026-07-09-lazy-livecow-restore.md`), and it is also why
the first `run_code` in a fresh sandbox costs ~105 ms against ~35 ms warm: the ipykernel
is already running in the snapshot (`warmKernel: true`), but its pages are not resident.

Time to Interactive on the hosted API is 307.7 ms P50, of which create is ~196 ms and the
first exec ~118 ms (`bench/results/2026-07-10-tti-hosted.md`). This is the single largest
lever on both halves.

## What is actually claim-specific

Reading `activateInstance` (`internal/husk/multivm.go`), only three things depend on the
claim:

1. the tenant's netfilter policy (`req.Egress`, `req.Allow`, `req.AllowCIDRs`,
   `req.Inbound`, `req.BlockNetwork`);
2. the fork-correctness handshake, which carries fresh entropy, the clock step, the
   guest's network re-addressing, and the tenant's secrets (`notifyReq`);
3. `onActivated` (serving the sandbox API with the claim's token).

Everything else is a function of the pod and its pool template:

- the tap name is `netconf.DeriveTapName(guestIP)`, and the in-pod guest IP is the FIXED
  constant `10.200.0.2` (`huskGuestIP`, `internal/controller/sandboxclaim_controller.go`);
- the snapshot dir and its expected digest are already known at Prepare
  (`PrepareSnapshotDir` / `PrepareExpectedDigest`), which is why Prepare already
  pre-pays the content-addressed verify;
- the rootfs CoW clone is per-pod;
- the live-cow write-protect arm already happens in `prepareInstance`.

So the restore itself (`setLiveCowMemSource` -> `LoadSnapshot` -> `PatchDrive` ->
`Resume` -> guest-ready) needs nothing from the claim except a tap to bind the baked NIC
to, and the tap name is knowable at Prepare.

## Target

Prepare (dormant, no tenant):

1. ensure the tap exists and install a DEFAULT-DENY netfilter policy on it (a dormant VM
   must reach nothing, fail closed);
2. `setLiveCowMemSource`, `LoadSnapshotWithOverrides` (paused), `PatchDrive` to the CoW
   clone, `Resume`;
3. wait for the guest agent (this is where the demand fault-in is paid);
4. optionally prefault the run_code kernel's working set by running one inert cell, the
   same trick the template build uses (`warmKernelCode`).

Activate (claim):

1. replace the netfilter policy with the tenant's, atomically, on the tap that already
   exists (one `nft -f -`, not the tap create plus policy);
2. the fork-correctness handshake (entropy, clock, network, secrets);
3. `onActivated`.

Expected activate: roughly `nft` + `handshake` + `serve_api`, i.e. ~30 ms against 62 to
113 ms today, plus the first `run_code` arriving warm.

## Guards, in order of how badly they bite

- **Snapshot identity.** If `req.SnapshotDir` is not the dir Prepare restored, the pod
  has already restored the wrong image. FAIL CLOSED with a named error; never silently
  reload, because a resumed VM cannot be reloaded.
- **Fork children.** A co-located fork child restores a node-local fork snapshot through
  `spawnVM` -> `prepareInstance` -> `activateInstance` with `req.ForkSnapshot` set. It
  must keep the current path exactly. Prepare-time restore applies ONLY to
  `defaultVMID` with `PrepareSnapshotDir` set and no fork snapshot.
- **Dormant egress.** Between the restore and the claim, a live guest runs behind a tap.
  It must be default-deny for that whole window, and the claim must be able to WIDEN it
  atomically without a window where no policy is installed. `nft -f -` replaces the
  ruleset in one transaction, which is the property we need.
- **Reseed.** The dormant guest holds the snapshot's CRNG until the claim's
  `NotifyForked`. It serves no tenant before then, and the handshake still gates Ready
  fail-closed. The dormancy window only makes the clock step larger, and the step is
  absolute.
- **Teardown.** `Close` must remove a tap created at Prepare, not only one created at
  Activate (`inst.activeTap` is already the seam).
- **Memory.** A restored, resumed, prefaulted dormant VM holds its working set resident:
  about 72 MiB per sandbox measured on the lazy path, against ~0 for a dormant VMM that
  never loaded. At `warm.min: 8` that is roughly 576 MiB per node. The husk pod memory
  request is already sized worst-case (`guestRAM * (1 + reserved forks)`), so this fits,
  but it must be stated and watched.
- **Rollout.** Default OFF behind a stub flag. Canary ONE dormant pod (delete a single
  dormant husk pod, check `restartCount=0`, claim it, compare stages) before recycling
  the pool. `git merge-base --is-ancestor <deployed-tag> HEAD` before building anything
  for prod.

## Slices

1. **Tap at Prepare.** DONE, default off. `applyEgressFilter` is split into
   `ensureEgressLink` (forwarding, link delete, `ip -batch`) and `applyEgressPolicy` (one
   atomic `nft -f -`), and the composed call still runs exactly the same commands in the
   same order (pinned by `TestApplyEgressFilterIsTheTwoHalves`). Behind
   `--husk-prepare-egress-link` the pod ensures the tap with a default-deny policy while
   dormant, and the claim installs the tenant policy on it: exactly one command on the hot
   path (`TestPrepareBringsTheTapUpDormantAndActivateOnlyInstallsThePolicy`). A rejected
   claim policy tears the tap down and clears the prepared marker, so the retry rebuilds
   the link rather than installing a policy on a tap that no longer exists
   (`TestAFailedClaimPolicyRebuildsTheLinkOnRetry`). Worth roughly 20 ms, and a
   prerequisite for slice 2 because Firecracker requires the tap to exist at restore time.
   Requires `--multi-vm-fork`; the controller refuses the combination at startup and the pod
   builder refuses it again. The slice-2 KVM gate exercises this link bring-up end to end
   on a real Firecracker (the restore binds the baked NIC to the tap Prepare created), so
   the dormant-tap half is KVM-covered; the default-deny policy CONTENT on real nftables
   is still pinned only by the unit-level command tests. Still to do: measure it on prod
   behind a one-pod canary. Until then, the threat model does NOT call this path verified.
2. **Restore at Prepare.** DONE, default off. Behind `--husk-prepare-restore` (which
   requires `--husk-prepare-egress-link`), `prepareRestoreDefaultVM` runs
   `setLiveCowMemSource` / `LoadSnapshotWithOverrides` / `PatchDrive` / `Resume` /
   guest-ready for the DEFAULT VM after the tap is up, closing the ready conn rather than
   holding it across dormancy. Activate takes a fast path when `inst.preRestored`: it
   skips the restore, re-dials the already-running guest, and pays only the handshake via
   the shared `activateHandshakeAndServe`. FAIL CLOSED on a snapshot-dir mismatch (a
   resumed guest cannot be reloaded). Co-located fork children and the pre-warm child are
   never pre-restored. Pinned by `TestPrepareRestoresAndResumesTheGuestWhileDormant`,
   `TestPrepareRestoreIsOptIn`, `TestActivateFailsClosedOnASnapshotMismatchAfterPrepareRestore`,
   `TestBuildHuskPodThreadsPrepareRestore`. KVM gate: the firecracker-test step
   "Prepare-time restore, dormant resume then fast-path activate and fail-closed
   mismatch" proves, against a real Firecracker and the engine-built networked
   template, that the dormant restore happens at Prepare (stderr marker), that
   activate skips the load (vmstate_restore ~0) and a real exec works through the
   pre-restored guest, and that a mismatched snapshot dir is refused with no side
   effects (the same stub then activates the right snapshot fine). Still to do: a
   one-pod prod canary before it is called verified.
3. **Prefault the kernel.** Run the inert warm cell at Prepare so the first tenant
   `run_code` does not fault the ipykernel's pages in.

Each slice ships with its own KVM gate in `firecracker-test`, because none of this is
observable without `/dev/kvm` and the mitos-patched Firecracker.

## Not in scope

`mark_pod_claimed` (16.5 ms) is the optimistic-lock mutual exclusion that stops two
claims taking one VM. Taking it off the hot path needs a reservation protocol, not a
reordering, and is tracked separately.
