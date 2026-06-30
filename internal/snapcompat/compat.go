// Package snapcompat defines the snapshot compatibility contract: whether a
// snapshot captured in one environment can be safely restored in another.
//
// A Firecracker snapshot is not portable across arbitrary hosts. The producing
// Firecracker version, the host CPU model, and the snapshot format are all part
// of whether a restore is safe. snapcompat records the producing environment in
// the manifest (see internal/cas.Manifest) and refuses an unsafe restore with an
// actionable error rather than crashing a guest or silently corrupting it.
package snapcompat

import (
	"errors"
	"fmt"

	"mitos.run/mitos/internal/cas"
)

// ErrIncompatible is wrapped by every compatibility refusal so callers can
// distinguish a compatibility refusal (errors.Is) from other errors.
var ErrIncompatible = errors.New("snapshot incompatible")

// Environment describes the host this build can restore snapshots on. It is
// detected once at engine start and held for the process lifetime.
type Environment struct {
	// FormatVersions is the set of snapshot format versions this build can
	// restore (currently a single element, cas.CurrentSnapshotFormatVersion).
	FormatVersions []int
	VMMVersion     string
	CPUModel       string
	KernelVersion  string
	// GuestProtocolVersions is the set of guest-agent vsock protocol versions
	// this build can drive (currently a single element,
	// cas.CurrentGuestProtocolVersion). An empty set means the caller did not
	// detect the environment (e.g. the development --allow-unverified path), so
	// the guest-protocol check is skipped rather than refusing every snapshot.
	GuestProtocolVersions []int
}

// Check reports whether a snapshot described by m can be safely restored in env.
// It returns nil when compatible, or an error wrapping ErrIncompatible with an
// actionable, remediation-bearing message for the first mismatch found.
//
// Check order is format, then VMM, then CPU, then guest-agent protocol; the
// first mismatch is returned. VMM and CPU are the more fundamental restore
// hazards, so they surface before the guest-protocol skew.
//
// Kernel-version decision (v1): a recorded kernel mismatch is treated as
// INFORMATIONAL, not fatal, when the format, VMM, and CPU all match. The guest
// kernel is baked into the snapshot image itself, so a differing recorded kernel
// usually just means a different snapshot was produced, not that this snapshot is
// unsafe to restore here. Failing on it alone would block legitimate restores
// without improving safety, so v1 does not gate on it.
//
// Guest-agent protocol (issue #459): a pure runtime upgrade can leave the
// format, VMM, and CPU all matching while the guest agent baked into the
// snapshot speaks an older vsock protocol than this build. The activate then
// proceeds through snapshot/load and Resume and only breaks at the
// fork-correctness handshake with an opaque vsock BrokenPipe. Gating on the
// recorded guest-protocol version turns that into an actionable rebuild refusal
// here, before any Firecracker launch.
func Check(m cas.Manifest, env Environment) error {
	if !containsInt(env.FormatVersions, m.SnapshotFormatVersion) {
		if m.SnapshotFormatVersion == 0 {
			return fmt.Errorf("snapshot format version 0 predates the snapshot compatibility contract; rebuild the template (this build supports %v): %w", env.FormatVersions, ErrIncompatible)
		}
		return fmt.Errorf("snapshot format version %d is not supported by this build (supports %v); rebuild the template: %w", m.SnapshotFormatVersion, env.FormatVersions, ErrIncompatible)
	}

	if m.VMMVersion != env.VMMVersion {
		return fmt.Errorf("snapshot was produced by Firecracker %q but this node runs %q; Firecracker does not guarantee cross-version snapshot restore, so rebuild the template on this node or pin the node to the producing version: %w", m.VMMVersion, env.VMMVersion, ErrIncompatible)
	}

	if m.CPUModel != env.CPUModel {
		return fmt.Errorf("snapshot was captured on CPU %q but this node is %q; cross-CPU-model restore is unsafe without a CPU template, so schedule the fork on a matching CPU or rebuild the template here: %w", m.CPUModel, env.CPUModel, ErrIncompatible)
	}

	// Guest-agent protocol skew (issue #459). Skipped when this build declares no
	// supported set (an undetected environment), so a caller that has not detected
	// the host does not refuse every snapshot.
	if len(env.GuestProtocolVersions) > 0 && !containsInt(env.GuestProtocolVersions, m.GuestProtocolVersion) {
		if m.GuestProtocolVersion == 0 {
			return fmt.Errorf("snapshot predates guest-agent protocol tracking (issue #459); its baked guest agent may not speak this build's vsock handshake, so rebuild the template (this build speaks guest-agent protocol %v): %w", env.GuestProtocolVersions, ErrIncompatible)
		}
		return fmt.Errorf("snapshot was built with guest-agent protocol version %d but this build speaks %v; the baked guest agent cannot complete this build's fork-correctness handshake, so rebuild the template: %w", m.GuestProtocolVersion, env.GuestProtocolVersions, ErrIncompatible)
	}

	return nil
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
