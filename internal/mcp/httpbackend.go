package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	connect "connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// sandboxIDRe is the allowlist for sandbox ids received from tool arguments
// (LLM-controlled). It matches the same pattern used by daemon/validate.go and
// firecracker/validate.go: start with alphanumeric, then up to 63 alphanumeric,
// underscore, or hyphen characters.
var sandboxIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// validSandboxID reports whether id is an acceptable sandbox id to embed in a
// URL path. An empty string, any id containing "/" or "..", or any id that does
// not match the allowlist pattern is rejected.
func validSandboxID(id string) bool {
	return sandboxIDRe.MatchString(id)
}

// HTTPBackend implements SandboxBackend over the standalone sandbox-server. It
// is the simplest real backend: one process, plain HTTP, a single launch-time
// bearer token. A Kubernetes claim backend (create a SandboxClaim, read its
// token Secret, exec via forkd) is a planned follow-up and is intentionally not
// implemented here.
//
// Transport split (issue #358): the runtime calls (exec, file read, file write)
// ride the Connect sandbox.v1.Sandbox protocol via the in-module generated
// connect-go client (ExecStream, ReadFile, WriteFile RPCs); the lifecycle calls
// (create, fork, terminate) stay on the /v1 JSON routes via do. Both reach the
// same baseURL with the same bearer token; the sandbox id rides the
// X-Sandbox-Id header on the runtime RPCs.
//
// Token scoping: every request carries the launch-time bearer token, so the MCP
// server can do exactly what that token authorizes on the sandbox-server and
// nothing more. The token is never logged and never placed in an error message;
// see do, which redacts any echo of the token from a response body before using
// it as error context.
//
// Pool-to-template mapping: the MCP sandbox_create tool takes a "pool" name. The
// sandbox-server has no pools; it forks a sandbox from a named template. The
// HTTP backend therefore treats the pool argument as the template id and forks
// from it (POST /v1/fork {template, id}). On a real k8s deployment a pool maps
// to a SandboxPool; that mapping belongs to the future k8s backend.
type HTTPBackend struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPBackend builds a backend against the sandbox-server at baseURL. When
// token is non-empty it is sent as "Authorization: Bearer <token>" on every
// request. A nil client defaults to http.DefaultClient.
func NewHTTPBackend(baseURL, token string, client *http.Client) *HTTPBackend {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPBackend{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  client,
	}
}

// newSandboxID returns a random hex id for a sandbox or fork. crypto/rand makes
// collisions across concurrent callers negligible without a uuid dependency.
func newSandboxID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never fails on supported platforms; degrade rather than panic.
		return "sbx-fallback"
	}
	return "sbx-" + hex.EncodeToString(b[:])
}

