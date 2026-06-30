//go:build linux

package network

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
)

// recordRunner captures every argv slice passed to it. Set failErr to make the
// runner return that error for any call whose argv contains failOn as a
// substring of the joined args.
type recordRunner struct {
	calls  [][]string
	failOn string
	failErr error
}

func (r *recordRunner) run(_ context.Context, argv []string, _ string) error {
	r.calls = append(r.calls, argv)
	if r.failOn != "" {
		for _, a := range argv {
			if a == r.failOn {
				return r.failErr
			}
		}
	}
	return nil
}

// ran returns true if the runner was ever called with exactly the given argv.
func (r *recordRunner) ran(argv ...string) bool {
	for _, c := range r.calls {
		if len(c) != len(argv) {
			continue
		}
		match := true
		for i, a := range argv {
			if c[i] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// newLinuxManagerForTest builds a linuxManager wired to the given recordRunner
// so unit tests can exercise FlushSource without root or a real conntrack binary.
func newLinuxManagerForTest(rec *recordRunner) *linuxManager {
	return &linuxManager{run: rec.run}
}

func TestFlushSourceBuildsConntrackDeleteByGuestIP(t *testing.T) {
	rec := &recordRunner{}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err != nil {
		t.Fatal(err)
	}
	if !rec.ran("conntrack", "-D", "-s", "10.200.0.6") {
		t.Fatalf("conntrack delete not issued: %v", rec.calls)
	}
}

// TestFlushSourceNoEntriesIsSuccess verifies that exit code 1 from conntrack
// (its "0 flow entries have been deleted" outcome) is treated as success so a
// flush on a child that never opened a proxied flow does not fail the fork path.
func TestFlushSourceNoEntriesIsSuccess(t *testing.T) {
	// Build a real *exec.ExitError with code 1, then wrap it exactly as
	// execRunner does so errors.As inside FlushSource can find it.
	exitOneErr := exec.Command("sh", "-c", "exit 1").Run()
	if exitOneErr == nil {
		t.Fatal("expected sh -c 'exit 1' to fail")
	}
	rec := &recordRunner{
		failOn:  "conntrack",
		failErr: fmt.Errorf("conntrack: %w", exitOneErr),
	}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err != nil {
		t.Fatalf("expected no-entries exit code 1 to be treated as success, got: %v", err)
	}
}

// TestFlushSourceRealErrorPropagates verifies that a genuine conntrack failure
// (e.g. conntrack binary missing) is returned to the caller as an error.
func TestFlushSourceRealErrorPropagates(t *testing.T) {
	// exit 2 is not the "no entries" code; it should propagate as an error.
	exitTwoErr := exec.Command("sh", "-c", "exit 2").Run()
	if exitTwoErr == nil {
		t.Fatal("expected sh -c 'exit 2' to fail")
	}
	rec := &recordRunner{
		failOn:  "conntrack",
		failErr: fmt.Errorf("conntrack: %w", exitTwoErr),
	}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err == nil {
		t.Fatal("expected error from exit code 2, got nil")
	}
}
