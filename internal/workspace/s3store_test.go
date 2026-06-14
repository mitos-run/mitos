package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeObjectStore is an in-memory ObjectClient standing in for an S3 bucket. It
// records every key written so a test can assert content-addressed dedup (no key
// is written twice). It is safe for concurrent use.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string // every key passed to PutObject, in order (including repeats)
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

func (f *fakeObjectStore) PutObject(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeObjectStore) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, errObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (f *fakeObjectStore) HeadObject(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeObjectStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func TestS3DehydrateHydrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	obj := newFakeObjectStore()
	s3, err := NewS3Store(obj, "ws-prefix")
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	src := newFakeAgent(t)
	want := map[string]string{
		"main.go":           "package main",
		"sub/nested.txt":    "nested content",
		"sub/deep/data.bin": "\x00\x01binary\xff",
		"empty":             "",
	}
	for rel, content := range want {
		src.writeFile(t, rel, content)
	}

	digest, err := DehydrateTo(ctx, src, s3, nil, nil)
	if err != nil {
		t.Fatalf("DehydrateTo: %v", err)
	}

	dst := newFakeAgent(t)
	if err := HydrateFrom(ctx, dst, s3, digest); err != nil {
		t.Fatalf("HydrateFrom: %v", err)
	}
	got := dst.listFiles(t)
	if len(got) != len(want) {
		t.Fatalf("round trip file count = %d, want %d", len(got), len(want))
	}
	for rel, content := range want {
		if got[rel] != content {
			t.Errorf("round trip %s = %q, want %q", rel, got[rel], content)
		}
	}
}

func TestS3DigestMatchesNodeCASDigest(t *testing.T) {
	// The S3 backend is plaintext content-addressed exactly like the node CAS, so
	// a given tree yields the SAME revision content identifier under both. This is
	// what makes S3 a drop-in alternative without breaking dedup.
	ctx := context.Background()
	tree := map[string]string{"a.txt": "alpha", "b/c.txt": "charlie"}

	node := newStore(t)
	na := newFakeAgent(t)
	for rel, c := range tree {
		na.writeFile(t, rel, c)
	}
	nodeDigest, err := Dehydrate(ctx, na, node, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	s3, err := NewS3Store(newFakeObjectStore(), "")
	if err != nil {
		t.Fatal(err)
	}
	sa := newFakeAgent(t)
	for rel, c := range tree {
		sa.writeFile(t, rel, c)
	}
	s3Digest, err := DehydrateTo(ctx, sa, s3, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nodeDigest != s3Digest {
		t.Fatalf("s3 digest %s != node CAS digest %s; S3 backend broke content addressing", s3Digest, nodeDigest)
	}
}

func TestS3DedupsByChunkDigest(t *testing.T) {
	ctx := context.Background()
	obj := newFakeObjectStore()
	s3, err := NewS3Store(obj, "")
	if err != nil {
		t.Fatal(err)
	}

	a := newFakeAgent(t)
	a.writeFile(t, "a.txt", "alpha")
	a.writeFile(t, "b/c.txt", "charlie")
	d1, err := DehydrateTo(ctx, a, s3, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	putsAfterFirst := obj.putCount()

	b := newFakeAgent(t)
	b.writeFile(t, "a.txt", "alpha")
	b.writeFile(t, "b/c.txt", "charlie")
	d2, err := DehydrateTo(ctx, b, s3, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("unchanged tree produced different digests: %s != %s", d1, d2)
	}
	// The manifest object may be re-put (idempotent, same key), but no NEW chunk
	// object should be written for the identical tree.
	newChunkPuts := 0
	for _, k := range obj.puts[putsAfterFirst:] {
		if strings.Contains(k, "/chunks/") || strings.HasPrefix(k, "chunks/") {
			newChunkPuts++
		}
	}
	if newChunkPuts != 0 {
		t.Fatalf("re-dehydrating an identical tree wrote %d new chunk objects; dedup broken", newChunkPuts)
	}
}

func TestS3EncryptedRoundTrip(t *testing.T) {
	// S3 + per-workspace encryption compose: an EncryptedStore can wrap an S3
	// object backend so artifacts are encrypted at rest in the bucket while the
	// round trip stays byte-identical and the digest stays plaintext-addressed.
	ctx := context.Background()
	obj := newFakeObjectStore()
	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)
	defer dek.Zeroize()

	es, err := NewEncryptedS3Store(obj, "enc", dek)
	if err != nil {
		t.Fatalf("NewEncryptedS3Store: %v", err)
	}

	src := newFakeAgent(t)
	const marker = "PLAINTEXT-MARKER-shouldnotappear"
	src.writeFile(t, "secret.txt", marker)
	src.writeFile(t, "data/x.txt", "hello")
	digest, err := DehydrateTo(ctx, src, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The bucket objects must be ciphertext.
	for _, data := range obj.objects {
		if bytes.Contains(data, []byte(marker)) {
			t.Fatal("plaintext marker found in an S3 object; encryption at rest failed")
		}
	}

	dst := newFakeAgent(t)
	if err := HydrateFrom(ctx, dst, es, digest); err != nil {
		t.Fatal(err)
	}
	got := dst.listFiles(t)
	if got["secret.txt"] != marker || got["data/x.txt"] != "hello" {
		t.Fatalf("encrypted S3 round trip mismatch: %v", got)
	}
}

func TestS3MissingObjectIsNotFound(t *testing.T) {
	obj := newFakeObjectStore()
	if _, err := obj.GetObject(context.Background(), "absent"); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("want errObjectNotFound, got %v", err)
	}
}
