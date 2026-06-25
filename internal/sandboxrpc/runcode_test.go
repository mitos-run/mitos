package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// runCodeGuest extends fakeGuest for RunCode operations. It holds scripted
// RunCodeFrames emitted by the fake kernel and records the open message it
// received.
type runCodeGuest struct {
	fakeGuest

	// gotOpen records the RunCodeOpen the fake received.
	gotOpen *sandboxv1.RunCodeOpen

	// frames is the scripted sequence of frames the fake emits.
	frames []*RunCodeFrame

	// openErr, when non-nil, is returned by RunCode instead of a stream.
	openErr error
}

// fakeRunCodeStream backs runCodeGuest.RunCode.
type fakeRunCodeStream struct {
	frames []*RunCodeFrame
	pos    int
}

func (s *fakeRunCodeStream) Recv() (*RunCodeFrame, error) {
	if s.pos >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.pos]
	s.pos++
	return f, nil
}

func (s *fakeRunCodeStream) Close() error { return nil }

func (g *runCodeGuest) RunCode(_ context.Context, open *sandboxv1.RunCodeOpen) (RunCodeStream, error) {
	g.gotOpen = open
	if g.openErr != nil {
		return nil, g.openErr
	}
	return &fakeRunCodeStream{frames: g.frames}, nil
}

// newRunCodeTestServer builds a Service wired with the runCodeGuest and returns
// the Connect client.
func newRunCodeTestServer(t *testing.T, g *runCodeGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// drainRunCode drives Service.RunCode via the in-memory Connect server,
// collecting all RunCodeResponse frames and returning them in order.
func drainRunCode(t *testing.T, client sandboxv1connect.SandboxClient, open *sandboxv1.RunCodeOpen) []*sandboxv1.RunCodeResponse {
	t.Helper()
	ctx := context.Background()
	stream := client.RunCode(ctx)
	if err := stream.Send(&sandboxv1.RunCodeRequest{
		Msg: &sandboxv1.RunCodeRequest_Open{Open: open},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	var frames []*sandboxv1.RunCodeResponse
	for {
		resp, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		frames = append(frames, resp)
	}
	return frames
}

// TestRunCodeForwardsStdoutResultAndExit is the Task 2.3 acceptance test.
// The fake guest emits:
//   - a stdout chunk ("hello\n"),
//   - a RunResult with text "42" and data["text/html"] = "<b>hi</b>",
//   - a terminal exit (code 0).
//
// The test asserts the Service forwards each frame INTACT: the stdout bytes,
// the result text, the data map entry, and the exit code.
func TestRunCodeForwardsStdoutResultAndExit(t *testing.T) {
	g := &runCodeGuest{
		frames: []*RunCodeFrame{
			{Kind: RunCodeFrameStdout, Stdout: []byte("hello\n")},
			{Kind: RunCodeFrameResult, Result: &RunCodeResult{
				Text: "42",
				Data: map[string][]byte{
					"text/html": []byte("<b>hi</b>"),
				},
			}},
			{Kind: RunCodeFrameExit, ExitCode: 0},
		},
	}
	client := newRunCodeTestServer(t, g)

	open := &sandboxv1.RunCodeOpen{Code: "print(42)", Language: "python"}
	got := drainRunCode(t, client, open)

	// Expect exactly 3 frames: stdout, result, exit_code.
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}

	// Frame 0: stdout.
	stdout, ok := got[0].Msg.(*sandboxv1.RunCodeResponse_Stdout)
	if !ok {
		t.Fatalf("frame 0 type = %T, want *RunCodeResponse_Stdout", got[0].Msg)
	}
	if string(stdout.Stdout) != "hello\n" {
		t.Fatalf("stdout = %q, want %q", string(stdout.Stdout), "hello\n")
	}

	// Frame 1: result with text and data map.
	result, ok := got[1].Msg.(*sandboxv1.RunCodeResponse_Result)
	if !ok {
		t.Fatalf("frame 1 type = %T, want *RunCodeResponse_Result", got[1].Msg)
	}
	if result.Result.GetText() != "42" {
		t.Fatalf("result.text = %q, want %q", result.Result.GetText(), "42")
	}
	htmlBytes, found := result.Result.GetData()["text/html"]
	if !found {
		t.Fatal("result.data[text/html] missing")
	}
	if string(htmlBytes) != "<b>hi</b>" {
		t.Fatalf("result.data[text/html] = %q, want %q", string(htmlBytes), "<b>hi</b>")
	}

	// Frame 2: terminal exit code 0.
	exit, ok := got[2].Msg.(*sandboxv1.RunCodeResponse_ExitCode)
	if !ok {
		t.Fatalf("frame 2 type = %T, want *RunCodeResponse_ExitCode", got[2].Msg)
	}
	if exit.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", exit.ExitCode)
	}

	// Assert the open message was forwarded with the correct fields.
	if g.gotOpen.GetCode() != "print(42)" {
		t.Fatalf("open.code = %q, want %q", g.gotOpen.GetCode(), "print(42)")
	}
	if g.gotOpen.GetLanguage() != "python" {
		t.Fatalf("open.language = %q, want %q", g.gotOpen.GetLanguage(), "python")
	}
}

// TestRunCodeForwardsRunError verifies that a RunError frame emitted by the
// fake guest is forwarded intact (name, value, traceback).
func TestRunCodeForwardsRunError(t *testing.T) {
	g := &runCodeGuest{
		frames: []*RunCodeFrame{
			{Kind: RunCodeFrameError, Error: &RunCodeError{
				Name:      "ZeroDivisionError",
				Value:     "division by zero",
				Traceback: []string{"  File \"<stdin>\", line 1, in <module>"},
			}},
			{Kind: RunCodeFrameExit, ExitCode: 1},
		},
	}
	client := newRunCodeTestServer(t, g)

	got := drainRunCode(t, client, &sandboxv1.RunCodeOpen{Code: "1/0"})
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}

	// Frame 0: kernel error.
	errFrame, ok := got[0].Msg.(*sandboxv1.RunCodeResponse_Error)
	if !ok {
		t.Fatalf("frame 0 type = %T, want *RunCodeResponse_Error", got[0].Msg)
	}
	if errFrame.Error.GetName() != "ZeroDivisionError" {
		t.Fatalf("error.name = %q, want ZeroDivisionError", errFrame.Error.GetName())
	}
	if errFrame.Error.GetValue() != "division by zero" {
		t.Fatalf("error.value = %q, want %q", errFrame.Error.GetValue(), "division by zero")
	}
	if len(errFrame.Error.GetTraceback()) != 1 {
		t.Fatalf("traceback len = %d, want 1", len(errFrame.Error.GetTraceback()))
	}

	// Frame 1: exit code 1.
	exit, ok := got[1].Msg.(*sandboxv1.RunCodeResponse_ExitCode)
	if !ok {
		t.Fatalf("frame 1 type = %T, want *RunCodeResponse_ExitCode", got[1].Msg)
	}
	if exit.ExitCode != 1 {
		t.Fatalf("exit_code = %d, want 1", exit.ExitCode)
	}
}

