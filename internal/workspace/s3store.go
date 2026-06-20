package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/storecrypt"
)

// errObjectNotFound is the sentinel an ObjectClient returns from GetObject when
// the key is absent. The S3Store maps it to a missing-chunk error.
var errObjectNotFound = errors.New("object not found")

// ObjectClient is the minimal S3-compatible object surface the workspace
// object-store backend needs: put, get, and existence-check a single object by
// key. The production implementation wraps an S3 SDK client (credentials resolved
// from the referenced Secret, never logged); tests supply an in-memory fake. The
// reader returned by GetObject is the caller's to close.
type ObjectClient interface {
	PutObject(ctx context.Context, key string, r io.Reader) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	HeadObject(ctx context.Context, key string) (bool, error)
}

// S3Store is a content-addressed chunk store backed by an S3-compatible object
// bucket. It is a drop-in alternative to the node CAS for workspace
// hydrate/dehydrate: it implements the same ChunkStore interface, is plaintext
// content-addressed (so a given tree yields the same revision digest as the node
// CAS and content-addressed dedup is preserved), and round-trips byte-identical.
//
// Object layout under the configured prefix mirrors the node CAS:
//
//	<prefix>/chunks/<plainDigest[:2]>/<plainDigest>
//	<prefix>/manifests/<manifestDigest>
//
// Dedup is by object key: a chunk whose digest object already exists (HeadObject)
// is not re-uploaded.
type S3Store struct {
	obj    ObjectClient
	prefix string
}

var _ ChunkStore = (*S3Store)(nil)

// NewS3Store returns an S3-backed chunk store over obj. prefix is an optional key
// prefix so several workspaces can share one bucket without colliding; the empty
// prefix writes at the bucket root.
func NewS3Store(obj ObjectClient, prefix string) (*S3Store, error) {
	if obj == nil {
		return nil, fmt.Errorf("S3 object client is required")
	}
	return &S3Store{obj: obj, prefix: prefix}, nil
}

// chunkKey returns the object key for a plaintext chunk digest.
func (s *S3Store) chunkKey(d cas.Digest) string {
	return path.Join(s.prefix, "chunks", string(d)[:2], string(d))
}

// manifestKey returns the object key for a manifest digest.
func (s *S3Store) manifestKey(d cas.Digest) string {
	return path.Join(s.prefix, "manifests", string(d))
}

// PutSnapshot builds the plaintext-addressed manifest, uploads each distinct
// chunk object (skipping one already present: the dedup skip), and uploads the
// manifest object. The returned manifest digest equals the node CAS digest for
// the same files.
func (s *S3Store) PutSnapshot(files map[string]string, meta cas.Metadata) (cas.Manifest, error) {
	return putSnapshotBlobs(files, meta, s)
}

// GetManifest fetches and decodes the manifest object for a digest.
func (s *S3Store) GetManifest(d cas.Digest) (cas.Manifest, error) {
	if err := d.Validate(); err != nil {
		return cas.Manifest{}, err
	}
	data, err := s.readKey(s.manifestKey(d))
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("read manifest object %s: %w", d, err)
	}
	m, err := cas.DecodeManifest(data)
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("decode manifest %s: %w", d, err)
	}
	return m, nil
}

// MaterializeFileTo reconstructs the named plaintext file from the manifest to
// dstPath, fetching and verifying each chunk object.
func (s *S3Store) MaterializeFileTo(manifestDigest cas.Digest, name, dstPath string) error {
	return materializeBlobFile(manifestDigest, name, dstPath, s)
}

// --- blobCAS adapter: the seam putSnapshotBlobs / materializeBlobFile drive. ---

// hasBlob reports whether the chunk object for a plaintext digest exists.
func (s *S3Store) hasBlob(d cas.Digest) bool {
	if d.Validate() != nil {
		return false
	}
	ok, err := s.obj.HeadObject(context.Background(), s.chunkKey(d))
	return err == nil && ok
}

// putChunkBlob uploads a chunk's bytes (already the at-rest representation: this
// plaintext store uploads the plaintext block).
func (s *S3Store) putChunkBlob(d cas.Digest, atRest []byte) error {
	return s.obj.PutObject(context.Background(), s.chunkKey(d), bytes.NewReader(atRest))
}

// putManifestBlob uploads the manifest's at-rest bytes.
func (s *S3Store) putManifestBlob(d cas.Digest, atRest []byte) error {
	return s.obj.PutObject(context.Background(), s.manifestKey(d), bytes.NewReader(atRest))
}

