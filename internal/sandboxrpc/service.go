// Package sandboxrpc serves the Sandbox runtime protocol (proto/sandbox/v1,
// issue #24) over Connect. This is the PROTOCOL FOUNDATION slice plus the file
// RPC group: it lands the Connect transport, a real streaming Exec that bridges
// to the existing sandbox-server exec path (SandboxAPI -> vsock -> guest agent),
// a real unary Budget, and the six file RPCs (ReadFile, WriteFile, List, Stat,
// Mkdir, Remove) which are real when a GuestConn is wired (s.Guest != nil) and
// otherwise report an LLM-legible CodeUnimplemented follow-up. The remaining
// RPCs return a CodeUnimplemented error with an LLM-legible message naming issue
// #24, so the service compiles and is honestly partial. The wire migration
// (guest agent JSON-lines -> this proto, the forkd cluster-internal bridge, the
// vsock and browser transports, PTY end to end) is a well-scoped set of
// follow-up slices (docs/api/runtime-protocol.md).
package sandboxrpc

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// ExecParams is the transport-neutral shape of one command execution, decoupled
// from the vsock/daemon types so the Connect handler can be driven by a fake
// backend in tests and by the real SandboxAPI path in production.
type ExecParams struct {
	// Command is the command line; when Args is empty the guest runs it through
	// the shell as a single string (today's behavior).
	Command string
	// Args, when set, makes Command argv[0] and Args the rest, run without a shell.
	Args []string
	// WorkingDir is the cwd; empty defaults to the guest default (/workspace).
	WorkingDir string
	// Env entries are merged onto the sandbox environment. Values may be secret
	// and are never logged.
	Env map[string]string
	// TimeoutSeconds bounds one execution; <= 0 applies the guest default.
	TimeoutSeconds int
}

// ExecBackend runs one command in a sandbox and delivers its output chunks
// incrementally through onChunk, returning the terminal exit code and the
// measured execution time. It is the seam between the Connect Exec handler and
// the existing sandbox-server exec plumbing: the production implementation
// bridges to SandboxAPI.RunExecStream (vsock -> guest agent); the test supplies
// a scripted fake. onChunk's stream argument is "stdout" or "stderr".
type ExecBackend interface {
	RunExecStream(ctx context.Context, sandboxID string, p ExecParams, onChunk func(stream string, data []byte) error) (exitCode int, execTimeMs float64, err error)
}

// BudgetProvider reports a sandbox's remaining self-service allowances (issue
// #25). It is optional: a nil provider makes Budget an honest Unimplemented.
type BudgetProvider interface {
	Budget(ctx context.Context, sandboxID string) (*sandboxv1.BudgetStatus, error)
}

// Service implements the Connect Sandbox handler. It embeds the generated
// UnimplementedSandboxHandler so the not-yet-built RPCs are present, then
// overrides Exec (real, streaming) and Budget (real when a provider is wired).
// The remaining overrides exist only to return an LLM-legible #24 follow-up
// message instead of the generated default, so a caller learns WHY the RPC is
// unavailable and where it is tracked.
//
// Guest, when non-nil, is the GuestConn port (Task 2.1 pattern). When it is
// set, Service.Exec delegates through it instead of the ExecBackend. Tasks
// 2.2-2.7 wire additional RPC groups through GuestConn.
type Service struct {
	sandboxv1connect.UnimplementedSandboxHandler
	exec   ExecBackend
	budget BudgetProvider
	// resolve returns the target sandbox id for a request. In this foundation
	// slice the transport credential is not yet attenuated per sandbox (issue
	// #25), so the default resolver returns the single logical sandbox id. The
	// sandbox-server wiring can override it with WithSandboxResolver once
	// per-sandbox routing lands.
	resolve func(ctx context.Context) (string, error)
	// Guest, when set, is the GuestConn factory used by the Task 2.1 path.
	// Takes priority over exec when non-nil. Zero-value Service{Guest: ...}
	// is valid for tests that drive the GuestConn seam directly.
	Guest func(sandboxID string) (GuestConn, error)
}

