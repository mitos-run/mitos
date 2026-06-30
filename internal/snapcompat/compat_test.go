package snapcompat

import (
	"errors"
	"strings"
	"testing"

	"mitos.run/mitos/internal/cas"
)

func goodEnv() Environment {
	return Environment{
		FormatVersions:        []int{cas.CurrentSnapshotFormatVersion},
		VMMVersion:            "1.15.0",
		CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:         "6.1.0",
		GuestProtocolVersions: []int{cas.CurrentGuestProtocolVersion},
	}
}

func goodManifest() cas.Manifest {
	return cas.Manifest{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
		VMMVersion:            "1.15.0",
		CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:         "6.1.0",
		GuestProtocolVersion:  cas.CurrentGuestProtocolVersion,
	}
}

func TestCheckMatchingEnvReturnsNil(t *testing.T) {
	if err := Check(goodManifest(), goodEnv()); err != nil {
		t.Fatalf("expected nil for matching env, got %v", err)
	}
}

func TestCheckFormatVersionUnsupported(t *testing.T) {
	m := goodManifest()
	m.SnapshotFormatVersion = 99
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for unsupported format version")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"99", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func TestCheckZeroFormatVersionPreContract(t *testing.T) {
	m := goodManifest()
	m.SnapshotFormatVersion = 0
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for zero format version")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"predates", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("pre-contract message %q missing %q", msg, want)
		}
	}
}

func TestCheckVMMMismatch(t *testing.T) {
	m := goodManifest()
	m.VMMVersion = "1.10.0"
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for VMM mismatch")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	// Both sides and remediation must be named.
	for _, want := range []string{"1.10.0", "1.15.0", "Firecracker", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("VMM message %q missing %q", msg, want)
		}
	}
}

func TestCheckCPUMismatch(t *testing.T) {
	m := goodManifest()
	m.CPUModel = "AMD EPYC 7B12"
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for CPU mismatch")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"AMD EPYC 7B12", "Xeon", "CPU template", "schedule"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("CPU message %q missing %q", msg, want)
		}
	}
}

func TestCheckKernelMismatchInformationalOnly(t *testing.T) {
	// Kernel differs but format+VMM+CPU all match: v1 treats this as
	// informational, so Check must return nil.
	m := goodManifest()
	m.KernelVersion = "5.10.0"
	if err := Check(m, goodEnv()); err != nil {
		t.Fatalf("kernel mismatch must not be fatal in v1, got %v", err)
	}
}

func TestCheckOrderFormatBeforeVMM(t *testing.T) {
	// Both format and VMM mismatch: format is reported first.
	m := goodManifest()
	m.SnapshotFormatVersion = 7
	m.VMMVersion = "0.0.1"
	err := Check(m, goodEnv())
	if err == nil || !strings.Contains(err.Error(), "format version") {
		t.Fatalf("expected format mismatch first, got %v", err)
	}
}

func TestCheckGuestProtocolMismatch(t *testing.T) {
	// Format, VMM, and CPU all match (a pure guest-agent upgrade, issue #459):
	// the snapshot's baked agent speaks a protocol this build does not, so the
	// restore must be refused fail-closed instead of breaking at the handshake.
	m := goodManifest()
	m.GuestProtocolVersion = cas.CurrentGuestProtocolVersion + 1
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for guest-protocol mismatch")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"guest-agent protocol", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("guest-protocol message %q missing %q", msg, want)
		}
	}
}

func TestCheckZeroGuestProtocolPreTracking(t *testing.T) {
	// A snapshot built before guest-agent protocol tracking (issue #459) records
	// version 0. Its baked agent's protocol is unknown, so it is refused with an
	// actionable rebuild message rather than a broken pipe at handshake time.
	m := goodManifest()
	m.GuestProtocolVersion = 0
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for zero guest-protocol version")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"predates", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("pre-tracking message %q missing %q", msg, want)
		}
	}
}

func TestCheckGuestProtocolSkippedWhenEnvUnset(t *testing.T) {
	// A caller that did not detect the environment (e.g. the development
	// --allow-unverified path) declares no supported guest-protocol set; the
	// check must not then refuse every snapshot.
	env := goodEnv()
	env.GuestProtocolVersions = nil
	m := goodManifest()
	m.GuestProtocolVersion = 0
	if err := Check(m, env); err != nil {
		t.Fatalf("expected nil when env declares no supported guest-protocol set, got %v", err)
	}
}

func TestCheckGuestProtocolAfterCPU(t *testing.T) {
	// Both CPU and guest-protocol mismatch: CPU (the more fundamental restore
	// hazard) is reported first.
	m := goodManifest()
	m.CPUModel = "AMD EPYC 7B12"
	m.GuestProtocolVersion = cas.CurrentGuestProtocolVersion + 1
	err := Check(m, goodEnv())
	if err == nil || !strings.Contains(err.Error(), "CPU") {
		t.Fatalf("expected CPU mismatch first, got %v", err)
	}
}
