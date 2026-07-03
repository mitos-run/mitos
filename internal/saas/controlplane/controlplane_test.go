package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

const (
	orgA = "alpha"
	orgB = "bravo"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1: %v", err)
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(objs...).
		Build()
}

// readySandbox builds a Ready sandbox in the org namespace with the org label,
// plus its token Secret.
func readySandbox(org, name, endpoint, token string) (*v1.Sandbox, *corev1.Secret) {
	ns := tenant.NamespaceForOrg(org)
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org),
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "default"}}},
		Status: v1.SandboxStatus{
			Phase:    v1.SandboxReady,
			Endpoint: endpoint,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + tokenSecretSuffix, Namespace: ns},
		Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
	}
	return sb, secret
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode response %q: %v", string(b), err)
	}
	return m
}

// ----- create -----

// TestCreateBuildsSandboxAndReturnsTokenOnReady asserts a create builds a
// Sandbox in mitos-org-<org> with the org label and the right pool, polls to
// Ready, and returns id+endpoint+token (the token comes from the Secret).
func TestCreateBuildsSandboxAndReturnsTokenOnReady(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	// Flip the created sandbox to Ready and seed its token Secret in the
	// background, mimicking the controller.
	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-secret-value")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default","env":{"FOO":"bar"}}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if m["phase"] != "Ready" {
		t.Errorf("phase = %v, want Ready", m["phase"])
	}
	if m["endpoint"] != "10.1.2.3:9091" {
		t.Errorf("endpoint = %v", m["endpoint"])
	}
	if m["token"] != "tok-secret-value" {
		t.Errorf("token = %v, want the Secret token", m["token"])
	}
	name, _ := m["id"].(string)
	if !strings.HasPrefix(name, "sb-") {
		t.Errorf("id = %v, want sb- prefix", m["id"])
	}

	// Verify the object: org namespace, org label, pool, env.
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: name}, &sb); err != nil {
		t.Fatalf("get created sandbox: %v", err)
	}
	if sb.Namespace != "mitos-org-alpha" {
		t.Errorf("namespace = %q, want mitos-org-alpha", sb.Namespace)
	}
	if sb.Labels[tenant.OrgLabelKey] != orgA {
		t.Errorf("org label = %q, want %q", sb.Labels[tenant.OrgLabelKey], orgA)
	}
	if sb.Spec.Source.PoolRef == nil || sb.Spec.Source.PoolRef.Name != "default" {
		t.Errorf("poolRef = %+v, want default", sb.Spec.Source.PoolRef)
	}
	if len(sb.Spec.Env) != 1 || sb.Spec.Env[0].Name != "FOO" || sb.Spec.Env[0].Value != "bar" {
		t.Errorf("env = %+v, want FOO=bar", sb.Spec.Env)
	}
}

// TestCreateUsesDefaultPoolWhenUnset asserts a create with no pool/image uses the
// configured default pool.
func TestCreateUsesDefaultPoolWhenUnset(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second), WithDefaultPool("base"))
	stop := flipToReadyWhenCreated(t, c, orgA, "1.2.3.4:9091", "tok")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	name := decodeBody(t, resp.Body)["id"].(string)
	var sb v1.Sandbox
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: name}, &sb)
	if sb.Spec.Source.PoolRef.Name != "base" {
		t.Errorf("poolRef = %v, want base", sb.Spec.Source.PoolRef)
	}
}

