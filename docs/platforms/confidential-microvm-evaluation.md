# Confidential forkable microVMs (AMD SEV-SNP / Intel TDX): spike

Issue: #354. This is a research SPIKE, not a delivery item. It answers one
question: can hardware memory-encrypted confidential VMs (AMD SEV-SNP, Intel TDX)
coexist with Mitos's snapshot/restore plus copy-on-write (CoW) fork model? No
confidential hardware was provisioned and no measurement was taken; every claim
below is sourced to primary documentation, and anything not yet hardware-verified
is marked as such (the no-unverified-claims rule). Builds on the PVM spike (#40,
`docs/platforms/pvm-evaluation.md`). Relates to `docs/threat-model.md` and
`docs/platforms/prerequisites.md`.

## Go/no-go

**NO-GO for confidential warm-fork.** "Confidential VM + warm fork" is infeasible
on current Firecracker and current hardware, for three independent reasons, any
one of which is disqualifying:

1. **Firecracker has no confidential-computing support at all**, and its
   maintainers have stated it is not on the roadmap, naming snapshot/restore (the
   exact primitive Mitos depends on) as the blocker.
2. **Snapshot/restore is fundamentally incompatible with SEV-SNP/TDX.** The host
   cannot read plaintext guest RAM to serialize it, and the ciphertext is
   physical-address-bound and integrity-protected, so it cannot be dumped to a
   host file and reloaded.
3. **CoW fork is doubly impossible.** Per-guest encryption keys make a shared
   ciphertext page meaningless across VMs, and the hardware ownership tables (SNP
   RMP, TDX HKID/TD-owner) admit exactly one owner per physical page. Warm-fork
   also breaks the attestation chain of trust, because a restored child never
   undergoes a fresh hardware-measured launch.

The only viable shape is a SEPARATE, second isolation tier: confidential
cold-boot sandboxes, each a fresh per-VM attested launch, with NO warm fork and NO
snapshot sharing, running on a DIFFERENT VMM (QEMU or cloud-hypervisor), not
Firecracker. This tier trades away Mitos's headline fork-speed advantage for
hardware confidentiality and attestation; the two are mutually exclusive on the
same VM.

## 1. Firecracker has no confidential support, and it is off-roadmap

Upstream Firecracker supports neither AMD SEV/SEV-ES/SEV-SNP nor Intel TDX. In
Firecracker issue #2332 (closed), a maintainer stated Firecracker "does not
support AMD Secure Encrypted Virtualization ... because we provide snapshot
capabilities and any operations that involve saving and restoring the memory and
state of the VM are unsupported by SEV," and a second maintainer confirmed it "is
not on our roadmap." Corroboration:

- Kata Containers' hypervisor comparison marks Firecracker as "no" for both Intel
  TDX and AMD SEV-SNP.
- Firecracker's shipped guest kernel config disables confidential-guest support
  (`# CONFIG_INTEL_TDX_GUEST is not set`, `# CONFIG_AMD_MEM_ENCRYPT is not set`).
- Only a research prototype (SEVeriFast) pairs SEV with Firecracker; there is no
  production path.

Firecracker's snapshot file "contains a full copy of the guest memory" written to
a host-readable file, which is exactly the operation SEV/TDX is designed to
prevent.

Sources: github.com/firecracker-microvm/firecracker/issues/2332;
github.com/kata-containers/kata-containers/blob/main/docs/hypervisors.md;
firecracker resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config;
github.com/SEVeriFast/severifast.

## 2. Snapshot/restore vs memory encryption

A normal VMM snapshot assumes the host can read guest RAM as plaintext, serialize
it, and reload it. SEV-SNP and TDX invert that assumption.

- **The host never sees plaintext.** Under SEV the hypervisor sees guest data
  "only in its encrypted form"; under TDX a non-SEAM host read of TD private
  memory "receives all zeros" and a host write poisons the line so a later read
  machine-checks.
- **The ciphertext is not portable.** SEV encrypts with a tweak "based on the
  physical address" (an explicit anti-ciphertext-relocation measure), so identical
  plaintext at a different physical address yields different ciphertext; the key is
  held by the SEV firmware and never exported to the hypervisor.
