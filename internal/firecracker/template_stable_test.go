package firecracker

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeChunk0 writes n bytes of value b at offset 0 so fileChunk0Digest sees a
// distinct chunk-0 content. A 4 MiB write covers the whole chunk the digest reads.
func writeChunk0(t *testing.T, path string, b byte) {
	t.Helper()
	buf := make([]byte, 4<<20)
	for i := range buf {
		buf[i] = b
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWaitForStableFileReturnsWhenSettled proves a file whose chunk 0 is not
// changing is reported stable promptly: two consecutive reads agree.
func TestWaitForStableFileReturnsWhenSettled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mem")
	writeChunk0(t, p, 0x11)
	start := time.Now()
	if err := waitForStableFile(p, 10*time.Millisecond, 2*time.Second); err != nil {
		t.Fatalf("stable file reported unstable: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a settled file took too long to confirm: %s", time.Since(start))
	}
}

// TestWaitForStableFileWaitsForChange proves that while chunk 0 keeps changing,
// waitForStableFile does NOT return (it must not record a mid-write digest), and
// returns only once the writes stop. This is the #461 race: forkd must not hash
// the mem until Firecracker's write has settled.
func TestWaitForStableFileWaitsForChange(t *testing.T) {
	// Drive the digest reader deterministically instead of racing a real writer
	// against the poll interval (a runnable writer can still be descheduled past a
	// whole interval under CI load, making the file briefly look stable and flaking
	// the assertion). The stub guarantees the property under test.
	orig := chunk0Digest
	t.Cleanup(func() { chunk0Digest = orig })

	// While the digest keeps changing on every read, it must never be declared
	// stable: it polls until the timeout and returns an error.
	var n uint64
	chunk0Digest = func(string) (string, error) {
		n++
		return strconv.FormatUint(n, 10), nil
	}
	if err := waitForStableFile("mem", time.Millisecond, 50*time.Millisecond); err == nil {
		t.Fatal("waitForStableFile returned stable while the digest was still changing")
	}

	// Once the digest is constant, two consecutive reads agree and it settles.
	chunk0Digest = func(string) (string, error) { return "stable", nil }
	if err := waitForStableFile("mem", time.Millisecond, 2*time.Second); err != nil {
		t.Fatalf("digest constant but waitForStableFile did not settle: %v", err)
	}
}

// TestWaitForStableFileTimesOutOnMissing proves a mem file that never appears
// surfaces a clear error rather than hanging or recording an empty digest.
func TestWaitForStableFileTimesOutOnMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "never")
	if err := waitForStableFile(p, 10*time.Millisecond, 100*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error for a missing file, got nil")
	}
}
