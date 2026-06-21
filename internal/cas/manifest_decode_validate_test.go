package cas

import "testing"

// TestDecodeManifestRejectsInvalidChunkDigest proves a manifest (which arrives
// from an untrusted peer over the snapshot-pull HTTP transport) cannot carry a
// chunk digest that is not a well-formed sha256. chunkPath joins the digest into
// a filesystem path, so "../../etc/passwd" would traverse out of the store (and
// its bytes stream to the output before the post-read digest check fails), and a
// sub-2-char digest would panic chunkPath's string(d)[:2]. decodeManifest must
// fail closed on such a manifest.
func TestDecodeManifestRejectsInvalidChunkDigest(t *testing.T) {
	bad := [][]byte{
		[]byte(`{"files":[{"name":"mem","size":4,"chunks":[{"digest":"../../etc/passwd","size":4}]}]}`),
		[]byte(`{"files":[{"name":"mem","size":4,"chunks":[{"digest":"a","size":4}]}]}`),
		[]byte(`{"files":[{"name":"mem","size":4,"chunks":[{"digest":"","size":4}]}]}`),
		[]byte(`{"files":[{"name":"mem","size":4,"chunks":[{"digest":"G000000000000000000000000000000000000000000000000000000000000000","size":4}]}]}`),
	}
	for _, data := range bad {
		if _, err := decodeManifest(data); err == nil {
			t.Fatalf("decodeManifest accepted an invalid chunk digest: %s", data)
		}
	}
}

// TestDecodeManifestAcceptsValidChunkDigest proves the validation does not reject
// a well-formed manifest.
func TestDecodeManifestAcceptsValidChunkDigest(t *testing.T) {
	real := digestBytes([]byte("hello"))
	data := []byte(`{"files":[{"name":"mem","size":5,"chunks":[{"digest":"` + string(real) + `","size":5}]}]}`)
	m, err := decodeManifest(data)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if len(m.Files) != 1 || len(m.Files[0].Chunks) != 1 {
		t.Fatalf("decode dropped a valid chunk: %+v", m)
	}
}
