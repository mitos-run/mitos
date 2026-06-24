//go:build linux

// Package conformance is the cross-agent regression harness for the sandbox.v1
// and sandbox.internal.v1 gRPC surfaces. It starts both the Go guest agent and
// the Rust guest agent as unix-socket gRPC servers, then drives the same RPC
// battery against each and asserts behavior-identical stable proto responses.
//
// # Normalization
//
// Non-deterministic fields are excluded from comparison and documented here:
//
//   - sampled_at_unix, modified_at_unix: wall-clock timestamps; vary per run.
//   - exec_time_ms: elapsed wall-clock for command execution; non-deterministic.
//   - cpu_steal_percent, cpu_percent: sampled CPU metrics; machine-load dependent.
//   - uptime_seconds: process uptime; always diverges between two separate processes.
//   - pid values in ProcessList: kernel-assigned; never stable across two processes.
//   - signaled_processes in NotifyForkedResponse: /proc-state dependent; see
//     host-safety note below.
//   - mem_used_bytes: current allocation; varies between two running processes.
//
// # Host safety
//
// NotifyForked with real entropy would reseed the kernel CRNG and broadcast
// SIGUSR2 to all /proc processes. On box2 this would kill k3s and other system
// daemons. The harness sends NotifyForked with empty entropy and
// host_wall_clock_nanos=0, which makes applied_clock_step_nanos=0 and
// reseeded_rng=false deterministic across both agents. signaled_processes is
// excluded from comparison (only >=0 is asserted per-agent).
//
// # Running the harness on box2
//
// 1. Build and copy the Go agent test binary:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./guest/agent/ -o agent-go-conform.test
//	scp -F .superpowers/ssh_config agent-go-conform.test box2:/root/
//
// 2. Build the Rust agent-unix binary on box2:
//
//	ssh box2 'source ~/.cargo/env && cd /root/agent-rs-sp1 && cargo build --bin agent-unix 2>&1 | tail -5'
//
// 3. Build and run the harness on box2:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./bench/agent-conformance/ -o conform-harness
//	scp -F .superpowers/ssh_config conform-harness box2:/root/
//	ssh box2 'GO_AGENT_BIN=/root/agent-go-conform.test RUST_AGENT_BIN=/root/agent-rs-sp1/target/debug/agent-unix /root/conform-harness -test.v -test.timeout 120s'
package conformance

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// goAgentBin and rustAgentBin are paths to the Go test binary and Rust
// agent-unix binary, read from env vars GO_AGENT_BIN and RUST_AGENT_BIN.
var (
	goAgentBin   = os.Getenv("GO_AGENT_BIN")
	rustAgentBin = os.Getenv("RUST_AGENT_BIN")
)

// agentConn holds a running agent process and connected gRPC clients.
type agentConn struct {
	name    string
	sandbox sandboxv1.SandboxClient
	control internalv1.ControlClient
	proc    *exec.Cmd
	sockDir string
}

func (a *agentConn) stop() {
	if a.proc != nil && a.proc.Process != nil {
		_ = a.proc.Process.Kill()
		_ = a.proc.Wait()
	}
	if a.sockDir != "" {
		_ = os.RemoveAll(a.sockDir)
	}
}

// startAgent starts an agent binary on a fresh unix socket and returns a
// connected gRPC client. Both the Go test binary and the Rust agent-unix
// binary print "READY\n" to stdout once the listener is up.
//
// The Go agent binary is a go test binary; pass -test.run=NOMATCH so TestMain
// detects AGENT_UNIX_SOCK and starts the unix socket server without running
// any tests.
//
// The workspace argument sets AGENT_WORKSPACE, overriding workspaceRoot in the
// Go agent and the Rust SandboxService so the workspace allowlist passes for
// /tmp paths used by the harness.
func startAgent(t *testing.T, name, binPath, workspace string) *agentConn {
	t.Helper()
	if binPath == "" {
		t.Skipf("skipping %s agent: binary path not set", name)
	}

	sockDir, err := os.MkdirTemp("", "agent-conform-"+name+"-")
	if err != nil {
		t.Fatalf("startAgent %s: MkdirTemp: %v", name, err)
	}
	sockPath := filepath.Join(sockDir, "agent.sock")

	var cmd *exec.Cmd
	if name == "go" {
		// The Go agent binary is a go test binary. -test.run=NOMATCH makes it
		// skip all tests; TestMain detects AGENT_UNIX_SOCK and starts the server.
		cmd = exec.Command(binPath, "-test.run=NOMATCH")
	} else {
		cmd = exec.Command(binPath)
	}
	cmd.Env = append(os.Environ(),
		"AGENT_UNIX_SOCK="+sockPath,
		"AGENT_WORKSPACE="+workspace,
	)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("startAgent %s: StdoutPipe: %v", name, err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("startAgent %s: Start %q: %v", name, binPath, err)
	}

	// Wait for "READY" on stdout.
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdoutPipe)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "READY") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(sockDir)
			t.Fatalf("startAgent %s: process exited before printing READY", name)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(sockDir)
		t.Fatalf("startAgent %s: timed out waiting for READY", name)
	}

	conn, err := grpc.NewClient("passthrough:///unix",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(sockDir)
		t.Fatalf("startAgent %s: grpc.NewClient: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })

	return &agentConn{
		name:    name,
		sandbox: sandboxv1.NewSandboxClient(conn),
		control: internalv1.NewControlClient(conn),
		proc:    cmd,
		sockDir: sockDir,
	}
}

// agents starts both agents and returns them. Tests call this to get a pair;
// if either binary is unset the test is skipped.
//
// Both agents use /tmp as their workspace root so that the workspace allowlist
// in pathAllowed (Go) and SandboxService (Rust) passes for the /tmp paths used
// throughout the harness. Tests that exercise the "outside workspace" rejection
// path (Archive, Watch with /etc) still pass because /etc is never under /tmp.
func agents(t *testing.T) (goAgent, rustAgent *agentConn) {
	t.Helper()
	if goAgentBin == "" {
		t.Skip("GO_AGENT_BIN not set")
	}
	if rustAgentBin == "" {
		t.Skip("RUST_AGENT_BIN not set")
	}

	// Both agents use /tmp as workspace so /tmp paths pass the allowlist.
	// /etc and other paths outside /tmp are still denied, exercising the reject path.
	goAgent = startAgent(t, "go", goAgentBin, "/tmp")
	t.Cleanup(goAgent.stop)

	rustAgent = startAgent(t, "rust", rustAgentBin, "/tmp")
	t.Cleanup(rustAgent.stop)

	return goAgent, rustAgent
}

// codeOf extracts the gRPC status code from an error.
func codeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return status.Code(err)
}