// TestCreateNoPoolNoDefaultRejected asserts a create with neither pool nor a
// default is a 400-style LLM-legible error and creates nothing. The body decoded
// fine (it is valid JSON), so the error must be invalid_input naming the missing
// pool field, NOT invalid_json (which would misleadingly blame the JSON syntax,
// the #28 LLM-legible error rule).
func TestCreateNoPoolNoDefaultRejected(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond))
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{}`)})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "invalid_input") {
		t.Errorf("error code must be invalid_input for a valid-JSON body missing the pool field, got: %s", body)
	}
	if strings.Contains(body, "invalid_json") || strings.Contains(body, "not valid JSON") {
		t.Errorf("error must not blame the JSON syntax when the body decoded fine: %s", body)
	}
	if !strings.Contains(body, "pool") {
		t.Errorf("error cause must name the missing pool field: %s", body)
	}
	var list v1.SandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Errorf("created %d sandboxes for a rejected request", len(list.Items))
	}
}

// TestCreateFailedReturnsRejectionMessage asserts a Failed phase yields an
// LLM-legible error carrying the rejection condition message.
func TestCreateFailedReturnsRejectionMessage(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	stop := flipWhenCreated(t, c, orgA, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxFailed
		sb.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "PoolMissing",
			Message: "pool default was not found", LastTransitionTime: metav1.Now(),
		}}
	})
	defer stop()

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`)})
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "pool default was not found") {
		t.Errorf("error body missing the rejection message: %s", resp.Body)
	}
}

// TestCreateTimeoutReturnsClearError asserts a sandbox that never becomes Ready
// times out with a 504-style error.
func TestCreateTimeoutReturnsClearError(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(40*time.Millisecond))
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`)})
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "did not become ready") {
		t.Errorf("timeout error not actionable: %s", resp.Body)
	}
}

// TestCreateMissingNamespaceClearError asserts a missing org namespace surfaces a
// clear error naming the org and never panics. The fake client does not enforce
// namespace existence, so this drives the helper directly.
func TestCreateMissingNamespaceClearError(t *testing.T) {
	e := namespaceMissingErr("ghost", tenant.NamespaceForOrg("ghost"))
	if e.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", e.Status)
	}
	if !strings.Contains(e.Cause, "mitos-org-ghost") || !strings.Contains(e.Cause, "ghost") {
		t.Errorf("namespace error does not name the org: %q", e.Cause)
	}
}

// ----- status / list / terminate -----

func TestStatusReturnsOwnedSandbox(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-aaaa", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, sb, secret)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-aaaa"})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if m["id"] != "sb-aaaa" || m["phase"] != "Ready" {
		t.Errorf("status payload = %v", m)
	}
	if strings.Contains(string(resp.Body), "tok") {
		t.Errorf("status leaked the token: %s", resp.Body)
	}
}

func TestListReturnsOnlyCallerOrg(t *testing.T) {
	a1, s1 := readySandbox(orgA, "sb-a1", "1.1.1.1:9091", "t1")
	a2, s2 := readySandbox(orgA, "sb-a2", "1.1.1.2:9091", "t2")
	b1, s3 := readySandbox(orgB, "sb-b1", "2.2.2.1:9091", "t3")
	c := newFakeClient(t, a1, a2, b1, s1, s2, s3)
	cp := New(c)

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.list", Path: "/v1/sandboxes"})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	var out struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(out.Sandboxes) != 2 {
		t.Fatalf("list returned %d sandboxes, want 2 (org A only)", len(out.Sandboxes))
	}
	for _, s := range out.Sandboxes {
		if s["id"] == "sb-b1" {
			t.Fatal("list leaked org B's sandbox to org A")
		}
	}
}

func TestTerminateDeletesOwnedSandbox(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-del", "1.1.1.1:9091", "tok")
	c := newFakeClient(t, sb, secret)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.terminate", Path: "/v1/sandboxes/sb-del"})
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", resp.Status, resp.Body)
	}
	var got v1.Sandbox
	err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: "sb-del"}, &got)
	if err == nil {
		t.Error("sandbox still exists after terminate")
	}
}

// ----- CROSS-TENANT isolation (critical) -----

// TestCrossTenantStatusIsNotFound asserts org A cannot read org B's sandbox: the
// id resolves to 404, never leaking that it exists.
func TestCrossTenantStatusIsNotFound(t *testing.T) {
	b1, s3 := readySandbox(orgB, "sb-secret", "2.2.2.2:9091", "t")
	c := newFakeClient(t, b1, s3)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-secret"})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for cross-org status", resp.Status)
	}
}

// TestCrossTenantTerminateIsNotFoundAndDoesNotDelete asserts org A cannot delete
// org B's sandbox: 404 AND the object survives.
func TestCrossTenantTerminateIsNotFoundAndDoesNotDelete(t *testing.T) {
	b1, s3 := readySandbox(orgB, "sb-victim", "2.2.2.2:9091", "t")
	c := newFakeClient(t, b1, s3)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.terminate", Path: "/v1/sandboxes/sb-victim"})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for cross-org terminate", resp.Status)
	}
	var got v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgB), Name: "sb-victim"}, &got); err != nil {
		t.Fatalf("org B's sandbox was deleted by org A's terminate: %v", err)
	}
}

// TestCrossTenantSameNamespaceMislabeledIsNotFound asserts that even an object
// physically in org A's namespace but carrying a different org label is treated
// as not-owned: the org-label re-check, not just the namespace, is the boundary.
func TestCrossTenantSameNamespaceMislabeledIsNotFound(t *testing.T) {
	// An object in mitos-org-alpha but labeled for org B (a defense-in-depth case).
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb-mislabeled",
			Namespace: tenant.NamespaceForOrg(orgA),
			Labels:    tenant.OrgLabels(orgB),
		},
	}
	c := newFakeClient(t, sb)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-mislabeled"})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a mislabeled object", resp.Status)
	}
}

// ----- runtime proxy -----

// TestProxyForwardsWithTokenAndSandboxID asserts a runtime call reaches the
// sandbox endpoint with Authorization: Bearer <token> and X-Sandbox-Id, streams
// the request and response, and strips a client-supplied Authorization.
func TestProxyForwardsWithTokenAndSandboxID(t *testing.T) {
	var gotAuth, gotSandboxID, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSandboxID = r.Header.Get("X-Sandbox-Id")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write([]byte(`{"streamed":true}`))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	sb, secret := readySandbox(orgA, "sb-run", endpoint, "the-real-token")
	c := newFakeClient(t, sb, secret)
	cp := New(c, WithHTTPClient(srv.Client()))

	hdr := http.Header{}
	hdr.Set("X-Sandbox-Id", "sb-run")
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Authorization", "Bearer attacker-supplied")

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID:      orgA,
		Op:         "sandbox.runtime",
		Path:       "/sandbox.v1.Sandbox/Exec",
		Method:     http.MethodPost,
		Header:     hdr,
		BodyStream: strings.NewReader(`{"cmd":["echo","hi"]}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if resp.BodyStream == nil {
		t.Fatal("expected a streamed response body")
	}
	got, _ := io.ReadAll(resp.BodyStream)
	_ = resp.BodyStream.Close()
	if string(got) != `{"streamed":true}` {
		t.Errorf("proxied body = %q", got)
	}
	if gotAuth != "Bearer the-real-token" {
		t.Errorf("upstream Authorization = %q, want the per-sandbox token (client value must be stripped)", gotAuth)
	}
	if gotSandboxID != "sb-run" {
		t.Errorf("upstream X-Sandbox-Id = %q", gotSandboxID)
	}
	if gotBody != `{"cmd":["echo","hi"]}` {
		t.Errorf("upstream body = %q", gotBody)
	}
	if gotPath != "/sandbox.v1.Sandbox/Exec" {
		t.Errorf("upstream path = %q", gotPath)
	}
}

