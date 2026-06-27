package fork

import (
	"path/filepath"
	"testing"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/firecracker"
)

// TestDeleteTemplateUnpinsManifest proves DeleteTemplate unpins the template's
// CAS manifest so its chunks become eligible for eviction (#464). Before this,
// DeleteTemplate only removed the snapshot dir and left the manifest pinned
// forever, so deleted templates' chunks grew the CAS unbounded.
func TestDeleteTemplateUnpinsManifest(t *testing.T) {
	dd := t.TempDir()
	store, err := cas.New(filepath.Join(dd, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	d := cas.Digest("sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err := store.Pin(d); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	e := &Engine{
		dataDir:         dd,
		casStore:        store,
		templateMgr:     firecracker.NewTemplateManager("firecracker", "vmlinux", dd, firecracker.JailerConfig{}),
		templateDigests: map[string]cas.Digest{"t1": d},
	}

	if err := e.DeleteTemplate("t1"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	pinned, err := store.IsPinned(d)
	if err != nil {
		t.Fatalf("IsPinned: %v", err)
	}
	if pinned {
		t.Fatal("DeleteTemplate left the manifest pinned; deleted-template chunks would leak (#464)")
	}
}