// NewService builds the Connect Sandbox handler. exec is required (the streaming
// Exec bridge); budget may be nil (Budget then reports Unimplemented). The
// default sandbox resolver returns a single logical sandbox; the sandbox-server
// can override it with WithSandboxResolver once per-sandbox routing lands.
func NewService(exec ExecBackend, budget BudgetProvider, opts ...Option) *Service {
	s := &Service{
		exec:    exec,
		budget:  budget,
		resolve: func(context.Context) (string, error) { return "", nil },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option configures a Service.
type Option func(*Service)

// WithSandboxResolver sets how the handler resolves the target sandbox id from
// the request context (e.g. from a bearer token or header). The foundation
// slice's default returns the empty single-sandbox id.
func WithSandboxResolver(r func(ctx context.Context) (string, error)) Option {
	return func(s *Service) { s.resolve = r }
}

// followup is the LLM-legible message every not-yet-built RPC returns, so a
// caller (often an agent) learns the RPC is a tracked follow-up of issue #24 and
// can fall back to the current HTTP/vsock surface in the meantime.
func followup(rpc string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf(
		"sandbox.v1.Sandbox/%s is not implemented yet: it is a tracked follow-up of issue #24 (Connect runtime protocol). "+
			"The streaming Exec, the unary Budget, and the file RPCs (ReadFile, WriteFile, List, Stat, Mkdir, Remove) are "+
			"live on this transport when a GuestConn is wired; the remaining runtime surface "+
			"(watch, archive, processes, signal, port-forward, fork/checkpoint/extend, vitals) still rides the "+
			"current JSON-over-HTTP sandbox API and the JSON-lines vsock protocol. See docs/api/runtime-protocol.md", rpc))
}

// connectErr builds an LLM-legible connect.Error (issue #28). code is the
// Connect error code; cause is the underlying error; remediation is a
// human/LLM-legible string suggesting how to resolve the problem. Secret
// values MUST NOT appear in remediation.
func connectErr(code connect.Code, cause error, remediation string) *connect.Error {
	msg := cause.Error()
	if remediation != "" {
		msg = msg + "; " + remediation
	}
	return connect.NewError(code, fmt.Errorf("%s", msg))
}

// Exec runs a command and streams its IO over the Connect bidi stream. The first
// client message MUST carry the open oneof; the handler bridges to either the
// GuestConn port (when s.Guest is set, Task 2.1 path) or the ExecBackend
// (legacy callback path) and forwards each output chunk as an ExecResponse
// stdout/stderr frame the instant it arrives, then a terminal ExecExit. Stdin
// and PTY resize messages are accepted by the protocol but not yet forwarded in
// this foundation slice (PTY end to end and stdin streaming are #24 follow-ups);
// they are drained so the client is not blocked.
func (s *Service) Exec(ctx context.Context, stream *connect.BidiStream[sandboxv1.ExecRequest, sandboxv1.ExecResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("exec stream closed before the opening message: the first ExecRequest must carry the open oneof (command, env, cwd, pty)"))
	}
	open := first.GetOpen()
	if open == nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first ExecRequest must carry the open oneof (command, env, cwd, pty), got %T", first.Msg))
	}

	// GuestConn path (Task 2.1): delegate through the port when it is wired.
	if s.Guest != nil {
		return s.execViaGuest(ctx, stream, open)
	}

	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	p := ExecParams{
		Command:        open.GetCommand(),
		Args:           open.GetArgs(),
		WorkingDir:     open.GetCwd(),
		TimeoutSeconds: int(open.GetTimeoutSeconds()),
	}
	if env := open.GetEnv(); len(env) > 0 {
		p.Env = make(map[string]string, len(env))
		for _, e := range env {
			p.Env[e.GetKey()] = e.GetValue()
		}
	}

	// Forward each chunk the instant the backend produces it. Honor context
	// cancellation: if the client goes away the send fails and we stop driving
	// the backend.
	onChunk := func(name string, data []byte) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Copy: the backend may reuse its buffer after onChunk returns.
		buf := make([]byte, len(data))
		copy(buf, data)
		var resp *sandboxv1.ExecResponse
		if name == string(streamStderr) {
			resp = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: buf}}
		} else {
			resp = &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: buf}}
		}
		return stream.Send(resp)
	}

	exitCode, execMs, err := s.exec.RunExecStream(ctx, sandboxID, p, onChunk)
	if err != nil {
		// The spawn or transport failed. Report it on the terminal ExecExit
		// frame as an LLM-legible remediation string (never a secret value), then
		// end the stream cleanly so the client reads the exit rather than a bare
		// transport error.
		exit := &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
			ExitCode:   1,
			ExecTimeMs: execMs,
			Error:      fmt.Sprintf("exec failed before or during execution: %v", err),
		}}}
		_ = stream.Send(exit)
		return nil
	}

	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
		ExitCode:   int32(exitCode),
		ExecTimeMs: execMs,
	}}})
}

