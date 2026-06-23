# PVM as a no-nested-virt node tier: evaluation (issue #40)

This document is the HONEST evaluation of PVM (pagetable-based virtual machine,
Ant/Alibaba) as a mitos node tier that runs Firecracker on plain cloud VPS with
no nested virtualization. It is an EVALUATION, not an adoption. The spike below
WAS run on 2026-06-23 (see "Spike results"); its findings are reported as
measured, and the one latency figure is explicitly marked indicative, not a
benchmark, because it was taken with a debug-laden guest kernel and a non-minimal
config (the no-unverified-claims rule). The reproducer is `bench/pvm-spike.sh`.

Status: **evaluated, NOT adopted (spike confirms the core fork primitive is
blocked under PVM today, see below).** What ships alongside this doc is the
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

The consequence that matters to mitos: most cloud VPS (Hetzner Cloud and the
majority of commodity providers) do NOT expose `/dev/kvm` because they do not
offer nested virtualization. On such a host, Firecracker cannot start today. With
a PVM host kernel, it can.

References:
- https://lwn.net/Articles/963718/
- https://blog.alexellis.io/how-to-run-firecracker-without-kvm-on-regular-cloud-vms/
- https://docs.slicervm.com/tasks/pvm/

## The benefit: run anywhere

The single, real benefit is RUN-ANYWHERE. With a PVM tier, a self-hoster can run
mitos's Firecracker snapshot-fork sandboxes on ANY cloud VPS that exposes no
nested virt: a plain Hetzner Cloud `cpx` instance, a generic DigitalOcean or
Vultr droplet, a bare provider VM. That is a story no competitor offers
self-hosters: the snapshot-fork primitive without requiring a metal host or a
nested-virt-capable cloud tier. For mitos specifically, bare metal is already a
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
   out-of-tree, RFC kernel patch set. mitos targets Talos; every Talos release
   would need a forked kernel build carrying the PVM patches. This DOUBLES the
   kernel-distributor tax already tracked in issue #35: a second kernel flavor to
   build, sign, distribute, and keep current with CVE fixes, forever, until (and
   if) PVM mainlines. A lagging PVM kernel is a security liability, so the tax is
   not optional maintenance.

