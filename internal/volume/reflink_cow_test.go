//go:build reflink_integration

package volume

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReflinkCopyRealFilesystem exercises ReflinkCopy against a real cp on the
// host filesystem: the destination must be a byte-identical copy of the source.
// It runs ONLY under the reflink_integration build tag (KVM CI / bare-metal),
// because cp --reflink does not exist on darwin and the destination filesystem
// must support FICLONE (XFS/Btrfs) for the fast path; the production
// ReflinkCopy falls back to a full copy when reflink is unavailable, so the
// byte-equality assertion holds either way.
func TestReflinkCopyRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "sub", "dst.ext4")
	want := bytes.Repeat([]byte("PAPERCLIP"), 4096) // ~36 KiB of recognizable bytes
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	b := New(dir) // production runner (real cp)
	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("clone is not byte-identical to source: got %d bytes, want %d", len(got), len(want))
	}
}
