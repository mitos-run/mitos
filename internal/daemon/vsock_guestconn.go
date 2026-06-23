package daemon

// vsock_guestconn.go: the REAL host-side sandboxrpc.GuestConn for issue #24
// stage 5. It dials the guest agent's gRPC server (vsock.AgentGRPCPort = 53)
// over the Firecracker vsock UDS and forwards every GuestConn method to the
// matching sandbox.v1.Sandbox RPC, mapping the gRPC stream/response shapes back
// to the transport-neutral GuestConn frame types.
//
// This replaces daemonGuestConn (the JSON-bridge stub) as forkd's Connect
// Sandbox GuestConn factory. The legacy JSON /v1/* handlers keep using the
// JSON path (RunExecStream over vsock.AgentPort = 52); the guest serves BOTH
// ports during the wire migration, so this change does not break the JSON SDK.
//
// Connection lifecycle (no goroutine or fd leak):
//   - Each GuestConn method opens a FRESH *grpc.ClientConn via dialGRPC, which
//     dials a new vsock stream to the guest gRPC port. Per-call dials are
//     acceptable here (the spike note, internal/vsock/grpcconn.go): the vsock
//     connection is local to the host and cheap, and a fresh conn per call keeps
//     the lifecycle trivially correct with no shared mutable state.
//   - UNARY calls (List, Stat, Mkdir, Remove, Processes, Signal, ReadFile,
//     WriteFile, Upload) close the *grpc.ClientConn before returning.
//   - STREAMING calls (Exec, RunCode, Archive, Vitals, Watch, PortForward)
//     transfer ownership of the *grpc.ClientConn to the returned stream handle;
//     the handle's Close() closes the conn, and the Connect Service ALWAYS calls
//     Close (defer in each Service handler). On a Recv error the handle keeps
//     the conn open until Close so the Service's error path is unaffected; the
//     Service then calls Close, tearing the conn down.
//
// Security: transport credentials are insecure because vsock is inside the
// Firecracker VM trust boundary (see internal/vsock/grpcconn.go). Secret env
// values forwarded to Exec are never logged here.

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/sandboxrpc"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// vsockGuestConn implements sandboxrpc.GuestConn by forwarding to the guest
// agent's gRPC server over vsock. One instance is created per Connect RPC by the
// SandboxAPI.Handler factory; the sandboxID is fixed at construction.
type vsockGuestConn struct {
	api       *SandboxAPI
	sandboxID string
}

// newVsockGuestConn returns a GuestConn backed by api's guest gRPC server for
// sandboxID.
func newVsockGuestConn(api *SandboxAPI, sandboxID string) sandboxrpc.GuestConn {
	return &vsockGuestConn{api: api, sandboxID: sandboxID}
}

// --- Exec ---

