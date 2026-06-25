package sandboxrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// newInterceptedTestServer starts a test HTTP/2 server with the given
// interceptor wired at the handler level. It returns the Connect client and a
// cleanup func. Budget is stubbed to succeed so non-auth errors are test bugs.
func newInterceptedTestServer(t *testing.T, ic connect.Interceptor) sandboxv1connect.SandboxClient {
	t.Helper()
	be := &fakeExecBackend{}
	bp := budgetFunc(func(_ context.Context, _ string) (*sandboxv1.BudgetStatus, error) {
		return &sandboxv1.BudgetStatus{}, nil
	})
	svc := NewService(be, bp)

	mux := http.NewServeMux()
	path, h := sandboxv1connect.NewSandboxHandler(svc, connect.WithInterceptors(ic))
	mux.Handle(path, h)

	srv := httptest.NewUnstartedServer(mux)
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

// newInterceptedExecTestServer starts a test HTTP/2 server with the given
// interceptor for streaming Exec tests.
func newInterceptedExecTestServer(t *testing.T, ic connect.Interceptor) sandboxv1connect.SandboxClient {
	t.Helper()
	be := &fakeExecBackend{exitCode: 0}
	svc := NewService(be, nil)

	mux := http.NewServeMux()
	path, h := sandboxv1connect.NewSandboxHandler(svc, connect.WithInterceptors(ic))
	mux.Handle(path, h)

	srv := httptest.NewUnstartedServer(mux)
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

// budgetCall issues a unary Budget call with the given Authorization and
// X-Sandbox-Id headers.
func budgetCall(t *testing.T, c sandboxv1connect.SandboxClient, authHeader, sandboxID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := connect.NewRequest(&sandboxv1.BudgetRequest{})
	if authHeader != "" {
		req.Header().Set("Authorization", authHeader)
	}
	if sandboxID != "" {
		req.Header().Set("X-Sandbox-Id", sandboxID)
	}
	_, err := c.Budget(ctx, req)
	return err
}

// TestBearerInterceptorRejectsWrongToken is the failing test from the brief:
// a wrong token must yield CodeUnauthenticated.
func TestBearerInterceptorRejectsWrongToken(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "Bearer wrong", "sandbox-1")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated, got %v", err)
	}
}

// TestBearerInterceptorMissingHeader rejects a request with no Authorization
// header with CodeUnauthenticated.
func TestBearerInterceptorMissingHeader(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "", "sandbox-1")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated on missing header, got %v", err)
	}
}

// TestBearerInterceptorMalformedHeader rejects a header that is not
// "Bearer <token>" with CodeUnauthenticated.
func TestBearerInterceptorMalformedHeader(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "Token secret", "sandbox-1")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated on malformed header, got %v", err)
	}
}

// TestBearerInterceptorCorrectTokenPasses lets a request with the right token
// through to the handler (Budget returns 200 OK with an empty status).
func TestBearerInterceptorCorrectTokenPasses(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "Bearer secret", "sandbox-1")
	if err != nil {
		t.Fatalf("want nil error for correct token, got %v", err)
	}
}

// TestBearerInterceptorUnknownSandboxFails rejects a request when the lookup
// function returns ok=false (sandbox not registered) with CodeUnauthenticated.
func TestBearerInterceptorUnknownSandboxFails(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "", false })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "Bearer anything", "unknown")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated for unknown sandbox, got %v", err)
	}
}

// TestBearerInterceptorEmptyRegisteredTokenFails is the defense-in-depth guard:
// a lookup that returns an empty token with ok=true must NOT be matched by an
// empty presented token ("Bearer " with nothing after it); the request is
// rejected before the constant-time compare.
func TestBearerInterceptorEmptyRegisteredTokenFails(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "", true })
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "Bearer ", "sandbox-1")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated for an empty registered token, got %v", err)
	}
}

// TestAllowTokenlessPassesWithoutToken verifies AllowTokenlessInterceptor
// passes requests even when no Authorization header is set.
func TestAllowTokenlessPassesWithoutToken(t *testing.T) {
	ic := AllowTokenlessInterceptor()
	c := newInterceptedTestServer(t, ic)
	err := budgetCall(t, c, "", "")
	if err != nil {
		t.Fatalf("AllowTokenless should pass without a token, got %v", err)
	}
}

// TestBearerInterceptorRejectsWrongTokenStreaming verifies the interceptor
// enforces auth on streaming (bidi) RPCs as well.
func TestBearerInterceptorRejectsWrongTokenStreaming(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedExecTestServer(t, ic)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := c.Exec(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer wrong")
	stream.RequestHeader().Set("X-Sandbox-Id", "sandbox-1")

	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "true"}},
	}); err != nil {
		// Send may surface the auth error; the subsequent Receive will too.
		_ = err
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want CodeUnauthenticated on streaming with wrong token, got %v", err)
	}
}

// TestBearerInterceptorCorrectTokenPassesStreaming verifies a streaming Exec
// call succeeds end-to-end when the correct token is presented.
func TestBearerInterceptorCorrectTokenPassesStreaming(t *testing.T) {
	ic := BearerInterceptor(func(_ string) (string, bool) { return "secret", true })
	c := newInterceptedExecTestServer(t, ic)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := c.Exec(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer secret")
	stream.RequestHeader().Set("X-Sandbox-Id", "sandbox-1")

	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "true"}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	_ = stream.CloseRequest()

	// The fakeExecBackend emits no chunks and exits 0. The handler sends an
	// ExecExit frame; Receive returns it without error.
	resp, err := stream.Receive()
	if err != nil {
		t.Fatalf("want ExecExit frame, got error: %v", err)
	}
	if resp.GetExit() == nil {
		t.Fatalf("want ExecExit in response, got %T", resp.Msg)
	}
}