// TestProxyCrossOrgNeverReachesEndpoint asserts a runtime call naming another
// org's sandbox is 404 AND never hits the endpoint.
func TestProxyCrossOrgNeverReachesEndpoint(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	// Sandbox belongs to org B.
	sb, secret := readySandbox(orgB, "sb-bonly", endpoint, "tok")
	c := newFakeClient(t, sb, secret)
	cp := New(c, WithHTTPClient(srv.Client()))

	hdr := http.Header{}
	hdr.Set("X-Sandbox-Id", "sb-bonly")
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/sandbox.v1.Sandbox/Exec", Method: http.MethodPost, Header: hdr,
		BodyStream: strings.NewReader("{}"),
	})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for cross-org proxy", resp.Status)
	}
	if reached {
		t.Fatal("cross-org runtime call REACHED the sandbox endpoint; the boundary failed")
	}
}

// TestProxyUnknownSandboxIsNotFound asserts a runtime call for a sandbox that
// does not exist for the caller org is 404.
func TestProxyUnknownSandboxIsNotFound(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c)
	hdr := http.Header{}
	hdr.Set("X-Sandbox-Id", "sb-nope")
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.runtime", Path: "/sandbox.v1.Sandbox/Exec", Header: hdr})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Status)
	}
}

// ----- pause / resume lifecycle proxy (#601) -----

