package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"net/http"

	connect "connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	v1 "mitos.run/mitos/api/v1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func execScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(v1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

// fakeSandboxSvc is a Connect sandbox.v1.Sandbox handler used to exercise the
// kubectl-mitos exec and vitals clients. It implements only the methods those
// commands call (ExecStream, Vitals, Processes); everything else stays
// unimplemented.
type fakeSandboxSvc struct {
	sandboxv1connect.UnimplementedSandboxHandler

	// execChunks are emitted as ExecResponse stdout/stderr frames in order,
	// followed by a terminal exit frame carrying execExit.
	execChunks []*sandboxv1.ExecResponse
	execExit   *sandboxv1.ExecExit
	// execErr, when set, is returned instead of streaming (e.g. an Unauthenticated
	// to model a rejected token).
	execErr error

	// gotCommand records the command the handler received.
	gotCommand string
	// gotAuth and gotSandboxID record the Authorization and X-Sandbox-Id headers
	// seen by the ExecStream and Vitals handlers.
	gotAuth      string
	gotSandboxID string

	// vitalsSamples are emitted as GuestVitals frames in order.
	vitalsSamples []*sandboxv1.GuestVitals
	vitalsErr     error

	// procList is returned by the Processes RPC; procErr, when set, is returned
	// instead. procAuth and procSandboxID record the headers the Processes handler
	// saw (kept distinct from the Vitals headers so a test can assert BOTH calls
	// authenticated).
	procList      *sandboxv1.ProcessList
	procErr       error
	procAuth      string
	procSandboxID string
}

func (f *fakeSandboxSvc) ExecStream(ctx context.Context, req *connect.Request[sandboxv1.ExecStreamRequest], stream *connect.ServerStream[sandboxv1.ExecResponse]) error {
	f.gotAuth = req.Header().Get("Authorization")
	f.gotSandboxID = req.Header().Get("X-Sandbox-Id")
	f.gotCommand = req.Msg.GetCommand()
	if f.execErr != nil {
		return f.execErr
	}
	for _, c := range f.execChunks {
		if err := stream.Send(c); err != nil {
			return err
		}
	}
	if f.execExit != nil {
		if err := stream.Send(&sandboxv1.ExecResponse{
			Msg: &sandboxv1.ExecResponse_Exit{Exit: f.execExit},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSandboxSvc) Vitals(ctx context.Context, req *connect.Request[sandboxv1.VitalsRequest], stream *connect.ServerStream[sandboxv1.GuestVitals]) error {
	f.gotAuth = req.Header().Get("Authorization")
	f.gotSandboxID = req.Header().Get("X-Sandbox-Id")
	if f.vitalsErr != nil {
		return f.vitalsErr
	}
	for _, s := range f.vitalsSamples {
		if err := stream.Send(s); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSandboxSvc) Processes(ctx context.Context, req *connect.Request[sandboxv1.ProcessesRequest]) (*connect.Response[sandboxv1.ProcessList], error) {
	f.procAuth = req.Header().Get("Authorization")
	f.procSandboxID = req.Header().Get("X-Sandbox-Id")
	if f.procErr != nil {
		return nil, f.procErr
	}
	pl := f.procList
	if pl == nil {
		pl = &sandboxv1.ProcessList{}
	}
	return connect.NewResponse(pl), nil
}

// newFakeSandboxServer stands up the fake Connect Sandbox service on an
// httptest server and returns the host:port endpoint (no scheme) plus the svc so
// the test can assert what the handler saw. Server-streaming RPCs (ExecStream,
// Vitals) work over HTTP/1.1 chunked transfer, so a plain httptest server is
// sufficient.
func newFakeSandboxServer(t *testing.T, svc *fakeSandboxSvc) string {
	t.Helper()
	path, h := sandboxv1connect.NewSandboxHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestExecSandboxStreamsAndFoldsResult(t *testing.T) {
	const token = "secret-bearer-xyz"
	svc := &fakeSandboxSvc{
		execChunks: []*sandboxv1.ExecResponse{
			{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte("hi")}},
			{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte("\n")}},
			{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: []byte("warn\n")}},
		},
		execExit: &sandboxv1.ExecExit{ExitCode: 3},
	}
	endpoint := newFakeSandboxServer(t, svc)

	res, err := execSandbox(context.Background(), nil, endpoint, token, "sbx-id", "echo hi")
	if err != nil {
		t.Fatalf("execSandbox: %v", err)
	}
	if svc.gotAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want bearer token", svc.gotAuth)
	}
	if svc.gotSandboxID != "sbx-id" {
		t.Errorf("X-Sandbox-Id header = %q, want sbx-id", svc.gotSandboxID)
	}
	if svc.gotCommand != "echo hi" {
		t.Errorf("command = %q, want echo hi", svc.gotCommand)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("stdout = %q, want hi\\n", res.Stdout)
	}
	if res.Stderr != "warn\n" {
		t.Errorf("stderr = %q, want warn\\n", res.Stderr)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestExecSandboxUnauthorizedDoesNotLeakToken(t *testing.T) {
	const token = "leaky-token-123"
	svc := &fakeSandboxSvc{
		// The server rejects with Unauthenticated; the message even echoes the
		// token to prove the client never surfaces it.
		execErr: connect.NewError(connect.CodeUnauthenticated, errInline("rejected token="+token)),
	}
	endpoint := newFakeSandboxServer(t, svc)

	_, err := execSandbox(context.Background(), nil, endpoint, token, "sbx-id", "false")
	if err == nil {
		t.Fatalf("want an error from an unauthenticated server")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should explain the bearer token was rejected, got %q", err.Error())
	}
}

func TestResolveSandboxAuthReadsTokenSecret(t *testing.T) {
	scheme := execScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			Endpoint:  "10.0.0.5:9091",
			SandboxID: "sbx-id-42",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("tkn-99")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, secret).Build()

	ref, endpoint, token, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err != nil {
		t.Fatalf("resolveSandboxAuth: %v", err)
	}
	if ref != "sbx-id-42" {
		t.Errorf("ref = %q, want the sandbox id", ref)
	}
	if endpoint != "10.0.0.5:9091" {
		t.Errorf("endpoint = %q", endpoint)
	}
	if token != "tkn-99" {
		t.Errorf("token = %q, want the secret token", token)
	}
}

func TestResolveSandboxAuthMissingTokenErrorsClearly(t *testing.T) {
	scheme := execScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, Endpoint: "ep:9091"},
	}
	// No token Secret in the cluster.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err == nil {
		t.Fatalf("want an error when the token Secret is missing")
	}
	if !strings.Contains(err.Error(), "sbx-sandbox-token") {
		t.Errorf("error should name the missing token Secret, got %q", err.Error())
	}
}

func TestResolveSandboxAuthNotReadyErrorsNotHang(t *testing.T) {
	scheme := execScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxPending},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err == nil {
		t.Fatalf("a not-Ready sandbox must error, not hang")
	}
	if !strings.Contains(err.Error(), "Ready") {
		t.Errorf("error should explain the sandbox is not Ready, got %q", err.Error())
	}
}

func TestResolveSandboxAuthNotFound(t *testing.T) {
	scheme := execScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "ghost")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing sandbox should error with not found, got %v", err)
	}
}

// errInline is a tiny error type for canned server-side error messages in tests.
type errInline string

func (e errInline) Error() string { return string(e) }
