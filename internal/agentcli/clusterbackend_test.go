package agentcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	v1 "mitos.run/mitos/api/v1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// connectExecFake is a Connect SandboxHandler stub serving ExecStream for the
// cluster-backend exec tests, which now ride the Connect runtime path (the
// legacy /v1/exec JSON route is retired, issue #358). It records the bearer and
// the sandbox id from the request headers and returns a canned exec result, or
// a Connect error (optionally echoing the token, to prove redaction).
type connectExecFake struct {
	sandboxv1connect.UnimplementedSandboxHandler

	stdout string
	stderr string
	exit   int32
	// errMsg, when non-empty, makes ExecStream return a CodeInternal error whose
	// message is errMsg with %TOKEN% replaced by the presented bearer token.
	errMsg string

	gotAuth    string
	gotSandbox string
	gotCommand string
}

func (f *connectExecFake) ExecStream(_ context.Context, req *connect.Request[sandboxv1.ExecStreamRequest], stream *connect.ServerStream[sandboxv1.ExecResponse]) error {
	f.gotAuth = req.Header().Get("Authorization")
	f.gotSandbox = req.Header().Get("X-Sandbox-Id")
	f.gotCommand = req.Msg.GetCommand()
	if f.errMsg != "" {
		presented := strings.TrimPrefix(f.gotAuth, "Bearer ")
		return connect.NewError(connect.CodeInternal, execErrString(strings.ReplaceAll(f.errMsg, "%TOKEN%", presented)))
	}
	if f.stdout != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte(f.stdout)}}); err != nil {
			return err
		}
	}
	if f.stderr != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: []byte(f.stderr)}}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: f.exit}}})
}

type execErrString string

func (e execErrString) Error() string { return string(e) }

// connectExecServer mounts the fake Connect Sandbox handler on an HTTP/1.1
// httptest server.
func connectExecServer(t *testing.T, fake *connectExecFake) *httptest.Server {
	t.Helper()
	path, handler := sandboxv1connect.NewSandboxHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(v1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func TestClusterBackendCreatePollsReady(t *testing.T) {
	// Pre-seed a Ready sandbox so the poll returns immediately. The backend names
	// the sandbox deterministically only for new objects; here we drive Create and
	// then assert it created a sandbox and returned its name. To exercise the
	// Ready poll we use a fake client whose Create stores the object and a status
	// that the backend can read back as Ready.
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()

	be := &ClusterBackend{
		client:    c,
		namespace: "default",
		now:       time.Now,
		// A short poll so the test stays fast; the backend flips the sandbox to
		// Ready via the readyHook injected for the test.
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
	// readyHook simulates the controller reconciling the sandbox to Ready: as
	// soon as the backend created the sandbox, mark it Ready with an endpoint.
	be.readyHook = func(ctx context.Context, name string) {
		var sandbox v1.Sandbox
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &sandbox); err != nil {
			return
		}
		sandbox.Status.Phase = v1.SandboxReady
		sandbox.Status.Endpoint = "10.0.0.5:9091"
		_ = c.Status().Update(ctx, &sandbox)
	}

	id, err := be.Create(context.Background(), "python-pool")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatalf("Create returned an empty id")
	}

	var sandbox v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: id}, &sandbox); err != nil {
		t.Fatalf("created sandbox not found: %v", err)
	}
	if sandbox.Spec.Source.PoolRef == nil || sandbox.Spec.Source.PoolRef.Name != "python-pool" {
		t.Fatalf("sandbox poolRef = %v, want python-pool", sandbox.Spec.Source.PoolRef)
	}
}

func TestClusterBackendList(t *testing.T) {
	scheme := testScheme(t)
	created := metav1.NewTime(time.Now().Add(-90 * time.Second))
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default", CreationTimestamp: created},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}},
		},
		Status: v1.SandboxStatus{
			Phase: v1.SandboxReady, Node: "node-a", Endpoint: "10.0.0.1:9091",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	be := &ClusterBackend{client: c, namespace: "default", now: time.Now}
	infos, err := be.List(context.Background(), "default")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List len = %d, want 1", len(infos))
	}
	got := infos[0]
	if got.Name != "sbx-1" || got.Pool != "python" || got.Phase != "Ready" || got.Node != "node-a" || got.Endpoint != "10.0.0.1:9091" {
		t.Fatalf("List info = %+v, want mapped fields", got)
	}
	if got.Age < 80*time.Second || got.Age > 200*time.Second {
		t.Fatalf("List age = %v, want ~90s", got.Age)
	}
}

