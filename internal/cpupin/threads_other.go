//go:build !linux

package cpupin

import "fmt"

// VCPUThreadIDs is unavailable off Linux: there is no /proc and no Firecracker
// vCPU kernel threads to enumerate. The non-Linux engine never reaches a real
// pin (the stub applier is a no-op), so this exists only to keep the post-ready
// hook compiling on darwin.
func VCPUThreadIDs(pid int) ([]int, error) {
	return nil, fmt.Errorf("cpupin: vCPU thread enumeration is Linux-only (pid=%d)", pid)
}