// assertParity reports a test failure when two status codes diverge.
func assertParity(t *testing.T, rpc string, goCode, rustCode codes.Code) {
	t.Helper()
	if goCode != rustCode {
		t.Errorf("[DIVERGENCE] %s: go=%v rust=%v", rpc, goCode, rustCode)
	}
}

// buildTar constructs a single-file tar in memory. It uses archive/tar
// directly so the conformance harness has no external dependency.
func buildTar(entryName string, content []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:     entryName,
		Size:     int64(len(content)),
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Stat RPC
// ---------------------------------------------------------------------------

// TestConformanceStat checks that both agents return is_dir=true, the correct
// name and path for /tmp. modified_at_unix is excluded (wall-clock).
func TestConformanceStat(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	type statResult struct {
		isDir bool
		name  string
		path  string
		code  codes.Code
	}

	doStat := func(a *agentConn, path string) statResult {
		fi, err := a.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: path})
		if err != nil {
			return statResult{code: codeOf(err)}
		}
		return statResult{isDir: fi.IsDir, name: fi.Name, path: fi.Path, code: codes.OK}
	}

	for _, path := range []string{"/tmp", "/"} {
		g := doStat(goA, path)
		r := doStat(rustA, path)
		assertParity(t, "Stat("+path+")", g.code, r.code)
		if g.code != codes.OK {
			continue
		}
		if g.isDir != r.isDir {
			t.Errorf("Stat(%s): is_dir go=%v rust=%v", path, g.isDir, r.isDir)
		}
		if g.name != r.name {
			t.Errorf("Stat(%s): name go=%q rust=%q", path, g.name, r.name)
		}
		if g.path != r.path {
			t.Errorf("Stat(%s): path go=%q rust=%q", path, g.path, r.path)
		}
	}
}

// TestConformanceStatMissing checks that both agents return NotFound for a
// nonexistent path.
func TestConformanceStatMissing(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	path := "/tmp/agent-conform-missing-" + fmt.Sprintf("%d", time.Now().UnixNano())
	_, gErr := goA.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: path})
	_, rErr := rustA.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: path})

	assertParity(t, "Stat(missing)", codeOf(gErr), codeOf(rErr))
	if codeOf(gErr) != codes.NotFound {
		t.Errorf("Stat(missing) go: want NotFound, got %v", codeOf(gErr))
	}
}

// ---------------------------------------------------------------------------
// Mkdir RPC
// ---------------------------------------------------------------------------

// TestConformanceMkdir checks that both agents create a directory and that
// Stat reports is_dir=true for it. Mode=0o755 is asserted on both.
func TestConformanceMkdir(t *testing.T) {
	goA, rustA := agents(t)

	doMkdir := func(a *agentConn) (mode uint32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		dir := filepath.Join("/tmp", fmt.Sprintf("agent-conform-mkdir-%s-%d", a.name, time.Now().UnixNano()))
		if _, err := a.sandbox.Mkdir(ctx, &sandboxv1.MkdirRequest{Path: dir}); err != nil {
			return 0, codeOf(err)
		}
		fi, err := a.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: dir})
		if err != nil {
			return 0, codeOf(err)
		}
		return fi.Mode & 0o777, codes.OK
	}

	gMode, gCode := doMkdir(goA)
	rMode, rCode := doMkdir(rustA)

	assertParity(t, "Mkdir", gCode, rCode)
	if gMode != rMode {
		t.Errorf("Mkdir: mode go=0o%o rust=0o%o", gMode, rMode)
	}
	if gMode != 0o755 {
		t.Errorf("Mkdir go: mode=0o%o, want 0o755", gMode)
	}
}

// ---------------------------------------------------------------------------
// WriteFile + ReadFile RPC
// ---------------------------------------------------------------------------

