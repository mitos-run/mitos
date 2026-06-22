package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// fakeExecBackend is an in-process stand-in for the sandbox-server exec path
// (SandboxAPI -> vsock -> guest agent). It records the params it was driven with
// and emits a scripted sequence of chunks so the test can assert the Connect
// Exec handler streams stdout INCREMENTALLY and then a terminal exit, without
// touching vsock or KVM. It mirrors the daemon exec_stream fake-agent harness.
type fakeExecBackend struct {
	gotSandbox string
	gotParams  ExecParams
	// chunks are emitted in order; each one is delivered through onChunk before
	// the next, with a per-chunk gate so the test can observe incremental
	// delivery (the server flushes each chunk as it arrives, not all at once).
	chunks   []execChunk
	released chan struct{}
	exitCode int
	execMs   float64
	err      error
}

type execChunk struct {
	stream string // "stdout" or "stderr"
	data   []byte
}

func (f *fakeExecBackend) RunExecStream(ctx context.Context, sandboxID string, p ExecParams, onChunk func(stream string, data []byte) error) (int, float64, error) {
	f.gotSandbox = sandboxID
	f.gotParams = p
	for _, c := range f.chunks {
		if err := onChunk(c.stream, c.data); err != nil {
			return 0, 0, err
		}
		// Block after the first chunk until the test releases it, proving the
		// handler forwarded the first chunk before the backend produced the rest.
		if f.released != nil {
			select {
			case <-f.released:
			case <-ctx.Done():
				return 0, 0, ctx.Err()
			}
			f.released = nil
		}
	}
	return f.exitCode, f.execMs, f.err
}

func newTestServer(t *testing.T, svc *Service) (sandboxv1connect.SandboxClient, func()) {
	t.Helper()
	mux := http.NewServeMux()
	path, h := sandboxv1connect.NewSandboxHandler(svc)
	mux.Handle(path, h)
	// Connect bidi streaming (Exec, PortForward) requires HTTP/2. Serve and dial
	// unencrypted HTTP/2 (the stdlib h2c support added in Go 1.26) so the
	// in-process test exercises the bidi transport without TLS.
	url, httpClient := serveUnencryptedHTTP2(t, mux)
	client := sandboxv1connect.NewSandboxClient(httpClient, url, connect.WithGRPC())
	return client, func() {}
}

// TestExecStreamsStdoutIncrementally is the #24 acceptance core for one
// transport: an Exec call over Connect runs the command in the target sandbox
// and streams stdout chunks INCREMENTALLY, then a terminal ExecExit with the
// expected exit code. The fake backend gates after the first chunk so the test
// proves the first chunk was delivered before the rest were produced.
func TestExecStreamsStdoutIncrementally(t *testing.T) {
	be := &fakeExecBackend{
		chunks: []execChunk{
			{stream: "stdout", data: []byte("hello ")},
			{stream: "stdout", data: []byte("world\n")},
			{stream: "stderr", data: []byte("warn\n")},
		},
		released: make(chan struct{}),
		exitCode: 7,
		execMs:   12.5,
	}
	svc := NewService(be, nil)
	client, closeSrv := newTestServer(t, svc)
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Exec(ctx)

	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Command: "echo hello world",
			Cwd:     "/workspace",
			Env:     []*sandboxv1.EnvVar{{Key: "FOO", Value: "bar"}},
		}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	// First response must be the first stdout chunk, received BEFORE we release
	// the backend to produce the rest. This is the incremental-streaming proof.
	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive first: %v", err)
	}
	if got := string(first.GetStdout()); got != "hello " {
		t.Fatalf("first chunk = %q, want %q", got, "hello ")
	}
	close(be.released)

	var stdout, stderr []byte
	var exit *sandboxv1.ExecExit
	for {
		resp, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout = append(stdout, m.Stdout...)
		case *sandboxv1.ExecResponse_Stderr:
			stderr = append(stderr, m.Stderr...)
		case *sandboxv1.ExecResponse_Exit:
			exit = m.Exit
		}
	}

	if exit == nil {
		t.Fatal("no terminal ExecExit frame")
	}
	if exit.GetExitCode() != 7 {
		t.Fatalf("exit code = %d, want 7", exit.GetExitCode())
	}
	if string(stdout) != "world\n" {
		t.Fatalf("remaining stdout = %q, want %q", string(stdout), "world\n")
	}
	if string(stderr) != "warn\n" {
		t.Fatalf("stderr = %q, want %q", string(stderr), "warn\n")
	}

	// The handler must have bridged to the backend with the command and cwd from
	// the ExecOpen.
	if be.gotParams.Command != "echo hello world" {
		t.Fatalf("backend command = %q", be.gotParams.Command)
	}
	if be.gotParams.WorkingDir != "/workspace" {
		t.Fatalf("backend cwd = %q", be.gotParams.WorkingDir)
	}
	if be.gotParams.Env["FOO"] != "bar" {
		t.Fatalf("backend env FOO = %q", be.gotParams.Env["FOO"])
	}
}