// getChunkBlob fetches a chunk object's at-rest bytes.
func (s *S3Store) getChunkBlob(d cas.Digest) ([]byte, error) {
	return s.readKey(s.chunkKey(d))
}

// sealChunk for the plaintext S3 store is the identity: the at-rest bytes are the
// plaintext block.
func (s *S3Store) sealChunk(_ cas.Digest, block []byte) []byte { return block }

// sealManifest for the plaintext S3 store is the identity.
func (s *S3Store) sealManifest(_ cas.Digest, canonical []byte) []byte { return canonical }

// openChunk for the plaintext S3 store verifies the digest and returns the bytes.
func (s *S3Store) openChunk(d cas.Digest, atRest []byte) ([]byte, error) {
	if got := chunkDigest(atRest); got != d {
		return nil, fmt.Errorf("chunk %s failed verification: got %s", d, got)
	}
	return atRest, nil
}

// readKey reads an object fully, mapping a not-found to a clear error.
func (s *S3Store) readKey(key string) ([]byte, error) {
	rc, err := s.obj.GetObject(context.Background(), key)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	return io.ReadAll(rc)
}

// --- shared blob-CAS driver -------------------------------------------------
//
// blobCAS is the narrow surface the content-addressed put/get logic needs from
// either the filesystem EncryptedStore or the S3Store (plaintext or encrypted):
// chunk existence, at-rest seal/open, and at-rest blob put/get. It lets the
// chunking, dedup, and reconstruct logic live in one place so all backends share
// identical content-addressing and byte-identical round trips.
type blobCAS interface {
	hasBlob(d cas.Digest) bool
	sealChunk(d cas.Digest, block []byte) []byte
	sealManifest(d cas.Digest, canonical []byte) []byte
	openChunk(d cas.Digest, atRest []byte) ([]byte, error)
	putChunkBlob(d cas.Digest, atRest []byte) error
	putManifestBlob(d cas.Digest, atRest []byte) error
	getChunkBlob(d cas.Digest) ([]byte, error)
}

// putSnapshotBlobs is the shared PutSnapshot: build the plaintext manifest, seal
// and store each distinct chunk (dedup skip via hasBlob), then seal and store the
// manifest. Identical for every blobCAS backend, so they all share the exact
// content-addressing and dedup semantics.
func putSnapshotBlobs(files map[string]string, meta cas.Metadata, b blobCAS) (cas.Manifest, error) {
	m, err := cas.BuildManifest(files, meta)
	if err != nil {
		return cas.Manifest{}, err
	}
	for _, fe := range m.Files {
		if err := putFileBlobs(files[fe.Name], fe, b); err != nil {
			return cas.Manifest{}, fmt.Errorf("store chunks for %s: %w", fe.Name, err)
		}
	}
	d := m.Digest()
	if err := b.putManifestBlob(d, b.sealManifest(d, m.Canonical())); err != nil {
		return cas.Manifest{}, fmt.Errorf("store manifest %s: %w", d, err)
	}
	return m, nil
}

// putFileBlobs reads the file in cas.ChunkSize blocks (sized by the manifest's
// chunk refs) and seals+stores each distinct chunk, skipping one already present.
func putFileBlobs(srcPath string, fe cas.FileEntry, b blobCAS) error {
	f, err := openForRead(srcPath)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file

	buf := make([]byte, cas.ChunkSize)
	for _, c := range fe.Chunks {
		block := buf[:c.Size]
		if _, err := readFull(f, block); err != nil {
			return fmt.Errorf("read chunk for %s: %w", fe.Name, err)
		}
		if b.hasBlob(c.Digest) {
			continue
		}
		if err := b.putChunkBlob(c.Digest, b.sealChunk(c.Digest, block)); err != nil {
			return fmt.Errorf("store chunk %s: %w", c.Digest, err)
		}
	}
	return nil
}

// materializeBlobFile reconstructs the named plaintext file to dstPath by
// fetching, opening (decrypting + verifying), and concatenating its chunks.
func materializeBlobFile(manifestDigest cas.Digest, name, dstPath string, b blobCASManifest) error {
	m, err := b.GetManifest(manifestDigest)
	if err != nil {
		return err
	}
	for _, fe := range m.Files {
		if fe.Name != name {
			continue
		}
		out, err := createForWrite(dstPath)
		if err != nil {
			return err
		}
		writeErr := streamBlobFile(out, fe, b)
		closeErr := out.Close()
		if writeErr != nil {
			removePartial(dstPath)
			return writeErr
		}
		if closeErr != nil {
			removePartial(dstPath)
			return fmt.Errorf("close %s: %w", dstPath, closeErr)
		}
		return nil
	}
	return fmt.Errorf("manifest %s has no file %q", manifestDigest, name)
}

