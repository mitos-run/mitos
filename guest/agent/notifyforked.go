//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"github.com/paperclipinc/mitos/internal/guestnet"
	"github.com/paperclipinc/mitos/internal/vsock"
	"golang.org/x/sys/unix"
)

// clockStepThresholdNanos is the drift past which the guest steps
// CLOCK_REALTIME. A restored guest's wall clock is frozen at snapshot time, so
// after a fork the drift is typically large; small drifts within this window
// are left alone to avoid fighting any in-guest NTP discipline.
const clockStepThresholdNanos = 500 * 1000 * 1000 // 500ms

// handleNotifyForked repairs fork-shared state after a restore:
//  1. reseed the kernel CRNG with the host-supplied entropy,
//  2. step CLOCK_REALTIME toward host wall time when drift is large,
//  3. record the fork generation at /run/sandbox/fork-generation,
//  4. signal userspace processes (SIGUSR2) to reseed their own PRNGs.
//
// Entropy bytes and the absolute clock value are never logged; only counts and
// the applied step magnitude are.
func handleNotifyForked(req *vsock.NotifyForkedRequest) vsock.Response {
	reseeded := reseedCRNG(req.Entropy)

	step := stepClock(req.HostWallClockNanos)

	writeForkGeneration(req.Generation)

	configureNetwork(req.Network)

	mounted := mountVolumes(req.Volumes)

	signaled := signalUserspace()

	fmt.Printf("sandbox-agent: notify_forked generation=%d entropy_bytes=%d reseeded=%v clock_step_ns=%d volumes_mounted=%d signaled=%d\n",
		req.Generation, len(req.Entropy), reseeded, step, mounted, signaled)

	return vsock.Response{
		OK: true,
		NotifyForked: &vsock.NotifyForkedResponse{
			AppliedClockStepNanos: step,
			ReseededRNG:           reseeded,
			SignaledProcesses:     signaled,
		},
	}
}

// rndAddEntropy mirrors the kernel's `struct rand_pool_info`:
//
//	struct rand_pool_info {
//	    int entropy_count;  // entropy credited, in bits
//	    int buf_size;       // length of buf in bytes
//	    __u32 buf[0];       // the entropy itself
//	};
//
// We build it as a packed little-endian byte slice and pass a pointer to the
// RNDADDENTROPY ioctl. unix.RNDADDENTROPY is an architecture-specific constant
// (0x40085203 on amd64/arm64); we use the package constant rather than a
// hardcoded request number so cross-arch builds stay correct.
func reseedCRNG(entropy []byte) bool {
	return reseedCRNGAt(entropy, "/dev/urandom")
}

// reseedCRNGAt credits the kernel CRNG at path (production: /dev/urandom) with
// the host-supplied entropy via RNDADDENTROPY, which adds the bytes AND credits
// the entropy count. It reports success ONLY when that credited ioctl succeeds.
//
// FAIL CLOSED: if RNDADDENTROPY fails it returns false rather than reporting
// success. The previous behavior fell back to a plain write to /dev/urandom and
// still returned true, but an uncredited write mixes the bytes into the input
// pool WITHOUT crediting entropy and does NOT guarantee the CRNG output diverges
// from a sibling fork that restored the same snapshot. The host fork-correctness
// gate keys entirely on this boolean, so over-reporting here silently defeated
// it: a fork that could not be credibly reseeded would be served sharing its
// siblings' CRNG output (duplicate keys/tokens/nonces). Returning false makes
// the host reap such a fork. The guest agent runs as PID 1 with full
// capabilities on our shipped kernel, where RNDADDENTROPY is available, so the
// credited path is the normal path; the fallback was the unsafe one.
func reseedCRNGAt(entropy []byte, path string) bool {
	if len(entropy) == 0 {
		return false
	}

	// header: entropy_count (bits) + buf_size (bytes), both int32, then bytes.
	buf := make([]byte, 8+len(entropy))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(entropy)*8))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(entropy)))
	copy(buf[8:], entropy)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: open %s: %v\n", path, err)
		return false
	}
	defer f.Close()

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(unix.RNDADDENTROPY),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if errno == 0 {
		return true
	}
	fmt.Fprintf(os.Stderr, "sandbox-agent: RNDADDENTROPY failed (errno %d); reseed NOT credited, reporting failure so the host reaps this fork\n", int(errno))
	return false
}

