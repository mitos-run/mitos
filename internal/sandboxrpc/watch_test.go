package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// watchGuest extends fakeGuest for Watch, Processes, and Signal operations.
// It holds scripted FsEvents, a scripted ProcessList, and records Signal args.
type watchGuest struct {
	fakeGuest

	// Watch: recorded path and scripted events.
	gotWatchPath string
	watchEvents  []*sandboxv1.FsEvent
	watchOpenErr error

	// Processes: scripted result.
	processList *sandboxv1.ProcessList
	processErr  error

	// Signal: recorded args and scripted error.
	gotSignalPid    int32
	gotSignalSignal int32
	signalErr       error
}

// fakeWatchStream backs watchGuest.Watch.
type fakeWatchStream struct {
	events []*sandboxv1.FsEvent
	pos    int
}

func (s *fakeWatchStream) Recv() (*sandboxv1.FsEvent, error) {
	if s.pos >= len(s.events) {
		return nil, io.EOF
	}
	e := s.events[s.pos]
	s.pos++
	return e, nil
}

func (s *fakeWatchStream) Close() error { return nil }

func (g *watchGuest) Watch(_ context.Context, path string) (WatchStream, error) {
	g.gotWatchPath = path
	if g.watchOpenErr != nil {
		return nil, g.watchOpenErr
	}
	return &fakeWatchStream{events: g.watchEvents}, nil
}

func (g *watchGuest) Processes(_ context.Context) (*sandboxv1.ProcessList, error) {
	return g.processList, g.processErr
}

func (g *watchGuest) Signal(_ context.Context, pid int32, signal int32) error {
	g.gotSignalPid = pid
	g.gotSignalSignal = signal
	return g.signalErr
}

// newWatchTestServer builds a Service wired with the watchGuest and returns
// the Connect client.
func newWatchTestServer(t *testing.T, g *watchGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// TestWatchForwardsCreateFsEvent is the Task 2.6 Watch acceptance test: the
// fake guest emits a CREATE FsEvent and the Service forwards it intact.
func TestWatchForwardsCreateFsEvent(t *testing.T) {
	g := &watchGuest{
		watchEvents: []*sandboxv1.FsEvent{
			{Kind: sandboxv1.FsEvent_CREATE, Path: "/workspace/new.txt"},
		},
	}
	client := newWatchTestServer(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wstream, err := client.Watch(ctx, connect.NewRequest(&sandboxv1.WatchRequest{
		Path:      "/workspace",
		Recursive: true,
	}))
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer wstream.Close()

	var got []*sandboxv1.FsEvent
	for wstream.Receive() {
		got = append(got, wstream.Msg())
	}
	if serr := wstream.Err(); serr != nil {
		t.Fatalf("stream error: %v", serr)
	}

	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].GetKind() != sandboxv1.FsEvent_CREATE {
		t.Fatalf("event kind = %v, want CREATE", got[0].GetKind())
	}
	if got[0].GetPath() != "/workspace/new.txt" {
		t.Fatalf("event path = %q, want /workspace/new.txt", got[0].GetPath())
	}
	if g.gotWatchPath != "/workspace" {
		t.Fatalf("watch path = %q, want /workspace", g.gotWatchPath)
	}
}

// TestProcessesReturnsFakeList is the Task 2.6 Processes acceptance test:
// Processes returns a fake ProcessList with the scripted entries.
func TestProcessesReturnsFakeList(t *testing.T) {
	g := &watchGuest{
		processList: &sandboxv1.ProcessList{
			Processes: []*sandboxv1.ProcessInfo{
				{Pid: 1, Ppid: 0, Command: "/init", State: "S"},
				{Pid: 42, Ppid: 1, Command: "python3 main.py", State: "R", CpuPercent: 5.2},
			},
		},
	}
	client := newWatchTestServer(t, g)
	ctx := context.Background()

	resp, err := client.Processes(ctx, connect.NewRequest(&sandboxv1.ProcessesRequest{}))
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	procs := resp.Msg.GetProcesses()
	if len(procs) != 2 {
		t.Fatalf("got %d processes, want 2", len(procs))
	}
	if procs[0].GetPid() != 1 || procs[0].GetCommand() != "/init" {
		t.Fatalf("proc[0] = %+v, want pid=1 command=/init", procs[0])
	}
	if procs[1].GetPid() != 42 || procs[1].GetCpuPercent() != 5.2 {
		t.Fatalf("proc[1] = %+v, want pid=42 cpu=5.2", procs[1])
	}
}

// TestSignalForwardsPidAndSignal is the Task 2.6 Signal acceptance test:
// Signal forwards the pid and signal number to the fake guest.
func TestSignalForwardsPidAndSignal(t *testing.T) {
	g := &watchGuest{}
	client := newWatchTestServer(t, g)
	ctx := context.Background()

	_, err := client.Signal(ctx, connect.NewRequest(&sandboxv1.SignalRequest{
		Pid:    99,
		Signal: 15, // SIGTERM
	}))
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	if g.gotSignalPid != 99 {
		t.Fatalf("gotSignalPid = %d, want 99", g.gotSignalPid)
	}
	if g.gotSignalSignal != 15 {
		t.Fatalf("gotSignalSignal = %d, want 15 (SIGTERM)", g.gotSignalSignal)
	}
}

// TestWatchGuestNilReturnsFollowup verifies that a Service without a Guest
// returns the honest #24 follow-up error for Watch.
func TestWatchGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	wstream, err := client.Watch(ctx, connect.NewRequest(&sandboxv1.WatchRequest{Path: "/workspace"}))
	if err != nil {
		var connErr *connect.Error
		if !errors.As(err, &connErr) {
			t.Fatalf("expected connect.Error, got %T: %v", err, err)
		}
		if connErr.Code() != connect.CodeUnimplemented {
			t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
		}
		return
	}
	defer wstream.Close()

	wstream.Receive()
	if serr := wstream.Err(); serr == nil {
		t.Fatal("expected error from nil Guest")
	} else if connect.CodeOf(serr) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connect.CodeOf(serr))
	}
}

