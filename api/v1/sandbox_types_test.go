package v1

import (
	"reflect"
	"testing"
)

func TestSandboxExposeDeepCopy(t *testing.T) {
	original := &Sandbox{}
	original.Spec.Expose = &SandboxExpose{
		Port:    8080,
		Label:   "openclaw",
		Sharing: "private",
	}

	copied := original.DeepCopy()

	if !reflect.DeepEqual(original, copied) {
		t.Fatalf("DeepCopy result differs from original: got %+v, want %+v", copied, original)
	}

	copied.Spec.Expose.Label = "mutated"
	if original.Spec.Expose.Label == "mutated" {
		t.Fatal("mutating the copy changed the original: DeepCopy did not produce a true copy")
	}
}

// TestSandboxExposeDeepCopySliceFields verifies that the new slice fields on
// SandboxExpose (Network, AllowedPrincipals, AllowedEmailDomains) are deep-copied
// independently, so mutating the copy does not affect the original.
func TestSandboxExposeDeepCopySliceFields(t *testing.T) {
	original := &Sandbox{}
	original.Spec.Expose = &SandboxExpose{
		Port:                 8080,
		Label:                "openclaw",
		Sharing:              "private",
		Network:              []string{"10.0.0.0/8", "192.168.1.0/24"},
		ForwardAuthURL:       "https://auth.example.com/verify",
		AllowedPrincipals:    []string{"alice@example.com", "bob@example.com"},
		AllowedEmailDomains:  []string{"example.com"},
	}

	copied := original.DeepCopy()

	if !reflect.DeepEqual(original, copied) {
		t.Fatalf("DeepCopy result differs from original: got %+v, want %+v", copied, original)
	}

	// Mutate slices in copy; originals must be unchanged.
	copied.Spec.Expose.Network[0] = "172.16.0.0/12"
	if original.Spec.Expose.Network[0] == "172.16.0.0/12" {
		t.Fatal("mutating copy Network changed original: DeepCopy did not produce an independent slice")
	}

	copied.Spec.Expose.AllowedPrincipals[0] = "eve@evil.com"
	if original.Spec.Expose.AllowedPrincipals[0] == "eve@evil.com" {
		t.Fatal("mutating copy AllowedPrincipals changed original: not a deep copy")
	}

	copied.Spec.Expose.AllowedEmailDomains[0] = "evil.com"
	if original.Spec.Expose.AllowedEmailDomains[0] == "evil.com" {
		t.Fatal("mutating copy AllowedEmailDomains changed original: not a deep copy")
	}
}
