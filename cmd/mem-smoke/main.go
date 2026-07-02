// Command mem-smoke drives the real KVM-backed fork engine to prove that the
// lifetime memory metering tracks a fork's memory GROWTH after a real workload,
// not just the T=0 dirty-page footprint recorded at fork time
// (fork-correctness section 5, issue #3). It builds a template, forks one
// sandbox, samples Engine.Metering().TotalUnique at T=0, runs a memory-growth
// workload IN the guest over the agent (allocate and touch many pages so the
// fork's private dirty set grows), re-samples, and asserts the metered unique
// bytes grew by at least a floor. Because Engine.Metering re-stats each live
// sandbox's /proc/<pid>/smaps_rollup on every call, the second sample reflects
// the post-workload footprint; a metric that stayed at the T=0 value would be
// the "misleading memory accounting" bug this phase regression-tests.
//
// This binary only does real work on a KVM host (the real fork engine needs
// /dev/kvm); it is built and invoked only from the KVM workflow. It compiles on
// any platform so the cross-build checks pass.
//
// Usage:
//
//	mem-smoke \
//	  --image /tmp/rootfs.ext4 \
//	  --data-dir /tmp/mem-smoke \
//	  --firecracker /usr/local/bin/firecracker \
//	  --kernel /tmp/vmlinux \
//	  --agent-bin /tmp/agent \
//	  --grow-mib 64 \
//	  --min-growth-bytes 33554432
//
// Every assertion gates: any failure exits nonzero so the CI step fails. A setup
// error (engine/template/fork) exits 2 so the workflow can distinguish it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func main() {
	image := flag.String("image", "", "rootfs.ext4 path (agent as /init) to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary")
	growMiB := flag.Int("grow-mib", 64, "MiB of memory the in-guest workload allocates and touches")
	minGrowth := flag.Int64("min-growth-bytes", 32<<20, "minimum metered unique-byte growth that must be observed after the workload")
	flag.Parse()

	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "mem-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}

	if err := run(opts{
		image:     *image,
		dataDir:   *dataDir,
		fcBin:     *fcBin,
		kernel:    *kernel,
		agentBin:  *agentBin,
		growMiB:   *growMiB,
		minGrowth: *minGrowth,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mem-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("mem-smoke: PASS: lifetime memory metering tracked fork growth after a real workload")
}

type opts struct {
	image, dataDir, fcBin, kernel, agentBin string
	growMiB                                 int
	minGrowth                               int64
}

func run(o opts) error {
	engine, err := fork.NewEngine(o.dataDir, o.fcBin, o.kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    o.agentBin,
	})
	if err != nil {
		return setupErr(fmt.Errorf("new engine: %w", err))
	}

	templateID := "mem-tmpl"
	fmt.Printf("mem-smoke: building template %q from %q\n", templateID, o.image)
	if err := engine.CreateTemplate(templateID, o.image, nil, nil, nil, nil, false); err != nil {
		return setupErr(fmt.Errorf("create template: %w", err))
	}

	sandboxID := "mem-fork-1"
	fmt.Printf("mem-smoke: forking sandbox %q\n", sandboxID)
	res, err := engine.Fork(templateID, sandboxID, fork.ForkOpts{})
	if err != nil {
		return setupErr(fmt.Errorf("fork: %w", err))
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	client, err := connect(res.VsockPath)
	if err != nil {
		return setupErr(fmt.Errorf("connect to forked guest agent: %w", err))
	}
	defer client.Close() //nolint:errcheck // best-effort

	// T=0: the metered unique bytes right after the fork, before the workload.
	// Engine.Metering re-stats smaps_rollup, so this is a live read.
	base := engine.Metering().TotalUnique
	fmt.Printf("mem-smoke: T=0 metered unique bytes = %d\n", base)

	// Allocate and TOUCH growMiB of memory inside the guest so the fork's
	// private dirty set actually grows (touching forces the pages resident;
	// merely reserving them would not). busybox dd writing /dev/zero into a
	// tmpfs-backed file under /dev/shm dirties anonymous pages held by the
	// page cache; on a microVM /dev/shm (or /tmp) is RAM-backed, so this shows
	// up as private dirty memory in smaps_rollup. We keep the file for the
	// duration of the second sample (no rm) so the pages stay resident.
	growCmd := fmt.Sprintf("mkdir -p /dev/shm && dd if=/dev/zero of=/dev/shm/grow bs=1M count=%d 2>/dev/null && sync && echo grown", o.growMiB)
	if out, err := execOK(client, growCmd); err != nil {
		return fmt.Errorf("memory-growth workload failed (out=%q): %w", out, err)
	}
	fmt.Printf("mem-smoke: workload touched %d MiB in the guest\n", o.growMiB)

	// Give the host a moment for the guest's faulted pages to be reflected in
	// the Firecracker process RSS, then re-sample.
	time.Sleep(2 * time.Second)
	after := engine.Metering().TotalUnique
	fmt.Printf("mem-smoke: post-workload metered unique bytes = %d\n", after)

	growth := after - base
	fmt.Printf("mem-smoke: metered growth = %d bytes (workload = %d MiB, floor = %d bytes)\n", growth, o.growMiB, o.minGrowth)
	if growth < o.minGrowth {
		return fmt.Errorf("metered unique bytes grew by only %d, want at least %d after a %d MiB workload; the metric is NOT tracking lifetime memory growth (still reporting the T=0 footprint?)", growth, o.minGrowth, o.growMiB)
	}
	fmt.Printf("mem-smoke: metering tracked %d bytes of growth (>= %d floor)\n", growth, o.minGrowth)
	return nil
}

// setupErr wraps an engine/template/fork failure so the workflow can exit 2 to
// distinguish a setup problem from a real metering regression.
func setupErr(err error) error {
	fmt.Fprintf(os.Stderr, "mem-smoke: SETUP: %v\n", err)
	os.Exit(2)
	return err
}

// execOK runs a command in the fork over the gRPC ExecStream RPC and returns its stdout,
// failing if the transport errors or the command exits nonzero.
func execOK(client *guestgrpc.Client, command string) (string, error) {
	ctx := context.Background()
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 120,
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
				return stdout.String(), fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}

// connect dials the forked guest agent over vsock with a bounded retry while the
// restored VM finishes coming up.
func connect(udsPath string) (*guestgrpc.Client, error) {
	ctx := context.Background()
	return guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
}