2. **A PVM-enlightened GUEST kernel.** PVM guests run a PVM-aware guest kernel.
   That is ANOTHER image-pipeline flavor on top of the existing guest image work
   (issue #10): a second guest kernel to build and ship, and to keep aligned with
   the host module's ABI.

3. **Core-primitive validation under PVM.** mitos's whole value is the
   snapshot-fork primitive. Every core mechanism must be RE-VALIDATED under PVM,
   because PVM is a different execution substrate, not a drop-in:
   - snapshot/restore correctness,
   - copy-on-write `MAP_PRIVATE` mmap semantics for the forked guest RAM,
   - dirty-page tracking,
   - `kvm-clock` and time correctness across a fork,
   - the ENTIRE fork-correctness suite (issue #3: RNG reseed, clock monotonicity,
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
4. Run the fork-correctness suite (issue #3) under PVM once it exists; it is a
   HARD gate: PVM cannot ship if fork-correctness does not pass under it.
5. Measure, against a real hardware-KVM baseline AND a gVisor systrap baseline:
   - 1-to-N fork latency (reuse the bench harness, issue #207/#15),
   - exec round-trip latency,
   - a pagetable-heavy workload (to expose the shadow-paging penalty).
   Record every number in `bench/` so it is reproducible; do NOT write a number
   into any doc that `bench/` cannot reproduce.
6. Assess Talos packaging: how much the PVM kernel build adds to the
   kernel-distributor pipeline (issue #35), concretely, not in the abstract.

## Spike results (2026-06-23)

The spike was run on a throwaway Hetzner CPX22 (2 vCPU AMD EPYC, 4 GiB, Ubuntu
26.04) that exposed no `vmx`/`svm` and no `/dev/kvm`: the exact no-nested-virt
substrate this tier targets. Reproducer: `bench/pvm-spike.sh`. Stack: the
prebuilt PVM host kernel 6.12.33 (`actuated-kernel-pvm-host`), a PVM-enlightened
guest `vmlinux` built from `virt-pvm/linux@pvm-612` with the published guest
config, and the Loophole Labs PVM Firecracker fork (`v1.13.0-dev`).

| Primitive | Result |
|---|---|
| `/dev/kvm` appears under `kvm_pvm` on a host with no hardware virt | PASS |
| microVM boot + guest exec (guest sees `virtflag-count=0`, `kvm-clock`) | PASS |
| Firecracker snapshot CREATE (full: vmstate + mem) | PASS |
| Firecracker snapshot RESTORE | **FAIL** |

The run-anywhere premise holds at the substrate level: a PVM host kernel does
make `/dev/kvm` appear on a plain VPS, and the PVM Firecracker fork boots a guest
and runs code there. But the CORE mitos primitive does not survive the move.

**Blocker: snapshot restore fails.** Restore aborts with `Failed to set all KVM
MSRs for this vCPU. Only a partial write was done`. Host `dmesg` pins the exact
cause: `Unhandled WRMSR(0xc0010007)`, which is `MSR_K7_PERFCTR3`, an AMD
performance-counter MSR (CPX is AMD EPYC). The PVM module does not handle this
MSR, so `KVM_SET_MSRS` partial-writes the set and Firecracker treats the partial
write as fatal. This is decisive for mitos because the fork primitive IS
restore-from-snapshot: boot works, but fork does not, and fork latency is
therefore unmeasurable under PVM until this is fixed.

**Attempted mitigation that did NOT work:** reloading `kvm` with `enable_pmu=0`.
The AMD perfctr MSR stays in Firecracker's restore MSR set regardless, and the
module still rejects it. So there is no config knob; restore needs a code change.

**Candidate fixes for a follow-up spike (none validated):**
- Patch the PVM KVM module to accept (or no-op) the AMD perfctr MSRs.
- Patch the Firecracker fork to tolerate a partial MSR write or drop unsupported
  MSRs from the restore set.
- Re-run on an Intel CPX, since `0xc0010007` is an AMD-specific MSR; the failing
  MSR set may differ (or not appear) on Intel.

**Indicative timing, NOT a benchmark:** cold-boot `InstanceStart` to first guest
exec marker was about 0.81 s, measured with a 277 MB debug `vmlinux` and a
non-minimal guest config. It is recorded only to show boot is in a sane range; it
is not a published number and is not in `bench/` results, per principle 1.

This moves the decision framework below: the "kvm-test snapshot/restore passes
under PVM" gate is currently a hard FAIL, so PVM is not adoptable as a fork tier
today regardless of the other costs. Re-evaluate after the restore blocker is
fixed or PVM mainlines.

## Upstream mainlining: revisit when merged

PVM is an RFC and is NOT mainlined as of this evaluation. The out-of-tree-kernel
tax (cost 1) is the dominant ongoing cost and it largely DISAPPEARS if PVM lands
in the mainline kernel: a stock Talos kernel would then carry it, and the second
kernel flavor goes away. TRACKING NOTE: revisit this evaluation if and when PVM
merges upstream. Mainlining materially shifts the decision framework below by
removing the heaviest recurring cost.

## Decision framework: when PVM is worth adopting vs not

This is deliberately not pre-decided. Adopt PVM only when the gating conditions
are met; otherwise do not.

**Adopt PVM as a tier when ALL of these hold:**
- The fork-correctness suite (issue #3) PASSES under PVM, with evidence in
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
- The PVM kernel maintenance (cost 1, #35) is staffed: someone owns rebuilding and
  CVE-patching the forked kernel per Talos release. A lagging PVM kernel is a
  security liability, so unstaffed maintenance is a reason NOT to adopt.

**Do NOT adopt PVM when ANY of these hold:**
- Fork-correctness does not pass under PVM, or the latency penalty is unacceptable
  for the target workloads.
- The kernel-distributor tax (#35) cannot be staffed sustainably.
- The demand is better served by bare metal (already first-class) or by a
  nested-virt-capable cloud tier, making the run-anywhere benefit marginal.
- The lower-assurance tier cannot be cleanly isolated from security-sensitive
  multi-tenant workloads in the target deployment.

**Strong signal to revisit:** PVM mainlines upstream (removes cost 1), OR a
concrete customer requires Firecracker on a specific no-nested-virt VPS that
mitos cannot otherwise serve.

## What ships now (the achievable, valuable slice)

- This evaluation doc (honest cost/benefit, spike plan, decision framework).
- The node isolation-tier model and label seam: `IsolationTier`,
  `IsolationTierFromNodeLabels`, and `NodeInfo.IsolationTier`
  (`internal/controller/isolation_tier.go`, `node_registry.go`), reading the
  `mitos.run/isolation-tier` node label (`hardware-kvm` / `pvm` / `gvisor`), with
  an undeclared node treated as the lowest assurance (fail-closed).
- The assurance FLOOR on the template: `spec.minIsolationTier` and the
  convenience `spec.requireHardwareKvm` (`api/v1alpha1/types.go`), folded into the
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
