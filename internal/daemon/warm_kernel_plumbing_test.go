package daemon

// Plumbing test for the warm_kernel flag on the forkd CreateTemplate RPC: the
// gRPC handler must pass CreateTemplateRequest.warm_kernel through to the
// engine unchanged (true when set, false when omitted).

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/volume"
	forkdpb "mitos.run/mitos/proto/forkd"
)

// warmKernelProbeEngine records the warmKernel argument of every
// CreateTemplate call, keyed by template id. It does no real work.
type warmKernelProbeEngine struct {
	ForkEngine // embedded so unused methods are present; calling them panics (none are exercised)

	mu    sync.Mutex
	calls map[string]bool
}

func (e *warmKernelProbeEngine) CreateTemplate(id string, _ string, _ []string, _ []volume.Spec, _ *firecracker.WorkloadSpec, _ *firecracker.VMResources, opts fork.CreateTemplateOpts) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.calls == nil {
		e.calls = make(map[string]bool)
	}
	e.calls[id] = opts.WarmKernel
	return nil
}

func (e *warmKernelProbeEngine) GetCapacity() fork.Capacity {
	return fork.Capacity{TemplateDigests: map[string]string{}}
}

func TestCreateTemplatePlumbsWarmKernel(t *testing.T) {
	engine := &warmKernelProbeEngine{}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis) //nolint:errcheck // test server; errors surface via RPC failures
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := forkdpb.NewForkDaemonClient(conn)

	if _, err := client.CreateTemplate(context.Background(), &forkdpb.CreateTemplateRequest{
		TemplateId: "warm",
		Image:      "python:3.12-slim",
		WarmKernel: true,
	}); err != nil {
		t.Fatalf("CreateTemplate(warm): %v", err)
	}
	if _, err := client.CreateTemplate(context.Background(), &forkdpb.CreateTemplateRequest{
		TemplateId: "cold",
		Image:      "python:3.12-slim",
	}); err != nil {
		t.Fatalf("CreateTemplate(cold): %v", err)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if v, ok := engine.calls["warm"]; !ok || !v {
		t.Errorf("expected warm_kernel=true to reach the engine for %q, got %v (called=%v)", "warm", v, ok)
	}
	if v, ok := engine.calls["cold"]; !ok || v {
		t.Errorf("expected warm_kernel=false (the default) to reach the engine for %q, got %v (called=%v)", "cold", v, ok)
	}
}
