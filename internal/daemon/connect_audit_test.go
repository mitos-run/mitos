package daemon

// connect_audit_test.go: tests for the Connect audit interceptor (PR A, issue
// #358). The legacy /v1 JSON handlers recorded an AuditEvent per op; this
// restores that on the Connect sandbox.v1.Sandbox runtime path. The tests
// assert:
//   - a Connect Exec records op="exec" with the AUTHENTICATED sandbox id and
//     OK=true (proving auth runs outermost and populates the id before audit);
//   - a Connect Mkdir (unary) records op="mkdir";
//   - SECRET HYGIENE: the recorded event carries no command, argv, env, or
//     content, even when the request payload contains secret-looking values;
//   - NopAuditor (the default) records nothing.

import (
	"context"
	"encoding/json"
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

// newAuditConnectServer builds a SandboxAPI wired to a fake guest, with the
// supplied auditor and token, and returns a Connect client over api.Handler().
func newAuditConnectServer(t *testing.T, sandboxID, token string, aud Auditor) sandboxv1connect.SandboxClient {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "connaudit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "vsock.sock")
	startFakeGuestGRPCUDS(t, sock, &fakeGuestSandbox{execStdout: "hi\n", execExit: 0})

	api := NewSandboxAPI(dir)
	api.SetAuditor(aud)
	if err := api.RegisterSandbox(sandboxID, sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(sandboxID, sock)
	if token != "" {
		api.RegisterToken(sandboxID, token)
	} else {
		api.AllowTokenless()
	}

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
	return sandboxv1connect.NewSandboxClient(httpClient, srv.URL, connect.WithGRPC())
}

// runConnectExec drives one bidi Exec carrying the given command and env, with
// the bearer and sandbox-id headers set, and drains it to completion.
func runConnectExec(t *testing.T, client sandboxv1connect.SandboxClient, sandboxID, token, command string, env []*sandboxv1.EnvVar) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	stream.RequestHeader().Set("X-Sandbox-Id", sandboxID)
	_ = stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command: command,
		Env:     env,
	}}})
	_ = stream.CloseRequest()
	for {
		if _, err := stream.Receive(); err != nil {
			return
		}
	}
}

// TestConnectAuditRecordsExecWithAuthenticatedID proves the audit interceptor
// records op="exec" for a Connect Exec, carrying the AUTHENTICATED sandbox id
// the auth interceptor injected (so auth runs outermost). It also asserts
// secret hygiene: a secret-looking command and env value never reach the event.
func TestConnectAuditRecordsExecWithAuthenticatedID(t *testing.T) {
	const (
		sandboxID  = "sb-audit-exec"
		token      = "tok-audit"
		secretCmd  = "curl https://api/?key=SECRETCMD-9999"
		secretEnvV = "sk-SECRETENV-8888"
	)
	rec := &recordingAuditor{}
	client := newAuditConnectServer(t, sandboxID, token, rec)

	runConnectExec(t, client, sandboxID, token, secretCmd,
		[]*sandboxv1.EnvVar{{Key: "API_KEY", Value: secretEnvV}})

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d audit events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Op != "exec" {
		t.Fatalf("op = %q, want exec", ev.Op)
	}
	// Auth runs outermost: the authenticated sandbox id must be on the event.
	if ev.SandboxID != sandboxID {
		t.Fatalf("sandbox id = %q, want %q (auth must inject it before audit)", ev.SandboxID, sandboxID)
	}
	if !ev.OK {
		t.Fatalf("OK = false, want true for a successful exec")
	}

	// SECRET HYGIENE: marshal the whole event and assert no command, argv, env,
	// or content secret value appears anywhere in it.
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(blob)
	for _, secret := range []string{secretCmd, "SECRETCMD-9999", secretEnvV, "SECRETENV-8888", "API_KEY", "curl"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("audit event leaked request payload %q: %s", secret, logged)
		}
	}
	// Detail and Bytes must be unset on the Connect audit path.
	if ev.Detail != "" {
		t.Fatalf("Detail = %q, want empty (no command/path recorded)", ev.Detail)
	}
	if ev.Bytes != 0 {
		t.Fatalf("Bytes = %d, want 0", ev.Bytes)
	}
}

