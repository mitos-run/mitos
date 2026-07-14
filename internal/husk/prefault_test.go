package husk

import (
	"context"
	"errors"
	"testing"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/guestgrpc"
)

// recordingPrefault records every prefault invocation so tests can assert when
// the seam fires (dormant Prepare, never Activate) and what it was handed.
type recordingPrefault struct {
	calls int
	err   error
}

func (p *recordingPrefault) run(_ context.Context, _ *guestgrpc.Client, vsockPath string) error {
	p.calls++
	if vsockPath == "" {
		return errors.New("prefault handed an empty vsock path")
	}
	return p.err
}

// newPrefaultStub is newPrepareRestoreStub plus the kernel-prefault opt-in and a
// recording seam.
func newPrefaultStub(t *testing.T, rr *recordingRunner, vms map[string]*fakeVMM, pf *recordingPrefault, optIn bool) *Stub {
	t.Helper()
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := vms[cfg.ID]
		if vm == nil {
			vm = &fakeVMM{}
			vms[cfg.ID] = vm
		}
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:                 start,
		Ready:                 readyOK,
		Notify:                (&fakeNotifier{}).notify,
		Verify:                verifyOK,
		MultiVM:               true,
		PrepareEgressLink:     true,
		PrepareRestore:        true,
		PrepareKernelPrefault: optIn,
		InPodGuestIP:          "10.200.0.2",
		InPodGatewayIP:        "10.200.0.1",
		PrepareSnapshotDir:    "/snap",
	})
	s.SetNetRunner(rr.run)
	s.prefaultKernel = pf.run
	return s
}

// TestPrepareRunsTheKernelPrefaultWhileDormant is the core of slice 3 (issue
// #889): with PrepareKernelPrefault on, the inert warm cell runs ONCE at
// Prepare, after the dormant restore, so the first tenant run_code does not
// fault the ipykernel's pages in. Activate must not run it again.
func TestPrepareRunsTheKernelPrefaultWhileDormant(t *testing.T) {
	var rr recordingRunner
	vms := map[string]*fakeVMM{}
	pf := &recordingPrefault{}
	s := newPrefaultStub(t, &rr, vms, pf, true)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if pf.calls != 1 {
		t.Fatalf("Prepare ran the kernel prefault %d times, want exactly 1", pf.calls)
	}
	if _, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if pf.calls != 1 {
		t.Errorf("Activate re-ran the kernel prefault (calls=%d); it belongs to the dormant window only", pf.calls)
	}
}

// TestPrepareKernelPrefaultIsOptIn: with the flag off, Prepare never touches the
// kernel, byte-for-byte the slice-2 behavior.
func TestPrepareKernelPrefaultIsOptIn(t *testing.T) {
	var rr recordingRunner
	vms := map[string]*fakeVMM{}
	pf := &recordingPrefault{}
	s := newPrefaultStub(t, &rr, vms, pf, false)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if pf.calls != 0 {
		t.Errorf("Prepare ran the kernel prefault without the opt-in (calls=%d)", pf.calls)
	}
}

// TestPrepareKernelPrefaultFailsOpen: a template without the run_code kernel (a
// non-python pool) must not crash-loop its husk pods. A prefault failure logs
// and continues; the pod still goes dormant and a claim still activates, paying
// the lazy kernel start exactly as before slice 3.
func TestPrepareKernelPrefaultFailsOpen(t *testing.T) {
	var rr recordingRunner
	vms := map[string]*fakeVMM{}
	pf := &recordingPrefault{err: errors.New("no kernel in this image")}
	s := newPrefaultStub(t, &rr, vms, pf, true)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare must fail OPEN on a prefault error, got: %v", err)
	}
	if pf.calls != 1 {
		t.Fatalf("prefault seam not invoked (calls=%d)", pf.calls)
	}
	res, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()})
	if err != nil || !res.OK {
		t.Fatalf("Activate after a failed-open prefault: res=%+v err=%v", res, err)
	}
}

// TestPrepareKernelPrefaultRequiresPrepareRestore: without the dormant restore
// there is no running guest to warm, so the prefault must be a no-op rather
// than a dial into an unloaded VMM.
func TestPrepareKernelPrefaultRequiresPrepareRestore(t *testing.T) {
	var rr recordingRunner
	vms := map[string]*fakeVMM{}
	pf := &recordingPrefault{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms[cfg.ID] = vm
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start: start, Ready: readyOK, Notify: (&fakeNotifier{}).notify, Verify: verifyOK,
		MultiVM: true, PrepareEgressLink: true,
		PrepareKernelPrefault: true, // but NO PrepareRestore
		InPodGuestIP:          "10.200.0.2", InPodGatewayIP: "10.200.0.1",
		PrepareSnapshotDir: "/snap",
	})
	s.SetNetRunner(rr.run)
	s.prefaultKernel = pf.run

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if pf.calls != 0 {
		t.Errorf("prefault ran without a pre-restored guest (calls=%d); there is nothing to warm in an unloaded VMM", pf.calls)
	}
}
