package daemon

// Bearer-token auth tests for the HTTP sandbox API.
//
// The token model: forkd registers one token per sandbox at fork time
// (Server.Fork / Server.ForkRunning). Every HTTP request must present
// Authorization: Bearer <token> for the sandbox named in its JSON body.
// A sandbox with no registered token fails closed (401) unless the API
// was built with AllowTokenless (standalone sandbox-server only).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/fork"
)

// newAuthTestAPI builds a SandboxAPI with a connected gRPC fake agent for
// sandbox "sb-auth" and returns the API plus an httptest server over its
// Handler.
func newAuthTestAPI(t *testing.T) (*SandboxAPI, *httptest.Server) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tok")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "vsock.sock")
	fake := &fakeGuestSandbox{
		execStdout: "hi\n",
		execExit:   0,
	}
	startFakeGuestGRPCUDS(t, sockPath, fake)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox("sb-auth", sockPath); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-auth", sockPath)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return api, ts
}

func postExec(t *testing.T, url, sandbox, bearer string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"sandbox": sandbox, "command": "echo hi"})
	req, err := http.NewRequest(http.MethodPost, url+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.String()
}

func TestHandlerWithValidBearerSucceeds(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.RegisterToken("sb-auth", "tok-correct")

	resp, body := postExec(t, ts.URL, "sb-auth", "tok-correct")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s, want 200", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("exec result not returned: %s", body)
	}
}

func TestHandlerWithoutBearerIs401(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.RegisterToken("sb-auth", "tok-correct")

	resp, body := postExec(t, ts.URL, "sb-auth", "")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, body = %s, want 401", resp.StatusCode, body)
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("401 must carry a JSON error: %s", body)
	}
}

func TestHandlerWithWrongBearerIs401(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.RegisterToken("sb-auth", "tok-correct")

	resp, body := postExec(t, ts.URL, "sb-auth", "tok-wrong")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, body = %s, want 401", resp.StatusCode, body)
	}
	// The 401 envelope must never reflect the presented bearer back to the
	// caller; the cause is a fixed string, not the supplied token.
	if strings.Contains(body, "tok-wrong") {
		t.Fatalf("401 body reflected the presented token: %s", body)
	}
}

func TestHandlerNoTokenRegisteredFailsClosed(t *testing.T) {
	// Sandbox registered but no token: every request 401s, even with a
	// bearer; there is nothing to compare against.
	_, ts := newAuthTestAPI(t)

	resp, _ := postExec(t, ts.URL, "sb-auth", "")
	if resp.StatusCode != 401 {
		t.Fatalf("tokenless sandbox without AllowTokenless: status = %d, want 401", resp.StatusCode)
	}
	resp, _ = postExec(t, ts.URL, "sb-auth", "anything")
	if resp.StatusCode != 401 {
		t.Fatalf("bearer against no registered token: status = %d, want 401", resp.StatusCode)
	}
}

func TestHandlerUnknownSandboxIs401(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.RegisterToken("sb-auth", "tok-correct")

	// Unknown sandbox has no token registered: 401 before any agent lookup.
	resp, _ := postExec(t, ts.URL, "sb-ghost", "tok-correct")
	if resp.StatusCode != 401 {
		t.Fatalf("unknown sandbox: status = %d, want 401", resp.StatusCode)
	}
}

