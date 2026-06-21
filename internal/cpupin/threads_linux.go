//go:build linux

package cpupin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VCPUThreadIDs enumerates the Firecracker vCPU thread IDs (tids) for the VMM
// process pid. Firecracker names each vCPU kernel thread "fc_vcpu N", so this
// reads /proc/<pid>/task/<tid>/comm and keeps the ones whose name starts with
// "fc_vcpu". The result is sorted by the vCPU index encoded in the name so
// thread i pins to the plan's i-th core. This is the Linux-only bridge between
// the VMM process and the (pure) pin plan; off Linux there are no such threads
// and the caller never reaches here (pinning is a no-op via the stub applier).
func VCPUThreadIDs(pid int) ([]int, error) {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, fmt.Errorf("cpupin: read %s: %w", taskDir, err)
	}
	type vcpuThread struct {
		tid, index int
	}
	var threads []vcpuThread
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "comm")) //nolint:gosec // fixed /proc path
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if !strings.HasPrefix(name, "fc_vcpu") {
			continue
		}
		idx := 0
		if f := strings.Fields(name); len(f) > 1 {
			if n, err := strconv.Atoi(f[len(f)-1]); err == nil {
				idx = n
			}
		}
		threads = append(threads, vcpuThread{tid: tid, index: idx})
	}
	if len(threads) == 0 {
		return nil, fmt.Errorf("cpupin: no fc_vcpu threads found under %s for pid %d", taskDir, pid)
	}
	// Sort by vCPU index so thread i maps to the plan's i-th core.
	for i := 1; i < len(threads); i++ {
		for j := i; j > 0 && threads[j-1].index > threads[j].index; j-- {
			threads[j-1], threads[j] = threads[j], threads[j-1]
		}
	}
	out := make([]int, len(threads))
	for i, t := range threads {
		out[i] = t.tid
	}
	return out, nil
}