// TestRunCodeGuestNilReturnsFollowup verifies that a Service without a Guest
// returns the honest #24 follow-up error for RunCode.
func TestRunCodeGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	stream := client.RunCode(ctx)
	if err := stream.Send(&sandboxv1.RunCodeRequest{
		Msg: &sandboxv1.RunCodeRequest_Open{Open: &sandboxv1.RunCodeOpen{Code: "1+1"}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	_, err := stream.Receive()
	if err == nil {
		t.Fatal("expected error from nil Guest, got nil")
	}
	// The error must carry CodeUnimplemented.
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("expected connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
	}
}

// TestRunCodeTransportErrorSendsTerminalExit verifies that when the guest stream
// ends without an Exit frame (a crash or a lost vsock connection, surfaced as an
// io.EOF from Recv), the Service still sends a clean terminal exit_code=1 so the
// client never hangs. exit_code is a bare int32 and cannot carry a message.
func TestRunCodeTransportErrorSendsTerminalExit(t *testing.T) {
	g := &runCodeGuest{
		// Scripted frames with NO terminal Exit frame: after the stdout frame
		// the fake returns io.EOF, which the Service treats as a transport
		// failure.
		frames: []*RunCodeFrame{
			{Kind: RunCodeFrameStdout, Stdout: []byte("partial output")},
		},
	}
	client := newRunCodeTestServer(t, g)
	got := drainRunCode(t, client, &sandboxv1.RunCodeOpen{Code: "crash()"})

	if len(got) == 0 {
		t.Fatal("expected at least the stdout and a terminal exit frame, got none")
	}
	last := got[len(got)-1]
	exit, ok := last.Msg.(*sandboxv1.RunCodeResponse_ExitCode)
	if !ok {
		t.Fatalf("last frame = %T, want a terminal RunCodeResponse_ExitCode", last.Msg)
	}
	if exit.ExitCode != 1 {
		t.Fatalf("terminal exit code = %d, want 1 (transport failure)", exit.ExitCode)
	}
}
