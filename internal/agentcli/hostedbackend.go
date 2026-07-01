package agentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"mitos.run/mitos/internal/mcp"
)

// DefaultHostedBaseURL is the hosted production API endpoint. Set MITOS_BASE_URL
// or pass --server to override. This is the API host (api.mitos.run), NOT the
// console origin (mitos.run), which serves the web app and returns HTML for
// /v1/* paths.
const DefaultHostedBaseURL = "https://api.mitos.run"

// inClusterBaseURL returns the in-cluster mitos gateway URL when running inside
// the mitos cluster, else the empty string. Kubernetes injects
// MITOS_GATEWAY_SERVICE_HOST / MITOS_GATEWAY_SERVICE_PORT into every pod in the
// mitos-gateway Service's namespace, a precise DNS-free signal that we are in
// the mitos control plane; an unrelated cluster with only a hosted API key never
// matches and keeps using the hosted endpoint.
func inClusterBaseURL() string {
	host := os.Getenv("MITOS_GATEWAY_SERVICE_HOST")
	if host == "" {
		return ""
	}
	port := os.Getenv("MITOS_GATEWAY_SERVICE_PORT")
	if port == "" {
		port = "80"
	}
	return "http://" + host + ":" + port
}

// HostedBackend implements Backend against the mitos.run hosted gateway (or a
// compatible standalone sandbox-server) via the /v1 REST API and the Connect
// sandbox.v1.Sandbox runtime protocol. It speaks the same wire as the Python,
// TypeScript, and Go SDKs: bearer token on every request, key value NEVER
// logged or placed in an error message.
//
// Verb support:
//
//	create, exec, read_file, write_file, fork, ls, terminate -> /v1 REST + Connect RPC
//	ws (workspace verbs)     -> not supported; use the hosted dashboard or cluster mode
//	template build/push      -> not supported; use cluster mode with a KVM node
//
// Fork semantics: Fork(sandboxID, n) issues n POST /v1/fork calls, each with
// {"template": sandboxID, "id": newID}. The gateway resolves the sandbox to its
// original template and re-forks from that snapshot -- exactly what
// mcp.HTTPBackend.Fork does and what the Python SDK does (passing self.template
// to /v1/fork). The source sandbox keeps running; each child is an independent
// sibling cloned from the shared template snapshot, NOT a live memory fork of
// the running sandbox. That live-fork capability requires the cluster backend
// (source.fromSandbox on a KVM node).
type HostedBackend struct {
	baseURL string           // trailing slash stripped; set once at construction
	apiKey  string           // bearer token; NEVER logged or placed in errors
	hb      *mcp.HTTPBackend // covers exec / read_file / write_file / fork / terminate
	client  *http.Client
}

// NewHostedBackend builds a HostedBackend targeting baseURL with apiKey as the
// bearer token. A nil httpClient uses http.DefaultClient. A blank baseURL falls
// back to DefaultHostedBaseURL.
func NewHostedBackend(baseURL, apiKey string, httpClient *http.Client) *HostedBackend {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		// No explicit --server / MITOS_BASE_URL: prefer the in-cluster gateway
		// when running inside the mitos cluster, else the hosted endpoint.
		baseURL = inClusterBaseURL()
	}
	if baseURL == "" {
		baseURL = DefaultHostedBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &HostedBackend{
		baseURL: baseURL,
		apiKey:  apiKey,
		hb:      mcp.NewHTTPBackend(baseURL, apiKey, httpClient),
		client:  httpClient,
	}
}

// redact removes the api key from s so a hostile or misconfigured server that
// echoes the Authorization header into its error body cannot leak the key.
func (b *HostedBackend) redact(s string) string {
	if b.apiKey == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, b.apiKey, "[REDACTED]")
}

// do issues an HTTP request to path with an optional JSON body and decodes a
// 2xx JSON response into out (when out is non-nil). Non-2xx responses surface
// the body as the error cause after the api key is redacted.
func (b *HostedBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s request: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, b.redact(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// Create ensures the named template exists on the server (POST /v1/templates),
// then forks one sandbox from it (POST /v1/fork) and returns the sandbox id.
// In hosted mode the --pool flag names a template, not a Kubernetes SandboxPool.
func (b *HostedBackend) Create(ctx context.Context, pool string) (string, error) {
	if pool == "" {
		return "", fmt.Errorf("create: a template name is required (in hosted mode --pool names a template)")
	}
	// Ensure the template exists before forking. The server is idempotent on
	// this call: a pre-existing template is returned unchanged.
	if err := b.do(ctx, http.MethodPost, "/v1/templates", map[string]any{
		"id":                pool,
		"init_wait_seconds": 5,
	}, nil); err != nil {
		return "", fmt.Errorf("ensure template %q: %w", pool, err)
	}
	// Fork one sandbox from the template. mcp.HTTPBackend.Create sends
	// POST /v1/fork {template: pool, id: <random>} and returns the sandbox id.
	return b.hb.Create(ctx, pool)
}

// Exec runs command in the sandbox over the Connect sandbox.v1.Sandbox/ExecStream
// RPC, identified by the X-Sandbox-Id header and routed by the gateway.
func (b *HostedBackend) Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	res, err := b.hb.Exec(ctx, sandboxID, command, timeoutSec)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// ReadFile reads path from the sandbox over the Connect sandbox.v1.Sandbox/ReadFile RPC.
func (b *HostedBackend) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	return b.hb.ReadFile(ctx, sandboxID, path)
}

// WriteFile writes content to path in the sandbox over the Connect
// sandbox.v1.Sandbox/WriteFile RPC.
func (b *HostedBackend) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	return b.hb.WriteFile(ctx, sandboxID, path, content)
}

