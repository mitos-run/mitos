package agentcli

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
	"time"

	connect "connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// hostedCaptured records one HTTP request made to the fake gateway.
type hostedCaptured struct {
	Method string
	Path   string
	Auth   string
	Idem   string // Idempotency-Key header, empty when absent
	Body   map[string]any
}

// hostedFakeSandbox is a Connect SandboxHandler used to fake the runtime RPCs
// (ExecStream) in hosted-mode tests. It records the Authorization and
// X-Sandbox-Id headers so tests can assert the api key reached the gateway and
// the sandbox id was routed correctly. The token VALUE is never logged here
// (just compared, to verify it was present).
type hostedFakeSandbox struct {
	sandboxv1connect.UnimplementedSandboxHandler

	requireToken string // when set, reject non-matching callers

	gotAuth      string
	gotSandboxID string
	gotCommand   string

	stdout   string
	stderr   string
	exitCode int32
}

func (f *hostedFakeSandbox) checkToken(h http.Header) error {
	if f.requireToken == "" {
		return nil
	}
	if h.Get("Authorization") != "Bearer "+f.requireToken {
		// Hostile-server simulation: echo the token so we can prove it is
		// redacted before reaching the caller.
		return connect.NewError(connect.CodeUnauthenticated, errors.New("rejected: "+h.Get("Authorization")))
	}
	return nil
}

func (f *hostedFakeSandbox) ExecStream(_ context.Context, req *connect.Request[sandboxv1.ExecStreamRequest], stream *connect.ServerStream[sandboxv1.ExecResponse]) error {
	if err := f.checkToken(req.Header()); err != nil {
		return err
	}
	f.gotAuth = req.Header().Get("Authorization")
	f.gotSandboxID = req.Header().Get("X-Sandbox-Id")
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
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: f.exitCode}}})
}

// hostedGateway is a fake mitos.run gateway that handles the /v1 REST routes
// and the Connect runtime RPC on one mux, exactly as the real gateway does.
// It records every REST request in Requests and returns canned responses.
type hostedGateway struct {
	Requests []*hostedCaptured

	// Canned REST responses.
	forkID string
	// listJSON is the EXACT GET /v1/sandboxes response body. The production
	// hosted gateway returns an OBJECT {"sandboxes":[...]} whose entries carry
	// id, phase, endpoint, and a camelCase createdAt; the standalone
	// sandbox-server returns a BARE ARRAY of {id, template_id, endpoint,
	// created_at, fork_time_ms}. Tests set whichever wire shape they assert.
	listJSON string

	// connect handler for runtime RPCs.
	connectFake *hostedFakeSandbox
}

func newHostedGateway(apiKey string) *hostedGateway {
	return &hostedGateway{
		forkID:      "sbx-hosted-1",
		connectFake: &hostedFakeSandbox{requireToken: apiKey, stdout: "hello", exitCode: 0},
	}
}

