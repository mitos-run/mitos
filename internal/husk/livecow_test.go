package husk

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
)

// fakeChildImportProvider stands in for the armed parent-side WP handler in the
// child-import env unit tests: it returns a fixed import (or an error) and records
// the dir the child bitmap was requested under.
type fakeChildImportProvider struct {
	imp    fork.ChildMemfdImport
	err    error
	gotDir string
}

func (f *fakeChildImportProvider) ChildImport(dir string) (fork.ChildMemfdImport, error) {
	f.gotDir = dir
	if f.err != nil {
		return fork.ChildMemfdImport{}, f.err
	}
	return f.imp, nil
}

// TestLiveCowForkGateDefaultOff proves the flag defaults off and the gate keeps
// the co-located fork on the disk path unless the pod opts in, so a deployment
// that leaves --live-cow-fork off is byte-for-byte the current behavior.
func TestLiveCowForkGateDefaultOff(t *testing.T) {
	off := New(firecracker.VMConfig{}, Options{MultiVM: true})
	if off.LiveCowForkEnabled() {
		t.Error("live-cow fork must default OFF")
	}
	if off.liveCowForkApplies(ActivateRequest{ForkSnapshot: true}) {
		t.Error("gate must be closed when the flag is off, even for a fork child")
	}
	if env := off.liveCowParentEnv("/run/vm"); env != nil {
		t.Errorf("flag off must emit no live-cow parent env; got %v", env)
	}
}

