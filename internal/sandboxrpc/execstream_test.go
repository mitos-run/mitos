package sandboxrpc

// execstream_test.go covers the two server-streaming RPCs ExecStream and
// RunCodeStream. They are the HTTP/1.1-reachable counterparts to the bidi Exec
// and RunCode (Connect serves bidi only over HTTP/2). Each builds the
// equivalent ExecOpen/RunCodeOpen from a unary request, opens the SAME
// GuestConn.Exec / GuestConn.RunCode the bidi handlers use, and copies frames
// to the server stream. These tests assert intact forwarding (stdout + exit,
// result + exit) and that frames are delivered INCREMENTALLY (the server-stream
// framing flushes each frame as it arrives, not all at the end).

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// recordingExecGuest embeds fakeGuest and records the ExecOpen it received, so
// the test can assert ExecStream built the open from the unary request. The
// returned ExecStream optionally gates after the first chunk so the test can
// prove incremental delivery.
type recordingExecGuest struct {
	fakeGuest

	gotOpen *sandboxv1.ExecOpen
	chunks  []string
	exit    int32
	// gate, when non-nil, blocks the second chunk until it is closed, proving
	// the first frame reached the client before the rest were produced.
	gate chan struct{}
}

type gatedExecStream struct {
	chunks []string
	exit   int32
	pos    int
	gate   chan struct{}
}

func (s *gatedExecStream) Recv() (*ExecFrame, error) {
	if s.pos < len(s.chunks) {
		// Block before producing the SECOND chunk until the gate is released.
		if s.pos == 1 && s.gate != nil {
			<-s.gate
		}
		chunk := s.chunks[s.pos]
		s.pos++
		return &ExecFrame{Stdout: []byte(chunk)}, nil
	}
	return &ExecFrame{ExitCode: s.exit, Done: true}, nil
}

func (s *gatedExecStream) Close() error { return nil }

func (g *recordingExecGuest) Exec(_ context.Context, open *sandboxv1.ExecOpen) (ExecStream, error) {
	g.gotOpen = open
	return &gatedExecStream{chunks: g.chunks, exit: g.exit, gate: g.gate}, nil
}

// TestExecStreamForwardsStdoutAndExit asserts the server-streaming ExecStream
// builds the ExecOpen from the unary request (command, args, env, cwd), opens
// the guest exec, and forwards the stdout chunks and terminal exit intact.
func TestExecStreamForwardsStdoutAndExit(t *testing.T) {
	g := &recordingExecGuest{chunks: []string{"hel", "lo\n"}, exit: 7}
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{
		Command:        "echo hello",
		Args:           []string{"-n"},
		Env:            []*sandboxv1.EnvVar{{Key: "FOO", Value: "bar"}},
		Cwd:            "/workspace",
		TimeoutSeconds: 5,
	})
	stream, err := client.ExecStream(ctx, req)
	if err != nil {
		t.Fatalf("open ExecStream: %v", err)
	}

	var stdout string
	var exitCode int32
	for stream.Receive() {
		resp := stream.Msg()
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout += string(m.Stdout)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if stdout != "hello\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "hello\n")
	}
	if exitCode != 7 {
		t.Fatalf("exit = %d, want 7", exitCode)
	}

	// The open must have been built from the unary request.
	if g.gotOpen.GetCommand() != "echo hello" {
		t.Fatalf("open.command = %q, want %q", g.gotOpen.GetCommand(), "echo hello")
	}
	if len(g.gotOpen.GetArgs()) != 1 || g.gotOpen.GetArgs()[0] != "-n" {
		t.Fatalf("open.args = %v, want [-n]", g.gotOpen.GetArgs())
	}
	if g.gotOpen.GetCwd() != "/workspace" {
		t.Fatalf("open.cwd = %q, want %q", g.gotOpen.GetCwd(), "/workspace")
	}
	if g.gotOpen.GetTimeoutSeconds() != 5 {
		t.Fatalf("open.timeout_seconds = %d, want 5", g.gotOpen.GetTimeoutSeconds())
	}
	env := g.gotOpen.GetEnv()
	if len(env) != 1 || env[0].GetKey() != "FOO" || env[0].GetValue() != "bar" {
		t.Fatalf("open.env = %v, want [{FOO bar}]", env)
	}
	// The open MUST NOT carry a pty: ExecStream is non-interactive only.
	if g.gotOpen.GetPty() != nil {
		t.Fatal("open.pty must be nil for the non-interactive ExecStream")
	}
}

