// Package sandboxrpc serves the Sandbox runtime protocol (proto/sandbox/v1,
// issue #24) over Connect. This is the PROTOCOL FOUNDATION slice: it lands the
// Connect transport plus a real, streaming Exec that bridges to the existing
// sandbox-server exec path (SandboxAPI -> vsock -> guest agent), and a real
// unary Budget. Every other RPC returns a CodeUnimplemented error with an
// LLM-legible message naming issue #24, so the service compiles and is honestly
// partial. The wire migration (guest agent JSON-lines -> this proto, the forkd
// cluster-internal bridge, the vsock and browser transports, PTY end to end) is
// a well-scoped set of follow-up slices (docs/api/runtime-protocol.md).
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
			"The streaming Exec and the unary Budget RPCs are live on this transport; the remaining runtime surface "+
			"(files, watch, archive, processes, signal, port-forward, fork/checkpoint/extend, vitals) still rides the "+
			"current JSON-over-HTTP sandbox API and the JSON-lines vsock protocol. See docs/api/runtime-protocol.md", rpc))
}

// Exec runs a command and streams its IO over the Connect bidi stream. The first
// client message MUST carry the open oneof; the handler bridges to the exec
// backend (the existing SandboxAPI -> vsock -> guest agent path) and forwards
// each output chunk as an ExecResponse stdout/stderr frame the instant it
// arrives, then a terminal ExecExit. Stdin and PTY resize messages are accepted
// by the protocol but not yet forwarded in this foundation slice (PTY end to end
// and stdin streaming are #24 follow-ups); they are drained so the client is not
// blocked.
func (s *Service) Exec(ctx context.Context, stream *connect.BidiStream[sandboxv1.ExecRequest, sandboxv1.ExecResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("exec stream closed before the opening message: the first ExecRequest must carry the open oneof (command, env, cwd, pty)"))
	}
	open := first.GetOpen()
	if open == nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("first ExecRequest must carry the open oneof (command, env, cwd, pty), got %T", first.Msg))
	}

	sandboxID, err := s.resolve(ctx)
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
	sandboxID, err := s.resolve(ctx)
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

func (s *Service) ReadFile(_ context.Context, _ *connect.Request[sandboxv1.ReadFileRequest], _ *connect.ServerStream[sandboxv1.Chunk]) error {
	return followup("ReadFile")
}

func (s *Service) WriteFile(_ context.Context, _ *connect.ClientStream[sandboxv1.WriteFileRequest]) (*connect.Response[sandboxv1.WriteFileResult], error) {
	return nil, followup("WriteFile")
}

func (s *Service) List(_ context.Context, _ *connect.Request[sandboxv1.ListRequest]) (*connect.Response[sandboxv1.ListResponse], error) {
	return nil, followup("List")
}

func (s *Service) Stat(_ context.Context, _ *connect.Request[sandboxv1.StatRequest]) (*connect.Response[sandboxv1.FileInfo], error) {
	return nil, followup("Stat")
}

func (s *Service) Archive(_ context.Context, _ *connect.Request[sandboxv1.ArchiveRequest], _ *connect.ServerStream[sandboxv1.Chunk]) error {
	return followup("Archive")
}

func (s *Service) Watch(_ context.Context, _ *connect.Request[sandboxv1.WatchRequest], _ *connect.ServerStream[sandboxv1.FsEvent]) error {
	return followup("Watch")
}

func (s *Service) Processes(_ context.Context, _ *connect.Request[sandboxv1.ProcessesRequest]) (*connect.Response[sandboxv1.ProcessList], error) {
	return nil, followup("Processes")
}

func (s *Service) Signal(_ context.Context, _ *connect.Request[sandboxv1.SignalRequest]) (*connect.Response[sandboxv1.SignalResponse], error) {
	return nil, followup("Signal")
}

func (s *Service) PortForward(_ context.Context, _ *connect.BidiStream[sandboxv1.Frame, sandboxv1.Frame]) error {
	return followup("PortForward")
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

func (s *Service) Vitals(_ context.Context, _ *connect.Request[sandboxv1.VitalsRequest], _ *connect.ServerStream[sandboxv1.GuestVitals]) error {
	return followup("Vitals")
}