func (g *hostedGateway) capture(r *http.Request) *hostedCaptured {
	c := &hostedCaptured{
		Method: r.Method,
		Path:   r.URL.Path,
		Auth:   r.Header.Get("Authorization"),
		Idem:   r.Header.Get("Idempotency-Key"),
	}
	if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(body) > 0 {
			_ = json.Unmarshal(body, &c.Body)
			// Restore body so other handlers could re-read (not needed here).
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	g.Requests = append(g.Requests, c)
	return c
}

func (g *hostedGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := g.capture(r)

	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/templates":
		// Idempotent template ensure: return the template.
		writeJSON(http.StatusOK, map[string]any{"id": c.Body["id"], "ready": true})

	case r.Method == http.MethodPost && r.URL.Path == "/v1/fork":
		id := g.forkID
		if raw, ok := c.Body["id"].(string); ok && raw != "" {
			id = raw
		}
		writeJSON(http.StatusOK, map[string]any{
			"id":           id,
			"template_id":  c.Body["template"],
			"endpoint":     "sandbox-endpoint:9091",
			"fork_time_ms": 12.5,
		})

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") && strings.HasSuffix(r.URL.Path, "/fork"):
		// Live fork (PR #710): create-shaped response {id, endpoint, token,
		// phase, template_id, fork_time_ms}.
		id := g.forkID
		if raw, ok := c.Body["id"].(string); ok && raw != "" {
			id = raw
		}
		writeJSON(http.StatusOK, map[string]any{
			"id":           id,
			"endpoint":     "sandbox-endpoint:9091",
			"token":        "child-token-never-used-by-fork",
			"phase":        "Ready",
			"template_id":  "python",
			"fork_time_ms": 42.5,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := g.listJSON
		if body == "" {
			body = `{"sandboxes":[]}`
		}
		_, _ = w.Write([]byte(body))

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		// The hosted gateway answers a terminate with 204 No Content.
		w.WriteHeader(http.StatusNoContent)

	default:
		// Fall through to the Connect RPC handler (served from the mux).
		http.NotFound(w, r)
	}
}

// hostedTestServer builds an httptest.Server that serves both the fake gateway
// REST routes and the Connect RPC handler on one mux.
func hostedTestServer(t *testing.T, gw *hostedGateway) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// Mount Connect handler first so /sandbox.v1.Sandbox/* routes to it.
	connectPath, connectHandler := sandboxv1connect.NewSandboxHandler(gw.connectFake)
	mux.Handle(connectPath, connectHandler)
	// Catch-all for the REST routes.
	mux.Handle("/", gw)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// bearerOf returns the bearer token value from an Authorization header
// ("Bearer <value>"), or the raw header value if it does not start with
// "Bearer ".
func bearerOf(auth string) string {
	return strings.TrimPrefix(auth, "Bearer ")
}

// TestHostedBackendCreate asserts Create sends POST /v1/templates then POST
// /v1/fork, that both carry the Bearer token, and that the returned id is
// non-empty.
func TestHostedBackendCreate(t *testing.T) {
	const apiKey = "sk-test-create"
	gw := newHostedGateway(apiKey)
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	id, err := b.Create(context.Background(), "python")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	// Expect exactly two REST requests: POST /v1/templates and POST /v1/fork.
	if len(gw.Requests) != 2 {
		t.Fatalf("expected 2 REST requests, got %d", len(gw.Requests))
	}
	tmplReq := gw.Requests[0]
	if tmplReq.Method != http.MethodPost || tmplReq.Path != "/v1/templates" {
		t.Fatalf("first request = %s %s, want POST /v1/templates", tmplReq.Method, tmplReq.Path)
	}
	if bearerOf(tmplReq.Auth) != apiKey {
		t.Fatalf("template request auth = %q, want Bearer %s", tmplReq.Auth, apiKey)
	}
	if tmplReq.Body["id"] != "python" {
		t.Fatalf("template id = %v, want python", tmplReq.Body["id"])
	}

	forkReq := gw.Requests[1]
	if forkReq.Method != http.MethodPost || forkReq.Path != "/v1/fork" {
		t.Fatalf("second request = %s %s, want POST /v1/fork", forkReq.Method, forkReq.Path)
	}
	if bearerOf(forkReq.Auth) != apiKey {
		t.Fatalf("fork request auth = %q, want Bearer %s", forkReq.Auth, apiKey)
	}
	if forkReq.Body["template"] != "python" {
		t.Fatalf("fork body template = %v, want python", forkReq.Body["template"])
	}
}

// TestHostedBackendExec asserts Exec rides the Connect ExecStream RPC with the
// Bearer token and X-Sandbox-Id header, and that the result round-trips.
func TestHostedBackendExec(t *testing.T) {
	const apiKey = "sk-test-exec"
	gw := newHostedGateway(apiKey)
	gw.connectFake.stdout = "world"
	gw.connectFake.exitCode = 0
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	res, err := b.Exec(context.Background(), "sbx-1", "echo world", 10)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "world" {
		t.Fatalf("Exec stdout = %q, want world", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec exit = %d, want 0", res.ExitCode)
	}
	if gw.connectFake.gotAuth != "Bearer "+apiKey {
		t.Fatalf("Exec auth = %q, want Bearer %s", gw.connectFake.gotAuth, apiKey)
	}
	if gw.connectFake.gotSandboxID != "sbx-1" {
		t.Fatalf("Exec X-Sandbox-Id = %q, want sbx-1", gw.connectFake.gotSandboxID)
	}
	if gw.connectFake.gotCommand != "echo world" {
		t.Fatalf("Exec command = %q, want echo world", gw.connectFake.gotCommand)
	}
}

// TestHostedBackendForkN asserts Fork(id, n) issues n POST
// /v1/sandboxes/{id}/fork live-fork requests (one per child, NEVER the flat
// /v1/fork template route, which the hosted control plane resolves as a pool
// name and 404s with `no such pool "sb-..."`), each carrying the Bearer token,
// a unique child id with pause_source, and a unique Idempotency-Key, and
// returns n distinct ids.
func TestHostedBackendForkN(t *testing.T) {
	const apiKey = "sk-test-fork"
	const srcID = "sbx-src"
	const n = 3

	gw := newHostedGateway(apiKey)
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	ids, err := b.Fork(context.Background(), srcID, n)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("Fork returned %d ids, want %d", len(ids), n)
	}

	if len(gw.Requests) != n {
		t.Fatalf("Fork made %d requests, want %d", len(gw.Requests), n)
	}
	seen := map[string]bool{}
	seenKeys := map[string]bool{}
	for i, req := range gw.Requests {
		if req.Method != http.MethodPost || req.Path != "/v1/sandboxes/"+srcID+"/fork" {
			t.Fatalf("Fork req[%d] = %s %s, want POST /v1/sandboxes/%s/fork", i, req.Method, req.Path, srcID)
		}
		if bearerOf(req.Auth) != apiKey {
			t.Fatalf("Fork req[%d] auth = %q, want Bearer %s", i, req.Auth, apiKey)
		}
		id, _ := req.Body["id"].(string)
		if id == "" || seen[id] {
			t.Fatalf("Fork req[%d] had empty or duplicate id %q", i, id)
		}
		seen[id] = true
		if pause, ok := req.Body["pause_source"].(bool); !ok || !pause {
			t.Fatalf("Fork req[%d] pause_source = %v, want true", i, req.Body["pause_source"])
		}
		if req.Idem == "" || seenKeys[req.Idem] {
			t.Fatalf("Fork req[%d] had empty or duplicate Idempotency-Key %q", i, req.Idem)
		}
		seenKeys[req.Idem] = true
	}
}

// TestHostedBackendForkFallsBackOnOlderGateway asserts that a server which
// answers the live-fork route with a route-level 404 (an older deployment
// without the sandbox.fork op) makes Fork fall back ONCE to the legacy flat
// /v1/fork template route for every child.
func TestHostedBackendForkFallsBackOnOlderGateway(t *testing.T) {
	const apiKey = "sk-test-fork-fallback"
	const srcID = "sbx-src"

	gw := newHostedGateway(apiKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := gw.capture(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") && strings.HasSuffix(r.URL.Path, "/fork"):
			// The older gateway's exact route-level not_found envelope.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such route or operation","cause":"the request did not map to a known gateway operation (resolved op \"sandbox.post\")","remediation":"Use a documented route."}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fork":
			id, _ := c.Body["id"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           id,
				"template_id":  c.Body["template"],
				"endpoint":     "ep:9091",
				"fork_time_ms": 5.0,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	ids, err := b.Fork(context.Background(), srcID, 2)
	if err != nil {
		t.Fatalf("Fork with fallback: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Fork returned %d ids, want 2", len(ids))
	}
	// Exactly one live-route probe, then 2 flat-route forks.
	if len(gw.Requests) != 3 {
		t.Fatalf("Fork made %d requests, want 3 (1 probe + 2 flat)", len(gw.Requests))
	}
	if gw.Requests[0].Path != "/v1/sandboxes/"+srcID+"/fork" {
		t.Fatalf("first request = %q, want the live-fork probe", gw.Requests[0].Path)
	}
	for i, req := range gw.Requests[1:] {
		if req.Path != "/v1/fork" || req.Body["template"] != srcID {
			t.Fatalf("fallback req[%d] = %s body %v, want POST /v1/fork with template %s", i, req.Path, req.Body, srcID)
		}
	}
}

// TestHostedBackendList asserts List calls GET /v1/sandboxes with the Bearer
// token and decodes the PRODUCTION hosted gateway shape: an OBJECT
// {"sandboxes":[...]} (not a bare array) whose entries carry id, phase,
// endpoint, and a camelCase createdAt. Phase and age must come from the wire.
func TestHostedBackendList(t *testing.T) {
	const apiKey = "sk-test-list"
	gw := newHostedGateway(apiKey)
	created1 := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	created2 := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	gw.listJSON = `{"sandboxes":[` +
		`{"id":"sbx-1","phase":"Ready","endpoint":"ep1:9091","createdAt":"` + created1 + `"},` +
		`{"id":"sbx-2","phase":"Pending","endpoint":"ep2:9091","createdAt":"` + created2 + `"}]}`
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	infos, err := b.List(context.Background(), "ignored-namespace")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List returned %d rows, want 2", len(infos))
	}

	// Find the list request.
	var listReq *hostedCaptured
	for _, r := range gw.Requests {
		if r.Method == http.MethodGet && r.Path == "/v1/sandboxes" {
			listReq = r
			break
		}
	}
	if listReq == nil {
		t.Fatal("List did not send GET /v1/sandboxes")
	}
	if bearerOf(listReq.Auth) != apiKey {
		t.Fatalf("List auth = %q, want Bearer %s", listReq.Auth, apiKey)
	}

	// Assert row mapping: phase and age come off the wire, not hardcoded.
	wantPhase := map[string]string{"sbx-1": "Ready", "sbx-2": "Pending"}
	for _, info := range infos {
		want, ok := wantPhase[info.Name]
		if !ok {
			t.Errorf("List row name = %q, want sbx-1 or sbx-2", info.Name)
			continue
		}
		if info.Phase != want {
			t.Errorf("List row %s phase = %q, want %q", info.Name, info.Phase, want)
		}
		if info.Age <= 0 {
			t.Errorf("List row %s age = %v, want > 0 (createdAt must be parsed)", info.Name, info.Age)
		}
	}
}

// TestHostedBackendListBareArray asserts List also decodes the STANDALONE
// sandbox-server shape: a bare JSON array of {id, template_id, endpoint,
// created_at, fork_time_ms} entries. Entries without a phase default to Ready.
func TestHostedBackendListBareArray(t *testing.T) {
	const apiKey = "sk-test-list-array"
	gw := newHostedGateway(apiKey)
	created := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	gw.listJSON = `[{"id":"sbx-a","template_id":"python","endpoint":"ep:9091","created_at":"` + created + `","fork_time_ms":12.5}]`
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	infos, err := b.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List (bare array): %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(infos))
	}
	info := infos[0]
	if info.Name != "sbx-a" {
		t.Errorf("row name = %q, want sbx-a", info.Name)
	}
	if info.Pool != "python" {
		t.Errorf("row pool = %q, want python (template_id)", info.Pool)
	}
	if info.Phase != "Ready" {
		t.Errorf("row phase = %q, want Ready default when the wire has no phase", info.Phase)
	}
	if info.Age <= 0 {
		t.Errorf("row age = %v, want > 0 (created_at must be parsed)", info.Age)
	}
}

// TestHostedBackendTerminate asserts Terminate sends DELETE /v1/sandboxes/{id}
// with the Bearer token.
func TestHostedBackendTerminate(t *testing.T) {
	const apiKey = "sk-test-terminate"
	gw := newHostedGateway(apiKey)
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	if err := b.Terminate(context.Background(), "sbx-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var deleteReq *hostedCaptured
	for _, r := range gw.Requests {
		if r.Method == http.MethodDelete {
			deleteReq = r
			break
		}
	}
	if deleteReq == nil {
		t.Fatal("Terminate did not send DELETE request")
	}
	if deleteReq.Path != "/v1/sandboxes/sbx-1" {
		t.Fatalf("Terminate path = %q, want /v1/sandboxes/sbx-1", deleteReq.Path)
	}
	if bearerOf(deleteReq.Auth) != apiKey {
		t.Fatalf("Terminate auth = %q, want Bearer %s", deleteReq.Auth, apiKey)
	}
}

// TestHostedBackendKeyNeverLogged asserts the api key never appears in an error
// string returned by any verb, even when the server echoes the Authorization
// header (which carries the key) into its error response.
func TestHostedBackendKeyNeverLogged(t *testing.T) {
	const apiKey = "ultra-secret-hosted-key"

	// Simulate a hostile server that echoes the bearer token in its error body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// Intentionally echo the token into the error body.
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "rejected token: " + auth})
	}))
	t.Cleanup(srv.Close)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())

	t.Run("Create", func(t *testing.T) {
		_, err := b.Create(context.Background(), "python")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("api key leaked in Create error: %q", err.Error())
		}
	})

	t.Run("List", func(t *testing.T) {
		_, err := b.List(context.Background(), "")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("api key leaked in List error: %q", err.Error())
		}
	})

	t.Run("Terminate", func(t *testing.T) {
		err := b.Terminate(context.Background(), "sbx-1")
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("api key leaked in Terminate error: %q", err.Error())
		}
	})
}

