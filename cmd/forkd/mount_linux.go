//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// prepareChrootMount prepares the forkd pod's mount namespace so the Firecracker
// jailer can pivot_root AND so the template rootfs/snapshot hard-link into each
// per-VM chroot stays copy-on-write. The jailer pivot_roots into
// <chroot-base>/firecracker/<vm-id>/root, and pivot_root(2) requires the new
// root's parent mount to NOT have shared propagation (a pod's container rootfs is
// commonly mounted shared, so a plain directory under it fails pivot_root with
// EINVAL/EBUSY). Separately, the jailer hard-links the template files (under the
// data dir) into each per-VM chroot (under chrootBase), and link(2) refuses to
// cross a mount boundary even on one filesystem; a chroot base that is its own
// mount, separate from the data dir, forces a full per-VM COPY that defeats fork
// CoW and can time the jailer out mid-build (issue #526).
//
// We satisfy both by turning ONE directory into a private mount, in forkd's own
// mount namespace, before the engine launches any jailed VM:
//
//   - When chrootBase is UNDER dataDir (the chart's co-located layout), bind
//     dataDir onto itself and mark it MS_PRIVATE|MS_REC. The per-VM chroots
//     inherit private propagation (pivot_root works) AND the template files share
//     the same mount, so the hard-links stay within one mount and remain CoW.
//   - Otherwise bind chrootBase itself (the prior behavior): pivot_root still
//     works, but template links cross into it and fall back to a copy. The
//     startup CoW self-check (verifyChrootCoW) warns with remediation.
//
// It needs CAP_SYS_ADMIN (mount(2)), granted to the forkd DaemonSet as one of the
// explicit jailer capabilities (jailerRequiredCapabilities). It is idempotent: a
// re-bind of an already-bound private base is harmless and re-marking private is a
// no-op, so forkd may call it again after a restart. The paths carry no secrets;
// errors name the path only.
func prepareChrootMount(dataDir, chrootBase string) error {
	target := chrootBase
	if dataDir != "" && pathUnder(chrootBase, dataDir) {
		target = dataDir
	}
	// Bind target onto itself so it is a mount point.
	if err := unix.Mount(target, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount jailer mount base %s onto itself (needed so the jailer can pivot_root inside the pod): %w", target, err)
	}
	// Mark it private (recursively) so pivot_root is not refused by shared
	// propagation on the parent mount.
	if err := unix.Mount("", target, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make jailer mount base %s a private mount (needed so the jailer can pivot_root inside the pod): %w", target, err)
	}
	return nil
}
