package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// All tests in this file run without KVM, root, or a jailer binary; they
// cover the pure launch helpers: argument shape, path layout, the uid
// allocator, the chroot file preparation, and its traversal guard.

func testJailerConfig(base, dataDir string) JailerConfig {
	return JailerConfig{
		JailerBin:     "/usr/local/bin/jailer",
		ChrootBaseDir: base,
		UIDRange:      [2]uint32{64000, 64002},
		DataDir:       dataDir,
	}
}

func TestJailerArgs(t *testing.T) {
	cfg := DefaultVMConfig()
	cfg.ID = "vm-1"
	cfg.FirecrackerBin = "/usr/local/bin/firecracker"
	cfg.Jailer = testJailerConfig("/srv/jailer", "/var/lib/agent-run")

	args := jailerArgs(cfg, 64000, 64000)
	want := []string{
		"--id", "vm-1",
		"--exec-file", "/usr/local/bin/firecracker",
		"--uid", "64000",
		"--gid", "64000",
		"--chroot-base-dir", "/srv/jailer",
		"--cgroup-version", "2",
		"--",
		"--api-sock", "/run/firecracker.socket",
	}
	if len(args) != len(want) {
		t.Fatalf("jailerArgs length = %d, want %d: %v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("jailerArgs[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}
}

func TestJailerArgsExplicitCgroupVersion(t *testing.T) {
	cfg := DefaultVMConfig()
	cfg.ID = "vm-1"
	cfg.Jailer = testJailerConfig("/srv/jailer", "/var/lib/agent-run")
	cfg.Jailer.CgroupVersion = 1

	args := jailerArgs(cfg, 64001, 64001)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cgroup-version 1") {
		t.Fatalf("expected --cgroup-version 1 in %v", args)
	}
}

func TestJailerPathLayout(t *testing.T) {
	chroot := jailerChrootDir("/srv/jailer", "vm-7")
	if chroot != "/srv/jailer/firecracker/vm-7/root" {
		t.Fatalf("jailerChrootDir = %q", chroot)
	}
	sock := jailedAPISocketPath("/srv/jailer", "vm-7")
	if sock != "/srv/jailer/firecracker/vm-7/root/run/firecracker.socket" {
		t.Fatalf("jailedAPISocketPath = %q", sock)
	}
	vmDir := jailerVMDir("/srv/jailer", "vm-7")
	if vmDir != "/srv/jailer/firecracker/vm-7" {
		t.Fatalf("jailerVMDir = %q", vmDir)
	}
}

func TestChrootPathMirrorsHostPath(t *testing.T) {
	got := chrootPath("/srv/jailer", "vm-7", "/var/lib/agent-run/templates/t1/snapshot/mem")
	want := "/srv/jailer/firecracker/vm-7/root/var/lib/agent-run/templates/t1/snapshot/mem"
	if got != want {
		t.Fatalf("chrootPath = %q, want %q", got, want)
	}
}

func TestUIDAllocatorAcquireReleaseReuse(t *testing.T) {
	a := NewUIDAllocator(64000, 64002)

	seen := map[uint32]bool{}
	for i := 0; i < 3; i++ {
		uid, gid, err := a.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if uid != gid {
			t.Fatalf("uid %d != gid %d", uid, gid)
		}
		if uid < 64000 || uid > 64002 {
			t.Fatalf("uid %d outside range", uid)
		}
		if seen[uid] {
			t.Fatalf("uid %d handed out twice", uid)
		}
		seen[uid] = true
	}

	// Range exhausted: typed error.
	_, _, err := a.Acquire()
	var exhausted *ErrUIDRangeExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected ErrUIDRangeExhausted, got %v", err)
	}
	if exhausted.Low != 64000 || exhausted.High != 64002 {
		t.Fatalf("exhaustion error range = %d-%d", exhausted.Low, exhausted.High)
	}

	// Release makes the uid acquirable again.
	a.Release(64001)
	uid, _, err := a.Acquire()
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if uid != 64001 {
		t.Fatalf("expected reuse of released uid 64001, got %d", uid)
	}
}