// Exec opens the guest Exec bidi stream, sends the ExecOpen, and returns an
// ExecStream that maps each ExecResponse frame to an ExecFrame. The terminal
// ExecExit maps to a Done frame with err == nil per the GuestConn contract.
func (g *vsockGuestConn) Exec(ctx context.Context, open *sandboxv1.ExecOpen) (sandboxrpc.ExecStream, error) {
	// Per-sandbox concurrent-stream cap: the Connect exec path counts against the
	// SAME ceiling the JSON /v1/exec/stream handler enforces, so a tenant cannot
	// open unbounded streams over Connect while JSON is capped. The slot is held
	// for the duration of the stream and released on Close.
	release, ok := g.api.acquireStream(g.sandboxID)
	if !ok {
		return nil, fmt.Errorf("sandbox %q is at its concurrent exec-stream limit; close an existing stream before opening another", g.sandboxID)
	}

	cc, client, err := g.connect()
	if err != nil {
		release()
		return nil, err
	}
	stream, err := client.Exec(ctx)
	if err != nil {
		_ = cc.Close()
		release()
		return nil, fmt.Errorf("open guest Exec stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: open}}); err != nil {
		_ = cc.Close()
		release()
		return nil, fmt.Errorf("send exec open: %w", err)
	}
	// No more client messages for a non-PTY exec: close the send direction so the
	// guest sees EOF on its input reader.
	_ = stream.CloseSend()
	return &grpcExecStream{cc: cc, stream: stream, release: release}, nil
}

// grpcExecStream adapts the guest Exec bidi gRPC stream to sandboxrpc.ExecStream.
// It owns the *grpc.ClientConn and the concurrent-stream slot; Close releases
// both. The guest emits stdout/stderr frames then a terminal ExecExit, which
// maps to a Done frame (err == nil) per the contract.
type grpcExecStream struct {
	cc      *grpc.ClientConn
	stream  grpc.BidiStreamingClient[sandboxv1.ExecRequest, sandboxv1.ExecResponse]
	release func()
	done    bool
}

func (s *grpcExecStream) Recv() (*sandboxrpc.ExecFrame, error) {
	if s.done {
		return nil, io.EOF
	}
	resp, err := s.stream.Recv()
	if err != nil {
		if err == io.EOF {
			// Stream closed before an exit frame: surface as an error, not a clean
			// Done, so the Service does not report a fake exit code 0.
			return nil, fmt.Errorf("guest Exec stream ended before exit frame")
		}
		return nil, fmt.Errorf("recv exec frame: %w", err)
	}
	switch m := resp.Msg.(type) {
	case *sandboxv1.ExecResponse_Stdout:
		return &sandboxrpc.ExecFrame{Stdout: m.Stdout}, nil
	case *sandboxv1.ExecResponse_Stderr:
		return &sandboxrpc.ExecFrame{Stderr: m.Stderr}, nil
	case *sandboxv1.ExecResponse_Exit:
		s.done = true
		if spawnErr := m.Exit.GetError(); spawnErr != "" {
			return nil, fmt.Errorf("guest exec failed: %s", spawnErr)
		}
		return &sandboxrpc.ExecFrame{Done: true, ExitCode: m.Exit.GetExitCode()}, nil
	default:
		return nil, fmt.Errorf("guest Exec sent an unexpected frame type")
	}
}

func (s *grpcExecStream) Close() error {
	if s.release != nil {
		s.release()
		s.release = nil
	}
	return s.cc.Close()
}

// --- File ops ---

// ReadFile streams the guest ReadFile RPC into a slice of chunks. The proto
// ReadFile reads the whole file (no offset/length on the wire), so partial reads
// requested via offset/length are sliced client-side from the assembled bytes.
func (g *vsockGuestConn) ReadFile(ctx context.Context, path string, offset, length int64) ([][]byte, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	defer cc.Close()

	stream, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("open guest ReadFile stream: %w", err)
	}
	var chunks [][]byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv read_file chunk: %w", err)
		}
		if len(chunk.GetData()) > 0 {
			buf := make([]byte, len(chunk.GetData()))
			copy(buf, chunk.GetData())
			chunks = append(chunks, buf)
		}
		if chunk.GetEof() {
			break
		}
	}

	// Apply offset/length client-side when requested (0,0 means whole file).
	if offset == 0 && length == 0 {
		return chunks, nil
	}
	return sliceChunks(chunks, offset, length), nil
}

// sliceChunks returns the [offset, offset+length) window of the concatenated
// chunks as a single chunk (or nil when the window is empty). length == 0 means
// "to end". This keeps the partial-read semantics the GuestConn contract
// promises even though the guest ReadFile RPC returns the whole file.
func sliceChunks(chunks [][]byte, offset, length int64) [][]byte {
	var total int64
	for _, c := range chunks {
		total += int64(len(c))
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return nil
	}
	end := total
	if length > 0 && offset+length < total {
		end = offset + length
	}
	flat := make([]byte, 0, end-offset)
	var pos int64
	for _, c := range chunks {
		cstart := pos
		cend := pos + int64(len(c))
		pos = cend
		if cend <= offset || cstart >= end {
			continue
		}
		lo := int64(0)
		if offset > cstart {
			lo = offset - cstart
		}
		hi := int64(len(c))
		if end < cend {
			hi = end - cstart
		}
		flat = append(flat, c[lo:hi]...)
	}
	if len(flat) == 0 {
		return nil
	}
	return [][]byte{flat}
}