// TestExecFirstMessageMustBeOpen rejects a stream whose first message is not an
// ExecOpen with an LLM-legible InvalidArgument, since the protocol requires the
// open oneof first.
func TestExecFirstMessageMustBeOpen(t *testing.T) {
	svc := NewService(&fakeExecBackend{}, nil)
	client, closeSrv := newTestServer(t, svc)
	defer closeSrv()

	stream := client.Exec(context.Background())
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("data")},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("expected error for non-open first message")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestBudgetReportsAllowances proves a second, unary RPC is real: Budget returns
// the allowances the provider reports.
func TestBudgetReportsAllowances(t *testing.T) {
	be := &fakeExecBackend{}
	bp := budgetFunc(func(ctx context.Context, sandboxID string) (*sandboxv1.BudgetStatus, error) {
		return &sandboxv1.BudgetStatus{
			Fork:       &sandboxv1.Allowance{Remaining: 3, Limit: 5},
			Checkpoint: &sandboxv1.Allowance{Remaining: 1, Limit: 2},
		}, nil
	})
	svc := NewService(be, bp)
	client, closeSrv := newTestServer(t, svc)
	defer closeSrv()

	resp, err := client.Budget(context.Background(), connect.NewRequest(&sandboxv1.BudgetRequest{}))
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if resp.Msg.GetFork().GetRemaining() != 3 || resp.Msg.GetFork().GetLimit() != 5 {
		t.Fatalf("fork allowance = %+v", resp.Msg.GetFork())
	}
}

// budgetFunc adapts a function to the BudgetProvider interface for the test.
type budgetFunc func(ctx context.Context, sandboxID string) (*sandboxv1.BudgetStatus, error)

func (f budgetFunc) Budget(ctx context.Context, sandboxID string) (*sandboxv1.BudgetStatus, error) {
	return f(ctx, sandboxID)
}

// TestUnimplementedRPCsAreHonest asserts every RPC not yet built returns
// CodeUnimplemented with a message that names issue #24, so the partial service
// is honest about what is a tracked follow-up.
func TestUnimplementedRPCsAreHonest(t *testing.T) {
	svc := NewService(&fakeExecBackend{}, nil)
	client, closeSrv := newTestServer(t, svc)
	defer closeSrv()

	ctx := context.Background()
	_, err := client.List(ctx, connect.NewRequest(&sandboxv1.ListRequest{Parent: "/workspace"}))
	if err == nil {
		t.Fatal("List should be unimplemented")
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("List code = %v, want Unimplemented", connect.CodeOf(err))
	}

	// Budget with no provider is also an honest Unimplemented.
	_, berr := client.Budget(ctx, connect.NewRequest(&sandboxv1.BudgetRequest{}))
	if berr == nil || connect.CodeOf(berr) != connect.CodeUnimplemented {
		t.Fatalf("Budget without provider code = %v, want Unimplemented", connect.CodeOf(berr))
	}
}

// serveUnencryptedHTTP2 starts an httptest server speaking unencrypted HTTP/2
// (stdlib h2c, Go 1.26) and returns its base URL and a matching client. Connect
// bidi streams need HTTP/2; this keeps the test off TLS without the deprecated
// x/net/http2/h2c shim. The server is closed via t.Cleanup.
func serveUnencryptedHTTP2(t *testing.T, h http.Handler) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)

	var cp http.Protocols
	cp.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &cp}}
	return srv.URL, client
}
