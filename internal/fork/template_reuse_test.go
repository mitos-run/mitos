package fork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/firecracker"
)

// makeTemplateArtifactsCompliant rewrites the on-disk template artifacts to the
// normalized ownership contract (this process's euid, the shared kvm group, and
// the group-readable mode 0o640) so a root-gated test can exercise the
// checkTemplateArtifactInvariants happy path. It requires the privilege to
// chgrp to the shared kvm gid, so every caller must root-gate.
func makeTemplateArtifactsCompliant(t *testing.T, dataDir, id string) {
	t.Helper()
	// The containing directories must be group-traversable (0o750, shared kvm
	// gid) too, mirroring normalizeTemplateArtifacts, or the reuse invariant
	// rejects on the dir before it ever reaches the files.
	dir := filepath.Join(dataDir, "templates", id)
	for _, d := range []string{dir, filepath.Join(dir, "snapshot")} {
		if err := os.Chown(d, os.Geteuid(), firecracker.SharedKVMGID); err != nil {
			t.Fatalf("chown %s: %v", d, err)
		}
		if err := os.Chmod(d, 0o750); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}
	for _, p := range templateSnapshotFiles(dataDir, id) {
		if err := os.Chown(p, os.Geteuid(), firecracker.SharedKVMGID); err != nil {
			t.Fatalf("chown %s: %v", p, err)
		}
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
	}
}

// TestCheckTemplateArtifactInvariantsRejectsUnnormalized runs everywhere
// (non-root CI included): a freshly written fixture is mode 0o644, i.e. it was
// NEVER normalized to the group-readable 0o640 contract. The invariant gate
// must refuse it and name the offending path, proving the reuse gate is active
// without needing root to construct a foreign-group fixture. The fixture is
// owned by the test process's own gid, which the gate accepts for a non-root
// builder (a root builder tags the shared kvm gid instead), so the mode is the
// difference the gate catches here.
func TestCheckTemplateArtifactInvariantsRejectsUnnormalized(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	// The gate checks artifacts in a stable sorted order (mem, rootfs, vmstate),
	// so it names the first non-compliant one; assert it names an artifact under
	// this template's dir and cites the group-readable mode mismatch.
	templatePath := filepath.Join(dataDir, "templates", id)

	err := checkTemplateArtifactInvariants(dataDir, id)
	if err == nil {
		t.Fatal("expected checkTemplateArtifactInvariants to reject an un-normalized template")
	}
	if !strings.Contains(err.Error(), templatePath) {
		t.Fatalf("error %q does not name an artifact under %q", err.Error(), templatePath)
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Fatalf("error %q should cite the group-readable mode mismatch", err.Error())
	}
}

// TestCheckTemplateArtifactInvariantsCompliant is the happy path: a template
// normalized to euid:SharedKVMGID at mode 0o640 clears the invariant gate.
// Requires root to chgrp to the shared kvm gid, so it is skipped otherwise.
func TestCheckTemplateArtifactInvariantsCompliant(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chgrp to the shared kvm gid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)

	if err := checkTemplateArtifactInvariants(dataDir, id); err != nil {
		t.Fatalf("checkTemplateArtifactInvariants: unexpected error %v", err)
	}
}

// TestCheckTemplateArtifactInvariantsWrongMode starts from a compliant template
// and drops the rootfs mode to 0o600 (no group read), then asserts the error
// names the offending path. Root-gated so the compliant base can be built.
func TestCheckTemplateArtifactInvariantsWrongMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chgrp to the shared kvm gid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)
	rootfsPath := filepath.Join(dataDir, "templates", id, "rootfs.ext4")
	if err := os.Chmod(rootfsPath, 0o600); err != nil {
		t.Fatalf("chmod rootfs: %v", err)
	}

	err := checkTemplateArtifactInvariants(dataDir, id)
	if err == nil {
		t.Fatal("expected checkTemplateArtifactInvariants to fail on wrong mode")
	}
	if !strings.Contains(err.Error(), rootfsPath) {
		t.Fatalf("error %q does not name the offending path %q", err.Error(), rootfsPath)
	}
}

// TestCheckTemplateArtifactInvariantsForeignOwner chowns the rootfs to the
// jailer's build uid (issue #583/#597) and asserts the invariant check rejects
// it. Requires root to chown, so it is skipped otherwise.
func TestCheckTemplateArtifactInvariantsForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to a foreign uid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)
	rootfsPath := filepath.Join(dataDir, "templates", id, "rootfs.ext4")
	if err := os.Chown(rootfsPath, firecracker.JailerBuildUID, firecracker.SharedKVMGID); err != nil {
		t.Fatalf("chown rootfs: %v", err)
	}

	err := checkTemplateArtifactInvariants(dataDir, id)
	if err == nil {
		t.Fatal("expected checkTemplateArtifactInvariants to fail on foreign owner")
	}
	if !strings.Contains(err.Error(), rootfsPath) {
		t.Fatalf("error %q does not name the offending path %q", err.Error(), rootfsPath)
	}
}

