// Package mitos is the official Go SDK for mitos: a thin, dependency-free
// (standard-library only) client for the standalone and hosted sandbox-server
// REST API. It mirrors the direct-mode surface of the Python SDK
// (sdk/python/mitos/direct.py), the TypeScript SDK (sdk/typescript/src/server.ts),
// the Ruby SDK (sdk/ruby), the Rust SDK (sdk/rust), and the Java SDK (sdk/java):
// create a template, fork a sandbox, run exec, list, and terminate.
//
// Scope: this SDK covers DIRECT mode only, the standalone cmd/sandbox-server and
// the hosted control plane at https://mitos.run. The Kubernetes / cluster mode
// (the controller, forkd, and the SandboxTemplate / SandboxPool / SandboxClaim /
// SandboxFork CRDs) is NOT part of this SDK.
//
// Quickstart (hosted): set MITOS_API_KEY in the environment; it is sent as
// Authorization: Bearer <key> and is never logged.
//
//	srv := mitos.NewSandboxServer()
//	tmpl, err := srv.CreateTemplate(ctx, "python")
//	sb, err := srv.Fork(ctx, "python", "")
//	res, err := sb.Exec(ctx, "echo hi")
//	fmt.Println(res.Stdout)
//	err = sb.Terminate(ctx)
//
// Auth: the base URL follows the precedence option, then MITOS_BASE_URL, then the
// hosted endpoint https://mitos.run. The bearer token follows option, then
// MITOS_API_KEY, then the CLI login credential file written by `mitos auth login`
// (~/.config/mitos/credentials.json, honoring MITOS_CONFIG_DIR), then none
// (tokenless). The token VALUE is never logged and is redacted from any error.
package mitos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// DefaultInitWaitSeconds is the default template build wait, matching the other
// SDKs.
const DefaultInitWaitSeconds = 5

// sandboxIDRe is the sandbox id allowlist: start with an alphanumeric, then up
// to 63 alphanumeric, underscore, or hyphen characters. Mirrors
// internal/daemon validation, the TypeScript validSandboxId, the Ruby
// SANDBOX_ID_RE, and the Java SANDBOX_ID_RE.
var sandboxIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidSandboxID reports whether id matches the sandbox id allowlist
// (^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$).
func ValidSandboxID(id string) bool {
	return sandboxIDRe.MatchString(id)
}

// Template is a template as reported by the sandbox-server.
type Template struct {
	// ID is the template id.
	ID string `json:"id"`
	// Ready reports whether the template is built and ready to fork from.
	Ready bool `json:"ready"`
	// CreatedAt is the server-side creation timestamp.
	CreatedAt string `json:"created_at"`
	// CreationTimeMs is how long the template build took, in milliseconds.
	CreationTimeMs float64 `json:"creation_time_ms"`
}

// ServerSandbox is a sandbox summary as reported by the sandbox-server
// (GET /v1/sandboxes), distinct from a live *Sandbox handle.
type ServerSandbox struct {
	// ID is the sandbox id.
	ID string `json:"id"`
	// TemplateID is the template this sandbox was forked from.
	TemplateID string `json:"template_id"`
	// Endpoint is the server address serving this sandbox.
	Endpoint string `json:"endpoint"`
	// CreatedAt is the server-side creation timestamp.
	CreatedAt string `json:"created_at"`
	// ForkTimeMs is how long the fork took, in milliseconds.
	ForkTimeMs float64 `json:"fork_time_ms"`
}

// ExecResult is the outcome of an exec call.
type ExecResult struct {
	// ExitCode is the command's exit status.
	ExitCode int `json:"exit_code"`
	// Stdout is the command's standard output.
	Stdout string `json:"stdout"`
	// Stderr is the command's standard error.
	Stderr string `json:"stderr"`
	// ExecTimeMs is how long the command ran, in milliseconds.
	ExecTimeMs float64 `json:"exec_time_ms"`
}

// Option configures a SandboxServer. Options take precedence over the
// environment and the credential file.
type Option func(*config)

type config struct {
	baseURL string
	apiKey  string
	http    httpClient
}

// WithBaseURL sets the base URL explicitly, overriding MITOS_BASE_URL and the
// hosted default. Trailing slashes are stripped.
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithAPIKey sets the bearer token explicitly, overriding MITOS_API_KEY and the
// CLI login credential file. The token VALUE is never logged.
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey = key }
}

// WithHTTPClient injects a custom HTTP client (for timeouts, proxies, transports,
// or tests). When omitted a default *http.Client with a 60s timeout is used.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) { c.http = client }
}

// SandboxServer is the direct-mode client for the standalone / hosted
// sandbox-server REST API. Fork returns a *Sandbox bound to this server: Exec
// round-trips through the server URL and Terminate issues DELETE
// /v1/sandboxes/{id}.
type SandboxServer struct {
	url string
	t   *transport
}

// NewSandboxServer builds a client from the given options. With no options the
// base URL is MITOS_BASE_URL or the hosted endpoint and the bearer token is
// MITOS_API_KEY or the CLI login credential file, else tokenless. The token
// VALUE is never logged.
func NewSandboxServer(opts ...Option) *SandboxServer {
	var c config
	for _, opt := range opts {
		opt(&c)
	}
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	url := resolveBaseURL(c.baseURL)
	return &SandboxServer{
		url: url,
		t: &transport{
			baseURL: url,
			token:   resolveToken(c.apiKey),
			http:    hc,
		},
	}
}

// URL is the resolved base URL this client targets.
func (s *SandboxServer) URL() string {
	return s.url
}