// TestPauseResumeForwardToSandboxEndpoint asserts sandbox.pause and
// sandbox.resume POST the daemon lifecycle routes on the org-owned sandbox
// endpoint with the per-sandbox bearer token and the body {"sandbox": <name>},
// and pass the upstream 2xx status and body through.
func TestPauseResumeForwardToSandboxEndpoint(t *testing.T) {
	cases := []struct {
		op       string
		route    string
		upstream string
	}{
		{op: "sandbox.pause", route: "/v1/pause", upstream: `{"status":"paused"}`},
		{op: "sandbox.resume", route: "/v1/resume", upstream: `{"status":"running"}`},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			var gotMethod, gotPath, gotAuth, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.upstream))
			}))
			defer srv.Close()
			endpoint := strings.TrimPrefix(srv.URL, "http://")

			sb, secret := readySandbox(orgA, "sb-hold", endpoint, "the-real-token")
			c := newFakeClient(t, sb, secret)
			cp := New(c, WithHTTPClient(srv.Client()))

			resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
				OrgID: orgA, Op: tc.op, Path: tc.route, Body: []byte(`{"sandbox":"sb-hold"}`),
			})
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			if resp.Status != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
			}
			if string(resp.Body) != tc.upstream {
				t.Errorf("body = %q, want the upstream body %q", resp.Body, tc.upstream)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("upstream method = %q, want POST", gotMethod)
			}
			if gotPath != tc.route {
				t.Errorf("upstream path = %q, want %q", gotPath, tc.route)
			}
			if gotAuth != "Bearer the-real-token" {
				t.Errorf("upstream Authorization = %q, want the per-sandbox token", gotAuth)
			}
			if gotBody != `{"sandbox":"sb-hold"}` {
				t.Errorf("upstream body = %q, want {\"sandbox\":\"sb-hold\"}", gotBody)
			}
		})
	}
}

