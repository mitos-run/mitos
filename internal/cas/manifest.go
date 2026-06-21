package cas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// FileEntry is one file within a snapshot: its logical name, byte size, and
// the ordered list of chunks that reconstruct it.
type FileEntry struct {
	Name   string
	Size   int64
	Chunks []ChunkRef
}

// CurrentSnapshotFormatVersion is the snapshot compatibility format version this
// build produces and can restore. It is stamped into every manifest at template
// build and checked on load (see internal/snapcompat). Bump it whenever the
// snapshot layout or restore contract changes incompatibly.
const CurrentSnapshotFormatVersion = 1

// HotPageSet is the captured hot-page working set for snapshot-resume prefetch
// (issue #167): the set of guest memory page offsets a userfaultfd handler
// should preload into the restored VM before resume, so the lazy-fault tail that
// dominates claim->first-exec is paid up front instead of as a storm of
// post-resume faults.
//
// It is an OPTIONAL, content-addressed descriptor on the snapshot manifest. A
// snapshot that never captured one carries a nil HotPageSet and is byte-identical
// (and digest-identical) to a snapshot built before the field existed, so the
// field is purely additive and does not break snapshot compatibility (#32). When
// present and non-empty it IS part of the content-addressed digest: two snapshots
// with different hot-page sets have different identities, while two forks that
// share the SAME captured set collapse to one digest, which is what keeps a
// prefetched shared page set counted once across forks per the #33 CoW story.
//
// PageSizeBytes is the unit the Offsets are expressed in (2 MiB for the
// hugepage-backed memory file this targets, 4 KiB for a base-page file). File
// names which manifest file the offsets index into (conventionally the memory
// file, "mem"). Offsets are byte offsets into that file, each a multiple of
// PageSizeBytes; the canonical encoding sorts and de-duplicates them so the
// descriptor's identity does not depend on capture order.
type HotPageSet struct {
	// PageSizeBytes is the page granularity the offsets are expressed in.
	PageSizeBytes int64
	// File is the manifest file name the offsets index into (e.g. "mem").
	File string
	// Offsets are the byte offsets of the hot pages within File. The canonical
	// encoding sorts and de-duplicates them, so order and duplicates do not
	// affect the snapshot's identity.
	Offsets []int64
}

// isEmpty reports whether the set carries nothing to prefetch. An empty set is
// identity-neutral: it is omitted from the canonical encoding so it never
// perturbs the digest, preserving compatibility with pre-field snapshots.
func (h *HotPageSet) isEmpty() bool {
	return h == nil || len(h.Offsets) == 0
}

// Manifest describes a complete snapshot as a set of files plus metadata.
//
// SnapshotFormatVersion, VMMVersion, CPUModel, and KernelVersion describe the
// environment that produced the snapshot. They are part of the content-addressed
// digest on purpose: the producing environment is part of a snapshot's identity,
// so a snapshot built under a different Firecracker or CPU never collides with
// one built here. ConfigHash binds the snapshot to the microvm machine config it
// was captured under.
//
// HotPages is the optional, additive hot-page working set for snapshot-resume
// prefetch (issue #167). A nil or empty set is omitted from the canonical
// encoding, so a snapshot without one keeps the exact identity it had before the
// field existed (#32 compatibility).
type Manifest struct {
	Files                 []FileEntry
	VMMVersion            string
	CreatedUnix           int64
	SnapshotFormatVersion int
	CPUModel              string
	KernelVersion         string
	ConfigHash            string
	HotPages              *HotPageSet
}

