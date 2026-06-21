package fork

import (
	"testing"

	"mitos.run/mitos/internal/cpupin"
)

// TestReadyEventGating proves the fork-path pin hook is skipped (ok=false) when
// pinning config is absent or disabled, and built (ok=true) when enabled. This
// is the gate that keeps the activate/fork path a no-op on darwin and for pools
// that never opt in. A nil *firecracker.Client is fine here: the disabled paths
// return before touching it, so the gating is testable without a live VM.
func TestReadyEventGating(t *testing.T) {
	if _, ok := readyEvent(nil, ForkOpts{}); ok {
		t.Fatal("nil CPUPinning must skip the pin hook")
	}
	if _, ok := readyEvent(nil, ForkOpts{CPUPinning: &cpupin.Config{Enabled: false}}); ok {
		t.Fatal("disabled CPUPinning must skip the pin hook")
	}
}

// TestReadyEventEnabledDefaultsVCPUs proves an enabled hook with an unset vCPU
// count defaults to 1, so the pin plan is always sizable, and that the config is
// carried through. A nil client leaves the thread set empty (Linux-only
// enumeration), which is fine: the assertion is on the defaulting, not the tids.
func TestReadyEventEnabledDefaultsVCPUs(t *testing.T) {
	ev, ok := readyEvent(nil, ForkOpts{CPUPinning: &cpupin.Config{Enabled: true}})
	if !ok {
		t.Fatal("enabled CPUPinning must build the pin hook event")
	}
	if ev.VCPUs != 1 {
		t.Fatalf("VCPUs default = %d, want 1", ev.VCPUs)
	}
	if !ev.Config.Enabled {
		t.Fatal("Config.Enabled must be carried through")
	}
}