// httpStatusError is a non-2xx /v1 response. The status and (already redacted)
// body stay structured so a caller can branch on the exact failure, e.g. Fork's
// live-route fallback, without parsing the error string. Its Error string is
// byte-for-byte the message do has always produced.
type httpStatusError struct {
	Method string
	Path   string
	Status int
	Body   string // redacted before construction; never carries the token
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// do issues an HTTP request to path with an optional JSON body and decodes a
// 2xx JSON response into out (when out is non-nil). A non-2xx status becomes an
// error carrying the response body as context, with any echo of the bearer
// token redacted first so the secret never reaches a caller, a log, or an LLM.
func (b *HTTPBackend) do(ctx context.Context, method, path string, body any, out any) error {
	return b.doWithHeader(ctx, method, path, nil, body, out)
}

// doWithHeader is do with extra request headers (e.g. Idempotency-Key on a
// fork). A nil header sends none beyond the standard Content-Type and
// Authorization pair. A non-2xx status returns an *httpStatusError.
func (b *HTTPBackend) doWithHeader(ctx context.Context, method, path string, header http.Header, body any, out any) error {
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
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{
			Method: method,
			Path:   path,
			Status: resp.StatusCode,
			Body:   b.redact(string(respBody)),
		}
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// redact removes any occurrence of the bearer token from s. A hostile or
// misconfigured server might echo the Authorization header into its error body;
// this guarantees the token never escapes through an error string.
func (b *HTTPBackend) redact(s string) string {
	if b.token == "" {
		return s
	}
	return strings.ReplaceAll(s, b.token, "[REDACTED]")
}

type forkResponse struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
}

// Create forks a new sandbox from the pool, treating pool as the sandbox-server
// template id. It generates the sandbox id client-side and returns it.
func (b *HTTPBackend) Create(ctx context.Context, pool string) (string, error) {
	id := newSandboxID()
	var resp forkResponse
	if err := b.do(ctx, http.MethodPost, "/v1/fork", map[string]any{
		"template": pool,
		"id":       id,
	}, &resp); err != nil {
		return "", err
	}
	if resp.ID != "" {
		return resp.ID, nil
	}
	return id, nil
}

// sandboxClient builds a Connect client for the runtime RPCs against baseURL,
// mirroring cmd/kubectl-mitos/exec.go and bench/claim/main.go. The per-sandbox
// bearer token and sandbox id are set as request headers by each caller, not
// here, so this stays a thin constructor.
func (b *HTTPBackend) sandboxClient() sandboxv1connect.SandboxClient {
	return sandboxv1connect.NewSandboxClient(b.client, b.baseURL)
}

// runtimeHeaders sets the Authorization and X-Sandbox-Id headers on a Connect
// request. The token is sent only when non-empty, matching the legacy do
// behavior; the token VALUE is never logged.
func (b *HTTPBackend) runtimeHeaders(h http.Header, sandboxID string) {
	if b.token != "" {
		h.Set("Authorization", "Bearer "+b.token)
	}
	h.Set("X-Sandbox-Id", sandboxID)
}

// connectError maps a Connect runtime failure to the backend's error shape. A
// connect unauthenticated code becomes the same auth error the legacy /v1 path
// produced (a 401 from the bearer-token gate); any other failure is wrapped
// with op as context. The token value is redacted from any wrapped cause so it
// never reaches an error string a caller, a log, or an LLM might see.
func (b *HTTPBackend) connectError(op string, err error) error {
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		return fmt.Errorf("%s: status 401: %s", op, b.redact(err.Error()))
	}
	return errors.New(op + ": " + b.redact(err.Error()))
}

// maxTransportExecOutputBytes bounds how many bytes of stdout/stderr Exec
// accumulates in memory PER STREAM while draining the ExecStream RPC, before
// any caller-side truncation (e.g. the console's own 256 KiB cap,
// maxExecOutputBytes in internal/saas/console/sandbox_ops.go). Without this
// cap, a sandbox command that never stops writing to stdout/stderr would
// make this loop buffer without bound: every caller of this transport (the
// CLI, the MCP server, the console) inherits that OOM risk, not just the one
// caller that happens to truncate downstream today. 1 MiB is well above any
// normal command's output (so it does not visibly change today's small/medium
// exec results) while still bounding the worst case; it is deliberately
// larger than the console's 256 KiB downstream cap because this transport is
// also used directly with no downstream truncation of its own (the CLI,
// sandbox-server tests).
const maxTransportExecOutputBytes = 1 << 20 // 1 MiB

// execOutputTruncatedMarker is appended (on its own line) to stdout or
// stderr when either was cut off at maxTransportExecOutputBytes, so a caller
// can tell truncated output apart from a command that genuinely produced
// exactly that many bytes.
const execOutputTruncatedMarker = "\n[exec output truncated at 1 MiB]"

// boundedExecOutput accumulates up to maxTransportExecOutputBytes of a single
// exec stream (stdout or stderr) and silently drops anything beyond the cap
// instead of growing without bound, remembering whether anything was dropped
// so String can append execOutputTruncatedMarker.
type boundedExecOutput struct {
	buf       strings.Builder
	truncated bool
}

// write appends p, capping accumulated bytes at maxTransportExecOutputBytes.
// Once the cap is reached (here or on an earlier call), further bytes are
// dropped and truncated is latched true; write never grows buf past the cap.
func (o *boundedExecOutput) write(p []byte) {
	if o.truncated {
		return
	}
	room := maxTransportExecOutputBytes - o.buf.Len()
	if room <= 0 {
		o.truncated = true
		return
	}
	if len(p) > room {
		p = p[:room]
		o.truncated = true
	}
	o.buf.Write(p)
}

