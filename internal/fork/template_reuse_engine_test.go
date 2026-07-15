package fork

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

// This file covers the CreateTemplate reuse-or-rebuild gate (#584) at the
// Engine level, i.e. through Engine.CreateTemplate itself rather than the
// extracted shouldReuseTemplate/checkTemplateArtifactInvariants helpers
// already covered directly in template_reuse_test.go. It reuses the
// newTestEngine seam from template_init_test.go (no KVM, no real builder)
// and TemplateManager.DeleteTemplate (a plain os.RemoveAll, also no KVM) to
// exercise the full gate: reuse a healthy template, rebuild a corrupted one,
// and always rebuild when the caller passes forceRebuild=true.

// seedHealthyTemplate writes a well-formed template (mem, vmstate, rootfs,
// all mode 0o644 and owned by this process, matching
// checkTemplateArtifactInvariants) directly into e's dataDir and records its
// digest through e.casStore with the SAME metadata Engine.VerifyTemplate
// re-derives with (e.manifestMetadata(firecracker.DefaultVMConfig())), so the
// reuse gate sees a template it considers healthy.
func seedHealthyTemplate(t *testing.T, e *Engine, id string) {
	t.Helper()
	snapDir := filepath.Join(e.dataDir, "templates", id, "snapshot")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	mustWrite(t, filepath.Join(snapDir, "mem"), bytes.Repeat([]byte{0xAB}, 9<<20))
	mustWrite(t, filepath.Join(snapDir, "vmstate"), bytes.Repeat([]byte{0xCD}, 1<<20))
	mustWrite(t, filepath.Join(e.dataDir, "templates", id, "rootfs.ext4"), bytes.Repeat([]byte{0xEF}, 5<<20))

	meta := e.manifestMetadata(firecracker.DefaultVMConfig())
	if _, err := recordTemplateDigest(e.casStore, e.dataDir, id, meta); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
}

// writeRebuiltTemplate simulates a completed rebuild: it lays down a fresh,
// healthy snapshot (distinct byte content from seedHealthyTemplate's fixture,
// so a test can tell a reused template apart from a rebuilt one) without
// booting Firecracker.
func writeRebuiltTemplate(t *testing.T, dataDir, id string) {
	t.Helper()
	snapDir := filepath.Join(dataDir, "templates", id, "snapshot")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("mkdir rebuilt snapshot: %v", err)
	}
	mustWrite(t, filepath.Join(snapDir, "mem"), bytes.Repeat([]byte{0x11}, 9<<20))
	mustWrite(t, filepath.Join(snapDir, "vmstate"), bytes.Repeat([]byte{0x22}, 1<<20))
	mustWrite(t, filepath.Join(dataDir, "templates", id, "rootfs.ext4"), bytes.Repeat([]byte{0x33}, 5<<20))
}

// TestCreateTemplateEngineReusesHealthyTemplate is the Engine-level reuse
// case: a healthy on-disk template (digest verifies, ownership invariants
// pass) must be reused, so runTemplateBuild must never run.
func TestCreateTemplateEngineReusesHealthyTemplate(t *testing.T) {
	e := newTestEngine(t)
	id := "py"
	seedHealthyTemplate(t, e, id)

	built := false
	e.runTemplateBuild = func(string, firecracker.VMConfig, []string, *firecracker.WorkloadSpec, bool) error {
		built = true
		return nil
	}

	if err := e.CreateTemplate(id, "/fake/reuse-rootfs.ext4", nil, nil, nil, nil, CreateTemplateOpts{}); err != nil {
		t.Fatalf("CreateTemplate: unexpected error %v", err)
	}
	if built {
		t.Fatal("expected a healthy on-disk template to be reused without invoking runTemplateBuild")
	}
}

// TestCreateTemplateEngineRebuildsCorruptedTemplate is the Engine-level
// rebuild case: a template whose recorded digest no longer matches its
// on-disk mem file must be deleted (old directory gone before the rebuild
// starts) and rebuilt via runTemplateBuild.
func TestCreateTemplateEngineRebuildsCorruptedTemplate(t *testing.T) {
	e := newTestEngine(t)
	id := "py"
	seedHealthyTemplate(t, e, id)

	// Corrupt the mem file so re-derivation no longer matches the recorded
	// digest: the reuse gate must reject this template.
	memPath := filepath.Join(e.dataDir, "templates", id, "snapshot", "mem")
	mustWrite(t, memPath, []byte("corrupted"))

	oldTemplateDir := templateDir(e.dataDir, id)

	built := false
	e.runTemplateBuild = func(gotID string, cfg firecracker.VMConfig, initCommands []string, workload *firecracker.WorkloadSpec, _ bool) error {
		built = true
		// The pre-rebuild delete must run BEFORE runTemplateBuild: the old
		// (corrupted) template directory must already be gone.
		if _, err := os.Stat(oldTemplateDir); !os.IsNotExist(err) {
			t.Errorf("expected old template dir %s to be deleted before rebuild, stat err = %v", oldTemplateDir, err)
		}
		writeRebuiltTemplate(t, e.dataDir, gotID)
		return nil
	}

	if err := e.CreateTemplate(id, "/fake/rebuild-rootfs.ext4", nil, nil, nil, nil, CreateTemplateOpts{}); err != nil {
		t.Fatalf("CreateTemplate: unexpected error %v", err)
	}
	if !built {
		t.Fatal("expected a corrupted on-disk template to trigger a rebuild via runTemplateBuild")
	}

	// The rebuilt content must be the fresh fixture, not the corrupted bytes.
	got, err := os.ReadFile(filepath.Join(e.dataDir, "templates", id, "snapshot", "mem"))
	if err != nil {
		t.Fatalf("read rebuilt mem: %v", err)
	}
	if bytes.Equal(got, []byte("corrupted")) {
		t.Fatal("rebuilt template still carries the corrupted mem content")
	}
}

// TestCreateTemplateEngineForceRebuildAlwaysRebuilds is the finding-1 case:
// forceRebuild=true must always delete and rebuild, even over a HEALTHY
// on-disk template that shouldReuseTemplate would otherwise reuse.
func TestCreateTemplateEngineForceRebuildAlwaysRebuilds(t *testing.T) {
	e := newTestEngine(t)
	id := "py"
	seedHealthyTemplate(t, e, id) // Healthy: would be reused if forceRebuild were false.

	oldTemplateDir := templateDir(e.dataDir, id)

	built := false
	e.runTemplateBuild = func(gotID string, cfg firecracker.VMConfig, initCommands []string, workload *firecracker.WorkloadSpec, _ bool) error {
		built = true
		if _, err := os.Stat(oldTemplateDir); !os.IsNotExist(err) {
			t.Errorf("expected old template dir %s to be deleted before a forced rebuild, stat err = %v", oldTemplateDir, err)
		}
		writeRebuiltTemplate(t, e.dataDir, gotID)
		return nil
	}

	if err := e.CreateTemplate(id, "/fake/force-rebuild-rootfs.ext4", nil, nil, nil, nil, CreateTemplateOpts{ForceRebuild: true}); err != nil {
		t.Fatalf("CreateTemplate: unexpected error %v", err)
	}
	if !built {
		t.Fatal("expected forceRebuild=true to always rebuild, even over a healthy on-disk template")
	}
}
