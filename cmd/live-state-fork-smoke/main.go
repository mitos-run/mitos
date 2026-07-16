// Command live-state-fork-smoke is the KVM acceptance gate for issue #596: a
// LIVE fork (Engine.ForkRunning) carries the SOURCE's running state, BOTH its
// on-disk filesystem AND its in-memory state, into the child, instead of
// re-forking the cold template.
//
// It proves end to end, on real Firecracker, the load-bearing property this
// issue fixes: before the fix ForkRunning booted the child on the read-only
// TEMPLATE rootfs, so every on-disk write the running source made was silently
// dropped; the child now boots on a copy-on-write clone of the SOURCE's OWN
// writable rootfs, captured while the source is paused so it is consistent with
// the memory checkpoint.
//
// Two markers are written in the running source and asserted in the child:
//
//	(a) DISK carry (the fix): a marker file written to the source's ext4 rootfs
//	    and fsync'd to the virtio block device before the fork. This lives on the
//	    per-fork rootfs clone, NOT in guest RAM. If the child boots on the
//	    template (the old bug) this marker is ABSENT; if it boots on the source's
//	    rootfs clone (the fix) the child reads it back. This is the discriminating
//	    proof of the fix.
//	(b) MEMORY carry: a marker file written to a tmpfs mounted inside the guest,
//	    so it lives purely in guest RAM and is captured by the memory checkpoint,
//	    NOT on any block device. The child must read it back, proving the running
//	    memory state (the SDK-level analog is a variable left set in a persistent
//	    run_code kernel: x = 21 in the source, print(x) == 21 in the child, which
//	    needs a python-capable guest rootfs and is exercised at the SDK layer; the
//	    tmpfs marker proves the same memory-carry property on a plain busybox
//	    guest so this gate runs in the standard KVM CI).
//
// Assertions (exit 1 on failure; setup errors exit 2, mirroring the other KVM
// smokes):
//
//  1. the child reads back the DISK marker byte-for-byte (the fix).
//  2. the child reads back the MEMORY (tmpfs) marker byte-for-byte.
//  3. the child's fork-correctness handshake reports ReseededRNG true, so the
//     child is served with a fresh CRNG (a live fork is still a fork).
//
// This binary only does real work on a KVM host (it needs /dev/kvm and boots a
// real Firecracker microVM through fork.NewEngine, exactly as forkd does). It
// compiles on any platform so cross-build checks pass; it is run only by the KVM
// CI phase (firecracker-test / kvm-test.yaml). To run it by hand on a KVM box:
//
//	sudo ./live-state-fork-smoke \
//	  --image /path/to/rootfs.ext4 --kernel /path/to/vmlinux \
//	  --agent-bin /path/to/agent --data-dir /tmp/live-state-fork-data
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func main() {
	image := flag.String("image", "", "rootfs.ext4 path (agent as /init, with a busybox shell) to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary")
	flag.Parse()
	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "live-state-fork-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}
	if err := run(*image, *dataDir, *fcBin, *kernel, *agentBin); err != nil {
		fmt.Fprintf(os.Stderr, "live-state-fork-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("live-state-fork-smoke: PASS: live fork carried the source filesystem AND memory into the child")
}

func run(image, dataDir, fcBin, kernel, agentBin string) error {
	engine, err := fork.NewEngine(dataDir, fcBin, kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    agentBin,
	})
	if err != nil {
		return setupErr(fmt.Errorf("new engine: %w", err))
	}

	const templateID = "lsf-tmpl"
	if err := engine.CreateTemplate(templateID, image, nil, nil, nil, nil, fork.CreateTemplateOpts{}); err != nil {
		return setupErr(fmt.Errorf("create template: %w", err))
	}

	// Boot the SOURCE sandbox from the cold template. This is the running sandbox
	// we will mutate and then live-fork.
	srcRes, err := engine.Fork(templateID, "lsf-src", fork.ForkOpts{})
	if err != nil {
		return setupErr(fmt.Errorf("fork source: %w", err))
	}
	defer func() { _ = engine.Terminate("lsf-src") }()

	srcClient, err := connect(srcRes.VsockPath)
	if err != nil {
		return setupErr(fmt.Errorf("connect source guest: %w", err))
	}
	defer func() { _ = srcClient.Close() }()
	// Deliver the SOURCE's fork-correctness handshake (cold fork from the
	// template): the agent reseeds its CRNG and steps its clock.
	if err := notifyForked(srcClient, 1); err != nil {
		return setupErr(fmt.Errorf("notify-forked source: %w", err))
	}

	// Distinct nonces per run so a stale template or a re-run can never make an
	// assertion pass by accident.
	diskNonce := "mitos-disk-" + randHex()
	memNonce := "mitos-mem-" + randHex()

	// (a) DISK state: write a marker to the source's ext4 rootfs and fsync it to
	// the virtio block device, so the byte is on the rootfs backing file (the
	// per-fork rootfs clone) BEFORE the checkpoint, not merely in the guest page
	// cache. This is the on-disk state the old code dropped by booting the child
	// on the template.
	const diskPath = "/mitos-disk-marker"
	if _, err := execOut(srcClient, fmt.Sprintf("printf %%s %s > %s && sync", diskNonce, diskPath)); err != nil {
		return setupErr(fmt.Errorf("write source disk marker: %w", err))
	}

	// (b) MEMORY state: mount a tmpfs in the guest and write a marker into it, so
	// the byte lives purely in guest RAM (captured by the memory checkpoint) and
	// on no block device. This proves the running memory state carries into the
	// child independently of the filesystem.
	const memDir = "/mitos-mem"
	const memPath = memDir + "/marker"
	memSetup := fmt.Sprintf("mkdir -p %s && mount -t tmpfs none %s && printf %%s %s > %s", memDir, memDir, memNonce, memPath)
	if _, err := execOut(srcClient, memSetup); err != nil {
		return setupErr(fmt.Errorf("write source memory (tmpfs) marker: %w", err))
	}

	// Sanity: the source itself reads both markers back before the fork, so a
	// later absence in the child is attributable to the fork, not a bad write.
	if got, err := execOut(srcClient, "cat "+diskPath); err != nil || strings.TrimSpace(got) != diskNonce {
		return setupErr(fmt.Errorf("source could not read back its own disk marker (got %q err %v)", strings.TrimSpace(got), err))
	}
	if got, err := execOut(srcClient, "cat "+memPath); err != nil || strings.TrimSpace(got) != memNonce {
		return setupErr(fmt.Errorf("source could not read back its own memory marker (got %q err %v)", strings.TrimSpace(got), err))
	}
	fmt.Printf("live-state-fork-smoke: source wrote disk marker %s and memory marker %s\n", diskNonce, memNonce)

	// === LIVE FORK the running source. ===
	// ForkRunning checkpoints the paused source (memory + the consistent rootfs
	// clone) and boots the child from it. This is the path under test.
	childRes, err := engine.ForkRunning("lsf-src", "lsf-child", true)
	if err != nil {
		return fmt.Errorf("ForkRunning of the running source: %w", err)
	}
	defer func() { _ = engine.Terminate("lsf-child") }()

	childClient, err := connect(childRes.VsockPath)
	if err != nil {
		return fmt.Errorf("connect child guest: %w", err)
	}
	defer func() { _ = childClient.Close() }()
	// The child is a fork: deliver its fork-correctness handshake and assert it
	// reseeded (a live fork is still a fork; the child must not share the source's
	// CRNG state).
	if err := notifyForkedChecked(childClient, 2); err != nil {
		return fmt.Errorf("child fork-correctness handshake: %w", err)
	}

	// (1) DISK carry, the discriminating proof of the fix: the child must read the
	// source's on-disk marker. If the child booted on the template (the old bug)
	// this file is absent and cat fails.
	got, err := execOut(childClient, "cat "+diskPath)
	if err != nil {
		return fmt.Errorf("child could not read the source's DISK marker (the live fork booted on the template, not the source rootfs): %w", err)
	}
	if strings.TrimSpace(got) != diskNonce {
		return fmt.Errorf("child DISK marker = %q, want the source's %q (filesystem not carried)", strings.TrimSpace(got), diskNonce)
	}
	fmt.Printf("live-state-fork-smoke: PASS disk carry, child read %s from the source rootfs\n", diskNonce)

	// (2) MEMORY carry: the child must read the source's tmpfs marker from RAM.
	gotMem, err := execOut(childClient, "cat "+memPath)
	if err != nil {
		return fmt.Errorf("child could not read the source's MEMORY (tmpfs) marker: %w", err)
	}
	if strings.TrimSpace(gotMem) != memNonce {
		return fmt.Errorf("child MEMORY marker = %q, want the source's %q (memory not carried)", strings.TrimSpace(gotMem), memNonce)
	}
	fmt.Printf("live-state-fork-smoke: PASS memory carry, child read %s from the source tmpfs\n", memNonce)

	return nil
}

func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// A failure here only weakens the anti-stale nonce; fall back to a fixed
		// tag rather than aborting the smoke.
		return "fixed"
	}
	return hex.EncodeToString(b)
}

