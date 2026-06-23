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

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, recvErr := rs.Recv()
		if recvErr != nil {
			// Any error from Recv (including unexpected io.EOF) is a transport
			// failure. Surface it as an LLM-legible exit frame so the client
			// always reads a clean terminal frame.
			_ = stream.Send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: 1},
			})
			return nil
		}

		switch frame.Kind {
		case RunCodeFrameStdout:
			if len(frame.Stdout) > 0 {
				if err := stream.Send(&sandboxv1.RunCodeResponse{
					Msg: &sandboxv1.RunCodeResponse_Stdout{Stdout: frame.Stdout},
				}); err != nil {
					return err
				}
			}

		case RunCodeFrameStderr:
			if len(frame.Stderr) > 0 {
				if err := stream.Send(&sandboxv1.RunCodeResponse{
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
			if err := stream.Send(&sandboxv1.RunCodeResponse{
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
			if err := stream.Send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_Error{Error: protoErr},
			}); err != nil {
				return err
			}

		case RunCodeFrameExit:
			return stream.Send(&sandboxv1.RunCodeResponse{
				Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: frame.ExitCode},
			})
		}
	}
}
