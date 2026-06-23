//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// grpc_server.go adds the gRPC runtime protocol to the guest agent. It runs on
// a SEPARATE vsock port (vsock.AgentGRPCPort = 53) ALONGSIDE the legacy
// JSON-lines accept loop on vsock.AgentPort, which remains in force during the
// wire migration (issue #24). Both the public sandbox.v1.Sandbox service and
// the host-trusted sandbox.internal.v1.Control service are served on this one
// gRPC server: inside the VM the vsock channel is reachable only by the host
// (forkd) over Firecracker's virtio-vsock, so colocating them on one in-guest
// port does not widen exposure. forkd routes Configure/NotifyForked to this
// internal-only channel and never re-exposes Control on its public :9091 edge.
//
// Transport credentials are insecure: the microVM boundary is the isolation,
// the exact same posture as the JSON-lines path, which has no in-guest auth.
// vsock is not reachable from tenant code in other sandboxes, the host network,
// or the internet. mTLS over vsock is a later hardening slice
// (docs/threat-model.md, internal/vsock/grpcconn.go).

// newGuestGRPCServer builds the grpc.Server with both guest services
// registered. It is split from the listen/serve wiring so a test can register
// the same services on its own listener.
func newGuestGRPCServer() *grpc.Server {
	s := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(s, &sandboxServer{})
	internalv1.RegisterControlServer(s, &controlServer{})
	return s
}

// startGRPCServer listens on the dedicated gRPC vsock port and serves the
// runtime protocol. It is best-effort and non-fatal, like the self-service
// socket: a listen failure is logged and the legacy JSON-lines loop is
// unaffected. It blocks in Serve, so callers run it in a goroutine.
func startGRPCServer() {
	listener, err := listenVsock(vsock.AgentGRPCPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: gRPC listen error: %v\n", err)
		return
	}
	srv := newGuestGRPCServer()
	fmt.Println("sandbox-agent: gRPC runtime protocol ready on vsock port", vsock.AgentGRPCPort)
	if err := srv.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: gRPC serve error: %v\n", err)
	}
}

// sandboxServer implements sandbox.v1.Sandbox by REUSING the existing guest
// handlers. This slice (Task 5.1b) wires the reuse-able RPCs: Exec (including
// the PTY path via open.pty), ReadFile, WriteFile, List, Stat, Mkdir, Remove,
// Archive (download), Upload (untar), RunCode, Vitals, and PortForward. Each
// calls the same logic the JSON-lines path uses, so the path sanitization, the
// workspace allowlist, the env merge, the kernel state, and the no-secret-log
// invariants are byte-for-byte identical. Watch, Processes, and Signal remain
// Unimplemented (Task 5.1c); Fork, Checkpoint, ExtendLifetime, and Budget remain
// Unimplemented (Stage 8). Embedding by value is required by the generated
// forward-compat contract.
type sandboxServer struct {
	sandboxv1.UnimplementedSandboxServer
}

// grpcExecSink adapts the shared exec engine (runExecStream) to the gRPC Exec
// reply stream. It reuses the exact spawn/env/process-group/exit-code logic of
// the JSON path; only the emission target differs. Sink calls are already
// serialized by runExecStream's mutex, so stream.Send is never called
// concurrently. A send error is recorded so the engine's later emissions become
// no-ops and the RPC returns it.
type grpcExecSink struct {
	stream sandboxv1.Sandbox_ExecServer
	mu     sync.Mutex
	err    error
}

func (s *grpcExecSink) chunk(stream vsock.StreamName, data []byte) {
	var msg *sandboxv1.ExecResponse
	if stream == vsock.StreamStderr {
		msg = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: data}}
	} else {
		msg = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: data}}
	}
	s.send(msg)
}

func (s *grpcExecSink) exit(exitCode int, execTimeMs float64, spawnErr string) {
	s.send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
		ExitCode:   int32(exitCode),
		ExecTimeMs: execTimeMs,
		Error:      spawnErr,
	}}})
}

func (s *grpcExecSink) send(msg *sandboxv1.ExecResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if err := s.stream.Send(msg); err != nil {
		s.err = err
	}
}