// TestPauseUpstreamErrorPassesThrough asserts a non-2xx daemon response (for
// example a failed engine pause) is passed through with its status and body,
// so the caller sees the daemon's actionable error, not a generic one.
func TestPauseUpstreamErrorPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"pause sandbox: snapshot failed"}`))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	sb, secret := readySandbox(orgA, "sb-err", endpoint, "tok")
	c := newFakeClient(t, sb, secret)
	cp := New(c, WithHTTPClient(srv.Client()))

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.pause", Path: "/v1/pause", Body: []byte(`{"sandbox":"sb-err"}`),
	})
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream 500 passed through; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "snapshot failed") {
		t.Errorf("body = %q, want the upstream error body passed through", resp.Body)
	}
}

// TestPauseCrossOrgIsNotFoundAndNeverReachesEndpoint asserts org A cannot pause
// org B's sandbox: not_found, the endpoint is never hit, AND the response is
// indistinguishable from a sandbox that does not exist at all (no oracle).
func TestPauseCrossOrgIsNotFoundAndNeverReachesEndpoint(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	sb, secret := readySandbox(orgB, "sb-bheld", endpoint, "tok")
	c := newFakeClient(t, sb, secret)
	cp := New(c, WithHTTPClient(srv.Client()))

	cross, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.pause", Path: "/v1/pause", Body: []byte(`{"sandbox":"sb-bheld"}`),
	})
	if cross.Status != http.StatusNotFound {
		t.Fatalf("cross-org pause: status = %d, want 404", cross.Status)
	}
	if reached {
		t.Fatal("cross-org pause REACHED the sandbox endpoint; the boundary failed")
	}

	// A truly missing id must be byte-for-byte indistinguishable modulo the id.
	missing, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.pause", Path: "/v1/pause", Body: []byte(`{"sandbox":"sb-ghost"}`),
	})
	if missing.Status != cross.Status {
		t.Errorf("missing-id status = %d, cross-org status = %d; the two must not differ (oracle)", missing.Status, cross.Status)
	}
	crossBody := strings.ReplaceAll(string(cross.Body), "sb-bheld", "<id>")
	missingBody := strings.ReplaceAll(string(missing.Body), "sb-ghost", "<id>")
	if crossBody != missingBody {
		t.Errorf("cross-org body %q differs from missing-id body %q; a probe can map ids", crossBody, missingBody)
	}
}

// TestPauseNotReadySandboxIsClearError asserts pausing a sandbox with no
// runtime endpoint yet returns an actionable error, mirroring the runtime
// proxy's not-ready behavior.
func TestPauseNotReadySandboxIsClearError(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-cold", "", "tok")
	sb.Status.Phase = v1.SandboxPending
	sb.Status.Endpoint = ""
	c := newFakeClient(t, sb, secret)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.pause", Path: "/v1/pause", Body: []byte(`{"sandbox":"sb-cold"}`),
	})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404-style not-ready error; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "not Ready") {
		t.Errorf("error body not actionable about readiness: %s", resp.Body)
	}
}

// TestPauseBodyWithoutSandboxIsInvalidInput asserts a pause or resume body that
// does not name a sandbox (missing field, empty value, or invalid JSON) is a
// 400 invalid_input with remediation, not a forwarded request.
func TestPauseBodyWithoutSandboxIsInvalidInput(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c)
	for _, body := range []string{`{}`, `{"sandbox":""}`, `not json`, ``} {
		for _, op := range []string{"sandbox.pause", "sandbox.resume"} {
			resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
				OrgID: orgA, Op: op, Path: "/v1/pause", Body: []byte(body),
			})
			if resp.Status != http.StatusBadRequest {
				t.Errorf("%s body=%q: status = %d, want 400; body = %s", op, body, resp.Status, resp.Body)
			}
			if !strings.Contains(string(resp.Body), "invalid_input") {
				t.Errorf("%s body=%q: error code missing invalid_input: %s", op, body, resp.Body)
			}
		}
	}
}

// TestTokenNeverAppearsInErrorOrNonCreateResponse asserts the per-sandbox token
// is returned ONLY on create: status and list responses, and every error body,
// are scanned and must not contain it.
func TestTokenNeverAppearsInErrorOrNonCreateResponse(t *testing.T) {
	const secretToken = "super-secret-token-value"
	sb, secret := readySandbox(orgA, "sb-scan", "10.0.0.9:9091", secretToken)
	c := newFakeClient(t, sb, secret)
	cp := New(c)
	ctx := context.Background()

	// status (owned), list, and a cross-org status (404) must not leak the token.
	for _, req := range []saas.ForwardRequest{
		{OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-scan"},
		{OrgID: orgA, Op: "sandbox.list", Path: "/v1/sandboxes"},
		{OrgID: orgB, Op: "sandbox.status", Path: "/v1/sandboxes/sb-scan"},
	} {
		resp, _ := cp.Forward(ctx, req)
		if strings.Contains(string(resp.Body), secretToken) {
			t.Fatalf("op %s leaked the token in its response body: %s", req.Op, resp.Body)
		}
	}
}

// ----- template.ensure / template.list -----

// TestEnsureTemplateReturnsReadyDescriptor asserts Forward("template.ensure") with
// a valid id returns 200 with a descriptor that matches the SDK-expected shape:
// id, ready (true), and created_at. This is the critical path broken by the bug:
// POST /v1/templates previously fell through to "sandbox.post" -> unknown op ->
// NotFound; this test proves it now returns 200+ready.
func TestEnsureTemplateReturnsReadyDescriptor(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c)
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "template.ensure", Body: []byte(`{"id":"python"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s (want 200+ready, got error)", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if m["id"] != "python" {
		t.Errorf("id = %v, want python", m["id"])
	}
	if m["ready"] != true {
		t.Errorf("ready = %v, want true", m["ready"])
	}
	if m["created_at"] == nil || m["created_at"] == "" {
		t.Errorf("created_at missing from descriptor: %v", m)
	}
}