// TestConformanceWriteReadFile writes bytes via WriteFile, reads them back via
// ReadFile, and asserts the round-trip content and bytes_written match.
// modified_at_unix is excluded (wall-clock).
func TestConformanceWriteReadFile(t *testing.T) {
	goA, rustA := agents(t)

	content := []byte("conformance payload\nsecond line\n")

	doRoundTrip := func(a *agentConn) (read []byte, written int64, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		path := filepath.Join("/tmp", fmt.Sprintf("agent-conform-rw-%s-%d.txt", a.name, time.Now().UnixNano()))

		ws, err := a.sandbox.WriteFile(ctx)
		if err != nil {
			return nil, 0, codeOf(err)
		}
		if err := ws.Send(&sandboxv1.WriteFileRequest{
			Msg: &sandboxv1.WriteFileRequest_Open{
				Open: &sandboxv1.WriteFileOpen{Path: path, Mode: 0o644},
			},
		}); err != nil {
			return nil, 0, codes.Unavailable
		}
		if err := ws.Send(&sandboxv1.WriteFileRequest{
			Msg: &sandboxv1.WriteFileRequest_Data{Data: content},
		}); err != nil {
			return nil, 0, codes.Unavailable
		}
		res, err := ws.CloseAndRecv()
		if err != nil {
			return nil, 0, codeOf(err)
		}
		written = res.BytesWritten

		rs, err := a.sandbox.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: path})
		if err != nil {
			return nil, written, codeOf(err)
		}
		for {
			chunk, err := rs.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return read, written, codeOf(err)
			}
			read = append(read, chunk.Data...)
			if chunk.Eof {
				break
			}
		}
		return read, written, codes.OK
	}

	gRead, gWritten, gCode := doRoundTrip(goA)
	rRead, rWritten, rCode := doRoundTrip(rustA)

	assertParity(t, "WriteFile+ReadFile", gCode, rCode)
	if gWritten != rWritten {
		t.Errorf("WriteFile: bytes_written go=%d rust=%d", gWritten, rWritten)
	}
	if !bytes.Equal(gRead, rRead) {
		t.Errorf("ReadFile: content diverges: go=%q rust=%q", gRead, rRead)
	}
	if !bytes.Equal(gRead, content) {
		t.Errorf("ReadFile go: got %q, want %q", gRead, content)
	}
}

// TestConformanceWriteFileModeAtomic checks that both agents set the file mode
// to exactly the requested mode immediately after WriteFile returns.
func TestConformanceWriteFileModeAtomic(t *testing.T) {
	goA, rustA := agents(t)

	doWriteMode := func(a *agentConn) (mode uint32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		path := filepath.Join("/tmp", fmt.Sprintf("agent-conform-mode-%s-%d.txt", a.name, time.Now().UnixNano()))
		ws, err := a.sandbox.WriteFile(ctx)
		if err != nil {
			return 0, codeOf(err)
		}
		if err := ws.Send(&sandboxv1.WriteFileRequest{
			Msg: &sandboxv1.WriteFileRequest_Open{
				Open: &sandboxv1.WriteFileOpen{Path: path, Mode: 0o600},
			},
		}); err != nil {
			return 0, codes.Unavailable
		}
		if err := ws.Send(&sandboxv1.WriteFileRequest{
			Msg: &sandboxv1.WriteFileRequest_Data{Data: []byte("mode test")},
		}); err != nil {
			return 0, codes.Unavailable
		}
		if _, err := ws.CloseAndRecv(); err != nil {
			return 0, codeOf(err)
		}
		fi, err := a.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: path})
		if err != nil {
			return 0, codeOf(err)
		}
		return fi.Mode & 0o777, codes.OK
	}

	gMode, gCode := doWriteMode(goA)
	rMode, rCode := doWriteMode(rustA)

	assertParity(t, "WriteFile(mode)", gCode, rCode)
	if gMode != rMode {
		t.Errorf("WriteFile(mode): go=0o%o rust=0o%o", gMode, rMode)
	}
	if gMode != 0o600 {
		t.Errorf("WriteFile(mode) go: mode=0o%o, want 0o600", gMode)
	}
}

// ---------------------------------------------------------------------------
// List RPC
// ---------------------------------------------------------------------------

// TestConformanceListMkdirSeeEntry creates a uniquely named directory via each
// agent, lists the parent, and asserts the entry appears. The exact entry count
// is not compared (each agent has its own /tmp namespace via separate processes
// on the same host), only that the created entry is found.
func TestConformanceListMkdirSeeEntry(t *testing.T) {
	goA, rustA := agents(t)

	doMkdirList := func(a *agentConn) (found bool, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		name := fmt.Sprintf("agent-conform-list-%s-%d", a.name, time.Now().UnixNano())
		dir := filepath.Join("/tmp", name)
		if _, err := a.sandbox.Mkdir(ctx, &sandboxv1.MkdirRequest{Path: dir}); err != nil {
			return false, codeOf(err)
		}
		resp, err := a.sandbox.List(ctx, &sandboxv1.ListRequest{Parent: "/tmp"})
		if err != nil {
			return false, codeOf(err)
		}
		for _, e := range resp.Entries {
			if e.Name == name {
				return true, codes.OK
			}
		}
		return false, codes.OK
	}

	gFound, gCode := doMkdirList(goA)
	rFound, rCode := doMkdirList(rustA)

	assertParity(t, "List(after Mkdir)", gCode, rCode)
	if gFound != rFound {
		t.Errorf("List: after Mkdir: go found=%v rust found=%v", gFound, rFound)
	}
	if !gFound {
		t.Errorf("List go: created entry not found in listing")
	}
}