func (s *grpcExecSink) sendErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Exec runs a command and streams its IO over the bidi stream. The first client
// message MUST carry the open oneof. A non-PTY exec REUSES runExecStream, the
// same engine the JSON /v1/exec/stream path uses, so the env merge (configured
// env + per-call env), working dir, process-group kill on cancel/timeout, and
// exit-code mapping are byte-for-byte identical and secret env values are never
// logged. A PTY exec (open.pty set) REUSES the shared runPTY engine through a
// gRPC transport, so the same session-leader spawn, env merge, kill-group, and
// resize logic the JSON PTY path uses drive the interactive terminal. The
// client's ctx cancel (hang-up) propagates into both engines to kill the process
// tree. argv (shell-less) exec is deferred to a follow-up slice and returns
// Unimplemented.
func (s *sandboxServer) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "exec: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "exec: first message must carry the open oneof")
	}
	if len(open.GetArgs()) > 0 {
		return status.Error(codes.Unimplemented, "exec: argv (shell-less) exec is not implemented in this slice; pass the command as a single shell string")
	}
	if open.GetPty() != nil {
		return s.execPTY(stream, open)
	}

	// Translate the open frame into the existing vsock.ExecRequest the shared
	// engine consumes. Env values may be secret and are copied verbatim into the
	// process environment without being logged.
	req := &vsock.ExecRequest{
		Command:    open.GetCommand(),
		WorkingDir: open.GetCwd(),
		Timeout:    int(open.GetTimeoutSeconds()),
		Env:        envVarsToMap(open.GetEnv()),
	}

	sink := &grpcExecSink{stream: stream}
	runExecStream(stream.Context(), req, sink)
	if sendErr := sink.sendErr(); sendErr != nil {
		return status.Errorf(codes.Unavailable, "exec: stream send failed: %v", sendErr)
	}
	return nil
}

// execPTY drives an interactive PTY exec over the gRPC Exec bidi stream by
// REUSING the shared runPTY engine. The grpcPtyTransport translates the wire
// shapes: ExecResponse stdout frames carry PTY output (the merged terminal
// stream), the ExecExit frame carries the terminal exit, and the reader pulls
// stdin (-> PTY input) and resize frames from the client. Output is on the
// merged stdout stream per the proto contract. The stream's ctx cancel makes the
// blocked Recv return an error, which the transport maps to ptyInputEOF so
// runPTY kills the shell group and joins; no goroutine leaks on client cancel.
func (s *sandboxServer) execPTY(stream sandboxv1.Sandbox_ExecServer, open *sandboxv1.ExecOpen) error {
	var cols, rows int
	if size := open.GetPty().GetSize(); size != nil {
		cols = int(size.GetCols())
		rows = int(size.GetRows())
	}
	t := &grpcPtyTransport{stream: stream}
	runPTY(ptyParams{
		Command:    open.GetCommand(),
		WorkingDir: open.GetCwd(),
		Env:        envVarsToMap(open.GetEnv()),
		Cols:       cols,
		Rows:       rows,
	}, t)
	if sendErr := t.sendErr(); sendErr != nil {
		return status.Errorf(codes.Unavailable, "exec: pty stream send failed: %v", sendErr)
	}
	return nil
}

// grpcPtyTransport adapts the shared runPTY engine to the gRPC Exec stream. PTY
// output is sent as ExecResponse stdout (the merged terminal stream, per the
// proto). exit is the terminal ExecExit. input blocks on stream.Recv(); a recv
// error (client hang-up or stream ctx cancel) returns ptyInputEOF so runPTY
// kills the shell group. Output and exit Sends are serialized by runPTY (the
// reader goroutine never sends), so no send mutex is needed; a send error is
// recorded so later emissions are no-ops and the RPC returns it.
type grpcPtyTransport struct {
	stream sandboxv1.Sandbox_ExecServer
	mu     sync.Mutex
	err    error
}

func (t *grpcPtyTransport) output(data []byte) {
	t.send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: data}})
}

func (t *grpcPtyTransport) exit(exitCode int, spawnErr string) {
	t.send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
		ExitCode: int32(exitCode),
		Error:    spawnErr,
	}}})
}

func (t *grpcPtyTransport) send(msg *sandboxv1.ExecResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return
	}
	if err := t.stream.Send(msg); err != nil {
		t.err = err
	}
}

