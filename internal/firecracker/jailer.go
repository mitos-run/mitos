package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// This file holds the pure jailer launch helpers: argument construction,
// chroot path layout, chroot file preparation, and the per-VM uid/gid
// allocator. Everything here is unit-testable without KVM, root, or a
// jailer binary; the process launch itself lives in client.go.

// jailerExecFileName is the basename the firecracker binary must carry.
// The jailer derives the first chroot path element from the basename of
// --exec-file, so the launch path requires the binary to be installed
// under this exact name and fails closed otherwise (startJailedVM).
const jailerExecFileName = "firecracker"

// jailedAPISocketRelPath is the API socket path as Firecracker sees it
// inside the chroot. The jailer creates <chroot>/run and Firecracker
// binds the socket there.
const jailedAPISocketRelPath = "/run/firecracker.socket"

// JailerConfig configures launching Firecracker through the jailer
// binary. The zero value disables the jailer: StartVM execs the
// firecracker binary directly, exactly as before.
type JailerConfig struct {
	// JailerBin is the path to the jailer binary. Empty disables the
	// jailer (direct exec; development only, flagged in the threat
	// model).
	JailerBin string
	// ChrootBaseDir is the jailer --chroot-base-dir. It must live on the
	// same filesystem as DataDir so snapshot, kernel, and rootfs files
	// can be hard-linked into each VM's chroot.
	ChrootBaseDir string
	// UIDRange is the inclusive [low, high] uid/gid range from which each
	// VM gets a dedicated uid (gid is the same value).
	UIDRange [2]uint32
	// CgroupVersion selects the jailer --cgroup-version; 0 means 2.
	CgroupVersion int
	// DataDir is the forkd data directory. Together with the per-VM
	// WorkDir it bounds which host files prepareChroot may expose inside
	// a chroot.
	DataDir string
	// Allocator hands out per-VM uids from UIDRange. It must be shared
	// across all VMs of one process; NewEngine sets it up.
	Allocator *UIDAllocator
}

// Enabled reports whether VMs are launched through the jailer.
func (j JailerConfig) Enabled() bool { return j.JailerBin != "" }

// jailerChrootDir returns the host directory that becomes the jailed
// Firecracker's root filesystem. This function is the single place that
// encodes the jailer's on-disk layout:
//
//	<chroot-base>/<basename of --exec-file>/<vm-id>/root
//
// We require the exec file basename to be jailerExecFileName, so the
// chroot root is <chroot-base>/firecracker/<vm-id>/root and the API
// socket requested at /run/firecracker.socket appears on the host at
// <chroot-base>/firecracker/<vm-id>/root/run/firecracker.socket.
func jailerChrootDir(baseDir, vmID string) string {
	return filepath.Join(baseDir, jailerExecFileName, vmID, "root")
}

// jailerVMDir is the per-VM jailer workspace (the parent of the chroot
// root); it is removed when the VM is killed.
func jailerVMDir(baseDir, vmID string) string {
	return filepath.Join(baseDir, jailerExecFileName, vmID)
}

// jailedAPISocketPath returns the host path of the jailed VM's API socket.
func jailedAPISocketPath(baseDir, vmID string) string {
	return filepath.Join(jailerChrootDir(baseDir, vmID), jailedAPISocketRelPath)
}

// chrootPath maps a host path to the host location of the same path
// inside the VM's chroot. The chroot mirrors the host layout: host file
// /a/b/c is materialized at <chroot>/a/b/c, so the path Firecracker is
// given over its API is byte-identical to the host path. That makes path
// translation the identity for prepared files, keeps differently-rooted
// files collision-free, and lets drive and vsock paths embedded in a
// snapshot resolve in any later chroot that prepares the same files.
func chrootPath(baseDir, vmID, p string) string {
	return filepath.Join(jailerChrootDir(baseDir, vmID), filepath.Clean(p))
}

// jailerArgs builds the jailer argv (excluding the jailer binary itself)
// for one VM. Everything after "--" is passed through to Firecracker,
// which resolves paths inside the chroot. The jailer itself appends
// --id to the Firecracker args.
func jailerArgs(cfg VMConfig, uid, gid uint32) []string {
	cgroupVersion := cfg.Jailer.CgroupVersion
	if cgroupVersion == 0 {
		cgroupVersion = 2
	}
	return []string{
		"--id", cfg.ID,
		"--exec-file", cfg.FirecrackerBin,
		"--uid", strconv.FormatUint(uint64(uid), 10),
		"--gid", strconv.FormatUint(uint64(gid), 10),
		"--chroot-base-dir", cfg.Jailer.ChrootBaseDir,
		"--cgroup-version", strconv.Itoa(cgroupVersion),
		"--",
		"--api-sock", jailedAPISocketRelPath,
	}
}

