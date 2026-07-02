// Command vol-smoke drives the real KVM-backed fork engine end to end to prove
// per-fork VOLUMES work: it builds a Firecracker template with TWO volumes (a
// Fresh volume and a Snapshot volume whose template seed is pre-written with a
// known file), forks TWO sandboxes with --enable-volumes, and execs assertions
// over the guest agent that prove:
//
//   - Fresh round-trip: fork1 writes a file to its Fresh volume mount path and
//     reads it back.
//   - Snapshot CoW independence: both forks see the seeded file; fork1 writes a
//     fork-unique file to its Snapshot volume; fork2 does NOT see it AND the
//     template seed image on the host is byte-for-byte unchanged (the writes
//     diverge, proving copy-on-write).
//   - Read-only Share (optional): if a read-only Share volume is in the template,
//     assert the guest CANNOT write to it.
//
// It seeds the Snapshot source host-side AFTER the template is built by
// rebuilding the template backing with mkfs.ext4 -d, writing the seed file
// into a temporary directory first. This requires no mount, no loop device,
// and no root. Each fork's reflink copy then sees the seeded image. This
// binary is linux + KVM only; it is the gate for the volume CI phase.
//
// Every assertion gates: any failure exits nonzero so the CI step fails. A
// busybox/image pull flake (when building from an OCI image) is surfaced as
// PULL_FAILED so the CI loop can retry only that and never mask a real volume
// failure.
//
//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/volume"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func main() {
	image := flag.String("image", "", "rootfs path or OCI image reference to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary injected as /init (image builds only)")
	busyboxBin := flag.String("busybox-bin", "", "path to a static busybox injected as /bin/sh (image builds only)")
	freshMount := flag.String("fresh-mount", "/mnt/fresh", "guest mount path for the Fresh volume")
	snapMount := flag.String("snap-mount", "/mnt/snap", "guest mount path for the Snapshot volume")
	shareMount := flag.String("share-mount", "", "guest mount path for an optional read-only Share volume (empty to skip)")
	seedContent := flag.String("seed-content", "seed", "content written to /seeded.txt in the Snapshot seed image")
	flag.Parse()

	if *image == "" || *dataDir == "" || *kernel == "" {
		fmt.Fprintln(os.Stderr, "vol-smoke: --image, --data-dir and --kernel are required")
		os.Exit(2)
	}

	if err := run(opts{
		image:       *image,
		dataDir:     *dataDir,
		fcBin:       *fcBin,
		kernel:      *kernel,
		agentBin:    *agentBin,
		busyboxBin:  *busyboxBin,
		freshMount:  *freshMount,
		snapMount:   *snapMount,
		shareMount:  *shareMount,
		seedContent: *seedContent,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "vol-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("vol-smoke: PASS: Fresh round-trip and Snapshot CoW independence proven")
}

type opts struct {
	image, dataDir, fcBin, kernel, agentBin, busyboxBin string
	freshMount, snapMount, shareMount, seedContent      string
}

const (
	templateID = "vol-tmpl"
	freshName  = "fresh"
	snapName   = "snap"
	shareName  = "share"
	seedFile   = "/seeded.txt"
)

func run(o opts) error {
	engine, err := fork.NewEngine(o.dataDir, o.fcBin, o.kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AllowUnverified: true,
		AgentBinPath:    o.agentBin,
		BusyboxPath:     o.busyboxBin,
		EnableVolumes:   true,
	})
	if err != nil {
		return fmt.Errorf("new engine: %w", err)
	}

	// The template declares a Fresh and a Snapshot volume; optionally a read-only
	// Share volume. The Share spec sets ReadOnly so its baked drive is read-only.
	specs := []volume.Spec{
		{Name: freshName, SizeMB: 64, MountPath: o.freshMount, Policy: volume.ForkPolicyFresh},
		{Name: snapName, SizeMB: 64, MountPath: o.snapMount, Policy: volume.ForkPolicySnapshot},
	}
	if o.shareMount != "" {
		specs = append(specs, volume.Spec{Name: shareName, SizeMB: 64, MountPath: o.shareMount, ReadOnly: true, Policy: volume.ForkPolicyShare})
	}

	fmt.Printf("vol-smoke: building template %q from %q with %d volumes\n", templateID, o.image, len(specs))
	buildStart := time.Now()
	if err := engine.CreateTemplate(templateID, o.image, nil, specs, nil, nil, false); err != nil {
		if isPullFailure(err) {
			fmt.Println("PULL_FAILED")
		}
		return fmt.Errorf("create template: %w", err)
	}
	fmt.Printf("vol-smoke: template built in %s\n", time.Since(buildStart).Round(time.Millisecond))

	// Seed the Snapshot source image host-side, AFTER the build, so each fork's
	// reflink copy sees /seeded.txt. The seed path is deterministic; recompute it
	// via a backend rooted at the same data dir. The size must match the Snapshot
	// volume spec so the drive geometry is preserved for forks.
	be := volume.New(o.dataDir)
	snapSeed := be.TemplateVolumePath(templateID, snapName)
	snapSizeMB := specs[1].SizeMB // specs[1] is the Snapshot volume (snapName, 64 MB)
	seedHash, err := seedVolumeImage(snapSeed, seedFile, []byte(o.seedContent), snapSizeMB)
	if err != nil {
		return fmt.Errorf("seed snapshot source: %w", err)
	}
	fmt.Printf("vol-smoke: seeded %s in %s (sha256=%s...)\n", seedFile, snapSeed, seedHash[:12])

	// Fork two sandboxes.
	fmt.Println("vol-smoke: forking two sandboxes")
	res1, err := engine.Fork(templateID, "vol-fork-1", fork.ForkOpts{Volumes: specs})
	if err != nil {
		return fmt.Errorf("fork 1: %w", err)
	}
	defer func() { _ = engine.Terminate("vol-fork-1") }()
	res2, err := engine.Fork(templateID, "vol-fork-2", fork.ForkOpts{Volumes: specs})
	if err != nil {
		return fmt.Errorf("fork 2: %w", err)
	}
	defer func() { _ = engine.Terminate("vol-fork-2") }()

	// The guest agent mounts the volumes from the notify_forked mount table the
	// engine returns. The engine itself does NOT send notify_forked (that is the
	// daemon's job), so vol-smoke delivers the mount table directly to each fork.
	c1, err := connect(res1.VsockPath)
	if err != nil {
		return fmt.Errorf("connect fork1: %w", err)
	}
	defer c1.Close() //nolint:errcheck // best-effort
	c2, err := connect(res2.VsockPath)
	if err != nil {
		return fmt.Errorf("connect fork2: %w", err)
	}
	defer c2.Close() //nolint:errcheck // best-effort

	ctx := context.Background()
	if _, err := c1.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
		Generation:         1,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            freshEntropy(),
		Volumes:            toProtoVolumes(res1.VolumeMounts),
	}); err != nil {
		return fmt.Errorf("notify fork1 (mount table): %w", err)
	}
	if _, err := c2.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
		Generation:         2,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            freshEntropy(),
		Volumes:            toProtoVolumes(res2.VolumeMounts),
	}); err != nil {
		return fmt.Errorf("notify fork2 (mount table): %w", err)
	}
	fmt.Printf("vol-smoke: delivered mount tables (fork1=%d entries, fork2=%d entries)\n", len(res1.VolumeMounts), len(res2.VolumeMounts))

	// --- Fresh round-trip: write to fork1's Fresh volume and read it back. ---
	freshPath := o.freshMount + "/roundtrip.txt"
	if _, err := execOK(c1, fmt.Sprintf("echo fresh-data > %s", freshPath)); err != nil {
		return fmt.Errorf("fresh round-trip: write failed (Fresh volume not mounted writable?): %w", err)
	}
	out, err := execOK(c1, "cat "+freshPath)
	if err != nil {
		return fmt.Errorf("fresh round-trip: read back failed: %w", err)
	}
	if !strings.Contains(out, "fresh-data") {
		return fmt.Errorf("fresh round-trip: read %q, want it to contain fresh-data", strings.TrimSpace(out))
	}
	fmt.Println("vol-smoke: Fresh round-trip OK (wrote and read back on the Fresh volume)")

	// --- Snapshot CoW: both forks see the seed; writes diverge. ---
	seedInGuest := o.snapMount + seedFile
	out, err = execOK(c1, "cat "+seedInGuest)
	if err != nil {
		return fmt.Errorf("snapshot CoW: fork1 cannot read the seeded file (Snapshot source not reflinked?): %w", err)
	}
	if !strings.Contains(out, o.seedContent) {
		return fmt.Errorf("snapshot CoW: fork1 seeded file = %q, want it to contain %q", strings.TrimSpace(out), o.seedContent)
	}
	out, err = execOK(c2, "cat "+seedInGuest)
	if err != nil {
		return fmt.Errorf("snapshot CoW: fork2 cannot read the seeded file: %w", err)
	}
	if !strings.Contains(out, o.seedContent) {
		return fmt.Errorf("snapshot CoW: fork2 seeded file = %q, want it to contain %q", strings.TrimSpace(out), o.seedContent)
	}
	fmt.Println("vol-smoke: both forks see the seeded file on their Snapshot volume")

	// fork1 writes a fork-unique file; fork2 must NOT see it (CoW independence).
	forkUnique := o.snapMount + "/fork1.txt"
	if _, err := execOK(c1, fmt.Sprintf("echo fork1-only > %s && sync", forkUnique)); err != nil {
		return fmt.Errorf("snapshot CoW: fork1 write to its Snapshot volume failed: %w", err)
	}
	// A successful cat in fork2 would mean the backings are shared: a failure
	// (nonzero exit) is the expected, passing case.
	res, execErr := execRaw(c2, "cat "+forkUnique)
	if execErr == nil && res == 0 {
		return fmt.Errorf("snapshot CoW VIOLATED: fork2 sees fork1's write %q (backings are shared, not copy-on-write)", forkUnique)
	}
	fmt.Println("vol-smoke: Snapshot CoW independence OK (fork2 does not see fork1's write)")

	// The template seed image on the host must be byte-for-byte unchanged: the
	// fork's writes went to its own reflink copy, never the source.
	afterHash, err := fileSHA256(snapSeed)
	if err != nil {
		return fmt.Errorf("snapshot CoW: re-hash seed: %w", err)
	}
	if afterHash != seedHash {
		return fmt.Errorf("snapshot CoW VIOLATED: template seed image changed (before=%s... after=%s...): a fork wrote through to the source", seedHash[:12], afterHash[:12])
	}
	fmt.Println("vol-smoke: template seed image unchanged on the host (writes stayed in the per-fork copy)")

	// --- Optional read-only Share: the guest must NOT be able to write to it. ---
	if o.shareMount != "" {
		sharePath := o.shareMount + "/should-fail.txt"
		exitCode, execErr := execRaw(c1, fmt.Sprintf("echo nope > %s", sharePath))
		if execErr == nil && exitCode == 0 {
			return fmt.Errorf("read-only Share VIOLATED: a write to the Share volume at %s succeeded; it must be read-only", sharePath)
		}
		fmt.Println("vol-smoke: read-only Share OK (guest write to the Share volume was refused)")
	}

	return nil
}