func (t *grpcPtyTransport) sendErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *grpcPtyTransport) input() (kind ptyInputKind, data []byte, cols, rows int) {
	for {
		req, err := t.stream.Recv()
		if err != nil {
			// EOF (client CloseSend) or any recv error (ctx cancel) ends input.
			return ptyInputEOF, nil, 0, 0
		}
		switch m := req.Msg.(type) {
		case *sandboxv1.ExecRequest_Stdin:
			return ptyInputData, m.Stdin, 0, 0
		case *sandboxv1.ExecRequest_Resize:
			return ptyInputResize, nil, int(m.Resize.GetCols()), int(m.Resize.GetRows())
		default:
			// stdin_close or a stray open: ignore and read the next frame.
		}
	}
}

// envVarsToMap flattens the repeated EnvVar list into the map shape the shared
// exec engine and guestenv.Merge consume. On a duplicate key the last entry
// wins, matching map-merge semantics. Values may be secret and are never logged.
func envVarsToMap(vars []*sandboxv1.EnvVar) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		if v == nil {
			continue
		}
		m[v.GetKey()] = v.GetValue()
	}
	return m
}

// grpcChunkBytes bounds one streamed Chunk on ReadFile and Archive. 32 KiB
// keeps each frame small relative to the gRPC default max message size and
// matches the exec/pty output chunking.
const grpcChunkBytes = 32 << 10

// ReadFile streams one file's bytes as Chunk frames, ending with an eof Chunk.
// It REUSES handleReadFile so the read semantics and error mapping match the
// JSON path; the bytes are then re-framed into bounded chunks for the stream.
// File content is never logged. A read failure becomes a NotFound/Internal
// error carrying only the OS error string (no secret value).
func (s *sandboxServer) ReadFile(req *sandboxv1.ReadFileRequest, stream sandboxv1.Sandbox_ReadFileServer) error {
	resp := handleReadFile(&vsock.ReadFileRequest{Path: req.GetPath()})
	if !resp.OK {
		return status.Errorf(codes.Internal, "read_file: %s", resp.Error)
	}
	data := resp.ReadFile.Content
	for len(data) > 0 {
		n := len(data)
		if n > grpcChunkBytes {
			n = grpcChunkBytes
		}
		if err := stream.Send(&sandboxv1.Chunk{Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}

// WriteFile accumulates the streamed open + data chunks and REUSES
// handleWriteFile, so the parent-dir creation, the default mode, and the error
// mapping are identical to the JSON path. File content is never logged. The
// first message must carry open; a missing open is InvalidArgument.
func (s *sandboxServer) WriteFile(stream sandboxv1.Sandbox_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "write_file: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "write_file: first message must carry the open oneof")
	}
	var content []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Aborted, "write_file: recv: %v", err)
		}
		if d := msg.GetData(); d != nil {
			content = append(content, d...)
		}
	}
	resp := handleWriteFile(&vsock.WriteFileRequest{
		Path:    open.GetPath(),
		Content: content,
		Mode:    open.GetMode(),
	})
	if !resp.OK {
		return status.Errorf(codes.Internal, "write_file: %s", resp.Error)
	}
	return stream.SendAndClose(&sandboxv1.WriteFileResult{BytesWritten: int64(len(content))})
}

// List enumerates a directory. It REUSES handleListDir so the entry metadata
// matches the JSON path. Pagination and filtering (AIP-158/160) are not applied
// in this slice: the full listing is returned with an empty next_page_token, the
// honest behavior until the paging slice lands. No path content is logged.
func (s *sandboxServer) List(_ context.Context, req *sandboxv1.ListRequest) (*sandboxv1.ListResponse, error) {
	resp := handleListDir(&vsock.ListDirRequest{Path: req.GetParent()})
	if !resp.OK {
		return nil, status.Errorf(codes.Internal, "list: %s", resp.Error)
	}
	out := &sandboxv1.ListResponse{}
	for _, e := range resp.ListDir.Entries {
		out.Entries = append(out.Entries, &sandboxv1.FileInfo{
			Name:           e.Name,
			Path:           filepath.Join(req.GetParent(), e.Name),
			IsDir:          e.IsDir,
			Size:           e.Size,
			Mode:           e.Mode,
			ModifiedAtUnix: e.ModifiedAt,
		})
	}
	return out, nil
}

