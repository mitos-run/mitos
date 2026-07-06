package metering

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadProcessMemory reads /proc/<pid>/smaps_rollup and returns the process's
// CoW-aware memory split: unique is Private_Clean + Private_Dirty (pages this
// process alone owns) and shared is Shared_Clean + Shared_Dirty (pages mapped
// from a shared file such as a template snapshot restored MAP_PRIVATE). This is
// the same split docs/metering.md documents for the fork engine's per-sandbox
// samples; the husk stub uses it to meter its single in-pod firecracker VM.
//
// A non-positive pid, a missing /proc entry (dead or recycled pid), or an
// unreadable file all return (0, 0): a sandbox going away meters nothing, it
// never errors the report.
func ReadProcessMemory(pid int) (unique, shared int64) {
	if pid <= 0 {
		return 0, 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0, 0
	}
	return parseSmapsRollup(string(data))
}

// parseSmapsRollup extracts the (unique, shared) byte totals from
// smaps_rollup content. Lines are "Field: <n> kB"; unknown fields are ignored.
func parseSmapsRollup(data string) (unique, shared int64) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		bytes := kb * 1024
		switch fields[0] {
		case "Private_Clean:", "Private_Dirty:":
			unique += bytes
		case "Shared_Clean:", "Shared_Dirty:":
			shared += bytes
		}
	}
	return unique, shared
}
