package fork

import (
	"fmt"
	"os"
)

// ensureGuestKernelStaged checks that the guest kernel the template build boots
// from is present, returning an actionable, LLM-legible error if not (issue #174
// box 5, extending the issue #28 error rule to the deploy layer). The kernel is
// staged on each KVM node by the kernel-provisioner DaemonSet, which may still be
// running when forkd starts; rather than letting CreateTemplate surface an opaque
// Firecracker boot failure, fail fast and name the path and the likely cause so
// an operator (or an agent reading the log) knows exactly what to check.
func ensureGuestKernelStaged(kernelPath string) error {
	info, err := os.Stat(kernelPath)
	if err != nil {
		return fmt.Errorf("guest kernel missing at %s: %w; the kernel-provisioner DaemonSet stages it on each KVM node, is it healthy? (kubectl -n <install-ns> get pods -l app=kernel-provisioner; run `mitos doctor` for a full preflight)", kernelPath, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("guest kernel at %s is not a usable image (empty or not a regular file); the kernel-provisioner stages it, is it healthy? run `mitos doctor` for a full preflight", kernelPath)
	}
	return nil
}
