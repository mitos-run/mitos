package daemon

import (
	"context"
	"net"
	"testing"

	"github.com/paperclipinc/sandbox/internal/fork"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func newTestClient(t *testing.T) (forkdpb.ForkDaemonClient, *fork.MockEngine) {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
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
	return forkdpb.NewForkDaemonClient(conn), engine
}

func TestGRPCForkLifecycle(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	if _, err := client.CreateTemplate(ctx, &forkdpb.CreateTemplateRequest{
		TemplateId: "py", Image: "python:3.12-slim",
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	forkResp, err := client.Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: "py",
		SandboxId:  "sb-1",
		Env:        []*forkdpb.EnvVar{{Key: "SESSION", Value: "abc"}},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forkResp.SandboxId != "sb-1" || forkResp.Endpoint == "" {
		t.Fatalf("bad fork response: %+v", forkResp)
	}

	runResp, err := client.ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: "sb-1", NewSandboxId: "sb-2", PauseSource: true,
	})
	if err != nil {
		t.Fatalf("ForkRunning: %v", err)
	}
	if runResp.SandboxId != "sb-2" {
		t.Fatalf("got %q, want sb-2", runResp.SandboxId)
	}

	capResp, err := client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{})
	if err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	if capResp.ActiveSandboxes != 2 {
		t.Fatalf("active = %d, want 2", capResp.ActiveSandboxes)
	}
	if len(capResp.TemplateIds) != 1 || capResp.TemplateIds[0] != "py" {
		t.Fatalf("templates = %v, want [py]", capResp.TemplateIds)
	}

	if _, err := client.Terminate(ctx, &forkdpb.TerminateRequest{SandboxId: "sb-1"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestGRPCForkUnknownSnapshot(t *testing.T) {
	client, _ := newTestClient(t)
	_, err := client.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId: "missing", SandboxId: "sb-x",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCUnimplementedRPCsSayWhere(t *testing.T) {
	client, _ := newTestClient(t)
	_, err := client.Exec(context.Background(), &forkdpb.ExecRequest{SandboxId: "sb", Command: "true"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", status.Code(err))
	}
}