// notifyForked delivers the fork-correctness handshake without asserting the
// result, used for the cold-forked source.
func notifyForked(client *guestgrpc.Client, generation uint64) error {
	_, err := doNotify(client, generation)
	return err
}

// notifyForkedChecked delivers the handshake and FAILS if the guest did not
// reseed its CRNG: a live fork is still a fork, so the child must reseed.
func notifyForkedChecked(client *guestgrpc.Client, generation uint64) error {
	reseeded, err := doNotify(client, generation)
	if err != nil {
		return err
	}
	if !reseeded {
		return fmt.Errorf("child did not reseed its RNG after the live fork")
	}
	return nil
}

func doNotify(client *guestgrpc.Client, generation uint64) (bool, error) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return false, fmt.Errorf("entropy: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
		Generation:         generation,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            entropy,
	})
	if err != nil {
		return false, fmt.Errorf("notify-forked rpc: %w", err)
	}
	if resp == nil {
		return false, fmt.Errorf("notify-forked returned no response")
	}
	return resp.GetReseededRng(), nil
}

func execOut(client *guestgrpc.Client, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
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
				return stdout.String(), fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}

func connect(udsPath string) (*guestgrpc.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
}

// setupErr reports an environment or setup failure and exits 2 (setup) rather
// than 1 (assertion failure), mirroring the other KVM smokes so a flaky boot is
// retried by the CI wrapper rather than reported as a real regression.
func setupErr(err error) error {
	fmt.Fprintf(os.Stderr, "live-state-fork-smoke: SETUP: %v\n", err)
	os.Exit(2)
	return err
}