// WriteFile streams the open frame plus data chunks to the guest WriteFile RPC.
func (g *vsockGuestConn) WriteFile(ctx context.Context, path string, mode uint32, chunks [][]byte) (*sandboxrpc.WriteFileResult, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	defer cc.Close()

	stream, err := client.WriteFile(ctx)
	if err != nil {
		return nil, fmt.Errorf("open guest WriteFile stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.WriteFileRequest{
		Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{Path: path, Mode: mode}},
	}); err != nil {
		return nil, fmt.Errorf("send write_file open: %w", err)
	}
	for _, c := range chunks {
		if len(c) == 0 {
			continue
		}
		if err := stream.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Data{Data: c}}); err != nil {
			return nil, fmt.Errorf("send write_file data: %w", err)
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("close write_file stream: %w", err)
	}
	return &sandboxrpc.WriteFileResult{BytesWritten: res.GetBytesWritten()}, nil
}

// List enumerates a directory via the guest List RPC and maps each FileInfo to
// the transport-neutral shape. The guest does not yet paginate, so pageToken is
// forwarded and the returned NextPageToken is whatever the guest reports.
func (g *vsockGuestConn) List(ctx context.Context, path string, pageSize int32, pageToken, filter string) (*sandboxrpc.ListResult, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	defer cc.Close()

	resp, err := client.List(ctx, &sandboxv1.ListRequest{
		Parent:    path,
		PageSize:  pageSize,
		PageToken: pageToken,
		Filter:    filter,
	})
	if err != nil {
		return nil, fmt.Errorf("guest List: %w", err)
	}
	out := &sandboxrpc.ListResult{NextPageToken: resp.GetNextPageToken()}
	for _, e := range resp.GetEntries() {
		out.Entries = append(out.Entries, fileInfoFromProto(e))
	}
	return out, nil
}

// Stat returns metadata for one path via the guest Stat RPC.
func (g *vsockGuestConn) Stat(ctx context.Context, path string) (*sandboxrpc.FileInfo, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	defer cc.Close()

	info, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("guest Stat: %w", err)
	}
	return fileInfoFromProto(info), nil
}

// fileInfoFromProto maps a proto FileInfo to the transport-neutral GuestConn
// FileInfo. No connect or proto types escape the GuestConn boundary.
func fileInfoFromProto(e *sandboxv1.FileInfo) *sandboxrpc.FileInfo {
	if e == nil {
		return nil
	}
	return &sandboxrpc.FileInfo{
		Name:           e.GetName(),
		Path:           e.GetPath(),
		IsDir:          e.GetIsDir(),
		Size:           e.GetSize(),
		Mode:           e.GetMode(),
		ModifiedAtUnix: e.GetModifiedAtUnix(),
	}
}

// Mkdir creates a directory via the guest Mkdir RPC. The proto Mkdir always
// creates parents (MkdirAll), so the recursive flag is accepted for both cases.
func (g *vsockGuestConn) Mkdir(ctx context.Context, path string, _ bool) error {
	cc, client, err := g.connect()
	if err != nil {
		return err
	}
	defer cc.Close()

	if _, err := client.Mkdir(ctx, &sandboxv1.MkdirRequest{Path: path}); err != nil {
		return fmt.Errorf("guest Mkdir: %w", err)
	}
	return nil
}

// Remove deletes a path via the guest Remove RPC. The proto Remove uses
// RemoveAll, so the recursive flag is accepted for both the single-entry and the
// tree case.
func (g *vsockGuestConn) Remove(ctx context.Context, path string, _ bool) error {
	cc, client, err := g.connect()
	if err != nil {
		return err
	}
	defer cc.Close()

	if _, err := client.Remove(ctx, &sandboxv1.RemoveRequest{Path: path}); err != nil {
		return fmt.Errorf("guest Remove: %w", err)
	}
	return nil
}

// --- RunCode ---

