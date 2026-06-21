package fork

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
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
	if err := engine.CreateTemplate(templateID, image, nil, nil); err != nil {
		t.Fatalf("create template: %v", err)
	}

	const sandboxID = "pause-sb-1"
	res, err := engine.Fork(templateID, sandboxID, ForkOpts{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	client, err := connectAgent(res.VsockPath)
	if err != nil {
		t.Fatalf("connect agent: %v", err)
	}
	defer client.Close()

	// Filesystem state: write a marker file BEFORE any pause. It must survive
	// every pause/resume cycle (the E2B filesystem-persistence bug).
	const marker = "/pause-marker.txt"
	const want = "state-before-pause"
	if _, err := execOKAgent(client, fmt.Sprintf("printf %s > %s", want, marker)); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	// Memory state: start a long-running process and capture its PID. If it is
	// still alive (same PID) after each resume, the in-memory process table
	// survived the snapshot+restore, i.e. memory was preserved, not rebooted.
	if _, err := execOKAgent(client, "sh -c 'sleep 600 & echo $! > /sleeper.pid'"); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid, err := execOKAgent(client, "cat /sleeper.pid")
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
		got, err := execOKAgent(client, fmt.Sprintf("cat %s", marker))
		if err != nil {
			t.Fatalf("cycle %d: read marker failed (filesystem not preserved?): %v", i, err)
		}
		if strings.TrimSpace(got) != want {
			t.Fatalf("cycle %d: marker = %q, want %q (filesystem state lost across pause/resume)", i, strings.TrimSpace(got), want)
		}

		// Memory invariant: the same process is still alive (proves the in-memory
		// state was restored, not rebooted).
		if _, err := execOKAgent(client, fmt.Sprintf("kill -0 %s", pid)); err != nil {
			t.Fatalf("cycle %d: sleeper pid %s not alive after resume (memory state lost across pause/resume): %v", i, pid, err)
		}
	}
}

// connectAgent dials a forked guest agent over vsock with a bounded retry.
func connectAgent(udsPath string) (*vsock.Client, error) {
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		client, err = vsock.Connect(udsPath, vsock.AgentPort)
		if err == nil {
			_, perr := client.Ping()
			if perr == nil {
				return client, nil
			}
			_ = client.Close()
			err = perr
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("connect after retries: %w", err)
}

// execOKAgent runs a command in the sandbox over the guest agent and returns
// stdout, failing if the transport errors or the command exits nonzero.
func execOKAgent(client *vsock.Client, command string) (string, error) {
	res, err := client.Exec(command, "/", nil, 60)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("command %q exited %d: %s", command, res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}
