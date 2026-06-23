package sandboxrpc

// portforward.go implements the PortForward bidi-stream RPC of the Sandbox
// Connect service (Task 2.4): proxies raw bytes from and to a guest TCP port
// over the Connect Frame stream. The first client Frame MUST carry the open
// oneof; subsequent frames carry data bytes. The handler delegates to the
// GuestConn.PortForward port so the logic is exercised by a fake in tests
// without a real vsock connection.

import (
	"context"
	"fmt"
	"io"
	"sync"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// PortForwardFrame is one event from a guest port-forward stream. When Close
// is true the stream has ended normally and no further frames should be read.
type PortForwardFrame struct {
	// Data carries bytes flowing from the guest port toward the client.
	// Empty unless this is a data frame (Close is false).
	Data []byte
	// Close is true for the terminal frame. No further Recv calls should be
	// made after receiving a Close frame.
	Close bool
}

// PortForwardStream is the handle returned by GuestConn.PortForward. Recv
// returns successive frames with err == nil for each frame including the
// terminal Close frame. After the Close frame, a subsequent call returns
// io.EOF (the Service never makes that call). Other errors indicate a
// transport or guest failure. Close releases resources.
type PortForwardStream interface {
	// Recv returns the next PortForwardFrame with err == nil, including the
	// terminal Close frame. io.EOF is returned only on a subsequent call
	// after Close, which the Service never makes. Other errors are transport
	// or guest failures.
	Recv() (*PortForwardFrame, error)
	// Send forwards bytes from the client toward the guest port.
	Send(data []byte) error
	// Close releases resources. Safe to call after the terminal Close frame.
	Close() error
}

// PortForward proxies a guest TCP port over a bidi Frame stream. The first
// client Frame MUST carry the open oneof to select the guest port; subsequent
// frames carry data bytes. The handler bridges to GuestConn.PortForward so
// both directions are proxied until the guest closes the stream.
//
// The guest-to-client goroutine calls stream.Send and is joined (via
// WaitGroup) before the handler returns, so stream.Send is never called after
// the handler has returned. The stream ends when the guest emits a Close frame
// or when the request context is cancelled.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) PortForward(ctx context.Context, stream *connect.BidiStream[sandboxv1.Frame, sandboxv1.Frame]) error {
	if s.Guest == nil {
		return followup("PortForward")
	}

	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("PortForward stream closed before the opening frame: the first Frame must carry the open oneof (port)"))
	}
	open := first.GetOpen()
	if open == nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first PortForward Frame must carry the open oneof (port), got a data frame"))
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	pf, err := conn.PortForward(ctx, open.GetPort())
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest PortForward open failed for port %d: %w; ensure the process is listening on the target port inside the sandbox", open.GetPort(), err))
	}
	// guestErrCh carries the result of the guest-to-client goroutine.
	// Buffered so the goroutine never blocks writing its result.
	guestErrCh := make(chan error, 1)

	// wg ensures the goroutine finishes (including all stream.Send calls)
	// before the handler returns, so stream.Send is never called post-return.
	var wg sync.WaitGroup
	wg.Add(1)

	// Defer order matters (LIFO): wg.Wait() is registered FIRST so it runs
	// LAST, and pf.Close() is registered SECOND so it runs FIRST. On any
	// return (client close, context cancel, error), pf.Close() unblocks the
	// goroutine's pf.Recv() before wg.Wait() joins it; the reverse order would
	// deadlock (wg.Wait() blocking on a goroutine stuck in a Recv that only
	// Close can unblock).
	defer wg.Wait()
	defer pf.Close()

	// Guest-to-client goroutine: reads from pf and writes to stream.Send.
	// It does NOT check any cancellation signal; it runs to completion and
	// writes one value to guestErrCh. The handler only returns after wg.Wait.
	go func() {
		defer wg.Done()
		for {
			frame, recvErr := pf.Recv()
			if recvErr != nil {
				if recvErr == io.EOF {
					guestErrCh <- nil
				} else {
					guestErrCh <- recvErr
				}
				return
			}
			if frame.Close {
				// Terminal close from guest: forward to client then signal done.
				_ = stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Close{Close: true}})
				guestErrCh <- nil
				return
			}
			if len(frame.Data) > 0 {
				if sendErr := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Data{Data: frame.Data}}); sendErr != nil {
					guestErrCh <- sendErr
					return
				}
			}
		}
	}()

	// Client-to-guest loop: runs on the main goroutine until the client closes
	// or the guest pump signals completion.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case guestErr := <-guestErrCh:
			return guestErr
		default:
		}

		clientFrame, recvErr := stream.Receive()
		if recvErr != nil {
			if recvErr == io.EOF {
				// Client done sending: wait for the guest pump to complete.
				return <-guestErrCh
			}
			return recvErr
		}
		if clientFrame.GetClose() {
			// Client signaled close: wait for the guest pump to complete.
			return <-guestErrCh
		}
		if data := clientFrame.GetData(); len(data) > 0 {
			if sendErr := pf.Send(data); sendErr != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("send to guest port: %w; the guest port may have closed", sendErr))
			}
		}
	}
}
