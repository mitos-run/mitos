//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"mitos.run/mitos/internal/vsock"
)

// TestReseedCRNGFailsClosedWhenNotCredited proves the guest reseed reports
// FAILURE when the credited RNDADDENTROPY ioctl cannot run, instead of
// over-reporting success on the uncredited write fallback. A regular file is not
// a char device, so the ioctl returns ENOTTY (the same shape as a kernel that
// cannot credit entropy); reseedCRNGAt must return false so the host
// fork-correctness gate reaps a fork whose CRNG could not be credibly reseeded
// rather than serve one that may share its siblings' CRNG output.
func TestReseedCRNGFailsClosedWhenNotCredited(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-urandom")
	if err := os.WriteFile(fake, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entropy := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	if reseedCRNGAt(entropy, fake) {
		t.Fatal("reseedCRNGAt reported success on an uncredited path; the fail-closed reseed gate is defeated")
	}
}

// TestReseedCRNGEmptyEntropyIsFalse keeps the empty-input contract.
func TestReseedCRNGEmptyEntropyIsFalse(t *testing.T) {
	if reseedCRNGAt(nil, filepath.Join(t.TempDir(), "unused")) {
		t.Fatal("empty entropy must report false")
	}
}

// TestMountVolumesEmpty proves an empty mount table mounts nothing.
func TestMountVolumesEmpty(t *testing.T) {
	if got := mountVolumes(nil); got != 0 {
		t.Errorf("mountVolumes(nil) = %d, want 0", got)
	}
}

// TestMountVolumesSkipsEmptyEntries proves an entry with no device or no mount
// path is skipped (not mounted) rather than attempted, so a malformed table
// cannot mount a device at an empty path.
func TestMountVolumesSkipsEmptyEntries(t *testing.T) {
	entries := []vsock.VolumeMountEntry{
		{Device: "", MountPath: "/x"},
		{Device: "/dev/vdb", MountPath: ""},
	}
	if got := mountVolumes(entries); got != 0 {
		t.Errorf("mountVolumes with only empty entries = %d, want 0", got)
	}
}

// TestIsMountedDetectsRoot proves isMounted parses /proc/mounts: the root
// filesystem "/" is always a mount point, and a path that is not a mount point
// is reported false.
func TestIsMountedDetectsRoot(t *testing.T) {
	if !isMounted("/") {
		t.Error("isMounted(\"/\") = false, want true (root is always mounted)")
	}
	if isMounted("/definitely/not/a/mount/point/zzz") {
		t.Error("isMounted of a non-mount path = true, want false")
	}
}

// TestWriteResolvConf proves the guest writes a single nameserver line for the
// delivered resolver IP, and that the write is idempotent (re-delivery yields
// the same content, not appended lines).
func TestWriteResolvConf(t *testing.T) {
	path := t.TempDir() + "/resolv.conf"

	if err := writeResolvConf(path, "169.254.1.1"); err != nil {
		t.Fatalf("writeResolvConf: %v", err)
	}
	want := "nameserver 169.254.1.1\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(got) != want {
		t.Errorf("resolv.conf = %q, want %q", got, want)
	}

	// Idempotent: writing again does not append, the content is identical.
	if err := writeResolvConf(path, "169.254.1.1"); err != nil {
		t.Fatalf("writeResolvConf (second): %v", err)
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf (second): %v", err)
	}
	if string(got2) != want {
		t.Errorf("resolv.conf after re-write = %q, want %q", got2, want)
	}
}

// TestWriteResolvConfEmptyIsNoop proves that with no resolver IP the guest does
// NOT create or clobber resolv.conf, preserving the feature-off behavior.
func TestWriteResolvConfEmptyIsNoop(t *testing.T) {
	path := t.TempDir() + "/resolv.conf"
	if err := os.WriteFile(path, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatalf("seed resolv.conf: %v", err)
	}
	if err := writeResolvConf(path, ""); err != nil {
		t.Fatalf("writeResolvConf(empty): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(got) != "nameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf was clobbered: %q", got)
	}
}
