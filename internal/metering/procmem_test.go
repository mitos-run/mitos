package metering

import "testing"

// TestParseSmapsRollup asserts the CoW split matches docs/metering.md: unique
// is Private_Clean + Private_Dirty, shared is Shared_Clean + Shared_Dirty, and
// every other rollup field is ignored.
func TestParseSmapsRollup(t *testing.T) {
	const rollup = `560f2ad00000-7ffc7de81000 ---p 00000000 00:00 0    [rollup]
Rss:              524288 kB
Pss:              300000 kB
Shared_Clean:     409600 kB
Shared_Dirty:      16384 kB
Private_Clean:     32768 kB
Private_Dirty:     65536 kB
Referenced:       524288 kB
Anonymous:         81920 kB
Swap:                  0 kB
`
	unique, shared := parseSmapsRollup(rollup)
	if want := int64(32768+65536) * 1024; unique != want {
		t.Errorf("unique = %d, want %d", unique, want)
	}
	if want := int64(409600+16384) * 1024; shared != want {
		t.Errorf("shared = %d, want %d", shared, want)
	}
}

// TestReadProcessMemoryGuards asserts the dead/invalid pid paths meter zero
// instead of erroring.
func TestReadProcessMemoryGuards(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if u, s := ReadProcessMemory(pid); u != 0 || s != 0 {
			t.Errorf("ReadProcessMemory(%d) = (%d, %d), want (0, 0)", pid, u, s)
		}
	}
}
