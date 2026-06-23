package daemon

// sandboxrpc_adapter.go: bridges the Connect Sandbox service (internal/sandboxrpc)
// to the existing SandboxAPI vsock/JSON-lines exec path for forkd's :9091 HTTP
// server (issue #24, Task 3.2). Only Exec is bridged in this task: the other
// GuestConn methods return a clear not-yet-bridged error so they fail honestly.
// They become real in Stage 5 (gRPC-over-vsock guest agent).
//
// Security notes:
// - Token values never appear in error messages or logs.
// - The adapter never logs env vars or command output.
// - All other GuestConn methods fail with an explicit LLM-legible message that
//   names issue #24 stage 5 so agents can fall back to the JSON API.

import (
	"context"
	"fmt"
	"io"
	"time"

	"mitos.run/mitos/internal/sandboxrpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// daemonGuestConn adapts a SandboxAPI's vsock exec path to the sandboxrpc.GuestConn
// interface. One instance is created per Exec call: the sandboxID is fixed at
// construction time by the Connect handler's sandbox resolver.
type daemonGuestConn struct {
	api       *SandboxAPI
	sandboxID string
}

// newDaemonGuestConn returns a GuestConn backed by api for sandboxID.
func newDaemonGuestConn(api *SandboxAPI, sandboxID string) sandboxrpc.GuestConn {
	return &daemonGuestConn{api: api, sandboxID: sandboxID}
}

// notBridged returns the LLM-legible error for methods not yet bridged in Task 3.2.
func notBridged(method string) error {
	return fmt.Errorf(
		"GuestConn.%s is not yet bridged to the current vsock/JSON-lines guest; "+
			"it requires the gRPC-over-vsock guest agent (issue #24 stage 5). "+
			"Fall back to the /v1/* JSON API until stage 5 lands.",
		method,
	)
}

// Exec bridges to SandboxAPI.RunExecStream, mapping the onChunk callback and
// the terminal exit code into the ExecStream/ExecFrame shape the Connect Service
// expects. The sandboxv1.ExecOpen fields (Command, Args, Cwd, Env, TimeoutSeconds)
// are forwarded; PTY fields are not forwarded (not yet bridged).
func (g *daemonGuestConn) Exec(ctx context.Context, open *sandboxv1.ExecOpen) (sandboxrpc.ExecStream, error) {
	env := make(map[string]string, len(open.GetEnv()))
	for _, e := range open.GetEnv() {
		env[e.GetKey()] = e.GetValue()
	}

	// Build the command string. When Args are present, join them into a single
	// shell command because RunExecStream's existing vsock path uses a single
	// command string. Direct argv exec (no-shell) arrives in stage 5 with the
	// gRPC-over-vsock guest.
	cmd := open.GetCommand()
	if len(open.GetArgs()) > 0 {
		// Prepend command to args and join; the vsock path will run through shell.
		all := append([]string{open.GetCommand()}, open.GetArgs()...)
		cmd = joinArgs(all)
	}

	// Per-sandbox concurrent-stream cap (production-blocker #2): the Connect exec
	// path must count against the SAME ceiling the JSON /v1/exec/stream handler
	// enforces (handleExecStream), or a tenant could open unbounded streams over
	// Connect while JSON is capped. Acquire a slot at OPEN and hold it for the
	// duration of the stream (released when RunExecStream returns).
	release, ok := g.api.acquireStream(g.sandboxID)
	if !ok {
		return nil, fmt.Errorf("sandbox %q is at its concurrent exec-stream limit; close an existing stream before opening another", g.sandboxID)
	}

	pipe := newExecPipe()
	go func() {
		defer release()
		exitCode, _, err := g.api.RunExecStream(
			ctx,
			g.sandboxID,
			cmd,
			open.GetCwd(),
			env,
			int(open.GetTimeoutSeconds()),
			func(stream string, data []byte) error {
				buf := make([]byte, len(data))
				copy(buf, data)
				return pipe.push(stream, buf)
			},
		)
		pipe.done(exitCode, err)
	}()
	return pipe, nil
}

// joinArgs joins args with spaces. Args containing spaces are not shell-quoted
// here: the existing vsock exec path already runs through a shell, so the raw
// join is consistent with the behavior of the JSON /v1/exec handler.
func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	total := 0
	for _, a := range args {
		total += len(a) + 1
	}
	b := make([]byte, 0, total)
	for i, a := range args {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, a...)
	}
	return string(b)
}

// execPipe is the ExecStream implementation for daemonGuestConn. It bridges
// the callback-based RunExecStream to the pull-based Recv() interface the
// Connect Service uses, via an unbuffered channel of execPipeItem values.
// The goroutine feeding the pipe calls push() for each chunk and done() once
// at the end. Recv() returns each frame (including the terminal Done frame)
// and returns io.EOF only if called again after Done (which the Service never
// does). Close cancels any pending push by draining the channel.
type execPipe struct {
	ch     chan execPipeItem
	closed chan struct{}
}

