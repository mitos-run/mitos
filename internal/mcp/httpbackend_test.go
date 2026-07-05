package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// fakeSandbox is a Connect sandbox.v1.Sandbox handler for the runtime RPCs the
// HTTPBackend now rides (issue #358). It implements ExecStream, ReadFile, and
// WriteFile with canned data and records the Authorization and X-Sandbox-Id
// headers and request fields each call carried so tests can assert auth and
// round-trip. requireToken, when set, rejects any call whose bearer token does
// not match with connect CodeUnauthenticated, modeling the forkd token gate.
type fakeSandbox struct {
	sandboxv1connect.UnimplementedSandboxHandler

	requireToken string

	// recorded inputs.
	execAuth       string
	execSandboxID  string
	execCommand    string
	execTimeout    int32
	readAuth       string
	readSandboxID  string
	readPath       string
	writeAuth      string
	writeSandboxID string
	writePath      string
	writeContent   []byte

	// canned exec output.
	execStdout   string
	execStderr   string
	execExitCode int32
	// canned file content for ReadFile.
	readContent string
}

func (f *fakeSandbox) checkToken(h http.Header) error {
	if f.requireToken == "" {
		return nil
	}
	if h.Get("Authorization") != "Bearer "+f.requireToken {
		// Hostile-server modeling: echo the presented Authorization header (which
		// carries the client's bearer token) into the error so the leak test can
		// prove the backend redacts it before surfacing the error.
		return connect.NewError(connect.CodeUnauthenticated, errors.New("rejected: "+h.Get("Authorization")))
	}
	return nil
}

func (f *fakeSandbox) ExecStream(_ context.Context, req *connect.Request[sandboxv1.ExecStreamRequest], stream *connect.ServerStream[sandboxv1.ExecResponse]) error {
	if err := f.checkToken(req.Header()); err != nil {
		return err
	}
	f.execAuth = req.Header().Get("Authorization")
	f.execSandboxID = req.Header().Get("X-Sandbox-Id")
	f.execCommand = req.Msg.GetCommand()
	f.execTimeout = req.Msg.GetTimeoutSeconds()
	if f.execStdout != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte(f.execStdout)}}); err != nil {
			return err
		}
	}
	if f.execStderr != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: []byte(f.execStderr)}}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: f.execExitCode}}})
}

