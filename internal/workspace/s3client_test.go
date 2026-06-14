package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memBucket is an httptest-backed S3-compatible endpoint that stores objects in
// memory, so the S3HTTPClient PUT/GET/HEAD path (URL building, SigV4 signing,
// status handling) is exercised end to end without a real bucket. It also
// records every Authorization header it saw so a test can assert the
// secret-access-key never appears on the wire.
type memBucket struct {
	mu       sync.Mutex
	objects  map[string][]byte
	authSeen []string
	bucket   string
}

func (m *memBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.authSeen = append(m.authSeen, r.Header.Get("Authorization"))
	m.mu.Unlock()

	// path-style: /<bucket>/<key>
	key := strings.TrimPrefix(r.URL.Path, "/"+m.bucket+"/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.objects[key] = body
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		m.mu.Lock()
		data, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "<Error>NoSuchKey</Error>", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodHead:
		m.mu.Lock()
		_, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestS3HTTPClientRoundTripAndSignature(t *testing.T) {
	mb := &memBucket{objects: map[string][]byte{}, bucket: "ws-bucket"}
	srv := httptest.NewServer(mb)
	defer srv.Close()

	const secret = "SUPER-SECRET-ACCESS-KEY-shouldnotleak"
	c, err := NewS3HTTPClient(S3HTTPConfig{
		Endpoint:  srv.URL,
		Bucket:    "ws-bucket",
		Region:    "us-east-1",
		Creds:     S3Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: secret},
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3HTTPClient: %v", err)
	}
	ctx := context.Background()

	// HEAD on an absent key reports false.
	if ok, err := c.HeadObject(ctx, "chunks/ab/abc"); err != nil || ok {
		t.Fatalf("HeadObject absent: ok=%v err=%v", ok, err)
	}
	// GET on an absent key is errObjectNotFound.
	if _, err := c.GetObject(ctx, "chunks/ab/abc"); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("GetObject absent: want errObjectNotFound, got %v", err)
	}

	payload := []byte("hello content-addressed world")
	if err := c.PutObject(ctx, "chunks/ab/abc", bytes.NewReader(payload)); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if ok, err := c.HeadObject(ctx, "chunks/ab/abc"); err != nil || !ok {
		t.Fatalf("HeadObject present: ok=%v err=%v", ok, err)
	}
	rc, err := c.GetObject(ctx, "chunks/ab/abc")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %q", got)
	}

	// The secret-access-key must never appear in any Authorization header.
	mb.mu.Lock()
	defer mb.mu.Unlock()
	for _, a := range mb.authSeen {
		if strings.Contains(a, secret) {
			t.Fatal("secret-access-key leaked into the Authorization header")
		}
		if a != "" && !strings.HasPrefix(a, "AWS4-HMAC-SHA256 ") {
			t.Fatalf("unexpected auth scheme: %q", a)
		}
	}
}

func TestS3StoreOverHTTPClientRoundTrip(t *testing.T) {
	// The S3Store composed over the real S3HTTPClient round-trips a dehydrate /
	// hydrate, proving the production wiring (not just the in-memory fake).
	mb := &memBucket{objects: map[string][]byte{}, bucket: "ws-bucket"}
	srv := httptest.NewServer(mb)
	defer srv.Close()

	c, err := NewS3HTTPClient(S3HTTPConfig{
		Endpoint:  srv.URL,
		Bucket:    "ws-bucket",
		Creds:     S3Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s3, err := NewS3Store(c, "wsX")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := newFakeAgent(t)
	src.writeFile(t, "main.go", "package main")
	src.writeFile(t, "sub/x.txt", "hello")
	digest, err := DehydrateTo(ctx, src, s3, nil, nil)
	if err != nil {
		t.Fatalf("DehydrateTo: %v", err)
	}
	dst := newFakeAgent(t)
	if err := HydrateFrom(ctx, dst, s3, digest); err != nil {
		t.Fatalf("HydrateFrom: %v", err)
	}
	got := dst.listFiles(t)
	if got["main.go"] != "package main" || got["sub/x.txt"] != "hello" {
		t.Fatalf("round trip mismatch: %v", got)
	}
}
