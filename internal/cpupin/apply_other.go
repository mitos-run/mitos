//go:build !linux

package cpupin

import "fmt"

// stubApplier is the non-Linux no-op applier. sched_setaffinity and the RT
// scheduling class are Linux-only, so on darwin (and any other non-Linux host)
// every method is a no-op that returns a clear not-supported signal where a
// value is required. This keeps the activate/fork path compiling and running on
// darwin while applying nothing: the post-ready hook is wired, but no scheduler
// state is touched, so darwin never pins or bumps priority and never fabricates
// a measurement.
type stubApplier struct{}

// NewApplier returns the no-op applier on non-Linux hosts.
func NewApplier() Applier { return stubApplier{} }

// ReadTopology is unavailable off Linux: /sys/devices/system/cpu topology files
// are Linux-only.
func (stubApplier) ReadTopology() (Topology, error) {
	return Topology{}, fmt.Errorf("cpupin: CPU topology read is Linux-only; not available on this platform")
}

// ApplyPin is a no-op off Linux. It still validates so callers get the same
// argument errors on every platform, but it never touches affinity.
func (stubApplier) ApplyPin(req PinRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	return nil
}

// RaiseLaunchPriority is a no-op off Linux.
func (stubApplier) RaiseLaunchPriority(threadIDs []int) error { return nil }

// DropLaunchPriority is a no-op off Linux.
func (stubApplier) DropLaunchPriority(threadIDs []int) error { return nil }

// Supported reports false: the stub changes no scheduler state.
func (stubApplier) Supported() bool { return false }
