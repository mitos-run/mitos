package firecracker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The jailed build VM hardlinks the canonical template rootfs into its chroot
// and chownIntoJail flips the SHARED inode to the per-VM jailed uid (it must:
// the deprivileged build VM writes the rootfs during the build). Left that way,
// a husk VMM that is neither the jailer uid nor privileged fails EACCES opening
// the artifacts (#583, #597). normalizeTemplateArtifacts is the
// correct-by-construction repair at the end of CreateTemplate: every canonical
// template artifact is handed back to the daemon's own uid and the shared kvm
// group with group-readable modes before the template can be registered or
// digest-recorded, so the current uid-0 husk reads them as owner and a future
// non-root husk (issue #585) in the shared kvm group reads them through the
// group class.
//
// Setting the group to SharedKVMGID requires the privilege to chgrp to a
// foreign gid, so the normalize tests run only as root (the KVM e2e runner);
// everywhere else they skip. In production normalize runs on a root forkd.

func TestNormalizeTemplateArtifactsSetsGroupReadableContract(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chgrp template artifacts to the shared kvm gid")
	}
	root := t.TempDir()
	snapDir := filepath.Join(root, "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(root, "rootfs.ext4"),
		filepath.Join(snapDir, "mem"),
		filepath.Join(snapDir, "vmstate"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := normalizeTemplateArtifacts(root); err != nil {
		t.Fatalf("normalizeTemplateArtifacts: %v", err)
	}

	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Errorf("%s mode = %o, want 0640 (group-readable, not world-writable)", f, got)
		}
		st := info.Sys().(*syscall.Stat_t)
		if int(st.Uid) != os.Geteuid() || int(st.Gid) != SharedKVMGID {
			t.Errorf("%s owned %d:%d, want %d:%d (root:SharedKVMGID)", f, st.Uid, st.Gid, os.Geteuid(), SharedKVMGID)
		}
	}
	for _, d := range []string{root, snapDir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o750 {
			t.Errorf("%s mode = %o, want 0750", d, got)
		}
		st := info.Sys().(*syscall.Stat_t)
		if int(st.Gid) != SharedKVMGID {
			t.Errorf("%s gid = %d, want %d (SharedKVMGID)", d, st.Gid, SharedKVMGID)
		}
	}
}

// TestNormalizeTemplateArtifactsRestoresJailFlippedOwner is the root-gated
// regression for the exact production failure (#597): a hardlink of the
// canonical rootfs chowned to the jailer build uid flips the shared inode, and
// normalization restores it to the root:SharedKVMGID group-readable contract.
func TestNormalizeTemplateArtifactsRestoresJailFlippedOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown to a foreign uid")
	}
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	jail := t.TempDir()
	link := filepath.Join(jail, "rootfs.ext4")
	if err := os.Link(rootfs, link); err != nil {
		t.Fatal(err)
	}
	// What chownIntoJail does to the build VM's chroot hardlink.
	if err := os.Chown(link, JailerBuildUID, JailerBuildUID); err != nil {
		t.Fatal(err)
	}
	st := statT(t, rootfs)
	if int(st.Uid) != JailerBuildUID {
		t.Fatalf("precondition: shared inode not flipped (uid=%d)", st.Uid)
	}

	if err := normalizeTemplateArtifacts(root); err != nil {
		t.Fatalf("normalizeTemplateArtifacts: %v", err)
	}
	st = statT(t, rootfs)
	if int(st.Uid) != 0 || int(st.Gid) != SharedKVMGID {
		t.Errorf("rootfs owned %d:%d after normalize, want 0:%d (root:SharedKVMGID)", st.Uid, st.Gid, SharedKVMGID)
	}
	if got := os.FileMode(st.Mode).Perm(); got != 0o640 {
		t.Errorf("rootfs mode = %o after normalize, want 0640", got)
	}
}

// TestNormalizeTemplateArtifactsRestorableByNonRootGroupMember is the key
// proof: after normalize, a NON-ROOT process that is in the shared kvm group
// can open (read) the template artifacts, while a process outside the group
// cannot. This is the read leg every husk restore depends on (issue #597, #585:
// the per-activation rootfs reflink clone reads rootfs.ext4; the snapshot load
// reads mem and vmstate). Dropping privileges in-process is unreliable on Go's
// multi-threaded runtime, so the read is exercised by a child process with
// dropped credentials (uid nobody, primary gid + supplemental group set to the
// shared kvm gid), which is exactly how the husk pod carries the group.
func TestNormalizeTemplateArtifactsRestorableByNonRootGroupMember(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to normalize to the shared kvm gid and drop privileges")
	}
	catBin, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat not found, cannot exercise a dropped-privilege read: %v", err)
	}

	root := t.TempDir()
	snapDir := filepath.Join(root, "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(root, "rootfs.ext4"),
		filepath.Join(snapDir, "mem"),
		filepath.Join(snapDir, "vmstate"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := normalizeTemplateArtifacts(root); err != nil {
		t.Fatalf("normalizeTemplateArtifacts: %v", err)
	}
	// t.TempDir() and its parents must be traversable by the group member, or
	// the group read is refused before it reaches the artifact. Widen the search
	// path to o+x (no read), which does not affect the artifacts' own 0o640.
	for d := root; d != "/" && d != "."; d = filepath.Dir(d) {
		info, statErr := os.Stat(d)
		if statErr != nil {
			break
		}
		if err := os.Chmod(d, info.Mode().Perm()|0o001); err != nil {
			t.Fatalf("widen traverse on %s: %v", d, err)
		}
	}

	const nobodyUID = 65534

	// readAs opens each artifact via cat under the given dropped credentials and
	// returns the first error (nil means every read succeeded).
	readAs := func(uid, gid uint32, groups []uint32) error {
		for _, f := range files {
			cmd := exec.Command(catBin, f)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups},
			}
			if out, runErr := cmd.CombinedOutput(); runErr != nil {
				return errors.New(f + ": " + string(out) + runErr.Error())
			}
		}
		return nil
	}

	// In the shared kvm group: every artifact reads without EACCES.
	if err := readAs(nobodyUID, SharedKVMGID, []uint32{SharedKVMGID}); err != nil {
		t.Fatalf("a non-root member of the shared kvm group must read the normalized template artifacts, got: %v", err)
	}

	// Not in the group (a different gid, no matching supplemental group): the
	// "other" class has no read bit on a 0o640 file, so the read is refused.
	// This proves the group is load-bearing, not that the files are world-read.
	const otherGID = 65533
	if err := readAs(nobodyUID, otherGID, []uint32{otherGID}); err == nil {
		t.Fatal("a non-root process outside the shared kvm group must NOT be able to read the 0o640 template artifacts")
	}
}

func statT(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t)
}
