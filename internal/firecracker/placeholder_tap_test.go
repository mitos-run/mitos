package firecracker

import "testing"

// TestPlaceholderTapNameForIsPerTemplateAndFitsIFNAMSIZ pins the fix for the outage:
// every template build used to bind one shared "sbtap-template", so a build racing
// itself or another template failed with ioctl(TUNSETIFF): Device or resource busy, the
// pool reported BuildFailed, and a rebuilding pool serves no warm husk pods, so creates
// went Pending until it stopped flapping.
func TestPlaceholderTapNameForIsPerTemplateAndFitsIFNAMSIZ(t *testing.T) {
	const maxIfName = 15 // IFNAMSIZ is 16 including the NUL

	seen := map[string]string{}
	for _, id := range []string{"python", "node", "browser", "python2", "a", "a-very-long-template-id-that-is-64-chars-long-aaaaaaaaaaaaaaaaaaaa"} {
		name := PlaceholderTapNameFor(id)
		if len(name) > maxIfName {
			t.Errorf("tap name for %q is %d chars (%q), the kernel caps an interface name at %d", id, len(name), name, maxIfName)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("template %q and %q derive the SAME tap %q; concurrent builds would collide", id, prev, name)
		}
		seen[name] = id
		if name != PlaceholderTapNameFor(id) {
			t.Errorf("tap name for %q is not deterministic", id)
		}
	}
}