// TestHostedBackendKeyNeverLoggedExec asserts the api key never appears in the
// error returned by Exec when the Connect server rejects the token and echoes
// the Authorization header.
func TestHostedBackendKeyNeverLoggedExec(t *testing.T) {
	const apiKey = "ultra-secret-exec-key"

	// requireToken mismatch causes the fake to echo the presented Authorization.
	fake := &hostedFakeSandbox{requireToken: "other-token"}
	path, handler := sandboxv1connect.NewSandboxHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	_, err := b.Exec(context.Background(), "sbx-1", "echo", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("api key leaked in Exec error: %q", err.Error())
	}
}

// TestHostedBackendWorkspaceNil asserts Workspace() returns nil so the CLI
// prints a clear "not supported in hosted mode" message rather than panicking.
func TestHostedBackendWorkspaceNil(t *testing.T) {
	b := NewHostedBackend("https://mitos.run", "sk-x", nil)
	if b.Workspace() != nil {
		t.Fatal("Workspace() should return nil for hosted mode")
	}
}

// TestHostedBackendTemplateNil asserts Template() returns nil so the CLI
// prints a clear "not supported in hosted mode" message rather than panicking.
func TestHostedBackendTemplateNil(t *testing.T) {
	b := NewHostedBackend("https://mitos.run", "sk-x", nil)
	if b.Template() != nil {
		t.Fatal("Template() should return nil for hosted mode")
	}
}