// String returns the accumulated (possibly capped) output, with
// execOutputTruncatedMarker appended when write ever dropped bytes.
func (o *boundedExecOutput) String() string {
	if o.truncated {
		return o.buf.String() + execOutputTruncatedMarker
	}
	return o.buf.String()
}

// Exec runs command in the sandbox over the Connect sandbox.v1.Sandbox/ExecStream
// RPC (the HTTP/1.1-reachable non-interactive exec). It drains the server stream,
// accumulating stdout and stderr chunks (each capped at
// maxTransportExecOutputBytes, see boundedExecOutput) and reading the
// terminal exit code, then folds the result into ExecResult. A timeoutSec of
// 0 passes 0 so the guest default applies.
func (b *HTTPBackend) Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	if !validSandboxID(sandboxID) {
		return ExecResult{}, fmt.Errorf("exec: invalid sandbox id %q", sandboxID)
	}
	timeout := 0
	if timeoutSec > 0 {
		timeout = timeoutSec
	}
	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{
		Command:        command,
		TimeoutSeconds: int32(timeout),
	})
	b.runtimeHeaders(req.Header(), sandboxID)

	stream, err := b.sandboxClient().ExecStream(ctx, req)
	if err != nil {
		return ExecResult{}, b.connectError("exec", err)
	}
	defer func() { _ = stream.Close() }()

	var res ExecResult
	var stdout, stderr boundedExecOutput
	for stream.Receive() {
		msg := stream.Msg()
		if out := msg.GetStdout(); len(out) > 0 {
			stdout.write(out)
		}
		if errOut := msg.GetStderr(); len(errOut) > 0 {
			stderr.write(errOut)
		}
		if exit := msg.GetExit(); exit != nil {
			res.ExitCode = int(exit.GetExitCode())
		}
	}
	if err := stream.Err(); err != nil {
		return ExecResult{}, b.connectError("exec", err)
	}
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	return res, nil
}

// ReadFile reads path from the sandbox over the Connect
// sandbox.v1.Sandbox/ReadFile RPC, concatenating the streamed byte chunks into
// the returned string.
func (b *HTTPBackend) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	if !validSandboxID(sandboxID) {
		return "", fmt.Errorf("read_file: invalid sandbox id %q", sandboxID)
	}
	req := connect.NewRequest(&sandboxv1.ReadFileRequest{Path: path})
	b.runtimeHeaders(req.Header(), sandboxID)

	stream, err := b.sandboxClient().ReadFile(ctx, req)
	if err != nil {
		return "", b.connectError("read_file", err)
	}
	defer func() { _ = stream.Close() }()

	var content bytes.Buffer
	for stream.Receive() {
		if data := stream.Msg().GetData(); len(data) > 0 {
			content.Write(data)
		}
	}
	if err := stream.Err(); err != nil {
		return "", b.connectError("read_file", err)
	}
	return content.String(), nil
}

// WriteFile writes content to path in the sandbox over the Connect
// sandbox.v1.Sandbox/WriteFile RPC: the first message carries the path (the
// guest applies its default mode), then a single content chunk.
func (b *HTTPBackend) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	if !validSandboxID(sandboxID) {
		return fmt.Errorf("write_file: invalid sandbox id %q", sandboxID)
	}
	stream := b.sandboxClient().WriteFile(ctx)
	b.runtimeHeaders(stream.RequestHeader(), sandboxID)

	if err := stream.Send(&sandboxv1.WriteFileRequest{
		Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{Path: path}},
	}); err != nil {
		return b.connectError("write_file", err)
	}
	if len(content) > 0 {
		if err := stream.Send(&sandboxv1.WriteFileRequest{
			Msg: &sandboxv1.WriteFileRequest_Data{Data: []byte(content)},
		}); err != nil {
			return b.connectError("write_file", err)
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		return b.connectError("write_file", err)
	}
	return nil
}

// idempotencyHeader is the header carrying the fork idempotency key, matching
// the Python and Go SDKs, so a transparently retried fork never double-creates
// a child (issue #22).
const idempotencyHeader = "Idempotency-Key"

