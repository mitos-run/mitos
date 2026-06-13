# Bare-metal husk activation: first real-KVM end-to-end run

Hardware: Hetzner dedicated (Intel Core i7-6700, 4c/8t, 64 GiB, NVMe), KVM enabled.
OS/cluster: Talos Linux v1.13.3 single node, Kubernetes v1.36.1, Flannel CNI.
Engine: Firecracker v1.15.0, forkd direct-exec (jailer disabled in-pod; see follow-up).
Template image: python:3.12-slim (mirrored to ghcr, authenticated pull).

## Measured (real, reproducible on this node)

- Snapshot restore (Firecracker `/snapshot/load`, claim activation): 6.09, 6.68, 8.13 ms
  across three husk pods. Sub-10 ms restore on this 2015-era CPU; the <=10 ms
  claim->first-exec target is met at the restore step.
- In-VM exec round trip (sandbox API -> vsock -> guest agent -> command):
  python compute 25.7 ms; echo 1.06 ms; non-zero-exit error path 3.9 ms.
- Snapshot artifacts: mem 512 MiB, rootfs.ext4 167 MiB.
- Fork / density: 2 independent sandboxes activated from ONE snapshot
  (fork-a, fork-b, distinct pods/IPs/tokens); pool scaled to 3 dormant pods.

## Verified paths

- Template build: forkd pulls OCI image -> ext4 rootfs -> boots Firecracker on
  /dev/kvm -> guest-ready -> snapshot (mem+vmstate) + CAS manifest. REAL KVM.
- Husk pod: dormant VMM up; claim -> in-place restore -> VcpuEvent::Resume -> Ready.
- Sandbox API (token-gated): exec (exit/stdout/stderr/env), files write+read.
- Auth fail-closed: wrong token 401, missing bearer 401.

## Open (bare-metal-surfaced follow-ups, tracked in the fix branch)

- Warm pool does not refill after a claim consumes a dormant pod (replicas honored
  on scale-up, not refilled per-claim).
- Releasing a claim does not recycle/free its husk pod.
- Per-activation rootfs CoW not yet implemented (shared template rootfs mounted rw).
- Jailer pivot_root unavailable in-pod (ran unjailed); needs a private bind-mount.
- Claim reports sandboxID=<pod>, but the exec API expects sandbox="husk".