// Stat returns metadata for one path without reading its contents. It uses
// os.Lstat (no content read, no secret exposure) and maps the result into the
// FileInfo shape List returns.
func (s *sandboxServer) Stat(_ context.Context, req *sandboxv1.StatRequest) (*sandboxv1.FileInfo, error) {
	info, err := os.Lstat(req.GetPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "stat: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "stat: %v", err)
	}
	return &sandboxv1.FileInfo{
		Name:           info.Name(),
		Path:           req.GetPath(),
		IsDir:          info.IsDir(),
		Size:           info.Size(),
		Mode:           uint32(info.Mode()),
		ModifiedAtUnix: info.ModTime().Unix(),
	}, nil
}

// Mkdir creates a directory and parents. It REUSES the exact os.MkdirAll call
// and mode (0o755) the JSON TypeMkdir handler uses.
func (s *sandboxServer) Mkdir(_ context.Context, req *sandboxv1.MkdirRequest) (*sandboxv1.MkdirResponse, error) {
	if err := os.MkdirAll(req.GetPath(), 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir: %v", err)
	}
	return &sandboxv1.MkdirResponse{}, nil
}

// Remove deletes a path. It REUSES os.RemoveAll, matching the JSON TypeRemove
// handler; RemoveAll already removes a tree, so the recursive flag is accepted
// for the non-recursive single-entry case and the tree case alike.
func (s *sandboxServer) Remove(_ context.Context, req *sandboxv1.RemoveRequest) (*sandboxv1.RemoveResponse, error) {
	if err := os.RemoveAll(req.GetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "remove: %v", err)
	}
	return &sandboxv1.RemoveResponse{}, nil
}

// Archive tars a subtree and streams the tar as Chunk frames (DOWNLOAD). It
// REUSES handleTarDir, so the /workspace transfer allowlist (pathAllowed), the
// symlink skipping, and the MaxTarBytes bound are enforced exactly as on the
// JSON path: an out-of-allowlist path is refused with PermissionDenied. The
// symmetric UNTAR direction is served by the Upload RPC, so Archive with UNTAR
// is rejected with InvalidArgument here.
func (s *sandboxServer) Archive(req *sandboxv1.ArchiveRequest, stream sandboxv1.Sandbox_ArchiveServer) error {
	if req.GetDirection() == sandboxv1.ArchiveRequest_UNTAR {
		return status.Error(codes.InvalidArgument, "archive: UNTAR direction is served by the Upload RPC; use Upload to extract a tar")
	}
	resp := handleTarDir(&vsock.TarDirRequest{Path: req.GetPath()})
	if !resp.OK {
		// The allowlist refusal and the size-bound refusal both surface here; the
		// message names the non-secret reason from the reused handler.
		return status.Errorf(codes.PermissionDenied, "archive: %s", resp.Error)
	}
	data := resp.TarDir.Tar
	for len(data) > 0 {
		n := len(data)
		if n > grpcChunkBytes {
			n = grpcChunkBytes
		}
		if err := stream.Send(&sandboxv1.Chunk{Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}

// Upload accepts a streamed tar and extracts it at the destination. It
// accumulates the open + chunk messages and REUSES handleUntarDir, so the
// /workspace allowlist (pathAllowed), the safeJoin traversal barrier, the
// symlink/device rejection, and the MaxTarBytes bound are enforced exactly as on
// the JSON path: an out-of-allowlist dest or a traversing member is refused. No
// archive bytes are logged.
func (s *sandboxServer) Upload(stream sandboxv1.Sandbox_UploadServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "upload: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "upload: first message must carry the open oneof")
	}
	var tarBytes []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Aborted, "upload: recv: %v", err)
		}
		if c := msg.GetChunk(); c != nil {
			tarBytes = append(tarBytes, c...)
		}
	}
	resp := handleUntarDir(&vsock.UntarDirRequest{Path: open.GetDest(), Tar: tarBytes})
	if !resp.OK {
		return status.Errorf(codes.PermissionDenied, "upload: %s", resp.Error)
	}
	return stream.SendAndClose(&sandboxv1.UploadResult{BytesWritten: int64(len(tarBytes))})
}