// ---------------------------------------------------------------------------
// Remove RPC
// ---------------------------------------------------------------------------

// TestConformanceRemove writes a file, removes it, and asserts Stat returns
// NotFound. Both agents must produce identical error codes.
func TestConformanceRemove(t *testing.T) {
	goA, rustA := agents(t)

	doRemove := func(a *agentConn) (postStatCode codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		path := filepath.Join("/tmp", fmt.Sprintf("agent-conform-rm-%s-%d.txt", a.name, time.Now().UnixNano()))
		ws, err := a.sandbox.WriteFile(ctx)
		if err != nil {
			return codeOf(err)
		}
		_ = ws.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{Path: path, Mode: 0o644}}})
		_ = ws.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Data{Data: []byte("to remove")}})
		if _, err := ws.CloseAndRecv(); err != nil {
			return codeOf(err)
		}
		if _, err := a.sandbox.Remove(ctx, &sandboxv1.RemoveRequest{Path: path}); err != nil {
			return codeOf(err)
		}
		_, serr := a.sandbox.Stat(ctx, &sandboxv1.StatRequest{Path: path})
		return codeOf(serr)
	}

	gCode := doRemove(goA)
	rCode := doRemove(rustA)

	assertParity(t, "Remove+Stat", gCode, rCode)
	if gCode != codes.NotFound {
		t.Errorf("Remove go: post-remove Stat code=%v, want NotFound", gCode)
	}
}

// TestConformanceRemoveMissingIsOK checks that removing a nonexistent path
// succeeds on both agents (mirrors os.RemoveAll no-op behavior).
func TestConformanceRemoveMissingIsOK(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	path := "/tmp/agent-conform-never-existed-" + fmt.Sprintf("%d", time.Now().UnixNano())
	_, gErr := goA.sandbox.Remove(ctx, &sandboxv1.RemoveRequest{Path: path})
	_, rErr := rustA.sandbox.Remove(ctx, &sandboxv1.RemoveRequest{Path: path})

	assertParity(t, "Remove(missing)", codeOf(gErr), codeOf(rErr))
	if codeOf(gErr) != codes.OK {
		t.Errorf("Remove(missing) go: %v", gErr)
	}
}

// ---------------------------------------------------------------------------
// Exec RPC
// ---------------------------------------------------------------------------

// TestConformanceExecEcho checks that both agents stream stdout correctly.
// exec_time_ms is excluded (non-deterministic wall-clock).
func TestConformanceExecEcho(t *testing.T) {
	goA, rustA := agents(t)

	doExec := func(a *agentConn) (stdout string, exit int32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		stream, err := a.sandbox.Exec(ctx)
		if err != nil {
			return "", 0, codeOf(err)
		}
		if err := stream.Send(&sandboxv1.ExecRequest{
			Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
				Command: "printf 'hello conformance'", Cwd: "/tmp", TimeoutSeconds: 10,
			}},
		}); err != nil {
			return "", 0, codes.Unavailable
		}
		_ = stream.CloseSend()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return stdout, exit, codeOf(err)
			}
			switch m := resp.Msg.(type) {
			case *sandboxv1.ExecResponse_Stdout:
				stdout += string(m.Stdout)
			case *sandboxv1.ExecResponse_Exit:
				exit = m.Exit.ExitCode
				// exec_time_ms excluded: wall-clock non-deterministic.
			}
		}
		return stdout, exit, codes.OK
	}

	gStdout, gExit, gCode := doExec(goA)
	rStdout, rExit, rCode := doExec(rustA)

	assertParity(t, "Exec(echo)", gCode, rCode)
	if gStdout != rStdout {
		t.Errorf("Exec(echo): stdout go=%q rust=%q", gStdout, rStdout)
	}
	if gExit != rExit {
		t.Errorf("Exec(echo): exit_code go=%d rust=%d", gExit, rExit)
	}
	if gStdout != "hello conformance" {
		t.Errorf("Exec(echo) go: got %q, want 'hello conformance'", gStdout)
	}
}

// TestConformanceExecNonZeroExit checks that both agents propagate non-zero
// exit codes identically.
func TestConformanceExecNonZeroExit(t *testing.T) {
	goA, rustA := agents(t)

	doExit := func(a *agentConn) (exit int32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		stream, err := a.sandbox.Exec(ctx)
		if err != nil {
			return 0, codeOf(err)
		}
		if err := stream.Send(&sandboxv1.ExecRequest{
			Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "exit 42", Cwd: "/tmp"}},
		}); err != nil {
			return 0, codes.Unavailable
		}
		_ = stream.CloseSend()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return exit, codeOf(err)
			}
			if e, ok := resp.Msg.(*sandboxv1.ExecResponse_Exit); ok {
				exit = e.Exit.ExitCode
			}
		}
		return exit, codes.OK
	}

	gExit, gCode := doExit(goA)
	rExit, rCode := doExit(rustA)

	assertParity(t, "Exec(exit 42)", gCode, rCode)
	if gExit != rExit {
		t.Errorf("Exec(exit 42): go=%d rust=%d", gExit, rExit)
	}
	if gExit != 42 {
		t.Errorf("Exec(exit 42) go: got %d, want 42", gExit)
	}
}

