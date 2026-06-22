//go:build !linux

package fork

import (
	"fmt"

	"mitos.run/mitos/internal/cas"
)

// This is the non-Linux stub of the userfaultfd memory backend (issue #167).
// userfaultfd is a Linux-only facility, so on darwin (and any other non-Linux
// host) the handler exists only to keep the package building and the engine
// restore path compiling; every operation returns a clear not-supported error.
// The region/offset arithmetic (uffd.go) and the hot-page selection (hotpages.go)
// are platform-independent and remain fully testable here. The real engine never
// constructs this on a non-Linux host: NewEngine requires /dev/kvm, which only
// exists on Linux.

// uffdHandler is the non-Linux stub. It carries the trace field so CaptureTrace
// has the same shape as the Linux build.
type uffdHandler struct {
	trace []FaultRecord
}

func newUFFDHandler(sockPath, memPath string, capture bool) (*uffdHandler, error) {
	return nil, fmt.Errorf("uffd: userfaultfd is Linux-only; not available on this platform (sock=%s mem=%s capture=%t)", sockPath, memPath, capture)
}

func (h *uffdHandler) receive() error {
	return fmt.Errorf("uffd: userfaultfd is Linux-only; receive unavailable on this platform")
}

func (h *uffdHandler) Preload(set cas.HotPageSet) (int, error) {
	_ = set
	return 0, fmt.Errorf("uffd: userfaultfd is Linux-only; Preload unavailable on this platform")
}

func (h *uffdHandler) Serve() error {
	return fmt.Errorf("uffd: userfaultfd is Linux-only; Serve unavailable on this platform")
}

func (h *uffdHandler) CaptureTrace() []FaultRecord {
	if h == nil {
		return nil
	}
	return h.trace
}

func (h *uffdHandler) FaultCount() int64 { return 0 }

func (h *uffdHandler) Close() error { return nil }
