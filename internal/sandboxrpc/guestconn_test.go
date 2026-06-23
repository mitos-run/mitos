package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakeGuest implements GuestConn for tests. It emits a scripted sequence of
// stdout chunks then a terminal exit code. All other GuestConn methods are
// stubs that return an "unimplemented in fake" error; Tasks 2.2+ override them
// via embedding in fileGuest (and the other task-specific fake types).
type fakeGuest struct {
	execChunks []string
	exit       int32
}

// fakeExecStream implements ExecStream backed by the fakeGuest.
type fakeExecStream struct {
	chunks []string
	exit   int32
	pos    int
}

func (s *fakeExecStream) Recv() (*ExecFrame, error) {
	if s.pos < len(s.chunks) {
		chunk := s.chunks[s.pos]
		s.pos++
		return &ExecFrame{Stdout: []byte(chunk)}, nil
	}
	return &ExecFrame{ExitCode: s.exit, Done: true}, nil
}

func (s *fakeExecStream) Close() error { return nil }

func (f *fakeGuest) Exec(_ context.Context, _ *sandboxv1.ExecOpen) (ExecStream, error) {
	return &fakeExecStream{chunks: f.execChunks, exit: f.exit}, nil
}

// File operation stubs. Tasks 2.2+ override these in embedding structs.

func (f *fakeGuest) ReadFile(_ context.Context, _ string, _, _ int64) ([][]byte, error) {
	return nil, errors.New("ReadFile: unimplemented in fakeGuest")
}

func (f *fakeGuest) WriteFile(_ context.Context, _ string, _ uint32, _ [][]byte) (*WriteFileResult, error) {
	return nil, errors.New("WriteFile: unimplemented in fakeGuest")
}

func (f *fakeGuest) List(_ context.Context, _ string, _ int32, _ string, _ string) (*ListResult, error) {
	return nil, errors.New("List: unimplemented in fakeGuest")
}

func (f *fakeGuest) Stat(_ context.Context, _ string) (*FileInfo, error) {
	return nil, errors.New("Stat: unimplemented in fakeGuest")
}

func (f *fakeGuest) Mkdir(_ context.Context, _ string, _ bool) error {
	return errors.New("Mkdir: unimplemented in fakeGuest")
}

func (f *fakeGuest) Remove(_ context.Context, _ string, _ bool) error {
	return errors.New("Remove: unimplemented in fakeGuest")
}

func (f *fakeGuest) RunCode(_ context.Context, _ *sandboxv1.RunCodeOpen) (RunCodeStream, error) {
	return nil, errors.New("RunCode: unimplemented in fakeGuest")
}

func (f *fakeGuest) PortForward(_ context.Context, _ uint32) (PortForwardStream, error) {
	return nil, errors.New("PortForward: unimplemented in fakeGuest")
}

func (f *fakeGuest) Vitals(_ context.Context, _ time.Duration) (VitalsStream, error) {
	return nil, errors.New("Vitals: unimplemented in fakeGuest")
}

func (f *fakeGuest) Watch(_ context.Context, _ string) (WatchStream, error) {
	return nil, errors.New("Watch: unimplemented in fakeGuest")
}

func (f *fakeGuest) Processes(_ context.Context) (*sandboxv1.ProcessList, error) {
	return nil, errors.New("Processes: unimplemented in fakeGuest")
}

func (f *fakeGuest) Signal(_ context.Context, _ int32, _ int32) error {
	return errors.New("Signal: unimplemented in fakeGuest")
}

func (f *fakeGuest) Archive(_ context.Context, _ string) (ArchiveStream, error) {
	return nil, errors.New("Archive: unimplemented in fakeGuest")
}

func (f *fakeGuest) Upload(_ context.Context, _ string, chunks <-chan []byte) (*UploadResult, error) {
	// Drain the channel so the caller's goroutine can exit cleanly.
	for range chunks {
	}
	return nil, errors.New("Upload: unimplemented in fakeGuest")
}

// execResult collects the output of drainExec.
type execResult struct {
	stdout string
	stderr string
	exit   int32
}

// drainExec drives the Service.Exec handler via an in-memory Connect server and
// collects all ExecResponse frames into an execResult. The ExecOpen is wrapped
// in the first ExecRequest.
func drainExecViaGuest(t *testing.T, svc *Service, open *sandboxv1.ExecOpen) execResult {
	t.Helper()
	client, _ := newTestServer(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := client.Exec(ctx)
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: open},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	var sb, eb strings.Builder
	var exitCode int32
	for {
		resp, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			sb.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			eb.Write(m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
		}
	}
	return execResult{stdout: sb.String(), stderr: eb.String(), exit: exitCode}
}

// TestServiceExecStreamsStdoutAndExit is the Task 2.1 acceptance test: a Service
// wired with a GuestConn (via the Guest field) streams stdout chunks and a
// terminal exit code over the Connect bidi stream.
func TestServiceExecStreamsStdoutAndExit(t *testing.T) {
	fake := &fakeGuest{execChunks: []string{"hel", "lo\n"}, exit: 0}
	svc := &Service{Guest: func(string) (GuestConn, error) { return fake, nil }}
	got := drainExecViaGuest(t, svc, &sandboxv1.ExecOpen{Command: "echo hello"})
	if got.stdout != "hello\n" || got.exit != 0 {
		t.Fatalf("want stdout=%q exit=0, got stdout=%q exit=%d", "hello\n", got.stdout, got.exit)
	}
}
