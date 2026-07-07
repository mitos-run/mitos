package husk

import (
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

// TestLiveCowForkGateDefaultOff proves the flag defaults off and the gate keeps
// the co-located fork on the disk path unless the pod opts in, so a deployment
// that leaves --live-cow-fork off is byte-for-byte the current behavior.
func TestLiveCowForkGateDefaultOff(t *testing.T) {
	off := New(firecracker.VMConfig{}, Options{MultiVM: true})
	if off.LiveCowForkEnabled() {
		t.Error("live-cow fork must default OFF")
	}
	if off.liveCowForkApplies(ActivateRequest{ForkSnapshot: true}) {
		t.Error("gate must be closed when the flag is off, even for a fork child")
	}
	if env := off.liveCowParentEnv("/run/vm"); env != nil {
		t.Errorf("flag off must emit no live-cow parent env; got %v", env)
	}
}

// TestLiveCowForkGateOn proves the gate opens ONLY for a co-located fork child
// (fork snapshot) when the flag is on, and the parent env is derived under the
// VM workdir. A fresh (non-fork) activation is never accelerated.
func TestLiveCowForkGateOn(t *testing.T) {
	on := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	if !on.LiveCowForkEnabled() {
		t.Fatal("live-cow fork must be enabled when opted in")
	}
	if !on.liveCowForkApplies(ActivateRequest{ForkSnapshot: true}) {
		t.Error("gate must be open for a co-located fork child when the flag is on")
	}
	if on.liveCowForkApplies(ActivateRequest{ForkSnapshot: false}) {
		t.Error("a fresh (non-fork) activation must never take the live-cow path")
	}

	env := on.liveCowParentEnv("/run/vm")
	want := []string{
		"FIRECRACKER_MITOS_SHARED_MEM=1",
		"FIRECRACKER_MITOS_SHARED_MEM_EXPORT=/run/vm/mitos-memfd.export",
		"FIRECRACKER_MITOS_WP_UDS=/run/vm/mitos-wp.sock",
	}
	if len(env) != len(want) {
		t.Fatalf("parent env = %v, want %d entries", env, len(want))
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
	// Empty workdir (the unit path) emits no env even with the flag on.
	if env := on.liveCowParentEnv(""); env != nil {
		t.Errorf("empty workdir must emit no env; got %v", env)
	}
}
