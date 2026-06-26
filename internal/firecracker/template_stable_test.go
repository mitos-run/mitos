package firecracker

import (
	"os"
	"path/filepath"
	"sync/atomic"
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
	p := filepath.Join(t.TempDir(), "mem")
	writeChunk0(t, p, 0x00)

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b byte
		for !stop.Load() {
			b++
			writeChunk0(t, p, b)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// While the writer churns, the file must not be declared stable.
	if err := waitForStableFile(p, 10*time.Millisecond, 200*time.Millisecond); err == nil {
		stop.Store(true)
		<-done
		t.Fatal("waitForStableFile returned stable while the file was still changing")
	}

	// Stop the writer; the file settles and waitForStableFile must now succeed.
	stop.Store(true)
	<-done
	if err := waitForStableFile(p, 10*time.Millisecond, 2*time.Second); err != nil {
		t.Fatalf("file did not settle after writes stopped: %v", err)
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
