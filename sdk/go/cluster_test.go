package mitos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultPoolName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"python:3.12", "mitos-default-python-3.12"},
		{"ghcr.io/Acme/Foo-Bar:latest", "mitos-default-ghcr.io-acme-foo-bar-latest"},
		{"busybox", "mitos-default-busybox"},
		{"UPPER/Case:TAG", "mitos-default-upper-case-tag"},
		{strings.Repeat("a", 60) + ":x", "mitos-default-" + strings.Repeat("a", 40)},
		{"registry.io/x@sha256:abc", "mitos-default-registry.io-x-sha256-abc"},
		{"node_18", "mitos-default-node-18"},
	}
	for _, tc := range cases {
		if got := DefaultPoolName(tc.in); got != tc.want {
			t.Errorf("DefaultPoolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// recordedRequest captures one observed request to the mock k8s API.
type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

// mockK8s is an httptest server reproducing the Kubernetes CRD REST surface the
// cluster client uses. Handlers are keyed by "METHOD /path"; an unmatched
// request returns a 404 Status so the SDK's absent-vs-present logic exercises
// the real code path.
type mockK8s struct {
	t        *testing.T
	srv      *httptest.Server
	handlers map[string]http.HandlerFunc
	requests []recordedRequest
}

func newMockK8s(t *testing.T) *mockK8s {
	t.Helper()
	m := &mockK8s{t: t, handlers: map[string]http.HandlerFunc{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
			// Restore the body so a per-path handler can read it again.
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		m.requests = append(m.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		if h, ok := m.handlers[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		writeStatus(w, http.StatusNotFound, "NotFound", "not found")
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// on registers a handler for "METHOD /path".
func (m *mockK8s) on(method, path string, h http.HandlerFunc) {
	m.handlers[method+" "+path] = h
}

// agent builds an AgentRun pointed at the mock with no TLS and no real config.
func (m *mockK8s) agent(t *testing.T, namespace string, opts ...AgentRunOption) *AgentRun {
	t.Helper()
	cfg := &k8sConfig{server: m.srv.URL, token: "test-token", http: m.srv.Client()}
	all := append([]AgentRunOption{WithNamespace(namespace), withK8sConfig(cfg)}, opts...)
	a, err := NewAgentRun(all...)
	if err != nil {
		t.Fatalf("NewAgentRun: %v", err)
	}
	return a
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "message": message, "code": code,
	})
}

func writeObj(w http.ResponseWriter, code int, obj map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

const ns = "agents"

func poolPath(name string) string {
	p := "/apis/mitos.run/v1/namespaces/" + ns + "/sandboxpools"
	if name != "" {
		p += "/" + name
	}
	return p
}

func sbPath(name string) string {
	p := "/apis/mitos.run/v1/namespaces/" + ns + "/sandboxes"
	if name != "" {
		p += "/" + name
	}
	return p
}

func TestSandboxGetOrCreatesPoolThenSandbox(t *testing.T) {
	m := newMockK8s(t)
	poolName := DefaultPoolName("python:3.12")

	// The pool does not exist yet: GET 404, POST creates it.
	m.on(http.MethodGet, poolPath(poolName), func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusNotFound, "NotFound", "no pool")
	})
	var poolCreate map[string]any
	m.on(http.MethodPost, poolPath(""), func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &poolCreate)
		writeObj(w, http.StatusCreated, poolCreate)
	})
	var sbCreate map[string]any
	m.on(http.MethodPost, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sbCreate)
		writeObj(w, http.StatusCreated, sbCreate)
	})

	a := m.agent(t, ns)
	sb, err := a.Sandbox(context.Background(), "python:3.12")
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	if sb.Pool != poolName {
		t.Errorf("sandbox pool = %q, want %q", sb.Pool, poolName)
	}

	// The pool body created the inline template with the right image.
	if got := deepString(poolCreate, "spec", "template", "image"); got != "python:3.12" {
		t.Errorf("pool spec.template.image = %q, want python:3.12", got)
	}
	if got := deepString(poolCreate, "metadata", "name"); got != poolName {
		t.Errorf("pool name = %q, want %q", got, poolName)
	}
	if deepFloat(poolCreate, "spec", "replicas") != 1 {
		t.Errorf("pool spec.replicas = %v, want 1", deepValue(poolCreate, "spec", "replicas"))
	}

	// The sandbox body references the pool via spec.source.poolRef.name.
	if got := deepString(sbCreate, "spec", "source", "poolRef", "name"); got != poolName {
		t.Errorf("sandbox spec.source.poolRef.name = %q, want %q", got, poolName)
	}
	if got := deepString(sbCreate, "kind"); got != "Sandbox" {
		t.Errorf("sandbox kind = %q, want Sandbox", got)
	}

	// The REST paths used must be the namespaced CRD paths.
	assertHit(t, m, http.MethodGet, poolPath(poolName))
	assertHit(t, m, http.MethodPost, poolPath(""))
	assertHit(t, m, http.MethodPost, sbPath(""))
}

func TestSandboxReusesExistingPool(t *testing.T) {
	m := newMockK8s(t)
	poolName := DefaultPoolName("python:3.12")
	m.on(http.MethodGet, poolPath(poolName), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": poolName},
			"spec":     map[string]any{"template": map[string]any{"image": "python:3.12"}},
		})
	})
	m.on(http.MethodPost, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusCreated, map[string]any{})
	})
	a := m.agent(t, ns)
	if _, err := a.Sandbox(context.Background(), "python:3.12"); err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	// No POST to the pool collection: the pool was reused untouched.
	for _, req := range m.requests {
		if req.method == http.MethodPost && req.path == poolPath("") {
			t.Fatalf("expected no pool create when the pool already exists")
		}
	}
}

