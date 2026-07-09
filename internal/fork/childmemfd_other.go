//go:build !linux

package fork

// ComposeChildFromImport is unavailable off Linux: the live-cow child-side memfd
// import needs a Linux memfd + /proc fd re-open + MAP_PRIVATE. Callers must fall
// back to the disk-snapshot restore (fail-closed). The signature matches the Linux
// build so the husk wiring compiles on any host.
func ComposeChildFromImport(_ ChildMemfdImport) ([]byte, error) {
	return nil, ErrLiveCowUnsupported
}
