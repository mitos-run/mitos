//go:build !linux

package fork

import (
	"fmt"

	"mitos.run/mitos/internal/cas"
)

// This file is the non-Linux counterpart of the userfaultfd prefetch skeleton
// (issue #167). userfaultfd is a Linux-only facility, so on darwin (and any
// other non-Linux host) the handler exists only to keep the package building and
// the restore-path caller compiling; every method returns a clear
// not-supported error. The capture-selection logic (SelectHotPages) and the
// manifest plumbing are platform-independent and live in hotpages.go, so they
// remain fully testable here.

// PrefetchHandler is the non-Linux stub of the userfaultfd handler. It holds
// only the trace field so CaptureTrace has the same shape as the Linux build.
type PrefetchHandler struct {
	pageSize int64
	capture  bool
	trace    []FaultRecord
}

// newPrefetchHandler always fails on non-Linux hosts: userfaultfd is Linux-only.
// The fields are still recorded so the stub carries the same shape as the Linux
// build for any caller that inspects the (nil) handler.
func newPrefetchHandler(memPath string, pageSize int64, capture bool) (*PrefetchHandler, error) {
	_ = &PrefetchHandler{pageSize: pageSize, capture: capture}
	return nil, fmt.Errorf("prefetch: userfaultfd is Linux-only; not available on this platform (memPath=%s pageSize=%d capture=%t)", memPath, pageSize, capture)
}

// Preload is unsupported off Linux.
func (h *PrefetchHandler) Preload(set cas.HotPageSet) error {
	_ = set
	return fmt.Errorf("prefetch: userfaultfd is Linux-only; Preload unavailable on this platform")
}

// Serve is unsupported off Linux.
func (h *PrefetchHandler) Serve() error {
	return fmt.Errorf("prefetch: userfaultfd is Linux-only; Serve unavailable on this platform")
}

// CaptureTrace returns the recorded trace (always empty off Linux).
func (h *PrefetchHandler) CaptureTrace() []FaultRecord {
	if h == nil {
		return nil
	}
	return h.trace
}

// Close is a no-op off Linux.
func (h *PrefetchHandler) Close() error { return nil }
