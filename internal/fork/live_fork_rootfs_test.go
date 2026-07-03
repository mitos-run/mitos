package fork

import "testing"

// TestLiveForkRootfsBaseIsSourceClone is the load-bearing correctness property
// of a live-state fork: the child must boot from the SOURCE's own writable
// rootfs clone (the on-disk file the running source has been writing to,
// captured while the source is PAUSED so it is consistent with the memory
// checkpoint), NOT from the read-only template the source was originally cloned
// from. Booting on the template silently loses every on-disk write the source
// made before the fork, which is the bug this slice fixes.
func TestLiveForkRootfsBaseIsSourceClone(t *testing.T) {
	source := &Sandbox{
		ID:          "src",
		rootfsPath:  "/data/templates/tmpl/rootfs.ext4", // read-only template CoW source
		rootfsClone: "/data/sandboxes/src/rootfs.ext4",  // the source's live writable clone
	}
	base := liveForkRootfsBase(source)
	if base != source.rootfsClone {
		t.Fatalf("live fork base = %q, want the source's writable clone %q", base, source.rootfsClone)
	}
	if base == source.rootfsPath {
		t.Fatalf("live fork base is the read-only template %q: the child would lose all of the source's on-disk writes", source.rootfsPath)
	}
}

// TestLiveForkRootfsBaseFallsBackToRootfsPath keeps the no-on-disk-rootfs paths
// (mock engine, a snapshot with no rootfs) unchanged: when the source carries no
// writable clone, the base falls back to rootfsPath (which is itself empty for
// mock paths), exactly as before this change.
func TestLiveForkRootfsBaseFallsBackToRootfsPath(t *testing.T) {
	withPath := &Sandbox{ID: "src", rootfsPath: "/data/templates/tmpl/rootfs.ext4"}
	if got := liveForkRootfsBase(withPath); got != withPath.rootfsPath {
		t.Fatalf("fallback base = %q, want %q", got, withPath.rootfsPath)
	}
	empty := &Sandbox{ID: "src"}
	if got := liveForkRootfsBase(empty); got != "" {
		t.Fatalf("empty source base = %q, want empty string", got)
	}
}
