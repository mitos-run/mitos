# PVM as a no-nested-virt node tier: evaluation

This document is the HONEST evaluation of PVM (pagetable-based virtual machine,
Ant/Alibaba) as a Mitos node tier that runs Firecracker on plain cloud VPS with
no nested virtualization. It is an EVALUATION, not an adoption. No PVM kernel was
built and no measurement was taken; every number below is a target to be produced
by the spike, never a claimed result (the no-unverified-claims rule).

Status: **evaluated, NOT adopted.** What ships alongside this doc is the
isolation-tier control the threat model needs regardless of PVM (the
`mitos.run/isolation-tier` node label, the `minIsolationTier` /
`requireHardwareKvm` template floor, and the scheduler filter that keeps a
security-sensitive tenant off a lower-assurance node). The actual PVM kernel and
guest work is a clearly-marked spike follow-up below.

## What PVM is

PVM is a KVM vendor module (out-of-tree, RFC, NOT mainlined) that runs guests in
ring 3 using pagetable switching instead of hardware virtualization. It needs no
VMX (Intel VT-x) or SVM (AMD-V), exposes the standard `/dev/kvm` ABI, and runs
Firecracker unmodified against that ABI. It is used in production at Alibaba/Ant.

The consequence that matters to Mitos: most cloud VPS (Hetzner Cloud and the
majority of commodity providers) do NOT expose `/dev/kvm` because they do not
offer nested virtualization. On such a host, Firecracker cannot start today. With
a PVM host kernel, it can.

References:
- https://lwn.net/Articles/963718/
- https://blog.alexellis.io/how-to-run-firecracker-without-kvm-on-regular-cloud-vms/
- https://docs.slicervm.com/tasks/pvm/

## The benefit: run anywhere

The single, real benefit is RUN-ANYWHERE. With a PVM tier, a self-hoster can run
Mitos's Firecracker snapshot-fork sandboxes on ANY cloud VPS that exposes no
nested virt: a plain Hetzner Cloud `cpx` instance, a generic DigitalOcean or
Vultr droplet, a bare provider VM. That is a story no competitor offers
self-hosters: the snapshot-fork primitive without requiring a metal host or a
nested-virt-capable cloud tier. For Mitos specifically, bare metal is already a
first-class target; PVM extends the reach to the commodity-VPS long tail without
asking the operator to find KVM-capable hosts.

This benefit is real ONLY if the core primitives survive the move (see the spike)
and ONLY if the lower assurance is honestly disclosed and enforced (see the
threat model). A run-anywhere tier that silently weakens isolation is not a
feature; it is a liability. The isolation-tier control shipped here is what keeps
it honest.

## The costs, evaluated honestly

PVM is not free. Each cost below is a real, recurring tax, not a one-time setup.

1. **Out-of-tree HOST kernel patches.** PVM is a forked KVM module on an
   out-of-tree, RFC kernel patch set. Mitos targets Talos; every Talos release
   would need a forked kernel build carrying the PVM patches. This DOUBLES the
   kernel-distributor tax: a second kernel flavor to
   build, sign, distribute, and keep current with CVE fixes, forever, until (and
   if) PVM mainlines. A lagging PVM kernel is a security liability, so the tax is
   not optional maintenance.

2. **A PVM-enlightened GUEST kernel.** PVM guests run a PVM-aware guest kernel.
   That is ANOTHER image-pipeline flavor on top of the existing guest image work:
   a second guest kernel to build and ship, and to keep aligned with
   the host module's ABI.

3. **Core-primitive validation under PVM.** Mitos's whole value is the
   snapshot-fork primitive. Every core mechanism must be RE-VALIDATED under PVM,
   because PVM is a different execution substrate, not a drop-in:
   - snapshot/restore correctness,
   - copy-on-write `MAP_PRIVATE` mmap semantics for the forked guest RAM,
   - dirty-page tracking,
   - `kvm-clock` and time correctness across a fork,
   - the ENTIRE fork-correctness suite (RNG reseed, clock monotonicity,
     secret non-inheritance, etc.) must pass under PVM.
   PVM uses shadow paging, which PENALIZES pagetable-heavy workloads; fork latency
   and exec round-trip must be measured, not assumed comparable to hardware KVM.

4. **Lower isolation tier.** Ring-3 pagetable isolation is WEAKER than hardware
   virtualization: no VT-x/AMD-V root-mode boundary, a larger and out-of-tree host
   TCB, and more shared host privilege machinery. A PVM node MUST be a documented
   LOWER-assurance tier in the threat model, never silently equivalent to a
   hardware-KVM node. This is the non-negotiable cost: if PVM ships, it ships as a
   marked tier with an enforceable floor that keeps security-sensitive tenants off
   it. That control is what this issue's achievable slice delivers
   (`docs/threat-model.md`, the isolation-tier section).

## The spike plan (the work this evaluation does NOT do)

The spike requires provisioning a real cloud VM with a forked kernel; it is NOT
performed here. The exact steps, for whoever runs it:

