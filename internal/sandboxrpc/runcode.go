package sandboxrpc

// runcode.go implements the RunCode RPC of the Sandbox Connect service (Task
// 2.3): a bidi-streaming RPC that executes code in the sandbox kernel and
// forwards stdout/stderr chunks, rich result frames, kernel error frames, and
// a terminal exit code from the GuestConn port to the Connect response stream.

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// RunCode executes a code snippet in the sandbox kernel and streams its output
// over the Connect bidi stream. The first client RunCodeRequest MUST carry the
// open oneof (code, language, timeout_seconds). The handler delegates to the
// GuestConn.RunCode port, forwarding each RunCodeFrame as a RunCodeResponse:
// stdout/stderr chunks, a RunResult (with text + data map), a RunError, and
// a terminal exit_code. Subsequent client messages (stdin) are drained so the
// client is not blocked, but interactive stdin is a #24 follow-up.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) RunCode(ctx context.Context, stream *connect.BidiStream[sandboxv1.RunCodeRequest, sandboxv1.RunCodeResponse]) error {
	if s.Guest == nil {
		return followup("RunCode")
	}

	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("RunCode stream closed before the opening message: the first RunCodeRequest must carry the open oneof (code, language, timeout_seconds)"))
	}
	open := first.GetOpen()
	if open == nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first RunCodeRequest must carry the open oneof (code, language, timeout_seconds), got %T", first.Msg))
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	rs, err := conn.RunCode(ctx, open)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest RunCode open failed: %w; check that the language kernel is available in the sandbox", err))
	}
	defer rs.Close()

	return copyRunCodeFrames(ctx, rs, stream.Send)
}

// RunCodeStream runs a code snippet and streams its output over a
// server-streaming RPC, the HTTP/1.1-reachable counterpart to the bidi RunCode
// (Connect serves bidi only over HTTP/2). It builds the equivalent RunCodeOpen
// from the unary request (no stdin), opens the guest run via the SAME
// GuestConn.RunCode path the bidi RunCode uses, and copies frames with the
// shared copyRunCodeFrames helper. Like the bidi RunCode, this path does not
// acquire a concurrent-stream slot (only Exec does in the daemon
// vsockGuestConn), so its cap behavior matches the bidi RunCode exactly.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) RunCodeStream(ctx context.Context, req *connect.Request[sandboxv1.RunCodeStreamRequest], stream *connect.ServerStream[sandboxv1.RunCodeResponse]) error {
	if s.Guest == nil {
		return followup("RunCodeStream")
	}

	r := req.Msg
	open := &sandboxv1.RunCodeOpen{
		Code:           r.GetCode(),
		Language:       r.GetLanguage(),
		TimeoutSeconds: r.GetTimeoutSeconds(),
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	rs, err := conn.RunCode(ctx, open)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest RunCode open failed: %w; check that the language kernel is available in the sandbox", err))
	}
	defer rs.Close()

	return copyRunCodeFrames(ctx, rs, stream.Send)
}

// copyRunCodeFrames drains a guest RunCodeStream and forwards each frame to
// send as a RunCodeResponse: stdout/stderr chunks, rich result, kernel error,
// then a terminal exit_code. It is shared by the bidi RunCode and the
// server-streaming RunCodeStream so both transports have identical copy
// semantics.
func copyRunCodeFrames(ctx context.Context, rs RunCodeStream, send func(*sandboxv1.RunCodeResponse) error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, recvErr := rs.Recv()
		if recvErr != nil {
			// Any error from Recv (including an unexpected io.EOF) is a transport
			// failure (the guest agent may have crashed or the vsock connection
			// was lost). Send a clean terminal exit frame so the client never
			// hangs. RunCodeResponse.exit_code is a bare int32 with no message
			// field, so unlike Exec this terminal frame cannot carry remediation
			// text; the non-zero exit is the only signal. Adding an error string
			// to RunCodeResponse is a tracked proto follow-up of issue #24.
			_ = send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: 1},
			})
			return nil
		}

		switch frame.Kind {
		case RunCodeFrameStdout:
			if len(frame.Stdout) > 0 {
				if err := send(&sandboxv1.RunCodeResponse{
					Msg: &sandboxv1.RunCodeResponse_Stdout{Stdout: frame.Stdout},
				}); err != nil {
					return err
				}
			}

		case RunCodeFrameStderr:
			if len(frame.Stderr) > 0 {
				if err := send(&sandboxv1.RunCodeResponse{
					Msg: &sandboxv1.RunCodeResponse_Stderr{Stderr: frame.Stderr},
				}); err != nil {
					return err
				}
			}

		case RunCodeFrameResult:
			protoResult := &sandboxv1.RunResult{}
			if frame.Result != nil {
				protoResult.Text = frame.Result.Text
				if len(frame.Result.Data) > 0 {
					protoResult.Data = make(map[string][]byte, len(frame.Result.Data))
					for k, v := range frame.Result.Data {
						protoResult.Data[k] = v
					}
				}
			}
			if err := send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_Result{Result: protoResult},
			}); err != nil {
				return err
			}

		case RunCodeFrameError:
			protoErr := &sandboxv1.RunError{}
			if frame.Error != nil {
				protoErr.Name = frame.Error.Name
				protoErr.Value = frame.Error.Value
				protoErr.Traceback = frame.Error.Traceback
			}
			if err := send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_Error{Error: protoErr},
			}); err != nil {
				return err
			}

		case RunCodeFrameExit:
			return send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: frame.ExitCode},
			})
		}
	}
}
