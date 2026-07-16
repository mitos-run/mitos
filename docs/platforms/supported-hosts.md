# Supported hosts

This page is the host-support matrix: which machines can run mitos sandboxes,
which can only run the control plane, and which cannot run mitos at all. It is
the decision companion to `host-prerequisites.md` (the authoritative per-node
kernel checklist) and to the concrete realizations in `talos-hetzner.md` and
`k3s-quickstart.md`.

The rule is simple and mechanical: a machine is a supported **KVM worker** if it
passes `mitos doctor`. That command is the per-requirement hard gate; it exits
non-zero and prints an actionable remediation for every requirement it finds
unmet, so you never have to guess why a node was rejected.

## Two kinds of node

mitos splits cleanly into a control plane and KVM workers, and they have
different requirements.

| Node role | Runs | KVM required | Requirement source |
| --- | --- | --- | --- |
| Control plane | controller, gateway, facade, console (Deployments) | No | ordinary Kubernetes node |
| KVM worker | forkd (DaemonSet) and the husk pods that boot microVMs | Yes | `host-prerequisites.md` + `mitos doctor` |

You can run the control plane on any Kubernetes node (including a managed cloud
control plane). Only the KVM workers, the nodes that actually boot and fork
microVMs, carry the kernel and device requirements below.

## What a KVM worker must provide

These are summarized from `host-prerequisites.md`, which remains the source of
truth. Each row names the `mitos doctor` check that gates it.

| Requirement | `mitos doctor` check | Needed for |
| --- | --- | --- |
| `/dev/kvm` present and usable | `kvm-device` | booting every microVM |
| `kvm_intel` / `kvm_amd` loaded | `kernel-module-*` | KVM acceleration |
| `vhost_vsock` module | `kernel-module-vhost_vsock` | guest agent exec/files/env over vsock |
| `tun` module | `kernel-module-tun` | per-sandbox tap networking |
| `nf_tables` support | (see prerequisites) | husk egress isolation and kube-proxy |
| containerd on a real filesystem | (see prerequisites) | the overlay snapshotter cannot stack on overlay/tmpfs |
| adequate free space on `--data-dir` | `data-dir-space` | template rootfs images, snapshots, jailer chroots |
| `CONFIG_USERFAULTFD=y` (perf path) | `userfaultfd` | hugepage-backed restore and snapshot-resume prefetch |

The userfaultfd row is required only for the snapshot-resume performance path; a
node without it still serves sandboxes on the slower 4 KiB file-backed restore.

## The matrix

"Supported" below means "can be a KVM worker". Anything can host the control
plane.

| Host | KVM worker | Notes |
| --- | --- | --- |
| Bare metal with VT-x/AMD-V exposed (for example Hetzner AX) | Yes | The first-class target. See `talos-hetzner.md` and `k3s-quickstart.md`. |
| Cloud instances that expose `/dev/kvm` (GCP nested virtualization, `*.metal` instance types on AWS/Azure) | Yes, if `mitos doctor` passes | Bare-metal or nested-virt-enabled node pools only; the managed control plane is unaffected. |
| Standard cloud VMs without nested virtualization (for example Hetzner Cloud) | No | No `/dev/kvm`. Use them for the control plane and attach a bare-metal KVM node pool for workers. |
| `kind` / CI with the mock engine | No | The mock engine (`KVMAvailable=false`) proves object-level behavior only; it never boots or forks a real microVM. Not a runtime host. |
| Rescue / minimal / ramdisk-root kernel | No | Typically fails `nf_tables` and the real-filesystem requirement; a minimal kernel also often ships `# CONFIG_USERFAULTFD is not set`. Install a real OS. See `host-prerequisites.md`. |

## Distro and kernel notes

- Stock Debian and Ubuntu kernels enable `CONFIG_USERFAULTFD=y`; the Hetzner
  rescue kernel and many minimal kernels do not, so the hugepage and prefetch
  path cannot run there (fall back to 4 KiB templates).
- A rescue/recovery environment fails two requirements at once (`nf_tables` and
  a real root filesystem), each with a symptom far from its cause. Boot a real
  installed OS before running mitos.

## The one gate to run

On every prospective KVM worker:

```bash
mitos doctor
```

It runs all of the checks in the table above against the live host and exits
non-zero if any fails, with a remediation line per failing check. Passing
`mitos doctor` on a node is the definition of that node being supported; there
is no separate approval list to maintain.
