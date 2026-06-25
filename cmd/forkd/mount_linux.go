//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// prepareChrootMount makes chrootBase usable as the jailer's pivot_root target
// INSIDE the forkd pod. The Firecracker jailer pivot_roots into
// <chroot-base>/firecracker/<vm-id>/root, and pivot_root(2) requires (a) the new
// root to be a mount point and (b) the new root's parent mount to NOT have
// shared propagation. A pod's container rootfs is commonly mounted with shared
// or otherwise-propagating flags, so a plain directory under it fails pivot_root
// with EINVAL/EBUSY. This is the same pivot-in-pod problem the husk jailer-in-pod
// design identified; here it is the precondition for forkd dropping
// privileged: true in favor of the explicit jailer capability set. We fix both
// preconditions once, in the pod's own mount namespace, before the engine
// launches any jailed VM:
//
//  1. bind-mount chrootBase onto itself, so it BECOMES a mount point (the parent
//     of every per-VM jail dir is now a mount the jailer can pivot under); and
//  2. recursively mark it MS_PRIVATE, so its (and its children's) propagation
//     does not defeat pivot_root.
//
// It needs CAP_SYS_ADMIN (mount(2)), which the forkd DaemonSet grants as one of
// the explicit jailer capabilities (jailerRequiredCapabilities). It is
// idempotent: a re-bind of an already-bound private base is harmless and
// re-marking private is a no-op, so forkd may call it again after a restart.
// chrootBase carries no secrets; errors name the path only.
func prepareChrootMount(chrootBase string) error {
	// Bind chrootBase onto itself so it is a mount point.
	if err := unix.Mount(chrootBase, chrootBase, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount jailer chroot base %s onto itself (needed so the jailer can pivot_root inside the pod): %w", chrootBase, err)
	}
	// Mark it private (recursively) so pivot_root is not refused by shared
	// propagation on the parent mount.
	if err := unix.Mount("", chrootBase, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make jailer chroot base %s a private mount (needed so the jailer can pivot_root inside the pod): %w", chrootBase, err)
	}
	return nil
}
