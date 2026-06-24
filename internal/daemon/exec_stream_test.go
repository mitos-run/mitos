package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// newStreamAPI creates a SandboxAPI backed by a fake in-process gRPC guest
// server that emits one stdout chunk ("hello\n") followed by a clean exit (0).
func newStreamAPI(t *testing.T) (*SandboxAPI, string) {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	fake := &fakeGuestSandbox{
		execStdout: "hello\n",
		execExit:   0,
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api, sock
}

func TestHandleExecStreamNDJSON(t *testing.T) {
	api, _ := newStreamAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var sawExit bool
	var out string
	for sc.Scan() {
		var line struct {
			Stream   string `json:"stream"`
			Data     string `json:"data"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad ndjson line %q: %v", sc.Text(), err)
		}
		if line.ExitCode != nil {
			sawExit = true
			if *line.ExitCode != 0 {
				t.Fatalf("exit code = %d", *line.ExitCode)
			}
			continue
		}
		if line.Stream == "stdout" {
			d, _ := base64.StdEncoding.DecodeString(line.Data)
			out += string(d)
		}
	}
	if !sawExit {
		t.Fatal("no exit line")
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestHandleExecAggregatesStream(t *testing.T) {
	// Use a fake that sends both stdout and stderr.
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1agg", "vsock.sock")
	fake := &fakeGuestSandboxWithStderr{
		fakeGuestSandbox: fakeGuestSandbox{execExit: 0},
		stdout:           "hello\n",
		stderr:           "warn\n",
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "hello\n" || got.Stderr != "warn\n" {
		t.Fatalf("aggregate = {Stdout:%q Stderr:%q}", got.Stdout, got.Stderr)
	}
}

func TestExecStreamRequiresToken(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb2", "vsock.sock")
	fake := &fakeGuestSandbox{execExit: 0}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir) // NOT tokenless
	if err := api.RegisterSandbox("sb2", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb2", sock)
	api.RegisterToken("sb2", "secret")
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb2", "command": "x"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestStreamPathRegisteredWithSandbox(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb3", "vsock.sock")
	// dialStream speaks AgentPort (52); use the dual UDS helper so the socket
	// accepts both the JSON preamble (port 52) and gRPC (port 53).
	startFakeGuestDualUDS(t, sock, nil, &fakeGuestSandbox{})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb3", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb3", sock)
	sc, err := api.dialStream("sb3")
	if err != nil {
		t.Fatalf("dialStream after register: %v", err)
	}
	sc.Close()
}

// fakeGuestSandboxWithStderr overrides Exec to also emit a stderr chunk,
// allowing tests to verify aggregate stdout+stderr collection.
type fakeGuestSandboxWithStderr struct {
	fakeGuestSandbox
	stdout string
	stderr string
}

func (s *fakeGuestSandboxWithStderr) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	if s.stdout != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte(s.stdout)}}); err != nil {
			return err
		}
	}
	if s.stderr != "" {
		if err := stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: []byte(s.stderr)}}); err != nil {
			return err
		}
	}
	return stream.Send(&sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: s.execExit}}})
}

// TestExecStreamExecTimeMsCarried verifies that exec_time_ms is non-zero and
// matches the guest-reported value on both the blocking /v1/exec path and the
// streaming /v1/exec/stream path. This is the regression guard for the finding
// that the gRPC migration silently zeroed exec_time_ms.
func TestExecStreamExecTimeMsCarried(t *testing.T) {
	const wantMs = 42.5

	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb-ms", "vsock.sock")
	fake := &fakeGuestSandbox{
		execStdout: "hi\n",
		execExit:   0,
		execTimeMs: wantMs,
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb-ms", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-ms", sock)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb-ms", "command": "true"})

	t.Run("blocking_exec", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got struct {
			ExecTimeMs float64 `json:"exec_time_ms"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ExecTimeMs != wantMs {
			t.Errorf("exec_time_ms = %v, want %v", got.ExecTimeMs, wantMs)
		}
	})

	t.Run("streaming_exec", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var gotMs float64
		for sc.Scan() {
			var line struct {
				ExitCode   *int    `json:"exit_code"`
				ExecTimeMs float64 `json:"exec_time_ms"`
			}
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Fatalf("bad ndjson: %v", err)
			}
			if line.ExitCode != nil {
				gotMs = line.ExecTimeMs
			}
		}
		if gotMs != wantMs {
			t.Errorf("streaming exec_time_ms = %v, want %v", gotMs, wantMs)
		}
	})
}

// TestExecStreamSingleSlotAcquire verifies that /v1/exec/stream consumes
// exactly ONE concurrent-stream slot (via vsockGuestConn.Exec), not two.
// The regression: the handler acquired a slot at line ~887 AND vsockGuestConn.Exec
// acquired a second, effectively halving the cap.
//
// We verify this with a cap=1 sandbox: a pre-held slot (simulating one open
// exec) must cause the next /v1/exec/stream to reject with 429. If the cap
// were halved (cap=1 but each exec consumes 2), even cap=2 would reject the
// first exec. We set cap=1, hold 0 slots pre-test, and drive one exec; it
// must succeed (not 429), proving it consumed exactly 1 slot out of 1.
func TestExecStreamSingleSlotAcquire(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb-cap", "vsock.sock")
	fake := &fakeGuestSandbox{execStdout: "ok\n", execExit: 0}
	startFakeGuestGRPCUDS(t, sock, fake)

	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	api.SetMaxStreamsPerSandbox(1) // cap=1: one exec must be admitted
	if err := api.RegisterSandbox("sb-cap", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-cap", sock)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb-cap", "command": "true"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("exec/stream consumed >1 slot (double-acquire regression): rejected 429 with cap=1 and 0 pre-held slots")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}

	// Read to drain the response so the slot is released.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
	}

	// Concurrent cap enforcement: pre-acquire the single slot, then a second
	// exec must be rejected with 429 (single-slot behaviour confirmed).
	rel, ok := api.acquireStream("sb-cap")
	if !ok {
		t.Fatal("setup: could not acquire slot after drain")
	}
	defer rel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r2, err2 := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
		if err2 != nil {
			return
		}
		defer r2.Body.Close()
		if r2.StatusCode != http.StatusTooManyRequests {
			t.Errorf("expected 429 when slot is saturated, got %d", r2.StatusCode)
		}
	}()
	wg.Wait()
}
