# KVM node prerequisites (any Kubernetes distribution)

mitos is a standard Kubernetes operator: the controller, the forkd DaemonSet, the
husk pods, and the device plugin run on any conformant cluster. The constraints
are on the **nodes that run sandboxes** (the KVM nodes) and on the **install
namespace**, NOT on the Kubernetes distribution. This page is the distro-neutral
checklist; `talos-hetzner.md` is one concrete realization.

These were learned the hard way on a clean-room bare-metal install (a rescue /
minimal-kernel host fails several of them); see
`docs/superpowers/plans/2026-06-18-deployment-ux-findings.md`.

## Every KVM node MUST provide

| Requirement | Why | Verify |
| --- | --- | --- |
| `/dev/kvm` present + usable | Firecracker boots each microVM through it | `ls -l /dev/kvm` |
| CPU virtualization (VT-x/AMD-V) | KVM needs it; Hetzner Cloud CX/CPX and gVisor-style sandboxes do NOT expose it | `egrep -c 'vmx\|svm' /proc/cpuinfo` (> 0) |
| `nf_tables` kernel support | The husk **egress isolation** filter is nftables-based; without it the security feature cannot run AND kube-proxy crash-loops | `nft list tables` succeeds |
| `vhost_vsock` module | The guest agent talks to forkd over vsock | `lsmod \| grep vhost_vsock` or it is built in |
| `tun` module | forkd creates a per-sandbox tap for guest networking | `lsmod \| grep '^tun'` |
| containerd snapshotter on a REAL filesystem | overlay-on-overlay (e.g. a rescue ramdisk root) fails the overlay snapshotter | root fs is ext4/xfs, not overlay/tmpfs |
| A writable data dir at `--data-dir` (default `/var/lib/mitos`) | forkd stores template snapshots + CoW forks here; it is root-owned | a real partition/dir, writable by root |

A rescue/recovery environment typically FAILS the `nf_tables` and overlay
requirements (minimal kernel, ramdisk root). Install a real OS on the node.

## One-shot verify (run on each candidate KVM node)

```bash
#!/bin/sh
# mitos KVM node preflight. Exit non-zero on any failure.
fail=0
[ -e /dev/kvm ] && echo "ok: /dev/kvm" || { echo "FAIL: /dev/kvm missing"; fail=1; }
[ "$(egrep -c 'vmx|svm' /proc/cpuinfo)" -gt 0 ] && echo "ok: CPU virt" || { echo "FAIL: no VT-x/AMD-V"; fail=1; }
nft list tables >/dev/null 2>&1 && echo "ok: nf_tables" || { echo "FAIL: nf_tables unavailable (husk egress + kube-proxy need it)"; fail=1; }
( lsmod | grep -q vhost_vsock || modprobe vhost_vsock 2>/dev/null ) && echo "ok: vhost_vsock" || { echo "FAIL: vhost_vsock"; fail=1; }
( lsmod | grep -q '^tun' || modprobe tun 2>/dev/null ) && echo "ok: tun" || { echo "FAIL: tun"; fail=1; }
rootfs=$(stat -f -c %T / 2>/dev/null); case "$rootfs" in overlayfs|tmpfs|ramfs) echo "FAIL: root fs is $rootfs (containerd overlay snapshotter needs a real fs)"; fail=1;; *) echo "ok: root fs ($rootfs)";; esac
exit $fail
```

## Node preparation per distribution

The node must load the kernel modules and label itself. How you do that is
distro-specific; the operator side is identical.

- **Talos** (the documented bare-metal target): the `deploy/talos/worker-kvm.yaml`
  MachineConfig patch loads `kvm`/`kvm_intel`/`kvm_amd`/`vhost_vsock`/`tun`, sets
  `nodeLabels: {mitos.run/kvm: "true"}`, and mounts a data partition at
  `/var/lib/mitos`. See `talos-hetzner.md`.
- **k3s / kubeadm / generic**: ensure the modules load at boot
  (`/etc/modules-load.d/mitos.conf` with `kvm_intel`/`kvm_amd`/`vhost_vsock`/`tun`),
  label the node `kubectl label node <n> mitos.run/kvm=true`, and provide a
  writable `/var/lib/mitos` (a mounted disk or a directory on the real root fs).
- **Managed metal (EKS metal pools, GKE bare-metal)**: same; use a node bootstrap
  script / DaemonSet to load modules + label.

The KVM device plugin advertises `mitos.run/kvm` only where `/dev/kvm` exists, so
forkd and husk pods schedule only on prepared nodes regardless of distro.

## Install namespace (PodSecurity)

forkd, the husk pods, and the device plugin are privileged with hostPath mounts,
so the install namespace MUST carry `pod-security.kubernetes.io/enforce:
privileged`. Helm cannot create-and-label its own release namespace, so create it
first (see the chart README Install section). This is distro-neutral: any cluster
with PodSecurity admission (the default in modern Kubernetes) needs it.

## Secrets backup (Talos and any self-managed control plane)

If you run your own control plane, BACK UP the cluster admin credentials the
moment you create the cluster:

- **Talos**: `_out/talosconfig` + `secrets.yaml` from `talosctl gen config`. They
  are shown once. Without them you cannot add a node, upgrade, or manage the
  cluster, and recovery means rebuilding from scratch. Store them in a secret
  manager, not just on one workstation.
- **kubeadm**: the CA keys under `/etc/kubernetes/pki` and a working admin
  kubeconfig.

## Quick health check after install

```bash
kubectl get pods -n mitos          # controller, forkd (per KVM node), device-plugin, kernel-stage all Running
kubectl get nodes -L mitos.run/kvm # KVM nodes labeled true
```

A `mitos doctor` preflight that automates the node + install checks above and
prints remediation is tracked in #174.