func TestUIDAllocatorConcurrent(t *testing.T) {
	a := NewUIDAllocator(64000, 64099)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[uint32]int{}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid, _, err := a.Acquire()
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			mu.Lock()
			seen[uid]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 100 {
		t.Fatalf("expected 100 distinct uids, got %d", len(seen))
	}
	for uid, n := range seen {
		if n != 1 {
			t.Fatalf("uid %d handed out %d times", uid, n)
		}
	}
}

func TestPrepareChrootHardLinksAndMaps(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	base := filepath.Join(root, "jail")
	src := filepath.Join(dataDir, "templates", "t1", "snapshot", "mem")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.ID = "vm-1"
	cfg.WorkDir = filepath.Join(dataDir, "sandboxes", "vm-1")
	cfg.Jailer = testJailerConfig(base, dataDir)

	mapping, err := prepareChroot(cfg, "vm-1", []string{src})
	if err != nil {
		t.Fatalf("prepareChroot: %v", err)
	}
	// The chroot mirrors the host layout, so the in-chroot API path is the
	// host path itself.
	if mapping[src] != src {
		t.Fatalf("mapping[%q] = %q, want identity", src, mapping[src])
	}

	linked := chrootPath(base, "vm-1", src)
	info, err := os.Stat(linked)
	if err != nil {
		t.Fatalf("linked file missing: %v", err)
	}
	srcInfo, _ := os.Stat(src)
	if !os.SameFile(info, srcInfo) {
		t.Fatalf("expected %q to be a hard link of %q", linked, src)
	}
}

func TestPrepareChrootIdempotent(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	src := filepath.Join(dataDir, "kernel")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	for i := 0; i < 2; i++ {
		if _, err := prepareChroot(cfg, "vm-1", []string{src}); err != nil {
			t.Fatalf("prepareChroot run %d: %v", i, err)
		}
	}
}

func TestPrepareChrootResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	target := filepath.Join(dataDir, "checkpoint", "mem")
	link := filepath.Join(dataDir, "templates", "t1-live", "snapshot", "mem")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte("mem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	mapping, err := prepareChroot(cfg, "vm-1", []string{link})
	if err != nil {
		t.Fatalf("prepareChroot: %v", err)
	}
	if mapping[link] != link {
		t.Fatalf("mapping[%q] = %q, want identity", link, mapping[link])
	}
	// The linked file must be a hard link to the symlink TARGET so it
	// resolves inside the chroot, where the symlink target path does not
	// exist.
	linked := chrootPath(cfg.Jailer.ChrootBaseDir, "vm-1", link)
	info, err := os.Lstat(linked)
	if err != nil {
		t.Fatalf("linked file missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%q is a symlink; want a hard link to the resolved target", linked)
	}
	targetInfo, _ := os.Stat(target)
	if !os.SameFile(info, targetInfo) {
		t.Fatalf("expected %q to share an inode with %q", linked, target)
	}
}

func TestPrepareChrootRefusesPathsOutsideRoots(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.WorkDir = filepath.Join(dataDir, "sandboxes", "vm-1")
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	cases := []string{
		outside,
		filepath.Join(dataDir, "..", "outside.txt"), // traversal out of the data dir
		"relative/path", // not absolute
		filepath.Join(dataDir, "../../../etc/passwd"), // deep traversal
	}
	for _, p := range cases {
		if _, err := prepareChroot(cfg, "vm-1", []string{p}); err == nil {
			t.Fatalf("prepareChroot accepted %q; want refusal", p)
		}
	}
}

func TestPrepareChrootRefusesSymlinkEscapingRoots(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dataDir, "innocent")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	if _, err := prepareChroot(cfg, "vm-1", []string{link}); err == nil {
		t.Fatal("prepareChroot followed a symlink out of the data dir; want refusal")
	}
}

