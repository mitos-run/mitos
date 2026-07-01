# Talos machine configs for KVM workers

forkd boots Firecracker microVMs, which need a real `/dev/kvm`. These configs
turn a stock Talos node into a KVM-capable forkd worker.

## Hardware requirement: Hetzner AX dedicated, not Hetzner Cloud

Firecracker requires hardware virtualization exposed to the OS as `/dev/kvm`.

- **Hetzner AX dedicated servers** (bare metal, AX41/AX52/AX102 and similar)
  expose the CPU virtualization extensions directly, so `/dev/kvm` is present
  and Firecracker runs. This is the supported target.
- **Hetzner Cloud** instances run on a hypervisor that does NOT expose nested
  `/dev/kvm`, and Hetzner Cloud uses gVisor-style isolation rather than nested
  KVM. Firecracker cannot start there. Do not try to run forkd on Hetzner
  Cloud; the DaemonSet will never become Ready.

## Files

- `worker-kvm.yaml`: a patch over the generated worker config. Loads the KVM /
  vsock / tun kernel modules and labels the node `mitos.run/kvm=true`. forkd's
  data dir `/var/lib/mitos` defaults to the node's `/var` (the ephemeral
  partition); a dedicated data disk is optional and commented out in the patch.
- `controlplane.yaml`: a tiny optional patch over the generated control-plane
  config (just a role label). A stock Talos control plane needs no KVM.

## Why each piece exists

- **Kernel modules** (`machine.kernel.modules`): `kvm`, `kvm_intel`, `kvm_amd`
  give Firecracker `/dev/kvm` (the vendor module for the absent CPU just fails
  to load); `vhost_vsock` is the host side of the guest-agent vsock transport;
  `tun` backs the per-sandbox tap devices forkd creates.
- **Node label** (`machine.nodeLabels: mitos.run/kvm: "true"`): the forkd
  DaemonSet in `deploy/daemon/daemonset.yaml` has `nodeSelector
  mitos.run/kvm: "true"`, so forkd only schedules onto labeled KVM workers.
- **Data dir** `/var/lib/mitos`: forkd's `--data-dir`. Snapshots, rootfs
  images, and the jailer chroot base all live here and must share one filesystem
  so forkd can hard-link them into each per-VM chroot (forkd refuses to start if
  they straddle filesystems). By default this is a directory on the node's
  `/var` (the Talos ephemeral partition, which normally spans the whole system
  disk), so nothing extra is needed. Size the system disk for your template set
  (tens of GB per template plus warm-pool snapshots).
  A dedicated data disk is optional (the commented `machine.disks` block). If
  you use one, point `device` at the FREE data disk, NOT the OS disk: verify
  with `talosctl -n <node> get disks` (or `lsblk`), and wipe any leftover
  partition table first. Talos will not repartition a disk with no free space,
  so a stale partition table can silently mount `/var/lib/mitos` on a tiny
  leftover partition; the template build then fails with `no space left on
  device` and every fork times out. `mitos doctor` flags an undersized data dir.

## Usage

Generate a base config for the cluster, then apply the patches.

```bash
# 1. Generate the base configs (writes controlplane.yaml, worker.yaml, talosconfig).
talosctl gen config my-cluster https://<CONTROL_PLANE_IP>:6443 --output-dir _out

# 2. Merge the KVM worker patch onto the generated worker config.
talosctl machineconfig patch _out/worker.yaml \
  --patch @deploy/talos/worker-kvm.yaml \
  -o _out/worker-kvm.yaml

# 3. (optional) Merge the control-plane label patch.
talosctl machineconfig patch _out/controlplane.yaml \
  --patch @deploy/talos/controlplane.yaml \
  -o _out/controlplane-merged.yaml

# 4. Validate the merged worker config for bare metal.
talosctl validate --config _out/worker-kvm.yaml --mode metal

# 5. Apply to each node.
talosctl apply-config --insecure --nodes <WORKER_IP> --file _out/worker-kvm.yaml
talosctl apply-config --insecure --nodes <CONTROL_PLANE_IP> --file _out/controlplane-merged.yaml
```

Alternatively, fold the patch in at generation time:

```bash
talosctl gen config my-cluster https://<CONTROL_PLANE_IP>:6443 \
  --config-patch-worker @deploy/talos/worker-kvm.yaml \
  --output-dir _out
```

After the workers join and forkd is scheduled, install the control plane with
`kubectl apply -k deploy/` (see `deploy/kustomization.yaml`).

## CI

The `talos-validate` job in `.github/workflows/ci.yaml` installs `talosctl`,
generates a throwaway base config, applies `worker-kvm.yaml` over the generated
worker config, and runs `talosctl validate --mode metal` on the merged result.
The patch is validated against the real Talos schema for the merged document, so
a malformed field fails CI.