// CreateTemplate creates (or builds) the template named id. It sends a fresh
// Idempotency-Key so a retried create returns the same template rather than a
// duplicate (issue #22). With no options it uses DefaultInitWaitSeconds.
func (s *SandboxServer) CreateTemplate(ctx context.Context, id string, opts ...CreateTemplateOption) (*Template, error) {
	cfg := createTemplateConfig{initWaitSeconds: DefaultInitWaitSeconds}
	for _, opt := range opts {
		opt(&cfg)
	}
	key := cfg.idempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	body := map[string]any{"id": id, "init_wait_seconds": cfg.initWaitSeconds}
	var tmpl Template
	if err := s.t.do(ctx, http.MethodPost, "/v1/templates", body, map[string]string{idempotencyHeader: key}, &tmpl); err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// CreateTemplateOption configures CreateTemplate.
type CreateTemplateOption func(*createTemplateConfig)

type createTemplateConfig struct {
	initWaitSeconds int
	idempotencyKey  string
}

// WithInitWaitSeconds sets how long the server waits for the template init to
// settle.
func WithInitWaitSeconds(seconds int) CreateTemplateOption {
	return func(c *createTemplateConfig) { c.initWaitSeconds = seconds }
}

// WithTemplateIdempotencyKey sets an explicit Idempotency-Key for the create so
// a retry across processes is de-duplicated by the server. When omitted a fresh
// key is generated.
func WithTemplateIdempotencyKey(key string) CreateTemplateOption {
	return func(c *createTemplateConfig) { c.idempotencyKey = key }
}

// ListTemplates lists the templates known to the server.
func (s *SandboxServer) ListTemplates(ctx context.Context) ([]Template, error) {
	var out []Template
	if err := s.t.do(ctx, http.MethodGet, "/v1/templates", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Fork forks a sandbox from the named template. When id is empty a
// "sandbox-<hex>" id is generated. The id is validated against the allowlist; an
// invalid id returns a typed *Error BEFORE any request is sent. A fresh
// Idempotency-Key is sent so a retried fork returns the same sandbox rather than
// a duplicate (issue #22). The returned *Sandbox is bound to this server.
func (s *SandboxServer) Fork(ctx context.Context, template, id string, opts ...ForkOption) (*Sandbox, error) {
	cfg := forkConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	sandboxID := id
	if sandboxID == "" {
		sandboxID = randomSandboxID()
	}
	if !ValidSandboxID(sandboxID) {
		return nil, &Error{
			Code:        "invalid_sandbox_id",
			Message:     fmt.Sprintf("invalid sandbox id: %q", sandboxID),
			Cause:       "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
			Remediation: "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars.",
		}
	}
	key := cfg.idempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	body := map[string]any{"template": template, "id": sandboxID}
	var out struct {
		ID         string  `json:"id"`
		TemplateID string  `json:"template_id"`
		Endpoint   string  `json:"endpoint"`
		ForkTimeMs float64 `json:"fork_time_ms"`
	}
	if err := s.t.do(ctx, http.MethodPost, "/v1/fork", body, map[string]string{idempotencyHeader: key}, &out); err != nil {
		return nil, err
	}
	resolvedID := out.ID
	if resolvedID == "" {
		resolvedID = sandboxID
	}
	return &Sandbox{
		ID:         resolvedID,
		Template:   out.TemplateID,
		Endpoint:   out.Endpoint,
		ForkTimeMs: out.ForkTimeMs,
		server:     s,
	}, nil
}

// ForkOption configures Fork.
type ForkOption func(*forkConfig)

type forkConfig struct {
	idempotencyKey string
}

// WithForkIdempotencyKey sets an explicit Idempotency-Key for the fork so a
// retry across processes is de-duplicated by the server. When omitted a fresh
// key is generated.
func WithForkIdempotencyKey(key string) ForkOption {
	return func(c *forkConfig) { c.idempotencyKey = key }
}

// ListSandboxes lists the live sandboxes known to the server.
func (s *SandboxServer) ListSandboxes(ctx context.Context) ([]ServerSandbox, error) {
	var out []ServerSandbox
	if err := s.t.do(ctx, http.MethodGet, "/v1/sandboxes", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// terminate issues DELETE /v1/sandboxes/{id}. Called by Sandbox.Terminate.
func (s *SandboxServer) terminate(ctx context.Context, id string) error {
	if !ValidSandboxID(id) {
		return &Error{
			Code:        "invalid_sandbox_id",
			Message:     fmt.Sprintf("invalid sandbox id: %q", id),
			Cause:       "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
			Remediation: "Terminate only ids that match the sandbox id allowlist.",
		}
	}
	return s.t.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, nil)
}

// idempotencyHeader is the header that carries a creating call's idempotency
// key. It matches the server's idempotencyHeader.
const idempotencyHeader = "Idempotency-Key"

// newIdempotencyKey returns a fresh client-side key so a retried creating call
// (template build or fork) is de-duplicated by the server rather than creating a
// second resource. The key VALUE is an opaque caller token, never a secret.
func newIdempotencyKey() string {
	return randomHex(16)
}

// randomSandboxID returns a generated "sandbox-<hex>" id, matching the
// convention of the other SDKs.
func randomSandboxID() string {
	return "sandbox-" + randomHex(4)
}

// randomHex returns n random bytes as a lowercase hex string. It uses
// crypto/rand and panics only on the (effectively impossible) failure of the
// system CSPRNG, matching the SecureRandom usage in the other SDKs.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("mitos: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buf)
}
