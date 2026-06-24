package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
