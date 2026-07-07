package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"mitos.run/mitos/internal/firecracker"
)

// Reuse-or-rebuild gate (issue #584): a template discovered on disk (typically
// after a forkd restart, or left behind by a prior failed rollout) is only
// ever reused after it clears two independent checks: digest verification
// (verify.go, proves the content was not tampered with) and the artifact
// ownership invariants enforced here (proves the jailed build did not leave
// the artifacts in a state the husk VMM cannot read).
//
// The production trigger for the invariant check is incident #583/#597: the
// jailed build flips template artifact ownership to the jailer's unprivileged
// build uid (firecracker.JailerBuildUID), which a husk VMM running as a
// different uid cannot open. A companion change (#587, extended by #597)
// normalizes ownership at the end of a successful build
// (normalizeTemplateArtifacts) to root:SharedKVMGID with a group-readable mode;
// this function is the READ-side gate that refuses to reuse a template that was
// never normalized, or whose ownership regressed after the fact, rather than
// trusting an on-disk template blindly.

// checkTemplateArtifactInvariants verifies that every snapshot artifact of
// template id under dataDir (rootfs.ext4 when present, snapshot/mem,
// snapshot/vmstate) matches the normalized ownership contract: owned by the
// calling process's effective uid, group-owned by the shared kvm gid
// (firecracker.SharedKVMGID), and carrying the group-readable file mode 0o640.
// This is the read side of the contract normalizeTemplateArtifacts writes at
// build time: root:SharedKVMGID, group-readable, not world-writable, so the
// current uid-0 husk reads as owner and a future non-root husk (issue #585) in
// the SharedKVMGID group reads through the group class. It returns a
// descriptive error naming the offending file on the first invariant that
// fails; artifacts are checked in a stable (sorted) order so the error is
// deterministic across runs.
func checkTemplateArtifactInvariants(dataDir, id string) error {
	wantUID := os.Geteuid()

	// checkOwnership enforces the shared uid/gid contract on one entry. kind is
	// "artifact" or "dir" for the message; wantMode is 0o640 for files and
	// 0o750 for directories.
	checkOwnership := func(kind, path string, wantMode os.FileMode) error {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat template %s %s %s: %w", id, kind, path, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("template %s %s %s: cannot determine owner on this platform", id, kind, path)
		}
		if gotUID := int(st.Uid); gotUID != wantUID {
			return fmt.Errorf("template %s %s %s is owned by uid %d, expected uid %d (this process's euid); the jailed build likely flipped ownership (issues #583, #597), so the template is unusable until rebuilt or its ownership is fixed", id, kind, path, gotUID, wantUID)
		}
		// A root builder tags artifacts with the shared kvm gid so a separate
		// husk uid reads them through the group class. A non-root builder
		// (standalone sandbox-server, self-host) cannot chgrp to that group and
		// does not need to: it restores as the same uid that built, so its own
		// gid is correct. Accept either, mirroring normalizeTemplateArtifacts'
		// fallback, so a non-root deployment reuses its templates instead of
		// rebuilding every one.
		if gotGID := int(st.Gid); gotGID != firecracker.SharedKVMGID && gotGID != os.Getegid() {
			return fmt.Errorf("template %s %s %s is group-owned by gid %d, expected gid %d (the shared kvm group a husk reads the template through, issues #585, #597) or this process's gid %d; the template is unusable until rebuilt or its group is fixed", id, kind, path, gotGID, firecracker.SharedKVMGID, os.Getegid())
		}
		if mode := info.Mode().Perm(); mode != wantMode {
			return fmt.Errorf("template %s %s %s has mode %#o, expected mode %#o; the template is unusable until rebuilt or its mode is fixed", id, kind, path, mode, wantMode)
		}
		return nil
	}

	// Check the containing directories first: normalizeTemplateArtifacts sets
	// them to 0o750, and a husk cannot traverse to the files (EACCES) if the
	// template or snapshot dir has lost group execute, even when the files
	// themselves are compliant.
	dir := templateDir(dataDir, id)
	for _, d := range []string{dir, filepath.Join(dir, "snapshot")} {
		if err := checkOwnership("dir", d, 0o750); err != nil {
			return err
		}
	}

	files := templateSnapshotFiles(dataDir, id)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := checkOwnership("artifact", files[name], 0o640); err != nil {
			return err
		}
	}
	return nil
}

// shouldReuseTemplate is the decision seam for the CreateTemplate
// reuse-or-rebuild gate (#584). It reports whether the on-disk template id
// under dataDir can be reused as-is:
//
//   - No rootfs.ext4 on disk: nothing to reuse (a fresh build), reuse=false,
//     err=nil. verify is not called.
//   - rootfs.ext4 present and verify(id) succeeds: reuse=true, err=nil.
//   - rootfs.ext4 present and verify(id) fails: reuse=false, err is verify's
//     error, so the caller can log why the template was rejected before
//     deleting and rebuilding it.
//
// verify is injected so callers can combine digest verification
// (Engine.VerifyTemplate) with the artifact ownership invariants
// (checkTemplateArtifactInvariants) without this function depending on the
// Engine type, keeping it unit-testable without a real build.
func shouldReuseTemplate(dataDir, id string, verify func(string) error) (bool, error) {
	if _, err := os.Stat(filepath.Join(dataDir, "templates", id, "rootfs.ext4")); err != nil {
		return false, nil
	}
	if err := verify(id); err != nil {
		return false, err
	}
	return true, nil
}
