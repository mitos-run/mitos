package fork

import (
	"strings"
	"testing"

	"mitos.run/mitos/internal/cas"
)

// TestCaptureHotPagesGatedNotWired proves the capture seam never fabricates a
// trace: until the userfaultfd handler's syscall wiring lands (and on any
// non-Linux host where userfaultfd does not exist at all), CaptureHotPages
// returns an actionable error rather than an empty-but-plausible hot-page set
// that would read as a real capture.
func TestCaptureHotPagesGatedNotWired(t *testing.T) {
	_, err := CaptureHotPages(PrefetchConfig{
		MemPath:       "/nonexistent/mem",
		File:          "mem",
		PageSizeBytes: 2 << 20,
	})
	if err == nil {
		t.Fatal("CaptureHotPages returned nil error; the handler is not wired, so it must fail loudly")
	}
	// The error must be honest about why no capture happened: either the handler
	// is not yet wired (Linux build) or userfaultfd is Linux-only (other builds).
	msg := err.Error()
	if !strings.Contains(msg, "#167") && !strings.Contains(msg, "Linux-only") {
		t.Fatalf("error should explain the gating (issue #167 or Linux-only), got: %v", err)
	}
}

// TestPreloadHotPagesGatedNotWired proves the preload seam is gated the same
// way: it must not silently no-op (which would look like prefetch happened) but
// surface that the handler is unavailable.
func TestPreloadHotPagesGatedNotWired(t *testing.T) {
	set := cas.HotPageSet{PageSizeBytes: 2 << 20, File: "mem", Offsets: []int64{0, 2 << 20}}
	_, err := PreloadHotPages(PrefetchConfig{
		MemPath:       "/nonexistent/mem",
		File:          "mem",
		PageSizeBytes: 2 << 20,
	}, set)
	if err == nil {
		t.Fatal("PreloadHotPages returned nil error; the handler is not wired, so it must fail loudly")
	}
}

// TestCaptureHotPagesRejectsBadPageSize proves a non-positive page size is
// rejected before any handler work: the page size is the unit every offset is
// floored to, so zero would be a divide-by-zero in selection.
func TestCaptureHotPagesRejectsBadPageSize(t *testing.T) {
	if _, err := CaptureHotPages(PrefetchConfig{MemPath: "/x", File: "mem", PageSizeBytes: 0}); err == nil {
		t.Fatal("expected error for zero page size")
	}
}