// Canonical returns a deterministic byte encoding of the manifest. Files are
// sorted by name and every object uses a fixed field order, so the result
// does not depend on Go map ordering or input ordering. Two manifests with
// the same logical content always produce identical bytes.
func (m Manifest) Canonical() []byte {
	files := append([]FileEntry(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	var buf bytes.Buffer
	buf.WriteByte('{')

	// Fixed field order, alphabetical by JSON key, so the encoding never depends
	// on struct or map iteration order. Keep this order stable: it is part of the
	// content-addressed digest.
	buf.WriteString(`"configHash":`)
	writeJSONString(&buf, m.ConfigHash)
	buf.WriteByte(',')

	buf.WriteString(`"cpuModel":`)
	writeJSONString(&buf, m.CPUModel)
	buf.WriteByte(',')

	buf.WriteString(`"createdUnix":`)
	writeJSONInt(&buf, m.CreatedUnix)
	buf.WriteByte(',')

	buf.WriteString(`"files":[`)
	for i, fe := range files {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('{')
		buf.WriteString(`"chunks":[`)
		for j, c := range fe.Chunks {
			if j > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(`{"digest":`)
			writeJSONString(&buf, string(c.Digest))
			buf.WriteString(`,"size":`)
			writeJSONInt(&buf, int64(c.Size))
			buf.WriteByte('}')
		}
		buf.WriteString(`],`)
		buf.WriteString(`"name":`)
		writeJSONString(&buf, fe.Name)
		buf.WriteString(`,"size":`)
		writeJSONInt(&buf, fe.Size)
		buf.WriteByte('}')
	}
	buf.WriteString(`],`)

	// hotPages is OPTIONAL and additive. It is emitted only when the set carries
	// pages to prefetch; an absent or empty set is omitted entirely so the bytes
	// (and therefore the digest) are identical to a snapshot built before the
	// field existed. When emitted, the offsets are sorted and de-duplicated so the
	// descriptor's identity does not depend on capture order or duplicates.
	if !m.HotPages.isEmpty() {
		offsets := dedupeSortedInt64(m.HotPages.Offsets)
		buf.WriteString(`"hotPages":{"file":`)
		writeJSONString(&buf, m.HotPages.File)
		buf.WriteString(`,"offsets":[`)
		for i, off := range offsets {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONInt(&buf, off)
		}
		buf.WriteString(`],"pageSizeBytes":`)
		writeJSONInt(&buf, m.HotPages.PageSizeBytes)
		buf.WriteString(`},`)
	}

	buf.WriteString(`"kernelVersion":`)
	writeJSONString(&buf, m.KernelVersion)
	buf.WriteByte(',')

	buf.WriteString(`"snapshotFormatVersion":`)
	writeJSONInt(&buf, int64(m.SnapshotFormatVersion))
	buf.WriteByte(',')

	buf.WriteString(`"vmmVersion":`)
	writeJSONString(&buf, m.VMMVersion)

	buf.WriteByte('}')
	return buf.Bytes()
}

// dedupeSortedInt64 returns the input sorted ascending with duplicates removed.
// The input is not mutated. It is the single place the hot-page offsets are
// normalized for the canonical encoding, so capture order and duplicate
// discovery never change the descriptor's identity.
func dedupeSortedInt64(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	cp := append([]int64(nil), in...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	out := cp[:1]
	for _, v := range cp[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func writeJSONInt(buf *bytes.Buffer, v int64) {
	b, _ := json.Marshal(v) //nolint:errcheck // int64 marshal never fails
	buf.Write(b)
}

func writeJSONString(buf *bytes.Buffer, s string) {
	b, _ := json.Marshal(s) //nolint:errcheck // string marshal never fails
	buf.Write(b)
}

// Digest returns the sha256 of the canonical encoding. It is the stable
// identifier for a snapshot and is safe to log.
func (m Manifest) Digest() Digest {
	return digestBytes(m.Canonical())
}

// Metadata carries the non-file manifest fields a caller stamps when building a
// snapshot: the producing environment (format version, Firecracker, CPU, kernel),
// the machine config hash, and the build time. All of these except CreatedUnix
// are part of the content-addressed digest; CreatedUnix is recorded for humans
// and is conventionally fixed at 0 for reproducible template digests.
type Metadata struct {
	SnapshotFormatVersion int
	VMMVersion            string
	CPUModel              string
	KernelVersion         string
	ConfigHash            string
	CreatedUnix           int64
	// HotPages is the optional captured hot-page working set for snapshot-resume
	// prefetch (issue #167). Nil when none was captured; in that case the built
	// manifest is byte-identical to a pre-field one.
	HotPages *HotPageSet
}

// BuildManifest chunks each file in the name to path map and assembles a
// manifest. The manifest is deterministic in the input map: file order does
// not affect the resulting digest.
func BuildManifest(files map[string]string, meta Metadata) (Manifest, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]FileEntry, 0, len(names))
	for _, name := range names {
		path := files[name]
		info, err := os.Stat(path)
		if err != nil {
			return Manifest{}, fmt.Errorf("stat %s: %w", path, err)
		}
		chunks, err := chunkFile(path)
		if err != nil {
			return Manifest{}, err
		}
		entries = append(entries, FileEntry{
			Name:   name,
			Size:   info.Size(),
			Chunks: chunks,
		})
	}

	return manifestFrom(entries, meta), nil
}

// manifestFrom assembles a Manifest from file entries and metadata. It is the
// single place that maps Metadata onto the manifest fields, so BuildManifest and
// the store's PutSnapshot stay in lockstep.
func manifestFrom(entries []FileEntry, meta Metadata) Manifest {
	return Manifest{
		Files:                 entries,
		VMMVersion:            meta.VMMVersion,
		CreatedUnix:           meta.CreatedUnix,
		SnapshotFormatVersion: meta.SnapshotFormatVersion,
		CPUModel:              meta.CPUModel,
		KernelVersion:         meta.KernelVersion,
		ConfigHash:            meta.ConfigHash,
		HotPages:              meta.HotPages,
	}
}