// stepClock reads CLOCK_REALTIME, compares it to the host wall clock delivered
// in the notification, and steps the clock when drift exceeds the threshold.
// Returns the signed adjustment applied in nanoseconds (0 when within
// tolerance or on error). The absolute clock value is never logged.
//
// CLOCK_MONOTONIC is deliberately NOT touched here, and cannot be: Linux rejects
// clock_settime(CLOCK_MONOTONIC) with EINVAL, so a literal monotonic step is
// impossible. It is also not needed for a clean restore: the VM is PAUSED across
// snapshot/restore, so CLOCK_MONOTONIC resumes continuously from its snapshot
// value (it does not jump by the wall-time gap), and a monotonic-anchored timer
// simply continues counting rather than mis-firing. The residual hazard is
// narrow: userspace code that derived a monotonic deadline from a wall-clock
// baseline (mixing the two clocks) can be off after the wall step above. The
// signalUserspace SIGUSR2 below is the reset signal for exactly that case: a
// runtime that pinned a deadline to old wall time re-derives it on the signal.
// This residual is documented in docs/fork-correctness.md; there is no correct
// monotonic step to apply.
func stepClock(hostWallClockNanos int64) int64 {
	if hostWallClockNanos == 0 {
		return 0
	}

	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: clock_gettime: %v\n", err)
		return 0
	}
	guestNanos := ts.Nano()
	drift := hostWallClockNanos - guestNanos
	if drift < 0 {
		if -drift <= clockStepThresholdNanos {
			return 0
		}
	} else if drift <= clockStepThresholdNanos {
		return 0
	}

	target := unix.NsecToTimespec(hostWallClockNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &target); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: clock_settime: %v\n", err)
		return 0
	}
	return drift
}

// writeForkGeneration records the fork generation at a fixed path so
// inotify-watching runtimes can detect a fork without a signal. Best effort.
func writeForkGeneration(generation uint64) {
	if err := os.MkdirAll("/run/sandbox", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: mkdir /run/sandbox: %v\n", err)
		return
	}
	data := []byte(strconv.FormatUint(generation, 10))
	if err := os.WriteFile("/run/sandbox/fork-generation", data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write fork-generation: %v\n", err)
	}
}

// guestNetIface is the guest-side NIC name. The snapshot bakes one NIC
// (firecracker.NetIfaceID = "eth0" on the host side); inside the guest the
// kernel names the single virtio-net device eth0.
const guestNetIface = "eth0"

// configureNetwork applies the per-fork eth0 address and default route after a
// restore. Every fork restores the same snapshot-baked guest IP, so without
// re-addressing here all forks would share one guest IP and the host could not
// route return traffic per fork. The address is flushed first so a re-fork or
// re-delivery is idempotent. This goes through rtnetlink (internal/guestnet),
// not an `ip` binary: templates are built from arbitrary user OCI images that
// may ship no iproute2, so shelling out cannot be relied on. Best effort: a
// failure logs (addresses only, no secrets) and leaves the guest without egress,
// which fails closed. No-op when the host did not deliver a network config.
func configureNetwork(cfg *vsock.NotifyForkedNetwork) {
	if cfg == nil {
		return
	}
	addr := fmt.Sprintf("%s/%d", cfg.GuestIP, cfg.PrefixLen)
	if err := guestnet.Configure(guestNetIface, cfg.GuestIP, cfg.GatewayIP, cfg.PrefixLen); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: net config failed: %v\n", err)
	}
	// Point the guest at the controlled resolver so every name lookup goes
	// through the proxy that enforces the name allowlist. Only when the host
	// delivered a resolver IP (DNS egress on); otherwise resolv.conf is left
	// untouched. This runs before the guest resolves anything (the agent
	// applies it on the post-restore notification, ahead of exec traffic).
	if err := writeResolvConf(resolvConfPath, cfg.ResolverIP); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write resolv.conf: %v\n", err)
	}
	fmt.Printf("sandbox-agent: configured %s addr=%s gateway=%s resolver=%s\n", guestNetIface, addr, cfg.GatewayIP, cfg.ResolverIP)
}