func TestSandboxRejectsPoolImageMismatch(t *testing.T) {
	m := newMockK8s(t)
	poolName := DefaultPoolName("python:3.12")
	m.on(http.MethodGet, poolPath(poolName), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": poolName},
			"spec":     map[string]any{"template": map[string]any{"image": "node:20"}},
		})
	})
	a := m.agent(t, ns)
	_, err := a.Sandbox(context.Background(), "python:3.12")
	if !errors.Is(err, &Error{Code: "pool_image_mismatch"}) {
		t.Fatalf("expected pool_image_mismatch, got %v", err)
	}
}

func TestPoolCreateTolerates409(t *testing.T) {
	m := newMockK8s(t)
	poolName := DefaultPoolName("busybox")
	m.on(http.MethodGet, poolPath(poolName), func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusNotFound, "NotFound", "no pool")
	})
	m.on(http.MethodPost, poolPath(""), func(w http.ResponseWriter, r *http.Request) {
		// A concurrent creator already made it: AlreadyExists 409.
		writeStatus(w, http.StatusConflict, "AlreadyExists", "pool exists")
	})
	created := false
	m.on(http.MethodPost, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		created = true
		writeObj(w, http.StatusCreated, map[string]any{})
	})
	a := m.agent(t, ns)
	sb, err := a.Sandbox(context.Background(), "busybox")
	if err != nil {
		t.Fatalf("Sandbox tolerating 409: %v", err)
	}
	if sb.Pool != poolName {
		t.Errorf("pool = %q, want %q", sb.Pool, poolName)
	}
	if !created {
		t.Errorf("expected the sandbox to be created after the 409 pool conflict")
	}
}

func TestCreateWithPoolRef(t *testing.T) {
	m := newMockK8s(t)
	var sbBody map[string]any
	m.on(http.MethodPost, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sbBody)
		writeObj(w, http.StatusCreated, sbBody)
	})
	a := m.agent(t, ns)
	sb, err := a.Create(context.Background(),
		WithPool("my-pool"),
		WithName("sb-1"),
		WithEnv(map[string]string{"FOO": "bar"}),
		WithTTL("30m"),
		WithWorkspace("ws-a"),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.Name != "sb-1" || sb.Pool != "my-pool" {
		t.Errorf("handle = %q/%q, want sb-1/my-pool", sb.Name, sb.Pool)
	}
	if got := deepString(sbBody, "spec", "source", "poolRef", "name"); got != "my-pool" {
		t.Errorf("spec.source.poolRef.name = %q, want my-pool", got)
	}
	if got := deepString(sbBody, "spec", "lifetime", "ttl"); got != "30m" {
		t.Errorf("spec.lifetime.ttl = %q, want 30m", got)
	}
	if got := deepString(sbBody, "spec", "workspaceRef", "name"); got != "ws-a" {
		t.Errorf("spec.workspaceRef.name = %q, want ws-a", got)
	}
	env, _ := deepValue(sbBody, "spec", "env").([]any)
	if len(env) != 1 {
		t.Fatalf("expected one env entry, got %v", env)
	}
	first, _ := env[0].(map[string]any)
	if first["name"] != "FOO" || first["value"] != "bar" {
		t.Errorf("env entry = %v, want {name:FOO,value:bar}", first)
	}
}

