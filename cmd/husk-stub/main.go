// Command husk-stub is the single-VM husk process: it brings up a DORMANT
// Firecracker VMM at start, then listens on a control Unix socket and ACTIVATES
// the VM in place by loading a snapshot when an activate request arrives. One
// husk-stub process owns exactly one VM.
//
// The activate path drives a VMM and FAILS CLOSED: a snapshot-load or
// guest-readiness failure is reported as an error result and the VM is left
// unusable rather than reported as live. All lifecycle logging goes to stderr
// and never includes secrets.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/husk"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "husk-stub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		firecrackerBin = flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
		kernel         = flag.String("kernel", "", "path to the guest kernel image")
		workdir        = flag.String("workdir", "", "per-VM working directory (firecracker cmd.Dir; vsock UDS is bound relative to it)")
		controlSocket  = flag.String("control-socket", "", "path to the control Unix socket to listen on for activate requests")
		vcpus          = flag.Int("vcpus", 1, "guest vCPU count")
		memMiB         = flag.Int("mem-mib", 512, "guest memory in MiB")
	)
	flag.Parse()

	if *workdir == "" {
		return fmt.Errorf("--workdir is required")
	}
	if *controlSocket == "" {
		return fmt.Errorf("--control-socket is required")
	}

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	cfg := firecracker.VMConfig{
		ID:             "husk",
		FirecrackerBin: *firecrackerBin,
		WorkDir:        *workdir,
		KernelPath:     *kernel,
		SocketPath:     filepath.Join(*workdir, "firecracker.sock"),
		VcpuCount:      *vcpus,
		MemSizeMib:     *memMiB,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stub := husk.New(cfg, husk.Options{})

	fmt.Fprintln(os.Stderr, "husk-stub: preparing dormant VMM")
	if err := stub.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare dormant VMM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: dormant, state=%s\n", stub.State())

	// Tear the VMM down on exit (signal or after serving), reaping the
	// firecracker process.
	defer func() {
		if err := stub.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "husk-stub: close: %v\n", err)
		}
	}()

	// Fresh control socket; a stale file from a prior run would block bind.
	_ = os.Remove(*controlSocket)
	ln, err := net.Listen("unix", *controlSocket)
	if err != nil {
		return fmt.Errorf("listen on control socket %s: %w", *controlSocket, err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: serving control socket %s\n", *controlSocket)

	if err := stub.Serve(ctx, ln); err != nil {
		return fmt.Errorf("serve control socket: %w", err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: stopped, state=%s\n", stub.State())
	return nil
}
