// Package casgc drives the content-addressed store's eviction (cas.EvictToFit)
// so orphaned chunks do not grow unbounded and trip node DiskPressure (#464).
// The CAS already evicts least-recently-used UNPINNED chunks and never touches a
// pinned (active-template) manifest; this package is the missing trigger: a
// periodic loop that evicts when the data-dir filesystem crosses a high
// watermark, down to a low watermark.
package casgc

import (
	"context"
	"time"
)

// Store is the slice of cas.Store this package needs (cas.Store satisfies it).
type Store interface {
	TotalBytes() (int64, error)
	EvictToFit(maxBytes int64) (int64, error)
}

// DiskUsageFunc reports the used and total bytes of the filesystem holding path.
type DiskUsageFunc func(path string) (used, total int64, err error)

// Target computes the CAS byte budget to evict down to. When the filesystem is
// below the high watermark it returns evict=false (nothing to do). Otherwise it
// returns the CAS size that, once evicted to, brings the filesystem to the low
// watermark, clamped to >= 0 (0 means evict every unpinned chunk). Pinned chunks
// are protected by EvictToFit regardless, so an active template is never evicted.
func Target(usedBytes, totalBytes, casBytes int64, high, low float64) (maxBytes int64, evict bool) {
	if totalBytes <= 0 || casBytes <= 0 {
		return 0, false
	}
	if float64(usedBytes) < high*float64(totalBytes) {
		return 0, false
	}
	target := int64(low * float64(totalBytes)) // desired used bytes after GC
	toFree := usedBytes - target
	if toFree <= 0 {
		return 0, false
	}
	maxCAS := casBytes - toFree
	if maxCAS < 0 {
		maxCAS = 0
	}
	return maxCAS, true
}

// tick runs one GC evaluation. Exposed for tests.
func tick(store Store, dataDir string, high, low float64, usage DiskUsageFunc, logf func(string, ...any)) {
	used, total, err := usage(dataDir)
	if err != nil {
		logf("cas gc: read disk usage of %s: %v", dataDir, err)
		return
	}
	casBytes, err := store.TotalBytes()
	if err != nil {
		logf("cas gc: read cas size: %v", err)
		return
	}
	maxBytes, evict := Target(used, total, casBytes, high, low)
	if !evict {
		return
	}
	freed, err := store.EvictToFit(maxBytes)
	if err != nil {
		logf("cas gc: evict to %d bytes: %v", maxBytes, err)
		return
	}
	if freed > 0 {
		logf("cas gc: freed %d bytes (disk used %d/%d, cas target %d)", freed, used, total, maxBytes)
	}
}

// Run drives tick on interval until ctx is cancelled. It is the trigger the CAS
// eviction primitive lacked (#464). logf is the (non-secret) log sink.
func Run(ctx context.Context, store Store, dataDir string, interval time.Duration, high, low float64, usage DiskUsageFunc, logf func(string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick(store, dataDir, high, low, usage, logf)
		}
	}
}