func TestCreateRequiresPool(t *testing.T) {
	m := newMockK8s(t)
	a := m.agent(t, ns)
	_, err := a.Create(context.Background())
	if !errors.Is(err, &Error{Code: "missing_pool"}) {
		t.Fatalf("expected missing_pool, got %v", err)
	}
}

func TestGetAndFromNameReadPoolRefAndPhase(t *testing.T) {
	m := newMockK8s(t)
	m.on(http.MethodGet, sbPath("sb-x"), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": "sb-x"},
			"spec":     map[string]any{"source": map[string]any{"poolRef": map[string]any{"name": "pool-x"}}},
			"status":   map[string]any{"phase": "Ready", "endpoint": "10.0.0.5:9091"},
		})
	})
	// Ready: the token Secret is read.
	m.on(http.MethodGet, "/api/v1/namespaces/"+ns+"/secrets/sb-x-sandbox-token", func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			// base64("s3cr3t") = "czNjcjN0"
			"data": map[string]any{"token": "czNjcjN0"},
		})
	})
	a := m.agent(t, ns)
	for _, name := range []string{"Get", "FromName"} {
		var sb *ClusterSandbox
		var err error
		if name == "Get" {
			sb, err = a.Get(context.Background(), "sb-x")
		} else {
			sb, err = a.FromName(context.Background(), "sb-x")
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sb.Pool != "pool-x" {
			t.Errorf("%s pool = %q, want pool-x", name, sb.Pool)
		}
		if sb.Phase != PhaseReady {
			t.Errorf("%s phase = %q, want Ready", name, sb.Phase)
		}
		if sb.Endpoint != "10.0.0.5:9091" {
			t.Errorf("%s endpoint = %q, want 10.0.0.5:9091", name, sb.Endpoint)
		}
		if sb.Token() != "s3cr3t" {
			t.Errorf("%s token not loaded from Secret", name)
		}
	}
}

func TestListFiltersByPool(t *testing.T) {
	m := newMockK8s(t)
	m.on(http.MethodGet, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"items": []any{
				map[string]any{
					"metadata": map[string]any{"name": "a"},
					"spec":     map[string]any{"source": map[string]any{"poolRef": map[string]any{"name": "p1"}}},
					"status":   map[string]any{"phase": "Ready"},
				},
				map[string]any{
					"metadata": map[string]any{"name": "b"},
					"spec":     map[string]any{"source": map[string]any{"poolRef": map[string]any{"name": "p2"}}},
					"status":   map[string]any{"phase": "Pending"},
				},
			},
		})
	})
	a := m.agent(t, ns)

	all, err := a.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List(all) = %d, want 2", len(all))
	}

	p1, err := a.List(context.Background(), "p1")
	if err != nil {
		t.Fatalf("List(p1): %v", err)
	}
	if len(p1) != 1 || p1[0].Name != "a" {
		t.Fatalf("List(p1) = %v, want [a]", p1)
	}
}

func TestPoolStatusReadsStatus(t *testing.T) {
	m := newMockK8s(t)
	m.on(http.MethodGet, poolPath("p1"), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": "p1"},
			"spec":     map[string]any{"replicas": 3},
			"status": map[string]any{
				"readySnapshots":   2,
				"nodeDistribution": map[string]any{"node-a": 1, "node-b": 1},
			},
		})
	})
	a := m.agent(t, ns)
	ps, err := a.PoolStatus(context.Background(), "p1")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if ps.Desired != 3 {
		t.Errorf("Desired = %d, want 3", ps.Desired)
	}
	if ps.ReadySnapshots != 2 {
		t.Errorf("ReadySnapshots = %d, want 2", ps.ReadySnapshots)
	}
	if ps.NodeDistribution["node-a"] != 1 || ps.NodeDistribution["node-b"] != 1 {
		t.Errorf("NodeDistribution = %v, want {node-a:1,node-b:1}", ps.NodeDistribution)
	}
}

