package sandboxrpc

// vitals.go implements the Vitals server-stream RPC of the Sandbox Connect
// service (Task 2.5): streams GuestVitals samples from the guest until the
// client cancels. The handler delegates to the GuestConn.Vitals port so the
// logic is exercised by a fake in tests without a real vsock connection.

import (
	"context"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// VitalsStream is the handle returned by GuestConn.Vitals. Recv returns
// successive GuestVitals samples with err == nil until the context is
// cancelled or the guest closes the stream (returns io.EOF). Other errors
// indicate a transport or guest failure. Close releases resources.
type VitalsStream interface {
	// Recv returns the next GuestVitals sample with err == nil. Returns
	// io.EOF when the guest stream ends normally. Other errors are transport
	// or guest failures.
	Recv() (*sandboxv1.GuestVitals, error)
	// Close releases resources. Safe to call after io.EOF.
	Close() error
}

// Vitals streams GuestVitals samples from the guest at the requested interval
// until the client cancels. The handler delegates to GuestConn.Vitals and
// forwards each sample to the Connect server stream immediately.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Vitals(ctx context.Context, req *connect.Request[sandboxv1.VitalsRequest], stream *connect.ServerStream[sandboxv1.GuestVitals]) error {
	if s.Guest == nil {
		return followup("Vitals")
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	intervalSeconds := req.Msg.GetIntervalSeconds()
	if intervalSeconds <= 0 {
		intervalSeconds = 1
	}
	interval := time.Duration(intervalSeconds) * time.Second

	vs, err := conn.Vitals(ctx, interval)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest Vitals open failed: %w; check that the guest agent is healthy", err))
	}
	defer vs.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sample, recvErr := vs.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				// Guest stream ended normally (e.g. sandbox shut down).
				return nil
			}
			return connect.NewError(connect.CodeInternal, fmt.Errorf("read vitals stream: %w; the guest agent may have crashed or the vsock connection was lost", recvErr))
		}
		if err := stream.Send(sample); err != nil {
			return err
		}
	}
}
