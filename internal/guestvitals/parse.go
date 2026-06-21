// Package guestvitals holds the platform-neutral parsers and arithmetic for the
// Layer 3 guest telemetry bridge (issue #164): CPU steal from /proc/stat, memory
// vs balloon, and the in-guest process table. The parsers take readers and byte
// slices rather than touching /proc directly, so the guest agent (linux) wires
// them to real /proc while the host and darwin CI exercise them against fixture
// content. The vsock message shapes that carry the result live in
// internal/vsock; this package only parses and computes.
package guestvitals

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ProcStat is the parsed aggregate "cpu " line of /proc/stat: cumulative jiffies
// since boot in each class. Only the fields the steal computation needs are
// kept; the rest of the line is ignored. Jiffies are monotonic per boot but a
// snapshot restore can rewind them, which StealDelta handles.
type ProcStat struct {
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	IOWait  uint64
	IRQ     uint64
	SoftIRQ uint64
	Steal   uint64
}

// Total is the sum of every accounted jiffy class on the line, i.e. the wall
// jiffies the CPU spent in any state including idle and stolen.
func (s ProcStat) Total() uint64 {
	return s.User + s.Nice + s.System + s.Idle + s.IOWait + s.IRQ + s.SoftIRQ + s.Steal
}

// ParseProcStat reads /proc/stat content and returns the aggregate "cpu " line.
// The aggregate line starts with "cpu " (two spaces because the field is padded
// against the per-core "cpu0" lines). It must carry at least 8 numeric fields so
// the steal column (the 8th) is present; a kernel too old to report steal is
// rejected rather than silently reporting 0 steal.
func ParseProcStat(r io.Reader) (ProcStat, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") && !strings.HasPrefix(line, "cpu\t") {
			continue
		}
		fields := strings.Fields(line)
		// fields[0] == "cpu"; the 8 jiffy classes follow.
		if len(fields) < 9 {
			return ProcStat{}, fmt.Errorf("proc/stat cpu line has %d fields, need >= 9", len(fields))
		}
		vals := make([]uint64, 8)
		for i := 0; i < 8; i++ {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return ProcStat{}, fmt.Errorf("proc/stat cpu field %d: %w", i, err)
			}
			vals[i] = v
		}
		return ProcStat{
			User:    vals[0],
			Nice:    vals[1],
			System:  vals[2],
			Idle:    vals[3],
			IOWait:  vals[4],
			IRQ:     vals[5],
			SoftIRQ: vals[6],
			Steal:   vals[7],
		}, nil
	}
	if err := sc.Err(); err != nil {
		return ProcStat{}, fmt.Errorf("read proc/stat: %w", err)
	}
	return ProcStat{}, fmt.Errorf("proc/stat: no aggregate cpu line")
}

// StealSample is the steal accounting over an interval bounded by two ProcStat
// snapshots: the jiffies stolen and the total wall jiffies elapsed.
type StealSample struct {
	StealJiffies uint64
	TotalJiffies uint64
}

// StealDelta computes the steal accounting between an earlier and later
// /proc/stat snapshot. If either cumulative counter went backwards (a snapshot
// restore can rewind /proc/stat) the interval is treated as zero so the caller
// reports 0 steal rather than a wrapped-around garbage value.
func StealDelta(earlier, later ProcStat) StealSample {
	if later.Steal < earlier.Steal || later.Total() < earlier.Total() {
		return StealSample{}
	}
	return StealSample{
		StealJiffies: later.Steal - earlier.Steal,
		TotalJiffies: later.Total() - earlier.Total(),
	}
}

// StealFraction is the fraction of the interval's wall time the vCPU spent
// involuntarily descheduled by the host (stolen). It is in [0,1]; a zero-length
// or non-monotonic interval reports 0.
func (s StealSample) StealFraction() float64 {
	if s.TotalJiffies == 0 {
		return 0
	}
	f := float64(s.StealJiffies) / float64(s.TotalJiffies)
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
