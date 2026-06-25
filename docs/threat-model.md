# Threat model

This document states what isolation `sandbox` provides today, what it intends
to provide, and the current status of every boundary. It is written against the
code in this repository, not the README. Statuses: **mitigated**, **partial**,
**open**.

For the operator-facing summary of how tenant secrets are delivered, isolated
across tenants, and encrypted at rest (consolidated with `docs/encryption.md` and
`docs/fork-correctness.md`), see `docs/secrets.md`.

**Honest summary for the current codebase: do not run untrusted code with this
project in production yet.** The KVM/Firecracker boundary is real, but several
defense-in-depth layers around it are still open, and an extended security review
(recorded in this document) found controls that the code did NOT implement.
The most serious of those, the husk default path having NO egress enforcement and
NO cloud-metadata block, is now IMPLEMENTED and KVM-VERIFIED end to end on a real
VM: the husk default path enforces in-pod default-deny egress with an
unconditional cloud-metadata block and the threaded per-template allowlist, and
the husk-network KVM cluster e2e (`test/cluster-e2e/husk-network-e2e.sh`, the
`cluster-husk-network-e2e` suite) now PASSES on the Hetzner Talos KVM cluster:
all three in-VM enforcement assertions are green inside a real restored VM
(metadata-blocked: a connect to `169.254.169.254` fails with exit 1 and no IAM
theft; default-deny: a non-allowlisted host fails to connect, exit 1; and
allowlist-works: the allowlisted name `example.com` IS reachable, connect exit 0).
The claim reaches Ready and the pool warms a dormant husk pod. This was verified
TWICE: once with a node kubelet sysctl allowance in place (proving the datapath),
then again with that node change fully reverted (the probe confirmed
SysctlForbidden), proving the feature needs NO node prerequisite. The remaining
open items below (item 1 is now resolved) are why this is not yet a production
posture. No external security review has happened.

## Running untrusted code: what is and is NOT warranted today

The microVM (KVM + Firecracker) is the isolation boundary, and the design DOES
target running untrusted code. But advertising this project as "safe for
untrusted code" is NOT yet warranted. The extended review found controls claimed
in this document that the code does not implement; those are corrected per row
below. Until the following are all true, do not run untrusted multi-tenant
workloads in production on this project, and do not claim it is safe to:

1. **Husk egress default-deny plus a cloud-metadata (169.254.169.254) block.**
   This was the top blocker; it is now IMPLEMENTED and KVM-VERIFIED end to end, so
   it is RESOLVED (mitigated). The husk default path enforces default-deny egress
   via the in-pod nftables filter the husk-stub programs in the pod's own netns at
   activation (`internal/husk/netfilter.go`, applied in `internal/husk/stub.go`,
   reusing `internal/netconf` and `internal/dnsproxy`). The cloud metadata
   endpoints (`169.254.169.254`, the `169.254.0.0/16` link-local range, and IPv6
   `fd00:ec2::254`) are an UNCONDITIONAL hard drop rendered before any allow
   (`netconf.RenderMetadataBlock`), so the allowlist can never reach them and a
   guest cannot steal the node's cloud IAM credentials. The per-template egress
   allowlist is threaded husk-side (`huskNotifyNetwork` + `huskEgressConfig` in
   `internal/controller/sandboxclaim_controller.go`, carried in
   `husk.ActivateRequest.Egress`/`Allow`), with a per-pod DNS proxy for name
   entries (`internal/dnsproxy`), and the husk pod adds exactly the scoped
   `NET_ADMIN` capability needed to program the filter. A best-effort Kubernetes
   NetworkPolicy (`internal/controller/husknetworkpolicy.go`) is created as
   defense in depth; HONEST CNI caveat: it enforces only on a CNI that implements
   NetworkPolicy and it cannot express name-based allows, so the in-pod nft filter
   is the guarantee that holds with no CNI policy at all (now KVM-verified).
   VERIFIED: the husk-network KVM cluster e2e
   (`test/cluster-e2e/husk-network-e2e.sh`, the `cluster-husk-network-e2e` suite)
   PASSES on the Hetzner Talos KVM cluster. The claim reaches Ready, the pool
   warms a dormant husk pod, and all three in-VM enforcement assertions are green
   inside a real restored VM: metadata-blocked (connect to `169.254.169.254`
   fails, exit 1, no IAM theft), default-deny (a non-allowlisted host fails to
   connect, exit 1), and allowlist-works (the allowlisted name `example.com` IS
   reachable, connect exit 0). It was verified TWICE: once with a node kubelet
   sysctl allowance in place (proving the datapath), then again with that node
   change fully reverted (the probe confirmed SysctlForbidden), proving the
   feature needs NO node prerequisite. See section 4 and section 0 surface 5.
2. **Fork-correctness fail-closed on ALL engines.** The raw-forkd and
   sandbox-server paths do NOT fail closed on an un-reseeded fork; only the husk
   path does (section 6, `docs/fork-correctness.md` row 1).
3. **Node-CAS bounds and integrity.** The node CAS is tenant-writable,
   unbounded, and activated with `--allow-unverified-snapshots` on the fork
   child path (section 3, W4 row).
4. **The secondary hardening** below: the raw-forkd privileged DaemonSet remains
   (the privileged-no-jailer DaemonSet is why raw-forkd is still NOT for untrusted
   multi-tenant). Four earlier secondary items are now SHIPPED: the husk pod no
   longer automounts the default SA token, the forkd gRPC surface fails closed
   without mTLS (opt-in `--allow-insecure-grpc` for dev), the host-side vsock
   client applies a per-request read deadline, and the raw-forkd shared-rootfs
   cross-fork write channel is FIXED via per-fork rootfs CoW (mirroring the husk
   fix). See the per-row table.
5. **The fork-correctness and failure/GC CI suites green** (a precondition
   already stated in CLAUDE.md and ROADMAP).
6. **A CVE / patch pipeline for the guest kernel and Firecracker** (today CVE
   watch is a manual process, section "Supply chain").
7. **An EXTERNAL security audit** (none has happened; see the review gate).

Must-fix-first set (the items that make the current posture actively unsafe, not
merely incomplete): item 1 (husk egress default-open + metadata reachable) is now
RESOLVED (mitigated): the husk default path enforces in-pod default-deny +
unconditional metadata block + threaded allowlist + per-pod DNS proxy via the
scoped `NET_ADMIN` capability (best-effort NetworkPolicy is defense in depth), and
the husk-network KVM cluster e2e (`cluster-husk-network-e2e`) PASSES on the
Hetzner Talos KVM cluster with all three in-VM enforcement assertions green
(metadata-blocked, default-deny, allowlist-works), verified twice (once with and
once without a node sysctl allowance), proving NO node prerequisite. The remaining
must-fix items are item 2 (fork-correctness not fail-closed off the husk path),
the raw-forkd privileged-no-jailer DaemonSet (item 4, which is why raw-forkd is
NOT for untrusted multi-tenant, even now that its shared-rootfs cross-fork write
channel is fixed by per-fork rootfs CoW), and item 3 (node-CAS
write/integrity/DoS). The per-row honest status discipline (mitigated / partial /
open, with a severity on the review rows) is preserved throughout.

## Components and trust

