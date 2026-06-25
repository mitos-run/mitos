package firecracker

import "testing"

// TestAssertSeccompEnforcedRejectsNoSeccomp is the fail-closed guard for issue
// #353: no Firecracker launch path may disable the built-in seccomp filter. If a
// future flag or refactor ever threaded `--no-seccomp` into the VMM argv, the
// launch must refuse rather than silently run the VMM with its full syscall
// surface.
func TestAssertSeccompEnforcedRejectsNoSeccomp(t *testing.T) {
	args := []string{"--api-sock", "run/firecracker.socket", "--no-seccomp"}
	if err := assertSeccompEnforced(args); err == nil {
		t.Fatal("assertSeccompEnforced accepted argv containing --no-seccomp; it must fail closed")
	}
}

// TestAssertSeccompEnforcedAllowsDefault confirms the normal argv (which relies
// on Firecracker's built-in production filter, installed unless --no-seccomp is
// passed) is accepted.
func TestAssertSeccompEnforcedAllowsDefault(t *testing.T) {
	args := []string{"--api-sock", "run/firecracker.socket"}
	if err := assertSeccompEnforced(args); err != nil {
		t.Fatalf("assertSeccompEnforced rejected a normal argv: %v", err)
	}
}

// TestAssertSeccompEnforcedRejectsNoSeccompWithValueForm guards the `=` form too
// (some launchers spell boolean flags as --no-seccomp=true).
func TestAssertSeccompEnforcedRejectsNoSeccompWithValueForm(t *testing.T) {
	for _, form := range []string{"--no-seccomp=true", "--no-seccomp=1"} {
		if err := assertSeccompEnforced([]string{"--api-sock", "s", form}); err == nil {
			t.Fatalf("assertSeccompEnforced accepted %q; it must fail closed", form)
		}
	}
}

// TestParseSeccompMode parses the Seccomp field of /proc/<pid>/status, which the
// KVM CI assertion reads to prove the VMM runs under a BPF filter (mode 2 =
// SECCOMP_MODE_FILTER). 0 = none, 1 = strict, 2 = filter.
func TestParseSeccompMode(t *testing.T) {
	status := "Name:\tfirecracker\nState:\tS (sleeping)\nSeccomp:\t2\nSeccomp_filters:\t1\n"
	mode, ok := parseSeccompMode(status)
	if !ok {
		t.Fatal("parseSeccompMode did not find the Seccomp field")
	}
	if mode != 2 {
		t.Fatalf("parseSeccompMode = %d, want 2 (SECCOMP_MODE_FILTER)", mode)
	}
}

// TestParseSeccompModeDisabled covers the --no-seccomp negative control (mode 0).
func TestParseSeccompModeDisabled(t *testing.T) {
	mode, ok := parseSeccompMode("Name:\tfirecracker\nSeccomp:\t0\n")
	if !ok || mode != 0 {
		t.Fatalf("parseSeccompMode = (%d, %v), want (0, true)", mode, ok)
	}
}

// TestParseSeccompModeMissing returns ok=false when the field is absent, so a
// caller does not mistake a parse miss for mode 0 (disabled).
func TestParseSeccompModeMissing(t *testing.T) {
	if _, ok := parseSeccompMode("Name:\tfirecracker\nState:\tR\n"); ok {
		t.Fatal("parseSeccompMode reported ok on status with no Seccomp field")
	}
}
