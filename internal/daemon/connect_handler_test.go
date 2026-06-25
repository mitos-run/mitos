package daemon

// connect_handler_test.go: integration tests for Task 3.2 (issue #24).
// Verifies that the Connect Sandbox service is mounted on the same HTTP mux as
// the JSON /v1/* routes, behind the BearerInterceptor, and that:
// - A Connect Exec call with the correct bearer token streams the fake guest's
//   stdout chunks and exit code end-to-end (auth + bridge).
// - A Connect Exec call WITHOUT a token (when a token is registered) returns
//   CodeUnauthenticated.
// - The legacy JSON /v1/exec/stream route is still registered and reachable.
//
// The test uses the same startFakeStreamUDS helper as exec_stream_test.go:
// a unix socket that speaks the Firecracker vsock UDS preamble and emits
// scripted ExecStreamFrame values. No KVM or real guest is required.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// newConnectTestServer builds a SandboxAPI wired to a fake guest gRPC server
// on AgentGRPCPort (53). All callers speak gRPC since the legacy JSON protocol
// was removed (#310). It registers the sandbox and (optionally) a token, and
// starts an HTTP/2 test server over api.Handler(). It returns the Connect
// client, the raw HTTP URL, and a cleanup func.
//
// The test server speaks both HTTP/1.1 (for the JSON smoke test) and
// unencrypted HTTP/2 (for Connect bidi streaming).
func newConnectTestServer(t *testing.T, sandboxID, token string) (sandboxv1connect.SandboxClient, string, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "conn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "vsock.sock")
	startFakeGuestGRPCUDS(t, sock,
		&fakeGuestSandbox{execStdout: "hello connect\n", execExit: 42},
	)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox(sandboxID, sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(sandboxID, sock)
	if token != "" {
		api.RegisterToken(sandboxID, token)
	} else {
		api.AllowTokenless()
	}

	handler := api.Handler()

	srv := httptest.NewUnstartedServer(handler)
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()

	var cp http.Protocols
	cp.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{Transport: &http.Transport{Protocols: &cp}}

	client := sandboxv1connect.NewSandboxClient(httpClient, srv.URL,
		connect.WithGRPC(),
	)
	return client, srv.URL, srv.Close
}

// drainConnect drives one Exec call to the Connect client, waits for all
// ExecResponse frames, and returns the collected stdout and final exit code.
// The sandboxID and optional bearer token are sent as request headers.
func drainConnect(t *testing.T, client sandboxv1connect.SandboxClient, sandboxID, bearer string) (stdout string, exit int32, connErr error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream := client.Exec(ctx)
	if bearer != "" {
		stream.RequestHeader().Set("Authorization", "Bearer "+bearer)
	}
	stream.RequestHeader().Set("X-Sandbox-Id", sandboxID)

	// Per the Connect contract, when the server returns an error (for example a
	// CodeUnauthenticated rejection from the BearerInterceptor, which fires
	// before any request body is read), Send and CloseRequest return an error
	// that wraps io.EOF. The authoritative status lives on the stream and must
	// be retrieved with Receive. Returning the Send/CloseRequest error here
	// instead would surface a transport-level "unknown" code that races the auth
	// rejection, so we swallow the io.EOF sentinel and fall through to Receive,
	// which deterministically yields the server's real status code.
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Command: "echo hello connect",
		}},
	}); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	if err := stream.CloseRequest(); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}

	var sb strings.Builder
	for {
		resp, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, err
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			sb.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Exit:
			exit = m.Exit.GetExitCode()
		}
	}
	return sb.String(), exit, nil
}

// TestConnectExecWithTokenStreamsOutput is the Task 3.2 acceptance test:
// a Connect Exec call to the mounted handler WITH the correct bearer token
// streams the fake guest's stdout chunks and exit code through.
func TestConnectExecWithTokenStreamsOutput(t *testing.T) {
	const (
		sandboxID = "c-sb1"
		token     = "tok-connect-ok"
	)
	client, _, cleanup := newConnectTestServer(t, sandboxID, token)
	defer cleanup()

	stdout, exit, err := drainConnect(t, client, sandboxID, token)
	if err != nil {
		t.Fatalf("Connect Exec failed: %v", err)
	}
	if stdout != "hello connect\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "hello connect\n")
	}
	if exit != 42 {
		t.Fatalf("exit code = %d, want 42", exit)
	}
}

