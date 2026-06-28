package husk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
)

// TestMonitorVMMReturnsNilOnContextCancel: a healthy VMM (Ping answers) must keep
// the monitor running until the context is cancelled, then return nil (a normal
// shutdown is not a VMM death).
func TestMonitorVMMReturnsNilOnContextCancel(t *testing.T) {
	vm := &fakeVMM{} // nil ping func => alive
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.MonitorVMM(ctx, time.Millisecond, 3) }()

	time.Sleep(20 * time.Millisecond) // let several healthy pings run
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MonitorVMM on a healthy VMM + cancel = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MonitorVMM did not return after context cancel")
	}
}

// TestMonitorVMMDetectsDeadVMM: once Firecracker stops answering its API socket
// (the connection-refused a husk claim hits when the VMM died after the pod went
// Ready), the monitor must return ErrVMMDead so the pod exits and restarts
// instead of advertising a dead warm slot (issue #527).
func TestMonitorVMMDetectsDeadVMM(t *testing.T) {
	dead := errors.New("connection refused")
	vm := &fakeVMM{ping: func() error { return dead }}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	err := s.MonitorVMM(context.Background(), time.Millisecond, 3)
	if !errors.Is(err, ErrVMMDead) {
		t.Fatalf("MonitorVMM on a dead VMM = %v, want ErrVMMDead", err)
	}
}

// TestMonitorVMMToleratesTransientBlip: a single (sub-threshold) run of failed
// pings, for example a slow API call during Activate, must NOT flap the pod. The
// consecutive-failure counter resets on the next good ping.
func TestMonitorVMMToleratesTransientBlip(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	blip := errors.New("temporarily busy")
	vm := &fakeVMM{ping: func() error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		// Fail pings 2 and 3 (2 consecutive, below the threshold of 3), then heal.
		if calls == 2 || calls == 3 {
			return blip
		}
		return nil
	}}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.MonitorVMM(ctx, time.Millisecond, 3); err != nil {
		t.Fatalf("MonitorVMM tripped on a sub-threshold blip: %v", err)
	}
}

// TestPrepareWaitsForSnapshotPresent: when the controller passes a snapshot dir +
// digest, Prepare verifies at the dormant stage. With the snapshot already on
// disk the wait is a no-op and Prepare reaches StateDormant.
func TestPrepareWaitsForSnapshotPresent(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"mem", "vmstate"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vm := &fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:                 func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:                 readyOK,
		Notify:                (&fakeNotifier{}).notify,
		Verify:                verifyOK,
		PrepareSnapshotDir:    dir,
		PrepareExpectedDigest: "sha256:deadbeef",
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare with a present snapshot: %v", err)
	}
	if s.State() != StateDormant {
		t.Fatalf("state = %s, want dormant", s.State())
	}
}

// TestPrepareSnapshotAbsentWaitsThenFails: a pod that starts before its snapshot
// exists must WAIT (not load an absent snapshot into a VMM that then dies). With
// the snapshot absent and the context cancelled, Prepare returns a not-ready
// error rather than proceeding, and tears the dormant VMM down (issue #527).
func TestPrepareSnapshotAbsentWaitsThenFails(t *testing.T) {
	dir := t.TempDir() // empty: no mem/vmstate
	vm := &fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:                 func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:                 readyOK,
		Notify:                (&fakeNotifier{}).notify,
		Verify:                verifyOK,
		PrepareSnapshotDir:    dir,
		PrepareExpectedDigest: "sha256:deadbeef",
	})
	// Cancel DURING the wait, not before: a pre-cancelled context bails at
	// Prepare's early guard before the VMM starts. Let Prepare start the dormant
	// VMM and enter the absent-snapshot wait, then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Prepare(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel() // the snapshot never appears
	var err error
	select {
	case err = <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare did not return after the wait context was cancelled")
	}
	if err == nil {
		t.Fatal("Prepare proceeded with an absent snapshot")
	}
	if s.State() == StateDormant {
		t.Fatal("Prepare reached dormant with an absent snapshot")
	}
	vm.mu.Lock()
	closed := vm.closed
	vm.mu.Unlock()
	if !closed {
		t.Fatal("Prepare did not tear the dormant VMM down on the absent-snapshot path")
	}
}