// TestConformanceExecStderr checks that both agents stream stderr correctly.
func TestConformanceExecStderr(t *testing.T) {
	goA, rustA := agents(t)

	doStderr := func(a *agentConn) (stderr string, exit int32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		stream, err := a.sandbox.Exec(ctx)
		if err != nil {
			return "", 0, codeOf(err)
		}
		if err := stream.Send(&sandboxv1.ExecRequest{
			Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
				Command: "printf 'err output' >&2", Cwd: "/tmp",
			}},
		}); err != nil {
			return "", 0, codes.Unavailable
		}
		_ = stream.CloseSend()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return stderr, exit, codeOf(err)
			}
			switch m := resp.Msg.(type) {
			case *sandboxv1.ExecResponse_Stderr:
				stderr += string(m.Stderr)
			case *sandboxv1.ExecResponse_Exit:
				exit = m.Exit.ExitCode
			}
		}
		return stderr, exit, codes.OK
	}

	gStderr, gExit, gCode := doStderr(goA)
	rStderr, rExit, rCode := doStderr(rustA)

	assertParity(t, "Exec(stderr)", gCode, rCode)
	if gStderr != rStderr {
		t.Errorf("Exec(stderr): go=%q rust=%q", gStderr, rStderr)
	}
	if gExit != rExit {
		t.Errorf("Exec(stderr): exit_code go=%d rust=%d", gExit, rExit)
	}
}

// TestConformanceExecArgsUnimplemented checks that both agents reject the
// argv (shell-less) Exec shape with Unimplemented.
func TestConformanceExecArgsUnimplemented(t *testing.T) {
	goA, rustA := agents(t)

	doArgsExec := func(a *agentConn) codes.Code {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		stream, err := a.sandbox.Exec(ctx)
		if err != nil {
			return codeOf(err)
		}
		if err := stream.Send(&sandboxv1.ExecRequest{
			Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
				Command: "echo", Args: []string{"hello"},
			}},
		}); err != nil {
			return codes.Unavailable
		}
		_ = stream.CloseSend()
		_, recvErr := stream.Recv()
		return codeOf(recvErr)
	}

	gCode := doArgsExec(goA)
	rCode := doArgsExec(rustA)

	assertParity(t, "Exec(args)", gCode, rCode)
	if gCode != codes.Unimplemented {
		t.Errorf("Exec(args) go: want Unimplemented, got %v", gCode)
	}
}

// ---------------------------------------------------------------------------
// Archive RPC
// ---------------------------------------------------------------------------

// TestConformanceArchiveUntarDirectionInvalid checks that both agents reject
// Archive with UNTAR direction as InvalidArgument.
func TestConformanceArchiveUntarDirectionInvalid(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doArchiveUntar := func(a *agentConn) codes.Code {
		stream, err := a.sandbox.Archive(ctx, &sandboxv1.ArchiveRequest{
			Direction: sandboxv1.ArchiveRequest_UNTAR, Path: "/tmp",
		})
		if err != nil {
			return codeOf(err)
		}
		_, recvErr := stream.Recv()
		return codeOf(recvErr)
	}

	gCode := doArchiveUntar(goA)
	rCode := doArchiveUntar(rustA)

	assertParity(t, "Archive(UNTAR)", gCode, rCode)
	if gCode != codes.InvalidArgument {
		t.Errorf("Archive(UNTAR) go: want InvalidArgument, got %v", gCode)
	}
}

// TestConformanceArchiveOutsideWorkspacePermDenied checks that both agents
// reject Archive of a path outside the workspace allowlist.
func TestConformanceArchiveOutsideWorkspace(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doArchiveEtc := func(a *agentConn) codes.Code {
		stream, err := a.sandbox.Archive(ctx, &sandboxv1.ArchiveRequest{
			Direction: sandboxv1.ArchiveRequest_DOWNLOAD, Path: "/etc",
		})
		if err != nil {
			return codeOf(err)
		}
		_, recvErr := stream.Recv()
		return codeOf(recvErr)
	}

	gCode := doArchiveEtc(goA)
	rCode := doArchiveEtc(rustA)

	assertParity(t, "Archive(outside-ws)", gCode, rCode)
	if gCode != codes.PermissionDenied {
		t.Errorf("Archive(outside-ws) go: want PermissionDenied, got %v", gCode)
	}
}

// ---------------------------------------------------------------------------
// Upload RPC
// ---------------------------------------------------------------------------

