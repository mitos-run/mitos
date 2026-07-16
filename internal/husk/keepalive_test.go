package husk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
)

// keepaliveSeam records every warm-pool keepalive round so a test can assert
// WHEN the dormant self-keepalive fires (repeatedly while dormant, never after a
// claim) and what vsock path it was handed, with no live gRPC guest. It mirrors
// the checkout buffer's injectable warm seam (internal/saas/controlplane).
type keepaliveSeam struct {
	mu    sync.Mutex
	calls int
	paths []string
	err   error
	// fired signals each round so a test waits for N rounds instead of sleeping.
	fired chan struct{}
}

func (k *keepaliveSeam) run(_ context.Context, vsockPath string) error {
	k.mu.Lock()
	k.calls++
	k.paths = append(k.paths, vsockPath)
	err := k.err
	k.mu.Unlock()
	select {
	case k.fired <- struct{}{}:
	default:
	}
	return err
}

func (k *keepaliveSeam) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

func newKeepaliveSeam() *keepaliveSeam {
	return &keepaliveSeam{fired: make(chan struct{}, 256)}
}

// waitFires blocks until the seam has fired n times or a generous timeout
// elapses, so the loop's cadence is proven without a real 60 s wait.
func waitFires(t *testing.T, k *keepaliveSeam, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-k.fired:
		case <-deadline:
			t.Fatalf("keepalive fired %d times, wanted at least %d within the timeout", k.count(), n)
		}
	}
}

// keepaliveCancelSet reports whether the default VM's keepalive loop is running
// (its cancel func is set), read under the instance lock.
func keepaliveCancelSet(s *Stub) bool {
	inst := s.instanceFor(defaultVMID, false)
	if inst == nil {
		return false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.keepaliveCancel != nil
}

// newKeepaliveStub builds a multi-VM husk stub with the prepare-restore surface
// wired, an injected keepalive seam, and a short interval so the loop's cadence
// is exercised in milliseconds. restore/prefault toggle the two gates the loop
// depends on. Close is registered so a never-claimed pod's loop goroutine is
// stopped at test end.
func newKeepaliveStub(t *testing.T, k *keepaliveSeam, interval time.Duration, restore, prefault bool) (*Stub, map[string]*fakeVMM) {
	t.Helper()
	rr := &recordingRunner{}
	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms[cfg.ID] = vm
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:                 start,
		Ready:                 readyOK,
		Notify:                (&fakeNotifier{}).notify,
		Verify:                verifyOK,
		MultiVM:               true,
		PrepareEgressLink:     true,
		PrepareRestore:        restore,
		PrepareKernelPrefault: prefault,
		InPodGuestIP:          "10.200.0.2",
		InPodGatewayIP:        "10.200.0.1",
		PrepareSnapshotDir:    "/snap",
	})
	s.SetNetRunner(rr.run)
	if k != nil {
		s.keepaliveWarm = k.run
	}
	s.keepaliveInterval = interval
	// The prepare-time prefault seam is separate from the keepalive seam; stub it
	// so Prepare's one-shot prefault does not dial a real guest.
	s.prefaultKernel = (&recordingPrefault{}).run
	t.Cleanup(func() { _ = s.Close() })
	return s, vms
}

// TestWarmPoolKeepaliveWarmsThePreRestoredGuestWhileDormant is the core of #913:
// a dormant, prepare-restored + prefaulted husk pod re-runs the inert warm cell
// against its OWN running guest on an interval, so the run_code kernel's working
// set stays resident until a claim arrives (countering the #903 idle decay).
func TestWarmPoolKeepaliveWarmsThePreRestoredGuestWhileDormant(t *testing.T) {
	k := newKeepaliveSeam()
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, true, true)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !keepaliveCancelSet(s) {
		t.Fatal("keepalive loop did not start for a pre-restored prefaulted dormant pod")
	}
	// It must fire REPEATEDLY, not once.
	waitFires(t, k, 3)

	k.mu.Lock()
	defer k.mu.Unlock()
	for _, p := range k.paths {
		if p == "" {
			t.Fatalf("keepalive round got an empty vsock path")
		}
	}
}

