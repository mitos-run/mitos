# ADR 0006: the husk-pod NET_ADMIN capability for in-pod egress firewalling

Status: accepted, implementation KVM-verification pending (2026-06-15). The
control is CODED and unit/envtest-tested, but the husk-network KVM cluster e2e
does not yet pass (see Consequences/Status below), so the egress guarantee is not
yet proven end to end on a real VM.
Issue: #18 (W1 husk pods), #3 (network identity and egress policy), #30 (residual
ADRs). Related: docs/threat-model.md section 0 surface 5 (was the top
must-fix-first blocker; the control is now IMPLEMENTED but the KVM e2e is not yet
passing, so it stays OPEN), section 4 (per-mode egress enforcement)
and the K8s NetworkPolicy row, section 3 (default-SA-token note);
docs/husk-pods.md section 6d; the enforcement model is
docs/superpowers/plans/2026-06-15-husk-network-isolation.md
(`internal/netconf`, `internal/dnsproxy`); the shipped mechanism is the in-pod
filter `internal/husk/netfilter.go` applied in `internal/husk/stub.go`, the
threaded allowlist in `internal/controller/sandboxclaim_controller.go`
(`huskNotifyNetwork` + `huskEgressConfig`), the `NET_ADMIN` add in
`internal/controller/huskpod.go`, and the best-effort NetworkPolicy in
`internal/controller/husknetworkpolicy.go`.

## Context

In the husk default the VM's tap lives inside the HUSK POD's network namespace,
so the sandbox's egress IS the pod's egress (docs/threat-model.md section 0
surface 5). Before this change the extended security review found that the
project shipped NO egress enforcement on the husk default path:

- The product created NO `NetworkPolicy`; no Go code imported
  `networking.k8s.io/v1` or constructed one, and `huskNotifyNetwork` returned nil
  (the per-template allowlist was never threaded into the husk guest network).
- The host-nftables egress dataplane (docs/threat-model.md section 4) was NOT
  installed for husk pods; it ran only on the opt-in raw-forkd path with
  `--enable-networking`.

Consequence, before the fix: husk egress was default-OPEN, the cloud metadata
endpoint `169.254.169.254` was reachable from a guest, and a guest (untrusted
code, by design) could fetch the node's cloud IAM credentials. This was the TOP
must-fix-first blocker for running untrusted code, now closed by the decision
below.

The enforcement model Mitos already proved on the raw-forkd path is host-side
nftables on the tap: a per-tap default-deny egress ruleset, accept
established/related, accept the allowlisted destinations, accept DNS only to the
controlled node resolver, terminal drop, with the policy rendered and applied
HOST-SIDE so the guest cannot influence it (docs/threat-model.md section 4;
docs/superpowers/plans/2026-06-11-guest-networking.md). On the husk path the tap
is in the POD's netns, not the host netns, so applying that same ruleset requires
configuring nftables and the tap from INSIDE the pod's network namespace, which a
fully drop-ALL unprivileged container cannot do.

This needs a recorded decision because it is a security-surface change to an
otherwise unprivileged, PSA-restricted-minus-two pod (ADR 0003): adding ANY Linux
capability back to that pod must be justified and bounded, or it erodes the husk
model's core property.

## Decision: add exactly one scoped NET_ADMIN capability, in the pod's own netns, as the minimal control for default-deny husk egress plus a metadata block

The recorded decision (the planned remediation for surface 5) is to enforce husk
egress INSIDE the husk pod with a host-side-style nftables ruleset applied to the
pod's own tap, and to grant the husk pod exactly ONE additional Linux capability,
`NET_ADMIN`, to do so. The bounding properties that make one capability the
MINIMAL control:

- `NET_ADMIN` is scoped to the POD'S OWN network namespace. The husk pod has its
  own netns (it is an ordinary pod), so `NET_ADMIN` lets the in-pod stub program
  nftables and configure the tap for THIS pod's egress only; it cannot touch the
  host netns, another pod's netns, or the node's routing. The capability's blast
  radius is the pod's own network namespace, not the node.
- It is added to an otherwise drop-ALL pod: `capabilities.drop: [ALL]` stays, and
  `NET_ADMIN` is the ONLY entry added back (`capabilities.add: [NET_ADMIN]`).
  `privileged: false`, `allowPrivilegeEscalation: false`, and `seccompProfile:
  RuntimeDefault` are unchanged. So the pod remains unprivileged with a single
  scoped network capability, not privileged-class.
- The control it buys is default-DENY egress with an explicit `169.254.169.254`
  metadata block and the per-template allowlist actually threaded through
  (`huskNotifyNetwork` wired to build the allowlist, the in-pod stub rendering and
  applying the per-tap ruleset via the existing `internal/netconf` /
  `internal/network` / `internal/dnsproxy` primitives). The default stays DENY;
  the metadata endpoint is blocked; allowed destinations are the operator/template
  allowlist plus DNS to the controlled resolver only.

