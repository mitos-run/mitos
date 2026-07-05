package fork

import "testing"

// TestMockPauseResumeRepeatedCycles exercises the pause/resume engine surface on
// the mock (issue #218): a sandbox can be paused and resumed repeatedly, the
// paused flag tracks the held state, and the per-sandbox cycle count records
// each pause. This is the mock-level behavior; the REAL memory+filesystem state
// preservation across N cycles (the E2B repeated-cycle bug we beat) is the
// KVM acceptance bar in TestEnginePauseResumePreservesStateKVM.
func TestMockPauseResumeRepeatedCycles(t *testing.T) {
	e := NewMockEngine()
	if err := e.CreateTemplate("py", "py", nil, nil, nil, nil, false, false); err != nil {
		t.Fatal(err)
	}
	e.ForkDelay = 0
	if _, err := e.Fork("py", "sb-1", ForkOpts{}); err != nil {
		t.Fatal(err)
	}

	const cycles = 5
	for i := 0; i < cycles; i++ {
		if err := e.Pause("sb-1"); err != nil {
			t.Fatalf("pause cycle %d: %v", i, err)
		}
		if !e.IsPaused("sb-1") {
			t.Fatalf("sandbox not paused after pause cycle %d", i)
		}
		if err := e.Resume("sb-1"); err != nil {
			t.Fatalf("resume cycle %d: %v", i, err)
		}
		if e.IsPaused("sb-1") {
			t.Fatalf("sandbox still paused after resume cycle %d", i)
		}
	}
	if got := e.PauseCycles("sb-1"); got != cycles {
		t.Fatalf("PauseCycles = %d, want %d (each pause must count)", got, cycles)
	}
}

// TestMockPauseUnknownSandbox asserts pausing or resuming a sandbox the engine
// does not hold is an error, not a silent success.
func TestMockPauseUnknownSandbox(t *testing.T) {
	e := NewMockEngine()
	if err := e.Pause("nope"); err == nil {
		t.Fatal("pause of an unknown sandbox must error")
	}
	if err := e.Resume("nope"); err == nil {
		t.Fatal("resume of an unknown sandbox must error")
	}
}
