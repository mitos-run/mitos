package controller

import (
	"errors"
	"testing"
)

// --- Isolation-tier model (issue #40) ---

// TestIsolationTierAssurance asserts the assurance ordering: hardware-KVM is the
// highest-assurance tier, PVM (ring-3 pagetable isolation) is weaker than
// hardware virt, and gVisor (syscall interposition) is the software tier. An
// unknown/empty tier is treated as the LOWEST assurance so a node that has not
// declared a tier never silently satisfies a hardware-KVM requirement.
func TestIsolationTierAssurance(t *testing.T) {
	if IsolationTierHardwareKVM.assurance() <= IsolationTierPVM.assurance() {
		t.Fatalf("hardware-kvm assurance %d must exceed pvm %d",
			IsolationTierHardwareKVM.assurance(), IsolationTierPVM.assurance())
	}
	if IsolationTierPVM.assurance() <= IsolationTierGVisor.assurance() {
		t.Fatalf("pvm assurance %d must exceed gvisor %d",
			IsolationTierPVM.assurance(), IsolationTierGVisor.assurance())
	}
	// An empty/unknown tier ranks below every named tier.
	if IsolationTier("").assurance() >= IsolationTierGVisor.assurance() {
		t.Fatalf("unknown tier assurance %d must be below gvisor %d",
			IsolationTier("").assurance(), IsolationTierGVisor.assurance())
	}
}

// TestIsolationTierMeets asserts a node tier MEETS a required minimum only when
// its assurance is greater than or equal to the requirement. A hardware-KVM node
// meets a PVM or gVisor floor; a PVM node does NOT meet a hardware-KVM floor; an
// unknown node tier meets only an empty (no) requirement.
func TestIsolationTierMeets(t *testing.T) {
	cases := []struct {
		node IsolationTier
		min  IsolationTier
		want bool
	}{
		{IsolationTierHardwareKVM, IsolationTierHardwareKVM, true},
		{IsolationTierHardwareKVM, IsolationTierPVM, true},
		{IsolationTierHardwareKVM, IsolationTierGVisor, true},
		{IsolationTierPVM, IsolationTierHardwareKVM, false},
		{IsolationTierPVM, IsolationTierPVM, true},
		{IsolationTierGVisor, IsolationTierPVM, false},
		{IsolationTierGVisor, IsolationTierGVisor, true},
		// No requirement: any node tier (even unknown) is acceptable.
		{IsolationTier(""), IsolationTier(""), true},
		{IsolationTierPVM, IsolationTier(""), true},
		// Unknown node tier never meets a real floor.
		{IsolationTier(""), IsolationTierGVisor, false},
	}
	for _, tc := range cases {
		if got := tc.node.meets(tc.min); got != tc.want {
			t.Errorf("node %q meets min %q = %v, want %v", tc.node, tc.min, got, tc.want)
		}
	}
}