// TestHostedBackendNoTokenOmitsHeader asserts that when no api key is
// configured, the Authorization header is absent from every request.
func TestHostedBackendNoTokenOmitsHeader(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/v1/templates":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "python"})
		case "/v1/fork":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sbx-nk-1", "template_id": "python", "endpoint": "ep:9091"})
		}
	}))
	t.Cleanup(srv.Close)

	b := NewHostedBackend(srv.URL, "", srv.Client())
	id, err := b.Create(context.Background(), "python")
	if err != nil {
		t.Fatalf("Create without key: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	for _, auth := range auths {
		if auth != "" {
			t.Fatalf("unexpected Authorization header when no key: %q", auth)
		}
	}
}

// TestIsHostedURL asserts that IsHostedURL correctly classifies hosted vs local
// URLs.
func TestIsHostedURL(t *testing.T) {
	cases := []struct {
		url    string
		hosted bool
	}{
		{DefaultHostedBaseURL, true},
		{"https://mitos.run", true},
		{"https://custom.mitos.run", true},
		{"https://my-sandbox-server.example.com", true},
		{"http://localhost:8080", false},
		{"http://127.0.0.1:8080", false},
		{"http://0.0.0.0:9000", false},
		{"http://[::1]:8080", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsHostedURL(tc.url)
		if got != tc.hosted {
			t.Errorf("IsHostedURL(%q) = %v, want %v", tc.url, got, tc.hosted)
		}
	}
}

// TestHostedBackendCLIRoutesOnAPIKey asserts that Run() routes to hosted mode
// when MITOS_API_KEY is set in the environment, and that the sandbox verbs
// (create, ls, fork, terminate) land on the fake gateway rather than trying
// to load a kubeconfig.
//
// This is an integration test that verifies the end-to-end path from the CLI
// flags / env to HostedBackend without the cluster in the loop.
func TestHostedBackendCLIRoutesOnAPIKey(t *testing.T) {
	// The CLI's Run() takes a pre-built backend, so we can bypass the env
	// detection and directly wire a HostedBackend as the backend for Run tests.
	// This exercises the agentcli.Run dispatch with a HostedBackend.
	const apiKey = "sk-cli-route-test"
	gw := newHostedGateway(apiKey)
	gw.listJSON = `{"sandboxes":[{"id":"sbx-1","phase":"Ready","endpoint":"ep:9091","createdAt":"` +
		time.Now().UTC().Format(time.RFC3339) + `"}]}`
	srv := hostedTestServer(t, gw)

	b := NewHostedBackend(srv.URL, apiKey, srv.Client())
	ctx := context.Background()
	var out, errw bytes.Buffer

	t.Run("create", func(t *testing.T) {
		gw.Requests = nil
		code := Run(ctx, []string{"sandbox", "create", "--pool", "python"}, b, &out, &errw)
		if code != 0 {
			t.Fatalf("create exit = %d, stderr: %s", code, errw.String())
		}
		if out.Len() == 0 {
			t.Fatal("create produced no output")
		}
		// Verify the gateway saw /v1/templates then /v1/fork.
		if len(gw.Requests) < 2 {
			t.Fatalf("expected >= 2 requests, got %d", len(gw.Requests))
		}
	})

	t.Run("ls", func(t *testing.T) {
		gw.Requests = nil
		out.Reset()
		errw.Reset()
		code := Run(ctx, []string{"sandbox", "ls"}, b, &out, &errw)
		if code != 0 {
			t.Fatalf("ls exit = %d, stderr: %s", code, errw.String())
		}
		if !strings.Contains(out.String(), "sbx-1") {
			t.Fatalf("ls output did not contain sbx-1: %s", out.String())
		}
	})

	t.Run("terminate", func(t *testing.T) {
		gw.Requests = nil
		out.Reset()
		errw.Reset()
		code := Run(ctx, []string{"sandbox", "terminate", "sbx-1"}, b, &out, &errw)
		if code != 0 {
			t.Fatalf("terminate exit = %d, stderr: %s", code, errw.String())
		}
		found := false
		for _, r := range gw.Requests {
			if r.Method == http.MethodDelete && strings.HasSuffix(r.Path, "sbx-1") {
				found = true
			}
		}
		if !found {
			t.Fatal("terminate did not issue DELETE request")
		}
	})
}

// TestHostedBackendWorkspaceUnsupportedMessage asserts the CLI prints a clear
// "not supported in hosted mode" message when workspace verbs are used against a
// HostedBackend, rather than panicking or producing a misleading error.
func TestHostedBackendWorkspaceUnsupportedMessage(t *testing.T) {
	b := NewHostedBackend("https://mitos.run", "sk-ws-test", nil)
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"ws", "ls"}, b, &out, &errw)
	if code == 0 {
		t.Fatal("expected non-zero exit for ws command on hosted backend")
	}
	msg := errw.String()
	if !strings.Contains(msg, "workspace") && !strings.Contains(msg, "not support") {
		t.Fatalf("expected 'not support...workspace' message, got: %q", msg)
	}
}
