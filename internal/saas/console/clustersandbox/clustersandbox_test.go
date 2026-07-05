package clustersandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("add v1 scheme: %v", err)
	}
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

// sb builds a v1.Sandbox owned by org, in that org's hard-isolation
// namespace and carrying the org label.
func sb(org, name, phase string) *v1.Sandbox {
	return &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tenant.NamespaceForOrg(org),
			Labels:    tenant.OrgLabels(org),
		},
		Spec:   v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}},
		Status: v1.SandboxStatus{Phase: v1.SandboxPhase(phase), SandboxID: "engine-" + name},
	}
}

func newControl(t *testing.T, objs ...client.Object) *Control {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return New(c, nil)
}

// newControlWithPods builds a Control wired with a PodLogStreamer, for the
// log-streaming tests below.
func newControlWithPods(t *testing.T, pods PodLogStreamer, objs ...client.Object) *Control {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return New(c, pods)
}

// TestListScopedToOrgNamespace asserts List returns only the caller org's
// sandboxes — bob's, in bob's namespace, are never seen by alice.
func TestListScopedToOrgNamespace(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"), sb("alice", "sb-a2", "Pending"), sb("bob", "sb-b1", "Ready"))
	got, err := c.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("alice saw %d sandboxes, want 2", len(got))
	}
	for _, v := range got {
		if v.OrgID != "alice" {
			t.Fatalf("cross-org sandbox in alice list: %+v", v)
		}
	}
}

// TestGetMapsViewFields asserts Get returns the mapped view for an owned sandbox.
func TestGetMapsViewFields(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"))
	v, err := c.Get(context.Background(), "alice", "sb-a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.ID != "sb-a1" || v.OrgID != "alice" || v.Template != "python" || string(v.Phase) != "Ready" {
		t.Fatalf("view = %+v, want id/org/template/phase mapped", v)
	}
}

// TestGetIgnoresOutOfRangeSizeAnnotations asserts that a requested-size
// annotation holding a value outside the int32 range (out-of-band edit or
// corruption; Create itself never writes one, since it only accepts the
// bounded allowedVCPUs/allowedMemGiB sets) is ignored like any other parse
// failure, leaving VCPUs/MemBytes at zero, rather than silently truncating or
// wrapping into a bogus (possibly negative) value.
func TestGetIgnoresOutOfRangeSizeAnnotations(t *testing.T) {
	s := sb("alice", "sb-a1", "Ready")
	s.Annotations = map[string]string{
		requestedVCPUsAnnotation:  "99999999999",
		requestedMemGiBAnnotation: "99999999999",
	}
	c := newControl(t, s)
	v, err := c.Get(context.Background(), "alice", "sb-a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.VCPUs != 0 {
		t.Errorf("VCPUs = %d, want 0 (out-of-range annotation ignored)", v.VCPUs)
	}
	if v.MemBytes != 0 {
		t.Errorf("MemBytes = %d, want 0 (out-of-range annotation ignored)", v.MemBytes)
	}
}

// TestGetIgnoresNegativeSizeAnnotations asserts that a requested-size
// annotation holding a negative value (a hand-edited or corrupted "-1", well
// within the int32 range so it parses cleanly) is ignored exactly like an
// out-of-range or non-numeric one, leaving VCPUs/MemBytes at zero rather
// than reporting a negative size.
func TestGetIgnoresNegativeSizeAnnotations(t *testing.T) {
	s := sb("alice", "sb-a2", "Ready")
	s.Annotations = map[string]string{
		requestedVCPUsAnnotation:  "-1",
		requestedMemGiBAnnotation: "-1",
	}
	c := newControl(t, s)
	v, err := c.Get(context.Background(), "alice", "sb-a2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.VCPUs != 0 {
		t.Errorf("VCPUs = %d, want 0 (negative annotation ignored)", v.VCPUs)
	}
	if v.MemBytes != 0 {
		t.Errorf("MemBytes = %d, want 0 (negative annotation ignored)", v.MemBytes)
	}
}

// TestGetCrossOrgIsNotFound asserts a sandbox owned by another org is reported
// as not-found (the namespace boundary plus the label check), indistinguishable
// from a missing one.
func TestGetCrossOrgIsNotFound(t *testing.T) {
	c := newControl(t, sb("bob", "sb-b1", "Ready"))
	if _, err := c.Get(context.Background(), "alice", "sb-b1"); err != console.ErrNotFound {
		t.Fatalf("cross-org Get err = %v, want console.ErrNotFound", err)
	}
}

