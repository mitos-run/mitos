package fork

import "testing"

// TestSafeSandboxIDComponent locks in the path-traversal guard used before a
// sandbox id is joined into a host path (issue #218 pause checkpoint dir). An id
// that is not a single safe path segment must be rejected so it can never escape
// the data dir.
func TestSafeSandboxIDComponent(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"sb-1234", true},
		{"abcDEF_09", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../escape", false},
		{"a/b", false},
		{`a\b`, false},
		{"/abs", false},
		{"../../etc/passwd", false},
		{"sb/../../x", false},
	}
	for _, tc := range cases {
		if got := safeSandboxIDComponent(tc.id); got != tc.want {
			t.Errorf("safeSandboxIDComponent(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