// TestProcessesGuestNilReturnsFollowup verifies that a Service without a Guest
// returns the honest #24 follow-up error for Processes.
func TestProcessesGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	_, err := client.Processes(ctx, connect.NewRequest(&sandboxv1.ProcessesRequest{}))
	if err == nil {
		t.Fatal("expected error from nil Guest")
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connect.CodeOf(err))
	}
}

// TestSignalGuestNilReturnsFollowup verifies that a Service without a Guest
// returns the honest #24 follow-up error for Signal.
func TestSignalGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	_, err := client.Signal(ctx, connect.NewRequest(&sandboxv1.SignalRequest{Pid: 1, Signal: 9}))
	if err == nil {
		t.Fatal("expected error from nil Guest")
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connect.CodeOf(err))
	}
}

// TestConnectErrSurfacesRemediationText is the Task 2.7 test: connectErr
// wraps a cause error and appends a remediation string; the resulting
// connect.Error carries both in its message and preserves the error chain.
func TestConnectErrSurfacesRemediationText(t *testing.T) {
	cause := errors.New("sandbox not found")
	err := connectErr(connect.CodeNotFound, cause, "check that the sandbox id is correct and the sandbox is running")
	if err.Code() != connect.CodeNotFound {
		t.Fatalf("code = %v, want CodeNotFound", err.Code())
	}
	msg := err.Error()
	if msg == "" {
		t.Fatal("empty error message")
	}
	// The remediation text must appear somewhere in the error message.
	if !strings.Contains(msg, "check that the sandbox id is correct") {
		t.Fatalf("remediation text not in error message: %q", msg)
	}
	// The cause must also appear and the chain must be preserved via %w.
	if !strings.Contains(msg, "sandbox not found") {
		t.Fatalf("cause text not in error message: %q", msg)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error chain not preserved: errors.Is(err, cause) = false")
	}
}