// TestLiveCowForkGateOn proves the gate opens ONLY for a co-located fork child
// (fork snapshot) when the flag is on, and the parent env is derived under the
// VM workdir. A fresh (non-fork) activation is never accelerated.
func TestLiveCowForkGateOn(t *testing.T) {
	on := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	if !on.LiveCowForkEnabled() {
		t.Fatal("live-cow fork must be enabled when opted in")
	}
	if !on.liveCowForkApplies(ActivateRequest{ForkSnapshot: true}) {
		t.Error("gate must be open for a co-located fork child when the flag is on")
	}
	if on.liveCowForkApplies(ActivateRequest{ForkSnapshot: false}) {
		t.Error("a fresh (non-fork) activation must never take the live-cow path")
	}

	env := on.liveCowParentEnv("/run/vm")
	want := []string{
		"FIRECRACKER_MITOS_SHARED_MEM=1",
		"FIRECRACKER_MITOS_SHARED_MEM_EXPORT=/run/vm/mitos-memfd.export",
		"FIRECRACKER_MITOS_WP_UDS=/run/vm/mitos-wp.sock",
		// LAZY restore: the patched Firecracker maps guest RAM as an EMPTY shared
		// memfd and takes a MISSING fault per chunk instead of copying the whole mem
		// file inside PUT /snapshot/load. It requires BOTH this and the WP UDS, and
		// the stub fails the activate if it cannot open the mem source first, so a
		// source is never resumed on zeroed memory.
		"FIRECRACKER_MITOS_LAZY_RESTORE=1",
	}
	if len(env) != len(want) {
		t.Fatalf("parent env = %v, want %d entries", env, len(want))
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
	// Empty workdir (the unit path) emits no env even with the flag on.
	if env := on.liveCowParentEnv(""); env != nil {
		t.Errorf("empty workdir must emit no env; got %v", env)
	}
}

// TestArmLiveCowSourceGatedOff proves the source-arm wiring (m6b) is a no-op when
// the flag is off or no real workdir exists: it binds no handler, arms no freezer,
// and emits no env, so a fork takes the Full-snapshot fallback byte-for-byte. This
// is the fail-safe that keeps turning the flag off (and the mock/unit path) exactly
// the current behavior.
func TestArmLiveCowSourceGatedOff(t *testing.T) {
	// Flag OFF: never arms, even with a real workdir.
	off := New(firecracker.VMConfig{}, Options{MultiVM: true})
	if env := off.armLiveCowSource(t.TempDir()); env != nil {
		t.Errorf("flag off must arm nothing; got env %v", env)
	}
	if off.liveCowSnapshotFreezer() != nil {
		t.Error("flag off must leave the freezer nil (Full-snapshot fallback)")
	}
	if off.liveCowHandle != nil {
		t.Error("flag off must bind no WP handler")
	}

	// Flag ON but EMPTY workdir (the unit/mock launch): still arms nothing, since
	// there is no real Firecracker to hand a socket to.
	on := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	if env := on.armLiveCowSource(""); env != nil {
		t.Errorf("empty workdir must arm nothing; got env %v", env)
	}
	if on.liveCowHandle != nil {
		t.Error("empty workdir must bind no WP handler")
	}
}

// TestArmLiveCowSourceBindsAndEmitsEnv proves that with the flag on and a real
// workdir the source-arm wiring (m6b) BINDS the write-protect handshake socket and
// returns the FIRECRACKER_MITOS_* env the source Firecracker must launch with, so
// the patched source can export its memfd + offer the WP uffd. It does NOT need KVM
// (only a unix-socket bind), so it runs in the ordinary go-test job on Linux; off
// Linux StartWPForkHandler is unsupported and the arm is a fail-safe no-op, which the
// test also accepts. The Receive goroutine is unblocked by closeLiveCowSource.
func TestArmLiveCowSourceBindsAndEmitsEnv(t *testing.T) {
	s := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	workDir := t.TempDir()
	env := s.armLiveCowSource(workDir)
	defer s.closeLiveCowSource()

	if env == nil {
		// Off Linux (or a kernel/socket that cannot bind) the arm fails closed to no
		// env and no handler: the source launches stock and forks use the Full path.
		if s.liveCowHandle != nil {
			t.Fatal("a nil-env arm must retain no handler (fail-safe)")
		}
		t.Skip("live-cow source arm unsupported on this host (StartWPForkHandler); Full-snapshot fallback")
	}

	want := []string{
		"FIRECRACKER_MITOS_SHARED_MEM=1",
		"FIRECRACKER_MITOS_SHARED_MEM_EXPORT=" + filepath.Join(workDir, "mitos-memfd.export"),
		"FIRECRACKER_MITOS_WP_UDS=" + filepath.Join(workDir, "mitos-wp.sock"),
		// LAZY restore: guest RAM comes back as an EMPTY shared memfd whose pages the
		// WP handler faults in from the mem file, instead of an eager copy inside
		// PUT /snapshot/load. Firecracker requires this AND the WP UDS.
		"FIRECRACKER_MITOS_LAZY_RESTORE=1",
	}
	if len(env) != len(want) {
		t.Fatalf("arm env = %v, want %d entries", env, len(want))
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
	// The handler is bound and retained for teardown; the socket exists on disk.
	if s.liveCowHandle == nil {
		t.Error("a successful arm must retain the WP handler for teardown")
	}
	if _, err := os.Stat(filepath.Join(workDir, "mitos-wp.sock")); err != nil {
		t.Errorf("arm must bind the WP handshake socket: %v", err)
	}
	// The freezer is NOT armed until the handshake completes (no Firecracker connected
	// in this unit test), so a fork here would still take the Full-snapshot fallback.
	if s.liveCowSnapshotFreezer() != nil {
		t.Error("freezer must stay nil until the WP handshake completes")
	}
}

// TestLiveCowChildImportEnvNoParent proves that with no armed live-cow parent the
// child-import env is empty (nil, nil), so SpawnVM restores the child from the
// disk fork snapshot. This is today's production wiring until the parent-arm +
// Firecracker child-restore patch land.
func TestLiveCowChildImportEnvNoParent(t *testing.T) {
	s := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	env, err := s.liveCowChildImportEnv(ActivateRequest{SnapshotDir: t.TempDir()})
	if err != nil {
		t.Fatalf("liveCowChildImportEnv (no parent) err = %v, want nil", err)
	}
	if env != nil {
		t.Fatalf("liveCowChildImportEnv (no parent) = %v, want nil (disk fallback)", env)
	}
}

// TestLiveCowChildImportEnvArmed proves that with an armed parent the child spawn
// gets FIRECRACKER_MITOS_CHILD_MEMFD pointing at an export file the helper wrote,
// and that the file carries the round-trippable import line the child Firecracker
// parses. This is the m5 child-side wiring that boots the child from the shared
// parent memfd.
func TestLiveCowChildImportEnvArmed(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeChildImportProvider{imp: fork.ChildMemfdImport{
		ParentPID: 4242, ParentFD: 5, ParentIno: 111, ParentDev: 42, Bytes: 256 << 20,
		FrozenPID: 4243, FrozenFD: 6, FrozenIno: 222, FrozenDev: 42,
		BitmapPID: 4243, BitmapFD: 7, BitmapIno: 333, BitmapDev: 42, PageSize: 4096,
	}}
	s := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	s.SetLiveCowParent(prov)

	env, err := s.liveCowChildImportEnv(ActivateRequest{SnapshotDir: dir})
	if err != nil {
		t.Fatalf("liveCowChildImportEnv (armed): %v", err)
	}
	if prov.gotDir != dir {
		t.Errorf("ChildImport dir = %q, want %q", prov.gotDir, dir)
	}
	exportPath := filepath.Join(dir, liveCowChildImportName)
	want := fork.EnvChildMemfd + "=" + exportPath
	if len(env) != 1 || env[0] != want {
		t.Fatalf("child env = %v, want [%q]", env, want)
	}
	raw, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	imp, err := fork.ParseChildMemfdImport(string(raw))
	if err != nil {
		t.Fatalf("parse export file %q: %v", string(raw), err)
	}
	if imp.ParentPID != 4242 || imp.Bytes != 256<<20 || imp.BitmapFD != 7 || imp.BitmapIno != 333 {
		t.Errorf("export line lost fields: %+v", imp)
	}
}

// TestLiveCowChildImportEnvFailClosed proves a provider error surfaces as an error
// (not a panic or silent memfd env), so SpawnVM falls back to the disk restore and
// the flag never breaks a fork.
func TestLiveCowChildImportEnvFailClosed(t *testing.T) {
	s := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	s.SetLiveCowParent(&fakeChildImportProvider{err: fmt.Errorf("handshake not complete")})
	env, err := s.liveCowChildImportEnv(ActivateRequest{SnapshotDir: t.TempDir()})
	if err == nil {
		t.Fatal("liveCowChildImportEnv must return the provider error, not nil")
	}
	if env != nil {
		t.Fatalf("failed import must emit no env; got %v", env)
	}
	// Empty snapshot dir is also a fail-closed error.
	if _, err := s.liveCowChildImportEnv(ActivateRequest{SnapshotDir: ""}); err == nil {
		t.Fatal("empty snapshot dir must be an error")
	}
}

// TestLiveCowChildUFFDPlanNoParent proves that with no armed live-cow parent the
// lazy-UFFD import plan is (nil, nil), so SpawnVM restores the child from the disk
// fork snapshot (fail-closed disk fallback).
func TestLiveCowChildUFFDPlanNoParent(t *testing.T) {
	s := New(firecracker.VMConfig{WorkDir: t.TempDir()}, Options{MultiVM: true, LiveCowFork: true})
	plan, err := s.liveCowChildUFFDPlan("child", ActivateRequest{SnapshotDir: t.TempDir()})
	if err != nil {
		t.Fatalf("liveCowChildUFFDPlan (no parent) err = %v, want nil", err)
	}
	if plan != nil {
		t.Fatalf("liveCowChildUFFDPlan (no parent) = %+v, want nil (disk fallback)", plan)
	}
}

// TestLiveCowChildUFFDPlanArmed proves that with an armed parent the plan carries
// the ChildImport coordinates and a backend socket path under the pod workdir whose
// absolute length stays under the AF_UNIX sun_path limit even for a long vmID.
func TestLiveCowChildUFFDPlanArmed(t *testing.T) {
	// A SHORT pod workdir under /tmp: the child uffd socket path must fit the AF_UNIX
	// sun_path limit, and t.TempDir() on some hosts is already too long on its own.
	// The production pod workdir is short (e.g. /run/husk).
	workDir, err := os.MkdirTemp("/tmp", "cu")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(workDir) //nolint:errcheck // test teardown
	prov := &fakeChildImportProvider{imp: fork.ChildMemfdImport{
		ParentPID: 4242, ParentFD: 5, ParentIno: 111, ParentDev: 42, Bytes: 256 << 20,
		FrozenPID: 4243, FrozenFD: 6, FrozenIno: 222, FrozenDev: 42,
		BitmapPID: 4243, BitmapFD: 7, BitmapIno: 333, BitmapDev: 42, PageSize: 4096,
	}}
	s := New(firecracker.VMConfig{WorkDir: workDir}, Options{MultiVM: true, LiveCowFork: true})
	s.SetLiveCowParent(prov)

	dir := t.TempDir()
	plan, err := s.liveCowChildUFFDPlan("livecow-childimp-colo-longish-vmid", ActivateRequest{SnapshotDir: dir})
	if err != nil {
		t.Fatalf("liveCowChildUFFDPlan (armed): %v", err)
	}
	if plan == nil {
		t.Fatal("liveCowChildUFFDPlan (armed) = nil, want a plan")
	}
	if prov.gotDir != dir {
		t.Errorf("ChildImport dir = %q, want %q", prov.gotDir, dir)
	}
	if plan.imp.ParentPID != 4242 || plan.imp.Bytes != 256<<20 || plan.imp.BitmapIno != 333 {
		t.Errorf("plan lost import fields: %+v", plan.imp)
	}
	if filepath.Dir(plan.sockPath) != workDir {
		t.Errorf("socket %q must live under the pod workdir %q", plan.sockPath, workDir)
	}
	// The socket basename must be SHORT and FIXED-LENGTH regardless of how long the
	// vmID is: a long vmID must not push the absolute path over the AF_UNIX sun_path
	// limit (the socket lives in the SHORT pod workdir under a hashed name, not the
	// nested per-VM workdir). The absolute length then depends only on the pod
	// workdir, which is short in production (e.g. /run/husk).
	base := filepath.Base(plan.sockPath)
	if len(base) > 28 {
		t.Errorf("socket basename %q is %d bytes, want a short fixed-length hashed name", base, len(base))
	}
	longer, err := s.liveCowChildUFFDPlan("this-is-a-much-much-longer-vm-identifier-than-before", ActivateRequest{SnapshotDir: dir})
	if err != nil {
		t.Fatalf("liveCowChildUFFDPlan (long vmID): %v", err)
	}
	if got := len(filepath.Base(longer.sockPath)); got != len(base) {
		t.Errorf("a longer vmID changed the socket basename length (%d -> %d); it must stay fixed", len(base), got)
	}
}

// TestLiveCowChildUFFDPlanFailClosed proves a provider error, an empty snapshot
// dir, and an empty pod workdir each surface as an error (no plan), so SpawnVM
// falls back to the disk restore and the flag never breaks a fork.
func TestLiveCowChildUFFDPlanFailClosed(t *testing.T) {
	s := New(firecracker.VMConfig{WorkDir: t.TempDir()}, Options{MultiVM: true, LiveCowFork: true})
	s.SetLiveCowParent(&fakeChildImportProvider{err: fmt.Errorf("handshake not complete")})
	if plan, err := s.liveCowChildUFFDPlan("child", ActivateRequest{SnapshotDir: t.TempDir()}); err == nil || plan != nil {
		t.Fatalf("provider error must yield (nil, err); got plan=%+v err=%v", plan, err)
	}
	if _, err := s.liveCowChildUFFDPlan("child", ActivateRequest{SnapshotDir: ""}); err == nil {
		t.Fatal("empty snapshot dir must be an error")
	}

	// Empty pod workdir has nowhere to bind the socket: fail closed (disk fallback).
	noWD := New(firecracker.VMConfig{}, Options{MultiVM: true, LiveCowFork: true})
	noWD.SetLiveCowParent(&fakeChildImportProvider{imp: fork.ChildMemfdImport{
		ParentPID: 1, ParentFD: 2, ParentIno: 3, ParentDev: 4, Bytes: 4096,
		FrozenPID: 1, FrozenFD: 3, FrozenIno: 5, FrozenDev: 4,
		BitmapPID: 1, BitmapFD: 4, BitmapIno: 6, BitmapDev: 4, PageSize: 4096,
	}})
	if _, err := noWD.liveCowChildUFFDPlan("child", ActivateRequest{SnapshotDir: t.TempDir()}); err == nil {
		t.Fatal("empty pod workdir must be an error (no place to bind the child uffd socket)")
	}

	// A pod workdir so long the absolute socket path would exceed the AF_UNIX
	// sun_path limit must fail closed rather than bind a truncated, unreachable path.
	longWD := New(firecracker.VMConfig{WorkDir: "/" + strings.Repeat("d", 200)}, Options{MultiVM: true, LiveCowFork: true})
	longWD.SetLiveCowParent(&fakeChildImportProvider{imp: fork.ChildMemfdImport{
		ParentPID: 1, ParentFD: 2, ParentIno: 3, ParentDev: 4, Bytes: 4096,
		FrozenPID: 1, FrozenFD: 3, FrozenIno: 5, FrozenDev: 4,
		BitmapPID: 1, BitmapFD: 4, BitmapIno: 6, BitmapDev: 4, PageSize: 4096,
	}})
	if _, err := longWD.liveCowChildUFFDPlan("child", ActivateRequest{SnapshotDir: t.TempDir()}); err == nil {
		t.Fatal("a too-long pod workdir must be an error (socket path over the sun_path limit)")
	}
}