// newIdempotencyKey returns a random hex Idempotency-Key. An empty string (the
// never-in-practice crypto/rand failure) means the header is simply omitted.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// isLiveForkRouteMissing reports whether err is a ROUTE-LEVEL 404 for the
// per-sandbox live-fork route: the server does not serve the route at all
// (an older gateway without the sandbox.fork op, or an older standalone
// sandbox-server whose mux answers a plain "404 page not found"). A 404 whose
// error envelope names a missing SANDBOX or POOL is a real answer about the
// fork source and must never be mistaken for a missing route.
func isLiveForkRouteMissing(err error) bool {
	var se *httpStatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		return false
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(se.Body), &env); jsonErr == nil && env.Error.Code != "" {
		if env.Error.Code != "not_found" {
			return false
		}
		// The older gateway's route-level 404 says "no such route or operation";
		// a missing source says "no such sandbox" and a pool lookup says "no such
		// pool", neither of which mentions a route or operation.
		msg := strings.ToLower(env.Error.Message)
		return strings.Contains(msg, "route") || strings.Contains(msg, "operation")
	}
	// Not the /v1 error envelope: an older standalone sandbox-server without
	// the route answers with net/http's default "404 page not found".
	return strings.Contains(strings.ToLower(se.Body), "page not found")
}

// Fork forks the RUNNING sandbox replicas times via the per-sandbox live-fork
// route: one POST /v1/sandboxes/{id}/fork per child with {"id": <child>,
// "pause_source": true} and a fresh Idempotency-Key, exactly like the Python
// SDK's _fork_one. The server checkpoints the running source (memory plus
// on-disk filesystem, captured while the source is paused so the two are
// consistent) and boots each child from that checkpoint; the response is
// create-shaped (id, endpoint, token, phase, template_id, fork_time_ms).
//
// The previous behavior (POST /v1/fork with the SOURCE SANDBOX ID in the
// template field) only worked when that id happened to name a template; the
// hosted control plane resolves the template field as a pool name, so it
// failed with 404 `no such pool "sb-..."`.
//
// Compatibility: a server that does not serve the live-fork route answers the
// FIRST call with a route-level 404 (see isLiveForkRouteMissing); Fork then
// falls back ONCE to the legacy flat template route (POST /v1/fork
// {template: sandboxID, id}) for every child. A 404 naming a missing sandbox
// or pool is a real failure and is returned, never treated as a missing route.
//
// A replicas value below 1 is treated as 1. On a mid-loop failure, the error
// wraps the already-created ids so callers can terminate them:
// "fork created [id1 id2] before failing: <cause>".
func (b *HTTPBackend) Fork(ctx context.Context, sandboxID string, replicas int) ([]string, error) {
	if !validSandboxID(sandboxID) {
		return nil, fmt.Errorf("fork: invalid sandbox id %q", sandboxID)
	}
	if replicas < 1 {
		replicas = 1
	}
	livePath := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/fork"
	useFlat := false
	ids := make([]string, 0, replicas)
	for i := 0; i < replicas; i++ {
		id := newSandboxID()
		var resp forkResponse
		var err error
		if !useFlat {
			header := http.Header{}
			if key := newIdempotencyKey(); key != "" {
				header.Set(idempotencyHeader, key)
			}
			err = b.doWithHeader(ctx, http.MethodPost, livePath, header, map[string]any{
				"id":           id,
				"pause_source": true,
			}, &resp)
			if err != nil && i == 0 && isLiveForkRouteMissing(err) {
				// Older server without the live-fork route: fall back once to
				// the legacy flat template route for this and every later child.
				useFlat = true
			}
		}
		if useFlat {
			resp = forkResponse{}
			err = b.do(ctx, http.MethodPost, "/v1/fork", map[string]any{
				"template": sandboxID,
				"id":       id,
			}, &resp)
		}
		if err != nil {
			return ids, fmt.Errorf("fork created %v before failing: %w", ids, err)
		}
		if resp.ID != "" {
			ids = append(ids, resp.ID)
		} else {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Terminate destroys the sandbox via DELETE /v1/sandboxes/{id}. The sandbox id
// is validated against the allowlist and path-escaped before being embedded in
// the URL so an LLM-controlled id cannot redirect the DELETE to an unintended
// path.
func (b *HTTPBackend) Terminate(ctx context.Context, sandboxID string) error {
	if !validSandboxID(sandboxID) {
		return fmt.Errorf("terminate: invalid sandbox id %q", sandboxID)
	}
	return b.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(sandboxID), nil, nil)
}
