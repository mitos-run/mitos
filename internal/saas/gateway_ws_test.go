package saas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"mitos.run/mitos/internal/apierr"
)

// fakeRuntimeCP implements both ControlPlane (Forward) and RuntimeResolver so the
// gateway can resolve a websocket runtime target without a cluster.
type fakeRuntimeCP struct {
	endpoint string
	token    string
	id       string
	// when notFoundOrg is set, ResolveRuntime returns not_found for that org to
	// model cross-org isolation.
	notFoundOrg string
}

func (f *fakeRuntimeCP) Forward(_ context.Context, req ForwardRequest) (ForwardResponse, error) {
	return ForwardResponse{Status: http.StatusOK, Body: []byte(`{"forwarded":true,"org":"` + req.OrgID + `"}`)}, nil
}

func (f *fakeRuntimeCP) ResolveRuntime(_ context.Context, orgID, sandboxID string) (RuntimeTarget, *apierr.Error) {
	if f.notFoundOrg != "" && orgID == f.notFoundOrg {
		e := apierr.Get(apierr.CodeNotFound).WithCause("no such sandbox for this organization")
		return RuntimeTarget{}, &e
	}
	return RuntimeTarget{Endpoint: f.endpoint, Token: f.token, SandboxID: f.id}, nil
}

func TestIsWebSocketUpgrade(t *testing.T) {
	cases := []struct {
		name   string
		method string
		conn   string
		upg    string
		want   bool
	}{
		{"plain get", http.MethodGet, "", "", false},
		{"upgrade", http.MethodGet, "Upgrade", "websocket", true},
		{"upgrade in list", http.MethodGet, "keep-alive, Upgrade", "websocket", true},
		{"upgrade mixed case", http.MethodGet, "upgrade", "WebSocket", true},
		{"post with upgrade", http.MethodPost, "Upgrade", "websocket", false},
		{"upgrade non-ws", http.MethodGet, "Upgrade", "h2c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/sandbox.v1.Sandbox/Exec?sandbox=sb1", nil)
			if tc.conn != "" {
				r.Header.Set("Connection", tc.conn)
			}
			if tc.upg != "" {
				r.Header.Set("Upgrade", tc.upg)
			}
			if got := isWebSocketUpgrade(r); got != tc.want {
				t.Fatalf("isWebSocketUpgrade = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRuntimeSandboxID(t *testing.T) {
	t.Run("from query", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox.v1.Sandbox/Exec?sandbox=sb-xyz", nil)
		if got := runtimeSandboxID(r); got != "sb-xyz" {
			t.Fatalf("got %q, want sb-xyz", got)
		}
	})
	t.Run("header wins", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox.v1.Sandbox/Exec?sandbox=sb-query", nil)
		r.Header.Set("X-Sandbox-Id", "sb-header")
		if got := runtimeSandboxID(r); got != "sb-header" {
			t.Fatalf("got %q, want sb-header", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox.v1.Sandbox/Exec", nil)
		if got := runtimeSandboxID(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

// echoBackend stands up a websocket echo server that records the Authorization
// header and ?sandbox= query it was dialed with, then echoes one binary frame.
func echoBackend(t *testing.T, gotAuth, gotSandbox *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotSandbox = r.URL.Query().Get("sandbox")
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"connect.sandbox.v1"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		_ = c.Write(ctx, typ, data)
	}))
}

func TestGatewayWebSocketProxyEndToEnd(t *testing.T) {
	var gotAuth, gotSandbox string
	backend := echoBackend(t, &gotAuth, &gotSandbox)
	defer backend.Close()

	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeRuntimeCP{endpoint: strings.TrimPrefix(backend.URL, "http://"), token: "per-sandbox-secret", id: "sb1"}
	gw := NewGateway(keys, nil, cp, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/sandbox.v1.Sandbox/Exec?sandbox=sb1"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"connect.sandbox.v1"},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + created.RawKey}},
	})
	if err != nil {
		t.Fatalf("ws dial through gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if err := c.Write(ctx, websocket.MessageBinary, []byte("ping-through-gateway")); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageBinary || string(data) != "ping-through-gateway" {
		t.Fatalf("echo = %q (%v), want ping-through-gateway", string(data), typ)
	}
	// The backend must have seen the PER-SANDBOX token, never the customer key,
	// and the resolved sandbox id on the query.
	if gotAuth != "Bearer per-sandbox-secret" {
		t.Fatalf("backend Authorization = %q, want the per-sandbox token (customer key must not leak)", gotAuth)
	}
	if strings.Contains(gotAuth, created.RawKey) {
		t.Fatal("customer key leaked to the sandbox backend")
	}
	if gotSandbox != "sb1" {
		t.Fatalf("backend ?sandbox= = %q, want sb1", gotSandbox)
	}
}

func TestGatewayWebSocketCrossOrgNotFound(t *testing.T) {
	var gotAuth, gotSandbox string
	backend := echoBackend(t, &gotAuth, &gotSandbox)
	defer backend.Close()

	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// The resolver returns not_found for org-a, modelling a sandbox owned by a
	// different org.
	cp := &fakeRuntimeCP{endpoint: strings.TrimPrefix(backend.URL, "http://"), token: "x", id: "sb1", notFoundOrg: "org-a"}
	gw := NewGateway(keys, nil, cp, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/sandbox.v1.Sandbox/Exec?sandbox=sb1"
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + created.RawKey}},
	})
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected the ws dial to fail with not_found, it succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v, want 404 not_found", resp)
	}
	if gotSandbox != "" {
		t.Fatal("the backend was dialed despite a cross-org not_found")
	}
}

func TestGatewayWebSocketRequiresAuth(t *testing.T) {
	cp := &fakeRuntimeCP{endpoint: "127.0.0.1:1", token: "x", id: "sb1"}
	gw := NewGateway(NewKeyService(NewMemStore()), nil, cp, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/sandbox.v1.Sandbox/Exec?sandbox=sb1"
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{})
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected unauth ws dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}
