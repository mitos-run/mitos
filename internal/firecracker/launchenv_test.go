package firecracker

import (
	"os"
	"testing"
)

// TestLaunchEnv proves the live-fork env plumbing: with no extra entries the
// process inherits its environment unchanged (nil, the stock path), and with
// extra entries they are appended on top of the inherited environment so the
// FIRECRACKER_MITOS_* vars reach the parent VMM without dropping PATH etc.
func TestLaunchEnv(t *testing.T) {
	if got := launchEnv(nil); got != nil {
		t.Errorf("launchEnv(nil) must return nil (inherit unchanged); got %d entries", len(got))
	}
	if got := launchEnv([]string{}); got != nil {
		t.Errorf("launchEnv(empty) must return nil; got %d entries", len(got))
	}

	extra := []string{"FIRECRACKER_MITOS_SHARED_MEM=1", "FIRECRACKER_MITOS_WP_UDS=/run/wp.sock"}
	got := launchEnv(extra)
	if len(got) != len(os.Environ())+len(extra) {
		t.Fatalf("launchEnv should append extra to inherited env: got %d, want %d", len(got), len(os.Environ())+len(extra))
	}
	// The extra entries must be at the tail so they win over any inherited value.
	tail := got[len(got)-len(extra):]
	for i := range extra {
		if tail[i] != extra[i] {
			t.Errorf("appended env[%d] = %q, want %q", i, tail[i], extra[i])
		}
	}
}

// TestLaunchEnvOverridesInheritedDuplicate guards the CodeRabbit fix: a stale
// inherited FIRECRACKER_MITOS_* must NOT shadow the armed value. exec resolves
// duplicate names by the first occurrence, so launchEnv must drop the inherited
// key rather than append behind it.
func TestLaunchEnvOverridesInheritedDuplicate(t *testing.T) {
	t.Setenv("FIRECRACKER_MITOS_SHARED_MEM", "stale-inherited")
	extra := []string{"FIRECRACKER_MITOS_SHARED_MEM=armed"}
	got := launchEnv(extra)

	var seen int
	for _, kv := range got {
		if kv == "FIRECRACKER_MITOS_SHARED_MEM=armed" {
			seen++
		}
		if kv == "FIRECRACKER_MITOS_SHARED_MEM=stale-inherited" {
			t.Errorf("stale inherited value survived: %q", kv)
		}
	}
	if seen != 1 {
		t.Fatalf("armed FIRECRACKER_MITOS_SHARED_MEM must appear exactly once, got %d", seen)
	}
	// The armed value must be the last occurrence so exec resolves to it.
	if got[len(got)-1] != "FIRECRACKER_MITOS_SHARED_MEM=armed" {
		t.Errorf("armed value must be last (exec picks first, but there must be no earlier dup); tail = %q", got[len(got)-1])
	}
}