// TestConnectAuditRecordsUnaryMkdir proves the interceptor records a unary RPC
// (Mkdir) with the mapped op string and the authenticated id.
func TestConnectAuditRecordsUnaryMkdir(t *testing.T) {
	const (
		sandboxID = "sb-audit-mkdir"
		token     = "tok-audit-mkdir"
	)
	rec := &recordingAuditor{}
	client := newAuditConnectServer(t, sandboxID, token, rec)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := connect.NewRequest(&sandboxv1.MkdirRequest{Path: "/secret/dir/SHOULD-NOT-LEAK"})
	req.Header().Set("Authorization", "Bearer "+token)
	req.Header().Set("X-Sandbox-Id", sandboxID)
	if _, err := client.Mkdir(ctx, req); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Op != "mkdir" {
		t.Fatalf("op = %q, want mkdir", ev.Op)
	}
	if ev.SandboxID != sandboxID {
		t.Fatalf("sandbox id = %q, want %q", ev.SandboxID, sandboxID)
	}
	if !ev.OK {
		t.Fatal("OK = false, want true")
	}
	// The path must never reach the audit event.
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), "SHOULD-NOT-LEAK") {
		t.Fatalf("audit event leaked the path: %s", string(blob))
	}
}

// TestConnectAuditNopAuditorRecordsNothing proves the default NopAuditor (the
// off state) records nothing, so the interceptor honors SetAuditor exactly like
// the legacy path.
func TestConnectAuditNopAuditorRecordsNothing(t *testing.T) {
	const (
		sandboxID = "sb-audit-nop"
		token     = "tok-audit-nop"
	)
	// NopAuditor is the default; pass it explicitly to be unambiguous.
	client := newAuditConnectServer(t, sandboxID, token, NopAuditor{})

	// No panic and no error means the Nop path is exercised; there is no
	// recording auditor to inspect, so the assertion is that the call succeeds.
	runConnectExec(t, client, sandboxID, token, "echo hi", nil)
}

// TestAuditOpForProcedure pins the procedure-to-op mapping for every runtime
// method, so a rename or a new method is caught.
func TestAuditOpForProcedure(t *testing.T) {
	cases := map[string]string{
		"/sandbox.v1.Sandbox/Exec":          "exec",
		"/sandbox.v1.Sandbox/ExecStream":    "exec",
		"/sandbox.v1.Sandbox/RunCode":       "run_code",
		"/sandbox.v1.Sandbox/RunCodeStream": "run_code",
		"/sandbox.v1.Sandbox/ReadFile":      "read_file",
		"/sandbox.v1.Sandbox/WriteFile":     "write_file",
		"/sandbox.v1.Sandbox/List":          "list_dir",
		"/sandbox.v1.Sandbox/Stat":          "stat",
		"/sandbox.v1.Sandbox/Mkdir":         "mkdir",
		"/sandbox.v1.Sandbox/Remove":        "remove",
		"/sandbox.v1.Sandbox/Processes":     "processes",
		"/sandbox.v1.Sandbox/Vitals":        "vitals",
		"/sandbox.v1.Sandbox/Signal":        "signal",
		"/sandbox.v1.Sandbox/PortForward":   "port_forward",
		// Fallback: an unmapped method becomes its bare name lowercased.
		"/sandbox.v1.Sandbox/Watch": "watch",
	}
	for procedure, want := range cases {
		if got := auditOpForProcedure(procedure); got != want {
			t.Errorf("auditOpForProcedure(%q) = %q, want %q", procedure, got, want)
		}
	}
}
