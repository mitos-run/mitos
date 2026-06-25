package firecracker

import (
	"fmt"
	"strconv"
	"strings"
)

// seccompDisableFlag is the Firecracker flag that turns OFF the built-in seccomp
// BPF filter. Firecracker installs its production filter on every VMM thread
// UNLESS this flag is passed, so the security guarantee is simply that no Mitos
// launch path ever passes it. assertSeccompEnforced enforces that invariant.
const seccompDisableFlag = "--no-seccomp"

// assertSeccompEnforced is the fail-closed guard (issue #353): it refuses any
// Firecracker argv that would disable the built-in seccomp filter. The VMM is
// the host-facing edge of the isolation boundary, so a VMM that ran with its
// full syscall surface (because seccomp was disabled) would be a materially
// larger blast radius after a microVM escape. Nothing in Mitos passes
// --no-seccomp today; this guard makes that a checked invariant rather than an
// implicit one, so a future flag or refactor that introduced it fails the launch
// instead of silently weakening the second wall behind KVM. It is called on the
// final argv of BOTH launch paths (direct exec and the jailer) before exec.
func assertSeccompEnforced(args []string) error {
	for _, a := range args {
		if a == seccompDisableFlag || strings.HasPrefix(a, seccompDisableFlag+"=") {
			return fmt.Errorf("refusing to launch Firecracker with %q: the built-in seccomp filter is the second wall behind KVM and must never be disabled (issue #353); remove the flag", a)
		}
	}
	return nil
}

// parseSeccompMode extracts the seccomp mode from the contents of
// /proc/<pid>/status. The kernel reports `Seccomp:\t<mode>` where 0 = disabled,
// 1 = SECCOMP_MODE_STRICT, 2 = SECCOMP_MODE_FILTER. The KVM CI assertion reads
// this to prove the launched Firecracker VMM runs under a BPF filter (mode 2),
// and that a --no-seccomp launch reports mode 0 (the negative control). It
// returns ok=false when the field is absent so a parse miss is never mistaken
// for mode 0 (disabled).
func parseSeccompMode(procStatus string) (mode int, ok bool) {
	for _, line := range strings.Split(procStatus, "\n") {
		rest, found := strings.CutPrefix(line, "Seccomp:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		v, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
