package guestvitals

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// PidStat is the subset of /proc/<pid>/stat the process table reports: the pid,
// the command (comm), the single-letter state, the user and system jiffies the
// process has accrued, and its resident set size in pages. Comm is not a secret
// (it is the program name, already visible to anyone who can exec in the guest)
// but it is not sanitized further here; the caller is responsible for any
// display escaping.
type PidStat struct {
	PID      int
	PPID     int
	Comm     string
	State    string
	UTime    uint64
	STime    uint64
	RSSPages uint64
}

// ParsePidStat parses one /proc/<pid>/stat line. The comm field (field 2) is
// wrapped in parentheses and may itself contain spaces and parentheses, so the
// command is delimited by the FIRST '(' and the LAST ')'; the remaining
// space-separated fields are offset from after that ')'. Field numbering in the
// proc(5) man page is 1-based starting at pid; after the comm the next field
// (state) is field 3, utime is field 14, stime field 15, rss field 24.
func ParsePidStat(line []byte) (PidStat, error) {
	s := string(line)
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return PidStat{}, fmt.Errorf("pid stat: no comm parentheses")
	}
	pidField := strings.TrimSpace(s[:open])
	pid, err := strconv.Atoi(pidField)
	if err != nil {
		return PidStat{}, fmt.Errorf("pid stat: pid field %q: %w", pidField, err)
	}
	comm := s[open+1 : close]
	rest := strings.Fields(s[close+1:])
	// rest[0] is state (field 3); ppid is field 4 => rest index 1. utime is field
	// 14 => rest index 11; stime is field 15 => rest index 12; rss is field 24 =>
	// rest index 21.
	if len(rest) < 22 {
		return PidStat{}, fmt.Errorf("pid stat: only %d post-comm fields, need >= 22", len(rest))
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return PidStat{}, fmt.Errorf("pid stat: ppid: %w", err)
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return PidStat{}, fmt.Errorf("pid stat: utime: %w", err)
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return PidStat{}, fmt.Errorf("pid stat: stime: %w", err)
	}
	rss, err := strconv.ParseUint(rest[21], 10, 64)
	if err != nil {
		return PidStat{}, fmt.Errorf("pid stat: rss: %w", err)
	}
	return PidStat{
		PID:      pid,
		PPID:     ppid,
		Comm:     comm,
		State:    rest[0],
		UTime:    utime,
		STime:    stime,
		RSSPages: rss,
	}, nil
}

// Meminfo is the subset of /proc/meminfo the vitals bridge reports: the
// guest-visible total, free, and available memory, all in kilobytes.
type Meminfo struct {
	TotalKB     uint64
	FreeKB      uint64
	AvailableKB uint64
}

// UsedKB is the memory the guest considers in use: total minus available (the
// kernel's own estimate of what is reclaimable without swapping). When
// MemAvailable is absent (very old kernels) the caller gets a parse error from
// ParseMeminfo instead, so this never divides by a phantom.
func (m Meminfo) UsedKB() uint64 {
	if m.AvailableKB > m.TotalKB {
		return 0
	}
	return m.TotalKB - m.AvailableKB
}

// ParseMeminfo reads /proc/meminfo content. MemTotal is required (without it the
// numbers are meaningless); MemFree and MemAvailable are best-effort. Each line
// is "Key:   <value> kB"; the value is the second field.
func ParseMeminfo(r io.Reader) (Meminfo, error) {
	var m Meminfo
	var haveTotal bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.TotalKB = v
			haveTotal = true
		case "MemFree":
			m.FreeKB = v
		case "MemAvailable":
			m.AvailableKB = v
		}
	}
	if err := sc.Err(); err != nil {
		return Meminfo{}, fmt.Errorf("read meminfo: %w", err)
	}
	if !haveTotal {
		return Meminfo{}, fmt.Errorf("meminfo: MemTotal absent")
	}
	return m, nil
}

// BalloonReclaimedKB is the memory the host has reclaimed from the guest via the
// virtio-balloon: the difference between the guest-visible total and the current
// balloon target (both in KB). A target at or above total means the balloon was
// never inflated, so nothing is reclaimed; the result is never negative.
func BalloonReclaimedKB(guestTotalKB, balloonTargetKB uint64) uint64 {
	if balloonTargetKB >= guestTotalKB {
		return 0
	}
	return guestTotalKB - balloonTargetKB
}