// TestTerminateCrossOrgIsNotFoundAndSurvives asserts alice cannot terminate
// bob's sandbox, and it survives.
func TestTerminateCrossOrgIsNotFoundAndSurvives(t *testing.T) {
	c := newControl(t, sb("bob", "sb-b1", "Ready"))
	if err := c.Terminate(context.Background(), "alice", "sb-b1"); err != console.ErrNotFound {
		t.Fatalf("cross-org Terminate err = %v, want console.ErrNotFound", err)
	}
	if _, err := c.Get(context.Background(), "bob", "sb-b1"); err != nil {
		t.Fatalf("bob's sandbox was terminated cross-org: %v", err)
	}
}

// TestTerminateOwnedDeletes asserts terminating an owned sandbox removes it.
func TestTerminateOwnedDeletes(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"))
	if err := c.Terminate(context.Background(), "alice", "sb-a1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := c.Get(context.Background(), "alice", "sb-a1"); err != console.ErrNotFound {
		t.Fatalf("sandbox not deleted: %v", err)
	}
}

// TestImplementsSandboxControl is a compile-time seam assertion.
func TestImplementsSandboxControl(t *testing.T) {
	var _ console.SandboxControl = (*Control)(nil)
}

// TestImplementsLogStreamer is a compile-time seam assertion: the cluster
// Control satisfies console.LogStreamer directly (StreamLogs, see logs.go),
// the same org-scoping pattern as Get/Terminate/Exec.
func TestImplementsLogStreamer(t *testing.T) {
	var _ console.LogStreamer = (*Control)(nil)
}

// TestCreateWritesOrgScopedSandboxWithPoolRef asserts Create writes a Sandbox
// in the org's namespace, labelled for the org, sourced from the requested
// template, and that the returned view carries the requested vcpu/mem as
// (informational, non-authoritative) annotations.
func TestCreateWritesOrgScopedSandboxWithPoolRef(t *testing.T) {
	c := newControl(t)
	v, err := c.Create(context.Background(), "alice", console.CreateSandboxRequest{Template: "python", VCPUs: 2, MemGiB: 4})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v.OrgID != "alice" || v.Template != "python" || v.VCPUs != 2 || v.MemBytes != int64(4)<<30 {
		t.Fatalf("created view = %+v, want org/template/sizing to match the request", v)
	}
	var sb v1.Sandbox
	if err := c.c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg("alice"), Name: v.ID}, &sb); err != nil {
		t.Fatalf("get created sandbox: %v", err)
	}
	if sb.Labels[tenant.OrgLabelKey] != "alice" {
		t.Fatalf("created sandbox missing org label: %+v", sb.Labels)
	}
	if sb.Spec.Source.PoolRef == nil || sb.Spec.Source.PoolRef.Name != "python" {
		t.Fatalf("created sandbox source = %+v, want poolRef python", sb.Spec.Source)
	}
	// The sandbox must be immediately visible through Get/List, not just
	// returned once.
	if got, err := c.Get(context.Background(), "alice", v.ID); err != nil || got.ID != v.ID {
		t.Fatalf("Get(alice, %s) = %+v, %v; want the created sandbox", v.ID, got, err)
	}
}

// TestCreateRejectsEmptyTemplate asserts Create refuses a request with no
// template rather than writing a Sandbox with a nil source.
func TestCreateRejectsEmptyTemplate(t *testing.T) {
	c := newControl(t)
	if _, err := c.Create(context.Background(), "alice", console.CreateSandboxRequest{}); err == nil {
		t.Fatal("Create with no template: want an error")
	}
}

// TestForkRefusesCrossOrgSource asserts Fork will not fork a sandbox owned by
// a different org, and creates nothing.
func TestForkRefusesCrossOrgSource(t *testing.T) {
	c := newControl(t, sb("bob", "sb-b1", "Ready"))
	if _, err := c.Fork(context.Background(), "alice", "sb-b1", 2); err != console.ErrNotFound {
		t.Fatalf("cross-org Fork err = %v, want console.ErrNotFound", err)
	}
	var list v1.SandboxList
	if err := c.c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("Fork created sandboxes despite the cross-org refusal: %d items", len(list.Items))
	}
}