1. Provision a scratch cloud VM with no nested virt (a Hetzner `cpx` instance is
   the canonical target: it exposes no `/dev/kvm`).
2. Build and boot a PVM host kernel on it (the out-of-tree patch set), and confirm
   `/dev/kvm` appears and Firecracker starts under `kvm-pvm`.
3. Run the existing `kvm-test` suite (the real-Firecracker snapshot/restore plus
   guest-agent exec over vsock path, `kvm-test.yaml`) under PVM and record
   pass/fail per assertion.
4. Run the fork-correctness suite under PVM once it exists; it is a
   HARD gate: PVM cannot ship if fork-correctness does not pass under it.
5. Measure, against a real hardware-KVM baseline AND a gVisor systrap baseline:
   - 1-to-N fork latency (reuse the bench harness),
   - exec round-trip latency,
   - a pagetable-heavy workload (to expose the shadow-paging penalty).
   Record every number in `bench/` so it is reproducible; do NOT write a number
   into any doc that `bench/` cannot reproduce.
6. Assess Talos packaging: how much the PVM kernel build adds to the
   kernel-distributor pipeline, concretely, not in the abstract.

## Upstream mainlining: revisit when merged

PVM is an RFC and is NOT mainlined as of this evaluation. The out-of-tree-kernel
tax (cost 1) is the dominant ongoing cost and it largely DISAPPEARS if PVM lands
in the mainline kernel: a stock Talos kernel would then carry it, and the second
kernel flavor goes away. If and when PVM merges upstream, this evaluation should be revisited: mainlining
materially shifts the decision framework below by removing the heaviest recurring
cost.

## Decision framework: when PVM is worth adopting vs not

This is deliberately not pre-decided. Adopt PVM only when the gating conditions
are met; otherwise do not.

**Adopt PVM as a tier when ALL of these hold:**
- The fork-correctness suite PASSES under PVM, with evidence in
  `bench/`. This is non-negotiable: an incorrect fork is worse than no fork.
- `kvm-test` snapshot/restore and guest-agent exec pass under PVM.
- Measured fork latency and exec round-trip are within an acceptable multiple of
  hardware KVM for the target workloads (the shadow-paging penalty on
  pagetable-heavy workloads is understood and bounded), with the numbers in
  `bench/`.
- There is concrete, sustained demand for run-anywhere on no-nested-virt VPS that
  cannot be served by bare metal or a nested-virt-capable cloud tier.
- The isolation-tier control (shipped here) is enforced so PVM nodes are a marked
  lower-assurance tier and security-sensitive tenants are kept off them, AND the
  threat-model row for PVM co-tenancy is honored operationally.
- The PVM kernel maintenance (cost 1) is staffed: someone owns rebuilding and
  CVE-patching the forked kernel per Talos release. A lagging PVM kernel is a
  security liability, so unstaffed maintenance is a reason NOT to adopt.

**Do NOT adopt PVM when ANY of these hold:**
- Fork-correctness does not pass under PVM, or the latency penalty is unacceptable
  for the target workloads.
- The kernel-distributor tax cannot be staffed sustainably.
- The demand is better served by bare metal (already first-class) or by a
  nested-virt-capable cloud tier, making the run-anywhere benefit marginal.
- The lower-assurance tier cannot be cleanly isolated from security-sensitive
  multi-tenant workloads in the target deployment.

**Strong signal to revisit:** PVM mainlines upstream (removes cost 1), OR a
concrete customer requires Firecracker on a specific no-nested-virt VPS that
Mitos cannot otherwise serve.

## What ships now (the achievable, valuable slice)

- This evaluation doc (honest cost/benefit, spike plan, decision framework).
- The node isolation-tier model and label seam: `IsolationTier`,
  `IsolationTierFromNodeLabels`, and `NodeInfo.IsolationTier`
  (`internal/controller/isolation_tier.go`, `node_registry.go`), reading the
  `mitos.run/isolation-tier` node label (`hardware-kvm` / `pvm` / `gvisor`), with
  an undeclared node treated as the lowest assurance (fail-closed).
- The assurance FLOOR on the template: `spec.template.minIsolationTier` and the
  convenience `spec.template.requireHardwareKvm` (`api/v1/types.go`), folded into the
  required tier by `MinIsolationTierFromSpec` (`requireHardwareKvm` can only
  tighten, never weaken).
- The scheduler tier filter: node selection admits only nodes whose declared tier
  MEETS the request's floor (`internal/controller/scheduler.go` `admitsTier`,
  wired through `selectNode` in `sandboxclaim_controller.go`), failing loudly with
  `ErrNoCapacity` when no node qualifies. Unit-tested
  (`internal/controller/isolation_tier_test.go`).
- The threat-model tier (`docs/threat-model.md`): PVM as a documented
  lower-assurance tier, the floor control as the mitigation, and the opt-in-only
  co-tenancy posture. The same control also covers the existing gVisor tier and
  ADR 0005 (raw forkd is not multi-tenant).

The PVM host kernel, the PVM guest kernel, and the measurements are the
clearly-marked spike follow-up; none of it is implemented or claimed here.