func TestClusterBackendTerminate(t *testing.T) {
	scheme := testScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "p"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now}

	if err := be.Terminate(context.Background(), "sbx-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	var got v1.Sandbox
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sbx-1"}, &got)
	if err == nil {
		t.Fatalf("sandbox still exists after Terminate")
	}
}

func TestClusterBackendForkCreatesFork(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	be := &ClusterBackend{
		client: c, namespace: "default", now: time.Now,
		pollInterval: time.Millisecond, pollTimeout: 2 * time.Second,
	}
	be.forkReadyHook = func(ctx context.Context, name string, n int) {
		var sandbox v1.Sandbox
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &sandbox); err != nil {
			return
		}
		children := make([]v1.SandboxChild, 0, n)
		for i := 0; i < n; i++ {
			children = append(children, v1.SandboxChild{
				Name:  name + "-" + string(rune('a'+i)),
				Phase: v1.SandboxReady,
			})
		}
		sandbox.Status.ReadyReplicas = int32(n)
		sandbox.Status.Children = children
		_ = c.Status().Update(ctx, &sandbox)
	}

	ids, err := be.Fork(context.Background(), "sbx-1", 2)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Fork ids = %v, want 2", ids)
	}
	var sandboxList v1.SandboxList
	if err := c.List(context.Background(), &sandboxList); err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	// One Sandbox object should have been created (the fork parent).
	if len(sandboxList.Items) != 1 {
		t.Fatalf("want 1 Sandbox created, got %d", len(sandboxList.Items))
	}
	forkSandbox := &sandboxList.Items[0]
	if forkSandbox.Spec.Source.FromSandbox == nil || forkSandbox.Spec.Source.FromSandbox.Name != "sbx-1" {
		t.Fatalf("fork source = %v, want sbx-1", forkSandbox.Spec.Source.FromSandbox)
	}
}

func TestClusterBackendExecSendsBearerAndRedactsToken(t *testing.T) {
	const token = "super-secret-token-value"
	// The Connect runtime server echoes the presented bearer token into its error
	// message; the backend must redact it before surfacing the error.
	connFake := &connectExecFake{errMsg: "boom token=%TOKEN%"}
	srv := connectExecServer(t, connFake)

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	scheme := testScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, Endpoint: endpoint},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, secret).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now, httpClient: srv.Client()}

	_, err := be.Exec(context.Background(), "sbx-1", "echo hi", 10)
	if err == nil {
		t.Fatalf("Exec: want an error from the failed exec RPC")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("error should show the token redacted, got: %q", err.Error())
	}
	// The exec rode the Connect runtime path with the bearer token and the
	// sandbox id on the headers (the SAME gate the SDK uses).
	if connFake.gotAuth != "Bearer "+token {
		t.Fatalf("Authorization header = %q, want bearer token", connFake.gotAuth)
	}
	if connFake.gotCommand != "echo hi" || connFake.gotSandbox != "sbx-1" {
		t.Fatalf("exec command = %q, sandbox = %q, want 'echo hi' / sbx-1", connFake.gotCommand, connFake.gotSandbox)
	}
}

func TestClusterBackendExecSuccess(t *testing.T) {
	const token = "tkn"
	connFake := &connectExecFake{stdout: "out", stderr: "err", exit: 5}
	srv := connectExecServer(t, connFake)
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	scheme := testScheme(t)
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, Endpoint: endpoint},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, secret).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now, httpClient: srv.Client()}

	res, err := be.Exec(context.Background(), "sbx-1", "ls", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 5 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("Exec result = %+v, want {5 out err}", res)
	}
}
