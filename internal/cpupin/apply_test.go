package cpupin

import "testing"

// TestNewApplierNonNil proves a usable applier is always returned (the Linux one
// on Linux, the no-op stub elsewhere), so callers never nil-check.
func TestNewApplierNonNil(t *testing.T) {
	if NewApplier() == nil {
		t.Fatal("NewApplier returned nil")
	}
}

// TestPinRequestEmptyCPUs proves applying an empty pin set is rejected, so the
// applier never issues a sched_setaffinity with a zero mask (which would be a
// kernel error and, worse, an unconstrained affinity).
func TestPinRequestValidate(t *testing.T) {
	cases := []struct {
		name string
		req  PinRequest
		ok   bool
	}{
		{"valid", PinRequest{ThreadIDs: []int{100}, CPUs: []int{0, 4}}, true},
		{"no threads", PinRequest{CPUs: []int{0}}, false},
		{"no cpus", PinRequest{ThreadIDs: []int{100}}, false},
	}
	for _, tc := range cases {
		err := tc.req.validate()
		if tc.ok && err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: expected validation error", tc.name)
		}
	}
}
