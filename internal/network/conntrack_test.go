//go:build linux

package network

import (
	"context"
	"net"
	"os/exec"
	"testing"
)

// recordRunner captures every argv slice passed to it. Set failOn to make the
// runner return failErr / flushErr (and flushOutput) for any call whose argv
// contains failOn as a element.
type recordRunner struct {
	calls       [][]string
	failOn      string
	failErr     error
	flushCalls  [][]string
	flushOutput string
	flushErr    error
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

func (r *recordRunner) flush(_ context.Context, argv []string) (string, error) {
	r.flushCalls = append(r.flushCalls, argv)
	if r.failOn != "" {
		for _, a := range argv {
			if a == r.failOn {
				return r.flushOutput, r.flushErr
			}
		}
	}
	return r.flushOutput, nil
}

// flushed returns true if the runner was ever called via flush with exactly the
// given argv.
func (r *recordRunner) flushed(argv ...string) bool {
	for _, c := range r.flushCalls {
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
	return &linuxManager{run: rec.run, flush: rec.flush}
}

func TestFlushSourceBuildsConntrackDeleteByGuestIP(t *testing.T) {
	rec := &recordRunner{}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err != nil {
		t.Fatal(err)
	}
	if !rec.flushed("conntrack", "-D", "-s", "10.200.0.6") {
		t.Fatalf("conntrack delete not issued: %v", rec.flushCalls)
	}
}

// TestFlushSourceNoEntriesIsSuccess verifies that a nonzero exit from conntrack
// is treated as success when its output contains "flow entries have been deleted",
// which is the summary conntrack writes when zero entries matched.
func TestFlushSourceNoEntriesIsSuccess(t *testing.T) {
	exitOneErr := exec.Command("sh", "-c", "exit 1").Run()
	if exitOneErr == nil {
		t.Fatal("expected sh -c 'exit 1' to fail")
	}
	rec := &recordRunner{
		failOn:      "conntrack",
		flushOutput: "conntrack v1.4.6 (conntrack-tools): 0 flow entries have been deleted.",
		flushErr:    exitOneErr,
	}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err != nil {
		t.Fatalf("expected no-entries output to be treated as success, got: %v", err)
	}
}

// TestFlushSourcePermissionDeniedPropagates verifies that a nonzero exit whose
// output does NOT contain the deletion marker is returned as an error so that
// genuine failures (e.g. "Operation not permitted") are visible to the caller.
func TestFlushSourcePermissionDeniedPropagates(t *testing.T) {
	exitOneErr := exec.Command("sh", "-c", "exit 1").Run()
	if exitOneErr == nil {
		t.Fatal("expected sh -c 'exit 1' to fail")
	}
	rec := &recordRunner{
		failOn:      "conntrack",
		flushOutput: "Operation not permitted",
		flushErr:    exitOneErr,
	}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err == nil {
		t.Fatal("expected permission-denied output without deletion marker to propagate as error, got nil")
	}
}
