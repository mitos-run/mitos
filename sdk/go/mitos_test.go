package mitos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubServer spins an httptest server reproducing the sandbox-server wire shapes
// and records the last request's headers so tests can assert on the
// Idempotency-Key and Authorization headers.
type stubServer struct {
	t            *testing.T
	srv          *httptest.Server
	lastHeader   http.Header
	lastMethod   string
	lastPath     string
	lastBodyJSON map[string]any
}

func newStubServer(t *testing.T, handler http.HandlerFunc) *stubServer {
	t.Helper()
	s := &stubServer{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastHeader = r.Header.Clone()
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path
		s.lastBodyJSON = nil
		if r.Body != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				s.lastBodyJSON = body
			}
		}
		handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientFor builds a SandboxServer pointed at the stub with no env interference.
func clientFor(t *testing.T, url string, opts ...Option) *SandboxServer {
	t.Helper()
	t.Setenv("MITOS_BASE_URL", "")
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir()) // isolate from any real ~/.config
	all := append([]Option{WithBaseURL(url)}, opts...)
	return NewSandboxServer(all...)
}

func TestCreateTemplate(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":               "python",
			"ready":            true,
			"created_at":       "2026-06-23T00:00:00Z",
			"creation_time_ms": 123.0,
		})
	})
	srv := clientFor(t, stub.srv.URL)
	tmpl, err := srv.CreateTemplate(context.Background(), "python")
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if tmpl.ID != "python" || !tmpl.Ready {
		t.Fatalf("got %+v, want id=python ready=true", tmpl)
	}
	if stub.lastHeader.Get("Idempotency-Key") == "" {
		t.Fatalf("CreateTemplate did not send an Idempotency-Key")
	}
	if stub.lastBodyJSON["init_wait_seconds"].(float64) != float64(DefaultInitWaitSeconds) {
		t.Fatalf("init_wait_seconds = %v, want %d", stub.lastBodyJSON["init_wait_seconds"], DefaultInitWaitSeconds)
	}
}

func TestListTemplates(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": "python", "ready": true, "created_at": "t", "creation_time_ms": 1.0},
			{"id": "node", "ready": false, "created_at": "t", "creation_time_ms": 2.0},
		})
	})
	srv := clientFor(t, stub.srv.URL)
	tmpls, err := srv.ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tmpls) != 2 || tmpls[0].ID != "python" || tmpls[1].ID != "node" {
		t.Fatalf("got %+v", tmpls)
	}
	if stub.lastMethod != http.MethodGet || stub.lastPath != "/v1/templates" {
		t.Fatalf("got %s %s", stub.lastMethod, stub.lastPath)
	}
}

func TestForkReturnsSandboxWithIdempotencyKey(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":           "sb-1",
			"template_id":  "python",
			"endpoint":     "http://node",
			"fork_time_ms": 5.0,
		})
	})
	srv := clientFor(t, stub.srv.URL)
	sb, err := srv.Fork(context.Background(), "python", "sb-1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if sb.ID != "sb-1" || sb.Template != "python" {
		t.Fatalf("got %+v", sb)
	}
	if stub.lastHeader.Get("Idempotency-Key") == "" {
		t.Fatalf("Fork did not send an Idempotency-Key")
	}
	if stub.lastBodyJSON["template"] != "python" || stub.lastBodyJSON["id"] != "sb-1" {
		t.Fatalf("fork body = %+v", stub.lastBodyJSON)
	}
}

func TestForkGeneratesSandboxID(t *testing.T) {
	var seenID string
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":           "", // server echoes nothing; SDK falls back to generated id
			"template_id":  "python",
			"endpoint":     "http://node",
			"fork_time_ms": 1.0,
		})
	})
	srv := clientFor(t, stub.srv.URL)
	sb, err := srv.Fork(context.Background(), "python", "")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	seenID, _ = stub.lastBodyJSON["id"].(string)
	if !strings.HasPrefix(seenID, "sandbox-") {
		t.Fatalf("generated id %q lacks sandbox- prefix", seenID)
	}
	if !ValidSandboxID(seenID) {
		t.Fatalf("generated id %q is not valid", seenID)
	}
	if sb.ID != seenID {
		t.Fatalf("handle id %q != sent id %q", sb.ID, seenID)
	}
}

func TestForkInvalidIDReturnsTypedErrorBeforeRequest(t *testing.T) {
	called := false
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	srv := clientFor(t, stub.srv.URL)
	_, err := srv.Fork(context.Background(), "python", "bad id!")
	if err == nil {
		t.Fatalf("expected error for invalid id")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not *Error", err)
	}
	if e.Code != "invalid_sandbox_id" {
		t.Fatalf("code = %q, want invalid_sandbox_id", e.Code)
	}
	if e.Status != 0 {
		t.Fatalf("status = %d, want 0 (no request)", e.Status)
	}
	if called {
		t.Fatalf("a request was sent despite the invalid id")
	}
}

