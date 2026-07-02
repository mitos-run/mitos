package saas

import (
	"regexp"
	"testing"
)

// Org ids are stamped on Sandbox objects as a Kubernetes label value and, in
// org-tenancy mode, embedded in the org namespace name. Both are stricter than
// base64url: a label value must begin and end alphanumeric, and an RFC1123
// name segment must be lowercase alphanumerics and hyphens only. Ids must
// satisfy the strictest consumer so no org can be born unable to create a
// sandbox (#593).
func TestRandomIDIsLabelValueAndRFC1123Safe(t *testing.T) {
	rfc1123 := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	for i := 0; i < 4096; i++ {
		id := randomID()
		if len(id) < 16 {
			t.Fatalf("randomID() = %q: too short to stay collision resistant", id)
		}
		if !rfc1123.MatchString(id) {
			t.Fatalf("randomID() = %q: not a valid k8s label value / RFC1123 name segment", id)
		}
	}
}