// TestEnsureTemplateRejectsEmptyID asserts Forward("template.ensure") with an
// empty or missing id field returns 400. A caller cannot ensure a nameless template.
func TestEnsureTemplateRejectsEmptyID(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c)
	for _, body := range []string{`{}`, `{"id":""}`} {
		resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
			OrgID: orgA, Op: "template.ensure", Body: []byte(body),
		})
		if resp.Status != http.StatusBadRequest {
			t.Errorf("body=%q: status = %d, want 400; body = %s", body, resp.Status, resp.Body)
		}
	}
}

// TestListTemplatesReturnsPoolDescriptors asserts Forward("template.list") maps
// existing SandboxPools to template descriptors. A pool with ReadySnapshots > 0
// returns ready: true; one with none returns ready: false.
func TestListTemplatesReturnsPoolDescriptors(t *testing.T) {
	readyPool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "python", Namespace: "mitos-system"},
		Status:     v1.SandboxPoolStatus{ReadySnapshots: 3},
	}
	emptyPool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "mitos-system"},
		Status:     v1.SandboxPoolStatus{ReadySnapshots: 0},
	}
	c := newFakeClient(t, readyPool, emptyPool)
	cp := New(c)
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "template.list",
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	var items []map[string]any
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		id, _ := it["id"].(string)
		byID[id] = it
	}
	if _, ok := byID["python"]; !ok {
		t.Errorf("python pool missing from template list: %v", byID)
	}
	if byID["python"]["ready"] != true {
		t.Errorf("python ready = %v, want true (ReadySnapshots=3)", byID["python"]["ready"])
	}
	if byID["node"]["ready"] != false {
		t.Errorf("node ready = %v, want false (ReadySnapshots=0)", byID["node"]["ready"])
	}
}

// TestForwardUnknownOpReturnsNotFound is a regression guard asserting the fallback
// ("sandbox.post", "sandbox.delete", etc.) still returns not_found, not a panic or
// silent success.
func TestForwardUnknownOpReturnsNotFound(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c)
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.post",
	})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown op", resp.Status)
	}
}

// TestForkBodyTemplateFieldResolvesPool asserts that a sandbox.create request
// carrying a fork-style body ({"template":"python","id":"sb-x"}) resolves the
// "python" pool from the template field. This is the path POST /v1/fork takes
// after opFromPath maps it to sandbox.create: the SDK sends template, not pool
// or image, so the create handler must check all three fields.
func TestForkBodyTemplateFieldResolvesPool(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-fork")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"template":"python","id":"sb-x"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s (template field must resolve the pool)", resp.Status, resp.Body)
	}

	name, _ := decodeBody(t, resp.Body)["id"].(string)
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: name}, &sb); err != nil {
		t.Fatalf("get created sandbox: %v", err)
	}
	if sb.Spec.Source.PoolRef == nil || sb.Spec.Source.PoolRef.Name != "python" {
		t.Errorf("poolRef = %+v, want python (template field not wired into pool resolution)", sb.Spec.Source.PoolRef)
	}
}

// TestCreateResponseIncludesTemplateIDAndForkTimeMs asserts the create (ready)
// response includes the template_id and fork_time_ms fields the Python SDK
// DirectSandbox constructor reads. Without them the SDK raises a KeyError even if
// the gateway correctly routes the request.
func TestCreateResponseIncludesTemplateIDAndForkTimeMs(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-shape")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"template":"python"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if _, ok := m["template_id"]; !ok {
		t.Errorf("template_id missing from create response: %v", m)
	}
	if _, ok := m["fork_time_ms"]; !ok {
		t.Errorf("fork_time_ms missing from create response: %v", m)
	}
	if m["template_id"] != "python" {
		t.Errorf("template_id = %v, want python", m["template_id"])
	}
}

// ----- single-tenant namespace override -----

