package fork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureGuestKernelStagedMissing proves the build path fails fast with an
// actionable, LLM-legible error (issue #174 box 5, extending #28 to the deploy
// layer) when the guest kernel is not staged, instead of surfacing an opaque
// Firecracker boot failure. The error must name the missing path and point at the
// kernel-provisioner so an operator (or an agent reading the log) knows the fix.
func TestEnsureGuestKernelStagedMissing(t *testing.T) {
	err := ensureGuestKernelStaged(filepath.Join(t.TempDir(), "vmlinux"))
	if err == nil {
		t.Fatal("expected an error for a missing guest kernel")
	}
	msg := err.Error()
	if !strings.Contains(msg, "vmlinux") {
		t.Errorf("error should name the missing kernel path, got: %v", err)
	}
	if !strings.Contains(msg, "kernel-provisioner") {
		t.Errorf("error should point at the kernel-provisioner, got: %v", err)
	}
}

// TestEnsureGuestKernelStagedPresent proves a staged kernel passes.
func TestEnsureGuestKernelStagedPresent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(p, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGuestKernelStaged(p); err != nil {
		t.Errorf("a staged kernel should pass, got: %v", err)
	}
}
