package cas

import (
	"bytes"
	"testing"
)

func TestBuildManifestDeterministicDigest(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a", bytes.Repeat([]byte{0x11}, 5<<20))
	b := writeFile(t, dir, "b", bytes.Repeat([]byte{0x22}, 3<<20))

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	m2, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
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

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Same logical content, different insertion order.
	m2, err := BuildManifest(map[string]string{"disk": b, "mem": a}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
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

	m1, err := BuildManifest(map[string]string{"mem": a}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	changed := append([]byte(nil), data...)
	changed[len(changed)-1] ^= 0xFF
	a2 := writeFile(t, dir, "a", changed)
	m2, err := BuildManifest(map[string]string{"mem": a2}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
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

func TestManifestCanonicalDeterministicWithEnvFields(t *testing.T) {
	mk := func() Manifest {
		return Manifest{
			Files:                 []FileEntry{{Name: "mem", Size: 3}},
			VMMVersion:            "1.15.0",
			CreatedUnix:           42,
			SnapshotFormatVersion: CurrentSnapshotFormatVersion,
			CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
			KernelVersion:         "6.1.0",
			ConfigHash:            "abc123",
		}
	}
	first, second := mk(), mk()
	if !bytes.Equal(first.Canonical(), second.Canonical()) {
		t.Fatal("same content produced different canonical bytes")
	}
	if first.Digest() != second.Digest() {
		t.Fatal("same content produced different digest")
	}
}

func TestManifestEnvFieldsChangeDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 3}},
		SnapshotFormatVersion: 1,
		CPUModel:              "cpuA",
		KernelVersion:         "kA",
		ConfigHash:            "hA",
	}
	for _, mut := range []func(*Manifest){
		func(m *Manifest) { m.SnapshotFormatVersion = 2 },
		func(m *Manifest) { m.CPUModel = "cpuB" },
		func(m *Manifest) { m.KernelVersion = "kB" },
		func(m *Manifest) { m.ConfigHash = "hB" },
	} {
		changed := base
		mut(&changed)
		if base.Digest() == changed.Digest() {
			t.Fatal("changing an env field did not change the digest")
		}
	}
}

func TestManifestHotPagesRoundTrip(t *testing.T) {
	m := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 7, Chunks: []ChunkRef{{Digest: digestBytes([]byte("x")), Size: 1}}}},
		VMMVersion:            "1.15.0",
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		HotPages: &HotPageSet{
			PageSizeBytes: 2 << 20,
			File:          "mem",
			Offsets:       []int64{0, 2 << 20, 4 << 20},
		},
	}
	got, err := decodeManifest(m.Canonical())
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if got.HotPages == nil {
		t.Fatalf("round-trip dropped HotPages")
	}
	if got.HotPages.PageSizeBytes != m.HotPages.PageSizeBytes ||
		got.HotPages.File != m.HotPages.File ||
		len(got.HotPages.Offsets) != len(m.HotPages.Offsets) {
		t.Fatalf("round-trip lost HotPages fields: got %+v want %+v", got.HotPages, m.HotPages)
	}
	for i, off := range m.HotPages.Offsets {
		if got.HotPages.Offsets[i] != off {
			t.Fatalf("offset %d round-trip mismatch: got %d want %d", i, got.HotPages.Offsets[i], off)
		}
	}
	if got.Digest() != m.Digest() {
		t.Fatalf("hot-page round-trip digest mismatch: %s vs %s", got.Digest(), m.Digest())
	}
}