// TestConformanceUploadRoundtrip uploads a tar and reads back the extracted
// file. bytes_written and content must match across both agents.
func TestConformanceUploadRoundtrip(t *testing.T) {
	goA, rustA := agents(t)

	fileContent := []byte("upload conformance content")
	tarBytes, err := buildTar("hello.txt", fileContent)
	if err != nil {
		t.Fatalf("buildTar: %v", err)
	}

	doUpload := func(a *agentConn) (written int64, extracted []byte, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		dest := filepath.Join("/tmp", fmt.Sprintf("agent-conform-upload-%s-%d", a.name, time.Now().UnixNano()))
		us, err := a.sandbox.Upload(ctx)
		if err != nil {
			return 0, nil, codeOf(err)
		}
		if err := us.Send(&sandboxv1.UploadRequest{
			Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: dest}},
		}); err != nil {
			return 0, nil, codes.Unavailable
		}
		if err := us.Send(&sandboxv1.UploadRequest{
			Msg: &sandboxv1.UploadRequest_Chunk{Chunk: tarBytes},
		}); err != nil {
			return 0, nil, codes.Unavailable
		}
		res, err := us.CloseAndRecv()
		if err != nil {
			return 0, nil, codeOf(err)
		}
		written = res.BytesWritten

		rs, err := a.sandbox.ReadFile(ctx, &sandboxv1.ReadFileRequest{
			Path: filepath.Join(dest, "hello.txt"),
		})
		if err != nil {
			return written, nil, codeOf(err)
		}
		for {
			chunk, err := rs.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return written, extracted, codeOf(err)
			}
			extracted = append(extracted, chunk.Data...)
			if chunk.Eof {
				break
			}
		}
		return written, extracted, codes.OK
	}

	gWritten, gContent, gCode := doUpload(goA)
	rWritten, rContent, rCode := doUpload(rustA)

	assertParity(t, "Upload", gCode, rCode)
	if gWritten != rWritten {
		t.Errorf("Upload: bytes_written go=%d rust=%d", gWritten, rWritten)
	}
	if !bytes.Equal(gContent, rContent) {
		t.Errorf("Upload: extracted content go=%q rust=%q", gContent, rContent)
	}
	if !bytes.Equal(gContent, fileContent) {
		t.Errorf("Upload go: got %q, want %q", gContent, fileContent)
	}
}

// ---------------------------------------------------------------------------
// Watch RPC
// ---------------------------------------------------------------------------

// TestConformanceWatchNonDirInvalidArg checks that both agents reject Watch on
// a regular file with InvalidArgument. Watch is a server-streaming RPC: the
// error comes via the first Recv(), not from the initial Watch() call.
func TestConformanceWatchNonDirInvalidArg(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	// Create the file under /tmp (which is the workspace root for both agents,
	// so the allowlist check passes and we reach the isDir check).
	path := filepath.Join("/tmp", fmt.Sprintf("agent-conform-watch-file-%d.txt", time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })

	doWatchFile := func(a *agentConn) codes.Code {
		// Watch() opens the stream without returning the handler error; the error
		// surfaces on the first Recv() call. If Watch() itself errors, return that.
		stream, err := a.sandbox.Watch(ctx, &sandboxv1.WatchRequest{Path: path})
		if err != nil {
			return codeOf(err)
		}
		// Drain one message to receive the error the handler sent.
		_, recvErr := stream.Recv()
		return codeOf(recvErr)
	}

	gCode := doWatchFile(goA)
	rCode := doWatchFile(rustA)

	assertParity(t, "Watch(file)", gCode, rCode)
	if gCode != codes.InvalidArgument {
		t.Errorf("Watch(file) go: want InvalidArgument, got %v", gCode)
	}
}

// TestConformanceWatchCreate checks that both agents deliver a CREATE FsEvent
// when a file is created under the watched directory.
func TestConformanceWatchCreate(t *testing.T) {
	goA, rustA := agents(t)

	doWatchCreate := func(a *agentConn) (kind sandboxv1.FsEvent_Kind, pathSuffix string, code codes.Code) {
		dir := filepath.Join("/tmp", fmt.Sprintf("agent-conform-watch-%s-%d", a.name, time.Now().UnixNano()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, "", codes.Internal
		}
		defer os.RemoveAll(dir)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		stream, err := a.sandbox.Watch(ctx, &sandboxv1.WatchRequest{Path: dir})
		if err != nil {
			return 0, "", codeOf(err)
		}
		// Give the inotify watch a moment to install.
		time.Sleep(150 * time.Millisecond)

		if err := os.WriteFile(filepath.Join(dir, "created.txt"), []byte("hi"), 0o644); err != nil {
			return 0, "", codes.Internal
		}

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ev, err := stream.Recv()
			if err != nil {
				return 0, "", codeOf(err)
			}
			if ev.Kind == sandboxv1.FsEvent_CREATE {
				return ev.Kind, filepath.Base(ev.Path), codes.OK
			}
		}
		return 0, "", codes.DeadlineExceeded
	}

	gKind, gPath, gCode := doWatchCreate(goA)
	rKind, rPath, rCode := doWatchCreate(rustA)

	assertParity(t, "Watch(CREATE)", gCode, rCode)
	if gKind != rKind {
		t.Errorf("Watch(CREATE): kind go=%v rust=%v", gKind, rKind)
	}
	if gPath != rPath {
		t.Errorf("Watch(CREATE): path suffix go=%q rust=%q", gPath, rPath)
	}
}

