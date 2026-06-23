package sandboxrpc

// files.go implements the six file RPCs of the Sandbox Connect service (Task
// 2.2): ReadFile, WriteFile, List, Stat, Mkdir, Remove. Each method delegates
// to the GuestConn port so the logic is exercised by the fake guest in tests
// without a real vsock connection or KVM guest.

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// ReadFile streams the file content as a sequence of Chunk messages, with the
// final message carrying eof=true. The GuestConn.ReadFile call returns all
// chunks at once; each is forwarded to the client stream immediately.
func (s *Service) ReadFile(ctx context.Context, req *connect.Request[sandboxv1.ReadFileRequest], stream *connect.ServerStream[sandboxv1.Chunk]) error {
	if s.Guest == nil {
		return followup("ReadFile")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	chunks, err := conn.ReadFile(ctx, req.Msg.GetPath(), 0, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("read file %q: %w; check the path exists and is readable inside the sandbox", req.Msg.GetPath(), err))
	}

	for i, data := range chunks {
		isLast := i == len(chunks)-1
		if err := stream.Send(&sandboxv1.Chunk{Data: data, Eof: isLast}); err != nil {
			return err
		}
	}
	// When the guest returned no chunks, send a single empty EOF frame so the
	// client always receives the terminal Chunk.
	if len(chunks) == 0 {
		if err := stream.Send(&sandboxv1.Chunk{Eof: true}); err != nil {
			return err
		}
	}
	return nil
}

// WriteFile collects all WriteFileRequest messages from the client stream. The
// first message MUST carry the open oneof (path, mode); subsequent messages
// carry data chunks. All data bytes are forwarded to GuestConn.WriteFile as a
// single call, and the total bytes_written is returned in the response.
func (s *Service) WriteFile(ctx context.Context, stream *connect.ClientStream[sandboxv1.WriteFileRequest]) (*connect.Response[sandboxv1.WriteFileResult], error) {
	if s.Guest == nil {
		return nil, followup("WriteFile")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	// Read the first message: must be the open frame.
	if !stream.Receive() {
		if serr := stream.Err(); serr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("WriteFile stream closed before the opening message: %w", serr))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("WriteFile stream closed before the opening message: the first WriteFileRequest must carry the open oneof (path, mode)"))
	}
	first := stream.Msg()
	open := first.GetOpen()
	if open == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first WriteFileRequest must carry the open oneof (path, mode), got a data frame"))
	}
	path := open.GetPath()

	// Collect all data chunks.
	var chunks [][]byte
	for stream.Receive() {
		data := stream.Msg().GetData()
		if len(data) > 0 {
			buf := make([]byte, len(data))
			copy(buf, data)
			chunks = append(chunks, buf)
		}
	}
	if serr := stream.Err(); serr != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read WriteFile stream: %w", serr))
	}

	result, err := conn.WriteFile(ctx, path, chunks)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("write file %q: %w; check the path is writable inside the sandbox", path, err))
	}
	return connect.NewResponse(&sandboxv1.WriteFileResult{
		BytesWritten: result.BytesWritten,
	}), nil
}

// List enumerates a directory with AIP-158 pagination, delegating to
// GuestConn.List and mapping the result to the proto ListResponse.
func (s *Service) List(ctx context.Context, req *connect.Request[sandboxv1.ListRequest]) (*connect.Response[sandboxv1.ListResponse], error) {
	if s.Guest == nil {
		return nil, followup("List")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	result, err := conn.List(ctx, req.Msg.GetParent(), req.Msg.GetPageSize(), req.Msg.GetPageToken(), req.Msg.GetFilter())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list %q: %w; check the path exists and is a directory inside the sandbox", req.Msg.GetParent(), err))
	}

	entries := make([]*sandboxv1.FileInfo, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, fileInfoToProto(e))
	}
	return connect.NewResponse(&sandboxv1.ListResponse{
		Entries:       entries,
		NextPageToken: result.NextPageToken,
	}), nil
}

// Stat returns metadata for one path, delegating to GuestConn.Stat and mapping
// the result to the proto FileInfo.
func (s *Service) Stat(ctx context.Context, req *connect.Request[sandboxv1.StatRequest]) (*connect.Response[sandboxv1.FileInfo], error) {
	if s.Guest == nil {
		return nil, followup("Stat")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	info, err := conn.Stat(ctx, req.Msg.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stat %q: %w; check the path exists inside the sandbox", req.Msg.GetPath(), err))
	}
	return connect.NewResponse(fileInfoToProto(info)), nil
}

// Mkdir creates a directory (and parents) at the given path, delegating to
// GuestConn.Mkdir and returning an empty MkdirResponse on success.
func (s *Service) Mkdir(ctx context.Context, req *connect.Request[sandboxv1.MkdirRequest]) (*connect.Response[sandboxv1.MkdirResponse], error) {
	if s.Guest == nil {
		return nil, followup("Mkdir")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	// The proto MkdirRequest has only path (no recursive field); recursive
	// defaults to true (mkdir -p semantics) to match the expected behavior.
	if err := conn.Mkdir(ctx, req.Msg.GetPath(), true); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mkdir %q: %w; check the parent path is writable inside the sandbox", req.Msg.GetPath(), err))
	}
	return connect.NewResponse(&sandboxv1.MkdirResponse{}), nil
}

// Remove deletes a file or directory, delegating to GuestConn.Remove and
// returning an empty RemoveResponse on success.
func (s *Service) Remove(ctx context.Context, req *connect.Request[sandboxv1.RemoveRequest]) (*connect.Response[sandboxv1.RemoveResponse], error) {
	if s.Guest == nil {
		return nil, followup("Remove")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	conn, err := s.Guest(sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection: %w; ensure the sandbox is running and the guest agent is healthy", err))
	}

	if err := conn.Remove(ctx, req.Msg.GetPath(), req.Msg.GetRecursive()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove %q: %w; check the path exists and is removable inside the sandbox", req.Msg.GetPath(), err))
	}
	return connect.NewResponse(&sandboxv1.RemoveResponse{}), nil
}

// fileInfoToProto converts a GuestConn FileInfo (Go primitives) to the proto
// FileInfo message. This mapping is the only place where local types cross into
// proto types, keeping the GuestConn interface free of proto dependencies.
func fileInfoToProto(fi *FileInfo) *sandboxv1.FileInfo {
	if fi == nil {
		return &sandboxv1.FileInfo{}
	}
	return &sandboxv1.FileInfo{
		Name:           fi.Name,
		Path:           fi.Path,
		IsDir:          fi.IsDir,
		Size:           fi.Size,
		Mode:           fi.Mode,
		ModifiedAtUnix: fi.ModifiedAtUnix,
	}
}