// blobCASManifest is blobCAS plus GetManifest, the surface materializeBlobFile
// needs (it loads the manifest then streams chunks).
type blobCASManifest interface {
	blobCAS
	GetManifest(d cas.Digest) (cas.Manifest, error)
}

// streamBlobFile fetches and opens each chunk of fe and writes the plaintext in
// order.
func streamBlobFile(out io.Writer, fe cas.FileEntry, b blobCAS) error {
	for _, c := range fe.Chunks {
		atRest, err := b.getChunkBlob(c.Digest)
		if err != nil {
			return fmt.Errorf("read chunk %s for file %s: %w", c.Digest, fe.Name, err)
		}
		pt, err := b.openChunk(c.Digest, atRest)
		if err != nil {
			return fmt.Errorf("file %s: %w", fe.Name, err)
		}
		if _, err := out.Write(pt); err != nil {
			return fmt.Errorf("write chunk %s for file %s: %w", c.Digest, fe.Name, err)
		}
	}
	return nil
}

// --- encrypted S3 store -----------------------------------------------------

// EncryptedS3Store is an S3-backed chunk store whose chunks and manifests are
// encrypted at rest in the bucket under a per-workspace DEK. It composes the S3
// object backend with the same AES-256-GCM envelope the filesystem
// EncryptedStore uses, so artifacts are encrypted at rest in S3 while the round
// trip stays byte-identical and the manifest digest stays plaintext-addressed
// (dedup preserved). It is the S3 equivalent of pairing EncryptionKeyRef with
// ObjectStorageRef.
type EncryptedS3Store struct {
	*S3Store
	enc *envelope // the AES-256-GCM seal/open envelope, shared with the filesystem EncryptedStore
}

var _ ChunkStore = (*EncryptedS3Store)(nil)

// NewEncryptedS3Store returns an encrypted S3-backed chunk store over obj using
// dek for at-rest encryption. The DEK is a secret value the caller may Zeroize
// after this returns.
func NewEncryptedS3Store(obj ObjectClient, prefix string, dek storecrypt.Key) (*EncryptedS3Store, error) {
	s3, err := NewS3Store(obj, prefix)
	if err != nil {
		return nil, err
	}
	enc, err := newEnvelope(dek)
	if err != nil {
		return nil, err
	}
	return &EncryptedS3Store{S3Store: s3, enc: enc}, nil
}

// PutSnapshot stores each chunk encrypted at rest in the bucket.
func (e *EncryptedS3Store) PutSnapshot(files map[string]string, meta cas.Metadata) (cas.Manifest, error) {
	return putSnapshotBlobs(files, meta, e)
}

// GetManifest fetches, decrypts, and decodes the manifest object.
func (e *EncryptedS3Store) GetManifest(d cas.Digest) (cas.Manifest, error) {
	if err := d.Validate(); err != nil {
		return cas.Manifest{}, err
	}
	atRest, err := e.readKey(e.manifestKey(d))
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("read manifest object %s: %w", d, err)
	}
	pt, err := e.enc.open(d, atRest)
	if err != nil {
		return cas.Manifest{}, err
	}
	m, err := cas.DecodeManifest(pt)
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("decode manifest %s: %w", d, err)
	}
	return m, nil
}

// MaterializeFileTo reconstructs a named plaintext file, decrypting each chunk.
func (e *EncryptedS3Store) MaterializeFileTo(manifestDigest cas.Digest, name, dstPath string) error {
	return materializeBlobFile(manifestDigest, name, dstPath, e)
}

// sealChunk / sealManifest / openChunk override the S3Store identity envelope
// with the AES-256-GCM envelope.
func (e *EncryptedS3Store) sealChunk(d cas.Digest, block []byte) []byte {
	return e.enc.seal(d, block)
}

func (e *EncryptedS3Store) sealManifest(d cas.Digest, canonical []byte) []byte {
	return e.enc.seal(d, canonical)
}

func (e *EncryptedS3Store) openChunk(d cas.Digest, atRest []byte) ([]byte, error) {
	return e.enc.open(d, atRest)
}