// RunCode opens the guest RunCode bidi stream, sends the RunCodeOpen, and returns
// a RunCodeStream mapping each RunCodeResponse frame to a RunCodeFrame.
func (g *vsockGuestConn) RunCode(ctx context.Context, open *sandboxv1.RunCodeOpen) (sandboxrpc.RunCodeStream, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	stream, err := client.RunCode(ctx)
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open guest RunCode stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.RunCodeRequest{Msg: &sandboxv1.RunCodeRequest_Open{Open: open}}); err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("send run_code open: %w", err)
	}
	_ = stream.CloseSend()
	return &grpcRunCodeStream{cc: cc, stream: stream}, nil
}

// grpcRunCodeStream adapts the guest RunCode bidi gRPC stream to
// sandboxrpc.RunCodeStream. It owns the *grpc.ClientConn; Close closes it.
type grpcRunCodeStream struct {
	cc     *grpc.ClientConn
	stream grpc.BidiStreamingClient[sandboxv1.RunCodeRequest, sandboxv1.RunCodeResponse]
	done   bool
}

func (s *grpcRunCodeStream) Recv() (*sandboxrpc.RunCodeFrame, error) {
	if s.done {
		return nil, io.EOF
	}
	resp, err := s.stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("guest RunCode stream ended before exit frame")
		}
		return nil, fmt.Errorf("recv run_code frame: %w", err)
	}
	switch m := resp.Msg.(type) {
	case *sandboxv1.RunCodeResponse_Stdout:
		return &sandboxrpc.RunCodeFrame{Kind: sandboxrpc.RunCodeFrameStdout, Stdout: m.Stdout}, nil
	case *sandboxv1.RunCodeResponse_Stderr:
		return &sandboxrpc.RunCodeFrame{Kind: sandboxrpc.RunCodeFrameStderr, Stderr: m.Stderr}, nil
	case *sandboxv1.RunCodeResponse_Result:
		res := &sandboxrpc.RunCodeResult{}
		if m.Result != nil {
			res.Text = m.Result.GetText()
			if data := m.Result.GetData(); len(data) > 0 {
				res.Data = make(map[string][]byte, len(data))
				for mime, payload := range data {
					res.Data[mime] = payload
				}
			}
		}
		return &sandboxrpc.RunCodeFrame{Kind: sandboxrpc.RunCodeFrameResult, Result: res}, nil
	case *sandboxv1.RunCodeResponse_Error:
		rerr := &sandboxrpc.RunCodeError{}
		if m.Error != nil {
			rerr.Name = m.Error.GetName()
			rerr.Value = m.Error.GetValue()
			rerr.Traceback = m.Error.GetTraceback()
		}
		return &sandboxrpc.RunCodeFrame{Kind: sandboxrpc.RunCodeFrameError, Error: rerr}, nil
	case *sandboxv1.RunCodeResponse_ExitCode:
		s.done = true
		return &sandboxrpc.RunCodeFrame{Kind: sandboxrpc.RunCodeFrameExit, ExitCode: m.ExitCode}, nil
	default:
		return nil, fmt.Errorf("guest RunCode sent an unexpected frame type")
	}
}

func (s *grpcRunCodeStream) Close() error { return s.cc.Close() }

// --- PortForward ---

// PortForward opens the guest PortForward bidi stream and sends the open frame
// with the target port. The returned stream proxies bytes both directions.
func (g *vsockGuestConn) PortForward(ctx context.Context, port uint32) (sandboxrpc.PortForwardStream, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	stream, err := client.PortForward(ctx)
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open guest PortForward stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: port}}}); err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("send port_forward open: %w", err)
	}
	return &grpcPortForwardStream{cc: cc, stream: stream}, nil
}

// grpcPortForwardStream adapts the guest PortForward bidi gRPC stream to
// sandboxrpc.PortForwardStream. It owns the *grpc.ClientConn; Close closes it.
type grpcPortForwardStream struct {
	cc     *grpc.ClientConn
	stream grpc.BidiStreamingClient[sandboxv1.Frame, sandboxv1.Frame]
	done   bool
}