- **Live migration, the closest analog to snapshot/restore, is firmware-mediated,
  not a host memcpy.** SEV-SNP migration goes through an in-guest Migration Agent
  (MA) and firmware `SNP_PAGE_SWAP_OUT/IN/MOVE` re-encryption with per-page
  metadata (the "Migration Information Page" MDATA entries: IV, AUTH_TAG, GPA,
  validation state). TDX migration routes through a MigTD service TD and the TDX
  Module's `TDH.EXPORT/IMPORT` SEAMCALLs. Both are a MOVE that pauses or tears
  down the source, not a fork; standard hypervisor-driven live migration "is not
  applicable for CVMs," and QEMU still lists SEV snapshot/migration as unsupported.

Sources: AMD SEV-SNP whitepaper and ciphertext bulletin; AMD SEV-SNP Firmware ABI
56860; Intel TDX module spec and TD Migration spec 348550; kernel.org TDX doc;
"Intel TDX Demystified" (arXiv 2303.15540); "Confidential VMs Explained"
(SIGMETRICS 2024); QEMU amd-memory-encryption doc.

## 3. CoW fork specifically: doubly impossible across guests

Forking restores one snapshot into many VMs sharing CoW backing pages, which
requires N guests to read identical bytes from one physical frame. SEV-SNP and TDX
forbid both halves:

- **Per-guest keys make sharing meaningless.** Each SEV guest gets a unique key
  set keyed by its ASID; the same physical bytes read under another VM's key are
  garbage. TDX is the same, keyed per-TD by HKID.
- **Single ownership in hardware.** The SEV-SNP Reverse Map Table (RMP) has one
  entry per physical page recording its single validated owner; "the RMP ensures
  that a page cannot be mapped into multiple guests at once," and ownership is set
  by the guest via `PVALIDATE`, which the hypervisor cannot fake. TDX sets a
  TD-owner bit binding a private page to one TD. There is no representation for two
  simultaneous validated owners.
- **Page dedup confirms it at the OS layer.** KSM cannot merge guest pages under
  SEV because the host cannot read guest memory.

Consequence for Mitos: the warm-fork primitive (one parent snapshot CoW-shared
across many children) is architecturally incompatible with running each fork as a
SEPARATE confidential guest. Separate ASIDs/HKIDs cannot share the parent's
encrypted pages; a confidential fork would have to either stay within a single
guest's trust boundary (not separate sandboxes) or pay full per-fork memory cost
with firmware-mediated re-encryption (not CoW).

Sources: AMD SEV-SNP whitepaper (RMP single-owner); AMD ciphertext bulletin
(per-ASID keys); "Intel TDX Demystified" (per-TD HKID); KSM documentation.

## 4. The MSR 0xc0010007 finding (correction to #40)

The PVM spike (#40) recorded that snapshot RESTORE failed on an AMD host with an
"unhandled MSR 0xc0010007," and treated it as a possible SEV-class blocker. That
attribution is WRONG and should be corrected: MSR `0xc0010007` is
`MSR_K7_PERFCTR3`, the AMD K7-family Performance Counter 3 (a PMU data counter),
verified against the Linux kernel header `arch/x86/include/asm/msr-index.h`. It is
NOT a SEV MSR; the SEV MSRs are higher (`MSR_AMD64_SYSCFG` `0xc0010010`,
`MSR_AMD64_SEV` `0xc0010131`).

So the #40 restore failure is most plausibly a PMU/CPUID modeling mismatch in
Firecracker's snapshot replay (Firecracker serializing an AMD PMU MSR the
destination vCPU does not expose, which `KVM_SET_MSRS` then rejects, stopping at
that entry), NOT a confidential-computing problem. This matters: #40 and #354
should not be conflated. The genuine SEV-snapshot impossibility (sections 2 and 3)
stands entirely on its own; the `0xc0010007` symptom is a separate, ordinary
PMU-replay bug and should be re-filed as such.

Sources: torvalds/linux `arch/x86/include/asm/msr-index.h`; KVM API doc
(`KVM_SET_MSRS` stop-on-failure semantics).

## 5. Attestation and warm fork

Attestation measures the INITIAL measured launch state, recorded by the hardware
at guest creation: SEV-SNP's launch MEASUREMENT (a hash of initial guest memory +
vCPU state, signed to an AMD root of trust), or TDX's MRTD plus RTMRs surfaced via
TDREPORT -> Quote. Freshness is enforced by a verifier nonce embedded in the
signed report.

A warm-forked or snapshot-restored CVM never executes that hardware launch
sequence: the child resumes from previously-running encrypted memory, with no
`SNP_LAUNCH_UPDATE/FINISH` or TD-build pass to recompute a fresh launch digest. So
a restored instance can present only the parent's stale measurement (a replay,
defeated by the nonce) or no valid hardware launch binding at all. The key broker
rejects it because the evidence cannot be both fresh and tied to a genuine
measured launch. Warm fork therefore breaks the attestation chain of trust by
construction.

