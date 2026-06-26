package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"mitos.run/mitos/internal/firecracker"
)

// jailerRequiredCapabilities is the EXACT, minimal capability set forkd must
// retain to build each VM's jail through the Firecracker jailer. It is the
// single source of truth the DaemonSet securityContext.capabilities.add and the
// buildJailerConfig nonroot error message both derive from, so the intended
// dropped-everything-else set is computed in one tested place:
//
//   - CAP_SYS_ADMIN: cgroup and namespace setup the jailer performs;
//   - CAP_CHOWN:     hand the per-VM chroot files to the dedicated uid/gid;
//   - CAP_SETUID:    drop the jailed Firecracker to the per-VM uid;
//   - CAP_SETGID:    drop the jailed Firecracker to the per-VM gid;
//   - CAP_MKNOD:     create the /dev/kvm and /dev/net/tun device nodes inside
//     each chroot.
//
// The kernel actually ENFORCING the drop of every other capability is proven on
// a non-root KVM runner, not here (the CI runner is root, so it cannot observe
// the bounding set shrink); this function and its tests assert only that the
// intended LIST is correct and stable. Order is part of the contract so the
// list diffs cleanly. The set is intentionally narrow: widening it is a
// threat-model change (docs/threat-model.md) and a reviewed diff.
func jailerRequiredCapabilities() []string {
	return []string{
		"CAP_SYS_ADMIN",
		"CAP_CHOWN",
		"CAP_SETUID",
		"CAP_SETGID",
		"CAP_MKNOD",
	}
}

// forkdRequiredCapabilities is the EXACT capability set the forkd CONTAINER runs
// with after dropping privileged: true. It is the jailer set
// (jailerRequiredCapabilities) plus CAP_NET_ADMIN: the privileged BUILDER also
// creates a per-template placeholder tap host-side with `ip tuntap add`
// (internal/fork/engine.go, when --enable-networking is set, as the shipped
// DaemonSet does), which needs CAP_NET_ADMIN. The jailer itself does NOT need
// NET_ADMIN, so the jailer list stays the minimal authority for the jail and
// NET_ADMIN is the single, documented builder extra. NET_ADMIN is scoped to
// forkd's own pod netns (forkd is not hostNetwork), exactly like the husk pod's
// NET_ADMIN; it cannot reach the host netns.
//
// It also includes CAP_DAC_OVERRIDE: the builder copies and finalizes the
// template rootfs.ext4 under <dataDir>/templates, and that file becomes owned by
// a per-VM jailed uid (the jailer hard-links the rootfs into the chroot and
// chowns the shared inode to the per-VM uid). On a subsequent build forkd (root,
// but subject to normal DAC checks without this cap) otherwise cannot reopen its
// own template rootfs for write and the snapshot build fails with EACCES (#426).
// DAC_OVERRIDE is negligible marginal authority next to the CAP_SYS_ADMIN the
// builder already holds, and like the rest of the set it is scoped to forkd's
// own pod, not the host.
//
// This is the single source of truth the DaemonSet
// securityContext.capabilities.add must match
// (cmd/forkd/manifest_conformance_test.go). Widening it is a threat-model change.
func forkdRequiredCapabilities() []string {
	return append(jailerRequiredCapabilities(), "CAP_NET_ADMIN", "CAP_DAC_OVERRIDE")
}

// parseUIDRange parses the --uid-range flag, "low-high" inclusive.
// uid 0 is refused: jailed VMs must never run as root.
func parseUIDRange(s string) (uint32, uint32, error) {
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

// buildJailerConfig validates the jailer flags and produces the engine
// JailerConfig. It fails closed on every misconfiguration:
//
//   - a malformed or root-including --uid-range;
//   - --chroot-base on a different filesystem from --data-dir (snapshot,
//     kernel, and rootfs files are hard-linked into each chroot; across
//     filesystems every fork would degrade to a full copy);
//   - forkd not running as root. The jailer needs root to set up the
//     jail; concretely it exercises CAP_SYS_ADMIN (cgroup and namespace
//     setup), CAP_CHOWN (handing the chroot to the per-VM uid),
//     CAP_SETUID and CAP_SETGID (dropping to that uid/gid), and
//     CAP_MKNOD (/dev/kvm and /dev/net/tun nodes inside the chroot);
//     forkd additionally needs to open /dev/kvm. The DaemonSet grants
//     exactly that set (deploy/daemon/daemonset.yaml).
//
// An empty jailerBin disables the jailer (development only; the caller
// logs a loud warning and the threat model flags the residual risk).
// sameFS compares the filesystems of two paths; it is injected so the
// check is unit-testable (see sameDevice for the platform versions).
func buildJailerConfig(jailerBin, chrootBase, uidRange, dataDir string, euid int, sameFS func(a, b string) (bool, error)) (firecracker.JailerConfig, error) {
	if jailerBin == "" {
		return firecracker.JailerConfig{}, nil
	}

	low, high, err := parseUIDRange(uidRange)
	if err != nil {
		return firecracker.JailerConfig{}, err
	}

	if euid != 0 {
		return firecracker.JailerConfig{}, fmt.Errorf("--jailer requires forkd to run as root (euid 0, currently %d): the jailer needs %s to build each VM's jail; run unjailed only for development by omitting --jailer", euid, strings.Join(jailerRequiredCapabilities(), ", "))
	}

	for _, dir := range []string{chrootBase, dataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return firecracker.JailerConfig{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	same, err := sameFS(chrootBase, dataDir)
	if err != nil {
		return firecracker.JailerConfig{}, fmt.Errorf("compare filesystems of --chroot-base %s and --data-dir %s: %w", chrootBase, dataDir, err)
	}
	if !same {
		return firecracker.JailerConfig{}, fmt.Errorf("--chroot-base %s and --data-dir %s are on different filesystems; snapshot and rootfs files are hard-linked into each VM chroot, which requires one filesystem. Move --chroot-base under the data dir (for example %s/jailer)", chrootBase, dataDir, dataDir)
	}

	return firecracker.JailerConfig{
		JailerBin:     jailerBin,
		ChrootBaseDir: chrootBase,
		UIDRange:      [2]uint32{low, high},
	}, nil
}