func (s *grpcPortForwardStream) Recv() (*sandboxrpc.PortForwardFrame, error) {
	if s.done {
		return nil, io.EOF
	}
	frame, err := s.stream.Recv()
	if err != nil {
		if err == io.EOF {
			// Clean stream end without an explicit Close frame: report a terminal
			// Close so the Service forwards a close to its client.
			s.done = true
			return &sandboxrpc.PortForwardFrame{Close: true}, nil
		}
		return nil, fmt.Errorf("recv port_forward frame: %w", err)
	}
	switch m := frame.Msg.(type) {
	case *sandboxv1.Frame_Data:
		return &sandboxrpc.PortForwardFrame{Data: m.Data}, nil
	case *sandboxv1.Frame_Close:
		s.done = true
		return &sandboxrpc.PortForwardFrame{Close: true}, nil
	default:
		// An open frame from the guest, or an empty frame: ignore the payload and
		// treat it as a no-op data frame so the loop continues.
		return &sandboxrpc.PortForwardFrame{}, nil
	}
}

func (s *grpcPortForwardStream) Send(data []byte) error {
	return s.stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Data{Data: data}})
}

func (s *grpcPortForwardStream) Close() error { return s.cc.Close() }

// --- Vitals ---

// Vitals opens the guest Vitals server stream at the given interval. interval is
// converted to whole seconds (the proto carries seconds); a sub-second interval
// rounds up to 1 so the guest always streams.
func (g *vsockGuestConn) Vitals(ctx context.Context, interval time.Duration) (sandboxrpc.VitalsStream, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	seconds := int32(interval / time.Second)
	if seconds <= 0 {
		seconds = 1
	}
	stream, err := client.Vitals(ctx, &sandboxv1.VitalsRequest{IntervalSeconds: seconds})
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open guest Vitals stream: %w", err)
	}
	return &grpcVitalsStream{cc: cc, stream: stream}, nil
}

// grpcVitalsStream adapts the guest Vitals server stream. Recv returns io.EOF on
// normal stream end (the Service treats io.EOF as a clean close).
type grpcVitalsStream struct {
	cc     *grpc.ClientConn
	stream grpc.ServerStreamingClient[sandboxv1.GuestVitals]
}

func (s *grpcVitalsStream) Recv() (*sandboxv1.GuestVitals, error) {
	v, err := s.stream.Recv()
	if err != nil {
		// io.EOF is forwarded verbatim: the Vitals Service handler maps it to a
		// clean close.
		return nil, err
	}
	return v, nil
}

func (s *grpcVitalsStream) Close() error { return s.cc.Close() }

// --- Watch ---

// Watch opens the guest Watch server stream for the subtree at path.
func (g *vsockGuestConn) Watch(ctx context.Context, path string) (sandboxrpc.WatchStream, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	stream, err := client.Watch(ctx, &sandboxv1.WatchRequest{Path: path})
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open guest Watch stream: %w", err)
	}
	return &grpcWatchStream{cc: cc, stream: stream}, nil
}

// grpcWatchStream adapts the guest Watch server stream. Recv returns io.EOF on
// normal stream end (the Service treats io.EOF as a clean close).
type grpcWatchStream struct {
	cc     *grpc.ClientConn
	stream grpc.ServerStreamingClient[sandboxv1.FsEvent]
}

func (s *grpcWatchStream) Recv() (*sandboxv1.FsEvent, error) {
	return s.stream.Recv()
}

func (s *grpcWatchStream) Close() error { return s.cc.Close() }

// --- Processes / Signal ---

// Processes returns the guest process table via the unary Processes RPC.
func (g *vsockGuestConn) Processes(ctx context.Context) (*sandboxv1.ProcessList, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	defer cc.Close()

	list, err := client.Processes(ctx, &sandboxv1.ProcessesRequest{})
	if err != nil {
		return nil, fmt.Errorf("guest Processes: %w", err)
	}
	return list, nil
}

// Signal delivers a POSIX signal to a guest process via the unary Signal RPC.
func (g *vsockGuestConn) Signal(ctx context.Context, pid, signal int32) error {
	cc, client, err := g.connect()
	if err != nil {
		return err
	}
	defer cc.Close()

	if _, err := client.Signal(ctx, &sandboxv1.SignalRequest{Pid: pid, Signal: signal}); err != nil {
		return fmt.Errorf("guest Signal: %w", err)
	}
	return nil
}

