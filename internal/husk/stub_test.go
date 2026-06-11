package husk

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/firecracker"
)

// fakeVMM records the snapshot-load arguments and returns a canned error.
type fakeVMM struct {
	loadErr error

	mu        sync.Mutex
	loadCalls int
	gotMem    string
	gotState  string
	gotResume bool
	gotOverr  []firecracker.NetworkOverride
	closed    bool
}

func (f *fakeVMM) LoadSnapshotWithOverrides(mem, snapshot string, resume bool, overrides []firecracker.NetworkOverride) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	f.gotMem = mem
	f.gotState = snapshot
	f.gotResume = resume
	f.gotOverr = overrides
	return f.loadErr
}

func (f *fakeVMM) VsockHostPath(rel string) string {
	return filepath.Join("/run/husk", rel)
}

func (f *fakeVMM) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func newTestStub(t *testing.T, vm *fakeVMM, ready guestReady) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start: func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready: ready,
	})
}

func readyOK(string, time.Duration) error { return nil }

func TestActivateBeforePrepareErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error activating before prepare")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK")
	}
	if s.State() == StateActive {
		t.Fatalf("state must not be active, got %s", s.State())
	}
}

func TestPrepareThenActivateSucceeds(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if s.State() != StateDormant {
		t.Fatalf("after prepare state = %s, want dormant", s.State())
	}

	overrides := []firecracker.NetworkOverride{{IfaceID: "eth0", HostDevName: "tap-1"}}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:      "/data/templates/tmpl/snapshot",
		NetworkOverrides: overrides,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %s", res.Error)
	}
	if s.State() != StateActive {
		t.Fatalf("after activate state = %s, want active", s.State())
	}

	// Loaded the engine-layout mem/vmstate paths under the snapshot dir.
	if vm.gotMem != "/data/templates/tmpl/snapshot/mem" {
		t.Errorf("mem path = %q", vm.gotMem)
	}
	if vm.gotState != "/data/templates/tmpl/snapshot/vmstate" {
		t.Errorf("vmstate path = %q", vm.gotState)
	}
	if !vm.gotResume {
		t.Error("expected resume=true")
	}
	if len(vm.gotOverr) != 1 || vm.gotOverr[0].HostDevName != "tap-1" {
		t.Errorf("overrides not threaded through: %+v", vm.gotOverr)
	}
	if res.VsockPath != "/run/husk/"+firecracker.VsockRelPath {
		t.Errorf("vsock path = %q", res.VsockPath)
	}
	if res.LatencyMs <= 0 {
		t.Errorf("LatencyMs must be > 0, got %v", res.LatencyMs)
	}
}

func TestActivateTwiceErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil || res.OK {
		t.Fatal("second activate must fail (one VM per husk)")
	}
}

func TestPrepareTwiceErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := s.Prepare(context.Background()); err == nil {
		t.Fatal("second Prepare must error (no second VMM)")
	}
}

func TestActivateLoadFailureFailsClosed(t *testing.T) {
	vm := &fakeVMM{loadErr: errors.New("snapshot corrupt")}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error on load failure")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK on load failure")
	}
	if res.VsockPath != "" {
		t.Fatal("fail closed: must not report a usable vsock path")
	}
	if s.State() == StateActive {
		t.Fatalf("fail closed: state must not be active, got %s", s.State())
	}
}

func TestActivateGuestNotReadyFailsClosed(t *testing.T) {
	vm := &fakeVMM{}
	readyTimeout := func(string, time.Duration) error {
		return errors.New("no ping")
	}
	s := newTestStub(t, vm, readyTimeout)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error when guest not ready")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK when guest never answers")
	}
	if s.State() == StateActive {
		t.Fatalf("fail closed: state must not be active, got %s", s.State())
	}
	// The snapshot DID load, proving we failed at the readiness gate, not before.
	if vm.loadCalls != 1 {
		t.Fatalf("expected load to be attempted once, got %d", vm.loadCalls)
	}
}

func TestServeDispatchesActivate(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(context.Background(), ln) }()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := WriteRequest(conn, ActivateRequest{SnapshotDir: "/data/templates/x/snapshot"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	res, err := ReadResult(conn)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate over control socket not OK: %s", res.Error)
	}
	if res.VsockPath == "" || res.LatencyMs <= 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Serve returns after a successful activate (one VM per husk).
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after successful activate")
	}
}

func TestCloseTearsDownVMM(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !vm.closed {
		t.Fatal("Close must tear down the VMM")
	}
	if s.State() != StateNew {
		t.Fatalf("after close state = %s, want new", s.State())
	}
}
