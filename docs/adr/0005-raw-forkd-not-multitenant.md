# ADR 0005: raw-forkd is not for untrusted multi-tenant

Status: accepted (2026-06-15)
Issue: #18 (W1 husk pods, husk-as-default), #30 (residual ADRs). Related:
docs/threat-model.md section 0 (the build-vs-run split, the per-axis tally),
section 1 (jailer row), section 2 (shared-rootfs row), section 3 (forkd
capability-minimization row), section 6 (fork-correctness reseed row); ADR 0008
(forkd is now non-privileged with the jailer enabled, #352); the code is
`cmd/forkd`, `internal/fork/engine.go`, `deploy/daemon/daemonset.yaml`,
`internal/controller` (the `--enable-husk-pods` / `--enable-raw-forkd` gating);
docs/husk-pods.md.

## Update (2026-06-25, #352)

forkd is now NON-privileged with the per-VM jailer ENABLED in the shipped
DaemonSet and Helm chart (ADR 0008, docs/threat-model.md section 1 jailer row and
section 3 forkd capability-minimization row): `privileged: false`,
`allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`,
`capabilities.drop: [ALL]` adding back only the explicit builder set
(`SYS_ADMIN`, `CHOWN`, `SETUID`, `SETGID`, `MKNOD` plus `NET_ADMIN`), `/dev/kvm`
and `/dev/net/tun` from the device plugin (`mitos.run/kvm`), and the jailer flags
(`--jailer`/`--chroot-base`/`--uid-range`) on by default. So the
privileged-container and jailer-disabled reasons recorded in the historical
Context below are now CLOSED: a VMM escape from a raw-forkd VM lands as a
throwaway jailed uid in a per-VM chroot inside a non-privileged container, not as
forkd's root. raw-forkd REMAINS not for untrusted multi-tenant for the remaining
documented reason, node-flat snapshots (ADR 0004, docs/threat-model.md section 3
forkd capability-minimization row). The historical decision below, including the
shared-rootfs and fork-reseed hazards it lists, is preserved as recorded; see
docs/threat-model.md for the current per-row status of each.

## Context

Mitos has two per-sandbox execution engines:

- The HUSK pod (`cmd/husk-stub`): the default. One unprivileged, capability-
  dropped, PSA-restricted-minus-two pod per VM (ADR 0003).
- raw-forkd (`--enable-raw-forkd`): the older engine, where forkd itself forks a
  VM per claim. AS ORIGINALLY SHIPPED (before #352), forkd was a root DaemonSet
  pod that ran `privileged: true` with `/dev/kvm` as a hostPath and the jailer
  DISABLED in the shipped DaemonSet, so a guest escape from a raw-forkd VM landed
  as ROOT in a privileged container with `/dev/kvm` and a hostPath to the node
  data dir: materially full node compromise. That posture is the one the
  2026-06-25 update above CLOSED; forkd now runs non-privileged with the jailer
  enabled (ADR 0008, docs/threat-model.md section 3 forkd capability-minimization
  row).

The control gating moved: pod-native execution is now the DEFAULT (the controller
runs `--enable-husk-pods` by default; `--enable-raw-forkd` and `--mock` select
the fork-per-claim fallback) (docs/threat-model.md section 0; ROADMAP.md W1).
That gating change is what makes this decision recordable: raw-forkd is no longer
the default tenant surface, it is an opt-in fallback the operator must explicitly
enable.

raw-forkd carries several hazards the husk path has fixed or does not have:

- It ORIGINALLY ran `privileged: true` with the jailer disabled as shipped, so a
  VMM compromise was forkd's root; that reason is now CLOSED (forkd is
  non-privileged with the jailer enabled, #352, ADR 0008; docs/threat-model.md
  section 1 jailer row, section 3 forkd row).
- All forks of one template on a node share a SINGLE writable rootfs inode (a
  cross-fork filesystem read/write channel and corruption vector); the husk path
  fixed this with a per-pod reflink rootfs clone rebound via `PatchDrive`, the
  raw-forkd path did NOT (docs/threat-model.md section 2, status open/critical for
  raw-forkd).
- Fork-correctness is NOT fail-closed on raw-forkd: a guest that connected but
  silently did not reseed its CRNG serves duplicate keys/tokens/UUIDs across
  forks, because the reseed response is discarded; the husk path IS fail-closed
  (docs/threat-model.md section 6, docs/fork-correctness.md row 1, status
  open/critical for raw-forkd).

## Decision: the husk pod is the default tenant runner; raw-forkd is an opt-in privileged fallback, not for untrusted multi-tenant use

We record that raw-forkd is NOT a safe runner for untrusted multi-tenant
workloads, and that the husk pod is the engine to use for that case. Concretely:

- The DEFAULT is husk pods (`--enable-husk-pods` default on). raw-forkd is
  reachable only by explicitly setting `--enable-raw-forkd` (or `--mock`, the
  dev/no-KVM path, which implies it). The operator must opt INTO raw-forkd; it is
  not what ships enabled (docs/threat-model.md section 0; docs/husk-pods.md).
- raw-forkd is documented as the fallback engine for cases that do NOT need the
  untrusted-multi-tenant posture (single-tenant, trusted-workload, or dev/no-KVM
  use), or as a transitional path. Its remaining hazards (originally the
  privileged-no-jailer DaemonSet, since CLOSED by #352, plus the shared writable
  rootfs inode and non-fail-closed fork reseed where those are not yet closed on
  raw-forkd) are why it is excluded from the untrusted-multi-tenant recommendation
  (docs/threat-model.md section 0 must-fix-first set, sections 1, 2, 3, 6).
- forkd-the-BUILDER remains the per-node BUILDER regardless of engine: building a
  template snapshot needs `/dev/kvm` and the jailer, so forkd is the per-node
  builder even when husk pods are the runner. Since #352 forkd is NON-privileged
  (an explicit, audited builder capability set with the jailer enabled, ADR 0008),
  a hardened minimal builder rather than a privileged container. The build path
  runs once per node per template, not per sandbox, so that residual builder
  surface is amortized and confined to building, not to executing tenant code
  (docs/threat-model.md section 0, the per-axis tally). This ADR distinguishes
  forkd-the-builder (a smaller, amortized control-plane surface that stays) from
  raw-forkd-the-runner (the per-sandbox engine the husk default replaces).

## Why not keep raw-forkd as a co-equal runner

- On the privilege and capabilities axes the husk model is better: an
  unprivileged, drop-ALL, no-escalation, PSA-restricted-minus-two pod versus the
  non-privileged-but-uid-0, CAP_SYS_ADMIN-holding raw-forkd builder/runner
  (docs/threat-model.md section 0 per-axis tally; raw-forkd dropped
  `privileged: true` in #352 but still runs uid 0 with the builder capability
  set). Offering raw-forkd as a co-equal default would re-expose the worse surface
  as a silent option.
- raw-forkd's shared-rootfs and non-fail-closed-reseed hazards are
  cross-tenant-affecting (a fork reads/writes a sibling's filesystem; duplicate
  CRNG output across forks). These are not acceptable under an untrusted-multi-
  tenant claim, and fixing them on raw-forkd is tracked but not done
  (docs/threat-model.md sections 2 and 6 "required fix" notes).

## Consequences

- The honest claim is: the untrusted-multi-tenant tenant execution surface is the
  husk pod; raw-forkd is an opt-in fallback that is not for untrusted multi-tenant
  use. docs/compliance-claims.md and the README must not present raw-forkd as a
  safe multi-tenant runner.
- An operator who enables `--enable-raw-forkd` is opting into the documented
  open/critical and open/high hazards above; the gating makes that an explicit
  choice, not a default.
- Lifting raw-forkd to a safe runner is tracked, not abandoned. The jailer is now
  enabled in-pod and `privileged: true` is dropped (#352, ADR 0008;
  docs/threat-model.md section 1 and section 3); what remains is the per-fork
  rootfs clone + `PatchDrive` mirroring the husk fix (section 2) and the fork
  reseed fail-closed on raw-forkd as it is on husk (section 6). Until those close,
  the not-for-untrusted-multi-tenant statement stands on node-flat snapshots
  (ADR 0004) plus any of those still open on the raw-forkd path.
- forkd-the-builder's residual is accepted separately and is out of scope for this
  ADR. Since #352 it is non-privileged with an explicit, audited builder
  capability set (still uid 0 with CAP_SYS_ADMIN; ADR 0008); a builder redesign
  that removes even that residual privilege is tracked but not planned here
  (docs/threat-model.md section 0 accepted-residuals list).