// --- Archive / Upload ---

// Archive opens the guest Archive (DOWNLOAD) server stream for the subtree at
// path and returns an ArchiveStream of tar chunks.
func (g *vsockGuestConn) Archive(ctx context.Context, path string) (sandboxrpc.ArchiveStream, error) {
	cc, client, err := g.connect()
	if err != nil {
		return nil, err
	}
	stream, err := client.Archive(ctx, &sandboxv1.ArchiveRequest{
		Path:      path,
		Direction: sandboxv1.ArchiveRequest_DOWNLOAD,
	})
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open guest Archive stream: %w", err)
	}
	return &grpcArchiveStream{cc: cc, stream: stream}, nil
}

// grpcArchiveStream adapts the guest Archive server stream to
// sandboxrpc.ArchiveStream. It owns the *grpc.ClientConn; Close closes it. The
// guest emits Chunk frames ending with eof = true.
type grpcArchiveStream struct {
	cc     *grpc.ClientConn
	stream grpc.ServerStreamingClient[sandboxv1.Chunk]
	done   bool
}

func (s *grpcArchiveStream) Recv() (*sandboxrpc.ArchiveChunk, error) {
	if s.done {
		return nil, io.EOF
	}
	chunk, err := s.stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("guest Archive stream ended before the eof chunk")
		}
		return nil, fmt.Errorf("recv archive chunk: %w", err)
	}
	if chunk.GetEof() {
		s.done = true
		return &sandboxrpc.ArchiveChunk{Data: chunk.GetData(), Eof: true}, nil
	}
	return &sandboxrpc.ArchiveChunk{Data: chunk.GetData()}, nil
}

func (s *grpcArchiveStream) Close() error { return s.cc.Close() }

// Upload streams tar bytes from chunks to the guest Upload client stream and
// returns the total bytes written. It drains chunks fully (per the contract) so
// the Service's upstream goroutine exits cleanly even on an early guest error.
func (g *vsockGuestConn) Upload(ctx context.Context, dest string, chunks <-chan []byte) (*sandboxrpc.UploadResult, error) {
	cc, client, err := g.connect()
	if err != nil {
		drainChunks(chunks)
		return nil, err
	}
	defer cc.Close()

	stream, err := client.Upload(ctx)
	if err != nil {
		drainChunks(chunks)
		return nil, fmt.Errorf("open guest Upload stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: dest}},
	}); err != nil {
		drainChunks(chunks)
		return nil, fmt.Errorf("send upload open: %w", err)
	}
	for c := range chunks {
		if len(c) == 0 {
			continue
		}
		if err := stream.Send(&sandboxv1.UploadRequest{Msg: &sandboxv1.UploadRequest_Chunk{Chunk: c}}); err != nil {
			// Drain the rest so the Service's reader goroutine is not blocked on a
			// full unbuffered channel after this error.
			drainChunks(chunks)
			return nil, fmt.Errorf("send upload chunk: %w", err)
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("close upload stream: %w", err)
	}
	return &sandboxrpc.UploadResult{BytesWritten: res.GetBytesWritten()}, nil
}

// drainChunks consumes the rest of chunks so the upstream sender goroutine can
// exit cleanly even when Upload returns early on an error.
func drainChunks(chunks <-chan []byte) {
	for range chunks {
	}
}

// connect dials a fresh gRPC client to the sandbox's guest gRPC server and
// returns the *grpc.ClientConn plus a SandboxClient bound to it. The caller owns
// the conn and must Close it (directly for unary calls, or via the returned
// stream handle's Close for streaming calls).
func (g *vsockGuestConn) connect() (*grpc.ClientConn, sandboxv1.SandboxClient, error) {
	cc, err := vsock.DialGRPCOverConn(func() (net.Conn, error) {
		return g.api.dialGuestGRPC(g.sandboxID)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dial guest gRPC for sandbox %q: %w", g.sandboxID, err)
	}
	return cc, sandboxv1.NewSandboxClient(cc), nil
}