// toProtoVolumes converts vsock VolumeMountEntry slice to proto VolumeMountEntry slice.
func toProtoVolumes(vols []vsock.VolumeMountEntry) []*internalv1.VolumeMountEntry {
	if len(vols) == 0 {
		return nil
	}
	out := make([]*internalv1.VolumeMountEntry, 0, len(vols))
	for _, v := range vols {
		out = append(out, &internalv1.VolumeMountEntry{
			Device:    v.Device,
			MountPath: v.MountPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	return out
}

// seedVolumeImage rebuilds the ext4 image at imagePath with content written to
// relPath, using mkfs.ext4 -d to populate the image from a temporary directory.
// This requires no mount, no loop device, and no elevated privileges, mirroring
// how ociroot/ext4.go builds rootfs images. sizeMB must match the volume size
// so the reflink copy and PATCH drive rebind see a correctly sized backing.
// Returns the image's sha256 AFTER seeding (the reference the CoW check
// compares against).
func seedVolumeImage(imagePath, relPath string, content []byte, sizeMB int) (string, error) {
	tmp, err := os.MkdirTemp("", "vol-seed-*")
	if err != nil {
		return "", fmt.Errorf("mkdir seed dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	// Write the seed file into the temp dir so mkfs.ext4 -d picks it up.
	seedRelPath := strings.TrimPrefix(relPath, "/")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, seedRelPath)), 0o755); err != nil {
		return "", fmt.Errorf("mkdir seed file parent: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, seedRelPath), content, 0o644); err != nil {
		return "", fmt.Errorf("write seed file: %w", err)
	}

	// Rebuild the backing image in-place: mkfs.ext4 -d populates the image
	// from tmp without mounting anything. -F forces overwrite of the existing
	// file. The size must match the original so the baked drive geometry is
	// preserved and forks can mount without errors.
	size := fmt.Sprintf("%dM", sizeMB)
	cmd := exec.Command("mkfs.ext4", "-F", "-q", "-d", tmp, imagePath, size) //nolint:gosec // argv built from validated spec and temp paths
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.ext4 -d %s %s %s: %w: %s", tmp, imagePath, size, err, string(out))
	}
	return fileSHA256(imagePath)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// freshEntropy returns 32 bytes for the notify_forked reseed. vol-smoke is a CI
// proof, not a security boundary, so a fixed pattern is fine; the guest only
// needs SOME entropy to exercise the reseed path before mounting volumes.
func freshEntropy() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func isPullFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "pull") || strings.Contains(s, "manifest") || strings.Contains(s, "registry") || strings.Contains(s, "timeout")
}

// execOK runs a command in the fork over the gRPC ExecStream RPC and returns its stdout,
// failing if the transport errors or the command exits nonzero.
func execOK(client *guestgrpc.Client, command string) (string, error) {
	ctx := context.Background()
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

// execRaw runs a command and returns the exit code. An error is returned only on
// transport failure; a nonzero exit code is returned as-is (not an error).
func execRaw(client *guestgrpc.Client, command string) (int32, error) {
	ctx := context.Background()
	stream, err := client.Sandbox.ExecStream(ctx, &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 30,
	})
	if err != nil {
		return 0, fmt.Errorf("exec stream: %w", err)
	}
	var exitCode int32
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("recv exec frame: %w", err)
		}
		if exit, ok := msg.Msg.(*sandboxv1.ExecResponse_Exit); ok {
			exitCode = exit.Exit.GetExitCode()
		}
	}
	return exitCode, nil
}

// connect dials the forked guest agent over vsock with a bounded retry while the
// restored VM finishes coming up.
func connect(udsPath string) (*guestgrpc.Client, error) {
	ctx := context.Background()
	return guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
}