// TestForkCreatesNIndependentAddressableSandboxes asserts Fork creates count
// separate top-level Sandbox objects (not one replicas=N object), each
// sourced from sandboxID, each immediately Get/Terminate-able through the
// SAME SandboxControl (the design choice documented on Control.Fork).
func TestForkCreatesNIndependentAddressableSandboxes(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"))
	ids, err := c.Fork(context.Background(), "alice", "sb-a1", 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("Fork returned %d ids, want 3", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate fork id %q", id)
		}
		seen[id] = true
		v, err := c.Get(context.Background(), "alice", id)
		if err != nil {
			t.Fatalf("Get(alice, %s): %v", id, err)
		}
		if v.OrgID != "alice" {
			t.Fatalf("fork child %+v not owned by alice", v)
		}
		// Terminate each child independently to prove it is a first-class
		// Sandbox, not just a status entry on the source.
		if err := c.Terminate(context.Background(), "alice", id); err != nil {
			t.Fatalf("Terminate(alice, %s): %v", id, err)
		}
	}
	var list v1.SandboxList
	if err := c.c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Only the original source sandbox should remain after terminating the 3
	// forks.
	if len(list.Items) != 1 || list.Items[0].Name != "sb-a1" {
		t.Fatalf("post-terminate sandboxes = %+v, want only sb-a1", list.Items)
	}
}

// execConnectFake is a minimal Connect SandboxHandler stub serving
// ExecStream for the clustersandbox exec test: it records the bearer/sandbox
// id it was called with and returns a canned result.
type execConnectFake struct {
	sandboxv1connect.UnimplementedSandboxHandler
	stdout, stderr string
	exit           int32
	gotAuth        string
	gotSandbox     string
	gotCommand     string
}

func (f *execConnectFake) ExecStream(_ context.Context, req *connect.Request[sandboxv1.ExecStreamRequest], stream *connect.ServerStream[sandboxv1.ExecResponse]) error {
	f.gotAuth = req.Header().Get("Authorization")
	f.gotSandbox = req.Header().Get("X-Sandbox-Id")
	f.gotCommand = req.Msg.GetCommand()
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

func execConnectServer(t *testing.T, fake *execConnectFake) *httptest.Server {
	t.Helper()
	path, handler := sandboxv1connect.NewSandboxHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestExecCrossOrgIsNotFoundAndNeverReachesTransport asserts Exec refuses a
// cross-org sandbox id BEFORE reading its token Secret or reaching the
// sandbox's HTTP endpoint (the same authorize-before-transport guarantee
// AuthorizingLogStreamer proves for log streaming).
func TestExecCrossOrgIsNotFoundAndNeverReachesTransport(t *testing.T) {
	connFake := &execConnectFake{stdout: "should not be reached"}
	srv := execConnectServer(t, connFake)
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	bobSandbox := sb("bob", "sb-b1", "Ready")
	bobSandbox.Status.Endpoint = endpoint
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-b1-sandbox-token", Namespace: tenant.NamespaceForOrg("bob")},
		Data:       map[string][]byte{"token": []byte("tkn")},
	}
	c := newControl(t, bobSandbox, secret)
	if _, err := c.Exec(context.Background(), "alice", "sb-b1", "echo hi", 0); err != console.ErrNotFound {
		t.Fatalf("cross-org Exec err = %v, want console.ErrNotFound", err)
	}
	if connFake.gotCommand != "" {
		t.Fatal("Exec reached the sandbox transport for a cross-org id; authorization bypassed")
	}
}

// TestExecOwnedSandboxRunsCommandOverItsEndpoint asserts Exec on an owned
// sandbox reaches its HTTP endpoint with the bearer token from its token
// Secret and returns the command's result.
func TestExecOwnedSandboxRunsCommandOverItsEndpoint(t *testing.T) {
	connFake := &execConnectFake{stdout: "out", stderr: "err", exit: 7}
	srv := execConnectServer(t, connFake)
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	aliceSandbox := sb("alice", "sb-a1", "Ready")
	aliceSandbox.Status.Endpoint = endpoint
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-a1-sandbox-token", Namespace: tenant.NamespaceForOrg("alice")},
		Data:       map[string][]byte{"token": []byte("tkn-alice")},
	}
	c := newControl(t, aliceSandbox, secret)
	c.httpClient = srv.Client()

	res, err := c.Exec(context.Background(), "alice", "sb-a1", "echo hi", 5)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("Exec result = %+v, want {7 out err}", res)
	}
	if connFake.gotAuth != "Bearer tkn-alice" {
		t.Fatalf("Authorization header = %q, want the sandbox's own bearer token", connFake.gotAuth)
	}
	if connFake.gotCommand != "echo hi" {
		t.Fatalf("exec command = %q, want 'echo hi'", connFake.gotCommand)
	}
}