| Component | Runs as | Trusts | Trusted by |
|---|---|---|---|
| Guest workload | VM guest, untrusted | nothing | nobody |
| Guest agent - Rust (`guest/agent-rs`) | PID 1 in guest; the SOLE production agent since Phase E (#310). Baked as `/init` by `guest/rootfs/build.sh` and `kvm-test.yaml`. Serves ONLY gRPC on vsock port 53 (AgentGRPCPort); all host callers speak gRPC (SP1.5 merged, Go agent and legacy JSON protocol removed). SECURITY-SENSITIVE: requires named human reviewer; see `docs/security-review-policy.md`. | nothing | forkd / husk stub (gRPC, port 53) |
| husk pod stub (`cmd/husk-stub`), the DEFAULT runner | unprivileged pod, `/dev/kvm` via device plugin (not `privileged`), drop ALL caps and add only `NET_ADMIN` (in-pod egress firewall, scoped to the pod netns), `seccomp: RuntimeDefault`, read-only snapshot mount; the long-lived husk-stub container is NOT privileged | controller (mTLS control channel) | controller |
| husk `enable-ip-forward` init container (name-egress pools only) | short-lived PRIVILEGED init container on the husk pod; writes `net.ipv4.ip_forward=1` in the shared pod netns and exits BEFORE the workload runs; added only when name-based egress is configured (`--husk-dns-upstream` set) | controller (it is part of the pod spec) | controller |
| forkd (`cmd/forkd`), the snapshot BUILDER and the raw-forkd fallback | NON-privileged DaemonSet pod (uid 0, `privileged: false`, `allowPrivilegeEscalation: false`, `seccomp: RuntimeDefault`): drops ALL capabilities and adds back ONLY the explicit builder set (`SYS_ADMIN`, `CHOWN`, `SETUID`, `SETGID`, `MKNOD` for the jailer, plus `NET_ADMIN` for the build-time placeholder tap; `cmd/forkd/jailer.go` `forkdRequiredCapabilities`). The per-VM jailer is ENABLED in the shipped DaemonSet (`deploy/daemon/daemonset.yaml`: `--jailer`/`--chroot-base`/`--uid-range`), so every build/raw-forkd VM runs under a dedicated uid/gid in a per-VM chroot. `/dev/kvm` and `/dev/net/tun` come from the device plugin (`mitos.run/kvm`), NOT a privileged hostPath. (#352) | controller | controller, nodes |
| controller (`cmd/controller`) | cluster Deployment, CRD + Secrets RBAC | kube-apiserver | forkd, husk pods |
| Snapshot artifacts | files under `/var/lib/mitos` on each node | - | forkd builds them; husk pods mount and execute them as memory images |

## 0. Default execution surface: the unprivileged husk pod (issue #18)

Pod-native execution is now the DEFAULT (the controller runs
`--enable-husk-pods` by default; `--enable-raw-forkd` and `--mock` select the
fork-per-claim fallback). This is a deliberate change to the per-sandbox
execution surface. This section is the FULL re-derivation for the
unprivileged-stub escape surface (issue #18); it re-derives the surface boundary
by boundary, states which CI-proven mechanism backs each claim, and names every
residual. It does not contradict the per-row sections below; it reconciles with
them (the networking egress row in section 4, the encryption/key-custody row in
section 5, the sandbox-API row in section 3) and points at each.

The build-vs-run split is the core idea: a SNAPSHOT is BUILT once per node by a
privileged process and RUN many times by unprivileged pods.

- **Old default (raw-forkd, now the fallback behind `--enable-raw-forkd`).** A
  sandbox VM was forked by forkd: a root DaemonSet with `/dev/kvm`, an explicit
  capability set (`CAP_SYS_ADMIN`, `SYS_CHROOT`, and others; section 3), and a
  hostPath to the node data dir. The per-sandbox EXECUTION surface WAS that
  privileged process.
- **New default (husk pods).** A sandbox VM runs inside an UNPRIVILEGED husk pod:
  `privileged: false`, `allowPrivilegeEscalation: false`, drop ALL capabilities,
  `seccompProfile: RuntimeDefault`, `/dev/kvm` injected by the device plugin
  (not a hostPath or privilege), and the template snapshot mounted READ-ONLY. It
  adds exactly one capability, `NET_ADMIN`, scoped to the pod's own netns, so the
  stub can program the in-pod egress firewall (surface 5). The three documented
  PSA exceptions are `runAsNonRoot: false` (the device exception), the read-only
  snapshot hostPath (surfaces 1 and 3 below), and `NET_ADMIN` (the in-pod
  firewall, surface 5). On name-based-egress pools ONLY (the controller flag
  `--husk-dns-upstream` set) the pod additionally carries a short-lived
  PRIVILEGED init container (`enable-ip-forward`) that sets `net.ipv4.ip_forward=1`
  in the shared pod netns and exits before the workload runs (surface 5); it is
  privileged but one-shot, and the long-lived husk-stub container itself stays
  unprivileged (`NET_ADMIN` only).
- **forkd-the-builder is the residual host privilege, but it is no longer a
  privileged container (#352).** Building a template snapshot still needs
  `/dev/kvm` and the jailer, so forkd remains the privileged BUILDER role on the
  KVM nodes (and the `--enable-raw-forkd` fork-per-claim engine). That role now
  runs as a NON-privileged pod confined to the explicit builder capability set,
  not `privileged: true`, and the privileged surface is confined to the BUILD
  path, run once per node per template, rather than every sandbox execution.
- forkd is NON-privileged (deploy/daemon/daemonset.yaml, #352). forkd is the
  per-node BUILDER and the raw-forkd fallback: building a snapshot runs the
  jailer, which needs `CAP_SYS_ADMIN` (chroot-base mount setup, cgroup, and
  namespace setup), `CAP_CHOWN`/`CAP_SETUID`/`CAP_SETGID` (hand and drop to the
  per-VM uid), `CAP_MKNOD` (in-chroot `/dev/kvm` and `/dev/net/tun` nodes), and
  `CAP_NET_ADMIN` (the build-time placeholder tap). forkd runs with EXACTLY that
  set (`drop: [ALL]`, `privileged: false`, `allowPrivilegeEscalation: false`,
  `seccompProfile: RuntimeDefault`), and `/dev/kvm`/`/dev/net/tun` come from the
  device plugin (`mitos.run/kvm`), not a privileged hostPath. Status: mitigated.
  Mitigation: the forkd pod runs only on labelled KVM nodes (`mitos.run/kvm`), is
  one-per-node (not one-per-sandbox), is not exposed to tenant traffic, and its
  build/raw-forkd VMMs run jailed under throwaway uids; husk pods, not forkd, are
  the tenant execution surface.
- The per-VM Firecracker jailer is deliberately NOT run inside the husk pod.
  jailer-in-pod was implemented and VERIFIED achievable on real KVM (branch
  feat/jailer-in-pod, closed PR #96), but it requires the full 9-cap jailer set,
  Unconfined seccomp, and a writable exec+dev hostPath chroot, which makes EVERY
  husk pod privileged-class and breaks the PSA-restricted model. It is declined:
  the jailer isolates many VMs sharing one process (raw-forkd), but in the husk
  model each pod runs exactly ONE VM, so the pod itself is the per-VM boundary
  (its own uid, netns, cgroup, PSA-restricted securityContext). Per-VM isolation
  comes from one-VM-per-unprivileged-pod plus the microVM, not an in-pod jailer.

### Unprivileged-stub escape surface (issue #18 re-derivation)

The honest framing up front: the per-sandbox EXECUTION surface is strictly
improved (an unprivileged, capability-dropped, restricted-minus-two,
pod-netns-governed container instead of a root process), while the INHERENT
microVM-host-escape risk (a KVM or host-kernel bug reachable from any
`/dev/kvm`-holder) is UNCHANGED, and forkd-the-builder remains a smaller
privileged control-plane surface (run once per node per template, not per
sandbox). "Provably better" is argued PER SURFACE below and tallied honestly; it
is NOT claimed globally, because the `/dev/kvm`-and-kernel axis is EQUAL, not
better.

**Surface 1: GUEST -> HUSK-STUB CONTAINER (the post-VMM-escape blast radius).**
A guest that breaks out of the microVM (a Firecracker or KVM escape) lands in the
process that hosts the VMM. Under the old default that process was forkd: ROOT,
with `/dev/kvm`, an explicit capability set including `CAP_SYS_ADMIN`, and a
hostPath to the node data dir (section 3). Under the husk default that process is
the husk stub inside an UNPRIVILEGED pod whose securityContext
(`internal/controller/huskpod.go`, proven in `internal/controller/huskpod_test.go`
at the object level and against the v1.31 PodSecurity admission plugin on the
`kind-e2e-husk` job, slice 4, conformance criterion 4) sets, each control
load-bearing:

- `privileged: false`,
- `allowPrivilegeEscalation: false` (no setuid path regains privilege),
- `capabilities.drop: [ALL]`, with exactly `NET_ADMIN` added back so the stub can
  program the in-pod egress firewall (surface 5) in the pod's OWN netns; this is
  scoped to the pod netns (not hostNetwork, not privileged) and cannot reach the
  host netns or another pod,
- `seccompProfile: RuntimeDefault` at BOTH the pod and the container level,
- the ONLY host mount is the READ-ONLY snapshot dir plus the read-only kernel
  file (surface 3),
- the ONLY device is `/dev/kvm` (and `/dev/net/tun`) via the device plugin, not a
  hostPath or privilege (surface 4).

This securityContext satisfies EVERY PSA `restricted` control except three
documented exceptions; the husk pod is kept out of a `restricted` namespace by
EXACTLY those three, each intrinsic to the model: the read-only snapshot hostPath
and `runAsNonRoot: false` (uid 0 so Firecracker can open the injected `/dev/kvm`
without `privileged`), both recorded as
docs/adr/0003-kvm-device-plugin-psa-exception.md, plus `NET_ADMIN` (forbidden
under PSA baseline/restricted "Capabilities") for the in-pod egress firewall,
recorded as docs/adr/0006-husk-netadmin-egress-firewall.md. Justification: the
capability is scoped to the pod's own netns (not hostNetwork, not privileged,
`allowPrivilegeEscalation: false`, `seccomp: RuntimeDefault`), so it cannot reach
the host netns or other pods; the net security change is strongly positive: it
trades one scoped capability in an already-unprivileged pod for closing
default-open egress and node IAM-credential theft. The husk pod is thus
"restricted EXCEPT the read-only snapshot hostPath, `runAsNonRoot: false`, and
`NET_ADMIN` (for in-pod firewalling)". A genuinely privileged pod IS rejected in
the same namespace (PSA is enforcing), asserted on `kind-e2e-husk` (slice 4,
section 6e of `docs/husk-pods.md`).

This is the core "provably better" claim, and it is bounded to THIS surface: a
guest that escapes the microVM lands with NO root authority, NO Linux
capabilities, NO privilege-escalation path, NO broad host filesystem, only a
read-only base-image mount and the pod's own netns and cgroup, instead of forkd's
root with `CAP_SYS_ADMIN` and host data-dir access. The post-escape blast radius
is strictly smaller. What this does NOT change is whether the guest can reach the
host kernel through `/dev/kvm` in the first place (surface 4).

**Surface 2: the CONTROL CHANNEL (activation + secret delivery).** Activating a
husk pod delivers the tenant's claim-time env, secrets, and the per-sandbox
bearer token into the pod. The channel is mTLS, `RequireAndVerifyClientCert`,
authorized to the controller identity ONLY: `internal/husk.ServeTLS` plus
`AuthorizeControllerIdentity` accept a connection only when the VERIFIED mTLS peer
(read from `VerifiedChains`, never from a merely-presented cert) carries the
`pki.ControllerName` SAN, and a nil TLS config or nil authorize hook is refused
(fail-closed: an unauthenticated activate channel that delivers secrets is
rejected before any request is read). CI-proven: the KVM husk network-activation
phase asserts a WRONG-CA controller cert is REJECTED by the mTLS gate before any
secret is read (slice 2, section 6b of `docs/husk-pods.md`). So an in-cluster
actor cannot activate or hijack a husk pod, or inject secrets into one. Residual:
a compromised CONTROLLER can activate any husk pod and deliver secrets to it. The
controller is the trust anchor here, the same anchor as in the raw-forkd model
and the encryption key custody (section 5); this is not a regression, it is the
same boundary.

**Surface 3: the READ-ONLY SNAPSHOT HOSTPATH.** The node template snapshot is
mounted READ-ONLY into the husk pod (`huskpod.go`: the snapshot hostPath and the
kernel file are both `ReadOnly: true`). The husk stub RE-VERIFIES the snapshot ON
ACTIVATE, before it loads it, applying the SAME fail-closed gate as raw-forkd:
the stub decodes the mounted CAS manifest, binds it to the controller-passed
recorded digest (`husk.ActivateRequest.ExpectedDigest`, fed from the
NodeRegistry's forkd-reported `TemplateDigests`), re-hashes the loaded
mem+vmstate against it (a sha256 digest verify, #9), and runs
`internal/snapcompat.Check` against THIS node's detected environment (#32), all
in `internal/husk` `verifySnapshot` (the production verifier, shared with the
fork path via the `internal/cas` chunk/hash primitives so the two cannot drift).
Both checks fail closed: a snapshot tampered on the node disk after forkd's
build-time verification, or one incompatible with this node, is REFUSED on the
husk path too and never loaded into the VM (proven by `internal/husk`
`TestActivateVerifyRefusesTamperedSnapshot` and
`TestActivateVerifyRefusesIncompatibleSnapshot`). So the husk path is no longer a
verify gap relative to raw-forkd: it is the same digest + snapcompat gate.
Residual, stated honestly: all husk pods on a node share the SAME read-only
snapshot dir. This IS a shared read-only host mount and is one of the two
documented PSA-restricted exceptions (the hostPath, surface 1). It is acceptable
because (a) it is READ-ONLY: a husk pod cannot WRITE it, so it cannot tamper with
the base image another pod loads; (b) it is integrity-verified and
content-addressed, and the husk stub re-checks the digest + compatibility on EACH
activate before loading, so a tampered-on-disk or incompatible snapshot is
refused at activate time on the husk path; and (c) it is a
BASE IMAGE, not tenant data: tenant secrets are delivered post-restore over the
control channel (surface 2), never baked into the shared snapshot (section 6).
Cross-pod isolation of the snapshot mem/vmstate is the read-only property, not a
per-pod copy. The ROOTFS isolation is from a per-activation copy-on-write clone
rebound while the guest is FROZEN, not from a read-only template mount. The
template dir (which holds `rootfs.ext4`) is mounted read-write, because
Firecracker opens the snapshot's baked rootfs path with O_RDWR during
`/snapshot/load` (a read-only mount fails the load EROFS, verified on real KVM);
isolation does NOT rely on the mount mode. Each activation gets its OWN clone:
`internal/husk` `Stub.Prepare` reflink-clones `<dataDir>/templates/<id>/rootfs
.ext4` to a PER-POD file `<dataDir>/husk-rootfs/<pod-name>/rootfs.ext4` (the clone
path is scoped to the per-pod VM id the controller passes via the downward API
`metadata.name`, so two husk pods sharing the node CoW hostPath never collide on,
overwrite, or delete each other's clone), and `Stub.Activate` loads the snapshot
PAUSED (`resume=false`), rebinds the baked `rootfs` drive to that clone with
`PatchDrive` while the guest is frozen, THEN resumes. The template is only OPENED
(never written) during the paused load and the drive fd is replaced by PatchDrive
before resume, so the guest writes only its own clone, never a single block of the
shared template, and concurrent activations of one template never leak one
tenant's filesystem state into another. The clone is removed on pod teardown
(`Stub.Close`). Fully pod-native snapshot delivery (a CAS pull into the pod,
removing the shared read-only mem/vmstate hostPath) remains a documented follow-up.

### Warm-pool autoscaling (no integrity-gate move)

Demand-driven warm-pool autoscaling changes only WHEN and HOW MANY dormant husk
pods the controller creates or deletes. It does NOT change the snapshot integrity
gate: every dormant pod still runs the same fail-closed Prepare-time verify
(digest + snapcompat) against the read-only mounted CAS manifest before it can be
offered for a claim (Surface 3). The autoscaler reads only pod labels (dormant vs
claimed) and a process-local claim-arrival timestamp; it trusts no
tenant-controlled input, holds no secret, and a compromised husk pod cannot
influence the desired count beyond appearing claimed (which only makes the pool
create MORE warm capacity, never fewer or unverified pods). Scale-down deletes
only surplus DORMANT pods, never a claimed/in-use one. Security surface: unchanged.

A SEPARATE follow-up (a per-node verify cache so the second dormant pod on a node
skips the ~680 MiB re-hash) WILL touch the integrity gate and must land with its
own threat-model delta; it is intentionally out of scope here.

### Husk fork snapshot (live fork on the husk path)

A fork (a `Sandbox` with `source.fromSandbox`) of a husk-backed source drives a `ForkSnapshot` control op against
the SOURCE husk pod's stub over the SAME mTLS channel as activate (authorized to
the controller identity only: `internal/husk.ServeTLS` plus
`AuthorizeControllerIdentity`; the op rides the same op-dispatched channel that
delivers secrets on activate). The op carries NO secrets (a fork id and a
node-local snapshot path). The stub pauses the running VM, writes a Full
Firecracker snapshot to a node hostPath `<dataDir>/forks/<fork-id>` (read-write
only to the source pod that owns the VM; read-only to the child pods on the SAME
node), then resumes the source unless `pauseSource`. The fork snapshot is a LIVE,
EPHEMERAL artifact created by a trusted node-local stub and consumed by child
stubs on the same node within the same trust boundary; it is NOT content-addressed,
so the children activate it with verify disabled (`--allow-unverified-snapshots`),
the same posture a pre-digest pool uses. This is acceptable because the artifact
is root-owned, never tenant-writable, and re-hashing would gate on a digest that
does not exist for a live fork. The child still runs the full fail-closed RNG/clock
reseed handshake (see `docs/fork-correctness.md`, husk fork children). Per-child
independence: each child is its own husk pod + dormant VMM + per-activation rootfs
CoW clone, so guest writes never cross between children or back to the source; the
children share only the read-only fork snapshot mem+vmstate as a restore image,
exactly as warm pods share the template snapshot. Each child mints its OWN bearer
token (the source's token never opens a child). Lifecycle: the fork snapshot is
owned by the forking `Sandbox` and removed by its finalizer (`RemoveForkSnapshot` op)
on delete; the child pods are owner-ref'd to the fork and reaped by Kubernetes GC.
Residual: a compromised controller can drive a fork-snapshot of any husk pod it
can reach, the same residual already stated for activate (Surface 2).

### Husk workspace hydrate/dehydrate (W4 transport on the husk path)

A bound claim's `/workspace` is persisted and restored over TWO new control ops on
the SAME op-dispatched mTLS channel as activate and fork-snapshot, authorized to
the controller identity ONLY (`internal/husk.ServeTLS` plus
`AuthorizeControllerIdentity`). The controller is not on the node and cannot reach
the guest vsock or the node CAS, so it DELEGATES the transfer to the husk-stub
that owns both:

- `dehydrate-workspace(excludePaths, capturePaths, parentManifestDigest)`: the stub
  runs the guest vsock `TarDir` over `/workspace`, stores the content-addressed
  chunks plus manifest into the node CAS (a `<dataDir>/cas` hostPath mounted
  read-write into the pod), and returns the manifest digest. It reuses
  `internal/workspace.Dehydrate` (the KVM-proven tar round trip), not a
  reimplementation. When `parentManifestDigest` is set (a `{diff: true}` terminate)
  it ALSO computes the content-hash diff of the new revision against that parent and
  returns it: the diff is computed on the node from the two MANIFESTS (path ->
  chunk-digest lists) in the node CAS via `internal/workspace.DiffManifests`, never
  the chunk bytes, because the off-node controller cannot read either node-CAS
  manifest. The diff carries content path NAMES only; no chunk bytes ride the result.
- `hydrate-workspace(manifestDigest)`: the stub reads the manifest plus chunks
  from the node CAS and `UntarDir`s them into the guest `/workspace`.

The ops carry NO secrets: the request is path lists / content-address manifest
digests, and the result is a manifest digest plus an optional content-path diff and
latency. Secret/credential paths
are stripped from the captured tree by the dehydrate exclude list
(`WorkspaceSecretExcludePaths`), so a committed revision is content only. Workspace
CONTENT bytes never appear in a log line or an error on either side. The ops FAIL
CLOSED: the stub requires an active VM and a configured node CAS (an unset
`--cas-dir` disables them), and the controller delegate refuses on an unreachable
pod or a not-OK result rather than committing a revision the node never produced;
the controller still owns the `WorkspaceRevision` commit + head advance. Surface
delta vs the prior model: the node CAS is now mounted READ-WRITE into the husk
pod (it was read-only manifests before) so the stub can persist a revision. The
content-addressed store stays plaintext-content-addressed (or per-workspace
encrypted at rest under `spec.store.encryptionKeyRef`, section 6). Residual: a
compromised controller can drive a dehydrate/hydrate of any husk pod it can reach
(the same activate/fork residual, Surface 2); a compromised husk pod already had
write access to its node `<dataDir>` subtree (forks dir, rootfs CoW), and the CAS
mount widens that to the content store on the same node, bounded to the node's own
data dir.

**Surface 4: the DEVICE `/dev/kvm`.** KVM access is injected by the device plugin
(`cmd/kvm-device-plugin`, `internal/deviceplugin`): the pod requests
`mitos.run/kvm` like any extended resource and the kubelet bind-mounts
`/dev/kvm` (and `/dev/net/tun`) on `Allocate`, so the pod sets NO `privileged:
true` and carries NO `/dev/kvm` hostPath. CI-proven: the `kind-e2e` job drives
the full advertise -> schedule -> inject path with a NON-privileged probe pod
(`privileged: false`, escalation false, drop ALL, read-only rootfs) and
`kubectl exec` confirms `/dev/kvm` is present inside the container, coming
entirely from `Allocate`, not from any privilege (section 5 of
`docs/husk-pods.md`). Residual, stated honestly: `/dev/kvm` IS exposed to the
pod, so a KVM-or-host-kernel escape from the VMM is STILL a host-escape vector.
The device plugin removes the PRIVILEGED requirement, NOT the `/dev/kvm` attack
surface itself; that surface is inherent to ANY Firecracker host and is UNCHANGED
between raw-forkd and husk. This is the axis on which the two models are EQUAL,
not better. The device-plugin DaemonSet has its own small surface
(`deploy/device-plugin/daemonset.yaml`): it runs as root because the kubelet
device-plugins dir is root-owned, but it is `privileged: false`,
`allowPrivilegeEscalation: false`, ALL capabilities dropped, and
`readOnlyRootFilesystem: true`; its only host access is the kubelet
device-plugins dir (to serve and register its socket) and a read-only `/dev`
(to `stat /dev/kvm`); it creates NO device nodes and starts NO VMs.

**Surface 5: the POD NETNS (egress). IMPLEMENTED and KVM-VERIFIED end to end
(in-pod default-deny + metadata block proven inside a real restored VM on the
husk path; raw-forkd unchanged).** In the husk default the VM's tap lives inside
the HUSK POD's network namespace, so the sandbox's traffic IS the pod's traffic.
This was the top must-fix-first blocker (default-open egress). The remediation is
an in-pod nftables filter the husk-stub programs in the pod's own netns at
activation, and it is now VERIFIED working inside a real restored VM by the
husk-network KVM cluster e2e (see the status note below). The datapath is:

- **The in-pod nftables filter is the guarantee (CNI-independent).** The
  husk-stub brings up the VM's tap and installs a default-deny egress chain in
  the husk pod netns at activation (`internal/husk/netfilter.go`, applied in
  `internal/husk/stub.go`), reusing the raw-forkd dataplane rendering
  (`internal/netconf`: per-tap chain, `ip saddr` anti-spoof) and the controlled
  DNS proxy (`internal/dnsproxy`: name-allowlist resolution + IP pinning).
  Because the tap is in the POD's netns, this holds regardless of the cluster
  CNI. It needs exactly `NET_ADMIN` scoped to the pod netns (surface 1, ADR
  0006).
- **The cloud-metadata block is unconditional.** `netconf.RenderMetadataBlock`
  emits a hard drop for `169.254.169.254`, the `169.254.0.0/16` link-local range
  (covers ECS task metadata), and IPv6 `fd00:ec2::254` BEFORE any allow rule and
  regardless of the template policy (even `EgressAllow`), so the allowlist can
  never reach the metadata endpoint and a guest cannot steal the node's cloud IAM
  credentials.
- **The guest agent configures its NIC via rtnetlink, not an `ip` binary.** The
  guest agent brings up its own `eth0` (address, route) through rtnetlink
  syscalls (`internal/guestnet`), not by shelling out to an `ip` binary, so the
  datapath works on an arbitrary OCI-image rootfs that may carry no networking
  tools.
- **Name-based egress runs an in-pod DNS proxy with a failover upstream list.**
  The in-pod DNS proxy resolves ONLY allowlisted names and pins each resolved IP
  into the per-tap nftables allow set, forwarding to a comma-separated upstream
  resolver list tried in failover order. The recommended value is the public pair
  `1.1.1.1:53,8.8.8.8:53`, deliberately NOT cluster DNS, so an untrusted sandbox
  cannot resolve internal service names. The proxy also REFUSES to pin a resolved
  address in any non-publicly-routable range (RFC1918, IPv6 ULA, loopback,
  link-local, multicast, unspecified, RFC6598 CGNAT, deprecated site-local
  fec0::/10) and strips it from the answer, so an allowlisted name whose
  authoritative DNS an attacker influences cannot be rebound at internal cluster
  services or node-local targets (DNS-rebinding-to-internal defense in depth,
  complementing the unconditional IMDS block). The check also decodes IPv6
  addresses that embed an IPv4 target inside the NAT64 well-known prefix
  (`64:ff9b::/96`) or the 6to4 prefix (`2002::/16`) and applies the same policy
  to the embedded IPv4, so on a DNS64/NAT64 cluster a wrapper such as
  `64:ff9b::a9fe:a9fe` (NAT64 of `169.254.169.254`) is refused while a wrapper of
  a public IPv4 stays allowed. RESIDUAL: a non-default, operator-configured NAT64
  prefix (other than the well-known `64:ff9b::/96`) is not yet decoded; clusters
  using a custom NAT64 prefix should set it via the resolver config once that
  knob lands.
- **Guest-to-pod-local traffic is filtered on the INPUT hook (husk path).** The
  per-tap forward chain governs transit egress, but a packet the guest sends to
  an address LOCAL to the pod netns (the tap gateway, the resolver, the
  husk-stub sandbox API on :9091 and mTLS control on :9443) is delivered on the
  kernel input hook, which a forward-only filter never evaluates. So without an
  input rule a guest could reach those pod-local listeners regardless of egress
  policy (their own auth gates limited impact, but the egress allowlist offered
  no protection and any future in-pod listener was exposed). `applyEgressFilter`
  now also installs an input-hooked base chain plus a per-tap input chain
  (`netconf.RenderSharedInputTable` / `RenderSandboxInputChain`) that accepts
  the guest only to the resolver on udp/tcp 53 and drops every other
  guest-sourced packet to a pod-local address. The base chain policy is accept
  so non-sandbox input (kubelet probes, the controller's mTLS dial arriving on
  the pod uplink) is untouched; isolation is via the per-tap dispatch jump. This
  is on the HUSK path only: the filter lives in the isolated pod netns, whereas
  the raw-forkd path runs in the node netns where a node-wide input hook is not
  added (raw-forkd's host-local exposure is tracked separately). Renderer and
  wiring are unit-tested (`TestRenderSandboxInputChainBlocksGuestToPodLocal`,
  `TestApplyEgressFilterInstallsInputGuard`); end-to-end enforcement inside a
  real restored VM is gated by the husk-network KVM e2e
  (`test/cluster-e2e/husk-network-e2e.sh`) which MUST be green before merge.
- **The per-template allowlist is threaded husk-side.** `huskNotifyNetwork`
  delivers the fixed in-pod /30 plus the in-pod resolver, and `huskEgressConfig`
  carries the template egress policy + allowlist in the activate request
  (`internal/controller/sandboxclaim_controller.go`,
  `husk.ActivateRequest.Egress`/`Allow`). IP:port entries become static chain
  accepts; name entries are resolved + pinned by the in-pod DNS proxy.
- **First-class network-posture knobs (issue #219): block_network, CIDR
  allowlists, deny-by-default inbound.** The per-sandbox `NetworkPolicy`
  (`api/v1`, threaded through `husk.ActivateRequest` and the proto
  `NetworkConfig`) now also expresses, in addition to the egress policy + name
  allowlist above:
  - **`blockNetwork` (total deny).** When set, the per-tap chain drops ALL
    egress, v4 and v6, with NO accept of any kind (not even established/related):
    the Modal `block_network=True` primitive for a sandbox that must never reach
    the network. It overrides the egress policy and every allowlist. Rendered by
    `netconf.RenderSandboxChainSpec` (the block branch emits only the metadata
    block and the v4/v6 drops).
  - **`allowCidrs` (CIDR egress allowlist).** A destination IP inside an allowed
    block is accepted, v4 saddr-pinned and v6 family-scoped, exactly like the
    static IP:port accepts. The CIDR list is parsed fail-closed
    (`netconf.ParseCIDRList`): a malformed CIDR fails the whole activation rather
    than silently dropping the rule, so a sandbox never comes up with a partially
    applied allowlist.
  - **Deny-by-default INBOUND (`inbound`, `inboundCidrs`).** The input-hook chain
    (above) already dropped all guest-sourced pod-local traffic except DNS; the
    secure default is now made explicit as `inbound: deny`. `inbound: allow`
    (optionally narrowed to `inboundCidrs` source blocks) accepts unsolicited
    inbound to the guest IP for a sandbox that intentionally hosts a listener.
    Return traffic for the guest's own egress is always accepted via the forward
    chain's established,related rule, so deny-by-default inbound never breaks the
    guest's outbound flows. The SECURE DEFAULT for an untrusted sandbox, applied
    by the SDK and the sandbox-server when no policy is supplied, is
    deny-by-default in BOTH directions: egress `deny` (no allows) and inbound
    `deny`. The rule rendering of each new dimension is unit-tested in
    `internal/netconf` (`TestRenderSandboxChainBlockNetwork`,
    `TestRenderSandboxChainCIDRAllowlist`, `TestRenderSandboxInputChainAllowCIDR`,
    and the deny-by-default cases); the real in-VM packet enforcement is KVM-gated
    by the existing husk-network e2e (the new dimensions reuse the SAME proven
    datapath: per-tap dispatch, `ip saddr` anti-spoof, terminal drop).
- **Per-sandbox egress byte counter (the #211 metering seam).** Each sandbox's
  chain carries a named nftables counter (`sb_<tap>_egress`) incremented on every
  guest-sourced egress packet at the top of the chain, so the metering pipeline
  (#211) reads per-sandbox egress bytes by name (`netconf.NftReadEgressCounterArgs`
  + `ParseEgressCounterBytes`, surfaced via `metering.Sample.EgressBytes`). It is
  a passive counting rule with no verdict, so it never changes enforcement; it is
  a usage-accounting and abuse-signal source, not a security control.
- **SNAT masquerade plus IPv4 forwarding let allowed traffic egress and return.**
  An nftables SNAT masquerade scoped to the guest source address, plus IPv4
  forwarding enabled in the pod netns, let allowed traffic reach the internet and
  return. `net.ipv4.ip_forward=1` is set in the pod netns by a SHORT-LIVED
  PRIVILEGED INIT CONTAINER on the husk pod (name `enable-ip-forward`), which
  writes the sysctl in the shared pod netns and exits before the workload runs.
  This needs NO node/kubelet change. It is added ONLY when name-based egress is
  configured (controller flag `--husk-dns-upstream` set). It runs in the
  privileged-PSA namespace husk already requires (for `NET_ADMIN` and hostPath),
  so it adds no new operator prerequisite. Blast radius, stated honestly: it is
  privileged but SHORT-LIVED (runs once, sets one sysctl, exits), versus a
  privileged long-lived workload; the long-lived husk-stub container itself stays
  unprivileged (`NET_ADMIN` only, via the device plugin for KVM, not
  `privileged: true`). It is gated to name-egress pools only.
- **A best-effort Kubernetes NetworkPolicy adds defense in depth.** The
  controller now creates one `networking.k8s.io/v1` NetworkPolicy per pool
  selecting `mitos.run/husk=true` with default-deny egress, a DNS allow, and one
  egress rule per enforceable IP:port allow, owner-referenced to the pool for GC
  (`internal/controller/husknetworkpolicy.go`). HONEST CNI caveat: a
  NetworkPolicy only enforces on a CNI that implements it, so it is defense in
  depth ONLY; the in-pod nft filter above is the guarantee that holds with no CNI
  policy at all.

Status: **mitigated (IMPLEMENTED and KVM-VERIFIED end to end).** The in-pod
default-deny filter, the unconditional metadata block, the threaded per-template
allowlist, the per-pod DNS proxy, and the scoped `NET_ADMIN` are all implemented,
the controller creates the best-effort NetworkPolicy, and the husk-network KVM
cluster e2e (`test/cluster-e2e/husk-network-e2e.sh`, the `cluster-husk-network-e2e`
suite) now PASSES on the Hetzner Talos KVM cluster. The claim reaches Ready, the
pool warms a dormant husk pod, and all three in-VM enforcement assertions are
green inside a real restored VM: metadata-blocked (connect to `169.254.169.254`
fails, exit 1, no IAM theft), default-deny (a non-allowlisted host fails to
connect, exit 1), and allowlist-works (the allowlisted name `example.com` IS
reachable, connect exit 0). It was verified TWICE: once with a node kubelet
sysctl allowance in place (proving the datapath), then again with that node change
fully reverted (the probe confirmed SysctlForbidden), proving the feature needs
NO node prerequisite (the `enable-ip-forward` init container sets the only sysctl
required, in the pod netns). The raw-forkd nftables egress remains CI-proven
in-VM on KVM (section 4) and is unchanged; it gains the same unconditional
metadata block because the rendering is shared. The earlier hand-applied allow-all
`husk-egress` NetworkPolicy in `.github/workflows/ci.yaml` "Conformance 3" proved
nothing about restriction and is superseded by the controller-emitted object plus
the in-pod filter. Residuals: `NET_ADMIN` is a documented PSA exception (surface
1, ADR 0006), and on name-egress pools the `enable-ip-forward` init container is a
short-lived privileged surface (one-shot, sets one sysctl, exits; gated to
name-egress pools; the long-lived husk-stub container stays unprivileged). The
trade is strongly positive: it closes default-open egress and node IAM-credential
theft for one scoped capability in an already-unprivileged pod plus a one-shot
privileged init step, and it is now KVM-verified end to end.

**Surface 6: the IN-POD SANDBOX API (exec and files).** After activation the husk
stub serves the SAME `internal/daemon.SandboxAPI` forkd serves, IN the pod, on
the sandbox port; the claim's `Status.Endpoint` is `podIP:sandboxPort`. Every
exec/files request is gated by the per-sandbox bearer token (32-byte
`crypto/rand`, constant-time compare, fail-closed: a sandbox with no registered
token rejects everything). The token is delivered to the stub over the mTLS
control channel (surface 2), never logged, never in argv. CI-proven: a tokened
HTTP exec reaches the guest over vsock and an UNTOKENED or WRONG-TOKEN request is
rejected with the token value absent from host-side logs (slice 2,
`internal/husk` `TestActivateServesTokenGatedSandboxAPI`, and the KVM
network-activation phase over the real endpoint; section 3 row below). Residual:
tokens are static per sandbox (no rotation or expiry); anyone with namespace-wide
Secret read can take them (section 3).

**Surface 7: the ENCRYPTION KEY (#31 PR2).** When `--enable-encryption` is on,
the per-template 256-bit key reaches the node ONLY over the mTLS control channel
(the `CreateTemplate`/`Fork` gRPC requests; the controller refuses to deliver the
key to a node whose connection is not mTLS, and forkd refuses to start encrypted
without its TLS flags), is held in node process memory while a container is open,
and is NEVER written to the node data disk (section 5: `RequestKeyProvider`,
key-not-on-disk proven by unit and envtest, key-never-logged enforced by grep in
CI). On the HUSK path the key reaches FORKD (the builder) ONLY, over the same
mTLS gRPC; forkd uses it to open the per-template LUKS container and the snapshot
is decrypted BELOW the page cache by forkd's `dm-crypt` mount. The husk pod mounts
that mount's PRE-DECRYPTED snapshot bytes read-only and NEVER receives the
encryption key: the key does not cross the controller-to-husk mTLS control channel
and is never present in the husk pod's address space. So a compromised husk pod
cannot exfiltrate the template key. Residual, stated honestly: the IN-MEMORY KEY
WINDOW on the FORKD process. While a container is open the key is necessarily in
forkd's process memory; a root attacker with a node-memory dump of FORKD while a
container is open recovers it. Zeroize-on-close is the current mitigation;
HSM/envelope custody is the follow-up.

**Surface 8: EVICTION and DRAIN (slice 4b).** A husk pod is an ordinary pod, so
it is subject to drain, eviction, preemption, and delete. A `policy/v1`
PodDisruptionBudget (`<pool>-husk`, `minAvailable = max(1, Replicas-1)`) BOUNDS
voluntary disruption to at most one warm slot at a time; a lost husk pod
re-pends the claim (Phase Pending, endpoint cleared) and the warm pool self-heals
a replacement; a `drainPolicy` governs an active sandbox (Kill re-pends,
Checkpoint snapshots the live VM first where the VMM still runs). CI-proven
object-level on `kind-e2e-husk` (slice 4b, section 6f of `docs/husk-pods.md`).
This is an AVAILABILITY surface, not a new ESCAPE surface: a drained or evicted
husk pod is gone, not escalated. The honest availability note vs the old model:
raw-forkd's VMs were not pods and did not feel drains, but they also had no
bounded, self-healing disruption story; the husk model trades that for ordinary,
self-healing pod disruption with a documented budget. The live-VM
Checkpoint-on-drain actually SURVIVING end to end is bare-metal work (it needs
the VMM running in the husk pod on a KVM-capable kubelet).

### Per-axis tally: old forkd vs husk pod

This compares the per-sandbox EXECUTION surface. forkd-the-builder (the privileged
snapshot builder, run once per node per template) is NOT a per-sandbox surface and
is discussed separately below the table.

| Axis | Old forkd (raw-forkd) | Husk pod | Verdict |
|---|---|---|---|
| Privilege | uid 0, `privileged: false` since #352 (jailer ENABLED; explicit builder cap set), `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault` | `privileged: false`, `runAsNonRoot: false` (one of the two PSA-restricted exceptions, the `/dev/kvm` device one; the other is the read-only snapshot hostPath), no escalation | husk BETTER (both non-privileged now; husk adds far fewer caps and runs one VM per pod) |
| Capabilities | `drop: [ALL]`, adds back the explicit builder set (`SYS_ADMIN`, `CHOWN`, `SETUID`, `SETGID`, `MKNOD`, `NET_ADMIN`; #352) | `drop: [ALL]`, adds only `NET_ADMIN` | husk BETTER (raw-forkd retains CAP_SYS_ADMIN + uid 0 for the builder/jailer; husk adds one scoped cap) |
| Host FS access | hostPath to the node data dir (RW) | the snapshot mem/vmstate mount and kernel file are READ-ONLY, but the husk pod ALSO has WRITABLE node hostPaths: its per-pod rootfs CoW dir, the fork-snapshot dir, and (W4) the node CAS mounted READ-WRITE (`huskCASMountPath`, `internal/controller/huskpod.go`) so the stub can persist a revision | husk BETTER on the base image (read-only) but NOT read-only-only: a compromised husk pod can write the node CAS and its CoW dirs, bounded to the node data dir (see the W4 CAS row in section 3) |
| Device access (`/dev/kvm` + kernel) | `/dev/kvm` + `/dev/net/tun` via the device plugin (no privilege; #352 removed the privileged hostPath) | `/dev/kvm` via device plugin (no privilege) | EQUAL on the inherent KVM/kernel escape surface; both now get the device via the plugin, not a privileged hostPath |
| Network governance | host-nftables in forkd's netns (default-deny egress allowlist, CI-proven in-VM on KVM, but only on opt-in `--enable-networking`) | in-pod nftables default-deny in the pod's OWN netns, applied by the husk-stub at activation (CNI-independent), with an unconditional cloud-metadata block and the threaded per-template allowlist, plus a best-effort controller-emitted NetworkPolicy; IMPLEMENTED and KVM-VERIFIED end to end by the husk-network KVM cluster e2e (surface 5) | EQUAL and KVM-VERIFIED: husk enforces default-deny egress in-pod regardless of CNI (the same posture as raw-forkd) and blocks the metadata endpoint unconditionally, proven inside a real restored VM (the three in-VM assertions are green and the feature needs no node prerequisite, verified twice) |
| Secret + key delivery | mTLS gRPC to forkd | tenant secrets + token over the mTLS control channel to the pod (controller-identity authz, never on disk); the per-template ENCRYPTION KEY never reaches the husk pod at all (it goes to forkd, which serves the pre-decrypted snapshot via dm-crypt) | EQUAL/BETTER (same mTLS anchor; enc key never enters the husk pod, in-memory-window residual is on forkd only) |

Honest conclusion: on the privilege and capabilities axes the husk model is
clearly BETTER. On the host-FS axis it is better for the BASE IMAGE (read-only)
but it still holds writable node hostPaths (the CoW dirs and the read-write node
CAS), so it is not the read-only-only surface earlier claimed. On the NETWORK
axis the husk default now MATCHES opt-in raw-forkd and is KVM-VERIFIED: it
enforces an in-pod default-deny egress filter in the pod's own netns
(CNI-independent), plus an unconditional cloud-metadata block and the threaded
per-template allowlist (surface 5, section 4), the same posture raw-forkd has
with `--enable-networking`, and it adds the metadata block raw-forkd now also
gains because the rendering is shared. This is IMPLEMENTED and KVM-VERIFIED end
to end: the husk-network cluster e2e PASSES on the Hetzner Talos KVM cluster with
all three in-VM enforcement assertions green (metadata-blocked, default-deny,
allowlist-works), verified twice (once with and once without a node sysctl
allowance, proving NO node prerequisite). It costs one scoped capability
(`NET_ADMIN` in the pod netns, ADR 0006) plus, on name-egress pools only, a
short-lived privileged `enable-ip-forward` init container (one-shot, sets one
sysctl, exits; the long-lived husk-stub container stays unprivileged). The
residuals named (shared read-only snapshot mount, in-memory key window) stand.
On the inherent `/dev/kvm`-and-kernel axis the two are EQUAL: a KVM or host-kernel
bug reachable from a `/dev/kvm`-holder is the same risk in both models, and the
device plugin removes the privileged requirement, NOT that attack surface. The
per-sandbox EXECUTION surface is therefore IMPROVED on privilege and
capabilities, the inherent microVM-host-escape risk is UNCHANGED, and the husk
EGRESS surface now MATCHES raw-forkd (in-pod default-deny + metadata block) and is
KVM-verified end to end.
Separately,
forkd-the-builder remains a
PRIVILEGED control-plane surface (root, `CAP_SYS_ADMIN`, `/dev/kvm`, the jailer),
but it is SMALLER than the old per-sandbox surface: it runs the BUILD path once
per node per template, not on every sandbox execution, so the privileged surface
is confined to the build path and amortized across all sandboxes a template
serves. Removing forkd's privilege entirely (a builder redesign) is out of scope.

RESOLVED gaps the review surfaced (were must-fix-first; now mitigated):

- HUSK EGRESS is now IMPLEMENTED and KVM-VERIFIED end to end (was the top
  blocker): the husk-stub enforces an in-pod default-deny egress filter in the
  pod's own netns with an unconditional cloud-metadata block and the threaded
  per-template allowlist, plus a best-effort controller-emitted NetworkPolicy
  (surface 5, section 4). FIX/PROOF: the husk-network KVM cluster e2e
  (`test/cluster-e2e/husk-network-e2e.sh`, `cluster-husk-network-e2e`) PASSES on
  the Hetzner Talos KVM cluster with all three in-VM enforcement assertions green
  (metadata-blocked, default-deny, allowlist-works), the claim reaches Ready, and
  the feature was verified twice (once with and once without a node sysctl
  allowance), proving NO node prerequisite. The warm-pool over-create that blocked
  activation (below) is fixed by the husk pod readiness probe.
- DEHYDRATE-ON-DELETE FINALIZER HOT-LOOP is FIXED. A `Sandbox` bound to an
  already-deleted `Workspace` now treats the missing workspace as a TERMINAL
  no-op: it drops the finalizer and the sandbox is garbage-collected instead of
  retrying the dehydrate step forever. Proven by envtest.
- HUSK WARM POOL OVER-CREATE is FIXED by the husk pod readiness probe gating
  dormant-Ready on the control listener: a dormant pod is only counted Ready once
  its control listener is up, so the pool no longer creates an unbounded number of
  never-Ready pods toward a deficit it cannot close.

The husk default-SA-token automount, the fail-open gRPC default, and the missing
host-side vsock read deadline that this list previously carried are now SHIPPED
(mitigated); see the per-row table entries above for the mechanism and the test.

Accepted residuals (tracked, see ROADMAP W1 #18):

- the SHARED READ-ONLY SNAPSHOT MOUNT across husk pods on a node (read-only,
  integrity-verified, non-tenant base image; fully pod-native CAS delivery is the
  follow-up);
- the WRITABLE node hostPaths a husk pod holds (per-pod CoW dirs and the
  read-write node CAS, bounded to the node data dir; see the W4 CAS row in
  section 3 for the unbounded-disk and cross-tenant-destruction concerns);
- the `/dev/kvm` INHERENT host-escape surface (unchanged from raw-forkd; inherent
  to any Firecracker host);
- the IN-MEMORY ENC-KEY WINDOW while a container is open (HSM custody is the
  follow-up, #31);
- the FORKD-BUILDER PRIVILEGE (it stays the privileged builder; a builder
  redesign is out of scope);
- the live-Checkpoint-on-drain survival, proven only on a KVM-capable kubelet
  (a bare-metal reference node, #16).

The device-plugin surface itself is in section 3; the per-mode networking
reconciliation is in section 4; the encryption-key custody is in section 5.

## 1. Guest → host escape

The primary boundary is KVM hardware virtualization via Firecracker.

| Control | Status | Detail |
|---|---|---|
| Firecracker microVM (minimal device model) | **mitigated** | Each sandbox is a separate Firecracker process with its own KVM VM (`internal/fork/engine.go`). |
| virtio-rng device (new guest device) | **mitigated** | Every template snapshot bakes a Firecracker virtio-rng device backed by the HOST RNG (`firecracker.Client.SetEntropy` -> `PUT /entropy`, `VMConfig.EntropyDevice` default-on), so each forked guest has a continuous entropy source in addition to the per-fork NotifyForked CRNG reseed (fork-correctness row 1). The device is a guest -> host surface: it adds the Firecracker virtio-rng emulation path. The flow is one-directional and read-only from the guest's view (the guest draws random bytes; it supplies nothing the host acts on), draws from the host CSPRNG (`crypto/rand`-grade kernel RNG), is attached unthrottled but carries no secret material, and crosses no new boundary beyond the existing Firecracker device model (same KVM/Firecracker and unprivileged-husk-pod containment as the rootfs and vsock devices). The host never reads guest-supplied bytes through it. The continuous source also removes the failure mode where a long-running fork could starve its CRNG between reseeds. The config/JSON-builder is darwin-unit-tested; the live device is exercised in the KVM fork-correctness phase (distinct UUID and TLS client random across forks). |
| Jailer (dedicated UID, chroot, cgroup, namespaces per VM) | **mitigated as shipped (jailer ENABLED + forkd non-privileged, #352); the kernel-enforced capability/uid drop is proven in the KVM CI jailer run (issue #2 Task 5)** | The jailer IS implemented (`internal/firecracker/jailer.go`, `client.go:startJailedVM`): a dedicated uid/gid per VM from `--uid-range` (default 64000-64999; uid 0 refused), a per-VM chroot under `--chroot-base` containing only the explicitly hard-linked kernel, rootfs, and snapshot files (a traversal guard refuses anything outside the data dir and the VM workspace), and cgroup v2 attachment; ids are validated at the gRPC boundary (`internal/daemon/validate.go`) and the launch path refuses ids whose jailer dirs would escape the chroot base. The SHIPPED DaemonSet (`deploy/daemon/daemonset.yaml`) now PASSES the jailer flags (`--jailer`/`--chroot-base=/var/lib/mitos/jailer`/`--uid-range=64000-64999`) and runs NON-privileged with the explicit builder capability set (section 3, forkd capability minimization row), so every build/raw-forkd VMM runs UNDER a throwaway jailed uid in a per-VM chroot: a VMM compromise lands as that disposable uid, not forkd's root. forkd makes `--chroot-base` a private mount point at startup (`cmd/forkd/prepareChrootMount`) so the jailer's `pivot_root` works inside the non-privileged pod. The direct-exec dev path still remains when `--jailer` is omitted (forkd logs a loud warning; standalone sandbox-server always runs unjailed). The conformance test `cmd/forkd` `TestShippedDaemonSetEnablesJailer` keeps the shipped flags from regressing; the kernel actually enforcing the uid/cap drop is asserted on the KVM runner (the CI host is root and cannot observe the bounding-set shrink). |
| Seccomp on the VMM process | **mitigated (enforced + asserted, #353)** | The VMM runs Firecracker's default production seccomp BPF filter on every launch path (engine fork, raw-forkd, and husk activate): Firecracker installs it on all VMM threads UNLESS `--no-seccomp` is passed, and Mitos never passes it. This is now a CHECKED invariant, not an implicit one: `internal/firecracker.assertSeccompEnforced` runs on the final argv of BOTH the direct-exec and jailer launch paths (`client.go`) and FAILS CLOSED if any future flag or refactor ever threaded `--no-seccomp` in, so the VMM can never silently come up with its full syscall surface (unit-tested in `seccomp_test.go`). The enforcement is PROVEN on real KVM: the `kvm-test.yaml` "Seccomp filter enforced on the Firecracker VMM" step (load-bearing, not continue-on-error) BOOTS a microVM (Firecracker installs its per-thread filters at vCPU start, not at API-server start) and asserts a VMM thread reports `Seccomp: 2` (SECCOMP_MODE_FILTER) in `/proc/<pid>/task/*/status`, so a disallowed syscall is blocked, while a `--no-seccomp` boot leaves every thread mode 0, proving the assertion distinguishes enforced from absent rather than being vacuous. Custom tightened profile: EVALUATED and declined (`docs/security/vmm-seccomp.md`); Firecracker's upstream "advanced" filter is already scoped to the syscalls the VMM needs and is maintained and tested against each Firecracker release, so a hand-rolled tighter filter would add a brittle, version-coupled maintenance surface (a too-tight filter SIGSYS-kills a legitimate VMM on a kernel/libc/FC change) for no demonstrated reduction; the decision is to assert the upstream filter holds rather than fork it, and to pass an explicit `--seccomp-filter` only if a concrete required-syscall delta is ever found. |
| CVE posture / version pinning | **partial** | CI pins Firecracker v1.15.0; there is no documented update policy or advisory tracking. |
| Guest agent as attack surface (Rust, `guest/agent-rs`) | **partial (gRPC surface; sole production agent since Phase E #310)** | The Rust agent is the SOLE production guest agent: baked as `/init` by `guest/rootfs/build.sh` and `kvm-test.yaml`. It serves ONLY gRPC on vsock port 53. SP1.5 is merged and the Go agent and legacy JSON protocol (port 52) are removed. All host callers (fork, daemon, husk, smoke binaries) speak gRPC. The full firecracker-test suite (exec-via-vsock, fork-correctness, ws-smoke, SDK example, etc.) boots the Rust agent. No fuzzing of the gRPC surface yet. SECURITY-SENSITIVE: requires named human reviewer before any PR touching `guest/agent-rs/src/sys/`, `guest/agent-rs/src/fork/`, `guest/agent-rs/src/init/mod.rs`, or `guest/agent-rs/src/main.rs` is merged. See `docs/security-review-policy.md`. |
| In-guest self-service socket (`MITOS_SOCKET`, issue #22) | **mitigated (in-guest only; no new host surface)** | The Rust guest agent serves a small JSON protocol over a unix socket INSIDE the VM at `MITOS_SOCKET` (default `/run/mitos.sock`, `internal/guestsock`) so the in-VM workload can read its OWN identity and budget (and, once issue #25 wires it, request a budget-gated fork). The socket is reachable only from inside the guest: it is a unix socket on the guest's own tmpfs, NOT exposed on the tenant-facing HTTP sandbox API and NOT bridged to the host vsock, so it adds NO new host boundary; a caller of it is already running inside the untrusted guest at the same privilege as any `exec` child (KVM/Firecracker and the unprivileged husk pod, section 0, bound it). The `info` response carries NAMES and budget NUMBERS only (sandbox id, claim, pool, workspace names; fork caps), assembled by a key-whitelisting handler from the delivered env; no secret VALUE ever crosses it (the handler reads only the `MITOS_*` identity/budget keys, never the secret keys also present in the guest env). The host advertises the path and the sandbox's own id via the existing best-effort env-delivery channel (`withSelfServiceEnv`, `internal/daemon/server.go`); a listen failure is non-fatal so exec/files over vsock are unaffected. The same `MaxMessageBytes` line cap applies. The fork verb is not yet enabled: it returns an LLM-legible not-enabled error naming the orchestrator escalation path until the budget ledger (issue #25) is wired. |
| Host resource exhaustion (memory + sandbox count) | **mitigated (production-blocker #2)** | Three host-DoS dimensions are now capped, each as an O(1) admission/ceiling/sizing check OFF the warm-claim activate/fork hot path so they do not regress the warm-claim latency. (1) **Husk pod memory.** A husk pod previously carried a memory REQUEST only and no LIMIT, so a tenant VM could grow without bound and OOM the node. The controller now sets a memory LIMIT sized = request + headroom (`internal/controller/huskpod.go`, `huskMemoryLimit`), headroom = max(`--husk-memory-headroom` floor, default 256Mi; `--husk-memory-headroom-percent` of the request, default 25%). The headroom is load-bearing: the cgroup the limit caps holds the Firecracker VMM, the husk-stub, and CoW dirty-page slack ON TOP of the guest RAM, so a too-tight limit would OOM-kill a normally-running VM (which is why the limit must exceed the request); the headroom keeps the limit transparent to a legitimate VM while capping a runaway. The kubelet enforces the cgroup; the controller never throttles the running VM. cpu is deliberately left WITHOUT a limit (cpu throttling would hurt the activate latency); cpu stays requests-only for scheduler truth. (2) **Per-node sandbox count.** The engine reported `MaxSandboxes` in `GetCapacity` but never enforced it at `Fork`, so a runaway tenant could exhaust a node by opening forks. `Engine.Fork` now refuses with the typed `ErrAtCapacity` once the live count reaches `--max-sandboxes` (`internal/fork/engine.go`, `admitFork`), BEFORE any verify, allocation, or Firecracker boot, mapped to gRPC `RESOURCE_EXHAUSTED` for the controller; 0 disables it. (3) **Concurrent streams per sandbox.** Capped via `--max-streams-per-sandbox` (see the Connect runtime exec/run_code and interactive Exec rows). Residuals: the memory headroom defaults are sized conservatively but are operator-tunable, not derived from a measured VMM+CoW profile per template (raise the floor if pods are OOM-killed at their configured RAM); the sandbox-count ceiling is per-node, not a global tenant quota; there is no per-tenant fair-share across sandboxes yet (a tenant with many sandboxes still consumes proportionally). |
| Dynamic CPU pinning + launch scheduling priority (issue #168) | **partial (within-cpuset, no new cross-tenant surface; affinity wired, RT class gated)** | Opt-in per pool via `SandboxPool.spec.cpuPinning`. After a fork's guest is ready, the node pins that fork's Firecracker vCPU threads to physical core(s) via `sched_setaffinity` and, during the activate window, bumps their scheduling priority then drops it after ready (`internal/cpupin`). The pin is applied WITHIN the husk pod's cpuset: the topology the planner uses is the pod's allowed CPU set, so a pinned thread can only land on a CPU the pod cgroup already grants. It adds NO new cross-tenant surface and does NOT regress the CoW memcg accounting (#33): affinity (which CPU a thread runs on) is orthogonal to memory accounting (which memcg pages are charged to), and the husk pod still owns both the cpuset and the memcg. The pin is best-effort and fail-open: a pin failure leaves the fork unpinned (floating in the pod cpuset, correct just less dense), never failing the fork. The applier is Linux-gated; the darwin stub is a no-op, so nothing is pinned off Linux. Residuals: the launch-window bump is a nice-level decrease today; the true `SCHED_FIFO` real-time class switch needs `CAP_SYS_NICE` in the pod and a host RT runtime budget and is the gated bare-metal follow-up (an unbounded RT priority would itself be a node-DoS surface, which is why it is gated, not shipped). The planner reads the node topology rather than the pod's exact cpuset today; reading the pod cpuset so the within-cgroup property is enforced in code, not only by construction, is tracked. No density or activate-success number is measured yet; all figures in `docs/perf/cpu-pinning.md` are TARGETS until run on #16. |
| userfaultfd memory restore backend (issue #167) | **mitigated (no new trust boundary; reads only the verified snapshot, writes only registered guest memory)** | Hugepage-backed restore and snapshot-resume prefetch restore guest memory through a userfaultfd backend instead of a file mmap (`internal/fork/uffd_linux.go`). Per restored VM, forkd binds a PRIVATE per-fork unix socket under the sandbox dir and points Firecracker at it via `mem_backend` on `/snapshot/load`; Firecracker connects, creates the userfaultfd over the guest memory, and sends the handler the region mappings (JSON) plus the uffd descriptor (SCM_RIGHTS). The handler then services page faults with `UFFDIO_COPY`, sourcing bytes from the snapshot mem file it mmaps READ-ONLY. Surface analysis: (1) it introduces NO new external/tenant input: the only data it parses is the region-mapping JSON Firecracker itself sends over the private socket (not tenant-reachable; the guest cannot speak to it), and the page bytes come from the SAME mem file the file-backed restore already maps and that the content-addressed, verify-on-load manifest already covers; (2) it makes NO host-path write: it only fills guest pages Firecracker registered, with bytes from the verified snapshot, so it reads nothing the lazy path would not read anyway, just sooner; (3) the preloaded hot-page set is part of the manifest digest, so a tampered set fails verify before any restore (the prefetch design doc reconciles this with the #33 CoW story: prefetched shared pages still count once across forks); (4) it requires a `CONFIG_USERFAULTFD=y` kernel and a hugepage pool (surfaced by `mitos doctor` and docs/platforms/host-prerequisites.md). Residuals: the handler runs in forkd's address space (a handler bug is a forkd bug, same blast radius as the rest of the restore path, not a new privilege); the per-VM socket lives under the node data dir bounded like the other per-fork artifacts; the syscall path (`UFFDIO_COPY`, SCM_RIGHTS recv) has unit-tested pure arithmetic and is validated end-to-end only on a userfaultfd-capable kernel (the Hetzner rescue kernel lacks it), so its KVM integration proof rides the same KVM-gated path as the rest of `internal/fork`. |
| Workspace tar transfer (W4 hydrate/dehydrate) | **mitigated** | The Rust guest agent serves `tar_dir`/`untar_dir` over the gRPC vsock channel; these are NOT exposed on the tenant-facing HTTP sandbox API and are called only by the controller workspace lifecycle. UntarDir (host tar bytes into the guest) rejects absolute, `..`, and out-of-target members with an anchored-separator prefix check, and refuses every non-regular member (symlinks, hardlinks, devices) before any write, so a crafted workspace revision cannot write outside `/workspace` or escape through a symlink. TarDir is allowlisted to `/workspace` only and does not follow symlinks out of it. The dehydrate excludes credential paths (`.ssh`, `.aws`, `.netrc`, `.git-credentials`, `.config/gh`, `.npmrc`) and secrets live only in the guest's in-memory env, never on disk under `/workspace`. Both directions enforce a 64MiB `MaxTarBytes` cap with a per-file `io.LimitReader`. Residuals: a guest already running as root could hardlink an on-disk file into `/workspace` to capture its bytes into a revision (not a cross-VM escape; secrets are in-memory so unaffected); there is no per-transfer member-count cap yet (a low-severity local DoS against a compromised sandbox), to be addressed with the streaming-tar slice. |

## 2. Guest → guest

| Control | Status | Detail |
|---|---|---|
| Separate KVM VMs per sandbox | **mitigated** | No two sandboxes share a kernel. |
| raw-forkd: forks share ONE writable rootfs inode (cross-fork filesystem channel) | **fixed (per-fork rootfs CoW) on raw-forkd** | Previously, on the raw-forkd fork path the template rootfs was hard-linked into each fork's chroot and the rootfs drive was attached with `readOnly=false` (`internal/firecracker/template.go` `AddDrive("rootfs", templateRootfs, false, true)`) and NEVER rebound, so all forks of one template on a node shared a SINGLE writable rootfs inode: a write by one fork was visible to its siblings (and across tenants, since snapshots are node-flat), a cross-fork filesystem read/write channel and a corruption vector. FIXED: `internal/fork/engine.go` now gives each fork its OWN copy-on-write clone of the template rootfs (`prepareForkRootfs` reflink-clones the template rootfs to `<dataDir>/sandboxes/<id>/rootfs.ext4` through the SAME `volume.Backend.ReflinkCopy` owner the husk path uses), loads the snapshot PAUSED (`resume=false`), rebinds the baked `rootfs` drive to that per-fork clone with `PatchDrive` while the guest is frozen, then `Resume`s, exactly mirroring the husk `Stub.Prepare`/`Stub.Activate` fix (section 0 surface 3). The template rootfs is now the READ-ONLY CoW SOURCE; no two forks, and no fork and the source, ever write the same rootfs path. The per-fork clone is hard-linked into the jailer chroot (never the shared template) and reaped with the sandbox dir at Terminate. Proven by `internal/fork/rootfs_cow_test.go` (distinct backing paths, distinct inodes, and a write in one fork's rootfs not visible in a sibling or the template); real-VM cross-fork isolation is KVM-gated (firecracker-test). Residual: raw-forkd is STILL not for untrusted multi-tenant for the OTHER reason below (node-flat snapshots); the privileged-DaemonSet and jailer-disabled reasons are closed since #352 (forkd is non-privileged with the jailer enabled). |
| CoW page sharing side channels | **open** | All forks of a snapshot share read-only pages via `mmap(MAP_PRIVATE)` of the same mem file. Flush+Reload-style attacks across forks of the *same tenant's* snapshot are in scope to document; cross-tenant page sharing must be prevented by never sharing snapshot files across trust boundaries. Not yet enforced anywhere. |
| KSM | **open** | We must mandate KSM off on hosts (we control the reference platform). Not yet documented in any platform guide or checked by forkd at startup. |
| CPU vulnerability mitigations | **open** | Reference hosts (bare metal) must run current microcode with mitigations on; forkd should refuse or warn on `/sys/devices/system/cpu/vulnerabilities` red flags. Not implemented. |

## 3. Sandbox / forkd → cluster

forkd is the highest-value HOST component: uid 0 in a NON-privileged container
with an explicit, minimal capability set, `/dev/kvm` + `/dev/net/tun` via the
device plugin (not a privileged hostPath), and a hostPath to `/var/lib/mitos`,
on every KVM node. Since #352 it is the privileged BUILDER concentrated to that
audited cap set, not a privileged-and-long-lived daemon; its build/raw-forkd
VMMs run jailed under throwaway uids.

| Control | Status | Detail |
|---|---|---|
| controller ↔ forkd authn/authz (mTLS) | **mitigated when deployed as shipped** | The controller bootstraps an internal CA and per-identity leaf certificates as Secrets (`internal/pki`); forkd requires TLS 1.3 client certificates signed by that CA and authorizes only the `controller.mitos` SAN via unary AND stream interceptors; per-identity EKUs prevent the forkd server cert acting as a client. Residuals, explicitly: programmatic insecure construction remains for tests and for deployments that omit the TLS flags (forkd logs a loud warning); no certificate rotation yet; the CA private key lives in a namespace Secret readable by namespace secret-readers. |
| Sandbox HTTP API (exec/files, :9091) | **mitigated** | Per-sandbox bearer tokens are minted at claim time (32-byte crypto/rand), compared in constant time, and fail closed: a sandbox with no registered token rejects everything. Tokens are delivered to clients via claim-owned Secrets, never logged and never in status. On the husk-pod path (#18, slice 2, `--enable-husk-pods`) the SAME `internal/daemon` `SandboxAPI` and bearer-token gate runs IN the pod: after activation the husk stub registers the activated VM + the per-sandbox token and serves the gated exec/files API on the sandbox port, so `Status.Endpoint` (podIP:port) is reachable only with the token. The token is delivered to the stub over the mTLS control channel (the same channel as the activate secrets; never logged, never in argv), so it never crosses an unauthenticated wire. A husk pod serves exactly ONE VM, so the stub runs the `SandboxAPI` in SINGLE-SANDBOX mode (`SetSingleSandbox`, opt-in, set ONLY by `cmd/husk-stub`): the per-sandbox bearer token is the auth gate, validated against the pod's one registered token regardless of the request's `sandbox` id, then routed to that one VM. This is required because the SDK addresses the in-pod API with the claim's `status.sandboxID` (the husk pod name), which never equals the stub's fixed local id; a strict per-id lookup 401s every SDK request (the cluster-e2e bug). The gate is NOT weakened: a wrong/absent bearer is still rejected 401 (constant-time compare), and an activated-but-untokened sandbox still fails closed. forkd NEVER sets single-sandbox mode, so its multi-sandbox per-id lookup is byte-identical: a token for sandbox A cannot authorize sandbox B. The in-pod exec/files surface is the Connect `sandbox.v1.Sandbox` protocol (the legacy `/v1` JSON runtime routes were removed in #358); the PTY upgrade (`ptyAuth`) AND the Connect bearer gate (`connectLookupToken`) BOTH resolve single-sandbox the same way (via `resolveSandboxID`), so cluster-mode exec/files over Connect against a husk pod is gated by the one registered token rather than 401'd by a strict per-id lookup. Proven in `internal/daemon` (`TestSingleSandboxAcceptsArbitrarySandboxIDWithCorrectToken`, `TestSingleSandboxRejectsWrongOrAbsentToken`, `TestSingleSandboxNoTokenFailsClosed`, `TestMultiSandboxModeStillRequiresExactIDMatch`, `TestSingleSandboxPtyAuthIgnoresRequestID`, `TestConnectLookupTokenResolvesSingleSandbox`, `TestConnectLookupTokenMultiSandboxStaysStrict`) and `internal/husk` (`TestActivateSingleSandboxAcceptsSDKPodID`). Residuals: tokens are static per sandbox (no rotation or expiry); anyone with namespace-wide Secret read can take them; standalone sandbox-server runs tokenless by explicit AllowTokenless design. Review finding (med/low): the :9091 sandbox API and its bearer tokens cross the POD NETWORK in cleartext HTTP (the in-pod and forkd sandbox API is plain HTTP, not TLS), so anyone who can observe the pod network sees the token and the exec/file traffic; an in-cluster TLS or service-mesh wrap is a follow-up. |
| Runtime exec/files/run_code/vitals (Connect `sandbox.v1.Sandbox`) | **mitigated (auth + concurrent-stream cap)** | The runtime surface (exec, file IO, run_code, per-sandbox guest vitals) is served by the Connect `sandbox.v1.Sandbox` service on the same `:9091` mux (`internal/daemon/sandbox_api.go`, `internal/sandboxrpc`); the legacy JSON `/v1/exec`, `/v1/exec/stream`, `/v1/run_code/stream`, `/v1/files/*`, and `/v1/vitals` routes were REMOVED once every SDK and kubectl-mitos moved to Connect (#358). AUTH is the per-sandbox bearer token enforced by the Connect `BearerInterceptor` (constant-time compare against the per-sandbox registered token, fail-closed; an untokened or wrong-token call is rejected `CodeUnauthenticated` before any guest RPC; `AllowTokenlessInterceptor` only on the standalone sandbox-server). The interceptor reads the token from the `Authorization: Bearer` header and the sandbox id from `X-Sandbox-Id`, never the body, and never logs or reflects a token value (tested in `internal/daemon/connect_handler_test.go` and `internal/sandboxrpc/interceptor_test.go`). Each streaming RPC opens a DEDICATED vsock gRPC connection to the guest, cancelled on client disconnect (the guest turns the cancel into a process-group SIGKILL). The per-sandbox concurrent-stream count is BOUNDED (production-blocker #2): a NEW stream over the `--max-streams-per-sandbox` ceiling (default 16) is rejected (`too_many_streams`); existing streams are never killed. The cap acquire/release logic is tested in `TestStreamCapAcquireRelease`/`TestStreamCapConcurrent`, and the HTTP-surface rejection on the interactive path in `TestExecWSStreamCapRejected`. run_code in-guest blast radius is unchanged from exec (see the code-interpreter subsection below). |
| `GET /sandbox.v1.Sandbox/Exec` (interactive bidi Exec over a Connect WebSocket, issue #358) | **mitigated (auth); accepted by design (in-guest blast radius, same as exec)** | The interactive terminal is the `sandbox.v1.Sandbox.Exec` schema carried over a WebSocket so the thin half-duplex-over-HTTP/1.1 SDK clients can reach the full-duplex interactive (PTY) case that Connect's HTTP/2-only bidi cannot serve them. The transport is the SAME endpoint path the Connect HTTP handler serves over HTTP/2; this handler (`handleExecWS`, `internal/daemon/exec_ws.go`) takes only the GET WebSocket upgrade (subprotocol `connect.sandbox.v1`) while the Connect handler keeps POST. Each WebSocket binary message carries ONE Connect enveloped frame (a 5-byte header then a protojson `sandbox.v1.ExecRequest` from the client, `ExecResponse` from the server); the client sends an open frame (with `pty` for a terminal), then `stdin` or `resize`, and receives `stdout`/`stderr` chunks then a terminal `exit` frame with the end-stream flag. SECURITY: the upgrade is a bodyless GET, so it is authenticated by `ptyAuth` (the per-sandbox bearer gate: `?sandbox=` + `Authorization: Bearer`, constant-time compare, fail-closed with the SAME semantics as `requireBearer`: no registered token rejects 401 unless `AllowTokenless`; missing/malformed/mismatched token is 401, all BEFORE the upgrade, tested in `TestExecWSRejectsBadToken`/`TestExecWSRejectsMissingToken`/`TestExecWSRejectsCrossSandboxToken`/`TestExecWSTokenlessAllowed`, and the single-sandbox husk resolution in `TestSingleSandboxPtyAuthIgnoresRequestID`), mounted on the outer mux outside `requireBearer`; token values are never logged. The per-sandbox concurrent-stream cap is acquired via `ExecPTY`; because the open frame arrives only after the upgrade, a cap rejection surfaces as a terminal error `exit` frame plus a policy-violation WebSocket close rather than a pre-upgrade 429 (tested in `TestExecWSStreamCapRejected`). The handler takes NO command from the client for the interactive case (the guest defaults to `/bin/sh`); inbound frames are bounded to 1 MiB. The shell runs INSIDE the untrusted guest at the same privilege as any `exec` child, bound by the KVM/Firecracker boundary and the unprivileged husk pod exactly as exec is: NO new host boundary is crossed. Auditing records an `exec_ws` op with the `pty` flag only, never terminal contents (tested in `TestAuditRecordsInteractiveExec`). |
| Connect runtime service audit + exec timeout ceiling (`sandbox.v1.Sandbox`, issue #358 / #216) | **mitigated** | The audit log and the exec/run_code timeout ceiling, which the legacy `/v1` JSON handlers enforced, now apply to the Connect runtime path so they survive the `/v1` retirement. AUDIT: a Connect interceptor (`connectAuditInterceptor`, `internal/daemon/connect_audit.go`) records ONE `AuditEvent` per runtime RPC (unary and streaming) AFTER it completes, carrying ONLY the op string (mapped from the procedure: `exec`, `run_code`, `read_file`, `write_file`, `list_dir`, `stat`, `mkdir`, `remove`, `processes`, `vitals`, `signal`, `port_forward`), the authenticated sandbox id, and OK. It NEVER reads or records the command, argv, env, file path, file content, stdin/stdout, or any token (a test marshals the whole event and asserts no command/argv/env appears); Detail and Bytes are deliberately unset on this path. The interceptor is wired INSIDE the auth interceptor (`connect.WithInterceptors(authIC, auditIC)`, auth outermost), so the sandbox id it records is the one the bearer gate authenticated, not client input; the auditor is read lazily so `SetAuditor` governs it exactly as on the legacy path (NopAuditor = off, the default). Auditing never fails an RPC. CEILING: `Service.MaxExecTimeoutSeconds` (set from the daemon's `maxExecTimeout`, default 86400s; 0 disables) is enforced in `Service.Exec`/`ExecStream`/`RunCode`/`RunCodeStream` (`internal/sandboxrpc`): a requested `timeout_seconds` over the ceiling is REJECTED before the guest stream opens with `connect.CodeInvalidArgument` naming the requested value and the ceiling, never silently reduced (issue #216), so a tenant cannot pin an exec for an unbounded time over Connect. Residual: the audit op is the RPC name plus the sandbox id and OK; per-op Detail (the command for exec, the path for files) is not enriched on this interceptor path yet (a follow-up), so the Connect audit is coarser than the legacy per-handler audit, but no runtime op is unaudited. |
| `POST /v1/sandboxes/{id}/forward` (standalone guest-port forward, issue #228) | **partial (standalone slice: loopback-only, per-sandbox cap, real-mode only; tokenless on the host listener, same as the rest of the standalone server)** | The standalone sandbox-server can expose a guest TCP port to the host: the endpoint opens a host TCP listener and bridges every accepted connection over the guest's gRPC `PortForward` stream (`internal/sandboxrpc/portforward.go` on the host, served by `guest/agent-rs/src/service/portforward.rs` in the guest) to the guest's `127.0.0.1:<guest_port>`, returning the `host:port` the caller dials (`internal/daemon/forward.go`, `cmd/sandbox-server` `handleForward`). It is a new host -> guest surface (a host listener that bridges into the guest), bounded as follows. (1) The host listener binds to LOOPBACK only (`127.0.0.1:0`), so it is reachable only from the host running the server, never from the network. (2) The guest dial is forced to LOOPBACK by the guest agent (`guest/agent-rs/src/service/portforward.rs`): the host carries only a bare port, the guest always dials `127.0.0.1:<port>`, so the tunnel can never be steered to the guest's other interfaces or back out to the host network; an out-of-range or non-loopback target is refused with an LLM-legible error, and a guest port with no listener returns a clean refused result (no hang). (3) The number of concurrent forwards per sandbox is CAPPED (`SetMaxForwardsPerSandbox`, default 16, mirroring the streaming-exec ceiling) so one sandbox cannot exhaust host sockets; each forward and all its tunnels are tracked and closed on `UnregisterSandbox` (terminate), so no host listener or tunnel goroutine outlives the sandbox. (4) It is REAL MODE only: mock mode returns a clean 501 (no guest TCP socket to bridge). The forward path carries NO auth on the host listener: this is the SAME tokenless trust model as the rest of the standalone sandbox-server (`AllowTokenless`, a single-tenant local server), NOT a new weakening; the tunnel bytes are application traffic and are never logged. The forkd/Kubernetes Service+Ingress routing and the CRD template/claim port-declaration fields are EXPLICIT follow-ups of #228 and are NOT built here, so this row covers the standalone surface only; auth on the forward path and UDP are also follow-ups. |
| `ANY /v1/sandboxes/{id}/expose/{port}/` (authenticated guest HTTP proxy, Mitos Expose slice 1, issues #230 and #312) | **mitigated (forkd: per-sandbox bearer gate, constant-time, never logged; standalone: loopback tokenless by design; SSRF allowlist-of-one; streaming not buffered; internet-facing edge proxy, TLS, and subdomain routing are slice 2 and NOT part of this surface)** | The expose route reverse-proxies an HTTP request (any method: GET for SSE, POST for agent RPC) to the guest's `127.0.0.1:<port>` over the existing vsock PortForward tunnel, streaming the response immediately (`FlushInterval -1`, no output buffering), so SSE sessions work without batching. Code: `internal/daemon/expose_conn.go` (net.Conn over the tunnel), `internal/daemon/expose.go` (`ProxyHTTP`), `internal/daemon/expose_route.go` (`checkBearer`, `handleExpose`, `HandleExpose`). The surface is bounded as follows. (1) AUTH on forkd: the per-sandbox bearer token is validated by `checkBearer` with a constant-time compare (`subtle.ConstantTimeCompare`); a missing, malformed, or wrong token is rejected 401. The token VALUE is never logged and never appears in an error body; only the sandbox id and port are logged. `checkBearer` is mounted OUTSIDE the body-peeking `requireBearer` middleware so the auth gate never buffers the request body: a large or streaming POST body is not read before auth completes and is not held in memory. (2) STANDALONE: inherits the same loopback tokenless trust model as the rest of the standalone sandbox-server (`AllowTokenless`), the same as the `/forward` path; this is not a new weakening. (3) SSRF: the guest dial is forced to loopback by the guest agent (`guest/agent-rs/src/service/portforward.rs`): the host carries only a bare integer port parsed from the URL path, the guest always dials `127.0.0.1:<port>` and refuses any non-loopback target or out-of-range port; the host never derives the dial target from any user-supplied input beyond the port integer, so there is no SSRF path to steer the tunnel to another guest interface, a host interface, or the host network. Port is range-checked to 1-65535 before the tunnel is opened. (4) Each request opens its own vsock tunnel and guest TCP connection and tears it down on close; there is no multiplexing and no shared state across requests. (5) DEFERRED to slice 2 and explicitly NOT part of this surface: the internet-facing edge proxy, TLS termination, the `<label>.<expose-domain>` subdomain scheme, the Host allowlist and reserved-name check, the wildcard certificate, the controller route-sync CRD fields, and the `internal/preview` to `internal/expose` package rename. This row covers only the forkd `:9091` and standalone sandbox-server paths; the public ingress surface is the section 7c area (preview URL rows). CONCURRENCY CAP ABSENT: the per-sandbox concurrent-tunnel cap and force-close-on-terminate that the `/forward` path enforces (`SetMaxForwardsPerSandbox`, tunnel tracking, `UnregisterSandbox` teardown) are NOT yet applied to this path; bounding concurrent expose tunnels per sandbox is a slice-2 follow-up, sequenced with the #213 abuse-control envelope. |
| Sandbox API error responses (`internal/apierr` envelope) | **addressed** | Runtime error responses from the forkd sandbox API and the standalone sandbox-server use the LLM-legible envelope `{error:{code, message, cause, remediation}}` (`internal/apierr`), so a caller gets a stable machine code and an actionable next step instead of an opaque string (issue #28). The `cause` is built from sandbox ids, paths, and operation names only (an exec/file failure surfaced from the guest agent or fork engine, a fixed string for the auth and bad-request paths); tokens and secret values never appear in any field. The `requireBearer` gate never echoes the presented token: its 401 cause is a fixed string, never the request header. CI asserts every error path carries a non-empty `code` and `remediation` (`internal/apierr/apierr_test.go`, `internal/daemon/error_envelope_test.go`). The Python and TypeScript SDKs additionally redact any bearer token a misconfigured server might reflect into a body before it becomes the client-side error cause. |
| forkd capability minimization | **mitigated as shipped (#352); kernel-enforced cap/uid drop proven in KVM CI (issue #2 Task 5)** | The SHIPPED DaemonSet (`deploy/daemon/daemonset.yaml`) and the Helm chart run forkd NON-privileged: `privileged: false`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, `capabilities.drop: [ALL]` and add back ONLY the explicit builder set `SYS_ADMIN`, `CHOWN`, `SETUID`, `SETGID`, `MKNOD` (the jailer) plus `NET_ADMIN` (the build-time placeholder tap). That set is the single source of truth in `cmd/forkd/jailer.go` (`forkdRequiredCapabilities`, derived from `jailerRequiredCapabilities`), and the conformance suite `cmd/forkd` (`TestShippedDaemonSetIsNotPrivileged`, `TestShippedDaemonSetHasExactJailerCapabilities`, `TestShippedDaemonSetHardensSecurityContext`, `TestShippedDaemonSetGetsKVMViaDevicePlugin`, `TestShippedDaemonSetEnablesJailer`) fails in the darwin unit run if a regression re-adds `privileged: true`, widens the cap set, drops the jailer flags, or re-introduces the privileged `/dev/kvm` hostPath. `/dev/kvm` and `/dev/net/tun` come from the device plugin (`mitos.run/kvm`); the device cgroup is scoped by the kubelet, which is what removes the need for `privileged`. The jailer is ENABLED, so a guest escape from a raw-forkd or build VM lands as a THROWAWAY jailed uid in a per-VM chroot inside a non-privileged container, not root in a privileged one. Residual: the container is still uid 0 and holds CAP_SYS_ADMIN (needed for the chroot-base mount setup and the jailer), and the data-dir hostPath remains, so a successful escape that ALSO defeats the jailed uid and CAP_SYS_ADMIN boundary is still a serious node event, just no longer trivial full-node root; the kernel ENFORCING the bounding-set/uid drop is asserted on the KVM runner (the CI host is root and cannot observe it). raw-forkd stays not-for-untrusted-multi-tenant for the REMAINING reasons (node-flat snapshots; see docs/adr/0005-raw-forkd-not-multitenant.md), the privileged-container reason is now closed. |
| Blast radius documentation | **mitigated (#352)** | This document. A forkd compromise is now uid 0 in a NON-privileged container with the explicit builder capability set (no `privileged: true`), `/dev/kvm`/`/dev/net/tun` via the device plugin, and a hostPath to the node data dir, plus the ability to read every snapshot and secret passed to it. The build/raw-forkd VMMs run jailed under throwaway uids, so a VMM escape no longer lands as forkd's root. The residual blast radius (CAP_SYS_ADMIN, uid 0, data-dir hostPath) is documented in the forkd capability minimization row above. |
| forkd crash recovery / orphan-VM leak on restart (issue #12) | **mitigated (artifact reap + re-adopt; unit-verified on darwin); real-VM reap on KVM is a TARGET pending a kvm-test.yaml crash-reap phase** | forkd tracks live VMs in an in-memory map, so before this change a forkd crash + restart lost all knowledge of its own pre-crash Firecracker processes: they kept running (consuming CPU, memory, `/dev/kvm` slots, jailer uids, disk) while `ListSandboxes` reported zero, so the controller GC could not see or reap them and they leaked until node reboot (a node-level availability/DoS surface, NOT a cross-tenant escape). forkd now persists a minimal per-VM journal record at `<dataDir>/sandboxes/<id>.json` (atomic temp+rename) when a VM reaches running and removes it on clean terminate; `NewEngine` reconciles the journal before serving. A record whose pid the PID-recycle guard confirms is still OUR live Firecracker (`/proc/<pid>/exe` resolves to the recorded firecracker binary, or comm is `firecracker` when exe is unreadable under the jailed uid) is re-adopted into the live map so `ListSandboxes` reports it and the controller GC reconciles it against the CRDs (terminating it if no live claim, directly from the recorded pid + jailer chroot + uid + network identity). A dead pid, or a recycled pid running an UNRELATED program, is treated as gone: its leaked artifacts (jailer workspace incl. chroot + rootfs CoW clone, sandbox dir, fork network tap/ruleset/identity, jailer uid, volume backings) are reaped best-effort and idempotently, and its record dropped. The PID-recycle guard is the safety property: a wrong kill of an unrelated reused pid is prevented because reconcile never SIGKILLs on the startup reap path (a dead pid has nothing to kill; a recycled pid is not ours), and adoption (which does enable a later kill via the GC) happens ONLY for a verified-our-firecracker pid. The later GC-driven kill is the most dangerous operation here: an adopted firecracker is re-parented to init across the crash (not a child of the restarted forkd), and Terminate runs a full GC interval after adoption, so between the two the adopted VM can exit on its own and its pid be recycled to an unrelated process on a busy node. To close that adoption-then-kill TOCTOU, `reapAdopted` RE-RUNS the SAME PID-recycle guard against the recorded firecracker binary immediately before signalling and skips the kill when the pid no longer resolves to our firecracker (artifacts are still reaped). The adopted VM's exact /30 network block is pinned from its recorded guest IP via `netconf.Allocator.MarkInUse` rather than re-Acquired, so the empty post-restart allocator cannot hand the same /30 to a fresh fork and Release frees the right block. Reconcile is fail-open (a single malformed record never blocks startup) and logs counts + ids/paths only, never secrets. The journal carries ids/pids/host paths/uids/IPs only; never env, secrets, or tokens. Verified on darwin via injected pid/verifier seams (`internal/fork/reconcile_engine_test.go`, including the reap-adopted recycled-pid skip and the network-block pinning) and `internal/netconf/identity_test.go`; the real Firecracker kill + chroot/CoW/network reap on KVM (start a sandbox, kill -9 forkd, restart forkd, assert the orphan FC is reaped or re-adopted + GC-terminated with no leaked process/chroot/uid) is a TARGET pending a kvm-test.yaml crash-reap phase (issue #12). Residual: a re-adopted orphan the GC terminates does not yet stamp a typed claim condition explaining the pre-crash origin. |
| Git rendezvous egress (W4 outputs) | **mitigated (arg injection); mitigated (credentialed egress); open (external-endpoint egress) (documented)** | A claim `spec.outputs` `{git}` entry is a NEW EGRESS: on terminate the control plane (the claim reconciler today; the node-side transfer path when wired) materializes the workspace `spec.git.paths` content (tenant data) into a commit and pushes it to an operator-declared external git remote on a per-attempt branch. The remote URL is operator-declared in the Workspace/output spec (not tenant-controlled), git is the merge layer (the engine only pushes a branch, it never merges working trees), and the secret exclude list still strips credential paths before any capture so a push carries repo content only. CONFIRMED arg-injection RCE (now closed): the push ran `git push <remote> <branch>` with no `--` separator, so a remote of `--receive-pack=<cmd>` was parsed by git as a FLAG and ran an arbitrary command on the pushing (controller) node, exploitable regardless of git version. Mitigations now in place: (a) the push uses a `--` separator (`git push -- <remote> <branch>`), so a flag-shaped remote lands as a positional and cannot inject an option; (b) `RenderBranch` rejects a rendered branch beginning with `-`, so a custom branch template cannot inject a flag even past the separator (defense in depth); (c) the push environment sets `GIT_CONFIG_NOSYSTEM=1` and points `HOME` at an empty temp dir, so ambient host git config (a controller image `~/.gitconfig` or `/etc/gitconfig`) cannot re-enable the `ext::`/`fd::` transports or alter push behavior; (d) the API enforces a `+kubebuilder:validation:Pattern` on `GitOutput.Remote` restricting it to `https://`, `http://`, `ssh://`, `git://`, `file://`, and scp-like `git@host:path` forms, rejecting flag-shaped and `ext::`/`fd::` values at admission. `ext::` is also disabled by default in git >= 2.38.4. These defenses close the arg-injection even for a misconfigured or compromised operator input. The operator-declared remote remains a high-trust boundary by design. **Rendezvous CREDENTIALS are now modeled (W4 Phase 3).** A push to a real external remote authenticates with a token from `Workspace.spec.git.credentialsSecretRef` (a referenced Secret key in the workspace namespace, resolved by the controller). The token is a SECRET VALUE and is handled per the secrets policy: (i) it NEVER appears on the git argv (so it is absent from the process table), in a log line, in an error, in a claim/revision condition, or in a committed revision; (ii) it reaches git ONLY through a mode `0o600` `.git-credentials` file written into an ephemeral, isolated `HOME` created per push and removed when the push returns (`credential.helper=store` reads only that file); (iii) any git output surfaced in an error is scrubbed of the token defensively; (iv) a missing/empty key yields an LLM-legible error that names only the Secret and key, never the value; (v) credentials require an `http(s)` remote that can carry basic auth (a `file://`/scp-like remote with credentials is rejected). Redaction on the failure path is asserted in `internal/workspace/git_test.go` and the token-never-in-conditions invariant in `internal/controller/workspace_binding_test.go` BEFORE the credential code merges. The matching authenticated remote is `internal/rendezvous` + `cmd/rendezvous-server`: a git-http server wrapping `git http-backend` behind HTTP basic auth (constant-time token compare; the server's token is mounted from a Secret via `-token-file`/`RENDEZVOUS_TOKEN`, never an argv flag, never logged), enabling `receive-pack` only after the auth check passes and rejecting traversal in the repo path. The operator may also point the output at any standard authenticated git remote. Residual EGRESS surface, explicitly: the push target is an EXTERNAL endpoint, so tenant repo bytes leave the cluster to wherever the operator pointed the remote (a high-trust, operator-declared boundary by design; the credential just authenticates that egress, it does not bound where it goes). The push to a REAL external server on a live cluster is the gated e2e tail (`test/cluster-e2e/workspace-e2e.sh`). A `{git}` output without `spec.git.paths` is a no-op; a push failure surfaces on the claim/revision condition and is retried, never silently swallowed. |
| Revision change feed egress (W4 slice 4) | **mitigated** | The controller emits CloudEvents (`dev.mitos.workspace.revision.created`, `dev.mitos.sandbox.phase.changed`) to an OPT-IN operator-configured webhook (`--event-sink-url`; empty disables it, leaving only on-cluster Kubernetes Events) and mirrors each as a Kubernetes Event. Payloads carry IDENTIFIERS only (workspace/revision names, the content-manifest DIGEST, lineage refs, phase transitions), never secret values, env, or file content, so the feed leaks metadata to wherever the operator points the sink, not tenant data. Delivery is at-least-once with the event id (object UID plus a sequence) as the idempotency key so an indexer dedupes; the webhook URL is operator-config (an SSRF-shaped surface like the git remote, the same high-trust boundary). Residual: no payload signing/auth on the webhook yet (the operator must trust the sink endpoint); NATS and the reference indexer are out of scope. |
| Console audit-sink egress (console B3b) | **mitigated (metadata-only, https-only, best-effort); open (dispatch bounding + full SSRF egress allowlist)** | The console BFF can forward each org audit event to org-admin-configured external sinks (webhook today; s3/splunk/datadog accepted as config types with drivers as follow-ups) via the `SinkRegistry` + `DispatchingRecorder` (`internal/saas/console/audit_sinks.go`). Payloads carry the `AuditEvent` IDENTIFIERS only (org id, actor id, action, target, a non-secret detail string, timestamp), never a secret value, env, or file content, so the feed leaks audit METADATA to wherever the org admin points the sink, not tenant data; `SinkConfig` stores no credential field and no endpoint or error is logged with a secret. Delivery is BEST-EFFORT: a sink failure is logged and never fails the audit `Record` or the user action. The sink endpoint is validated to require an `https://` URL with a non-empty host at `POST /console/audit/sinks` (rejecting `http` and other schemes), which blocks the obvious SSRF-to-internal-http vector; the endpoint remains an admin-config SSRF-shaped surface like the git remote and the revision feed above. The registry is org-scoped (a cross-org sink is `not_found`) and every sink endpoint is added to the auth-gate table. Residual: no payload signing/auth on the sink yet (the admin must trust the endpoint); the per-event dispatch is unbounded goroutine-per-sink (a bounded worker pool is a tracked follow-up); full SSRF egress allowlisting (blocking private/loopback ranges) and the non-webhook sink drivers are follow-ups. |
| Memory-snapshot pairing (W4 resumable head) | **mitigated** | A workspace head can be paired with a VM MEMORY snapshot (`WorkspaceRevision.memorySnapshotRef`), which captures guest RAM and therefore CAN contain secrets that were delivered into the guest. The pairing is PRINCIPAL-BOUND: on a checkpoint-on-terminate the new revision records the CAPTURING claim's principal (`memorySnapshotPrincipal` = its `ServiceAccount`); `status.resumable` is true only when the head's snapshot still exists AND is verified principal-bound; and a new claim's activation REFUSES to restore a memory snapshot whose principal does not match the activating claim's principal (a cross-principal resume is rejected fail-closed, never silently downgraded to a cold start). So a memory image is never served to a principal other than the one that created it. The refusal is enforced at TWO layers: the controller's `maybeResumeMemory` (`internal/controller/workspace_binding.go`), and the `WorkspaceMemorySnapshotAdapter` (`internal/controller/workspace_memory_snapshot.go`) which refuses any cross-principal resume/exists it is asked for directly even if a caller bypassed the upstream check. The whole flow (checkpoint pairs the snapshot; the head becomes resumable; a same-principal claim resumes; a DIFFERENT-principal claim is refused and never resumed/hydrated) is proven END TO END in envtest, `internal/controller/resumable_envtest_test.go` `TestResumableHeadFromMemorySnapshot` (the `sa-b` intruder case asserts the refusal fires and the resume seam is never reached for the intruder), plus adapter unit tests (`workspace_memory_snapshot_test.go`). Wiring the seams to a real memory snapshot is gated behind the controller `--workspace-memory-snapshots` flag; off by default a checkpoint-on-terminate FAILS LOUD rather than producing a revision falsely marked resumable (no fabricated snapshot, no-unverified-claims). PRINCIPAL AUTHENTICITY: the principal is the claim's `spec.serviceAccount`, which a claim author sets freely, so the equality check above is only an authorization boundary if that field is bound to the creator's identity. The validating admission webhook `internal/admission.ClaimServiceAccountValidator` (controller `--enable-principal-webhook`, Helm `admissionWebhook.enabled`) closes that gap: on claim CREATE/UPDATE it runs a SubjectAccessReview with the request creator's identity and rejects the claim unless the creator may `impersonate` the named ServiceAccount, so a tenant cannot assert another principal's value and resume its secrets-in-RAM. It is off by default (single-tenant and webhook-less installs unaffected) and STRONGLY RECOMMENDED whenever memory snapshots are enabled multi-tenant; the controller logs a warning if `--workspace-memory-snapshots` is on without it. The webhook handler + SAR decision are unit-tested (`internal/admission`); the cert wiring and a webhook admission e2e are the remaining gate before it is relied on, and it touches the authz boundary so needs a named human reviewer. Residual: the memory snapshot at rest inherits the snapshot store's encryption (#31, the per-workspace key in section 5 now extends to the workspace store); the real VM-memory restore runs on a KVM-capable kubelet (the cluster-gated in-VM tail), while the pairing decision and the principal check are object-level proven in envtest. |
| Workspace S3 object-store egress (W4 Phase 4) | **mitigated (credentials); open (external-endpoint egress) (documented)** | A workspace may select an S3-compatible object store (`Workspace.spec.store.s3` + `objectStorageRef`) as the content-addressed backend for its hydrate/dehydrate revision artifacts, an ALTERNATIVE to the node CAS. This is a NEW EGRESS: tenant workspace content (chunked, content-addressed) leaves the cluster to the operator-declared bucket. Bucket and endpoint are operator-declared (not tenant-controlled). CREDENTIALS are handled per the secrets policy: the access-key id and secret-access-key come from `s3.credentialsSecretRef` (a referenced Secret), the secret-access-key is a SECRET VALUE used ONLY to derive the SigV4 signing-key chain (`internal/workspace/s3client.go`), and it NEVER appears on the wire in cleartext, in a log line, in an error, in a condition, or in a committed object. The signed request carries only the SigV4 signature and the (non-secret) credential scope; the secret-never-on-the-wire invariant is asserted in `internal/workspace/s3client_test.go`. The store is plaintext CONTENT-ADDRESSED exactly like the node CAS (same manifest digest for a tree), so it is a drop-in that preserves byte-identical round trip and chunk-level dedup (`TestS3DigestMatchesNodeCASDigest`, `TestS3DedupsByChunkDigest`), and it composes with per-workspace encryption so the at-rest bucket objects are ciphertext (`TestS3EncryptedRoundTrip`). Residual EGRESS surface, explicitly: bytes leave the cluster to wherever the operator pointed the bucket (a high-trust, operator-declared boundary by design); the credential authenticates the egress, it does not bound where it goes. The node CAS stays the default when no `objectStorageRef` is set; the live round trip is the gated e2e tail. |
| Node CAS: tenant-writable, unbounded, integrity-DoS (W4) | **open (high)** | The node content-addressed store (`<dataDir>/cas`) is mounted READ-WRITE and shared per node across all husk pods (`huskCASMountPath`, `internal/controller/huskpod.go`), so a guest that escapes its VM into the husk pod can write, delete, or corrupt other tenants' committed-revision chunks on the same node. Content-addressing protects INTEGRITY of what is read (the read side verifies the digest, so a forged chunk is rejected; and the manifest decoder now validates each chunk digest is a well-formed sha256 before it is ever used as a chunk path, `internal/cas.decodeManifest` calling `Digest.Validate`, so a malformed manifest from the untrusted peer-pull transport, `internal/cas/http_transport.go`, cannot traverse out of the chunk store (`chunkPath` joins the digest into a path) to stream an arbitrary host file to the output before the post-read verify, nor panic `string(d)[:2]` on a sub-2-char digest), but it does NOT prevent DESTRUCTION: a compromised pod can delete or truncate another tenant's chunks, a cross-tenant AVAILABILITY attack. The store is also UNBOUNDED in production: `internal/cas.EvictToFit` exists but is called from NOWHERE outside its own test (`internal/cas/evict.go`, `evict_test.go`), so node disk is uncapped and a tenant can fill it (node-disk DoS). Separately, fork-child activation uses `--allow-unverified-snapshots` (the live fork snapshot is not content-addressed, `internal/husk/control.go`, `stub.go`), so a node-local attacker who escaped a VM could tamper a fork artifact a sibling pod loads UNVERIFIED. Required fix: wire `EvictToFit` (or another disk cap) into production, scope the CAS write surface per tenant (or per node with destruction protection), and remove the unverified fork-load path or bind it to a digest. |
| Husk pod default ServiceAccount token automount | **mitigated (shipped)** | The husk pod spec now sets `AutomountServiceAccountToken: false` (`internal/controller/huskpod.go` `buildHuskPod`), so the kubelet does NOT mount the namespace default ServiceAccount token into a husk pod. The stub speaks vsock + mTLS and never calls the Kubernetes API (verified: no client-go, no `InClusterConfig`, no SA token read in `cmd/husk-stub` or `internal/husk`), so the token was dead weight; a guest that escapes into the stub no longer inherits a free `system:authenticated` token. The opt-out applies to BOTH warm pods and fork-child pods, which share the builder, proven in `TestBuildHuskPodDisablesSATokenAutomount`. |
| forkd gRPC fails OPEN without TLS flags | **mitigated (shipped)** | `grpcServerOptions` (`cmd/forkd/main.go`) now FAILS CLOSED: with no TLS flags it returns a fatal error naming the missing `--tls-cert/--tls-key/--tls-ca` flags and the opt-in, so forkd refuses to start unauthenticated. The legacy insecure-with-loud-warning behavior is reachable only behind an explicit `--allow-insecure-grpc` opt-in (default false) for local development. A partial TLS triple is still a configuration error. The shipped DaemonSet always sets TLS, so production is unaffected; this only stops a silent-insecure misconfig. Proven in `TestGRPCServerOptionsFailClosed` (refuses with no TLS and no opt-in; allowed with the opt-in; allowed with a full TLS triple; partial is an error). |
| Host-side vsock read has no deadline | **mitigated (shipped)** | The host-side `vsock.Client` now applies a per-request read deadline in `send` (`internal/vsock/client.go`), defaulting to `DefaultRequestTimeout` (60s) and overridable via `SetRequestTimeout`. The CONNECT preamble reads in `Connect`/`DialStream`/`DialStreamUnix` are likewise bounded (then cleared so the long-lived streaming reads stay ctx/`conn.Close`-cancelled). A malicious or wedged guest that connects then stalls (or dribbles a partial line under the `MaxMessageBytes` cap) now causes the one-shot call to return a timeout error rather than hang the host caller goroutine, vsock fd, and (for the husk stub) stream slot indefinitely. Proven in `TestSendReadDeadlineUnblocksOnStall` (a fake agent that connects then never responds makes `Ping` return rather than hang). |
| Husk egress enforcement | **mitigated (IMPLEMENTED and KVM-VERIFIED end to end)** | See section 0 surface 5 and section 4. The husk default path enforces an in-pod nftables default-deny egress filter in the pod's own netns (CNI-independent, the guarantee), an unconditional cloud-metadata block (`169.254.169.254`, `169.254.0.0/16`, IPv6 `fd00:ec2::254`, before any allow), and the threaded per-template allowlist with a per-pod DNS proxy (failover upstream list, recommended `1.1.1.1:53,8.8.8.8:53`, not cluster DNS); a best-effort controller-emitted NetworkPolicy adds defense in depth (CNI-dependent, cannot express name-based allows). Costs one scoped `NET_ADMIN` capability (ADR 0006) plus, on name-egress pools only, a short-lived privileged `enable-ip-forward` init container that sets `net.ipv4.ip_forward=1` in the pod netns and exits (one-shot; no node prerequisite; the long-lived husk-stub container stays unprivileged). VERIFIED: the husk-network KVM cluster e2e (`test/cluster-e2e/husk-network-e2e.sh`, `cluster-husk-network-e2e`) PASSES on the Hetzner Talos KVM cluster; the claim reaches Ready and all three in-VM enforcement assertions are green inside a real restored VM (metadata-blocked exit 1, default-deny exit 1, allowlist-works `example.com` connect exit 0), verified twice (with and without a node sysctl allowance), proving NO node prerequisite. |

### Code interpreter (run_code) surface

`run_code` (the Connect `sandbox.v1.Sandbox.RunCode` RPC; the legacy `POST
/v1/run_code/stream` JSON route was removed in #358) runs tenant code in a
long-lived Python kernel (`/opt/mitos/kernel_driver.py`, ipykernel)
that the guest agent spawns lazily and keeps for the VM lifetime. Status:
**mitigated** for host isolation, **partial** for in-guest blast radius.

- The kernel runs INSIDE the untrusted guest VM, at the same privilege as any
  `exec` child. It crosses no new host boundary: the KVM/Firecracker boundary
  and the unprivileged husk pod (section 0) bound it exactly as they bound
  `exec`. The host treats kernel output (frames) as data only.
- It is a PERSISTENT interpreter holding tenant state across calls. Within one
  sandbox this is by design (statefulness is the feature); across tenants there
  is no sharing because each sandbox is its own VM.
- Fork inheritance: a forked VM inherits the live kernel and its namespace (it
  is part of the snapshot). This is the same fork-shared-state surface as any
  in-VM process; the RNG/clock caveat is in docs/fork-correctness.md.
- Optionality: a base image without the kernel returns a KernelUnavailable
  error frame (exit 127); no new attack surface exists on minimal images, and
  plain `exec` is unaffected.
- The kernel driver reads only newline-delimited JSON {id, code} on its stdin
  from the agent, never from the network, so there is no kernel-protocol
  exposure beyond the existing vsock/HTTP authz (per-sandbox bearer token).

Residual (open): the kernel inherits the configured env+secrets exactly as
`exec` does; secret values are never logged in frames (only stdout the tenant
itself prints, which is the tenant's own choice). No CPU/memory cgroup bounds
the kernel beyond the VM's own limits.

### Interactive terminal surface (`GET /sandbox.v1.Sandbox/Exec` over WebSocket)

The interactive terminal is the `sandbox.v1.Sandbox.Exec` bidi RPC carried over a
WebSocket upgrade (subprotocol `connect.sandbox.v1`, `handleExecWS`,
`internal/daemon/exec_ws.go`); it bridges to a dedicated vsock gRPC Exec stream on
which the guest agent allocates a pseudo-terminal and starts `/bin/sh` as a
session leader. This is a LIVE interactive shell into the VM: stdin frames flow
client to guest and stdout/stderr frames flow back, for as long as the connection
is held. The bespoke `/v1/pty` JSON WebSocket wire was removed in #358; the row
above (`GET /sandbox.v1.Sandbox/Exec`) is the authoritative surface entry. The
threat properties carry over unchanged:

- Authentication. The upgrade is a bodyless GET, so it does NOT pass through
  the JSON-body-peeking `requireBearer` middleware. `handleExecWS` authenticates
  the upgrade itself (`ptyAuth`): the sandbox id comes from the `?sandbox=` query
  parameter and the token from the `Authorization: Bearer` header, compared in
  constant time. Semantics match `requireBearer`: a sandbox with no registered
  token fails closed with 401 (unless `AllowTokenless`, the standalone
  sandbox-server only); a missing, malformed, or mismatched token is 401. Token
  values are never logged. Status: mitigated (same per-sandbox token custody
  as exec/files).
- Process containment. The shell runs in its own session and process group;
  a host hangup (WebSocket close, ctx cancel, or vsock drop) cancels the guest
  Exec stream, which the guest turns into a process-group SIGKILL, so a terminal
  session and its children do not outlive the connection. Status: mitigated.
- No command injection at the edge. `handleExecWS` does NOT take the shell
  command from the client for the interactive case; the guest defaults to
  `/bin/sh`. Only the PTY window size crosses from the open frame. The shell, of
  course, can run any command the guest user can, exactly like exec; the terminal
  does not widen the in-guest capability set, only the interactivity of access.
  Status: accepted by design, identical to the exec surface.
- Concurrent-session cap. The terminal holds a dedicated vsock connection for
  the session lifetime, so it counts against the SAME per-sandbox
  concurrent-stream ceiling as exec and run_code (production-blocker #2):
  `handleExecWS` reserves a slot via `ExecPTY`; because the open frame arrives
  after the upgrade, a session over the `--max-streams-per-sandbox` ceiling is
  refused with a terminal error `exit` frame plus a policy-violation close (not a
  pre-upgrade 429); existing sessions are never killed. So a tenant cannot open
  unbounded terminals to exhaust host connections and goroutines. Status:
  mitigated (tested in `TestExecWSStreamCapRejected`).
- Residual. The terminal inherits the exec surface's residuals (the in-guest
  user is unconfined within the VM; isolation is the microVM boundary, not
  in-guest privilege separation). It adds no new host-side privilege. The auditor
  records an `exec_ws` op with the `pty` flag only, never terminal contents.

## 4. Sandbox → network

See `docs/networking.md` for the full design (tap-per-sandbox, nftables
dispatch model, per-fork identity). Networking is opt-in: with forkd's
`--enable-networking` off, restored VMs have no NIC and egress is denied by
absence. With it on, each fork gets its own tap and a host-side default-deny
egress ruleset.

PER-MODE ENFORCEMENT (reconciled with the husk default, section 0 surface 5).
The nftables egress dataplane described in the rows below is the SAME rendering
(`internal/netconf`) used in both modes. In RAW-FORKD mode (`--enable-raw-forkd`)
with `--enable-networking` on, it is applied host-side in forkd's netns; those
rows are CI-proven in-VM on KVM. On the HUSK DEFAULT path (the shipped default)
the SAME rendering is applied IN-POD by the husk-stub in the pod's OWN netns at
activation (`internal/husk/netfilter.go`, `internal/husk/stub.go`), so a husk
sandbox gets a CNI-independent default-deny egress chain, the unconditional
cloud-metadata block, and the threaded per-template allowlist;
`169.254.169.254` (and the v4 link-local range and IPv6 IMDS) is dropped before
any allow. Name-based egress runs the in-pod DNS proxy that resolves only
allowlisted names and pins each resolved IP into the per-tap allow set, forwarding
to a comma-separated failover upstream list (recommended `1.1.1.1:53,8.8.8.8:53`,
NOT cluster DNS); allowed traffic egresses via an nftables SNAT masquerade scoped
to the guest source plus IPv4 forwarding, the latter enabled by a short-lived
privileged `enable-ip-forward` init container (name-egress pools only, no node
prerequisite). The controller additionally emits a best-effort
`networking.k8s.io/v1` NetworkPolicy selecting `mitos.run/husk=true`
(`internal/controller/husknetworkpolicy.go`); HONEST CNI caveat: a NetworkPolicy
enforces only on a CNI that implements it and cannot express name-based allows, so
the in-pod nft filter is the guarantee and the NetworkPolicy is defense in depth.
The earlier CI step that hand-applied an allow-all (`0.0.0.0/0`) policy proved no
restriction and is superseded. This in-pod enforcement is IMPLEMENTED and
KVM-VERIFIED end to end: the husk-network KVM cluster e2e
(`test/cluster-e2e/husk-network-e2e.sh`, `cluster-husk-network-e2e`) PASSES on the
Hetzner Talos KVM cluster, the claim reaches Ready, and all three in-VM
enforcement assertions are green inside a real restored VM (metadata-blocked,
default-deny, allowlist-works), verified twice (with and without a node sysctl
allowance), proving NO node prerequisite. The raw-forkd in-VM KVM proof is
unchanged. See `docs/husk-pods.md` section 6d.

| Control | Status | Detail |
|---|---|---|
| Egress default-deny (IP:port) | **partial / mitigated** | Enforced host-side for literal IP:port allowlist entries. Each fork sits on its own tap with its own /30; a shared `inet` nftables table dispatches by inbound interface (the tap) into that sandbox's regular chain, which accepts established/related, the allowlisted `ip daddr/tcp dport` pairs (each pinned to the sandbox's `ip saddr` as anti-spoof), and ends in a terminal drop. The guest cannot influence the host ruleset and cannot spoof another sandbox's source address onto its own tap. Proven in KVM CI: one VM reaches an allowed destination and is blocked from a denied one, plus a two-sandbox `nft` install proving cross-tap isolation (one sandbox's drop never kills another's allowed traffic). |
| Host-side enforcement | **enforced** | Egress policy is rendered and applied host-side only (`nft` per tap), never in-guest. The guest agent never edits nftables; the guest's only network config is its own eth0 address. |
| DNS-based allowlists (name egress) | **partial / enforced** | Names like `api.anthropic.com:443` are now enforced through a controlled per-node resolver (`internal/dnsproxy`, #47, behind `--enable-dns-egress`). The guest's only resolver is the node resolver IP (`169.254.1.1`, written into the guest `/etc/resolv.conf`). The proxy resolves ONLY names on that sandbox's allowlist, and for each resolved record pins `(ip . port)` into that sandbox's nftables timeout set; the guest can then reach exactly the address it resolved, for exactly the allowed ports, for `max(recordTTL, 30s)`. A name not on the allowlist gets REFUSED and nothing is pinned. **Allowlist names: exact OR anchored suffix wildcard.** An entry is matched exactly (case-insensitive, trailing-dot tolerant) OR, when it is written `*.D`, by the ANCHOR RULE: the query must end with `.D` and carry a NON-EMPTY label before that `.D`, so `*.example.com` matches `a.example.com` and `a.b.example.com` but NEVER the apex `example.com`, NEVER a look-alike (`notexample.com`, `evilexample.com`, `xexample.com`), and NEVER a name that carries `D` only as a non-suffix label (`example.com.evil.com`, `a.example.com.evil.com`). The match is a LITERAL anchored suffix check (`strings`-level, no regex); this anchor is the load-bearing guarantee and is exhaustively bypass-tested. A wildcard is validated at the boundary where the template `networkPolicy` names build the allowlist (`ParseNameAllowList`): it must be exactly a single leading `*.` plus a valid domain, so `*`, `*.`, `*foo.com`, `a.*.com`, `**.com`, and multi-star names are REJECTED, never silently treated as a literal. **AAAA/IPv6.** The proxy now also forwards AAAA and pins each resolved v6 address into a SEPARATE per-sandbox v6 nftables timeout set (`ipv6_addr . inet_service`), and each per-sandbox chain carries a v6 default-deny (`meta nfproto ipv6 drop` under `egress: deny`), so an unpinned v6 destination is dropped rather than falling through to the base chain's accept; v6 egress is therefore enforced by the same resolve-then-pin model as v4. Honest v6 scope: the guest is assigned only a v4 `/30` source identity today (no v6 source address), so the v6 accept is NOT `ip saddr` anti-spoof-pinned the way the v4 accepts are; in single-stack guests this is moot (the guest cannot source v6), and the dataplane fails closed regardless because of the v6 default-deny. Exact and wildcard entries coexist; a double match unions the ports. The default stays DENY. Proven in KVM CI for v4: a resolved allowlisted name:port is reachable while an unlisted name (refused), the right name on a wrong port, and an un-resolved direct IP are all blocked. Residual risks are the next four rows. Literal IP:port rules remain the statically enforced path. |
| Name egress: upstream-resolver trust | **open (documented)** | The controlled proxy forwards allowed queries to a configured upstream (`--dns-upstream`, default the host resolver or `1.1.1.1:53`) and pins whatever A records it returns. A malicious or compromised upstream can answer an allowlisted name with an attacker-controlled IP, which the proxy will then pin and the guest will reach. The trust boundary is the upstream resolver. Mitigations not yet in v1: DNSSEC validation, a pinned/known-good resolver set, response-IP sanity checks. |
| Name egress: bounded TTL window | **partial / mitigated** | A pinned `(ip . port)` stays reachable for `max(recordTTL, 30s)` after it is resolved, even if the name later stops resolving to that IP. The window is bounded by the record TTL (floored at 30s so a very short TTL cannot expire the pin before the guest connects) and the set's timeout, after which the element is evicted and the IP is no longer reachable unless re-resolved. There is no manual revocation of a live pin before its timeout. |
| Name egress: shared-CDN-IP caveat | **open (documented)** | Pinning is by IP after resolution, so if an allowlisted name and a denied name resolve to the SAME IP (a shared CDN or load-balancer address), resolving the allowlisted name pins that IP and makes it reachable on the allowed port, including for traffic the operator intended to deny that happens to share the address. The denied NAME is still refused at the resolver (it is never answered or pinned), but the IP it shares becomes reachable once the allowlisted name is resolved. This is inherent to IP-level enforcement of name policy. |
| Name egress: DoH/DoT and DNS tunneling | **mitigated** | A guest cannot bypass the controlled resolver. Only `udp/tcp 53` to the resolver IP is permitted by the egress chain, so a guest cannot reach an arbitrary external DoH/DoT server (its `IP:port` is not allowlisted and was never pinned). The resolver answers only A and AAAA queries for allowlisted names and REFUSES every other qtype, so it cannot be used as a covert DNS tunnel: only A/AAAA records are forwarded and the resolved IPs are constrained to the allowlist (and pinned into the v4 or v6 set by address family). |
| Name egress: source attribution | **enforced** | The proxy attributes each query to a sandbox by the query's source guest IP (each sandbox has a unique /30 from the identity allocator) and pins into THAT sandbox's set. A guest cannot grant itself another sandbox's reach by spoofing a source IP: the per-tap dispatch sends a tap's traffic only into its own chain, and every v4 accept (including the dynamic v4 pin-set accept) is `ip saddr`-pinned to the sandbox's guest IP, so a spoofed-source query cannot land a pin that the spoofing guest can use. A query whose source has no live tap mapping is REFUSED and pins nothing. The v6 accept is not `ip saddr`-pinned because the guest has no v6 source address to spoof from today; the v6 default-deny in each chain remains the boundary there. |
| Layering: host netns vs per-VM netns | **host-netns today** | The tap and nftables ruleset live in forkd's (the host's) network namespace; isolation between sandboxes is by per-tap dispatch + per-/30 addressing + saddr anti-spoof, not by a kernel netns boundary per VM. Moving each VM into its own pod netns (husk pods, #18) adds a second, defense-in-depth layer and is where snapshot-fork-under-netns is resolved. Live-fork (`ForkRunning`) of a networked sandbox fails closed today (#18): a live fork would restore the source's baked NIC and collide on tap/MAC/IP. |
| K8s NetworkPolicy | **in-pod nft is the guarantee (IMPLEMENTED and KVM-VERIFIED end to end); NetworkPolicy is best-effort defense in depth** | In RAW-FORKD mode sandboxes are not pods: NetworkPolicy does not govern them and our nftables egress layer is ours and documented as ours (CI-proven in-VM on KVM, opt-in). In the HUSK default the VM's tap is in the husk pod's netns, and the husk-stub programs an in-pod nftables default-deny egress filter there (the CNI-independent GUARANTEE, with the unconditional metadata block and the threaded allowlist). The controller additionally creates one `networking.k8s.io/v1` NetworkPolicy per pool (`internal/controller/husknetworkpolicy.go`) selecting `mitos.run/husk=true` with default-deny egress, a DNS allow, and one egress rule per enforceable IP:port allow, owner-referenced to the pool for GC. HONEST CNI caveat: the NetworkPolicy enforces only on a CNI that implements it and cannot express name-based allows, so it is defense in depth ONLY; the in-pod nft filter holds with no CNI policy at all. The superseded CI "Conformance 3" allow-all step proved no restriction. The in-pod guarantee is now KVM-verified end to end: the husk-network KVM cluster e2e (`test/cluster-e2e/husk-network-e2e.sh`, `cluster-husk-network-e2e`) PASSES on the Hetzner Talos KVM cluster, the claim reaches Ready, and all three in-VM enforcement assertions are green inside a real restored VM (metadata-blocked, default-deny, allowlist-works), verified twice (with and without a node sysctl allowance), proving NO node prerequisite. |

## 5. Snapshot integrity and supply chain

Snapshots are executable memory images; loading one is equivalent to running
arbitrary code at sandbox privilege.

| Control | Status | Detail |
|---|---|---|
| Content addressing (digest in CRD status) | **mitigated** | Every template snapshot is content-addressed in a CAS store the moment it is built: its sha256 manifest digest is recorded to `<dataDir>/templates/<id>/manifest.digest`, pinned in the store, reported through forkd `GetCapacity`/`CreateTemplate`, and written to `SandboxPoolStatus.TemplateDigest` so the snapshot identity is visible in `kubectl get sandboxpool -o yaml`. The digest is a content address and is safe to log. |
| Verify-on-load | **mitigated** | forkd verifies a snapshot's on-disk bytes against the recorded digest before it is forked, and refuses on mismatch. To keep the fork hot path cheap, verification is verify-once-at-registration: at build time (trusted, marker written without re-hash) or at first use after a restart (lazy re-hash), recorded by a `verified` marker that Fork only stats. The dev-mode escape `--allow-unverified-snapshots` downgrades a failed verification to a loud one-time warning. Residual: verification is at registration, not per fork, so tampering AFTER a snapshot is verified is not re-detected until the marker is cleared; external snapshot import is not yet supported. |
| Publish authorization | **mitigated** | Snapshots are produced only by forkd's own `CreateTemplate`, which is reachable solely over the mTLS-gated gRPC surface from the controller (PR #41). Externally supplied snapshots are not accepted, so the publish surface is exactly that authenticated `CreateTemplate` call. External snapshot import is future work. |
| Compatibility verification (no unsafe restore) | **mitigated** | The same load gate also runs the snapshot compatibility contract (`internal/snapcompat.Check`, issue #32) after the digest verify and before any Firecracker launch. The manifest records the producing environment (snapshot format version, Firecracker version, CPU model, kernel, config hash) as part of the content-addressed digest, so these fields cannot be tampered with or downgraded without changing the digest and failing the verify-on-load step above. A benign mismatch (a snapshot legitimately built under a different Firecracker version, a different CPU model, or an unsupported format version) fails closed: the restore is refused with an actionable error rather than crashing or silently corrupting a guest. The dev-mode escape `--allow-incompatible-snapshots` downgrades a refusal to a loud warning. Kernel mismatch is informational. Residual: cross-CPU-model restore via Firecracker CPU templates and live cross-Firecracker-version restore are out of scope (the contract refuses them today). |
| Encryption at rest + crypto-shredding (#31) | **mitigated** | Behind `--enable-encryption` (default off) each scope (a template now; a workspace when #21 lands) gets its own LUKS2 container (`internal/storecrypt`) backed by a sparse image; the snapshot and volumes are built inside the mounted, decrypted container, so the bytes at rest in `<scope>.img` are ciphertext, not the plaintext snapshot. dm-crypt sits below the page cache, so the mem mmap CoW restore reads decrypted pages and CoW page sharing across forks is preserved (no per-fork decryption copy). Erasure is crypto-shredding: `luksErase` wipes the LUKS keyslots and the image is removed at template delete, after which the ciphertext is unrecoverable even with the key. The key reaches cryptsetup only on stdin (`--key-file -`), never in argv or any log; `storecrypt.Key` redacts itself on any format. Proven in KVM CI on real cryptsetup: the marker is absent in the raw image but present in the decrypted mount (ciphertext at rest), reopen+read returns it intact (decrypt/restore works), and after shred a reopen with the original key fails and the image is gone (unrecoverable). Key custody (envelope, #31 follow-up): the controller generates a per-template 256-bit DEK with `crypto/rand`, WRAPS it with a KMS key-encryption key (KEK) via `kms.Wrapper` (`internal/kms`), zeroizes the plaintext DEK immediately, and stores ONLY the wrapped DEK plus the non-secret KEK id in a `<template>-enc-key` Kubernetes Secret owner-referenced to the `SandboxPool` (so GC of the pool GCs the Secret). The plaintext DEK never persists to etcd or disk. The controller delivers the WRAPPED DEK plus the KEK id to forkd in the mTLS-protected `CreateTemplate` and `Fork` gRPC requests; forkd unwraps via its KEK (`--kek-file` local AES-256-GCM provider) into process memory only, uses it for cryptsetup, and zeroizes the plaintext immediately after. forkd holds only the wrapped DEK via `RequestKeyProvider` and NEVER writes the plaintext or wrapped DEK to the node data disk; encryption enabled with no delivered wrapped DEK, or an unwrap failure (wrong KEK), fails closed. forkd refuses to start under `--enable-encryption` without `--kek-file`. The KEK never leaves the `kms.Wrapper` boundary: the local provider loads it by PATH from a Secret-mounted file (never argv, never logged; only the non-secret KEKID fingerprint is logged); a cloud KMS/HSM provider (AWS/GCP/Vault) is an interface-only documented follow-up where the KEK never leaves the HSM. The mTLS channel is ENFORCED, not merely used: forkd refuses to start with `--enable-encryption` unless its TLS cert/key/CA flags are set, and the controller refuses to deliver the wrapped DEK to a node whose connection is not mTLS (it fails the encrypted build/fork for that node rather than transmit it in cleartext). The DEK and the KEK are never logged anywhere in the key-custody code path (enforced by grep in CI). Proven by envtest and unit tests: the `internal/kms` round-trip/tamper/wrong-length/KEK-mismatch tests; the envtest proving the Secret stores the wrapped DEK + KEK id and never a raw key, and that the RPC carries the wrapped DEK + KEK id; daemon stash-and-forget of the wrapped form; forkd unwrap-and-zeroize; fail-closed; and DEK/KEK-never-logged. See docs/encryption.md. HUSK DEFAULT (section 0 surface 7): the same mTLS-only delivery and node-memory-only custody apply on the husk path: the wrapped DEK reaches the node over the mTLS control channel, the plaintext is unwrapped and zeroized while a container is opened, and neither is written to the node disk; the in-memory-DEK window is the named residual (HSM custody narrows but cannot eliminate it). Residuals, explicitly: (1) etcd now holds only the WRAPPED DEK and the non-secret KEK id, never the plaintext DEK; the etcd-at-rest-encryption trust is DOWNGRADED to defense-in-depth (an etcd exfiltration without the KEK cannot unwrap the DEK). The KEK custody is the `internal/kms` Wrapper: local AES-256-GCM from a Secret-mounted KEK file in dev/CI; a cloud KMS/HSM where the KEK never leaves the HSM is the documented follow-up. (2) Controller trust: a compromised controller can read the Secret and deliver the wrapped DEK to any forkd, and (with the KEK) wrap/unwrap; the cluster admin boundary is the trust anchor. The controller no longer holds the plaintext DEK after `EnsureEncKey` returns (it zeroizes immediately post-wrap). (3) Node-memory dump while open: while a container is open the plaintext DEK is necessarily in forkd process memory to serve I/O; a root attacker with a memory dump recovers it; zeroize-immediately-after-use is the current mitigation, full HSM custody narrows but cannot eliminate this window (dm-crypt requires the key in kernel memory). (4) TEARDOWN BOUNDARY: the controller does not yet send a DeleteTemplate RPC on SandboxTemplate deletion, so the node-side container is GC'd by node data dir lifecycle rather than a controller-driven crypto-shred; tracked as follow-up. **PER-WORKSPACE STORE ENCRYPTION (W4 Phase 4, #21 + #31) is now in place.** When `Workspace.spec.store.encryptionKeyRef` is set, every workspace revision CHUNK and MANIFEST is encrypted at rest with AES-256-GCM under a per-workspace DEK before it reaches the store (node CAS or S3), and decrypted on hydrate (`internal/workspace/encryption.go`, `s3store.go`). It reuses the SAME envelope custody as templates: the DEK is wrapped by the KEK via `kms.Wrapper`, the plaintext DEK is unwrapped only in node memory for the duration of a hydrate/dehydrate and zeroized after, and it is never logged, never in an error, never written to a host path. The key is PRINCIPAL-BOUND, pairing with the memory-snapshot policy in section 3. Two invariants are asserted in unit tests BEFORE merge: the manifest digest is computed over PLAINTEXT, so an encrypted dehydrate yields the SAME content identifier as a plaintext dehydrate (content-addressed dedup is preserved: `TestEncryptedDigestMatchesPlaintextDigest`, `TestEncryptedDehydrateDedups`), and the encrypted round trip is BYTE-IDENTICAL with chunks ciphertext at rest (`TestEncryptedDehydrateHydrateRoundTrip`, `TestEncryptedChunksAreCiphertextAtRest`); a wrong key fails closed at GCM Open (`TestEncryptedHydrateWrongKeyFailsClosed`). The per-chunk GCM nonce is a keyed HMAC over the plaintext digest (domain-separated from the GCM key), so identical plaintext re-encrypts byte-identically and the at-rest dedup skip still holds. Absent the key ref keeps today's plaintext store path (backward compatible). Out of scope for now: cloud KMS/HSM providers (AWS/GCP/Vault, interface-only here), KEK rotation and DEK re-wrap, DEK rotation/re-encryption, and encrypting the template snapshot CAS chunk store (the workspace artifact store is now encrypted; the template snapshot CAS chunk store remains a follow-up). |

### Device passthrough (GPU/VFIO) and the GPU node tier (issue #221)

GPU support attaches a physical device into the microVM via VFIO passthrough.
This MOVES the security surface: a passthrough device is a new, privileged path
between the guest and host hardware, and a GPU node is a DISTINCT, higher-trust
tier from a CPU-only node. NONE of this is implemented or hardware-validated
here; the device-attach and fork-with-device code is a documented,
HARDWARE-GATED follow-up (see `docs/platforms/gpu.md`). What ships today is the
scheduling/metering/spec plumbing only (GPU node selection in the registry,
`resources.gpu` on the template, GPU-seconds metering). The rows below state the
surface so it is not silently expanded when the device path lands.

| Control | Status | Detail |
|---|---|---|
| VFIO passthrough surface | **open (not implemented; hardware-gated)** | A passthrough GPU gives the guest DMA-capable access to a real PCI device. Without a correctly configured IOMMU and per-device IOMMU group isolation, a malicious guest can program the device's DMA engine to read or write host memory outside the VM, defeating the KVM boundary. A GPU node is therefore only as isolated as its IOMMU configuration: IOMMU must be enabled and the GPU must sit in a clean IOMMU group (no bridge or sibling device shared with the host). This is a NODE prerequisite the operator owns; the controller cannot verify it. Until the device path is implemented AND validated on real GPU KVM hardware, do not attach a GPU to a sandbox that runs untrusted code. |
| GPU node tier (higher privilege) | **open (documented)** | GPU nodes are a SEPARATE, higher-trust node pool, labeled `mitos.run/gpu`. They carry expensive, DMA-capable hardware and (per the row above) a larger host-escape surface than CPU-only nodes. Treat them as a distinct blast-radius tier: dedicate them (issue #172 `spec.placement`) to trusted or single-tenant workloads, do not co-schedule untrusted multi-tenant sandboxes onto them until the VFIO surface is validated, and account for the fact that a successful device-DMA escape on a GPU node compromises that node's host, not just the VM. The scheduler pins GPU pools to these nodes; it does NOT downgrade their trust requirement. |
| Fork-with-device (no live fork of a GPU sandbox) | **refused by design (documented)** | A GPU sandbox CANNOT be live-forked while the device is attached. A snapshot/fork captures and restores guest RAM with `MAP_PRIVATE`, but a passthrough PCI device has live hardware state (queues, BARs, on-device memory, DMA mappings) that is NOT part of the guest RAM image and cannot be coherently duplicated into a fork; restoring two VMs that both believe they own the same physical GPU is incoherent and unsafe. This matches Modal's documented limitation (memory-snapshot fork is GPU-incompatible). The stance, stated like the issue requires: a GPU sandbox is NOT forkable with the GPU attached. The fork engine must fail closed on a fork of a GPU-attached sandbox when that path is built (the device must be detached first, or the sandbox forked only before attach). See `docs/platforms/gpu.md`. |
| GPU-seconds metering integrity | **partial (accounting only; measurement hardware-gated)** | GPU-seconds is added as a billable unit (`internal/metering`, summed straight per sandbox since a GPU is exclusively assigned and never CoW-shared across forks). The accounting math is unit-tested. The REAL per-device GPU-second measurement (reading actual device-busy time) is hardware-gated and not implemented; today the field is the seam the usage pipeline (#211) and Stripe metering (#212) bill on, fed by wall-clock-held-device time, not on-device utilization. |

### Isolation tiers: not every node is the same assurance (issue #40)

Mitos's default and strongest isolation is the hardware-virtualization microVM
(KVM + Firecracker). Two WEAKER mechanisms exist as run-anywhere or fallback
tiers, and the threat model's job is to make sure they are NEVER silently treated
as equivalent to hardware virt:

- **PVM** (Firecracker on the PVM kernel module, pagetable-based virtual machine,
  Ant/Alibaba): runs guests in ring 3 via pagetable switching with NO VMX/SVM, so
  Firecracker runs on a plain cloud VPS that exposes no `/dev/kvm` nested virt.
  Ring-3 pagetable isolation is WEAKER than hardware virt: there is no VT-x/AMD-V
  root-mode boundary, the host kernel runs out-of-tree patches, and the guest
  shares more of the host's privilege machinery. PVM is EVALUATED, NOT ADOPTED
  (`docs/platforms/pvm-evaluation.md`); this row exists so that if a PVM tier is
  ever enabled it is a documented lower-assurance tier from day one.
- **gVisor** (userspace-kernel syscall interposition): not a VM at all. The
  software-isolation tier, relevant to the gVisor fallback and to ADR 0005 (raw
  forkd is not multi-tenant). Weaker still than a VM boundary.

The MITIGATION that makes a mixed fleet safe is a NODE isolation tier plus a
per-pool/template assurance FLOOR, both shipped here:

| Control | Status | Detail |
|---|---|---|
| Node isolation tier label | **mitigated (scheduling control)** | Each node declares its isolation assurance via the `mitos.run/isolation-tier` label (`hardware-kvm`, `pvm`, or `gvisor`), mirrored into the scheduler's `NodeInfo.IsolationTier` (`IsolationTierFromNodeLabels`). An UNDECLARED node is treated as the LOWEST assurance (fail-closed): it satisfies no floor. The tier is the node's property, never inferred from the workload. The node-side mechanism that legitimately earns a tier (real hardware virt, a PVM host kernel, a gVisor runtime) is operational and out of scope for the controller; a typo or unrecognized label value never promotes a node above its declared assurance. |
| Required assurance floor (`minIsolationTier` / `requireHardwareKvm`) | **mitigated (scheduling control)** | A template sets `spec.minIsolationTier` (`hardware-kvm`/`pvm`/`gvisor`) or the convenience `spec.requireHardwareKvm: true`; the controller's node selection (`internal/controller/scheduler.go` `admitsTier`) admits ONLY nodes whose declared tier meets the floor. A floor is a minimum, so a stronger node still qualifies. A security-sensitive tenant requiring `hardware-kvm` therefore NEVER lands on a PVM or gVisor node, and when no node meets the floor the placement fails loudly with `ErrNoCapacity` (a relabel-or-relax remediation), never a silent downgrade. `requireHardwareKvm` can only tighten, never weaken, an explicit floor. Unit-tested: a hardware-kvm floor skips a PVM node, an undeclared node fails a real floor, and no floor uses any node. |
| PVM / gVisor co-tenancy posture | **open (documented; opt-in only)** | A PVM or gVisor node is a LOWER-assurance tier and must NOT co-host security-sensitive multi-tenant work unless the operator explicitly opts in. The default posture for an untrusted multi-tenant pool is to set a `hardware-kvm` floor so it cannot be scheduled onto a weaker tier. PVM nodes additionally carry an out-of-tree host kernel (a larger and less-reviewed host TCB than a stock hardware-virt node); treat a PVM-node host escape as compromising that node, and dedicate PVM nodes to trusted or explicitly-lower-assurance workloads. The control above is the enforcement seam; the operator owns the policy decision of which tenants may use which tier. |
| Confidential tier (SEV-SNP / TDX), memory encrypted from the host/operator | **evaluated, NOT adopted (#354)** | A hardware-memory-encrypted tier (AMD SEV-SNP / Intel TDX) was researched as the highest-ceiling assurance level: encrypt guest memory even from the node operator. Finding (`docs/platforms/confidential-microvm-evaluation.md`): confidential plus WARM FORK is infeasible on current Firecracker and hardware (Firecracker has no confidential support and snapshot/restore is named as the blocker; snapshot/restore and CoW fork both violate the per-guest-key, single-physical-page-owner, and fresh-attested-launch invariants of SEV-SNP/TDX). The only viable shape is a SEPARATE cold-boot tier (fresh per-VM attested launch, no fork, on QEMU or cloud-hypervisor, not Firecracker), which is a large separate program, out of scope until regulated demand justifies it. Per the no-unverified-claims rule, Mitos does not market confidential microVMs until such a tier is built and attested end to end. The spike also corrects the #40 `0xc0010007` finding (it is a PMU counter MSR, `MSR_K7_PERFCTR3`, not a SEV MSR; a PMU snapshot-replay bug, not a confidential-computing one). |

## 6. Secrets

| Control | Status | Detail |
|---|---|---|
| Claim-time injection (not baked into snapshots) | **partial** | The design is right: pools snapshot before secrets exist; the controller resolves Secret refs at claim time (`sandboxclaim_controller.go:resolveSecrets`). Delivery into the guest is implemented over vsock post-restore (`internal/daemon/server.go:deliverConfig`); never via boot args, never via the FC API socket. Strict on real engines: if secrets cannot be delivered, the fork fails and the VM is reaped (a sandbox that reports Ready without its secrets is a lie). The mock engine skips delivery entirely; no guest exists. The same post-restore handshake also sends `NotifyForked` (32 bytes of host `crypto/rand` entropy plus a fork generation) before config. The reseed is now FAIL-CLOSED on every engine. `internal/daemon/sandbox_api.go` `NotifyForked` RETURNS the guest `NotifyForkedResponse`; `internal/daemon/server.go` `notifyForked` reaps the fork when the guest reports `ReseededRNG:false` (and `ForkRunning` reaps the live-fork child the same way), and `cmd/sandbox-server/main.go` `handleFork` runs `reseedFork` and unregisters the sandbox on a `ReseededRNG:false` result, so a guest that connected but did not reseed is never served (no duplicate keys/tokens/UUIDs across forks). The HUSK path was already fail-closed (`internal/husk` `productionNotifier`). The guest side is now honest too: the Rust agent's `reseed_crng` reports success ONLY when the credited `RNDADDENTROPY` ioctl succeeds and returns false on the uncredited write fallback, so the boolean the gate keys on cannot be a false positive. Status: mitigated on all engines. See `docs/fork-correctness.md` row 1. Entropy bytes are never logged by host or guest. Resolved secret values (`ForkRequest.Secrets`) now transit the mTLS-protected controller→forkd channel when deployed as shipped (§3); they remain plaintext on the wire only in flag-less dev deployments, where forkd warns loudly. |
| Live-fork secret inheritance | **mitigated (default-deny)** | Forks of secret-holding sandboxes are rejected by the fork path without explicit `spec.secretInheritance: inherit`; opt-ins are recorded as an audit condition (`sandboxfork_controller.go`). Per-fork credential reissue remains the end state (open). See `docs/fork-correctness.md` §3. |
| Controller RBAC for Secrets | **narrowing shipped, opt-in** | A namespaced path now exists: the chart ships a `mitos-pool-secrets` ClusterRole (Secrets verbs, never bound cluster-wide) and grants the controller a `bind` verb SCOPED to that one ClusterRole plus rolebindings management; the controller binds itself to it per pool namespace as it adopts one (`EnsurePoolSecretsRoleBinding`, additive and idempotent), and in its own namespace via a chart RoleBinding. With `controller.namespacedSecretsRBAC=true` the cluster-wide Secrets grant is REMOVED, so a stolen controller token reaches Secrets only in the controller namespace and adopted pool namespaces, not cluster-wide. Default false for safe rollout (the per-pool bindings reconcile while the cluster-wide grant is still present, then operators flip the value). The `bind` grant is pinned by `resourceNames` to the one ClusterRole and is not a general escalation primitive. Residual: the controller can still write Secrets into any adopted pool namespace, and the cluster-wide `pods` create/delete grant (husk pods) is unchanged. Escalation-knot resolution and rollout are documented in `docs/superpowers/plans/2026-06-18-rbac-narrowing.md`. |

- Cross-namespace secret replication. The controller projects ONLY mitos-ca
  (ca.crt) from its namespace into every pool namespace where it creates husk
  pods (ReplicateHuskSecrets). The CA private key (ca.key) is never copied, and
  the forkd server leaf is no longer copied either: the husk control channel
  serves a per-namespace leaf (mitos-husk-tls, issued in-namespace by
  EnsureHuskTLS), so a pool namespace holds only the public CA cert and its own
  husk server key, never the CA signing key and never the shared forkd key.
  Scope: the cluster-wide secrets grant is the enabling privilege for writing
  ca.crt into pool namespaces; a namespaced grant scoped to pool namespaces is a
  follow-up once pool namespaces are enumerable at install time.
- Husk activation target authenticity (warm-slot impersonation). Activation
  delivers the claim's resolved secrets plus the per-sandbox bearer token to the
  husk pod's self-reported PodIP over the mTLS control channel. So if pod
  selection trusted only the husk LABELS (which any principal with pod-create in
  the pool namespace can set), a tenant could stand up a decoy pod pointing at
  their own listener and receive another claim's secrets and token. Mitigated by
  two independent, composing controls:
  1. PROVENANCE: `selectDormantHuskPod` requires the controller owner reference
     reconcileHuskPods stamps (a controller reference of Kind SandboxPool naming
     the pool with BlockOwnerDeletion=true), so only pods the controller actually
     created are activation targets. The forgery barrier is BlockOwnerDeletion:
     the OwnerReferencesPermissionEnforcement admission plugin refuses to let a
     tenant set it referencing a pool whose finalizers subresource they cannot
     update.
  2. PER-NAMESPACE IDENTITY (now implemented): the husk control channel no longer
     uses the shared forkd server key. Each pool namespace gets its OWN server
     leaf, `husk.<namespace>.mitos` with a distinct key, issued by EnsureHuskTLS
     into a namespace-local `mitos-husk-tls` Secret; the controller pins
     `husk.<namespace>.mitos` when dialing a pod in that namespace
     (`HuskDialTLSConfig`), and ReplicateHuskSecrets projects only `ca.crt` into a
     pool namespace, never the forkd server key. So even a per-namespace key
     leaked in namespace A cannot impersonate a husk in namespace B (the controller
     dials B pinning `husk.B.mitos`, which A's leaf does not satisfy); within A,
     impersonation yields only A's own tenant secrets. This does not depend on the
     OwnerReferencesPermissionEnforcement plugin, so it is the durable control on
     clusters where control (1) is weaker. End-to-end verified by the husk KVM
     e2e (the controller-pins-vs-husk-serves handshake).
  ROTATION: EnsureHuskTLS now reissues the per-namespace leaf when it is within
  HuskCertRenewBefore (30 days) of NotAfter, so a long-lived pool rotates its
  husk serving cert ahead of expiry across reconciles rather than serving an
  aging leaf until the control channel breaks.

## 7. Multi-tenancy statement

What a namespace boundary buys you **today**: RBAC on the CRDs, and nothing
else. Pools, claims, and forks are namespace-scoped objects, but:

- Snapshots on a node are a flat directory shared by all tenants; no
  per-namespace separation, no enforcement that a claim only forks snapshots
  its namespace published. **open**
- VMs of different namespaces share nodes, host kernel, and forkd BY DEFAULT. **open**
- A pool MAY opt into node separation via `.spec.placement` (`PoolPlacement`: a
  `nodeSelector` ANDed onto the husk pods plus `tolerations` for tainted dedicated
  nodes). The controller pins the pool's husk pods to the matching nodes AND
  constrains the template snapshot build/distribution to that same node set
  (`placementFilter` / `createSnapshotsOnNodes`,
  `internal/controller/sandboxpool_controller.go`, issue #172), so a placed pool's
  VMs and their snapshot never land on a node outside its dedicated set. This is
  SCHEDULING-enforced separation (Kubernetes `nodeSelector` / taints), NOT a
  hardware or hypervisor isolation guarantee: it depends on the operator labeling
  and tainting the dedicated nodes and not co-scheduling other tenants there.
  Node-side taint enforcement and per-tenant node-pool provisioning remain the
  operator's responsibility. **partial**

Until the above are closed, treat the whole cluster as one trust domain unless an
operator has provisioned dedicated, tainted node pools and placed each tenant's
pools onto them. This posture is recorded as a residual decision in
docs/adr/0004-node-flat-snapshot-trust-domain.md.

## 7b. Customer front door: accounts, orgs, and the public API gateway (issue #210)

The hosted offering adds a NEW public attack surface layered ABOVE the internal
mTLS and per-sandbox token plane: an internet-facing gateway (`cmd/gateway`,
`internal/saas`), customer-presented API keys, external accounts and
organizations, and cross-tenant (org) isolation. This surface does NOT replace
the internal plane; it sits in front of it and forwards org-scoped, authenticated
requests to the control plane. Design: docs/saas/accounts-gateway.md.

PRODUCTION GATE: this front door is NOT cleared for production tenants until the
external security review (issue #194) covers it. Until then it is a development
and review artifact only.

| Boundary | Status | Mechanism |
|---|---|---|
| Customer key authentication | partial | Prefix-tagged keys (`mitos_live_...`) hashed at rest with a process pepper, never stored in the clear, verified in CONSTANT TIME (`crypto/subtle`), with expiry, revocation, and scope checks. Only a masked prefix is shown after creation; the raw key is returned exactly once and is never logged or placed in an error. Unit-tested: verify rejects forged, malformed, expired, revoked, and wrong-scope keys (`internal/saas/keys_test.go`). The pepper is loaded from a secret in production (a follow-up); the default empty pepper still gives a sound sha256 hash. TLS termination and edge rate-limiting are follow-ups. |
| Cross-org (tenant) isolation | partial | The `OrgID` the gateway forwards to the control plane is taken SOLELY from the verified key, never from the request body or path, so a key for org A cannot address org B's resources even by stuffing another org id into the body (`TestGatewayCrossOrgIsolation`). Key resolution maps a key to its own org only (`TestOrgAKeyCannotResolveOrgB`). The management verbs reject a non-member, so a user cannot mint, list, or revoke another org's keys (`internal/saas/account_test.go`). The control-plane forward target that enforces this org boundary inside the cluster is a documented follow-up; the seam and its contract are tested here against a fake. |
| Public error surface | mitigated | The gateway maps every internal failure to the LLM-legible envelope (`internal/apierr`). A missing, malformed, unknown, expired, or revoked key all collapse to `unauthorized` so a probe cannot distinguish them; a valid-but-not-permitted key is `forbidden`; an org over a hosted quota is `quota_exceeded`. The two new codes are in sync with docs/api/errors.md, the JSON Schema, and llms.txt (the #28 sync tests are green). |
| Hosted quota and abuse controls | partial | The real `QuotaEnforcer` (issue #213, `internal/saas/quota`) now plugs into the gateway seam: it resolves the org's plan tier, enforces per-sandbox size, live concurrency, and aggregate vCPU/memory/storage caps against the org's CURRENT footprint (a live-count seam, with a #211-usage-store-backed fallback), and charges per-org AND per-IP token buckets split into lifecycle and in-sandbox rate windows plus a separate creation-rate bucket. A denial maps to the precise public code (`quota_exceeded`, `rate_limited`, or `forbidden`) via the gateway's `apiErrorProvider` seam. Tiers are a prepaid ladder: a new signup lands on the tightest tier with a deny-by-default egress posture and the smallest caps; paying climbs the ladder. The enforcer, the rate limiter, the tier->egress mapping, and the kill-switch are unit-tested (over-concurrency, over-aggregate, over-size, within-quota, cross-org isolation, rate-limit allow/reject/refill, suspended-org fail-closed, emergency stop, abuse-signal-driven suspension). HONEST CAVEAT: the live multi-node count and the abuse-detection SIGNALS are documented seams (`LiveCounter`, `AbuseSignal`); this slice ships the enforcement decision and the suspend mechanism they drive, not the cluster-wide counter or the detectors. This abuse-control envelope is the HARD GATE on enabling public self-serve untrusted multi-tenancy: it must exist and be wired before that surface opens. |
| Per-tier egress policy (issue #219 datapath, #36 abuse ports) | partial | The tier->egress-policy mapping (`internal/saas/quota` `EgressTier.Policy` / `Tier.ResolveEgress`) selects which #219 `netconf.SandboxPolicy` a tier gets: free is deny-by-default (no egress without an explicit allowlist), a blocked tier maps to `BlockNetwork`, an open (paid) tier maps to `EgressAllow`. Every tier, including the open one, carries the fleet-wide abuse-port block (outbound SMTP 25/465/587, issue #36) and inherits the unconditional cloud-metadata drop, so even an open tier cannot send mail spam or steal node IAM credentials. This is policy SELECTION, unit-tested in isolation; the real packet enforcement is the KVM-gated #219 datapath. |
| Kill-switch / org suspension (issue #36) | partial | Org suspension is a first-class verb (`internal/saas/quota` `KillSwitch`): suspend one org (manual review hook), suspend a set at once (pool-wide / org-wide emergency stop), or suspend automatically from an abuse signal. A suspended org fails closed: the enforcer denies it before any quota math (`forbidden`), so its keys are rejected at the gateway and new claims are refused. An automated suspension is held for human review before it can be lifted. Unit-tested: a suspended org's request is rejected, lifting restores access, emergency stop suspends all named orgs, an abuse signal drives suspension. The VM-freeze effect on already-running sandboxes and the durable suspension store are documented seams. |
| Metered billing and payment secrets (issue #212) | partial | The billing core (`internal/saas/billing`, docs/saas/pricing.md) wires Stripe metered usage, a credit ledger, spend caps, dunning, and a webhook handler. It is built ENTIRELY against a `StripeClient` interface with an in-memory `FakeStripe`: NOTHING in this slice makes a real charge, and the Stripe Go SDK is not a hard dependency. A Stripe API key, the webhook signing secret, and any payment-method detail are SECRETS that are never logged, never put in an error or a condition/webhook note, and never written to a host path; the kill-switch and webhook notes carry org ids, caps, and counts only. The metered push is idempotent on the `(org, sandbox, window)`+meter key (the same #211 record key), so a retried push never double-reports; the credit ledger is append-only and floors at zero so accounting cannot go negative or double-debit. A breached HARD spend cap suspends the org through the #213 kill-switch (the runaway-agent backstop) with a manual hold. Unit-tested: idempotent push (incl. retry after a transient failure), credit drawdown/no-negative/idempotent replay, soft-cap alert vs hard-cap suspend, dunning transitions, webhook status update. SEAMS (documented follow-ups, NOT enabled here): the real Stripe SDK adapter, the real webhook SIGNATURE verification (the signing-secret check over `Stripe-Signature`; the test uses a fake verifier that trusts the body), and durable stores. The webhook HTTP entry point never echoes the raw body or the signature in an error. PRODUCTION GATE: real-charge wiring is not enabled and must be reviewed (key handling, no-secret-logging, no-unverified published prices) before going live. |
| Session and login | partial | Token-based session resolution, hashed at rest, constant-time (`internal/saas/session.go`). Browser-based OAuth login is a documented follow-up; `mitos auth login` is `--token` only today. |
| Persistence | open | The account, org, membership, key, and session stores are IN-MEMORY for this slice (lost on restart, single-process). The Postgres store and its migrations are a documented follow-up behind the same `Store` interface. Do not run the front door as a stateful production service until that lands. |

## 7c. Expose URL ingress: per-sandbox port exposure (issues #126, #230)

Expose URLs add a NEW public ingress that maps a single-label hostname,
`<label>.<expose-domain>`, to a port INSIDE a running sandbox through the Mitos
expose edge proxy (`cmd/preview-proxy`, `internal/preview`). `<label>` is an opaque
routing key; the operator configures `<expose-domain>`. This is the E2B
`get_host(port)` / Daytona signed-preview-URL surface. It is a deliberate,
caller-initiated hole in the otherwise deny-by-default inbound posture (section
4): a tenant who mints an expose URL is asking for that port to be reachable
from the internet for the URL's lifetime. Design: docs/preview-urls.md.

PRODUCTION GATE: this ingress is NOT cleared for production tenants until the
external security review (issue #194) covers the public attack surface it adds
and the #213 abuse-control envelope lands. Until then it is a development and
review artifact only. Slice 3 ships TLS termination directly at the Go proxy
with a wildcard cert and hybrid post-quantum key exchange; the proxy is now a
deployed public ingress surface gated by `expose.enabled` (default false) in the
Helm chart.

Architecture as of slice 2b: the proxy resolves the `<label>` in the incoming
hostname to a route (NodeEndpoint, SandboxID, Port, Token) via the route table,
verifies the signed expose token, and reverse-proxies to the forkd expose handler
at `http://<NodeEndpoint>/v1/sandboxes/<id>/expose/<port>/` (the slice-1 forkd
surface, section 3 row `ANY /v1/sandboxes/{id}/expose/{port}/`). The upstream is
NEVER derived from request input; it comes only from the route table. The
per-sandbox bearer crosses the cluster network to forkd `:9091` in CLEARTEXT:
this is the SAME trust model as the existing SDK path (the SDK reaches forkd
`:9091` in cleartext with a per-sandbox bearer); the cluster network is the trust
boundary. In-cluster TLS for forkd `:9091` is a recorded follow-up.

The route table is populated by the controller `ExposeRouteReconciler` (slice 2b).
On each reconcile it lists all Sandboxes, selects those that are Ready
(`Status.Phase==Ready`) with `spec.expose` set and a non-empty
`Status.Endpoint`, reads each one's per-sandbox bearer from its
`<name>-sandbox-token` Secret (key `token`, namespace-scoped so two same-named
Sandboxes in different org namespaces never collide), builds the full route set,
and POSTs it to `POST /internal/routes` authenticated by the shared admin bearer.
The POST body carries per-sandbox bearer tokens over the in-cluster hop in
CLEARTEXT, the same cluster-network trust model as the forkd `:9091` hop. The
admin token is sourced from `EXPOSE_PROXY_ADMIN_TOKEN` (environment variable, never
argv, never logged). The reconciler is disabled by default; it activates only when
`--expose-proxy-admin-url` is set. This completes the end-to-end path: a Sandbox
with `spec.expose` set and `Status.Phase==Ready` is reachable at
`<label>.<expose-domain>` with routes kept current by the reconciler.

| Boundary | Status | Mechanism |
|---|---|---|
| Signed, expiring URL minting and verification | mitigated (unit-tested) | An expose token is a detached HMAC-SHA256 over `(sandboxID, port, expiry)`, base64url payload + tag, keyed by a server secret (`internal/preview/sign.go`). It reuses the SAME standard-library HMAC + `crypto/subtle` constant-time-compare core proven in `internal/captoken` and the W4 SigV4 signer; it is NOT a captoken because it needs no macaroon attenuation chain, only a single expiring binding. Verify rejects an expired token (never-accept-after-expiry, boundary-inclusive), a tampered payload or signature, and a token signed with a different key. The token VALUE and the server secret are bearer credentials: never logged, never in an error/condition, never on a host path. The signing path logs nothing. Unit-tested: round-trip, expiry-boundary, tamper (payload and signature), wrong-key, malformed. |
| Cross-sandbox token binding | mitigated (unit-tested) | The proxy resolves the label to a route (carrying SandboxID and Port) and requires the verified token to name THAT SAME sandbox and port: a token minted for sb-2 presented against a route for sb-1 is rejected with 403 even though it verifies cryptographically. So a leaked URL is bound to exactly one sandbox and one port; it cannot be replayed against another sandbox or a different port. |
| Reserved-name blocklist and Host allowlist | mitigated | A fixed set of labels (`www`, `app`, `api`, `console`, `admin`, `auth`, `login`, and others) is rejected with 404 by `IsReservedLabel` in the host parser BEFORE route table lookup, so a reserved name cannot be routed even if a route were registered under it. The route table itself acts as a Host allowlist: any label not present in the table returns 404, so arbitrary `<label>.<expose-domain>` hostnames have no target to proxy to. |
| Upstream derived from route table, never from request input | mitigated | The NodeEndpoint, SandboxID, and Port that identify the forkd upstream are read exclusively from the route table (populated only by the authenticated admin route-sync endpoint). No part of the upstream URL is derived from request headers, query parameters, or the sub-path beyond the per-route port. An SSRF path to steer the proxy to an unintended backend does not exist. |
| Per-sandbox bearer inject, token strip, and Authorization delete | mitigated | After token + binding checks pass, the proxy (a) injects the per-sandbox bearer token into the upstream `Authorization` header, (b) STRIPS the expose token from the forwarded query so forkd never sees the public expose bearer, and (c) unconditionally deletes any inbound `Authorization` header from the original request so a caller cannot relay an unrelated bearer to forkd. The per-sandbox token VALUE is never logged. |
| Dot-segment cleaning prevents prefix escape | mitigated | The URL sub-path after the label hostname is passed through `path.Clean` before being appended to `http://<NodeEndpoint>/v1/sandboxes/<id>/expose/<port>/`. Traversal sequences (`../`, `./`) are collapsed so the proxy cannot be steered to a forkd route outside the expose prefix. |
| Route table GC on terminate | mitigated (unit-tested) | The route table maps `<label>` to a `Route` (NodeEndpoint, SandboxID, Port, Token, Sharing). `RouteTable.Sync(routes)` reconciles the table to exactly the provided set; a label not in the new set is REAPED immediately, so a terminated sandbox is unroutable (`404`) on the next sync. No route means 404; an expose URL for a dead sandbox cannot proxy anywhere. Unit-tested: add-on / remove-on-sync GC against an injectable route set. The slice-2b `ExposeRouteReconciler` drives Sync from the live Ready Sandbox watch; a Sandbox that leaves the Ready-and-exposed set drops from the next posted set and is reaped by the proxy. |
| Per-sandbox bearer hop to forkd :9091 in cleartext | partial | The per-sandbox bearer is injected into the upstream request and sent to forkd `:9091` over plain HTTP (the same cleartext path the existing SDK uses). The cluster network is the trust boundary. A cluster-internal attacker with network access could observe or replay the bearer. In-cluster TLS for forkd `:9091` is a recorded follow-up; until it lands, this hop is no weaker than the existing SDK path but is not hardened against a cluster-internal observer. |
| Admin route-sync endpoint (`POST /internal/routes`) | mitigated | The admin endpoint accepts the new route set from the slice-2b `ExposeRouteReconciler`. The admin bearer (`MITOS_EXPOSE_ADMIN_TOKEN`) is read from the environment at startup and compared with `crypto/subtle` constant-time on every request; the token VALUE is never logged and never appears in an error body. An empty `MITOS_EXPOSE_ADMIN_TOKEN` disables the endpoint entirely (404 for all `POST /internal/routes` requests); it does NOT default to open. The endpoint is a new authenticated control surface: a compromised admin token lets an attacker inject arbitrary routes (redirect expose URLs to attacker-controlled NodeEndpoints). The token is protected the same way as all other bearer secrets in this system: environment variable, never logged, constant-time compare. |
| Route-sync control loop: controller-to-proxy authentication | mitigated | The `ExposeRouteReconciler` authenticates each `POST /internal/routes` with the shared admin bearer sourced from `EXPOSE_PROXY_ADMIN_TOKEN` (environment variable on the controller, never passed as argv, never logged). The proxy compares it with `crypto/subtle` constant-time on every request. A bearer-less or wrong-bearer POST is rejected 401. The token VALUE is never logged on either side. The controller POSTs to the plaintext ClusterIP admin Service (`mitos-expose-admin:8080`, the `--http-addr` listener); that port is separate from the public TLS Service and is never exposed via the LoadBalancer, Ingress, or wildcard cert. Per-sandbox bearers carried in the POST body travel over the in-cluster hop in cleartext, the documented in-cluster trust model (the same boundary as the forkd `:9091` hop); in-cluster TLS for this hop is a recorded follow-up. |
| Route-sync POST body carries per-sandbox bearers over the in-cluster hop in cleartext | partial | The JSON body of `POST /internal/routes` contains per-sandbox bearer tokens. This POST crosses the in-cluster network in CLEARTEXT, the same cluster-network trust boundary as the forkd `:9091` hop the proxy already uses. A cluster-internal attacker with network access could observe the bearer values in transit. In-cluster TLS for this control hop is a recorded follow-up; until it lands, this hop is no weaker than the existing SDK-to-forkd path but is not hardened against a cluster-internal observer. |
| Namespace-scoped token keying prevents cross-tenant token bleed | mitigated | Each Sandbox's bearer is read from its `<name>-sandbox-token` Secret in the Sandbox's own namespace. The controller reads only Secrets in the Sandbox namespace, so a Sandbox in org namespace `mitos-org-A` can never read the Secret of a same-named Sandbox in `mitos-org-B`. The route entry keyed by `label` carries only that Sandbox's token; the proxy injects only the per-route token into the upstream request. No cross-namespace Secret read path exists. |
| Fail-safe: missing Secret or unreachable proxy requeues, never crashes | mitigated | If the `<name>-sandbox-token` Secret for a Sandbox is missing, the reconciler logs the event, skips that Sandbox, and requeues after 1 second; it does NOT panic and does NOT block reconciliation for other Sandboxes. If the admin POST fails (5xx or transport error), the reconciler requeues with backoff. Terminal 4xx responses (misconfigured URL or disabled endpoint) are not retried until the next Sandbox change triggers a reconcile. The controller process never crashes on a transient proxy failure. |
| Reconciler disabled by default | mitigated | The `ExposeRouteReconciler` starts only when `--expose-proxy-admin-url` is set on the controller. When the flag is absent, no admin POST is ever sent and the expose ingress feature is entirely inactive. This prevents accidental use against a cluster that has not deployed the proxy. |
| TLS at the public edge: wildcard cert | mitigated (slice 3) | The proxy (`cmd/preview-proxy`) terminates TLS directly. Pass `--tls-cert`/`--tls-key` to load an operator-provided wildcard `*.<expose-domain>` cert (`WildcardProvider`, `internal/preview/cert.go`); both flags are required together. A missing or unparseable file is a startup-time fatal error (fails closed). When `--tls-cert` is absent the proxy falls back to per-SNI self-signed certificates: NOT browser-trusted, suitable for local dev only. In the Helm chart the `--tls-cert`/`--tls-key` flags are always set when `expose.enabled: true`, so self-signed is not the deployed path. Single-key blast radius: the wildcard key covers all subdomains. Mitigation: (1) when `expose.tls.certManager.enabled` is true (not the default), cert-manager auto-rotation issues short-lived certificates; when `certManager.enabled` is false (the default), the operator owns rotation and a long-lived static wildcard key has a correspondingly larger exposure window until it is rotated; (2) the key is mounted read-only into a single unprivileged container (`allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`) so its exposure is confined to the proxy process; (3) when cert-manager is enabled, cert rotation replaces the secret and rolling update recycles the pod without downtime. |
| TLS at the public edge: post-quantum key exchange | mitigated (slice 3) | `preview.ServerTLSConfig` leaves `CurvePreferences` nil. On Go 1.24+, the default key-exchange preference list leads with X25519MLKEM768 (hybrid FIPS 203 ML-KEM-768 + X25519), so the session key is PQ-protected when the client supports it. This provides confidentiality against harvest-now-decrypt-later attacks. HONEST SCOPE: this is PQ KEY EXCHANGE for CONFIDENTIALITY ONLY. The certificate signature stays classical (ECDSA or RSA); no post-quantum CA exists in the public PKI, so there is NO post-quantum authentication and none is claimed. A guardrail test (`internal/preview/tls_pq_test.go`, `TestServerTLSConfigNegotiatesPostQuantum`) asserts that a PQ-only TLS 1.3 client completes the handshake with `CurveID == X25519MLKEM768` and that `cfg.CurvePreferences == nil`; a future PR that pins curves will break this test and cannot silently regress PQ support. |
| Proxy deployed as a public ingress surface (slice 3) | partial | The Helm chart (`deploy/charts/mitos/templates/expose-proxy.yaml`) deploys the proxy Deployment, Service, and optional Ingress when `expose.enabled: true` (default false). The controller `--expose-proxy-admin-url` and `EXPOSE_PROXY_ADMIN_TOKEN` are wired to the proxy Service. The proxy container runs with `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`, and `seccompProfile: RuntimeDefault`. Residuals: per-IP rate limiting and a global connection cap are open (see the Ingress DoS row); the ingress is NOT cleared for production tenants until the #194 external security review covers it and the #213 abuse-control envelope lands. `expose.enabled` defaults to false so an unaware operator does not accidentally expose the surface. |
| Ingress DoS / unbounded label spray | open | This slice does not add per-IP rate limiting or a global connection cap at the expose ingress; an attacker could spray arbitrary `<label>.<expose-domain>` hostnames. Unknown labels return 404 quickly (no route lookup cost beyond a map read), but connection-level rate limiting is a documented follow-up, sequenced with the #213 abuse-control envelope and the #194 review before this ingress opens to untrusted tenants. |
| Auth-origin session cookie isolation (`__Host-` prefix) | mitigated (unit-tested) | The preview proxy issues its session cookie with the `__Host-` prefix, enforcing `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, and no `Domain` attribute. A browser cannot send the cookie to a subdomain (a sandboxes URL lives under `<label>.<expose-domain>`; the session origin is the proxy root), preventing cross-subdomain session fixation. Codec: `internal/preview/session.go`. Unit-tested: every cookie attribute is asserted. |
| Single-use HMAC grant token | mitigated (unit-tested) | The central-auth origin issues a short-lived HMAC-SHA256 grant token (payload + detached tag, domain-tagged as `mitos-expose-grant-v1\x00`) that is accepted exactly once: the nonce is recorded in a per-process sync.Map on first verify and rejected on replay. Expiry is checked before nonce recording. The grant token VALUE is a bearer credential: never logged, never in an error string. `internal/preview/grant.go`. Unit-tested: round-trip, expiry, replay (nonce reuse), label mismatch, tampered payload, wrong key, malformed token. |
| ForwardAuth BYO-IdP subrequest: client header spoof prevention | mitigated (unit-tested) | The `ForwardAuth` function builds the subrequest from scratch (`http.NewRequestWithContext`), forwarding only `X-Forwarded-Method`, `X-Forwarded-Uri`, `X-Forwarded-Host`, `X-Forwarded-For`, and `Cookie`. `StripForwardAuthHeaders` deletes all `X-Auth-Request-*` keys from the inbound request BEFORE `ForwardAuth` is called and BEFORE the request is proxied upstream, so a client cannot inject a forged `X-Auth-Request-Email` or `X-Auth-Request-User` to claim an identity the auth service did not assert. Identity is built exclusively from the auth service RESPONSE headers. `internal/preview/forwardauth.go`. Unit-tested: 2xx allow with identity from response; 401 deny; `StripForwardAuthHeaders` removes client-supplied `X-Auth-Request-*`; non-request headers are not removed; `X-Forwarded-For` is forwarded to the auth service. |
| Verified-email to org resolution: bearer-gated in-cluster hop | mitigated (unit-tested) | The `POST /internal/identity/resolve` endpoint in the console binary resolves a verified email to an account ID and org IDs via `AccountService.FindOrCreateByEmail` and `Organizations`. The endpoint is protected by a shared bearer secret (`MITOS_IDENTITY_RESOLVE_TOKEN`, environment variable, never logged) compared with `hmac.Equal` (constant-time, length-safe). The email-to-org mapping is NEVER logged; only the org count is logged. The resolver client (`internal/preview/resolve.go`) also never logs the token or email-to-org mapping; a non-empty configured URL is required or `ErrResolveDisabled` is returned immediately without making a request. `internal/saas/identity_resolve.go`, `cmd/console/main.go`. Unit-tested: bearer gate (missing, wrong, empty configured token); email resolves to account+orgs; bad body returns 400; empty URL returns `ErrResolveDisabled` with no HTTP request; non-2xx returns error without token in message. |
| Authorization pipeline: fail-closed tier, network, audience | mitigated (unit-tested) | The slice-4 `Authorize` function applies three layers in sequence: (1) sharing-tier check (`public`, `org`, `private`); (2) network origin check against the route's `Network` field (`any`, `internal`, `external`); (3) audience check (allowed principals list and allowed email domains). Unknown or empty `Sharing` values are treated as `private` (default-deny). Empty entries in the allowlists are skipped so a malformed list cannot match. Domain comparison is case-insensitive suffix-only (exact-match `acme.com` cannot match `evilacme.com`). `internal/preview/authz.go`. Unit-tested across all tier/network/audience combinations; suffix-trick rejected; case-insensitive; empty entries guarded. |

## 8. What we explicitly do NOT claim

- No pod-scoped Kubernetes mechanism (NetworkPolicy, PodSecurity, pod quotas)
  applies to sandbox VMs. Where we provide an equivalent, it is documented as
  ours.
- No external security review has been performed. The README must continue to
  say so until one has.
- Side-channel resistance between forks of the same snapshot is not claimed.

## Supply chain and artifact provenance (issue #35)

| Boundary | Status | Mechanism |
|---|---|---|
| Image provenance (controller, forkd, husk-stub) | mitigated for published releases | cosign keyless signing + SPDX SBOM attestation, bound to the image digest, produced by `.github/workflows/publish.yaml`; consumer verification in docs/supply-chain.md. |
| Image CVEs | partial | Trivy scans the built images on every PR for HIGH/CRITICAL fixable findings (ADVISORY today: reported, not yet gating, pending base-image remediation); govulncheck is the BLOCKING gate for Go call-graph-reachable vulnerabilities; base-image and dependency bumps arrive via dependabot. Runtime re-scan of long-lived published tags is not yet automated. |
| Guest kernel currency | partial | The shipped vmlinux is pinned to an exact version (docs/kernel-cve.md) and validated end to end by the KVM workflow; CVE watch is a documented manual process, not an automated feed. |
| Guest kernel integrity at stage time | partial | The Helm kernel-stage init container downloads the guest kernel and, when `kernelProvisioner.kernelSha256` is set, verifies the staged file against that digest and FAILS CLOSED on mismatch (covering both a fresh download and an already-cached file), so a swapped upstream object or a MITMed fetch cannot boot a backdoored kernel fleet-wide. The digest is empty by default (a warning is logged); operators should pin it. The privileged forkd, kvm-device-plugin, and kernel-stage pods set `automountServiceAccountToken: false` since none call the Kubernetes API. |
| Admission-time signature enforcement | open | The project ships signatures; requiring them at admission (policy-controller/Kyverno) is a documented operator choice, not a default. |
| Org-scoped usage attribution (billing) | **mitigated** | The org a sandbox bills to is a trust boundary: it is derived only from control-plane identity, never from client input. The controller stamps the `mitos.run/org` label on a sandbox's husk pod from the sandbox's hard-isolation namespace `mitos-org-<id>` via `tenant.OrgFromNamespace` (`internal/controller/huskpod.go`); a client-set `mitos.run/org` on the input object is overwritten, so a tenant cannot relabel its workload to another org (an adversarial test asserts the namespace value wins). The live usage scraper (`internal/usage/livesource.go`, run behind the off-by-default `--usage-collector` flag) reads org only through that controller-stamped label (`LabelOrgResolver`), and the per-sandbox metering report it pulls carries only ids, byte counts, and seconds, never secret values, argv, env, or file content. A self-host sandbox in a non-org namespace is left unattributed rather than misbilled to a default org. Usage aggregation is idempotent on (org, sandbox, window), so a re-scrape or a credit drawdown cannot double-count. The per-org Prometheus series is labeled by org only (no per-sandbox cardinality, no identifiers beyond the org id). The org/pool-labeled metering gauges (`mitos_usage_{vcpu_seconds,mem_gib_seconds,egress_bytes,gpu_seconds}_total`) are fed from the SAME store-fed cumulative, so the dashboard figure and the bill are one number; their only label is org (pool is a documented follow-up since the store keys on (org, sandbox, window)). Residual: the in-memory usage store is bounded by a retention window and survives no controller restart; the durable store and the real billing-provider push are the SaaS follow-up (issue #211), where the durable store is the billing system of record. |
| Guest vitals telemetry surface (issue #164 Phase 1.a) | **mitigated** | The guest health metrics (`mitos_guest_{cpu_steal_percent,mem_balloon_bytes,mem_used_bytes,process_count}`) are sampled by an off-by-default control-plane runnable (`internal/controller/vitals_sampler.go`, `--vitals-sampler` flag). A NEW node-scoped operational endpoint `GET /v1/vitals/node` (`internal/daemon/vitals_api.go`, mounted on the forkd operational mux next to `/v1/metering`, `/metrics`, `/healthz`) is UNAUTHENTICATED (the same access class as `/metrics`), so it returns one numeric-only entry per sandbox on that node. SECRET HYGIENE: each entry carries ONLY the control-plane labels (claim, pool, workspace, namespace; all k8s object names), the numeric guest vitals (steal, balloon, used/total memory), and a numeric process COUNT (`process_count`). It carries NO per-process table at all: no process command name, pid, state, rss, argv, or env crosses this unauthenticated wire. This is enforced at the source by a dedicated node-batch struct (`NodeVitalsNumbers`) that has no field for a process object, so a per-process string CANNOT be serialized here; `handleNodeVitals` writes only `len(processes)` as the count. The FULL per-process table (program names, pid, state, rss) remains available ONLY behind the per-sandbox bearer-authenticated Connect `sandbox.v1.Sandbox.Vitals` RPC (used by `kubectl mitos ps --processes`; the legacy `/v1/vitals` JSON route was removed in #358), never on this node batch. The sampler decodes ONLY the numeric fields including `process_count`, and NEVER receives or relies on a per-process command line, argv, pid, env value, or any free-form string. The published metric label set is EXACTLY {org, pool}, both bounded trusted control-plane values, so there is no per-sandbox/per-pid cardinality and no identifier beyond org+pool. Org is resolved via the SAME trusted `mitos.run/org` husk-pod label (`LabelOrgResolver`) the usage scraper uses; a sandbox with no resolvable org is left unattributed (counted), never attributed to a guessed or empty org. The node endpoint is NOT per-sandbox traffic, so it carries no per-sandbox bearer token (the controller holds none); it shares the access class of `/v1/metering`. A sandbox whose guest is unreachable is skipped and counted, never failing the report or the sample cycle. Residual: the endpoint is plaintext on the operational mux today (an https operational mux is the same documented follow-up as `/v1/metering`); the cpu_steal aggregation is the per-bucket MAX and memory/process_count the SUM, documented in each gauge's Help text. |

## Review gate

An external security review is required before any 1.0 (or "production-ready")
claim. Tracking: not scheduled. The hosted customer front door (section 7b,
issue #210: the public gateway, customer keys, and cross-org isolation) is
explicitly gated on this review (issue #194) before it serves production
tenants.

## Change discipline

Any PR that moves the security surface (new listener, new privilege, new
artifact type, new cross-component call) must update this file in the same PR.
