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
	files := templateSnapshotFiles(dataDir, id)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := files[name]
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat template %s artifact %s: %w", id, path, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("template %s artifact %s: cannot determine owner on this platform", id, path)
		}
		if gotUID := int(st.Uid); gotUID != wantUID {
			return fmt.Errorf("template %s artifact %s is owned by uid %d, expected uid %d (this process's euid); the jailed build likely flipped ownership (issues #583, #597), so the template is unusable until rebuilt or its ownership is fixed", id, path, gotUID, wantUID)
		}
		if gotGID := int(st.Gid); gotGID != firecracker.SharedKVMGID {
			return fmt.Errorf("template %s artifact %s is group-owned by gid %d, expected gid %d (the shared kvm group a husk reads the template through, issues #585, #597); the template is unusable until rebuilt or its group is fixed", id, path, gotGID, firecracker.SharedKVMGID)
		}
		if mode := info.Mode().Perm(); mode != 0o640 {
			return fmt.Errorf("template %s artifact %s has mode %#o, expected mode 0o640 (group-readable, not world-writable); the template is unusable until rebuilt or its mode is fixed", id, path, mode)
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