func TestWorkspaceOps(t *testing.T) {
	m := newMockK8s(t)
	var wsBody map[string]any
	wsPath := "/apis/mitos.run/v1/namespaces/" + ns + "/workspaces"
	m.on(http.MethodPost, wsPath, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &wsBody)
		writeObj(w, http.StatusCreated, wsBody)
	})
	m.on(http.MethodGet, wsPath+"/ws-1", func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": "ws-1"},
			"status":   map[string]any{"head": "ws-1-rev-3", "resumable": true},
		})
	})
	m.on(http.MethodGet, wsPath, func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusOK, map[string]any{
			"items": []any{map[string]any{"metadata": map[string]any{"name": "ws-1"}}},
		})
	})
	a := m.agent(t, ns)

	ws, err := a.CreateWorkspace(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws.Name != "ws-1" {
		t.Errorf("workspace name = %q, want ws-1", ws.Name)
	}
	if got := deepString(wsBody, "kind"); got != "Workspace" {
		t.Errorf("workspace kind = %q, want Workspace", got)
	}

	got, err := a.GetWorkspace(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	head, err := got.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "ws-1-rev-3" {
		t.Errorf("head = %q, want ws-1-rev-3", head)
	}

	list, err := a.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ws-1" {
		t.Fatalf("ListWorkspaces = %v, want [ws-1]", list)
	}
}

func TestGetWorkspaceNotFound(t *testing.T) {
	m := newMockK8s(t)
	wsPath := "/apis/mitos.run/v1/namespaces/" + ns + "/workspaces/missing"
	m.on(http.MethodGet, wsPath, func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusNotFound, "NotFound", "no ws")
	})
	a := m.agent(t, ns)
	_, err := a.GetWorkspace(context.Background(), "missing")
	if !errors.Is(err, &Error{Code: "workspace_not_found"}) {
		t.Fatalf("expected workspace_not_found, got %v", err)
	}
}

func TestSandboxNeedsImageOrPool(t *testing.T) {
	m := newMockK8s(t)
	a := m.agent(t, ns)
	_, err := a.Sandbox(context.Background(), "")
	if !errors.Is(err, &Error{Code: "missing_image_or_pool"}) {
		t.Fatalf("expected missing_image_or_pool, got %v", err)
	}
}

func TestSandboxExplicitPoolSkipsDefaultPool(t *testing.T) {
	m := newMockK8s(t)
	m.on(http.MethodPost, sbPath(""), func(w http.ResponseWriter, r *http.Request) {
		writeObj(w, http.StatusCreated, map[string]any{})
	})
	a := m.agent(t, ns)
	if _, err := a.Sandbox(context.Background(), "", WithPool("explicit")); err != nil {
		t.Fatalf("Sandbox with explicit pool: %v", err)
	}
	// No pool GET/POST occurred: the explicit-pool path never touches a pool.
	for _, req := range m.requests {
		if strings.Contains(req.path, "sandboxpools") {
			t.Fatalf("explicit pool path must not touch sandboxpools, hit %s %s", req.method, req.path)
		}
	}
}

// --- small helpers for asserting on decoded JSON bodies ---

func deepValue(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func deepString(m map[string]any, keys ...string) string {
	s, _ := deepValue(m, keys...).(string)
	return s
}

func deepFloat(m map[string]any, keys ...string) float64 {
	f, _ := deepValue(m, keys...).(float64)
	return f
}

func assertHit(t *testing.T, m *mockK8s, method, path string) {
	t.Helper()
	for _, req := range m.requests {
		if req.method == method && req.path == path {
			return
		}
	}
	t.Errorf("expected a %s %s request; recorded: %v", method, path, m.requests)
}
