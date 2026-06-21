//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"mitos.run/mitos/internal/guestvitals"
	"mitos.run/mitos/internal/vsock"
)

// vitalsSampleWindow is how long handleVitals samples /proc/stat to compute the
// steal fraction: two reads this far apart. It is short so a TypeVitals request
// stays well under the host's per-request vsock deadline, yet long enough that a
// steal spike registers across at least one scheduler tick.
const vitalsSampleWindow = 100 * time.Millisecond

// procRoot is the procfs mount the collector reads. It is a var so tests on a
// linux runner can point it at a fixture tree; production leaves it at /proc.
var procRoot = "/proc"

// guestPageSize is the page size used to convert RSS pages to kilobytes. It is a
// var for the same fixture reason; on a real guest it is the OS page size.
var guestPageSize = os.Getpagesize()

// balloonTargetKB, when > 0, is the current virtio-balloon target in KB. The
// guest agent has no direct view of the host balloon device, so this is left 0
// unless a future in-guest hook (reading the balloon's reported target via the
// balloon stats vq) populates it; with 0 the reported BalloonReclaimedKB is 0.
// It is a var so the KVM integration run and tests can inject a known target.
var balloonTargetKB uint64

// handleVitals samples the guest telemetry the Layer 3 bridge reports: CPU steal
// over vitalsSampleWindow, memory vs balloon from /proc/meminfo, and the
// in-guest process table from /proc/<pid>/stat. It reads real /proc on the guest
// (KVM-gated end to end) using the platform-neutral parsers in
// internal/guestvitals, which the host and darwin CI exercise against fixtures.
func handleVitals() vsock.Response {
	v, err := sampleVitals()
	if err != nil {
		return vsock.Response{OK: false, Error: err.Error()}
	}
	return vsock.Response{OK: true, Vitals: v}
}

// sampleVitals does the work behind handleVitals, returning a typed error so a
// linux fixture test can assert the assembled snapshot.
func sampleVitals() (*vsock.VitalsResponse, error) {
	t0, err := readProcStat()
	if err != nil {
		return nil, fmt.Errorf("read steal t0: %w", err)
	}
	time.Sleep(vitalsSampleWindow)
	t1, err := readProcStat()
	if err != nil {
		return nil, fmt.Errorf("read steal t1: %w", err)
	}
	steal := guestvitals.StealDelta(t0, t1).StealFraction()

	mem, err := readMeminfo()
	if err != nil {
		return nil, fmt.Errorf("read meminfo: %w", err)
	}

	procs, err := readProcessTable()
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}

	return &vsock.VitalsResponse{
		StealFraction:      steal,
		SampleWindowMs:     float64(vitalsSampleWindow.Milliseconds()),
		MemTotalKB:         mem.TotalKB,
		MemAvailableKB:     mem.AvailableKB,
		MemUsedKB:          mem.UsedKB(),
		BalloonReclaimedKB: guestvitals.BalloonReclaimedKB(mem.TotalKB, balloonTargetKB),
		Processes:          procs,
	}, nil
}

func readProcStat() (guestvitals.ProcStat, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return guestvitals.ProcStat{}, err
	}
	defer f.Close()
	return guestvitals.ParseProcStat(f)
}

func readMeminfo() (guestvitals.Meminfo, error) {
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return guestvitals.Meminfo{}, err
	}
	defer f.Close()
	return guestvitals.ParseMeminfo(f)
}

// readProcessTable walks /proc, parsing each numeric pid directory's stat file
// into a ProcessEntry. A pid that vanishes mid-walk (read error) is skipped
// rather than failing the whole snapshot, since the process table is inherently
// racy. RSS pages are converted to KB with the guest page size.
func readProcessTable() ([]vsock.ProcessEntry, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	pageKB := uint64(guestPageSize) / 1024
	if pageKB == 0 {
		pageKB = 4 // sane fallback; standard 4KB page
	}
	out := make([]vsock.ProcessEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue // pid exited mid-walk
		}
		p, err := guestvitals.ParsePidStat(data)
		if err != nil {
			continue
		}
		out = append(out, vsock.ProcessEntry{
			PID:        p.PID,
			Comm:       p.Comm,
			State:      p.State,
			CPUJiffies: p.UTime + p.STime,
			RSSKB:      p.RSSPages * pageKB,
		})
	}
	return out, nil
}
