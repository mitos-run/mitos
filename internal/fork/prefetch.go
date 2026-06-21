package fork

import (
	"fmt"

	"mitos.run/mitos/internal/cas"
)

// PrefetchConfig parameterizes a capture or prefetch resume (issue #167): the
// guest memory file the userfaultfd handler registers over, the page
// granularity (2 MiB for hugepage-backed memory), and the selection cap applied
// when a capture trace is reduced to a hot-page set.
type PrefetchConfig struct {
	// MemPath is the guest memory file path the handler registers userfaultfd
	// over.
	MemPath string
	// File is the manifest file name the captured offsets index into
	// (conventionally "mem").
	File string
	// PageSizeBytes is the prefetch unit. 2 MiB for hugepage-backed memory.
	PageSizeBytes int64
	// Cap bounds the captured set to the Cap hottest pages; zero means no cap.
	Cap int
}

// CaptureHotPages runs a CAPTURE resume against cfg.MemPath, records the faults
// the userfaultfd handler services, and reduces them to the hot-page set to
// stamp onto the snapshot manifest. It is the single platform-neutral seam that
// ties the (Linux-gated) handler to the (pure) SelectHotPages selection, so the
// rest of the engine and the bench driver depend only on this signature.
//
// It is honestly gated: on a non-Linux host newPrefetchHandler fails because
// userfaultfd is Linux-only, and on a Linux host the handler's syscall wiring is
// the bare-metal follow-up, so this returns an actionable error rather than a
// fabricated trace until that wiring lands. The selection it would call
// (SelectHotPages) is already pure and unit-tested, so once the handler reports
// real faults this function needs no further logic.
func CaptureHotPages(cfg PrefetchConfig) (cas.HotPageSet, error) {
	h, err := newPrefetchHandler(cfg.MemPath, cfg.PageSizeBytes, true)
	if err != nil {
		return cas.HotPageSet{}, fmt.Errorf("capture hot pages: %w", err)
	}
	defer func() { _ = h.Close() }()

	if err := h.Serve(); err != nil {
		return cas.HotPageSet{}, fmt.Errorf("capture hot pages: serve userfaultfd: %w", err)
	}

	trace := h.CaptureTrace()
	return SelectHotPages(trace, HotPageSelection{
		PageSizeBytes: cfg.PageSizeBytes,
		File:          cfg.File,
		Cap:           cfg.Cap,
	}), nil
}

// PreloadHotPages registers a userfaultfd handler over cfg.MemPath and preloads
// the manifest's captured hot-page set before the VM is resumed, paying the
// lazy-fault tail up front. The returned handler must be Served for the life of
// the VM (to serve the pages NOT preloaded) and Closed on teardown.
//
// Like CaptureHotPages it is honestly gated: the handler is Linux-only and its
// syscall wiring is the bare-metal follow-up, so this returns an actionable
// error until that lands. The set is consumed in the ascending order
// SelectHotPages emits, which is the sequential prefetch order the memory file
// backing store prefers.
func PreloadHotPages(cfg PrefetchConfig, set cas.HotPageSet) (*PrefetchHandler, error) {
	h, err := newPrefetchHandler(cfg.MemPath, cfg.PageSizeBytes, false)
	if err != nil {
		return nil, fmt.Errorf("preload hot pages: %w", err)
	}
	if err := h.Preload(set); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("preload hot pages: %w", err)
	}
	return h, nil
}