func (f *fakeSandbox) ReadFile(_ context.Context, req *connect.Request[sandboxv1.ReadFileRequest], stream *connect.ServerStream[sandboxv1.Chunk]) error {
	if err := f.checkToken(req.Header()); err != nil {
		return err
	}
	f.readAuth = req.Header().Get("Authorization")
	f.readSandboxID = req.Header().Get("X-Sandbox-Id")
	f.readPath = req.Msg.GetPath()
	if f.readContent != "" {
		if err := stream.Send(&sandboxv1.Chunk{Data: []byte(f.readContent)}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}

func (f *fakeSandbox) WriteFile(_ context.Context, stream *connect.ClientStream[sandboxv1.WriteFileRequest]) (*connect.Response[sandboxv1.WriteFileResult], error) {
	if err := f.checkToken(stream.RequestHeader()); err != nil {
		return nil, err
	}
	f.writeAuth = stream.RequestHeader().Get("Authorization")
	f.writeSandboxID = stream.RequestHeader().Get("X-Sandbox-Id")
	var written int64
	for stream.Receive() {
		msg := stream.Msg()
		if open := msg.GetOpen(); open != nil {
			f.writePath = open.GetPath()
		}
		if data := msg.GetData(); len(data) > 0 {
			f.writeContent = append(f.writeContent, data...)
			written += int64(len(data))
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&sandboxv1.WriteFileResult{BytesWritten: written}), nil
}

// connectServer stands up an httptest.Server that serves the Connect
// sandbox.v1.Sandbox handler (runtime RPCs) AND the legacy /v1/fork and
// /v1/sandboxes/ lifecycle JSON routes on one mux, so a single HTTPBackend can
// exercise both transports against one baseURL. lifecycle handles the legacy
// routes; it may be nil when a test needs only the runtime RPCs.
func connectServer(t *testing.T, fake *fakeSandbox, lifecycle http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := sandboxv1connect.NewSandboxHandler(fake)
	mux.Handle(path, handler)
	if lifecycle != nil {
		mux.HandleFunc("/v1/", lifecycle)
	}
	return httptest.NewServer(mux)
}

// capturedRequest records what the backend sent so assertions can inspect the
// method, path, headers, and decoded body of each HTTP call.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// recordingServer returns an httptest.Server that records every request into
// got and replies with the per-path canned response. The handler func decides
// the response body and status for a given request.
func recordingServer(t *testing.T, got *[]capturedRequest, handler func(cr capturedRequest) (int, any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cr := capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &cr.Body)
		}
		*got = append(*got, cr)
		status, body := handler(cr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

func TestHTTPBackendCreate(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"], "template_id": cr.Body["template"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	id, err := b.Create(context.Background(), "python")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	req := got[0]
	if req.Method != http.MethodPost || req.Path != "/v1/fork" {
		t.Fatalf("Create sent %s %s, want POST /v1/fork", req.Method, req.Path)
	}
	if req.Auth != "Bearer tok-123" {
		t.Fatalf("Create auth = %q, want Bearer tok-123", req.Auth)
	}
	if req.Body["template"] != "python" {
		t.Fatalf("Create body template = %v, want python", req.Body["template"])
	}
	if req.Body["id"] != id {
		t.Fatalf("Create body id %v != returned id %s", req.Body["id"], id)
	}
}

// TestHTTPBackendExec asserts Exec rides the Connect ExecStream RPC: it folds
// stdout chunks and the terminal exit code into ExecResult, passes the timeout
// through, and carries BOTH the bearer token (Authorization) and the sandbox id
// (X-Sandbox-Id) on the request.
func TestHTTPBackendExec(t *testing.T) {
	fake := &fakeSandbox{execStdout: "out", execStderr: "err", execExitCode: 7}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	res, err := b.Exec(context.Background(), "sbx-1", "echo hi", 12)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("Exec result = %+v", res)
	}
	if fake.execAuth != "Bearer tok-123" {
		t.Fatalf("Exec Authorization = %q, want Bearer tok-123", fake.execAuth)
	}
	if fake.execSandboxID != "sbx-1" {
		t.Fatalf("Exec X-Sandbox-Id = %q, want sbx-1", fake.execSandboxID)
	}
	if fake.execCommand != "echo hi" {
		t.Fatalf("Exec command = %q, want echo hi", fake.execCommand)
	}
	if fake.execTimeout != 12 {
		t.Fatalf("Exec timeout = %d, want 12", fake.execTimeout)
	}
}

// TestHTTPBackendExecZeroTimeout asserts a timeoutSec of 0 passes 0 on the wire
// so the guest default applies.
func TestHTTPBackendExecZeroTimeout(t *testing.T) {
	fake := &fakeSandbox{execExitCode: 0}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	if _, err := b.Exec(context.Background(), "sbx-1", "true", 0); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if fake.execTimeout != 0 {
		t.Fatalf("Exec timeout = %d, want 0", fake.execTimeout)
	}
}

// TestHTTPBackendExecCapsOutputAtOneMiB asserts a command whose stdout (or
// stderr) exceeds maxTransportExecOutputBytes is capped there with a trailing
// truncation marker, rather than Exec buffering the whole thing in memory
// without bound.
func TestHTTPBackendExecCapsOutputAtOneMiB(t *testing.T) {
	big := strings.Repeat("a", 2*maxTransportExecOutputBytes) // well past the 1 MiB cap
	fake := &fakeSandbox{execStdout: big, execStderr: big, execExitCode: 0}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "", srv.Client())
	res, err := b.Exec(context.Background(), "sbx-1", "produce-lots-of-output", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for name, got := range map[string]string{"stdout": res.Stdout, "stderr": res.Stderr} {
		if !strings.HasSuffix(got, execOutputTruncatedMarker) {
			t.Fatalf("%s missing truncation marker, len=%d", name, len(got))
		}
		body := strings.TrimSuffix(got, execOutputTruncatedMarker)
		if len(body) != maxTransportExecOutputBytes {
			t.Fatalf("%s truncated body len = %d, want %d", name, len(body), maxTransportExecOutputBytes)
		}
	}
}

// TestHTTPBackendExecUnderCapIsUntouched asserts output well under the cap is
// returned exactly, with no truncation marker appended.
func TestHTTPBackendExecUnderCapIsUntouched(t *testing.T) {
	fake := &fakeSandbox{execStdout: "small output", execExitCode: 0}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "", srv.Client())
	res, err := b.Exec(context.Background(), "sbx-1", "echo hi", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "small output" {
		t.Fatalf("Stdout = %q, want unmodified %q", res.Stdout, "small output")
	}
}

// TestHTTPBackendReadFile asserts ReadFile rides the Connect ReadFile RPC,
// concatenating the streamed chunks, and carries both auth headers.
func TestHTTPBackendReadFile(t *testing.T) {
	fake := &fakeSandbox{readContent: "hello"}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	content, err := b.ReadFile(context.Background(), "sbx-1", "/etc/hosts")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "hello" {
		t.Fatalf("ReadFile content = %q", content)
	}
	if fake.readAuth != "Bearer tok-123" {
		t.Fatalf("ReadFile Authorization = %q", fake.readAuth)
	}
	if fake.readSandboxID != "sbx-1" {
		t.Fatalf("ReadFile X-Sandbox-Id = %q, want sbx-1", fake.readSandboxID)
	}
	if fake.readPath != "/etc/hosts" {
		t.Fatalf("ReadFile path = %q, want /etc/hosts", fake.readPath)
	}
}

// TestHTTPBackendWriteFile asserts WriteFile rides the Connect WriteFile
// client-stream RPC: the open carries the path, the content round-trips, and
// both auth headers ride the request.
func TestHTTPBackendWriteFile(t *testing.T) {
	fake := &fakeSandbox{}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	if err := b.WriteFile(context.Background(), "sbx-1", "/tmp/x", "data"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if fake.writeAuth != "Bearer tok-123" {
		t.Fatalf("WriteFile Authorization = %q", fake.writeAuth)
	}
	if fake.writeSandboxID != "sbx-1" {
		t.Fatalf("WriteFile X-Sandbox-Id = %q, want sbx-1", fake.writeSandboxID)
	}
	if fake.writePath != "/tmp/x" {
		t.Fatalf("WriteFile path = %q, want /tmp/x", fake.writePath)
	}
	if string(fake.writeContent) != "data" {
		t.Fatalf("WriteFile content = %q, want data", string(fake.writeContent))
	}
}

func TestHTTPBackendFork(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"], "template_id": cr.Body["template"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-1", 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("Fork returned %d ids, want 3", len(ids))
	}
	if len(got) != 3 {
		t.Fatalf("Fork made %d requests, want 3", len(got))
	}
	seen := map[string]bool{}
	for i, req := range got {
		if req.Method != http.MethodPost || req.Path != "/v1/fork" {
			t.Fatalf("Fork req %d = %s %s", i, req.Method, req.Path)
		}
		if req.Auth != "Bearer tok-123" {
			t.Fatalf("Fork req %d auth = %q", i, req.Auth)
		}
		id, _ := req.Body["id"].(string)
		if id == "" || seen[id] {
			t.Fatalf("Fork req %d had empty or duplicate id %q", i, id)
		}
		seen[id] = true
	}
}

func TestHTTPBackendForkDefaultsToOne(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-1", 0)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 1 || len(got) != 1 {
		t.Fatalf("Fork with 0 replicas made %d reqs / %d ids, want 1/1", len(got), len(ids))
	}
}

func TestHTTPBackendTerminate(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"status": "terminated"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	if err := b.Terminate(context.Background(), "sbx-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	req := got[0]
	if req.Method != http.MethodDelete || req.Path != "/v1/sandboxes/sbx-1" {
		t.Fatalf("Terminate sent %s %s, want DELETE /v1/sandboxes/sbx-1", req.Method, req.Path)
	}
	if req.Auth != "Bearer tok-123" {
		t.Fatalf("Terminate auth = %q", req.Auth)
	}
}

func TestHTTPBackendNoTokenOmitsHeader(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "", srv.Client())
	if _, err := b.Create(context.Background(), "python"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got[0].Auth != "" {
		t.Fatalf("expected no Authorization header without a token, got %q", got[0].Auth)
	}
}

func TestHTTPBackendNon2xxIsError(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusNotFound, map[string]any{"error": "template \"nope\" not found"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	_, err := b.Create(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected an error on non-2xx response")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should carry response body context, got %q", err.Error())
	}
}

// TestHTTPBackendNeverLeaksToken asserts the token never appears in an error
// returned to the caller, even when the server echoes the Authorization header
// (which carries the token) back in its error message. The backend must not
// log; this test guards the error path, the only string the backend surfaces.
func TestHTTPBackendNeverLeaksToken(t *testing.T) {
	const token = "super-secret-token-value"
	// The fake requires a DIFFERENT token, so the presented (correct-for-client)
	// token is rejected and echoed into the connect error message.
	fake := &fakeSandbox{requireToken: "the-expected-token"}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, token, srv.Client())
	_, err := b.Exec(context.Background(), "sbx-1", "x", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestHTTPBackendExecAuthError asserts a rejected bearer token (connect
// unauthenticated) maps to an auth error that names the 401 status, mirroring
// the legacy /v1 bearer-token gate, and never leaks the token.
func TestHTTPBackendExecAuthError(t *testing.T) {
	const token = "client-token"
	fake := &fakeSandbox{requireToken: "server-expects-other"}
	srv := connectServer(t, fake, nil)
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, token, srv.Client())
	_, err := b.Exec(context.Background(), "sbx-1", "x", 0)
	if err == nil {
		t.Fatal("expected an auth error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("auth error should name 401, got %q", err.Error())
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into auth error: %q", err.Error())
	}
}

// TestHTTPBackendUnsafeIDRejected asserts that Terminate and Exec with a
// path-traversal sandbox id return an error without sending any HTTP request.
func TestHTTPBackendUnsafeIDRejected(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"status": "ok"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())

	unsafeIDs := []string{"../../x", "../etc/passwd", "", "sbx/bad", "sbx..bad"}
	for _, id := range unsafeIDs {
		err := b.Terminate(context.Background(), id)
		if err == nil {
			t.Errorf("Terminate(%q): expected error, got nil", id)
		}
		_, err = b.Exec(context.Background(), id, "echo", 0)
		if err == nil {
			t.Errorf("Exec(%q): expected error, got nil", id)
		}
	}
	if len(got) != 0 {
		t.Errorf("backend sent %d requests, want 0", len(got))
	}
}

// TestHTTPBackendForkPartialIDsInError asserts that when a mid-loop fork fails,
// the returned error names the sandbox ids that were already created so the
// caller can terminate them.
func TestHTTPBackendForkPartialIDsInError(t *testing.T) {
	call := 0
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		call++
		if call == 1 {
			// First fork succeeds; capture the id it used.
			return http.StatusOK, map[string]any{"id": cr.Body["id"]}
		}
		// Second fork fails.
		return http.StatusInternalServerError, map[string]any{"error": "quota exceeded"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-src", 2)
	if err == nil {
		t.Fatal("expected error on second fork, got nil")
	}
	// The error must name the id created in the first (successful) call.
	if len(ids) != 1 {
		t.Fatalf("expected 1 partial id returned, got %d: %v", len(ids), ids)
	}
	if !strings.Contains(err.Error(), ids[0]) {
		t.Errorf("error %q does not mention created id %q", err.Error(), ids[0])
	}
}

// TestHTTPBackendTerminatePathEscape asserts that a valid sandbox id that
// contains URL-special chars (none allowed by validSandboxID, but an id whose
// PathEscape is a no-op for safe chars) is embedded correctly.
func TestHTTPBackendTerminatePathEscape(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, nil
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())
	// sbx-abc is valid and path-safe; confirm the path is built correctly.
	if err := b.Terminate(context.Background(), "sbx-abc"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/v1/sandboxes/sbx-abc" {
		t.Fatalf("path = %q, want /v1/sandboxes/sbx-abc", got[0].Path)
	}
}