type execPipeItem struct {
	stream   string
	data     []byte
	exitCode int
	err      error
	done     bool
}

func newExecPipe() *execPipe {
	return &execPipe{
		ch:     make(chan execPipeItem, 32),
		closed: make(chan struct{}),
	}
}

// push delivers a stdout/stderr chunk. Returns an error when the stream is
// already closed (client went away).
func (p *execPipe) push(stream string, data []byte) error {
	select {
	case p.ch <- execPipeItem{stream: stream, data: data}:
		return nil
	case <-p.closed:
		return fmt.Errorf("exec stream closed by client")
	}
}

// done signals the terminal exit. err is a transport/spawn failure; exitCode is
// the process exit code on success.
func (p *execPipe) done(exitCode int, err error) {
	select {
	case p.ch <- execPipeItem{done: true, exitCode: exitCode, err: err}:
	case <-p.closed:
	}
}

// Recv returns the next ExecFrame. It returns the terminal Done frame (with
// err == nil and the process exit code) when the command has finished. It
// returns a non-nil error only on transport or spawn failures. A subsequent
// call after the Done frame returns io.EOF (the Service never makes that call).
func (p *execPipe) Recv() (*sandboxrpc.ExecFrame, error) {
	select {
	case item, ok := <-p.ch:
		if !ok {
			// Channel closed after done was already consumed: this is the post-Done
			// call the Service never makes. Return io.EOF per the contract.
			return nil, io.EOF
		}
		if item.done {
			// Close the channel so subsequent Recv() calls return io.EOF.
			close(p.ch)
			if item.err != nil {
				return nil, item.err
			}
			return &sandboxrpc.ExecFrame{Done: true, ExitCode: int32(item.exitCode)}, nil
		}
		frame := &sandboxrpc.ExecFrame{}
		if item.stream == "stderr" {
			frame.Stderr = item.data
		} else {
			frame.Stdout = item.data
		}
		return frame, nil
	case <-p.closed:
		return nil, fmt.Errorf("exec stream closed by client")
	}
}

// Close releases resources. Safe to call at any time; idempotent via the
// once-closed closed channel.
func (p *execPipe) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	return nil
}

// --- Not-yet-bridged GuestConn methods (all return notBridged) ---

func (g *daemonGuestConn) ReadFile(_ context.Context, _ string, _, _ int64) ([][]byte, error) {
	return nil, notBridged("ReadFile")
}

func (g *daemonGuestConn) WriteFile(_ context.Context, _ string, _ uint32, _ [][]byte) (*sandboxrpc.WriteFileResult, error) {
	return nil, notBridged("WriteFile")
}

func (g *daemonGuestConn) List(_ context.Context, _ string, _ int32, _ string, _ string) (*sandboxrpc.ListResult, error) {
	return nil, notBridged("List")
}

func (g *daemonGuestConn) Stat(_ context.Context, _ string) (*sandboxrpc.FileInfo, error) {
	return nil, notBridged("Stat")
}

func (g *daemonGuestConn) Mkdir(_ context.Context, _ string, _ bool) error {
	return notBridged("Mkdir")
}

func (g *daemonGuestConn) Remove(_ context.Context, _ string, _ bool) error {
	return notBridged("Remove")
}

func (g *daemonGuestConn) RunCode(_ context.Context, _ *sandboxv1.RunCodeOpen) (sandboxrpc.RunCodeStream, error) {
	return nil, notBridged("RunCode")
}

func (g *daemonGuestConn) PortForward(_ context.Context, _ uint32) (sandboxrpc.PortForwardStream, error) {
	return nil, notBridged("PortForward")
}

func (g *daemonGuestConn) Vitals(_ context.Context, _ time.Duration) (sandboxrpc.VitalsStream, error) {
	return nil, notBridged("Vitals")
}

func (g *daemonGuestConn) Watch(_ context.Context, _ string) (sandboxrpc.WatchStream, error) {
	return nil, notBridged("Watch")
}

func (g *daemonGuestConn) Processes(_ context.Context) (*sandboxv1.ProcessList, error) {
	return nil, notBridged("Processes")
}

func (g *daemonGuestConn) Signal(_ context.Context, _ int32, _ int32) error {
	return notBridged("Signal")
}

func (g *daemonGuestConn) Archive(_ context.Context, _ string) (sandboxrpc.ArchiveStream, error) {
	return nil, notBridged("Archive")
}

func (g *daemonGuestConn) Upload(_ context.Context, _ string, chunks <-chan []byte) (*sandboxrpc.UploadResult, error) {
	// Drain the channel so the upstream goroutine can exit cleanly.
	for range chunks {
	}
	return nil, notBridged("Upload")
}
