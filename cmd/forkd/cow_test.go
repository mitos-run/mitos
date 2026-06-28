package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPathUnder(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/var/lib/mitos/jailer", "/var/lib/mitos", true},
		{"/var/lib/mitos", "/var/lib/mitos", true},
		{"/var/lib/mitos/jailer/x", "/var/lib/mitos", true},
		{"/var/lib/mitos-evil", "/var/lib/mitos", false}, // prefix but not nested
		{"/srv/jailer", "/var/lib/mitos", false},
		{"/var/lib/mitos/../mitos/jailer", "/var/lib/mitos", true}, // cleaned
	}
	for _, c := range cases {
		if got := pathUnder(c.child, c.parent); got != c.want {
			t.Errorf("pathUnder(%q, %q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

// TestVerifyChrootCoWLinkSucceeds: when a hard link from the data dir into the
// chroot base stays within one mount (the co-located layout the fix produces),
// the probe reports cow=true and leaves no probe files behind.
func TestVerifyChrootCoWLinkSucceeds(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	chrootBase := filepath.Join(dataDir, "jailer") // under dataDir, same tmpfs
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cow, detail, err := verifyChrootCoW(dataDir, chrootBase)
	if err != nil {
		t.Fatalf("verifyChrootCoW: %v", err)
	}
	if !cow {
		t.Fatalf("cow = false (%s), want true on a same-mount layout", detail)
	}
	// No probe leftovers.
	entries, _ := os.ReadDir(chrootBase)
	for _, e := range entries {
		if strings.Contains(e.Name(), "cow-probe") {
			t.Fatalf("probe left a file behind: %s", e.Name())
		}
	}
}

// TestVerifyChrootCoWDetectsCrossMount: an EXDEV from the probe link (the jailer
// hard-link crossing a mount boundary) reports cow=false with actionable
// remediation naming issue #526, so forkd warns instead of silently copying the
// rootfs per build.
func TestVerifyChrootCoWDetectsCrossMount(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	chrootBase := filepath.Join(root, "jail") // sibling, simulated cross-mount
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	exdev := func(string, string) error { return syscall.EXDEV }
	cow, detail, err := verifyChrootCoWWithLink(dataDir, chrootBase, exdev)
	if err != nil {
		t.Fatalf("verifyChrootCoWWithLink: %v", err)
	}
	if cow {
		t.Fatal("cow = true, want false on EXDEV")
	}
	if !strings.Contains(detail, "#526") || !strings.Contains(detail, "--chroot-base") {
		t.Fatalf("remediation detail not actionable: %q", detail)
	}
}

// TestVerifyChrootCoWPropagatesUnexpectedError: a non-EXDEV link failure is a
// real error, not a CoW verdict.
func TestVerifyChrootCoWPropagatesUnexpectedError(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	chrootBase := filepath.Join(root, "jail")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	boom := func(string, string) error { return syscall.EPERM }
	if _, _, err := verifyChrootCoWWithLink(dataDir, chrootBase, boom); err == nil {
		t.Fatal("expected an error on a non-EXDEV link failure")
	}
}