// RunCode runs a code snippet in the per-sandbox kernel and streams the result.
// It REUSES the package guestKernel via the same run() entry the JSON
// handleRunCodeStream uses, so the kernel state persists across calls, the
// language gate and the KernelUnavailable remediation are identical, and the
// rich result/error frames map to RunResult/RunError. The first message must
// carry open. The stream's ctx cancel propagates: run() is synchronous under the
// kernel mutex, so no goroutine outlives this call. Code and output are never
// logged.
func (s *sandboxServer) RunCode(stream sandboxv1.Sandbox_RunCodeServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "run_code: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "run_code: first message must carry the open oneof")
	}

	var sendErr error
	emit := func(fr vsock.ExecStreamFrame) {
		if sendErr != nil {
			return
		}
		sendErr = stream.Send(runCodeResponseFromFrame(fr))
	}
	if runErr := guestKernel.run(open.GetCode(), open.GetLanguage(), int(open.GetTimeoutSeconds()), emit); runErr != nil {
		// A transport failure mid-stream: surface a kernel error frame then exit 1,
		// matching the JSON handleRunCodeStream tail. runErr carries no secret.
		emit(vsock.ExecStreamFrame{Kind: vsock.FrameError, ErrorInfo: &vsock.ErrorFrame{Name: "KernelStreamError", Value: runErr.Error()}})
		emit(vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 1})
	}
	if sendErr != nil {
		return status.Errorf(codes.Unavailable, "run_code: stream send failed: %v", sendErr)
	}
	return nil
}

// runCodeResponseFromFrame maps one kernel ExecStreamFrame to the proto
// RunCodeResponse. The rich result data (MIME -> bytes) and the error
// name/value/traceback are carried verbatim; chunk frames map to stdout/stderr;
// the exit frame carries the exit code.
func runCodeResponseFromFrame(fr vsock.ExecStreamFrame) *sandboxv1.RunCodeResponse {
	switch fr.Kind {
	case vsock.FrameChunk:
		if fr.Stream == vsock.StreamStderr {
			return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_Stderr{Stderr: fr.Data}}
		}
		return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_Stdout{Stdout: fr.Data}}
	case vsock.FrameResult:
		res := &sandboxv1.RunResult{}
		if fr.Result != nil {
			res.Text = fr.Result.Text
			if len(fr.Result.Data) > 0 {
				res.Data = make(map[string][]byte, len(fr.Result.Data))
				for mime, payload := range fr.Result.Data {
					res.Data[mime] = []byte(payload)
				}
			}
		}
		return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_Result{Result: res}}
	case vsock.FrameError:
		rerr := &sandboxv1.RunError{}
		if fr.ErrorInfo != nil {
			rerr.Name = fr.ErrorInfo.Name
			rerr.Value = fr.ErrorInfo.Value
			rerr.Traceback = fr.ErrorInfo.Traceback
		}
		return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_Error{Error: rerr}}
	default: // vsock.FrameExit
		return &sandboxv1.RunCodeResponse{Msg: &sandboxv1.RunCodeResponse_ExitCode{ExitCode: int32(fr.ExitCode)}}
	}
}

// Vitals streams guest health samples. It REUSES sampleVitals (the same
// collector handleVitals uses) so the steal/memory/process snapshot is
// identical to the JSON path. interval <= 0 yields a single sample then closes;
// otherwise it samples on a ticker until the client cancels. The stream ctx
// terminates the ticker loop and the goroutine, so nothing leaks on cancel.
func (s *sandboxServer) Vitals(req *sandboxv1.VitalsRequest, stream sandboxv1.Sandbox_VitalsServer) error {
	ctx := stream.Context()
	sendOne := func() error {
		v, err := sampleVitals()
		if err != nil {
			return status.Errorf(codes.Internal, "vitals: %v", err)
		}
		return stream.Send(guestVitalsFromResponse(v))
	}
	if req.GetIntervalSeconds() <= 0 {
		return sendOne()
	}
	ticker := time.NewTicker(time.Duration(req.GetIntervalSeconds()) * time.Second)
	defer ticker.Stop()
	// Emit the first sample immediately so a client sees data before one interval.
	if err := sendOne(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sendOne(); err != nil {
				return err
			}
		}
	}
}

