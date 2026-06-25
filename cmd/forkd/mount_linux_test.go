//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPrepareChrootMountMakesPrivateMountPoint proves the jailer chroot base
// becomes a private MOUNT POINT, which is exactly what the jailer's pivot_root
// requires inside a pod. A pod's container rootfs is commonly mounted shared, so
// a plain directory under it fails pivot_root with EINVAL/EBUSY; bind-mounting
// the base onto itself and marking it private fixes both preconditions. It needs
// CAP_SYS_ADMIN to call mount(2); on a host without it the test SKIPS (so the
// darwin/unprivileged unit suite stays green) and the real assertion runs in the
// KVM-CI / bare-metal forkd-jailer phase.
func TestPrepareChrootMountMakesPrivateMountPoint(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("prepareChrootMount needs CAP_SYS_ADMIN; verified in the KVM-CI forkd-jailer phase")
	}
	base := filepath.Join(t.TempDir(), "jail")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := prepareChrootMount(base); err != nil {
		t.Fatalf("prepareChrootMount: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(base, unix.MNT_DETACH) })

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !mountinfoHasPrivateMount(string(data), base) {
		t.Fatalf("chroot base %q is not a private mount point after prepareChrootMount:\n%s", base, data)
	}

	// Idempotent: a second call (forkd may re-run setup on restart) must not error.
	if err := prepareChrootMount(base); err != nil {
		t.Fatalf("prepareChrootMount second call: %v", err)
	}
}

// mountinfoHasPrivateMount reports whether mountinfo lists target as a mount
// point whose optional propagation fields do NOT include a "shared:" tag (i.e.
// it is private). pivot_root refuses a new root whose parent mount is shared, so
// the forkd chroot base must be private. The mountinfo line format is:
// id parent major:minor root mountpoint options - optional... where the optional
// fields between the 7th field and the standalone "-" carry the propagation tags.
func mountinfoHasPrivateMount(mountinfo, target string) bool {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		if fields[4] != target {
			continue
		}
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				return true // reached the separator with no shared: tag
			}
			if strings.HasPrefix(fields[i], "shared:") {
				return false
			}
		}
		return true
	}
	return false
}
