package cas

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Pin records a manifest as pinned by writing a marker file under
// <root>/pins/<digest>. While pinned, none of the chunks the manifest
// references may be evicted. Pin markers are persisted so pinning survives
// restarts.
func (s *Store) Pin(manifestDigest Digest) error {
	return atomicWrite(s.pinPath(manifestDigest), nil)
}

// Unpin removes a manifest's pin marker. Unpinning a manifest that is not
// pinned is a no-op.
func (s *Store) Unpin(manifestDigest Digest) error {
	if err := os.Remove(s.pinPath(manifestDigest)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pin %s: %w", manifestDigest, err)
	}
	return nil
}

func (s *Store) pinPath(d Digest) string {
	return filepath.Join(s.root, "pins", string(d))
}

// pinnedChunks returns the union of all chunk digests referenced by any
// currently pinned manifest. A chunk in this set is never evicted.
func (s *Store) pinnedChunks() (map[Digest]struct{}, error) {
	pinned := make(map[Digest]struct{})
	entries, err := os.ReadDir(filepath.Join(s.root, "pins"))
	if err != nil {
		if os.IsNotExist(err) {
			return pinned, nil
		}
		return nil, fmt.Errorf("read pins: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		md := Digest(e.Name())
		m, err := s.GetManifest(md)
		if err != nil {
			// A pinned manifest whose body is missing pins nothing; skip it
			// rather than failing the whole eviction pass.
			continue
		}
		for _, fe := range m.Files {
			for _, c := range fe.Chunks {
				pinned[c.Digest] = struct{}{}
			}
		}
	}
	return pinned, nil
}

// chunkStat is a chunk's on-disk identity for eviction decisions.
type chunkStat struct {
	digest Digest
	path   string
	size   int64
	mtime  time.Time
}

// scanChunks walks the chunk tree, returning one chunkStat per chunk file.
func (s *Store) scanChunks() ([]chunkStat, error) {
	root := filepath.Join(s.root, "chunks")
	var stats []chunkStat
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		stats = append(stats, chunkStat{
			digest: Digest(info.Name()),
			path:   path,
			size:   info.Size(),
			mtime:  info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan chunks: %w", err)
	}
	return stats, nil
}

// totalChunkBytes returns the total size of all chunk files in the store.
func (s *Store) totalChunkBytes() (int64, error) {
	stats, err := s.scanChunks()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, st := range stats {
		total += st.size
	}
	return total, nil
}

// EvictToFit deletes least-recently-used unpinned chunks until the total chunk
// size is at or below maxBytes. Pinned chunks (referenced by any pinned
// manifest) are never deleted, even if that leaves the store above budget. It
// returns the number of bytes freed. Recency is read from chunk file mtime,
// which Materialize refreshes on access.
func (s *Store) EvictToFit(maxBytes int64) (int64, error) {
	stats, err := s.scanChunks()
	if err != nil {
		return 0, err
	}
	pinned, err := s.pinnedChunks()
	if err != nil {
		return 0, err
	}

	var total int64
	for _, st := range stats {
		total += st.size
	}
	if total <= maxBytes {
		return 0, nil
	}

	// Oldest mtime first: least recently used is evicted first.
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].mtime.Before(stats[j].mtime)
	})

	var freed int64
	for _, st := range stats {
		if total <= maxBytes {
			break
		}
		if _, isPinned := pinned[st.digest]; isPinned {
			continue
		}
		if err := os.Remove(st.path); err != nil {
			return freed, fmt.Errorf("evict chunk %s: %w", st.digest, err)
		}
		total -= st.size
		freed += st.size
	}
	return freed, nil
}
