# ADR 0008: forkd runs non-privileged with the jailer enabled

Status: accepted (2026-06-25)
Issue: #352 (reduce forkd's privileged TCB). Related: docs/threat-model.md
section 0 (the build-vs-run split, the per-axis tally), section 1 (jailer row),
section 3 (forkd capability-minimization + blast-radius rows); ADR 0003 (the KVM
device-plugin PSA exception), ADR 0005 (raw-forkd is not for untrusted
multi-tenant), the closed jailer-in-pod design (PR #96, for husk). Code:
`cmd/forkd/jailer.go` (`forkdRequiredCapabilities`), `cmd/forkd/mount_linux.go`
(`prepareChrootMount`), `cmd/forkd/main.go`, `deploy/daemon/daemonset.yaml`,
`deploy/charts/mitos/templates/forkd-daemonset.yaml`.

## Context

The tenant EXECUTION surface is already unprivileged: husk pods run
`privileged: false`, drop ALL capabilities and add only `NET_ADMIN`, seccomp
`RuntimeDefault`, and satisfy PSA-restricted minus three documented exceptions
(ADR 0003). The remaining privileged host component was the forkd DaemonSet,
which shipped with `securityContext.privileged: true` and the per-VM jailer
DISABLED, even though the jailer code and its minimal capability set already
existed (`internal/firecracker/jailer.go`, `cmd/forkd/jailer.go`). That was the
worst shape for the most security-sensitive host component: privileged AND
long-lived. A guest escape from a build VM or a raw-forkd fork landed as root in
a privileged container with `/dev/kvm` and a hostPath to the node data dir:
materially full node compromise (the old "forkd capability minimization" row,
status open/high).

The jailer was left off because of the `pivot_root(2)` problem inside a pod: the
jailer pivot_roots into `<chroot-base>/firecracker/<vm-id>/root`, and
`pivot_root` requires the new root to be a mount point whose parent mount is not
shared. A pod's container rootfs is commonly mounted shared, so a plain directory
fails. The husk jailer-in-pod work (PR #96) solved this with a one-time
bind-mount-self + `MS_PRIVATE` setup needing `CAP_SYS_ADMIN`, but it was DECLINED
for husk because pushing `CAP_SYS_ADMIN` into EVERY husk pod breaks
PSA-restricted (ADR 0006 reasoning; one VM per pod already gives the per-VM
boundary, so husk needs no in-pod jailer).

For forkd the calculus is the opposite. forkd is the privileged BUILDER, one per
node, not a per-tenant pod, and is not subject to PSA-restricted. Applying the
same mount technique to forkd lets it drop `privileged: true` for an explicit,
minimal capability set: a strict improvement, not a regression.

## Decision

Run forkd NON-privileged with the per-VM jailer ENABLED in the shipped DaemonSet
and Helm chart.

1. **Enable the jailer.** The shipped DaemonSet passes `--jailer`,
   `--chroot-base=/var/lib/mitos/jailer` (under `--data-dir` so snapshot/kernel/
   rootfs files hard-link into each chroot on one filesystem), and
   `--uid-range=64000-64999`. Every build and raw-forkd VMM now runs under a
   dedicated throwaway uid in a per-VM chroot.

2. **Drop privileged for the explicit capability set.** forkd runs
   `privileged: false`, `allowPrivilegeEscalation: false`,
   `seccompProfile: RuntimeDefault`, `capabilities.drop: [ALL]`, adding back ONLY
   `forkdRequiredCapabilities` (`cmd/forkd/jailer.go`, the single source of
   truth): `SYS_ADMIN`, `CHOWN`, `SETUID`, `SETGID`, `MKNOD` (the jailer set),
   plus `NET_ADMIN` (the build-time placeholder tap, `internal/fork/engine.go`,
   when `--enable-networking` is set, as the shipped manifest does). `NET_ADMIN`
   is scoped to forkd's own pod netns (forkd is not hostNetwork), exactly like
   the husk pod's `NET_ADMIN`.

3. **Get devices from the device plugin, not a privileged hostPath.** `/dev/kvm`
   and `/dev/net/tun` are injected by the KVM device plugin (`mitos.run/kvm`),
   which sets the device cgroup allow the kubelet otherwise denies a
   non-privileged container. The privileged `/dev/kvm` hostPath CharDevice is
   removed. This adds a scheduling dependency: forkd stays Pending until the
   device plugin advertises `mitos.run/kvm` on the node (the plugin runs on every
   node and tolerates the dedicated taint, so this self-heals).

4. **Make pivot_root work in the pod.** forkd bind-mounts `--chroot-base` onto
   itself and marks it `MS_PRIVATE` at startup, in its own mount namespace,
   before the engine launches any jailed VM (`cmd/forkd/prepareChrootMount`,
   `CAP_SYS_ADMIN`). This is the same technique PR #96 designed for husk, applied
   to the one builder pod instead of every tenant pod.

This splits the privileged BUILDER role (forkd, the audited non-privileged
capability set) from the unprivileged SERVING role (husk pods, the tenant
execution surface), which is the shape #352 asked for: the residual host
privilege is a minimal, audited, single-per-node builder, not a
privileged-and-long-lived daemon.

## Consequences

- forkd no longer runs `privileged: true` in steady state. The "forkd capability
  minimization" and "blast radius" threat-model rows flip from open/high to
  mitigated; the privileged-container reason for raw-forkd-not-multi-tenant
  (ADR 0005) is closed, leaving node-flat snapshots (ADR 0004) as the remaining
  reason.
- A VMM escape from a build or raw-forkd VM lands as a throwaway jailed uid in a
  per-VM chroot inside a non-privileged container, not forkd's root.
- **Residual, stated honestly.** forkd is still uid 0 and still holds
  `CAP_SYS_ADMIN` (for the chroot-base mount setup and the jailer) and a hostPath
  to the node data dir. `CAP_SYS_ADMIN` is a powerful capability; an escape that
  ALSO defeats the jailed uid and the `CAP_SYS_ADMIN`/seccomp boundary is still a
  serious node event, just no longer trivial full-node root. Reducing forkd
  below `CAP_SYS_ADMIN` would require removing its need for in-pod mounts and the
  jailer's cgroup/namespace setup, which is not currently possible.
- The conformance suite (`cmd/forkd`, `TestShippedDaemonSet*`) asserts the
  shipped manifest stays non-privileged with exactly the audited cap set, the
  jailer flags, and the device-plugin request, in the darwin unit run. The
  kernel ENFORCING the bounding-set and uid drop is asserted on the KVM runner
  (the CI host is root and cannot observe the bounding-set shrink; issue #2
  Task 5).
- The development direct-exec path (omit `--jailer`) and the standalone
  sandbox-server remain unjailed and are flagged loudly, unchanged.

## Alternatives considered

- **Keep forkd privileged, accept it as the builder.** Rejected: privileged AND
  long-lived is the worst shape for the most security-sensitive host component,
  and "the execution surface is unprivileged" cannot honestly precede "the host
  blast radius is minimal" while a steady-state `privileged: true` remains.
- **jailer-in-pod for husk (PR #96).** Rejected for husk (breaks PSA-restricted
  by pushing `CAP_SYS_ADMIN` into every tenant pod); the technique is reused here
  for the single builder pod, where PSA-restricted does not apply.
- **A separate, time-boxed builder Job instead of a DaemonSet.** Considered for
  "not a long-lived privileged daemon"; deferred because template builds are
  on-demand and per-node, and forkd also serves CAS distribution and raw-forkd,
  so a per-build Job would fragment the node-local artifact ownership. The
  capability minimization above achieves the "minimal, audited" goal without that
  restructuring; a time-boxed builder remains a possible future refinement.