// prepareChroot hard-links each file into the VM's chroot at its mirrored
// location and returns the host-path to in-chroot-path mapping (identity
// under the mirror layout; see chrootPath). Symlinks are resolved first
// so the chroot receives the real inode. Hard linking across filesystems
// fails with EXDEV; prepareChroot then falls back to a full copy and logs
// a warning naming the file path and size, never its contents. Paths
// outside the VM workspace and the data dir are refused.
func prepareChroot(cfg VMConfig, vmID string, files []string) (map[string]string, error) {
	return prepareChrootWithLink(cfg, vmID, files, os.Link)
}

// prepareChrootWithLink is prepareChroot with an injectable link function
// so the EXDEV fallback is unit-testable on a single filesystem.
func prepareChrootWithLink(cfg VMConfig, vmID string, files []string, link func(oldname, newname string) error) (map[string]string, error) {
	mapping := make(map[string]string, len(files))
	for _, f := range files {
		if err := guardChrootSource(cfg, f); err != nil {
			return nil, err
		}
		resolved, err := filepath.EvalSymlinks(f)
		if err != nil {
			return nil, fmt.Errorf("resolve %s for chroot: %w", f, err)
		}
		if err := guardChrootSource(cfg, resolved); err != nil {
			return nil, err
		}

		dst := chrootPath(cfg.Jailer.ChrootBaseDir, vmID, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("create chroot dir for %s: %w", f, err)
		}
		if same, err := sameInode(resolved, dst); err == nil && same {
			// Already prepared (idempotent re-prepare).
			mapping[f] = f
			continue
		}
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("replace stale chroot file %s: %w", dst, err)
		}
		if err := link(resolved, dst); err != nil {
			if !errors.Is(err, syscall.EXDEV) {
				return nil, fmt.Errorf("hard link %s into chroot: %w", f, err)
			}
			info, statErr := os.Stat(resolved)
			size := int64(-1)
			if statErr == nil {
				size = info.Size()
			}
			fmt.Fprintf(os.Stderr, "firecracker: hard link of %s into the chroot crossed filesystems (EXDEV); copying %d bytes instead. Co-locate --chroot-base and --data-dir on one filesystem to keep forks CoW-cheap.\n", f, size)
			if err := copyFile(resolved, dst); err != nil {
				return nil, fmt.Errorf("copy %s into chroot after EXDEV: %w", f, err)
			}
		}
		mapping[f] = f
	}
	return mapping, nil
}

func sameInode(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// guardChrootSource refuses any path that is not absolute or escapes both
// the VM workspace and the data dir. It is applied to the requested path
// and again to its symlink-resolved form.
func guardChrootSource(cfg VMConfig, p string) error {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("refusing relative path %q for the jailer chroot; pass absolute paths", p)
	}
	for _, root := range []string{cfg.WorkDir, cfg.Jailer.DataDir} {
		if root == "" {
			continue
		}
		roots := []string{filepath.Clean(root)}
		// Compare against the symlink-resolved root as well, so a root
		// reached through a benign directory symlink (such as /var on
		// darwin) still contains its own resolved children.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, filepath.Clean(resolved))
		}
		for _, r := range roots {
			if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
				return nil
			}
		}
	}
	return fmt.Errorf("refusing to expose %q inside the jailer chroot: outside the VM workspace (%q) and the data dir (%q); place VM artifacts under the data dir", p, cfg.WorkDir, cfg.Jailer.DataDir)
}

// ErrUIDRangeExhausted is returned by UIDAllocator.Acquire when every uid
// in the configured range is in use by a live VM.
type ErrUIDRangeExhausted struct {
	Low, High uint32
}

func (e *ErrUIDRangeExhausted) Error() string {
	return fmt.Sprintf("jailer uid range %d-%d exhausted; terminate sandboxes or widen --uid-range", e.Low, e.High)
}

// UIDAllocator hands out dedicated uid/gid pairs for jailed VMs from an
// inclusive range, round-robin over the free set. It is safe for
// concurrent use. The gid always equals the uid.
type UIDAllocator struct {
	mu    sync.Mutex
	low   uint32
	high  uint32
	next  uint32
	inUse map[uint32]bool
}

// NewUIDAllocator builds an allocator over the inclusive range
// [low, high]. low must be <= high; the caller validates the range.
func NewUIDAllocator(low, high uint32) *UIDAllocator {
	return &UIDAllocator{low: low, high: high, inUse: make(map[uint32]bool)}
}

// Acquire reserves and returns a uid and gid (the same value) for one VM.
func (a *UIDAllocator) Acquire() (uid, gid uint32, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.high - a.low + 1
	for i := uint32(0); i < n; i++ {
		candidate := a.low + (a.next+i)%n
		if a.inUse[candidate] {
			continue
		}
		a.inUse[candidate] = true
		a.next = (a.next + i + 1) % n
		return candidate, candidate, nil
	}
	return 0, 0, &ErrUIDRangeExhausted{Low: a.low, High: a.high}
}

// Release returns a uid to the pool. Releasing an unallocated uid is a
// no-op.
func (a *UIDAllocator) Release(uid uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, uid)
}
