package firecracker

import (
	"encoding/json"
	"testing"
)

// The entropy (virtio-rng) device gives every restored VM a CONTINUOUS host
// entropy source in addition to the per-fork NotifyForked reseed. Firecracker
// bakes the device model into the snapshot, so the device must be attached at
// template-build time (before InstanceStart); each restored fork then wakes
// with a virtio-rng device already wired to the host RNG. These tests cover the
// darwin-testable config/JSON-building logic; the live in-guest behavior is
// KVM-gated.

// TestEntropyRequestJSON pins the PUT /entropy body shape Firecracker expects.
// The entropy device takes an optional rate_limiter; with no limiter the body
// is an empty object (the device defaults to no rate limiting). The empty body
// must NOT emit a null rate_limiter, which Firecracker rejects.
func TestEntropyRequestJSON(t *testing.T) {
	data, err := json.Marshal(Entropy{})
	if err != nil {
		t.Fatalf("marshal Entropy: %v", err)
	}
	if got := string(data); got != "{}" {
		t.Fatalf("Entropy{} JSON = %q, want %q (a bare rate-limiter-less entropy device)", got, "{}")
	}
}

// TestDefaultVMConfigEnablesEntropy asserts the entropy device is ON by default:
// every template the engine builds must bake a virtio-rng device so every fork
// inherits a continuous entropy source. A regression that silently dropped the
// device would leave forks with only the one-shot NotifyForked reseed.
func TestDefaultVMConfigEnablesEntropy(t *testing.T) {
	cfg := DefaultVMConfig()
	if !cfg.EntropyDevice {
		t.Fatal("DefaultVMConfig().EntropyDevice = false, want true: the virtio-rng device must be baked into every template snapshot by default")
	}
}
