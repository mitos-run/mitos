//go:build !linux

package fork

// StartWPForkHandler is unavailable off Linux: the live copy-on-write fork engine
// needs userfaultfd write-protect, a Linux-only mechanism. Callers must fall back
// to the disk-snapshot restore path (fail-closed). The signature matches the
// Linux build so the husk wiring compiles on any host.
func StartWPForkHandler(_ WPForkConfig) (WPForkHandle, error) {
	return nil, ErrLiveCowUnsupported
}