// guestVitalsFromResponse maps the internal vsock vitals snapshot to the proto
// GuestVitals. KB fields are converted to bytes; the steal fraction is reported
// as a percent. No secret data is involved.
func guestVitalsFromResponse(v *vsock.VitalsResponse) *sandboxv1.GuestVitals {
	return &sandboxv1.GuestVitals{
		SampledAtUnix:   time.Now().Unix(),
		CpuStealPercent: v.StealFraction * 100,
		MemUsedBytes:    int64(v.MemUsedKB) * 1024,
		MemTotalBytes:   int64(v.MemTotalKB) * 1024,
		MemBalloonBytes: int64(v.BalloonReclaimedKB) * 1024,
		ProcessCount:    int32(len(v.Processes)),
	}
}

// PortForward proxies a guest TCP port over the bidi byte stream. The first
// client Frame MUST carry open with the port; the guest then REUSES the tunnel
// dial restriction by dialing 127.0.0.1:<port> ONLY (never an arbitrary host),
// matching handleTunnel. After a successful dial it splices bytes both
// directions: client data frames -> guest socket, guest socket -> server data
// frames. Both directions close the guest socket and signal each other on EOF,
// and the stream ctx cancel closes the guest socket so both goroutines join: no
// leak or deadlock on client cancel.
func (s *sandboxServer) PortForward(stream sandboxv1.Sandbox_PortForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "port_forward: first message recv: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "port_forward: first message must carry the open oneof")
	}
	port := int(open.GetPort())
	if port < 1 || port > 65535 {
		return status.Errorf(codes.InvalidArgument, "port_forward: invalid guest port %d: must be 1-65535", port)
	}

	// Loopback ONLY. The client carries a bare port; the guest always dials
	// 127.0.0.1 so a forward cannot be steered to another interface or the host.
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), tunnelDialTimeout)
	if err != nil {
		// The dial error names the loopback target and the OS cause; no secret.
		return status.Errorf(codes.Unavailable, "port_forward: dial 127.0.0.1:%d in guest: %v", port, err)
	}

	ctx := stream.Context()
	var once sync.Once
	var errMu sync.Mutex
	var firstErr error
	stop := func(e error) {
		once.Do(func() {
			errMu.Lock()
			firstErr = e
			errMu.Unlock()
			target.Close()
		})
	}

	// ctx watcher: a client cancel (or normal stream end) closes the guest socket
	// so the guest->client reader's Read returns and that goroutine exits. It also
	// unblocks the client->guest Recv (which returns a ctx error). The watcher is
	// itself unblocked when stop() below races it (ctx.Done fires on stream end
	// regardless), so it never outlives the RPC.
	go func() {
		<-ctx.Done()
		stop(ctx.Err())
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> guest: pull data frames and write them to the guest socket.
	go func() {
		defer wg.Done()
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				stop(nil) // EOF (client CloseSend) or recv error: tear down.
				return
			}
			switch m := msg.Msg.(type) {
			case *sandboxv1.Frame_Data:
				if _, werr := target.Write(m.Data); werr != nil {
					stop(werr)
					return
				}
			case *sandboxv1.Frame_Close:
				stop(nil)
				return
			}
		}
	}()

	// guest -> client: copy the guest socket into server data frames. A read
	// error (socket closed by stop() or by the peer) ends this direction and
	// tears the other down.
	go func() {
		defer wg.Done()
		buf := make([]byte, grpcChunkBytes)
		for {
			n, rerr := target.Read(buf)
			if n > 0 {
				if serr := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Data{Data: append([]byte(nil), buf[:n]...)}}); serr != nil {
					stop(serr)
					return
				}
			}
			if rerr != nil {
				stop(nil)
				return
			}
		}
	}()

	wg.Wait()
	errMu.Lock()
	e := firstErr
	errMu.Unlock()
	if e != nil && e != context.Canceled {
		return status.Errorf(codes.Unavailable, "port_forward: %v", e)
	}
	return nil
}

// controlServer implements sandbox.internal.v1.Control, the host-trusted
// control channel. Every method REUSES the existing guest handlers
// (handleConfigure, handleNotifyForked) and the same uptime source as the JSON
// ping, so the secret handling and the fork-correctness reseed are byte-for-byte
// equivalent to the legacy path. This service is served ONLY on the in-guest
// vsock gRPC port and is never exposed on the public Sandbox surface or forkd
// :9091.
type controlServer struct {
	internalv1.UnimplementedControlServer
}