// TestWarmPoolKeepaliveRequiresPreRestore: a pod that never pre-restored has no
// running dormant guest to warm, so the keepalive is a no-op (nothing to dial).
func TestWarmPoolKeepaliveRequiresPreRestore(t *testing.T) {
	k := newKeepaliveSeam()
	// PrepareRestore off => the default VM is never resumed while dormant.
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, false, true)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if keepaliveCancelSet(s) {
		t.Fatal("keepalive loop started without a pre-restored guest; there is nothing to warm")
	}
	// Give a few intervals a chance to (wrongly) fire.
	time.Sleep(20 * time.Millisecond)
	if got := k.count(); got != 0 {
		t.Fatalf("keepalive fired %d times without a pre-restored guest, want 0", got)
	}
}

// TestWarmPoolKeepaliveRequiresPrefault proves the design choice (no new flag):
// the keepalive engages ONLY when prepare-kernel-prefault is on. A pod that
// pre-restored but did not opt into the prefault keeps no kernel warm, so it
// runs no keepalive either.
func TestWarmPoolKeepaliveRequiresPrefault(t *testing.T) {
	k := newKeepaliveSeam()
	// PrepareRestore on, but PrepareKernelPrefault off.
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, true, false)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if keepaliveCancelSet(s) {
		t.Fatal("keepalive loop started without the prefault opt-in")
	}
	time.Sleep(20 * time.Millisecond)
	if got := k.count(); got != 0 {
		t.Fatalf("keepalive fired %d times without the prefault opt-in, want 0", got)
	}
}

// TestWarmPoolKeepaliveStopsAtActivate: once a claim activates the VM, the
// keepalive must stop. The tenant's own execs keep the guest warm, and the
// keepalive must never contend with tenant run_code.
func TestWarmPoolKeepaliveStopsAtActivate(t *testing.T) {
	k := newKeepaliveSeam()
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, true, true)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	waitFires(t, k, 2)

	if _, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if keepaliveCancelSet(s) {
		t.Fatal("keepalive loop still running after activate; it belongs to the dormant window only")
	}
	// After the loop stops, the call count must not keep climbing.
	stopped := k.count()
	time.Sleep(20 * time.Millisecond)
	if after := k.count(); after != stopped {
		t.Fatalf("keepalive fired %d more times after activate (from %d to %d); it must stop at the claim", after-stopped, stopped, after)
	}
}

// TestWarmPoolKeepaliveFailureDoesNotBreakActivate: a keepalive round that
// errors (a wedged or kernel-less guest) must be best-effort only. It logs and
// continues, and it never blocks or fails a subsequent activate.
func TestWarmPoolKeepaliveFailureDoesNotBreakActivate(t *testing.T) {
	k := newKeepaliveSeam()
	k.err = errors.New("keepalive round boom")
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, true, true)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Let a couple of rounds fail.
	waitFires(t, k, 2)

	res, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()})
	if err != nil || !res.OK {
		t.Fatalf("Activate after failing keepalive rounds: res=%+v err=%v", res, err)
	}
	if keepaliveCancelSet(s) {
		t.Fatal("keepalive loop still running after activate")
	}
}

// TestWarmPoolKeepaliveStopsAtClose: a pod torn down before any claim stops its
// keepalive loop so no goroutine outlives the pod.
func TestWarmPoolKeepaliveStopsAtClose(t *testing.T) {
	k := newKeepaliveSeam()
	s, _ := newKeepaliveStub(t, k, 2*time.Millisecond, true, true)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	waitFires(t, k, 1)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if keepaliveCancelSet(s) {
		t.Fatal("keepalive loop still running after Close")
	}
	stopped := k.count()
	time.Sleep(20 * time.Millisecond)
	if after := k.count(); after != stopped {
		t.Fatalf("keepalive fired %d more times after Close", after-stopped)
	}
}
