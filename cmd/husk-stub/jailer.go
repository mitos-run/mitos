package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/paperclipinc/mitos/internal/firecracker"
)

// parseHuskUIDRange parses "low-high" (inclusive). uid 0 is refused: jailed VMs
// must never run as root. It mirrors cmd/forkd's parseUIDRange so the two jailer
// front ends share the same fail-closed shape.
func parseHuskUIDRange(s string) (uint32, uint32, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("--uid-range %q: expected the form low-high, for example 64000-64999", s)
	}
	low, err := strconv.ParseUint(lo, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--uid-range %q: low bound: %w", s, err)
	}
	high, err := strconv.ParseUint(hi, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--uid-range %q: high bound: %w", s, err)
	}
	if low == 0 {
		return 0, 0, fmt.Errorf("--uid-range %q: uid 0 is root; jailed VMs must run as an unprivileged uid", s)
	}
	if low > high {
		return 0, 0, fmt.Errorf("--uid-range %q: low bound above high bound", s)
	}
	return uint32(low), uint32(high), nil
}

// buildHuskJailerConfig validates the husk pod's jailer flags and produces the
// firecracker.JailerConfig the stub launches each VM through. It fails closed on
// every misconfiguration (malformed/root-including uid range, non-root euid).
//
// Unlike cmd/forkd's buildJailerConfig it does NOT require the chroot base to
// share a filesystem with the data dir: in the husk pod the chroot base lives on
// a pod-writable emptyDir, while the snapshot/kernel come from a READ-ONLY node
// hostPath, so the two are intentionally on different filesystems. prepareChroot
// already handles that with its EXDEV copy fallback (it copies the ~680 MiB mem
// file into the chroot once at Activate). The same-filesystem CoW optimization
// is a forkd-builder concern, not a husk-runner one.
//
// TODO(perf, tracked follow-up): that EXDEV copy is ~one full memfile copy
// (~268 MiB+ per activation in the KVM CI) because the chroot base (pod emptyDir)
// and the snapshot (read-only node hostPath) are deliberately on DIFFERENT
// filesystems. Co-locating them is NOT a safe drop-in: the chroot base must be
// pod-writable and torn down with the pod, whereas the snapshot hostPath is
// read-only and node-shared, so making the chroot base a writable subdir of the
// snapshot hostPath would both widen the host write surface and leave per-VM jails
// outliving the pod. The real fix is the per-activation copy-on-write plan (share
// the snapshot pages CoW instead of copying), which owns this; do not redesign the
// volume layout here just to dodge the copy.
//
// An empty jailerBin disables the jailer (the development direct-exec path; the
// caller logs a loud warning and the threat model flags the residual). euid is
// the caller's effective uid (os.Geteuid()), injected so the check is testable.
func buildHuskJailerConfig(jailerBin, chrootBase, uidRange string, euid int) (firecracker.JailerConfig, error) {
	if jailerBin == "" {
		return firecracker.JailerConfig{}, nil
	}
	low, high, err := parseHuskUIDRange(uidRange)
	if err != nil {
		return firecracker.JailerConfig{}, err
	}
	if euid != 0 {
		return firecracker.JailerConfig{}, fmt.Errorf("--jailer requires the husk stub to run as root (euid 0, currently %d): the jailer needs CAP_SYS_ADMIN, CAP_CHOWN, CAP_SETUID, CAP_SETGID, and CAP_MKNOD to build each VM's jail; run unjailed only for development by omitting --jailer", euid)
	}
	return firecracker.JailerConfig{
		JailerBin:     jailerBin,
		ChrootBaseDir: chrootBase,
		UIDRange:      [2]uint32{low, high},
	}, nil
}
