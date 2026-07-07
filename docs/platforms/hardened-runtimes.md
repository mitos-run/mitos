# Hardened container runtimes (forkd seccomp and AppArmor posture)

forkd builds template snapshots and runs raw forks through the Firecracker
jailer, which `pivot_root(2)`s into a per VM chroot. The chart ships forkd with a
hardened securityContext, `privileged: false`, `drop: [ALL]` plus the explicit
builder capability set, and `seccompProfile: RuntimeDefault`. That default is
validated on kind and containerd, whose default seccomp profile permits
`pivot_root` when `CAP_SYS_ADMIN` is held.

Some hardened runtimes deny `pivot_root` under their default seccomp profile even
when `CAP_SYS_ADMIN` is held. Talos Linux is the known case. On those runtimes
forkd cannot launch the jailer and builds no snapshots; only the husk path (which
runs Firecracker directly, no jailer) works.

## Symptoms

forkd's template build loop logs, in order, as each layer is uncovered:

```
Failed to pivot root: Operation not permitted (os error 1)
Error: PivotRoot(Os { code: 1, kind: PermissionDenied })
```

and, once seccomp is relaxed but `CAP_FOWNER` is still missing:

```
Failed to change permissions on /: Operation not permitted (os error 1)
Error: Chmod("/", Os { code: 1, kind: PermissionDenied })
```

The first is the seccomp filter denying `pivot_root`; the second is the jailer's
`chmod` of the per VM chroot root, which needs `CAP_FOWNER`.

## Fix: the Talos values profile

`deploy/charts/mitos/values/talos.yaml` applies the exact minimal delta proven on
a bare metal Intel KVM node joined to a Talos v1.12.8 cluster:

```yaml
forkd:
  seccompProfile:
    type: Unconfined
  extraCapabilities:
    - FOWNER
```

Install with it layered on top of your own values:

```bash
helm install mitos deploy/charts/mitos \
  -f my-values.yaml \
  -f deploy/charts/mitos/values/talos.yaml
```

This keeps `privileged: false`, `drop: [ALL]`, and the full base builder
capability set. It does NOT use `privileged: true`; that also works but is a large
unnecessary blast radius. The three knobs cannot remove a base capability or
enable privilege escalation. They select seccomp or AppArmor confinement, or ADD
capabilities.

`CAP_FOWNER` is negligible marginal authority next to the `CAP_SYS_ADMIN` the
builder already holds (the same argument the threat model makes for
`CAP_DAC_OVERRIDE`).

## Prefer a Localhost profile over blanket Unconfined

`Unconfined` removes the syscall filter entirely. Where you can deliver a seccomp
JSON to every node's kubelet seccomp root (`/var/lib/kubelet/seccomp`), prefer a
tailored `Localhost` profile that is `RuntimeDefault` plus `pivot_root` and the
jailer's mount syscalls, so the filter is kept for everything else:

```yaml
forkd:
  seccompProfile:
    type: Localhost
    localhostProfile: profiles/mitos-forkd.json
  extraCapabilities:
    - FOWNER
```

The `seccompProfile` value is rendered verbatim into the container
securityContext, so any valid profile object is expressible. Shipping and
distributing the JSON to every node is operator owned and out of scope for the
chart.

## AppArmor-constrained runtimes

Some runtime-provided AppArmor profiles deny the recursive bind mount and
private mount propagation forkd needs while it prepares the Firecracker jailer
chroot. In that case, forkd cannot build template snapshots.

Check the node audit log for an AppArmor denial during a failed template build,
typically an `apparmor="DENIED"` event associated with a `mount` operation on
forkd's data directory. The chart default is `/var/lib/mitos`.

The chart leaves AppArmor selection to the runtime unless
`forkd.appArmorProfile` is configured. On Kubernetes 1.30 or later, prefer a
reviewed and pre-loaded `Localhost` profile over `Unconfined`:

```yaml
forkd:
  appArmorProfile:
    type: Localhost
    localhostProfile: mitos-forkd
```

[`examples/mitos-forkd.apparmor`](examples/mitos-forkd.apparmor) is a
compatibility example validated with the chart default
`forkd.dataDir=/var/lib/mitos`. Load it on every eligible KVM node before forkd
is scheduled, and review it for the Mitos version, container runtime,
distribution, and any custom data directory.

`Unconfined` removes AppArmor confinement for forkd and should only be an
explicit, reviewed operator choice.

## Why the default is not changed

The default stays `RuntimeDefault` because it is the most locked down profile that
passes on the CI runtime (containerd) and on the majority of clusters. Relaxing it
for everyone would weaken the common case to accommodate a minority of runtimes.
The platform profile is opt in and documented instead. See docs/threat-model.md
for the security rationale, and `cmd/forkd/jailer.go` `forkdRequiredCapabilities`
for the base capability set. Relates to issues #525, #352, #353.
