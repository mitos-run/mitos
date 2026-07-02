package firecracker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The jailed build VM hardlinks the canonical template rootfs into its chroot
// and chownIntoJail flips the SHARED inode to the per-VM jailed uid (it must:
// the deprivileged build VM writes the rootfs during the build). Left that
// way, the husk VMM (uid 0 with ALL capabilities dropped, so no
// CAP_DAC_OVERRIDE) fails EACCES opening the rootfs O_RDWR at /snapshot/load
// (#583). normalizeTemplateArtifacts is the correct-by-construction repair at
// the end of CreateTemplate: every canonical template artifact is handed back
// to the daemon's own uid with world-readable modes before the template can
// be registered or digest-recorded.

func TestNormalizeTemplateArtifactsResetsModes(t *testing.T) {
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
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("%s mode = %o, want 0644", f, got)
		}
		st := info.Sys().(*syscall.Stat_t)
		if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
			t.Errorf("%s owned %d:%d, want %d:%d", f, st.Uid, st.Gid, os.Geteuid(), os.Getegid())
		}
	}
	for _, d := range []string{root, snapDir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("%s mode = %o, want 0755", d, got)
		}
	}
}

// Root-gated regression for the exact production failure: a hardlink of the
// canonical rootfs chowned to a jailed uid flips the shared inode, and
// normalization restores it. Requires CAP_CHOWN, so it runs only as root
// (the KVM e2e runner); everywhere else it skips.
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
	if err := os.Chown(link, 64000, 64000); err != nil {
		t.Fatal(err)
	}
	st := statT(t, rootfs)
	if st.Uid != 64000 {
		t.Fatalf("precondition: shared inode not flipped (uid=%d)", st.Uid)
	}

	if err := normalizeTemplateArtifacts(root); err != nil {
		t.Fatalf("normalizeTemplateArtifacts: %v", err)
	}
	st = statT(t, rootfs)
	if int(st.Uid) != 0 || int(st.Gid) != 0 {
		t.Errorf("rootfs owned %d:%d after normalize, want 0:0", st.Uid, st.Gid)
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
