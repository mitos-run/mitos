package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client talks to a single Firecracker process via its Unix socket API.
type Client struct {
	socketPath string
	http       *http.Client
	process    *os.Process
	// workDir is the working directory the Firecracker process was
	// launched with (cmd.Dir). A relative vsock uds_path is bound by
	// Firecracker against this directory, so the host path of the vsock
	// socket is workDir/<uds_path>. Empty for ConnectVM clients.
	workDir string
	// wait reaps the launched process (exec.Cmd.Wait for StartVM
	// clients). Kill calls it after the kill signal so the process is
	// gone before its uid is released; nil for clients without a child
	// process (ConnectVM).
	wait func() error

	// Jailer state; zero values for direct exec.
	id          string // validated VM id (passed validateVMID), "" for ConnectVM
	chrootDir   string // host path of the chroot root, "" when not jailed
	jailerVMDir string // per-VM jailer workspace, removed on Kill
	dataDir     string // forkd data dir; bounds export-from-jail host paths
	jailedUID   uint32
	jailedGID   uint32
	allocator   *UIDAllocator
}

// launchEnv returns the process environment for a Firecracker launch: the
// inherited environment with any extra "KEY=VALUE" entries, where an extra
// OVERRIDES an inherited variable of the same name. nil extra (every stock
// launch) returns nil so exec uses the inherited environment unchanged
// (byte-for-byte the prior behavior); a non-empty extra is the live-cow fork
// path arming the parent (VMConfig.Env). Appending alone would not override,
// because exec resolves duplicate names by the FIRST occurrence, so a stray
// inherited FIRECRACKER_MITOS_* would win; drop colliding inherited keys first.
func launchEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	override := make(map[string]struct{}, len(extra))
	for _, kv := range extra {
		if i := strings.IndexByte(kv, '='); i > 0 {
			override[kv[:i]] = struct{}{}
		}
	}
	inherited := os.Environ()
	out := make([]string, 0, len(inherited)+len(extra))
	for _, kv := range inherited {
		i := strings.IndexByte(kv, '=')
		if i > 0 {
			if _, clash := override[kv[:i]]; clash {
				continue
			}
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// StartVM launches a Firecracker process and returns a client connected
// to it. With cfg.Jailer enabled the process is launched through the
// jailer binary inside a per-VM chroot under a dedicated uid/gid; with
// the zero JailerConfig the firecracker binary is exec'd directly,
// exactly as before.
func StartVM(cfg VMConfig) (*Client, error) {
	if cfg.Jailer.Enabled() {
		return startJailedVM(cfg)
	}

	// Validate the id before it is used anywhere, so the same allowlist
	// barrier that protects the jailed path builders also guards direct
	// exec. An empty id is allowed here (direct exec does not require one);
	// a non-empty id must pass the allowlist.
	if cfg.ID != "" {
		if err := validateVMID(cfg.ID); err != nil {
			return nil, err
		}
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(cfg.WorkDir, "firecracker.sock")
	}

	os.Remove(socketPath)
	// Firecracker binds the vsock host UDS at WorkDir/VsockRelPath and does NOT
	// unlink it on exit. A crashed or killed prior VM in this same WorkDir (the
	// template build dir is reused across build attempts; direct-exec forks
	// reuse a sandbox dir on retry) therefore leaves a stale socket file, and
	// the next launch fails its PUT /vsock with EADDRINUSE ("Address in use").
	// Remove it up front, symmetric with the API socket above, so a retry binds
	// cleanly instead of wedging the build/fork in a collision loop. Under the
	// jailer this path lives in the per-VM chroot and the collision cannot
	// occur; this guards the direct-exec path.
	os.Remove(filepath.Join(cfg.WorkDir, VsockRelPath))

	args := []string{
		"--api-sock", socketPath,
	}
	if cfg.ID != "" {
		args = append(args, "--id", cfg.ID)
	}

	// Fail closed if any path ever tried to disable the built-in seccomp filter
	// (issue #353). Firecracker installs its production filter unless
	// --no-seccomp is passed, so this keeps the VMM's second wall behind KVM
	// always on.
	if err := assertSeccompEnforced(args); err != nil {
		return nil, err
	}

	cmd := exec.Command(cfg.FirecrackerBin, args...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Append any live-fork env (FIRECRACKER_MITOS_*) on top of the inherited
	// environment. Empty for every stock launch, so this is a no-op unless the
	// live-cow fork path armed the parent (VMConfig.Env).
	cmd.Env = launchEnv(cfg.Env)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	client := &Client{
		socketPath: socketPath,
		process:    cmd.Process,
		wait:       cmd.Wait,
		workDir:    cfg.WorkDir,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := client.waitReady(readyTimeoutFor(5 * time.Second)); err != nil {
		_ = cmd.Process.Kill()
		// Reap the failed launch so it does not linger as a zombie (I2).
		_ = cmd.Wait()
		return nil, fmt.Errorf("firecracker not ready: %w", err)
	}

	return client, nil
}

// startJailedVM launches Firecracker through the jailer: it allocates a
// per-VM uid/gid, hard-links the configured files into the per-VM
// chroot, and waits for the API socket at its jailed location.
func startJailedVM(cfg VMConfig) (*Client, error) {
	// The Firecracker jailer scrubs its environment (clean_env_vars) before it
	// exec's the jailed VMM, so per-launch env cannot reach it on this path. The
	// live-cow fork therefore runs only on the direct-exec husk path (which sets no
	// JailerConfig). Fail closed BEFORE allocating any jail resources rather than
	// silently booting a stock VMM a caller believes is armed.
	if len(cfg.Env) > 0 {
		return nil, fmt.Errorf("firecracker: per-launch env (%d vars) is unsupported on the jailed path; the jailer scrubs its environment, so the live-cow fork is direct-exec only", len(cfg.Env))
	}
	if cfg.ID == "" {
		return nil, fmt.Errorf("jailer launch requires a VM id (jailer --id)")
	}
	// Allowlist barrier: the id is joined into every per-VM path below, so
	// validate it before ANY path is built from it (CodeQL go/path-injection
	// sanitizer). All downstream path builders consume this validated id.
	if err := validateVMID(cfg.ID); err != nil {
		return nil, err
	}
	id := cfg.ID
	if filepath.Base(cfg.FirecrackerBin) != jailerExecFileName {
		return nil, fmt.Errorf("jailer launch requires the firecracker binary to be named %q (the jailer derives the chroot layout from the --exec-file basename); got %q", jailerExecFileName, cfg.FirecrackerBin)
	}
	if cfg.Jailer.Allocator == nil {
		return nil, fmt.Errorf("jailer launch requires a uid allocator; construct the engine with a uid range")
	}
	// C1 defense in depth: refuse ids whose `..` segments would move the
	// per-VM directories outside the chroot base, before any allocation
	// or filesystem operation derived from the id.
	if err := guardJailerLayout(cfg); err != nil {
		return nil, err
	}

	uid, gid, err := cfg.Jailer.Allocator.Acquire()
	if err != nil {
		return nil, fmt.Errorf("allocate jailer uid for %s: %w", cfg.ID, err)
	}
	launched := false
	defer func() {
		if !launched {
			cfg.Jailer.Allocator.Release(uid)
		}
	}()

	chrootDir := jailerChrootDir(cfg.Jailer.ChrootBaseDir, id)

	// Before creating the fresh chroot tree, remove any stale per-vm dir left
	// by a prior aborted launch of this vm-id. The jailer creates
	// <vmDir>/root/ (the chroot root) and then mkdir <chroot>/old_root inside
	// it for pivot_root(2); a leftover old_root from a prior failed run causes
	// MkdirOldRoot(EEXIST) and the jailer refuses to start. Removing the entire
	// per-vm dir (strictly within ChrootBaseDir, verified by guardJailerLayout
	// above) starts fresh and lets the jailer succeed on a retry. Path only is
	// logged; no secrets are involved. Mirrors the control-socket os.Remove in
	// the direct-exec path.
	vmDir := jailerVMDir(cfg.Jailer.ChrootBaseDir, id)
	if _, statErr := os.Stat(vmDir); statErr == nil {
		fmt.Fprintf(os.Stderr, "firecracker: removing stale jailer dir %s before launch\n", vmDir)
		if err := os.RemoveAll(vmDir); err != nil {
			return nil, fmt.Errorf("remove stale jailer dir %s before launch: %w", vmDir, err)
		}
	}

	if err := os.MkdirAll(filepath.Join(chrootDir, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("create chroot run dir: %w", err)
	}
	if _, err := prepareChroot(cfg, id, cfg.ChrootFiles); err != nil {
		return nil, fmt.Errorf("prepare chroot for %s: %w", id, err)
	}
	chownIntoJail(chrootDir, cfg, id, uid, gid)

	socketPath := jailedAPISocketPath(cfg.Jailer.ChrootBaseDir, id)
	os.Remove(socketPath)

	jArgs := jailerArgs(cfg, id, uid, gid)
	// Fail closed if the firecracker portion of the jailer argv (after `--`)
	// ever disabled seccomp (issue #353): the jailed VMM is exactly the
	// post-escape process whose syscall surface seccomp bounds.
	if err := assertSeccompEnforced(jArgs); err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Jailer.JailerBin, jArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Jailed launches carry no per-launch env (guarded at entry: the jailer scrubs
	// its environment). nil keeps the inherited-env behavior byte-for-byte.
	cmd.Env = launchEnv(cfg.Env)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start jailer: %w", err)
	}

	client := &Client{
		socketPath:  socketPath,
		process:     cmd.Process,
		wait:        cmd.Wait,
		id:          id,
		chrootDir:   chrootDir,
		jailerVMDir: jailerVMDir(cfg.Jailer.ChrootBaseDir, id),
		dataDir:     cfg.Jailer.DataDir,
		jailedUID:   uid,
		jailedGID:   gid,
		allocator:   cfg.Jailer.Allocator,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := client.waitReady(readyTimeoutFor(10 * time.Second)); err != nil {
		_ = cmd.Process.Kill()
		// Reap before the deferred uid Release (I2): the uid must not be
		// reusable while the killed jailer can still run under it.
		_ = cmd.Wait()
		return nil, fmt.Errorf("jailed firecracker not ready: %w", err)
	}

	launched = true
	return client, nil
}

// chownIntoJail hands the prepared chroot files and the API socket dir
// to the jailed uid/gid so the deprivileged Firecracker can open them.
// Failures are logged (path only, never contents) and not fatal: on a
// correctly deployed root forkd they do not happen, and the VM fails
// later with a clear permission error if one slipped through.
func chownIntoJail(chrootDir string, cfg VMConfig, id string, uid, gid uint32) {
	targets := []string{filepath.Join(chrootDir, "run")}
	for _, f := range cfg.ChrootFiles {
		targets = append(targets, chrootPath(cfg.Jailer.ChrootBaseDir, id, f))
	}
	for _, t := range targets {
		if err := os.Chown(t, int(uid), int(gid)); err != nil {
			fmt.Fprintf(os.Stderr, "firecracker: chown %s to jailed uid %d failed: %v\n", t, uid, err)
		}
	}
}

// HostPath maps a path as Firecracker sees it over its API to the host
// location of the same file. For a jailed VM that is the mirrored path
// inside the chroot; for direct exec it is the path itself.
func (c *Client) HostPath(p string) string {
	if c.chrootDir == "" {
		return p
	}
	return filepath.Join(c.chrootDir, filepath.Clean(p))
}

// VsockHostPath returns the host path at which Firecracker binds the vsock
// UDS for a (relative) uds_path. Firecracker resolves a relative uds_path
// against its own working directory: in direct-exec mode that is the
// per-VM WorkDir (cmd.Dir), in jailer mode it is the chroot root after the
// jailer chdir's into it. Either way distinct VMs get distinct sockets,
// which is the whole point of baking a relative path into the snapshot.
func (c *Client) VsockHostPath(relUDSPath string) string {
	base := c.workDir
	if c.chrootDir != "" {
		base = c.chrootDir
	}
	return filepath.Join(base, relUDSPath)
}

// ConnectVM connects to an already-running Firecracker instance.
func ConnectVM(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

// readyTimeoutFor returns base, or an override from MITOS_FC_READY_TIMEOUT_SECONDS
// when set (and larger). The wait is a ceiling: a healthy Firecracker returns as
// soon as its API socket answers, so a larger value only gives a slow or
// resource-contended launch (co-located child VMs on a loaded KVM runner) more
// headroom without slowing the common path. Prod leaves the env unset (base wins).
func readyTimeoutFor(base time.Duration) time.Duration {
	if v := os.Getenv("MITOS_FC_READY_TIMEOUT_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			if d := time.Duration(secs) * time.Second; d > base {
				return d
			}
		}
	}
	return base
}

func (c *Client) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.socketPath); err == nil {
			if _, err := c.get("/"); err == nil {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", c.socketPath)
}

// --- VM Configuration ---

func (c *Client) SetBootSource(kernel string, bootArgs string) error {
	return c.put("/boot-source", BootSource{
		KernelImagePath: kernel,
		BootArgs:        bootArgs,
	})
}

// ValidateHugePages checks a guest-memory page-granularity value (issue #167).
// "" selects the Firecracker default (4 KiB base pages) and "2M" selects 2 MiB
// hugetlbfs-backed memory; any other value returns an actionable, LLM-legible
// error (issue #28) that names the bad value and the supported options, rather
// than letting Firecracker refuse it later with an opaque 400. It is shared by
// the client (request time) and the engine (construction time) so both reject
// the same set.
func ValidateHugePages(hugePages string) error {
	switch hugePages {
	case "", "2M":
		return nil
	default:
		return fmt.Errorf("invalid huge_pages %q: want \"\" (default 4KiB base pages) or \"2M\" (2MiB hugetlbfs-backed)", hugePages)
	}
}

// SetMachineConfig sets the guest vCPU count, memory size, and (issue #167)
// the guest-memory page granularity. hugePages is "" for the Firecracker default
// (4 KiB base pages) or "2M" for 2 MiB hugetlbfs-backed memory; any other value
// is rejected here with an actionable error rather than forwarded for Firecracker
// to refuse with an opaque 400 (issue #28).
func (c *Client) SetMachineConfig(vcpus int, memMB int, hugePages string) error {
	if err := ValidateHugePages(hugePages); err != nil {
		return err
	}
	return c.put("/machine-config", MachineConfig{
		VcpuCount:  vcpus,
		MemSizeMib: memMB,
		HugePages:  hugePages,
	})
}

func (c *Client) AddDrive(driveID string, path string, readOnly bool, rootDevice bool) error {
	return c.put("/drives/"+driveID, Drive{
		DriveID:      driveID,
		PathOnHost:   path,
		IsReadOnly:   readOnly,
		IsRootDevice: rootDevice,
	})
}

// PatchDrive rebinds an existing drive's backing file to pathOnHost via
// PATCH /drives/{drive_id}. Firecracker has long supported updating a drive's
// path_on_host on a configured VM, including one restored from a snapshot. It
// is how each fork gives its baked placeholder volume drive its OWN backing:
// the snapshot bakes the block device by driveID, and every fork PATCHes that
// driveID to the fork's prepared backing after the snapshot is loaded and
// resumed but before the guest mounts it. The drive id and host path carry no
// secrets and are safe to log.
func (c *Client) PatchDrive(driveID, pathOnHost string) error {
	return c.patch("/drives/"+driveID, DrivePatch{
		DriveID:    driveID,
		PathOnHost: pathOnHost,
	})
}

// SetNetwork attaches a guest NIC bound to a host tap device via
// PUT /network-interfaces/{ifaceID}. It must be called before InstanceStart
// (Firecracker does not support hot-plugging a NIC after boot). For a
// fresh-boot sandbox this gives the guest its egress device; for template
// creation it bakes a placeholder NIC into the snapshot that forks later
// remap with LoadSnapshotWithOverrides. The MAC and tap name are safe to log.
func (c *Client) SetNetwork(ifaceID, guestMAC, hostDevName string) error {
	return c.put("/network-interfaces/"+ifaceID, NetworkInterface{
		IfaceID:     ifaceID,
		GuestMAC:    guestMAC,
		HostDevName: hostDevName,
	})
}

func (c *Client) SetVsock(guestCID int, udsPath string) error {
	// For a jailed VM Firecracker binds the UDS inside its chroot; the
	// mirrored parent directory must exist and be writable by the jailed
	// uid before the API call.
	if err := c.ensureJailedDir(filepath.Dir(udsPath)); err != nil {
		return fmt.Errorf("prepare vsock dir in chroot: %w", err)
	}
	return c.put("/vsock", Vsock{
		GuestCID: guestCID,
		UdsPath:  udsPath,
	})
}

// SetEntropy attaches a virtio-rng device backed by the host RNG via
// PUT /entropy. It must be called before InstanceStart: Firecracker bakes its
// device model into the snapshot and cannot add a device on restore, so the
// device has to exist at build time. Once baked, every fork restores the device
// and the guest keeps a continuous host entropy source after the one-shot
// NotifyForked reseed (fork-correctness row 1). The device is attached
// unthrottled (no rate limiter); the request carries no secrets.
func (c *Client) SetEntropy() error {
	return c.put("/entropy", Entropy{})
}

// ensureJailedDir creates the in-chroot mirror of a host directory and
// hands it to the jailed uid. No-op for direct-exec clients.
func (c *Client) ensureJailedDir(hostDir string) error {
	if c.chrootDir == "" {
		return nil
	}
	dir := c.HostPath(hostDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, int(c.jailedUID), int(c.jailedGID)); err != nil {
		fmt.Fprintf(os.Stderr, "firecracker: chown %s to jailed uid %d failed: %v\n", dir, c.jailedUID, err)
	}
	return nil
}

// --- VM Lifecycle ---

func (c *Client) Start() error {
	return c.put("/actions", Action{ActionType: "InstanceStart"})
}

func (c *Client) Pause() error {
	return c.patch("/vm", VMState{State: "Paused"})
}

func (c *Client) Resume() error {
	return c.patch("/vm", VMState{State: "Resumed"})
}

// --- Snapshot Operations ---

// snapshotTypeFull is the stock Firecracker Full snapshot: it copies the whole
// guest RAM to mem_file_path AND writes the device/CPU vmstate to snapshot_path.
// snapshotTypeMitosVmstateOnly is the Mitos-patched vmstate-only capture: it
// writes ONLY the vmstate to snapshot_path and SKIPS the guest-memory copy (the
// ~364ms `mem` write measured on prod, issue #832), because on the live-cow fork
// path the guest RAM is already resident in the exported MAP_SHARED memfd a
// co-located child MAP_PRIVATEs.
const (
	snapshotTypeFull             = "Full"
	snapshotTypeMitosVmstateOnly = "MitosVmstateOnly"
)

func (c *Client) CreateSnapshot(memPath, snapshotPath string) error {
	// A jailed Firecracker writes both files inside its chroot; the
	// mirrored destination dirs must exist and be writable by the jailed
	// uid first, and the results are linked back out to the requested
	// host paths afterwards so callers see them where they asked.
	for _, p := range []string{memPath, snapshotPath} {
		if err := c.ensureJailedDir(filepath.Dir(p)); err != nil {
			return fmt.Errorf("prepare snapshot dir in chroot: %w", err)
		}
	}
	if err := c.put("/snapshot/create", SnapshotCreate{
		SnapshotType: snapshotTypeFull,
		SnapshotPath: snapshotPath,
		MemFilePath:  memPath,
	}); err != nil {
		return err
	}
	for _, p := range []string{memPath, snapshotPath} {
		if err := c.exportFromJail(p); err != nil {
			return fmt.Errorf("export snapshot file from chroot: %w", err)
		}
	}
	return nil
}

// CreateSnapshotVMStateOnly writes ONLY the device + CPU vmstate to snapshotPath
// and does NOT copy guest memory to a mem file. It is the live-cow fork capture:
// the running guest RAM is already resident in the MAP_SHARED memfd the patched
// parent Firecracker exports (m1), which a co-located fork child MAP_PRIVATEs, so
// the Full snapshot's guest-RAM copy (the ~364ms `mem` write, issue #832) is
// redundant. It issues PUT /snapshot/create with the Mitos vmstate-only type and
// NO mem_file_path (the omitempty tag drops the field), and exports only the small
// vmstate file back out of the jail.
//
// It REQUIRES the Mitos-patched Firecracker vmstate-only snapshot mode
// (mitos-run/firecracker): stock Firecracker rejects a /snapshot/create without a
// mem_file_path. The husk only calls it on the gated live-cow fork path (an armed
// write-protect parent handle present) and falls back to the Full CreateSnapshot
// otherwise, so a pod without the patch never reaches this call.
func (c *Client) CreateSnapshotVMStateOnly(snapshotPath string) error {
	if err := c.ensureJailedDir(filepath.Dir(snapshotPath)); err != nil {
		return fmt.Errorf("prepare snapshot dir in chroot: %w", err)
	}
	if err := c.put("/snapshot/create", SnapshotCreate{
		SnapshotType: snapshotTypeMitosVmstateOnly,
		SnapshotPath: snapshotPath,
	}); err != nil {
		return err
	}
	if err := c.exportFromJail(snapshotPath); err != nil {
		return fmt.Errorf("export snapshot file from chroot: %w", err)
	}
	return nil
}

// exportFromJail hard-links a file Firecracker produced inside the
// chroot back to its host path (copy on EXDEV). No-op for direct exec.
//
// The destination host path is bounded to the forkd data dir with a
// canonical containment check (filepath.Clean plus a separator-anchored
// prefix) before it reaches any os.* sink. This is the CodeQL-recognized
// sanitizer for the snapshot export flow (go/path-injection): the snapshot
// mem and vmstate paths originate from caller-supplied sandbox ids, and this
// barrier guarantees a cleaned path cannot escape the data dir.
func (c *Client) exportFromJail(hostPath string) error {
	if c.chrootDir == "" {
		return nil
	}
	if err := guardExportPath(hostPath, c.dataDir); err != nil {
		return err
	}
	src := c.HostPath(hostPath)
	if same, err := sameInode(src, hostPath); err == nil && same {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(src, hostPath); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		return copyFile(src, hostPath)
	}
	return nil
}

func (c *Client) LoadSnapshot(memPath, snapshotPath string, resumeVM bool) error {
	return c.LoadSnapshotWithOverrides(memPath, snapshotPath, resumeVM, nil)
}

// LoadSnapshotWithOverrides loads a snapshot like LoadSnapshot but additionally
// remaps the snapshot's network interfaces to fresh host taps via the
// network_overrides field (Firecracker >= v1.12; pinned CI is v1.15). This is
// how each fork of one shared snapshot binds its OWN tap: the snapshot bakes a
// placeholder NIC by iface_id, and every fork passes an override mapping that
// iface_id to the fork's freshly created tap. Passing nil overrides is
// identical to LoadSnapshot (the field is omitted, preserving prior behavior).
func (c *Client) LoadSnapshotWithOverrides(memPath, snapshotPath string, resumeVM bool, overrides []NetworkOverride) error {
	return c.put("/snapshot/load", SnapshotLoad{
		SnapshotPath:        snapshotPath,
		MemFilePath:         memPath,
		EnableDiffSnapshots: false,
		ResumeVM:            resumeVM,
		NetworkOverrides:    overrides,
	})
}

// LoadSnapshotUFFD loads a snapshot through the userfaultfd memory backend (issue
// #167): instead of a mem file path it points Firecracker at uffdSocketPath, a
// unix socket an external handler is already listening on. Firecracker connects
// to it during the load, creates the guest userfaultfd, and sends the handler the
// region mappings and the uffd descriptor. This path is REQUIRED to restore a
// hugetlbfs-backed snapshot (Firecracker refuses to file-map one) and is how a
// hot-page set is preloaded before resume. It always loads paused (resume_vm
// false): the engine preloads, then Resume()s. NetworkOverrides behave exactly as
// in LoadSnapshotWithOverrides.
func (c *Client) LoadSnapshotUFFD(snapshotPath, uffdSocketPath string, overrides []NetworkOverride) error {
	return c.put("/snapshot/load", SnapshotLoad{
		SnapshotPath:     snapshotPath,
		ResumeVM:         false,
		NetworkOverrides: overrides,
		MemBackend:       &MemBackend{BackendType: "Uffd", BackendPath: uffdSocketPath},
	})
}

// --- Process Management ---

func (c *Client) Kill() error {
	var killErr error
	if c.process != nil {
		killErr = c.process.Kill()
		// Reap the killed process BEFORE releasing its uid (I2): until
		// the wait returns, the process can still run under the jailed
		// uid, and releasing first could hand that uid to a new VM while
		// the old one lives. The wait error is ignored; it reports the
		// kill signal, and the zombie is reaped either way.
		if c.wait != nil {
			_ = c.wait()
		} else {
			_, _ = c.process.Wait()
		}
	}
	// Jailed VMs: return the dedicated uid to the pool and remove the
	// per-VM chroot workspace (hard links only; originals stay put).
	if c.allocator != nil {
		c.allocator.Release(c.jailedUID)
		c.allocator = nil
	}
	if c.jailerVMDir != "" {
		if err := os.RemoveAll(c.jailerVMDir); err != nil {
			fmt.Fprintf(os.Stderr, "firecracker: remove jailer dir %s: %v\n", c.jailerVMDir, err)
		}
	}
	return killErr
}

func (c *Client) PID() int {
	if c.process != nil {
		return c.process.Pid
	}
	return 0
}

// Ping reports whether the Firecracker VMM still answers its API socket (GET /
// InstanceInfo). It returns an error when the process is gone or unresponsive:
// a dead or defunct Firecracker refuses the socket connection, which is exactly
// the connection-refused a husk claim hits when the VMM died after the pod went
// Ready (issue #527). Reusing the API GET, rather than a kill -0 signal probe,
// is deliberate: a defunct (zombie) process still owns its pid until reaped, so a
// signal probe would call a dead VMM alive; the API socket does not lie. The
// path carries no secret. The underlying http.Client has a bounded timeout, so
// Ping cannot hang.
func (c *Client) Ping() error {
	if _, err := c.get("/"); err != nil {
		return fmt.Errorf("firecracker API socket %s unresponsive: %w", c.socketPath, err)
	}
	return nil
}

// JailerState exposes the per-VM jailer artifacts a crash-recovery journal must
// record so a restarted forkd can reap a dead VM's leaked chroot and return its
// jailed uid. The values are zero for a direct-exec (non-jailed) client. They
// carry host paths and a uid only, no secrets.
type JailerState struct {
	ChrootDir   string
	JailerVMDir string
	JailedUID   uint32
}

// JailerState returns this client's jailer artifacts (chroot root, per-VM
// jailer workspace, and dedicated uid). All zero for direct-exec clients.
func (c *Client) JailerState() JailerState {
	return JailerState{
		ChrootDir:   c.chrootDir,
		JailerVMDir: c.jailerVMDir,
		JailedUID:   c.jailedUID,
	}
}

// --- HTTP helpers ---

func (c *Client) put(path string, body interface{}) error {
	return c.do(http.MethodPut, path, body)
}

func (c *Client) patch(path string, body interface{}) error {
	return c.do(http.MethodPatch, path, body)
}

func (c *Client) get(path string) ([]byte, error) {
	resp, err := c.http.Get("http://localhost" + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, string(data))
	}
	return data, nil
}

func (c *Client) do(method, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return nil
}
