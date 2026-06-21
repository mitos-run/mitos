package cas

import "testing"

// TestManifestEmptyHugePagesPreservesLegacyDigest proves the HugePages field is
// additive and #32-safe: a snapshot with no hugepage backing ("") encodes and
// digests identically to one built before the field existed. The default 4 KiB
// path must not change any existing snapshot's identity.
func TestManifestEmptyHugePagesPreservesLegacyDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 4096}},
		VMMVersion:            "v1.15.0",
		SnapshotFormatVersion: 1,
		CPUModel:              "test",
		KernelVersion:         "6.1.155",
		ConfigHash:            "abc",
	}
	withEmpty := base
	withEmpty.HugePages = ""
	if string(base.Canonical()) != string(withEmpty.Canonical()) {
		t.Fatalf("empty HugePages changed the canonical bytes:\n base=%s\n empty=%s", base.Canonical(), withEmpty.Canonical())
	}
	if base.Digest() != withEmpty.Digest() {
		t.Fatal("empty HugePages changed the digest; the field must be identity-neutral when unset")
	}
}

// TestManifestHugePagesChangeDigest proves a hugepage-backed snapshot has a
// DIFFERENT identity from a 4 KiB one: the backing is part of the content address,
// so a 2M template never collides with a 4 KiB template of otherwise identical
// content, and the field is captured in the canonical encoding.
func TestManifestHugePagesChangeDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 4096}},
		VMMVersion:            "v1.15.0",
		SnapshotFormatVersion: 1,
		ConfigHash:            "abc",
	}
	twoM := base
	twoM.HugePages = "2M"
	if base.Digest() == twoM.Digest() {
		t.Fatal("a 2M snapshot must not share a digest with a 4 KiB snapshot")
	}
	if got := string(twoM.Canonical()); !contains(got, `"hugePages":"2M"`) {
		t.Errorf("canonical encoding missing hugePages key, got %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
