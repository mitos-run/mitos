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

// Manifest describes a complete snapshot as a set of files plus metadata.
type Manifest struct {
	Files       []FileEntry
	VMMVersion  string
	CreatedUnix int64
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

	buf.WriteString(`"vmmVersion":`)
	writeJSONString(&buf, m.VMMVersion)

	buf.WriteByte('}')
	return buf.Bytes()
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

// BuildManifest chunks each file in the name to path map and assembles a
// manifest. The manifest is deterministic in the input map: file order does
// not affect the resulting digest.
func BuildManifest(files map[string]string, vmmVersion string, createdUnix int64) (Manifest, error) {
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

	return Manifest{
		Files:       entries,
		VMMVersion:  vmmVersion,
		CreatedUnix: createdUnix,
	}, nil
}