// TestSingleTenantNamespaceAllOpsUseMitosNS asserts that when
// WithSingleTenantNamespace("mitos") is set, create/status/terminate all use
// "mitos" as the namespace, not the per-org mitos-org-<id> namespace. This
// unblocks hosted QA where a shared pool exists in "mitos" but per-org
// namespaces are not provisioned.
func TestSingleTenantNamespaceAllOpsUseMitosNS(t *testing.T) {
	const ns = "mitos"
	c := newFakeClient(t)
	cp := New(c,
		WithPollInterval(5*time.Millisecond),
		WithReadyTimeout(2*time.Second),
		WithDefaultPool("python"),
		WithSingleTenantNamespace(ns),
	)

	stop := flipToReadyWhenCreatedInNs(t, c, ns, "10.0.0.1:9091", "tok-single")
	defer stop()

	// create: must land in "mitos", not "mitos-org-alpha".
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`),
	})
	if err != nil {
		t.Fatalf("Forward create: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("create response missing id: %v", m)
	}

	// Sandbox must exist in "mitos".
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: id}, &sb); err != nil {
		t.Fatalf("sandbox not found in namespace %q: %v", ns, err)
	}
	if sb.Namespace != ns {
		t.Errorf("namespace = %q, want %q", sb.Namespace, ns)
	}
	// Org label must still be set for authz.
	if sb.Labels[tenant.OrgLabelKey] != orgA {
		t.Errorf("org label = %q, want %q", sb.Labels[tenant.OrgLabelKey], orgA)
	}
	// Must NOT exist in the per-org namespace.
	var wrongSB v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: id}, &wrongSB); err == nil {
		t.Errorf("sandbox found in per-org namespace %q; it should only be in %q", tenant.NamespaceForOrg(orgA), ns)
	}

	// status: must find the sandbox in "mitos".
	resp, _ = cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/" + id,
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("status: status = %d, body = %s", resp.Status, resp.Body)
	}

	// terminate: must delete from "mitos".
	resp, _ = cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.terminate", Path: "/v1/sandboxes/" + id,
	})
	if resp.Status != http.StatusNoContent {
		t.Fatalf("terminate: status = %d, body = %s", resp.Status, resp.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: id}, &sb); err == nil {
		t.Error("sandbox still exists in mitos namespace after terminate")
	}
}

// TestSingleTenantNamespaceCrossOrgAuthzPreserved asserts that single-tenant
// mode does NOT weaken org-label authz: org A cannot read or delete org B's
// sandbox even though both share the fixed namespace. The org-label check, not
// the namespace, is the authz boundary in single-tenant mode.
func TestSingleTenantNamespaceCrossOrgAuthzPreserved(t *testing.T) {
	const ns = "mitos"
	// Pre-seed a sandbox in "mitos" labeled for org B.
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb-orgb-shared",
			Namespace: ns,
			Labels:    tenant.OrgLabels(orgB),
		},
		Status: v1.SandboxStatus{Phase: v1.SandboxReady, Endpoint: "2.2.2.2:9091"},
	}
	c := newFakeClient(t, sb)
	cp := New(c, WithSingleTenantNamespace(ns))

	// org A must not read org B's sandbox.
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-orgb-shared",
	})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("cross-org status in single-tenant mode: status = %d, want 404 (org-label authz broken)", resp.Status)
	}

	// org A must not terminate org B's sandbox.
	resp, _ = cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.terminate", Path: "/v1/sandboxes/sb-orgb-shared",
	})
	if resp.Status != http.StatusNotFound {
		t.Fatalf("cross-org terminate in single-tenant mode: status = %d, want 404 (org-label authz broken)", resp.Status)
	}
	// Sandbox must survive.
	var got v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sb-orgb-shared"}, &got); err != nil {
		t.Fatalf("org B sandbox was deleted by org A in single-tenant mode: %v", err)
	}
}

// TestSingleTenantNamespaceEmptyIsNoOp asserts WithSingleTenantNamespace("")
// is a no-op: the per-org namespace is still used, preserving the
// mitos-org-alpha default-mode behavior.
func TestSingleTenantNamespaceEmptyIsNoOp(t *testing.T) {
	c := newFakeClient(t)
	cp := New(c,
		WithPollInterval(5*time.Millisecond),
		WithReadyTimeout(2*time.Second),
		WithDefaultPool("default"),
		WithSingleTenantNamespace(""),
	)
	stop := flipToReadyWhenCreated(t, c, orgA, "10.0.0.1:9091", "tok")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	id, _ := decodeBody(t, resp.Body)["id"].(string)

	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: id}, &sb); err != nil {
		t.Fatalf("sandbox not in per-org namespace (empty WithSingleTenantNamespace must be a no-op): %v", err)
	}
	if sb.Namespace != "mitos-org-alpha" {
		t.Errorf("namespace = %q, want mitos-org-alpha", sb.Namespace)
	}
}

// ----- test helpers -----

// flipToReadyWhenCreated watches the org namespace for a newly created sandbox
// and flips it to Ready, seeding its token Secret, mimicking the controller.
func flipToReadyWhenCreated(t *testing.T, c client.Client, org, endpoint, token string) (stop func()) {
	return flipWhenCreated(t, c, org, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxReady
		sb.Status.Endpoint = endpoint
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sb.Name + tokenSecretSuffix, Namespace: sb.Namespace},
			Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
		}
		_ = c.Create(context.Background(), secret)
	})
}

// flipWhenCreated polls the org namespace until a sandbox appears, then applies
// mutate to its status. It mimics the controller asynchronously moving the phase.
func flipWhenCreated(t *testing.T, c client.Client, org string, mutate func(*v1.Sandbox)) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ns := tenant.NamespaceForOrg(org)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			default:
			}
			var list v1.SandboxList
			if err := c.List(context.Background(), &list, client.InNamespace(ns)); err == nil {
				for i := range list.Items {
					sb := &list.Items[i]
					if sb.Status.Phase == "" {
						mutate(sb)
						_ = c.Status().Update(context.Background(), sb)
						return
					}
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// flipToReadyWhenCreatedInNs is the namespace-explicit counterpart to
// flipToReadyWhenCreated, used by single-tenant tests where the sandbox lands
// in a fixed namespace rather than a per-org namespace.
func flipToReadyWhenCreatedInNs(t *testing.T, c client.Client, ns, endpoint, token string) (stop func()) {
	t.Helper()
	return flipWhenCreatedInNs(t, c, ns, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxReady
		sb.Status.Endpoint = endpoint
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sb.Name + tokenSecretSuffix, Namespace: sb.Namespace},
			Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
		}
		_ = c.Create(context.Background(), secret)
	})
}

// flipWhenCreatedInNs polls ns until a sandbox appears, then applies mutate to
// its status. It is the namespace-explicit counterpart to flipWhenCreated.
func flipWhenCreatedInNs(t *testing.T, c client.Client, ns string, mutate func(*v1.Sandbox)) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			default:
			}
			var list v1.SandboxList
			if err := c.List(context.Background(), &list, client.InNamespace(ns)); err == nil {
				for i := range list.Items {
					sb := &list.Items[i]
					if sb.Status.Phase == "" {
						mutate(sb)
						_ = c.Status().Update(context.Background(), sb)
						return
					}
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// TestCreateApiServerInvalidSurfacesValidationCause asserts that when the api
// server rejects the sandbox OBJECT as invalid (for example an org id that is
// not a valid label value, #593), the error surfaced to the caller carries the
// validation message and does not claim their request body was malformed JSON.
func TestCreateApiServerInvalidSurfacesValidationCause(t *testing.T) {
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewInvalid(
				v1.GroupVersion.WithKind("Sandbox").GroupKind(), obj.GetName(),
				field.ErrorList{field.Invalid(
					field.NewPath("metadata", "labels"), "bad_",
					"a valid label must be an empty string or consist of alphanumeric characters")})
		},
	})
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if strings.Contains(body, "invalid_json") || strings.Contains(body, "not valid JSON") {
		t.Errorf("error blames the caller's JSON for an object validation failure: %s", body)
	}
	if !strings.Contains(body, "metadata.labels") {
		t.Errorf("error body missing the api server validation detail: %s", body)
	}
}