func TestPrepareChrootEXDEVFallsBackToCopy(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	src := filepath.Join(dataDir, "rootfs.ext4")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("rootfs-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	// Simulate a chroot base on a different filesystem.
	exdev := func(string, string) error { return syscall.EXDEV }
	mapping, err := prepareChrootWithLink(cfg, "vm-1", []string{src}, exdev)
	if err != nil {
		t.Fatalf("prepareChrootWithLink with EXDEV stub: %v", err)
	}
	if mapping[src] != src {
		t.Fatalf("mapping[%q] = %q, want identity", src, mapping[src])
	}
	copied := chrootPath(cfg.Jailer.ChrootBaseDir, "vm-1", src)
	data, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("copy fallback did not produce %q: %v", copied, err)
	}
	if string(data) != "rootfs-bytes" {
		t.Fatalf("copy fallback produced wrong content")
	}
}

func TestPrepareChrootPropagatesNonEXDEVLinkErrors(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	src := filepath.Join(dataDir, "mem")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	boom := errors.New("disk on fire")
	fail := func(string, string) error { return boom }
	if _, err := prepareChrootWithLink(cfg, "vm-1", []string{src}, fail); !errors.Is(err, boom) {
		t.Fatalf("expected link error to propagate, got %v", err)
	}
}

func TestJailerConfigEnabled(t *testing.T) {
	var j JailerConfig
	if j.Enabled() {
		t.Fatal("zero JailerConfig must be disabled")
	}
	j.JailerBin = "/usr/local/bin/jailer"
	if !j.Enabled() {
		t.Fatal("JailerConfig with JailerBin set must be enabled")
	}
}

func TestStartVMJailedFailsClosedOnMisconfiguration(t *testing.T) {
	base := DefaultVMConfig()
	base.Jailer = testJailerConfig(t.TempDir(), t.TempDir())
	base.Jailer.Allocator = NewUIDAllocator(64000, 64002)
	base.ID = "vm-1"

	t.Run("missing id", func(t *testing.T) {
		cfg := base
		cfg.ID = ""
		if _, err := StartVM(cfg); err == nil {
			t.Fatal("StartVM accepted a jailed VM without an id")
		}
	})

	t.Run("exec file basename", func(t *testing.T) {
		cfg := base
		cfg.FirecrackerBin = "/usr/local/bin/firecracker-v1.15"
		if _, err := StartVM(cfg); err == nil {
			t.Fatal("StartVM accepted an exec file whose basename breaks the chroot layout")
		}
	})

	t.Run("missing allocator", func(t *testing.T) {
		cfg := base
		cfg.Jailer.Allocator = nil
		if _, err := StartVM(cfg); err == nil {
			t.Fatal("StartVM accepted a jailed VM without a uid allocator")
		}
	})
}

func TestStartVMJailedReleasesUIDOnLaunchFailure(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultVMConfig()
	cfg.ID = "vm-1"
	cfg.WorkDir = filepath.Join(root, "data", "sandboxes", "vm-1")
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), filepath.Join(root, "data"))
	cfg.Jailer.JailerBin = filepath.Join(root, "no-such-jailer")
	cfg.Jailer.UIDRange = [2]uint32{64000, 64000}
	cfg.Jailer.Allocator = NewUIDAllocator(64000, 64000)

	if _, err := StartVM(cfg); err == nil {
		t.Fatal("StartVM succeeded with a nonexistent jailer binary")
	}
	// The single uid must have been released on failure.
	uid, _, err := cfg.Jailer.Allocator.Acquire()
	if err != nil {
		t.Fatalf("uid leaked by failed StartVM: %v", err)
	}
	if uid != 64000 {
		t.Fatalf("unexpected uid %d", uid)
	}
}

func TestClientHostPath(t *testing.T) {
	direct := &Client{}
	if got := direct.HostPath("/var/lib/agent-run/sandboxes/s1/vsock.sock"); got != "/var/lib/agent-run/sandboxes/s1/vsock.sock" {
		t.Fatalf("direct HostPath = %q", got)
	}
	jailed := &Client{chrootDir: "/srv/jailer/firecracker/vm-1/root"}
	want := "/srv/jailer/firecracker/vm-1/root/var/lib/agent-run/sandboxes/s1/vsock.sock"
	if got := jailed.HostPath("/var/lib/agent-run/sandboxes/s1/vsock.sock"); got != want {
		t.Fatalf("jailed HostPath = %q, want %q", got, want)
	}
}
