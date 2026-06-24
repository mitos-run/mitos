# GPU support and larger sandbox sizes (issue #221)

This document is the HONEST design for GPU passthrough into the microVM and for
sandbox sizes above the E2B 8 vCPU / 8 GiB / no-GPU class. It separates, clearly,
what is implemented and darwin-testable today from what is hardware-gated and must
be validated on real GPU KVM hardware before it can be claimed.

No GPU hardware was used to write or test any of this. Every statement about
device passthrough below is a DESIGN STANCE or a documented constraint, not a
verified result.

## Summary of what ships now vs what is hardware-gated

IMPLEMENTED and unit-tested on darwin (no hardware):

- Larger sandbox sizes. There is no hard vCPU/memory cap in the spec; the
  scheduler now admits an explicit large memory size against real node capacity
  (`SelectNodeForFork`, `internal/controller/scheduler.go`), so a sandbox above
  8 GiB lands only on a node that has the RAM, and is rejected with
  `ErrNoCapacity` when no node can hold it.
- The `resources.gpu: {count, type}` field on `SandboxPool.spec.template`
  (`api/v1/types.go`, `SandboxResources.GPU`).
- GPU-aware node SELECTION: a GPU pool is scheduled ONLY onto GPU-capable nodes
  (the registry's `GPUTotal`/`GPUType`, fed from the `mitos.run/gpu` and
  `mitos.run/gpu-type` node labels via `GPUFromNodeLabels`), mirroring how the
  KVM node selection pins husk pods to KVM nodes.
