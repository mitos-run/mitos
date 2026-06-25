package mitos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DefaultExecTimeoutSeconds is the default exec timeout, matching the other SDKs.
const DefaultExecTimeoutSeconds = 30

// Sandbox is a live sandbox handle bound to the SandboxServer that produced it.
// Exec and ExecStream run over the Connect sandbox.v1.Sandbox runtime protocol
// (issue #24); Terminate issues DELETE /v1/sandboxes/{id}.
type Sandbox struct {
	// ID is the sandbox id.
	ID string
	// Template is the template this sandbox was forked from.
	Template string
	// Endpoint is the server address serving this sandbox.
	Endpoint string
	// ForkTimeMs is how long the fork took, in milliseconds.
	ForkTimeMs float64

	server *SandboxServer
}

// execStreamRequest is the proto-JSON ExecStreamRequest (camelCase fields). Only
// the fields the Go SDK exposes today are sent; the rest default server-side.
type execStreamRequest struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// execResponseWire is the proto-JSON ExecResponse oneof: exactly one of stdout,
// stderr, or exit is set per frame. encoding/json decodes the base64 bytes fields
// straight into []byte.
type execResponseWire struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Exit   *struct {
		ExitCode   int     `json:"exitCode"`
		ExecTimeMs float64 `json:"execTimeMs"`
		Error      string  `json:"error"`
	} `json:"exit"`
}

// ExecChunk is one piece of streamed output: exactly one of Stdout or Stderr is
// non-empty.
type ExecChunk struct {
	// Stdout is a chunk of standard output, or nil for a stderr chunk.
	Stdout []byte
	// Stderr is a chunk of standard error, or nil for a stdout chunk.
	Stderr []byte
}

// ExecStream is a live, incremental exec over the Connect runtime protocol.
// Recv yields each output chunk as it arrives; after Recv returns io.EOF the exit
// status is available from Result. Always Close it (a deferred Close is fine) to
// release the underlying connection.
type ExecStream struct {
	cs     *connectStream
	result ExecResult
}

// ExecStream runs command in the sandbox and streams its output incrementally
// over the Connect sandbox.v1.Sandbox/ExecStream RPC. Stdout arrives as it is
// produced (not buffered to the end), so a long-running command's output is live.
// With no options it uses DefaultExecTimeoutSeconds.
func (s *Sandbox) ExecStream(ctx context.Context, command string, opts ...ExecOption) (*ExecStream, error) {
	cfg := execConfig{timeoutSeconds: DefaultExecTimeoutSeconds}
	for _, opt := range opts {
		opt(&cfg)
	}
	req := execStreamRequest{Command: command}
	if cfg.timeoutSeconds > 0 {
		req.TimeoutSeconds = cfg.timeoutSeconds
	}
	cs, err := s.server.t.connectServerStream(ctx, "ExecStream", s.ID, req)
	if err != nil {
		return nil, err
	}
	return &ExecStream{cs: cs}, nil
}

// Recv returns the next output chunk. It returns io.EOF when the command has
// exited (after which Result holds the exit status) and a typed *Error on a
// failure. The terminal exit frame is consumed internally (it carries the exit
// status, not output), so Recv only ever returns real output chunks.
func (st *ExecStream) Recv() (*ExecChunk, error) {
	for {
		payload, err := st.cs.Recv()
		if err != nil {
			return nil, err // io.EOF on clean end, *Error on failure
		}
		var wire execResponseWire
		if err := json.Unmarshal(payload, &wire); err != nil {
			return nil, fmt.Errorf("decode ExecResponse: %w", err)
		}
		if wire.Exit != nil {
			st.result = ExecResult{ExitCode: wire.Exit.ExitCode, ExecTimeMs: wire.Exit.ExecTimeMs}
			continue // the exit frame is not output; read on to the end-stream
		}
		if len(wire.Stdout) == 0 && len(wire.Stderr) == 0 {
			continue // an empty frame carries no output
		}
		return &ExecChunk{Stdout: wire.Stdout, Stderr: wire.Stderr}, nil
	}
}

// Result returns the exit status. It is valid after Recv has returned io.EOF.
func (st *ExecStream) Result() ExecResult { return st.result }

// Close releases the stream's connection. Safe to call more than once.
func (st *ExecStream) Close() error { return st.cs.Close() }

// Exec runs command in the sandbox and returns its buffered result. It drains the
// Connect ExecStream into a single ExecResult, so it is the simple one-shot form
// of ExecStream. With no options it uses DefaultExecTimeoutSeconds.
func (s *Sandbox) Exec(ctx context.Context, command string, opts ...ExecOption) (*ExecResult, error) {
	st, err := s.ExecStream(ctx, command, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()

	var stdout, stderr strings.Builder
	for {
		chunk, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		stdout.Write(chunk.Stdout)
		stderr.Write(chunk.Stderr)
	}
	res := st.Result()
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	return &res, nil
}

// ExecOption configures Exec and ExecStream.
type ExecOption func(*execConfig)

type execConfig struct {
	timeoutSeconds int
}

// WithExecTimeout sets the per-command timeout in seconds.
func WithExecTimeout(seconds int) ExecOption {
	return func(c *execConfig) { c.timeoutSeconds = seconds }
}

// Terminate terminates the sandbox: it issues DELETE /v1/sandboxes/{id}.
func (s *Sandbox) Terminate(ctx context.Context) error {
	return s.server.terminate(ctx, s.ID)
}