// TestCheckTemplateArtifactInvariantsWrongGroup chgrps the rootfs to a group
// other than SharedKVMGID and asserts the invariant check rejects it: a husk
// that is not in the artifact's group cannot read it, so the template is not
// reusable. Requires root to chgrp, so it is skipped otherwise.
func TestCheckTemplateArtifactInvariantsWrongGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chgrp to a foreign gid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)
	rootfsPath := filepath.Join(dataDir, "templates", id, "rootfs.ext4")
	const otherGID = firecracker.SharedKVMGID + 1
	if err := os.Chown(rootfsPath, os.Geteuid(), otherGID); err != nil {
		t.Fatalf("chgrp rootfs: %v", err)
	}

	err := checkTemplateArtifactInvariants(dataDir, id)
	if err == nil {
		t.Fatal("expected checkTemplateArtifactInvariants to fail on wrong group")
	}
	if !strings.Contains(err.Error(), rootfsPath) {
		t.Fatalf("error %q does not name the offending path %q", err.Error(), rootfsPath)
	}
}

// TestShouldReuseTemplateNoOnDiskTemplate exercises the decision seam used by
// the CreateTemplate reuse-or-rebuild gate (#584) when no template exists on
// disk yet: verify must never be called, and the answer is "do not reuse"
// without an error (a fresh build, not a broken one).
func TestShouldReuseTemplateNoOnDiskTemplate(t *testing.T) {
	dataDir := t.TempDir()
	called := false
	reuse, err := shouldReuseTemplate(dataDir, "py", func(string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("shouldReuseTemplate: unexpected error %v", err)
	}
	if reuse {
		t.Fatal("expected reuse=false when no template exists on disk")
	}
	if called {
		t.Fatal("verify must not be called when no template exists on disk")
	}
}

// TestCreateTemplateReusesHealthyOnDisk exercises the reuse seam directly: a
// healthy on-disk template (recorded digest verifies, artifacts pass the
// ownership invariants) must be reused. Root-gated because the compliant
// artifacts must be group-owned by the shared kvm gid.
func TestCreateTemplateReusesHealthyOnDisk(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chgrp to the shared kvm gid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)
	store := newTestStore(t, dataDir)
	if _, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}

	verify := func(string) error {
		if _, err := verifyTemplate(dataDir, id, cas.Metadata{}); err != nil {
			return err
		}
		return checkTemplateArtifactInvariants(dataDir, id)
	}

	reuse, err := shouldReuseTemplate(dataDir, id, verify)
	if err != nil {
		t.Fatalf("shouldReuseTemplate: unexpected error %v", err)
	}
	if !reuse {
		t.Fatal("expected a healthy on-disk template to be reused")
	}
}

// TestCreateTemplateRebuildsBrokenOnDisk exercises the reuse seam when the
// on-disk template fails digest verification (tampered content): the gate must
// refuse reuse and surface the verification error so the caller can delete and
// rebuild. Digest verification runs before the ownership invariants, so this
// runs non-root.
func TestCreateTemplateRebuildsBrokenOnDisk(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)
	if _, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	// Tamper the mem file after recording so re-derivation fails.
	memPath := filepath.Join(dataDir, "templates", id, "snapshot", "mem")
	mustWrite(t, memPath, []byte("tampered"))

	verify := func(string) error {
		if _, err := verifyTemplate(dataDir, id, cas.Metadata{}); err != nil {
			return err
		}
		return checkTemplateArtifactInvariants(dataDir, id)
	}

	reuse, err := shouldReuseTemplate(dataDir, id, verify)
	if err == nil {
		t.Fatal("expected shouldReuseTemplate to surface the verification error")
	}
	if reuse {
		t.Fatal("expected a broken on-disk template not to be reused")
	}
}

// TestCreateTemplateRebuildsBrokenInvariants exercises the reuse seam when
// digest verification succeeds but the artifact ownership invariants fail
// (issue #583/#597): the gate must still refuse reuse. Root-gated so the
// compliant-then-broken base can be built.
func TestCreateTemplateRebuildsBrokenInvariants(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chgrp to the shared kvm gid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	makeTemplateArtifactsCompliant(t, dataDir, id)
	store := newTestStore(t, dataDir)
	if _, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	rootfsPath := filepath.Join(dataDir, "templates", id, "rootfs.ext4")
	if err := os.Chmod(rootfsPath, 0o600); err != nil {
		t.Fatalf("chmod rootfs: %v", err)
	}

	verify := func(string) error {
		if _, err := verifyTemplate(dataDir, id, cas.Metadata{}); err != nil {
			return err
		}
		return checkTemplateArtifactInvariants(dataDir, id)
	}

	reuse, err := shouldReuseTemplate(dataDir, id, verify)
	if err == nil {
		t.Fatal("expected shouldReuseTemplate to surface the invariant error")
	}
	if reuse {
		t.Fatal("expected a template with broken invariants not to be reused")
	}
}