// TestConnectExecWithoutTokenIsUnauthenticated verifies that a Connect Exec
// call to the mounted handler WITHOUT a bearer (when a token IS registered)
// returns CodeUnauthenticated. This is the auth gate test.
func TestConnectExecWithoutTokenIsUnauthenticated(t *testing.T) {
	const (
		sandboxID = "c-sb2"
		token     = "tok-connect-secret"
	)
	client, _, cleanup := newConnectTestServer(t, sandboxID, token)
	defer cleanup()

	_, _, err := drainConnect(t, client, sandboxID, "") // no bearer
	if err == nil {
		t.Fatal("expected unauthenticated error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
	// The error message must NOT contain any token values. The token value
	// is "tok-connect-secret"; it must never appear in a returned error.
	if strings.Contains(err.Error(), "tok-connect-secret") {
		t.Fatalf("error message reflected the registered token value: %v", err)
	}
}

// TestConnectExecWrongTokenIsUnauthenticated verifies that a wrong bearer token
// returns CodeUnauthenticated and never echoes the presented value.
func TestConnectExecWrongTokenIsUnauthenticated(t *testing.T) {
	const (
		sandboxID = "c-sb3"
		token     = "tok-connect-registered"
		wrong     = "tok-connect-wrong"
	)
	client, _, cleanup := newConnectTestServer(t, sandboxID, token)
	defer cleanup()

	_, _, err := drainConnect(t, client, sandboxID, wrong)
	if err == nil {
		t.Fatal("expected unauthenticated error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
	// The presented wrong token must not be reflected in the error.
	if strings.Contains(err.Error(), wrong) {
		t.Fatalf("error message reflected the presented wrong token: %v", err)
	}
}

// TestLegacyJSONRuntimeRoutesRemoved asserts the legacy JSON /v1 runtime routes
// (exec, exec/stream, run_code/stream, files, vitals, pty) no longer exist on the
// mux: the runtime surface is served only by the Connect sandbox.v1.Sandbox
// protocol now (#358). A POST to any of them must NOT be handled (404 from the
// catch-all under requireBearer), never a 200 NDJSON/JSON runtime response.
func TestLegacyJSONRuntimeRoutesRemoved(t *testing.T) {
	const sandboxID = "c-sb4"
	_, rawURL, cleanup := newConnectTestServer(t, sandboxID, "")
	defer cleanup()

	for _, route := range []string{
		"/v1/exec",
		"/v1/exec/stream",
		"/v1/run_code/stream",
		"/v1/files/read",
		"/v1/files/write",
		"/v1/vitals",
	} {
		body, _ := json.Marshal(map[string]any{"sandbox": sandboxID, "command": "echo hello"})
		resp, err := http.Post(rawURL+route, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", route, err)
		}
		gotCT := resp.Header.Get("Content-Type")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (route removed)", route, resp.StatusCode)
		}
		if gotCT == "application/x-ndjson" {
			t.Errorf("%s: still served a runtime NDJSON response; the route was not removed", route)
		}
	}
}

// TestConnectExecCarriesExecTimeMs is the regression guard for the gRPC
// migration silently zeroing exec_time_ms: it drives a Connect Exec whose guest
// reports a non-zero exec_time_ms and asserts the value rides through to the
// terminal ExecExit frame. This preserves the coverage the legacy
// /v1/exec exec_time_ms test once provided on the JSON wire.
func TestConnectExecCarriesExecTimeMs(t *testing.T) {
	const (
		sandboxID = "c-ms"
		wantMs    = 42.5
	)
	dir, err := os.MkdirTemp("/tmp", "connms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "vsock.sock")
	startFakeGuestGRPCUDS(t, sock, &fakeGuestSandbox{execStdout: "hi\n", execExit: 0, execTimeMs: wantMs})

	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox(sandboxID, sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(sandboxID, sock)

	srv := httptest.NewUnstartedServer(api.Handler())
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)

	var cp http.Protocols
	cp.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{Transport: &http.Transport{Protocols: &cp}}
	client := sandboxv1connect.NewSandboxClient(httpClient, srv.URL, connect.WithGRPC())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	stream.RequestHeader().Set("X-Sandbox-Id", sandboxID)
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "true"}},
	}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("close request: %v", err)
	}
	var gotMs float64
	for {
		resp, rerr := stream.Receive()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("receive: %v", rerr)
		}
		if ex := resp.GetExit(); ex != nil {
			gotMs = ex.GetExecTimeMs()
		}
	}
	if gotMs != wantMs {
		t.Fatalf("exec_time_ms = %v, want %v", gotMs, wantMs)
	}
}
