package sandboxrpc

// watch.go implements the Watch server-stream RPC, the Processes unary RPC,
// and the Signal unary RPC of the Sandbox Connect service (Task 2.6):
// - Watch: streams FsEvents for a subtree until the client cancels.
// - Processes: returns the guest process table as a ProcessList.
// - Signal: delivers a POSIX signal to a guest process.
//
// Each method delegates to the corresponding GuestConn port method so the
// logic is exercised by a fake in tests without a real vsock connection.

import (
	"context"
	"fmt"
	"io"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// WatchStream is the handle returned by GuestConn.Watch. Recv returns
// successive FsEvent values with err == nil until the context is cancelled
// or the stream ends (returns io.EOF). Other errors indicate a transport or
// guest failure. Close releases resources.
type WatchStream interface {
	// Recv returns the next FsEvent with err == nil. Returns io.EOF when the
	// stream ends normally. Other errors are transport or guest failures.
	Recv() (*sandboxv1.FsEvent, error)
	// Close releases resources. Safe to call after io.EOF.
	Close() error
}

// Watch streams filesystem change events for a path subtree until the client
// cancels. The handler delegates to GuestConn.Watch and forwards each FsEvent
// to the Connect server stream immediately.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Watch(ctx context.Context, req *connect.Request[sandboxv1.WatchRequest], stream *connect.ServerStream[sandboxv1.FsEvent]) error {
	if s.Guest == nil {
		return followup("Watch")
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	ws, err := conn.Watch(ctx, req.Msg.GetPath())
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest Watch open failed for path %q: %w; check that the path exists inside the sandbox", req.Msg.GetPath(), err))
	}
	defer ws.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		event, recvErr := ws.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				return nil
			}
			return connect.NewError(connect.CodeInternal, fmt.Errorf("read watch stream: %w; the guest agent may have crashed or the vsock connection was lost", recvErr))
		}
		if err := stream.Send(event); err != nil {
			return err
		}
	}
}

// Processes returns the guest process table as a ProcessList.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Processes(ctx context.Context, req *connect.Request[sandboxv1.ProcessesRequest]) (*connect.Response[sandboxv1.ProcessList], error) {
	if s.Guest == nil {
		return nil, followup("Processes")
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	list, err := conn.Processes(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read process list: %w; check that the guest agent is healthy", err))
	}
	return connect.NewResponse(list), nil
}

// Signal delivers a POSIX signal to a process in the guest.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Signal(ctx context.Context, req *connect.Request[sandboxv1.SignalRequest]) (*connect.Response[sandboxv1.SignalResponse], error) {
	if s.Guest == nil {
		return nil, followup("Signal")
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	if err := conn.Signal(ctx, req.Msg.GetPid(), req.Msg.GetSignal()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("deliver signal %d to pid %d: %w; check that the process is running inside the sandbox", req.Msg.GetSignal(), req.Msg.GetPid(), err))
	}
	return connect.NewResponse(&sandboxv1.SignalResponse{}), nil
}
