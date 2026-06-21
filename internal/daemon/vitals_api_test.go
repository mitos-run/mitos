package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/vsock"
)

// startVitalsVsockAgent answers the agent protocol, returning the supplied
// vitals snapshot for a TypeVitals request (OK for everything else). It lets the
// host-side consumer be exercised on darwin with no KVM.
func startVitalsVsockAgent(t *testing.T, sockPath string, v *vsock.VitalsResponse) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				if !sc.Scan() { // "CONNECT 52"
					return
				}
				if _, err := c.Write([]byte("OK 52\n")); err != nil {
					return
				}
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					resp := vsock.Response{OK: true}
					if req.Type == vsock.TypeVitals {
						resp.Vitals = v
					}
					out, _ := json.Marshal(resp)
					if _, err := c.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
}

func vitalsAPI(t *testing.T, sandboxID string, v *vsock.VitalsResponse) *SandboxAPI {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vitals")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "vsock.sock")
	startVitalsVsockAgent(t, sockPath, v)

	api := NewSandboxAPI(dir)
	api.RegisterToken(sandboxID, "tok")
	api.EnableUnixFallback()
	if err := api.RegisterSandbox(sandboxID, sockPath); err != nil {
		t.Fatal(err)
	}
	return api
}

// TestHandleVitals_Labeled drives the host-side guest telemetry consumer: a
// /v1/vitals request resolves a sandbox, asks its guest agent over vsock, and
// returns the snapshot LABELED with the claim/pool/workspace the host knows. The
// guest data (steal, memory, process table) is carried through verbatim.
func TestHandleVitals_Labeled(t *testing.T) {
	api := vitalsAPI(t, "sb-1", &vsock.VitalsResponse{
		StealFraction:      0.2,
		SampleWindowMs:     100,
		MemTotalKB:         2048000,
		MemAvailableKB:     1024000,
		MemUsedKB:          1024000,
		BalloonReclaimedKB: 512000,
		Processes: []vsock.ProcessEntry{
			{PID: 1, Comm: "agent", State: "S", CPUJiffies: 42, RSSKB: 4096},
			{PID: 99, Comm: "python", State: "R", CPUJiffies: 7, RSSKB: 65536},
		},
	})
	api.SetVitalsLabels("sb-1", VitalsLabels{Claim: "claim-a", Pool: "pool-x", Workspace: "ws-1", Namespace: "team-ns"})

	req := httptest.NewRequest(http.MethodPost, "/v1/vitals", strings.NewReader(`{"sandbox":"sb-1"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got LabeledVitals
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Claim != "claim-a" || got.Pool != "pool-x" || got.Workspace != "ws-1" || got.Namespace != "team-ns" {
		t.Errorf("labels not applied: %+v", got)
	}
	if got.Vitals.StealFraction != 0.2 || got.Vitals.BalloonReclaimedKB != 512000 {
		t.Errorf("vitals not carried: %+v", got.Vitals)
	}
	if len(got.Vitals.Processes) != 2 || got.Vitals.Processes[1].Comm != "python" {
		t.Errorf("process table not carried: %+v", got.Vitals.Processes)
	}
}

// TestForkRecordsVitalsLabels drives the Fork path's label plumbing (issue
// #164): forkd records the claim/pool/workspace/namespace the controller passed
// in the Fork request so the sandbox's /v1/vitals snapshot is labeled. The mock
// engine has no guest, so this asserts the recording, not the live sample.
func TestForkRecordsVitalsLabels(t *testing.T) {
	engine := fork.NewMockEngine() // KVMAvailable=false
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	api := NewSandboxAPI(t.TempDir())
	srv := NewServer(engine, api)

	labels := VitalsLabels{Claim: "claim-a", Pool: "pool-x", Workspace: "ws-1", Namespace: "team-ns"}
	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil, nil, nil, nil, "tok", labels); err != nil {
		t.Fatalf("fork: %v", err)
	}

	got := api.vitalsLabelsFor("sb-mock")
	if got != labels {
		t.Errorf("recorded labels = %+v, want %+v", got, labels)
	}
}

func TestHandleVitals_RequiresBearer(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.RegisterToken("sb-1", "tok")
	req := httptest.NewRequest(http.MethodPost, "/v1/vitals", strings.NewReader(`{"sandbox":"sb-1"}`))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