// resolvConfPath is the guest resolver configuration file. It is a package var
// so the write is unit-testable against a temp path.
const resolvConfPath = "/etc/resolv.conf"

// writeResolvConf points the guest's resolver at resolverIP by writing a single
// `nameserver <resolverIP>` line, so every name lookup goes through the
// controlled DNS proxy (the only address the egress chain allows on port 53).
// The write replaces the file in full, so it is idempotent: re-delivery of the
// same resolver yields identical content rather than appended lines. An empty
// resolverIP is a no-op so the feature-off path never clobbers an existing
// resolv.conf. The address is config, not a secret.
func writeResolvConf(path, resolverIP string) error {
	if resolverIP == "" {
		return nil
	}
	content := fmt.Sprintf("nameserver %s\n", resolverIP)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// volumeFSType is the filesystem the host formats every volume backing with.
// Fresh and Snapshot volumes are ext4 images; Share/Clone copy an ext4 seed.
const volumeFSType = "ext4"

// mountVolumes mounts each volume in the post-restore mount table. For every
// entry it mkdir -p's the mount path and mounts the device read-write, or
// read-only (MS_RDONLY) when ReadOnly is set so a shared or read-only volume
// cannot be written from the guest. The host has already rebound each baked
// placeholder drive to this fork's backing before sending the table, so the
// device is in place. It is idempotent: an already-mounted path is skipped
// (a re-delivered notification does not double-mount). Best effort per entry:
// a failure is logged (device and path only, no secrets) and the others still
// mount. Returns the count of devices now mounted at their path.
func mountVolumes(entries []vsock.VolumeMountEntry) int {
	mounted := 0
	for _, e := range entries {
		if e.Device == "" || e.MountPath == "" {
			fmt.Fprintf(os.Stderr, "sandbox-agent: skipping volume with empty device/path: device=%q path=%q\n", e.Device, e.MountPath)
			continue
		}
		if isMounted(e.MountPath) {
			mounted++
			continue
		}
		if err := os.MkdirAll(e.MountPath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: mkdir mount path %s: %v\n", e.MountPath, err)
			continue
		}
		var flags uintptr
		if e.ReadOnly {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount(e.Device, e.MountPath, volumeFSType, flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: mount %s at %s (ro=%v): %v\n", e.Device, e.MountPath, e.ReadOnly, err)
			continue
		}
		mounted++
	}
	if len(entries) > 0 {
		fmt.Printf("sandbox-agent: mounted %d/%d volumes\n", mounted, len(entries))
	}
	return mounted
}

// isMounted reports whether mountPath is already a mount point by scanning
// /proc/mounts (field 2 is the mount target). It makes mountVolumes idempotent
// across a re-delivered fork notification. On any read error it returns false so
// the caller attempts the mount (a redundant mount fails loudly rather than
// silently skipping).
func isMounted(mountPath string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[1] == mountPath {
			return true
		}
	}
	return false
}

// signalUserspace sends SIGUSR2 to every userspace process except PID 1 (this
// init) and the agent itself, prompting language runtimes and TLS libraries to
// reseed their PRNGs. Best effort: failures per pid are ignored and the count
// of successful signals is returned.
func signalUserspace() int {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: read /proc: %v\n", err)
		return 0
	}

	signaled := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a numeric pid entry
		}
		if pid == 1 || pid == self {
			continue
		}
		if err := unix.Kill(pid, unix.SIGUSR2); err == nil {
			signaled++
		}
	}
	return signaled
}
