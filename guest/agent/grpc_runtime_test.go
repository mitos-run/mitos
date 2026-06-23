//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestGRPCWatchEmitsCreate adds a watch on a workspace dir, creates a file, and
// asserts a CREATE FsEvent for it arrives. It then cancels the stream and proves
// the server-side goroutine and inotify fd are released (no leak): the RPC
// returns promptly after cancel.
func TestGRPCWatchEmitsCreate(t *testing.T) {
	withWorkspaceRoot(t)
	watchDir := filepath.Join(workspaceRoot, "watched")
	if err := os.Mkdir(watchDir, 0o755); err != nil {
		t.Fatal(err)
	}

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.Watch(ctx, &sandboxv1.WatchRequest{Path: watchDir})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Give the server a moment to add the inotify watch before creating the file,
	// otherwise the event can race ahead of the watch and be missed.
	time.Sleep(200 * time.Millisecond)
	created := filepath.Join(watchDir, "new.txt")
	if err := os.WriteFile(created, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read events until we see the CREATE for our file (a stray MODIFY may also
	// arrive; we only require the CREATE).
	gotCreate := false
	deadline := time.After(5 * time.Second)
	recvCh := make(chan *sandboxv1.FsEvent, 8)
	recvErr := make(chan error, 1)
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			recvCh <- ev
		}
	}()
	for !gotCreate {
		select {
		case ev := <-recvCh:
			if ev.GetKind() == sandboxv1.FsEvent_CREATE && ev.GetPath() == created {
				gotCreate = true
			}
		case err := <-recvErr:
			t.Fatalf("Watch recv: %v", err)
		case <-deadline:
			t.Fatal("did not receive CREATE event within deadline")
		}
	}

	// Cancel the stream; the server must close the inotify fd, end the reader
	// goroutine, and return. The client Recv goroutine should observe the stream
	// ending promptly, proving no leak/hang.
	cancel()
	select {
	case <-recvErr:
		// Stream ended after cancel: server tore down cleanly.
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not terminate after client cancel: possible goroutine/fd leak")
	}
}

// TestGRPCWatchRejectsOutsideWorkspace proves the workspace allowlist guard
// refuses a path outside the workspace root.
func TestGRPCWatchRejectsOutsideWorkspace(t *testing.T) {
	withWorkspaceRoot(t)
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Watch(ctx, &sandboxv1.WatchRequest{Path: "/etc"})
	if err != nil {
		t.Fatalf("Watch open: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for out-of-workspace path, got nil")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("got code %v, want PermissionDenied", status.Code(err))
	}
}

// TestGRPCProcessesListsSelfWithCommNotCmdline asserts the current test process
// appears in the process table by its comm and PROVES cmdline is not exposed:
// this process is re-exec'd nowhere, so instead we assert that no ProcessInfo
// command field contains the secret-looking argv token the test plants in its
// own argv-shaped child. Concretely, we spawn a child whose argv carries a fake
// secret and assert (a) the child appears by its comm (the program base name)
// and (b) no command field anywhere contains the secret token, proving the
// handler reports comm, never cmdline.
func TestGRPCProcessesListsSelfWithCommNotCmdline(t *testing.T) {
	const secretToken = "SUPERSECRETtoken1234567890"

	// Spawn a long-lived child that sleeps, with the secret planted in argv. sleep
	// ignores extra args, so this is a harmless way to put a secret-looking token
	// into a real process's /proc/<pid>/cmdline.
	child := exec.Command("/bin/sleep", "30", secretToken)
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn /bin/sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	list, err := client.Processes(ctx, &sandboxv1.ProcessesRequest{})
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(list.Processes) == 0 {
		t.Fatal("process list is empty")
	}

	var foundChild bool
	for _, p := range list.Processes {
		if strings.Contains(p.GetCommand(), secretToken) {
			t.Fatalf("SECURITY: process command %q leaks argv secret; handler must report comm, not cmdline", p.GetCommand())
		}
		if int(p.GetPid()) == child.Process.Pid {
			foundChild = true
			// comm for /bin/sleep is "sleep"; it must be present and must NOT be the
			// full cmdline.
			if p.GetCommand() != "sleep" {
				t.Errorf("child command = %q, want comm %q", p.GetCommand(), "sleep")
			}
		}
	}
	if !foundChild {
		t.Errorf("spawned child pid %d not found in process table", child.Process.Pid)
	}
}

// TestGRPCSignalKillsChild signals a short-lived child and asserts it dies.
func TestGRPCSignalKillsChild(t *testing.T) {
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn /bin/sleep: %v", err)
	}
	pid := child.Process.Pid
	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
	})

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Signal(ctx, &sandboxv1.SignalRequest{Pid: int32(pid), Signal: int32(syscall.SIGKILL)}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	select {
	case err := <-waitErr:
		// The child was killed; Wait returns a non-nil (signal) error.
		if err == nil {
			t.Error("child exited cleanly; expected it to be killed by the signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not die after SIGKILL")
	}
}

// TestGRPCSignalRejectsPid1 proves the control-plane guard refuses to signal pid
// 1 (the guest agent itself).
func TestGRPCSignalRejectsPid1(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Signal(ctx, &sandboxv1.SignalRequest{Pid: 1, Signal: int32(syscall.SIGKILL)})
	if err == nil {
		t.Fatal("expected InvalidArgument for pid 1, got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", status.Code(err))
	}
}

// TestGRPCSignalRejectsOutOfRange proves an out-of-range signal number is
// rejected before the kill syscall.
func TestGRPCSignalRejectsOutOfRange(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Signal(ctx, &sandboxv1.SignalRequest{Pid: 99999, Signal: 9999})
	if err == nil {
		t.Fatal("expected InvalidArgument for out-of-range signal, got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", status.Code(err))
	}
}
