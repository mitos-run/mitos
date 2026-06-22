package mitos

import (
	"context"
	"net/http"
)

// DefaultExecTimeoutSeconds is the default exec timeout, matching the other SDKs.
const DefaultExecTimeoutSeconds = 30

// Sandbox is a live sandbox handle bound to the SandboxServer that produced it.
// Exec round-trips through the server URL and Terminate issues DELETE
// /v1/sandboxes/{id}.
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

// Exec runs command in the sandbox and returns its result. With no options it
// uses DefaultExecTimeoutSeconds.
func (s *Sandbox) Exec(ctx context.Context, command string, opts ...ExecOption) (*ExecResult, error) {
	cfg := execConfig{timeoutSeconds: DefaultExecTimeoutSeconds}
	for _, opt := range opts {
		opt(&cfg)
	}
	body := map[string]any{
		"sandbox": s.ID,
		"command": command,
		"timeout": cfg.timeoutSeconds,
	}
	var res ExecResult
	if err := s.server.t.do(ctx, http.MethodPost, "/v1/exec", body, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ExecOption configures Exec.
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
