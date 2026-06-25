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
