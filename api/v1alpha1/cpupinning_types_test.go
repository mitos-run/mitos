package v1alpha1

import "testing"

// TestCPUPinningSpecDeepCopy proves DeepCopy allocates a new pointer and
// isolates the copy from the original, including the *bool sub-fields.
func TestCPUPinningSpecDeepCopy(t *testing.T) {
	sib := false
	rt := false
	in := &SandboxPool{
		Spec: SandboxPoolSpec{
			Replicas: 1,
			CPUPinning: &CPUPinningSpec{
				Enabled:          true,
				Policy:           CPUPinningSpread,
				SiblingPairing:   &sib,
				LaunchRtPriority: &rt,
			},
		},
	}
	out := in.DeepCopy()
	if out.Spec.CPUPinning == in.Spec.CPUPinning {
		t.Fatal("DeepCopy must allocate a new CPUPinning pointer, got the same pointer")
	}
	if out.Spec.CPUPinning.SiblingPairing == in.Spec.CPUPinning.SiblingPairing {
		t.Fatal("DeepCopy must allocate a new SiblingPairing pointer")
	}
	if out.Spec.CPUPinning.Policy != CPUPinningSpread || out.Spec.CPUPinning.Enabled != true {
		t.Fatalf("DeepCopy lost field values: %+v", out.Spec.CPUPinning)
	}
	// Mutating the copy must not affect the original.
	*out.Spec.CPUPinning.SiblingPairing = true
	out.Spec.CPUPinning.Policy = CPUPinningPack
	if *in.Spec.CPUPinning.SiblingPairing != false || in.Spec.CPUPinning.Policy != CPUPinningSpread {
		t.Fatal("DeepCopy did not isolate CPUPinning from the original")
	}
}

// TestCPUPinningDefaultsNil proves a nil spec normalizes to the documented
// safe default: pinning disabled, so existing pools keep the legacy unpinned
// behavior with no opt-in.
func TestCPUPinningDefaultsNil(t *testing.T) {
	got := (*CPUPinningSpec)(nil).Normalized()
	if got.Enabled {
		t.Fatal("nil CPUPinning must normalize to disabled")
	}
	if got.Policy != CPUPinningPack {
		t.Fatalf("default policy = %q, want pack", got.Policy)
	}
	if !got.SiblingPairingEnabled() {
		t.Fatal("default sibling pairing must be true")
	}
	if !got.LaunchRtPriorityEnabled() {
		t.Fatal("default launch RT priority must be true")
	}
}

// TestCPUPinningDefaultsPartial proves an Enabled spec with the optional fields
// left unset fills in the documented defaults (pack, sibling pairing on, RT on).
func TestCPUPinningDefaultsPartial(t *testing.T) {
	got := (&CPUPinningSpec{Enabled: true}).Normalized()
	if !got.Enabled {
		t.Fatal("Enabled must be preserved")
	}
	if got.Policy != CPUPinningPack {
		t.Fatalf("default policy = %q, want pack", got.Policy)
	}
	if !got.SiblingPairingEnabled() {
		t.Fatal("default sibling pairing must be true")
	}
	if !got.LaunchRtPriorityEnabled() {
		t.Fatal("default launch RT priority must be true")
	}
}

// TestCPUPinningExplicitOverrides proves explicit values survive normalization.
func TestCPUPinningExplicitOverrides(t *testing.T) {
	sib := false
	rt := false
	got := (&CPUPinningSpec{
		Enabled:          true,
		Policy:           CPUPinningSpread,
		SiblingPairing:   &sib,
		LaunchRtPriority: &rt,
	}).Normalized()
	if got.Policy != CPUPinningSpread {
		t.Fatalf("policy = %q, want spread", got.Policy)
	}
	if got.SiblingPairingEnabled() {
		t.Fatal("explicit siblingPairing=false must survive")
	}
	if got.LaunchRtPriorityEnabled() {
		t.Fatal("explicit launchRtPriority=false must survive")
	}
}
