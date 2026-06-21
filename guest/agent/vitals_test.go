//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFixtureProc lays down a minimal fixture procfs tree (a /proc/stat,
// /proc/meminfo, and two pid directories) and points the collector at it. It
// returns nothing; the caller asserts on sampleVitals. This is the darwin-style
// fixture exercise of the real /proc reader, runnable on the linux CI runner and
// the KVM integration run.
func writeFixtureProc(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "stat"),
		"cpu  1000 0 500 5000 0 0 0 100 0 0\ncpu0 1000 0 500 5000 0 0 0 100 0 0\nintr 1\n")
	mustWrite(t, filepath.Join(root, "meminfo"),
		"MemTotal:        2048000 kB\nMemFree:          512000 kB\nMemAvailable:    1024000 kB\n")
	mustWrite(t, filepath.Join(root, "1", "stat"),
		"1 (agent) S 0 1 1 0 -1 4194304 100 0 0 0 30 12 0 0 20 0 1 0 1 100 256 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")
	mustWrite(t, filepath.Join(root, "99", "stat"),
		"99 (python) R 1 99 99 0 -1 0 0 0 0 0 5 5 0 0 20 0 1 0 1 100 512 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")

	oldRoot, oldPage := procRoot, guestPageSize
	procRoot = root
	guestPageSize = 4096
	t.Cleanup(func() { procRoot, guestPageSize = oldRoot, oldPage })
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSampleVitals_Fixture(t *testing.T) {
	writeFixtureProc(t)
	balloonTargetKB = 1536000 // host reclaimed 512000 KB
	t.Cleanup(func() { balloonTargetKB = 0 })

	v, err := sampleVitals()
	if err != nil {
		t.Fatalf("sampleVitals: %v", err)
	}
	if v.MemTotalKB != 2048000 {
		t.Errorf("mem total = %d, want 2048000", v.MemTotalKB)
	}
	if v.MemUsedKB != 1024000 {
		t.Errorf("mem used = %d, want 1024000", v.MemUsedKB)
	}
	if v.BalloonReclaimedKB != 512000 {
		t.Errorf("balloon reclaimed = %d, want 512000", v.BalloonReclaimedKB)
	}
	byPID := map[int]bool{}
	for _, p := range v.Processes {
		byPID[p.PID] = true
		if p.PID == 1 {
			if p.Comm != "agent" || p.State != "S" || p.CPUJiffies != 42 || p.RSSKB != 256*4 {
				t.Errorf("pid 1 entry wrong: %+v", p)
			}
		}
	}
	if !byPID[1] || !byPID[99] {
		t.Errorf("process table missing pids: %+v", v.Processes)
	}
}