// ---------------------------------------------------------------------------
// Processes RPC
// ---------------------------------------------------------------------------

// TestConformanceProcessesSane checks that both agents return at least one
// process with valid field invariants. PIDs are excluded (non-deterministic).
func TestConformanceProcessesSane(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doProcesses := func(a *agentConn) (count int, allSane bool, code codes.Code) {
		resp, err := a.sandbox.Processes(ctx, &sandboxv1.ProcessesRequest{})
		if err != nil {
			return 0, false, codeOf(err)
		}
		for _, p := range resp.Processes {
			if p.Pid <= 0 || p.State == "" || p.RssBytes < 0 {
				return len(resp.Processes), false, codes.OK
			}
		}
		return len(resp.Processes), true, codes.OK
	}

	gCount, gSane, gCode := doProcesses(goA)
	rCount, rSane, rCode := doProcesses(rustA)

	if gCode == codes.Internal {
		t.Skip("Processes: /proc not available")
	}

	assertParity(t, "Processes", gCode, rCode)
	if !gSane {
		t.Errorf("Processes go: some entries fail field invariants")
	}
	if !rSane {
		t.Errorf("Processes rust: some entries fail field invariants")
	}
	if gCount == 0 {
		t.Errorf("Processes go: returned 0 entries")
	}
	if rCount == 0 {
		t.Errorf("Processes rust: returned 0 entries")
	}
}

// ---------------------------------------------------------------------------
// Signal RPC
// ---------------------------------------------------------------------------

// TestConformanceSignalPid1InvalidArg checks that both agents reject Signal to
// pid 1 with InvalidArgument (the guest control plane guard).
func TestConformanceSignalPid1InvalidArg(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doSignalPid1 := func(a *agentConn) codes.Code {
		_, err := a.sandbox.Signal(ctx, &sandboxv1.SignalRequest{Pid: 1, Signal: 15})
		return codeOf(err)
	}

	gCode := doSignalPid1(goA)
	rCode := doSignalPid1(rustA)

	assertParity(t, "Signal(pid=1)", gCode, rCode)
	if gCode != codes.InvalidArgument {
		t.Errorf("Signal(pid=1) go: want InvalidArgument, got %v", gCode)
	}
}

// TestConformanceSignalBadSigInvalidArg checks that both agents reject
// out-of-range signal numbers with InvalidArgument.
func TestConformanceSignalBadSigInvalidArg(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	for _, sig := range []int32{0, 65, -1} {
		s := sig
		doSignalBadSig := func(a *agentConn) codes.Code {
			_, err := a.sandbox.Signal(ctx, &sandboxv1.SignalRequest{Pid: 2, Signal: s})
			return codeOf(err)
		}
		gCode := doSignalBadSig(goA)
		rCode := doSignalBadSig(rustA)

		assertParity(t, fmt.Sprintf("Signal(sig=%d)", s), gCode, rCode)
		if gCode != codes.InvalidArgument {
			t.Errorf("Signal(sig=%d) go: want InvalidArgument, got %v", s, gCode)
		}
	}
}

// ---------------------------------------------------------------------------
// PortForward RPC
// ---------------------------------------------------------------------------

// TestConformancePortForwardInvalidPort checks that both agents reject invalid
// port numbers with InvalidArgument.
func TestConformancePortForwardInvalidPort(t *testing.T) {
	goA, rustA := agents(t)

	for _, port := range []uint32{0, 65536} {
		p := port
		doPortFwdBadPort := func(a *agentConn) codes.Code {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			stream, err := a.sandbox.PortForward(ctx)
			if err != nil {
				return codeOf(err)
			}
			if err := stream.Send(&sandboxv1.Frame{
				Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: p}},
			}); err != nil {
				return codes.Unavailable
			}
			_ = stream.CloseSend()
			_, recvErr := stream.Recv()
			return codeOf(recvErr)
		}

		gCode := doPortFwdBadPort(goA)
		rCode := doPortFwdBadPort(rustA)

		assertParity(t, fmt.Sprintf("PortForward(port=%d)", p), gCode, rCode)
		if gCode != codes.InvalidArgument {
			t.Errorf("PortForward(port=%d) go: want InvalidArgument, got %v", p, gCode)
		}
	}
}

// TestConformancePortForwardMissingOpen checks that both agents reject a
// stream whose first frame is not open with InvalidArgument.
func TestConformancePortForwardMissingOpen(t *testing.T) {
	goA, rustA := agents(t)

	doMissingOpen := func(a *agentConn) codes.Code {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		stream, err := a.sandbox.PortForward(ctx)
		if err != nil {
			return codeOf(err)
		}
		if err := stream.Send(&sandboxv1.Frame{
			Msg: &sandboxv1.Frame_Data{Data: []byte("too early")},
		}); err != nil {
			return codes.Unavailable
		}
		_ = stream.CloseSend()
		_, recvErr := stream.Recv()
		return codeOf(recvErr)
	}

	gCode := doMissingOpen(goA)
	rCode := doMissingOpen(rustA)

	assertParity(t, "PortForward(no-open)", gCode, rCode)
	if gCode != codes.InvalidArgument {
		t.Errorf("PortForward(no-open) go: want InvalidArgument, got %v", gCode)
	}
}