- GPU-seconds as a billable metering unit (`internal/metering`,
  `Sample.GPUSeconds`/`Report.TotalGPUSeconds`), summed straight per sandbox so
  the usage pipeline (#211) and Stripe metered billing (#212) can charge it
  alongside vCPU-seconds.

HARDWARE-GATED (NOT implemented, must be validated on real GPU KVM hardware):

- The actual VFIO device-attach into the Firecracker/husk microVM.
- The real per-device GPU-second measurement (on-device busy time).
- Any claim that fork-with-GPU works. It does NOT; see the stance below.

## GPU passthrough into the microVM: the honest constraints

### Which GPUs, and the node setup

GPU passthrough means assigning a physical PCI GPU to a single VM via VFIO. The
node prerequisites the OPERATOR owns (the controller cannot verify them):

- IOMMU enabled in firmware and kernel (`intel_iommu=on` / `amd_iommu=on`).
- The GPU in a CLEAN IOMMU group: no PCIe bridge or sibling device shared with
  the host in the same group, or the whole group must be passed and the host
  must not need any of it.
- The GPU bound to `vfio-pci` (unbound from the host driver) before a VM can
  claim it.
- A device plugin advertising the GPU as a schedulable node resource, and the
  node labeled `mitos.run/gpu` (count) and `mitos.run/gpu-type` (SKU), which is
  what the controller's node selection consumes.

Realistic first targets are datacenter NVIDIA SKUs commonly run under VFIO
(for example A100, H100, L4/L40, T4). Consumer cards work under VFIO too but are
out of scope for a supported tier. The exact validated SKU list MUST come from a
real hardware run; this document does not assert one.

### Firecracker upstream limitation (do not overclaim)

Firecracker's device model is deliberately minimal and, upstream, does NOT
provide general VFIO/PCI passthrough: it exposes virtio devices over MMIO, not a
PCI passthrough path. So "GPU on Firecracker" is not a flip-a-flag feature today;
it requires either a VMM that supports VFIO passthrough (for example
Cloud Hypervisor, which does support VFIO) or out-of-tree Firecracker work. This
is the central honest constraint: the snapshot-fork engine in this repo is built
around Firecracker's snapshot/restore, and adding a passthrough GPU is a
substantial VMM-level change, not an incremental one. The `husk` path (the
shipped default) runs the VMM inside a pod and would additionally need the device
surfaced into that pod (device plugin + the pod requesting the GPU resource).

We do not claim Firecracker passthrough works. The plumbing here is VMM-agnostic
at the scheduling/spec/metering layer; the device-attach implementation is the
hardware-gated follow-up and will name the VMM it targets when it lands.

## Snapshot/fork with a device attached: the stance

A GPU sandbox CANNOT be live-forked while the device is attached. This is a
DECISION, stated plainly as the issue requires, not an open question:

- A snapshot/fork captures and restores guest RAM (`MAP_PRIVATE` CoW). A
  passthrough PCI device has LIVE hardware state outside that RAM image: command
  queues, BAR-mapped registers, on-device memory, and active DMA mappings. None
  of that is in the guest memory snapshot, and it cannot be coherently cloned.
- Restoring two VMs that each believe they own the same physical GPU is
  incoherent and unsafe: both would drive the same queues and DMA engine.
- This matches Modal's documented behavior: memory-snapshot fork is
  GPU-incompatible.

Therefore: the fork engine MUST fail closed on a fork of a GPU-attached sandbox
when the device path is built. A GPU sandbox is a leaf in the fork tree. The
supported patterns are (a) build and fork the template BEFORE any GPU is
attached, then attach the GPU to each fork at start, or (b) detach the GPU,
snapshot, then re-attach, accepting the device-reset cost. We do NOT pretend fork
works with a live GPU.

This is why GPU support and the 1-to-N snapshot-fork story are largely
orthogonal: the fork superpower is a CPU/RAM story; GPU sandboxes trade the live
fork for raw device access, exactly as Modal does.

## GPU-seconds as a billable unit

GPU-seconds is the GPU analogue of vCPU-seconds: the wall-clock seconds a sandbox
holds its assigned GPU(s), multiplied by the device count (a 2-GPU sandbox alive
60s bills 120 GPU-seconds). It is added to `internal/metering`:

- `Sample.GPUCount` / `Sample.GPUSeconds` per sandbox; `Report.TotalGPUSeconds`.
- Unlike the CoW memory totals, GPU-seconds is SUMMED STRAIGHT, never
  deduplicated across forks: a GPU is assigned EXCLUSIVELY to one sandbox and is
  never CoW-shared (forks do not share a device, and a GPU sandbox does not fork
  anyway, per the stance above).

The accounting math is unit-tested (`TestAggregatePassesGPUSeconds`). The REAL
per-device measurement (reading actual on-device busy time, for example via NVML)
is hardware-gated; today the field is the billing seam the usage pipeline (#211)
and Stripe metered billing (#212) charge on, fed by wall-clock-held-device time.

## Larger sizes and the CoW metering/quota interaction

E2B caps at 8 vCPU / 8 GiB. There is no hard cap here. Two pieces make larger
sizes correct rather than just unbounded:

1. Capacity-aware admission. The scheduler's CoW bin-packing
   (`internal/controller/scheduler.go`) projects a fork's MARGINAL memory from
   the per-template estimate (shared-once + per-fork unique). A large sandbox's
   EXPLICIT size can exceed that marginal estimate, so `admitsRequest` now gates
   on the LARGER of the projected CoW cost and the request's explicit
   `MemoryBytes`. A 32 GiB sandbox is admitted only on a node whose headroom
   (`total*overcommitFactor - used`) actually fits 32 GiB, and is rejected with
   `ErrNoCapacity` everywhere else. This is unit-tested
   (`TestSelectNodeForForkAdmitsLargeSizeThatFits`,
   `...RejectsLargeSizeOverCapacity`, `...LargeSizeSpillsToBigNode`).

2. The CoW model still helps the SHARED part, not the unique part. Many large
   forks of one template still share that template's resident page set once
   (`docs/metering.md`), so density is preserved for the shared base; only each
   fork's UNIQUE divergence and its explicit size headroom are charged per fork.
   Quota (the budget model, `api/v1/budget.go`, and the per-org
   quotas of #213) bounds the SUM of unique footprint and now GPU-seconds, so a
   tenant cannot request unbounded large or GPU sandboxes; the overcommit factor
   is the operator's lever for how hard to lean on CoW sharing when packing.

GPU sizes interact with quota the same way: GPU-seconds is the metered unit, and
free-device count on a node is a hard scheduling ceiling (a GPU is exclusive, so
there is no overcommit for it, unlike CoW memory).

## Cross-references

- Threat model: device passthrough surface and the GPU node tier are in
  `docs/threat-model.md` section 5 (Device passthrough / GPU).
- Metering: `docs/metering.md` for the CoW model GPU-seconds sits alongside.
- Node selection mirrors the KVM node selection in
  `internal/controller/huskpod.go` (`huskKVMNodeLabel`).
