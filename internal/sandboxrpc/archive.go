package sandboxrpc

// archive.go implements the Archive (server-stream) and Upload (client-stream)
// RPCs of the Sandbox Connect service, completing the Stage 2 runtime RPC
// surface for issue #24. Both methods delegate to the GuestConn port so the
// logic is exercised by the fake guest in tests without a real vsock connection
// or KVM guest.
//
// Archive(DOWNLOAD): tars a guest subtree and streams the bytes as Chunks.
// Upload: accepts a streamed tar and extracts it at a guest destination path.

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// ArchiveChunk is one frame from a guest Archive call. When Eof is true, this
// is the terminal frame and Data may be empty. No connect or proto types appear
// here; the Service maps it to sandboxv1.Chunk.
type ArchiveChunk struct {
	// Data holds tar bytes for this chunk (may be empty on the terminal frame).
	Data []byte
	// Eof is true on the terminal frame; the stream is exhausted after this.
	Eof bool
}

// ArchiveStream is the handle returned by GuestConn.Archive. Recv returns
// successive ArchiveChunk values with err == nil, including the terminal frame
// where Eof is true. After the terminal frame, a subsequent call returns
// io.EOF (the Service never makes that call). Other errors indicate a transport
// or guest failure. Close releases resources.
type ArchiveStream interface {
	// Recv returns the next ArchiveChunk with err == nil, including the terminal
	// frame where Eof is true. io.EOF is returned only on a subsequent call
	// after the terminal frame, which the Service never makes. Other errors are
	// transport or guest failures.
	Recv() (*ArchiveChunk, error)
	// Close releases resources. Safe to call after the terminal frame.
	Close() error
}

// UploadResult holds the outcome of an Upload guest call.
type UploadResult struct {
	// BytesWritten is the total number of bytes written to the guest filesystem
	// by extracting the uploaded tar archive.
	BytesWritten int64
}

// Archive tars the subtree at the requested path and streams the bytes as
// Chunk messages with the final Chunk carrying eof = true. Only the DOWNLOAD
// direction is implemented on this RPC; UNTAR and DIRECTION_UNSPECIFIED return
// CodeInvalidArgument directing the caller to use the Upload RPC instead.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Archive(ctx context.Context, req *connect.Request[sandboxv1.ArchiveRequest], stream *connect.ServerStream[sandboxv1.Chunk]) error {
	if s.Guest == nil {
		return followup("Archive")
	}

	dir := req.Msg.GetDirection()
	if dir != sandboxv1.ArchiveRequest_DOWNLOAD {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"Archive direction %v is not supported on this RPC: use the Upload RPC for untarring (UNTAR direction); "+
				"Archive only accepts DOWNLOAD to stream a tar of the given path", dir))
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connectErr(connect.CodeUnavailable,
			fmt.Errorf("open guest connection: %w", err),
			"ensure the sandbox is running and the guest agent is healthy")
	}

	as, err := conn.Archive(ctx, req.Msg.GetPath())
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("archive %q: %w; check the path exists and is readable inside the sandbox", req.Msg.GetPath(), err))
	}
	defer as.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chunk, recvErr := as.Recv()
		if recvErr != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("read archive stream: %w; the guest agent may have crashed or the vsock connection was lost", recvErr))
		}
		if err := stream.Send(&sandboxv1.Chunk{Data: chunk.Data, Eof: chunk.Eof}); err != nil {
			return err
		}
		if chunk.Eof {
			return nil
		}
	}
}

// Upload receives a streamed tar archive from the client and extracts it at
// the destination directory given in the opening frame. The first UploadRequest
// MUST carry the open oneof (dest); subsequent messages carry raw tar bytes as
// chunk. Returns UploadResult with the total bytes written.
//
// When s.Guest is nil the handler returns the honest #24 follow-up message.
func (s *Service) Upload(ctx context.Context, stream *connect.ClientStream[sandboxv1.UploadRequest]) (*connect.Response[sandboxv1.UploadResult], error) {
	if s.Guest == nil {
		return nil, followup("Upload")
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connectErr(connect.CodeUnavailable,
			fmt.Errorf("open guest connection: %w", err),
			"ensure the sandbox is running and the guest agent is healthy")
	}

	// Read the first message: must be the open frame.
	if !stream.Receive() {
		if serr := stream.Err(); serr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("Upload stream closed before the opening message: %w", serr))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("Upload stream closed before the opening message: the first UploadRequest must carry the open oneof (dest)"))
	}
	first := stream.Msg()
	open := first.GetOpen()
	if open == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first UploadRequest must carry the open oneof (dest), got a chunk frame"))
	}
	dest := open.GetDest()

	// Collect all chunk bytes and forward them to the guest via a channel so
	// the GuestConn.Upload signature stays clean (no proto types on the port).
	chunks := make(chan []byte)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunks)
		for stream.Receive() {
			data := stream.Msg().GetChunk()
			if len(data) > 0 {
				buf := make([]byte, len(data))
				copy(buf, data)
				select {
				case chunks <- buf:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
		}
		if serr := stream.Err(); serr != nil {
			errCh <- serr
		}
	}()

	result, uploadErr := conn.Upload(ctx, dest, chunks)

	// Drain any goroutine-reported stream error; it takes priority over the
	// guest error when both arrive (the stream error is the root cause).
	select {
	case streamErr := <-errCh:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read Upload stream: %w", streamErr))
	default:
	}

	if uploadErr != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("upload to %q: %w; check the destination path is writable inside the sandbox", dest, uploadErr))
	}
	return connect.NewResponse(&sandboxv1.UploadResult{
		BytesWritten: result.BytesWritten,
	}), nil
}