// ---------------------------------------------------------------------------
// Vitals RPC
// ---------------------------------------------------------------------------

// TestConformanceVitalsSingleShot checks that both agents return a plausible
// single vitals sample. sampled_at_unix, cpu_steal_percent, mem_used_bytes, and
// process_count are non-deterministic between two separate processes; we assert
// mem_total_bytes > 0 (every host has memory) on each agent independently.
func TestConformanceVitalsSingleShot(t *testing.T) {
	goA, rustA := agents(t)

	doVitals := func(a *agentConn) (memTotal int64, procCount int32, code codes.Code) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		stream, err := a.sandbox.Vitals(ctx, &sandboxv1.VitalsRequest{IntervalSeconds: 0})
		if err != nil {
			return 0, 0, codeOf(err)
		}
		v, err := stream.Recv()
		if err != nil {
			return 0, 0, codeOf(err)
		}
		// sampled_at_unix: excluded (wall-clock).
		// cpu_steal_percent: excluded (machine-load).
		// mem_used_bytes: excluded (process-specific).
		return v.MemTotalBytes, v.ProcessCount, codes.OK
	}

	gMem, gProc, gCode := doVitals(goA)
	rMem, rProc, rCode := doVitals(rustA)

	if gCode == codes.Internal {
		t.Skip("Vitals: /proc not available")
	}

	assertParity(t, "Vitals", gCode, rCode)
	if gMem <= 0 {
		t.Errorf("Vitals go: mem_total_bytes=%d, want > 0", gMem)
	}
	if rMem <= 0 {
		t.Errorf("Vitals rust: mem_total_bytes=%d, want > 0", rMem)
	}
	if gProc <= 0 {
		t.Errorf("Vitals go: process_count=%d, want > 0", gProc)
	}
	if rProc <= 0 {
		t.Errorf("Vitals rust: process_count=%d, want > 0", rProc)
	}
}

// ---------------------------------------------------------------------------
// Control: Ping, Configure, NotifyForked
// ---------------------------------------------------------------------------

// TestConformanceControlPing checks that both agents return a non-negative
// uptime. The exact value is excluded (non-deterministic uptime).
func TestConformanceControlPing(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doPing := func(a *agentConn) (uptime float64, code codes.Code) {
		resp, err := a.control.Ping(ctx, &internalv1.PingRequest{})
		if err != nil {
			return 0, codeOf(err)
		}
		return resp.UptimeSeconds, codes.OK
	}

	gUp, gCode := doPing(goA)
	rUp, rCode := doPing(rustA)

	assertParity(t, "Ping", gCode, rCode)
	if gUp < 0 {
		t.Errorf("Ping go: uptime=%f, want >= 0", gUp)
	}
	if rUp < 0 {
		t.Errorf("Ping rust: uptime=%f, want >= 0", rUp)
	}
}

// TestConformanceControlConfigure checks that both agents accept Configure
// and return an empty OK response.
func TestConformanceControlConfigure(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doConfigure := func(a *agentConn) codes.Code {
		_, err := a.control.Configure(ctx, &internalv1.ConfigureRequest{
			Env:     map[string]string{"TEST_KEY": "test_value"},
			Secrets: map[string]string{"TEST_SECRET": "test_secret_value"},
		})
		return codeOf(err)
	}

	gCode := doConfigure(goA)
	rCode := doConfigure(rustA)

	assertParity(t, "Configure", gCode, rCode)
	if gCode != codes.OK {
		t.Errorf("Configure go: %v", gCode)
	}
}

// TestConformanceControlNotifyForked checks that both agents return identical
// deterministic fields for NotifyForked with empty entropy and zero clock.
//
// Host-safe: empty entropy -> no RNDADDENTROPY ioctl. Zero clock -> no
// CLOCK_REALTIME step. signaled_processes excluded (see package comment).
func TestConformanceControlNotifyForked(t *testing.T) {
	goA, rustA := agents(t)
	ctx := context.Background()

	doNotify := func(a *agentConn) (reseeded bool, stepNanos int64, code codes.Code) {
		resp, err := a.control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
			Generation:         1,
			HostWallClockNanos: 0,
			Entropy:            []byte{},
		})
		if err != nil {
			return false, 0, codeOf(err)
		}
		// signaled_processes: excluded (see host-safety note in package comment).
		return resp.ReseededRng, resp.AppliedClockStepNanos, codes.OK
	}

	gReseeded, gStep, gCode := doNotify(goA)
	rReseeded, rStep, rCode := doNotify(rustA)

	assertParity(t, "NotifyForked", gCode, rCode)
	if gReseeded != rReseeded {
		t.Errorf("NotifyForked: reseeded_rng go=%v rust=%v", gReseeded, rReseeded)
	}
	if gStep != rStep {
		t.Errorf("NotifyForked: applied_clock_step_nanos go=%d rust=%d", gStep, rStep)
	}
	// Deterministic assertions for zero-entropy zero-clock call.
	if gReseeded {
		t.Errorf("NotifyForked go: empty entropy must yield reseeded_rng=false")
	}
	if gStep != 0 {
		t.Errorf("NotifyForked go: zero clock must yield step=0, got %d", gStep)
	}
}