// execViaGuest implements the Task 2.1 GuestConn path for Exec: opens an
// ExecStream from the GuestConn and copies frames to the Connect response stream
// until the terminal Done frame. Errors from the guest are surfaced as an
// LLM-legible exit frame so the client always reads a clean terminal frame.
func (s *Service) execViaGuest(ctx context.Context, stream *connect.BidiStream[sandboxv1.ExecRequest, sandboxv1.ExecResponse], open *sandboxv1.ExecOpen) error {
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}

	conn, err := s.Guest(sandboxID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("open guest connection for sandbox %q: %w; ensure the sandbox is running and the guest agent is healthy", sandboxID, err))
	}

	es, err := conn.Exec(ctx, open)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest exec open failed: %w; check that the command is accessible in the sandbox filesystem", err))
	}
	defer es.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, recvErr := es.Recv()
		if recvErr != nil {
			// io.EOF is handled by the Done frame below, not here; any other
			// error is a transport failure surfaced as an LLM-legible exit.
			_ = stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
				ExitCode: 1,
				Error:    fmt.Sprintf("exec stream read error: %v; the guest agent may have crashed or the vsock connection was lost", recvErr),
			}}})
			return nil
		}
		if frame.Done {
			return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{
				ExitCode: frame.ExitCode,
			}}})
		}
		if len(frame.Stdout) > 0 {
			if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: frame.Stdout}}); err != nil {
				return err
			}
		}
		if len(frame.Stderr) > 0 {
			if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: frame.Stderr}}); err != nil {
				return err
			}
		}
	}
}

// resolveID returns the sandbox id from the resolver, or the empty string when
// the resolver is nil (zero-value Service used in tests with the Guest field).
func (s *Service) resolveID(ctx context.Context) (string, error) {
	if s.resolve == nil {
		return "", nil
	}
	return s.resolve(ctx)
}

// streamName mirrors the daemon/vsock stream identifiers so the handler does not
// import the vsock package (keeping the transport seam clean).
type streamName string

const (
	streamStdout streamName = "stdout"
	streamStderr streamName = "stderr"
)

// Budget reports the caller's remaining self-service allowances. It is real when
// a BudgetProvider is wired; otherwise it is an honest #24 follow-up.
func (s *Service) Budget(ctx context.Context, req *connect.Request[sandboxv1.BudgetRequest]) (*connect.Response[sandboxv1.BudgetStatus], error) {
	if s.budget == nil {
		return nil, followup("Budget")
	}
	sandboxID, err := s.resolveID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("resolve target sandbox: %w", err))
	}
	status, err := s.budget.Budget(ctx, sandboxID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read budget: %w", err))
	}
	return connect.NewResponse(status), nil
}

// --- Honest #24 follow-ups: every RPC below rides the current HTTP/vsock
// surface until its dedicated slice lands. ---

func (s *Service) Archive(_ context.Context, _ *connect.Request[sandboxv1.ArchiveRequest], _ *connect.ServerStream[sandboxv1.Chunk]) error {
	return followup("Archive")
}

func (s *Service) Fork(_ context.Context, _ *connect.Request[sandboxv1.ForkRequest]) (*connect.Response[sandboxv1.Operation], error) {
	return nil, followup("Fork")
}

func (s *Service) Checkpoint(_ context.Context, _ *connect.Request[sandboxv1.CheckpointRequest]) (*connect.Response[sandboxv1.Revision], error) {
	return nil, followup("Checkpoint")
}

func (s *Service) ExtendLifetime(_ context.Context, _ *connect.Request[sandboxv1.ExtendRequest]) (*connect.Response[sandboxv1.Lease], error) {
	return nil, followup("ExtendLifetime")
}
