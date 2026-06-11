package firecracker

import (
	"strings"
	"testing"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// fakeExec records the commands it is asked to run and returns a scripted
// response per command, standing in for the guest-agent vsock exec so the
// init-command safety logic can be tested without booting Firecracker.
type fakeExec struct {
	ran      []string
	response map[string]*vsock.ExecResponse
	err      error
}

func (f *fakeExec) exec(command string) (*vsock.ExecResponse, error) {
	f.ran = append(f.ran, command)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.response[command]; ok {
		return r, nil
	}
	return &vsock.ExecResponse{ExitCode: 0}, nil
}

func TestRunInitCommands_AllSucceed(t *testing.T) {
	f := &fakeExec{}
	cmds := []string{"echo a", "pip install flask"}
	if err := runInitCommands(f.exec, cmds); err != nil {
		t.Fatalf("runInitCommands: unexpected error: %v", err)
	}
	if len(f.ran) != 2 || f.ran[0] != "echo a" || f.ran[1] != "pip install flask" {
		t.Errorf("commands ran in wrong order or count: %v", f.ran)
	}
}

func TestRunInitCommands_NonzeroExitFails(t *testing.T) {
	f := &fakeExec{response: map[string]*vsock.ExecResponse{
		"pip install nope": {ExitCode: 1, Stderr: "No matching distribution found"},
	}}
	cmds := []string{"echo a", "pip install nope", "echo never"}
	err := runInitCommands(f.exec, cmds)
	if err == nil {
		t.Fatal("expected error when an init command exits nonzero, got nil")
	}
	// The error must name the failing command and carry its stderr so the
	// operator can see why the template build was aborted.
	if !strings.Contains(err.Error(), "pip install nope") {
		t.Errorf("error missing failing command: %v", err)
	}
	if !strings.Contains(err.Error(), "No matching distribution found") {
		t.Errorf("error missing stderr: %v", err)
	}
	// Execution must stop at the first failure: "echo never" must not run.
	if len(f.ran) != 2 {
		t.Errorf("expected exactly 2 commands run before abort, got %v", f.ran)
	}
}

func TestRunInitCommands_TransportErrorFails(t *testing.T) {
	f := &fakeExec{err: strErr("connection closed")}
	err := runInitCommands(f.exec, []string{"echo a"})
	if err == nil {
		t.Fatal("expected error on transport failure")
	}
	if !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("error missing transport cause: %v", err)
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }
