package fork

import (
	"strings"
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

// TestNewEngineRejectsBadHugePages proves the engine validates the HugePages
// option early with an actionable, LLM-legible error (issue #167, #28), BEFORE
// the KVM check, so a misconfigured node fails fast at construction instead of
// at the first template build. It runs on any host (including darwin) precisely
// because the validation precedes validateKVM: the error must name the bad value
// and the supported option, and must NOT be the KVM error.
func TestNewEngineRejectsBadHugePages(t *testing.T) {
	_, err := NewEngine(t.TempDir(), "/usr/local/bin/firecracker", "/tmp/vmlinux", firecracker.JailerConfig{}, EngineOpts{
		HugePages: "1G",
	})
	if err == nil {
		t.Fatal("expected error for unsupported HugePages, got nil")
	}
	if !strings.Contains(err.Error(), "1G") || !strings.Contains(err.Error(), "2M") {
		t.Errorf("error should name the bad value (1G) and the valid option (2M), got: %v", err)
	}
	if strings.Contains(err.Error(), "KVM") {
		t.Errorf("HugePages must be validated BEFORE the KVM check, got KVM error: %v", err)
	}
}