func TestExecRoundTrip(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/fork" {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "sb-1", "template_id": "python", "endpoint": "http://node", "fork_time_ms": 1.0,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exit_code": 0, "stdout": "hello\n", "stderr": "", "exec_time_ms": 4.0,
		})
	})
	srv := clientFor(t, stub.srv.URL)
	sb, err := srv.Fork(context.Background(), "python", "sb-1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	res, err := sb.Exec(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("got %+v", res)
	}
	if stub.lastBodyJSON["command"] != "echo hello" || stub.lastBodyJSON["sandbox"] != "sb-1" {
		t.Fatalf("exec body = %+v", stub.lastBodyJSON)
	}
	if stub.lastBodyJSON["timeout"].(float64) != float64(DefaultExecTimeoutSeconds) {
		t.Fatalf("timeout = %v", stub.lastBodyJSON["timeout"])
	}
}

func TestTerminateIssuesDelete(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/fork" {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "sb-1", "template_id": "python", "endpoint": "http://node", "fork_time_ms": 1.0,
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := clientFor(t, stub.srv.URL)
	sb, err := srv.Fork(context.Background(), "python", "sb-1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := sb.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if stub.lastMethod != http.MethodDelete || stub.lastPath != "/v1/sandboxes/sb-1" {
		t.Fatalf("got %s %s, want DELETE /v1/sandboxes/sb-1", stub.lastMethod, stub.lastPath)
	}
}

func TestListSandboxes(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": "sb-1", "template_id": "python", "endpoint": "e", "created_at": "t", "fork_time_ms": 1.0},
		})
	})
	srv := clientFor(t, stub.srv.URL)
	out, err := srv.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(out) != 1 || out[0].ID != "sb-1" || out[0].TemplateID != "python" {
		t.Fatalf("got %+v", out)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	t.Setenv("MITOS_BASE_URL", "")
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	srv := NewSandboxServer()
	if srv.URL() != DefaultBaseURL {
		t.Fatalf("URL() = %q, want %q", srv.URL(), DefaultBaseURL)
	}
}

func TestBaseURLPrecedence(t *testing.T) {
	t.Setenv("MITOS_BASE_URL", "http://from-env:8080/")
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())

	// Env beats the default.
	if got := NewSandboxServer().URL(); got != "http://from-env:8080" {
		t.Fatalf("env URL = %q", got)
	}
	// Option beats env, and a trailing slash is trimmed.
	if got := NewSandboxServer(WithBaseURL("http://opt:9090/")).URL(); got != "http://opt:9090" {
		t.Fatalf("option URL = %q", got)
	}
}

func TestTokenCredentialFileFallbackAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"token":"file-token"}`), 0o600); err != nil {
		t.Fatalf("write cred file: %v", err)
	}
	t.Setenv("MITOS_CONFIG_DIR", dir)
	t.Setenv("MITOS_BASE_URL", "")

	// The Authorization header is observed by a stub for each precedence case.
	check := func(name, wantBearer string, opts ...Option) {
		t.Helper()
		var seen string
		stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
			writeJSON(w, http.StatusOK, []map[string]any{})
		})
		all := append([]Option{WithBaseURL(stub.srv.URL)}, opts...)
		srv := NewSandboxServer(all...)
		if _, err := srv.ListTemplates(context.Background()); err != nil {
			t.Fatalf("%s: ListTemplates: %v", name, err)
		}
		if seen != wantBearer {
			t.Fatalf("%s: Authorization = %q, want %q", name, seen, wantBearer)
		}
	}

	// File fallback: no option, no env.
	t.Setenv("MITOS_API_KEY", "")
	check("file", "Bearer file-token")

	// Env beats file.
	t.Setenv("MITOS_API_KEY", "env-token")
	check("env-beats-file", "Bearer env-token")

	// Option beats env and file.
	check("option-beats-all", "Bearer opt-token", WithAPIKey("opt-token"))
}

func TestErrorEnvelopeParsed(t *testing.T) {
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"code":        "not_found",
				"message":     "no such sandbox",
				"cause":       "sandbox sb-x does not exist",
				"remediation": "Fork a sandbox first.",
			},
		})
	})
	srv := clientFor(t, stub.srv.URL)
	_, err := srv.ListSandboxes(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not *Error", err)
	}
	if e.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", e.Code)
	}
	if e.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", e.Status)
	}
	if !errors.Is(err, &Error{Code: "not_found"}) {
		t.Fatalf("errors.Is did not match code not_found")
	}
	if errors.Is(err, &Error{Code: "internal"}) {
		t.Fatalf("errors.Is matched the wrong code")
	}
}

func TestAPIKeyNeverInErrorString(t *testing.T) {
	const secret = "sk-super-secret-value"
	// A hostile/misconfigured server reflects the bearer token back in its error
	// body; the SDK must redact it.
	stub := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"code":        "unauthorized",
				"message":     "bad token " + secret,
				"cause":       "rejected " + secret,
				"remediation": "rotate " + secret,
			},
		})
	})
	t.Setenv("MITOS_BASE_URL", "")
	t.Setenv("MITOS_API_KEY", "")
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	srv := NewSandboxServer(WithBaseURL(stub.srv.URL), WithAPIKey(secret))
	_, err := srv.ListSandboxes(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("api key leaked into err.Error(): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redaction marker in %q", err.Error())
	}
}
