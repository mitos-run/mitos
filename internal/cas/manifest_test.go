package cas

import (
	"bytes"
	"testing"
)

func TestBuildManifestDeterministicDigest(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a", bytes.Repeat([]byte{0x11}, 5<<20))
	b := writeFile(t, dir, "b", bytes.Repeat([]byte{0x22}, 3<<20))

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	m2, err := BuildManifest(map[string]string{"mem": a, "disk": b}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() != m2.Digest() {
		t.Fatalf("same inputs produced different digests: %s vs %s", m1.Digest(), m2.Digest())
	}
}

func TestBuildManifestInputMapOrderInvariant(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a", bytes.Repeat([]byte{0x11}, 5<<20))
	b := writeFile(t, dir, "b", bytes.Repeat([]byte{0x22}, 3<<20))

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Same logical content, different insertion order.
	m2, err := BuildManifest(map[string]string{"disk": b, "mem": a}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() != m2.Digest() {
		t.Fatalf("input map reorder changed digest: %s vs %s", m1.Digest(), m2.Digest())
	}
	if !bytes.Equal(m1.Canonical(), m2.Canonical()) {
		t.Fatalf("canonical encodings differ across map order")
	}
}

func TestBuildManifestChangedByteChangesDigest(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x11}, 5<<20)
	a := writeFile(t, dir, "a", data)

	m1, err := BuildManifest(map[string]string{"mem": a}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	changed := append([]byte(nil), data...)
	changed[len(changed)-1] ^= 0xFF
	a2 := writeFile(t, dir, "a", changed)
	m2, err := BuildManifest(map[string]string{"mem": a2}, "v1", 1000)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() == m2.Digest() {
		t.Fatalf("changed byte did not change manifest digest")
	}
}

func TestManifestCanonicalSortsFilesByName(t *testing.T) {
	m := Manifest{
		Files: []FileEntry{
			{Name: "zeta", Size: 1},
			{Name: "alpha", Size: 2},
		},
		VMMVersion:  "v1",
		CreatedUnix: 1,
	}
	canon := m.Canonical()
	ai := bytes.Index(canon, []byte("alpha"))
	zi := bytes.Index(canon, []byte("zeta"))
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("Canonical did not sort files by name: %s", canon)
	}
}
