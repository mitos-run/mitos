package fork

import (
	"strings"
	"testing"
)

// TestForkRejectsUnsafeIDs proves Fork validates the snapshot and sandbox ids
// before they are joined into host filesystem paths. The forkd gRPC boundary
// validates ids, but the standalone sandbox-server REST handler does not, so the
// engine entry point is the chokepoint both callers traverse. Without this guard
// a crafted id (CodeQL go/path-injection) could escape the data dir.
func TestForkRejectsUnsafeIDs(t *testing.T) {
	e := newGateEngine(t, t.TempDir(), false)
	cases := []struct {
		name              string
		snapshot, sandbox string
	}{
		{"snapshot traversal", "../../etc", "sb-1"},
		{"sandbox traversal", "tmpl", "../../etc"},
		{"snapshot slash", "a/b", "sb-1"},
		{"sandbox slash", "tmpl", "a/b"},
		{"snapshot dotdot", "..", "sb-1"},
		{"sandbox backslash", "tmpl", `a\b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Fork(tc.snapshot, tc.sandbox, ForkOpts{})
			if err == nil {
				t.Fatalf("Fork(%q, %q) = nil error, want rejection", tc.snapshot, tc.sandbox)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("Fork(%q, %q) error = %q, want an invalid-id rejection", tc.snapshot, tc.sandbox, err)
			}
		})
	}
}

// TestCreateTemplateRejectsUnsafeID proves CreateTemplate validates the template
// id before it becomes a host path component (templates/<id>/...).
func TestCreateTemplateRejectsUnsafeID(t *testing.T) {
	e := newGateEngine(t, t.TempDir(), false)
	for _, id := range []string{"../../etc", "a/b", "..", ".", `a\b`} {
		err := e.CreateTemplate(id, "img", nil, nil, nil, nil, false, false)
		if err == nil {
			t.Fatalf("CreateTemplate(%q) = nil error, want rejection", id)
		}
		if !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("CreateTemplate(%q) error = %q, want an invalid-id rejection", id, err)
		}
	}
}

// TestTerminateRejectsUnsafeID proves Terminate validates the sandbox id before
// it is joined into the RemoveAll path, matching the Pause guard.
func TestTerminateRejectsUnsafeID(t *testing.T) {
	e := newGateEngine(t, t.TempDir(), false)
	e.sandboxes = make(map[string]*Sandbox)
	for _, id := range []string{"../../etc", "a/b", ".."} {
		err := e.Terminate(id)
		if err == nil {
			t.Fatalf("Terminate(%q) = nil error, want rejection", id)
		}
		if !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("Terminate(%q) error = %q, want an invalid-id rejection", id, err)
		}
	}
}
