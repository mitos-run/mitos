package daemon

import (
	"fmt"
	"regexp"
)

// sandboxIDPattern constrains every caller-supplied id (sandbox, snapshot,
// template) that forkd later embeds in host filesystem paths: workspace
// dirs, snapshot files, and the jailer chroot layout. No dots and no
// separators, so a validated id can never introduce a `..` segment or an
// extra path element; this is the gRPC-boundary half of the C1 traversal
// defense (the firecracker package independently refuses escaping paths).
var sandboxIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// validateSandboxID rejects ids that fail sandboxIDPattern before any
// engine or filesystem operation sees them.
func validateSandboxID(s string) error {
	if !sandboxIDPattern.MatchString(s) {
		return fmt.Errorf("invalid id %q: ids must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes); use a plain identifier such as sb-1234", s)
	}
	return nil
}

// validateVolumeName applies the same path-safety guard as validateSandboxID to
// a volume name. Volume names flow from the CRD through the Fork/CreateTemplate
// RPCs into filepath.Join (the host backing path) and the Firecracker drive id,
// so a name like `../../etc/x` would otherwise escape the sandbox volumes dir.
// The pattern forbids dots and separators, so a validated name can never
// introduce a `..` segment or an extra path element.
func validateVolumeName(name string) error {
	if !sandboxIDPattern.MatchString(name) {
		return fmt.Errorf("invalid volume name %q: names must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", name)
	}
	return nil
}