Sources: AWS and Google Cloud SEV-SNP/TDX attestation docs; Linux sev-guest doc;
SNPGuard (arXiv 2406.01186).

## 6. The frontier comparison: Kata Confidential Containers

Kata CoCo (the confidential-containers project) is the frontier production design,
and it confirms the recommended shape. Each pod runs in a confidential VM whose
lifecycle is strictly: boot a fresh CVM (TDX or SEV-SNP) -> hardware measures the
launch -> attest via a Key Broker Service (KBS/Trustee, the RCAR handshake) ->
release secrets. There is NO fork, snapshot, save, or restore primitive anywhere
in the design. Its confidential path runs on QEMU (and Cloud Hypervisor);
Firecracker is explicitly excluded.

Sources: confidentialcontainers.org architecture and attestation docs;
github.com/confidential-containers/trustee; Kata hypervisors.md.

## 7. Hardware and VMM availability

- **SEV-SNP**: AMD EPYC Milan (7003, Zen 3) or newer. **TDX**: Intel 4th Gen Xeon
  Sapphire Rapids (select SKUs) or newer.
- **Cloud**: Azure (DCasv5/ECasv5 = SEV-SNP, DCesv6/ECesv6 = TDX); GCP (N2D =
  SEV/SEV-SNP, C3 = TDX). AWS is a dead end for guest-facing SEV-SNP/TDX TEEs (its
  model is Nitro Enclaves; it exposes neither as a guest TEE).
- **Bare metal (Mitos/Hetzner)**: AMD EPYC bare metal CAN run SEV-SNP, gated on
  BIOS toggles plus a recent kernel and firmware. Hetzner SEV-SNP exposure is
  UNVERIFIED (no Hetzner doc confirms it; treat as not-available pending a hardware
  test, consistent with the #40 AMD restore failure on the Hetzner box).
- **VMM support**: QEMU/KVM (SEV-SNP yes, TDX yes) and cloud-hypervisor (SNP on
  MSHV today, KVM in progress; TDX on the KVM TDX tree) both support confidential
  guests. Firecracker supports neither. Caveat: even QEMU/cloud-hypervisor only
  enable fresh attested boots and firmware-mediated migration, NOT host-side CoW
  snapshot-fork. Swapping the VMM is necessary but NOT sufficient for warm fork;
  warm fork remains impossible regardless of VMM, because the hardware forbids it.

Sources: AMDESE/AMDSEV; canonical/tdx; Azure/GCP/AWS confidential-computing docs;
QEMU and cloud-hypervisor SEV-SNP/TDX docs.

## Recommendation

If Mitos ever pursues confidential sandboxes, it must be as a distinct tier, not a
modification of the fork hot path:

> **Tier C, "Confidential cold-boot":** a separate isolation tier. Each
> confidential sandbox is a fresh, per-VM, hardware-attested launch of an SEV-SNP
> or TDX guest. NO warm fork, NO snapshot CoW sharing, NO save/restore. Runs on
> QEMU or cloud-hypervisor on KVM, not Firecracker, with an attestation/secret-
> release flow modeled on Kata CoCo's Trustee (KBS + Attestation Service + RVPS).
> Target SEV-SNP EPYC Milan+ or TDX Sapphire Rapids+ hosts; in cloud, Azure
> DCasv5/DCesv6 or GCP N2D/C3; for bare metal, validate SEV-SNP enablement on the
> actual host first (Hetzner unverified).

This would sit alongside the fork-native default as a distinct, lower-throughput
assurance tier, surfaced through the same `mitos.run/isolation-tier` node label
and template floor the PVM evaluation (#40) introduced. It is a large, separate
program (different VMM, attestation service, key broker), not a feature of the
existing engine, and is out of scope until there is regulated demand to justify
it. Per the no-unverified-claims rule, Mitos must not market "confidential
microVMs" until such a tier is built and attested end to end.

### Follow-ups

- Re-file the #40 `0xc0010007` failure as a PMU/CPUID snapshot-replay bug,
  decoupled from this SEV analysis (section 4).
- A confidential tier inherits the active CVM-migration attack-research surface
  (for example SEV CEK-extraction and TDX live-migration findings); it must be
  threat-modeled per the security-findings-block-features rule before any adoption.
