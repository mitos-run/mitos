package husk

import (
	"fmt"
	"os"
	"path/filepath"
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
	imp := f.imp
	imp.BitmapPath = filepath.Join(dir, "mitos-frozen.bm")
	return imp, nil
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
		ParentPID: 4242, ParentFD: 5, Bytes: 256 << 20, FrozenPID: 4243, FrozenFD: 6, PageSize: 4096,
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
	if imp.ParentPID != 4242 || imp.Bytes != 256<<20 || imp.BitmapPath != filepath.Join(dir, "mitos-frozen.bm") {
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