func TestAllowTokenlessPermitsOnlyTokenlessSandboxes(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.AllowTokenless()

	// No token registered: tokenless request passes through to the agent.
	resp, body := postExec(t, ts.URL, "sb-auth", "")
	if resp.StatusCode != 200 {
		t.Fatalf("tokenless with AllowTokenless: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	// Once a token IS registered, AllowTokenless does not bypass it.
	api.RegisterToken("sb-auth", "tok-correct")
	resp, _ = postExec(t, ts.URL, "sb-auth", "")
	if resp.StatusCode != 401 {
		t.Fatalf("registered token must still be enforced: status = %d, want 401", resp.StatusCode)
	}
	resp, _ = postExec(t, ts.URL, "sb-auth", "tok-correct")
	if resp.StatusCode != 200 {
		t.Fatalf("correct bearer with AllowTokenless: status = %d, want 200", resp.StatusCode)
	}
}

func TestUnregisterSandboxClearsToken(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.AllowTokenless()
	api.RegisterToken("sb-auth", "tok-correct")

	api.UnregisterSandbox("sb-auth")

	// Token gone: under AllowTokenless the request passes auth again and
	// then 404s on the missing sandbox; the old token must not linger.
	resp, body := postExec(t, ts.URL, "sb-auth", "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, body = %s, want 404 (auth passed, sandbox gone)", resp.StatusCode, body)
	}
}

func TestForkRegistersTokenOnServer(t *testing.T) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	api := NewSandboxAPI(t.TempDir())
	srv := NewServer(engine, api)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	if _, err := srv.Fork(context.Background(), "py", "sb-tok", nil, nil, nil, nil, "tok-fork", VitalsLabels{}); err != nil {
		t.Fatal(err)
	}

	// Without the bearer: 401.
	resp, _ := postExec(t, ts.URL, "sb-tok", "")
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// With the bearer: auth passes; mock mode has no vsock path, so the request
	// reaches the handler and 404s with the sandbox-missing error. That
	// distinction (404 not 401) is the proof the token was registered.
	resp, body := postExec(t, ts.URL, "sb-tok", "tok-fork")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, body = %s, want 404", resp.StatusCode, body)
	}
	if !strings.Contains(body, "not found or not registered") {
		t.Fatalf("want sandbox-missing error, got: %s", body)
	}
}

func TestForkWithEmptyTokenFailsClosed(t *testing.T) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	api := NewSandboxAPI(t.TempDir())
	srv := NewServer(engine, api)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	if _, err := srv.Fork(context.Background(), "py", "sb-naked", nil, nil, nil, nil, "", VitalsLabels{}); err != nil {
		t.Fatal(err)
	}

	// Empty api_token registers NO token: all HTTP access fails closed.
	for _, bearer := range []string{"", "guess"} {
		resp, _ := postExec(t, ts.URL, "sb-naked", bearer)
		if resp.StatusCode != 401 {
			t.Fatalf("bearer %q: status = %d, want 401", bearer, resp.StatusCode)
		}
	}
}

func TestForkRunningRegistersToken(t *testing.T) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	api := NewSandboxAPI(t.TempDir())
	srv := NewServer(engine, api)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	if _, err := srv.Fork(context.Background(), "py", "parent", nil, nil, nil, nil, "tok-parent", VitalsLabels{}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ForkRunning(context.Background(), "parent", "child", false, "tok-child"); err != nil {
		t.Fatal(err)
	}

	resp, _ := postExec(t, ts.URL, "child", "")
	if resp.StatusCode != 401 {
		t.Fatalf("child without bearer: status = %d, want 401", resp.StatusCode)
	}
	resp, body := postExec(t, ts.URL, "child", "tok-child")
	if resp.StatusCode != 404 || !strings.Contains(body, "not found or not registered") {
		t.Fatalf("child with bearer: status = %d, body = %s, want 404 sandbox-missing", resp.StatusCode, body)
	}
	// The parent's token does not open the child.
	resp, _ = postExec(t, ts.URL, "child", "tok-parent")
	if resp.StatusCode != 401 {
		t.Fatalf("cross-sandbox token: status = %d, want 401", resp.StatusCode)
	}
}

// Guard: the middleware must hand the buffered body through unmodified so
// handlers decode the full request, not just the peeked sandbox field.
func TestAuthMiddlewarePreservesBody(t *testing.T) {
	api, ts := newAuthTestAPI(t)
	api.RegisterToken("sb-auth", "tok-correct")

	payload := map[string]any{"sandbox": "sb-auth", "command": "echo hi", "timeout": 7}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-correct")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("status = %d, body = %s, want 200", resp.StatusCode, buf.String())
	}
}