// TestManifestNilHotPagesPreservesLegacyDigest is the snapshot-compat (#32)
// guard: a manifest with no hot-page set must produce the SAME canonical bytes
// and digest as one built before the field existed. The hot-page set is purely
// additive, so a snapshot that never captured one keeps its old identity.
func TestManifestNilHotPagesPreservesLegacyDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 3}},
		VMMVersion:            "1.15.0",
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		CPUModel:              "cpuA",
		KernelVersion:         "kA",
		ConfigHash:            "hA",
	}
	withNil := base
	withNil.HotPages = nil
	if !bytes.Equal(base.Canonical(), withNil.Canonical()) {
		t.Fatalf("nil hot-page set changed canonical bytes:\n base: %s\n nil:  %s", base.Canonical(), withNil.Canonical())
	}
	if base.Digest() != withNil.Digest() {
		t.Fatalf("nil hot-page set changed digest: %s vs %s", base.Digest(), withNil.Digest())
	}
	// An empty (non-nil but zero) set is also identity-neutral: there is nothing
	// to prefetch, so it must not perturb the digest either.
	withEmpty := base
	withEmpty.HotPages = &HotPageSet{}
	if base.Digest() != withEmpty.Digest() {
		t.Fatalf("empty hot-page set changed digest: %s vs %s", base.Digest(), withEmpty.Digest())
	}
}

// TestManifestHotPagesChangeDigest proves the hot-page set is content-addressed:
// a non-empty set is part of the snapshot's identity, so two snapshots that
// prefetch different page sets get different digests. This is what lets a shared
// prefetched set still count once across forks per the #33 CoW story: identical
// hot-page sets collapse to one digest, divergent ones do not.
func TestManifestHotPagesChangeDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 3}},
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		HotPages:              &HotPageSet{PageSizeBytes: 2 << 20, File: "mem", Offsets: []int64{0, 2 << 20}},
	}
	for name, mut := range map[string]func(*HotPageSet){
		"offsets":  func(h *HotPageSet) { h.Offsets = []int64{0, 4 << 20} },
		"pagesize": func(h *HotPageSet) { h.PageSizeBytes = 4 << 20 },
		"file":     func(h *HotPageSet) { h.File = "disk" },
	} {
		changed := base
		hp := *base.HotPages
		mut(&hp)
		changed.HotPages = &hp
		if base.Digest() == changed.Digest() {
			t.Fatalf("changing hot-page %s did not change the digest", name)
		}
	}
}

// TestManifestHotPagesCanonicalOrderInvariant proves the canonical encoding does
// not depend on the order offsets were appended: two sets with the same offsets
// in different order hash identically, so a re-captured set with a stable
// content (same pages, any discovery order) keeps the snapshot's identity.
func TestManifestHotPagesCanonicalOrderInvariant(t *testing.T) {
	a := Manifest{
		Files:    []FileEntry{{Name: "mem", Size: 3}},
		HotPages: &HotPageSet{PageSizeBytes: 2 << 20, File: "mem", Offsets: []int64{0, 2 << 20, 4 << 20}},
	}
	b := Manifest{
		Files:    []FileEntry{{Name: "mem", Size: 3}},
		HotPages: &HotPageSet{PageSizeBytes: 2 << 20, File: "mem", Offsets: []int64{4 << 20, 0, 2 << 20}},
	}
	if !bytes.Equal(a.Canonical(), b.Canonical()) {
		t.Fatalf("hot-page offset order changed canonical bytes")
	}
}

func TestManifestRoundTripEnvFields(t *testing.T) {
	m := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 7, Chunks: []ChunkRef{{Digest: digestBytes([]byte("x")), Size: 1}}}},
		VMMVersion:            "1.15.0",
		CreatedUnix:           99,
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:         "6.1.0",
		ConfigHash:            "deadbeef",
	}
	got, err := decodeManifest(m.Canonical())
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if got.SnapshotFormatVersion != m.SnapshotFormatVersion ||
		got.CPUModel != m.CPUModel ||
		got.KernelVersion != m.KernelVersion ||
		got.ConfigHash != m.ConfigHash ||
		got.VMMVersion != m.VMMVersion ||
		got.CreatedUnix != m.CreatedUnix {
		t.Fatalf("round-trip lost env fields: got %+v want %+v", got, m)
	}
	// A decoded manifest must re-derive the same digest.
	if got.Digest() != m.Digest() {
		t.Fatalf("round-trip digest mismatch: %s vs %s", got.Digest(), m.Digest())
	}
}
