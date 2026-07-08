//go:build !linux

package fork

// StartChildUFFDHandler is unavailable off Linux: the lazy child-side UFFD import
// needs userfaultfd + memfd + /proc fd re-open, all Linux-only. Callers must fall
// back to the disk-snapshot restore (fail-closed). The signature matches the Linux
// build so the husk wiring compiles on any host.
func StartChildUFFDHandler(_ string, _ ChildMemfdImport) (ChildUFFDHandle, error) {
	return nil, ErrChildUFFDUnsupported
}