The enforcement is HOST-SIDE-STYLE in the sense that matters: it is applied by the
trusted in-pod stub (`cmd/husk-stub`), not by the guest, and the GUEST cannot edit
nftables (its only network config is its own eth0 address), exactly as on the
raw-forkd path (docs/threat-model.md section 4 "host-side enforcement" row). The
trust boundary is the stub, which already holds the per-sandbox token and the
control channel; the guest never gains `NET_ADMIN`.

## Why one scoped capability rather than the alternatives

- A cluster-wide CNI default-deny is an OPERATOR action, not something this
  project can ship, require, or detect the absence of (docs/threat-model.md
  section 0 surface 5). Relying on it would leave the default posture open and
  silently unsafe.
- A `NetworkPolicy` cannot, by itself, block the link-local metadata endpoint
  reliably across CNIs, cannot enforce the name-based allowlist, and would still
  require a CNI that honors egress policy; the threat model records that the CI
  step which "proved" a husk NetworkPolicy actually applied a hand-written
  allow-all (`0.0.0.0/0`) object and proved nothing about restriction
  (docs/threat-model.md section 4 K8s NetworkPolicy row). A `NetworkPolicy` MAY
  be added as a defense-in-depth layer, but it is not the load-bearing control.
- Running the per-VM Firecracker jailer in the pod (which would also need network
  capabilities) was declined because the full jailer set makes every husk pod
  privileged-class and breaks the PSA-restricted model (docs/threat-model.md
  section 0). `NET_ADMIN` alone is the minimal addition that does not.
- Granting more than `NET_ADMIN` (or `privileged`) would re-expose the surface
  ADR 0003 closed. One scoped capability is the smallest grant that enforces
  default-deny egress in the pod's own netns.

## Consequences

- This ADR is `accepted, implementation KVM-verification pending`: the control is
  CODED and unit/envtest-tested, but it is NOT yet proven end to end on a real VM.
  The husk-stub programs the in-pod nftables default-deny egress filter in the
  pod's own netns at activation (`internal/husk/netfilter.go`,
  `internal/husk/stub.go`), the unconditional cloud-metadata block is rendered
  before any allow (`netconf.RenderMetadataBlock`), the per-template allowlist is
  threaded through (`internal/controller/sandboxclaim_controller.go`,
  `husk.ActivateRequest.Egress`/`Allow`), and the controller emits a best-effort
  NetworkPolicy as defense in depth (`internal/controller/husknetworkpolicy.go`).
  STATUS / KVM-verification pending: the in-VM proof on a KVM-capable kubelet
  (default-deny enforced, metadata blocked, allowlist honored) is the gated
  cluster e2e `test/cluster-e2e/husk-network-e2e.sh` (the
  `cluster-husk-network-e2e` suite), and that e2e does NOT yet pass. It fails
  BEFORE the enforcement assertions run: the network husk pods do not hold the
  pool's dormant-Ready count (the controller logs `dormant:0 desired:1 creating:1`
  repeatedly, the pods churn and one crash-loops), so the claim never reaches
  Ready, never activates, and the three enforcement curls (metadata-blocked,
  default-deny, allowlist-allowed) never execute. The in-pod filter applies only
  at activation, which the e2e never reaches, so the default-deny and metadata
  block are DESIGN claims that have NOT been observed working inside a real VM.
  Two real bugs found while debugging this are tracked OPEN in the threat model
  (the dehydrate-on-delete finalizer hot-loop on a deleted workspace, and the husk
  warm pool over-creating dormant pods when claims cannot be satisfied). The
  threat-model surface-5 status is therefore "IMPLEMENTED, KVM end-to-end
  verification IN PROGRESS / NOT yet passing", and husk egress stays OPEN for the
  untrusted-code claim until this e2e is green.
- The PSA exception accounting (ADR 0003) gains a THIRD documented item for the
  husk pod: the single `capabilities.add: [NET_ADMIN]`. The threat model exception
  list now names it, so "drop ALL capabilities" reads "drop ALL except the one
  scoped `NET_ADMIN` for in-netns egress firewalling", stated honestly rather than
  omitted.
- The capability is bounded to the pod's own netns; it does not grant host
  network control. This bound is the load-bearing justification and must be
  preserved: any future use of `NET_ADMIN` that reached beyond the pod netns would
  be a new surface needing its own threat-model delta.
- This pairs with the separate open/high default-SA-token automount finding
  (docs/threat-model.md section 3): the egress control closes the metadata-credential
  theft vector, and `automountServiceAccountToken: false` closes the in-cluster
  token vector; both are part of the husk untrusted-code readiness set, tracked
  separately.