// TestExecStreamDeliversIncrementally proves the server-stream flushes each
// frame as it arrives: the fake gates before the second chunk, so the client
// MUST be able to read the first stdout frame before the test releases the
// gate. If frames were buffered until the end, the first Receive would block
// forever and the test would time out.
func TestExecStreamDeliversIncrementally(t *testing.T) {
	gate := make(chan struct{})
	g := &recordingExecGuest{chunks: []string{"first\n", "second\n"}, exit: 0, gate: gate}
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{Command: "x"})
	stream, err := client.ExecStream(ctx, req)
	if err != nil {
		t.Fatalf("open ExecStream: %v", err)
	}
	defer stream.Close()

	// Read the FIRST frame before releasing the gate; this only succeeds if the
	// server flushed it incrementally.
	if !stream.Receive() {
		t.Fatalf("expected first frame, got err: %v", stream.Err())
	}
	first, ok := stream.Msg().Msg.(*sandboxv1.ExecResponse_Stdout)
	if !ok {
		t.Fatalf("first frame = %T, want stdout", stream.Msg().Msg)
	}
	if string(first.Stdout) != "first\n" {
		t.Fatalf("first stdout = %q, want %q", string(first.Stdout), "first\n")
	}

	// Now release the gate so the rest of the stream completes.
	close(gate)

	var rest string
	for stream.Receive() {
		if m, ok := stream.Msg().Msg.(*sandboxv1.ExecResponse_Stdout); ok {
			rest += string(m.Stdout)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if rest != "second\n" {
		t.Fatalf("rest stdout = %q, want %q", rest, "second\n")
	}
}

// TestExecStreamGuestNilReturnsFollowup verifies a Service without a Guest
// returns the honest #24 CodeUnimplemented follow-up for ExecStream.
func TestExecStreamGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	stream, err := client.ExecStream(ctx, connect.NewRequest(&sandboxv1.ExecStreamRequest{Command: "x"}))
	if err == nil {
		// On gRPC the error may surface on the first Receive instead of the call.
		stream.Receive()
		err = stream.Err()
	}
	if err == nil {
		t.Fatal("expected error from nil Guest, got nil")
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("expected connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
	}
}

// recordingRunCodeGuest embeds fakeGuest and records the RunCodeOpen it
// received so the test can assert RunCodeStream built the open from the unary
// request.
type recordingRunCodeGuest struct {
	fakeGuest

	gotOpen *sandboxv1.RunCodeOpen
	frames  []*RunCodeFrame
}

func (g *recordingRunCodeGuest) RunCode(_ context.Context, open *sandboxv1.RunCodeOpen) (RunCodeStream, error) {
	g.gotOpen = open
	return &fakeRunCodeStream{frames: g.frames}, nil
}

// drainRunCodeStream drives Service.RunCodeStream via the in-memory Connect
// server and collects all RunCodeResponse frames in order.
func drainRunCodeStream(t *testing.T, client sandboxv1connect.SandboxClient, req *sandboxv1.RunCodeStreamRequest) []*sandboxv1.RunCodeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.RunCodeStream(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("open RunCodeStream: %v", err)
	}
	var frames []*sandboxv1.RunCodeResponse
	for stream.Receive() {
		frames = append(frames, stream.Msg())
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("stream err: %v", err)
	}
	return frames
}

// TestRunCodeStreamForwardsResultAndExit asserts the server-streaming
// RunCodeStream builds the RunCodeOpen from the unary request, opens the guest
// run, and forwards a result frame and the terminal exit intact.
func TestRunCodeStreamForwardsResultAndExit(t *testing.T) {
	g := &recordingRunCodeGuest{
		frames: []*RunCodeFrame{
			{Kind: RunCodeFrameStdout, Stdout: []byte("hi\n")},
			{Kind: RunCodeFrameResult, Result: &RunCodeResult{
				Text: "42",
				Data: map[string][]byte{"text/html": []byte("<b>hi</b>")},
			}},
			{Kind: RunCodeFrameExit, ExitCode: 0},
		},
	}
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)

	got := drainRunCodeStream(t, client, &sandboxv1.RunCodeStreamRequest{
		Code:           "print(42)",
		Language:       "python",
		TimeoutSeconds: 3,
	})

	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	stdout, ok := got[0].Msg.(*sandboxv1.RunCodeResponse_Stdout)
	if !ok || string(stdout.Stdout) != "hi\n" {
		t.Fatalf("frame 0 = %T %q, want stdout hi\\n", got[0].Msg, "")
	}
	result, ok := got[1].Msg.(*sandboxv1.RunCodeResponse_Result)
	if !ok {
		t.Fatalf("frame 1 = %T, want result", got[1].Msg)
	}
	if result.Result.GetText() != "42" {
		t.Fatalf("result.text = %q, want 42", result.Result.GetText())
	}
	if string(result.Result.GetData()["text/html"]) != "<b>hi</b>" {
		t.Fatalf("result.data missing or wrong: %v", result.Result.GetData())
	}
	exit, ok := got[2].Msg.(*sandboxv1.RunCodeResponse_ExitCode)
	if !ok || exit.ExitCode != 0 {
		t.Fatalf("frame 2 = %T, want exit 0", got[2].Msg)
	}

	// The open must have been built from the unary request.
	if g.gotOpen.GetCode() != "print(42)" {
		t.Fatalf("open.code = %q, want %q", g.gotOpen.GetCode(), "print(42)")
	}
	if g.gotOpen.GetLanguage() != "python" {
		t.Fatalf("open.language = %q, want python", g.gotOpen.GetLanguage())
	}
	if g.gotOpen.GetTimeoutSeconds() != 3 {
		t.Fatalf("open.timeout_seconds = %d, want 3", g.gotOpen.GetTimeoutSeconds())
	}
}

// TestRunCodeStreamGuestNilReturnsFollowup verifies a Service without a Guest
// returns the honest #24 CodeUnimplemented follow-up for RunCodeStream.
func TestRunCodeStreamGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	stream, err := client.RunCodeStream(ctx, connect.NewRequest(&sandboxv1.RunCodeStreamRequest{Code: "1+1"}))
	if err == nil {
		stream.Receive()
		err = stream.Err()
	}
	if err == nil {
		t.Fatal("expected error from nil Guest, got nil")
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("expected connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
	}
}