// TestIsolationTierFromNodeLabels asserts the node-label-to-scheduler mapping: a
// node declares its isolation tier via the mitos.run/isolation-tier label. An
// absent label is the unknown/lowest tier (empty). An unrecognized value is
// treated as unknown (empty) rather than silently trusted, so a typo never
// promotes a node to a higher assurance than it has.
func TestIsolationTierFromNodeLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   IsolationTier
	}{
		{"absent", map[string]string{}, IsolationTier("")},
		{"hardware-kvm", map[string]string{"mitos.run/isolation-tier": "hardware-kvm"}, IsolationTierHardwareKVM},
		{"pvm", map[string]string{"mitos.run/isolation-tier": "pvm"}, IsolationTierPVM},
		{"gvisor", map[string]string{"mitos.run/isolation-tier": "gvisor"}, IsolationTierGVisor},
		{"unrecognized stays unknown", map[string]string{"mitos.run/isolation-tier": "magic"}, IsolationTier("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsolationTierFromNodeLabels(tc.labels); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestMinIsolationTierFromSpec asserts the spec-to-request mapping: an explicit
// minIsolationTier is carried through, and the requireHardwareKvm convenience
// flag is equivalent to minIsolationTier=hardware-kvm. When both are set the
// stronger floor wins (requireHardwareKvm never weakens an explicit floor).
func TestMinIsolationTierFromSpec(t *testing.T) {
	cases := []struct {
		name       string
		min        string
		requireKVM bool
		want       IsolationTier
	}{
		{"neither", "", false, IsolationTier("")},
		{"explicit pvm", "pvm", false, IsolationTierPVM},
		{"explicit hardware-kvm", "hardware-kvm", false, IsolationTierHardwareKVM},
		{"requireHardwareKvm only", "", true, IsolationTierHardwareKVM},
		{"requireHardwareKvm strengthens pvm", "pvm", true, IsolationTierHardwareKVM},
		{"requireHardwareKvm does not weaken", "hardware-kvm", true, IsolationTierHardwareKVM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MinIsolationTierFromSpec(tc.min, tc.requireKVM); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// --- Scheduler isolation-tier filter (issue #40) ---

// tierNode builds a healthy warm node holding "py" and declaring an isolation
// tier, so the scheduler tier filter can be exercised without any hardware.
func tierNode(name string, tier IsolationTier) *NodeInfo {
	n := warmNode(name, 64*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 1)
	n.IsolationTier = tier
	return n
}

// TestSelectNodeRequireHardwareKVMNeverPicksPVM asserts a request that requires
// the hardware-KVM tier is NEVER scheduled onto a PVM (lower-assurance) node,
// even when the PVM node is the only warm holder and has the most headroom. A
// security-sensitive tenant must not land on a weaker-isolation node.
func TestSelectNodeRequireHardwareKVMNeverPicksPVM(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(tierNode("pvm-node", IsolationTierPVM))
	r.Register(tierNode("kvm-node", IsolationTierHardwareKVM))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MinIsolationTier: IsolationTierHardwareKVM})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "kvm-node" {
		t.Fatalf("got %q want kvm-node (hardware-kvm floor must skip the pvm node)", node.Name)
	}
}

// TestSelectNodeRequireHardwareKVMNoHardwareNodeIsNoCapacity asserts a
// hardware-KVM requirement when only PVM/gVisor nodes exist is ErrNoCapacity, a
// loud failure, never a silent placement on a weaker tier.
func TestSelectNodeRequireHardwareKVMNoHardwareNodeIsNoCapacity(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(tierNode("pvm-node", IsolationTierPVM))
	r.Register(tierNode("gvisor-node", IsolationTierGVisor))

	_, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MinIsolationTier: IsolationTierHardwareKVM})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (no hardware-kvm node), got %v", err)
	}
}

// TestSelectNodeNoTierRequirementUsesAnyNode asserts a request with no isolation
// floor schedules onto any healthy node, including a PVM node, so the control is
// strictly opt-in and never narrows the default fleet.
func TestSelectNodeNoTierRequirementUsesAnyNode(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(tierNode("pvm-node", IsolationTierPVM))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py"})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "pvm-node" {
		t.Fatalf("got %q want pvm-node (no floor must use any node)", node.Name)
	}
}

// TestSelectNodeUnknownTierNodeFailsHardwareFloor asserts a node that has NOT
// declared a tier (empty) does not satisfy a hardware-KVM floor: tiering is
// fail-closed, an undeclared node is treated as the lowest assurance.
func TestSelectNodeUnknownTierNodeFailsHardwareFloor(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(tierNode("unknown-node", IsolationTier("")))

	_, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MinIsolationTier: IsolationTierHardwareKVM})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (undeclared node cannot meet hardware-kvm floor), got %v", err)
	}
}

// TestSelectNodePVMFloorAcceptsHardwareKVMNode asserts a PVM floor is satisfied
// by a HIGHER-assurance hardware-KVM node (a floor is a minimum, not an exact
// match), so requiring "at least pvm" can still pack onto stronger nodes.
func TestSelectNodePVMFloorAcceptsHardwareKVMNode(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(tierNode("kvm-node", IsolationTierHardwareKVM))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MinIsolationTier: IsolationTierPVM})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "kvm-node" {
		t.Fatalf("got %q want kvm-node (a hardware-kvm node meets a pvm floor)", node.Name)
	}
}