// Ping returns the agent uptime, the same value the JSON TypePing handler
// returns (time.Since(startTime)). Carries no secrets.
func (c *controlServer) Ping(_ context.Context, _ *internalv1.PingRequest) (*internalv1.PingResponse, error) {
	return &internalv1.PingResponse{UptimeSeconds: uptimeSeconds()}, nil
}

// Configure delivers claim-time env and secrets to the guest. THIS RPC CARRIES
// SECRET VALUES. It reuses handleConfigure verbatim: the same additive merge
// into configuredEnv under configuredMu, the same secrets-only-in-env handling,
// and the same key-count-only logging. Values are never logged or echoed. A
// non-OK reuse result becomes an Internal error whose message carries only the
// non-secret reason string from the handler.
func (c *controlServer) Configure(_ context.Context, req *internalv1.ConfigureRequest) (*internalv1.ConfigureResponse, error) {
	resp := handleConfigure(&vsock.ConfigureRequest{
		Env:     req.GetEnv(),
		Secrets: req.GetSecrets(),
	})
	if !resp.OK {
		return nil, status.Errorf(codes.Internal, "configure: %s", resp.Error)
	}
	return &internalv1.ConfigureResponse{}, nil
}

// NotifyForked applies the post-restore fork-correctness repairs. It reuses
// handleNotifyForked verbatim, so the RNDADDENTROPY reseed (fail-closed), the
// CLOCK_REALTIME step, the fork-generation write, the network reconfigure, the
// volume mounts, and the SIGUSR2 userspace reseed are byte-for-byte identical to
// the JSON path. Entropy and the absolute clock value are never logged; the
// response carries only the applied-step magnitude, the reseed boolean, and the
// signaled-process count.
func (c *controlServer) NotifyForked(_ context.Context, req *internalv1.NotifyForkedRequest) (*internalv1.NotifyForkedResponse, error) {
	vreq := &vsock.NotifyForkedRequest{
		Generation:         req.GetGeneration(),
		HostWallClockNanos: req.GetHostWallClockNanos(),
		Entropy:            req.GetEntropy(),
		Network:            notifyForkedNetworkFromProto(req.GetNetwork()),
		Volumes:            volumeMountsFromProto(req.GetVolumes()),
	}
	resp := handleNotifyForked(vreq)
	if !resp.OK {
		return nil, status.Errorf(codes.Internal, "notify_forked: %s", resp.Error)
	}
	out := resp.NotifyForked
	if out == nil {
		out = &vsock.NotifyForkedResponse{}
	}
	return &internalv1.NotifyForkedResponse{
		AppliedClockStepNanos: out.AppliedClockStepNanos,
		ReseededRng:           out.ReseededRNG,
		SignaledProcesses:     int32(out.SignaledProcesses),
	}, nil
}

// notifyForkedNetworkFromProto maps the proto network identity to the internal
// vsock shape the existing configureNetwork handler consumes. All fields are
// plain addresses (no secrets). Returns nil when the host delivered no network
// config, preserving the JSON path's nil-means-no-op behavior.
func notifyForkedNetworkFromProto(n *internalv1.NotifyForkedNetwork) *vsock.NotifyForkedNetwork {
	if n == nil {
		return nil
	}
	return &vsock.NotifyForkedNetwork{
		GuestIP:    n.GetGuestIp(),
		GatewayIP:  n.GetGatewayIp(),
		PrefixLen:  int(n.GetPrefixLen()),
		GuestMAC:   n.GetGuestMac(),
		ResolverIP: n.GetResolverIp(),
	}
}

// volumeMountsFromProto maps the proto per-fork mount table to the internal
// vsock shape mountVolumes consumes. All fields are config values, not secrets.
func volumeMountsFromProto(entries []*internalv1.VolumeMountEntry) []vsock.VolumeMountEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]vsock.VolumeMountEntry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		out = append(out, vsock.VolumeMountEntry{
			Device:    e.GetDevice(),
			MountPath: e.GetMountPath(),
			ReadOnly:  e.GetReadOnly(),
		})
	}
	return out
}
