package agentcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSandboxLsJSONShape asserts `sandbox ls -o json` emits the documented
// stable shape: a top-level object with a "sandboxes" array of typed rows.
func TestSandboxLsJSONShape(t *testing.T) {
	fb := NewFakeBackend()
	fb.ListInfos = []SandboxInfo{
		{Name: "sbx-1", Pool: "python", Phase: "Ready", Node: "node-a", Endpoint: "10.0.0.1:9091", Age: 90 * time.Second},
	}
	for _, form := range [][]string{
		{"sandbox", "ls", "-o", "json"},
		{"sandbox", "ls", "--json"},
		{"sandbox", "ls", "-o=json"},
		{"sandbox", "ls", "--output", "json"},
	} {
		code, out, _ := runCLI(t, fb, form...)
		if code != ExitOK {
			t.Fatalf("%v: exit code = %d, want %d", form, code, ExitOK)
		}
		var got struct {
			Sandboxes []struct {
				Name       string `json:"name"`
				Pool       string `json:"pool"`
				Phase      string `json:"phase"`
				Node       string `json:"node"`
				Endpoint   string `json:"endpoint"`
				AgeSeconds int    `json:"ageSeconds"`
			} `json:"sandboxes"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("%v: output is not valid JSON: %v\noutput=%q", form, err, out.String())
		}
		if len(got.Sandboxes) != 1 {
			t.Fatalf("%v: got %d sandboxes, want 1", form, len(got.Sandboxes))
		}
		s := got.Sandboxes[0]
		if s.Name != "sbx-1" || s.Pool != "python" || s.Phase != "Ready" || s.Node != "node-a" || s.Endpoint != "10.0.0.1:9091" || s.AgeSeconds != 90 {
			t.Fatalf("%v: row = %+v, want the injected values with ageSeconds=90", form, s)
		}
	}
}

// TestSandboxLsJSONEmptyIsArray asserts an empty listing emits an empty JSON
// array, never null, so a consumer can iterate unconditionally.
func TestSandboxLsJSONEmptyIsArray(t *testing.T) {
	fb := NewFakeBackend()
	fb.ListInfos = nil
	code, out, _ := runCLI(t, fb, "sandbox", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), `"sandboxes": []`) {
		t.Fatalf("empty ls json = %q, want an empty sandboxes array", out.String())
	}
}

// TestOutputFormatUnknownIsUsageError asserts an unrecognized output format is a
// usage error (exit 2), not a silent human render.
func TestOutputFormatUnknownIsUsageError(t *testing.T) {
	fb := NewFakeBackend()
	code, _, errw := runCLI(t, fb, "sandbox", "ls", "-o", "yaml")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d for unknown format", code, ExitUsage)
	}
	if errw.Len() == 0 {
		t.Fatalf("want a diagnostic on stderr for an unknown format")
	}
}

// TestWorkspaceLsAndLogJSON asserts the workspace list and log read verbs also
// honor -o json with documented shapes.
func TestWorkspaceLsAndLogJSON(t *testing.T) {
	fb := NewFakeBackend()
	ws := fb.Workspace().(*FakeWorkspaceBackend)
	ws.Workspaces = []WorkspaceInfo{{Name: "w1", Head: "rev-2", Revisions: 2, Resumable: true}}
	ws.Revisions = []RevisionInfo{{Name: "rev-2", Phase: "Committed", Resumable: true, Lineage: "root"}}

	code, out, _ := runCLI(t, fb, "ws", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ws ls json: exit code = %d, want %d", code, ExitOK)
	}
	var wl struct {
		Workspaces []struct {
			Name      string `json:"name"`
			Head      string `json:"head"`
			Revisions int    `json:"revisions"`
			Resumable bool   `json:"resumable"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(out.Bytes(), &wl); err != nil {
		t.Fatalf("ws ls json invalid: %v\n%q", err, out.String())
	}
	if len(wl.Workspaces) != 1 || wl.Workspaces[0].Name != "w1" || wl.Workspaces[0].Revisions != 2 {
		t.Fatalf("ws ls json = %+v, want one w1 with 2 revisions", wl.Workspaces)
	}

	code, out, _ = runCLI(t, fb, "ws", "log", "w1", "--json")
	if code != ExitOK {
		t.Fatalf("ws log json: exit code = %d, want %d", code, ExitOK)
	}
	var rl struct {
		Revisions []struct {
			Name    string `json:"name"`
			Phase   string `json:"phase"`
			Lineage string `json:"lineage"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(out.Bytes(), &rl); err != nil {
		t.Fatalf("ws log json invalid: %v\n%q", err, out.String())
	}
	if len(rl.Revisions) != 1 || rl.Revisions[0].Name != "rev-2" || rl.Revisions[0].Lineage != "root" {
		t.Fatalf("ws log json = %+v, want one rev-2 root revision", rl.Revisions)
	}
}

// TestExitCodeNotFound asserts a not-found backend error maps to the documented
// not-found exit code, distinct from a generic runtime error.
func TestExitCodeNotFound(t *testing.T) {
	fb := NewFakeBackend()
	fb.Errors["terminate"] = ErrNotFound
	code, _, errw := runCLI(t, fb, "sandbox", "terminate", "sbx-missing")
	if code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d (not found)", code, ExitNotFound)
	}
	if errw.Len() == 0 {
		t.Fatalf("want a diagnostic on stderr")
	}
}

// TestExitCodeTimeout asserts a deadline-exceeded error maps to the documented
// timeout exit code so an agent can distinguish a slow op from a hard failure.
func TestExitCodeTimeout(t *testing.T) {
	fb := NewFakeBackend()
	fb.Errors["create"] = context.DeadlineExceeded
	code, _, _ := runCLI(t, fb, "sandbox", "create", "--pool", "p")
	if code != ExitTimeout {
		t.Fatalf("exit code = %d, want %d (timeout)", code, ExitTimeout)
	}
}

// TestCreateTimeoutFlagSetsDeadline asserts --timeout wires a context deadline
// into the backend call.
func TestCreateTimeoutFlagSetsDeadline(t *testing.T) {
	fb := NewFakeBackend()
	code, _, _ := runCLI(t, fb, "sandbox", "create", "--pool", "p", "--timeout", "5")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	calls := fb.RecordedCalls()
	if len(calls) != 1 || calls[0].Method != "create" {
		t.Fatalf("calls = %v, want a single create", calls)
	}
	if !calls[0].HasDeadline {
		t.Fatalf("create call had no context deadline; --timeout was not wired")
	}
}

// TestCreateNoWaitFlag asserts --no-wait (and --wait=false) thread the no-wait
// signal to the backend.
func TestCreateNoWaitFlag(t *testing.T) {
	for _, args := range [][]string{
		{"sandbox", "create", "--pool", "p", "--no-wait"},
		{"sandbox", "create", "--pool", "p", "--wait=false"},
	} {
		fb := NewFakeBackend()
		code, _, _ := runCLI(t, fb, args...)
		if code != ExitOK {
			t.Fatalf("%v: exit code = %d, want %d", args, code, ExitOK)
		}
		calls := fb.RecordedCalls()
		if len(calls) != 1 || !calls[0].NoWait {
			t.Fatalf("%v: create call NoWait = %v, want true", args, calls[0].NoWait)
		}
	}
}

// TestCreateWaitsByDefault asserts the default is to wait: no no-wait signal is
// threaded when neither flag is given.
func TestCreateWaitsByDefault(t *testing.T) {
	fb := NewFakeBackend()
	code, _, _ := runCLI(t, fb, "sandbox", "create", "--pool", "p")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if fb.RecordedCalls()[0].NoWait {
		t.Fatalf("default create threaded a no-wait signal; want wait by default")
	}
}

// TestForkTimeoutAndWaitFlags asserts fork accepts the same --wait/--timeout
// contract as create.
func TestForkTimeoutAndWaitFlags(t *testing.T) {
	fb := NewFakeBackend()
	fb.ForkIDs = []string{"f1", "f2"}
	code, _, _ := runCLI(t, fb, "fork", "sbx-1", "--count", "2", "--timeout", "10", "--no-wait")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	calls := fb.RecordedCalls()
	if calls[0].Method != "fork" || calls[0].Replicas != 2 {
		t.Fatalf("calls = %v, want fork x2", calls)
	}
	if !calls[0].HasDeadline || !calls[0].NoWait {
		t.Fatalf("fork call HasDeadline=%v NoWait=%v, want both true", calls[0].HasDeadline, calls[0].NoWait)
	}
}

// TestForkIDAfterFlags asserts the sandbox id parses correctly when it follows
// value-taking flags (e.g. --count 2 sbx-1). A naive single flag.Parse would
// mis-read the flag value "2" as the id.
func TestForkIDAfterFlags(t *testing.T) {
	for _, args := range [][]string{
		{"fork", "--count", "2", "--timeout", "10", "sbx-1"},
		{"fork", "sbx-1", "--count", "2"},
		{"fork", "--count", "2", "sbx-1"},
	} {
		fb := NewFakeBackend()
		fb.ForkIDs = []string{"f1", "f2"}
		code, _, errw := runCLI(t, fb, args...)
		if code != ExitOK {
			t.Fatalf("%v: exit code = %d (%s), want %d", args, code, errw.String(), ExitOK)
		}
		calls := fb.RecordedCalls()
		if len(calls) != 1 || calls[0].Method != "fork" || calls[0].SandboxID != "sbx-1" || calls[0].Replicas != 2 {
			t.Fatalf("%v: call = %+v, want fork sbx-1 x2", args, calls)
		}
	}
}

// TestTerminateIDAfterFlags asserts terminate parses the id after --timeout.
func TestTerminateIDAfterFlags(t *testing.T) {
	fb := NewFakeBackend()
	code, _, errw := runCLI(t, fb, "sandbox", "terminate", "--timeout", "5", "sbx-9")
	if code != ExitOK {
		t.Fatalf("exit code = %d (%s), want %d", code, errw.String(), ExitOK)
	}
	calls := fb.RecordedCalls()
	if len(calls) != 1 || calls[0].SandboxID != "sbx-9" || !calls[0].HasDeadline {
		t.Fatalf("call = %+v, want terminate sbx-9 with deadline", calls)
	}
}

// TestOutputFormatLastFlagWins asserts a later human format overrides an earlier
// --json (last flag wins), so an explicit -o table after --json renders human.
func TestOutputFormatLastFlagWins(t *testing.T) {
	fb := NewFakeBackend()
	fb.ListInfos = []SandboxInfo{{Name: "sbx-1", Pool: "python", Phase: "Ready"}}
	code, out, _ := runCLI(t, fb, "sandbox", "ls", "--json", "-o", "table")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if strings.Contains(out.String(), `"sandboxes"`) {
		t.Fatalf("got JSON despite a trailing -o table (last flag should win): %q", out.String())
	}
}

// TestWsLogNotFoundExitCode asserts a not-found workspace maps to ExitNotFound
// on the ws log path.
func TestWsLogNotFoundExitCode(t *testing.T) {
	fb := NewFakeBackend()
	ws := fb.Workspace().(*FakeWorkspaceBackend)
	ws.Errors["ws_log"] = ErrNotFound
	code, _, errw := runCLI(t, fb, "ws", "log", "ghost")
	if code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d (not found)", code, ExitNotFound)
	}
	if errw.Len() == 0 {
		t.Fatalf("want a diagnostic on stderr")
	}
}
