package fork

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsImageRef(t *testing.T) {
	// An existing file on disk is always treated as a rootfs file path, never
	// an image ref, so the hand-built CI rootfs and existing file-path tests
	// keep working.
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("not really ext4"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"existing file is a path", rootfs, false},
		{"docker hub short ref", "python:3.12-slim", true},
		{"alpine ref", "alpine:3.20", true},
		{"fully qualified ref", "docker.io/library/busybox:latest", true},
		{"missing abs path treated as file-path intent", "/abs/path/does-not-exist/rootfs.ext4", false},
		{"empty string", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isImageRef(tc.in); got != tc.want {
				t.Errorf("isImageRef(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
