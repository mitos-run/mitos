package fork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/cas"
)

// TestCheckTemplateArtifactInvariantsOwnedByEuid is the happy path: a
// template fixture written by this test process is owned by the process's
// own euid and carries mode 0o644 on every artifact (mustWrite in
// verify_test.go already writes 0o644), so the invariant check must pass.
func TestCheckTemplateArtifactInvariantsOwnedByEuid(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)

	if err := checkTemplateArtifactInvariants(dataDir, id); err != nil {
		t.Fatalf("checkTemplateArtifactInvariants: unexpected error %v", err)
	}
}

// TestCheckTemplateArtifactInvariantsWrongMode writes the rootfs with mode
// 0o600 instead of the required 0o644 and asserts the error names the
// offending path.
func TestCheckTemplateArtifactInvariantsWrongMode(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
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

// TestCheckTemplateArtifactInvariantsForeignOwner chowns the rootfs to a
// foreign uid (64000, the jailer's build uid per issue #583) and asserts the
// invariant check rejects it. Requires root to chown, so it is skipped
// otherwise.
func TestCheckTemplateArtifactInvariantsForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to a foreign uid requires root")
	}
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	rootfsPath := filepath.Join(dataDir, "templates", id, "rootfs.ext4")
	if err := os.Chown(rootfsPath, 64000, 64000); err != nil {
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
// ownership invariants) must be reused.
func TestCreateTemplateReusesHealthyOnDisk(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
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
// on-disk template fails digest verification (tampered content): the gate
// must refuse reuse and surface the verification error so the caller can
// delete and rebuild.
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
// (issue #583): the gate must still refuse reuse.
func TestCreateTemplateRebuildsBrokenInvariants(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
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
