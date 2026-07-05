package fork

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/guestgrpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestEnginePauseResumePreservesStateKVM is the KVM acceptance bar for issue
// #218: it boots a REAL Firecracker VM, then pauses and resumes it N times and
// asserts that BOTH the filesystem AND the in-memory process state survive every
// cycle. This is the exact bug E2B has (filesystem not persisting after repeated
// pause/resume, auto-pause overridden on reconnect): we beat it by asserting the
// invariant directly as a regression test.
//
// It is GATED: it skips unless /dev/kvm exists AND the asset env vars are set,
// because real pause/resume correctness needs KVM (the mock cannot snapshot real
// memory). The KVM CI workflow (kvm-test.yaml) provides /dev/kvm, the pinned
// kernel, the built agent, and a busybox, and sets:
//
//	MITOS_KVM_KERNEL    path to the guest kernel (vmlinux)
//	MITOS_KVM_AGENT     path to the guest agent binary injected as /init
//	MITOS_KVM_BUSYBOX   path to a static busybox (optional)
//	MITOS_KVM_IMAGE     OCI image to build the template from (default busybox:stable)
//
// On a developer's darwin box or any non-KVM runner the test skips cleanly, so
// it is never a fake pass: it only asserts when it can really boot a VM.
func TestEnginePauseResumePreservesStateKVM(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping pause/resume correctness: /dev/kvm not available (needs a KVM runner)")
	}
	kernel := os.Getenv("MITOS_KVM_KERNEL")
	agentBin := os.Getenv("MITOS_KVM_AGENT")
	if kernel == "" || agentBin == "" {
		t.Skip("skipping pause/resume correctness: set MITOS_KVM_KERNEL and MITOS_KVM_AGENT to run (KVM CI sets these)")
	}
	image := os.Getenv("MITOS_KVM_IMAGE")
	if image == "" {
		image = "busybox:stable"
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	engine, err := NewEngine(t.TempDir(), fcBin, kernel, firecracker.JailerConfig{}, EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    agentBin,
		BusyboxPath:     os.Getenv("MITOS_KVM_BUSYBOX"),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	const templateID = "pause-tmpl"
	if err := engine.CreateTemplate(templateID, image, nil, nil, nil, nil, false, false); err != nil {
		t.Fatalf("create template: %v", err)
	}

	const sandboxID = "pause-sb-1"
	res, err := engine.Fork(templateID, sandboxID, ForkOpts{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	client, err := connectAgentGRPC(res.VsockPath)
	if err != nil {
		t.Fatalf("connect agent: %v", err)
	}
	defer client.Close() //nolint:errcheck // best-effort teardown

	// Filesystem state: write a marker file BEFORE any pause. It must survive
	// every pause/resume cycle (the E2B filesystem-persistence bug).
	const marker = "/pause-marker.txt"
	const want = "state-before-pause"
	if _, err := execOKAgentGRPC(client, fmt.Sprintf("printf %s > %s", want, marker)); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	// Memory state: start a long-running process and capture its PID. If it is
	// still alive (same PID) after each resume, the in-memory process table
	// survived the snapshot+restore, i.e. memory was preserved, not rebooted.
	if _, err := execOKAgentGRPC(client, "sh -c 'sleep 600 & echo $! > /sleeper.pid'"); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid, err := execOKAgentGRPC(client, "cat /sleeper.pid")
	if err != nil {
		t.Fatalf("read sleeper pid: %v", err)
	}
	pid = strings.TrimSpace(pid)

	const cycles = 3
	for i := 0; i < cycles; i++ {
		if err := engine.Pause(sandboxID); err != nil {
			t.Fatalf("pause cycle %d: %v", i, err)
		}
		if err := engine.Resume(sandboxID); err != nil {
			t.Fatalf("resume cycle %d: %v", i, err)
		}

		// Filesystem invariant: the marker survives.
		got, err := execOKAgentGRPC(client, fmt.Sprintf("cat %s", marker))
		if err != nil {
			t.Fatalf("cycle %d: read marker failed (filesystem not preserved?): %v", i, err)
		}
		if strings.TrimSpace(got) != want {
			t.Fatalf("cycle %d: marker = %q, want %q (filesystem state lost across pause/resume)", i, strings.TrimSpace(got), want)
		}

		// Memory invariant: the same process is still alive (proves the in-memory
		// state was restored, not rebooted).
		if _, err := execOKAgentGRPC(client, fmt.Sprintf("kill -0 %s", pid)); err != nil {
			t.Fatalf("cycle %d: sleeper pid %s not alive after resume (memory state lost across pause/resume): %v", i, pid, err)
		}
	}
}

// connectAgentGRPC dials the Rust guest agent on AgentGRPCPort (53) with a
// bounded retry, replacing the removed JSON vsock.Connect path (#310).
func connectAgentGRPC(udsPath string) (*guestgrpc.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect agent gRPC: %w", err)
	}
	return client, nil
}

// execOKAgentGRPC runs a shell command in the sandbox via the gRPC Sandbox
// service and returns stdout, failing if the transport errors or the command
// exits nonzero.
func execOKAgentGRPC(client *guestgrpc.Client, command string) (string, error) {
	stream, err := client.Sandbox.ExecStream(context.Background(), &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 60,
	})
	if err != nil {
		return "", fmt.Errorf("exec stream: %w", err)
	}
	var stdout, stderr strings.Builder
	var exitCode int32
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("recv exec frame: %w", err)
		}
		switch m := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			stderr.Write(m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
			if spawnErr := m.Exit.GetError(); spawnErr != "" {
				return "", fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}
