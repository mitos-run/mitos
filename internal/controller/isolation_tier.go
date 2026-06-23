package controller

// Isolation tiers (issue #40). A Mitos node isolates a sandbox with one of
// several mechanisms whose assurance levels are NOT equivalent, and the
// controller must never silently treat a weaker tier as if it were a stronger
// one. The tier is a property of the NODE (its kernel/runtime), declared via the
// mitos.run/isolation-tier node label, and a pool/template can REQUIRE a minimum
// floor so a security-sensitive tenant never lands on a lower-assurance node.
//
// The assurance ordering, strongest to weakest:
//
//   - hardware-kvm: a hardware virtualization microVM (Firecracker on /dev/kvm
//     backed by VMX/SVM). The strongest tier Mitos offers and the default
//     posture; the threat model's isolation assumptions (docs/threat-model.md)
//     are written for it.
//   - pvm: Firecracker on PVM (pagetable-based virtual machine, Ant/Alibaba), a
//     KVM vendor module that runs guests in ring 3 via pagetable switching with
//     NO VMX/SVM, so it runs on plain cloud VPS with no nested virt. Ring-3
//     pagetable isolation is WEAKER than hardware virt; PVM is evaluated, NOT
//     adopted (docs/platforms/pvm-evaluation.md), and if ever enabled it is a
//     documented lower-assurance tier, never silently equivalent to hardware-kvm.
//   - gvisor: a userspace kernel (syscall interposition) rather than a VM. The
//     software-isolation tier; relevant to the existing gVisor fallback and ADR
//     0005 (raw forkd is not multi-tenant).
//
// An UNDECLARED node tier (empty) is treated as the LOWEST assurance: tiering is
// fail-closed, so a node that has not declared its tier never silently satisfies
// a hardware-kvm floor.
type IsolationTier string

const (
	// IsolationTierHardwareKVM is hardware virtualization (Firecracker on /dev/kvm
	// with VMX/SVM). The strongest tier and the default posture.
	IsolationTierHardwareKVM IsolationTier = "hardware-kvm"
	// IsolationTierPVM is Firecracker on PVM (ring-3 pagetable isolation, no
	// nested virt). Lower assurance than hardware virt; evaluated, not adopted.
	IsolationTierPVM IsolationTier = "pvm"
	// IsolationTierGVisor is gVisor (userspace-kernel syscall interposition). The
	// software-isolation tier.
	IsolationTierGVisor IsolationTier = "gvisor"
)

// isolationTierNodeLabel is the node label a node carries to declare its
// isolation tier (issue #40). The node-side setup that earns a tier (a real
// hardware-virt host for hardware-kvm, a PVM host kernel for pvm, a gVisor
// runtime for gvisor) is operational and out of scope here; the controller only
// consumes the label a properly prepared node advertises. Mirrors the GPU node
// label seam (gpuNodeLabel).
const isolationTierNodeLabel = "mitos.run/isolation-tier"

// assurance is the numeric strength of a tier; higher is stronger. The values
// are an ORDER ONLY, not a measurement: hardware virt beats ring-3 pagetable
// isolation beats userspace syscall interposition. An unknown/empty tier ranks
// at 0 (lowest), below every named tier, so an undeclared node never satisfies a
// real floor (fail-closed).
func (t IsolationTier) assurance() int {
	switch t {
	case IsolationTierHardwareKVM:
		return 3
	case IsolationTierPVM:
		return 2
	case IsolationTierGVisor:
		return 1
	default:
		return 0
	}
}

// meets reports whether a node of THIS tier satisfies the required minimum floor
// min. An empty floor (no requirement) is met by ANY node tier, including an
// undeclared one, so the control is strictly opt-in. A non-empty floor is met
// only when this tier's assurance is at least the floor's: a hardware-kvm node
// meets a pvm or gvisor floor, but a pvm node never meets a hardware-kvm floor,
// and an undeclared node (assurance 0) never meets a real floor.
func (t IsolationTier) meets(min IsolationTier) bool {
	if min == "" {
		return true
	}
	return t.assurance() >= min.assurance()
}

// IsolationTierFromNodeLabels reads a node's isolation tier from its Kubernetes
// labels (issue #40), the bridge from a tier-declared node to the NodeInfo the
// scheduler consumes. It mirrors GPUFromNodeLabels. An absent label is the
// unknown/lowest tier (empty). An UNRECOGNIZED value is also treated as unknown
// (empty) rather than silently trusted, so a typo in the label never promotes a
// node above the assurance it actually has. The node-side setup that legitimately
// earns a tier is operational and not validated here.
func IsolationTierFromNodeLabels(labels map[string]string) IsolationTier {
	switch IsolationTier(labels[isolationTierNodeLabel]) {
	case IsolationTierHardwareKVM:
		return IsolationTierHardwareKVM
	case IsolationTierPVM:
		return IsolationTierPVM
	case IsolationTierGVisor:
		return IsolationTierGVisor
	default:
		return ""
	}
}

// MinIsolationTierFromSpec maps a pool/template spec to the required isolation
// floor (issue #40). It combines the explicit minIsolationTier field with the
// requireHardwareKvm convenience flag: requireHardwareKvm is equivalent to
// minIsolationTier=hardware-kvm, and when both are set the STRONGER floor wins so
// the convenience flag can only tighten, never weaken, an explicit floor. An
// unrecognized minIsolationTier string is treated as no floor (empty); CRD
// validation (an enum on the field) is the primary guard, this is the
// defensive fallback.
func MinIsolationTierFromSpec(minTier string, requireHardwareKVM bool) IsolationTier {
	floor := normalizeTier(minTier)
	if requireHardwareKVM && IsolationTierHardwareKVM.assurance() > floor.assurance() {
		return IsolationTierHardwareKVM
	}
	return floor
}

// normalizeTier coerces an arbitrary spec string to a known IsolationTier,
// returning empty (no floor) for anything unrecognized.
func normalizeTier(s string) IsolationTier {
	switch IsolationTier(s) {
	case IsolationTierHardwareKVM:
		return IsolationTierHardwareKVM
	case IsolationTierPVM:
		return IsolationTierPVM
	case IsolationTierGVisor:
		return IsolationTierGVisor
	default:
		return ""
	}
}