// Fork forks sandboxID into n independent siblings by issuing n POST /v1/fork
// calls, each with {"template": sandboxID, "id": <random>}. The gateway
// resolves the sandbox to its template and re-forks from that snapshot; the
// source sandbox keeps running. This matches the mcp.HTTPBackend.Fork and
// Python SDK fork behavior. A replicas value below 1 is treated as 1. On a
// mid-loop failure the error names the already-created ids.
func (b *HostedBackend) Fork(ctx context.Context, sandboxID string, n int) ([]string, error) {
	return b.hb.Fork(ctx, sandboxID, n)
}

// Terminate destroys the sandbox: DELETE /v1/sandboxes/{id}.
func (b *HostedBackend) Terminate(ctx context.Context, sandboxID string) error {
	return b.hb.Terminate(ctx, sandboxID)
}

// hostedSandboxEntry is one element of the GET /v1/sandboxes response.
type hostedSandboxEntry struct {
	ID         string  `json:"id"`
	TemplateID string  `json:"template_id"`
	Endpoint   string  `json:"endpoint"`
	CreatedAt  string  `json:"created_at"`
	ForkTimeMs float64 `json:"fork_time_ms"`
}

// List calls GET /v1/sandboxes and maps the results to SandboxInfo rows. The
// namespace argument is ignored in hosted mode (the api key scopes visibility).
// The Phase is always "Ready" because listed sandboxes are live by definition.
// The Pool field carries the template id, the nearest analog to a pool name.
func (b *HostedBackend) List(ctx context.Context, _ string) ([]SandboxInfo, error) {
	var items []hostedSandboxEntry
	if err := b.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &items); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	now := time.Now()
	out := make([]SandboxInfo, 0, len(items))
	for _, s := range items {
		age := time.Duration(0)
		if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
			age = now.Sub(t)
		}
		out = append(out, SandboxInfo{
			Name:     s.ID,
			Pool:     s.TemplateID,
			Phase:    "Ready",
			Node:     "",
			Endpoint: s.Endpoint,
			Age:      age,
		})
	}
	return out, nil
}

// Workspace returns nil: workspace verbs (ws create/ls/log/fork/revert/rm) are
// cluster-only and require a Kubernetes backend with Workspace CRDs. Use the
// hosted dashboard at https://mitos.run or cluster mode (KUBECONFIG) for
// workspace management.
func (b *HostedBackend) Workspace() WorkspaceBackend {
	return nil
}

// Template returns nil: template build/push require a KVM node and the
// Kubernetes SandboxPool CRD, which are cluster-only. Use cluster mode
// (KUBECONFIG) or the hosted dashboard to manage templates. In hosted mode
// `mitos sandbox create --pool <name>` auto-provisions the template via the
// gateway.
func (b *HostedBackend) Template() TemplateBackend {
	return nil
}

// IsHostedURL reports whether rawURL looks like a hosted endpoint (i.e. NOT a
// localhost / 127.0.0.1 / private-network address) OR if it is the
// DefaultHostedBaseURL. It is used by the CLI to decide whether a --server flag
// alone (without --api-key) implies hosted mode.
func IsHostedURL(rawURL string) bool {
	rawURL = strings.ToLower(strings.TrimRight(rawURL, "/"))
	if rawURL == strings.ToLower(DefaultHostedBaseURL) {
		return true
	}
	// localhost, 127.x, 0.0.0.0, ::1 are local; anything else is "remote".
	lower := rawURL
	for _, local := range []string{
		"http://localhost", "https://localhost",
		"http://127.", "https://127.",
		"http://0.0.0.0", "https://0.0.0.0",
		"http://[::1]", "https://[::1]",
	} {
		if strings.HasPrefix(lower, local) {
			return false
		}
	}
	// A plain http://hostname:port that is not localhost is treated as remote.
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Host != ""
}
