# External security review: scope & package (G4, #194)

Mitos runs untrusted AI-agent code in Firecracker microVMs on shared Kubernetes
nodes. Before any 1.0 / production-tenant claim, an independent security review is
a gating requirement (ROADMAP G4, issue #194). This page is the package to hand a
reviewer: the trust model, the surfaces to examine, the threat model, and the
known findings already closed, so the engagement focuses on residual risk. The
live findings tracker that drives each finding to closure is
[external-review-findings.md](external-review-findings.md).

## Trust model (what is and is not trusted)

- **Untrusted:** code running inside a sandbox VM (the agent workload); a tenant
  with `create` on Sandbox in their namespace; anything reachable
  from inside the guest.
- **Trusted:** the controller, forkd, the host kernel, the node, the control-plane
  PKI CA. The husk stub is trusted host-side but serves an untrusted guest.
- **Boundary:** the microVM (Firecracker + KVM) is the primary isolation boundary
  between an untrusted guest and the host. The second boundary is Kubernetes RBAC
  + the admission webhook between tenants.

## Primary surfaces to review (highest risk first)

1. **VM escape / Firecracker config** (`internal/firecracker`, `internal/fork`):
   the device model, the jailer configuration, `/dev/kvm` exposure, the snapshot
   restore path. The forkd DaemonSet runs NON-privileged with the per-VM jailer
   ENABLED (#352, ADR 0008): an explicit builder capability set, `/dev/kvm` from
   the device plugin, every build/raw-forkd VMM under a throwaway jailed uid in a
   per-VM chroot. The residual is uid 0 + `CAP_SYS_ADMIN` + a node-data-dir
   hostPath; review that blast radius.
2. **Guest -> host channels** (`guest/agent-rs`, `internal/vsock`, `internal/daemon`
   :9091): the vsock + HTTP exec/file API the guest and SDK reach. Token gating,
   input handling, path traversal in the file API, the exec request parsing.
3. **Snapshot integrity** (`internal/fork`, `internal/husk`): the CAS
   manifest/digest verification on restore (verify-on-load), and the per-node
   digest handling (#175/#177). Can a tenant cause a sandbox to restore an
   unverified or swapped snapshot?
4. **Egress / network isolation** (`internal/dnsproxy`, `internal/netfilter`,
   husk in-pod nftables): default-deny egress, the allowlist, the cloud-metadata
   (169.254.169.254) block. DNS-rebind pinning. Verified working on real KVM
   (deny-default / allow / metadata-blocked) but the filter logic warrants review.
5. **Multi-tenant authz** (`internal/controller`, `internal/admission`): the
   ServiceAccount-impersonation admission webhook, the per-namespace husk PKI
   identity, the Secrets-RBAC narrowing, OwnerReferencesPermissionEnforcement on
   husk pods (the tenant-decoy-pod gate in `selectDormantHuskPod`).
6. **Secret handling**: secret VALUES must never be logged / in conditions / on
   host paths (the project rule). Forks duplicate guest memory incl. secrets and
   are default-denied without `spec.secretInheritance: inherit` (#fork-correctness §3).
7. **Fork correctness** (`docs/fork-correctness.md`): RNG reseed (credited
   entropy), clock resync, secret inheritance across CoW forks.

## Threat model & docs to read
- `docs/threat-model.md` (per-row status of the forkd threat surface).
- `docs/fork-correctness.md` (RNG/clock/secret hazards).
- `docs/platforms/prerequisites.md` (the host/kernel trust assumptions).
- This session's clean-room findings: `docs/superpowers/plans/2026-06-18-deployment-ux-findings.md`.

## Already closed (so the review can skip / spot-check, not re-find)
- Security audit (6 findings) shipped in 0.6.0; per-namespace husk identity; RNG
  reseed fail-closed.
- 0.7.x: husk cert rotation, Secrets-RBAC narrowing (scoped `bind`), DNS-rebind
  pinning, NAT64/6to4 embedded-IPv4 blocking.
- Verified live on bare metal: egress isolation (deny/allow/metadata-block),
  per-node snapshot digest, cross-node failover, no tenant-decoy husk activation.

## Known residual / explicitly out-of-scope-until-fixed
- forkd residual privilege after #352: non-privileged with the jailer enabled, but
  still uid 0 holding `CAP_SYS_ADMIN` with a node-data-dir hostPath (a hardened
  minimal builder, not zero-privilege; documented in the threat model and ADR
  0008).
- `dedicatedNodes` hard tenant node separation is operator-provided, not yet a
  product feature (#172); without it, tenants share a host kernel.
- Node-loss / fork-replica edge behaviors (#177 follow-ups, #183) are availability,
  not isolation, concerns.

## Suggested engagement shape
A focused review of surfaces 1-3 (the VM/guest/host boundary) is the highest
leverage; 4-7 are defense-in-depth. Provide the reviewer a KVM-capable cluster
(the bare-metal target) so the Firecracker/jailer path is exercised, not mocked.
