package fork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateKernelStaged covers the deploy-layer LLM-legible error retrofit
// for the guest kernel (issue #174): a missing, empty, or directory kernel path
// returns an actionable error naming the path and the kernel-provisioner; a
// staged image passes.
func TestValidateKernelStaged(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "vmlinux")
	err := validateKernelStaged(missing)
	if err == nil {
		t.Fatal("missing kernel: want an error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q must name the path %q", err, missing)
	}
	if !strings.Contains(err.Error(), "kernel-provisioner") {
		t.Errorf("error %q must name the kernel-provisioner remediation", err)
	}
	if !strings.Contains(err.Error(), "mitos doctor") {
		t.Errorf("error %q must point at mitos doctor", err)
	}

	empty := filepath.Join(dir, "empty-vmlinux")
	if werr := os.WriteFile(empty, nil, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if err := validateKernelStaged(empty); err == nil {
		t.Error("empty kernel: want an error")
	}

	if err := validateKernelStaged(dir); err == nil {
		t.Error("directory as kernel path: want an error")
	}

	staged := filepath.Join(dir, "good-vmlinux")
	if werr := os.WriteFile(staged, []byte("ELF kernel image"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if err := validateKernelStaged(staged); err != nil {
		t.Errorf("staged kernel: unexpected error %v", err)
	}
}
