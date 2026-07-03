package husk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

// newTestStubWithID is newTestStub but lets a test pin the pod's vm-id so it can
// assert the single metering Sample carries it (the id the controller maps to an
// org). The other seams are the same no-op fakes newTestStub uses.
func newTestStubWithID(t *testing.T, id string, vm *fakeVMM, ready guestReady) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: id}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:  ready,
		Notify: (&fakeNotifier{}).notify,
		Verify: verifyOK,
	})
}

// TestMeteringEmptyBeforeActivate asserts a husk pod meters NOTHING before a
// successful activate (StateNew and StateDormant), so a scrape during the warm
// window is a clean empty report, never a 5xx.
func TestMeteringEmptyBeforeActivate(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)

	// StateNew (before Prepare).
	if rep := s.Metering(); len(rep.Sandboxes) != 0 {
		t.Fatalf("StateNew metering must be empty, got %d sandboxes", len(rep.Sandboxes))
	}

	// StateDormant (after Prepare).
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	rep := s.Metering()
	if len(rep.Sandboxes) != 0 {
		t.Fatalf("StateDormant metering must be empty, got %d sandboxes", len(rep.Sandboxes))
	}
	if len(rep.Templates) != 0 {
		t.Fatalf("StateDormant metering must have no templates, got %d", len(rep.Templates))
	}
}

// TestMeteringSingleVMAfterActivate asserts an activated husk pod reports EXACTLY
// one sample and that its id is the pod's vm-id (the id the controller attributes
// to an org).
func TestMeteringSingleVMAfterActivate(t *testing.T) {
	const vmID = "husk-pod-acme-xyz"
	s := newTestStubWithID(t, vmID, &fakeVMM{}, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}

	rep := s.Metering()
	if len(rep.Sandboxes) != 1 {
		t.Fatalf("active metering must have exactly one sample, got %d", len(rep.Sandboxes))
	}
	if rep.Sandboxes[0].ID != vmID {
		t.Fatalf("sample id = %q, want the pod vm-id %q", rep.Sandboxes[0].ID, vmID)
	}
}

// TestMeteringReportIsSecretFree asserts the metering report never carries a
// secret value: activating with an env value, a secret value, and a bearer token,
// then marshaling the report, must not leak any of them.
func TestMeteringReportIsSecretFree(t *testing.T) {
	const (
		vmID        = "husk-pod-secret"
		envValue    = "ENV-VALUE-SHOULD-NOT-LEAK"
		secretValue = "SECRET-VALUE-SHOULD-NOT-LEAK"
		tokenValue  = "TOKEN-SHOULD-NOT-LEAK"
	)
	s := newTestStubWithID(t, vmID, &fakeVMM{}, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir: "/snap",
		Env:         map[string]string{"MY_ENV": envValue},
		Secrets:     map[string]string{"MY_SECRET": secretValue},
		Token:       tokenValue,
	})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}

	body, err := json.Marshal(s.Metering())
	if err != nil {
		t.Fatalf("marshal metering report: %v", err)
	}
	for _, secret := range []string{envValue, secretValue, tokenValue} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("metering report body leaked a secret value: %s", body)
		}
	}
}
