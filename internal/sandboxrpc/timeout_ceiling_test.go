package sandboxrpc

// timeout_ceiling_test.go covers the exec-timeout ceiling (issue #216) on the
// Connect runtime path (PR A, issue #358). The legacy /v1 handlers rejected a
// requested timeout over the ceiling with a typed timeout_too_large error,
// never silently reducing it; these tests assert the Connect Exec, ExecStream,
// RunCode, and RunCodeStream RPCs do the same:
//   - a request OVER the ceiling is rejected with CodeInvalidArgument before the
//     guest stream is opened (the fake guest is never reached);
//   - a request AT/UNDER the ceiling is accepted and reaches the fake guest;
//   - the rejection message names the requested timeout and the ceiling.

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

// ceilingExecGuest is a fake GuestConn that records whether Exec/RunCode was
// reached, so a ceiling test can prove an over-ceiling request never opens the
// guest stream.
type ceilingExecGuest struct {
	fakeGuest

	execReached    bool
	runCodeReached bool
}

func (g *ceilingExecGuest) Exec(_ context.Context, _ *sandboxv1.ExecOpen) (ExecStream, error) {
	g.execReached = true
	return &scriptedExecStream{}, nil
}

func (g *ceilingExecGuest) RunCode(_ context.Context, _ *sandboxv1.RunCodeOpen) (RunCodeStream, error) {
	g.runCodeReached = true
	return &fakeRunCodeStream{frames: []*RunCodeFrame{{Kind: RunCodeFrameExit, ExitCode: 0}}}, nil
}

// scriptedExecStream emits a single terminal exit frame.
type scriptedExecStream struct{ done bool }

func (s *scriptedExecStream) Recv() (*ExecFrame, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return &ExecFrame{ExitCode: 0, Done: true}, nil
}

func (s *scriptedExecStream) Close() error { return nil }

func newCeilingServer(t *testing.T, g *ceilingExecGuest, ceiling int) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{
		Guest:                 func(string) (GuestConn, error) { return g, nil },
		MaxExecTimeoutSeconds: ceiling,
	}
	client, _ := newTestServer(t, svc)
	return client
}

const testCeiling = 100

// drainExecBidi sends an ExecOpen with the given timeout over the bidi Exec and
// returns the terminal error (nil on success).
func drainExecBidi(t *testing.T, client sandboxv1connect.SandboxClient, timeout int32) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command:        "echo hi",
		TimeoutSeconds: timeout,
	}}}); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := stream.CloseRequest(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for {
		_, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestExecBidiRejectsOverCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	err := drainExecBidi(t, client, testCeiling+1)
	if err == nil {
		t.Fatal("expected over-ceiling Exec to be rejected, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "101") || !strings.Contains(err.Error(), "100") {
		t.Fatalf("error %q must name the requested timeout (101) and the ceiling (100)", err.Error())
	}
	if g.execReached {
		t.Fatal("guest Exec must NOT be opened for an over-ceiling request")
	}
}

func TestExecBidiAcceptsAtCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	if err := drainExecBidi(t, client, testCeiling); err != nil {
		t.Fatalf("at-ceiling Exec should be accepted: %v", err)
	}
	if !g.execReached {
		t.Fatal("guest Exec must be reached for an at-ceiling request")
	}
}

func TestExecStreamRejectsOverCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ExecStream(ctx, connect.NewRequest(&sandboxv1.ExecStreamRequest{
		Command:        "echo hi",
		TimeoutSeconds: testCeiling + 1,
	}))
	if err != nil {
		t.Fatalf("open ExecStream: %v", err)
	}
	for stream.Receive() {
	}
	rerr := stream.Err()
	if rerr == nil {
		t.Fatal("expected over-ceiling ExecStream to be rejected, got nil")
	}
	if connect.CodeOf(rerr) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(rerr))
	}
	if g.execReached {
		t.Fatal("guest Exec must NOT be opened for an over-ceiling ExecStream")
	}
}

func TestExecStreamAcceptsUnderCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ExecStream(ctx, connect.NewRequest(&sandboxv1.ExecStreamRequest{
		Command:        "echo hi",
		TimeoutSeconds: testCeiling - 1,
	}))
	if err != nil {
		t.Fatalf("open ExecStream: %v", err)
	}
	for stream.Receive() {
	}
	if rerr := stream.Err(); rerr != nil {
		t.Fatalf("under-ceiling ExecStream should be accepted: %v", rerr)
	}
	if !g.execReached {
		t.Fatal("guest Exec must be reached for an under-ceiling ExecStream")
	}
}

func TestRunCodeBidiRejectsOverCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.RunCode(ctx)
	if err := stream.Send(&sandboxv1.RunCodeRequest{Msg: &sandboxv1.RunCodeRequest_Open{Open: &sandboxv1.RunCodeOpen{
		Code:           "print(1)",
		TimeoutSeconds: int64(testCeiling) + 1,
	}}}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("close: %v", err)
	}
	var rerr error
	for {
		_, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			rerr = err
			break
		}
	}
	if rerr == nil {
		t.Fatal("expected over-ceiling RunCode to be rejected, got nil")
	}
	if connect.CodeOf(rerr) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(rerr))
	}
	if g.runCodeReached {
		t.Fatal("guest RunCode must NOT be opened for an over-ceiling request")
	}
}

func TestRunCodeStreamAcceptsUnderCeilingTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, testCeiling)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.RunCodeStream(ctx, connect.NewRequest(&sandboxv1.RunCodeStreamRequest{
		Code:           "print(1)",
		TimeoutSeconds: int64(testCeiling),
	}))
	if err != nil {
		t.Fatalf("open RunCodeStream: %v", err)
	}
	for stream.Receive() {
	}
	if rerr := stream.Err(); rerr != nil {
		t.Fatalf("at-ceiling RunCodeStream should be accepted: %v", rerr)
	}
	if !g.runCodeReached {
		t.Fatal("guest RunCode must be reached for an at-ceiling request")
	}
}

// TestCeilingDisabledHonorsAnyTimeout proves MaxExecTimeoutSeconds <= 0 disables
// the ceiling: a huge timeout is accepted and reaches the guest.
func TestCeilingDisabledHonorsAnyTimeout(t *testing.T) {
	g := &ceilingExecGuest{}
	client := newCeilingServer(t, g, 0)

	if err := drainExecBidi(t, client, 1<<30); err != nil {
		t.Fatalf("disabled ceiling should honor any timeout: %v", err)
	}
	if !g.execReached {
		t.Fatal("guest Exec must be reached when the ceiling is disabled")
	}
}
