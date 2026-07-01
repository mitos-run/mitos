# Host kernel prerequisites for KVM nodes

This is the host/kernel checklist every Mitos KVM node must satisfy, the two
failure modes a rescue/minimal kernel causes, and the one-shot verify. It is the
companion to `prerequisites.md` (the distro-neutral node/install checklist) and
`talos-hetzner.md` (one concrete realization). These were learned the hard way
on a clean-room two-node bare-metal install; a minimal or rescue kernel can cost
a new operator hours, so check this FIRST.

## The checklist

Every KVM node MUST provide all of the following:

| Requirement | Why | One-line check |
| --- | --- | --- |
| `/dev/kvm` present and usable | Firecracker boots each microVM through it | `ls -l /dev/kvm` |
| `nf_tables` kernel support | The husk egress isolation filter is nftables-based AND kube-proxy needs it | `nft list tables` |
| `vhost_vsock` module | The guest agent talks to forkd over vsock (exec, files, env) | `lsmod \| grep vhost_vsock` |
| `tun` module | forkd creates a per-sandbox tap for guest networking | `lsmod \| grep '^tun'` |
| containerd on a REAL filesystem | The overlay snapshotter cannot stack on another overlay/tmpfs | root fs is ext4/xfs, not overlay/tmpfs |

A rescue/recovery environment (minimal kernel, ramdisk root) typically FAILS the
`nf_tables` and the real-filesystem requirements. Install a real OS on the node.

The following is REQUIRED only for the snapshot-resume performance path; the node
still serves sandboxes without it, just on the slower 4 KiB file-backed restore:

| Requirement | Why | One-line check |
| --- | --- | --- |
| `CONFIG_USERFAULTFD=y` kernel | Hugepage-backed restore and snapshot-resume prefetch both ride userfaultfd; Firecracker refuses to restore a hugetlbfs snapshot without it | `mitos doctor` (userfaultfd check) |
| 2 MiB hugepage pool reserved | `huge_pages: 2M` templates need free hugepages to restore into | `grep HugePages_Total /proc/meminfo` |

## userfaultfd: required for hugepages and prefetch

The snapshot-resume performance work backs guest memory with 2 MiB
hugepages and preloads a captured hot-page working set before resume. Both ride
the kernel `userfaultfd(2)` facility: Firecracker hands the restore's guest
memory to an external handler over a unix socket, and that handler fills pages
from the snapshot. Firecracker REFUSES to restore a hugetlbfs-backed snapshot
through the plain file-mapping backend ("Cannot restore hugetlbfs backed snapshot
by mapping the memory file. Please use uffd."), so without userfaultfd the
hugepage and prefetch path simply cannot run.

A minimal or rescue kernel is frequently built WITHOUT `CONFIG_USERFAULTFD`
(the Hetzner rescue kernel, for example, has `# CONFIG_USERFAULTFD is not set`),
in which case `userfaultfd(2)` returns ENOSYS and Firecracker fails the restore
with "Failed to UFFD object: System error". Stock distro kernels (Debian, Ubuntu)
enable `CONFIG_USERFAULTFD=y`. To reserve the hugepage pool at runtime:
`echo 2048 > /proc/sys/vm/nr_hugepages` (2048 x 2 MiB = 4 GiB), or persist it via
`vm.nr_hugepages` in sysctl / the kernel cmdline. This is a node prerequisite,
not part of a snapshot's portable identity: a hugepage snapshot is self-describing
(its manifest records the backing) so any node knows it must restore via uffd, but
that node still needs a userfaultfd kernel and a hugepage pool to do so. Without
either, fall back to the default 4 KiB templates (omit `huge_pages`).

## The two failure modes a minimal kernel causes

A rescue or minimal kernel fails in two distinct, easy-to-miss ways. Each costs
hours because the symptom is far from the cause.

### 1. No `nf_tables` breaks egress isolation AND kube-proxy

`nf_tables` is the single module whose absence breaks two things at once:

- **husk egress isolation cannot run.** The per-sandbox egress allowlist is an
  nftables ruleset. Without `nf_tables` the security control silently has nothing
  to program, so a sandbox that should be network-isolated is not.
- **kube-proxy crash-loops.** Modern kube-proxy programs service routing through
  nftables (or iptables-nft). On a kernel without `nf_tables` it fails to install
  its rules and crash-loops, which takes cluster service networking down with it.

So a single missing module presents as "my security feature does nothing" and
"my cluster networking is broken" simultaneously, with no obvious shared cause.

### 2. overlay-on-overlay breaks containerd

containerd's default `overlayfs` snapshotter cannot stack an overlay mount on top
of another overlay (or on tmpfs/ramfs). A rescue ramdisk root, or any node whose
root filesystem is itself an overlay, fails the snapshotter at image-pull time
with an opaque mount error. The fix is a real block-backed root filesystem
(ext4/xfs); it is not a containerd configuration tweak.

### 3. data dir too small, or on the wrong disk

forkd's `--data-dir` (default `/var/lib/mitos`) holds template rootfs images,
snapshots, and the per-VM jailer chroot base. If it sits on an undersized or
full filesystem, the template build fails with `no space left on device`, the
husk warm pool never fills, and every sandbox create/fork times out. The SDK
sees only a generic `fork_unavailable`, so the symptom is far from the cause.

The common trap is a dedicated data disk pointed at the wrong device. On a bare
metal box that was reset but whose old partition table was not wiped, Talos (or
any partitioner) will not repartition a disk with no free space, so a
`machine.disks` mount can silently land `/var/lib/mitos` on a tiny leftover
partition (we hit a 100MB one) while the real disk sits unused. Keep the data
dir on the system `/var` unless you deliberately add a wiped, correctly chosen
data disk. Budget tens of GB per template plus the warm-pool snapshots. `mitos
doctor` fails the `data-dir-space` check when free space is below the minimum.

## One-shot verify

Run `mitos doctor` on each candidate node (or as an in-cluster Job). It checks
`/dev/kvm`, the required kernel modules, the staged guest kernel, free space on
the data dir, the minted PKI secrets, the image pull secret, and the privileged
PodSecurity label, and prints an actionable remediation per failing check:

```bash
mitos doctor                 # node + cluster preflight, exits non-zero on any failure
mitos doctor -n mitos        # target a specific install namespace
```

`mitos doctor` is the automated form of this checklist. For a quick shell-only
check before the binary is on the node, the script in `prerequisites.md`
(`## One-shot verify`) covers the node-local kernel requirements.

## See also

- `prerequisites.md`: distro-neutral node + install checklist and the running-on
  matrix.
- `talos-hetzner.md`: end-to-end Talos + Hetzner AX runbook, including the
  secrets-backup step.
- `docs/threat-model.md`: why the egress filter and the privileged namespace
  matter.
